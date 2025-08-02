package router

import (
	"net/http"
)

type Upstream = Route
type CacheProxy = Upstream

type Route interface {
	IsEnabled() bool
	IsInternal() bool
	Paths() []string
	ServeHTTP(http.ResponseWriter, *http.Request) error
}
