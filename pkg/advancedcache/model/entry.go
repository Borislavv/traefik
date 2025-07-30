package model

import (
	"bytes"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/rs/zerolog/log"
	"github.com/traefik/traefik/v3/pkg/advancedcache/config"
	"github.com/traefik/traefik/v3/pkg/advancedcache/list"
	"github.com/traefik/traefik/v3/pkg/advancedcache/pools"
	"github.com/traefik/traefik/v3/pkg/advancedcache/repository"
	"github.com/traefik/traefik/v3/pkg/advancedcache/sort"
	sharded "github.com/traefik/traefik/v3/pkg/advancedcache/storage/map"
	"github.com/valyala/fasthttp"
	"github.com/zeebo/xxh3"
)

var (
	bufPool           = &sync.Pool{New: func() any { return new(bytes.Buffer) }}
	hasherPool        = &sync.Pool{New: func() any { return xxh3.New() }}
	ruleNotFoundError = errors.New("rule not found")
)

// Entry is the packed request+response payload
type Entry struct {
	key          uint64   // 64  bit xxh
	shard        uint64   // 64  bit xxh % NumOfShards
	fingerprint  [16]byte // 128 bit xxh
	rule         *config.Rule
	payload      *atomic.Pointer[[]byte]
	lruListElem  *atomic.Pointer[list.Element[*Entry]]
	revalidator  Revalidator
	updatedAt    int64 // atomic: unix nano (last update was at)
	isCompressed int64 // atomic: bool as int64
}

func (e *Entry) Init() *Entry {
	e.payload = &atomic.Pointer[[]byte]{}
	e.lruListElem = &atomic.Pointer[list.Element[*Entry]]{}
	atomic.StoreInt64(&e.updatedAt, time.Now().UnixNano())
	return e
}

// NewEntryNetHttp accepts path, query and request headers as bytes slices.
func NewEntryNetHttp(cfg *config.Cache, r *http.Request) (*Entry, error) {
	// path is a string in net/http so easily refer to it inside request
	rule := MatchRuleStr(cfg, r.URL.Path)
	if rule == nil {
		return nil, ruleNotFoundError
	}

	entry := new(Entry).Init()
	entry.rule = rule

	filteredQueries, filteredQueriesReleaser := entry.GetFilteredAndSortedKeyQueriesNetHttp(r)
	defer filteredQueriesReleaser(filteredQueries)

	filteredHeaders, filteredHeadersReleaser := entry.GetFilteredAndSortedKeyHeadersNetHttp(r)
	defer filteredHeadersReleaser(filteredHeaders)

	entry.calculateAndSetUpKeys(filteredQueries, filteredHeaders)

	return entry, nil
}

// NewEntryFastHttp accepts path, query and request headers as bytes slices.
func NewEntryFastHttp(cfg *config.Cache, r *fasthttp.RequestCtx) (*Entry, error) {
	rule := MatchRule(cfg, r.Path())
	if rule == nil {
		return nil, ruleNotFoundError
	}

	entry := new(Entry).Init()
	entry.rule = rule

	filteredQueries, filteredQueriesReleaser := entry.getFilteredAndSortedKeyQueriesFastHttp(r)
	defer filteredQueriesReleaser(filteredQueries)

	filteredHeaders, filteredHeadersReleaser := entry.getFilteredAndSortedKeyHeadersFastHttp(r)
	defer filteredHeadersReleaser(filteredHeaders)

	entry.calculateAndSetUpKeys(filteredQueries, filteredHeaders)

	return entry, nil
}

func IsRouteWasNotFound(err error) bool {
	return errors.Is(err, ruleNotFoundError)
}

func NewEntryManual(cfg *config.Cache, path, query []byte, headers *[][2][]byte, revalidator Revalidator) (*Entry, error) {
	rule := MatchRule(cfg, path)
	if rule == nil {
		return nil, ruleNotFoundError
	}

	entry := new(Entry).Init()
	entry.rule = rule
	entry.revalidator = revalidator

	filteredQueries, filteredQueriesReleaser := entry.parseFilterAndSortQuery(query) // here, we are referring to the same query buffer which used in payload which have been mentioned before
	defer filteredQueriesReleaser(filteredQueries)                                   // this is really reduce memory usage and GC pressure

	filteredHeaders := entry.filteredAndSortedKeyHeadersInPlace(headers)

	entry.calculateAndSetUpKeys(filteredQueries, filteredHeaders)

	return entry, nil
}

func NewEntryFromField(
	key uint64,
	shard uint64,
	fingerprint [16]byte,
	payload []byte,
	rule *config.Rule,
	revalidator Revalidator,
	isCompressed int64,
	updatedAt int64,
) *Entry {
	entry := new(Entry).Init()
	entry.key = key
	entry.shard = shard
	entry.fingerprint = fingerprint
	entry.rule = rule
	entry.payload.Store(&payload)
	entry.revalidator = revalidator
	entry.isCompressed = isCompressed
	entry.updatedAt = updatedAt
	return entry
}

func (e *Entry) MapKey() uint64   { return e.key }
func (e *Entry) ShardKey() uint64 { return e.shard }

var keyBufPool = sync.Pool{
	New: func() interface{} {
		return new(bytes.Buffer)
	},
}

func (e *Entry) calculateAndSetUpKeys(filteredQueries, filteredHeaders *[][2][]byte) *Entry {
	l := 0
	for _, pair := range *filteredQueries {
		l += len(pair[0]) + len(pair[1])
	}
	for _, pair := range *filteredHeaders {
		l += len(pair[0]) + len(pair[1])
	}

	buf := keyBufPool.Get().(*bytes.Buffer)
	buf.Grow(l)
	defer func() {
		buf.Reset()
		keyBufPool.Put(buf)
	}()

	for _, pair := range *filteredQueries {
		buf.Write(pair[0])
		buf.Write(pair[1])
	}
	for _, pair := range *filteredHeaders {
		buf.Write(pair[0])
		buf.Write(pair[1])
	}

	hasher := hasherPool.Get().(*xxh3.Hasher)
	defer func() {
		hasher.Reset()
		hasherPool.Put(hasher)
	}()

	// calculate key hash
	if _, err := hasher.Write(buf.Bytes()); err != nil {
		panic(err)
	}
	e.key = hasher.Sum64()

	// calculate fingerprint hash
	fp := hasher.Sum128()
	var fingerprint [16]byte
	binary.LittleEndian.PutUint64(fingerprint[0:8], fp.Lo)
	binary.LittleEndian.PutUint64(fingerprint[8:16], fp.Hi)
	e.fingerprint = fingerprint

	// calculate shard index
	e.shard = sharded.MapShardKey(e.key)

	return e
}

func (e *Entry) DumpBuffer(r *fasthttp.RequestCtx) {
	filteredQueries, filteredQueriesReleaser := e.getFilteredAndSortedKeyQueriesFastHttp(r)
	defer filteredQueriesReleaser(filteredQueries)

	filteredHeaders, filteredHeadersReleaser := e.getFilteredAndSortedKeyHeadersFastHttp(r)
	defer filteredHeadersReleaser(filteredHeaders)

	l := 0
	for _, pair := range *filteredQueries {
		l += len(pair[0]) + len(pair[1])
	}
	for _, pair := range *filteredHeaders {
		l += len(pair[0]) + len(pair[1])
	}

	buf := keyBufPool.Get().(*bytes.Buffer)
	buf.Grow(l)
	defer func() {
		buf.Reset()
		keyBufPool.Put(buf)
	}()

	for _, pair := range *filteredQueries {
		buf.Write(pair[0])
		buf.Write(pair[1])
	}
	for _, pair := range *filteredHeaders {
		buf.Write(pair[0])
		buf.Write(pair[1])
	}

	fmt.Println("buffer--->>>>", buf.String())
}

func (e *Entry) Fingerprint() [16]byte {
	return e.fingerprint
}

func (e *Entry) IsSamePayload(another *Entry) bool {
	return e.isPayloadsAreEquals(e.PayloadBytes(), another.PayloadBytes())
}

func (e *Entry) IsSameFingerprint(another [16]byte) bool {
	return subtle.ConstantTimeCompare(e.fingerprint[:], another[:]) == 1
}

func (e *Entry) IsSameEntry(another *Entry) bool {
	return subtle.ConstantTimeCompare(e.fingerprint[:], another.fingerprint[:]) == 1 &&
		e.isPayloadsAreEquals(e.PayloadBytes(), another.PayloadBytes())
}

func (e *Entry) SwapPayloads(another *Entry) {
	another.payload.Store(e.payload.Swap(another.payload.Load()))
}

func (e *Entry) TouchUpdatedAt() {
	atomic.StoreInt64(&e.updatedAt, time.Now().Unix())
}

func (e *Entry) SetRevalidator(revalidator Revalidator) {
	e.revalidator = revalidator
}

func (e *Entry) isPayloadsAreEquals(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	if len(a) < 32 {
		return bytes.Equal(a, b)
	}

	ha := xxh3.Hash(a[:8]) ^ xxh3.Hash(a[len(a)/2:len(a)/2+8]) ^ xxh3.Hash(a[len(a)-8:])
	hb := xxh3.Hash(b[:8]) ^ xxh3.Hash(b[len(b)/2:len(b)/2+8]) ^ xxh3.Hash(b[len(b)-8:])
	return ha == hb
}

// SetPayload packs and gzip-compresses the entire payload: Path, Query, QueryHeaders, StatusCode, ResponseHeaders, Body.
func (e *Entry) SetPayload(
	path, query []byte,
	queryHeaders *[][2][]byte,
	headers *[][2][]byte,
	body []byte,
	status int,
) {
	queryHeadersDeref := *queryHeaders
	responseHeadersDeref := *headers

	numQueryHeaders := len(queryHeadersDeref)
	numResponseHeaders := len(responseHeadersDeref)

	// === 1) Calculate total size ===
	total := 0
	total += 4 + len(path)
	total += 4 + len(query)
	total += 4
	for _, kv := range queryHeadersDeref {
		total += 4 + len(kv[0]) + 4 + len(kv[1])
	}
	total += 4
	total += 4
	for _, kv := range responseHeadersDeref {
		total += 4 + len(kv[0]) + 4 + 4 + len(kv[1])
	}
	total += 4 + len(body)

	// === 2) Allocate ===
	payloadBuf := make([]byte, 0, total)
	offset := 0

	var scratch [4]byte

	// === 3) Write ===

	// Path
	binary.LittleEndian.PutUint32(scratch[:], uint32(len(path)))
	payloadBuf = append(payloadBuf, scratch[:]...)
	payloadBuf = append(payloadBuf, path...)
	offset += len(path)

	// Query
	binary.LittleEndian.PutUint32(scratch[:], uint32(len(query)))
	payloadBuf = append(payloadBuf, scratch[:]...)
	payloadBuf = append(payloadBuf, query...)
	offset += len(query)

	// QueryHeaders
	binary.LittleEndian.PutUint32(scratch[:], uint32(numQueryHeaders))
	payloadBuf = append(payloadBuf, scratch[:]...)
	for _, kv := range queryHeadersDeref {
		binary.LittleEndian.PutUint32(scratch[:], uint32(len(kv[0])))
		payloadBuf = append(payloadBuf, scratch[:]...)
		payloadBuf = append(payloadBuf, kv[0]...)
		offset += len(kv[0])

		binary.LittleEndian.PutUint32(scratch[:], uint32(len(kv[1])))
		payloadBuf = append(payloadBuf, scratch[:]...)
		payloadBuf = append(payloadBuf, kv[1]...)
		offset += len(kv[1])
	}

	// StatusCode
	binary.LittleEndian.PutUint32(scratch[:], uint32(status))
	payloadBuf = append(payloadBuf, scratch[:]...)

	// ResponseHeaders
	binary.LittleEndian.PutUint32(scratch[:], uint32(numResponseHeaders))
	payloadBuf = append(payloadBuf, scratch[:]...)
	for _, kv := range responseHeadersDeref {
		binary.LittleEndian.PutUint32(scratch[:], uint32(len(kv[0])))
		payloadBuf = append(payloadBuf, scratch[:]...)
		payloadBuf = append(payloadBuf, kv[0]...)
		offset += len(kv[0])

		binary.LittleEndian.PutUint32(scratch[:], uint32(1))
		payloadBuf = append(payloadBuf, scratch[:]...)
		binary.LittleEndian.PutUint32(scratch[:], uint32(len(kv[1])))
		payloadBuf = append(payloadBuf, scratch[:]...)
		payloadBuf = append(payloadBuf, kv[1]...)
		offset += len(kv[1])
	}

	// Body
	binary.LittleEndian.PutUint32(scratch[:], uint32(len(body)))
	payloadBuf = append(payloadBuf, scratch[:]...)
	payloadBuf = append(payloadBuf, body...)
	offset += len(body)

	// === 5) Store raw ===
	payloadBuf = payloadBuf[:]
	e.payload.Store(&payloadBuf)
	atomic.StoreInt64(&e.isCompressed, 0)
}

var payloadReleaser = func(queryHeaders *[][2][]byte, responseHeaders *[][2][]byte) {
	*queryHeaders = (*queryHeaders)[:0]
	pools.KeyValueSlicePool.Put(queryHeaders)
	*responseHeaders = (*responseHeaders)[:0]
	pools.KeyValueSlicePool.Put(responseHeaders)
}

// Payload decompresses the entire payload and unpacks it into fields.
func (e *Entry) Payload() (
	path []byte,
	query []byte,
	queryHeaders *[][2][]byte,
	responseHeaders *[][2][]byte,
	body []byte,
	status int,
	releaseFn func(q, h *[][2][]byte),
	err error,
) {
	payload := e.PayloadBytes()
	if len(payload) == 0 {
		return nil, nil, nil, nil, nil, 0, emptyReleaser, fmt.Errorf("payload is empty")
	}

	offset := 0

	// --- Path
	pathLen := binary.LittleEndian.Uint32(payload[offset:])
	offset += 4
	path = payload[offset : offset+int(pathLen)]
	offset += int(pathLen)

	// --- Query
	queryLen := binary.LittleEndian.Uint32(payload[offset:])
	offset += 4
	query = payload[offset : offset+int(queryLen)]
	offset += int(queryLen)

	// --- QueryHeaders
	numQueryHeaders := binary.LittleEndian.Uint32(payload[offset:])
	offset += 4
	queryHeaders = pools.KeyValueSlicePool.Get().(*[][2][]byte)
	for i := 0; i < int(numQueryHeaders); i++ {
		keyLen := binary.LittleEndian.Uint32(payload[offset:])
		offset += 4
		k := payload[offset : offset+int(keyLen)]
		offset += int(keyLen)

		valueLen := binary.LittleEndian.Uint32(payload[offset:])
		offset += 4
		v := payload[offset : offset+int(valueLen)]
		offset += int(valueLen)

		*queryHeaders = append(*queryHeaders, [2][]byte{k, v})
	}

	// --- StatusCode
	status = int(binary.LittleEndian.Uint32(payload[offset:]))
	offset += 4

	// --- Response Headers
	numHeaders := binary.LittleEndian.Uint32(payload[offset:])
	offset += 4
	responseHeaders = pools.KeyValueSlicePool.Get().(*[][2][]byte)
	for i := 0; i < int(numHeaders); i++ {
		keyLen := binary.LittleEndian.Uint32(payload[offset:])
		offset += 4
		key := payload[offset : offset+int(keyLen)]
		offset += int(keyLen)

		numVals := binary.LittleEndian.Uint32(payload[offset:])
		offset += 4
		for v := 0; v < int(numVals); v++ {
			valueLen := binary.LittleEndian.Uint32(payload[offset:])
			offset += 4
			val := payload[offset : offset+int(valueLen)]
			offset += int(valueLen)
			*responseHeaders = append(*responseHeaders, [2][]byte{key, val})
		}
	}

	// --- Body
	offset += 4
	body = payload[offset:]

	releaseFn = payloadReleaser

	return
}

func (e *Entry) Rule() *config.Rule {
	return e.rule
}

func (e *Entry) PayloadBytes() []byte {
	var payload []byte
	ptr := e.payload.Load()
	if ptr != nil {
		return *ptr
	}
	return payload
}

func (e *Entry) Weight() int64 {
	return int64(unsafe.Sizeof(*e)) + int64(cap(e.PayloadBytes()))
}

func (e *Entry) IsCompressed() bool {
	return atomic.LoadInt64(&e.isCompressed) == 1
}

func (e *Entry) UpdateAt() int64 {
	return atomic.LoadInt64(&e.updatedAt)
}

func (e *Entry) parseFilterAndSortQuery(b []byte) (queries *[][2][]byte, releaseFn func(*[][2][]byte)) {
	b = bytes.TrimLeft(b, "?")

	out := kvPool.Get().(*[][2][]byte)
	*out = (*out)[:0]

	type state struct {
		kIdx   int
		vIdx   int
		kFound bool
		vFound bool
	}

	s := state{}
	n := 0

	for idx, bt := range b {
		if bt == '&' {
			if s.kFound {
				var key, val []byte
				if s.vFound {
					key = b[s.kIdx : s.vIdx-1]
					val = b[s.vIdx:idx]
				} else {
					key = b[s.kIdx:idx]
					val = nil
				}

				if n < cap(*out) {
					if n < len(*out) {
						(*out)[n][0] = key
						(*out)[n][1] = val
					} else {
						*out = (*out)[:n+1]
						(*out)[n][0] = key
						(*out)[n][1] = val
					}
				} else {
					*out = append(*out, [2][]byte{key, val})
				}
				n++
			}
			s.kIdx = idx + 1
			s.kFound = true
			s.vIdx = 0
			s.vFound = false
		} else if bt == '=' && !s.vFound {
			s.vIdx = idx + 1
			s.vFound = true
		} else if !s.kFound {
			s.kIdx = idx
			s.kFound = true
		}
	}

	if s.kFound {
		var key, val []byte
		if s.vFound {
			key = b[s.kIdx : s.vIdx-1]
			val = b[s.vIdx:]
		} else {
			key = b[s.kIdx:]
			val = nil
		}

		if n < cap(*out) {
			if n < len(*out) {
				(*out)[n][0] = key
				(*out)[n][1] = val
			} else {
				*out = (*out)[:n+1]
				(*out)[n][0] = key
				(*out)[n][1] = val
			}
		} else {
			*out = append(*out, [2][]byte{key, val})
		}
		n++
	}

	*out = (*out)[:n]

	filtered := (*out)[:0]
	allowed := e.rule.CacheKey.QueryBytes

	for i := 0; i < n; i++ {
		kv := (*out)[i]
		keep := false
		for _, allowedKey := range allowed {
			if bytes.HasPrefix(kv[0], allowedKey) {
				keep = true
				break
			}
		}
		if keep {
			filtered = append(filtered, kv)
		}
	}

	*out = filtered

	if len(*out) > 1 {
		sort.KVSlice(*out)
	}

	return out, queriesReleaser
}

var kvPool = sync.Pool{
	New: func() interface{} {
		sl := make([][2][]byte, 0, 32)
		return &sl
	},
}

var queriesReleaser = func(queries *[][2][]byte) {
	*queries = (*queries)[:0]
	kvPool.Put(queries)
}

func (e *Entry) getFilteredAndSortedKeyQueriesFastHttp(r *fasthttp.RequestCtx) (kvPairs *[][2][]byte, releaseFn func(*[][2][]byte)) {
	out := kvPool.Get().(*[][2][]byte)
	*out = (*out)[:0]

	allowedKeys := e.rule.CacheKey.QueryBytes

	r.QueryArgs().All()(func(key, value []byte) bool {
		for _, ak := range allowedKeys {
			if bytes.HasPrefix(key, ak) {
				*out = append(*out, [2][]byte{key, value})
				break
			}
		}
		return true
	})

	if len(*out) > 1 {
		sort.KVSlice(*out)
	}

	return out, queriesReleaser
}

func (e *Entry) GetFilteredAndSortedKeyQueriesNetHttp(r *http.Request) (kvPairs *[][2][]byte, releaseFn func(*[][2][]byte)) {
	// r.URL.RawQuery - is static immutable string, therefor we can easily refer to it without any allocations.
	return e.parseFilterAndSortQuery(unsafe.Slice(unsafe.StringData(r.URL.RawQuery), len(r.URL.RawQuery)))
}

var hKvPool = sync.Pool{
	New: func() interface{} {
		sl := make([][2][]byte, 0, 32)
		return &sl
	},
}

var headersReleaser = func(headers *[][2][]byte) {
	*headers = (*headers)[:0]
	hKvPool.Put(headers)
}

func (e *Entry) getFilteredAndSortedKeyHeadersFastHttp(r *fasthttp.RequestCtx) (kvPairs *[][2][]byte, releaseFn func(*[][2][]byte)) {
	out := hKvPool.Get().(*[][2][]byte)
	*out = (*out)[:0]
	allowed := e.rule.CacheKey.HeadersMap

	n := 0
	r.Request.Header.All()(func(k, v []byte) bool {
		if _, ok := allowed[unsafe.String(unsafe.SliceData(k), len(k))]; !ok {
			return true
		}

		if n < cap(*out) {
			if n < len(*out) {
				(*out)[n][0] = k
				(*out)[n][1] = v
			} else {
				*out = (*out)[:n+1]
				(*out)[n][0] = k
				(*out)[n][1] = v
			}
		} else {
			*out = append(*out, [2][]byte{k, v})
		}
		n++

		return true
	})

	*out = (*out)[:n]
	if n > 1 {
		sort.KVSlice(*out)
	}

	return out, headersReleaser
}

func (e *Entry) GetFilteredAndSortedKeyHeadersNetHttp(r *http.Request) (kvPairs *[][2][]byte, releaseFn func(*[][2][]byte)) {
	out := hKvPool.Get().(*[][2][]byte)
	*out = (*out)[:0] // reuse

	allowed := e.rule.CacheKey.HeadersMap
	n := 0

	for k, vv := range r.Header {
		// Check if the key is allowed (string compare, avoid conversion)
		if _, ok := allowed[k]; !ok {
			continue
		}

		kb := unsafe.Slice(unsafe.StringData(k), len(k))
		for _, v := range vv {
			vb := unsafe.Slice(unsafe.StringData(v), len(v))

			if n < cap(*out) {
				if n < len(*out) {
					(*out)[n][0] = kb
					(*out)[n][1] = vb
				} else {
					*out = (*out)[:n+1]
					(*out)[n][0] = kb
					(*out)[n][1] = vb
				}
			} else {
				*out = append(*out, [2][]byte{kb, vb})
			}
			n++
		}
	}

	*out = (*out)[:n]
	if n > 1 {
		sort.KVSlice(*out)
	}

	return out, headersReleaser
}

// filteredAndSortedKeyQueriesInPlace - filters an input slice, be careful!
func (e *Entry) filteredAndSortedKeyQueriesInPlace(queries *[][2][]byte) *[][2][]byte {
	q := *queries
	n := 0
	allowed := e.rule.CacheKey.QueryBytes

	for i := 0; i < len(q); i++ {
		key := q[i][0]
		keep := false
		for _, ak := range allowed {
			if bytes.HasPrefix(key, ak) {
				keep = true
				break
			}
		}
		if keep {
			q[n] = q[i]
			n++
		}
	}

	if n > 1 {
		sort.KVSlice(q[:n])
	}

	*queries = q[:n]
	return queries
}

// filteredAndSortedKeyHeadersInPlace - filters an input slice, be careful!
func (e *Entry) filteredAndSortedKeyHeadersInPlace(headers *[][2][]byte) *[][2][]byte {
	h := *headers
	allowedMap := e.rule.CacheKey.HeadersMap

	// in-place write index
	n := 0
	for i := 0; i < len(h); i++ {
		key := unsafe.String(unsafe.SliceData(h[i][0]), len(h[i][0]))
		if _, ok := allowedMap[key]; ok {
			h[n] = h[i]
			n++
		}
	}

	if n > 1 {
		sort.KVSlice(h[:n])
	}

	*headers = h[:n]
	return headers
}

// SetLruListElement sets the LRU list element pointer.
func (e *Entry) SetLruListElement(el *list.Element[*Entry]) {
	e.lruListElem.Store(el)
}

// LruListElement returns the LRU list element pointer (for LRU cache management).
func (e *Entry) LruListElement() *list.Element[*Entry] {
	return e.lruListElem.Load()
}

// ShouldBeRefreshed implements probabilistic refresh logic ("beta" algorithm).
// Returns true if the entry is stale and, with a probability proportional to its staleness, should be refreshed now.
func (e *Entry) ShouldBeRefreshed(cfg *config.Cache) bool {
	if e == nil {
		return false
	}

	var (
		ttl         = cfg.Cache.Refresh.TTL.Nanoseconds()
		beta        = cfg.Cache.Refresh.Beta
		coefficient = cfg.Cache.Refresh.Coefficient
	)

	if e.rule.Refresh != nil {
		if !e.rule.Refresh.Enabled {
			return false
		}

		if e.rule.Refresh.TTL.Nanoseconds() > 0 {
			ttl = e.rule.Refresh.TTL.Nanoseconds()
		}
		if e.rule.Refresh.Beta > 0 {
			beta = e.rule.Refresh.Beta
		}
		if e.rule.Refresh.Coefficient > 0 {
			coefficient = e.rule.Refresh.Coefficient
		}
	}

	// время, прошедшее с последнего обновления
	elapsed := time.Now().UnixNano() - atomic.LoadInt64(&e.updatedAt)
	minStale := int64(float64(ttl) * coefficient)

	if minStale > elapsed {
		return false
	}

	// нормируем x = elapsed / ttl в [0,1]
	x := float64(elapsed) / float64(ttl)
	if x < 0 {
		x = 0
	} else if x > 1 {
		x = 1
	}

	// вероятность экспоненциального распределения
	prob := 1 - math.Exp(-beta*x)
	return rand.Float64() < prob
}

var invalidUpstreamStatusCodeReceivedError = errors.New("invalid upstream status code")

// Revalidate calls the revalidator closure to fetch fresh data and updates the timestamp.
func (e *Entry) Revalidate() error {
	path, query, headers, respHeaders, _, _, release, err := e.Payload()
	defer release(headers, respHeaders)
	if err != nil {
		return err
	}

	statusCode, respHeaders, body, releaser, err := e.revalidator(e.rule, path, query, headers)
	defer releaser()
	if err != nil {
		return err
	}
	if statusCode != http.StatusOK {
		return invalidUpstreamStatusCodeReceivedError
	}

	e.SetPayload(path, query, headers, respHeaders, body, statusCode)

	// successful refresh, set up current timestamp as last update point
	atomic.StoreInt64(&e.updatedAt, time.Now().UnixNano())

	return nil
}

func (e *Entry) ToBytes() (data []byte, releaseFn func()) {
	var scratch8 [8]byte
	var scratch4 [4]byte

	payload := e.PayloadBytes()
	rulePath := e.Rule().PathBytes

	// Забираем buffer из пула и очищаем
	buf := bufPool.Get().(*bytes.Buffer)
	releaseFn = func() {
		buf.Reset()
		bufPool.Put(buf)
	}

	// === RulePath ===
	binary.LittleEndian.PutUint32(scratch4[:], uint32(len(rulePath)))
	buf.Write(scratch4[:])
	buf.Write(rulePath)

	// === RuleKey ===
	binary.LittleEndian.PutUint64(scratch8[:], e.key)
	buf.Write(scratch8[:])

	// === Shard ===
	binary.LittleEndian.PutUint64(scratch8[:], e.shard)
	buf.Write(scratch8[:])

	// === Fingerprint ===
	buf.Write(e.fingerprint[:])

	// === IsCompressed ===
	if e.IsCompressed() {
		buf.WriteByte(1)
	} else {
		buf.WriteByte(0)
	}

	// === UpdateAt ===
	binary.LittleEndian.PutUint64(scratch8[:], uint64(e.UpdateAt()))
	buf.Write(scratch8[:])

	// === Payload ===
	binary.LittleEndian.PutUint32(scratch4[:], uint32(len(payload)))
	buf.Write(scratch4[:])
	buf.Write(payload)

	// Возвращаем готовый []byte и release
	return buf.Bytes(), releaseFn
}

func EntryFromBytes(data []byte, cfg *config.Cache, backend repository.Backender) (*Entry, error) {
	var offset int

	// RulePath
	rulePathLen := binary.LittleEndian.Uint32(data[offset:])
	offset += 4
	rulePath := data[offset : offset+int(rulePathLen)]
	offset += int(rulePathLen)

	rule := MatchRule(cfg, rulePath)
	if rule == nil {
		return nil, fmt.Errorf("rule not found for path: '%s'", string(rulePath))
	}

	// RuleKey
	key := binary.LittleEndian.Uint64(data[offset:])
	offset += 8

	// Shard
	shard := binary.LittleEndian.Uint64(data[offset:])
	offset += 8

	// Fingerprint
	var fp [16]byte
	copy(fp[:], data[offset:offset+16])
	offset += 16

	// IsCompressed
	compressed := data[offset] == 1
	var isCompressed int64
	if compressed {
		isCompressed = 1
	}
	offset += 1

	// UpdateAt
	updatedAt := int64(binary.LittleEndian.Uint64(data[offset:]))
	offset += 8

	// Payload
	payloadLen := binary.LittleEndian.Uint32(data[offset:])
	offset += 4
	payload := data[offset : offset+int(payloadLen)]

	return NewEntryFromField(
		key, shard, fp, payload, rule,
		backend.RevalidatorMaker(), isCompressed, updatedAt,
	), nil
}

func MatchRule(cfg *config.Cache, path []byte) *config.Rule {
	if rule, ok := cfg.Cache.Rules[unsafe.String(unsafe.SliceData(path), len(path))]; ok {
		return rule
	}
	return nil
}

func MatchRuleStr(cfg *config.Cache, path string) *config.Rule {
	if rule, ok := cfg.Cache.Rules[path]; ok {
		return rule
	}
	return nil
}

// SetMapKey is really dangerous - must be used exclusively in tests.
func (e *Entry) SetMapKey(key uint64) *Entry {
	e.key = key
	return e
}

func (e *Entry) DumpPayload() {
	path, query, queryHeaders, responseHeaders, body, status, releaseFn, err := e.Payload()
	defer releaseFn(queryHeaders, responseHeaders)
	if err != nil {
		log.Error().Err(err).Msg("[dump] failed to unpack payload")
		return
	}

	fmt.Printf("\n========== DUMP PAYLOAD ==========\n")
	fmt.Printf("RuleKey:          %d\n", e.key)
	fmt.Printf("Shard:        %d\n", e.shard)
	fmt.Printf("IsCompressed: %v\n", e.IsCompressed())
	fmt.Printf("UpdateAt:	 %s\n", time.Unix(0, e.UpdateAt()).Format(time.RFC3339Nano))
	fmt.Printf("----------------------------------\n")

	fmt.Printf("Path:   	   %q\n", string(path))
	fmt.Printf("Query:  	   %q\n", string(query))
	fmt.Printf("StatusCode: %d\n", status)

	fmt.Printf("\nQuery Headers:\n")
	if len(*queryHeaders) == 0 {
		fmt.Println("  (none)")
	} else {
		for i, kv := range *queryHeaders {
			fmt.Printf("  [%02d] %q : %q\n", i, kv[0], kv[1])
		}
	}

	fmt.Printf("\nResponse Headers:\n")
	if len(*responseHeaders) == 0 {
		fmt.Println("  (none)")
	} else {
		for i, kv := range *responseHeaders {
			fmt.Printf("  [%02d] %q : %q\n", i, kv[0], kv[1])
		}
	}

	fmt.Printf("\nBody (%d bytes):\n", len(body))
	if len(body) > 0 {
		const maxLen = 500
		if len(body) > maxLen {
			fmt.Printf("  %q ... [truncated, total %d bytes]\n", body[:maxLen], len(body))
		} else {
			fmt.Printf("  %q\n", body)
		}
	}

	fmt.Println("==================================")
}
