// Package service is the SERVICE LAYER (also called the application layer).
//
// ============================================================================
// WHAT BELONGS HERE, AND WHAT DOES NOT
// ============================================================================
// The service layer orchestrates a USE CASE. For "cancel an order" it:
//   - decides which locking strategy to use
//   - loads the aggregate through the repository
//   - asks the DOMAIN to apply the rule (order.Cancel)
//   - builds the outbox event
//   - records metrics and logs
//
// WHAT IT MUST NOT CONTAIN:
//   - HTTP types. No http.Request, no status codes. (That is transport's job,
//     and keeping it out is what lets a CLI or a queue consumer reuse this.)
//   - SQL. (repository's job)
//   - The RULES THEMSELVES. `if status == SHIPPED` belongs in the domain.
//     The service asks the domain to decide; it does not decide itself.
//
// WHY HAVE THIS LAYER AT ALL, rather than putting it in the HTTP handler?
// Because then "create an order" would be reachable only over HTTP. Our
// background worker, our saga recovery job and our tests all drive these same
// use cases with no server running. A fat controller makes that impossible.
// ============================================================================
package service

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/chahat/order-processing-system/internal/order/domain"
	"github.com/chahat/order-processing-system/internal/platform/config"
	"github.com/chahat/order-processing-system/internal/platform/constants"
	"github.com/chahat/order-processing-system/internal/platform/errs"
	"github.com/chahat/order-processing-system/internal/platform/idgen"
	"github.com/chahat/order-processing-system/internal/platform/logging"
	"github.com/chahat/order-processing-system/internal/platform/metrics"
	"github.com/chahat/order-processing-system/internal/platform/retry"
)

// OrderService implements the use cases.
//
// EVERY DEPENDENCY IS AN INTERFACE, INJECTED VIA THE CONSTRUCTOR.
// That is not ceremony — it is what makes the test file next door able to
// construct this with a fake repository, a fake clock and a fake inventory
// service, and assert on real behaviour in microseconds with no Docker.
type OrderService struct {
	repo  domain.OrderRepository
	clock domain.Clock
	log   *slog.Logger
	cfg   config.LockingConfig
	mtx   *metrics.Registry

	// saga is optional (nil when SAGA_ENABLED=false). A nil check at the call
	// site keeps the dependency genuinely optional rather than requiring a
	// stub everywhere.
	saga *SagaOrchestrator
}

func NewOrderService(
	repo domain.OrderRepository,
	clock domain.Clock,
	log *slog.Logger,
	cfg config.LockingConfig,
	mtx *metrics.Registry,
	saga *SagaOrchestrator,
) *OrderService {
	return &OrderService{repo: repo, clock: clock, log: log, cfg: cfg, mtx: mtx, saga: saga}
}

// ---------------------------------------------------------------------------
// USE CASE 1: Create an order
// ---------------------------------------------------------------------------

// Create validates, persists, and (if enabled) runs the distributed saga.
func (s *OrderService) Create(ctx context.Context, in domain.NewOrderInput) (*domain.Order, error) {
	log := logging.FromContext(ctx)
	now := s.clock.Now()

	// STEP 1: build the aggregate. The FACTORY does all validation and
	// computes the total, so if this returns an order, it is valid. The
	// service does not re-check anything.
	order, err := domain.NewOrder(in, now)
	if err != nil {
		s.mtx.IncCounter("orders_rejected_total", "Orders rejected at validation", "reason", codeOf(err))
		return nil, err
	}

	// STEP 2: persist the order AND its outbox event in ONE transaction.
	event, err := s.buildEvent(order, domain.EventOrderCreated, map[string]any{
		"order_id":     order.ID,
		"order_number": order.OrderNumber,
		"customer_id":  order.CustomerID,
		"total_minor":  order.TotalAmount.Minor,
		"currency":     order.TotalAmount.Currency,
		"item_count":   order.ItemCount(),
	}, now)
	if err != nil {
		return nil, err
	}

	if err := s.repo.Create(ctx, order, event); err != nil {
		log.ErrorContext(ctx, "failed to persist new order",
			slog.String("order_id", order.ID), slog.String("error", err.Error()))
		return nil, err
	}

	log.InfoContext(ctx, "order created",
		slog.String("order_id", order.ID),
		slog.String("order_number", order.OrderNumber),
		slog.String("customer_id", order.CustomerID),
		slog.String("total", order.TotalAmount.String()),
		slog.Int("items", len(order.Items)))

	s.mtx.IncCounter("orders_created_total", "Orders created", "currency", order.TotalAmount.Currency)
	s.mtx.ObserveHistogramWithBuckets("order_value_minor", "Order value distribution in minor units",
		[]float64{500, 2000, 5000, 10000, 50000, 100000, 500000},
		float64(order.TotalAmount.Minor))

	// STEP 3: run the saga (reserve stock -> charge -> confirm).
	//
	// ============================================================================
	// AN IMPORTANT SEQUENCING DECISION: THE ORDER IS COMMITTED AS *PENDING*
	// BEFORE ANY REMOTE CALL IS MADE.
	//
	// WHY: if we called inventory and payment first and only then saved, a
	// crash between the charge and the save would leave a customer charged
	// for an order that does not exist — invisible, unrecoverable money.
	//
	// By committing PENDING first, the worst case is an order stuck in
	// PENDING with no reservation. That is VISIBLE (it is a row in the
	// database), and the saga recovery worker can finish or compensate it.
	// "Prefer a recoverable inconsistency over an invisible one" is the whole
	// design instinct behind sagas.
	// ============================================================================
	if s.saga != nil {
		if sagaErr := s.saga.RunCreateOrderSaga(ctx, order); sagaErr != nil {
			// The saga has ALREADY run its compensations and marked the order
			// FAILED. We return the error so the client learns the order did
			// not go through, but the order row survives for audit and support.
			log.WarnContext(ctx, "order saga failed; compensations have been applied",
				slog.String("order_id", order.ID), slog.String("error", sagaErr.Error()))
			return order, sagaErr
		}
		// Re-read so the caller sees the status the saga committed (PROCESSING).
		if fresh, err := s.repo.GetByID(ctx, order.ID); err == nil {
			order = fresh
		}
	}
	return order, nil
}

// ---------------------------------------------------------------------------
// USE CASE 2 & 3: reads
// ---------------------------------------------------------------------------

func (s *OrderService) GetByID(ctx context.Context, id string) (*domain.Order, error) {
	if id == "" {
		return nil, errs.Invalid("missing_id", "an order id is required")
	}
	return s.repo.GetByID(ctx, id)
}

func (s *OrderService) GetByOrderNumber(ctx context.Context, number string) (*domain.Order, error) {
	if number == "" {
		return nil, errs.Invalid("missing_order_number", "an order number is required")
	}
	return s.repo.GetByOrderNumber(ctx, number)
}

// List applies pagination defaults and caps.
func (s *OrderService) List(ctx context.Context, f domain.ListFilter) (domain.ListResult, error) {
	// NORMALISE AT THE EDGE OF THE SERVICE, so the repository can trust its
	// input and every storage adapter behaves identically. If each adapter
	// applied its own defaults, Postgres and the in-memory store would
	// silently disagree and tests would not catch it.
	if f.Limit <= 0 {
		f.Limit = constants.DefaultPageSize
	}
	if f.Limit > constants.MaxPageSize {
		// CAPPING, not erroring. `?size=100000` is usually a confused client,
		// not an attack, and quietly giving them 100 is friendlier than a 400.
		// The cap is what matters: it bounds the memory and query cost a single
		// request can impose.
		f.Limit = constants.MaxPageSize
	}
	if f.Offset < 0 {
		f.Offset = 0
	}
	if f.Status != nil && !f.Status.IsValid() {
		return domain.ListResult{}, errs.Invalid("invalid_status_filter",
			"the status filter is not a recognised order status").
			WithDetail("allowed_statuses", domain.AllStatuses())
	}
	return s.repo.List(ctx, f)
}

func (s *OrderService) Stats(ctx context.Context) (domain.Stats, error) {
	return s.repo.Stats(ctx)
}

// ---------------------------------------------------------------------------
// USE CASE 4: Update status — THE HYBRID LOCKING PATH
// ---------------------------------------------------------------------------

// UpdateStatus changes an order's status, choosing a locking strategy.
//
// ============================================================================
// THIS METHOD IS THE ANSWER TO "use locking based on the use case, or a
// hybrid approach".
//
//	LOCK_STRATEGY=optimistic  -> try the compare-and-swap, retry on conflict,
//	                             give up with a 409 after N attempts.
//	LOCK_STRATEGY=pessimistic -> take a row lock immediately; no retries.
//	LOCK_STRATEGY=hybrid      -> optimistic first; if we keep losing, that is
//	                             EVIDENCE of real contention, so ESCALATE to a
//	                             pessimistic lock for the final attempt.
//
// WHY HYBRID IS THE DEFAULT:
// 99% of status updates have no contention at all. Taking a row lock for all
// of them would pay the serialisation cost on every request to avoid a
// problem that almost never happens. Conversely, pure optimism behaves badly
// on the 1% of genuinely hot orders: the retries thrash and some users still
// get a 409 after exhausting the budget.
//
// Hybrid gets the best of both: the fast path stays lock-free, and the rare
// contended case is handled by QUEUEING instead of RETRYING. It turns
// "probably fine, occasionally fails" into "fast, and always completes".
// ============================================================================
func (s *OrderService) UpdateStatus(ctx context.Context, id string, target domain.Status, reason string) (*domain.Order, error) {
	log := logging.FromContext(ctx)

	if !target.IsValid() {
		return nil, errs.Invalid("invalid_status", "the requested status is not recognised").
			WithDetail("requested_status", string(target)).
			WithDetail("allowed_statuses", domain.AllStatuses())
	}

	start := time.Now()
	defer func() {
		s.mtx.ObserveHistogram("order_status_update_duration_seconds",
			"Status update latency by locking strategy",
			time.Since(start).Seconds(), "strategy", string(s.cfg.Strategy))
	}()

	switch s.cfg.Strategy {
	case config.LockPessimistic:
		return s.updateStatusPessimistic(ctx, id, target, reason)

	case config.LockOptimistic:
		order, err := s.updateStatusOptimistic(ctx, id, target, reason, s.cfg.MaxRetries)
		if err != nil && errs.Is(err, errs.KindConcurrency) {
			// We exhausted the retry budget. Returning 409 and asking the
			// client to retry is correct for pure optimism — the server
			// refuses to serialise.
			s.mtx.IncCounter("order_lock_exhausted_total", "Optimistic retry budgets exhausted", "strategy", "optimistic")
		}
		return order, err

	default: // hybrid
		// Spend MOST of the budget optimistically (fast, lock-free)...
		attempts := s.cfg.MaxRetries
		if attempts > 1 {
			attempts--
		}
		order, err := s.updateStatusOptimistic(ctx, id, target, reason, attempts)
		if err == nil {
			return order, nil
		}
		// ...and only ESCALATE for a genuine concurrency conflict. A 404 or a
		// rejected transition is not contention, and escalating would just
		// take a pointless lock before failing in the same way.
		if !errs.Is(err, errs.KindConcurrency) {
			return nil, err
		}
		log.InfoContext(ctx, "optimistic attempts exhausted; escalating to a pessimistic row lock",
			slog.String("order_id", id), slog.Int("optimistic_attempts", attempts))
		s.mtx.IncCounter("order_lock_escalations_total",
			"Times the hybrid strategy escalated from optimistic to pessimistic")
		return s.updateStatusPessimistic(ctx, id, target, reason)
	}
}

// updateStatusOptimistic: read, decide, compare-and-swap, retry on conflict.
func (s *OrderService) updateStatusOptimistic(ctx context.Context, id string, target domain.Status, reason string, maxAttempts int) (*domain.Order, error) {
	log := logging.FromContext(ctx)
	policy := retry.DefaultPolicy(maxAttempts, s.cfg.RetryBase)

	var result *domain.Order
	err := retry.Do(ctx, policy, errs.IsRetryable, func(ctx context.Context, attempt int) error {
		// RE-READ ON EVERY ATTEMPT. This is essential and easy to get wrong:
		// retrying with the SAME stale version would fail identically every
		// time, burning the whole budget to no purpose. A retry is only
		// meaningful if it re-reads the current state first.
		order, err := s.repo.GetByID(ctx, id)
		if err != nil {
			return err
		}

		// Ask the DOMAIN whether this move is legal. The service never
		// inspects statuses itself.
		if err := order.TransitionTo(target, s.clock.Now()); err != nil {
			return err // a business rejection: not retryable, bail out
		}
		if reason != "" {
			order.Notes = appendNote(order.Notes, reason)
		}

		event, err := s.buildEvent(order, domain.EventOrderStatusChanged, map[string]any{
			"order_id": order.ID, "to": order.Status, "reason": reason, "attempt": attempt,
		}, s.clock.Now())
		if err != nil {
			return err
		}

		if err := s.repo.UpdateOptimistic(ctx, order, event); err != nil {
			if errs.Is(err, errs.KindConcurrency) {
				log.InfoContext(ctx, "version conflict; will retry with a fresh read",
					slog.String("order_id", id), slog.Int("attempt", attempt), slog.Int("max", maxAttempts))
				s.mtx.IncCounter("order_version_conflicts_total", "Optimistic version conflicts")
			}
			return err
		}
		result = order
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.logStatusChange(ctx, result, "optimistic")
	return result, nil
}

// updateStatusPessimistic: lock the row, then decide. No retries needed —
// the lock makes a conflict impossible rather than merely detectable.
func (s *OrderService) updateStatusPessimistic(ctx context.Context, id string, target domain.Status, reason string) (*domain.Order, error) {
	// Bound the lock wait. Without this, a request could sit behind a stuck
	// transaction indefinitely and hold its own resources while doing so.
	ctx, cancel := context.WithTimeout(ctx, s.cfg.PessimisticTimeout)
	defer cancel()

	updated, err := s.repo.UpdateWithPessimisticLock(ctx, id, func(order *domain.Order) (domain.OutboxEvent, error) {
		// We are inside the lock. The state we see cannot change underneath
		// us, so this check-then-act is safe — which is the entire reason to
		// pay for a lock.
		if err := order.TransitionTo(target, s.clock.Now()); err != nil {
			return domain.NoEvent, err
		}
		if reason != "" {
			order.Notes = appendNote(order.Notes, reason)
		}
		return s.buildEventUnchecked(order, domain.EventOrderStatusChanged, map[string]any{
			"order_id": order.ID, "to": order.Status, "reason": reason, "lock": "pessimistic",
		}, s.clock.Now()), nil
	})
	if err != nil {
		return nil, err
	}
	s.logStatusChange(ctx, updated, "pessimistic")
	return updated, nil
}

// ---------------------------------------------------------------------------
// USE CASE 5: Cancel — ALWAYS pessimistic, on purpose
// ---------------------------------------------------------------------------

// Cancel cancels an order, if the business rules allow it.
//
// ============================================================================
// WHY CANCELLATION IGNORES LOCK_STRATEGY AND ALWAYS TAKES A ROW LOCK
// ============================================================================
// Cancellation is not a simple field update. It is a CHECK-THEN-ACT with real
// side effects: refund the payment, release the stock, email the customer.
//
// Consider a customer double-clicking "Cancel" (two requests, ~10ms apart):
//
//	WITH OPTIMISTIC LOCKING:
//	  Both read status=PENDING. Both pass the "can cancel?" check. Both try to
//	  write. One wins; the other gets a version conflict and RETRIES — now
//	  reading CANCELLED and correctly refusing. That works, but only because
//	  we were careful. If the refund had been issued before the write, we
//	  would have refunded twice.
//
//	WITH A PESSIMISTIC LOCK:
//	  Request 2 BLOCKS at the SELECT ... FOR UPDATE. When request 1 commits,
//	  request 2 reads status=CANCELLED and returns "already cancelled"
//	  immediately. One decision, one refund, no retry, no race window at all.
//
// The general rule: when an operation has EXTERNAL SIDE EFFECTS that cannot be
// rolled back, prefer to SERIALISE (lock) rather than DETECT-AND-RETRY. The
// cost is a few milliseconds of waiting on a rare double-click; the benefit is
// that the dangerous interleaving cannot occur.
// ============================================================================
func (s *OrderService) Cancel(ctx context.Context, id, reason string) (*domain.Order, error) {
	log := logging.FromContext(ctx)

	ctx, cancel := context.WithTimeout(ctx, s.cfg.PessimisticTimeout)
	defer cancel()

	var wasAlreadyCancelled bool

	updated, err := s.repo.UpdateWithPessimisticLock(ctx, id, func(order *domain.Order) (domain.OutboxEvent, error) {
		if order.Status == domain.StatusCancelled {
			// IDEMPOTENT: a retry or a double-click returns success rather
			// than an error, and crucially emits NO event — so no second
			// refund and no second email.
			wasAlreadyCancelled = true
			return domain.NoEvent, nil
		}
		// The DOMAIN owns the rule "only a PENDING order may be cancelled by
		// a customer".
		if err := order.Cancel(reason, s.clock.Now()); err != nil {
			return domain.NoEvent, err
		}
		return s.buildEventUnchecked(order, domain.EventOrderCancelled, map[string]any{
			"order_id": order.ID, "reason": order.CancelReason, "total_minor": order.TotalAmount.Minor,
		}, s.clock.Now()), nil
	})
	if err != nil {
		return nil, err
	}

	if wasAlreadyCancelled {
		log.InfoContext(ctx, "cancel request for an already-cancelled order; treated as a no-op",
			slog.String("order_id", id))
		return updated, nil
	}

	log.InfoContext(ctx, "order cancelled",
		slog.String("order_id", updated.ID),
		slog.String("order_number", updated.OrderNumber),
		slog.String("reason", updated.CancelReason))
	s.mtx.IncCounter("orders_cancelled_total", "Orders cancelled")

	// Compensate the saga: release reserved stock and refund any charge.
	// This runs AFTER the lock is released (the repository call has returned),
	// because holding a database row lock across network calls to two other
	// services is how you convert a 200ms outage into a total one.
	if s.saga != nil {
		if err := s.saga.CompensateCancelledOrder(ctx, updated); err != nil {
			// The cancellation itself is already committed and is what the
			// customer asked for. A failed compensation is an internal
			// follow-up, so we log it for the recovery worker rather than
			// telling the customer their cancellation failed.
			log.ErrorContext(ctx, "compensation after cancellation failed; the recovery worker will retry",
				slog.String("order_id", updated.ID), slog.String("error", err.Error()))
		}
	}
	return updated, nil
}

// ---------------------------------------------------------------------------
// USE CASE 6: the background job
// ---------------------------------------------------------------------------

// AdvanceStalePendingOrders promotes PENDING -> PROCESSING in one set-based
// statement. Called by the worker; see internal/order/workers.
func (s *OrderService) AdvanceStalePendingOrders(ctx context.Context, olderThan time.Duration, limit int) (int, error) {
	cutoff := s.clock.Now().Add(-olderThan)
	count, err := s.repo.AdvanceStalePending(ctx, cutoff, limit)
	if err != nil {
		return 0, err
	}
	if count > 0 {
		logging.FromContext(ctx).InfoContext(ctx, "auto-advanced stale pending orders",
			slog.Int("count", count),
			slog.Time("cutoff", cutoff),
			slog.Duration("older_than", olderThan))
		s.mtx.AddCounter("orders_auto_advanced_total", "Orders auto-advanced by the background worker", float64(count))
	}
	return count, nil
}

// SagaStatesForOrder exposes saga progress for the diagnostic endpoint.
//
// It is on the SERVICE rather than letting the handler reach into the saga
// directly, so that the transport layer keeps depending on exactly one thing.
// Returning an empty slice when sagas are disabled (rather than nil-panicking)
// keeps the endpoint usable in every configuration.
func (s *OrderService) SagaStatesForOrder(orderID string) []SagaState {
	if s.saga == nil {
		return nil
	}
	return s.saga.StatesForOrder(orderID)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// buildEvent constructs an outbox event with a UNIQUE id.
func (s *OrderService) buildEvent(order *domain.Order, eventType string, payload map[string]any, now time.Time) (domain.OutboxEvent, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return domain.NoEvent, errs.Internal("event_encode_failed", "failed to encode the event payload").Wrap(err)
	}
	return domain.OutboxEvent{
		ID:            idgen.NewUUID(),
		AggregateType: "order",
		AggregateID:   order.ID,
		EventType:     eventType,
		Payload:       raw,
		CreatedAt:     now,
	}, nil
}

// buildEventUnchecked is for use inside a mutate callback, where returning an
// error would abort the whole transaction.
//
// A marshalling failure here is impossible in practice (the payloads are
// plain maps of strings and numbers), so DEGRADING to an event with an empty
// payload is strictly better than rolling back a legitimate cancellation
// because of a logging concern. The event still carries its type and
// aggregate id, which is what consumers key on.
func (s *OrderService) buildEventUnchecked(order *domain.Order, eventType string, payload map[string]any, now time.Time) domain.OutboxEvent {
	raw, err := json.Marshal(payload)
	if err != nil {
		s.log.Error("failed to encode an event payload; emitting the event without it",
			slog.String("event_type", eventType), slog.String("error", err.Error()))
		raw = []byte(`{}`)
	}
	return domain.OutboxEvent{
		ID:            idgen.NewUUID(),
		AggregateType: "order",
		AggregateID:   order.ID,
		EventType:     eventType,
		Payload:       raw,
		CreatedAt:     now,
	}
}

func (s *OrderService) logStatusChange(ctx context.Context, order *domain.Order, strategy string) {
	if order == nil {
		return
	}
	logging.FromContext(ctx).InfoContext(ctx, "order status updated",
		slog.String("order_id", order.ID),
		slog.String("order_number", order.OrderNumber),
		slog.String("status", string(order.Status)),
		slog.Int("version", order.Version),
		slog.String("lock_strategy", strategy))
	s.mtx.IncCounter("order_status_transitions_total", "Status transitions",
		"to", string(order.Status), "strategy", strategy)
}

// appendNote appends to the notes field, keeping it bounded.
//
// The length cap is not cosmetic: without it, a client that PATCHes an order
// 10,000 times grows one row's TEXT column without limit, which bloats the
// table, every backup, and every JSON response that includes the order.
func appendNote(existing, addition string) string {
	if addition == "" {
		return existing
	}
	combined := addition
	if existing != "" {
		combined = existing + " | " + addition
	}
	const maxNotes = 1000
	if len(combined) > maxNotes {
		// Keep the MOST RECENT note: when truncating an audit trail, the
		// latest entry is almost always the one being investigated.
		return combined[len(combined)-maxNotes:]
	}
	return combined
}

// codeOf extracts a stable error code for use as a metric label.
//
// IMPORTANT: we use the CODE, never the message. Messages are free-form and
// often interpolate ids, which would give one metric series per order id and
// destroy your monitoring system. Codes are a small fixed set.
func codeOf(err error) string {
	if e, ok := errs.As(err); ok {
		return e.Code
	}
	return "unknown"
}
