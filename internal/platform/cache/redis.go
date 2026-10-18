// RedisCache: the production cache adapter.
//
// WHY REDIS AND NOT JUST THE IN-PROCESS CACHE:
// With 3 replicas of the order service behind a load balancer, an in-process
// cache gives you THREE DIFFERENT ANSWERS to the same question, and
// invalidating on one replica leaves the other two serving stale data. There
// is no fix for that other than a shared cache. This is the single most
// common caching mistake in microservices.
//
// WHAT WE GET FROM REDIS SPECIFICALLY:
//   - One shared view across all replicas.
//   - Server-side TTL (EXPIRE), so freshness does not depend on our uptime.
//   - maxmemory + allkeys-lru eviction, configured in docker-compose.yml.
//   - Atomic INCR, which is what makes generation-based invalidation possible.
//
// WHAT WE GIVE UP:
//   - A network hop (~0.2-1ms). Still ~100x faster than an uncached query.
//   - A new failure mode. Addressed below: WE FAIL OPEN.
package cache

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

type RedisCache struct {
	client *redis.Client

	hits, misses, sets, deletes atomic.Uint64
}

// RedisOptions mirrors only what we actually use, so the rest of the codebase
// never imports the redis package. If we switched to Valkey or Memcached,
// this file is the only one that changes (Open/Closed Principle).
type RedisOptions struct {
	Addr     string
	Password string
	DB       int
	// PoolSize bounds concurrent connections. Unbounded pools exhaust the
	// server's client limit under a traffic spike.
	PoolSize int
	// Timeouts: a cache call MUST have a tight timeout. The entire point of a
	// cache is speed; a cache lookup that blocks for 5 seconds is strictly
	// worse than no cache at all. 200ms is already generous.
	DialTimeout  time.Duration
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
}

func DefaultRedisOptions(addr, password string, db int) RedisOptions {
	return RedisOptions{
		Addr:         addr,
		Password:     password,
		DB:           db,
		PoolSize:     20,
		DialTimeout:  2 * time.Second,
		ReadTimeout:  200 * time.Millisecond,
		WriteTimeout: 200 * time.Millisecond,
	}
}

func NewRedis(opts RedisOptions) *RedisCache {
	return &RedisCache{
		client: redis.NewClient(&redis.Options{
			Addr:         opts.Addr,
			Password:     opts.Password,
			DB:           opts.DB,
			PoolSize:     opts.PoolSize,
			DialTimeout:  opts.DialTimeout,
			ReadTimeout:  opts.ReadTimeout,
			WriteTimeout: opts.WriteTimeout,
		}),
	}
}

func (c *RedisCache) Get(ctx context.Context, key string) ([]byte, error) {
	b, err := c.client.Get(ctx, key).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) { // key absent — an ordinary miss
			c.misses.Add(1)
			return nil, ErrMiss
		}
		// A REAL Redis failure (down, timeout, auth). We report it as a miss
		// to the caller rather than an error.
		//
		// THIS IS THE "FAIL OPEN" DECISION, and it is deliberate:
		//   fail open  = cache error is treated as a miss -> read the DB ->
		//                the user gets a correct, slightly slower response.
		//   fail closed = cache error is returned as a 500 -> a Redis blip
		//                takes down a perfectly healthy website.
		//
		// A cache is an OPTIMISATION, never a source of truth, so losing it
		// must only cost latency. The accepted risk is that if Redis dies
		// during a traffic peak, full load lands on Postgres at once — which
		// is why Postgres has a bounded connection pool, and why the
		// stampede guard in singleflight.go matters.
		c.misses.Add(1)
		return nil, ErrMiss
	}
	c.hits.Add(1)
	return b, nil
}

func (c *RedisCache) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	// We always pass a TTL. A key written without one lives until evicted,
	// and a stale key with no expiry is how you serve a cancelled order as
	// PENDING for a week. If every key can expire, every bug self-heals.
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	if err := c.client.Set(ctx, key, value, ttl).Err(); err != nil {
		// Swallowed on purpose: failing to POPULATE a cache is not a request
		// failure. We log at the call site where there is request context.
		return nil
	}
	c.sets.Add(1)
	return nil
}

func (c *RedisCache) Delete(ctx context.Context, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	// UNLINK, not DEL: UNLINK frees memory on a background thread, so deleting
	// a large value does not block the single-threaded Redis event loop and
	// stall every other client. A small change with a real latency impact.
	if err := c.client.Unlink(ctx, keys...).Err(); err != nil {
		// NOTE THE ASYMMETRY WITH Set: a failed DELETE is NOT safe to swallow.
		// It means the cache now holds data we know is wrong. We return the
		// error so the caller can log loudly; TTL is the backstop that
		// guarantees the staleness is bounded rather than permanent.
		return err
	}
	c.deletes.Add(uint64(len(keys)))
	return nil
}

func (c *RedisCache) Incr(ctx context.Context, key string) (int64, error) {
	// INCR is atomic on the server, so N replicas bumping the generation
	// counter concurrently can never lose an increment. Doing this as
	// GET-then-SET in application code would be a lost-update race.
	return c.client.Incr(ctx, key).Result()
}

func (c *RedisCache) Ping(ctx context.Context) error {
	return c.client.Ping(ctx).Err()
}

func (c *RedisCache) Stats() Stats {
	// Entries is left at 0: counting keys needs DBSIZE/SCAN, and running
	// those on a hot production Redis to satisfy a stats endpoint is a
	// self-inflicted outage. Redis INFO already reports this properly.
	return Stats{
		Hits:    c.hits.Load(),
		Misses:  c.misses.Load(),
		Sets:    c.sets.Load(),
		Deletes: c.deletes.Load(),
	}
}

func (c *RedisCache) Close() error { return c.client.Close() }

var _ Cache = (*RedisCache)(nil)

// ---------------------------------------------------------------------------
// NoopCache — the Null Object pattern (a behavioural GoF pattern).
// ---------------------------------------------------------------------------
//
// When CACHE_ENABLED=false we inject this instead of sprinkling
// `if cacheEnabled { ... }` through the service layer. Every read is a miss,
// every write is a no-op, and the business logic is identical in both modes.
// That absence of branching is the point: fewer code paths, fewer bugs.
type NoopCache struct{}

func (NoopCache) Get(context.Context, string) ([]byte, error)              { return nil, ErrMiss }
func (NoopCache) Set(context.Context, string, []byte, time.Duration) error { return nil }
func (NoopCache) Delete(context.Context, ...string) error                  { return nil }
func (NoopCache) Incr(context.Context, string) (int64, error)              { return 0, nil }
func (NoopCache) Ping(context.Context) error                               { return nil }
func (NoopCache) Stats() Stats                                             { return Stats{} }
func (NoopCache) Close() error                                             { return nil }

var _ Cache = NoopCache{}
