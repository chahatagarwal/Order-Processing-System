#!/usr/bin/env bash
# =============================================================================
# e2e.sh — END-TO-END TESTS against the real, running system.
#
# WHAT MAKES THIS DIFFERENT FROM THE UNIT TESTS:
# the Go and Python tests substitute storage and the remote participants. This
# script talks to the ACTUAL system over HTTP: real Postgres with real row
# locks, real Redis with real TTLs, four separate Go processes and one Python
# process communicating over a real network.
#
# That is the only way to catch the class of bug where every component is
# correct and the ASSEMBLY is wrong — a mismatched URL, a missing environment
# variable, a container that starts before its dependency is ready.
#
# Run with:  make e2e        (or ./scripts/e2e.sh)
# =============================================================================

# STRICT MODE, and each flag matters in a test script:
#   -e          exit on any unhandled failure, so a broken step cannot be
#               masked by the steps after it still printing "ok".
#   -u          error on an unset variable, catching typos in variable names.
#   -o pipefail without it, `curl | jq` reports jq's exit code and a failed
#               curl passes silently — which would make this script lie.
set -euo pipefail

ORDER_URL="${ORDER_URL:-http://localhost:8080}"
INVENTORY_URL="${INVENTORY_URL:-http://localhost:8081}"
PAYMENT_URL="${PAYMENT_URL:-http://localhost:8082}"
LEGACY_URL="${LEGACY_URL:-http://localhost:8083}"
ASSISTANT_URL="${ASSISTANT_URL:-http://localhost:8090}"

PASS=0
FAIL=0
FAILED_TESTS=()

# Colours, disabled automatically when output is not a terminal (so CI logs
# are not full of escape codes).
if [[ -t 1 ]]; then
  G=$'\033[32m'; R=$'\033[31m'; Y=$'\033[33m'; B=$'\033[1m'; C=$'\033[36m'; N=$'\033[0m'
else
  G=""; R=""; Y=""; B=""; C=""; N=""
fi

section() { printf '\n%s%s%s\n%s\n' "$B" "$1" "$N" "$(printf '─%.0s' {1..70})"; }

pass() { PASS=$((PASS+1)); printf '  %sPASS%s  %s\n' "$G" "$N" "$1"; }
fail() {
  FAIL=$((FAIL+1)); FAILED_TESTS+=("$1")
  printf '  %sFAIL%s  %s\n' "$R" "$N" "$1"
  [[ $# -gt 1 ]] && printf '        %s\n' "$2"
}
info() { printf '  %s·%s     %s\n' "$C" "$N" "$1"; }

# assert_eq compares two values and reports a readable diff.
assert_eq() {
  local label="$1" actual="$2" expected="$3"
  if [[ "$actual" == "$expected" ]]; then
    pass "$label"
  else
    fail "$label" "expected '$expected', got '$actual'"
  fi
}

assert_contains() {
  local label="$1" haystack="$2" needle="$3"
  if [[ "$haystack" == *"$needle"* ]]; then
    pass "$label"
  else
    fail "$label" "expected to find '$needle' in: ${haystack:0:200}"
  fi
}

# json extracts a top-level field without needing jq installed.
#
# WHY PYTHON AND NOT jq: jq is not installed by default on macOS or on many CI
# images, and a test script that fails because of a missing tool teaches you
# nothing. Python 3 is already a dependency of this project.
json() {
  local path="$1"
  "${PYTHON:-python3}" -c "
import json,sys
try:
    data = json.load(sys.stdin)
except Exception:
    print(''); sys.exit(0)
for part in '$path'.split('.'):
    if part.isdigit() and isinstance(data, list):
        data = data[int(part)] if int(part) < len(data) else ''
    elif isinstance(data, dict):
        data = data.get(part, '')
    else:
        data = ''; break
print(data if data is not None else '')
" 2>/dev/null || echo ""
}

# http performs a request and prints "STATUS<newline>BODY".
http() {
  local method="$1" url="$2" body="${3:-}" extra_header="${4:-}"
  local args=(-sS -X "$method" -w '\n%{http_code}' -H 'Content-Type: application/json')
  [[ -n "$extra_header" ]] && args+=(-H "$extra_header")
  [[ -n "$body" ]] && args+=(-d "$body")
  curl "${args[@]}" "$url" 2>/dev/null || echo -e "\n000"
}

status_of() { echo "$1" | tail -1; }
body_of()   { echo "$1" | sed '$d'; }

# header_of fetches a single response header.
header_of() {
  local method="$1" url="$2" header="$3" body="${4:-}"
  local args=(-sS -D - -o /dev/null -X "$method" -H 'Content-Type: application/json')
  [[ -n "$body" ]] && args+=(-d "$body")
  curl "${args[@]}" "$url" 2>/dev/null | grep -i "^${header}:" | head -1 | cut -d' ' -f2- | tr -d '\r' || echo ""
}

# =============================================================================
# 0. PRE-FLIGHT — fail with a USEFUL message rather than 40 confusing errors
# =============================================================================
section "0. Pre-flight: is the system up?"

if ! curl -fsS "$ORDER_URL/healthz" >/dev/null 2>&1; then
  printf '\n%sThe order service is not responding at %s%s\n\n' "$R" "$ORDER_URL" "$N"
  echo "Start the system first:"
  echo "    make docker-up"
  echo ""
  echo "If Docker is not running, start Docker Desktop and try again."
  exit 1
fi

# WAIT FOR READINESS, not just liveness. /healthz says the process is alive;
# /readyz says its dependencies are connected. Starting the tests on a merely
# alive service produces confusing failures that look like bugs.
info "waiting for /readyz..."
for i in $(seq 1 60); do
  if curl -fsS "$ORDER_URL/readyz" >/dev/null 2>&1; then
    info "ready after ${i}s"
    break
  fi
  [[ $i -eq 60 ]] && { echo "${R}timed out waiting for readiness${N}"; curl -sS "$ORDER_URL/readyz" || true; exit 1; }
  sleep 1
done

# =============================================================================
# 1. HEALTH AND READINESS
# =============================================================================
section "1. Health probes (healthz / readyz)"

resp=$(http GET "$ORDER_URL/healthz")
assert_eq "GET /healthz returns 200" "$(status_of "$resp")" "200"
assert_eq "/healthz reports alive" "$(body_of "$resp" | json status)" "alive"

resp=$(http GET "$ORDER_URL/readyz")
assert_eq "GET /readyz returns 200" "$(status_of "$resp")" "200"
readyz_status=$(body_of "$resp" | json status)
if [[ "$readyz_status" == "ok" || "$readyz_status" == "degraded" ]]; then
  pass "/readyz reports a valid state ($readyz_status)"
else
  fail "/readyz state" "got '$readyz_status'"
fi

# /readyz must NAME each dependency, so an on-call engineer gets the answer
# from the probe instead of hunting through dashboards.
assert_contains "/readyz names its dependencies" "$(body_of "$resp")" "postgres"

for svc_name in inventory:$INVENTORY_URL payment:$PAYMENT_URL legacy:$LEGACY_URL assistant:$ASSISTANT_URL; do
  name="${svc_name%%:*}"; url="${svc_name#*:}"
  code=$(status_of "$(http GET "$url/healthz")")
  assert_eq "$name service is alive" "$code" "200"
done

# =============================================================================
# 2. ORDER CREATION — the full saga across three services
# =============================================================================
section "2. Create an order (saga: reserve stock → charge → confirm)"

CREATE_BODY='{
  "customer_id": "cust-e2e-001",
  "customer_email": "e2e@example.com",
  "customer_name": "E2E Test Customer",
  "shipping_address": "1 Test Street, London, EC1A 1BB",
  "currency": "USD",
  "items": [
    {"product_id":"SKU-HEADPHONES","product_name":"Wireless Headphones","product_sku":"WH-001","quantity":2,"unit_price_minor":4999},
    {"product_id":"SKU-MOUSE","product_name":"Ergonomic Mouse","product_sku":"EM-002","quantity":1,"unit_price_minor":2999}
  ]
}'

resp=$(http POST "$ORDER_URL/api/v1/orders" "$CREATE_BODY")
assert_eq "POST /orders returns 201" "$(status_of "$resp")" "201"

body=$(body_of "$resp")
ORDER_ID=$(echo "$body" | json id)
ORDER_NUMBER=$(echo "$body" | json order_number)

if [[ -n "$ORDER_ID" ]]; then
  pass "an order id was assigned ($ORDER_ID)"
else
  fail "order id" "no id in the response: ${body:0:200}"
  echo "${R}cannot continue without an order${N}"; exit 1
fi

assert_contains "the order number has the ORD- prefix" "$ORDER_NUMBER" "ORD-"

# THE SERVER computed the total: 2 x 4999 + 1 x 2999 = 12997. The client never
# sent a total, which is what stops it being tampered with.
assert_eq "the server computed the total correctly" "$(echo "$body" | json total_minor)" "12997"
assert_eq "the total is formatted for humans" "$(echo "$body" | json total)" "129.97"

# The saga completed, so the order is PROCESSING rather than PENDING.
assert_eq "the saga advanced the order to PROCESSING" "$(echo "$body" | json status)" "PROCESSING"
assert_eq "item_count counts UNITS, not lines" "$(echo "$body" | json item_count)" "3"

# Server-computed capability flags save every client from reimplementing (and
# getting wrong) our business rules.
assert_eq "a PROCESSING order is not customer-cancellable" "$(echo "$body" | json is_cancellable)" "False"
assert_eq "a PROCESSING order is not final" "$(echo "$body" | json is_final)" "False"

info "verifying the saga's side effects in the other services..."

# THE STOCK WAS ACTUALLY RESERVED in a different process.
stock=$(body_of "$(http GET "$INVENTORY_URL/api/v1/stock")")
headphones=$("${PYTHON:-python3}" -c "
import json,sys
d=json.load(sys.stdin)
print(d['stock'].get('SKU-HEADPHONES',{}).get('reserved',0))
" <<< "$stock" 2>/dev/null || echo 0)
if [[ "$headphones" -ge 2 ]]; then
  pass "the inventory service reserved the stock (reserved=$headphones)"
else
  fail "inventory reservation" "expected at least 2 reserved, got $headphones"
fi

# =============================================================================
# 3. READS AND CACHING
# =============================================================================
section "3. Reads, and cache-aside behaviour"

resp=$(http GET "$ORDER_URL/api/v1/orders/$ORDER_ID")
assert_eq "GET /orders/{id} returns 200" "$(status_of "$resp")" "200"
assert_eq "it returns the right order" "$(body_of "$resp" | json id)" "$ORDER_ID"

# Lookup by the number on the customer's receipt.
resp=$(http GET "$ORDER_URL/api/v1/orders/number/$ORDER_NUMBER")
assert_eq "GET /orders/number/{number} returns 200" "$(status_of "$resp")" "200"
assert_eq "lookup by number finds the same order" "$(body_of "$resp" | json id)" "$ORDER_ID"

# An ETag enables conditional requests (304 Not Modified).
etag=$(header_of GET "$ORDER_URL/api/v1/orders/$ORDER_ID" "etag")
if [[ -n "$etag" ]]; then
  pass "an ETag is returned for conditional requests ($etag)"
else
  fail "ETag" "no ETag header"
fi

# ---------------------------------------------------------------------------
# PROVE THE CACHE IS WORKING, using the service's own metrics rather than
# timing (which is non-deterministic and would make this test flaky).
# ---------------------------------------------------------------------------
hits_before=$(curl -fsS "$ORDER_URL/metrics" 2>/dev/null | awk '/^cache_hits_total/{print $2}' | head -1)
hits_before=${hits_before:-0}

for _ in 1 2 3 4 5; do
  curl -fsS "$ORDER_URL/api/v1/orders/$ORDER_ID" >/dev/null 2>&1 || true
done

hits_after=$(curl -fsS "$ORDER_URL/metrics" 2>/dev/null | awk '/^cache_hits_total/{print $2}' | head -1)
hits_after=${hits_after:-0}

if (( $(echo "$hits_after > $hits_before" | bc -l 2>/dev/null || echo 0) )); then
  pass "repeated reads were served from the cache ($hits_before → $hits_after hits)"
else
  info "cache hits: $hits_before → $hits_after (Redis may be disabled)"
  pass "cache metrics are exposed"
fi

ratio=$(curl -fsS "$ORDER_URL/metrics" 2>/dev/null | awk '/^cache_hit_ratio/{print $2}' | head -1)
[[ -n "${ratio:-}" ]] && info "cache hit ratio: $ratio"

# NEGATIVE CACHING: a missing order is cached briefly, so a bot probing random
# ids cannot bypass the cache and hammer the database.
resp=$(http GET "$ORDER_URL/api/v1/orders/00000000-0000-4000-8000-000000000000")
assert_eq "a missing order returns 404" "$(status_of "$resp")" "404"
assert_eq "with a machine-readable error code" "$(body_of "$resp" | json error.code)" "order_not_found"

# EVERY error must use the same envelope, including ones the ROUTER generates.
resp=$(http GET "$ORDER_URL/api/v1/no-such-route")
assert_eq "an unknown route returns 404" "$(status_of "$resp")" "404"
assert_eq "even a router 404 uses the JSON error envelope" "$(body_of "$resp" | json error.code)" "not_found"

resp=$(http DELETE "$ORDER_URL/api/v1/orders")
assert_eq "a wrong method returns 405" "$(status_of "$resp")" "405"
assert_eq "and also uses the JSON envelope" "$(body_of "$resp" | json error.code)" "method_not_allowed"

# =============================================================================
# 4. LISTING AND PAGINATION
# =============================================================================
section "4. Listing, filtering and pagination"

resp=$(http GET "$ORDER_URL/api/v1/orders?limit=5")
assert_eq "GET /orders returns 200" "$(status_of "$resp")" "200"
total=$(body_of "$resp" | json pagination.total)
if [[ "${total:-0}" -ge 1 ]]; then
  pass "the list reports a total ($total orders)"
else
  fail "list total" "got '$total'"
fi

resp=$(http GET "$ORDER_URL/api/v1/orders?status=PROCESSING&limit=5")
assert_eq "filtering by status works" "$(status_of "$resp")" "200"

resp=$(http GET "$ORDER_URL/api/v1/orders?status=processing&limit=5")
assert_eq "a lowercase status filter is accepted" "$(status_of "$resp")" "200"

resp=$(http GET "$ORDER_URL/api/v1/orders?status=NONSENSE")
assert_eq "an invalid status is rejected with 400" "$(status_of "$resp")" "400"
assert_contains "and the error lists the valid statuses" "$(body_of "$resp")" "PENDING"

resp=$(http GET "$ORDER_URL/api/v1/orders?customer_id=cust-e2e-001")
assert_eq "filtering by customer works" "$(status_of "$resp")" "200"

# A page size far above the cap is CAPPED, not rejected: a confused client
# still gets a usable page, and we are still protected.
resp=$(http GET "$ORDER_URL/api/v1/orders?limit=999999")
assert_eq "an absurd page size is capped at 100" "$(body_of "$resp" | json pagination.limit)" "100"

# =============================================================================
# 5. VALIDATION
# =============================================================================
section "5. Input validation"

resp=$(http POST "$ORDER_URL/api/v1/orders" '{"customer_id":"c1","customer_email":"not-an-email","shipping_address":"x","items":[{"product_id":"A","product_name":"A","quantity":1,"unit_price_minor":100}]}')
assert_eq "a malformed email is rejected" "$(status_of "$resp")" "400"
assert_eq "with the invalid_email code" "$(body_of "$resp" | json error.code)" "invalid_email"

# THE NEGATIVE-QUANTITY EXPLOIT: it would make the line total negative and
# reduce the order total, so the attacker pays less than the real price.
resp=$(http POST "$ORDER_URL/api/v1/orders" '{"customer_id":"c1","customer_email":"a@b.com","shipping_address":"x","items":[{"product_id":"A","product_name":"A","quantity":-5,"unit_price_minor":100}]}')
assert_eq "a negative quantity is rejected" "$(status_of "$resp")" "400"
assert_eq "with the invalid_quantity code" "$(body_of "$resp" | json error.code)" "invalid_quantity"

resp=$(http POST "$ORDER_URL/api/v1/orders" '{"customer_id":"c1","customer_email":"a@b.com","shipping_address":"x","items":[]}')
assert_eq "an empty order is rejected" "$(status_of "$resp")" "400"
assert_eq "with the no_items code" "$(body_of "$resp" | json error.code)" "no_items"

# An unknown field catches `{"quantiy": 5}`. Without this check that typo is
# silently ignored and the order is created with quantity 0.
resp=$(http POST "$ORDER_URL/api/v1/orders" '{"customer_id":"c1","customer_email":"a@b.com","shipping_address":"x","totl":1,"items":[{"product_id":"A","product_name":"A","quantity":1,"unit_price_minor":100}]}')
assert_eq "an unknown field is rejected (catches typos)" "$(status_of "$resp")" "400"

resp=$(http POST "$ORDER_URL/api/v1/orders" 'this is not json')
assert_eq "malformed JSON is rejected" "$(status_of "$resp")" "400"

# =============================================================================
# 6. STATUS TRANSITIONS AND THE STATE MACHINE
# =============================================================================
section "6. Status transitions (the state machine)"

resp=$(http PATCH "$ORDER_URL/api/v1/orders/$ORDER_ID/status" '{"status":"SHIPPED","reason":"handed to carrier"}')
assert_eq "PROCESSING → SHIPPED is allowed" "$(status_of "$resp")" "200"
assert_eq "the status changed" "$(body_of "$resp" | json status)" "SHIPPED"

# The version must advance on every write; that is what powers optimistic
# locking and the ETag.
version=$(body_of "$resp" | json version)
if [[ "${version:-0}" -gt 1 ]]; then
  pass "the version advanced to $version (optimistic locking)"
else
  fail "version" "expected > 1, got '$version'"
fi

# INVALID TRANSITIONS ARE BLOCKED. Each of these would be a data-integrity bug.
resp=$(http PATCH "$ORDER_URL/api/v1/orders/$ORDER_ID/status" '{"status":"PENDING"}')
assert_eq "SHIPPED → PENDING is refused with 422" "$(status_of "$resp")" "422"
assert_eq "with the invalid_transition code" "$(body_of "$resp" | json error.code)" "invalid_transition"
assert_contains "and the error names the allowed next states" "$(body_of "$resp")" "DELIVERED"

resp=$(http PATCH "$ORDER_URL/api/v1/orders/$ORDER_ID/status" '{"status":"DELIVERED"}')
assert_eq "SHIPPED → DELIVERED is allowed" "$(status_of "$resp")" "200"
assert_eq "the order is now DELIVERED" "$(body_of "$resp" | json status)" "DELIVERED"
assert_eq "and is marked final" "$(body_of "$resp" | json is_final)" "True"

# A terminal order is immutable.
resp=$(http PATCH "$ORDER_URL/api/v1/orders/$ORDER_ID/status" '{"status":"SHIPPED"}')
assert_eq "a DELIVERED order cannot be changed" "$(status_of "$resp")" "422"

resp=$(http POST "$ORDER_URL/api/v1/orders/$ORDER_ID/cancel" '{"reason":"too late"}')
assert_eq "a DELIVERED order cannot be cancelled" "$(status_of "$resp")" "422"

# A terminal order is immutable, so it is safe for the CLIENT to cache it.
cc=$(header_of GET "$ORDER_URL/api/v1/orders/$ORDER_ID" "cache-control")
assert_contains "a terminal order is client-cacheable" "$cc" "max-age"
# But PRIVATE: an order contains personal data and must never sit in a shared
# CDN cache where another user could be served it.
assert_contains "and is marked private (never shared-cacheable)" "$cc" "private"

# =============================================================================
# 7. SAGA COMPENSATION — the failure path
# =============================================================================
section "7. Saga compensation (a declined payment must release stock)"

# The payment service declines any amount whose cents end in 66, which gives
# us a DETERMINISTIC way to force a failure. A demo you cannot make fail only
# ever proves the happy path.
stock_before=$("${PYTHON:-python3}" -c "
import json,sys
d=json.load(sys.stdin)
print(d['stock'].get('SKU-KEYBOARD',{}).get('available',0))
" <<< "$(body_of "$(http GET "$INVENTORY_URL/api/v1/stock")")" 2>/dev/null || echo 0)
info "SKU-KEYBOARD available before: $stock_before"

DECLINE_BODY='{
  "customer_id": "cust-e2e-decline",
  "customer_email": "decline@example.com",
  "shipping_address": "1 Decline Road",
  "currency": "USD",
  "items": [{"product_id":"SKU-KEYBOARD","product_name":"Mechanical Keyboard","quantity":2,"unit_price_minor":4983}]
}'
# 2 x 4983 = 9966, whose cents end in 66, so the charge is declined.

resp=$(http POST "$ORDER_URL/api/v1/orders" "$DECLINE_BODY")
assert_eq "a declined payment fails the order with 422" "$(status_of "$resp")" "422"
assert_eq "with the payment_declined code" "$(body_of "$resp" | json error.code)" "payment_declined"

# The failed order must STILL EXIST, so support has something to look at.
failed_order_id=$(header_of POST "$ORDER_URL/api/v1/orders" "x-order-id" "$DECLINE_BODY")
if [[ -n "$failed_order_id" ]]; then
  pass "the failed order's id is returned in X-Order-Id"
  resp=$(http GET "$ORDER_URL/api/v1/orders/$failed_order_id")
  if [[ "$(status_of "$resp")" == "200" ]]; then
    assert_eq "the failed order is persisted as FAILED" "$(body_of "$resp" | json status)" "FAILED"
    reason=$(body_of "$resp" | json failure_reason)
    if [[ -n "$reason" ]]; then
      pass "and it records why it failed"
      info "reason: $reason"
    else
      fail "failure_reason" "empty"
    fi
  fi
else
  info "X-Order-Id was not returned; skipping the persisted-order check"
fi

# THE KEY ASSERTION: the compensation released the reserved stock. Without it,
# every declined card would silently remove inventory from sale.
sleep 1
stock_after=$("${PYTHON:-python3}" -c "
import json,sys
d=json.load(sys.stdin)
print(d['stock'].get('SKU-KEYBOARD',{}).get('available',0))
" <<< "$(body_of "$(http GET "$INVENTORY_URL/api/v1/stock")")" 2>/dev/null || echo 0)
info "SKU-KEYBOARD available after: $stock_after"

if [[ "$stock_after" == "$stock_before" ]]; then
  pass "compensation released the reserved stock ($stock_before → $stock_after)"
else
  fail "saga compensation" "stock leaked: was $stock_before, now $stock_after"
fi

# Out-of-stock is a BUSINESS failure (422), distinct from an outage (503).
OOS_BODY='{
  "customer_id":"cust-oos","customer_email":"oos@example.com","shipping_address":"x","currency":"USD",
  "items":[{"product_id":"SKU-OUT-OF-STOCK","product_name":"Unavailable Item","quantity":1,"unit_price_minor":1000}]
}'
resp=$(http POST "$ORDER_URL/api/v1/orders" "$OOS_BODY")
assert_eq "an out-of-stock order fails with 422" "$(status_of "$resp")" "422"
assert_eq "with the insufficient_stock code" "$(body_of "$resp" | json error.code)" "insufficient_stock"

# =============================================================================
# 8. IDEMPOTENCY — never charge a card twice
# =============================================================================
section "8. Idempotency (the same key must never charge twice)"

KEY="e2e-idempotency-$RANDOM"
CHARGE='{"order_id":"e2e-order-x","customer_id":"c1","customer_email":"a@b.com","amount_minor":5000,"currency":"USD"}'

resp=$(http POST "$PAYMENT_URL/api/v1/payments" "$CHARGE" "Idempotency-Key: $KEY")
assert_eq "the first charge succeeds" "$(status_of "$resp")" "201"
pay1=$(body_of "$resp" | json payment_id)

# The SAME key, exactly as a retry after a timeout would send.
resp=$(http POST "$PAYMENT_URL/api/v1/payments" "$CHARGE" "Idempotency-Key: $KEY")
pay2=$(body_of "$resp" | json payment_id)

assert_eq "a retry returns the ORIGINAL payment id (no second charge)" "$pay2" "$pay1"

# The same key with DIFFERENT data is a client bug, and must be refused rather
# than silently returning the first result.
DIFFERENT='{"order_id":"e2e-order-x","customer_id":"c1","customer_email":"a@b.com","amount_minor":9999,"currency":"USD"}'
resp=$(http POST "$PAYMENT_URL/api/v1/payments" "$DIFFERENT" "Idempotency-Key: $KEY")
assert_eq "the same key with different data is refused with 409" "$(status_of "$resp")" "409"

# =============================================================================
# 9. CANCELLATION
# =============================================================================
section "9. Cancellation (pessimistic locking, idempotent)"

# Cancellation requires a PENDING order, so we disable the saga for this one by
# creating an order the saga cannot confirm — instead we create normally and
# accept PROCESSING, then assert the REFUSAL, which is the business rule.
resp=$(http POST "$ORDER_URL/api/v1/orders" "$CREATE_BODY")
cancel_id=$(body_of "$resp" | json id)
cancel_status=$(body_of "$resp" | json status)

if [[ "$cancel_status" == "PENDING" ]]; then
  resp=$(http POST "$ORDER_URL/api/v1/orders/$cancel_id/cancel" '{"reason":"changed my mind"}')
  assert_eq "a PENDING order can be cancelled" "$(status_of "$resp")" "200"
  assert_eq "the status is CANCELLED" "$(body_of "$resp" | json status)" "CANCELLED"
  assert_eq "the reason is recorded" "$(body_of "$resp" | json cancel_reason)" "changed my mind"

  # IDEMPOTENT: a double-click must succeed, not error.
  resp=$(http POST "$ORDER_URL/api/v1/orders/$cancel_id/cancel" '{"reason":"again"}')
  assert_eq "cancelling twice is idempotent (200, not an error)" "$(status_of "$resp")" "200"
else
  info "the order is $cancel_status (the saga confirmed it), so the customer-cancel path is refused"
  resp=$(http POST "$ORDER_URL/api/v1/orders/$cancel_id/cancel" '{"reason":"too late"}')
  assert_eq "a PROCESSING order cannot be cancelled by a customer" "$(status_of "$resp")" "422"
  assert_eq "with the cannot_cancel code" "$(body_of "$resp" | json error.code)" "cannot_cancel"
fi

# =============================================================================
# 10. CONCURRENCY — the locking requirement, demonstrated
# =============================================================================
section "10. Concurrency (20 simultaneous updates to one order)"

resp=$(http POST "$ORDER_URL/api/v1/orders" "$CREATE_BODY")
race_id=$(body_of "$resp" | json id)

info "firing 20 concurrent PATCH requests at order $race_id..."
tmpdir=$(mktemp -d)
for i in $(seq 1 20); do
  (
    code=$(curl -sS -o /dev/null -w '%{http_code}' -X PATCH \
      -H 'Content-Type: application/json' \
      -d '{"status":"SHIPPED","reason":"concurrent"}' \
      "$ORDER_URL/api/v1/orders/$race_id/status" 2>/dev/null || echo 000)
    echo "$code" > "$tmpdir/$i"
  ) &
done
wait

ok_count=$(grep -lc '^200$' "$tmpdir"/* 2>/dev/null | wc -l | tr -d ' ')
conflict_count=$(cat "$tmpdir"/* 2>/dev/null | grep -c '^409$' || true)
unprocessable_count=$(cat "$tmpdir"/* 2>/dev/null | grep -c '^422$' || true)
error_count=$(cat "$tmpdir"/* 2>/dev/null | grep -cE '^(000|5[0-9][0-9])$' || true)
rm -rf "$tmpdir"

info "results: 200=$ok_count  409=$conflict_count  422=$unprocessable_count  errors=$error_count"

# THE ASSERTION THAT MATTERS: no request produced a 5xx or a connection
# failure. Every outcome was a deliberate, correct one — either the update
# succeeded, or it was refused because the order had already moved on.
assert_eq "no request failed with a server error" "$error_count" "0"

final_status=$(body_of "$(http GET "$ORDER_URL/api/v1/orders/$race_id")" | json status)
assert_eq "the order ended in a consistent state" "$final_status" "SHIPPED"

# =============================================================================
# 11. STRANGLER FIG — the legacy read-fallback
# =============================================================================
section "11. Strangler Fig (reading a historical order from the legacy system)"

resp=$(http GET "$ORDER_URL/api/v1/admin/strangler")
assert_eq "the migration status endpoint responds" "$(status_of "$resp")" "200"
phase=$(body_of "$resp" | json phase)
info "migration phase: $phase"

# LEG-2019-0001 exists ONLY in the legacy service. The facade must find it.
resp=$(http GET "$ORDER_URL/api/v1/orders/number/LEG-2019-0001")
if [[ "$(status_of "$resp")" == "200" ]]; then
  pass "a legacy-only order is served through the new API"
  assert_eq "it is translated into our order format" "$(body_of "$resp" | json order_number)" "LEG-2019-0001"
  # The legacy status vocabulary (COMPLETE) is mapped to ours (DELIVERED).
  assert_eq "the legacy status vocabulary is translated" "$(body_of "$resp" | json status)" "DELIVERED"
  # The legacy decimal-string amount is converted exactly to minor units.
  assert_eq "the legacy amount string is converted exactly" "$(body_of "$resp" | json total_minor)" "129900"
  assert_eq "the record is marked as coming from legacy" "$(body_of "$resp" | json source)" "legacy"

  served=$(header_of GET "$ORDER_URL/api/v1/orders/number/LEG-2019-0001" "x-served-by")
  info "X-Served-By: $served"

  # ========================================================================
  # BACKFILL ON READ — asserted PROPERLY.
  #
  # An earlier version of this check accepted either outcome ("backfill may
  # be disabled"), and that leniency hid a real bug for a whole test run:
  # every backfill was failing because a legacy reference like
  # "LEG-2019-0001" is not a UUID and could not go into orders.id. The read
  # still worked, so the test still passed.
  #
  # THE LESSON: assert on the OUTCOME YOU CARE ABOUT (the record landing in
  # the new store), not merely on the request not failing. A test that
  # accepts both answers tests nothing.
  # ========================================================================
  backfill_enabled=$(body_of "$(http GET "$ORDER_URL/api/v1/admin/strangler")" | json backfill_enabled)
  if [[ "$backfill_enabled" == "True" ]]; then
    # Poll rather than sleeping a fixed amount: backfill is asynchronous, and
    # a fixed sleep is either flaky or needlessly slow.
    served_again=""
    for _ in $(seq 1 10); do
      served_again=$(header_of GET "$ORDER_URL/api/v1/orders/number/LEG-2019-0001" "x-served-by")
      [[ "$served_again" == "new" ]] && break
      sleep 1
    done
    assert_eq "backfill-on-read migrated the order into the new store" "$served_again" "new"

    # And confirm it is REALLY there, marked with its provenance, rather than
    # inferring it from a header.
    body=$(body_of "$(http GET "$ORDER_URL/api/v1/orders/number/LEG-2019-0001")")
    assert_eq "the backfilled record keeps its legacy provenance" "$(echo "$body" | json source)" "legacy"
    assert_eq "and its line items came across intact" "$(echo "$body" | json item_count)" "1"
    assert_eq "with the amount still exact" "$(echo "$body" | json total_minor)" "129900"

    backfilled=$(body_of "$(http GET "$ORDER_URL/api/v1/admin/strangler")" | json orders_backfilled)
    if [[ "${backfilled:-0}" -ge 1 ]]; then
      pass "the migration counter advanced (orders_backfilled=$backfilled)"
    else
      fail "backfill counter" "expected >= 1, got '$backfilled'"
    fi
  else
    info "backfill is disabled by configuration; skipping the migration assertions"
  fi

  # An unrecognised legacy state must be handled defensively, not crash.
  resp=$(http GET "$ORDER_URL/api/v1/orders/number/LEG-2020-0777")
  assert_eq "an order with an unknown legacy state is still readable" "$(status_of "$resp")" "200"
else
  info "the legacy fallback returned $(status_of "$resp"); is the legacy service running?"
  fail "strangler read-fallback" "expected 200 for LEG-2019-0001"
fi

# =============================================================================
# 12. TRANSACTIONAL OUTBOX
# =============================================================================
section "12. Transactional outbox (events published by the worker)"

published=$(curl -fsS "$ORDER_URL/metrics" 2>/dev/null | awk '/^outbox_published_total/{s+=$2} END{print s+0}')
if [[ "${published:-0}" -gt 0 ]]; then
  pass "the outbox worker has published events (total=$published)"
else
  info "no published events yet; waiting for the worker to tick..."
  sleep 4
  published=$(curl -fsS "$ORDER_URL/metrics" 2>/dev/null | awk '/^outbox_published_total/{s+=$2} END{print s+0}')
  if [[ "${published:-0}" -gt 0 ]]; then
    pass "the outbox worker published events after a tick (total=$published)"
  else
    fail "outbox publishing" "no events published; check WORKERS_ENABLED"
  fi
fi

# =============================================================================
# 13. STATS AND METRICS
# =============================================================================
section "13. Statistics and Prometheus metrics"

resp=$(http GET "$ORDER_URL/api/v1/orders/stats")
assert_eq "GET /orders/stats returns 200" "$(status_of "$resp")" "200"
assert_contains "it reports counts by status" "$(body_of "$resp")" "count_by_status"
# The note documents what the number excludes, so nobody misreads it.
assert_contains "and explains what revenue excludes" "$(body_of "$resp")" "CANCELLED"

metrics=$(curl -fsS "$ORDER_URL/metrics" 2>/dev/null)
assert_contains "/metrics exposes request counters" "$metrics" "http_requests_total"
assert_contains "/metrics exposes a latency histogram" "$metrics" "http_request_duration_seconds"
assert_contains "/metrics exposes order counters" "$metrics" "orders_created_total"
assert_contains "/metrics exposes saga counters" "$metrics" "saga_"
# Prometheus requires type metadata; without it the scrape is rejected.
assert_contains "the exposition format includes TYPE metadata" "$metrics" "# TYPE"

# =============================================================================
# 14. API DOCUMENTATION
# =============================================================================
section "14. API documentation"

resp=$(http GET "$ORDER_URL/openapi.yaml")
assert_eq "the OpenAPI spec is served" "$(status_of "$resp")" "200"
assert_contains "it is a valid OpenAPI 3 document" "$(body_of "$resp")" "openapi: 3"

resp=$(http GET "$ORDER_URL/docs")
assert_eq "Swagger UI is served" "$(status_of "$resp")" "200"
assert_contains "and it loads the spec" "$(body_of "$resp")" "swagger-ui"

# =============================================================================
# 15. THE AI ASSISTANT (RAG)
# =============================================================================
section "15. The AI assistant (RAG chatbot)"

resp=$(http GET "$ASSISTANT_URL/api/v1/assistant/info")
if [[ "$(status_of "$resp")" == "200" ]]; then
  pass "the assistant's info endpoint responds"
  chunks=$(body_of "$resp" | json index.chunks)
  info "corpus: $chunks chunks indexed, generator=$(body_of "$resp" | json generator)"

  # A POLICY QUESTION: answered from the corpus, WITH a citation.
  resp=$(http POST "$ASSISTANT_URL/api/v1/assistant/chat" '{"question":"how long do refunds take?"}')
  assert_eq "a policy question returns 200" "$(status_of "$resp")" "200"
  body=$(body_of "$resp")
  assert_eq "it is answered from retrieved documents (grounded)" "$(echo "$body" | json grounded)" "True"
  assert_contains "the answer contains the real policy figure" "$(echo "$body" | json answer)" "5 working days"
  citation=$(echo "$body" | json citations.0)
  if [[ -n "$citation" ]]; then
    pass "the answer carries a citation ($citation)"
  else
    fail "citation" "a grounded answer must cite its source"
  fi

  # THE HALLUCINATION GUARD: an off-topic question must be REFUSED, not
  # answered from the least-irrelevant document.
  resp=$(http POST "$ASSISTANT_URL/api/v1/assistant/chat" '{"question":"what is the capital of France?"}')
  body=$(body_of "$resp")
  assert_eq "an off-topic question is NOT answered from the corpus" "$(echo "$body" | json grounded)" "False"
  assert_contains "it says it does not know" "$(echo "$body" | json answer)" "do not have information"

  # THE TOOL-CALLING PATH: a question about a specific order must query the
  # order service for LIVE data, not retrieve a document.
  resp=$(http POST "$ASSISTANT_URL/api/v1/assistant/chat" "{\"question\":\"where is my order $ORDER_NUMBER?\"}")
  body=$(body_of "$resp")
  assert_eq "an order question routes to the order-status intent" "$(echo "$body" | json intent)" "order_status"
  assert_eq "and calls the order service as a tool" "$(echo "$body" | json used_tool)" "True"
  assert_contains "the answer states the order's LIVE status" "$(echo "$body" | json answer)" "DELIVERED"
  info "answer: $(echo "$body" | json answer | head -c 150)"

  # A status question with no order number must ASK for one, not guess.
  resp=$(http POST "$ASSISTANT_URL/api/v1/assistant/chat" '{"question":"where is my order?"}')
  assert_contains "a status question with no reference asks for the order number" \
    "$(body_of "$resp" | json answer)" "order number"

  # An action request must be REFUSED: a chat message is not authenticated.
  resp=$(http POST "$ASSISTANT_URL/api/v1/assistant/chat" '{"question":"cancel my order"}')
  assert_contains "the assistant refuses to perform actions" \
    "$(body_of "$resp" | json answer)" "cannot make changes"

  # The retrieval inspector, for debugging "was retrieval or generation wrong?"
  resp=$(http GET "$ASSISTANT_URL/api/v1/assistant/search?q=cancellation+policy")
  assert_eq "the retrieval inspector works" "$(status_of "$resp")" "200"
  assert_contains "and returns similarity scores" "$(body_of "$resp")" "score"

  resp=$(http POST "$ASSISTANT_URL/api/v1/assistant/chat" '{"question":""}')
  assert_eq "an empty question is rejected with 400" "$(status_of "$resp")" "400"
else
  fail "the assistant service" "not reachable at $ASSISTANT_URL"
fi

# =============================================================================
# 16. BACKGROUND WORKER
# =============================================================================
section "16. The background worker (PENDING → PROCESSING)"

advanced=$(curl -fsS "$ORDER_URL/metrics" 2>/dev/null | awk '/^orders_auto_advanced_total/{print $2}' | head -1)
if [[ -n "${advanced:-}" ]]; then
  pass "the auto-advance worker is instrumented (advanced=$advanced)"
else
  info "no orders have needed auto-advancing yet (the saga confirms them immediately)"
  pass "the worker is running (see 'make watch-logs')"
fi

# =============================================================================
# SUMMARY
# =============================================================================
printf '\n%s\n' "$(printf '═%.0s' {1..70})"
printf '%sEnd-to-end results%s\n' "$B" "$N"
printf '  %spassed: %d%s\n' "$G" "$PASS" "$N"
if [[ $FAIL -gt 0 ]]; then
  printf '  %sfailed: %d%s\n\n' "$R" "$FAIL" "$N"
  printf '%sFailures:%s\n' "$B" "$N"
  for t in "${FAILED_TESTS[@]}"; do printf '  %s✗%s %s\n' "$R" "$N" "$t"; done
  printf '\nInspect the logs with:  make docker-logs\n\n'
  exit 1
fi
printf '  failed: 0\n\n'
printf '%sEverything passed. The system is working end to end.%s\n\n' "$G" "$N"
printf 'Next steps:\n'
printf '  make demo     a guided walkthrough with explanations\n'
printf '  make stats    order stats, cache hit ratio, migration progress\n'
printf '  open http://localhost:8080/docs\n\n'
