package domain

import (
	"context"
	"time"
)

// ============================================================================
// PORTS — the interfaces the domain REQUIRES from the outside world.
// ============================================================================
// This is "Ports and Adapters" (a.k.a. Hexagonal Architecture). The domain
// declares what it NEEDS; the infrastructure provides adapters that satisfy
// it. Note the direction: the interface is defined HERE, by the consumer,
// not in the repository package by the implementer.
//
// WHY THAT DIRECTION IS THE WHOLE POINT (Dependency Inversion):
//
//	WRONG: service imports repository/postgres -> business logic now depends
//	       on pgx, and cannot be tested or reused without a database.
//	RIGHT: service imports domain (interfaces) <- postgres implements them.
//	       The business logic depends only on an abstraction it owns.
//
// This is also the INTERFACE SEGREGATION PRINCIPLE in action: instead of one
// fat `Repository` with 20 methods, we define several small, focused
// interfaces. A read-only consumer depends on OrderReader and cannot even
// express a write, which is both safer and far easier to fake in tests.
// ============================================================================

// OrderReader is the read side. The AI assistant and reporting endpoints need
// only this, so that is all they get.
type OrderReader interface {
	GetByID(ctx context.Context, id string) (*Order, error)
	GetByOrderNumber(ctx context.Context, orderNumber string) (*Order, error)
	List(ctx context.Context, filter ListFilter) (ListResult, error)
	Stats(ctx context.Context) (Stats, error)
}

// OrderWriter is the write side.
//
// ----------------------------------------------------------------------------
// THE THREE WRITE METHODS MAP EXACTLY TO THE THREE LOCKING STRATEGIES.
// This is deliberate: the choice of lock is visible AT THE CALL SITE, rather
// than hidden inside a repository that decides for you.
// ----------------------------------------------------------------------------
type OrderWriter interface {
	// Create inserts the order, its items, and an outbox event ATOMICALLY in
	// one transaction. No locking needed: nothing else can contend for a row
	// that does not exist yet.
	Create(ctx context.Context, order *Order, event OutboxEvent) error

	// UpdateOptimistic writes only if the stored version still matches
	// order.Version, and returns ErrVersionConflict otherwise.
	//
	// USE WHEN: contention is low (the common case). No locks are held while
	// the service thinks, so throughput is high. The loser retries.
	UpdateOptimistic(ctx context.Context, order *Order, event OutboxEvent) error

	// UpdateWithPessimisticLock runs `mutate` while holding a real row lock
	// (SELECT ... FOR UPDATE) for the whole read-modify-write.
	//
	// USE WHEN: contention is high, or the operation must not be retried.
	// The callback shape is essential — it guarantees the lock is released
	// exactly once, on every path including a panic. If we instead exposed
	// Lock() and Unlock(), one forgotten Unlock would block that order for
	// every user until the connection was recycled.
	UpdateWithPessimisticLock(ctx context.Context, id string, mutate func(*Order) (OutboxEvent, error)) (*Order, error)
}

// OrderRepository is the full read+write port, composed from the two halves.
//
// Go's interface embedding means we define the composite without repeating a
// single method signature — one more application of DRY.
type OrderRepository interface {
	OrderReader
	OrderWriter

	// AdvanceStalePending is the background job's query. It is a SET-BASED
	// operation on purpose.
	//
	// WHY NOT "load all pending orders, loop, save each":
	//   - 100,000 pending orders would be loaded into memory (OOM risk).
	//   - It would be 100,000 round-trips instead of one.
	//   - Each one could fail independently, leaving a partial, unrepeatable run.
	// One UPDATE ... WHERE does it in a single statement, inside one
	// transaction, in milliseconds. "Push the work to the data" is usually
	// the right instinct for batch jobs.
	AdvanceStalePending(ctx context.Context, olderThan time.Time, limit int) (int, error)

	// Ping backs the /readyz probe.
	Ping(ctx context.Context) error
}

// ListFilter describes a paginated, filtered query.
//
// WHY A STRUCT RATHER THAN 8 POSITIONAL PARAMETERS:
//
//	List(ctx, "PENDING", "cust-1", 0, 20, "created_at", "desc", nil)
//
// is unreadable and silently breaks if two string parameters are swapped.
// A struct is self-documenting at the call site, makes every field optional
// by default, and lets us add a field later without changing any caller.
// (This is the Parameter Object pattern.)
type ListFilter struct {
	Status     *Status // nil = any status. A POINTER so "" and "unset" differ.
	CustomerID string
	// CreatedAfter / CreatedBefore support date-range queries.
	CreatedAfter  *time.Time
	CreatedBefore *time.Time

	// Cursor-based pagination. See the note in ListResult for why we prefer
	// this over OFFSET.
	Limit  int
	Offset int

	SortBy  string // created_at | updated_at | total_amount
	SortDir string // asc | desc
}

// ListResult is one page of results plus the metadata a client needs.
type ListResult struct {
	Orders []Order
	// Total is the full count matching the filter.
	//
	// AN HONEST TRADE-OFF: computing Total needs a second COUNT(*) query,
	// which on a large table is the slowest part of the request. The
	// alternative is to drop Total and return only "has_more", which is what
	// large-scale APIs do. We keep Total because a page count is genuinely
	// useful in an admin UI, and document the cost rather than hiding it.
	//
	// ON OFFSET PAGINATION: OFFSET 100000 makes Postgres scan and discard
	// 100,000 rows — it gets linearly slower the deeper you page. Keyset
	// ("cursor") pagination, `WHERE created_at < $last`, stays constant-time.
	// We expose offset because it is what an admin table needs, and cap the
	// page size to bound the damage.
	Total   int
	Limit   int
	Offset  int
	HasMore bool
}

// Stats is the aggregate report for the dashboard endpoint.
type Stats struct {
	CountByStatus map[Status]int
	TotalOrders   int
	TotalRevenue  Money // excludes CANCELLED and FAILED orders
	AverageOrder  Money
	GeneratedAt   time.Time
}

// ---------------------------------------------------------------------------
// OutboxEvent — the TRANSACTIONAL OUTBOX pattern
// ---------------------------------------------------------------------------
//
// ============================================================================
// THE PROBLEM: THE DUAL-WRITE
// ============================================================================
// We want to save the order AND tell other services about it. The obvious code
// is:
//
//	db.Save(order)        // succeeds
//	queue.Publish(event)  // network error!
//
// Now the order exists but nobody knows. Inventory never reserves stock, no
// email is sent, and the order silently rots. Swap the order of the two lines
// and you get the opposite bug: an event for an order that was never saved.
//
// You cannot fix this by reordering, by retrying, or by being careful: a
// database and a message broker cannot be updated atomically without a
// distributed transaction (2PC), and 2PC brings its own blocking failure modes.
//
// ============================================================================
// THE SOLUTION
// ============================================================================
// Write the event INTO THE SAME DATABASE, IN THE SAME TRANSACTION, as a row
// in an `outbox` table. One transaction, so it is genuinely atomic: either
// both the order and the event exist, or neither does.
//
// Then a separate worker polls the outbox and publishes. If publishing fails,
// the row is still there and it tries again.
//
// THE GUARANTEE THIS GIVES: AT-LEAST-ONCE delivery. The worker could crash
// after publishing but before marking the row done, so an event may be
// published TWICE. That is unavoidable, and it is why every consumer must be
// IDEMPOTENT — keyed on EventID. "Exactly once" delivery does not exist; what
// exists is at-least-once delivery plus idempotent consumers.
//
// See internal/order/workers/outbox_worker.go for the publisher.
// ============================================================================
type OutboxEvent struct {
	ID            string
	AggregateType string // "order"
	AggregateID   string
	EventType     string // order.created, order.status_changed, ...
	Payload       []byte // JSON snapshot of what happened
	CreatedAt     time.Time

	// Attempts / LastError make a stuck event DIAGNOSABLE instead of
	// mysterious. An event with 50 attempts and an error message tells you
	// exactly what to fix.
	Attempts  int
	LastError string
}

// Event type constants. Using constants rather than string literals means a
// typo in a publisher is a compile error, not an event silently nobody
// subscribes to.
const (
	EventOrderCreated       = "order.created"
	EventOrderStatusChanged = "order.status_changed"
	EventOrderCancelled     = "order.cancelled"
	EventOrderFailed        = "order.failed"
)

// NoEvent is the explicit "this mutation publishes nothing" value.
//
// Why not just pass a zero OutboxEvent? Because a reader cannot tell whether
// `OutboxEvent{}` means "no event" or "I forgot to fill this in". A named
// value states the intent. (This is the Null Object pattern again, in miniature.)
var NoEvent = OutboxEvent{}

func (e OutboxEvent) IsEmpty() bool { return e.EventType == "" }

// OutboxRepository is the port used by the publisher worker.
type OutboxRepository interface {
	// FetchUnpublished claims a batch of events for THIS worker.
	//
	// It is implemented with `FOR UPDATE SKIP LOCKED`, which is the key to
	// running several workers safely:
	//   - FOR UPDATE locks the rows this worker claimed.
	//   - SKIP LOCKED makes other workers IGNORE those rows instead of
	//     queueing behind them.
	// The result is N workers processing N disjoint batches in parallel, with
	// no duplicate work and no coordination service. Without SKIP LOCKED, the
	// workers would serialise and you would get no parallelism at all.
	FetchUnpublished(ctx context.Context, limit int) ([]OutboxEvent, error)
	MarkPublished(ctx context.Context, ids []string) error
	MarkFailed(ctx context.Context, id string, reason string) error
	// PurgePublished stops the outbox table growing without bound. An
	// unbounded append-only table eventually dominates your disk and your
	// backups; retention is part of the design, not an afterthought.
	PurgePublished(ctx context.Context, olderThan time.Time) (int, error)
}

// EventPublisher is where outbox events eventually go.
//
// Today: a structured log line (and an in-process bus for the saga). In
// production: Kafka, SNS, RabbitMQ. Because this is an interface, that swap
// is one new file and one line in main() — no business logic changes.
// That is the Open/Closed Principle: open to extension, closed to modification.
type EventPublisher interface {
	Publish(ctx context.Context, event OutboxEvent) error
}

// ---------------------------------------------------------------------------
// Saga ports
// ---------------------------------------------------------------------------

// InventoryPort is what the order service needs from inventory.
//
// NOTE THE NAMING: it is defined by the CONSUMER (order) in the consumer's
// vocabulary, not dictated by the inventory service's API shape. If inventory
// renames its endpoint, we change one adapter, not our domain.
type InventoryPort interface {
	// Reserve holds stock for this order. idempotencyKey makes a retry safe.
	Reserve(ctx context.Context, req ReservationRequest) (ReservationResult, error)
	// Release is the COMPENSATING ACTION for Reserve — the saga's "undo".
	Release(ctx context.Context, reservationID, idempotencyKey string) error
}

type ReservationRequest struct {
	OrderID        string
	IdempotencyKey string
	Items          []ReservationItem
}

type ReservationItem struct {
	ProductID string
	Quantity  int
}

type ReservationResult struct {
	ReservationID string
	Reserved      bool
	Reason        string
}

// PaymentPort is what the order service needs from payments.
type PaymentPort interface {
	Charge(ctx context.Context, req ChargeRequest) (ChargeResult, error)
	// Refund is the COMPENSATING ACTION for Charge.
	//
	// IMPORTANT CONCEPTUAL POINT: this is NOT a rollback. A charge that has
	// been captured cannot be erased — it happened, and it is on the
	// customer's statement. A refund is a NEW, compensating transaction that
	// cancels out the effect. This is precisely why sagas exist: once you
	// have crossed a service boundary, the world has changed and "undo" is
	// always a forward action, never a rewind.
	Refund(ctx context.Context, paymentID, idempotencyKey, reason string) error
}

type ChargeRequest struct {
	OrderID        string
	CustomerID     string
	CustomerEmail  string
	AmountMinor    int64
	Currency       string
	IdempotencyKey string
}

type ChargeResult struct {
	PaymentID string
	Captured  bool
	Reason    string
}

// LegacyOrderPort is the Strangler Fig read path into the old system.
// See internal/order/transport/strangler.go.
type LegacyOrderPort interface {
	GetByID(ctx context.Context, id string) (*Order, error)
	GetByOrderNumber(ctx context.Context, orderNumber string) (*Order, error)
	Create(ctx context.Context, order *Order) error
}

// Clock abstracts time.
//
// WHY: a test for "auto-advance orders older than 5 minutes" must not sleep
// for 5 minutes. With an injectable clock the test advances time instantly and
// deterministically. Tests that depend on real time are slow AND flaky — they
// fail on a loaded CI machine for no reason, and people learn to ignore them.
type Clock interface {
	Now() time.Time
}

// SystemClock is the production implementation.
type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now().UTC() }

// FixedClock is the test implementation. It is in the domain package (not a
// _test file) so that every package's tests can use the same one.
type FixedClock struct {
	Current time.Time
}

func (c *FixedClock) Now() time.Time { return c.Current }

// Advance moves the fake clock forward.
func (c *FixedClock) Advance(d time.Duration) { c.Current = c.Current.Add(d) }
