package lru

import (
	"context"
	"github.com/rs/zerolog/log"
	"github.com/traefik/traefik/v3/pkg/advancedcache/config"
	"runtime"
	"time"

	"github.com/traefik/traefik/v3/pkg/advancedcache/utils"
)

const fatShardsPercent = 0.17

var evictionStatCh = make(chan EvictionStat, runtime.GOMAXPROCS(0)*4)

// EvictionStat carries statistics for each eviction batch.
type EvictionStat struct {
	items    int   // Number of evicted items
	freedMem int64 // Total freed Weight
}

type Evictor interface {
	run()
}

type Evict struct {
	ctx                 context.Context
	cfg                 *config.Cache
	db                  *InMemoryStorage
	balancer            *Balance
	memoryThreshold     int64
	fatShardsPercentage int
}

func NewEvictor(ctx context.Context, cfg *config.Cache, db *InMemoryStorage, balancer *Balance) *Evict {
	return &Evict{
		ctx:                 ctx,
		cfg:                 cfg,
		db:                  db,
		balancer:            balancer,
		fatShardsPercentage: int(float64(cfg.Cache.Preallocate.Shards) * fatShardsPercent),
		memoryThreshold:     int64(float64(cfg.Cache.Storage.Size) * cfg.Cache.Eviction.Threshold),
	}
}

// Run is the main background eviction loop for one worker.
// Each worker tries to bring Weight usage under the threshold by evicting from most loaded shards.
func (e *Evict) Run() *Evict {
	if e.cfg.Cache.Enabled && e.cfg.Cache.Eviction.Enabled {
		e.runLogger()
		e.runEvictor()
	}
	return e
}

func (e *Evict) runEvictor() {
	go func() {
		t := utils.NewTicker(e.ctx, time.Millisecond*500)
		for {
			select {
			case <-e.ctx.Done():
				return
			case <-t:
				items, freedMem := e.evictUntilWithinLimit()
				if items > 0 || freedMem > 0 {
					select {
					case <-e.ctx.Done():
						return
					case evictionStatCh <- EvictionStat{items: items, freedMem: freedMem}:
					}
				}
			}
		}
	}()
}

// ShouldEvict [HOT PATH METHOD] (max stale value = 25ms) checks if current Weight usage has reached or exceeded the threshold.
func (e *Evict) ShouldEvict() bool {
	return e.db.Mem() >= e.memoryThreshold
}

// shouldEvictRightNow (returns a honest memory usage) checks if current Weight usage has reached or exceeded the threshold.
func (e *Evict) shouldEvictRightNow() bool {
	return e.db.RealMem() >= e.memoryThreshold
}

// evictUntilWithinLimit repeatedly removes entries from the most loaded Shard (tail of InMemoryStorage)
// until Weight drops below threshold or no more can be evicted.
func (e *Evict) evictUntilWithinLimit() (items int, mem int64) {
	shardOffset := 0
	for e.shouldEvictRightNow() {
		shardOffset++
		if shardOffset >= e.fatShardsPercentage {
			e.balancer.Rebalance()
			shardOffset = 0
		}

		shard, found := e.balancer.MostLoaded(shardOffset)
		if !found {
			continue
		}

		if shard.LruList().Len() == 0 {
			continue
		}

		offset := 0
		evictions := 0
		for e.shouldEvictRightNow() {
			el, ok := shard.LruList().Next(offset)
			if !ok {
				break // end of the LRU list, move to next
			}

			freedMem, hit := e.db.Remove(el.Value())
			if hit {
				items++
				evictions++
				mem += freedMem
			}

			offset++
		}
	}
	return
}

// runLogger emits detailed stats about evictions, Weight, and GC activity every 5 seconds if debugging is enabled.
func (e *Evict) runLogger() {
	go func() {
		var (
			evictsNumPer5Sec int
			evictsMemPer5Sec int64
			ticker           = utils.NewTicker(e.ctx, 5*time.Second)
		)
	loop:
		for {
			select {
			case <-e.ctx.Done():
				return
			case stat := <-evictionStatCh:
				evictsNumPer5Sec += stat.items
				evictsMemPer5Sec += stat.freedMem
			case <-ticker:
				if evictsNumPer5Sec <= 0 && evictsMemPer5Sec <= 0 {
					continue loop
				}

				log.Info().
					Str("target", "eviction").
					Int64("freedMemBytes", evictsMemPer5Sec).
					Int("freedItems", evictsNumPer5Sec).
					Msg("[eviction][5s]")

				evictsNumPer5Sec = 0
				evictsMemPer5Sec = 0
			}
		}
	}()
}
