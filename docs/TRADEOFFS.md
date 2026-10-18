# Trade-offs

Every significant decision in this system, what it bought, and what it cost.

A design document that lists only benefits is marketing. The entries below are
written so that a reviewer can disagree with the *conclusion* while agreeing
that the *question* was understood.

---

## 1. Language: Go (+ a Python sidecar)

**Chosen:** Go for the four business services, Python for the AI sidecar.

| Gained | Paid |
|---|---|
| No framework magic — every layer, interface and lock is visible in the code | More lines than Python/FastAPI would need for the same endpoints |
| Goroutines + the race detector make the concurrency requirement testable | No ORM, so SQL is hand-written (which we count as a benefit here, but it is work) |
| Static binary, ~15MB image, no runtime to install | Verbose error handling (`if err != nil`) |
| The AI work lands in the ecosystem that has the libraries | **Two toolchains, two Dockerfiles, two test runners** |

**Why the polyglot split and not one language:** the AI workload is
latency-tolerant and occasionally memory-hungry; the order service must stay
fast and predictable. Separate processes mean a slow retrieval cannot consume
the goroutines taking orders. It also means the assistant can crash without
affecting revenue — which is why nothing in the order service's `/readyz`
depends on it.

**The honest cost:** cross-cutting concerns (log format, health checks,
request-id propagation) are now duplicated in two languages and kept
consistent by discipline rather than by a shared library.

---

## 2. Database: PostgreSQL

**Chosen:** PostgreSQL 16 in Docker. **Rejected:** SQLite, MongoDB.

### Why not SQLite

This is the decision with the least room for argument. SQLite **cannot
demonstrate the core requirement**:

| Needed | SQLite |
|---|---|
| `SELECT … FOR UPDATE` | Does not exist |
| `FOR UPDATE SKIP LOCKED` | Does not exist |
| Advisory locks | Does not exist |
| Concurrent writers | Serialised by a single database-level write lock |

A locking demonstration on SQLite demonstrates nothing. Every strategy would
be either absent or faked.

### Why not MongoDB

A closer call, and it has real advantages here:

| MongoDB would win | But |
|---|---|
| An order + its items fit one document, so no join | We get that anyway by denormalising item snapshots |
| No migrations to write | Schema-on-read means bad data is found by a *reader*, in production |
| Horizontal sharding is native | We have no sharding requirement (YAGNI) |

The deciding factors were:

1. **An order is a transaction.** "Insert the order, insert 3 items, append an
   outbox event" must be all-or-nothing. Postgres gives this in one
   `BEGIN`/`COMMIT`. Mongo has multi-document transactions, but only on a
   replica set, and they are a bolt-on rather than the native model.
2. **Pessimistic locking is first-class.** In Mongo you emulate it with
   `findAndModify` lease fields — rebuilding a database feature in application
   code.
3. **`FOR UPDATE SKIP LOCKED` turns a table into a work queue.** This is why
   the outbox is a dozen lines instead of a Kafka deployment.
4. **The data is relational and the shape is stable.**

**Where Mongo would genuinely win:** a product catalogue where every category
has different attributes, or an append-only event store read only by id.

### Why Docker rather than a local install

| Gained | Paid |
|---|---|
| `make docker-up` is identical on every machine | A running daemon, ~400MB of images |
| Version-pinned, so "works on my machine" means something | Slower first start |
| Teardown is `docker compose down -v` | Docker must be installed |

**And we kept a no-Docker path** (`POSTGRES_ENABLED=false`) because being able
to read and run the code with nothing installed is genuinely valuable. What you
lose is stated plainly in the README: durability, real locks, cache coherence.

---

## 3. Cache: Redis, with an in-process fallback

**Chosen:** Redis, with `MemoryCache` as a fallback and `NoopCache` for off.

### Why not only the in-process cache

With 3 replicas behind a load balancer, an in-process cache gives you **three
different answers** to the same question, and invalidating on one replica
leaves the other two serving stale data. There is no fix other than a shared
cache. This is the most common caching mistake in microservices.

### What Redis costs

| Gained | Paid |
|---|---|
| One shared view across replicas | A network hop (~0.2-1ms) |
| Server-side TTL, independent of our uptime | A new failure mode |
| `maxmemory` + `allkeys-lru` eviction | Another thing to operate and monitor |
| Atomic `INCR` (which powers generation invalidation) | |

**The failure mode is handled by failing open:** a Redis error is treated as a
cache miss, so the user gets a correct, slightly slower response. The accepted
risk is that if Redis dies at peak, full load lands on Postgres at once —
which is why Postgres has a bounded pool and why singleflight exists.

### Cache-aside, not write-through or write-behind

| Pattern | Why not |
|---|---|
| Write-through | Every write pays cache latency, and you cache data nobody reads |
| **Write-behind** | **You can lose committed data if the cache dies before the flush. Never acceptable for orders** |
| Read-through | Needs a cache that supports loaders; couples the cache to the schema |
| Refresh-ahead | Wastes work on keys that may never be read again |

### Delete on write, never update

If two concurrent writes both *update* the cache, they can land in the opposite
order to how they landed in the database, leaving the cache **permanently**
wrong. Deleting is idempotent and self-correcting.

**The residual race we accept:** a concurrent reader could load the old row,
then our invalidation runs, then that reader writes its stale value into the
cache. The window is microseconds and the TTL bounds the damage. The
alternatives (a distributed lock on every read, or versioned CAS writes into
the cache) cost more than the problem. Knowing this window exists, and having
chosen to accept it, is the point.

---

## 4. Locking: hybrid by default

**Chosen:** optimistic → escalate to pessimistic, configurable per deployment.

| Strategy | Gained | Paid |
|---|---|---|
| Optimistic | No locks held; no deadlocks possible; scales across replicas | The loser retries; under contention retries dominate and some users get a 409 |
| Pessimistic | No retries; safe for check-then-act with side effects | Serialises the row; deadlock risk with multiple rows; holds a connection |
| **Hybrid** | **Fast when uncontended, completes when contended** | **More code, and one more concept to explain** |

**Why hybrid is worth the extra complexity:** 99% of updates have no
contention, so locking all of them pays a serialisation cost to prevent a rare
problem. But pure optimism behaves badly on the 1% that are hot. Hybrid turns
"usually fine, occasionally fails" into "fast, and always completes".

**Cancellation ignores the config and always locks**, because it is a
check-then-act with an irreversible side effect (a refund). Under a
double-click, a lock turns a race into a queue.

**The general rule:** when an operation has external side effects that cannot
be rolled back, serialise rather than detect-and-retry.

Full detail: [LOCKING.md](LOCKING.md).

---

## 5. Saga: orchestration, not choreography

| | Orchestration (ours) | Choreography |
|---|---|---|
| The flow | Readable in **one function** | Exists **nowhere**; you reconstruct it by reading every service |
| Debugging | One log stream, one saga row | Trace events across five log streams |
| Coupling | The coordinator knows every participant | Services only know events |
| Adding a step | Edit the coordinator | Deploy a new subscriber, touch nothing else |
| Failure | The coordinator must itself be made reliable | No central component to fail |

**Chosen orchestration** because the explicit brief is that this codebase must
be readable and explainable. For 3 steps, orchestration is clearer. At 15 steps
across 10 teams, choreography's decoupling starts to win.

### Why not two-phase commit

- **It blocks.** If the coordinator dies after "prepare", every participant
  holds its locks waiting for a decision that never comes. Your inventory table
  is locked and nobody can order anything.
- **Participants must support XA.** Stripe does not offer you a 2PC endpoint.
- **Availability multiplies down:** the system is only as available as the
  least available participant.

### What the saga costs

| Gained | Paid |
|---|---|
| No distributed locks; each service stays autonomous | **No atomicity.** There is a window where stock is reserved but payment has not been taken |
| Participants can be third parties | Every step must be idempotent *and* compensatable |
| Failure is localised and recoverable | A compensation can itself fail, and then a **human must reconcile** |

That last row is the real cost, and it is why
`saga_compensation_failed_total` is the one metric this system says you should
alert on.

Full detail: [SAGA.md](SAGA.md).

---

## 6. Transactional outbox, not direct publishing

**The problem:** `db.Save(order)` then `queue.Publish(event)` — if the second
fails, the order exists and nobody knows. Reversing the order gives the opposite
bug. You cannot fix this by being careful.

| Gained | Paid |
|---|---|
| Genuine atomicity between the state change and its event | A polling worker (ours polls every 2s, so events lag slightly) |
| Survives a broker outage: rows accumulate and drain | A table that grows and needs a retention policy |
| No distributed transaction needed | **At-least-once delivery**, so every consumer must be idempotent |

**Why we accept at-least-once:** the worker could crash after publishing but
before marking the row. The alternative (mark-then-publish) risks **losing**
events, which is strictly worse — a duplicate email annoys a customer, a
missing order loses money.

"Exactly once" delivery does not exist. What exists is at-least-once plus
idempotent consumers.

---

## 7. Strangler Fig, not a big-bang rewrite

| Gained | Paid |
|---|---|
| Each phase is an env-var change, so **rollback is instant** | Two systems run in parallel for weeks or months |
| Risk is spread across many small steps | A facade layer to build and maintain |
| Legacy quirks are discovered gradually, with legacy still authoritative | Translation code that is pure overhead once the migration ends |
| `legacy_share` makes "are we done?" a graph, not a guess | |

**Why not a rewrite-and-cutover:** on cutover day you discover the 200
undocumented behaviours the old system had, all at once, in production.

**The honest limitation of backfill-on-read:** cold records are never migrated,
so you still need a batch job for the long tail before phase 4.
Backfill-on-read reduces that job's size and risk; it does not eliminate it.

Full detail: [STRANGLER_FIG.md](STRANGLER_FIG.md).

---

## 8. Hand-written components instead of libraries

A pattern across this codebase, with one consistent justification: **the
requirement is that every line can be read and explained.**

| Component | Lines | Library we skipped | Why | What we gave up |
|---|---|---|---|---|
| Metrics registry | ~150 | `prometheus/client_golang` | The exposition format is just text; writing it shows you understand counters vs gauges vs histograms | Exemplars, native histograms, battle-tested atomics |
| LRU cache | ~200 | `hashicorp/golang-lru` | Eviction is something you should be able to draw: a map for O(1) lookup + a linked list for O(1) recency | Sharding, approximate-LRU optimisations |
| Singleflight | ~40 | `x/sync/singleflight` | The mechanism *is* the lesson | `DoChan`, `Forget` |
| Migrations | ~90 | `golang-migrate` | Shows the three things that matter: a ledger, a checksum, an advisory lock | Down-migrations, a CLI, multi-dialect support |
| UUID generation | ~40 | `google/uuid` | 40 lines of stdlib we can fully explain | UUIDv7 (time-ordered), parsing, other versions |
| HTTP router | 0 | `chi`, `gin` | The stdlib router is enough for 12 routes | Route groups, regex constraints, path middleware |
| Retry | ~60 | `cenkalti/backoff` | Shows the three things naive retry gets wrong | Context-aware tickers, more policies |
| TF-IDF + cosine | ~200 | `sentence-transformers` | Fully explainable; no 2GB download | **Semantic** matching — see §10 |

**Where this trade-off would be wrong:** in a production team, you take the
library. The maintenance burden of hand-rolled infrastructure is real, and
`prometheus/client_golang` has handled edge cases we have not thought of. This
codebase optimises for a different goal, and says so.

**Where we did NOT hand-roll:** `pgx` (a database driver must be correct about
protocol details, prepared statements and connection pooling) and `go-redis`
(same reasoning). The line is: hand-roll what teaches, buy what must be
correct.

---

## 9. API design

| Decision | Why | Cost |
|---|---|---|
| **DTOs, not domain structs on the wire** | Stops accidental field leaks; lets the API stay stable while internals change; makes mass-assignment attacks inexpressible | Mapping code in `transport/dto.go` |
| **Money as both `total_minor` and `total`** | `1999` is exact for machines; `"19.99"` is right for display | Two fields where one looks sufficient |
| **Prices in as integer OR decimal string** | A JSON *number* loses precision before our code sees it | Two input paths, and a conflict check between them |
| **`POST /cancel`, not `DELETE`** | Cancelling is a state change; the record survives for audit | Less "RESTful" by one reading |
| **`PATCH /status`, not `PUT`** | One field changes, not the whole resource | |
| **URL versioning (`/api/v1`)** | An old mobile app keeps working forever while `/v2` ships alongside | Slightly uglier URLs |
| **Offset pagination** | What an admin table needs | Gets linearly slower as you page deeper; keyset pagination is the fix if that becomes a problem |
| **Returning `total` in list responses** | A page count is genuinely useful | A second `COUNT(*)` query — the slowest part of the request on a large table |
| **`DisallowUnknownFields`** | Catches `{"quantiy": 5}`, which would otherwise silently become quantity 0 | A client sending an extra field gets a 400 |
| **Capability flags (`is_cancellable`)** | Clients would reimplement the rule and get it wrong | Response size, and the flags must stay in sync with the domain |

---

## 10. AI: TF-IDF, offline by default

**Chosen:** hand-written TF-IDF + cosine similarity, with a deterministic
template generator. The LLM path exists and is documented but is **off**.

| Gained | Paid |
|---|---|
| Runs with **no API key, no network, no pip install** | **Matches words, not meaning** |
| Every answer is deterministic, so it is testable | No fluent paraphrasing |
| Zero cost and zero latency | No multi-hop synthesis across chunks |
| ~200 lines you can fully explain | Brittle to phrasings the synonym map misses |

**The limitation, concretely:**

```
query:    "how do I get my money back?"
document: "Refunds are issued within 5 working days."
```

These share no words, so TF-IDF scores them near zero. A real embedding model
maps both into nearby vectors because it has *learned* that "money back" and
"refund" mean the same thing.

**Our mitigation** is a hand-built synonym map, which is the classic pre-neural
technique. It works, it is auditable, and it does not scale to open-domain
questions.

**The upgrade path is two functions.** Everything downstream depends only on
`vectorize(text) -> dict` and `cosine(a, b) -> float`. Replace those with model
calls and the retriever, the generator and the API are untouched — Dependency
Inversion applied to an ML component.

**What we kept regardless of the generator:** chunking, retrieval, ranking,
citations, the confidence threshold, and the refusal path. Those are the parts
that carry the engineering value, and they are identical either way.

### Brute-force search, not a vector database

At 32 chunks, scanning all of them is tens of microseconds — a vector database
would be slower once you counted the network hop. The documented threshold is
~10,000 chunks, or when the index no longer fits in memory, or when it must be
shared across replicas. Reaching for pgvector at 32 documents is the same
mistake as brute-forcing 10 million.

Full detail: [AI_RAG.md](AI_RAG.md).

---

## 11. Observability

| Decision | Why | Cost |
|---|---|---|
| **Structured logs (`log/slog`)** | `order_id=7 from=SHIPPED` is filterable; "order 7 failed" is not | Slightly more verbose call sites |
| **One log line per request, at the end** | Two lines doubles volume and cost for little benefit | You lose the "started" marker (we log it at DEBUG) |
| **Level chosen by outcome** (5xx→ERROR, 4xx→WARN) | Makes `level>=warn` a meaningful alert filter rather than noise | |
| **Probes excluded from logs and metrics** | Health checks hit every few seconds forever; logging them buries real traffic | You cannot see probe failures in the request log |
| **Label by route pattern, never raw URL** | Labelling by URL creates one series per order id — millions of series, and you have taken down your own Prometheus | You cannot slice metrics by individual order |
| **Histograms, not averages** | 99 requests at 1ms and one at 10s averages 100ms, which **no user experienced** | More series per metric |
| **Error details hidden from 5xx responses** | SQL fragments and hostnames are information disclosure | The client sees a generic message (the detail is in the logs, keyed by request id) |

---

## 12. Testing

| Decision | Why | Cost |
|---|---|---|
| **Fakes that preserve semantics** | `MemoryRepository` reproduces version conflicts, lock serialisation and atomic outbox writes. A fake that is *easier* than reality gives green tests and a broken system | More code in the fake than a trivial stub |
| **A shared contract suite for both adapters** | The service layer is written against the interface, so both implementations must behave identically (Liskov, enforced by a test) | The suite must work on both, which constrains both |
| **Postgres tests skip when unavailable** | `go test ./...` must work on a laptop with no Docker. A suite that needs infrastructure gets run rarely, and a test that is never run is worse than no test | Those tests do not run by default — a real gap, mitigated by `make test-integration` and CI |
| **An injectable clock** | Testing "advance orders older than 5 minutes" without waiting 5 minutes | A `Clock` interface threaded through the service |
| **`-race` in CI** | The only reliable way to find a data race | ~10x slower |
| **E2E as a shell script** | Tests the real wiring: containers, env vars, networking, startup order | Shell is awkward; assertions are less expressive than Go |
| **Python assertions on *properties*, not wording** | Asserting exact generated text breaks on every template edit and proves nothing | Slightly looser than an exact-match test |

### Two bugs this suite actually caught

Recorded because they are the kind that only surface when you run things:

1. **Plain-text 404s from `ServeMux`** broke the API's single-error-shape
   promise. A client with one error parser would fail on the first mistyped
   URL.
2. **Every Strangler Fig backfill was failing silently** — a legacy reference
   is not a UUID, and backfill is deliberately fire-and-forget, so reads kept
   working. The e2e assertion was too lenient ("backfill may be disabled"), and
   both the bug and the weak test are now fixed.

The second is the more instructive: **assert on the outcome you care about, not
merely on the request not failing.**

---

## 13. What we deliberately did not build

| Not built | Why | What it would take |
|---|---|---|
| **Authentication** | Well-understood and orthogonal; it would add volume without teaching anything about orders, sagas or locking | JWT or mTLS at a gateway, plus per-customer authorisation on every read |
| **Saga recovery worker** | The `saga_instances` table and its partial indexes exist and are documented; only the worker is missing | Find `RUNNING` sagas older than N minutes, then complete or compensate them |
| **Persistent inventory/payment** | They exist to demonstrate the saga participant role and the locking contrast, not to be a second persistence exercise | Postgres with `FOR UPDATE` on a stock table |
| **Rate limiting** | Out of scope | A token bucket per API key, at the gateway |
| **Distributed tracing (OpenTelemetry)** | We propagate `request_id`, which gives most of the benefit for a fraction of the setup | OTel SDK + a collector + Jaeger |
| **Kafka** | `FOR UPDATE SKIP LOCKED` already gives a safe multi-worker queue, and Kafka is a large operational commitment | A broker, plus a consumer-group story |
| **Multi-region** | Out of scope | Read replicas, and a decision about cross-region cache coherence |
| **Down-migrations** | In practice you roll *forward*: a down-migration that drops a column loses data, so nobody runs it in production | A second SQL file per migration |

Each of these is a scope decision with a stated reason, not an oversight. That
distinction is the one worth being able to defend.
