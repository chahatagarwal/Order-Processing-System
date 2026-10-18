# Locking

Why this system uses three different locking strategies, how each one works,
and when each is correct.

---

## The problem all locking solves: the lost update

Two support agents open the same order.

```
  time   AGENT A                          AGENT B
  ────   ───────────────────────────      ───────────────────────────
   t1    read order (notes = "")
   t2                                     read order (notes = "")
   t3    write notes = "call customer"
   t4                                     write notes = "gift wrap"

  RESULT: A's note is GONE.
          No error. No warning. No trace in any log.
```

This is the **lost update**, and it is the quiet one — nothing fails, so nobody
investigates. A's note simply never existed as far as the system is concerned.

Every strategy below is a different answer to the same question: *how do we
stop that?*

---

## Strategy 1 — Optimistic locking

**The bet:** collisions are rare, so do not pay for a lock. Detect the
collision instead, and retry.

### How it works

Every row carries a `version` column. Every write includes it:

```sql
UPDATE orders
   SET status = 'SHIPPED',
       version = version + 1
 WHERE id = 'abc'
   AND version = 3;        -- ◀── THIS is the entire mechanism
```

If B committed first, the stored version is now 4, the `WHERE` matches **zero
rows**, and `RowsAffected() == 0`. We did not overwrite anything — we *detected*
that we would have.

Replaying the trace above:

```
  time   AGENT A                          AGENT B
  ────   ───────────────────────────      ───────────────────────────
   t1    read (version 3)
   t2                                     read (version 3)
   t3    UPDATE ... WHERE version=3
         → 1 row. Committed. Now v4.
   t4                                     UPDATE ... WHERE version=3
                                          → 0 ROWS. Conflict detected.
   t5                                     RE-READ (v4), re-apply, retry
                                          → succeeds, now v5.

  RESULT: both changes survive.
```

### Where it lives

`internal/order/repository/postgres.go` → `UpdateOptimistic`
`internal/order/service/order_service.go` → `updateStatusOptimistic`

### The one subtlety people get wrong

**You must RE-READ on every retry.** Retrying with the same stale version fails
identically every time, burning the whole retry budget to no purpose. A retry
is only meaningful if it observes the new state first — which is why the retry
closure in `updateStatusOptimistic` begins with `repo.GetByID`.

### Zero rows is ambiguous

Zero rows affected means *either* "the version moved" *or* "the row is gone".
Reporting a conflict for a deleted order sends the client into a retry loop that
can never succeed, so we disambiguate with a follow-up read and return
`404` vs `409` accordingly.

### Trade-offs

| | |
|---|---|
| ✅ | No locks held while the service thinks. Readers are never blocked |
| ✅ | **Deadlock is impossible** — there is nothing to deadlock on |
| ✅ | Works across stateless replicas with no coordination |
| ✅ | A client can participate: we expose `version` as the `ETag` |
| ❌ | The loser must redo its work |
| ❌ | Under heavy contention on one row, retries dominate and waste CPU |
| ❌ | After the budget runs out, the user gets a `409` for a problem the server could have resolved |

**Use it when:** conflicts are rare — which is the overwhelmingly common case.
Two people rarely touch the same order in the same instant.

---

## Strategy 2 — Pessimistic locking

**The bet:** collisions are likely, or the operation must not be retried. Take
the lock up front and let everyone else queue.

### How it works

```sql
BEGIN;
  SET LOCAL lock_timeout = '3s';            -- ◀── never omit this

  SELECT * FROM orders
   WHERE id = 'abc'
     FOR UPDATE;                            -- ◀── the lock is taken HERE

  -- Any other transaction doing FOR UPDATE on this row now WAITS.
  -- Plain SELECTs are NOT blocked: Postgres uses MVCC, so readers
  -- see the last committed version.

  UPDATE orders SET ... WHERE id = 'abc';
COMMIT;                                     -- ◀── the lock is released HERE
```

Replaying the trace:

```
  time   AGENT A                          AGENT B
  ────   ───────────────────────────      ───────────────────────────
   t1    SELECT ... FOR UPDATE
         → lock acquired
   t2                                     SELECT ... FOR UPDATE
                                          → BLOCKS, waiting
   t3    UPDATE; COMMIT
         → lock released
   t4                                     → unblocks, reads the NEW state,
                                            applies its change, commits

  RESULT: both changes survive, with no retry.
```

### Where it lives

`internal/order/repository/postgres.go` → `UpdateWithPessimisticLock`

### Why the API is a callback

```go
UpdateWithPessimisticLock(ctx, id, func(o *Order) (OutboxEvent, error) { ... })
```

The alternative — exposing `Lock()` and `Unlock()` — means one forgotten
`Unlock` blocks that order for **every user** until the connection is recycled.
A callback guarantees release on every path: normal return, error, or a panic
inside the caller's own code. The `defer` in the implementation is the whole
reason the port has this shape.

### `lock_timeout` is not optional

Without it, `FOR UPDATE` waits **forever**. One stuck transaction then piles up
every subsequent request for that row, exhausts the connection pool, and takes
down endpoints that have nothing to do with this order. Bounded waiting turns a
total outage into a handful of `504`s.

**Always bound your lock waits.**

### Deadlock, and why we cannot have one

A deadlock needs two transactions each holding what the other wants:

```
  T1: locks order A ... then wants order B
  T2: locks order B ... then wants order A      → deadlock
```

Postgres detects this (after `deadlock_timeout`, 1s) and kills one with
SQLSTATE `40P01`, which we translate to a retryable `KindConcurrency`.

**Our defence is structural:** every pessimistic operation locks **exactly one
row**. One lock cannot deadlock. If we ever needed to lock several, the rule
would be to acquire them in a consistent order (e.g. sorted by id) — which is
the general answer to deadlock avoidance.

### The rule about network calls

**Never hold a row lock across a network call.** A 200ms call to the payment
service, made while holding a lock, serialises every request for that order
behind a remote dependency — converting someone else's slowness into your
outage.

This is why `Cancel` does its compensation **after** the repository call has
returned and the lock is released:

```go
// internal/order/service/order_service.go
updated, err := s.repo.UpdateWithPessimisticLock(...)   // lock held, then released
...
s.saga.CompensateCancelledOrder(ctx, updated)           // network calls, no lock
```

### Trade-offs

| | |
|---|---|
| ✅ | No retries; the operation completes deterministically |
| ✅ | Safe for check-then-act with side effects |
| ✅ | Under contention it **queues** rather than failing |
| ❌ | Serialises access to that row; throughput caps at 1/lock-duration |
| ❌ | Deadlock is possible if you lock multiple rows carelessly |
| ❌ | Holds a connection for the transaction's lifetime |

**Use it when:** contention is high, or the operation has side effects that
cannot be rolled back.

---

## Strategy 3 — Hybrid (our default)

Optimistic first; escalate to pessimistic on **proven** contention.

```
  UpdateStatus(id, SHIPPED)
        │
        ▼
  ┌──────────────────────────────────────────────────────────┐
  │ ATTEMPTS 1 … N-1  ·  OPTIMISTIC                           │
  │   re-read → apply the domain rule → CAS on version        │
  │   conflict? back off (exponential + JITTER) and retry     │
  └───────────────────────────┬──────────────────────────────┘
                              │ still conflicting after N-1 tries?
                              │ That is EVIDENCE of contention,
                              │ not bad luck.
                              ▼
  ┌──────────────────────────────────────────────────────────┐
  │ FINAL ATTEMPT  ·  PESSIMISTIC                             │
  │   SELECT … FOR UPDATE (bounded by lock_timeout)           │
  │   apply the rule — the state cannot shift under us        │
  │   UPDATE; COMMIT                                          │
  └──────────────────────────────────────────────────────────┘
```

### Why this is the right default

- **99% of updates have no contention at all.** Taking a row lock for all of
  them pays the serialisation cost on every request to avoid a problem that
  almost never happens.
- **The 1% that are hot behave badly under pure optimism.** Retries thrash, and
  some users still get a `409` after the budget is exhausted.

Hybrid gets the fast path *and* a guaranteed completion: it turns "usually
fine, occasionally fails" into "fast, and always completes".

### Only escalate for the right reason

```go
if !errs.Is(err, errs.KindConcurrency) {
    return nil, err      // a 404 or a rejected transition is NOT contention
}
```

Escalating on a business rejection would take a pointless lock before failing
in exactly the same way.

### Watching it work

`order_lock_escalations_total` on `/metrics` counts escalations. It is also
visible in the logs:

```
INFO optimistic attempts exhausted; escalating to a pessimistic row lock
     order_id=dab485d1 optimistic_attempts=2
```

`make load-test` fires 20 concurrent updates at one order and prints the
before/after counters, so you can see escalation happen rather than take it on
trust.

### Configuring it

```bash
LOCK_STRATEGY=hybrid        # optimistic | pessimistic | hybrid
LOCK_MAX_RETRIES=3
LOCK_RETRY_BASE=20ms
LOCK_PESSIMISTIC_TIMEOUT=3s
```

All three strategies pass the same test suite
(`TestUpdateStatus_AllStrategiesWork`), because a strategy must change the
*concurrency behaviour*, never the *business outcome*.

---

## Strategy 4 — Advisory locks (for background jobs)

A different problem: three replicas each run the same cron loop, so every job
executes three times.

```sql
SELECT pg_try_advisory_lock(950001);   -- returns immediately: true or false
```

An advisory lock is a **named lock that means whatever you decide**, attached to
no row. One `pg_try_advisory_lock` makes a job a cluster singleton.

### Why `try` and not the blocking variant

`pg_advisory_lock` (blocking) would make all three replicas **queue** and run
the job one after another — correctness without any parallelism, and the job
takes 3× longer for no benefit. `pg_try_advisory_lock` returns `false`
immediately and the other two replicas skip the cycle.

### Why not a Redis lock

A Redis `SETNX` + TTL lock is not safe under network partitions without
Redlock-style complexity, and a TTL that expires while the job is still running
hands the lock to a second worker. A Postgres advisory lock is released
**automatically when the session dies** — no TTL to tune, no split-brain. We
already depend on Postgres, so it adds no new infrastructure.

### Where it lives

`internal/platform/postgres/postgres.go` → `TryAdvisoryLock`
`internal/order/workers/workers.go` → both workers
`internal/platform/postgres/migrate.go` → migrations (uses the **blocking**
variant, because there the other replicas genuinely should wait)

### The logging detail that matters

Two of three replicas fail to acquire the lock **every single cycle**. That is
the healthy path, so it logs at `DEBUG`. At `WARN` a 3-replica deployment would
emit two warnings a minute forever and train everyone to ignore warnings.

---

## Strategy 5 — `FOR UPDATE SKIP LOCKED` (work queues)

The outbox poller and the batch advance job need the opposite of a singleton:
**several workers processing disjoint batches in parallel.**

```sql
SELECT * FROM outbox
 WHERE published_at IS NULL
 ORDER BY created_at
 LIMIT 50
   FOR UPDATE SKIP LOCKED;     -- ◀── "...and IGNORE rows others hold"
```

```
  worker A  claims rows   1–50
  worker B  skips those,  claims 51–100
  worker C  skips those,  claims 101–150
```

Without `SKIP LOCKED`, B and C would **block** waiting for A: you get the
correctness but none of the parallelism.

This one clause is why the project needs no Kafka, Celery or job runner for
this workload — Postgres *is* the queue. The integration test
`TestPostgres_OutboxSkipLocked` asserts the batches are genuinely disjoint.

---

## Two services, two opposite defaults

The clearest illustration of "choose locking per use case" is that the two
services in this repository choose **differently**, for sound reasons.

| | **Order service** | **Inventory service** |
|---|---|---|
| Access pattern | Each request touches **its own** order row | 5,000 req/s touch **the same** stock row for a hot SKU |
| Conflict rate | Rare | Constant |
| Default | **Optimistic**, escalating to pessimistic | **Pessimistic**, always |
| Why | Locking every request would cost throughput to prevent a rare problem | Optimistic here would thrash: nearly every request loses the CAS, and some customers get a `409` for an item that *is* in stock |
| The invariant | A status transition must be legal | **Stock must never go negative** — overselling means telling a customer their order is confirmed and then not shipping it |

The inventory service also demonstrates the discipline that makes pessimistic
locking on a hot row acceptable: the critical section contains **only
arithmetic** — no I/O, no logging that could block, no network. Hold the lock
for microseconds, never across anything slow.

See `internal/inventory/inventory.go`, where the lock is taken and the
check-all-then-mutate-all sequence is explained inline.

---

## Isolation levels, briefly

We run **READ COMMITTED** (the Postgres default) plus explicit row locks where
an invariant demands one.

| Level | Guarantee | Cost |
|---|---|---|
| **READ COMMITTED** ← ours | Each *statement* sees a fresh snapshot | Two statements in one transaction can see different data — so a check-then-act needs an explicit lock |
| REPEATABLE READ | The whole transaction sees one snapshot | A conflicting write makes `COMMIT` fail with a serialization error that **you** must catch and retry |
| SERIALIZABLE | As if transactions ran one at a time | Highest abort rate under contention; **every** transaction needs retry logic |

**Why READ COMMITTED + explicit locks** rather than SERIALIZABLE: the locking
is then *visible in the code* rather than implied by a session setting, and
there are no surprise aborts in unrelated transactions. The cost is that you
must remember to lock where it matters — which is exactly what
`UpdateWithPessimisticLock` makes explicit at the call site.

---

## Picking a strategy: a decision guide

```
  Does the operation have EXTERNAL SIDE EFFECTS that cannot be undone?
  (a refund, an email, a dispatch instruction)
        │
        ├── YES ──▶ PESSIMISTIC. Serialise.
        │           Detect-and-retry risks doing the side effect twice.
        │           (This is why Cancel always locks.)
        │
        └── NO
             │
             Is the row HOT — many writers, same row, concurrently?
             │
             ├── YES ──▶ PESSIMISTIC. Queue instead of thrashing.
             │           (This is why inventory locks.)
             │
             └── NO ──▶ OPTIMISTIC, or HYBRID if you want a guarantee
                        that a contended request still completes.
                        (This is the order service's default.)

  Separately:
    Only ONE replica should do this work?        → advisory lock (try, not block)
    MANY workers should share the work?          → FOR UPDATE SKIP LOCKED
```

---

## Where to read the code

| File | What to look at |
|---|---|
| `internal/order/repository/postgres.go` | All three strategies as real SQL, with the `AND version = $N` and the `FOR UPDATE` visible |
| `internal/order/service/order_service.go` | `UpdateStatus` (the hybrid switch) and `Cancel` (why it always locks) |
| `internal/order/repository/memory.go` | The in-memory adapter *faithfully reproduces* version conflicts and lock serialisation — a fake that was easier than reality would give green tests and a broken system |
| `internal/platform/retry/retry.go` | Exponential backoff **with jitter**, and why jitter prevents a thundering herd |
| `internal/platform/postgres/postgres.go` | `TryAdvisoryLock`, and the SQLSTATE → error-kind translation |
| `internal/inventory/inventory.go` | The opposite default, and the microsecond critical section |

## Where to read the tests

| Test | What it proves |
|---|---|
| `TestRepositoryContract/.../optimistic update with a stale version is rejected` | The lost update is **detected**, not silently applied |
| `TestRepositoryContract/.../pessimistic locking serialises concurrent writers` | 20 goroutines each append one character; the result is **exactly** 20 characters long. Any lost update makes it shorter |
| `TestUpdateStatus_ConcurrentUpdatesDoNotLoseData` | 20-way contention produces no `5xx` and a consistent final state |
| `TestCancel_DoubleClickIsSafe` | 10 simultaneous cancels → one cancellation, one refund, **zero errors** |
| `TestPostgres_OutboxSkipLocked` | Two concurrent workers claim genuinely disjoint batches |
| `TestPostgres_PessimisticLockTimesOutRatherThanHangingForever` | A lock contender fails fast instead of hanging |

Run the concurrency tests under the race detector, which is the only reliable
way to find a data race:

```bash
make test-race
```
