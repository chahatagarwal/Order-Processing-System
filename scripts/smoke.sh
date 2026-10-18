#!/usr/bin/env bash
# smoke.sh — a 10-second check that the system is fundamentally working.
#
# WHY A SMOKE TEST SEPARATE FROM THE E2E SUITE: after a deploy you want one
# fast, unambiguous answer to "is it up?". The full e2e suite takes a minute
# and tests edge cases; this tests the critical path only. Run this first —
# if it fails, there is no point running anything else.
set -euo pipefail

URL="${ORDER_URL:-http://localhost:8080}"
if [[ -t 1 ]]; then G=$'\033[32m'; R=$'\033[31m'; N=$'\033[0m'; else G=""; R=""; N=""; fi

step() { printf '%-50s' "$1"; }
ok()   { printf '%sok%s\n' "$G" "$N"; }
bad()  { printf '%sFAILED%s\n  %s\n' "$R" "$N" "${1:-}"; exit 1; }

step "order service is alive"
curl -fsS "$URL/healthz" >/dev/null 2>&1 || bad "GET $URL/healthz failed. Run: make docker-up"
ok

step "order service is ready (deps connected)"
curl -fsS "$URL/readyz" >/dev/null 2>&1 || bad "$(curl -sS "$URL/readyz" 2>&1 | head -c 300)"
ok

step "creating an order"
body='{"customer_id":"smoke","customer_email":"smoke@example.com","shipping_address":"1 Smoke St","items":[{"product_id":"SKU-MOUSE","product_name":"Mouse","quantity":1,"unit_price_minor":2999}]}'
resp=$(curl -fsS -X POST -H 'Content-Type: application/json' -d "$body" "$URL/api/v1/orders" 2>&1) || bad "$resp"
id=$(python3 -c "import json,sys; print(json.load(sys.stdin).get('id',''))" <<<"$resp" 2>/dev/null)
[[ -n "$id" ]] || bad "no order id in the response: ${resp:0:200}"
ok

step "reading it back"
curl -fsS "$URL/api/v1/orders/$id" >/dev/null 2>&1 || bad "could not read order $id"
ok

step "listing orders"
curl -fsS "$URL/api/v1/orders?limit=1" >/dev/null 2>&1 || bad "list failed"
ok

step "metrics are exposed"
curl -fsS "$URL/metrics" 2>/dev/null | grep -q http_requests_total || bad "no metrics"
ok

step "API docs are served"
curl -fsS "$URL/openapi.yaml" >/dev/null 2>&1 || bad "no openapi spec"
ok

printf '\n%sSmoke test passed.%s Order %s was created and read back.\n' "$G" "$N" "$id"
printf 'Run the full suite with: make e2e\n\n'
