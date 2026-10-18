# The Saga pattern

Why distributed transactions do not work, what a saga does instead, and the
four rules every step must obey.

---

## 1. The problem

Placing an order spans three services, each with its own database:

```
  order service      →  create the order row
  inventory service  →  reserve 2 units of SKU-123
  payment service    →  charge $99.98
```

We need **all three or none**. In a single database that is one transaction.
Across three services it is impossible, because there is no shared transaction
to roll back.

### Why not two-phase commit

2PC is the textbook answer, and it is the wrong one here:

- **It BLOCKS.** If the coordinator dies after "prepare", every participant
  holds its locks indefinitely, waiting for a decision that never comes. Your
  inventory table is locked and **nobody can order anything**.
- **Every participant must support XA.** Stripe does not offer you a 2PC
  endpoint, and never will.
- **Availability multiplies down.** The system is only as available as the
  *least* available participant — three services at 99.9% give you 99.7%.

---

## 2. The saga answer

Give up atomicity; keep consistency, **eventually**. Run a sequence of local
transactions, each with a **compensating action** that semantically undoes it.

```
  FORWARD:       reserve stock  →  charge card  →  confirm order
                      │                │
  COMPENSATE:    release stock  ←  refund card  ←  (nothing to undo)
                 (in REVERSE order)
```

### A compensation is not a rollback

This distinction matters more than anything else here.

```
  ROLLBACK      the write never happened. The world is unchanged.
  COMPENSATION  the charge REALLY HAPPENED. The customer saw it on their
                statement. A refund is a NEW, FORWARD transaction that
                cancels its effect.
```

Once you cross a service boundary, the world has changed and **"undo" is always
a forward action, never a rewind**. That is precisely why sagas exist.

---

## 3. Orchestration vs choreography

| | **ORCHESTRATION** (ours) | **CHOREOGRAPHY** |
|---|---|---|
| Who decides | One coordinator calls each step | Each service reacts to events |
| The flow | Readable in **one function** | **Exists nowhere.** You reconstruct it by reading every service and mentally simulating the event graph |
| Debugging | One log stream, one saga row recording where it got to | Trace events across five log streams |
| Coupling | The coordinator knows every participant | Services only know events |
| Adding a step | Edit the coordinator | Deploy a new subscriber, touch nothing else |
| Failure mode | The coordinator must itself be made reliable | No central component to fail |

**We chose orchestration** because the brief is explicitly that this codebase
must be readable and explainable. For 3 steps, orchestration is clearer, and
you can see the whole business process in
[`saga.go`](../internal/order/service/saga.go).

At 15 steps across 10 teams, choreography's decoupling starts to win.

---

## 4. The four rules every step must obey

### Rule 1 — IDEMPOTENT

A step may be retried after an **ambiguous timeout**: we do not know whether it
succeeded. Every call therefore carries an idempotency key.

```go
idempotencyKey(order.ID, "charge")   // → "order-123:charge"
```

**The key must be DETERMINISTIC.** With a random key per attempt:

```
  attempt 1:  POST /charge  key=abc  → times out (BUT THE CARD WAS CHARGED)
  attempt 2:  POST /charge  key=xyz  → a NEW key → CHARGES AGAIN
                                       → customer billed twice
```

With a deterministic key, attempt 2 presents the same key and the payment
service returns the original result, moving no money. **This is how Stripe's
API works, and it is the only correct way to retry a non-idempotent
operation.**

### Rule 2 — COMPENSATABLE

There must be a semantic undo. `reserve` has `release`; `charge` has `refund`.

If a step has **no possible compensation** — sending an email, printing a label
— it must be the **last** step, or be made conditional on the saga succeeding.

### Rule 3 — COMMUTATIVE-SAFE compensation

The compensation may arrive **before** the action's own response. This is the
"empty compensation" problem:

```
  step 1 times out.  We never learn the reservation id.
  But inventory DID reserve the stock.
  Now we must compensate — with no handle.
```

Two defences, both in the code:

```go
Compensate: func(ctx context.Context, handle string) error {
    if handle == "" {
        return nil          // nothing we can target
    }
    return s.inventory.Release(ctx, handle, ...)
}
```

and, crucially, **the inventory service expires reservations by TTL**
(`Reservation.ExpiresAt`), which is the real backstop for exactly this case.
Without it, every timed-out saga would leak stock permanently.

Relatedly: **releasing a reservation that does not exist must SUCCEED.** The
desired end state already holds. Returning an error would make a correct
compensation look like a failure and trigger false alerts.

### Rule 4 — RETRYABLE compensation

Compensations must *eventually* succeed, because **there is nothing to
compensate a failed compensation with.** They therefore retry harder than
forward steps:

```go
for attempt := 1; attempt <= 3; attempt++ {
    compErr = step.Compensate(compCtx, done.Handle)
    if compErr == nil { break }
    time.Sleep(time.Duration(attempt) * 100 * time.Millisecond)
}
```

And if they still fail, the saga enters `COMPENSATION_FAILED` and a **human
must reconcile**. See §8.

---

## 5. Step ordering is a business decision

```go
steps := []SagaStep{
    {Name: "reserve_inventory", ...},   // 1
    {Name: "charge_payment",    ...},   // 2
    {Name: "confirm_order",     ...},   // 3
}
```

**We reserve stock BEFORE charging the card.** If we charged first and then
found no stock, we would have to refund — which costs processor fees, appears
on the customer's statement twice, and generates a support contact.

Reserving first means the **common** failure (out of stock) costs nothing and
is invisible to the customer.

**The general rule:** order saga steps **cheapest-to-compensate first**, and
put the irreversible or expensive step **last**.

---

## 6. The sequencing decision that protects money

The order is committed as `PENDING` **before any remote call is made**.

```
  WHAT WE DO                      WHAT WOULD BE WORSE
  ──────────                      ───────────────────
  1. save the order (PENDING)     1. reserve stock
  2. reserve stock                2. charge the card
  3. charge the card              3. ← CRASH HERE
  4. confirm (PROCESSING)         4. save the order

  Worst case: an order stuck in    Worst case: A CUSTOMER IS CHARGED FOR
  PENDING with no reservation.     AN ORDER THAT DOES NOT EXIST.
  That is a ROW IN THE DATABASE:   Invisible. Unrecoverable. Nobody finds
  visible, and recoverable by      out until they complain.
  the recovery worker.
```

**Prefer a recoverable inconsistency over an invisible one.** That instinct is
the whole design philosophy behind sagas.

---

## 7. The most consequential lines in the implementation

```go
compCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.StepTimeout)
```

**Why `WithoutCancel` is load-bearing:** the incoming context is *very likely
already cancelled* — the step failed because it timed out, or because the client
hung up. If we reused it, every compensation would fail instantly with "context
canceled", and we would leave the stock reserved and the card charged.

`context.WithoutCancel` (Go 1.21+) keeps the context's **values** — so
`request_id` still appears in the logs, which is how you trace this later —
while discarding the cancellation.

This is one of the most consequential bugs you can write in a saga, and it is
invisible until the day something actually times out.

---

## 8. When compensation fails

```go
log.ErrorContext(ctx, "COMPENSATION FAILED - MANUAL RECONCILIATION REQUIRED",
    slog.String("step", done.Name),
    slog.String("handle", done.Handle),
    slog.String("error", compErr.Error()),
    slog.String("original_cause", cause.Error()))
s.mtx.IncCounter("saga_compensation_failed_total", "... (ALERT ON THIS)")
```

This is the worst case: we failed to undo our own work and cannot fix it
automatically. It may mean **a customer is charged for an order they will never
receive**.

The one thing we must do is make it **impossible to miss**:

- ERROR level, with every identifier a human needs (order, saga, step, handle)
- A dedicated metric, which is the one series this system says you should alert
  on
- The saga is marked `COMPENSATION_FAILED`, and the migration has a **partial
  index** on exactly that state so the alerting query is instant

Silently swallowing it would mean nobody ever finds out.

---

## 9. Saga state, and recovery

```sql
CREATE TABLE saga_instances (
    id               UUID PRIMARY KEY,
    order_id         UUID NOT NULL,
    status           TEXT CHECK (status IN ('RUNNING','COMPLETED',
                       'COMPENSATING','COMPENSATED','COMPENSATION_FAILED')),
    completed_steps  JSONB,   -- each step AND THE HANDLE it returned
    last_error       TEXT,
    started_at       TIMESTAMPTZ,
    ended_at         TIMESTAMPTZ
);
```

**Why this table exists:** if the process dies mid-saga, nobody knows whether
the card was charged or the stock reserved — so nobody can know what to
compensate. This row is the difference between "eventually consistent" and
"permanently broken, with a customer charged for nothing".

`completed_steps` stores the **handles** (reservation id, payment id). Without
them, a recovery worker knows a step ran but cannot undo it.

**Stated limitation:** the running code keeps saga state in memory. The table,
its partial indexes and this design are production-ready; the recovery
**worker** is not implemented. That gap is listed in
[TRADEOFFS.md](TRADEOFFS.md#13-what-we-deliberately-did-not-build) rather than
glossed over.

---

## 10. Classifying failures correctly

This is what stops a saga retrying a guaranteed failure.

| Failure | Kind | HTTP | Retryable? | Why |
|---|---|---|---|---|
| Insufficient stock | `Unprocessable` | 422 | **No** | Retrying will not create stock |
| Card declined | `Unprocessable` | 422 | **No** | The bank will not change its mind in 50ms |
| Inventory service down | `Unavailable` | 503 | **Yes** | Transient; it may be back in 2s |
| Step timed out | `Timeout` | 504 | **Yes** | The world may differ on retry |
| Version conflict | `Concurrency` | 409 | **Yes** | Someone beat us; re-read and retry |

One function consults this taxonomy:

```go
func IsRetryable(err error) bool {
    switch KindOf(err) {
    case KindConcurrency, KindUnavailable, KindTimeout: return true
    default: return false
    }
}
```

Retry logic, saga compensation and the circuit breaker **all** use it, instead
of three separate implementations doing string matching on error messages.

---

## 11. Seeing it work

```bash
# The payment service declines any amount whose CENTS END IN 66.
# 2 x 4983 = 9966  →  declined.
curl -X POST http://localhost:8080/api/v1/orders -H 'Content-Type: application/json' -d '{
  "customer_id":"c1","customer_email":"a@b.com","shipping_address":"x",
  "items":[{"product_id":"SKU-KEYBOARD","product_name":"Keyboard",
            "quantity":2,"unit_price_minor":4983}]}'

# → 422 payment_declined, and the order is FAILED.

# Check the stock came back:
curl -s http://localhost:8081/api/v1/stock | python3 -m json.tool

# See exactly which step died:
curl -s http://localhost:8080/api/v1/admin/sagas/<order-id> | python3 -m json.tool
```

A deterministic failure switch matters: **a demo you cannot make fail only ever
proves the happy path.**

---

## 12. The tests that prove it

| Test | What it proves |
|---|---|
| `TestSaga_PaymentDeclinedReleasesReservedStock` | **The headline behaviour.** Step 1 succeeds, step 2 fails, and the stock is returned. Without compensation, every declined card silently removes inventory from sale |
| `TestSaga_StopsAtTheFirstFailedStep` | Inventory fails → payment is **never attempted**. Charging for an order we know cannot be fulfilled is what the step ordering prevents |
| `TestSaga_IdempotencyPreventsDoubleCharging` | The same key twice → the **same** payment id, and `CapturedCount() == 1` |
| `TestSaga_CompensationIsIdempotent` | A double release does **not** credit the stock twice (which would invent inventory) |
| `TestSaga_ReleasingAnUnknownReservationSucceeds` | Rule 3: the desired end state already holds, so it is a success |
| `TestSaga_InsufficientStockIsNotRetried` | The classification works: a business refusal is not retryable |
| `TestSaga_RecordsWhichStepFailed` | "Which step did it die on?" is answerable |
| `TestCreate_PersistsBeforeAnyRemoteCall` | The row survives a failed saga, as `FAILED`, with a reason |

---

## 13. What to read

| File | Why |
|---|---|
| [`service/saga.go`](../internal/order/service/saga.go) | The orchestrator. **Read the header in full** |
| [`domain/ports.go`](../internal/order/domain/ports.go) | `InventoryPort` / `PaymentPort`, and the long note on the dual-write problem |
| [`service/adapters.go`](../internal/order/service/adapters.go) | How a 422 becomes `Reserved=false` instead of an error, so the saga can tell "broken" from "cannot fulfil" |
| [`internal/payment/payment.go`](../internal/payment/payment.go) | Idempotency from the participant's side, including why **failures** are recorded too |
| [`internal/inventory/inventory.go`](../internal/inventory/inventory.go) | Reservation TTL as the empty-compensation backstop |
| [`workers/workers.go`](../internal/order/workers/workers.go) | The outbox publisher, and the at-least-once moment made explicit |
