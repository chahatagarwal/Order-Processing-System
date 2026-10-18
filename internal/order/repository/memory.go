// Package repository holds the ADAPTERS that implement domain.OrderRepository.
//
// THE REPOSITORY PATTERN: it puts a collection-like interface in front of
// storage. The service layer says "give me the order with this id"; it does
// not say "run this SQL". Three things follow:
//
//  1. Business logic is testable without a database (this file).
//  2. SQL lives in one place, so a slow query has one place to be fixed.
//  3. Storage can be swapped without touching business logic.
//
// WE SHIP TWO ADAPTERS:
//   - MemoryRepository (this file) — maps + a mutex. Used by unit tests and by
//     POSTGRES_ENABLED=false, so you can run the whole system with no Docker.
//   - PostgresRepository (postgres.go) — the production adapter.
//
// They are interchangeable because they satisfy the same interface. That is
// the LISKOV SUBSTITUTION PRINCIPLE, and it is only true because we made it
// true: the in-memory version faithfully reproduces the *semantics* that
// matter — version conflicts, lock serialisation, atomic outbox writes. A
// fake that is easier than reality gives you green tests and a broken system.
package repository

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/chahat/order-processing-system/internal/order/domain"
	"github.com/chahat/order-processing-system/internal/platform/errs"
)

// MemoryRepository is a concurrency-safe in-process implementation.
type MemoryRepository struct {
	// A single RWMutex guards all maps. The granularity is deliberate: the
	// production store is Postgres, so this needs to be CORRECT and OBVIOUS,
	// not fast. Per-key striping here would add risk for no benefit.
	mu        sync.RWMutex
	orders    map[string]*domain.Order // id -> order
	byNum     map[string]string        // order number -> id (a secondary index)
	outbox    []domain.OutboxEvent
	published map[string]bool

	// rowLocks emulates `SELECT ... FOR UPDATE`: one mutex per order id.
	//
	// THIS IS THE IMPORTANT PART OF THE FAKE. If pessimistic locking were a
	// no-op here, our concurrency tests would pass in memory and the real
	// system would still lose updates. Instead, two goroutines contending for
	// the same order id serialise exactly as they would in Postgres, so the
	// test genuinely exercises the behaviour.
	rowLocks sync.Map // id -> *sync.Mutex

	// failNext lets a test inject a storage failure (to prove the saga
	// compensates, and that we do not leave partial state behind). Fault
	// injection is not a hack; it is the only way to test the paths that
	// matter most and that never happen on a developer's laptop.
	failNext error
}

func NewMemoryRepository() *MemoryRepository {
	return &MemoryRepository{
		orders:    map[string]*domain.Order{},
		byNum:     map[string]string{},
		published: map[string]bool{},
	}
}

// FailNext makes the next write return err. Test-only.
func (r *MemoryRepository) FailNext(err error) {
	r.mu.Lock()
	r.failNext = err
	r.mu.Unlock()
}

func (r *MemoryRepository) takeFailure() error {
	if r.failNext != nil {
		err := r.failNext
		r.failNext = nil
		return err
	}
	return nil
}

// clone performs a DEEP COPY of an order.
//
// ============================================================================
// WHY CLONING IS MANDATORY, AND WHY FORGETTING IT IS A CLASSIC BUG
// ============================================================================
// If GetByID returned the stored *domain.Order, the caller would hold a
// pointer INTO our storage. Then:
//
//	o, _ := repo.GetByID(ctx, id)
//	o.Status = domain.StatusCancelled   // never saved...
//	                                    // ...but the "database" changed anyway
//
// Worse, a failed save would leave the mutation in place — so a rolled-back
// transaction would appear to have committed. The in-memory fake would be
// MORE PERMISSIVE than Postgres, and tests would pass for code that cannot
// work in production. Cloning makes the fake behave like a real database,
// where you get a row copy and must write it back explicitly.
//
// The Items slice needs its own copy too: copying the struct copies the slice
// HEADER, so both copies would still share one backing array.
// ============================================================================
func clone(o *domain.Order) *domain.Order {
	if o == nil {
		return nil
	}
	cp := *o
	cp.Items = make([]domain.OrderItem, len(o.Items))
	copy(cp.Items, o.Items)
	return &cp
}

func (r *MemoryRepository) rowLock(id string) *sync.Mutex {
	// LoadOrStore is atomic, so two goroutines racing to create the lock for
	// the same id both receive the SAME mutex. A plain "check then create"
	// would hand them two different mutexes and provide no mutual exclusion
	// whatsoever — the bug would only appear under concurrency.
	actual, _ := r.rowLocks.LoadOrStore(id, &sync.Mutex{})
	return actual.(*sync.Mutex)
}

// ---------------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------------

func (r *MemoryRepository) GetByID(ctx context.Context, id string) (*domain.Order, error) {
	// Honour cancellation even in the fake: it keeps behaviour identical to
	// the real adapter, so a context bug cannot hide until production.
	if err := ctx.Err(); err != nil {
		return nil, errs.Timeout("request_canceled", "the request was canceled").Wrap(err)
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	o, ok := r.orders[id]
	if !ok {
		return nil, domain.ErrOrderNotFound
	}
	return clone(o), nil
}

func (r *MemoryRepository) GetByOrderNumber(ctx context.Context, orderNumber string) (*domain.Order, error) {
	if err := ctx.Err(); err != nil {
		return nil, errs.Timeout("request_canceled", "the request was canceled").Wrap(err)
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	id, ok := r.byNum[strings.ToUpper(orderNumber)]
	if !ok {
		return nil, domain.ErrOrderNotFound
	}
	return clone(r.orders[id]), nil
}

func (r *MemoryRepository) List(ctx context.Context, f domain.ListFilter) (domain.ListResult, error) {
	if err := ctx.Err(); err != nil {
		return domain.ListResult{}, errs.Timeout("request_canceled", "the request was canceled").Wrap(err)
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	matched := make([]domain.Order, 0, len(r.orders))
	for _, o := range r.orders {
		if !matchesFilter(o, f) {
			continue
		}
		matched = append(matched, *clone(o))
	}

	sortOrders(matched, f.SortBy, f.SortDir)

	total := len(matched)
	start := f.Offset
	if start > total {
		start = total
	}
	end := start + f.Limit
	if f.Limit <= 0 || end > total {
		end = total
	}
	page := matched[start:end]

	return domain.ListResult{
		Orders:  page,
		Total:   total,
		Limit:   f.Limit,
		Offset:  f.Offset,
		HasMore: end < total,
	}, nil
}

func matchesFilter(o *domain.Order, f domain.ListFilter) bool {
	if f.Status != nil && o.Status != *f.Status {
		return false
	}
	if f.CustomerID != "" && o.CustomerID != f.CustomerID {
		return false
	}
	if f.CreatedAfter != nil && o.CreatedAt.Before(*f.CreatedAfter) {
		return false
	}
	if f.CreatedBefore != nil && o.CreatedAt.After(*f.CreatedBefore) {
		return false
	}
	return true
}

// sortOrders applies the requested ordering.
//
// NOTE THE ALLOW-LIST on sortBy: we switch on known field names rather than
// interpolating the client's string. In the SQL adapter that same discipline
// is what prevents SQL injection via an ORDER BY clause — a real and
// frequently-missed injection point, because people only parameterise values.
func sortOrders(orders []domain.Order, sortBy, sortDir string) {
	desc := !strings.EqualFold(sortDir, "asc")

	less := func(i, j int) bool { return orders[i].CreatedAt.Before(orders[j].CreatedAt) }
	switch sortBy {
	case "updated_at":
		less = func(i, j int) bool { return orders[i].UpdatedAt.Before(orders[j].UpdatedAt) }
	case "total_amount":
		less = func(i, j int) bool { return orders[i].TotalAmount.Minor < orders[j].TotalAmount.Minor }
	}

	// sort.SliceStable, not sort.Slice: with a stable sort, two orders created
	// in the same millisecond keep a consistent relative order between
	// requests. An unstable sort can return them in a different order each
	// time, which makes PAGINATION DROP OR DUPLICATE ROWS — a genuinely
	// confusing bug to chase.
	sort.SliceStable(orders, func(i, j int) bool {
		if desc {
			return less(j, i)
		}
		return less(i, j)
	})
}

func (r *MemoryRepository) Stats(ctx context.Context) (domain.Stats, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	stats := domain.Stats{
		CountByStatus: map[domain.Status]int{},
		GeneratedAt:   time.Now().UTC(),
	}
	// Seed every status with 0 so a client always receives the same keys.
	// Omitting empty buckets forces every consumer to write defensive
	// "if key exists" code, and charts end up with missing series.
	for _, s := range domain.AllStatuses() {
		stats.CountByStatus[s] = 0
	}

	revenue := domain.Zero("USD")
	counted := 0
	for _, o := range r.orders {
		stats.CountByStatus[o.Status]++
		stats.TotalOrders++
		// Revenue excludes CANCELLED and FAILED: counting money we never
		// collected (or refunded) would overstate revenue, and that is the
		// kind of metric error that gets reported to a board.
		if o.Status != domain.StatusCancelled && o.Status != domain.StatusFailed {
			if sum, err := revenue.Add(o.TotalAmount); err == nil {
				revenue = sum
				counted++
			}
		}
	}
	stats.TotalRevenue = revenue
	if counted > 0 {
		stats.AverageOrder = domain.NewMoney(revenue.Minor/int64(counted), revenue.Currency)
	} else {
		stats.AverageOrder = domain.Zero(revenue.Currency)
	}
	return stats, nil
}

// ---------------------------------------------------------------------------
// Writes
// ---------------------------------------------------------------------------

func (r *MemoryRepository) Create(ctx context.Context, o *domain.Order, event domain.OutboxEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := r.takeFailure(); err != nil {
		return err
	}
	if _, exists := r.orders[o.ID]; exists {
		return domain.ErrDuplicateOrderNumber
	}
	if _, exists := r.byNum[strings.ToUpper(o.OrderNumber)]; exists {
		return domain.ErrDuplicateOrderNumber
	}

	// ATOMICITY: the order, its index entry and the outbox event all land
	// under one mutex, mirroring the single Postgres transaction. If this
	// fake wrote the order and then the event separately, it would not
	// reproduce the guarantee the outbox pattern exists to provide.
	r.orders[o.ID] = clone(o)
	r.byNum[strings.ToUpper(o.OrderNumber)] = o.ID
	if !event.IsEmpty() {
		r.outbox = append(r.outbox, event)
	}
	return nil
}

// UpdateOptimistic implements compare-and-swap on Version.
func (r *MemoryRepository) UpdateOptimistic(ctx context.Context, o *domain.Order, event domain.OutboxEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := r.takeFailure(); err != nil {
		return err
	}
	stored, ok := r.orders[o.ID]
	if !ok {
		return domain.ErrOrderNotFound
	}

	// THE COMPARE-AND-SWAP. In SQL this is the `AND version = $N` in the
	// WHERE clause; here it is an explicit check. Same semantics: if someone
	// committed since we read, we refuse and report a conflict rather than
	// silently overwriting their work.
	if stored.Version != o.Version {
		return domain.ErrVersionConflict.
			WithDetail("expected_version", o.Version).
			WithDetail("actual_version", stored.Version)
	}

	updated := clone(o)
	updated.Version = o.Version + 1 // the swap
	r.orders[o.ID] = updated
	if !event.IsEmpty() {
		r.outbox = append(r.outbox, event)
	}

	// Write the new version back into the caller's struct, exactly as the SQL
	// adapter does with RETURNING version. Without this, a caller performing
	// two updates in a row would send a stale version on the second and get a
	// spurious conflict.
	o.Version = updated.Version
	return nil
}

// UpdateWithPessimisticLock emulates SELECT ... FOR UPDATE.
func (r *MemoryRepository) UpdateWithPessimisticLock(
	ctx context.Context,
	id string,
	mutate func(*domain.Order) (domain.OutboxEvent, error),
) (*domain.Order, error) {

	// Take the per-row lock FIRST and hold it across read-modify-write. This
	// is the defining property of pessimistic locking: nobody else can even
	// READ-for-update this row until we commit, so a conflict is impossible
	// rather than merely detected.
	lock := r.rowLock(id)

	// Respect the context while waiting, instead of blocking forever. The
	// real adapter does the same with lock_timeout; an unbounded lock wait is
	// how one slow transaction stalls an entire endpoint.
	acquired := make(chan struct{})
	go func() {
		lock.Lock()
		close(acquired)
	}()
	select {
	case <-acquired:
	case <-ctx.Done():
		// The goroutine will still acquire the lock eventually; we unlock it
		// as soon as that happens so it is not leaked permanently.
		go func() { <-acquired; lock.Unlock() }()
		return nil, errs.Timeout("lock_timeout", "timed out waiting for the order lock").Wrap(ctx.Err())
	}
	// defer guarantees the unlock on EVERY path — normal return, error, or a
	// panic inside the caller's mutate function. This is exactly why the port
	// exposes a callback rather than separate Lock/Unlock methods.
	defer lock.Unlock()

	r.mu.Lock()
	stored, ok := r.orders[id]
	if !ok {
		r.mu.Unlock()
		return nil, domain.ErrOrderNotFound
	}
	working := clone(stored)
	r.mu.Unlock()

	// Run the caller's mutation OUTSIDE the global map lock, so one slow
	// callback cannot block unrelated orders. The row lock still protects
	// THIS order. (Mirrors a real database: a row lock, not a table lock.)
	event, err := mutate(working)
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.takeFailure(); err != nil {
		return nil, err
	}
	// Re-check existence: the row could have been removed while we were in
	// the callback. Defensive, and cheap.
	if _, ok := r.orders[id]; !ok {
		return nil, domain.ErrOrderNotFound
	}
	// We still bump the version under a pessimistic lock. It costs nothing
	// and keeps readers that cached a version able to detect the change.
	working.Version++
	r.orders[id] = clone(working)
	if !event.IsEmpty() {
		r.outbox = append(r.outbox, event)
	}
	return clone(working), nil
}

// AdvanceStalePending is the batch job's set-based update.
func (r *MemoryRepository) AdvanceStalePending(ctx context.Context, olderThan time.Time, limit int) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Collect candidate ids first, then mutate.
	//
	// WHY TWO PASSES: mutating a map while ranging over it gives Go undefined
	// iteration behaviour for the entries you touch. Even where it is legal,
	// it makes the code impossible to reason about. Collect, then act.
	ids := make([]string, 0, limit)
	for id, o := range r.orders {
		if o.Status != domain.StatusPending {
			continue
		}
		if o.StatusChangedAt.After(olderThan) {
			continue // not stale enough yet
		}
		ids = append(ids, id)
	}
	// Sort for DETERMINISM. Go randomises map iteration order deliberately,
	// so without this the job would pick a different arbitrary subset each
	// run when limited — and the test would be flaky.
	sort.Strings(ids)
	if limit > 0 && len(ids) > limit {
		ids = ids[:limit]
	}

	now := time.Now().UTC()
	count := 0
	for _, id := range ids {
		o := clone(r.orders[id])
		if err := o.TransitionTo(domain.StatusProcessing, now); err != nil {
			continue // the state machine refused; skip rather than force it
		}
		o.Version++
		r.orders[id] = o
		r.outbox = append(r.outbox, domain.OutboxEvent{
			ID:            o.ID + ":auto-advance:" + now.Format(time.RFC3339Nano),
			AggregateType: "order",
			AggregateID:   o.ID,
			EventType:     domain.EventOrderStatusChanged,
			CreatedAt:     now,
		})
		count++
	}
	return count, nil
}

func (r *MemoryRepository) Ping(context.Context) error { return nil }

// ---------------------------------------------------------------------------
// Outbox (same struct implements OutboxRepository)
// ---------------------------------------------------------------------------

func (r *MemoryRepository) FetchUnpublished(ctx context.Context, limit int) ([]domain.OutboxEvent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]domain.OutboxEvent, 0, limit)
	for _, e := range r.outbox {
		if r.published[e.ID] {
			continue
		}
		out = append(out, e)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (r *MemoryRepository) MarkPublished(ctx context.Context, ids []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, id := range ids {
		r.published[id] = true
	}
	return nil
}

func (r *MemoryRepository) MarkFailed(ctx context.Context, id, reason string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.outbox {
		if r.outbox[i].ID == id {
			r.outbox[i].Attempts++
			r.outbox[i].LastError = reason
		}
	}
	return nil
}

func (r *MemoryRepository) PurgePublished(ctx context.Context, olderThan time.Time) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	kept := r.outbox[:0]
	purged := 0
	for _, e := range r.outbox {
		if r.published[e.ID] && e.CreatedAt.Before(olderThan) {
			purged++
			continue
		}
		kept = append(kept, e)
	}
	r.outbox = kept
	return purged, nil
}

// OutboxSize and SeedOrder are test helpers.
func (r *MemoryRepository) OutboxSize() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.outbox)
}

// SeedOrder inserts an order directly, bypassing validation. Tests use it to
// construct a specific scenario (e.g. "an order that has been SHIPPED for
// three days") without performing the whole lifecycle first.
func (r *MemoryRepository) SeedOrder(o *domain.Order) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.orders[o.ID] = clone(o)
	r.byNum[strings.ToUpper(o.OrderNumber)] = o.ID
}

// Compile-time interface checks.
var (
	_ domain.OrderRepository  = (*MemoryRepository)(nil)
	_ domain.OutboxRepository = (*MemoryRepository)(nil)
)
