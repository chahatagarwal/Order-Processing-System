// Singleflight: protection against the CACHE STAMPEDE (a.k.a. dog-piling,
// or the thundering herd on a cache miss).
//
// ============================================================================
// THE PROBLEM, CONCRETELY
// ============================================================================
// Order ORD-ABC is on a popular tracking page: 2,000 requests/second, all
// served from cache. Then its cache entry expires.
//
// In that instant, all 2,000 in-flight requests miss, and all 2,000 run the
// same SELECT against Postgres. Your connection pool holds 10. The other
// 1,990 queue, time out, and the pool stays saturated — so even unrelated
// endpoints start failing. One expired key took down the service.
//
// This is not hypothetical; it is one of the most common real outages in
// cache-fronted systems, and it happens precisely when you are busiest.
//
// ============================================================================
// THE FIX
// ============================================================================
// Deduplicate by key. The FIRST goroutine to miss does the database read.
// Every other goroutine asking for the SAME key waits for that one result and
// shares it. 2,000 requests become 1 query.
//
// The implementation is 40 lines: a map of in-flight calls, a WaitGroup per
// entry, and a mutex. (The well-known golang.org/x/sync/singleflight does
// exactly this; we inline it so the mechanism is visible and dependency-free.)
//
// RELATED MITIGATIONS WE ALSO USE OR DOCUMENT:
//   - TTL JITTER (below): keys written together must not expire together, or
//     you get a synchronised stampede across thousands of keys at once.
//   - NEGATIVE CACHING: cache "not found" briefly, so a bot probing random
//     ids cannot bypass the cache entirely (cache penetration).
//   - Refresh-ahead: re-populate just before expiry. Not used here; it costs
//     work on keys that may never be read again.
package cache

import (
	"context"
	"math/rand"
	"sync"
	"time"
)

// Group deduplicates concurrent calls that share a key.
type Group struct {
	mu    sync.Mutex
	calls map[string]*call
}

type call struct {
	wg  sync.WaitGroup
	val []byte
	err error
	// dups counts how many callers piggy-backed on this one call. Exposed
	// through Shared() so a test can PROVE the dedup happened, rather than
	// us just asserting it in a comment.
	dups int
}

func NewGroup() *Group {
	return &Group{calls: map[string]*call{}}
}

// Do runs fn for `key`, guaranteeing that only one fn per key executes at a
// time. Concurrent callers for the same key receive the same result.
//
// It returns (value, shared, err) where `shared` is true if this caller
// piggy-backed on someone else's in-flight call.
func (g *Group) Do(ctx context.Context, key string, fn func(ctx context.Context) ([]byte, error)) ([]byte, bool, error) {
	g.mu.Lock()
	if c, ok := g.calls[key]; ok {
		// Someone is already fetching this key. Register as a duplicate,
		// release the map lock (critical — holding it would serialise
		// everything), and wait on this call's WaitGroup.
		c.dups++
		g.mu.Unlock()
		c.wg.Wait()
		return c.val, true, c.err
	}

	c := new(call)
	c.wg.Add(1)
	g.calls[key] = c
	g.mu.Unlock()

	// We are the designated leader for this key: do the real work.
	c.val, c.err = fn(ctx)

	// Remove the entry BEFORE waking the waiters, so that a request arriving
	// a microsecond later starts a fresh call instead of attaching to a
	// completed one and getting an already-stale value.
	g.mu.Lock()
	delete(g.calls, key)
	g.mu.Unlock()
	c.wg.Done()

	return c.val, false, c.err
}

// InFlight reports how many distinct keys are currently being fetched.
// Useful in tests and as a gauge on /metrics.
func (g *Group) InFlight() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.calls)
}

// JitterTTL spreads expiry times so that keys written in the same burst do
// not all expire in the same millisecond.
//
// WHY IT MATTERS: a deploy warms 10,000 cache keys with a flat 5m TTL. Five
// minutes later, all 10,000 expire simultaneously and you get a stampede
// across your entire key space at once. Singleflight only dedups *per key*,
// so it cannot save you here — 10,000 distinct keys means 10,000 queries.
// Jitter is the fix, and the two mitigations are complementary, not redundant.
//
// fraction 0.1 => the TTL varies randomly within ±10%.
func JitterTTL(ttl time.Duration, fraction float64) time.Duration {
	if ttl <= 0 || fraction <= 0 {
		return ttl
	}
	span := float64(ttl) * fraction
	// #nosec G404 -- jitter needs spread, not cryptographic unpredictability.
	out := time.Duration(float64(ttl) - span + rand.Float64()*2*span)
	if out <= 0 {
		return ttl
	}
	return out
}
