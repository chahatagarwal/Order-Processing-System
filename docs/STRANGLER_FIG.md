# The Strangler Fig pattern

How to replace a legacy system without a big-bang rewrite, in four phases that
are each a configuration change rather than a deploy.

---

## 1. The name

A strangler fig seeds in the canopy of a host tree and grows roots downward
around its trunk. Over years it takes over the host's structure; eventually the
host dies and rots away, leaving a self-supporting fig **in exactly the host's
shape**.

Martin Fowler borrowed it for incremental migration, and the metaphor is exact:
the new system grows around the old one, assuming its responsibilities one at a
time, until the old one can be removed with nothing depending on it.

---

## 2. The problem

You have a 12-year-old order system. 400 endpoints, no tests, three people who
understand it, and it is load-bearing for revenue.

You cannot stop shipping features for 18 months to rewrite it. And:

> **A big-bang rewrite-and-cutover is the most reliable way to destroy a
> business.** On cutover day you discover the 200 undocumented behaviours the
> old system had — all at once, in production, with revenue stopped.

---

## 3. The shape of the solution

Put a **facade** in front of the legacy system. Route one slice of traffic to a
new implementation. Then another. The legacy system shrinks until nothing
reaches it, then you delete it.

```
                        ┌───────────────────┐
     client ──────────▶ │ StranglerFacade   │
                        └─────────┬─────────┘
                                  │
                    new store?  ──┴──  legacy service?
                         │                    │
                  ┌──────▼──────┐      ┌──────▼───────┐
                  │  Postgres   │ miss │ legacy-order │
                  │   (new)     │─────▶│   service    │
                  └─────────────┘      └──────┬───────┘
                         ▲                    │
                         └──── backfill ──────┘
                            ("migrate on read")
```

The facade implements `domain.OrderReader`, so **the HTTP handler cannot tell
the difference**. That opacity is the entire value.

---

## 4. The four phases

Every transition is an **environment-variable change**, which means
**rollback is instant**. If phase 3 goes wrong at 2am you set one variable
back, rather than reverting a deploy under pressure.

### Phase 1 — Legacy authoritative, new system shadowing

```bash
STRANGLER_WRITE_MODE=legacy
STRANGLER_READ_FALLBACK=true
```

All writes go to legacy. We read from legacy. The new system is deployed but
idle.

**Purpose:** prove the facade, the routing and the deployment work, with **zero
risk**. Nothing depends on the new system yet.

### Phase 2 — Dual write

```bash
STRANGLER_WRITE_MODE=dual
STRANGLER_READ_FALLBACK=true
```

Writes go to **both**. Reads still prefer legacy (or are shadowed and
compared). Legacy remains the source of truth.

**Purpose:** this is where you find the behaviour differences — with legacy
still authoritative, so a mismatch is a **bug report, not an outage**.

> **This is the most valuable phase, and the one most often skipped.** It is
> also where you discover the undocumented quirks: that order refs are
> case-insensitive, that `AWAIT_FAX` is still a valid state, that a particular
> client depends on a field you were about to drop.

**What it costs:** writes are slower (two systems), and you need a reconciler
to compare the two stores and report divergence.

### Phase 3 — New authoritative, legacy for history ← **our default**

```bash
STRANGLER_WRITE_MODE=new
STRANGLER_READ_FALLBACK=true
STRANGLER_BACKFILL_ON_READ=true
```

The new system is now the source of truth. Historical orders still live in
legacy, so a miss falls back — and optionally **backfills**, letting ordinary
traffic perform the migration.

### Phase 4 — Legacy decommissioned

```bash
STRANGLER_READ_FALLBACK=false
```

Nothing reaches legacy. Turn it off, keep a database snapshot, delete the code.
**The fig stands alone.**

---

## 5. Backfill on read ("migrate on read")

```go
if f.cfg.BackfillOnRead {
    go f.backfill(context.WithoutCancel(ctx), legacyOrder)
}
```

**The idea:** the orders customers actually look at are the ones worth
migrating. Ordinary traffic does the migration for you, **warmest records
first**, with no big batch job and no maintenance window.

### Three properties that make it safe

**1. Fire-and-forget.** A failed backfill must **never** fail the customer's
read. They asked for their order; they get their order. Treating the backfill
as optional is what keeps it safe.

**2. Its own panic recovery.**

```go
defer func() {
    if rec := recover(); rec != nil { f.log.Error("panic during backfill", ...) }
}()
```

A panicking goroutine takes down the **whole process** in Go, and this one is
not covered by the HTTP middleware's recover. A background goroutine without
panic recovery is a time bomb.

**3. It emits NO event.**

```go
// NOTE: we deliberately emit NO outbox event for a backfill. A backfill is a
// MIGRATION, not a business event — publishing order.created for a
// three-year-old order would re-trigger confirmation emails to customers for
// orders they received years ago.
```

**This is the single most common and most embarrassing migration mistake.** One
line of comment is cheaper than finding out the hard way.

### The honest limitation

Cold records are **never** migrated, because nobody reads them. You still need
a batch job for the long tail before phase 4. Backfill-on-read reduces that
job's size and risk; it does not eliminate it.

---

## 6. The decision that keeps the fallback safe

```go
order, err := f.newStore.GetByID(ctx, id)
if err == nil { return order, ServedByNew, nil }

// ONLY a not-found justifies trying legacy.
if !errs.Is(err, errs.KindNotFound) {
    return nil, ServedByNew, err
}
```

**Why this matters:** a timeout or a connection error means the new system is
**sick**. Falling back to legacy then would silently **double the load on the
system you are trying to retire** — at exactly the worst moment, and you would
not notice because requests would still succeed.

Distinguishing "absent" from "broken" is what makes the fallback safe rather
than dangerous.

### And the error you return matters too

```go
if legacyErr != nil && !errs.Is(legacyErr, errs.KindNotFound) {
    return nil, ServedByLegacy, errs.Unavailable("legacy_unavailable", ...)
}
```

If legacy is **down**, we return `503`, not `404`. Telling a customer their real
order "does not exist" because a backend is down is far worse than asking them
to retry.

---

## 7. Translation: where the two worlds meet

The legacy system is deliberately awkward, and
[`HTTPLegacyAdapter.translate()`](../internal/order/service/strangler.go) is
the only place that knows it.

| Legacy | Ours | The problem |
|---|---|---|
| `order_ref` | `order_number` | Different field names throughout |
| `cust_ref` | `customer_id` | |
| `"1299.00"` (a **string**, in dollars) | `129900` (int64 cents) | Parsed with `ParseMoney` for an **exact** conversion. Using a float here would reintroduce the precision bug `money.go` exists to prevent |
| `COMPLETE` | `DELIVERED` | An entirely different status vocabulary |
| `AWAIT_FAX` | `PENDING` | **A state nobody remembers the meaning of** |
| `"2019-04-02 09:13:44"` | RFC-3339 | A non-standard timestamp format |
| `lines` | `items` | |
| *(no idempotency support)* | — | So the adapter sets `Retry: false` for writes |

### Two defensive decisions worth noticing

**An unknown status maps to PENDING, not an error:**

```go
default:
    return domain.StatusPending
```

A 12-year-old system **will** have states nobody remembers. Refusing to display
those orders at all would be a worse outcome than showing a conservative
status. Defensive translation is a requirement, not paranoia.

**An unparseable timestamp does not drop the order:**

```go
created, err := time.Parse("2006-01-02 15:04:05", dto.Created)
if err != nil {
    a.log.Warn("could not parse the legacy created timestamp; leaving it empty", ...)
}
```

A wrong `created_at` is cosmetic. A dropped order is a revenue problem.

**And writes are never retried:**

```go
// Retry is FALSE: the legacy system has no idempotency support, so a retry
// after a timeout would create a DUPLICATE ORDER.
Retry: false,
```

When you cannot make a call idempotent, **you must not retry it.** Recognising
that constraint matters more than the convenience of retrying.

---

## 8. Measuring progress

```bash
curl -s http://localhost:8080/api/v1/admin/strangler | python3 -m json.tool
```

```json
{
  "phase": "phase 3: new system authoritative, legacy read-fallback for history",
  "write_mode": "new",
  "read_fallback_enabled": true,
  "backfill_enabled": true,
  "served_by_new": 12,
  "served_by_legacy": 5,
  "legacy_share": 0.294,
  "orders_backfilled": 1
}
```

### `legacy_share` is the number that matters

It answers: **"can we delete the old system yet?"**

When it reaches 0 **and stays there across a full business cycle** — including
month-end, and any annual reporting job that might read ancient orders — phase
4 is safe.

Deciding that from a graph rather than from confidence is the difference
between a calm decommission and an incident.

### Also available in SQL

```sql
SELECT source, COUNT(*) FROM orders GROUP BY source;
```

That is why every order carries a `source` column: migration progress is a
`GROUP BY`, not a guess.

### And per-request, from outside

```bash
curl -i http://localhost:8080/api/v1/orders/number/LEG-2019-0001 | grep -i x-served-by
# X-Served-By: legacy
```

Surfacing the backend in a header makes the migration **testable from
outside** — the e2e suite asserts on it, rather than trusting a log line.

---

## 9. Seeing it work

```bash
# LEG-2019-0001 exists ONLY in the legacy service.
curl -i http://localhost:8080/api/v1/orders/number/LEG-2019-0001 | grep -i x-served-by
# → X-Served-By: legacy

sleep 2

# Read it again — ordinary traffic has migrated it.
curl -i http://localhost:8080/api/v1/orders/number/LEG-2019-0001 | grep -i x-served-by
# → X-Served-By: new

# And it is really in Postgres now, with its provenance recorded:
make psql
#   SELECT order_number, source, status FROM orders WHERE source = 'legacy';
```

Or just run `make demo`, which walks through this with explanations.

---

## 10. A bug worth recording

Found by **running** the system, not by reading the code.

**The symptom:** `orders_backfilled` stayed at 0 forever, but every read
succeeded and the e2e test passed.

**The cause:** the legacy identifier `LEG-2019-0001` is not a UUID, so
inserting it into `orders.id` failed with SQLSTATE 22P02. And because backfill
is *deliberately* fire-and-forget, the failure was swallowed — only a WARN log
revealed it.

**The fix:** mint a new internal UUID and keep the legacy reference as the
**order number**, which is the customer-facing identifier anyway.

**Why that is better than widening the column to TEXT:**

- The legacy reference is what the **customer** quotes, and `order_number` is
  exactly the field for that. Lookups by `LEG-2019-0001` keep working.
- Our internal id stays a real UUID, keeping the properties we chose it for.
- Changing `orders.id` to TEXT to accommodate a system we are **actively
  retiring** would let the legacy system's design dictate ours permanently —
  the exact outcome the Strangler Fig exists to avoid.

**The test lesson, which is the more useful one:** the e2e assertion accepted
either outcome ("backfill may be disabled"), so it could not fail. **Assert on
the outcome you care about — the record landing in the new store — not merely
on the request not failing.** A test that accepts both answers tests nothing.

Both the fix and this reasoning are recorded in the code, in
`StranglerFacade.backfill`.

---

## 11. When NOT to use this pattern

| Situation | Better approach |
|---|---|
| The legacy system is small and well-tested | Just rewrite it. The facade is overhead |
| There is no traffic to route (an internal batch job) | A one-off migration script |
| The data models are **identical** | A database-level replication/migration |
| You can take downtime and the system is simple | A maintenance window is cheaper than months of dual-running |

The pattern earns its complexity when the legacy system is **large, poorly
understood, and cannot be taken offline** — which is precisely when a rewrite
is most tempting and most dangerous.

---

## 12. What to read

| File | Why |
|---|---|
| [`service/strangler.go`](../internal/order/service/strangler.go) | The facade, the four phases, backfill, and the identifier-translation bug |
| [`internal/legacy/legacy.go`](../internal/legacy/legacy.go) | The fake legacy system, and why each quirk is there |
| [`transport/http.go`](../internal/order/transport/http.go) | `getOrder` routing through the facade and setting `X-Served-By` |
| [`platform/config/config.go`](../internal/platform/config/config.go) | `StranglerConfig` and `WriteMode`, with each phase documented |
