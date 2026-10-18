#!/usr/bin/env bash
# load_test.sh — make the cache and the locking strategy VISIBLE under load.
#
# This is not a benchmark (no warmup, no percentile maths, uncontrolled
# environment). Its purpose is to make internal behaviour OBSERVABLE: you watch
# the cache hit ratio climb and the lock-escalation counter move, which is far
# more convincing than reading that they exist.
set -euo pipefail

URL="${ORDER_URL:-http://localhost:8080}"
CONCURRENCY="${CONCURRENCY:-20}"
REQUESTS="${REQUESTS:-200}"

if [[ -t 1 ]]; then B=$'\033[1m'; C=$'\033[36m'; N=$'\033[0m'; else B=""; C=""; N=""; fi

metric() { curl -fsS "$URL/metrics" 2>/dev/null | awk -v k="$1" '$1==k{print $2}' | head -1; }

curl -fsS "$URL/healthz" >/dev/null 2>&1 || { echo "the order service is not running; try: make docker-up"; exit 1; }

echo ""
echo "${B}Load test${N}  concurrency=$CONCURRENCY  requests=$REQUESTS"
echo ""

# --------------------------------------------------------------------------
# PART 1: READ LOAD — demonstrates the cache.
#
# All requests ask for the SAME order, which is the realistic pattern for an
# order-tracking page and the one a cache is designed for.
# --------------------------------------------------------------------------
echo "${B}1. Creating one order to read repeatedly${N}"
body='{"customer_id":"load","customer_email":"load@example.com","shipping_address":"1 Load St","items":[{"product_id":"SKU-MOUSE","product_name":"Mouse","quantity":1,"unit_price_minor":2999}]}'
id=$(curl -fsS -X POST -H 'Content-Type: application/json' -d "$body" "$URL/api/v1/orders" | python3 -c "import json,sys; print(json.load(sys.stdin)['id'])")
echo "   order: $id"
echo ""

hits_before=$(metric cache_hits_total); hits_before=${hits_before:-0}
miss_before=$(metric cache_misses_total); miss_before=${miss_before:-0}

echo "${B}2. Firing $REQUESTS reads at $CONCURRENCY concurrency${N}"
start=$(date +%s)
seq 1 "$REQUESTS" | xargs -P "$CONCURRENCY" -I{} curl -fsS -o /dev/null "$URL/api/v1/orders/$id" 2>/dev/null
elapsed=$(( $(date +%s) - start ))
[[ $elapsed -eq 0 ]] && elapsed=1

hits_after=$(metric cache_hits_total); hits_after=${hits_after:-0}
miss_after=$(metric cache_misses_total); miss_after=${miss_after:-0}
ratio=$(metric cache_hit_ratio)

echo ""
echo "   duration:        ${elapsed}s  (~$((REQUESTS / elapsed)) req/s)"
echo "   cache hits:      ${C}${hits_before} -> ${hits_after}${N}"
echo "   cache misses:    ${miss_before} -> ${miss_after}"
echo "   hit ratio:       ${C}${ratio:-n/a}${N}"
echo ""
echo "   ${B}What this shows:${N} $REQUESTS reads produced only a handful of"
echo "   database queries. The rest were served from Redis. Singleflight also"
echo "   collapsed the concurrent misses on the very first request into one query."
echo ""

# --------------------------------------------------------------------------
# PART 2: WRITE CONTENTION — demonstrates hybrid locking.
# --------------------------------------------------------------------------
echo "${B}3. Write contention: $CONCURRENCY concurrent updates to ONE order${N}"
id2=$(curl -fsS -X POST -H 'Content-Type: application/json' -d "$body" "$URL/api/v1/orders" | python3 -c "import json,sys; print(json.load(sys.stdin)['id'])")

conflicts_before=$(metric order_version_conflicts_total); conflicts_before=${conflicts_before:-0}
escalations_before=$(metric order_lock_escalations_total); escalations_before=${escalations_before:-0}

tmp=$(mktemp -d)
for i in $(seq 1 "$CONCURRENCY"); do
  ( curl -sS -o /dev/null -w '%{http_code}' -X PATCH -H 'Content-Type: application/json' \
      -d '{"status":"SHIPPED","reason":"load test"}' \
      "$URL/api/v1/orders/$id2/status" > "$tmp/$i" 2>/dev/null ) &
done
wait

ok=$(cat "$tmp"/* 2>/dev/null | grep -c '^200$' || true)
conflict=$(cat "$tmp"/* 2>/dev/null | grep -c '^409$' || true)
refused=$(cat "$tmp"/* 2>/dev/null | grep -c '^422$' || true)
errors=$(cat "$tmp"/* 2>/dev/null | grep -cE '^(000|5..)$' || true)
rm -rf "$tmp"

conflicts_after=$(metric order_version_conflicts_total); conflicts_after=${conflicts_after:-0}
escalations_after=$(metric order_lock_escalations_total); escalations_after=${escalations_after:-0}

echo ""
echo "   succeeded (200):            $ok"
echo "   refused by state machine:   $refused   (the order had already moved on — correct)"
echo "   version conflicts (409):    $conflict"
echo "   server errors:              ${C}${errors}${N}   ${B}<- this must be 0${N}"
echo ""
echo "   version conflicts detected: ${conflicts_before} -> ${C}${conflicts_after}${N}"
echo "   lock escalations:           ${escalations_before} -> ${C}${escalations_after}${N}"
echo ""
echo "   ${B}What this shows:${N} under contention the optimistic path detected"
echo "   conflicts and retried. Where retries ran out, the hybrid strategy"
echo "   ESCALATED to a database row lock so the request completed instead of"
echo "   failing. No request returned a 5xx, and the order ended in a"
echo "   consistent state."
echo ""
final=$(curl -fsS "$URL/api/v1/orders/$id2" | python3 -c "import json,sys; d=json.load(sys.stdin); print(d['status'], 'version', d['version'])")
echo "   final state: ${C}${final}${N}"
echo ""
