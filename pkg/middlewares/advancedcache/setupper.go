package middleware

import (
	"context"
	"github.com/rs/zerolog/log"
	"github.com/traefik/traefik/v3/pkg/advancedcache/config"
	"github.com/traefik/traefik/v3/pkg/advancedcache/prometheus/metrics"
	"github.com/traefik/traefik/v3/pkg/advancedcache/repository"
	"github.com/traefik/traefik/v3/pkg/advancedcache/storage"
	"github.com/traefik/traefik/v3/pkg/advancedcache/storage/lru"
)

var (
	cacheCfg    *config.Cache
	cacheDumper storage.Dumper
)

func (m *TraefikCacheMiddleware) setUpCache() {
	enabled.Store(m.cfg.Cache.Enabled)

	cacheCfg = m.cfg
	m.metrics = metrics.New()
	m.backend = repository.NewBackend(m.ctx, m.cfg)
	m.storage = lru.NewStorage(m.ctx, m.cfg, m.backend)
	cacheDumper = storage.NewDumper(m.cfg, m.storage, m.backend)

	LoadDumpIfNecessary(m.ctx)
	m.LoadMocksIfNecessary()
}

func (m *TraefikCacheMiddleware) LoadMocksIfNecessary() {
	if cacheCfg.Cache.Persistence.Mock.Enabled {
		storage.LoadMocks(m.ctx, m.cfg, m.backend, m.storage, m.cfg.Cache.Persistence.Mock.Length)
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
