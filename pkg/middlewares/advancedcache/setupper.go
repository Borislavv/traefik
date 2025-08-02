package middleware

import (
	"context"
	"github.com/rs/zerolog/log"
	"github.com/traefik/traefik/v3/pkg/advancedcache/config"
	"github.com/traefik/traefik/v3/pkg/advancedcache/prometheus/metrics"
	"github.com/traefik/traefik/v3/pkg/advancedcache/repository"
	"github.com/traefik/traefik/v3/pkg/advancedcache/storage"
	"github.com/traefik/traefik/v3/pkg/advancedcache/storage/lru"
	"github.com/traefik/traefik/v3/pkg/middlewares/advancedcache/route"
	"github.com/traefik/traefik/v3/pkg/middlewares/advancedcache/router"
)

var (
	cacheCfg    *config.Cache
	cacheDumper storage.Dumper
)

func (m *TraefikCacheMiddleware) setUpCache() {
	ctx := m.ctx
	cacheCfg = m.cfg

	// build dependencies
	meter := metrics.New()
	backend := repository.NewBackend(ctx, cacheCfg)
	db := lru.NewStorage(ctx, cacheCfg, backend)
	cacheDumper = storage.NewDumper(cacheCfg, db, backend)

	m.router = router.NewRouter(ctx,
		route.NewUpstream(backend),
		route.NewCacheRoutes(cacheCfg, db, backend),
		route.NewClearRoute(cacheCfg, db),
		route.NewK8sProbeRoute(),
		route.NewEnableRoute(),
		route.NewDisableRoute(),
		route.NewMetricsRoute(),
	)

	// run additional workers
	NewMetricsLogger(ctx, cacheCfg, db, meter).run()

	// load data if necessary
	LoadDumpIfNecessary(ctx)
	m.loadMocksIfNecessary(ctx, backend, db)

	// tell everyone that cache is enabled
	route.EnableCache()
}

func (m *TraefikCacheMiddleware) loadMocksIfNecessary(ctx context.Context, backend repository.Backender, db storage.Storage) {
	if cacheCfg.Cache.Persistence.Mock.Enabled {
		storage.LoadMocks(ctx, cacheCfg, backend, db, cacheCfg.Cache.Persistence.Mock.Length)
	}
}

func LoadDumpIfNecessary(ctx context.Context) {
	if cacheCfg.Cache.Enabled && cacheCfg.Cache.Persistence.Dump.IsEnabled {
		if err := cacheDumper.Load(ctx); err != nil {
			log.Error().Err(err).Msg("[dump] failed to load cache dump")
		}
	}
}

func StoreDumpIfNecessary(ctx context.Context) {
	if cacheCfg.Cache.Enabled && cacheCfg.Cache.Persistence.Dump.IsEnabled {
		if err := cacheDumper.Dump(ctx); err != nil {
			log.Error().Err(err).Msg("[dump] failed to store cache dump")
		}
	}
}
