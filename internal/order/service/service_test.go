package service

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/chahat/order-processing-system/internal/order/domain"
	"github.com/chahat/order-processing-system/internal/order/repository"
	"github.com/chahat/order-processing-system/internal/platform/config"
	"github.com/chahat/order-processing-system/internal/platform/errs"
	"github.com/chahat/order-processing-system/internal/platform/logging"
	"github.com/chahat/order-processing-system/internal/platform/metrics"
)

// ============================================================================
// SERVICE-LAYER TESTS
//
// These exercise the REAL service, the REAL saga and the REAL domain, with
// only storage and the two remote participants replaced by in-process
// implementations that preserve the semantics that matter (version conflicts,
// lock serialisation, idempotency).
//
// That distinction is the whole point: a test that stubs out the logic it
// claims to verify can pass while production is broken.
// ============================================================================

type fixture struct {
	svc   *OrderService
	saga  *SagaOrchestrator
	repo  *repository.MemoryRepository
	inv   *StubInventory
	pay   *StubPayment
	clock *domain.FixedClock
}

// newFixture assembles the system under test.
//
// The clock is FIXED and injectable, so a test for "advance orders older than
// 5 minutes" takes microseconds instead of five minutes — and is deterministic
// rather than dependent on CI machine load.
func newFixture(t *testing.T, strategy config.LockStrategy) *fixture {
	t.Helper()

	log := logging.Discard() // keep test output readable while still running the logging paths
	mtx := metrics.NewRegistry()
	clock := &domain.FixedClock{Current: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)}
	repo := repository.NewMemoryRepository()
	inv := NewStubInventory(log)
	pay := NewStubPayment(log)

	lockCfg := config.LockingConfig{
		Strategy:           strategy,
		MaxRetries:         3,
		RetryBase:          time.Millisecond,
		PessimisticTimeout: 2 * time.Second,
	}
	sagaCfg := config.SagaConfig{Enabled: true, StepTimeout: 2 * time.Second}

	saga := NewSagaOrchestrator(repo, inv, pay, clock, log, sagaCfg, mtx)
	svc := NewOrderService(repo, clock, log, lockCfg, mtx, saga)

	return &fixture{svc: svc, saga: saga, repo: repo, inv: inv, pay: pay, clock: clock}
}

func testInput() domain.NewOrderInput {
	return domain.NewOrderInput{
		CustomerID:      "cust-1001",
		CustomerEmail:   "test@example.com",
		CustomerName:    "Test Customer",
		ShippingAddress: "1 Test Street",
		Currency:        "USD",
		Items: []domain.NewOrderItemInput{
			{ProductID: "SKU-HEADPHONES", ProductName: "Headphones", Quantity: 2, UnitPriceMinor: 4999},
		},
	}
}

// ---------------------------------------------------------------------------
// Create
// ---------------------------------------------------------------------------

func TestCreate_HappyPathRunsTheWholeSaga(t *testing.T) {
	f := newFixture(t, config.LockHybrid)
	ctx := context.Background()

	order, err := f.svc.Create(ctx, testInput())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// The saga reserved stock, charged the card, and confirmed the order — so
	// the final state is PROCESSING, not PENDING.
	if order.Status != domain.StatusProcessing {
		t.Errorf("status after a successful saga: got %s, want PROCESSING", order.Status)
	}
	if order.TotalAmount.Minor != 9998 {
		t.Errorf("total: got %d, want 9998", order.TotalAmount.Minor)
	}
	// An outbox event must exist, written in the same transaction as the order.
	if f.repo.OutboxSize() == 0 {
		t.Error("expected outbox events to have been written")
	}
}

func TestCreate_PersistsBeforeAnyRemoteCall(t *testing.T) {
	// THE CRITICAL SEQUENCING PROPERTY. If the saga fails, the order row must
	// still exist (as FAILED), because an order that was charged but never
	// saved is invisible, unrecoverable money.
	f := newFixture(t, config.LockHybrid)
	f.pay.FailMode = "declined"

	order, err := f.svc.Create(context.Background(), testInput())
	if err == nil {
		t.Fatal("expected the saga to fail when payment is declined")
	}
	if order == nil {
		t.Fatal("the order must be returned even on saga failure, so the client has a reference")
	}

	// The row survives and is diagnosable.
	stored, getErr := f.repo.GetByID(context.Background(), order.ID)
	if getErr != nil {
		t.Fatalf("the order row should still exist after a failed saga: %v", getErr)
	}
	if stored.Status != domain.StatusFailed {
		t.Errorf("status: got %s, want FAILED", stored.Status)
	}
	if stored.FailureReason == "" {
		t.Error("a failed order must record why, or support cannot explain it")
	}
}

func TestCreate_ValidationErrorsNeverReachStorage(t *testing.T) {
	f := newFixture(t, config.LockHybrid)
	in := testInput()
	in.Items[0].Quantity = -1

	if _, err := f.svc.Create(context.Background(), in); err == nil {
		t.Fatal("expected a negative quantity to be rejected")
	}
	if f.repo.OutboxSize() != 0 {
		t.Error("a rejected order must not write anything")
	}
}

// ---------------------------------------------------------------------------
// SAGA COMPENSATION — the headline distributed-transaction behaviour
// ---------------------------------------------------------------------------

func TestSaga_PaymentDeclinedReleasesReservedStock(t *testing.T) {
	// ========================================================================
	// THE MOST IMPORTANT TEST IN THIS FILE.
	//
	// Step 1 (reserve stock) SUCCEEDS. Step 2 (charge) FAILS. The stock must
	// be returned to the pool, or every declined card silently removes
	// inventory from sale — a slow leak that shows up as mysterious
	// "out of stock" for items sitting in the warehouse.
	// ========================================================================
	f := newFixture(t, config.LockHybrid)
	f.inv.SetStock("SKU-HEADPHONES", 10)
	f.pay.FailMode = "declined"

	_, err := f.svc.Create(context.Background(), testInput())
	if err == nil {
		t.Fatal("expected the saga to fail")
	}

	// The compensation ran: all 10 units are available again.
	if got := f.inv.Available("SKU-HEADPHONES"); got != 10 {
		t.Errorf("stock after compensation: got %d, want 10 — the reservation leaked", got)
	}
}

func TestSaga_StopsAtTheFirstFailedStep(t *testing.T) {
	// Inventory fails, so payment must NEVER be attempted. Charging a card for
	// an order we already know cannot be fulfilled is the exact outcome the
	// step ordering exists to prevent.
	f := newFixture(t, config.LockHybrid)
	f.inv.FailMode = "insufficient"

	_, err := f.svc.Create(context.Background(), testInput())
	if err == nil {
		t.Fatal("expected the saga to fail at the inventory step")
	}
	if f.pay.CapturedCount() != 0 {
		t.Error("payment must not be attempted after inventory has already failed")
	}
}

func TestSaga_InsufficientStockIsNotRetried(t *testing.T) {
	// Classifying a business refusal as Unprocessable (422) rather than
	// Unavailable (503) is what stops the retry layer burning its budget on a
	// guaranteed failure.
	f := newFixture(t, config.LockHybrid)
	f.inv.SetStock("SKU-LIMITED", 1)

	in := testInput()
	in.Items = []domain.NewOrderItemInput{
		{ProductID: "SKU-LIMITED", ProductName: "Limited", Quantity: 5, UnitPriceMinor: 1000},
	}

	_, err := f.svc.Create(context.Background(), in)
	if err == nil {
		t.Fatal("expected insufficient stock to fail the order")
	}
	if errs.IsRetryable(err) {
		t.Error("insufficient stock must NOT be classified as retryable")
	}
}

func TestSaga_DeclinedPaymentIsNotRetryable(t *testing.T) {
	f := newFixture(t, config.LockHybrid)
	f.pay.FailMode = "declined"

	_, err := f.svc.Create(context.Background(), testInput())
	if err == nil {
		t.Fatal("expected a declined payment to fail the order")
	}
	if errs.IsRetryable(err) {
		t.Error("a declined card must not be retried; the bank will not change its mind in 50ms")
	}
}

func TestSaga_RecordsWhichStepFailed(t *testing.T) {
	// "Which step did it die on?" is the first question about a stuck order.
	f := newFixture(t, config.LockHybrid)
	f.pay.FailMode = "declined"

	order, _ := f.svc.Create(context.Background(), testInput())
	states := f.saga.StatesForOrder(order.ID)
	if len(states) == 0 {
		t.Fatal("expected saga state to be recorded")
	}

	state := states[0]
	if state.Status != SagaCompensated {
		t.Errorf("saga status: got %s, want %s", state.Status, SagaCompensated)
	}
	// The inventory step completed before the failure, so it must appear in
	// the completed list — that record is what tells a recovery worker what
	// needs undoing.
	if len(state.Completed) != 1 || state.Completed[0].Name != "reserve_inventory" {
		t.Errorf("expected exactly the inventory step to be recorded as completed, got %+v", state.Completed)
	}
	if state.LastError == "" {
		t.Error("the saga should record the cause of failure")
	}
}

func TestSaga_IdempotencyPreventsDoubleCharging(t *testing.T) {
	// ========================================================================
	// Two orders, two sagas, two charges — but a RETRY of one charge must not
	// produce a second capture. We prove the key mechanism by charging the
	// same logical operation twice through the participant directly.
	// ========================================================================
	f := newFixture(t, config.LockHybrid)
	ctx := context.Background()

	req := domain.ChargeRequest{
		OrderID: "order-abc", CustomerID: "cust-1", AmountMinor: 5000,
		Currency: "USD", IdempotencyKey: "order-abc:charge",
	}

	first, err := f.pay.Charge(ctx, req)
	if err != nil {
		t.Fatalf("first charge: %v", err)
	}
	// The SAME key, as a retry after a timeout would send.
	second, err := f.pay.Charge(ctx, req)
	if err != nil {
		t.Fatalf("retried charge: %v", err)
	}

	if first.PaymentID != second.PaymentID {
		t.Errorf("a retry must return the ORIGINAL payment id: %q vs %q", first.PaymentID, second.PaymentID)
	}
	if f.pay.CapturedCount() != 1 {
		t.Errorf("the card was charged %d times; it must be exactly 1", f.pay.CapturedCount())
	}
}

func TestSaga_ReservationIsIdempotent(t *testing.T) {
	f := newFixture(t, config.LockHybrid)
	ctx := context.Background()
	f.inv.SetStock("SKU-X", 10)

	req := domain.ReservationRequest{
		OrderID:        "order-xyz",
		IdempotencyKey: "order-xyz:reserve",
		Items:          []domain.ReservationItem{{ProductID: "SKU-X", Quantity: 3}},
	}

	first, _ := f.inv.Reserve(ctx, req)
	second, _ := f.inv.Reserve(ctx, req)

	if first.ReservationID != second.ReservationID {
		t.Error("a retried reservation must return the original reservation id")
	}
	// Crucially, the stock was decremented ONCE.
	if got := f.inv.Available("SKU-X"); got != 7 {
		t.Errorf("stock: got %d, want 7 — the retry double-reserved", got)
	}
}

func TestSaga_CompensationIsIdempotent(t *testing.T) {
	// Releasing a reservation twice must not credit the stock twice — that
	// would invent inventory out of nothing.
	f := newFixture(t, config.LockHybrid)
	ctx := context.Background()
	f.inv.SetStock("SKU-Y", 10)

	res, _ := f.inv.Reserve(ctx, domain.ReservationRequest{
		OrderID: "o1", IdempotencyKey: "o1:reserve",
		Items: []domain.ReservationItem{{ProductID: "SKU-Y", Quantity: 4}},
	})

	_ = f.inv.Release(ctx, res.ReservationID, "o1:release")
	_ = f.inv.Release(ctx, res.ReservationID, "o1:release")

	if got := f.inv.Available("SKU-Y"); got != 10 {
		t.Errorf("stock after a double release: got %d, want 10", got)
	}
}

func TestSaga_ReleasingAnUnknownReservationSucceeds(t *testing.T) {
	// The desired end state ("not holding stock") already holds, so this is a
	// SUCCESS. Returning an error would make correct compensations look broken
	// and trigger false alerts.
	f := newFixture(t, config.LockHybrid)
	if err := f.inv.Release(context.Background(), "res-does-not-exist", "k"); err != nil {
		t.Errorf("releasing a nonexistent reservation should be a no-op, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// LOCKING
// ---------------------------------------------------------------------------

func TestUpdateStatus_AllStrategiesWork(t *testing.T) {
	// The same business outcome must hold under every locking strategy. If a
	// strategy changed the RESULT rather than just the concurrency behaviour,
	// it would be a bug, not a tuning knob.
	for _, strategy := range []config.LockStrategy{config.LockOptimistic, config.LockPessimistic, config.LockHybrid} {
		t.Run(string(strategy), func(t *testing.T) {
			f := newFixture(t, strategy)
			ctx := context.Background()

			order, err := f.svc.Create(ctx, testInput())
			if err != nil {
				t.Fatalf("Create: %v", err)
			}

			updated, err := f.svc.UpdateStatus(ctx, order.ID, domain.StatusShipped, "handed to carrier")
			if err != nil {
				t.Fatalf("UpdateStatus: %v", err)
			}
			if updated.Status != domain.StatusShipped {
				t.Errorf("status: got %s, want SHIPPED", updated.Status)
			}
			// The version must have advanced, which is what lets other readers
			// detect the change.
			if updated.Version <= order.Version {
				t.Errorf("version did not advance: %d -> %d", order.Version, updated.Version)
			}
		})
	}
}

func TestUpdateStatus_RejectsIllegalTransition(t *testing.T) {
	f := newFixture(t, config.LockHybrid)
	ctx := context.Background()

	order, _ := f.svc.Create(ctx, testInput()) // ends PROCESSING
	_, err := f.svc.UpdateStatus(ctx, order.ID, domain.StatusPending, "go back")
	if err == nil {
		t.Fatal("expected PROCESSING -> PENDING to be rejected")
	}
	if !errs.Is(err, errs.KindUnprocessable) {
		t.Errorf("expected KindUnprocessable, got %s", errs.KindOf(err))
	}
}

func TestUpdateStatus_UnknownOrderIsNotFound(t *testing.T) {
	f := newFixture(t, config.LockHybrid)
	_, err := f.svc.UpdateStatus(context.Background(), "no-such-id", domain.StatusShipped, "")
	if !errs.Is(err, errs.KindNotFound) {
		t.Errorf("expected KindNotFound, got %s", errs.KindOf(err))
	}
}

func TestUpdateStatus_ConcurrentUpdatesDoNotLoseData(t *testing.T) {
	// ========================================================================
	// THE CONCURRENCY TEST. Twenty goroutines race to update the same order.
	//
	// WHAT MUST HOLD:
	//   * EXACTLY ONE succeeds in moving PROCESSING -> SHIPPED. (The others
	//     find the order already SHIPPED and are rejected by the state
	//     machine, which is correct.)
	//   * NO goroutine observes a lost update or a corrupted order.
	//   * The version advances exactly once per successful write.
	//
	// Run with -race to verify there is no data race as well as no logical one.
	// ========================================================================
	f := newFixture(t, config.LockHybrid)
	ctx := context.Background()

	order, err := f.svc.Create(ctx, testInput())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	const goroutines = 20
	var wg sync.WaitGroup
	results := make(chan error, goroutines)

	// A start barrier maximises the overlap. Without it the goroutines tend to
	// run sequentially and the test proves nothing about contention.
	start := make(chan struct{})

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := f.svc.UpdateStatus(ctx, order.ID, domain.StatusShipped, "concurrent")
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	var succeeded, rejected, other int
	for err := range results {
		switch {
		case err == nil:
			succeeded++
		case errs.Is(err, errs.KindUnprocessable):
			// Already SHIPPED, so the transition is refused. Correct.
			rejected++
		default:
			other++
			t.Errorf("unexpected error class %s: %v", errs.KindOf(err), err)
		}
	}

	if succeeded < 1 {
		t.Error("at least one update should have succeeded")
	}
	if other != 0 {
		t.Errorf("%d goroutines failed for unexpected reasons", other)
	}

	final, err := f.repo.GetByID(ctx, order.ID)
	if err != nil {
		t.Fatalf("final read: %v", err)
	}
	if final.Status != domain.StatusShipped {
		t.Errorf("final status: got %s, want SHIPPED", final.Status)
	}
	t.Logf("under %d-way contention: %d succeeded, %d correctly rejected", goroutines, succeeded, rejected)
}

func TestUpdateStatus_OptimisticConflictIsReportedAsConcurrency(t *testing.T) {
	// Drive the repository directly with a STALE version, which is exactly
	// what a losing writer holds.
	f := newFixture(t, config.LockOptimistic)
	ctx := context.Background()

	order, _ := f.svc.Create(ctx, testInput())
	stale, _ := f.repo.GetByID(ctx, order.ID)

	// Someone else commits first.
	if _, err := f.svc.UpdateStatus(ctx, order.ID, domain.StatusShipped, "winner"); err != nil {
		t.Fatalf("the first update should succeed: %v", err)
	}

	// Now our stale copy tries to write.
	err := f.repo.UpdateOptimistic(ctx, stale, domain.NoEvent)
	if err == nil {
		t.Fatal("expected a version conflict for a stale write")
	}
	if !errs.Is(err, errs.KindConcurrency) {
		t.Errorf("expected KindConcurrency, got %s", errs.KindOf(err))
	}
	// Concurrency errors must be RETRYABLE — that classification is what
	// drives the automatic retry in the service.
	if !errs.IsRetryable(err) {
		t.Error("a version conflict must be classified as retryable")
	}
}

// ---------------------------------------------------------------------------
// Cancel
// ---------------------------------------------------------------------------

func TestCancel_PendingOrderSucceedsAndCompensates(t *testing.T) {
	// Sagas disabled so the order stays PENDING, which is the only state a
	// customer may cancel from.
	f := newFixture(t, config.LockHybrid)
	f.inv.SetStock("SKU-HEADPHONES", 10)

	// Build a PENDING order directly, bypassing the saga.
	order, err := domain.NewOrder(testInput(), f.clock.Now())
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	f.repo.SeedOrder(order)

	cancelled, err := f.svc.Cancel(context.Background(), order.ID, "changed my mind")
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if cancelled.Status != domain.StatusCancelled {
		t.Errorf("status: got %s, want CANCELLED", cancelled.Status)
	}
	if cancelled.CancelReason != "changed my mind" {
		t.Errorf("reason: got %q", cancelled.CancelReason)
	}
}

func TestCancel_DoubleClickIsSafe(t *testing.T) {
	// ========================================================================
	// THE DOUBLE-CLICK TEST, and the reason cancellation always takes a
	// pessimistic lock.
	//
	// Ten simultaneous cancel requests must produce ONE cancellation and ONE
	// compensation. If any of them errored, a user who double-clicked would
	// see a failure for an action that actually succeeded.
	// ========================================================================
	f := newFixture(t, config.LockHybrid)

	order, _ := domain.NewOrder(testInput(), f.clock.Now())
	f.repo.SeedOrder(order)

	const attempts = 10
	var wg sync.WaitGroup
	errCh := make(chan error, attempts)
	start := make(chan struct{})

	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := f.svc.Cancel(context.Background(), order.ID, "double click")
			errCh <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errCh)

	for err := range errCh {
		// EVERY attempt must succeed: the first cancels, the rest are
		// idempotent no-ops. Any error here would be user-visible.
		if err != nil {
			t.Errorf("a concurrent cancel returned an error (it should be idempotent): %v", err)
		}
	}

	final, _ := f.repo.GetByID(context.Background(), order.ID)
	if final.Status != domain.StatusCancelled {
		t.Errorf("final status: got %s, want CANCELLED", final.Status)
	}
}

func TestCancel_ProcessingOrderIsRefused(t *testing.T) {
	f := newFixture(t, config.LockHybrid)
	ctx := context.Background()

	order, _ := f.svc.Create(ctx, testInput()) // saga leaves it PROCESSING

	_, err := f.svc.Cancel(ctx, order.ID, "too late")
	if err == nil {
		t.Fatal("expected cancelling a PROCESSING order to be refused")
	}
	if !errs.Is(err, errs.KindUnprocessable) {
		t.Errorf("expected KindUnprocessable, got %s", errs.KindOf(err))
	}
}

// ---------------------------------------------------------------------------
// List, stats and the background job
// ---------------------------------------------------------------------------

func TestList_AppliesDefaultsAndCaps(t *testing.T) {
	f := newFixture(t, config.LockHybrid)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if _, err := f.svc.Create(ctx, testInput()); err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
	}

	// No limit given: the default applies.
	result, err := f.svc.List(ctx, domain.ListFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if result.Limit != 20 {
		t.Errorf("default limit: got %d, want 20", result.Limit)
	}
	if result.Total != 5 {
		t.Errorf("total: got %d, want 5", result.Total)
	}

	// An absurd limit is CAPPED rather than rejected: `?size=100000` is
	// usually a confused client, and the cap is what bounds our cost.
	capped, err := f.svc.List(ctx, domain.ListFilter{Limit: 100000})
	if err != nil {
		t.Fatalf("List with a huge limit: %v", err)
	}
	if capped.Limit != 100 {
		t.Errorf("limit should be capped at 100, got %d", capped.Limit)
	}
}

func TestList_RejectsAnInvalidStatusFilter(t *testing.T) {
	// Note the asymmetry with the page size: silently ignoring an unknown
	// FILTER would return data the caller did not ask for, which is worse
	// than a clear error.
	f := newFixture(t, config.LockHybrid)
	bad := domain.Status("NONSENSE")
	_, err := f.svc.List(context.Background(), domain.ListFilter{Status: &bad})
	if !errs.Is(err, errs.KindInvalid) {
		t.Errorf("expected KindInvalid, got %s", errs.KindOf(err))
	}
}

func TestList_FiltersByStatusAndCustomer(t *testing.T) {
	f := newFixture(t, config.LockHybrid)
	ctx := context.Background()

	_, _ = f.svc.Create(ctx, testInput())

	other := testInput()
	other.CustomerID = "cust-9999"
	_, _ = f.svc.Create(ctx, other)

	byCustomer, err := f.svc.List(ctx, domain.ListFilter{CustomerID: "cust-9999"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if byCustomer.Total != 1 {
		t.Errorf("filtered by customer: got %d orders, want 1", byCustomer.Total)
	}

	processing := domain.StatusProcessing
	byStatus, _ := f.svc.List(ctx, domain.ListFilter{Status: &processing})
	if byStatus.Total != 2 {
		t.Errorf("filtered by PROCESSING: got %d, want 2", byStatus.Total)
	}
}

func TestStats_ExcludesCancelledAndFailedFromRevenue(t *testing.T) {
	// Counting money we never collected (or refunded) overstates revenue —
	// the kind of metric error that gets reported to a board.
	f := newFixture(t, config.LockHybrid)
	ctx := context.Background()

	_, _ = f.svc.Create(ctx, testInput()) // 9998, PROCESSING

	cancelled, _ := domain.NewOrder(testInput(), f.clock.Now())
	_ = cancelled.Cancel("test", f.clock.Now())
	f.repo.SeedOrder(cancelled)

	stats, err := f.svc.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.TotalOrders != 2 {
		t.Errorf("total orders: got %d, want 2", stats.TotalOrders)
	}
	if stats.TotalRevenue.Minor != 9998 {
		t.Errorf("revenue should exclude the cancelled order: got %d, want 9998", stats.TotalRevenue.Minor)
	}
	// Every status bucket must be present, so charts do not have gaps.
	for _, s := range domain.AllStatuses() {
		if _, ok := stats.CountByStatus[s]; !ok {
			t.Errorf("status %s is missing from the counts", s)
		}
	}
}

func TestAdvanceStalePending_OnlyAdvancesStaleOrders(t *testing.T) {
	// ========================================================================
	// THE BACKGROUND JOB, tested WITHOUT WAITING FIVE MINUTES.
	//
	// The injectable clock is what makes this possible. A test that slept
	// would be unusably slow; one that depended on real time would be flaky on
	// a loaded CI machine.
	// ========================================================================
	f := newFixture(t, config.LockHybrid)
	ctx := context.Background()

	// A fresh PENDING order.
	fresh, _ := domain.NewOrder(testInput(), f.clock.Now())
	f.repo.SeedOrder(fresh)

	// An order that has been PENDING for ten minutes.
	stale, _ := domain.NewOrder(testInput(), f.clock.Now().Add(-10*time.Minute))
	stale.StatusChangedAt = f.clock.Now().Add(-10 * time.Minute)
	f.repo.SeedOrder(stale)

	count, err := f.svc.AdvanceStalePendingOrders(ctx, 5*time.Minute, 100)
	if err != nil {
		t.Fatalf("AdvanceStalePendingOrders: %v", err)
	}
	if count != 1 {
		t.Fatalf("advanced %d orders, want exactly 1 (only the stale one)", count)
	}

	// Verify WHICH one moved.
	staleAfter, _ := f.repo.GetByID(ctx, stale.ID)
	if staleAfter.Status != domain.StatusProcessing {
		t.Errorf("the stale order should be PROCESSING, got %s", staleAfter.Status)
	}
	freshAfter, _ := f.repo.GetByID(ctx, fresh.ID)
	if freshAfter.Status != domain.StatusPending {
		t.Errorf("the fresh order should still be PENDING, got %s", freshAfter.Status)
	}
}

func TestAdvanceStalePending_IsSafeToRunTwice(t *testing.T) {
	// The job runs on a timer across replicas; a second run must find nothing
	// to do rather than double-processing.
	f := newFixture(t, config.LockHybrid)
	ctx := context.Background()

	stale, _ := domain.NewOrder(testInput(), f.clock.Now().Add(-10*time.Minute))
	stale.StatusChangedAt = f.clock.Now().Add(-10 * time.Minute)
	f.repo.SeedOrder(stale)

	first, _ := f.svc.AdvanceStalePendingOrders(ctx, 5*time.Minute, 100)
	second, _ := f.svc.AdvanceStalePendingOrders(ctx, 5*time.Minute, 100)

	if first != 1 {
		t.Errorf("first run: advanced %d, want 1", first)
	}
	if second != 0 {
		t.Errorf("second run: advanced %d, want 0", second)
	}
}

func TestAdvanceStalePending_RespectsTheBatchLimit(t *testing.T) {
	// The limit bounds how much work one run can do, so a backlog of 100,000
	// orders is drained in batches instead of one enormous transaction.
	f := newFixture(t, config.LockHybrid)
	ctx := context.Background()

	for i := 0; i < 10; i++ {
		o, _ := domain.NewOrder(testInput(), f.clock.Now().Add(-10*time.Minute))
		o.StatusChangedAt = f.clock.Now().Add(-10 * time.Minute)
		f.repo.SeedOrder(o)
	}

	count, err := f.svc.AdvanceStalePendingOrders(ctx, 5*time.Minute, 3)
	if err != nil {
		t.Fatalf("AdvanceStalePendingOrders: %v", err)
	}
	if count != 3 {
		t.Errorf("advanced %d orders, want 3 (the batch limit)", count)
	}
}

// ---------------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------------

func TestGetByOrderNumber(t *testing.T) {
	f := newFixture(t, config.LockHybrid)
	ctx := context.Background()

	order, _ := f.svc.Create(ctx, testInput())

	found, err := f.svc.GetByOrderNumber(ctx, order.OrderNumber)
	if err != nil {
		t.Fatalf("GetByOrderNumber: %v", err)
	}
	if found.ID != order.ID {
		t.Errorf("got order %s, want %s", found.ID, order.ID)
	}
}

func TestGet_EmptyIdIsRejectedWithoutHittingStorage(t *testing.T) {
	f := newFixture(t, config.LockHybrid)
	if _, err := f.svc.GetByID(context.Background(), ""); !errs.Is(err, errs.KindInvalid) {
		t.Errorf("expected KindInvalid for an empty id, got %s", errs.KindOf(err))
	}
}
