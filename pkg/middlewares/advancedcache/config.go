package advancedcachemiddleware

import (
	"github.com/traefik/traefik/v3/pkg/advancedcache/config"
)

func (m *AdvancedCacheMiddleware) loadConfig(cfg *config.TraefikIntermediateConfig) (*config.Cache, error) {
	return config.LoadConfig(cfg.ConfigPath)
}
