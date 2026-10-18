// Package constants holds every magic value in the system in ONE place.
//
// WHY THIS PACKAGE EXISTS (DRY principle):
// A "magic number" is a bare value like `5` or `"PENDING"` scattered through
// code. When the business says "make it 10 instead of 5", you have to hunt
// through every file. Here, you change one line.
//
// RULE OF THUMB WE FOLLOW:
//   - constants  = values that are part of the program's MEANING and never
//     change at runtime (status names, HTTP header names, cache key prefixes).
//   - config/env = values that change per environment (DB password, port,
//     TTL tuning). See internal/platform/config.
//
// Putting a DB password here would be a bug; putting a cache-key prefix in
// the environment would be pointless noise. That split is the whole idea.
package constants

import "time"

// ---------------------------------------------------------------------------
// Service identity
// ---------------------------------------------------------------------------

const (
	ServiceOrder     = "order-service"
	ServiceInventory = "inventory-service"
	ServicePayment   = "payment-service"
	ServiceLegacy    = "legacy-order-service"
	ServiceAssistant = "assistant-service"
)

// APIVersion is the URL prefix for v1 of the public API. Versioning the URL
// (rather than a header) means an old mobile app keeps working forever while
// we ship /api/v2 alongside it.
const APIVersion = "/api/v1"

// ---------------------------------------------------------------------------
// HTTP: header names and operational endpoints
// ---------------------------------------------------------------------------

const (
	// HeaderRequestID carries a unique id for one request. We log it on every
	// line, so "customer says order failed at 2pm" becomes one grep.
	HeaderRequestID = "X-Request-Id"

	// HeaderIdempotencyKey lets a client safely retry a POST. Same key twice
	// => we return the first result instead of charging the card twice.
	HeaderIdempotencyKey = "Idempotency-Key"

	// HeaderCacheStatus is a debugging aid: HIT, MISS, or BYPASS. You can see
	// the cache working with curl -i, without reading logs.
	HeaderCacheStatus = "X-Cache"

	// HeaderServedBy tells you WHICH backend answered. During a Strangler Fig
	// migration this is how you prove traffic moved off the legacy system.
	HeaderServedBy = "X-Served-By"
)

const (
	CacheStatusHit    = "HIT"
	CacheStatusMiss   = "MISS"
	CacheStatusBypass = "BYPASS"
)

// Operational endpoints. These are deliberately NOT under /api/v1: they are
// for Kubernetes and your monitoring system, not for customers.
const (
	// PathHealthz = LIVENESS. "Is this process alive?" It must not touch the
	// database. If it did, a brief DB blip would make Kubernetes kill and
	// restart every healthy pod at once — turning a small outage into a big one.
	PathHealthz = "/healthz"

	// PathReadyz = READINESS. "Can this process serve traffic right now?"
	// This one DOES check the DB, cache, and downstream services. A failing
	// readyz removes the pod from the load balancer but leaves it running,
	// so it can recover and rejoin.
	PathReadyz = "/readyz"

	PathMetrics = "/metrics" // Prometheus scrape target
	PathDocs    = "/docs"    // Swagger UI
	PathOpenAPI = "/openapi.yaml"
)

// ---------------------------------------------------------------------------
// Cache keys
// ---------------------------------------------------------------------------
//
// Every key embeds a SCHEMA VERSION (v1). If we ever change the shape of the
// cached JSON, we bump this to v2 and every old entry becomes unreachable
// instantly — no flush, no downtime, no risk of decoding old data with new
// code. This is "versioned key invalidation".
const (
	CacheKeyVersion = "v1"

	CacheNamespaceOrderByID  = "order:" + CacheKeyVersion + ":id"
	CacheNamespaceOrderByNum = "order:" + CacheKeyVersion + ":num"
	CacheNamespaceOrderList  = "order:" + CacheKeyVersion + ":list"
	CacheNamespaceStats      = "order:" + CacheKeyVersion + ":stats"

	// CacheKeyListGeneration is the counter behind "generation-based group
	// invalidation". Every list cache key includes the current generation
	// number. Any write bumps the counter, so ALL cached list pages become
	// unreachable in one atomic INCR — no need to SCAN Redis for keys to
	// delete (SCAN on a hot production Redis is how you cause an outage).
	CacheKeyListGeneration = "order:" + CacheKeyVersion + ":list:generation"
)

// ---------------------------------------------------------------------------
// Default timings
// ---------------------------------------------------------------------------
//
// These are DEFAULTS; config can override each one from the environment.
const (
	DefaultHTTPReadTimeout     = 10 * time.Second
	DefaultHTTPWriteTimeout    = 15 * time.Second
	DefaultHTTPIdleTimeout     = 60 * time.Second
	DefaultShutdownGracePeriod = 20 * time.Second

	// DefaultCacheTTL — short enough that stale data self-heals, long enough
	// to absorb a traffic spike. TTL is our BACKSTOP; explicit invalidation on
	// write is our primary mechanism.
	DefaultCacheTTL = 5 * time.Minute

	// DefaultNegativeCacheTTL — we cache "this order does not exist" too, but
	// only briefly. Without it, a bot hammering random IDs hits the database
	// on every single request (a "cache penetration" attack).
	DefaultNegativeCacheTTL = 30 * time.Second

	// DefaultLockLeaseTTL — how long a distributed lock is held before it
	// auto-expires. If a worker crashes mid-job, the lease expiring is what
	// lets another worker take over instead of the job being stuck forever.
	DefaultLockLeaseTTL = 30 * time.Second
)

// ---------------------------------------------------------------------------
// Pagination
// ---------------------------------------------------------------------------
const (
	DefaultPageSize = 20
	MaxPageSize     = 100 // A hard cap. Without it, ?size=1000000 is a free DoS.
)

// ---------------------------------------------------------------------------
// Retry policy for optimistic locking
// ---------------------------------------------------------------------------
const (
	// DefaultOptimisticRetries: on a version conflict we re-read and retry.
	// 3 is a deliberate middle ground — see docs/LOCKING.md for the trade-off
	// (too few = spurious 409s for users; too many = retry storms under load).
	DefaultOptimisticRetries = 3
	DefaultRetryBaseDelay    = 20 * time.Millisecond
)

// ---------------------------------------------------------------------------
// Business rules
// ---------------------------------------------------------------------------
const (
	// MaxItemsPerOrder bounds the work a single request can create.
	MaxItemsPerOrder = 50

	// MinOrderAmountMinor / MaxOrderAmountMinor are in MINOR UNITS (cents).
	// We never use float64 for money: 0.1 + 0.2 != 0.3 in binary floating
	// point, and that becomes a one-cent accounting discrepancy that an
	// auditor will find. int64 cents is exact.
	MinOrderAmountMinor = int64(1)
	MaxOrderAmountMinor = int64(100_000_000) // 1,000,000.00

	CurrencyDefault = "USD"

	// AutoAdvanceAfter: the background job promotes PENDING -> PROCESSING
	// for orders older than this.
	AutoAdvanceAfter = 5 * time.Minute

	// OrderNumberPrefix is the human-facing id on the receipt: ORD-7K2M9QX4.
	OrderNumberPrefix = "ORD-"
)
