package advancedcachemiddleware

import (
	"context"
	"github.com/rs/zerolog/log"
	"github.com/traefik/traefik/v3/pkg/advancedcache/config"
	"github.com/traefik/traefik/v3/pkg/advancedcache/gc"
)

func (m *AdvancedCacheMiddleware) run(ctx context.Context, config *config.TraefikIntermediateConfig) error {
	log.Info().Msg("[advanced-cache] starting")

	if cfg, err := m.loadConfig(config); err != nil {
		log.Error().Err(err).Msg("[advanced-cache] failed to config")
		return err
	} else {
		m.cfg = cfg
	}

	m.ctx = ctx

	m.setUpCache()

	if err := m.loadDump(); err != nil {
		log.Error().Err(err).Msg("[dump] failed to load")
	}

	enabled.Store(m.cfg.Cache.Enabled)
	m.storage.Run()
	m.evictor.Run()
	m.refresher.Run()
	m.runLoggerMetricsWriter()
	gc.Run(ctx, m.cfg)

	log.Info().Msg("[advanced-cache] has been started")

	return nil
}
