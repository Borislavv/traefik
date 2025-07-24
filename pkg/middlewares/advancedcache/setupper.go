package advancedcachemiddleware

import (
	"github.com/traefik/traefik/v3/pkg/advancedcache/model"
	"github.com/traefik/traefik/v3/pkg/advancedcache/prometheus/metrics"
	"github.com/traefik/traefik/v3/pkg/advancedcache/repository"
	"github.com/traefik/traefik/v3/pkg/advancedcache/storage"
	"github.com/traefik/traefik/v3/pkg/advancedcache/storage/lfu"
	"github.com/traefik/traefik/v3/pkg/advancedcache/storage/lru"
	sharded "github.com/traefik/traefik/v3/pkg/advancedcache/storage/map"
)

func (m *AdvancedCacheMiddleware) setUpCache() {
	shardedMap := sharded.NewMap[*model.VersionPointer](m.ctx, m.cfg.Cache.Preallocate.PerShard)
	m.backend = repository.NewBackend(m.ctx, m.cfg)
	balancer := lru.NewBalancer(m.ctx, shardedMap)
	tinyLFU := lfu.NewTinyLFU(m.ctx)
	m.storage = lru.NewStorage(m.ctx, m.cfg, balancer, m.backend, tinyLFU, shardedMap)
	m.refresher = storage.NewRefresher(m.ctx, m.cfg, balancer, m.storage)
	m.dumper = storage.NewDumper(m.cfg, shardedMap, m.storage, m.backend)
	m.evictor = storage.NewEvictor(m.ctx, m.cfg, m.storage, balancer)
	m.metrics = metrics.New()
}
