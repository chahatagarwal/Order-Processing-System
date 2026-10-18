# Caching

The five cache patterns, why we chose cache-aside, the difference between
invalidation and eviction, and the failure modes that take services down.

---

## 1. The three layers

Cheapest first. Each one that hits saves everything below it.

```
  ┌──────────────────────────────────────────────────────────────┐
  │ LAYER 1 · HTTP caching            (the client's own cache)   │
  │                                                               │
  │   Cache-Control: private, max-age=3600   ← terminal orders    │
  │   Cache-Control: private, no-store       ← live orders        │
  │   ETag: "7"                              ← the version        │
  │                                                               │
  │   The request NEVER REACHES US. This is the cheapest cache    │
  │   that exists, and the one people forget to use.              │
  └────────────────────────────┬─────────────────────────────────┘
                               │ miss
                               ▼
  ┌──────────────────────────────────────────────────────────────┐
  │ LAYER 2 · Redis                   (shared across replicas)   │
  │                                                               │
  │   cache-aside + singleflight + negative caching               │
  │   maxmemory 256mb, allkeys-lru                                │
  │   ~0.2-1ms, and ~100x faster than an uncached query           │
  └────────────────────────────┬─────────────────────────────────┘
                               │ miss
                               ▼
  ┌──────────────────────────────────────────────────────────────┐
  │ LAYER 3 · PostgreSQL              (the source of truth)      │
  └──────────────────────────────────────────────────────────────┘
```

### Why `private` and never `public`

An order contains a customer's name, email and postal address. `public` would
allow a shared CDN cache to store it and serve it **to a different user**.
Getting this directive wrong is a genuine data-leak mechanism, not a
performance detail.

### Why `ETag: version`

The order's `version` increments on exactly every mutation, which makes it a
perfect ETag. A client can then send `If-None-Match: "7"` and get `304 Not
Modified` with an empty body.

---

## 2. The five patterns, and why we chose cache-aside

| Pattern | How it works | Why not |
|---|---|---|
| **CACHE-ASIDE** ← ours | read: try cache → miss → DB → populate. write: DB → **delete** key | *(chosen; see below)* |
| Read-through | the cache library loads from the DB on a miss | Needs a cache that supports loaders; couples the cache to your schema |
| Write-through | write to cache **and** DB synchronously | Every write pays cache latency, and you cache data nobody reads |
| **Write-behind** | write to cache, flush to DB asynchronously | **You can LOSE COMMITTED DATA if the cache dies before the flush.** Never acceptable for orders or payments. Fine for view counters |
| Refresh-ahead | proactively refresh hot keys before they expire | Wastes work on keys that would not have been read again |

### Why cache-aside

1. **It only caches what is actually requested** — memory-efficient by
   construction.
2. **Cache failure is survivable.** A miss just means a database read. With
   write-behind, cache failure means *data loss*.
3. **It is the simplest thing that is correct**, and you can reason about every
   path.

The cost is honest: the first request after a write is always a miss.

---

## 3. Invalidate, never update

This is the single most important rule in the implementation.

```
  WRONG:  write DB, then SET the cache to the new value
  RIGHT:  write DB, then DELETE the cache key
```

**Why.** If two concurrent writes both *update* the cache, they can land in the
opposite order to how they landed in the database:

```
  t1  writer A  writes DB: status = PROCESSING
  t2  writer B  writes DB: status = CANCELLED      ← DB is now CANCELLED
  t3  writer B  SETs cache: CANCELLED
  t4  writer A  SETs cache: PROCESSING             ← CACHE IS NOW WRONG
```

The cache is now **permanently** wrong, and nothing will correct it until the
TTL expires. Deleting is **idempotent and self-correcting**: whichever order
the deletes land in, the next read repopulates from the single source of truth.

### And the order matters

```go
// repository/cached.go
if err := r.next.UpdateOptimistic(ctx, o, event); err != nil {
    return err          // ← note: NO invalidation on failure
}
r.invalidateOrder(ctx, o)   // only AFTER a successful write
```

- **Invalidate after, not before.** Invalidating first and then failing throws
  away a valid entry for nothing. Worse, if a concurrent read repopulates
  between the delete and the write, the cache holds the **old** row and is never
  invalidated again — permanently stale.
- **Do not invalidate on failure.** A version conflict means *nothing changed*,
  so the cached entry is still correct. Invalidating would turn every conflict
  into an extra database read.

### The residual race we accept

Even write-then-invalidate has a window:

```
  reader loads the old row ─┐
                            │ our DELETE runs here
  reader SETs it in cache ──┘   ← stale value, just written
```

The window is microseconds, and the TTL bounds the damage. The alternatives — a
distributed lock on every read, or versioned compare-and-swap writes into the
cache — cost more than the problem. **Knowing this window exists, and having
chosen to accept it, is the point.**

---

## 4. Invalidation vs eviction

A distinction that gets probed, because confusing them causes real mistakes.

|  | **INVALIDATION** | **EVICTION** |
|---|---|---|
| Why | The data is now **WRONG** | We are **OUT OF ROOM** |
| Decided by | **Our code**, deliberately | **The cache**, automatically |
| Trigger | A write happened | `maxmemory` reached |
| Is the data valid? | No | **Usually yes** |
| Concern | **Correctness** | **Capacity** |
| Here | `DELETE` key, `INCR` generation | `allkeys-lru` |

**Raising `maxmemory` does not fix stale data. Shortening TTLs does not fix
memory pressure.** They are different problems with different levers.

---

## 5. Eviction policies

```
  LRU    evict what was touched longest ago.      ← OUR CHOICE
         Assumes TEMPORAL LOCALITY, which is exactly true for order
         tracking: people re-check the order they just placed.
         O(1) with a map + a doubly-linked list.

  LFU    evict the least FREQUENTLY used.
         Better for a stable hot set, but it remembers history too well:
         yesterday's viral order stays resident forever. Needs decay.

  FIFO   evict the oldest INSERTED, ignoring use.
         Simple, but throws away your hottest key the moment it ages out.

  RANDOM evict anything. Sounds terrible; is actually fine at huge scale
         because it needs no bookkeeping (allkeys-random exists for this).

  TTL    not really an eviction policy — a CORRECTNESS BACKSTOP. Use both.
```

### Why `allkeys-lru` and not `volatile-lru`

`volatile-lru` only evicts keys that have a TTL. We *do* set a TTL on
everything — but if one key were ever written without one, it would become
immortal and could fill the instance. `allkeys-*` is the safer default.

### Why not `noeviction`

Writes start **failing** once memory is full. For a cache that is the wrong
answer: we would rather lose a cold entry than reject a new one.

Configured in [`docker-compose.yml`](../docker-compose.yml), with each
alternative and its reason in the comments.

---

## 6. The implementation, piece by piece

### 6a · Cache-aside with singleflight

[`repository/cached.go`](../internal/order/repository/cached.go) → `GetByID`

```
 1. GET the key                           → hit? return
 2. MISS → load from the DB, DEDUPLICATED by singleflight
 3. populate the cache (TTL from the DOMAIN, jittered)
 4. also index by order number
```

### 6b · Singleflight — the stampede guard

[`cache/singleflight.go`](../internal/platform/cache/singleflight.go)

**The outage it prevents:**

```
  An order is on a popular tracking page: 2,000 req/s, all from cache.
  Its cache entry expires.

  In that instant, all 2,000 in-flight requests MISS, and all 2,000 run
  the same SELECT. Your connection pool holds 10. The other 1,990 queue
  and time out, the pool stays saturated, and UNRELATED endpoints start
  failing.

  ONE EXPIRED KEY TOOK DOWN THE SERVICE.
```

**The fix:** deduplicate by key. The first goroutine to miss does the read;
every other goroutine asking for the *same* key waits and shares the result.
2,000 requests become **1 query**.

`TestSingleflight_CollapsesConcurrentMisses` fires 100 goroutines and asserts
the loader ran **exactly once**.

### 6c · Jitter — for the stampede singleflight cannot prevent

Singleflight dedups *per key*. If a deploy warms 10,000 keys with a flat 5m
TTL, five minutes later **all 10,000 expire in the same millisecond** — 10,000
distinct keys means 10,000 queries, and singleflight cannot help.

```go
cache.JitterTTL(5*time.Minute, 0.1)   // 4m30s … 5m30s, randomly
```

The two mitigations are complementary, not redundant.

### 6d · Negative caching — the penetration guard

```go
if errs.Is(err, errs.KindNotFound) {
    entry, _ := json.Marshal(cachedOrder{NotFound: true})
    _ = r.cache.Set(ctx, key, entry, JitterTTL(r.negativeTTL, 0.2))
}
```

**Without it:** a scanner requesting 10,000 random ids per second produces
10,000 database queries per second, every one returning nothing. The cache is
bypassed entirely by data that does not exist. This is **cache penetration**.

**Why the TTL is short (30s vs 5m):** an order that does not exist now may be
created a moment later, and we must not serve a 404 for a real order. Short
negative TTL, long positive TTL is the standard asymmetry.

### 6e · Generation-based group invalidation

**The problem:** we cache list pages under keys like

```
order:v1:list:g7:status=PENDING|limit=20|offset=0
order:v1:list:g7:status=PENDING|limit=20|offset=20
order:v1:list:g7:customer=c-42|limit=20|offset=0      ... thousands
```

When **any** order changes, every one of those may be wrong. How do you
invalidate thousands of keys you cannot enumerate?

```
  BAD   KEYS order:v1:list:*
        O(n) over the ENTIRE keyspace, and it BLOCKS single-threaded Redis
        while it runs. On a production instance with millions of keys this
        is a self-inflicted outage. NEVER USE KEYS IN PRODUCTION.

  BAD   SCAN in a loop
        Non-blocking, but many round-trips, it races with concurrent writes,
        and it may miss keys added mid-scan.

  BAD   FLUSHDB
        Throws away every other service's cache too.

  GOOD  Never delete them at all. Put a GENERATION COUNTER in the key.
        On any write:  INCR order:v1:list:generation

        Every subsequent read builds keys with g8, so ALL g7 keys become
        UNREACHABLE instantly — one atomic O(1) operation, no matter how
        many keys exist. The orphans are reclaimed by TTL expiry and LRU
        eviction, which is exactly the job eviction already does.
```

This is sometimes called a *cache namespace* or *key versioning*, and it is the
standard answer to invalidating a group you cannot enumerate.

### 6f · Schema versioning in the key

Every key embeds `v1`:

```go
CacheNamespaceOrderByID = "order:" + CacheKeyVersion + ":id"
```

If the cached JSON's shape ever changes, bump it to `v2` and every old entry
becomes unreachable **instantly** — no flush, no downtime, and no risk of
decoding old data with new code.

### 6g · Adaptive TTL, decided by the domain

```go
// domain/order.go
func (o *Order) CacheTTL(defaultTTL time.Duration) time.Duration {
    if o.Status.IsTerminal() { return time.Hour }   // immutable
    return defaultTTL
}
```

A `DELIVERED` or `CANCELLED` order can **never** change, so it can be cached
for an hour with zero staleness risk. A `PENDING` order could be cancelled a
second from now.

Letting the **domain** answer this is the Single Responsibility Principle
paying off: the rule lives with the knowledge that justifies it, and the cache
just does as it is told.

### 6h · The full TTL strategy

TTL is matched to **volatility** and **cost**, never set globally:

| Key | TTL | Why |
|---|---|---|
| Terminal order | 1h | Immutable. Zero staleness risk |
| Live order | 5m | Could change at any moment |
| List page | 1m | **Any** order's change can stale it, so the probability is far higher |
| Stats | 5m | The most expensive query, and the most tolerant of lag — nobody needs a dashboard accurate to the second |
| Not found | 30s | Short, so a newly created order is not hidden |

Matching TTL to volatility, rather than using one number, is most of what
"tuning a cache" actually means.

---

## 7. Failing open

```go
// cache/redis.go — Get()
if err != nil {
    c.misses.Add(1)
    return nil, ErrMiss      // a Redis ERROR is reported as a MISS
}
```

```
  FAIL OPEN   (ours)  cache error → treated as a miss → read the DB →
                      the user gets a correct, slightly slower response.

  FAIL CLOSED         cache error → 500 → a Redis blip takes down a
                      perfectly healthy website.
```

**A cache is an optimisation, never a source of truth, so losing it must only
cost latency.**

**The accepted risk:** if Redis dies during a traffic peak, full load lands on
Postgres at once. That is why Postgres has a bounded connection pool and why
singleflight exists.

### The one asymmetry

A failed `Set` is swallowed (failing to *populate* a cache is not a request
failure). A failed `Delete` **returns an error**, because it means the cache now
holds data we *know* is wrong. The caller logs it loudly, and the TTL guarantees
the staleness is bounded rather than permanent.

---

## 8. Why a shared cache

With 3 replicas behind a load balancer and an in-process cache:

```
  replica A cache: order 7 = PROCESSING
  replica B cache: order 7 = CANCELLED     ← three different answers
  replica C cache: order 7 = PENDING          to the same question

  replica A invalidates on write → B and C STILL SERVE STALE DATA
```

There is no fix other than a shared cache. **This is the most common caching
mistake in microservices.**

`make run-nodocker` uses the in-process cache and logs a warning that says
exactly this.

---

## 9. Measuring it

```bash
make stats          # hit ratio, evictions, expirations
make load-test      # watch the ratio climb under real load
make redis-cli      # KEYS order:*  ·  TTL <key>  ·  INFO memory
```

| Metric | How to read it |
|---|---|
| `cache_hit_ratio` | **Below ~0.8** on a read-heavy workload usually means the TTL is too short, the keys are too granular, or you are invalidating too aggressively |
| `cache_evictions_total` | Rising steadily → `maxmemory` is too small for the working set |
| `cache_expired_total` | High relative to hits → the TTL may be shorter than it needs to be |
| A ratio of 1.0 **with** stale complaints | The opposite problem: you are not invalidating |

---

## 10. What to read

| File | Why |
|---|---|
| [`cache/cache.go`](../internal/platform/cache/cache.go) | The port, and all five patterns compared in the header |
| [`cache/memory.go`](../internal/platform/cache/memory.go) | A hand-written LRU, so eviction is visible. Note `Get` takes a plain `Mutex`, because a cache read *moves* the entry |
| [`cache/redis.go`](../internal/platform/cache/redis.go) | Fail-open, `UNLINK` instead of `DEL`, and the `Set`/`Delete` asymmetry |
| [`cache/singleflight.go`](../internal/platform/cache/singleflight.go) | 40 lines that prevent an outage |
| [`repository/cached.go`](../internal/order/repository/cached.go) | The Decorator, generation invalidation, negative caching |
| [`cache/cache_test.go`](../internal/platform/cache/cache_test.go) | `TestMemory_LRUEviction` and `TestSingleflight_CollapsesConcurrentMisses` |
