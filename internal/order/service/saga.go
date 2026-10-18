package service

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/chahat/order-processing-system/internal/order/domain"
	"github.com/chahat/order-processing-system/internal/platform/config"
	"github.com/chahat/order-processing-system/internal/platform/errs"
	"github.com/chahat/order-processing-system/internal/platform/idgen"
	"github.com/chahat/order-processing-system/internal/platform/logging"
	"github.com/chahat/order-processing-system/internal/platform/metrics"
)

// ============================================================================
// THE SAGA PATTERN
// ============================================================================
//
// THE PROBLEM IT SOLVES
// ---------------------
// Placing an order spans THREE services, each with its own database:
//
//	order service      -> create the order row
//	inventory service  -> reserve 2 units of SKU-123
//	payment service    -> charge $99.98
//
// We need all three or none. In a single database that is one transaction.
// Across three services it is impossible, because there is no shared
// transaction to roll back.
//
// WHY NOT TWO-PHASE COMMIT (2PC)?
//   - It is BLOCKING: if the coordinator dies after "prepare", every
//     participant holds its locks indefinitely, waiting for a decision that
//     never comes. Your inventory table is locked and nobody can order
//     anything.
//   - It requires every participant to support XA transactions. Stripe does
//     not offer you a 2PC endpoint, and never will.
//   - It couples availability: the system is only as available as the LEAST
//     available participant, multiplied together.
//
// THE SAGA ANSWER
// ---------------
// Give up atomicity; keep CONSISTENCY, eventually. Run a sequence of LOCAL
// transactions, each with a COMPENSATING ACTION that semantically undoes it.
// If step 3 fails, run the compensations for steps 2 and 1 in reverse.
//
//	FORWARD:       reserve stock -> charge card -> confirm order
//	COMPENSATE:    release stock <- refund card <- mark order FAILED
//
// A compensation is NOT a rollback. The charge really happened; the customer
// saw it on their statement. The refund is a NEW transaction that cancels its
// effect. Once you cross a service boundary, "undo" is always a forward action.
//
// ORCHESTRATION (what we do) vs CHOREOGRAPHY
// -----------------------------------------
//
//	ORCHESTRATION: one coordinator (this file) explicitly calls each step and
//	  decides what to compensate.
//	  + The whole flow is readable in ONE place. You can see the business
//	    process by reading one function.
//	  + Easy to debug: one log stream, one saga row recording where it got to.
//	  - The coordinator is a dependency hub and must itself be made reliable.
//
//	CHOREOGRAPHY: no coordinator. Each service listens for events and reacts.
//	  Order publishes OrderCreated -> Inventory reacts, publishes
//	  StockReserved -> Payment reacts...
//	  + No central component; services are decoupled.
//	  - THE FLOW EXISTS NOWHERE. To understand it you must read every service
//	    and mentally simulate the event graph. Debugging a stuck order means
//	    tracing events across five log streams.
//
// WE CHOSE ORCHESTRATION because the brief is explicitly that this codebase
// must be READABLE and EXPLAINABLE. For 3 steps, orchestration is clearer and
// the trade-off is favourable. At 15 steps across 10 teams, choreography's
// decoupling starts to win.
//
// THE FOUR RULES EVERY SAGA STEP MUST OBEY
// ----------------------------------------
//  1. IDEMPOTENT: it may be retried after an ambiguous timeout. Every call
//     carries an idempotency key.
//  2. COMPENSATABLE: there must be a semantic undo.
//  3. COMMUTATIVE-SAFE compensation: the compensation may arrive BEFORE the
//     action's own response (the "empty compensation" problem) — so releasing
//     a reservation that does not exist must succeed, not error.
//  4. RETRYABLE COMPENSATION: compensations must eventually succeed, because
//     there is nothing to compensate a failed compensation WITH. This is why
//     they retry harder than forward steps and ultimately alert a human.
// ============================================================================

// SagaStep is one unit of work with its undo.
//
// Modelling a step as DATA (a struct with two functions) rather than as code
// in a long function is what makes the orchestrator generic: it can log,
// time, retry and compensate every step uniformly, with no per-step
// boilerplate. This is the COMMAND pattern (behavioural, GoF).
type SagaStep struct {
	Name string
	// Execute performs the step. The returned string is an opaque handle (a
	// reservation id, a payment id) that Compensate needs.
	Execute func(ctx context.Context) (handle string, err error)
	// Compensate undoes the step, given the handle Execute returned.
	Compensate func(ctx context.Context, handle string) error
}

// SagaState records progress so a crashed saga can be recovered.
//
// WITHOUT PERSISTED STATE, a crash mid-saga is unrecoverable: nobody knows
// whether the card was charged or the stock reserved, so nobody can know what
// to compensate. The state row is the difference between "eventually
// consistent" and "permanently broken".
type SagaState struct {
	ID        string
	OrderID   string
	Status    string
	Completed []CompletedStep
	StartedAt time.Time
	EndedAt   time.Time
	LastError string
}

type CompletedStep struct {
	Name   string
	Handle string
	At     time.Time
}

const (
	SagaRunning      string = "RUNNING"
	SagaCompleted    string = "COMPLETED"
	SagaCompensating string = "COMPENSATING"
	SagaCompensated  string = "COMPENSATED"
	// SagaFailedToCompensate is the state that needs a HUMAN. We could not
	// undo our own partial work, which may mean a customer is charged for an
	// order they will not receive. It must be loud, visible and alertable —
	// silently swallowing it is the one unacceptable option.
	SagaFailedToCompensate string = "COMPENSATION_FAILED"
)

// SagaOrchestrator coordinates the create-order saga.
type SagaOrchestrator struct {
	repo      domain.OrderRepository
	inventory domain.InventoryPort
	payment   domain.PaymentPort
	clock     domain.Clock
	log       *slog.Logger
	cfg       config.SagaConfig
	mtx       *metrics.Registry

	// store keeps saga state. In-memory here, which is honest about its
	// limitation: state is lost if THIS process dies. The production answer is
	// a saga_instances table (the migration includes one) read by a recovery
	// worker. The interface is the same either way.
	mu    sync.RWMutex
	store map[string]*SagaState
}

func NewSagaOrchestrator(
	repo domain.OrderRepository,
	inventory domain.InventoryPort,
	payment domain.PaymentPort,
	clock domain.Clock,
	log *slog.Logger,
	cfg config.SagaConfig,
	mtx *metrics.Registry,
) *SagaOrchestrator {
	return &SagaOrchestrator{
		repo: repo, inventory: inventory, payment: payment,
		clock: clock, log: log, cfg: cfg, mtx: mtx,
		store: map[string]*SagaState{},
	}
}

// RunCreateOrderSaga executes the order-placement saga.
func (s *SagaOrchestrator) RunCreateOrderSaga(ctx context.Context, order *domain.Order) error {
	log := logging.FromContext(ctx).With(
		slog.String("order_id", order.ID),
		slog.String("saga", "create_order"))

	sagaID := idgen.NewUUID()
	state := &SagaState{
		ID: sagaID, OrderID: order.ID, Status: SagaRunning,
		StartedAt: s.clock.Now(),
	}
	s.saveState(state)

	log.InfoContext(ctx, "saga started", slog.String("saga_id", sagaID))
	s.mtx.IncCounter("saga_started_total", "Sagas started", "saga", "create_order")

	// ------------------------------------------------------------------
	// DEFINE THE STEPS.
	//
	// THE ORDER IS A BUSINESS DECISION, NOT AN ARBITRARY ONE:
	// we reserve stock BEFORE charging the card. If we charged first and then
	// found no stock, we would have to refund — which costs processor fees,
	// appears on the customer's statement twice, and generates a support
	// contact. Reserving first means the common failure (out of stock) costs
	// nothing and is invisible to the customer.
	//
	// GENERAL RULE: order saga steps cheapest-to-compensate FIRST, and put the
	// irreversible/expensive step LAST.
	// ------------------------------------------------------------------
	steps := []SagaStep{
		{
			Name: "reserve_inventory",
			Execute: func(ctx context.Context) (string, error) {
				items := make([]domain.ReservationItem, 0, len(order.Items))
				for _, it := range order.Items {
					items = append(items, domain.ReservationItem{ProductID: it.ProductID, Quantity: it.Quantity})
				}
				// A DETERMINISTIC idempotency key, derived from the order id
				// and the step name. Retrying this step reuses the key, so
				// inventory returns the EXISTING reservation instead of
				// reserving the stock a second time. A random key per attempt
				// would double-reserve on every retry.
				res, err := s.inventory.Reserve(ctx, domain.ReservationRequest{
					OrderID:        order.ID,
					IdempotencyKey: idempotencyKey(order.ID, "reserve"),
					Items:          items,
				})
				if err != nil {
					return "", err
				}
				if !res.Reserved {
					// A BUSINESS refusal, not a technical failure. Classified
					// as Unprocessable so the retry layer does not keep
					// asking for stock that does not exist.
					return "", errs.Unprocessable("insufficient_stock", res.Reason).
						WithDetail("reason", res.Reason)
				}
				return res.ReservationID, nil
			},
			Compensate: func(ctx context.Context, handle string) error {
				if handle == "" {
					// THE "EMPTY COMPENSATION" CASE (rule 3 above). The step
					// may have timed out AFTER inventory reserved the stock,
					// in which case we never learned the handle. Returning nil
					// here would silently leak the reservation; instead the
					// inventory service also expires reservations by TTL,
					// which is the backstop for exactly this case.
					return nil
				}
				return s.inventory.Release(ctx, handle, idempotencyKey(order.ID, "release"))
			},
		},
		{
			Name: "charge_payment",
			Execute: func(ctx context.Context) (string, error) {
				res, err := s.payment.Charge(ctx, domain.ChargeRequest{
					OrderID:        order.ID,
					CustomerID:     order.CustomerID,
					CustomerEmail:  order.CustomerEmail,
					AmountMinor:    order.TotalAmount.Minor,
					Currency:       order.TotalAmount.Currency,
					IdempotencyKey: idempotencyKey(order.ID, "charge"),
				})
				if err != nil {
					return "", err
				}
				if !res.Captured {
					return "", errs.Unprocessable("payment_declined", res.Reason).
						WithDetail("reason", res.Reason)
				}
				return res.PaymentID, nil
			},
			Compensate: func(ctx context.Context, handle string) error {
				if handle == "" {
					return nil
				}
				return s.payment.Refund(ctx, handle, idempotencyKey(order.ID, "refund"), "order saga failed")
			},
		},
		{
			Name: "confirm_order",
			Execute: func(ctx context.Context) (string, error) {
				// The FINAL local transaction, which makes the saga's outcome
				// durable: PENDING -> PROCESSING.
				_, err := s.repo.UpdateWithPessimisticLock(ctx, order.ID, func(o *domain.Order) (domain.OutboxEvent, error) {
					if o.Status != domain.StatusPending {
						// Someone cancelled the order while the saga was
						// running. We must NOT force it to PROCESSING; the
						// correct response is to fail the saga so the
						// compensations release the stock and refund the card.
						return domain.NoEvent, errs.Conflict("order_no_longer_pending",
							fmt.Sprintf("the order moved to %s while the saga was running", o.Status))
					}
					if err := o.TransitionTo(domain.StatusProcessing, s.clock.Now()); err != nil {
						return domain.NoEvent, err
					}
					o.SagaID = sagaID
					return domain.OutboxEvent{
						ID:            idgen.NewUUID(),
						AggregateType: "order",
						AggregateID:   o.ID,
						EventType:     domain.EventOrderStatusChanged,
						Payload:       []byte(fmt.Sprintf(`{"order_id":%q,"to":"PROCESSING","via":"saga"}`, o.ID)),
						CreatedAt:     s.clock.Now(),
					}, nil
				})
				return order.ID, err
			},
			// Confirming is the last step. If it fails, there is nothing after
			// it to undo, and the FAILED status is written by the compensation
			// path below. So this step needs no compensation of its own.
			Compensate: nil,
		},
	}

	// ------------------------------------------------------------------
	// EXECUTE FORWARD.
	// ------------------------------------------------------------------
	for _, step := range steps {
		// Each step gets its OWN timeout. A single timeout for the whole saga
		// would let a slow first step consume the entire budget and starve the
		// rest, making the failure look like it came from the wrong service.
		stepCtx, cancel := context.WithTimeout(ctx, s.cfg.StepTimeout)
		start := time.Now()
		handle, err := step.Execute(stepCtx)
		cancel()

		s.mtx.ObserveHistogram("saga_step_duration_seconds", "Saga step latency",
			time.Since(start).Seconds(), "step", step.Name)

		if err != nil {
			log.WarnContext(ctx, "saga step failed; beginning compensation",
				slog.String("step", step.Name), slog.String("error", err.Error()))
			s.mtx.IncCounter("saga_step_failed_total", "Saga steps that failed", "step", step.Name)

			state.Status = SagaCompensating
			state.LastError = err.Error()
			s.saveState(state)

			// Compensate everything completed SO FAR, then report the ORIGINAL
			// error — not the compensation's error. The customer needs to know
			// "payment declined", not "stock release returned 204".
			s.compensate(ctx, state, steps, err)
			return err
		}

		state.Completed = append(state.Completed, CompletedStep{
			Name: step.Name, Handle: handle, At: s.clock.Now(),
		})
		s.saveState(state)

		log.InfoContext(ctx, "saga step completed",
			slog.String("step", step.Name),
			slog.String("handle", handle),
			slog.Duration("duration", time.Since(start)))
	}

	state.Status = SagaCompleted
	state.EndedAt = s.clock.Now()
	s.saveState(state)

	log.InfoContext(ctx, "saga completed successfully", slog.String("saga_id", sagaID))
	s.mtx.IncCounter("saga_completed_total", "Sagas completed successfully", "saga", "create_order")
	return nil
}

// compensate undoes completed steps IN REVERSE ORDER.
//
// WHY REVERSE ORDER MATTERS: later steps may depend on earlier ones. Undoing
// forwards could remove a prerequisite that a later compensation still needs.
// Reverse order (a stack) is the only generally-correct choice — the same
// reason `defer` unwinds in reverse.
func (s *SagaOrchestrator) compensate(ctx context.Context, state *SagaState, steps []SagaStep, cause error) {
	log := logging.FromContext(ctx).With(
		slog.String("order_id", state.OrderID),
		slog.String("saga_id", state.ID))

	byName := map[string]SagaStep{}
	for _, st := range steps {
		byName[st.Name] = st
	}

	allCompensated := true

	for i := len(state.Completed) - 1; i >= 0; i-- {
		done := state.Completed[i]
		step, ok := byName[done.Name]
		if !ok || step.Compensate == nil {
			continue
		}

		// ------------------------------------------------------------------
		// COMPENSATIONS USE A FRESH, DETACHED CONTEXT.
		//
		// THIS IS SUBTLE AND IMPORTANT: the incoming ctx is very likely
		// ALREADY CANCELLED — the step failed because it timed out, or the
		// client hung up. If we reused it, every compensation would fail
		// instantly with "context canceled", and we would leave the stock
		// reserved and the card charged.
		//
		// context.WithoutCancel (Go 1.21+) keeps the context's VALUES (so the
		// request id still appears in logs, which is how you trace this later)
		// while discarding the cancellation. Getting this wrong is one of the
		// most consequential bugs you can write in a saga.
		// ------------------------------------------------------------------
		compCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.StepTimeout)

		// Compensations retry HARDER than forward steps (rule 4): a forward
		// step that fails is simply not done, but a compensation that fails
		// leaves real inconsistency — money or stock in the wrong place.
		var compErr error
		for attempt := 1; attempt <= 3; attempt++ {
			compErr = step.Compensate(compCtx, done.Handle)
			if compErr == nil {
				break
			}
			log.WarnContext(ctx, "compensation attempt failed",
				slog.String("step", done.Name),
				slog.Int("attempt", attempt),
				slog.String("error", compErr.Error()))
			// A short linear backoff, bounded. We cannot wait long: the
			// customer is waiting for a response.
			time.Sleep(time.Duration(attempt) * 100 * time.Millisecond)
		}
		cancel()

		if compErr != nil {
			// THE WORST CASE. We have failed to undo our own work and cannot
			// fix it automatically.
			//
			// The ONE thing we must do is make it IMPOSSIBLE TO MISS: log at
			// ERROR with every identifier a human needs (order, saga, step,
			// handle), and increment a metric that an alert fires on. Someone
			// must reconcile this by hand. Silently swallowing it would mean a
			// customer is charged for nothing and nobody ever finds out.
			allCompensated = false
			log.ErrorContext(ctx, "COMPENSATION FAILED - MANUAL RECONCILIATION REQUIRED",
				slog.String("step", done.Name),
				slog.String("handle", done.Handle),
				slog.String("error", compErr.Error()),
				slog.String("original_cause", cause.Error()))
			s.mtx.IncCounter("saga_compensation_failed_total",
				"Compensations that could not be applied (ALERT ON THIS)", "step", done.Name)
			continue // still try to compensate the remaining steps
		}

		log.InfoContext(ctx, "compensation applied",
			slog.String("step", done.Name), slog.String("handle", done.Handle))
		s.mtx.IncCounter("saga_compensated_total", "Compensations applied", "step", done.Name)
	}

	// Mark the order FAILED so it is not left PENDING forever, pretending to
	// be in progress. A visible terminal state is what makes this recoverable.
	failCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, err := s.repo.UpdateWithPessimisticLock(failCtx, state.OrderID, func(o *domain.Order) (domain.OutboxEvent, error) {
		if err := o.Fail(cause.Error(), s.clock.Now()); err != nil {
			return domain.NoEvent, err
		}
		o.SagaID = state.ID
		return domain.OutboxEvent{
			ID:            idgen.NewUUID(),
			AggregateType: "order",
			AggregateID:   o.ID,
			EventType:     domain.EventOrderFailed,
			Payload:       []byte(fmt.Sprintf(`{"order_id":%q,"reason":%q}`, o.ID, cause.Error())),
			CreatedAt:     s.clock.Now(),
		}, nil
	}); err != nil {
		log.ErrorContext(ctx, "failed to mark the order FAILED after compensation",
			slog.String("error", err.Error()))
	}

	if allCompensated {
		state.Status = SagaCompensated
	} else {
		state.Status = SagaFailedToCompensate
	}
	state.EndedAt = s.clock.Now()
	s.saveState(state)
}

// CompensateCancelledOrder releases stock and refunds a charge after a
// customer cancellation.
//
// Note that this reuses the SAME idempotency keys as the forward saga. If the
// saga already refunded (because it failed), the payment service recognises
// the key and returns the existing refund rather than refunding twice. One
// consistent key scheme protects every path — that is why it is a function.
func (s *SagaOrchestrator) CompensateCancelledOrder(ctx context.Context, order *domain.Order) error {
	log := logging.FromContext(ctx).With(slog.String("order_id", order.ID))

	// Detached so a cancelled client context cannot abort the refund.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.StepTimeout*2)
	defer cancel()

	var firstErr error

	// Release the reservation. The handle is derived from the order id, which
	// is why the inventory service accepts a release keyed by order as well as
	// by reservation id — it means we do not need to have stored the handle.
	if err := s.inventory.Release(ctx, "order:"+order.ID, idempotencyKey(order.ID, "release")); err != nil {
		// NOT FOUND is a SUCCESS here: it means there is nothing to release,
		// which is exactly the state we want. Treating it as an error would
		// make a correct outcome look like a failure and trigger false alerts.
		if !errs.Is(err, errs.KindNotFound) {
			log.WarnContext(ctx, "failed to release the inventory reservation on cancellation",
				slog.String("error", err.Error()))
			firstErr = err
		}
	}

	if err := s.payment.Refund(ctx, "order:"+order.ID, idempotencyKey(order.ID, "refund"), "order cancelled by customer"); err != nil {
		if !errs.Is(err, errs.KindNotFound) {
			log.WarnContext(ctx, "failed to refund the payment on cancellation",
				slog.String("error", err.Error()))
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// idempotencyKey builds a DETERMINISTIC key for one (order, operation) pair.
//
// ============================================================================
// WHY DETERMINISTIC AND NOT RANDOM — the single most important line in this
// file for correctness of money.
//
// With a random key per attempt:
//
//	attempt 1: POST /charge  key=abc  -> times out (BUT THE CARD WAS CHARGED)
//	attempt 2: POST /charge  key=xyz  -> payment service sees a NEW key
//	                                     -> CHARGES AGAIN. Customer billed 2x.
//
// With a deterministic key:
//
//	attempt 1: POST /charge  key=order-123:charge -> times out (charged)
//	attempt 2: POST /charge  key=order-123:charge -> "I have seen this key;
//	                                     here is the original result" -> no
//	                                     second charge.
//
// The key must be derived from the LOGICAL OPERATION, never from the attempt.
// ============================================================================
func idempotencyKey(orderID, operation string) string {
	return fmt.Sprintf("%s:%s", orderID, operation)
}

func (s *SagaOrchestrator) saveState(state *SagaState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Store a COPY: the caller keeps mutating `state` as the saga proceeds,
	// and we do not want a reader to observe a half-updated struct.
	cp := *state
	cp.Completed = make([]CompletedStep, len(state.Completed))
	copy(cp.Completed, state.Completed)
	s.store[state.ID] = &cp
}

// GetState exposes saga state for the debug endpoint and for tests.
func (s *SagaOrchestrator) GetState(id string) (*SagaState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st, ok := s.store[id]
	return st, ok
}

// StatesForOrder returns every saga recorded for an order. The debug endpoint
// uses it to show exactly which step a stuck order died on — which is the
// first question anyone asks.
func (s *SagaOrchestrator) StatesForOrder(orderID string) []SagaState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []SagaState
	for _, st := range s.store {
		if st.OrderID == orderID {
			out = append(out, *st)
		}
	}
	return out
}
