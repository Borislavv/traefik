package sharded

import (
	"context"
	"github.com/rs/zerolog/log"
	"github.com/traefik/traefik/v3/pkg/advancedcache/types"
	"github.com/traefik/traefik/v3/pkg/advancedcache/utils"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"
)

const NumOfShards uint64 = 2049  // 2048 total shards (one for collisions)
const ActiveShards uint64 = 2047 // 2047 active shards

// Value must implement all cache entry interfaces: keying, sizing, and releasability.
type Value interface {
	types.Keyed
	types.Sized
}

// Map is a sharded concurrent map for high-performance caches.
type Map[V Value] struct {
	ctx    context.Context
	len    int64
	mem    int64
	shards [NumOfShards]*Shard[V]
}

// NewMap creates a new sharded map with preallocated shards and a default per-shard map capacity.
func NewMap[V Value](ctx context.Context, defaultLen int) *Map[V] {
	m := &Map[V]{ctx: ctx}
	for id := uint64(0); id < NumOfShards; id++ {
		m.shards[id] = NewShard[V](id, defaultLen)
	}
	m.runMemRefresher()
	return m
}

// MapShardKey calculates the shard index for a given key.
func MapShardKey(key uint64) uint64 {
	// rewrite 0 idx to the last shard
	if k := key % ActiveShards; k == 0 {
		return ActiveShards + 1
	} else {
		return k
	}
}

// Set inserts or updates a value in the correct shard. Returns a releaser for ref counting.
func (smap *Map[V]) Set(key uint64, value V) {
	smap.Shard(key).Set(key, value)
}

// Get fetches a value and its releaser from the correct shard.
// found==false means the value is absent.
func (smap *Map[V]) Get(key uint64) (value V, ok bool) {
	return smap.Shard(key).Get(key)
}

func (smap *Map[V]) Rnd() (value V, ok bool) {
	return smap.shards[uint64(rand.Intn(int(ActiveShards)))].GetRand()
}

// Remove deletes a value by key, returning how much memory was freed and a pointer to its LRU/list element.
func (smap *Map[V]) Remove(key uint64) (freedBytes int64, hit bool) {
	return smap.Shard(key).Remove(key)
}

// Walk applies fn to all key/value pairs in the shard, optionally locking for writing.
func (shard *Shard[V]) Walk(ctx context.Context, fn func(uint64, V) bool, lockRead bool) {
	if lockRead {
		shard.Lock()
		defer shard.Unlock()
	} else {
		shard.RLock()
		defer shard.RUnlock()
	}
	for k, v := range shard.items {
		select {
		case <-ctx.Done():
			return
		default:
			ok := fn(k, v)
			if !ok {
				return
			}
		}
	}
}

// Shard returns the shard that stores the given key.
func (smap *Map[V]) Shard(key uint64) *Shard[V] {
	return smap.shards[MapShardKey(key)]
}

// WalkShards launches fn concurrently for each shard (key, *Shard[V]).
// The callback runs in a separate goroutine for each shard; fn should be goroutine-safe.
func (smap *Map[V]) WalkShards(ctx context.Context, fn func(key uint64, shard *Shard[V])) {
	var wg sync.WaitGroup
	wg.Add(int(NumOfShards))
	defer wg.Wait()
	for k, s := range smap.shards {
		select {
		case <-ctx.Done():
			return
		default:
			go func(key uint64, shard *Shard[V]) {
				defer wg.Done()
				fn(key, shard)
			}(uint64(k), s)
		}
	}
}

// Len returns the total number of elements in all shards (O(NumOfShards)).
func (smap *Map[V]) Len() int64 {
	return atomic.LoadInt64(&smap.len)
}

func (smap *Map[V]) RealLen() int64 {
	length := int64(0)
	for _, shard := range smap.shards {
		length += shard.Len()
	}
	atomic.StoreInt64(&smap.len, length)
	return length
}

func (smap *Map[V]) Mem() int64 {
	return atomic.LoadInt64(&smap.mem)
}

func (smap *Map[V]) RealMem() int64 {
	mem := int64(0)
	for _, shard := range smap.shards {
		mem += shard.Weight()
	}
	atomic.StoreInt64(&smap.mem, mem)
	return mem
}

func (smap *Map[V]) runMemRefresher() {
	go func() {
		log.Info().Msg("[storage] memory refresher has been launched (refresh each 100ms)")
		t := utils.NewTicker(smap.ctx, time.Millisecond*100)
		for {
			select {
			case <-smap.ctx.Done():
				log.Info().Msg("[storage] memory refresher has been closed")
				return
			case <-t:
				mem := int64(0)
				length := int64(0)
				for _, shard := range smap.shards {
					mem += shard.Weight()
					length += shard.Len()
				}
				atomic.StoreInt64(&smap.mem, mem)
				atomic.StoreInt64(&smap.len, length)
			}
		}
	}()
}
