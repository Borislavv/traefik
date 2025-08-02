package router

import (
	"context"
	"github.com/rs/zerolog/log"
	"github.com/traefik/traefik/v3/pkg/advancedcache/utils"
	"github.com/traefik/traefik/v3/pkg/middlewares/advancedcache/counter"
	"net/http"
	"time"
)

type Router struct {
	ctx         context.Context
	routing     map[string]Route
	upstream    Upstream
	errored     http.Handler
	notEnabled  http.Handler
	unavailable http.Handler
	errorsCh    chan error
}

func NewRouter(ctx context.Context, upstream Upstream, routes ...Route) *Router {
	routing := make(map[string]Route, len(routes)*4)
	for _, route := range routes {
		for _, path := range route.Paths() {
			routing[path] = route
		}
	}
	router := &Router{
		ctx:         ctx,
		routing:     routing,
		upstream:    upstream,
		errorsCh:    make(chan error, 2048),
		errored:     NewRouteInternalError(),
		notEnabled:  NewRouteNotEnabled(),
		unavailable: NewUnavailableRoute(),
	}

	router.runErrorLogger()

	return router
}

func (router *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var from = time.Now()
	defer func() { counter.Duration.Add(time.Since(from).Nanoseconds()) }()
	counter.Total.Add(1)

	defer func() {
		if err := recover(); err != nil {
			counter.Panics.Add(1)
			log.Panic().Msgf("Recovered from panic: %v\n", err)
			router.unavailable.ServeHTTP(w, r)
			return
		}
	}()

	if route, ok := router.routing[r.URL.Path]; ok {
		if !route.IsEnabled() {
			router.notEnabled.ServeHTTP(w, r)
			return
		}

		if err := route.ServeHTTP(w, r); err != nil {
			router.errorsCh <- err
			counter.Errors.Add(1)
			if route.IsInternal() {
				return // error: respond error from internal route
			} else {
				// error: otherwise fallback to upstream
			}
		} else {
			return // success: respond with route response
		}
	}

	if router.upstream.IsEnabled() {
		if err := router.upstream.ServeHTTP(w, r); err != nil {
			router.errorsCh <- err
			counter.Errors.Add(1)
			// error: respond that server is unavailable
		} else {
			return // success: respond with upstream response
		}
	}

	router.unavailable.ServeHTTP(w, r)
	return
}

func (router *Router) runErrorLogger() {
	go func() {
		var prev map[string]int
		dedupMap := make(map[string]int, 2048)
		each5Secs := utils.NewTicker(router.ctx, time.Second*5)

		writeTrigger := make(chan struct{}, 1)
		go func() {
			for range writeTrigger {
				for err, count := range prev {
					log.Error().Msgf("[error-logger] %s (count=%d)", err, count)
				}
			}
		}()

		for {
			select {
			case <-router.ctx.Done():
				return
			case err := <-router.errorsCh:
				dedupMap[err.Error()]++
			case <-each5Secs:
				prev = dedupMap
				dedupMap = make(map[string]int, len(prev))
				writeTrigger <- struct{}{}
			}
		}
	}()
}
