#!/usr/bin/env bash
# =============================================================================
# demo.sh — a guided tour of every feature, with the reasoning stated as it
# happens.
#
# This is deliberately different from e2e.sh: that script ASSERTS, this one
# EXPLAINS. Run it when you want to understand what the system does, or when
# you need to walk someone else through it.
# =============================================================================
set -euo pipefail

ORDER="${ORDER_URL:-http://localhost:8080}"
INV="${INVENTORY_URL:-http://localhost:8081}"
PAY="${PAYMENT_URL:-http://localhost:8082}"
AI="${ASSISTANT_URL:-http://localhost:8090}"
PY="${PYTHON:-python3}"

if [[ -t 1 ]]; then
  B=$'\033[1m'; C=$'\033[36m'; G=$'\033[32m'; Y=$'\033[33m'; D=$'\033[2m'; N=$'\033[0m'
else
  B=""; C=""; G=""; Y=""; D=""; N=""
fi

# PAUSE lets a reader keep up. Set DEMO_FAST=1 to run straight through.
pause() { [[ "${DEMO_FAST:-}" == "1" ]] || { printf '\n%s   [enter to continue]%s' "$D" "$N"; read -r; }; }

title() {
  printf '\n\n%s%s%s\n' "$B" "$(printf '═%.0s' {1..72})" "$N"
  printf '%s  %s%s\n' "$B" "$1" "$N"
  printf '%s%s%s\n\n' "$B" "$(printf '═%.0s' {1..72})" "$N"
}

explain() { printf '%s%s%s\n' "$Y" "$1" "$N"; }
note()    { printf '%s  %s%s\n' "$D" "$1" "$N"; }
cmd()     { printf '\n%s$ %s%s\n' "$C" "$1" "$N"; }
pretty()  { "$PY" -m json.tool 2>/dev/null || cat; }

curl -fsS "$ORDER/healthz" >/dev/null 2>&1 || {
  printf 'The system is not running. Start it with:\n    make docker-up\n'
  exit 1
}

printf '\n%sOrder Processing System — guided demo%s\n' "$B" "$N"
printf 'Five services, a distributed saga, three locking strategies,\n'
printf 'a three-layer cache, a legacy migration, and an offline RAG chatbot.\n'
pause

# ---------------------------------------------------------------------------
title "1. Health probes — and why the distinction matters"

explain "/healthz answers 'is this process alive?' and checks NOTHING else."
note "If it checked the database, a 30-second Postgres blip would make every"
note "replica fail liveness at once — and Kubernetes would kill them ALL."
note "Your health check would have caused the outage it was meant to prevent."
cmd "curl $ORDER/healthz"
curl -fsS "$ORDER/healthz" | pretty

explain ""
explain "/readyz answers 'can I serve a request right now?' and DOES check deps."
note "Failing readiness only removes the instance from the load balancer. The"
note "process keeps running and rejoins when its dependencies recover."
note "Note 'critical': postgres is critical, redis is NOT — a cache outage"
note "makes us slower, not broken, so we stay in rotation."
cmd "curl $ORDER/readyz"
curl -fsS "$ORDER/readyz" | pretty
pause

# ---------------------------------------------------------------------------
title "2. Creating an order — a saga across three services"

explain "One POST triggers a distributed transaction:"
note "  step 1  reserve stock      → inventory service (:8081)"
note "  step 2  capture payment    → payment service   (:8082)"
note "  step 3  confirm the order  → local transaction"
note ""
note "Each step has a COMPENSATING ACTION. If step 2 fails, step 1 is undone."
note "The order is committed as PENDING BEFORE any remote call, so a crash"
note "leaves a visible, recoverable order rather than invisible charged money."

BODY='{
  "customer_id": "cust-demo-001",
  "customer_email": "demo@example.com",
  "customer_name": "Demo Customer",
  "shipping_address": "1 Demo Street, London, EC1A 1BB",
  "items": [
    {"product_id":"SKU-HEADPHONES","product_name":"Wireless Headphones","quantity":2,"unit_price_minor":4999},
    {"product_id":"SKU-MOUSE","product_name":"Ergonomic Mouse","quantity":1,"unit_price_minor":2999}
  ]
}'
cmd "curl -X POST $ORDER/api/v1/orders -d '{...2 items...}'"
RESP=$(curl -fsS -X POST -H 'Content-Type: application/json' -d "$BODY" "$ORDER/api/v1/orders")
echo "$RESP" | pretty

ID=$(echo "$RESP" | "$PY" -c "import json,sys; print(json.load(sys.stdin)['id'])")
NUM=$(echo "$RESP" | "$PY" -c "import json,sys; print(json.load(sys.stdin)['order_number'])")

explain ""
explain "Things to notice in that response:"
note "status = PROCESSING, not PENDING — the saga completed across 3 services."
note "total_minor = 12997 (exact cents) AND total = '129.97' (for display)."
note "  The SERVER computed that. The client cannot send a total, so it cannot"
note "  be tampered with. And money is int64 cents, never a float:"
note "  in float64, 0.1 + 0.2 != 0.3, and that becomes an audit finding."
note "item_count = 3 UNITS across 2 lines — the warehouse cares about units."
note "is_cancellable = false, computed server-side so every client agrees."
note "version = 1 — this is what optimistic locking compares against."
note "order_number = $NUM (Crockford base32: no I/L/O/U, so it survives"
note "  being read aloud to support)."
pause

# ---------------------------------------------------------------------------
title "3. The saga's side effects are real, in another process"

explain "The stock was reserved by a DIFFERENT service over HTTP."
cmd "curl $INV/api/v1/stock"
curl -fsS "$INV/api/v1/stock" | pretty | head -20
note "SKU-HEADPHONES shows reserved=2. That is the saga's step 1, and it"
note "happened in a separate container with its own state."
pause

# ---------------------------------------------------------------------------
title "4. Caching — three layers"

explain "Read the order twice and watch the cache serve the second read."
cmd "curl $ORDER/api/v1/orders/$ID   (twice)"
curl -fsS "$ORDER/api/v1/orders/$ID" >/dev/null
H1=$(curl -fsS "$ORDER/metrics" | awk '/^cache_hits_total/{print $2}')
curl -fsS "$ORDER/api/v1/orders/$ID" >/dev/null
H2=$(curl -fsS "$ORDER/metrics" | awk '/^cache_hits_total/{print $2}')
printf '   cache hits: %s%s → %s%s\n' "$G" "$H1" "$H2" "$N"

explain ""
explain "LAYER 1 is HTTP caching — the request never even reaches us."
cmd "curl -i $ORDER/api/v1/orders/$ID | grep -i 'cache-control\|etag'"
curl -fsS -D - -o /dev/null "$ORDER/api/v1/orders/$ID" | grep -i "cache-control\|etag" || true
note "A live order gets 'no-store' because it could change any second."
note "A TERMINAL order (DELIVERED/CANCELLED) is immutable, so it gets"
note "max-age=3600. 'private' is deliberate: an order holds personal data and"
note "must never sit in a shared CDN cache."
note ""
note "LAYER 2 is Redis, shared across replicas. An in-process cache would give"
note "3 replicas 3 different answers, and invalidating one would leave the"
note "other two stale. That is the most common caching mistake in microservices."
pause

# ---------------------------------------------------------------------------
title "5. Cache invalidation on write"

explain "Writes DELETE the cached entry — they never UPDATE it."
note "Why: two concurrent writes could update the cache in the opposite order"
note "to the database, leaving the cache permanently wrong. Deleting is"
note "idempotent and self-correcting: the next read repopulates from truth."
cmd "curl -X PATCH $ORDER/api/v1/orders/$ID/status -d '{\"status\":\"SHIPPED\"}'"
curl -fsS -X PATCH -H 'Content-Type: application/json' \
  -d '{"status":"SHIPPED","reason":"handed to carrier"}' \
  "$ORDER/api/v1/orders/$ID/status" | pretty | head -12
note "version went 1 → 2. The cached entry was deleted, and the list cache"
note "generation was INCREMENTED — one atomic INCR invalidates every cached"
note "list page at once, with no SCAN over the Redis keyspace."
pause

# ---------------------------------------------------------------------------
title "6. The state machine refuses illegal transitions"

explain "An order can only move FORWARDS. Try to un-ship it:"
cmd "curl -X PATCH .../status -d '{\"status\":\"PENDING\"}'"
curl -sS -X PATCH -H 'Content-Type: application/json' -d '{"status":"PENDING"}' \
  "$ORDER/api/v1/orders/$ID/status" | pretty
note "422, not 400: the request was well-formed, the transition is impossible."
note "And the error NAMES what is allowed. 'invalid transition' costs a"
note "developer 20 minutes; 'allowed_next: [DELIVERED]' costs 5 seconds."
note ""
note "The whole state machine is ONE map in internal/order/domain/status.go."
note "Adding a status means editing one table, not hunting five call sites."
pause

# ---------------------------------------------------------------------------
title "7. Saga compensation — forcing a failure on purpose"

explain "The payment service declines any amount whose CENTS END IN 66."
note "A deterministic failure switch, so the compensation path can be"
note "demonstrated. A demo you cannot make FAIL only proves the happy path."

BEFORE=$(curl -fsS "$INV/api/v1/stock" | "$PY" -c "import json,sys; print(json.load(sys.stdin)['stock'].get('SKU-KEYBOARD',{}).get('available',0))")
printf '\n   SKU-KEYBOARD available before: %s%s%s\n' "$G" "$BEFORE" "$N"

cmd "curl -X POST $ORDER/api/v1/orders -d '{... 2 x 4983 = 9966 ...}'"
curl -sS -X POST -H 'Content-Type: application/json' -d '{
  "customer_id":"cust-demo-fail","customer_email":"fail@example.com",
  "shipping_address":"1 Fail Road",
  "items":[{"product_id":"SKU-KEYBOARD","product_name":"Mechanical Keyboard","quantity":2,"unit_price_minor":4983}]
}' "$ORDER/api/v1/orders" | pretty

sleep 1
AFTER=$(curl -fsS "$INV/api/v1/stock" | "$PY" -c "import json,sys; print(json.load(sys.stdin)['stock'].get('SKU-KEYBOARD',{}).get('available',0))")
printf '\n   SKU-KEYBOARD available after:  %s%s%s\n' "$G" "$AFTER" "$N"

explain ""
explain "What happened, in order:"
note "  1. reserve_inventory  SUCCEEDED (2 units held)"
note "  2. charge_payment     FAILED (declined)"
note "  3. COMPENSATION ran in REVERSE: the reservation was released"
note "  4. the order was marked FAILED, with the reason recorded"
note ""
note "Stock went $BEFORE → $AFTER. Nothing leaked. Without compensation, every"
note "declined card would silently remove inventory from sale forever."
note ""
note "Note also: a refund is NOT a rollback. A captured charge HAPPENED and is"
note "on the customer's statement. Compensation is a NEW forward transaction"
note "that cancels the effect. Once you cross a service boundary, 'undo' is"
note "always a forward action."
pause

# ---------------------------------------------------------------------------
title "8. Idempotency — why a retry cannot double-charge"

explain "The scenario that makes this non-negotiable:"
note "  POST /payments  → the card IS charged → the response is LOST to a timeout"
note "  The caller cannot know whether it worked. Retrying risks charging twice;"
note "  giving up risks an order nobody paid for. BOTH are wrong."
note ""
explain "The only correct answer is an idempotency key."

KEY="demo-$RANDOM"
CHARGE='{"order_id":"demo-order","customer_id":"c1","customer_email":"a@b.com","amount_minor":5000,"currency":"USD"}'
cmd "curl -X POST $PAY/api/v1/payments -H 'Idempotency-Key: $KEY'   (twice)"
P1=$(curl -fsS -X POST -H 'Content-Type: application/json' -H "Idempotency-Key: $KEY" -d "$CHARGE" "$PAY/api/v1/payments" | "$PY" -c "import json,sys; print(json.load(sys.stdin)['payment_id'])")
P2=$(curl -fsS -X POST -H 'Content-Type: application/json' -H "Idempotency-Key: $KEY" -d "$CHARGE" "$PAY/api/v1/payments" | "$PY" -c "import json,sys; print(json.load(sys.stdin)['payment_id'])")
printf '   first  charge → %s\n   retried charge → %s\n' "$P1" "$P2"
[[ "$P1" == "$P2" ]] && printf '   %sSame payment id. The card was charged ONCE.%s\n' "$G" "$N"

explain ""
explain "The key must be DETERMINISTIC, derived from the logical operation:"
note "  idempotencyKey(orderID, 'charge') → 'order-123:charge'"
note "A random key per attempt would make every retry look like a NEW charge."
note "This is exactly how Stripe's API works."
pause

# ---------------------------------------------------------------------------
title "9. Concurrency — hybrid locking under contention"

explain "20 simultaneous updates to ONE order. Watch what the server does."
R2=$(curl -fsS -X POST -H 'Content-Type: application/json' -d "$BODY" "$ORDER/api/v1/orders")
RID=$(echo "$R2" | "$PY" -c "import json,sys; print(json.load(sys.stdin)['id'])")

ESC_BEFORE=$(curl -fsS "$ORDER/metrics" | awk '/^order_lock_escalations_total/{print $2}'); ESC_BEFORE=${ESC_BEFORE:-0}
CONF_BEFORE=$(curl -fsS "$ORDER/metrics" | awk '/^order_version_conflicts_total/{print $2}'); CONF_BEFORE=${CONF_BEFORE:-0}

cmd "20 x PATCH /orders/$RID/status  (concurrently)"
TMP=$(mktemp -d)
for i in $(seq 1 20); do
  ( curl -sS -o /dev/null -w '%{http_code}' -X PATCH -H 'Content-Type: application/json' \
      -d '{"status":"SHIPPED","reason":"concurrent"}' \
      "$ORDER/api/v1/orders/$RID/status" > "$TMP/$i" 2>/dev/null ) &
done
wait
OK=$(cat "$TMP"/* | grep -c '^200$' || true)
ERR=$(cat "$TMP"/* | grep -cE '^(000|5..)$' || true)
rm -rf "$TMP"

ESC_AFTER=$(curl -fsS "$ORDER/metrics" | awk '/^order_lock_escalations_total/{print $2}'); ESC_AFTER=${ESC_AFTER:-0}
CONF_AFTER=$(curl -fsS "$ORDER/metrics" | awk '/^order_version_conflicts_total/{print $2}'); CONF_AFTER=${CONF_AFTER:-0}

printf '\n   succeeded:          %s\n' "$OK"
printf '   server errors:      %s%s%s  ← must be 0\n' "$G" "$ERR" "$N"
printf '   version conflicts:  %s → %s%s%s\n' "$CONF_BEFORE" "$C" "$CONF_AFTER" "$N"
printf '   lock escalations:   %s → %s%s%s\n' "$ESC_BEFORE" "$C" "$ESC_AFTER" "$N"

explain ""
explain "The HYBRID strategy, which is the answer to 'which lock should I use?':"
note "  attempts 1-2  OPTIMISTIC: no lock held. UPDATE ... WHERE version = N."
note "                0 rows means someone else won, so RE-READ and retry."
note "                (Retrying with the same stale version would fail forever.)"
note "  final attempt ESCALATE to a real row lock (SELECT ... FOR UPDATE),"
note "                because repeated conflicts are EVIDENCE of contention."
note ""
note "Why not always pessimistic? 99% of updates have no contention, and"
note "locking them all pays a serialisation cost to prevent a rare problem."
note "Why not always optimistic? On a hot row, retries thrash and some users"
note "get a 409 for something the server could have resolved by waiting 3ms."
note ""
note "Switch with LOCK_STRATEGY=optimistic|pessimistic|hybrid and compare."
pause

# ---------------------------------------------------------------------------
title "10. Strangler Fig — migrating off a legacy system"

explain "LEG-2019-0001 exists ONLY in the old system (a separate service)."
cmd "curl -i $ORDER/api/v1/orders/number/LEG-2019-0001 | grep x-served-by"
S1=$(curl -fsS -D - -o /dev/null "$ORDER/api/v1/orders/number/LEG-2019-0001" | grep -i 'x-served-by' | tr -d '\r')
printf '   %s%s%s\n' "$C" "$S1" "$N"
curl -fsS "$ORDER/api/v1/orders/number/LEG-2019-0001" | pretty | head -16

explain ""
explain "That record was TRANSLATED on the way through:"
note "  order_ref      → order_number"
note "  'COMPLETE'     → DELIVERED       (their status vocabulary is different)"
note "  '1299.00'      → 129900 cents    (they store money as a STRING)"
note "  source         → 'legacy'        (so GROUP BY source measures progress)"
note ""
explain "Now read it again — ordinary traffic has MIGRATED it:"
sleep 2
S2=$(curl -fsS -D - -o /dev/null "$ORDER/api/v1/orders/number/LEG-2019-0001" | grep -i 'x-served-by' | tr -d '\r')
printf '   %s%s%s\n' "$G" "$S2" "$N"
note "That is 'backfill on read': the orders customers actually look at are"
note "the ones worth migrating, warmest first, with no batch job and no"
note "maintenance window."
note ""
note "NOTE: a backfill deliberately emits NO event. Publishing order.created"
note "for a 2019 order would re-send confirmation emails to customers for"
note "orders they received years ago — the classic migration embarrassment."

cmd "curl $ORDER/api/v1/admin/strangler"
curl -fsS "$ORDER/api/v1/admin/strangler" | pretty
note "legacy_share is the number that answers 'can we delete the old system?'"
note "When it reaches 0 and stays there across a full business cycle, yes."
note ""
note "All four phases are env-var changes, so ROLLBACK IS INSTANT:"
note "  phase 1  STRANGLER_WRITE_MODE=legacy   legacy authoritative"
note "  phase 2  STRANGLER_WRITE_MODE=dual     write both, compare"
note "  phase 3  STRANGLER_WRITE_MODE=new      new authoritative  ← we are here"
note "  phase 4  STRANGLER_READ_FALLBACK=false legacy decommissioned"
pause

# ---------------------------------------------------------------------------
title "11. The transactional outbox"

explain "The dual-write problem this solves:"
note "  db.Save(order)        ← succeeds"
note "  queue.Publish(event)  ← network error!"
note "Now the order exists but nobody knows. Swap the lines and you get the"
note "opposite bug. You cannot fix this by being careful: a database and a"
note "broker cannot be updated atomically without distributed transactions."
note ""
explain "The fix: write the event INTO THE SAME DATABASE, IN THE SAME TRANSACTION."
cmd "docker compose exec postgres psql -c 'SELECT ... FROM outbox'"
docker compose exec -T postgres psql -U orders -d orders -c \
  "SELECT event_type, (published_at IS NOT NULL) AS published, COUNT(*) FROM outbox GROUP BY 1,2 ORDER BY 1;" 2>/dev/null || true
note ""
note "A worker then polls with FOR UPDATE SKIP LOCKED, which lets N workers"
note "claim DISJOINT batches with no coordination service. That one clause is"
note "why this project needs no Kafka for this workload."
note ""
note "The guarantee is AT-LEAST-ONCE: the worker could crash after publishing"
note "but before marking the row, so an event may be delivered twice. That is"
note "unavoidable, which is why consumers must be idempotent on event id."
note "'Exactly once' delivery does not exist — at-least-once plus idempotent"
note "consumers is what it means in practice."
pause

# ---------------------------------------------------------------------------
title "12. The AI assistant — RAG with a hallucination guard"

explain "FIRST it classifies the question, because there are two kinds:"
note "  'what is your refund policy?'  → written down → RETRIEVE (RAG)"
note "  'where is ORD-XXXX?'           → a live fact  → TOOL CALL"
note ""
note "A system that treats everything as RAG answers the second by retrieving"
note "the shipping policy and confidently explaining typical delivery times."
note "Not what was asked, and indistinguishable from a correct answer."

cmd "POST $AI/api/v1/assistant/chat  {'question':'how long do refunds take?'}"
curl -fsS -X POST -H 'Content-Type: application/json' \
  -d '{"question":"how long do refunds take?"}' "$AI/api/v1/assistant/chat" | pretty
note "grounded=true, and it CITES its source. An answer you cannot verify is"
note "just an assertion."

explain ""
explain "Now an order question — this needs LIVE data, so it calls a tool:"
cmd "POST .../chat  {'question':'where is my order $NUM?'}"
curl -fsS -X POST -H 'Content-Type: application/json' \
  -d "{\"question\":\"where is my order $NUM?\"}" "$AI/api/v1/assistant/chat" | pretty
note "used_tool=true. It queried the order service for the real status."
note "Note what the answer does NOT contain: no email, no postal address."
note "Those fields are not in the data structure at all, so a careless prompt"
note "cannot leak them. Designing the limit in beats instructing a model."

explain ""
explain "And the most important behaviour — REFUSING to answer:"
cmd "POST .../chat  {'question':'what is the capital of France?'}"
curl -fsS -X POST -H 'Content-Type: application/json' \
  -d '{"question":"what is the capital of France?"}' "$AI/api/v1/assistant/chat" | pretty
note "grounded=false, and it says so. Below a cosine-similarity threshold we"
note "REFUSE rather than summarising the least-irrelevant document."
note ""
note "'I do not know' is a GOOD answer. A fluent wrong one is worse than"
note "useless, because nobody catches it."
note ""
note "This runs with NO API key, NO network and NO pip install: TF-IDF plus"
note "cosine similarity, ~200 lines you can fully explain. The limitation"
note "(it matches words, not meaning) is documented, along with the two-line"
note "upgrade path to a real embedding model."
pause

# ---------------------------------------------------------------------------
title "13. Observability"

explain "Every log line carries request_id, propagated across ALL FIVE services."
cmd "curl -H 'X-Request-Id: my-trace-123' $ORDER/api/v1/orders/$ID"
curl -fsS -D - -o /dev/null -H 'X-Request-Id: my-trace-123' "$ORDER/api/v1/orders/$ID" | grep -i 'x-request-id' | tr -d '\r'
note "One customer complaint = one grep, across every service."

explain ""
cmd "curl $ORDER/metrics | grep -E 'cache_hit_ratio|escalations|saga_'"
curl -fsS "$ORDER/metrics" | grep -E "^(cache_hit_ratio|order_lock_escalations_total|saga_completed_total|saga_compensated_total)" || true
note ""
note "The series that matter most:"
note "  saga_compensation_failed_total  ← ALERT. A customer may be charged for"
note "                                    an order they will never receive."
note "  cache_hit_ratio                 ← below 0.8 on read-heavy load means"
note "                                    the TTL is too short."
note "  http_request_duration_seconds   ← a HISTOGRAM, so you get p99."
note "                                    An average hides the tail: 99 requests"
note "                                    at 1ms and one at 10s averages 100ms,"
note "                                    which NO user experienced."
pause

# ---------------------------------------------------------------------------
title "Done"

printf 'Explore further:\n\n'
printf '  %smake e2e%s         118 assertions against this running system\n' "$C" "$N"
printf '  %smake load-test%s   watch the cache and lock escalation under load\n' "$C" "$N"
printf '  %smake stats%s       order stats, cache ratio, migration progress\n' "$C" "$N"
printf '  %smake psql%s        watch the version column increment yourself\n' "$C" "$N"
printf '  %smake redis-cli%s   KEYS order:*  and inspect the TTLs\n' "$C" "$N"
printf '  %smake watch-logs%s  structured logs with request_id\n' "$C" "$N"
printf '\n'
printf 'Read the code in this order:\n'
printf '  1. cmd/orderd/main.go                        the composition root\n'
printf '  2. internal/order/domain/order.go            business rules, zero infra\n'
printf '  3. internal/order/service/order_service.go   use cases + locking\n'
printf '  4. internal/order/repository/postgres.go     the three strategies as SQL\n'
printf '  5. internal/order/service/saga.go            the distributed transaction\n'
printf '\n'
printf 'Then: docs/CODE_WALKTHROUGH.md and docs/TRADEOFFS.md\n\n'
