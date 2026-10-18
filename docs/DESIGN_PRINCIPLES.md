# Design principles

KISS, YAGNI, DRY and SOLID — each with the **specific file** that demonstrates
it, and the cost it carries.

Principles quoted without an example are decoration. Every entry below names
code you can open.

---

## KISS — Keep It Simple

> The simplest thing that is **correct**. Not the simplest thing that compiles.

| Decision | The simpler choice we made | The complex choice we avoided |
|---|---|---|
| HTTP routing | `net/http.ServeMux` (Go 1.22+ supports method patterns) | chi / gin / echo for 12 routes |
| Metrics | A 150-line registry — the exposition format is just text | `prometheus/client_golang` |
| Dependency injection | Constructor arguments, wired in `main()` | wire / dig / a reflection container |
| Migrations | 90 lines: a ledger, a checksum, an advisory lock | golang-migrate / atlas |
| Event publishing | `FOR UPDATE SKIP LOCKED` on a table | Kafka |
| Vector search | Brute-force cosine over 32 chunks | pgvector / FAISS / Qdrant |
| Saga coordination | One function you can read top to bottom | A workflow engine (Temporal, Cadence) |

### Where simplicity is the *correct* answer, not just the easy one

**No dependency-injection framework.** Dependencies are passed as arguments,
so **the compiler verifies the whole graph**. A missing dependency is a compile
error, not a 3am runtime panic. A container would give you lazy resolution and
a stack trace instead.

**`FOR UPDATE SKIP LOCKED` instead of Kafka.** One SQL clause gives N workers
disjoint batches with no coordination service. Kafka would add a broker, a
consumer-group story, and an operational commitment — to solve a problem we
already solved in one line.

### Where KISS would be WRONG

We did **not** hand-roll the Postgres driver or the Redis client. A database
driver must be correct about wire protocol, prepared statements and connection
pooling. **The line is: hand-roll what teaches, buy what must be correct.**

### The honest cost

Hand-rolled infrastructure is **maintenance you own**. `prometheus/client_golang`
has handled edge cases we have not thought of. In a production team you take
the library; this codebase optimises for explainability and says so.

---

## YAGNI — You Aren't Gonna Need It

> Build for the requirement you have, and **document the threshold** at which
> the bigger thing starts to pay.

The second half is what makes YAGNI defensible rather than lazy.

| Not built | Documented threshold |
|---|---|
| A vector database | **~10,000 chunks**, or when the index stops fitting in memory, or when it must be shared across replicas. We have 32 |
| Keyset (cursor) pagination | When `OFFSET` depth becomes a measured problem. Offset is what an admin table needs, and we cap `limit` at 100 to bound the damage |
| Read replicas | When read load actually exceeds one instance. The repository interface makes this a new adapter, not a rewrite |
| An events service | When more than ~3 consumers need the stream. The outbox already decouples the write |
| Multi-currency FX | When a second currency is actually sold. `Money` already carries its currency and **refuses to add across currencies** |
| Down-migrations | Never, realistically. In practice you roll **forward**: a down-migration that drops a column loses data, so nobody runs it in production |
| OpenTelemetry tracing | When `request_id` propagation stops being enough. It gives most of the benefit for a fraction of the setup |

### Where YAGNI was deliberately OVERRULED

Three things were built before they were strictly needed, because **retrofitting
them is expensive or impossible**:

| Built early | Why it could not wait |
|---|---|
| **The outbox table** | Retrofitting atomic event publishing means finding every write site and wrapping it in a transaction. Far cheaper to design in |
| **The `version` column** | Adding optimistic locking later means backfilling a column *and* auditing every update path for lost updates |
| **`request_id` propagation** | Adding correlation after an incident means you cannot debug the incident you are currently having |

The test is: **how expensive is this to add later?** Cheap to retrofit →
YAGNI applies. Expensive or impossible → build it now.

---

## DRY — Don't Repeat Yourself

> Remove duplication of **knowledge**, not of characters.

Two pieces of code that look identical but change for *different reasons* are
not duplication. Merging them creates coupling.

### Where we removed duplication

| The shared thing | Where it lives | What duplicating it would cost |
|---|---|---|
| **One Dockerfile for four services** | `deploy/Dockerfile.services`, with a `SERVICE` build arg | Four files that drift: one gets a security patch, the others do not |
| **The transaction helper** | `postgres.InTx` | Three classic bugs — a forgotten rollback (which **holds its locks** and stalls the endpoint), a panic that skips cleanup, and an ignored commit error — repeated at every call site |
| **`rowQuerier`** (a one-method interface) | `repository/postgres.go` | `loadItems()` **and** `loadItemsTx()` — the same SQL twice, which is exactly the duplication that drifts |
| **The error vocabulary** | `platform/errs` | Each layer inventing its own error taxonomy, and the HTTP layer string-matching on messages |
| **`IsRetryable`** | `platform/errs` | Retry, saga and circuit-breaker each re-implementing "is this transient?" — three chances to get it wrong |
| **Middleware** | `platform/httpx` | Panic recovery copy-pasted into 12 handlers, and one of them forgetting it |
| **The `platform` package** | shared by all 4 services | Four different log formats, four health-check shapes, four retry policies |
| **One error envelope** | `httpx.NormalizeErrors` | A client writing one parser, then failing on the router's plain-text 404 |

### Where we deliberately did NOT merge

| Looks duplicated | Why it stays separate |
|---|---|
| `MemoryRepository` and `PostgresRepository` | Same interface, **completely different** reasons to change. One changes when SQL changes; the other when test needs change |
| `cachedOrder` DTO vs `domain.Order` | Merging them couples the cache's wire format to the in-memory layout. Rename a domain field and every cached entry silently decodes to zero values |
| `OrderResponse` vs `domain.Order` | Merging them leaks new internal fields into the public API automatically, and makes mass assignment possible |
| `legacyOrderDTO` vs `domain.Order` | The whole point is that the legacy shape stops at one file |
| The four `main.go` files | They look similar but wire **different** dependency graphs. A shared "framework" would need a flag for every difference |

> **The DRY test:** "if requirement X changes, how many places must I edit?"
> If the answer is one, you are fine — even if two files look alike.

---

## SOLID

### S — Single Responsibility

> A class should have **one reason to change**.

The clearest demonstration is the repository stack:

```
  PostgresRepository   changes when   the SQL schema changes
  CachedRepository     changes when   the caching strategy changes
  OrderService         changes when   a business rule changes
  Handler              changes when   the API contract changes
```

Four files, four reasons to change, **zero overlap**. A caching bug is fixed in
exactly one file, and that fix cannot break the SQL.

**Counter-example avoided:** putting `if cacheEnabled { ... }` in the service
would give it two reasons to change — business rules *and* caching.

### O — Open/Closed

> Open to extension, closed to modification.

**The demonstration:** we added caching to the repository **without editing a
single line** of `PostgresRepository` or of `OrderService`.

```go
// main.go — one line
repo := domain.OrderRepository(repository.NewCachedRepository(baseRepo, cacheImpl, ...))
```

And it **composes**. You could wrap it again, in any order, with no change
anywhere:

```go
Cached(Metrics(Retrying(Postgres(db))))
```

Try that with if-statements.

**It is also removable.** Caching misbehaving in production? Stop wrapping it
in `main()` and the system runs identically, just slower. That is a genuinely
valuable property at 3am.

### L — Liskov Substitution

> A subtype must be usable **anywhere** its supertype is, without surprises.

This one is **enforced by a test**, not asserted in a comment:

```go
// repository/repository_test.go
func TestRepositoryContract(t *testing.T) {
    adapters := map[string]repoFactory{
        "memory":   func(t *testing.T) domain.OrderRepository { ... },
        "postgres": func(t *testing.T) domain.OrderRepository { ... },
    }
    for name, factory := range adapters {
        t.Run(name, func(t *testing.T) { runContractSuite(t, factory) })
    }
}
```

**Both adapters run the same suite**, so the in-memory version must reproduce:

- Version conflicts (an explicit check where SQL has `AND version = $N`)
- Lock serialisation (a mutex per order id)
- Atomic outbox writes
- Returning a **copy**, so a caller's mutation cannot alter storage

**Why that last one matters:** if `GetByID` returned a pointer into storage, an
unsaved mutation would appear to have committed — making the fake **more
permissive than the real database**. Tests would pass for code that cannot
work.

> **A fake that is easier than reality gives you green tests and a broken
> system.**

### I — Interface Segregation

> Clients should not depend on methods they do not use.

```go
type OrderReader interface { GetByID; GetByOrderNumber; List; Stats }
type OrderWriter interface { Create; UpdateOptimistic; UpdateWithPessimisticLock }
type OrderRepository interface { OrderReader; OrderWriter; AdvanceStalePending; Ping }
```

**What this buys:**
- A read-only consumer depends on `OrderReader` and **cannot express a write**
- A test fake implements 4 methods instead of 8
- Composition via embedding means no signature is written twice

The most literal example is one method wide:

```go
type rowQuerier interface {
    Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}
```

Both `pgxpool.Pool` and `pgx.Tx` satisfy it, so **one** `loadItems` serves the
pooled and transactional paths. Without it: two functions, same SQL.

### D — Dependency Inversion

> High-level policy must not depend on low-level detail. **Both** should depend
> on abstractions.

Note the **direction** — the interface is defined by the **consumer**:

```
  WRONG:  service → repository/postgres
          business logic now depends on pgx, and cannot be tested or reused
          without a database

  RIGHT:  service → domain (interfaces) ← postgres
          business logic depends only on an abstraction it OWNS
```

The abstractions live in [`domain/ports.go`](../internal/order/domain/ports.go),
and the infrastructure implements them.

**The proof it worked:** `internal/order/domain` imports **nothing but the Go
standard library**. No `net/http`, no `pgx`, no `redis`, no JSON tags.

**The consequences are concrete:**
- Every business rule is unit-testable with no database and no server
- The whole suite runs in ~2 seconds
- Swapping Postgres for MongoDB touches **zero** domain lines
- Adding gRPC touches **zero** domain lines

This also applies to the ML component: everything in the RAG pipeline depends
only on `vectorize(text) -> dict` and `cosine(a, b) -> float`. Replace those two
with model calls and the retriever, generator and API are untouched.

---

## The principles in tension

Principles conflict. Where they did, here is how it was resolved.

| Tension | Resolution |
|---|---|
| **DRY vs SRP** — one `Repository` with 20 methods is DRY-er | Interface Segregation won: several small interfaces. Reason: a read-only consumer should not be *able* to write |
| **KISS vs Open/Closed** — a direct Redis call is simpler than a decorator | Open/Closed won, because caching must be **removable without a code change**. That is worth one layer of indirection |
| **YAGNI vs the outbox** — we have one consumer today | The outbox won, because retrofitting atomicity means touching every write site |
| **DRY vs clarity** — the four `main.go` files share structure | Clarity won. A shared "service framework" would need a flag for every difference, and nobody could read one service in isolation |
| **KISS vs correctness** — `float64` for money is simpler to write | Correctness won, obviously. `0.1 + 0.2 != 0.3` becomes an audit finding |
| **DRY vs decoupling** — three near-identical order DTOs | Decoupling won. Each boundary needs the freedom to change independently |

**The meta-principle:** when principles conflict, ask *which failure is worse*.
A little duplication is cheap; a coupling you cannot undo is expensive, and
incorrect money is unacceptable.

---

## Where to see each one

| Principle | Open this file |
|---|---|
| KISS | [`platform/metrics/metrics.go`](../internal/platform/metrics/metrics.go) — the whole protocol is text |
| YAGNI | [`ai/rag/index.py`](../ai/rag/index.py) — `VectorIndex`, with the stated threshold |
| DRY | [`platform/postgres/postgres.go`](../internal/platform/postgres/postgres.go) — `InTx` and the three bugs it fixes |
| SRP | [`repository/cached.go`](../internal/order/repository/cached.go) — knows only caching |
| Open/Closed | [`cmd/orderd/main.go`](../cmd/orderd/main.go) — the one-line decorator |
| Liskov | [`repository/repository_test.go`](../internal/order/repository/repository_test.go) — the contract suite |
| Interface Segregation | [`domain/ports.go`](../internal/order/domain/ports.go) — Reader / Writer / Repository |
| Dependency Inversion | [`domain/order.go`](../internal/order/domain/order.go) — look at the imports |
