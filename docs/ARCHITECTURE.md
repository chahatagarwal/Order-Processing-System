# Architecture

This document explains **what the system is** and **why it is shaped this
way**. Read it alongside [CODE_WALKTHROUGH.md](CODE_WALKTHROUGH.md), which
takes you through the code in reading order.

---

## 1. The system at a glance

```
                              ┌──────────────┐
                              │    Client    │
                              │ (curl / web) │
                              └──────┬───────┘
                                     │ HTTPS/JSON
                 ┌───────────────────┼───────────────────┐
                 │                   │                   │
                 ▼                   ▼                   ▼
      ┌────────────────────┐  ┌─────────────┐  ┌──────────────────┐
      │   ORDER SERVICE    │  │  ASSISTANT  │  │   (Prometheus)   │
      │   Go · :8080       │  │ Python·8090 │  │   scrapes        │
      │                    │  │  RAG chat   │  │   /metrics       │
      │  THE SYSTEM OF     │  └──────┬──────┘  └──────────────────┘
      │  RECORD FOR ORDERS │         │
      └─┬────┬────┬────┬───┘         │ read-only
        │    │    │    │             │ lookup
        │    │    │    └─────────────┘
        │    │    │
        │    │    └──────────────────────┐
        │    │                           │
        │    ▼                           ▼
        │ ┌────────────────┐    ┌──────────────────┐
        │ │   POSTGRES     │    │      REDIS       │
        │ │   :5432        │    │      :6379       │
        │ │                │    │                  │
        │ │ source of      │    │ shared cache     │
        │ │ truth +        │    │ maxmemory 256mb  │
        │ │ outbox +       │    │ allkeys-lru      │
        │ │ advisory locks │    │ (safe to lose)   │
        │ └────────────────┘    └──────────────────┘
        │
        │  saga participants (HTTP, with idempotency keys)
        ├──────────────────┬──────────────────┬─────────────────┐
        ▼                  ▼                  ▼                 │
┌───────────────┐  ┌───────────────┐  ┌───────────────┐         │
│  INVENTORY    │  │   PAYMENT     │  │    LEGACY     │         │
│  Go · :8081   │  │   Go · :8082  │  │   Go · :8083  │         │
│               │  │               │  │               │         │
│ reserve /     │  │ charge /      │  │ the OLD order │         │
│ release       │  │ refund        │  │ system being  │         │
│               │  │               │  │ STRANGLED     │         │
│ pessimistic   │  │ idempotency   │  │               │         │
│ locking on    │  │ keys prevent  │  │ holds orders  │         │
│ hot stock     │  │ double charge │  │ that exist    │         │
│ counters      │  │               │  │ nowhere else  │         │
└───────────────┘  └───────────────┘  └───────┬───────┘         │
                                              │                 │
                                              └─ read fallback ──┘
                                                 + backfill
```

**Why five services and not one?** Because three of the requirements —
saga compensation, the Strangler Fig migration, and contrasting locking
strategies — are *only meaningful across a process boundary*. A saga inside one
process is just a function with a `defer`. The cost is real (more moving parts,
a network to fail), and it is accepted deliberately for that reason.

---

## 2. Layering inside the order service

This is the most important diagram in the document. **Every arrow points
inwards.**

```
  ┌─────────────────────────────────────────────────────────────────┐
  │  TRANSPORT            internal/order/transport                   │
  │  ─────────────────────────────────────────────────────────────   │
  │  HTTP handlers, DTOs, status codes, routing                      │
  │                                                                  │
  │  Knows about: net/http, JSON                                     │
  │  Does NOT know: SQL, Redis, business rules                       │
  └────────────────────────────┬────────────────────────────────────┘
                               │ calls
                               ▼
  ┌─────────────────────────────────────────────────────────────────┐
  │  SERVICE              internal/order/service                     │
  │  ─────────────────────────────────────────────────────────────   │
  │  Use-case orchestration · locking strategy · saga coordination   │
  │                                                                  │
  │  Knows about: the domain, the ports it depends on                │
  │  Does NOT know: HTTP, SQL, Redis                                 │
  └────────────────────────────┬────────────────────────────────────┘
                               │ depends on INTERFACES
                               ▼
  ┌─────────────────────────────────────────────────────────────────┐
  │  DOMAIN               internal/order/domain          ◀── the core │
  │  ─────────────────────────────────────────────────────────────   │
  │  Order aggregate · Money · Status state machine · PORTS          │
  │                                                                  │
  │  Imports: the Go standard library. NOTHING ELSE.                 │
  │                                                                  │
  │  This is why the whole test suite runs in ~2 seconds with no      │
  │  database and no Docker.                                          │
  └────────────────────────────▲────────────────────────────────────┘
                               │ implements the ports
         ┌─────────────────────┴─────────────────────┐
         │                                           │
  ┌──────────────────────────┐          ┌───────────────────────────┐
  │  REPOSITORY              │          │  ADAPTERS                 │
  │  internal/.../repository │          │  internal/.../service     │
  │  ──────────────────────  │          │  ───────────────────────  │
  │  CachedRepository        │          │  HTTPInventoryAdapter     │
  │        ↓ wraps           │          │  HTTPPaymentAdapter       │
  │  PostgresRepository      │          │  HTTPLegacyAdapter        │
  │  MemoryRepository        │          │  StubInventory/Payment    │
  └──────────────────────────┘          └───────────────────────────┘
```

### What this buys, concretely

| Property | Because of |
|---|---|
| Tests run in 2s with no Docker | The domain imports nothing; storage is behind an interface |
| Swap Postgres → MongoDB | One new file implementing `OrderRepository` |
| Add a gRPC API | One new transport package; zero business-logic change |
| Caching can be removed at 3am | Stop wrapping the decorator in `main()` |
| Run with no infrastructure | `POSTGRES_ENABLED=false` swaps in the in-memory adapter |

### The dependency rule, stated plainly

A package may only import packages **inwards** of it. The compiler enforces
this, because the domain has no import of `net/http` or `pgx` to misuse. If you
ever find yourself wanting to `import "net/http"` inside the domain, the design
is telling you the logic belongs in a different layer.

---

## 3. Request flow: creating an order

The numbered steps correspond to real functions you can read.

```
 1. POST /api/v1/orders
        │
        ▼
 2. MIDDLEWARE CHAIN  (internal/platform/httpx/middleware.go)
    Recover → NormalizeErrors → RequestID → Logger → Metrics → Timeout
        │     ▲ panic→500      ▲ one error  ▲ trace  ▲ p99     ▲ bounded
        │                        shape                           deadline
        ▼
 3. HANDLER  (transport/http.go)
    DecodeJSON (bounded size, unknown fields rejected)
    CreateOrderRequest.ToDomain()
        │
        ▼
 4. SERVICE  (service/order_service.go)
    domain.NewOrder(...)  ◀── the FACTORY validates everything
        │                     and computes the total server-side
        ▼
 5. REPOSITORY WRITE — ONE TRANSACTION
    ┌──────────────────────────────────────┐
    │ BEGIN                                │
    │   INSERT INTO orders                 │
    │   INSERT INTO order_items (batched)  │
    │   INSERT INTO outbox       ◀─────────┼── atomic with the state change
    │ COMMIT                               │
    └──────────────────────────────────────┘
        │
        │   The order is now DURABLE as PENDING, before any remote call.
        │   WHY: if we called payment first and crashed before saving, a
        │   customer would be charged for an order that does not exist —
        │   invisible, unrecoverable money. A PENDING order with no
        │   reservation is visible and recoverable.
        ▼
 6. SAGA  (service/saga.go)
    ┌─────────────────────┬──────────────────┬───────────────────┐
    │ reserve_inventory   │ charge_payment   │ confirm_order     │
    │ → inventory:8081    │ → payment:8082   │ → local tx        │
    │ undo: release       │ undo: refund     │ (nothing after)   │
    └─────────────────────┴──────────────────┴───────────────────┘
        │
        │  On failure at ANY step: compensate completed steps IN REVERSE,
        │  then mark the order FAILED. Return the ORIGINAL error.
        ▼
 7. CACHE INVALIDATION  (repository/cached.go)
    DELETE order:v1:id:<id>, order:v1:num:<number>
    INCR   order:v1:list:generation   ◀── invalidates every cached list
                                          page in ONE atomic operation
        │
        ▼
 8. 201 Created + Location + the order JSON
```

### Meanwhile, asynchronously

```
  OUTBOX WORKER (every 2s)                AUTO-ADVANCE WORKER (every 1m)
  ────────────────────────                ──────────────────────────────
  advisory lock (efficiency)              advisory lock (CORRECTNESS:
       │                                  only one replica may run)
       ▼                                       │
  SELECT ... WHERE published_at IS NULL         ▼
    FOR UPDATE SKIP LOCKED                 UPDATE orders SET
       │  ▲                                  status='PROCESSING'
       │  └── lets N workers claim           WHERE status='PENDING'
       │      DISJOINT batches                 AND status_changed_at <= cutoff
       ▼                                       FOR UPDATE SKIP LOCKED
  publish → MarkPublished (batched)             │
                                                ▼
  Guarantee: AT-LEAST-ONCE.                 one set-based statement, not
  Consumers MUST be idempotent              100,000 round-trips
  on event id.
```

---

## 4. The data model

```
┌─────────────────────────────┐
│ orders                      │
├─────────────────────────────┤
│ id                 UUID PK  │  app-generated: no volume leak, not guessable
│ order_number       UNIQUE   │  ORD-7K2M9QX4 (Crockford base32: no I/L/O/U)
│ customer_id, _email, _name  │
│ status             CHECK    │  the state machine, enforced in the DB too
│ total_amount_minor BIGINT   │  ◀── CENTS. Never float. Never NUMERIC.
│ currency           CHAR(3)  │
│ version            INTEGER  │  ◀── OPTIMISTIC LOCKING lives here
│ cancel_reason               │  separate from failure_reason: customer
│ failure_reason              │  choice vs system failure are different
│ saga_id            UUID     │  links a stuck order to its saga
│ created_at         TIMESTAMPTZ
│ updated_at         TIMESTAMPTZ
│ status_changed_at  TIMESTAMPTZ  ◀── separate from updated_at ON PURPOSE:
│ source             CHECK       │   editing a note must not reset the
└──────────┬──────────────────┘   │   worker's staleness clock
           │ 1:N  ON DELETE CASCADE
           ▼
┌─────────────────────────────┐
│ order_items                 │
├─────────────────────────────┤
│ id                 UUID PK  │
│ order_id           FK       │
│ product_id                  │
│ product_name                │  ◀── DENORMALISED SNAPSHOT, deliberately.
│ unit_price_minor            │      An order is a HISTORICAL RECORD. If the
│ line_total_minor            │      product is renamed, repriced or deleted,
│ quantity           CHECK >0 │      last month's invoice must not change.
│ UNIQUE (order_id,product_id)│      Joining live data would rewrite history.
└─────────────────────────────┘

┌─────────────────────────────┐    ┌──────────────────────────────┐
│ outbox                      │    │ saga_instances               │
├─────────────────────────────┤    ├──────────────────────────────┤
│ id            TEXT PK       │    │ id             UUID PK       │
│ aggregate_id                │    │ order_id                     │
│ event_type                  │    │ status         CHECK         │
│ payload       JSONB         │    │ completed_steps JSONB        │
│ published_at  NULL=pending  │    │ last_error                   │
│ attempts      poison guard  │    └──────────────────────────────┘
└─────────────────────────────┘      Without this row, a crash mid-saga is
  Written in the SAME TX as the       UNRECOVERABLE: nobody knows whether
  state change it describes.          the card was charged.
```

### Indexes, and why each one exists

| Index | Serves | Note |
|---|---|---|
| `(customer_id, created_at DESC)` | "my orders, newest first" | Composite, so filter **and** sort come from one scan |
| `(status_changed_at) WHERE status='PENDING'` | the auto-advance worker | **PARTIAL.** 99% of orders are DELIVERED and never match, so indexing them is wasted. This index *shrinks* as orders complete |
| `(status, created_at DESC)` | `?status=X`, stats `GROUP BY` | |
| `(created_at) WHERE published_at IS NULL` | the outbox poller | **PARTIAL.** Its size tracks the *backlog*, not the history, so polling stays constant-time forever |
| `(started_at) WHERE status IN ('RUNNING','COMPENSATING')` | saga recovery | **PARTIAL.** Finds crashed sagas without scanning millions of completed ones |

Partial indexes are the single highest-leverage, least-used Postgres feature
for the "hot subset of a big table" pattern, which is exactly what a queue or
a work backlog is.

---

## 5. Caching architecture

Three independent layers, cheapest first:

```
  ┌──────────────────────────────────────────────────────────────┐
  │ LAYER 1 · HTTP caching                                        │
  │ Cache-Control: private, max-age=3600   (terminal orders)      │
  │ ETag: "7"                               (the version)         │
  │                                                                │
  │ The request never reaches us at all. Free.                     │
  │ `private`, never `public`: an order holds personal data and    │
  │ must never sit in a shared CDN cache.                          │
  └────────────────────────────┬─────────────────────────────────┘
                               │ on a miss
                               ▼
  ┌──────────────────────────────────────────────────────────────┐
  │ LAYER 2 · Redis (shared across replicas)                      │
  │                                                                │
  │  CACHE-ASIDE:  read → miss → DB → populate → return            │
  │  ON WRITE:     write DB → DELETE the key (never UPDATE it)     │
  │                                                                │
  │  ┌─ singleflight ──────────────────────────────────────────┐   │
  │  │ 2,000 concurrent misses for one key → ONE DB query       │   │
  │  │ Without it, one expired hot key saturates the pool and   │   │
  │  │ takes the service down. That is a cache stampede.        │   │
  │  └──────────────────────────────────────────────────────────┘   │
  │                                                                │
  │  TTLs are matched to VOLATILITY, not set globally:              │
  │    terminal order   1h    (immutable — zero staleness risk)    │
  │    live order       5m                                         │
  │    list page        1m    (ANY order's change can stale it)    │
  │    stats            5m    (expensive, most tolerant of lag)    │
  │    "not found"      30s   (negative caching, short on purpose) │
  │  Every TTL is JITTERED ±10%, so keys written together do not   │
  │  all expire in the same millisecond.                           │
  └────────────────────────────┬─────────────────────────────────┘
                               │ on a miss
                               ▼
  ┌──────────────────────────────────────────────────────────────┐
  │ LAYER 3 · PostgreSQL — the source of truth                    │
  └──────────────────────────────────────────────────────────────┘
```

### Invalidation vs eviction — the distinction that gets probed

|  | **Invalidation** | **Eviction** |
|---|---|---|
| Why | The data is **wrong** | We are **out of room** |
| Decided by | Our code, deliberately | The cache, automatically |
| Trigger | A write happened | `maxmemory` reached |
| Here | `DELETE` key + `INCR` generation | `allkeys-lru` |

Confusing them causes real mistakes: raising `maxmemory` does not fix stale
data, and shortening TTLs does not fix memory pressure.

### Generation-based group invalidation

The problem: thousands of cached list pages, any of which a single write could
invalidate. You cannot enumerate them.

```
  BAD:  KEYS order:v1:list:*     → O(n) over the WHOLE keyspace, and it
                                   BLOCKS single-threaded Redis. This is how
                                   you cause an outage while fixing a bug.
  BAD:  SCAN in a loop           → many round-trips, races with writes
  BAD:  FLUSHDB                  → throws away everyone else's cache

  OURS: every list key embeds a generation number:
            order:v1:list:g7:status=PENDING|limit=20|offset=0

        On any write:  INCR order:v1:list:generation   →  g8

        Every g7 key is now UNREACHABLE. One atomic O(1) operation,
        regardless of how many keys exist. The orphans are reclaimed by
        TTL expiry and LRU eviction — which is already eviction's job.
```

Full detail, including the five cache patterns and why we chose cache-aside, is
in [CACHING.md](CACHING.md).

---

## 6. Locking architecture

Two services, two opposite strategies, for one reason: **the workloads differ**.

```
  ORDER SERVICE                          INVENTORY SERVICE
  ─────────────                          ─────────────────
  Each request touches ITS OWN           5,000 req/s all touch THE SAME
  order row. Conflicts are rare.         stock row for a popular SKU.

  → OPTIMISTIC first (lock-free,         → PESSIMISTIC always. Requests
    high throughput), ESCALATING           queue for microseconds and
    to pessimistic under proven            succeed deterministically.
    contention.                            Optimistic here would thrash.
```

### The hybrid strategy, in detail

```
  UpdateStatus(id, SHIPPED)
        │
        ▼
  ┌─────────────────────────────────────────────────────────┐
  │ ATTEMPT 1-2: OPTIMISTIC                                  │
  │   read order (version 3)                                 │
  │   apply the domain rule                                  │
  │   UPDATE ... WHERE id=? AND version=3                    │
  │                                                           │
  │   0 rows? Someone committed first. Re-READ and retry     │
  │   (retrying with the same stale version is pointless).   │
  │   Backoff is exponential WITH JITTER, so 500 losers do    │
  │   not all retry in the same millisecond.                  │
  └───────────────────────┬─────────────────────────────────┘
                          │ still conflicting? That is EVIDENCE
                          │ of real contention, not bad luck.
                          ▼
  ┌─────────────────────────────────────────────────────────┐
  │ FINAL ATTEMPT: ESCALATE TO PESSIMISTIC                   │
  │   SET LOCAL lock_timeout = '3s'   ◀── ALWAYS bound it    │
  │   SELECT ... WHERE id=? FOR UPDATE                       │
  │   apply the rule (state cannot change under us)          │
  │   UPDATE                                                 │
  │   COMMIT   ← the lock is released here, automatically    │
  └─────────────────────────────────────────────────────────┘

  Result: fast and lock-free when uncontended; QUEUES rather than
  FAILS when contended. The alternative (pure optimism) returns 409s
  to users for a problem the server could have resolved.
```

### Cancellation always locks

Cancellation ignores `LOCK_STRATEGY` and always takes a row lock, because it is
a **check-then-act with irreversible side effects** (a refund). Under a
double-click, a lock turns a race into a queue: request 2 waits, reads
`CANCELLED`, and returns "already cancelled" — one refund, not two.

The general rule: **when an operation has external side effects that cannot be
rolled back, serialise rather than detect-and-retry.**

Full detail, including deadlock avoidance and isolation levels, is in
[LOCKING.md](LOCKING.md).

---

## 7. Design patterns used

Grouped by GoF category, with the file to read for each.

### Creational

| Pattern | Where | Why |
|---|---|---|
| **Factory Method** | `domain.NewOrder` | Makes an invalid order *inexpressible*. If you hold an `*Order` from here, it is valid — so nothing downstream needs defensive re-checking |
| **Builder-ish** | `domain.NewOrderInput` | A parameter object; the input type cannot express `status` or `version`, so mass-assignment attacks do not compile |
| **Singleton (scoped)** | `cmd/*/main.go` | One pool, one cache, one registry per process — created once in the composition root, never a global |

### Structural

| Pattern | Where | Why |
|---|---|---|
| **Decorator** | `repository.CachedRepository` | Adds caching to **any** repository without editing it. Open/Closed, demonstrated: `Cached(Postgres(db))` |
| **Adapter** | `service.HTTPInventoryAdapter`, `HTTPLegacyAdapter` | Translates a remote API's shape into our domain's vocabulary. The legacy system's string money and odd statuses never leak past one file |
| **Facade** | `service.StranglerFacade` | Hides "new store or legacy system?" behind one interface the handler cannot see through |
| **Proxy / Null Object** | `cache.NoopCache` | `CACHE_ENABLED=false` needs **zero** `if` statements elsewhere |

### Behavioural

| Pattern | Where | Why |
|---|---|---|
| **State** | `domain.Status` + `transitions` map | The state machine is ONE map in ONE place. Adding a status means editing one table, not hunting five call sites |
| **Strategy** | `config.LockStrategy`, `Generator` | Swap locking or answer generation by configuration, with no branching in business logic |
| **Command** | `service.SagaStep` | Modelling a step as data (two functions) is what lets the orchestrator log, time, retry and compensate every step uniformly |
| **Observer** | `workers.LogPublisher.Subscribe` | In-process event subscribers without a broker |
| **Template Method** | `postgres.Pool.InTx` | The transaction skeleton is written once; callers supply only the body. Fixes forgotten rollbacks for every caller at once |
| **Chain of Responsibility** | `httpx.Chain` | Middleware composition |

### Architectural

| Pattern | Where |
|---|---|
| **Repository** | `internal/order/repository` |
| **Service Layer** | `internal/order/service` |
| **Ports & Adapters** | `domain/ports.go` declares; infrastructure implements |
| **DTO** | `transport/dto.go` |
| **Transactional Outbox** | `outbox` table + `workers.OutboxWorker` |
| **Saga (orchestrated)** | `service/saga.go` |
| **Strangler Fig** | `service/strangler.go` |
| **Circuit Breaker** | `httpx.CircuitBreaker` |
| **Singleflight** | `cache.Group` |

---

## 8. The AI layer

```
  POST /api/v1/assistant/chat  {"question": "..."}
        │
        ▼
  ┌──────────────────────────────────────────────────────────┐
  │ 1. CLASSIFY INTENT   (ai/rag/intent.py)                   │
  │                                                            │
  │    THE KEY ARCHITECTURAL IDEA. Two kinds of question:      │
  │                                                            │
  │    "What is your refund policy?"  → written down → RAG     │
  │    "Where is ORD-7K2M9QX4?"       → a live fact → TOOL     │
  │                                                            │
  │    A system that treats everything as RAG answers the      │
  │    second by retrieving the shipping policy and            │
  │    confidently explaining typical delivery times — which   │
  │    is not what was asked and is indistinguishable from a   │
  │    correct answer to someone who does not already know.    │
  └────────────┬──────────────────────────────┬──────────────┘
               │ ORDER_STATUS                 │ POLICY_QUESTION
               ▼                              ▼
  ┌────────────────────────┐    ┌──────────────────────────────┐
  │ 2a. TOOL CALL           │    │ 2b. RETRIEVE                  │
  │ GET /api/v1/orders/...  │    │ TF-IDF → cosine similarity    │
  │                         │    │ → top-k chunks                │
  │ READ-ONLY, deliberately │    │                               │
  │ (a chat message is not  │    │ chunked on MARKDOWN HEADINGS, │
  │ authenticated, so it     │    │ i.e. the author's own          │
  │ must not mutate state)  │    │ semantic boundaries           │
  │                         │    │                               │
  │ Projects to ONLY the    │    │                               │
  │ fields allowed in an    │    │                               │
  │ answer — no email, no   │    │                               │
  │ address, by construction│    │                               │
  └───────────┬─────────────┘    └──────────────┬───────────────┘
              └──────────────┬──────────────────┘
                             ▼
  ┌──────────────────────────────────────────────────────────┐
  │ 3. GENERATE, GROUNDED   (ai/rag/generator.py)             │
  │                                                            │
  │    ┌─ THE HALLUCINATION GUARD ───────────────────────────┐ │
  │    │ best cosine score < 0.12  →  REFUSE TO ANSWER        │ │
  │    │                                                      │ │
  │    │ "I don't have information about that" is a GOOD      │ │
  │    │ answer. A fluent wrong one is worse than useless,    │ │
  │    │ because nobody catches it.                           │ │
  │    └──────────────────────────────────────────────────────┘ │
  │                                                            │
  │    Every grounded answer carries a CITATION, so it can be │
  │    verified. An answer you cannot check is just an        │
  │    assertion.                                             │
  └──────────────────────────────────────────────────────────┘
```

**Offline and deterministic by default.** `TemplateGenerator` composes the
answer from retrieved sentences — no API key, no network, no cost, and every
answer is reproducible in a test. `LLMGenerator` is the documented swap-in
(one line in `server.py`), and its system prompt is worth reading even
unexecuted: each rule is annotated with the failure mode it prevents,
including prompt injection.

Full detail in [AI_RAG.md](AI_RAG.md).

---

## 9. Observability

```
  EVERY log line carries request_id, so one customer complaint = one grep,
  ACROSS ALL FIVE SERVICES (the id is forwarded on every outbound call).

  Log levels are chosen so that `level>=warn` is a meaningful alert filter:
    ERROR  5xx, panics, FAILED COMPENSATIONS ◀── page someone
    WARN   4xx (their mistake), degraded modes
    INFO   business events (order created, status changed)
    DEBUG  cache hits, saga step detail

  /metrics — the series that matter most:
    saga_compensation_failed_total   ◀── ALERT. A customer may be charged
                                         for an order they will not receive.
    cache_hit_ratio                  ◀── <0.8 on a read-heavy load means the
                                         TTL is too short or keys too granular
    order_lock_escalations_total      ◀── real contention, not theory
    http_request_duration_seconds     ◀── a HISTOGRAM, so you get p99.
                                         An average hides the tail: 99 requests
                                         at 1ms and one at 10s averages 100ms,
                                         which NO user experienced.

  CARDINALITY DISCIPLINE: we label by ROUTE PATTERN ("/orders/{id}"), never
  by raw URL. Labelling by URL creates one time series per order id — millions
  of series, and you have taken down your own Prometheus.
```

---

## 10. Failure modes, and what happens

| What fails | What happens | Why that is the right behaviour |
|---|---|---|
| **Redis dies** | Reads go to Postgres. `/readyz` says `degraded` but returns **200**, so the instance stays in the load balancer | A cache is an optimisation. Losing it must cost **latency only**. Pulling instances over a cache outage would take the site down to protect a speed-up |
| **Postgres dies** | `/readyz` returns **503**, the instance leaves the LB, the process keeps running and retries | Restarting cannot fix someone else's database. Readiness (not liveness) is the correct lever |
| **Inventory/payment down** | The saga fails, compensations run, the order is `FAILED`, the circuit breaker opens to stop hammering a struggling service | Failing fast is kinder to both sides than queueing on timeouts |
| **Payment times out after charging** | The retry carries the **same idempotency key**, so the payment service returns the original result and charges nothing more | The only correct way to retry a non-idempotent operation |
| **A saga crashes mid-flight** | The order is visibly `PENDING` with a `saga_id`; the reservation expires by TTL and the stock returns | The *recoverable* inconsistency was chosen over the invisible one |
| **Compensation itself fails** | `COMPENSATION_FAILED`, logged at ERROR with every identifier, and a metric to alert on | There is nothing to compensate a failed compensation with. A human must reconcile, so it must be impossible to miss |
| **The order service is killed mid-deploy** | SIGTERM → stop accepting → drain in-flight requests → run cleanup hooks | Without this, a customer who just clicked "Place order" gets a connection reset and does not know if they were charged |
| **A background worker overruns its interval** | The next tick **skips** (atomic flag) rather than running concurrently | Two copies processing the same batch is worse than running late |
| **Three replicas all run the cron** | A Postgres **advisory lock** makes the job a cluster singleton | Triple work, and triple the chance of a duplicated side effect |
| **The assistant is down** | Orders are completely unaffected; nothing in the order service's `/readyz` depends on it | The AI layer is explicitly non-critical, and the topology enforces that |

---

## 11. Where to start reading

1. **`cmd/orderd/main.go`** — the composition root. The entire dependency
   graph of the system is one readable function.
2. **`internal/order/domain/order.go`** — the business rules, with no
   infrastructure in sight.
3. **`internal/order/service/order_service.go`** — the use cases and the
   locking decisions.
4. **`internal/order/repository/postgres.go`** — the three locking strategies
   as actual SQL.
5. **`internal/order/service/saga.go`** — the distributed transaction.

[CODE_WALKTHROUGH.md](CODE_WALKTHROUGH.md) does this in detail, with the
specific lines worth pausing on.
