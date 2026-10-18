// Package cache defines the cache PORT (interface) and its adapters.
//
// ============================================================================
// WHY AN INTERFACE AND NOT "just call Redis"
// ============================================================================
// The service layer says `cache.Get(ctx, key)`. It does not know or care
// whether that is Redis, an in-process map, Memcached, or a no-op. That is the
// Dependency Inversion Principle, and it buys us three concrete things:
//
//  1. Tests run in milliseconds with no Docker (swap in MemoryCache).
//  2. You can read the whole system without infrastructure installed.
//  3. If Redis is down, we degrade to "no cache" instead of 500-ing.
//
// ============================================================================
// THE FIVE CACHE PATTERNS — AND WHY WE CHOSE CACHE-ASIDE
// ============================================================================
//
//  1. CACHE-ASIDE (lazy loading)   <-- WE USE THIS
//     Read:  try cache -> on miss read DB -> populate cache -> return.
//     Write: write DB -> DELETE the cache key.
//     + Only caches what is actually requested (memory-efficient).
//     + Cache failure is survivable: you just hit the DB.
//     - First request after a write is always slow (a cold miss).
//
//  2. READ-THROUGH
//     The cache library itself loads from the DB on a miss.
//     + Application code is simpler.
//     - Needs a cache that supports loaders; couples cache to schema.
//
//  3. WRITE-THROUGH
//     Write to cache AND DB synchronously, in that order.
//     + Cache is never stale.
//     - Every write pays cache latency; you cache data nobody reads.
//
//  4. WRITE-BEHIND (write-back)
//     Write to cache, flush to DB asynchronously.
//     + Fastest writes; absorbs bursts.
//     - YOU CAN LOSE COMMITTED DATA if the cache dies before the flush.
//     Never acceptable for orders or payments. Fine for view counters.
//
//  5. REFRESH-AHEAD
//     Proactively refresh hot keys shortly before they expire.
//     + Hides miss latency from users entirely.
//     - Wastes work on keys that would not have been requested again.
//
// WHY CACHE-ASIDE + DELETE-ON-WRITE (rather than update-on-write):
// If two concurrent writes both UPDATE the cache, they can land in the
// opposite order to how they landed in the database, leaving the cache
// permanently wrong. DELETING is idempotent and self-correcting — the next
// read repopulates from the single source of truth. "Invalidate, don't
// update" is the safe default.
//
// See docs/CACHING.md for invalidation vs eviction in full.
// ============================================================================
package cache

import (
	"context"
	"errors"
	"time"
)

// ErrMiss is returned when a key is absent. It is a sentinel error, not an
// (value, bool) pair, because Redis adapters already return errors and this
// keeps one uniform signature.
var ErrMiss = errors.New("cache: miss")

// Cache is the port every adapter implements.
//
// NOTE ON THE SIGNATURE: we store []byte, not `any`. Storing `any` would mean
// the in-memory cache hands back a POINTER TO THE SAME STRUCT to every
// caller — so one request mutating its "copy" silently corrupts the cached
// value for everyone else. Forcing serialisation to bytes makes the cache
// behave identically whether it is local or remote. That bug class is a
// classic, and this signature designs it out of existence.
type Cache interface {
	Get(ctx context.Context, key string) ([]byte, error)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	Delete(ctx context.Context, keys ...string) error

	// Incr atomically increments a counter and returns the new value. This is
	// what powers generation-based group invalidation (see constants
	// CacheKeyListGeneration): one INCR invalidates thousands of list keys.
	Incr(ctx context.Context, key string) (int64, error)

	// Ping is used by /readyz to decide if we can serve traffic.
	Ping(ctx context.Context) error

	// Stats exposes hit/miss/eviction counters for /metrics and for proving
	// in an interview that the cache is actually working.
	Stats() Stats

	Close() error
}

// Stats is a snapshot of cache effectiveness.
type Stats struct {
	Hits      uint64
	Misses    uint64
	Sets      uint64
	Deletes   uint64
	Evictions uint64 // only meaningful for a size-bounded cache
	Expired   uint64 // removed because TTL elapsed
	Entries   int
}

// HitRatio is the number that actually matters operationally.
//
// INTERPRETING IT: below ~0.8 for a read-heavy workload usually means your
// TTL is too short, your keys are too granular, or you are invalidating too
// aggressively. A ratio of 1.0 with stale complaints means the opposite.
func (s Stats) HitRatio() float64 {
	total := s.Hits + s.Misses
	if total == 0 {
		return 0
	}
	return float64(s.Hits) / float64(total)
}
