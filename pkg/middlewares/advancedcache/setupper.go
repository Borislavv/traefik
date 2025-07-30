package middleware

import (
	"github.com/rs/zerolog/log"
	"github.com/traefik/traefik/v3/pkg/advancedcache/prometheus/metrics"
	"github.com/traefik/traefik/v3/pkg/advancedcache/repository"
	"github.com/traefik/traefik/v3/pkg/advancedcache/storage"
	"github.com/traefik/traefik/v3/pkg/advancedcache/storage/lru"
)

var Dumper storage.Dumper

func (m *TraefikCacheMiddleware) setUpCache() {
	enabled.Store(m.cfg.Cache.Enabled)

	Dumper = storage.NewDumper(m.cfg, m.storage, m.backend)
	if err := Dumper.Load(m.ctx); err != nil {
		log.Error().Err(err).Msg("[dump] failed to load cache dump")
	}

	m.metrics = metrics.New()
	m.backend = repository.NewBackend(m.ctx, m.cfg)
	m.storage = lru.NewStorage(m.ctx, m.cfg, m.backend)
}
