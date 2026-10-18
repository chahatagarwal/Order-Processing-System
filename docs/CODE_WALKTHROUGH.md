# Code walkthrough

**Read the codebase in this order.** Each section says what the file is for,
what to look at, and the question it answers.

Budget about 90 minutes for the whole thing. If you have 15, read §1 and §2.

---

## The map

```
order-processing-system/
├── cmd/                        ◀── 4 composition roots (START HERE)
│   ├── orderd/main.go              the one worth reading in full
│   ├── inventoryd/main.go
│   ├── paymentd/main.go
│   └── legacyd/main.go
│
├── internal/
│   ├── order/                  ◀── the main service, layered
│   │   ├── domain/                 business rules. Imports NOTHING but stdlib
│   │   │   ├── order.go            the aggregate + the factory
│   │   │   ├── money.go            int64 cents, and why
│   │   │   ├── status.go           the State pattern as one table
│   │   │   ├── ports.go            the interfaces the domain REQUIRES
│   │   │   └── errors.go           sentinel errors
│   │   ├── repository/             storage adapters
│   │   │   ├── postgres.go         the 3 locking strategies as SQL
│   │   │   ├── memory.go           a fake that preserves semantics
│   │   │   └── cached.go           the Decorator
│   │   ├── service/                use cases
│   │   │   ├── order_service.go    orchestration + the locking decision
│   │   │   ├── saga.go             the distributed transaction
│   │   │   ├── adapters.go         outbound HTTP + test stubs
│   │   │   └── strangler.go        the migration facade
│   │   ├── transport/              HTTP
│   │   │   ├── http.go             handlers (deliberately thin)
│   │   │   └── dto.go              wire types
│   │   └── workers/workers.go      background jobs
│   │
│   ├── inventory/              ◀── a saga participant (PESSIMISTIC locking)
│   ├── payment/                ◀── a saga participant (IDEMPOTENCY keys)
│   ├── legacy/                 ◀── the system being strangled
│   │
│   └── platform/               ◀── shared, reused by all 4 services (DRY)
│       ├── config/                 typed, validated at boot
│       ├── constants/              every magic value, in one file
│       ├── logging/                structured, request-scoped
│       ├── errs/                   the error vocabulary
│       ├── httpx/                  server, client, middleware, responses
│       ├── cache/                  the port + 3 adapters + singleflight
│       ├── postgres/               pool, transactions, migrations, locks
│       ├── health/                 healthz vs readyz
│       ├── metrics/                a hand-rolled Prometheus registry
│       ├── retry/                  backoff + jitter
│       └── idgen/                  UUIDs and order numbers
│
├── ai/                         ◀── the RAG sidecar (Python, zero deps)
│   ├── server.py                   HTTP + the 5-step pipeline
│   ├── rag/
│   │   ├── vectorizer.py           TF-IDF + cosine, from scratch
│   │   ├── index.py                chunking + search
│   │   ├── intent.py               RAG vs tool-call routing
│   │   ├── orders.py               the order-lookup tool
│   │   └── generator.py            grounded answers + the refusal path
│   ├── corpus/                     6 policy documents
│   └── tests/test_rag.py           60 tests
│
├── scripts/                        e2e, smoke, demo, load test
├── deploy/                         Dockerfiles, postgres init
└── docs/                           this, and 9 other documents
```

---

## §1 · The composition root

**File:** [`cmd/orderd/main.go`](../cmd/orderd/main.go) · ~350 lines ·
**read it in full**

This is the single best place to start, because the entire dependency graph of
the system is one readable function.

### What to look at

**The 11 numbered steps in `run()`.** They are in dependency order:

```
 1. config      load and VALIDATE (fail fast, with a clear message)
 2. logging     + metrics, built early so later steps can report
 3. cache       Redis → in-process LRU → no-op   (3 lines to swap)
 4. repository  Postgres or in-memory, then WRAPPED with the cache decorator
 5. saga        HTTP adapters, or in-process stubs
 6. service     the use cases
 7. strangler   the migration facade
 8. workers     background jobs, with a cancellable context
 9. routing     handlers + healthz/readyz/metrics/docs
10. middleware  the chain, in a deliberate order
11. serve       flip the readiness gate, then listen
```

**Why `main()` does nothing but call `run()`:**

```go
func main() {
    if err := run(); err != nil { os.Exit(1) }
}
```

`os.Exit` skips every deferred function. If the logic were in `main`,
`defer pool.Close()` would never run on the error path.

**The decorator being applied** — one line that adds cache-aside reads,
write-invalidation, negative caching and stampede protection:

```go
repo := domain.OrderRepository(repository.NewCachedRepository(
    baseRepo, cacheImpl, log, cfg.Cache.TTL, cfg.Cache.NegativeTTL))
```

**Redis is probed but not required; Postgres is required.** Read the comments
on both. The asymmetry is the whole "fail open vs fail closed" decision.

**The middleware order, and why:**

```go
httpx.Recover(log),          // OUTERMOST: catches a panic from any inner layer
httpx.NormalizeErrors(),     // so even the ROUTER's 404s use our JSON shape
httpx.RequestID(),           // before the logger, so it has an id to attach
httpx.Logger(log),
httpx.Metrics(mtx),
httpx.Timeout(20*time.Second), // INNERMOST: so the logger still records a timeout
```

Each position is justified in a comment. Getting `Recover` wrong puts your
panic handler *inside* the thing that panics.

### The question it answers

> "How do I swap Postgres for something else?"

One line in step 4. Nothing else in the system changes, because everything
depends on `domain.OrderRepository`.

---

## §2 · The domain

**Files:** [`domain/order.go`](../internal/order/domain/order.go),
[`money.go`](../internal/order/domain/money.go),
[`status.go`](../internal/order/domain/status.go),
[`ports.go`](../internal/order/domain/ports.go)

### 2a · `money.go` — read this first

**Look at:** the header comment, then `Allocate`.

The header explains why money is `int64` cents and never a float: in IEEE-754,
`0.1 + 0.2 != 0.3`, and `19.99 * 3 == 59.97000000000001`. Multiply by a
million orders and you have an audit finding nobody can reconstruct.

`Allocate` solves the **penny problem**, which is a classic interview question:

```
Split $10.00 three ways.
  naive:  1000/3 = 333 each  →  999.  ONE CENT HAS VANISHED.
  ours:   base 333, distribute the remainder  →  [334, 333, 333] = 1000
```

`TestMoney_AllocateIsExhaustive` sweeps 200 totals × 7 split counts and asserts
the parts always sum back. A single hand-picked example can pass by luck.

Also look at `decimalPlaces`: JPY has **zero** decimal places, so formatting
¥100 as "1.00" is a visible bug.

### 2b · `status.go` — the State pattern

**Look at:** the `transitions` map, and `ValidateTransition`.

The entire state machine is **one map**:

```go
var transitions = map[Status][]Status{
    StatusPending:    {StatusProcessing, StatusCancelled, StatusFailed},
    StatusProcessing: {StatusShipped, StatusCancelled, StatusFailed},
    StatusShipped:    {StatusDelivered},
    StatusDelivered:  {},   // empty = TERMINAL
    ...
}
```

Terminality is encoded as "no outgoing edges", so the two can never disagree.

**The subtlety worth noticing:** `CanBeCancelledByCustomer()` is **stricter**
than the machine. The machine permits `PROCESSING → CANCELLED` (an admin or a
failed saga needs it); the business rule says a *customer* may only cancel while
`PENDING`. Conflating "technically possible" with "allowed for this actor" is
how authorisation bugs are born.

### 2c · `order.go` — the aggregate and the factory

**Look at:** the header comment, `NewOrder`, and `CacheTTL`.

The header states the dependency rule. Note what this file does **not** import:
no `net/http`, no `pgx`, no JSON tags. That is why the test suite runs in ~2
seconds.

`NewOrder` is a **Factory Method**, and the comment explains the real payoff:

> **If you hold an `*Order` returned from here, it is valid.**

Everything downstream can stop checking. That elimination of defensive code is
the benefit, not the object creation.

Two details worth pausing on:

- The **total is computed from the items**, never accepted from the client.
  Trusting a client total is how you get charged $0.01 for a television.
- **Duplicate product lines are merged.** A double-clicked form becomes
  quantity 4 on one line, not two lines the warehouse picks twice.

`CacheTTL` is a small idea with a large effect: the **domain** decides how long
an order may be cached, because the domain knows a terminal order is immutable.

### 2d · `ports.go` — the interfaces

**Look at:** the header, and the three write methods.

This is Ports & Adapters. The key point is the **direction**: the interface is
defined here, by the *consumer*, not in the repository package by the
implementer.

```go
type OrderWriter interface {
    Create(...)                      // no lock needed: the row does not exist
    UpdateOptimistic(...)            // compare-and-swap on version
    UpdateWithPessimisticLock(...)   // a real row lock, callback-shaped
}
```

The three methods **map exactly to the three locking strategies**, so the
choice of lock is visible at the call site rather than hidden.

Read the long comment on `OutboxEvent` for the **dual-write problem**, and the
one on `UpdateWithPessimisticLock` for why the API is a callback (a forgotten
`Unlock` would block that order for every user).

---

## §3 · The repository

**Files:** [`repository/postgres.go`](../internal/order/repository/postgres.go),
[`memory.go`](../internal/order/repository/memory.go),
[`cached.go`](../internal/order/repository/cached.go)

### 3a · `postgres.go` — the locking, as real SQL

**This is where the locking requirement is actually met.** Read these four
methods:

| Method | The SQL that matters |
|---|---|
| `Create` | one transaction across `orders`, `order_items` and `outbox` |
| `UpdateOptimistic` | `UPDATE … WHERE id=$1 AND version=$2` ← the whole mechanism |
| `UpdateWithPessimisticLock` | `SELECT … FOR UPDATE`, preceded by `SET LOCAL lock_timeout` |
| `AdvanceStalePending` | `FOR UPDATE SKIP LOCKED` in a CTE |

**Specific things to look at:**

- In `UpdateOptimistic`, the handling of `RowsAffected() == 0`. Zero rows is
  **ambiguous** — gone, or version moved? We disambiguate, because reporting
  "conflict" for a deleted order sends the client into a retry loop that can
  never succeed.
- In `UpdateWithPessimisticLock`, `SET LOCAL lock_timeout = '3s'`. Without it
  the statement waits **forever**, and one stuck transaction stalls every
  request for that row until the pool is exhausted.
- In `List`, the **N+1 fix**. The naive version runs 1 + 20 queries; we run 2,
  using `WHERE order_id = ANY($1)`. N+1 is usually invisible in development
  (latency ~0) and crippling in production.
- In `List`, `sortColumn()` is an **allow-list**. Identifiers cannot be
  parameterised, so this mapping *is* the SQL-injection control for `ORDER BY`
  — a real injection point that people miss because they only parameterise
  values.
- The secondary sort on `id`. Without a tie-breaker, rows sharing a
  `created_at` have no defined order and pagination can drop or duplicate rows.

### 3b · `memory.go` — the fake that tells the truth

**Look at:** `clone()`, `rowLock()`, and `UpdateOptimistic`.

The header states the principle: **a fake that is easier than reality gives you
green tests and a broken system.** So this adapter faithfully reproduces:

- **Version conflicts** — an explicit check where SQL has `AND version = $N`
- **Lock serialisation** — one mutex per order id, so two goroutines contending
  for the same order serialise exactly as they would in Postgres
- **Atomic outbox writes** — order, index and event all land under one mutex

`clone()` is the most important function in the file. Without it, `GetByID`
would hand back a pointer into storage, and an unsaved mutation would appear to
have committed — making the fake *more permissive* than the real database.

### 3c · `cached.go` — the Decorator

**Look at:** the header comment, `keyOrderList`, and `GetByID`.

The header makes the Open/Closed case concretely: caching was added to the
repository **without editing a line** of `PostgresRepository` or of the
service. And it is removable at 3am by not wrapping it in `main()`.

`keyOrderList` implements **generation-based group invalidation**, which is the
best idea in the file. The problem: thousands of cached list pages you cannot
enumerate. The bad answers (`KEYS` blocks Redis; `SCAN` races; `FLUSHDB` nukes
everything) and the good one (put a counter in the key and `INCR` it) are all
explained in place.

`GetByID` is the canonical cache-aside read. Follow the four steps, and note:

- **Negative caching** (`entry.NotFound`) — without it, a bot probing random
  ids reaches the database every time. That is *cache penetration*.
- **Singleflight** around the load — 2,000 concurrent misses become one query.
- The **TTL comes from the domain**, and is jittered.

Then read the write methods and the comment above them: **write first,
invalidate second**, and never on failure. The residual race is named and the
reason for accepting it is given.

---

## §4 · The service layer

**File:** [`service/order_service.go`](../internal/order/service/order_service.go)

**Look at:** the header, then `UpdateStatus`, then `Cancel`.

The header states what belongs here and what does not. The service decides
*which lock*, loads the aggregate, asks the **domain** to apply the rule, and
records metrics. It does not contain the rules themselves.

### `UpdateStatus` — the hybrid strategy

This method is the answer to "use locking based on the use case, or a hybrid
approach". Read the `switch`, then the long comment above it:

- Most of the retry budget is spent **optimistically** (fast, lock-free)
- It escalates **only** on `KindConcurrency` — a 404 or a rejected transition
  is not contention, and escalating would take a pointless lock before failing
  identically
- In `updateStatusOptimistic`, note that it **re-reads on every attempt**.
  Retrying with the same stale version fails identically every time

### `Cancel` — always pessimistic

Read the long comment. It walks through a double-click under both strategies
and shows why a lock is the right answer when the operation has an
irreversible side effect (a refund).

Also note the sequencing: compensation happens **after** the repository call
returns and the lock is released. Holding a row lock across network calls to
two other services converts their latency into your outage.

### `Create` — the sequencing decision

Read the comment at step 3. The order is committed as `PENDING` **before** any
remote call, because:

> if we called inventory and payment first and crashed between the charge and
> the save, a customer would be charged for an order that does not exist —
> invisible, unrecoverable money.

The worst case instead is a visible `PENDING` order with no reservation, which
the recovery worker can finish or compensate. **Prefer a recoverable
inconsistency over an invisible one.**

---

## §5 · The saga

**File:** [`service/saga.go`](../internal/order/service/saga.go) · read the
header in full

The header covers: the problem, why not 2PC, orchestration vs choreography, and
**the four rules every saga step must obey**.

### What to look at

**The step definitions**, and the comment on their order:

> we reserve stock BEFORE charging the card. If we charged first and found no
> stock, we would have to refund — processor fees, two lines on the customer's
> statement, and a support contact. **Order saga steps
> cheapest-to-compensate first.**

**`compensate()` runs in REVERSE order.** Later steps may depend on earlier
ones, so undoing forwards could remove a prerequisite — the same reason `defer`
unwinds in reverse.

**The most consequential eight lines in the file:**

```go
compCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ...)
```

The incoming context is **very likely already cancelled** — the step failed
because it timed out, or the client hung up. Reusing it would make every
compensation fail instantly with "context canceled", leaving stock reserved and
the card charged. `WithoutCancel` keeps the values (so `request_id` still
appears in logs) and discards the cancellation.

**`idempotencyKey(orderID, operation)`** — read the comment. A *deterministic*
key is what makes a retry safe:

```
random key:        attempt 2 looks like a NEW charge  →  customer billed twice
deterministic key: attempt 2 returns the original     →  charged once
```

**The `COMPENSATION_FAILED` branch.** We failed to undo our own work and cannot
fix it automatically. The one thing we must do is make it **impossible to
miss**: ERROR-level log with every identifier, plus a metric to alert on.
Silently swallowing it would mean a customer is charged for nothing and nobody
ever finds out.

---

## §6 · The Strangler Fig

**File:** [`service/strangler.go`](../internal/order/service/strangler.go)

**Look at:** the header (the four phases), `GetByIDTraced`, and `backfill`.

In `GetByIDTraced`, the crucial detail:

```go
if !errs.Is(err, errs.KindNotFound) {
    return nil, ServedByNew, err   // do NOT fall back
}
```

**Only a not-found justifies trying legacy.** A timeout means the new system is
*sick*, and falling back then would silently double the load on the system you
are trying to retire — at exactly the worst moment.

In `backfill`, three things:

1. It is **fire-and-forget**. A failed backfill must never fail the customer's
   read.
2. It has its **own panic recovery**. A panicking goroutine takes down the
   whole process in Go, and this one is not covered by the HTTP middleware.
3. It emits **no event**. Publishing `order.created` for a 2019 order would
   re-send confirmation emails for orders customers received years ago — the
   classic migration embarrassment.

Also read the **identifier translation** block. It documents a bug found by
running the system: a legacy reference is not a UUID, so every backfill was
failing with SQLSTATE 22P02 — silently, because backfill is fire-and-forget.
The fix, and why it is better than widening the column, are both recorded.

Finally, `HTTPLegacyAdapter.translate()` is the Adapter pattern earning its
keep: string money, a different status vocabulary, and a non-RFC3339 timestamp
all stop at this file.

---

## §7 · Transport

**Files:** [`transport/http.go`](../internal/order/transport/http.go),
[`dto.go`](../internal/order/transport/dto.go)

**Look at:** the header of `http.go`, then `setCacheHeaders`.

A handler has exactly four jobs: parse, convert, call one service method,
render. If a handler contains an `if` about a **business rule**, it is in the
wrong place.

`setCacheHeaders` is worth reading: a terminal order gets `max-age=3600`, a
live one gets `no-store`, and both are `private` — never `public`, because an
order holds personal data and must never sit in a shared CDN cache.

In `dto.go`, read the header. It makes the case for DTOs concretely, including
**mass assignment**: a request DTO that cannot express `status` or `version`
makes that attack *not compile*.

Two small details with real impact:

- `resp.Items = make([]OrderItemResponse, 0, ...)` — a nil slice marshals to
  `null`, and clients crash on `order.items.length`. Returning `[]` is the
  kind thing to do.
- `statusDescription()` — the server owns the glossary, so every client shows
  the same wording.

---

## §8 · The platform layer

Read these three in full; skim the rest.

### `platform/health/health.go`

The header explains **why a liveness probe must not check the database**: if it
did, a 30-second Postgres blip would fail liveness on every replica at once and
Kubernetes would kill them all. Your health check would have *caused* the
outage.

Then look at the `Critical` flag on `Check`. Postgres is critical; Redis is
not. That single boolean is the difference between graceful degradation and an
outage.

### `platform/cache/memory.go`

A hand-written LRU, so eviction is something you can see: a map for O(1)
lookup plus a doubly-linked list for O(1) recency. The header also draws the
**invalidation vs eviction** distinction and compares LRU, LFU, FIFO and
random.

Look at `Get()`: it takes a plain `Mutex`, not `RWMutex`, because a cache read
*moves* the entry to the front — so "reads" are writes.

### `platform/cache/singleflight.go`

Forty lines that prevent an outage. The header walks through a cache stampede:
2,000 requests miss the same expired key at once, all run the same query, and
your 10-connection pool saturates. Read `JitterTTL` too — singleflight dedups
*per key*, so it cannot save you when 10,000 different keys expire together.

### Skim these

| File | The one thing to notice |
|---|---|
| `platform/errs/errs.go` | `IsRetryable` — one function that retry, saga and circuit-breaker all consult, instead of three string-matching implementations |
| `platform/httpx/client.go` | `CircuitBreaker` as a 3-state machine, and why HALF-OPEN admits exactly **one** probe |
| `platform/httpx/middleware.go` | `NormalizeErrors` — why a catch-all route cannot fix a 405 |
| `platform/httpx/server.go` | Why graceful shutdown needs a **fresh** context (reusing the cancelled one defeats it entirely) |
| `platform/postgres/postgres.go` | `InTx` and the three bugs it fixes for every caller; `TranslateError` and why you match SQLSTATE codes, not English text |
| `platform/postgres/migrate.go` | The three things that matter: a ledger, a **checksum**, and an advisory lock |
| `platform/retry/retry.go` | The three things naive retry gets wrong, especially **jitter** |
| `platform/idgen/idgen.go` | Crockford base32 omits I, L, O and U, so an order number survives being read aloud |

---

## §9 · The other services

### `internal/inventory/inventory.go`

**Read the header.** It makes the central locking argument: inventory uses
**pessimistic** locking because stock is a shared, contended counter — the
exact opposite of the order service's workload.

Then look at `Reserve` and notice the discipline that makes a lock on a hot row
acceptable: the critical section is **pure arithmetic**. No I/O, no network, no
blocking logging. Microseconds, never across anything slow.

Also note the **check-all-then-mutate-all** structure: a partial reservation
would hold stock for an order that cannot be fulfilled.

### `internal/payment/payment.go`

**Read the header**, which states the scenario that makes idempotency
non-negotiable: the charge succeeds and the *response* is lost.

Look at:
- The idempotency check happening **first**, before any state change
- `RequestFingerprint` — the same key with *different* data is a client bug, and
  returning the first result silently would mean the second charge never happens
- **Failures are recorded too.** Not recording a decline is a classic bug: the
  retry succeeds where the original failed, and the two systems disagree
- The double-refund guard, which is the mirror image of double-charging

### `internal/legacy/legacy.go`

Read the header's list of deliberate quirks. Each is a translation problem the
adapter has to solve. `AWAIT_FAX` exists specifically to exercise the defensive
default in `fromLegacyState`.

---

## §10 · The AI layer

**Files:** [`ai/rag/*.py`](../ai/rag/), [`ai/server.py`](../ai/server.py)

### Read in this order

**1. `intent.py`** — the key architectural idea, and the one most often got
wrong:

```
"What is your refund policy?"  → written down → RETRIEVE (RAG)
"Where is ORD-7K2M9QX4?"       → a live fact  → TOOL CALL
```

A system that treats everything as RAG answers the second by retrieving the
*shipping policy*. Also read `_is_question_not_command` — it fixes a real
misclassification found by running the classifier: "can I cancel my order?" is
a *question about a rule*, not a command.

**2. `vectorizer.py`** — TF-IDF in ~200 lines. The header explains why we
wrote it instead of importing a model, **and states the limitation plainly**:
it matches words, not meaning. Read `_l2_normalise` and the note on why
normalising is a correctness requirement, not an optimisation.

Also read the stop-word list comment: removing the interrogatives ("when",
"how") measurably **broke** retrieval, because policy headings are full of
them. That is a good illustration of measuring a retrieval change rather than
reasoning about it.

**3. `index.py`** — chunking, which the header calls the most
under-appreciated decision in RAG. We split on **markdown headings**, i.e. the
author's own semantic boundaries, with overlap at the seams. The
`VectorIndex` header states the threshold (~10k chunks) at which brute force
stops being the right answer.

**4. `generator.py`** — the hallucination guard:

```python
if not results or best_score < MIN_CONFIDENCE:
    return Answer(text="I do not have information about that...", grounded=False)
```

Read the comment above `MIN_CONFIDENCE` for how the number was chosen, and the
`LLMGenerator.SYSTEM_PROMPT` even though it does not run by default — **every
rule in it is annotated with the failure mode it prevents**, including prompt
injection.

**5. `orders.py`** — the tool. Note `OrderFacts` has **no field** for an email
or address, so a careless prompt cannot leak them. Designing the limit into the
data structure beats instructing a model to be careful.

**6. `server.py`** — `Assistant.ask()` is the whole pipeline in five steps.

---

## §11 · The tests

Read the test files as documentation of intent.

| File | The tests worth reading |
|---|---|
| `domain/domain_test.go` | `TestMoney_AllocateIsExhaustive` (property-style); `TestStatus_IllegalTransitionsAreBlocked` (each case names the bug it prevents) |
| `service/service_test.go` | `TestSaga_PaymentDeclinedReleasesReservedStock` (the headline behaviour); `TestCancel_DoubleClickIsSafe` (10 concurrent cancels, **zero** errors) |
| `repository/repository_test.go` | The **contract suite** run against both adapters; `pessimistic locking serialises concurrent writers` — 20 goroutines each append one character, and the result must be *exactly* 20 long |
| `transport/http_test.go` | Real HTTP through the real middleware chain |
| `cache/cache_test.go` | `TestSingleflight_CollapsesConcurrentMisses` — 100 goroutines, loader runs **exactly once**; `TestMemory_ReturnsACopyNotTheStoredSlice` |
| `ai/tests/test_rag.py` | `test_off_topic_queries_score_below_the_threshold` — the most important test in the AI layer |

Also read the comment in `TestPostgres_OutboxSkipLockedWithinOneTransaction`,
which records a test that was **wrong** and why: it asserted a guarantee the
production code never claimed to make.

---

## The five questions this code answers

| Question | Where |
|---|---|
| "How do you prevent two requests overwriting each other?" | `repository/postgres.go` → `UpdateOptimistic`, and `service/order_service.go` → `UpdateStatus` |
| "How do you keep data consistent across services without 2PC?" | `service/saga.go`, and the `outbox` table |
| "How do you cache without serving stale data?" | `repository/cached.go` → write-then-invalidate, and generation-based group invalidation |
| "How do you migrate off a legacy system without a rewrite?" | `service/strangler.go` → four phases, each an env-var change |
| "How do you stop an LLM making things up?" | `ai/rag/generator.py` → retrieve, ground, cite, and **refuse** below a threshold |
