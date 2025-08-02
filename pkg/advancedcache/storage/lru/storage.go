package lru

import (
	"context"
	"github.com/traefik/traefik/v3/pkg/advancedcache/storage/lfu"
	"runtime"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/traefik/traefik/v3/pkg/advancedcache/config"
	"github.com/traefik/traefik/v3/pkg/advancedcache/model"
	"github.com/traefik/traefik/v3/pkg/advancedcache/repository"
	sharded "github.com/traefik/traefik/v3/pkg/advancedcache/storage/map"
	"github.com/traefik/traefik/v3/pkg/advancedcache/utils"
)

// InMemoryStorage is a Weight-aware, sharded InMemoryStorage cache with background eviction and refreshItem support.
type InMemoryStorage struct {
	ctx             context.Context            // Main context for lifecycle control
	cfg             *config.Cache              // CacheBox configuration
	shardedMap      *sharded.Map[*model.Entry] // Sharded storage for cache entries
	tinyLFU         *lfu.TinyLFU               // Helps hold more frequency used items in cache while eviction
	backend         repository.Backender       // Remote backend server.
	balancer        Balancer                   // Helps pick shards to evict from
	mem             int64                      // Current Weight usage (bytes)
	memoryThreshold int64                      // Threshold for triggering eviction (bytes)
}

// NewStorage constructs a new InMemoryStorage cache instance and launches eviction and refreshItem routines.
func NewStorage(ctx context.Context, cfg *config.Cache, backend repository.Backender) *InMemoryStorage {
	shardedMap := sharded.NewMap[*model.Entry](ctx, cfg.Cache.Preallocate.PerShard)
	balancer := NewBalancer(ctx, shardedMap)

	db := (&InMemoryStorage{
		ctx:             ctx,
		cfg:             cfg,
		shardedMap:      shardedMap,
		balancer:        balancer,
		backend:         backend,
		tinyLFU:         lfu.NewTinyLFU(ctx),
		memoryThreshold: int64(float64(cfg.Cache.Storage.Size) * cfg.Cache.Eviction.Threshold),
	}).init().runLogger()

	NewRefresher(ctx, cfg, db).Run()
	NewEvictor(ctx, cfg, db, balancer).Run()

	return db
}

func (s *InMemoryStorage) init() *InMemoryStorage {
	// Register all existing shards with the balancer.
	s.shardedMap.WalkShards(s.ctx, func(shardKey uint64, shard *sharded.Shard[*model.Entry]) {
		s.balancer.Register(shard)
	})

	return s
}

func (s *InMemoryStorage) Clear() {
	s.shardedMap.WalkShards(s.ctx, func(shardKey uint64, shard *sharded.Shard[*model.Entry]) {
		shard.Walk(s.ctx, func(key uint64, entry *model.Entry) bool {
			s.balancer.Remove(shardKey, entry.LruListElement())
			return true
		}, true)

		shard.Clear()
	})
}

// Rand returns a random item from storage.
func (s *InMemoryStorage) Rand() (entry *model.Entry, ok bool) {
	return s.shardedMap.Rnd()
}

// Get retrieves a response by request and bumps its InMemoryStorage position.
// Returns: (response, releaser, found).
func (s *InMemoryStorage) Get(req *model.Entry) (ptr *model.Entry, found bool) {
	ptr, found = s.shardedMap.Get(req.MapKey())
	if !found || !ptr.IsSameFingerprint(req.Fingerprint()) {
		return nil, false
	} else {
		s.touch(ptr)
		return ptr, true
	}
}

// Set inserts or updates a response in the cache, updating Weight usage and InMemoryStorage position.
// On 'wasPersisted=true' must be called Entry.Finalize, otherwise Entry.Finalize.
func (s *InMemoryStorage) Set(new *model.Entry) (persisted bool) {
	key := new.MapKey()

	// increase access counter of tinyLFU
	s.tinyLFU.Increment(key)

	// try to find existing entry
	if old, found := s.shardedMap.Get(key); found {
		if old.IsSameFingerprint(new.Fingerprint()) {
			// entry was found, no hash collisions, fingerprint check has passed, next check payload
			if old.IsSamePayload(new) {
				// nothing change, an existing entry has the same payload, just up the element in LRU list
				s.touch(old)
			} else {
				// payload has changes, updated it and up the element in LRU list of course
				s.update(old, new)
			}
			return true
		}
		// hash collision found, remove collision element and try to set new one
		s.Remove(old)
	}

	// check whether we are still into memory limit
	if s.ShouldEvict() { // if so then check admission by tinyLFU
		if victim, admit := s.balancer.FindVictim(new.ShardKey()); !admit || !s.tinyLFU.Admit(new, victim) {
			return false
		}
	}

	// insert a new one Entry into map
	s.shardedMap.Set(key, new)
	// insert a new one Entry LRU element into LRU list
	s.balancer.Push(new)

	return true
}

func (s *InMemoryStorage) Remove(entry *model.Entry) (freedBytes int64, hit bool) {
	s.balancer.Remove(entry.ShardKey(), entry.LruListElement())
	return s.shardedMap.Remove(entry.MapKey())
}

func (s *InMemoryStorage) Len() int64 {
	return s.shardedMap.Len()
}

func (s *InMemoryStorage) RealLen() int64 {
	return s.shardedMap.RealLen()
}

func (s *InMemoryStorage) Mem() int64 {
	return s.shardedMap.Mem() + s.balancer.Mem()
}

func (s *InMemoryStorage) RealMem() int64 {
	return s.shardedMap.RealMem()
}

func (s *InMemoryStorage) Stat() (bytes int64, length int64) {
	return s.shardedMap.Mem(), s.shardedMap.Len()
}

// ShouldEvict [HOT PATH METHOD] (max stale value = 25ms) checks if current Weight usage has reached or exceeded the threshold.
func (s *InMemoryStorage) ShouldEvict() bool {
	return s.Mem() >= s.memoryThreshold
}

func (s *InMemoryStorage) WalkShards(ctx context.Context, fn func(key uint64, shard *sharded.Shard[*model.Entry])) {
	s.shardedMap.WalkShards(ctx, fn)
}

// touch bumps the InMemoryStorage position of an existing entry (MoveToFront) and increases its refCount.
func (s *InMemoryStorage) touch(existing *model.Entry) {
	s.balancer.Update(existing)
}

// update refreshes Weight accounting and InMemoryStorage position for an updated entry.
func (s *InMemoryStorage) update(existing, new *model.Entry) {
	existing.SwapPayloads(new)
	existing.TouchUpdatedAt()
	s.balancer.Update(existing)
}

// runLogger emits detailed stats about evictions, Weight, and GC activity every 5 seconds if debugging is enabled.
func (s *InMemoryStorage) runLogger() *InMemoryStorage {
	go func() {
		var ticker = utils.NewTicker(s.ctx, 5*time.Second)

		for {
			select {
			case <-s.ctx.Done():
				return
			case <-ticker:
				var m runtime.MemStats
				runtime.ReadMemStats(&m)

				log.Info().
					Str("target", "storage").
					Int64("len", s.shardedMap.Len()).
					Str("memoryUsage", utils.FmtMem(s.shardedMap.Mem())).
					Str("memLimit", utils.FmtMem(int64(s.cfg.Cache.Storage.Size))).
					Str("allocStr", utils.FmtMem(int64(m.Alloc))).
					Int("goroutines", runtime.NumGoroutine()).
					Uint32("gc", m.NumGC).
					Msg("[storage][5s]")
			}
		}
	}()
	return s
}
