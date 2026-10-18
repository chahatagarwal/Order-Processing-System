package cache

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ============================================================================
// CACHE TESTS
//
// These are the tests that prove the EVICTION and INVALIDATION mechanics work,
// which is the part of caching that is easy to assert in a comment and hard to
// get right in code.
//
// Note the injected clock: TTL behaviour is tested by MOVING TIME, not by
// sleeping. A test that sleeps for a TTL is slow; one that sleeps for *almost*
// a TTL is flaky on a loaded machine.
// ============================================================================

func newTestCache(maxEntries int) (*MemoryCache, func(time.Duration)) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	advance := func(d time.Duration) {
		mu.Lock()
		now = now.Add(d)
		mu.Unlock()
	}
	return NewMemoryForTest(maxEntries, clock), advance
}

func TestMemory_GetSetDelete(t *testing.T) {
	c, _ := newTestCache(10)
	ctx := context.Background()

	if _, err := c.Get(ctx, "missing"); err != ErrMiss {
		t.Errorf("expected ErrMiss for an absent key, got %v", err)
	}

	if err := c.Set(ctx, "k", []byte("v"), time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := c.Get(ctx, "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != "v" {
		t.Errorf("got %q, want %q", got, "v")
	}

	if err := c.Delete(ctx, "k"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := c.Get(ctx, "k"); err != ErrMiss {
		t.Error("the key should be gone after Delete")
	}
}

func TestMemory_ReturnsACopyNotTheStoredSlice(t *testing.T) {
	// ========================================================================
	// THE MOST IMPORTANT TEST IN THIS FILE.
	//
	// If Get returned the stored byte slice, a caller mutating "their" copy
	// would silently corrupt the cached value for every other caller. That bug
	// is invisible until two requests disagree about the same order, and it is
	// almost impossible to diagnose from the symptoms.
	// ========================================================================
	c, _ := newTestCache(10)
	ctx := context.Background()

	_ = c.Set(ctx, "k", []byte("original"), time.Minute)

	first, _ := c.Get(ctx, "k")
	first[0] = 'X' // a caller mutating what it believes is its own data

	second, _ := c.Get(ctx, "k")
	if string(second) != "original" {
		t.Errorf("the cached value was corrupted by a caller: got %q, want %q", second, "original")
	}
}

func TestMemory_SetAlsoCopiesTheInput(t *testing.T) {
	// The mirror image: if the caller mutates the slice it passed to Set, the
	// cached value must not change underneath us.
	c, _ := newTestCache(10)
	ctx := context.Background()

	value := []byte("original")
	_ = c.Set(ctx, "k", value, time.Minute)
	value[0] = 'X'

	got, _ := c.Get(ctx, "k")
	if string(got) != "original" {
		t.Errorf("mutating the caller's slice changed the cache: got %q", got)
	}
}

func TestMemory_TTLExpiry(t *testing.T) {
	c, advance := newTestCache(10)
	ctx := context.Background()

	_ = c.Set(ctx, "k", []byte("v"), time.Minute)

	advance(59 * time.Second)
	if _, err := c.Get(ctx, "k"); err != nil {
		t.Error("the key should still be live just before its TTL")
	}

	advance(2 * time.Second) // now past 60s
	if _, err := c.Get(ctx, "k"); err != ErrMiss {
		t.Error("the key should have expired")
	}

	stats := c.Stats()
	if stats.Expired == 0 {
		t.Error("the expiry should be counted, so it is visible on /metrics")
	}
}

func TestMemory_LRUEviction(t *testing.T) {
	// ========================================================================
	// EVICTION: the cache is FULL, so something must go. LRU says it is the
	// least recently USED entry — not the oldest inserted.
	// ========================================================================
	c, _ := newTestCache(3)
	ctx := context.Background()

	_ = c.Set(ctx, "a", []byte("1"), time.Hour)
	_ = c.Set(ctx, "b", []byte("2"), time.Hour)
	_ = c.Set(ctx, "c", []byte("3"), time.Hour)

	// TOUCH "a", which promotes it to most-recently-used. This is the line
	// that distinguishes LRU from FIFO: under FIFO, "a" would be evicted next
	// because it was inserted first.
	if _, err := c.Get(ctx, "a"); err != nil {
		t.Fatalf("Get a: %v", err)
	}

	// Inserting "d" exceeds the capacity of 3, so one entry is evicted.
	_ = c.Set(ctx, "d", []byte("4"), time.Hour)

	// "b" was the least recently used, so "b" is gone.
	if _, err := c.Get(ctx, "b"); err != ErrMiss {
		t.Error("b was the least recently used and should have been evicted")
	}
	// "a" survived BECAUSE we touched it.
	if _, err := c.Get(ctx, "a"); err != nil {
		t.Error("a was recently used and should have survived")
	}
	for _, k := range []string{"c", "d"} {
		if _, err := c.Get(ctx, k); err != nil {
			t.Errorf("%s should still be cached", k)
		}
	}

	if c.Stats().Evictions != 1 {
		t.Errorf("evictions: got %d, want 1", c.Stats().Evictions)
	}
}

func TestMemory_NeverExceedsItsCapacity(t *testing.T) {
	// The bound is what stops the cache consuming all available memory.
	c, _ := newTestCache(5)
	ctx := context.Background()

	for i := 0; i < 100; i++ {
		_ = c.Set(ctx, fmt.Sprintf("key-%d", i), []byte("v"), time.Hour)
	}
	if entries := c.Stats().Entries; entries > 5 {
		t.Errorf("the cache holds %d entries, exceeding its capacity of 5", entries)
	}
}

func TestMemory_OverwriteDoesNotGrowTheCache(t *testing.T) {
	// Re-Setting an existing key must reuse its list node. If it appended a
	// second node, a hot key written repeatedly would evict everything else.
	c, _ := newTestCache(10)
	ctx := context.Background()

	for i := 0; i < 20; i++ {
		_ = c.Set(ctx, "same-key", []byte("v"), time.Hour)
	}
	if entries := c.Stats().Entries; entries != 1 {
		t.Errorf("entries: got %d, want 1", entries)
	}
}

func TestMemory_JanitorSweepsExpiredKeys(t *testing.T) {
	// ACTIVE EXPIRY. Lazy expiry alone leaks memory: a key written once and
	// never read again holds its bytes forever.
	c, advance := newTestCache(100)
	ctx := context.Background()

	for i := 0; i < 10; i++ {
		_ = c.Set(ctx, fmt.Sprintf("short-%d", i), []byte("v"), time.Second)
	}
	_ = c.Set(ctx, "long", []byte("v"), time.Hour)

	advance(2 * time.Second)

	if removed := c.SweepExpired(); removed != 10 {
		t.Errorf("the sweep removed %d keys, want 10", removed)
	}
	if _, err := c.Get(ctx, "long"); err != nil {
		t.Error("the long-TTL key should have survived the sweep")
	}
}

func TestMemory_ZeroTTLMeansNoExpiry(t *testing.T) {
	c, advance := newTestCache(10)
	ctx := context.Background()

	_ = c.Set(ctx, "forever", []byte("v"), 0)
	advance(100 * time.Hour)

	if _, err := c.Get(ctx, "forever"); err != nil {
		t.Error("a zero TTL should mean the key does not expire")
	}
}

func TestMemory_IncrIsMonotonic(t *testing.T) {
	// Incr backs generation-based group invalidation, so it must never lose a
	// bump.
	c, _ := newTestCache(10)
	ctx := context.Background()

	for want := int64(1); want <= 5; want++ {
		got, err := c.Incr(ctx, "gen")
		if err != nil {
			t.Fatalf("Incr: %v", err)
		}
		if got != want {
			t.Errorf("Incr: got %d, want %d", got, want)
		}
	}
}

func TestMemory_HitRatio(t *testing.T) {
	c, _ := newTestCache(10)
	ctx := context.Background()

	_ = c.Set(ctx, "k", []byte("v"), time.Hour)
	for i := 0; i < 3; i++ {
		_, _ = c.Get(ctx, "k")
	}
	_, _ = c.Get(ctx, "nope")

	if ratio := c.Stats().HitRatio(); ratio != 0.75 {
		t.Errorf("hit ratio: got %v, want 0.75", ratio)
	}
}

func TestMemory_HitRatioOfAnIdleCacheIsZeroNotNaN(t *testing.T) {
	// A division by zero here would put NaN on a dashboard and break alerting.
	c, _ := newTestCache(10)
	if ratio := c.Stats().HitRatio(); ratio != 0 {
		t.Errorf("an idle cache should report 0, got %v", ratio)
	}
}

func TestMemory_ConcurrentAccessIsSafe(t *testing.T) {
	// Run with -race. An HTTP server touches the cache from many goroutines at
	// once, so this is the normal case, not an edge case.
	c := NewMemory(50, 0)
	defer func() { _ = c.Close() }()
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			key := fmt.Sprintf("key-%d", n%10)
			for j := 0; j < 100; j++ {
				_ = c.Set(ctx, key, []byte("v"), time.Minute)
				_, _ = c.Get(ctx, key)
				if j%10 == 0 {
					_ = c.Delete(ctx, key)
				}
				_, _ = c.Incr(ctx, "counter")
			}
		}(i)
	}
	wg.Wait()
}

func TestMemory_DoubleCloseDoesNotPanic(t *testing.T) {
	// Real shutdown paths call Close more than once more often than you expect,
	// and closing an already-closed channel panics.
	c := NewMemory(10, time.Minute)
	if err := c.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close should be safe, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Singleflight — stampede protection
// ---------------------------------------------------------------------------

func TestSingleflight_CollapsesConcurrentMisses(t *testing.T) {
	// ========================================================================
	// THE CACHE-STAMPEDE TEST.
	//
	// 100 goroutines miss the same key simultaneously. Without deduplication
	// that is 100 identical database queries, which is how one expired key
	// saturates a connection pool and takes the service down.
	//
	// We assert the loader ran EXACTLY ONCE.
	// ========================================================================
	g := NewGroup()
	ctx := context.Background()

	var loaderCalls atomic.Int64
	start := make(chan struct{})

	const goroutines = 100
	var wg sync.WaitGroup
	values := make([][]byte, goroutines)
	sharedCount := atomic.Int64{}

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start // a barrier, so they genuinely overlap
			val, shared, err := g.Do(ctx, "hot-key", func(ctx context.Context) ([]byte, error) {
				loaderCalls.Add(1)
				// Simulate a slow database read, so the other 99 goroutines
				// arrive while this one is still in flight.
				time.Sleep(20 * time.Millisecond)
				return []byte("loaded-once"), nil
			})
			if err != nil {
				t.Errorf("goroutine %d: %v", idx, err)
				return
			}
			values[idx] = val
			if shared {
				sharedCount.Add(1)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if calls := loaderCalls.Load(); calls != 1 {
		t.Errorf("the loader ran %d times; stampede protection requires exactly 1", calls)
	}
	if shared := sharedCount.Load(); shared != goroutines-1 {
		t.Errorf("%d goroutines reported a shared result, want %d", shared, goroutines-1)
	}
	// Every caller must receive the SAME correct value.
	for i, v := range values {
		if string(v) != "loaded-once" {
			t.Errorf("goroutine %d got %q", i, v)
		}
	}
}

func TestSingleflight_DifferentKeysRunInParallel(t *testing.T) {
	// Deduplication must be PER KEY. If it serialised across keys, it would
	// turn a concurrency optimisation into a global bottleneck.
	g := NewGroup()
	ctx := context.Background()

	var calls atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_, _, _ = g.Do(ctx, fmt.Sprintf("key-%d", n), func(ctx context.Context) ([]byte, error) {
				calls.Add(1)
				return []byte("v"), nil
			})
		}(i)
	}
	wg.Wait()

	if calls.Load() != 10 {
		t.Errorf("10 distinct keys should produce 10 loads, got %d", calls.Load())
	}
}

func TestSingleflight_ErrorsAreSharedToo(t *testing.T) {
	// If the loader fails, every waiter must learn about it — not hang, and
	// not silently receive a nil value.
	g := NewGroup()
	ctx := context.Background()

	wantErr := fmt.Errorf("database is down")
	var wg sync.WaitGroup
	errCount := atomic.Int64{}

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := g.Do(ctx, "failing", func(ctx context.Context) ([]byte, error) {
				time.Sleep(5 * time.Millisecond)
				return nil, wantErr
			})
			if err != nil {
				errCount.Add(1)
			}
		}()
	}
	wg.Wait()

	if errCount.Load() != 10 {
		t.Errorf("%d of 10 goroutines received the error; all should", errCount.Load())
	}
}

func TestSingleflight_ReleasesTheKeyAfterCompletion(t *testing.T) {
	// A key left in the in-flight map would permanently block future loads
	// for it — a cache that stops working for exactly its hottest keys.
	g := NewGroup()
	ctx := context.Background()

	_, _, _ = g.Do(ctx, "k", func(context.Context) ([]byte, error) { return []byte("v"), nil })
	if n := g.InFlight(); n != 0 {
		t.Errorf("%d keys still in flight after completion, want 0", n)
	}

	var ran bool
	_, _, _ = g.Do(ctx, "k", func(context.Context) ([]byte, error) {
		ran = true
		return []byte("v2"), nil
	})
	if !ran {
		t.Error("a later call for the same key must run its own loader")
	}
}

func TestJitterTTL_StaysWithinBounds(t *testing.T) {
	// Jitter stops a batch of keys written together from expiring together.
	base := time.Minute
	for i := 0; i < 1000; i++ {
		got := JitterTTL(base, 0.1)
		if got < 54*time.Second || got > 66*time.Second {
			t.Fatalf("jittered TTL %s is outside the expected ±10%% band", got)
		}
	}
}

func TestJitterTTL_ActuallyVaries(t *testing.T) {
	// A "jitter" that always returns the same value provides no spread and
	// would leave the synchronised-expiry problem in place.
	seen := map[time.Duration]bool{}
	for i := 0; i < 100; i++ {
		seen[JitterTTL(time.Minute, 0.2)] = true
	}
	if len(seen) < 10 {
		t.Errorf("only %d distinct TTLs across 100 calls; jitter is not spreading", len(seen))
	}
}

func TestJitterTTL_HandlesEdgeCases(t *testing.T) {
	if got := JitterTTL(0, 0.5); got != 0 {
		t.Errorf("a zero TTL should stay zero, got %s", got)
	}
	if got := JitterTTL(time.Minute, 0); got != time.Minute {
		t.Errorf("zero jitter should return the TTL unchanged, got %s", got)
	}
}

// ---------------------------------------------------------------------------
// NoopCache — the Null Object
// ---------------------------------------------------------------------------

func TestNoopCache_AlwaysMissesAndNeverErrors(t *testing.T) {
	// This is what makes CACHE_ENABLED=false need no if-statements anywhere
	// else in the system.
	var c Cache = NoopCache{}
	ctx := context.Background()

	if err := c.Set(ctx, "k", []byte("v"), time.Minute); err != nil {
		t.Errorf("Set: %v", err)
	}
	if _, err := c.Get(ctx, "k"); err != ErrMiss {
		t.Errorf("a noop cache must always miss, got %v", err)
	}
	if err := c.Delete(ctx, "k"); err != nil {
		t.Errorf("Delete: %v", err)
	}
	if err := c.Ping(ctx); err != nil {
		t.Errorf("Ping: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}
