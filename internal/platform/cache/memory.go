// MemoryCache: an in-process LRU + TTL cache, written by hand.
//
// WHY WRITE THIS INSTEAD OF IMPORTING ONE:
// "Use of cache eviction strategy" is a core requirement, and eviction is
// something you should be able to draw on a whiteboard. This file IS the
// explanation: a hash map for O(1) lookup plus a doubly-linked list for O(1)
// recency ordering. That pairing is what every LRU cache in the world is.
//
// ============================================================================
// EVICTION vs INVALIDATION — the distinction interviewers probe
// ============================================================================
//
//	EVICTION    = the cache removes data because it is OUT OF ROOM.
//	              The data may still be perfectly valid. This is a CAPACITY
//	              decision, made by the cache, automatically.
//	              Policies: LRU, LFU, FIFO, Random, TTL-based.
//
//	INVALIDATION = WE remove data because it is now WRONG (the underlying row
//	              changed). This is a CORRECTNESS decision, made by our code,
//	              deliberately.
//
// Confusing them causes real bugs: raising maxmemory does not fix stale data,
// and shortening TTLs does not fix memory pressure.
//
// ============================================================================
// WHY LRU (Least Recently Used) AS THE DEFAULT
// ============================================================================
//
//	LRU    - evict what was touched longest ago. Assumes temporal locality
//	         ("just-looked-at orders get looked at again"), which is exactly
//	         true for order tracking pages. CHEAP: O(1). OUR CHOICE.
//	LFU    - evict the least frequently used. Better for stable hot sets, but
//	         it remembers history too well: yesterday's viral order keeps
//	         resident forever ("cache pollution"). Needs decay to be usable.
//	FIFO   - evict the oldest inserted, ignoring use. Simple, but throws away
//	         your hottest key the moment it ages out.
//	RANDOM - evict anything. Sounds terrible; is actually fine at huge scale
//	         and needs no bookkeeping (Redis allkeys-random exists for this).
//	TTL    - not really an eviction policy; a correctness backstop. Use both.
//
// We combine LRU (capacity) with TTL (freshness) — which is what Redis does
// with `maxmemory-policy allkeys-lru` plus per-key EXPIRE.
package cache

import (
	"container/list"
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// entry is what we store in the linked list. It holds its own key so that,
// when we evict the list's back element, we know which map key to delete.
// (Without the back-reference you cannot do O(1) eviction — a detail people
// get wrong when implementing LRU from memory.)
type entry struct {
	key       string
	value     []byte
	expiresAt time.Time // zero means "never expires"
}

// MemoryCache is safe for concurrent use.
type MemoryCache struct {
	// mu is a plain Mutex, not RWMutex. Surprising, but correct: a cache GET
	// must MOVE the entry to the front of the LRU list, which is a WRITE to
	// shared state. So "reads" are not read-only and RWMutex would buy nothing
	// while costing more per operation.
	mu         sync.Mutex
	maxEntries int
	items      map[string]*list.Element // key -> node in recency list
	recency    *list.List               // front = most recently used
	counters   map[string]*int64        // simple counter store for Incr

	hits, misses, sets, deletes, evictions, expired atomic.Uint64

	// now is injectable so tests can simulate time passing without sleeping.
	// Real TTL tests that sleep are slow and flaky; this makes them instant
	// and deterministic.
	now func() time.Time

	stopJanitor chan struct{}
	janitorOnce sync.Once
}

// NewMemory builds a cache bounded to maxEntries. A background "janitor"
// sweeps expired keys every interval.
//
// WHY A JANITOR AT ALL, given we also check expiry on read:
// Lazy expiry alone leaks memory. A key written once and never read again
// stays resident forever, holding its bytes. The janitor reclaims it.
// This is exactly the trade-off Redis makes (lazy + sampled active expiry).
func NewMemory(maxEntries int, janitorInterval time.Duration) *MemoryCache {
	if maxEntries <= 0 {
		maxEntries = 1000
	}
	c := &MemoryCache{
		maxEntries:  maxEntries,
		items:       make(map[string]*list.Element, maxEntries),
		recency:     list.New(),
		counters:    map[string]*int64{},
		now:         time.Now,
		stopJanitor: make(chan struct{}),
	}
	if janitorInterval > 0 {
		go c.janitor(janitorInterval)
	}
	return c
}

// NewMemoryForTest returns a cache with a controllable clock and no janitor.
func NewMemoryForTest(maxEntries int, clock func() time.Time) *MemoryCache {
	c := NewMemory(maxEntries, 0)
	c.now = clock
	return c
}

func (c *MemoryCache) Get(_ context.Context, key string) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	el, ok := c.items[key]
	if !ok {
		c.misses.Add(1)
		return nil, ErrMiss
	}

	ent := el.Value.(*entry)

	// LAZY EXPIRY: an expired key is a miss, and we delete it on the way out.
	if !ent.expiresAt.IsZero() && c.now().After(ent.expiresAt) {
		c.removeElement(el)
		c.expired.Add(1)
		c.misses.Add(1)
		return nil, ErrMiss
	}

	// THE "RECENTLY USED" PART OF LRU: touching a key promotes it to the
	// front, so it is the last thing we would evict.
	c.recency.MoveToFront(el)
	c.hits.Add(1)

	// Return a COPY. If we returned ent.value, the caller could mutate the
	// backing array and silently corrupt the cached entry for everyone.
	// This is the single most important line in the file.
	out := make([]byte, len(ent.value))
	copy(out, ent.value)
	return out, nil
}

func (c *MemoryCache) Set(_ context.Context, key string, value []byte, ttl time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	var expiresAt time.Time
	if ttl > 0 {
		expiresAt = c.now().Add(ttl)
	}

	// Defensive copy on the way IN, for the same reason as on the way out.
	stored := make([]byte, len(value))
	copy(stored, value)

	if el, ok := c.items[key]; ok { // overwrite in place, keep one list node
		ent := el.Value.(*entry)
		ent.value = stored
		ent.expiresAt = expiresAt
		c.recency.MoveToFront(el)
		c.sets.Add(1)
		return nil
	}

	el := c.recency.PushFront(&entry{key: key, value: stored, expiresAt: expiresAt})
	c.items[key] = el
	c.sets.Add(1)

	// EVICT while over capacity. A `for` loop, not an `if`: maxEntries could
	// have been lowered at runtime, and defensive loops here are free.
	for c.recency.Len() > c.maxEntries {
		oldest := c.recency.Back() // the least recently used element
		if oldest == nil {
			break
		}
		c.removeElement(oldest)
		c.evictions.Add(1)
	}
	return nil
}

func (c *MemoryCache) Delete(_ context.Context, keys ...string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, k := range keys {
		if el, ok := c.items[k]; ok {
			c.removeElement(el)
			c.deletes.Add(1)
		}
	}
	return nil
}

func (c *MemoryCache) Incr(_ context.Context, key string) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.counters[key]
	if !ok {
		var v int64
		p = &v
		c.counters[key] = p
	}
	*p++
	return *p, nil
}

// Ping always succeeds: an in-process cache is up exactly when the process is.
func (c *MemoryCache) Ping(_ context.Context) error { return nil }

func (c *MemoryCache) Stats() Stats {
	c.mu.Lock()
	n := len(c.items)
	c.mu.Unlock()
	return Stats{
		Hits:      c.hits.Load(),
		Misses:    c.misses.Load(),
		Sets:      c.sets.Load(),
		Deletes:   c.deletes.Load(),
		Evictions: c.evictions.Load(),
		Expired:   c.expired.Load(),
		Entries:   n,
	}
}

func (c *MemoryCache) Close() error {
	// sync.Once guards against a double Close panicking on a closed channel —
	// which happens in real shutdown paths more often than you would think.
	c.janitorOnce.Do(func() { close(c.stopJanitor) })
	return nil
}

// removeElement must be called with c.mu held.
func (c *MemoryCache) removeElement(el *list.Element) {
	c.recency.Remove(el)
	delete(c.items, el.Value.(*entry).key)
}

// janitor performs ACTIVE EXPIRY. It scans the recency list from the back
// (coldest first), which is where expired keys are most likely to be.
func (c *MemoryCache) janitor(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-c.stopJanitor:
			return
		case <-ticker.C:
			c.sweepExpired()
		}
	}
}

// SweepExpired is exported for tests so they can trigger a sweep explicitly.
func (c *MemoryCache) SweepExpired() int { return c.sweepExpired() }

func (c *MemoryCache) sweepExpired() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()
	removed := 0
	// Walk back-to-front, capturing Prev() BEFORE removing — removing an
	// element invalidates its own links, so reading el.Prev() afterwards is
	// use-after-free. A real bug that is easy to write.
	for el := c.recency.Back(); el != nil; {
		prev := el.Prev()
		ent := el.Value.(*entry)
		if !ent.expiresAt.IsZero() && now.After(ent.expiresAt) {
			c.removeElement(el)
			c.expired.Add(1)
			removed++
		}
		el = prev
	}
	return removed
}

// Keys returns a snapshot of live keys. Test/debug helper only — never call
// this on a production cache with millions of entries.
func (c *MemoryCache) Keys() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.items))
	for el := c.recency.Front(); el != nil; el = el.Next() {
		out = append(out, el.Value.(*entry).key)
	}
	return out
}

// Compile-time proof that MemoryCache satisfies the Cache port. If the
// interface changes, the build breaks HERE with a clear message, instead of
// at some distant call site. Cheap, and worth doing for every adapter.
var _ Cache = (*MemoryCache)(nil)
