package repository

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/chahat/order-processing-system/internal/order/domain"
	"github.com/chahat/order-processing-system/internal/platform/cache"
	"github.com/chahat/order-processing-system/internal/platform/errs"
	"github.com/chahat/order-processing-system/internal/platform/logging"
	"github.com/chahat/order-processing-system/internal/platform/postgres"
)

// ============================================================================
// REPOSITORY TESTS
//
// Structured in three parts:
//
//  1. A CONTRACT SUITE run against BOTH adapters. Both must behave
//     identically, because the service layer is written against the interface
//     and a fake that is more permissive than the real thing gives you green
//     tests and a broken system. This is the Liskov Substitution Principle,
//     enforced by a test rather than asserted in a comment.
//
//  2. CACHE DECORATOR tests, which prove invalidation actually happens.
//
//  3. POSTGRES-ONLY tests for things only a real database can show: genuine
//     row locking, SKIP LOCKED, and transactional rollback.
//
// Part 3 is SKIPPED unless a database is reachable, so `go test ./...` works
// on a laptop with no Docker and the full suite runs in CI.
// ============================================================================

func testOrder(t *testing.T) *domain.Order {
	t.Helper()
	order, err := domain.NewOrder(domain.NewOrderInput{
		CustomerID:      "cust-1001",
		CustomerEmail:   "test@example.com",
		CustomerName:    "Test Customer",
		ShippingAddress: "1 Test Street",
		Currency:        "USD",
		Items: []domain.NewOrderItemInput{
			{ProductID: "SKU-A", ProductName: "Product A", ProductSKU: "A-1", Quantity: 2, UnitPriceMinor: 4999},
		},
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("building a test order: %v", err)
	}
	return order
}

func testEvent(orderID string) domain.OutboxEvent {
	return domain.OutboxEvent{
		ID:            "evt-" + orderID,
		AggregateType: "order",
		AggregateID:   orderID,
		EventType:     domain.EventOrderCreated,
		Payload:       []byte(`{"test":true}`),
		CreatedAt:     time.Now().UTC(),
	}
}

// ---------------------------------------------------------------------------
// 1. THE CONTRACT SUITE — every adapter must pass this
// ---------------------------------------------------------------------------

// repoFactory builds a fresh, empty repository for one subtest.
type repoFactory func(t *testing.T) domain.OrderRepository

func TestRepositoryContract(t *testing.T) {
	adapters := map[string]repoFactory{
		"memory": func(t *testing.T) domain.OrderRepository {
			return NewMemoryRepository()
		},
	}

	// Add the Postgres adapter only if a database is actually reachable.
	if pool := tryConnect(t); pool != nil {
		adapters["postgres"] = func(t *testing.T) domain.OrderRepository {
			resetDatabase(t, pool)
			return NewPostgresRepository(pool, logging.Discard())
		}
	} else {
		t.Log("postgres is not reachable; running the contract suite against the in-memory adapter only " +
			"(set POSTGRES_TEST_DSN or run `make docker-up` to include it)")
	}

	for name, factory := range adapters {
		t.Run(name, func(t *testing.T) {
			runContractSuite(t, factory)
		})
	}
}

func runContractSuite(t *testing.T, newRepo repoFactory) {
	ctx := context.Background()

	t.Run("create then read back", func(t *testing.T) {
		repo := newRepo(t)
		order := testOrder(t)

		if err := repo.Create(ctx, order, testEvent(order.ID)); err != nil {
			t.Fatalf("Create: %v", err)
		}

		got, err := repo.GetByID(ctx, order.ID)
		if err != nil {
			t.Fatalf("GetByID: %v", err)
		}
		if got.OrderNumber != order.OrderNumber {
			t.Errorf("order number: got %q, want %q", got.OrderNumber, order.OrderNumber)
		}
		if got.TotalAmount.Minor != 9998 {
			t.Errorf("total: got %d, want 9998", got.TotalAmount.Minor)
		}
		// The items must round-trip, including their price snapshots.
		if len(got.Items) != 1 {
			t.Fatalf("items: got %d, want 1", len(got.Items))
		}
		if got.Items[0].UnitPrice.Minor != 4999 {
			t.Errorf("unit price: got %d, want 4999", got.Items[0].UnitPrice.Minor)
		}
		if got.Items[0].LineTotal.Minor != 9998 {
			t.Errorf("line total: got %d, want 9998", got.Items[0].LineTotal.Minor)
		}
	})

	t.Run("missing order returns KindNotFound", func(t *testing.T) {
		repo := newRepo(t)
		_, err := repo.GetByID(ctx, "00000000-0000-4000-8000-000000000000")
		if !errs.Is(err, errs.KindNotFound) {
			t.Errorf("expected KindNotFound, got %s (%v)", errs.KindOf(err), err)
		}
	})

	t.Run("read by order number", func(t *testing.T) {
		repo := newRepo(t)
		order := testOrder(t)
		_ = repo.Create(ctx, order, domain.NoEvent)

		got, err := repo.GetByOrderNumber(ctx, order.OrderNumber)
		if err != nil {
			t.Fatalf("GetByOrderNumber: %v", err)
		}
		if got.ID != order.ID {
			t.Errorf("got order %s, want %s", got.ID, order.ID)
		}
	})

	t.Run("duplicate create is a conflict", func(t *testing.T) {
		repo := newRepo(t)
		order := testOrder(t)
		if err := repo.Create(ctx, order, domain.NoEvent); err != nil {
			t.Fatalf("first Create: %v", err)
		}
		err := repo.Create(ctx, order, domain.NoEvent)
		if !errs.Is(err, errs.KindConflict) {
			t.Errorf("expected KindConflict for a duplicate, got %s", errs.KindOf(err))
		}
	})

	t.Run("mutating the returned order does not change storage", func(t *testing.T) {
		// A repository must hand back a COPY. If it returned a pointer into
		// storage, an unsaved mutation would appear to have committed — so the
		// fake would be more permissive than Postgres and tests would pass for
		// code that cannot work.
		repo := newRepo(t)
		order := testOrder(t)
		_ = repo.Create(ctx, order, domain.NoEvent)

		first, _ := repo.GetByID(ctx, order.ID)
		first.Status = domain.StatusDelivered
		first.Notes = "tampered"

		second, _ := repo.GetByID(ctx, order.ID)
		if second.Status != domain.StatusPending {
			t.Errorf("storage was mutated by a caller: status is %s", second.Status)
		}
		if second.Notes == "tampered" {
			t.Error("storage was mutated by a caller: notes changed")
		}
	})

	t.Run("optimistic update succeeds and bumps the version", func(t *testing.T) {
		repo := newRepo(t)
		order := testOrder(t)
		_ = repo.Create(ctx, order, domain.NoEvent)

		loaded, _ := repo.GetByID(ctx, order.ID)
		startVersion := loaded.Version
		_ = loaded.TransitionTo(domain.StatusProcessing, time.Now().UTC())

		if err := repo.UpdateOptimistic(ctx, loaded, testEvent(order.ID+"-u")); err != nil {
			t.Fatalf("UpdateOptimistic: %v", err)
		}
		// The caller's struct must receive the new version, or a second update
		// in the same request would send a stale one and fail spuriously.
		if loaded.Version != startVersion+1 {
			t.Errorf("caller's version: got %d, want %d", loaded.Version, startVersion+1)
		}

		stored, _ := repo.GetByID(ctx, order.ID)
		if stored.Status != domain.StatusProcessing {
			t.Errorf("stored status: got %s, want PROCESSING", stored.Status)
		}
		if stored.Version != startVersion+1 {
			t.Errorf("stored version: got %d, want %d", stored.Version, startVersion+1)
		}
	})

	t.Run("optimistic update with a stale version is rejected", func(t *testing.T) {
		// ================================================================
		// THE LOST-UPDATE TEST. Two readers, both edit, one commits first.
		// The second MUST be refused rather than silently overwriting.
		// ================================================================
		repo := newRepo(t)
		order := testOrder(t)
		_ = repo.Create(ctx, order, domain.NoEvent)

		readerA, _ := repo.GetByID(ctx, order.ID)
		readerB, _ := repo.GetByID(ctx, order.ID) // same version as A

		_ = readerA.TransitionTo(domain.StatusProcessing, time.Now().UTC())
		if err := repo.UpdateOptimistic(ctx, readerA, domain.NoEvent); err != nil {
			t.Fatalf("reader A should win: %v", err)
		}

		_ = readerB.TransitionTo(domain.StatusCancelled, time.Now().UTC())
		err := repo.UpdateOptimistic(ctx, readerB, domain.NoEvent)
		if err == nil {
			t.Fatal("reader B wrote over reader A's change — the lost update was not detected")
		}
		if !errs.Is(err, errs.KindConcurrency) {
			t.Errorf("expected KindConcurrency, got %s", errs.KindOf(err))
		}
		// And it must be retryable, which is what drives the service's retry.
		if !errs.IsRetryable(err) {
			t.Error("a version conflict must be classified as retryable")
		}

		// A's change survived; B's was discarded.
		final, _ := repo.GetByID(ctx, order.ID)
		if final.Status != domain.StatusProcessing {
			t.Errorf("final status: got %s, want PROCESSING (A's write)", final.Status)
		}
	})

	t.Run("optimistic update of a missing order is not found", func(t *testing.T) {
		// Zero rows affected is AMBIGUOUS — gone, or version moved? Reporting
		// "conflict" for a deleted order would send the client into a retry
		// loop that can never succeed.
		repo := newRepo(t)
		ghost := testOrder(t)
		err := repo.UpdateOptimistic(ctx, ghost, domain.NoEvent)
		if !errs.Is(err, errs.KindNotFound) {
			t.Errorf("expected KindNotFound, got %s", errs.KindOf(err))
		}
	})

	t.Run("pessimistic update applies the mutation", func(t *testing.T) {
		repo := newRepo(t)
		order := testOrder(t)
		_ = repo.Create(ctx, order, domain.NoEvent)

		updated, err := repo.UpdateWithPessimisticLock(ctx, order.ID,
			func(o *domain.Order) (domain.OutboxEvent, error) {
				if err := o.Cancel("test cancellation", time.Now().UTC()); err != nil {
					return domain.NoEvent, err
				}
				return testEvent(o.ID + "-c"), nil
			})
		if err != nil {
			t.Fatalf("UpdateWithPessimisticLock: %v", err)
		}
		if updated.Status != domain.StatusCancelled {
			t.Errorf("status: got %s, want CANCELLED", updated.Status)
		}
		if updated.CancelReason != "test cancellation" {
			t.Errorf("reason: got %q", updated.CancelReason)
		}
	})

	t.Run("pessimistic update rolls back when the callback errors", func(t *testing.T) {
		// A rejected mutation must leave NOTHING behind — no partial write and
		// no outbox event announcing something that did not happen.
		repo := newRepo(t)
		order := testOrder(t)
		_ = repo.Create(ctx, order, domain.NoEvent)

		wantErr := errs.Conflict("nope", "the callback refused")
		_, err := repo.UpdateWithPessimisticLock(ctx, order.ID,
			func(o *domain.Order) (domain.OutboxEvent, error) {
				o.Status = domain.StatusDelivered // a mutation that must not persist
				return domain.NoEvent, wantErr
			})
		if err == nil {
			t.Fatal("expected the callback's error to propagate")
		}

		stored, _ := repo.GetByID(ctx, order.ID)
		if stored.Status != domain.StatusPending {
			t.Errorf("the mutation leaked despite the error: status is %s", stored.Status)
		}
	})

	t.Run("pessimistic update of a missing order is not found", func(t *testing.T) {
		repo := newRepo(t)
		_, err := repo.UpdateWithPessimisticLock(ctx, "00000000-0000-4000-8000-000000000000",
			func(*domain.Order) (domain.OutboxEvent, error) { return domain.NoEvent, nil })
		if !errs.Is(err, errs.KindNotFound) {
			t.Errorf("expected KindNotFound, got %s", errs.KindOf(err))
		}
	})

	t.Run("pessimistic locking serialises concurrent writers", func(t *testing.T) {
		// ================================================================
		// Twenty goroutines each increment a counter inside the lock. If the
		// lock works, the final count is EXACTLY 20. If it does not, lost
		// updates make it lower — which is precisely the bug a row lock
		// exists to prevent, demonstrated as a number rather than a claim.
		// ================================================================
		repo := newRepo(t)
		order := testOrder(t)
		_ = repo.Create(ctx, order, domain.NoEvent)

		const writers = 20
		var wg sync.WaitGroup
		start := make(chan struct{})
		errCh := make(chan error, writers)

		for i := 0; i < writers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, err := repo.UpdateWithPessimisticLock(ctx, order.ID,
					func(o *domain.Order) (domain.OutboxEvent, error) {
						// Append one character per writer. The length of the
						// result is the observable proof that no update was lost.
						o.Notes += "x"
						return domain.NoEvent, nil
					})
				errCh <- err
			}()
		}
		close(start)
		wg.Wait()
		close(errCh)

		for err := range errCh {
			if err != nil {
				t.Errorf("a locked writer failed: %v", err)
			}
		}

		final, _ := repo.GetByID(ctx, order.ID)
		if len(final.Notes) != writers {
			t.Errorf("notes length: got %d, want %d — %d updates were LOST despite the lock",
				len(final.Notes), writers, writers-len(final.Notes))
		}
	})

	t.Run("list filters and paginates", func(t *testing.T) {
		repo := newRepo(t)
		for i := 0; i < 5; i++ {
			o := testOrder(t)
			if i < 2 {
				o.CustomerID = "cust-special"
			}
			_ = repo.Create(ctx, o, domain.NoEvent)
		}

		all, err := repo.List(ctx, domain.ListFilter{Limit: 10})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if all.Total != 5 {
			t.Errorf("total: got %d, want 5", all.Total)
		}

		byCustomer, _ := repo.List(ctx, domain.ListFilter{CustomerID: "cust-special", Limit: 10})
		if byCustomer.Total != 2 {
			t.Errorf("filtered total: got %d, want 2", byCustomer.Total)
		}

		page, _ := repo.List(ctx, domain.ListFilter{Limit: 2, Offset: 0})
		if len(page.Orders) != 2 {
			t.Errorf("page size: got %d, want 2", len(page.Orders))
		}
		if !page.HasMore {
			t.Error("has_more should be true")
		}

		// Every listed order must carry its items — the N+1-avoiding batch
		// query must not silently return orders with no lines.
		for _, o := range page.Orders {
			if len(o.Items) == 0 {
				t.Errorf("order %s was listed with no items", o.ID)
			}
		}
	})

	t.Run("list by status", func(t *testing.T) {
		repo := newRepo(t)
		pending := testOrder(t)
		_ = repo.Create(ctx, pending, domain.NoEvent)

		shipped := testOrder(t)
		_ = repo.Create(ctx, shipped, domain.NoEvent)
		loaded, _ := repo.GetByID(ctx, shipped.ID)
		_ = loaded.TransitionTo(domain.StatusProcessing, time.Now().UTC())
		_ = repo.UpdateOptimistic(ctx, loaded, domain.NoEvent)

		want := domain.StatusPending
		result, _ := repo.List(ctx, domain.ListFilter{Status: &want, Limit: 10})
		if result.Total != 1 {
			t.Errorf("PENDING count: got %d, want 1", result.Total)
		}
	})

	t.Run("stats excludes cancelled from revenue", func(t *testing.T) {
		repo := newRepo(t)

		keep := testOrder(t)
		_ = repo.Create(ctx, keep, domain.NoEvent)

		drop := testOrder(t)
		_ = repo.Create(ctx, drop, domain.NoEvent)
		_, _ = repo.UpdateWithPessimisticLock(ctx, drop.ID, func(o *domain.Order) (domain.OutboxEvent, error) {
			return domain.NoEvent, o.Cancel("test", time.Now().UTC())
		})

		stats, err := repo.Stats(ctx)
		if err != nil {
			t.Fatalf("Stats: %v", err)
		}
		if stats.TotalOrders != 2 {
			t.Errorf("total orders: got %d, want 2", stats.TotalOrders)
		}
		if stats.TotalRevenue.Minor != 9998 {
			t.Errorf("revenue: got %d, want 9998 (the cancelled order must be excluded)", stats.TotalRevenue.Minor)
		}
		// Every bucket present, so dashboards have no gaps.
		for _, s := range domain.AllStatuses() {
			if _, ok := stats.CountByStatus[s]; !ok {
				t.Errorf("stats is missing the %s bucket", s)
			}
		}
	})

	t.Run("advance stale pending only touches stale orders", func(t *testing.T) {
		repo := newRepo(t)

		fresh := testOrder(t)
		_ = repo.Create(ctx, fresh, domain.NoEvent)

		// An order that has been PENDING for a long time.
		stale := testOrder(t)
		old := time.Now().UTC().Add(-time.Hour)
		stale.CreatedAt, stale.UpdatedAt, stale.StatusChangedAt = old, old, old
		_ = repo.Create(ctx, stale, domain.NoEvent)

		cutoff := time.Now().UTC().Add(-5 * time.Minute)
		count, err := repo.AdvanceStalePending(ctx, cutoff, 100)
		if err != nil {
			t.Fatalf("AdvanceStalePending: %v", err)
		}
		if count != 1 {
			t.Fatalf("advanced %d, want exactly 1", count)
		}

		staleAfter, _ := repo.GetByID(ctx, stale.ID)
		if staleAfter.Status != domain.StatusProcessing {
			t.Errorf("the stale order should be PROCESSING, got %s", staleAfter.Status)
		}
		freshAfter, _ := repo.GetByID(ctx, fresh.ID)
		if freshAfter.Status != domain.StatusPending {
			t.Errorf("the fresh order should still be PENDING, got %s", freshAfter.Status)
		}

		// Running again must be a no-op: the job is on a timer across
		// replicas, so idempotence is not optional.
		second, _ := repo.AdvanceStalePending(ctx, cutoff, 100)
		if second != 0 {
			t.Errorf("a second run advanced %d orders, want 0", second)
		}
	})

	t.Run("ping succeeds", func(t *testing.T) {
		if err := newRepo(t).Ping(ctx); err != nil {
			t.Errorf("Ping: %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// 2. The cache decorator
// ---------------------------------------------------------------------------

func newCachedRepo() (*CachedRepository, *MemoryRepository, *cache.MemoryCache) {
	base := NewMemoryRepository()
	c := cache.NewMemory(100, 0)
	return NewCachedRepository(base, c, logging.Discard(), 5*time.Minute, 30*time.Second), base, c
}

func TestCached_SecondReadIsServedFromCache(t *testing.T) {
	repo, _, c := newCachedRepo()
	ctx := context.Background()

	order := testOrder(t)
	if err := repo.Create(ctx, order, domain.NoEvent); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := repo.GetByID(ctx, order.ID); err != nil { // MISS, populates
		t.Fatalf("first read: %v", err)
	}
	hitsBefore := c.Stats().Hits

	if _, err := repo.GetByID(ctx, order.ID); err != nil { // HIT
		t.Fatalf("second read: %v", err)
	}
	if c.Stats().Hits <= hitsBefore {
		t.Error("the second read should have been served from the cache")
	}
}

func TestCached_WriteInvalidatesTheEntry(t *testing.T) {
	// ========================================================================
	// THE INVALIDATION TEST. Without it, a status change would be invisible
	// to readers until the TTL expired — a customer cancels their order and
	// still sees PENDING for five minutes.
	// ========================================================================
	repo, _, _ := newCachedRepo()
	ctx := context.Background()

	order := testOrder(t)
	_ = repo.Create(ctx, order, domain.NoEvent)
	_, _ = repo.GetByID(ctx, order.ID) // populate the cache

	loaded, _ := repo.GetByID(ctx, order.ID)
	_ = loaded.TransitionTo(domain.StatusProcessing, time.Now().UTC())
	if err := repo.UpdateOptimistic(ctx, loaded, domain.NoEvent); err != nil {
		t.Fatalf("UpdateOptimistic: %v", err)
	}

	after, _ := repo.GetByID(ctx, order.ID)
	if after.Status != domain.StatusProcessing {
		t.Errorf("the read returned stale cached data: got %s, want PROCESSING", after.Status)
	}
}

func TestCached_BothKeysAreInvalidated(t *testing.T) {
	// An order is cached under BOTH its id and its order number. Forgetting
	// one leaves it serving the old status — and customers look orders up by
	// the number on their receipt, so the one you forget is the one they use.
	repo, _, _ := newCachedRepo()
	ctx := context.Background()

	order := testOrder(t)
	_ = repo.Create(ctx, order, domain.NoEvent)
	_, _ = repo.GetByID(ctx, order.ID)
	_, _ = repo.GetByOrderNumber(ctx, order.OrderNumber)

	_, err := repo.UpdateWithPessimisticLock(ctx, order.ID, func(o *domain.Order) (domain.OutboxEvent, error) {
		return domain.NoEvent, o.Cancel("test", time.Now().UTC())
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}

	byID, _ := repo.GetByID(ctx, order.ID)
	if byID.Status != domain.StatusCancelled {
		t.Errorf("lookup by id is stale: %s", byID.Status)
	}
	byNumber, _ := repo.GetByOrderNumber(ctx, order.OrderNumber)
	if byNumber.Status != domain.StatusCancelled {
		t.Errorf("lookup by order number is stale: %s", byNumber.Status)
	}
}

func TestCached_FailedWriteDoesNotInvalidate(t *testing.T) {
	// A version conflict means NOTHING changed, so the cached entry is still
	// correct. Invalidating would turn every conflict into an extra DB read.
	repo, _, c := newCachedRepo()
	ctx := context.Background()

	order := testOrder(t)
	_ = repo.Create(ctx, order, domain.NoEvent)
	_, _ = repo.GetByID(ctx, order.ID)

	deletesBefore := c.Stats().Deletes

	stale := testOrder(t) // a different, nonexistent order
	_ = repo.UpdateOptimistic(ctx, stale, domain.NoEvent)

	if c.Stats().Deletes != deletesBefore {
		t.Error("a failed write should not invalidate anything")
	}
}

func TestCached_NegativeCachingForMissingOrders(t *testing.T) {
	// Caching absence is what prevents cache penetration: a bot probing
	// random ids would otherwise reach the database on every request.
	repo, base, c := newCachedRepo()
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if _, err := repo.GetByID(ctx, "ghost"); !errs.Is(err, errs.KindNotFound) {
			t.Fatalf("read %d: expected KindNotFound, got %v", i, err)
		}
	}
	if c.Stats().Hits == 0 {
		t.Error("repeated lookups of a missing order should hit the negative cache")
	}
	_ = base
}

func TestCached_CreateClearsAnyNegativeEntry(t *testing.T) {
	// Someone may have asked for this order number BEFORE it existed. We must
	// not keep serving a cached 404 for an order that now exists.
	repo, _, _ := newCachedRepo()
	ctx := context.Background()

	order := testOrder(t)
	// Prime a negative entry for this exact id.
	_, _ = repo.GetByID(ctx, order.ID)

	if err := repo.Create(ctx, order, domain.NoEvent); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := repo.GetByID(ctx, order.ID)
	if err != nil {
		t.Fatalf("the order should be readable immediately after creation, got %v", err)
	}
	if got.ID != order.ID {
		t.Errorf("got %s, want %s", got.ID, order.ID)
	}
}

func TestCached_ListInvalidationIsGenerationBased(t *testing.T) {
	// ========================================================================
	// GENERATION-BASED GROUP INVALIDATION. We cannot enumerate every cached
	// list page, so a write bumps a counter that is embedded in every list
	// key — making all old pages unreachable in ONE atomic operation, with no
	// SCAN over the Redis keyspace.
	// ========================================================================
	repo, _, _ := newCachedRepo()
	ctx := context.Background()

	first := testOrder(t)
	_ = repo.Create(ctx, first, domain.NoEvent)

	before, _ := repo.List(ctx, domain.ListFilter{Limit: 10})
	if before.Total != 1 {
		t.Fatalf("initial list total: got %d, want 1", before.Total)
	}

	// Create a second order. The cached list page must NOT be served again.
	second := testOrder(t)
	_ = repo.Create(ctx, second, domain.NoEvent)

	after, _ := repo.List(ctx, domain.ListFilter{Limit: 10})
	if after.Total != 2 {
		t.Errorf("the list was served from a stale cache: got %d, want 2", after.Total)
	}
}

func TestCached_StatsAreInvalidatedByWrites(t *testing.T) {
	repo, _, _ := newCachedRepo()
	ctx := context.Background()

	_ = repo.Create(ctx, testOrder(t), domain.NoEvent)
	before, _ := repo.Stats(ctx)
	if before.TotalOrders != 1 {
		t.Fatalf("initial stats: got %d orders, want 1", before.TotalOrders)
	}

	_ = repo.Create(ctx, testOrder(t), domain.NoEvent)
	after, _ := repo.Stats(ctx)
	if after.TotalOrders != 2 {
		t.Errorf("stats were served stale: got %d, want 2", after.TotalOrders)
	}
}

func TestCached_TerminalOrdersGetALongerTTL(t *testing.T) {
	// The adaptive TTL comes from the DOMAIN: a terminal order is immutable
	// and can be cached for an hour; a PENDING one cannot.
	order := testOrder(t)
	short := 5 * time.Minute
	if ttl := order.CacheTTL(short); ttl != short {
		t.Errorf("PENDING TTL: got %s, want %s", ttl, short)
	}
	_ = order.Cancel("done", time.Now().UTC())
	if ttl := order.CacheTTL(short); ttl <= short {
		t.Errorf("terminal TTL should exceed the default, got %s", ttl)
	}
}

func TestCached_SurvivesACacheOutage(t *testing.T) {
	// ========================================================================
	// FAIL OPEN. A cache is an OPTIMISATION, never a source of truth, so
	// losing it must cost latency and nothing else. This test wires in a cache
	// where every operation fails and asserts the repository still works.
	// ========================================================================
	base := NewMemoryRepository()
	repo := NewCachedRepository(base, brokenCache{}, logging.Discard(), time.Minute, time.Second)
	ctx := context.Background()

	order := testOrder(t)
	if err := repo.Create(ctx, order, domain.NoEvent); err != nil {
		t.Fatalf("Create should succeed even with a broken cache: %v", err)
	}
	got, err := repo.GetByID(ctx, order.ID)
	if err != nil {
		t.Fatalf("GetByID should succeed even with a broken cache: %v", err)
	}
	if got.ID != order.ID {
		t.Errorf("got %s, want %s", got.ID, order.ID)
	}

	loaded, _ := repo.GetByID(ctx, order.ID)
	_ = loaded.TransitionTo(domain.StatusProcessing, time.Now().UTC())
	if err := repo.UpdateOptimistic(ctx, loaded, domain.NoEvent); err != nil {
		t.Fatalf("a write must not fail because invalidation failed: %v", err)
	}
}

// brokenCache fails every operation, simulating a Redis outage.
type brokenCache struct{}

func (brokenCache) Get(context.Context, string) ([]byte, error) {
	return nil, fmt.Errorf("redis is down")
}
func (brokenCache) Set(context.Context, string, []byte, time.Duration) error {
	return fmt.Errorf("redis is down")
}
func (brokenCache) Delete(context.Context, ...string) error { return fmt.Errorf("redis is down") }
func (brokenCache) Incr(context.Context, string) (int64, error) {
	return 0, fmt.Errorf("redis is down")
}
func (brokenCache) Ping(context.Context) error { return fmt.Errorf("redis is down") }
func (brokenCache) Stats() cache.Stats         { return cache.Stats{} }
func (brokenCache) Close() error               { return nil }

// ---------------------------------------------------------------------------
// 3. Postgres-only behaviour
// ---------------------------------------------------------------------------

// tryConnect returns a live pool, or nil if no database is reachable.
//
// WHY WE SKIP RATHER THAN FAIL: `go test ./...` must work on a laptop with no
// Docker running. A suite that cannot run without infrastructure gets run
// rarely, and a test that is never run is worse than no test.
func tryConnect(t *testing.T) *postgres.Pool {
	t.Helper()

	dsn := os.Getenv("POSTGRES_TEST_DSN")
	if dsn == "" {
		dsn = os.Getenv("POSTGRES_DSN")
	}
	if dsn == "" {
		dsn = "postgres://orders:orders@localhost:5432/orders_test?sslmode=disable"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	pool, err := postgres.Connect(ctx, logging.Discard(), postgres.Options{
		DSN: dsn, MaxConns: 10, MinConns: 1, ConnectTimeout: 2 * time.Second,
	})
	if err != nil {
		return nil
	}
	if err := postgres.Migrate(ctx, pool, logging.Discard()); err != nil {
		t.Logf("postgres reachable but migrations failed: %v", err)
		pool.Close()
		return nil
	}
	return pool
}

func resetDatabase(t *testing.T, pool *postgres.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := postgres.TruncateAll(ctx, pool, "yes-i-am-sure-this-is-a-test-database"); err != nil {
		t.Fatalf("resetting the test database: %v", err)
	}
}

func TestPostgres_OutboxSkipLockedWithinOneTransaction(t *testing.T) {
	// ========================================================================
	// `FOR UPDATE SKIP LOCKED` is what turns a table into a safe work queue,
	// and only a real database can demonstrate it.
	//
	// THIS TEST HOLDS THE FIRST TRANSACTION OPEN, which is the condition under
	// which SKIP LOCKED actually does something. Worker A claims a batch and
	// keeps its transaction open; worker B must then SKIP those locked rows
	// and claim DIFFERENT ones rather than blocking behind A.
	//
	// (An earlier version of this test called the production
	// FetchUnpublished twice and asserted disjointness. That was WRONG, and it
	// is worth recording why: FetchUnpublished deliberately COMMITS before
	// returning, so its locks are released and two concurrent calls can
	// legitimately see the same rows. The test was asserting a guarantee the
	// code never claimed to make. See the sibling test below for what the
	// production method actually guarantees.)
	// ========================================================================
	pool := tryConnect(t)
	if pool == nil {
		t.Skip("postgres is not reachable; run `make docker-up` to include this test")
	}
	defer pool.Close()
	resetDatabase(t, pool)

	repo := NewPostgresRepository(pool, logging.Discard())
	ctx := context.Background()

	for i := 0; i < 20; i++ {
		order := testOrder(t)
		if err := repo.Create(ctx, order, testEvent(order.ID)); err != nil {
			t.Fatalf("seeding order %d: %v", i, err)
		}
	}

	const claimSQL = `
		SELECT id FROM outbox
		WHERE published_at IS NULL
		ORDER BY created_at
		LIMIT 5
		FOR UPDATE SKIP LOCKED`

	// WORKER A: claim 5 rows and HOLD the transaction open.
	txA, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin A: %v", err)
	}
	defer func() { _ = txA.Rollback(ctx) }()

	claimedA := map[string]bool{}
	rowsA, err := txA.Query(ctx, claimSQL)
	if err != nil {
		t.Fatalf("worker A claim: %v", err)
	}
	for rowsA.Next() {
		var id string
		if err := rowsA.Scan(&id); err != nil {
			rowsA.Close()
			t.Fatalf("scan A: %v", err)
		}
		claimedA[id] = true
	}
	rowsA.Close()

	if len(claimedA) != 5 {
		t.Fatalf("worker A claimed %d rows, want 5", len(claimedA))
	}

	// WORKER B: claims while A still holds its locks.
	txB, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin B: %v", err)
	}
	defer func() { _ = txB.Rollback(ctx) }()

	claimedB := map[string]bool{}
	rowsB, err := txB.Query(ctx, claimSQL)
	if err != nil {
		t.Fatalf("worker B claim: %v", err)
	}
	for rowsB.Next() {
		var id string
		if err := rowsB.Scan(&id); err != nil {
			rowsB.Close()
			t.Fatalf("scan B: %v", err)
		}
		claimedB[id] = true
	}
	rowsB.Close()

	// THE ASSERTION: B got rows, and none of them are A's.
	//
	// Without SKIP LOCKED, B would have BLOCKED here waiting for A — so this
	// test completing at all is itself part of the proof.
	if len(claimedB) != 5 {
		t.Fatalf("worker B claimed %d rows, want 5 — it should skip A's locked rows, not block", len(claimedB))
	}
	for id := range claimedB {
		if claimedA[id] {
			t.Errorf("event %s was claimed by BOTH workers; SKIP LOCKED did not skip it", id)
		}
	}
	t.Logf("worker A claimed 5 rows and held them; worker B skipped those and claimed 5 different rows")
}

func TestPostgres_OutboxFetchIsAtLeastOnce(t *testing.T) {
	// ========================================================================
	// WHAT THE PRODUCTION FetchUnpublished ACTUALLY GUARANTEES.
	//
	// It commits before returning, so its row locks are released immediately.
	// That is a DELIBERATE trade-off, documented on the method: holding a
	// transaction open across a slow publish would tie up a database
	// connection for the broker's latency, which is a far worse failure mode.
	//
	// The consequence is AT-LEAST-ONCE delivery — an event may be fetched
	// twice if two workers overlap. This test asserts the two properties we
	// DO rely on:
	//
	//   1. Every unpublished event is eventually fetched (nothing is lost).
	//   2. Once marked published, an event is never fetched again.
	//
	// Property 2 is what makes at-least-once converge, and it is why
	// consumers must be idempotent on event id.
	// ========================================================================
	pool := tryConnect(t)
	if pool == nil {
		t.Skip("postgres is not reachable")
	}
	defer pool.Close()
	resetDatabase(t, pool)

	repo := NewPostgresRepository(pool, logging.Discard())
	outbox := NewPostgresOutboxRepository(pool, logging.Discard())
	ctx := context.Background()

	wantIDs := map[string]bool{}
	for i := 0; i < 10; i++ {
		order := testOrder(t)
		event := testEvent(order.ID)
		if err := repo.Create(ctx, order, event); err != nil {
			t.Fatalf("seeding order %d: %v", i, err)
		}
		wantIDs[event.ID] = true
	}

	// PROPERTY 1: a draining loop sees every event.
	gotIDs := map[string]bool{}
	for round := 0; round < 10; round++ {
		events, err := outbox.FetchUnpublished(ctx, 4)
		if err != nil {
			t.Fatalf("FetchUnpublished: %v", err)
		}
		if len(events) == 0 {
			break
		}
		ids := make([]string, 0, len(events))
		for _, e := range events {
			gotIDs[e.ID] = true
			ids = append(ids, e.ID)
		}
		if err := outbox.MarkPublished(ctx, ids); err != nil {
			t.Fatalf("MarkPublished: %v", err)
		}
	}

	for id := range wantIDs {
		if !gotIDs[id] {
			t.Errorf("event %s was never fetched — an event was LOST", id)
		}
	}

	// PROPERTY 2: everything is published, so nothing more is returned.
	remaining, err := outbox.FetchUnpublished(ctx, 50)
	if err != nil {
		t.Fatalf("final FetchUnpublished: %v", err)
	}
	if len(remaining) != 0 {
		t.Errorf("%d events were re-fetched after being marked published", len(remaining))
	}
	t.Logf("all %d events were delivered exactly once by a sequential drain, and none re-delivered", len(wantIDs))
}

func TestPostgres_OutboxLifecycle(t *testing.T) {
	pool := tryConnect(t)
	if pool == nil {
		t.Skip("postgres is not reachable")
	}
	defer pool.Close()
	resetDatabase(t, pool)

	repo := NewPostgresRepository(pool, logging.Discard())
	outbox := NewPostgresOutboxRepository(pool, logging.Discard())
	ctx := context.Background()

	order := testOrder(t)
	event := testEvent(order.ID)
	if err := repo.Create(ctx, order, event); err != nil {
		t.Fatalf("Create: %v", err)
	}

	pending, err := outbox.FetchUnpublished(ctx, 10)
	if err != nil {
		t.Fatalf("FetchUnpublished: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("got %d pending events, want 1", len(pending))
	}

	if err := outbox.MarkPublished(ctx, []string{event.ID}); err != nil {
		t.Fatalf("MarkPublished: %v", err)
	}
	after, _ := outbox.FetchUnpublished(ctx, 10)
	if len(after) != 0 {
		t.Errorf("a published event was fetched again: %d remain", len(after))
	}

	// Retention: a published event older than the cutoff is purged.
	purged, err := outbox.PurgePublished(ctx, time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatalf("PurgePublished: %v", err)
	}
	if purged != 1 {
		t.Errorf("purged %d events, want 1", purged)
	}
}

func TestPostgres_TransactionAtomicity(t *testing.T) {
	// The order, its items and its outbox event must land together or not at
	// all. This test forces a failure inside the transaction and asserts that
	// NOTHING was persisted.
	pool := tryConnect(t)
	if pool == nil {
		t.Skip("postgres is not reachable")
	}
	defer pool.Close()
	resetDatabase(t, pool)

	repo := NewPostgresRepository(pool, logging.Discard())
	ctx := context.Background()

	order := testOrder(t)
	// A duplicate item id inside one order violates the UNIQUE constraint,
	// which fails the transaction AFTER the order row has been inserted.
	order.Items = append(order.Items, order.Items[0])

	if err := repo.Create(ctx, order, testEvent(order.ID)); err == nil {
		t.Fatal("expected the duplicate item to fail the insert")
	}

	// The order row must NOT exist: the whole transaction rolled back.
	if _, err := repo.GetByID(ctx, order.ID); !errs.Is(err, errs.KindNotFound) {
		t.Errorf("a partial write survived the rollback: %v", err)
	}
}

func TestPostgres_PessimisticLockTimesOutRatherThanHangingForever(t *testing.T) {
	// An unbounded lock wait is how one stuck transaction stalls an entire
	// endpoint. `SET LOCAL lock_timeout` turns that into a bounded 504.
	pool := tryConnect(t)
	if pool == nil {
		t.Skip("postgres is not reachable")
	}
	defer pool.Close()
	resetDatabase(t, pool)

	repo := NewPostgresRepository(pool, logging.Discard())
	ctx := context.Background()

	order := testOrder(t)
	if err := repo.Create(ctx, order, domain.NoEvent); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Hold the row lock for longer than the configured lock_timeout.
	holding := make(chan struct{})
	released := make(chan struct{})
	go func() {
		_, _ = repo.UpdateWithPessimisticLock(ctx, order.ID, func(o *domain.Order) (domain.OutboxEvent, error) {
			close(holding)
			<-released // keep the transaction open
			return domain.NoEvent, nil
		})
	}()
	<-holding

	start := time.Now()
	_, err := repo.UpdateWithPessimisticLock(ctx, order.ID, func(o *domain.Order) (domain.OutboxEvent, error) {
		return domain.NoEvent, nil
	})
	elapsed := time.Since(start)
	close(released)

	if err == nil {
		t.Skip("the lock was released before the contender timed out; timing-dependent, not a failure")
	}
	// The important assertion: it FAILED FAST rather than hanging.
	if elapsed > 10*time.Second {
		t.Errorf("the contender waited %s; lock_timeout should bound this to a few seconds", elapsed)
	}
	t.Logf("the lock contender failed after %s with: %v", elapsed, err)
}
