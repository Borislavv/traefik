package repository

import (
	"bytes"
	"context"
	"github.com/traefik/traefik/v3/pkg/advancedcache/config"
	"github.com/traefik/traefik/v3/pkg/advancedcache/pools"
	"github.com/valyala/fasthttp"
	"golang.org/x/time/rate"
	"net"
	"net/http"
	"sync"
	"time"
	"unsafe"
)

var transport = &http.Transport{
	// Max idle (keep-alive) connections across ALL hosts
	MaxIdleConns: 10000,

	// Max idle (keep-alive) connections per host
	MaxIdleConnsPerHost: 1000,

	// Max concurrent connections per host (optional)
	MaxConnsPerHost: 0, // 0 = unlimited (use with caution)

	IdleConnTimeout: 30 * time.Second,

	// Optional: tune dialer
	DialContext: (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext,

	// Optional: configure TLS handshake timeout, etc.
	TLSHandshakeTimeout: 10 * time.Second,

	// ExpectContinueTimeout: wait time for 100-continue
	ExpectContinueTimeout: 1 * time.Second,
}

// Backender defines the interface for a repository that provides SEO page data.
type Backender interface {
	Fetch(
		rule *config.Rule, path []byte, query []byte, queryHeaders *[][2][]byte,
	) (
		status int, headers *[][2][]byte, body []byte, releaseFn func(), err error,
	)

	RevalidatorMaker() func(
		rule *config.Rule, path []byte, query []byte, queryHeaders *[][2][]byte,
	) (
		status int, headers *[][2][]byte, body []byte, releaseFn func(), err error,
	)
}

// Backend implements the Backender interface.
// It fetches and constructs SEO page data responses from an external backend.
type Backend struct {
	ctx         context.Context
	cfg         *config.Cache // Global configuration (backend URL, etc)
	transport   *http.Transport
	clientsPool *sync.Pool
	rateLimiter *rate.Limiter
}

// NewBackend creates a new instance of Backend.
func NewBackend(ctx context.Context, cfg *config.Cache) *Backend {
	return &Backend{
		ctx: ctx,
		cfg: cfg,
		clientsPool: &sync.Pool{
			New: func() interface{} {
				return &http.Client{
					Transport: transport,
					Timeout:   10 * time.Second,
				}
			},
		},
		rateLimiter: rate.NewLimiter(
			rate.Limit(cfg.Cache.Upstream.Rate),
			cfg.Cache.Upstream.Rate/10,
		),
	}
}

func (s *Backend) Fetch(
	rule *config.Rule, path []byte, query []byte, queryHeaders *[][2][]byte,
) (
	status int, headers *[][2][]byte, body []byte, releaseFn func(), err error,
) {
	if err = s.rateLimiter.Wait(s.ctx); err != nil {
		return 0, nil, nil, emptyReleaseFn, err
	}

	return s.requestExternalBackend(rule, path, query, queryHeaders)
}

// RevalidatorMaker builds a new revalidator for model.Response by catching a request into closure for be able to call backend later.
func (s *Backend) RevalidatorMaker() func(
	rule *config.Rule, path []byte, query []byte, queryHeaders *[][2][]byte,
) (
	status int, headers *[][2][]byte, body []byte, releaseFn func(), err error,
) {
	return func(
		rule *config.Rule, path []byte, query []byte, queryHeaders *[][2][]byte,
	) (
		status int, headers *[][2][]byte, body []byte, releaseFn func(), err error,
	) {
		return s.requestExternalBackend(rule, path, query, queryHeaders)
	}
}

var (
	emptyReleaseFn = func() {}
	urlBufPool     = sync.Pool{
		New: func() interface{} {
			return new(bytes.Buffer)
		},
	}
	queryPrefix = []byte("?")
)

// requestExternalBackend actually performs the HTTP request to backend and parses the response.
func (s *Backend) requestExternalBackend(
	rule *config.Rule, path []byte, query []byte, queryHeaders *[][2][]byte,
) (status int, headers *[][2][]byte, body []byte, releaseFn func(), err error) {
	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)

	req.Header.SetMethod(fasthttp.MethodGet)
	url := unsafe.Slice(unsafe.StringData(s.cfg.Cache.Upstream.Url), len(s.cfg.Cache.Upstream.Url))

	urlBuf := urlBufPool.Get().(*bytes.Buffer)
	urlBuf.Grow(len(url) + len(path) + len(query) + 1)
	defer func() {
		urlBuf.Reset()
		urlBufPool.Put(urlBuf)
	}()
	if _, err = urlBuf.Write(url); err != nil {
		return 0, nil, nil, emptyReleaseFn, err
	}
	if _, err = urlBuf.Write(path); err != nil {
		return 0, nil, nil, emptyReleaseFn, err
	}
	if _, err = urlBuf.Write(queryPrefix); err != nil {
		return 0, nil, nil, emptyReleaseFn, err
	}
	if _, err = urlBuf.Write(query); err != nil {
		return 0, nil, nil, emptyReleaseFn, err
	}
	req.SetRequestURI(unsafe.String(unsafe.SliceData(urlBuf.Bytes()), urlBuf.Len()))

	var isBot bool
	for _, kv := range *queryHeaders {
		req.Header.SetBytesKV(kv[0], kv[1])

		if bytes.Equal(kv[0], s.cfg.Cache.LifeTime.EscapeMaxReqDurationHeaderBytes) {
			isBot = true
		}
	}

	var timeout = s.cfg.Cache.LifeTime.MaxReqDuration
	if isBot {
		timeout = s.cfg.Cache.Upstream.Timeout
	}

	resp := fasthttp.AcquireResponse()
	if err = pools.BackendHttpClientPool.DoTimeout(req, resp, timeout); err != nil {
		return 0, nil, nil, emptyReleaseFn, err
	}

	headers = pools.KeyValueSlicePool.Get().(*[][2][]byte)

	allowedHeadersMap := rule.CacheValue.HeadersMap
	resp.Header.VisitAll(func(k, v []byte) {
		if _, ok := allowedHeadersMap[unsafe.String(unsafe.SliceData(k), len(k))]; ok {
			*headers = append(*headers, [2][]byte{k, v})
		}
	})

	buf := pools.BackendBodyBufferPool.Get().(*bytes.Buffer)
	if _, err = buf.Write(resp.Body()); err != nil {
		return 0, nil, nil, emptyReleaseFn, err
	}

	return resp.StatusCode(), headers, buf.Bytes(), func() {
		*headers = (*headers)[:0]
		pools.KeyValueSlicePool.Put(headers)

		buf.Reset()
		pools.BackendBodyBufferPool.Put(buf)

		fasthttp.ReleaseResponse(resp)
	}, nil
}
