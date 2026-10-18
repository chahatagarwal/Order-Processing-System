# Order Processing System

A production-shaped e-commerce order backend: **5 microservices**, a
**distributed saga** with real compensating transactions, **three locking
strategies**, a **three-layer cache** with explicit invalidation and eviction
policies, a **Strangler Fig** migration from a legacy system, and a **RAG
chatbot** that runs entirely offline.

Built to be **read and explained**, not just run. Every non-obvious decision is
commented *in the code* with the reasoning and the trade-off, because
"why is it like this?" is the question that actually gets asked.

```bash
make docker-up      # start everything (one command)
make e2e            # 118 end-to-end assertions against the live system
make demo           # a guided walkthrough with real curl commands
```

> `CLAUDE.md` is a symlink to this file, so the same guidance serves both human
> readers and AI tooling.

---

## Contents

- [Quick start](#quick-start)
- [What this demonstrates](#what-this-demonstrates)
- [The services](#the-services)
- [Architecture in one diagram](#architecture-in-one-diagram)
- [Reading the code](#reading-the-code-start-here)
- [Testing](#testing)
- [Configuration](#configuration)
- [Try it yourself](#try-it-yourself)
- [Documentation](#documentation)
- [Honest limitations](#honest-limitations)

---

## Quick start

### With Docker (recommended)

```bash
make docker-up          # builds and starts 7 containers, waits for health
make smoke              # 10-second sanity check
make e2e                # the full end-to-end suite
open http://localhost:8080/docs
```

### Without Docker

Everything runs with no infrastructure at all, because every dependency sits
behind an interface:

```bash
make run-nodocker       # in-memory repository, in-process LRU cache
```

What you lose, stated plainly: durability (data is gone on restart), real row
locks, and cross-replica cache coherence. Everything else behaves identically
— the same business logic, the same saga, the same state machine.

### Prerequisites

| | Version | Why |
|---|---|---|
| Go | 1.24+ | Uses `ServeMux` method patterns (1.22+) and `context.WithoutCancel` (1.21+) |
| Python | 3.9+ | The assistant sidecar. **Zero pip packages** — standard library only |
| Docker | any recent | Postgres + Redis. Optional (see above) |

---

## What this demonstrates

| Requirement | Where it lives | The short version |
|---|---|---|
| **KISS** | throughout | A hand-rolled 150-line metrics registry instead of a dependency; the stdlib router instead of a framework |
| **YAGNI** | throughout | Brute-force vector search for 32 chunks, with the documented threshold (~10k) at which a vector DB starts to pay |
| **DRY** | `internal/platform/` | One `Dockerfile` for four services; one `InTx` helper that fixes forgotten rollbacks everywhere |
| **SOLID** | see [DESIGN_PRINCIPLES.md](docs/DESIGN_PRINCIPLES.md) | Each principle with the *specific file* that demonstrates it |
| **Service layer** | `internal/order/service` | Use-case orchestration; no HTTP types, no SQL |
| **Repository layer** | `internal/order/repository` | 3 adapters behind one interface, verified by a shared contract test suite |
| **Caching** | `internal/platform/cache`, `repository/cached.go` | Cache-aside + singleflight + negative caching + generation-based group invalidation |
| **Cache invalidation vs eviction** | [CACHING.md](docs/CACHING.md) | Two different problems; conflating them causes real bugs |
| **Locking: pessimistic / optimistic / hybrid** | [LOCKING.md](docs/LOCKING.md) | All three, switchable by config, plus advisory locks and `SKIP LOCKED` |
| **Postgres** | `internal/platform/postgres` | Chosen *because* SQLite cannot demonstrate row locking at all |
| **Constants / config / env** | `platform/constants`, `platform/config` | Validated at boot; nothing calls `os.Getenv` directly |
| **GoF patterns** | [ARCHITECTURE.md §7](docs/ARCHITECTURE.md#7-design-patterns-used) | Creational, structural and behavioural, each with its file |
| **healthz / readyz** | `internal/platform/health` | And *why* liveness must not check the database |
| **Microservices** | 5 services | Because saga and Strangler Fig are meaningless in one process |
| **API documentation** | `docs/openapi.yaml`, `/docs` | Hand-written contract, served as Swagger UI |
| **Saga** | `internal/order/service/saga.go` | Orchestrated, with compensations and idempotency keys |
| **Strangler Fig** | `internal/order/service/strangler.go` | 4 migration phases, switchable by env var |
| **Trade-offs** | [TRADEOFFS.md](docs/TRADEOFFS.md) | Every significant decision, with what it costs |
| **Unit tests** | `*_test.go` | 160+ Go tests, 60 Python tests, all with `-race` |
| **End-to-end tests** | `scripts/e2e.sh` | 118 assertions against the running system |
| **Efficient logs** | `internal/platform/logging` | Structured, with `request_id` propagated across all 5 services |
| **AI / RAG** | `ai/` | A real retrieval pipeline with a tested hallucination guard |

---

## The services

| Service | Port | Language | Its one job |
|---|---|---|---|
| **order** | 8080 | Go | The system of record. Orchestrates the saga, owns the state machine |
| **inventory** | 8081 | Go | Reserve/release stock. Uses **pessimistic** locking — the opposite choice to the order service, for a documented reason |
| **payment** | 8082 | Go | Charge/refund. Built around **idempotency keys** so a retry can never double-charge |
| **legacy** | 8083 | Go | The old system being **strangled**. Deliberately awkward: string money, its own status vocabulary, no idempotency |
| **assistant** | 8090 | Python | A RAG chatbot. Offline, deterministic, **zero pip dependencies** |

Plus **Postgres** (:5432) as the source of truth and **Redis** (:6379) as the
shared cache.

---

## Architecture in one diagram

```
                        ┌──────────────┐
                        │    Client    │
                        └──┬────────┬──┘
                           │        │
          ┌────────────────▼──┐  ┌──▼─────────────────┐
          │   ORDER SERVICE   │  │   ASSISTANT (RAG)  │
          │   Go · :8080      │◀─┤   Python · :8090   │
          │                   │  │   read-only lookup │
          │  ┌─────────────┐  │  └────────────────────┘
          │  │ transport   │  │
          │  ├─────────────┤  │     ┌──────────────┐
          │  │ service     │  │────▶│  POSTGRES    │ source of truth
          │  ├─────────────┤  │     │  + outbox    │ + advisory locks
          │  │ DOMAIN ◀────┼──┼─┐   └──────────────┘
          │  ├─────────────┤  │ │
          │  │ repository  │──┼─┘   ┌──────────────┐
          │  └─────────────┘  │────▶│    REDIS     │ shared cache
          └──┬─────┬─────┬────┘     │ allkeys-lru  │ (safe to lose)
             │     │     │          └──────────────┘
             ▼     ▼     ▼
      ┌──────────┐ ┌──────────┐ ┌──────────┐
      │INVENTORY │ │ PAYMENT  │ │  LEGACY  │
      │  :8081   │ │  :8082   │ │  :8083   │
      │          │ │          │ │          │
      │pessimistic│ │idempotency│ │ strangled│
      │  locking │ │   keys   │ │  + backfill│
      └──────────┘ └──────────┘ └──────────┘
        saga participants         read fallback
```

**All arrows inside the order service point inwards.** The domain imports
nothing but the Go standard library, which is why the whole test suite runs in
~2 seconds with no database.

Full diagrams, request flows and the data model: [ARCHITECTURE.md](docs/ARCHITECTURE.md).

---

## Reading the code: start here

Read these five files in order and you will understand the system.

| # | File | What you will learn |
|---|---|---|
| 1 | [`cmd/orderd/main.go`](cmd/orderd/main.go) | **The composition root.** The entire dependency graph in one readable function. Where every interface is bound to an implementation |
| 2 | [`internal/order/domain/order.go`](internal/order/domain/order.go) | The business rules, with no infrastructure in sight. Why the factory makes an invalid order *inexpressible* |
| 3 | [`internal/order/service/order_service.go`](internal/order/service/order_service.go) | Use cases, and the **hybrid locking** decision at the call site |
| 4 | [`internal/order/repository/postgres.go`](internal/order/repository/postgres.go) | The three locking strategies as **actual SQL** you can read |
| 5 | [`internal/order/service/saga.go`](internal/order/service/saga.go) | The distributed transaction, its compensations, and why idempotency keys are non-negotiable |

[CODE_WALKTHROUGH.md](docs/CODE_WALKTHROUGH.md) does this in depth, naming the
specific lines worth pausing on.

### Also worth reading on their own

- [`internal/order/domain/money.go`](internal/order/domain/money.go) — why money is `int64` cents and never a float, including the penny-allocation problem
- [`internal/order/domain/status.go`](internal/order/domain/status.go) — the State pattern as one transition table
- [`internal/platform/cache/memory.go`](internal/platform/cache/memory.go) — a hand-written LRU, so eviction is something you can see
- [`internal/platform/cache/singleflight.go`](internal/platform/cache/singleflight.go) — 40 lines that prevent a cache stampede from taking the service down
- [`internal/platform/health/health.go`](internal/platform/health/health.go) — why a liveness probe that checks the database *causes* outages
- [`ai/rag/generator.py`](ai/rag/generator.py) — the hallucination guard, and an LLM system prompt where every rule is annotated with the failure it prevents

---

## Testing

```bash
make test          # Go + Python
make test-race     # with the race detector (the only reliable way to find data races)
make test-cover    # HTML coverage report
make e2e           # 118 assertions against the running system
make load-test     # watch the cache and lock escalation under load
```

### What the layers cover

| Layer | Count | Needs infrastructure? | What it proves |
|---|---|---|---|
| Domain | 40+ | No | Money arithmetic is exact; the state machine rejects every illegal transition |
| Service | 30+ | No | Saga compensation, idempotency, hybrid locking, 20-way concurrency |
| Repository | 25+ | Optional | A **shared contract suite** both adapters must pass, so the fake cannot be more permissive than Postgres |
| Transport | 40+ | No | Real HTTP: status codes, JSON field names, middleware ordering |
| Cache | 25+ | No | LRU eviction, TTL expiry, stampede collapse (loader runs **exactly once** under 100 concurrent misses) |
| Python | 60 | No | TF-IDF maths, chunking, intent routing, and the refusal path |
| End-to-end | 118 | Yes | The assembled system: real Postgres, real Redis, 5 processes, real network |

### Two bugs these tests actually caught

Worth recording, because they are the kind that only surface when you run
things:

1. **Go's `ServeMux` returned plain-text 404s**, breaking the API's
   one-error-shape promise. A client with a single error parser would fail on
   the first mistyped URL. Fixed with `httpx.NormalizeErrors()`.

2. **Every Strangler Fig backfill was failing silently.** A legacy reference
   (`LEG-2019-0001`) is not a UUID, so the insert failed with SQLSTATE 22P02 —
   but backfill is deliberately fire-and-forget, so reads kept working and the
   test passed. The e2e assertion was too lenient ("backfill may be
   disabled"). Both the bug and the weak test are fixed, and the reasoning is
   recorded in the code.

---

## Configuration

Nothing calls `os.Getenv` directly. Everything flows through
[`internal/platform/config`](internal/platform/config/config.go), which
validates at boot and **fails fast** with a clear message.

### The knobs that change behaviour

```bash
# LOCKING — the headline knob. Change it and restart to compare.
LOCK_STRATEGY=hybrid              # optimistic | pessimistic | hybrid
LOCK_MAX_RETRIES=3
LOCK_PESSIMISTIC_TIMEOUT=3s

# CACHING
CACHE_ENABLED=true
CACHE_TTL=5m
CACHE_NEGATIVE_TTL=30s            # for "not found" — short on purpose

# STRANGLER FIG — the migration phase, switchable with no deploy
STRANGLER_WRITE_MODE=new          # legacy → dual → new  (phases 1 → 2 → 3)
STRANGLER_READ_FALLBACK=true      # false = phase 4, legacy decommissioned
STRANGLER_BACKFILL_ON_READ=true   # let traffic perform the migration

# INFRASTRUCTURE — set either to false to run without it
POSTGRES_ENABLED=true
REDIS_ENABLED=true

# AI — off by default, so the system is deterministic, offline and free
ASSISTANT_LLM=off
ANTHROPIC_API_KEY=
```

Full reference, including every timeout and worker interval:
[CONFIGURATION.md](docs/CONFIGURATION.md).

---

## Try it yourself

### Create an order

```bash
curl -X POST http://localhost:8080/api/v1/orders \
  -H 'Content-Type: application/json' \
  -d '{
    "customer_id": "cust-1001",
    "customer_email": "john.doe@example.com",
    "shipping_address": "123 Main St, New York, NY 10001",
    "items": [
      {"product_id":"SKU-HEADPHONES","product_name":"Wireless Headphones",
       "quantity":2,"unit_price_minor":4999}
    ]
  }'
```

The response is `PROCESSING`, not `PENDING` — the saga reserved stock and
captured payment across two other services before returning.

### Watch a saga fail and compensate

The payment service declines any amount whose **cents end in 66**, which gives
you a deterministic way to force a failure:

```bash
# 2 × 4983 = 9966  →  declined
curl -X POST http://localhost:8080/api/v1/orders \
  -H 'Content-Type: application/json' \
  -d '{"customer_id":"c1","customer_email":"a@b.com","shipping_address":"x",
       "items":[{"product_id":"SKU-KEYBOARD","product_name":"Keyboard",
                 "quantity":2,"unit_price_minor":4983}]}'

# → 422 payment_declined, and the order is FAILED.
# Check that the reserved stock came back:
curl -s http://localhost:8080/api/v1/orders/stats | python3 -m json.tool
curl -s http://localhost:8081/api/v1/stock | python3 -m json.tool
```

### Watch the Strangler Fig migrate an order

```bash
# This order exists ONLY in the legacy system.
curl -i http://localhost:8080/api/v1/orders/number/LEG-2019-0001 | grep -i x-served-by
# → X-Served-By: legacy

sleep 2

# Read it again: backfill-on-read has migrated it.
curl -i http://localhost:8080/api/v1/orders/number/LEG-2019-0001 | grep -i x-served-by
# → X-Served-By: new

curl -s http://localhost:8080/api/v1/admin/strangler | python3 -m json.tool
```

### Ask the chatbot

```bash
# A policy question → answered from the corpus, with a citation
curl -s -X POST http://localhost:8090/api/v1/assistant/chat \
  -H 'Content-Type: application/json' \
  -d '{"question":"how long do refunds take?"}' | python3 -m json.tool

# An order question → a TOOL CALL for live data
curl -s -X POST http://localhost:8090/api/v1/assistant/chat \
  -d '{"question":"where is my order ORD-XXXXXXXX?"}' | python3 -m json.tool

# An off-topic question → REFUSED, not hallucinated
curl -s -X POST http://localhost:8090/api/v1/assistant/chat \
  -d '{"question":"what is the capital of France?"}' | python3 -m json.tool
```

### See the locking and caching work

```bash
make load-test     # prints cache hit ratio and lock escalations before/after
make stats         # order stats, cache effectiveness, migration progress
make psql          # look at the version column incrementing yourself
make redis-cli     # KEYS order:*   and watch the TTLs
```

---

## Documentation

| Document | What it covers |
|---|---|
| [ARCHITECTURE.md](docs/ARCHITECTURE.md) | Diagrams, layering, request flows, the data model, all the patterns |
| [CODE_WALKTHROUGH.md](docs/CODE_WALKTHROUGH.md) | **Read the codebase in order**, with the lines that matter called out |
| [LOCKING.md](docs/LOCKING.md) | All five locking mechanisms, deadlock avoidance, isolation levels, a decision guide |
| [CACHING.md](docs/CACHING.md) | The 5 cache patterns, invalidation vs eviction, stampedes, TTL strategy |
| [SAGA.md](docs/SAGA.md) | Why not 2PC, orchestration vs choreography, the 4 rules of a saga step |
| [STRANGLER_FIG.md](docs/STRANGLER_FIG.md) | The 4 migration phases and how to run them with no deploy |
| [TRADEOFFS.md](docs/TRADEOFFS.md) | Every significant decision and what it costs |
| [DESIGN_PRINCIPLES.md](docs/DESIGN_PRINCIPLES.md) | KISS, YAGNI, DRY and SOLID, each with its file |
| [AI_RAG.md](docs/AI_RAG.md) | The RAG pipeline, chunking, the hallucination guard, the LLM upgrade path |
| [CONFIGURATION.md](docs/CONFIGURATION.md) | Every environment variable |
| [INTERVIEW_GUIDE.md](docs/INTERVIEW_GUIDE.md) | Likely questions with answers grounded in this code |
| [openapi.yaml](docs/openapi.yaml) | The API contract (also at `/docs` as Swagger UI) |

---

## current limitations

| Limitation | Why | What production needs |
|---|---|---|
| **No authentication** | Auth is a well-understood, orthogonal concern; adding it would add volume without demonstrating anything new about orders, sagas or locking | JWT or mTLS at a gateway, plus per-customer authorisation on every read |
| **Saga state is in-memory** | The `saga_instances` table and its indexes exist and are documented; the recovery *worker* is not implemented | A worker that finds `RUNNING` sagas older than N minutes and completes or compensates them |
| **Inventory/payment are in-memory** | They exist to demonstrate the *saga participant role* and the locking contrast, not to be a second persistence exercise | Postgres with `SELECT … FOR UPDATE` on a stock table |
| **TF-IDF, not embeddings** | It is fully explainable and needs no model download; the limitation (word matching, not meaning) is documented with its mitigation | A real embedding model plus pgvector — two functions change, nothing else |
| **No rate limiting** | Out of scope | A token bucket per API key, at the gateway |
| **Single-region** | Out of scope | Read replicas, and a decision about cross-region cache coherence |

---