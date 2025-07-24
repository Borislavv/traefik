package header

import (
	"github.com/traefik/traefik/v3/pkg/advancedcache/model"
	"github.com/valyala/fasthttp"
	"net/http"
	"sync"
	"time"
)

var (
	lastModifiedStrKey   = "Last-Modified"
	lastModifiedBytesKey = []byte(lastModifiedStrKey)
)

var (
	timePool = sync.Pool{
		New: func() any {
			return new(time.Time)
		},
	}
	bufPool = sync.Pool{
		New: func() interface{} {
			sl := make([]byte, 0, 32)
			return &sl
		},
	}
)

func SetLastModifiedNetHttp(w http.ResponseWriter, entry *model.VersionPointer) {
	SetLastModifiedValueNetHttp(w, entry.UpdateAt())
}

func SetLastModifiedValueNetHttp(w http.ResponseWriter, v int64) {
	buf := bufPool.Get().(*[]byte)
	*buf = (*buf)[:0]

	*buf = appendLastModifiedHeader(buf, v)
	w.Header().Set(lastModifiedStrKey, string(*buf))

	bufPool.Put(buf)
}

func SetLastModifiedFastHttp(r *fasthttp.RequestCtx, entry *model.VersionPointer) {
	SetLastModifiedValueFastHttp(r, entry.UpdateAt())
}

func SetLastModifiedValueFastHttp(r *fasthttp.RequestCtx, v int64) {
	buf := bufPool.Get().(*[]byte)
	*buf = (*buf)[:0]

	*buf = appendLastModifiedHeader(buf, v)
	r.Response.Header.SetBytesKV(lastModifiedBytesKey, *buf)

	bufPool.Put(buf)
}

func appendLastModifiedHeader(dst *[]byte, unixNano int64) []byte {
	t := timePool.Get().(*time.Time)
	defer timePool.Put(t)

	*t = time.Unix(0, unixNano).UTC() // must be UTC per RFC 7231

	return t.AppendFormat(*dst, time.RFC1123)
}
