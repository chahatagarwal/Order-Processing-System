# Configuration reference

Every environment variable, its default, and what changing it does.

---

## The philosophy

**Code never calls `os.Getenv()` directly, anywhere.** Everything flows through
[`internal/platform/config`](../internal/platform/config/config.go), which
returns a typed struct. Three reasons:

1. **Validation happens once, at boot.** A typo in `CACHE_TTL` crashes the
   process on startup with a clear message, instead of silently becoming a zero
   TTL and quietly disabling your cache at 3am.
2. **It is discoverable.** That one file *is* the documentation of every knob.
3. **It is testable.** Tests build a `Config` literal — no global state, no
   environment pollution between tests.

### Constants vs configuration

| | `platform/constants` | `platform/config` |
|---|---|---|
| Holds | Values that are part of the program's **meaning** | Values that change per **environment** |
| Examples | Status names, header names, cache-key prefixes, API version | DB password, port, TTL tuning |
| Changes at runtime? | Never | Per deployment |

Putting a DB password in `constants` would be a bug; putting a cache-key prefix
in the environment would be pointless noise.

---

## Service identity

| Variable | Default | Notes |
|---|---|---|
| `APP_ENV` | `dev` | `dev` \| `test` \| `prod`. **`prod` refuses to start with `POSTGRES_ENABLED=false`** — an in-memory database in production is never intentional |
| `HTTP_ADDR` | `:8080` (order) | Also `:8081` inventory, `:8082` payment, `:8083` legacy |
| `SHUTDOWN_GRACE` | `20s` | How long to drain in-flight requests on SIGTERM. Must be **longer** than your slowest normal request and **shorter** than Kubernetes' `terminationGracePeriodSeconds` (default 30s), or the kernel SIGKILLs you mid-cleanup |

---

## Logging

| Variable | Default | Notes |
|---|---|---|
| `LOG_LEVEL` | `info` | `debug` \| `info` \| `warn` \| `error`. `debug` shows cache hits and saga step detail |
| `LOG_FORMAT` | `text` | `text` for your terminal, **`json` for production** (so a log aggregator can parse it) |

---

## PostgreSQL

| Variable | Default | Notes |
|---|---|---|
| `POSTGRES_ENABLED` | `true` | `false` swaps in the in-memory repository. **You lose durability, real row locks and cross-replica coherence** |
| `POSTGRES_DSN` | `postgres://orders:orders@localhost:5432/orders?sslmode=disable` | The full connection string, which is what every hosting provider hands you |
| `POSTGRES_MAX_CONNS` | `10` | See the sizing note below |
| `POSTGRES_MIN_CONNS` | `1` | Keeps warm connections so a spike does not pay connection-establishment cost on the critical path |
| `POSTGRES_MAX_CONN_LIFETIME` | `1h` | Recycling prevents unbounded per-backend memory growth and lets a connection pick up DNS/failover changes |

### On pool sizing

This is a real trade-off, not a number to leave at default:

```
  too small  →  requests QUEUE waiting for a connection, and your p99
                latency becomes "time spent waiting for the pool"

  too large  →  you exhaust Postgres's max_connections (default 100), and
                because each backend is an OS PROCESS, hundreds of them
                thrash the server. More connections past saturation makes
                throughput WORSE, not better.
```

Rule of thumb: start near `(cpu_cores × 2) + effective_spindles`, measure, then
adjust. **Remember that `N replicas × MAX_CONNS` is the real total.**

---

## Redis / caching

| Variable | Default | Notes |
|---|---|---|
| `REDIS_ENABLED` | `true` | `false` falls back to the in-process LRU. **With multiple replicas, each caches independently and invalidation does not propagate** |
| `REDIS_ADDR` | `localhost:6379` | |
| `REDIS_PASSWORD` | *(empty)* | |
| `REDIS_DB` | `0` | |
| `CACHE_ENABLED` | `true` | `false` injects `NoopCache` (the Null Object), so no branching appears in business logic. Useful for proving a bug is a cache bug |
| `CACHE_TTL` | `5m` | The base TTL. Terminal orders override to 1h; list pages use TTL/5 |
| `CACHE_NEGATIVE_TTL` | `30s` | For "not found". **Short on purpose**: an order that does not exist now may be created a moment later |
| `CACHE_MAX_ENTRIES` | `10000` | In-process cache only. Redis is bounded by `maxmemory` in `docker-compose.yml` |

**Eviction policy is configured in `docker-compose.yml`**, not here, because it
is a property of the Redis deployment: `maxmemory 256mb` +
`maxmemory-policy allkeys-lru`. Each alternative and its reason is in the
comments there.

---

## Locking

**The headline knob.** Change it, restart, and compare behaviour.

| Variable | Default | Notes |
|---|---|---|
| `LOCK_STRATEGY` | `hybrid` | `optimistic` \| `pessimistic` \| `hybrid` |
| `LOCK_MAX_RETRIES` | `3` | Optimistic attempts (including the first). Validated to 0..20 |
| `LOCK_RETRY_BASE` | `20ms` | Backoff base. Doubles per attempt, with ±50% jitter |
| `LOCK_PESSIMISTIC_TIMEOUT` | `3s` | **Caps how long we wait for a row lock.** Without a bound, one stuck transaction piles up every request until connections run out |

```
  optimistic    read → compute → UPDATE ... WHERE version = N
                No locks held. Cheap and concurrent, but the loser retries.
                BEST WHEN: conflicts are rare.

  pessimistic   SELECT ... FOR UPDATE, held until commit
                Others queue. No retries, no lost updates, but it serialises.
                BEST WHEN: conflicts are common, or the operation must not
                be retried (it has side effects).

  hybrid        optimistic first; escalate on PROVEN contention.
                Fast when uncontended, correct and bounded when contended.
```

**Note:** `Cancel` **always** uses a pessimistic lock regardless of this
setting, because it is a check-then-act with an irreversible side effect (a
refund). See [LOCKING.md](LOCKING.md).

---

## Saga

| Variable | Default | Notes |
|---|---|---|
| `SAGA_ENABLED` | `true` | `false` creates orders as `PENDING` with **no** stock reservation or payment |
| `SAGA_STEP_TIMEOUT` | `5s` | **Per step**, not for the whole saga. A single budget would let a slow first step starve the rest and make the failure look like it came from the wrong service |
| `SAGA_RECOVERY_INTERVAL` | `30s` | For the recovery worker (**not yet implemented** — see [TRADEOFFS.md](TRADEOFFS.md)) |

---

## Background workers

| Variable | Default | Notes |
|---|---|---|
| `WORKERS_ENABLED` | `true` | `false` means pending orders do not auto-advance and outbox events are not published |
| `WORKER_AUTO_ADVANCE_INTERVAL` | `1m` | How often the job runs. **Docker Compose sets `15s`** so it is observable during a demo |
| `WORKER_AUTO_ADVANCE_AFTER` | `5m` | How old a `PENDING` order must be. **Compose sets `1m`** |
| `WORKER_OUTBOX_INTERVAL` | `2s` | Event publishing lag |
| `WORKER_OUTBOX_BATCH` | `50` | Events per cycle. Bounds how much one run can do, so a 100,000-event backlog drains in batches rather than one enormous transaction |

---

## Downstream services

| Variable | Default | Notes |
|---|---|---|
| `INVENTORY_URL` | `http://localhost:8081` | **Empty** → use the in-process stub |
| `PAYMENT_URL` | `http://localhost:8082` | **Empty** → use the in-process stub |
| `LEGACY_URL` | `http://localhost:8083` | **Empty** → disables the strangler facade |
| `ASSISTANT_URL` | `http://localhost:8090` | Informational |
| `DEPS_TIMEOUT` | `5s` | Overall deadline per outbound call: DNS + connect + send + wait + read |

In Docker Compose these are **service names** (`http://inventory:8081`), not
`localhost` — inside the compose network a service is reachable by its name,
and `localhost` would be the container itself.

---

## Strangler Fig

| Variable | Default | Notes |
|---|---|---|
| `STRANGLER_ENABLED` | `true` | `false` → read only from the new store |
| `STRANGLER_WRITE_MODE` | `new` | `legacy` (phase 1) \| `dual` (phase 2) \| `new` (phase 3) |
| `STRANGLER_READ_FALLBACK` | `true` | `false` → **phase 4**: legacy is never consulted |
| `STRANGLER_BACKFILL_ON_READ` | `true` | Let read traffic migrate records into the new store |

### The four phases

```
  PHASE 1   WRITE_MODE=legacy                      legacy authoritative
  PHASE 2   WRITE_MODE=dual                        write both, compare
  PHASE 3   WRITE_MODE=new,  READ_FALLBACK=true    new authoritative  ← default
  PHASE 4   READ_FALLBACK=false                    legacy decommissioned
```

Every transition is an env-var change, so **rollback is instant**. See
[STRANGLER_FIG.md](STRANGLER_FIG.md).

---

## The assistant (Python)

| Variable | Default | Notes |
|---|---|---|
| `ASSISTANT_ADDR` | `0.0.0.0:8090` | |
| `ORDER_SERVICE_URL` | `http://localhost:8080` | For the order-lookup tool |
| `ASSISTANT_CORPUS_DIR` | `ai/corpus` | Markdown documents to index |
| `ASSISTANT_TOP_K` | `3` | Chunks retrieved per query |
| `ASSISTANT_LLM` | `off` | `on` switches to the real LLM generator |
| `ANTHROPIC_API_KEY` | *(empty)* | Required when `ASSISTANT_LLM=on`. Without it, the service logs a specific warning and falls back |
| `ASSISTANT_LLM_MODEL` | `claude-sonnet-5-5` | |
| `LOG_LEVEL` | `info` | |

The confidence threshold (`MIN_CONFIDENCE = 0.12`) is a **constant**, not an
env var, because it was chosen by measurement against the test corpus and
changing it should be a code change with a test run — not a production tweak.

---

## Useful combinations

### Run with nothing installed

```bash
POSTGRES_ENABLED=false REDIS_ENABLED=false STRANGLER_ENABLED=false \
INVENTORY_URL= PAYMENT_URL= LEGACY_URL= \
LOG_LEVEL=debug go run ./cmd/orderd
```

Or just `make run-nodocker`.

### Compare locking strategies

```bash
for s in optimistic pessimistic hybrid; do
  docker compose stop order
  LOCK_STRATEGY=$s docker compose up -d order
  sleep 3
  make load-test
done
```

Watch `order_version_conflicts_total` and `order_lock_escalations_total`.

### Prove a bug is a cache bug

```bash
CACHE_ENABLED=false docker compose up -d order
```

### Watch the background worker quickly

```bash
WORKER_AUTO_ADVANCE_INTERVAL=5s WORKER_AUTO_ADVANCE_AFTER=10s \
  docker compose up -d order
make watch-logs
```

### Step through the migration phases

```bash
STRANGLER_WRITE_MODE=dual docker compose up -d order        # phase 2
curl -s localhost:8080/api/v1/admin/strangler | python3 -m json.tool

STRANGLER_READ_FALLBACK=false docker compose up -d order    # phase 4
curl -i localhost:8080/api/v1/orders/number/LEG-2019-0001   # now 404
```

---

## Fail-fast validation

`Config.validate()` rejects nonsense **at boot**:

- `LOCK_STRATEGY` must be one of the three known values
- `STRANGLER_WRITE_MODE` must be one of the three known values
- `LOCK_MAX_RETRIES` must be 0..20
- `CACHE_MAX_ENTRIES` must be > 0
- `CACHE_TTL` must be > 0
- `POSTGRES_DSN` is required when `POSTGRES_ENABLED=true`
- `REDIS_ADDR` is required when `REDIS_ENABLED=true`

> **A service that boots with nonsense settings and fails later, under load, is
> far worse than one that refuses to start and tells you exactly why.**

Note the deliberate asymmetry: a *malformed* optional value (e.g.
`CACHE_TTL=banana`) falls back to its default rather than crashing, because
making every typo in every optional var fatal is operationally brittle.
`validate()` still catches values that are **semantically** wrong, which is the
dangerous class.
