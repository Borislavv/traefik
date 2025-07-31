package router

import (
	"github.com/rs/zerolog/log"
	httpwriter "github.com/traefik/traefik/v3/pkg/advancedcache/writer"
	"github.com/traefik/traefik/v3/pkg/middlewares/advancedcache/counter"
	"net/http"
	"time"
)

type Router struct {
	routing     map[string]Route
	upstream    Upstream
	errored     http.Handler
	notEnabled  http.Handler
	unavailable http.Handler
}

func NewRouter(upstream Upstream, routes ...Route) *Router {
	routing := make(map[string]Route, len(routes)*4)
	for _, route := range routes {
		for _, path := range route.Paths() {
			routing[path] = route
		}
	}
	return &Router{
		routing:     routing,
		upstream:    upstream,
		errored:     NewRouteInternalError(),
		notEnabled:  NewRouteNotEnabled(),
		unavailable: NewUnavailableRoute(),
	}
}

func (router *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var from = time.Now()
	defer func() { counter.Duration.Add(time.Since(from).Nanoseconds()) }()
	counter.Total.Add(1)

	captured, releaser := httpwriter.NewCaptureResponseWriter(w)
	defer releaser()

	defer func() {
		if err := recover(); err != nil {
			counter.Panics.Add(1)
			log.Panic().Msgf("Recovered from panic: %v\n", err)
			captured.Reset()
			router.unavailable.ServeHTTP(captured, r)
			captured.Copy(w)
			return
		}
	}()

	if route, ok := router.routing[r.URL.Path]; ok {
		if !route.IsEnabled() {
			router.notEnabled.ServeHTTP(w, r)
			return
		}

		if err := route.ServeHTTP(w, r); err != nil {
			counter.Errors.Add(1)
			if route.IsInternal() {
				return // error: respond error from internal route
			}
			// error: fallback to upstream
		} else {
			return // success: respond with route response
		}
	}

	if router.upstream.IsEnabled() {
		if err := router.upstream.ServeHTTP(w, r); err != nil {
			counter.Errors.Add(1)
			// error: responed that server is unavailable
		} else {
			return // success: respond with upstream response
		}
	}

	router.unavailable.ServeHTTP(w, r)
	return
}
