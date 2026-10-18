package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/chahat/order-processing-system/internal/order/domain"
	"github.com/chahat/order-processing-system/internal/platform/cache"
	"github.com/chahat/order-processing-system/internal/platform/constants"
	"github.com/chahat/order-processing-system/internal/platform/errs"
)

// CachedRepository adds caching to ANY domain.OrderRepository.
//
// ============================================================================
// THE DECORATOR PATTERN (structural, GoF)
// ============================================================================
// It implements the SAME interface it wraps, so the service layer cannot tell
// the difference:
//
//	service -> CachedRepository -> PostgresRepository -> database
//	           (same interface)    (same interface)
//
// WHY THIS IS BETTER THAN PUTTING `if cached { ... }` IN THE SERVICE:
//
//  1. OPEN/CLOSED PRINCIPLE. We added caching to the repository WITHOUT
//     editing a single line of PostgresRepository or of the service. Open to
//     extension, closed to modification — demonstrated, not asserted.
//
//  2. SINGLE RESPONSIBILITY. PostgresRepository knows only SQL. This file
//     knows only caching. The service knows only business rules. Each has one
//     reason to change, so a caching bug is fixed in exactly one file.
//
//  3. IT COMPOSES. We could wrap this again with a MetricsRepository or a
//     RetryingRepository, in any order, with no changes anywhere:
//     Cached(Metrics(Postgres(db))). Try that with if-statements.
//
//  4. IT IS REMOVABLE. Caching misbehaving in production? Stop wrapping it in
//     main() and the system runs identically, just slower. That is a genuinely
//     valuable property at 3am.
//
// This same shape is Java's BufferedReader wrapping a FileReader, and Go's
// gzip.Writer wrapping an io.Writer. Once you see it, it is everywhere.
// ============================================================================
type CachedRepository struct {
	// next is the wrapped repository. Named "next" (not "postgres") on
	// purpose: this decorator must not know or care what it wraps.
	next domain.OrderRepository

	cache cache.Cache
	log   *slog.Logger

	ttl         time.Duration
	negativeTTL time.Duration

	// flight collapses concurrent misses for the same key into one query.
	// See platform/cache/singleflight.go for the stampede it prevents.
	flight *cache.Group
}

func NewCachedRepository(next domain.OrderRepository, c cache.Cache, log *slog.Logger, ttl, negativeTTL time.Duration) *CachedRepository {
	return &CachedRepository{
		next:        next,
		cache:       c,
		log:         log,
		ttl:         ttl,
		negativeTTL: negativeTTL,
		flight:      cache.NewGroup(),
	}
}

// ---------------------------------------------------------------------------
// Cache key construction
// ---------------------------------------------------------------------------

func keyOrderByID(id string) string {
	return constants.CacheNamespaceOrderByID + ":" + id
}

func keyOrderByNumber(num string) string {
	return constants.CacheNamespaceOrderByNum + ":" + strings.ToUpper(num)
}

// keyOrderList builds a list key that embeds the GENERATION NUMBER.
//
// ============================================================================
// GENERATION-BASED GROUP INVALIDATION — the best idea in this file
// ============================================================================
// THE PROBLEM: we cache list pages under keys like
//
//	order:v1:list:g7:status=PENDING|limit=20|offset=0
//	order:v1:list:g7:status=PENDING|limit=20|offset=20
//	order:v1:list:g7:customer=c-42|limit=20|offset=0     ... thousands of them
//
// When ANY order changes, every one of those pages may now be wrong. How do
// you invalidate thousands of keys you cannot enumerate?
//
// THREE BAD ANSWERS:
//  1. `KEYS order:v1:list:*` — KEYS is O(n) over the ENTIRE keyspace and
//     BLOCKS single-threaded Redis while it runs. On a production instance
//     with millions of keys this is a self-inflicted outage. Never use KEYS.
//  2. `SCAN` in a loop — non-blocking, but it is many round-trips, it races
//     with concurrent writes, and it may miss keys added mid-scan.
//  3. `FLUSHDB` — throws away every other service's cache too.
//
// THE GOOD ANSWER: never delete them at all. Put a GENERATION counter in the
// key. On any write, `INCR order:v1:list:generation`. Every subsequent read
// builds keys with g8, so all the g7 keys become UNREACHABLE instantly — one
// atomic O(1) operation, no matter how many keys exist. The orphaned g7
// entries are then reclaimed by TTL expiry and LRU eviction, which is exactly
// the job eviction already exists to do.
//
// This is sometimes called a "cache namespace" or "key versioning", and it is
// the standard solution to invalidating a group you cannot enumerate.
// ============================================================================
func keyOrderList(generation int64, f domain.ListFilter) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s:g%d:", constants.CacheNamespaceOrderList, generation)

	// The key must be DETERMINISTIC: identical filters must always produce
	// the identical key, or you cache the same page under many keys and your
	// hit ratio quietly collapses. We therefore write fields in a fixed order.
	if f.Status != nil {
		fmt.Fprintf(&sb, "status=%s|", *f.Status)
	}
	if f.CustomerID != "" {
		fmt.Fprintf(&sb, "customer=%s|", f.CustomerID)
	}
	if f.CreatedAfter != nil {
		fmt.Fprintf(&sb, "after=%d|", f.CreatedAfter.Unix())
	}
	if f.CreatedBefore != nil {
		fmt.Fprintf(&sb, "before=%d|", f.CreatedBefore.Unix())
	}
	fmt.Fprintf(&sb, "limit=%d|offset=%d|sort=%s:%s", f.Limit, f.Offset, f.SortBy, f.SortDir)
	return sb.String()
}

func keyStats(generation int64) string {
	return fmt.Sprintf("%s:g%d", constants.CacheNamespaceStats, generation)
}

// generation reads the current list generation.
//
// It FAILS OPEN (returns 0 on error): if Redis is unreachable we still build
// a valid key. Worst case we serve a slightly different key space for a
// moment — never an error to the customer.
func (r *CachedRepository) generation(ctx context.Context) int64 {
	raw, err := r.cache.Get(ctx, constants.CacheKeyListGeneration)
	if err != nil {
		return 0
	}
	var gen int64
	if err := json.Unmarshal(raw, &gen); err != nil {
		return 0
	}
	return gen
}

// bumpGeneration invalidates every cached list and stats page at once.
func (r *CachedRepository) bumpGeneration(ctx context.Context) {
	gen, err := r.cache.Incr(ctx, constants.CacheKeyListGeneration)
	if err != nil {
		// We log at WARN rather than failing the write. The write itself
		// already SUCCEEDED and is durable; failing the request now would be
		// wrong. TTL bounds the staleness, which is precisely why every key
		// has one.
		r.log.WarnContext(ctx, "failed to bump cache generation; list caches may be stale until TTL expiry",
			slog.String("error", err.Error()))
		return
	}
	// Mirror the counter into a readable key so generation() can read it back
	// with a plain GET.
	if b, err := json.Marshal(gen); err == nil {
		_ = r.cache.Set(ctx, constants.CacheKeyListGeneration, b, 24*time.Hour)
	}
}

// ---------------------------------------------------------------------------
// cachedOrder — the wire format
// ---------------------------------------------------------------------------

// cachedOrder is the cache's OWN representation, deliberately separate from
// domain.Order.
//
// WHY NOT JUST json.Marshal THE DOMAIN STRUCT:
// the cached bytes would then be coupled to the in-memory layout. Rename a
// domain field and every existing cache entry decodes into a zero value —
// silently, with no error. A dedicated DTO plus the `v1` in the key namespace
// means a shape change is an explicit, safe, versioned decision.
//
// The `NotFound` flag implements NEGATIVE CACHING: see the note in GetByID.
type cachedOrder struct {
	NotFound bool             `json:"not_found,omitempty"`
	Order    *serialisedOrder `json:"order,omitempty"`
}

type serialisedOrder struct {
	ID              string           `json:"id"`
	OrderNumber     string           `json:"order_number"`
	CustomerID      string           `json:"customer_id"`
	CustomerEmail   string           `json:"customer_email"`
	CustomerName    string           `json:"customer_name"`
	Status          domain.Status    `json:"status"`
	TotalMinor      int64            `json:"total_minor"`
	Currency        string           `json:"currency"`
	ShippingAddress string           `json:"shipping_address"`
	Notes           string           `json:"notes"`
	Version         int              `json:"version"`
	CancelReason    string           `json:"cancel_reason,omitempty"`
	FailureReason   string           `json:"failure_reason,omitempty"`
	SagaID          string           `json:"saga_id,omitempty"`
	CreatedAt       time.Time        `json:"created_at"`
	UpdatedAt       time.Time        `json:"updated_at"`
	StatusChangedAt time.Time        `json:"status_changed_at"`
	Source          string           `json:"source"`
	Items           []serialisedItem `json:"items"`
}

type serialisedItem struct {
	ID          string `json:"id"`
	ProductID   string `json:"product_id"`
	ProductName string `json:"product_name"`
	ProductSKU  string `json:"product_sku"`
	Quantity    int    `json:"quantity"`
	UnitMinor   int64  `json:"unit_minor"`
	LineMinor   int64  `json:"line_minor"`
	Currency    string `json:"currency"`
}

func toSerialised(o *domain.Order) *serialisedOrder {
	s := &serialisedOrder{
		ID: o.ID, OrderNumber: o.OrderNumber, CustomerID: o.CustomerID,
		CustomerEmail: o.CustomerEmail, CustomerName: o.CustomerName,
		Status: o.Status, TotalMinor: o.TotalAmount.Minor, Currency: o.TotalAmount.Currency,
		ShippingAddress: o.ShippingAddress, Notes: o.Notes, Version: o.Version,
		CancelReason: o.CancelReason, FailureReason: o.FailureReason, SagaID: o.SagaID,
		CreatedAt: o.CreatedAt, UpdatedAt: o.UpdatedAt, StatusChangedAt: o.StatusChangedAt,
		Source: o.Source,
	}
	for _, it := range o.Items {
		s.Items = append(s.Items, serialisedItem{
			ID: it.ID, ProductID: it.ProductID, ProductName: it.ProductName,
			ProductSKU: it.ProductSKU, Quantity: it.Quantity,
			UnitMinor: it.UnitPrice.Minor, LineMinor: it.LineTotal.Minor,
			Currency: it.UnitPrice.Currency,
		})
	}
	return s
}

func fromSerialised(s *serialisedOrder) *domain.Order {
	o := &domain.Order{
		ID: s.ID, OrderNumber: s.OrderNumber, CustomerID: s.CustomerID,
		CustomerEmail: s.CustomerEmail, CustomerName: s.CustomerName,
		Status: s.Status, TotalAmount: domain.NewMoney(s.TotalMinor, s.Currency),
		ShippingAddress: s.ShippingAddress, Notes: s.Notes, Version: s.Version,
		CancelReason: s.CancelReason, FailureReason: s.FailureReason, SagaID: s.SagaID,
		CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt, StatusChangedAt: s.StatusChangedAt,
		Source: s.Source,
	}
	for _, it := range s.Items {
		o.Items = append(o.Items, domain.OrderItem{
			ID: it.ID, OrderID: s.ID, ProductID: it.ProductID,
			ProductName: it.ProductName, ProductSKU: it.ProductSKU, Quantity: it.Quantity,
			UnitPrice: domain.NewMoney(it.UnitMinor, it.Currency),
			LineTotal: domain.NewMoney(it.LineMinor, it.Currency),
		})
	}
	return o
}

// ---------------------------------------------------------------------------
// Reads — CACHE-ASIDE
// ---------------------------------------------------------------------------

// GetByID is the canonical cache-aside read.
func (r *CachedRepository) GetByID(ctx context.Context, id string) (*domain.Order, error) {
	key := keyOrderByID(id)

	// STEP 1: try the cache.
	if raw, err := r.cache.Get(ctx, key); err == nil {
		var entry cachedOrder
		if err := json.Unmarshal(raw, &entry); err == nil {
			if entry.NotFound {
				// A NEGATIVE CACHE HIT.
				//
				// WHY WE CACHE ABSENCE: without it, a scanner requesting
				// 10,000 random ids per second produces 10,000 database
				// queries per second, every one of which returns nothing.
				// The cache is bypassed entirely by data that does not exist
				// — this is called CACHE PENETRATION.
				//
				// The TTL is short (30s vs 5m) because an order that does not
				// exist now may be created a moment later, and we do not want
				// to serve a 404 for a real order. Short negative TTL, long
				// positive TTL is the standard asymmetry.
				return nil, domain.ErrOrderNotFound
			}
			if entry.Order != nil {
				r.log.DebugContext(ctx, "cache hit", slog.String("key", key))
				return fromSerialised(entry.Order), nil
			}
		}
		// A decode failure means a corrupt or old-shaped entry. Treat it as a
		// miss and move on — never return an error to the customer because of
		// a cache problem.
		r.log.WarnContext(ctx, "discarding undecodable cache entry", slog.String("key", key))
	}

	// STEP 2: miss. Load from the source of truth, but DEDUPLICATED, so 1,000
	// concurrent misses for this id produce exactly one database query.
	raw, shared, err := r.flight.Do(ctx, key, func(ctx context.Context) ([]byte, error) {
		order, err := r.next.GetByID(ctx, id)
		if err != nil {
			if errs.Is(err, errs.KindNotFound) {
				// STEP 3a: populate the NEGATIVE entry and signal not-found
				// to the waiters via the payload rather than an error, so
				// they all get the same cheap answer.
				entry, _ := json.Marshal(cachedOrder{NotFound: true})
				_ = r.cache.Set(ctx, key, entry, cache.JitterTTL(r.negativeTTL, 0.2))
				return entry, nil
			}
			return nil, err
		}
		// STEP 3b: populate the positive entry.
		entry, mErr := json.Marshal(cachedOrder{Order: toSerialised(order)})
		if mErr != nil {
			return nil, errs.Internal("cache_encode_failed", "failed to encode the order for caching").Wrap(mErr)
		}
		// The TTL comes from the DOMAIN: a terminal order is immutable and is
		// cached for an hour; a PENDING order gets the short TTL. Jitter stops
		// a batch of keys expiring in the same millisecond.
		_ = r.cache.Set(ctx, key, entry, cache.JitterTTL(order.CacheTTL(r.ttl), 0.1))
		// Also index by order number, so a lookup by either identifier hits.
		_ = r.cache.Set(ctx, keyOrderByNumber(order.OrderNumber), entry,
			cache.JitterTTL(order.CacheTTL(r.ttl), 0.1))
		return entry, nil
	})
	if err != nil {
		return nil, err
	}
	if shared {
		r.log.DebugContext(ctx, "coalesced onto an in-flight load (stampede prevented)",
			slog.String("key", key))
	}

	var entry cachedOrder
	if err := json.Unmarshal(raw, &entry); err != nil {
		return nil, errs.Internal("cache_decode_failed", "failed to decode the loaded order").Wrap(err)
	}
	if entry.NotFound || entry.Order == nil {
		return nil, domain.ErrOrderNotFound
	}
	return fromSerialised(entry.Order), nil
}

func (r *CachedRepository) GetByOrderNumber(ctx context.Context, orderNumber string) (*domain.Order, error) {
	key := keyOrderByNumber(orderNumber)
	if raw, err := r.cache.Get(ctx, key); err == nil {
		var entry cachedOrder
		if err := json.Unmarshal(raw, &entry); err == nil {
			if entry.NotFound {
				return nil, domain.ErrOrderNotFound
			}
			if entry.Order != nil {
				return fromSerialised(entry.Order), nil
			}
		}
	}

	order, err := r.next.GetByOrderNumber(ctx, orderNumber)
	if err != nil {
		if errs.Is(err, errs.KindNotFound) {
			entry, _ := json.Marshal(cachedOrder{NotFound: true})
			_ = r.cache.Set(ctx, key, entry, cache.JitterTTL(r.negativeTTL, 0.2))
		}
		return nil, err
	}
	if entry, mErr := json.Marshal(cachedOrder{Order: toSerialised(order)}); mErr == nil {
		ttl := cache.JitterTTL(order.CacheTTL(r.ttl), 0.1)
		_ = r.cache.Set(ctx, key, entry, ttl)
		_ = r.cache.Set(ctx, keyOrderByID(order.ID), entry, ttl)
	}
	return order, nil
}

// List caches each page under a generation-scoped key.
func (r *CachedRepository) List(ctx context.Context, f domain.ListFilter) (domain.ListResult, error) {
	gen := r.generation(ctx)
	key := keyOrderList(gen, f)

	if raw, err := r.cache.Get(ctx, key); err == nil {
		var cached cachedList
		if err := json.Unmarshal(raw, &cached); err == nil {
			result := domain.ListResult{
				Total: cached.Total, Limit: cached.Limit,
				Offset: cached.Offset, HasMore: cached.HasMore,
			}
			for i := range cached.Orders {
				result.Orders = append(result.Orders, *fromSerialised(&cached.Orders[i]))
			}
			return result, nil
		}
	}

	result, err := r.next.List(ctx, f)
	if err != nil {
		return domain.ListResult{}, err
	}

	payload := cachedList{Total: result.Total, Limit: result.Limit, Offset: result.Offset, HasMore: result.HasMore}
	for i := range result.Orders {
		payload.Orders = append(payload.Orders, *toSerialised(&result.Orders[i]))
	}
	if raw, mErr := json.Marshal(payload); mErr == nil {
		// A SHORTER TTL FOR LISTS than for single orders, deliberately.
		//
		// WHY: one order's cache entry is invalidated precisely when THAT
		// order changes. A list page can be invalidated by ANY of thousands
		// of orders changing, so its probability of being stale is far higher.
		// Higher staleness risk => shorter TTL. Matching TTL to volatility,
		// rather than using one global number, is most of what "tuning a
		// cache" actually means.
		_ = r.cache.Set(ctx, key, raw, cache.JitterTTL(r.ttl/5, 0.2))
	}
	return result, nil
}

type cachedList struct {
	Orders  []serialisedOrder `json:"orders"`
	Total   int               `json:"total"`
	Limit   int               `json:"limit"`
	Offset  int               `json:"offset"`
	HasMore bool              `json:"has_more"`
}

func (r *CachedRepository) Stats(ctx context.Context) (domain.Stats, error) {
	gen := r.generation(ctx)
	key := keyStats(gen)

	if raw, err := r.cache.Get(ctx, key); err == nil {
		var cached cachedStats
		if err := json.Unmarshal(raw, &cached); err == nil {
			return cached.toDomain(), nil
		}
	}

	stats, err := r.next.Stats(ctx)
	if err != nil {
		return domain.Stats{}, err
	}
	if raw, mErr := json.Marshal(fromDomainStats(stats)); mErr == nil {
		// Stats are the most expensive query (a full aggregate) and the most
		// tolerant of staleness — nobody needs a dashboard to be accurate to
		// the second. So this gets the LONGEST TTL of any key. Cost of
		// computation and tolerance for staleness are the two inputs to every
		// TTL decision.
		_ = r.cache.Set(ctx, key, raw, cache.JitterTTL(r.ttl, 0.2))
	}
	return stats, nil
}

type cachedStats struct {
	CountByStatus map[string]int `json:"count_by_status"`
	TotalOrders   int            `json:"total_orders"`
	RevenueMinor  int64          `json:"revenue_minor"`
	AverageMinor  int64          `json:"average_minor"`
	Currency      string         `json:"currency"`
	GeneratedAt   time.Time      `json:"generated_at"`
}

func fromDomainStats(s domain.Stats) cachedStats {
	c := cachedStats{
		CountByStatus: map[string]int{},
		TotalOrders:   s.TotalOrders,
		RevenueMinor:  s.TotalRevenue.Minor,
		AverageMinor:  s.AverageOrder.Minor,
		Currency:      s.TotalRevenue.Currency,
		GeneratedAt:   s.GeneratedAt,
	}
	for k, v := range s.CountByStatus {
		c.CountByStatus[string(k)] = v
	}
	return c
}

func (c cachedStats) toDomain() domain.Stats {
	s := domain.Stats{
		CountByStatus: map[domain.Status]int{},
		TotalOrders:   c.TotalOrders,
		TotalRevenue:  domain.NewMoney(c.RevenueMinor, c.Currency),
		AverageOrder:  domain.NewMoney(c.AverageMinor, c.Currency),
		GeneratedAt:   c.GeneratedAt,
	}
	for k, v := range c.CountByStatus {
		s.CountByStatus[domain.Status(k)] = v
	}
	return s
}

// ---------------------------------------------------------------------------
// Writes — INVALIDATION
// ---------------------------------------------------------------------------
//
// EVERY write method does the same two things, in this order:
//
//	1. Perform the write against the wrapped repository.
//	2. ONLY IF IT SUCCEEDED, invalidate the affected keys.
//
// THE ORDER IS NOT NEGOTIABLE. If we invalidated first and the write then
// failed, we would have thrown away a valid cache entry for nothing — a
// pointless miss. Far worse, if we invalidated first in a system where
// another request repopulates between our DELETE and our write, the cache
// would be repopulated from the OLD row and then never invalidated again:
// permanently stale data. Write-then-invalidate avoids both.
//
// THE RESIDUAL RACE (and why it is acceptable):
// Even write-then-invalidate has a tiny window — a concurrent reader could
// load the old row, then our invalidation runs, then that reader writes its
// stale value into the cache. The windows is microseconds, the TTL bounds the
// damage, and the alternatives (distributed locks on every read, or
// versioned CAS writes into the cache) cost more than the problem. Knowing
// that this window exists, and having chosen to accept it, is the point.

func (r *CachedRepository) Create(ctx context.Context, o *domain.Order, event domain.OutboxEvent) error {
	if err := r.next.Create(ctx, o, event); err != nil {
		return err
	}
	// A new order invalidates every list page and the stats — but no
	// single-order key, because this order was not cached before. One INCR.
	r.bumpGeneration(ctx)
	// We also clear any NEGATIVE entry: someone may have asked for this id
	// (or more realistically this order number) before it existed, and we do
	// not want to serve a cached 404 for an order that now exists.
	_ = r.cache.Delete(ctx, keyOrderByID(o.ID), keyOrderByNumber(o.OrderNumber))
	return nil
}

func (r *CachedRepository) UpdateOptimistic(ctx context.Context, o *domain.Order, event domain.OutboxEvent) error {
	if err := r.next.UpdateOptimistic(ctx, o, event); err != nil {
		// NOTE: we do NOT invalidate on failure. A version conflict means
		// nothing changed, so the cached entry is still correct. Invalidating
		// here would turn every conflict into an extra database read.
		return err
	}
	r.invalidateOrder(ctx, o)
	return nil
}

func (r *CachedRepository) UpdateWithPessimisticLock(ctx context.Context, id string, mutate func(*domain.Order) (domain.OutboxEvent, error)) (*domain.Order, error) {
	updated, err := r.next.UpdateWithPessimisticLock(ctx, id, mutate)
	if err != nil {
		return nil, err
	}
	r.invalidateOrder(ctx, updated)
	return updated, nil
}

// invalidateOrder removes BOTH keys for one order and bumps the generation.
//
// WHY BOTH KEYS: the same order is cached under its id AND its order number.
// Deleting only one leaves the other serving the old status — and because
// customers look orders up by the number on their receipt, the one you forget
// is usually the one they use.
func (r *CachedRepository) invalidateOrder(ctx context.Context, o *domain.Order) {
	if o == nil {
		return
	}
	keys := []string{keyOrderByID(o.ID), keyOrderByNumber(o.OrderNumber)}
	if err := r.cache.Delete(ctx, keys...); err != nil {
		// A failed DELETE means the cache may now hold data we KNOW is wrong,
		// so this is logged at WARN with the keys named. The TTL is what
		// guarantees the staleness ends; the log is what tells you it happened.
		r.log.WarnContext(ctx, "cache invalidation failed; entries will be stale until TTL expiry",
			slog.Any("keys", keys), slog.String("error", err.Error()))
	}
	r.bumpGeneration(ctx)
}

func (r *CachedRepository) AdvanceStalePending(ctx context.Context, olderThan time.Time, limit int) (int, error) {
	count, err := r.next.AdvanceStalePending(ctx, olderThan, limit)
	if err != nil {
		return count, err
	}
	if count > 0 {
		// A BULK change just altered N specific orders, but we do not know
		// which ids (the batch UPDATE did not return them to this layer).
		//
		// This is the honest trade-off of batch invalidation: bumping the
		// generation clears every LIST page in one operation, but the N
		// individual order keys can only be reached by their own keys. Rather
		// than fetch all N ids purely to delete N cache keys, we let their
		// TTL expire them. The staleness is bounded by the TTL (minutes) and
		// affects only a direct GET of an order that was just auto-advanced.
		//
		// If that were unacceptable, the fix is for AdvanceStalePending to
		// return the ids — a deliberate interface change with a real cost,
		// which is why it is documented here rather than done silently.
		r.bumpGeneration(ctx)
		r.log.InfoContext(ctx, "bulk status advance invalidated list caches",
			slog.Int("orders_advanced", count))
	}
	return count, nil
}

func (r *CachedRepository) Ping(ctx context.Context) error { return r.next.Ping(ctx) }

// CacheStats exposes hit/miss counters for /metrics and for the e2e test that
// PROVES the cache is working.
func (r *CachedRepository) CacheStats() cache.Stats { return r.cache.Stats() }

// The decorator satisfies the very interface it wraps — this line is the
// pattern, stated in code.
var _ domain.OrderRepository = (*CachedRepository)(nil)
