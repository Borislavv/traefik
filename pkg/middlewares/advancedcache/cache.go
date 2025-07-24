package advancedcachemiddleware

import (
	"context"
	"github.com/traefik/traefik/v3/pkg/advancedcache/config"
	"github.com/traefik/traefik/v3/pkg/advancedcache/header"
	"github.com/traefik/traefik/v3/pkg/advancedcache/model"
	"github.com/traefik/traefik/v3/pkg/advancedcache/prometheus/metrics"
	"github.com/traefik/traefik/v3/pkg/advancedcache/repository"
	"github.com/traefik/traefik/v3/pkg/advancedcache/storage"
	httpwriter "github.com/traefik/traefik/v3/pkg/advancedcache/writer"
	"net/http"
	"sync/atomic"
	"time"
	"unsafe"
)

var (
	contentTypeKey       = "Content-Type"
	applicationJsonValue = "application/json"
)

// enabled indicates whether the advanced cache is turned on or off.
// It can be safely accessed and modified concurrently.
var enabled atomic.Bool

var (
	hits          = &atomic.Uint64{}
	misses        = &atomic.Uint64{}
	errors        = &atomic.Uint64{}
	totalDuration = &atomic.Int64{} // UnixNano
)

type AdvancedCacheMiddleware struct {
	ctx       context.Context
	next      http.Handler
	name      string
	cfg       *config.Cache
	storage   storage.Storage
	backend   repository.Backender
	refresher storage.Refresher
	evictor   storage.Evictor
	dumper    storage.Dumper
	metrics   metrics.Meter
	count     int64 // Num
	duration  int64 // UnixNano
}

func NewAdvancedCache(ctx context.Context, next http.Handler, config *config.TraefikIntermediateConfig, name string) http.Handler {
	cacheMiddleware := &AdvancedCacheMiddleware{
		ctx:  ctx,
		next: next,
		name: name,
	}

	if err := cacheMiddleware.run(ctx, config); err != nil {
		panic(err)
	}

	return cacheMiddleware
}

func (m *AdvancedCacheMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var from = time.Now()
	defer func() { totalDuration.Add(time.Since(from).Nanoseconds()) }()

	if enabled.Load() {
		m.handleThroughCache(w, r)
	} else {
		hits.Add(1)
		m.next.ServeHTTP(w, r)
	}
	return
}

func (m *AdvancedCacheMiddleware) handleThroughCache(w http.ResponseWriter, r *http.Request) {
	newEntry, err := model.NewEntryNetHttp(m.cfg, r)
	if err != nil {
		errors.Add(1)
		// Path was not matched, then handle request through upstream without cache.
		m.next.ServeHTTP(w, r)
		return
	}

	var (
		status       int
		headers      *[][2][]byte
		body         []byte
		lastModified int64
	)

	foundEntry, found := m.storage.Get(newEntry)
	if !found {
		misses.Add(1)

		// MISS — prepare capture writer
		captured, releaseCapturer := httpwriter.NewCaptureResponseWriter(w)
		defer releaseCapturer()

		// Run downstream handler
		m.next.ServeHTTP(captured, r)

		// path is immutable and used only inside request
		path := unsafe.Slice(unsafe.StringData(r.URL.Path), len(r.URL.Path))

		// query immutable and used only inside request
		query := unsafe.Slice(unsafe.StringData(r.URL.RawQuery), len(r.URL.RawQuery))

		// Get query headers from original request
		queryHeaders, queryHeadersReleaser := newEntry.GetFilteredAndSortedKeyHeadersNetHttp(r)
		defer queryHeadersReleaser(queryHeaders)

		var extractReleaser func(*[][2][]byte)
		status, headers, body, extractReleaser = captured.ExtractPayload()
		defer extractReleaser(headers)

		if status != http.StatusOK {
			errors.Add(1)

			// non-positive status code received, skip saving
			defer newEntry.Remove()

			lastModified = time.Now().Unix()
		} else {
			// Save the response into the new newEntry
			newEntry.SetPayload(path, query, queryHeaders, headers, body, status)
			newEntry.SetRevalidator(m.backend.RevalidatorMaker())

			// build and store new Entry in cache
			foundEntry = m.storage.Set(model.NewVersionPointer(newEntry))
			defer foundEntry.Release() // an Entry stored in the cache must be released after use

			lastModified = foundEntry.UpdateAt()
		}
	} else {
		hits.Add(1)

		// deferred release and remove
		newEntry.Remove()          // new Entry which was used as request for query cache does not need anymore
		defer foundEntry.Release() // an Entry retrieved from the cache must be released after use

		// Always read from cached foundEntry
		var queryHeaders *[][2][]byte
		var payloadReleaser func(q, h *[][2][]byte)
		_, _, queryHeaders, headers, body, status, payloadReleaser, err = foundEntry.Payload()
		defer payloadReleaser(queryHeaders, headers)
		if err != nil {
			errors.Add(1)

			// ERROR — prepare capture writer
			captured, releaseCapturer := httpwriter.NewCaptureResponseWriter(w)
			defer releaseCapturer()

			m.next.ServeHTTP(captured, r)

			var extractReleaser func(*[][2][]byte)
			status, headers, body, extractReleaser = captured.ExtractPayload()
			defer extractReleaser(headers)
		}

		lastModified = foundEntry.UpdateAt()
	}

	// Write cached headers
	for _, kv := range *headers {
		w.Header().Add(
			unsafe.String(unsafe.SliceData(kv[0]), len(kv[0])),
			unsafe.String(unsafe.SliceData(kv[1]), len(kv[1])),
		)
	}

	// Last-Modified
	header.SetLastModifiedValueNetHttp(w, lastModified)

	// Content-Type
	w.Header().Set(contentTypeKey, applicationJsonValue)

	// StatusCode-code
	w.WriteHeader(status)

	// Write a response body
	_, _ = w.Write(body)

	// Metrics
	atomic.AddInt64(&m.count, 1)
}
