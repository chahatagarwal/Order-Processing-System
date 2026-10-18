package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/chahat/order-processing-system/internal/order/domain"
	"github.com/chahat/order-processing-system/internal/platform/postgres"
)

// PostgresRepository is the production adapter.
//
// ============================================================================
// THIS FILE IS WHERE THE LOCKING REQUIREMENT IS ACTUALLY MET.
// Three strategies, each in its own method, each with the exact SQL visible:
//
//	Create                     -> no lock needed (the row does not exist yet)
//	UpdateOptimistic           -> UPDATE ... WHERE version = $N  (compare-and-swap)
//	UpdateWithPessimisticLock  -> SELECT ... FOR UPDATE           (real row lock)
//	AdvanceStalePending        -> set-based UPDATE + SKIP LOCKED  (work queue)
//
// The service layer chooses between them per use case, and can escalate from
// the first to the second — that is the "hybrid" strategy. See docs/LOCKING.md.
// ============================================================================
type PostgresRepository struct {
	db  *postgres.Pool
	log *slog.Logger
}

func NewPostgresRepository(db *postgres.Pool, log *slog.Logger) *PostgresRepository {
	return &PostgresRepository{db: db, log: log}
}

// orderColumns is declared once and reused by every SELECT.
//
// WHY NOT `SELECT *`:
//  1. Column ORDER would then depend on the physical table definition, so
//     adding a column silently shifts every Scan() by one position.
//  2. It fetches columns you do not need, wasting network and memory.
//  3. The query no longer documents what the code actually reads.
const orderColumns = `
	id, order_number, customer_id, customer_email, customer_name,
	status, total_amount_minor, currency, shipping_address, notes,
	version, cancel_reason, failure_reason, saga_id,
	created_at, updated_at, status_changed_at, source`

// ---------------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------------

func (r *PostgresRepository) GetByID(ctx context.Context, id string) (*domain.Order, error) {
	return r.getOneBy(ctx, "id", id)
}

func (r *PostgresRepository) GetByOrderNumber(ctx context.Context, orderNumber string) (*domain.Order, error) {
	return r.getOneBy(ctx, "order_number", strings.ToUpper(orderNumber))
}

// getOneBy fetches an order and its items.
//
// NOTE: `column` is NOT interpolated from user input — it is passed as a
// literal by the two callers above. Interpolating a client-supplied column
// name here would be a SQL injection hole that no amount of parameterising
// the VALUE can fix. Identifiers can never be parameterised, so they must
// come from an allow-list.
func (r *PostgresRepository) getOneBy(ctx context.Context, column, value string) (*domain.Order, error) {
	var o domain.Order
	var totalMinor int64
	var currency string
	var cancelReason, failureReason, sagaID *string // nullable columns -> pointers

	query := fmt.Sprintf(`SELECT %s FROM orders WHERE %s = $1`, orderColumns, column)
	row := r.db.QueryRow(ctx, query, value)

	if err := row.Scan(
		&o.ID, &o.OrderNumber, &o.CustomerID, &o.CustomerEmail, &o.CustomerName,
		&o.Status, &totalMinor, &currency, &o.ShippingAddress, &o.Notes,
		&o.Version, &cancelReason, &failureReason, &sagaID,
		&o.CreatedAt, &o.UpdatedAt, &o.StatusChangedAt, &o.Source,
	); err != nil {
		// Translate pgx.ErrNoRows into our domain's not-found error, so the
		// service layer never needs to know pgx exists.
		if err == pgx.ErrNoRows {
			return nil, domain.ErrOrderNotFound
		}
		return nil, postgres.TranslateError(err)
	}

	o.TotalAmount = domain.NewMoney(totalMinor, currency)
	o.CancelReason = deref(cancelReason)
	o.FailureReason = deref(failureReason)
	o.SagaID = deref(sagaID)

	items, err := r.loadItems(ctx, r.db, o.ID)
	if err != nil {
		return nil, err
	}
	o.Items = items
	return &o, nil
}

// rowQuerier is the smallest interface that both pgxpool.Pool and pgx.Tx
// satisfy.
//
// WHY THIS TINY INTERFACE MATTERS: without it we would need loadItems() and
// loadItemsTx() — the same SQL twice, which is exactly the duplication that
// drifts. With it, one function serves both the pooled and the transactional
// path. Small interfaces, big DRY win, and it is the Interface Segregation
// Principle at its most literal: we ask for the one method we use.
type rowQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func (r *PostgresRepository) loadItems(ctx context.Context, q rowQuerier, orderID string) ([]domain.OrderItem, error) {
	rows, err := q.Query(ctx, `
		SELECT id, order_id, product_id, product_name, product_sku,
		       quantity, unit_price_minor, line_total_minor, currency
		FROM order_items
		WHERE order_id = $1
		ORDER BY created_at, id`, orderID)
	if err != nil {
		return nil, postgres.TranslateError(err)
	}
	// rows.Close() is REQUIRED. A leaked rows object holds its connection,
	// and enough of them exhaust the pool — the service then hangs with no
	// obvious cause. defer makes it unconditional.
	defer rows.Close()

	var items []domain.OrderItem
	for rows.Next() {
		var it domain.OrderItem
		var unitMinor, lineMinor int64
		var currency string
		if err := rows.Scan(&it.ID, &it.OrderID, &it.ProductID, &it.ProductName,
			&it.ProductSKU, &it.Quantity, &unitMinor, &lineMinor, &currency); err != nil {
			return nil, postgres.TranslateError(err)
		}
		it.UnitPrice = domain.NewMoney(unitMinor, currency)
		it.LineTotal = domain.NewMoney(lineMinor, currency)
		items = append(items, it)
	}
	// rows.Err() catches a failure that happened PART-WAY THROUGH streaming.
	// Skipping this check is a silent-truncation bug: you get 3 of 5 items
	// and no error at all.
	if err := rows.Err(); err != nil {
		return nil, postgres.TranslateError(err)
	}
	return items, nil
}

func (r *PostgresRepository) List(ctx context.Context, f domain.ListFilter) (domain.ListResult, error) {
	// ------------------------------------------------------------------
	// DYNAMIC SQL, BUILT SAFELY.
	// Every user-supplied VALUE becomes a numbered placeholder ($1, $2...)
	// and is sent separately from the SQL text. The database therefore never
	// parses user input as code, which is what makes SQL injection
	// structurally impossible here — not "unlikely", impossible.
	// ------------------------------------------------------------------
	var where []string
	var args []any
	add := func(clause string, val any) {
		args = append(args, val)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}

	if f.Status != nil {
		add("status = $%d", string(*f.Status))
	}
	if f.CustomerID != "" {
		add("customer_id = $%d", f.CustomerID)
	}
	if f.CreatedAfter != nil {
		add("created_at >= $%d", *f.CreatedAfter)
	}
	if f.CreatedBefore != nil {
		add("created_at <= $%d", *f.CreatedBefore)
	}

	whereSQL := ""
	if len(where) > 0 {
		whereSQL = " WHERE " + strings.Join(where, " AND ")
	}

	// COUNT runs first so we can report Total. This is the expensive half of
	// the request on a large table — documented as a trade-off in ports.go.
	var total int
	if err := r.db.QueryRow(ctx, `SELECT COUNT(*) FROM orders`+whereSQL, args...).Scan(&total); err != nil {
		return domain.ListResult{}, postgres.TranslateError(err)
	}

	// ORDER BY comes from an ALLOW-LIST, never from the client's string.
	// Identifiers cannot be parameterised, so this mapping IS the security
	// control. `?sortBy=id; DROP TABLE orders--` maps to "created_at".
	orderBy := sortColumn(f.SortBy)
	dir := "DESC"
	if strings.EqualFold(f.SortDir, "asc") {
		dir = "ASC"
	}

	limit := f.Limit
	if limit <= 0 {
		limit = 20
	}
	args = append(args, limit, f.Offset)
	// The secondary sort on id is a TIE-BREAKER. Without it, rows sharing a
	// created_at have no defined order, and paging can show the same row on
	// two pages or skip one entirely. A one-column fix for a confusing bug.
	query := fmt.Sprintf(`SELECT %s FROM orders%s ORDER BY %s %s, id %s LIMIT $%d OFFSET $%d`,
		orderColumns, whereSQL, orderBy, dir, dir, len(args)-1, len(args))

	rows, err := r.db.Query(ctx, query, args...)
	if err != nil {
		return domain.ListResult{}, postgres.TranslateError(err)
	}
	defer rows.Close()

	var orders []domain.Order
	var ids []string
	for rows.Next() {
		var o domain.Order
		var totalMinor int64
		var currency string
		var cancelReason, failureReason, sagaID *string
		if err := rows.Scan(
			&o.ID, &o.OrderNumber, &o.CustomerID, &o.CustomerEmail, &o.CustomerName,
			&o.Status, &totalMinor, &currency, &o.ShippingAddress, &o.Notes,
			&o.Version, &cancelReason, &failureReason, &sagaID,
			&o.CreatedAt, &o.UpdatedAt, &o.StatusChangedAt, &o.Source,
		); err != nil {
			return domain.ListResult{}, postgres.TranslateError(err)
		}
		o.TotalAmount = domain.NewMoney(totalMinor, currency)
		o.CancelReason = deref(cancelReason)
		o.FailureReason = deref(failureReason)
		o.SagaID = deref(sagaID)
		orders = append(orders, o)
		ids = append(ids, o.ID)
	}
	if err := rows.Err(); err != nil {
		return domain.ListResult{}, postgres.TranslateError(err)
	}

	// ------------------------------------------------------------------
	// SOLVING THE N+1 QUERY PROBLEM.
	//
	// The naive approach loops over the 20 orders and runs one items query
	// each: 1 + 20 = 21 round-trips. At 1ms of network latency per query that
	// is 21ms of pure waiting, and it grows linearly with page size.
	//
	// Instead we fetch ALL items for ALL 20 orders in ONE query using
	// `WHERE order_id = ANY($1)`, then group them in memory. 2 queries total,
	// regardless of page size.
	//
	// N+1 is the single most common performance bug in data-access code, and
	// it is usually invisible in development (where latency is ~0) and
	// crippling in production.
	// ------------------------------------------------------------------
	if len(ids) > 0 {
		itemsByOrder, err := r.loadItemsForOrders(ctx, ids)
		if err != nil {
			return domain.ListResult{}, err
		}
		for i := range orders {
			orders[i].Items = itemsByOrder[orders[i].ID]
		}
	}

	return domain.ListResult{
		Orders:  orders,
		Total:   total,
		Limit:   limit,
		Offset:  f.Offset,
		HasMore: f.Offset+len(orders) < total,
	}, nil
}

func (r *PostgresRepository) loadItemsForOrders(ctx context.Context, orderIDs []string) (map[string][]domain.OrderItem, error) {
	rows, err := r.db.Query(ctx, `
		SELECT id, order_id, product_id, product_name, product_sku,
		       quantity, unit_price_minor, line_total_minor, currency
		FROM order_items
		WHERE order_id = ANY($1)
		ORDER BY order_id, created_at, id`, orderIDs)
	if err != nil {
		return nil, postgres.TranslateError(err)
	}
	defer rows.Close()

	out := map[string][]domain.OrderItem{}
	for rows.Next() {
		var it domain.OrderItem
		var unitMinor, lineMinor int64
		var currency string
		if err := rows.Scan(&it.ID, &it.OrderID, &it.ProductID, &it.ProductName,
			&it.ProductSKU, &it.Quantity, &unitMinor, &lineMinor, &currency); err != nil {
			return nil, postgres.TranslateError(err)
		}
		it.UnitPrice = domain.NewMoney(unitMinor, currency)
		it.LineTotal = domain.NewMoney(lineMinor, currency)
		out[it.OrderID] = append(out[it.OrderID], it)
	}
	return out, postgres.TranslateError(rows.Err())
}

// sortColumn is the ORDER BY allow-list.
func sortColumn(requested string) string {
	switch requested {
	case "updated_at":
		return "updated_at"
	case "total_amount":
		return "total_amount_minor"
	case "status":
		return "status"
	default:
		return "created_at"
	}
}

func (r *PostgresRepository) Stats(ctx context.Context) (domain.Stats, error) {
	// ONE aggregate query instead of six COUNT queries. The database is far
	// better at this than we are: it scans the index once and buckets by
	// status, where six round-trips would scan six times.
	rows, err := r.db.Query(ctx, `
		SELECT status, COUNT(*), COALESCE(SUM(total_amount_minor), 0)
		FROM orders
		GROUP BY status`)
	if err != nil {
		return domain.Stats{}, postgres.TranslateError(err)
	}
	defer rows.Close()

	stats := domain.Stats{
		CountByStatus: map[domain.Status]int{},
		GeneratedAt:   time.Now().UTC(),
	}
	for _, s := range domain.AllStatuses() {
		stats.CountByStatus[s] = 0 // always return every bucket
	}

	var revenueMinor int64
	var revenueCount int
	for rows.Next() {
		var status domain.Status
		var count int
		var sumMinor int64
		if err := rows.Scan(&status, &count, &sumMinor); err != nil {
			return domain.Stats{}, postgres.TranslateError(err)
		}
		stats.CountByStatus[status] = count
		stats.TotalOrders += count
		if status != domain.StatusCancelled && status != domain.StatusFailed {
			revenueMinor += sumMinor
			revenueCount += count
		}
	}
	if err := rows.Err(); err != nil {
		return domain.Stats{}, postgres.TranslateError(err)
	}

	stats.TotalRevenue = domain.NewMoney(revenueMinor, "USD")
	if revenueCount > 0 {
		stats.AverageOrder = domain.NewMoney(revenueMinor/int64(revenueCount), "USD")
	} else {
		stats.AverageOrder = domain.Zero("USD")
	}
	return stats, nil
}

// ---------------------------------------------------------------------------
// Create — one transaction, three tables
// ---------------------------------------------------------------------------

func (r *PostgresRepository) Create(ctx context.Context, o *domain.Order, event domain.OutboxEvent) error {
	// InTx gives us all-or-nothing across orders + order_items + outbox.
	//
	// THE GUARANTEE: if the process is killed between the two INSERTs, NOTHING
	// is persisted. You never get an order with no items, or an order with no
	// outbox event (which would mean nobody downstream ever hears about it).
	return r.db.InTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO orders (
				id, order_number, customer_id, customer_email, customer_name,
				status, total_amount_minor, currency, shipping_address, notes,
				version, saga_id, created_at, updated_at, status_changed_at, source
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`,
			o.ID, o.OrderNumber, o.CustomerID, o.CustomerEmail, o.CustomerName,
			string(o.Status), o.TotalAmount.Minor, o.TotalAmount.Currency,
			o.ShippingAddress, o.Notes, o.Version, nullable(o.SagaID),
			o.CreatedAt, o.UpdatedAt, o.StatusChangedAt, o.Source)
		if err != nil {
			return postgres.TranslateError(err)
		}

		// BATCH the item inserts. pgx.Batch pipelines all of them in a single
		// network round-trip; a loop of Exec calls would be one round-trip per
		// item (the N+1 problem again, this time on the write path).
		if len(o.Items) > 0 {
			batch := &pgx.Batch{}
			for _, it := range o.Items {
				batch.Queue(`
					INSERT INTO order_items (
						id, order_id, product_id, product_name, product_sku,
						quantity, unit_price_minor, line_total_minor, currency, created_at
					) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
					it.ID, o.ID, it.ProductID, it.ProductName, it.ProductSKU,
					it.Quantity, it.UnitPrice.Minor, it.LineTotal.Minor,
					it.UnitPrice.Currency, o.CreatedAt)
			}
			br := tx.SendBatch(ctx, batch)
			// Close() is what surfaces errors from a batch. Forgetting it
			// means a failed INSERT is silently ignored.
			if err := br.Close(); err != nil {
				return postgres.TranslateError(err)
			}
		}

		// The outbox event joins the SAME transaction. This is the entire
		// point of the pattern (see domain/ports.go for the dual-write story).
		if !event.IsEmpty() {
			if err := insertOutbox(ctx, tx, event); err != nil {
				return err
			}
		}
		return nil
	})
}

func insertOutbox(ctx context.Context, tx pgx.Tx, e domain.OutboxEvent) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO outbox (id, aggregate_type, aggregate_id, event_type, payload, created_at)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (id) DO NOTHING`,
		e.ID, e.AggregateType, e.AggregateID, e.EventType, e.Payload, e.CreatedAt)
	// ON CONFLICT DO NOTHING makes event insertion IDEMPOTENT. If a retry
	// replays the same logical operation with the same event id, we get one
	// event, not two — so consumers are not notified twice for one change.
	return postgres.TranslateError(err)
}

// ---------------------------------------------------------------------------
// STRATEGY 1: OPTIMISTIC LOCKING
// ---------------------------------------------------------------------------

// UpdateOptimistic performs a compare-and-swap on the version column.
//
// ============================================================================
// HOW IT WORKS, AND WHY THE `AND version = $N` IS THE WHOLE MECHANISM
// ============================================================================
//
//	UPDATE orders SET status = 'SHIPPED', version = version + 1
//	WHERE id = 'abc' AND version = 3;
//
// If another transaction already committed, the stored version is 4, the
// WHERE matches ZERO rows, and RowsAffected() == 0. We did not overwrite
// anything — we detected that we WOULD have, and we report a conflict.
//
// THE COST/BENEFIT:
//   - No locks are held while the service computes. Readers are never blocked.
//   - No deadlocks are possible: there is nothing to deadlock on.
//   - It works across stateless replicas with no coordination.
//   - The loser must re-read and retry. Under heavy contention on one row,
//     retries can dominate and you waste work (hence the hybrid escalation).
//
// WHEN THIS IS THE RIGHT CHOICE: the overwhelmingly common case where two
// people rarely touch the same order in the same instant.
// ============================================================================
func (r *PostgresRepository) UpdateOptimistic(ctx context.Context, o *domain.Order, event domain.OutboxEvent) error {
	return r.db.InTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE orders SET
				status = $1, total_amount_minor = $2, shipping_address = $3, notes = $4,
				cancel_reason = $5, failure_reason = $6, saga_id = $7,
				updated_at = $8, status_changed_at = $9,
				version = version + 1
			WHERE id = $10 AND version = $11`,
			string(o.Status), o.TotalAmount.Minor, o.ShippingAddress, o.Notes,
			nullable(o.CancelReason), nullable(o.FailureReason), nullable(o.SagaID),
			o.UpdatedAt, o.StatusChangedAt,
			o.ID, o.Version)
		if err != nil {
			return postgres.TranslateError(err)
		}

		if tag.RowsAffected() == 0 {
			// ZERO ROWS IS AMBIGUOUS, so we must disambiguate: either the row
			// is gone (404) or the version moved (409 conflict). Reporting
			// "conflict" for a deleted order would send the client into a
			// retry loop that can never succeed.
			var currentVersion int
			err := tx.QueryRow(ctx, `SELECT version FROM orders WHERE id = $1`, o.ID).Scan(&currentVersion)
			if err == pgx.ErrNoRows {
				return domain.ErrOrderNotFound
			}
			if err != nil {
				return postgres.TranslateError(err)
			}
			r.log.InfoContext(ctx, "optimistic lock conflict detected",
				slog.String("order_id", o.ID),
				slog.Int("our_version", o.Version),
				slog.Int("current_version", currentVersion))
			return domain.ErrVersionConflict.
				WithDetail("expected_version", o.Version).
				WithDetail("actual_version", currentVersion)
		}

		if !event.IsEmpty() {
			if err := insertOutbox(ctx, tx, event); err != nil {
				return err
			}
		}
		// Reflect the committed version back to the caller, so a second
		// update in the same request does not send a stale version.
		o.Version++
		return nil
	})
}

// ---------------------------------------------------------------------------
// STRATEGY 2: PESSIMISTIC LOCKING
// ---------------------------------------------------------------------------

// UpdateWithPessimisticLock takes a real row lock for the whole
// read-modify-write cycle.
//
// ============================================================================
// SELECT ... FOR UPDATE — WHAT IT ACTUALLY DOES
// ============================================================================
//
//	BEGIN;
//	SELECT * FROM orders WHERE id = 'abc' FOR UPDATE;  -- lock acquired here
//	-- ... any other transaction doing FOR UPDATE on this row now WAITS ...
//	UPDATE orders SET ... WHERE id = 'abc';
//	COMMIT;                                            -- lock released here
//
// Readers doing a plain SELECT are NOT blocked (Postgres uses MVCC, so they
// see the last committed version). Only other writers queue.
//
// WHY WE USE IT FOR CANCELLATION SPECIFICALLY:
// Cancelling checks an invariant ("status must still be PENDING") and then
// acts on it. With optimistic locking, a conflict means re-running the whole
// decision — and under a double-click, the two requests can ping-pong. With a
// row lock, request 2 waits, then reads status=CANCELLED and correctly
// returns "already cancelled" instead of cancelling twice and issuing two
// refunds. The lock converts a race into a queue.
//
// THE COSTS, STATED HONESTLY:
//   - It SERIALISES access to that row. Throughput on a single hot row is
//     capped at 1/lock-duration.
//   - DEADLOCK RISK if two transactions lock rows in opposite orders.
//     The defence: always acquire locks in a consistent order (we lock
//     exactly one row, which makes deadlock impossible here).
//   - A lock held across a network call is a disaster. We therefore NEVER
//     call another service while holding this lock — the saga does its
//     remote calls outside, and only takes the lock to record the result.
//
// ============================================================================
func (r *PostgresRepository) UpdateWithPessimisticLock(
	ctx context.Context,
	id string,
	mutate func(*domain.Order) (domain.OutboxEvent, error),
) (*domain.Order, error) {

	var result *domain.Order
	err := r.db.InTx(ctx, func(tx pgx.Tx) error {
		// lock_timeout is ESSENTIAL. Without it, this statement waits
		// FOREVER for the lock. One stuck transaction then piles up every
		// subsequent request, exhausts the connection pool, and takes down
		// endpoints that have nothing to do with this order. Bounded waiting
		// turns a total outage into a handful of 504s.
		if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '3s'`); err != nil {
			return postgres.TranslateError(err)
		}

		var o domain.Order
		var totalMinor int64
		var currency string
		var cancelReason, failureReason, sagaID *string

		// THE LOCK IS TAKEN HERE.
		query := fmt.Sprintf(`SELECT %s FROM orders WHERE id = $1 FOR UPDATE`, orderColumns)
		if err := tx.QueryRow(ctx, query, id).Scan(
			&o.ID, &o.OrderNumber, &o.CustomerID, &o.CustomerEmail, &o.CustomerName,
			&o.Status, &totalMinor, &currency, &o.ShippingAddress, &o.Notes,
			&o.Version, &cancelReason, &failureReason, &sagaID,
			&o.CreatedAt, &o.UpdatedAt, &o.StatusChangedAt, &o.Source,
		); err != nil {
			if err == pgx.ErrNoRows {
				return domain.ErrOrderNotFound
			}
			return postgres.TranslateError(err)
		}
		o.TotalAmount = domain.NewMoney(totalMinor, currency)
		o.CancelReason = deref(cancelReason)
		o.FailureReason = deref(failureReason)
		o.SagaID = deref(sagaID)

		items, err := r.loadItems(ctx, tx, o.ID)
		if err != nil {
			return err
		}
		o.Items = items

		// Apply the caller's business logic. We are holding the lock, so the
		// state we read cannot change underneath us — which is exactly the
		// guarantee that makes a check-then-act safe.
		event, err := mutate(&o)
		if err != nil {
			return err
		}

		// No `AND version = $N` needed here: the lock already guarantees
		// exclusivity. We still bump the version so that optimistic readers
		// elsewhere can detect the change.
		if _, err := tx.Exec(ctx, `
			UPDATE orders SET
				status = $1, cancel_reason = $2, failure_reason = $3, saga_id = $4,
				notes = $5, updated_at = $6, status_changed_at = $7,
				version = version + 1
			WHERE id = $8`,
			string(o.Status), nullable(o.CancelReason), nullable(o.FailureReason),
			nullable(o.SagaID), o.Notes, o.UpdatedAt, o.StatusChangedAt, o.ID); err != nil {
			return postgres.TranslateError(err)
		}

		if !event.IsEmpty() {
			if err := insertOutbox(ctx, tx, event); err != nil {
				return err
			}
		}

		o.Version++
		result = &o
		return nil
	})
	// The lock is released by COMMIT or ROLLBACK — both handled by InTx. There
	// is no "unlock" call to forget, which is why the port is callback-shaped.
	if err != nil {
		return nil, err
	}
	return result, nil
}

// ---------------------------------------------------------------------------
// STRATEGY 3: set-based batch update with SKIP LOCKED
// ---------------------------------------------------------------------------

// AdvanceStalePending promotes PENDING -> PROCESSING for stale orders.
//
// ============================================================================
// WHY `FOR UPDATE SKIP LOCKED` TURNS A TABLE INTO A SAFE WORK QUEUE
// ============================================================================
// Three replicas run this job on a timer. Without coordination, all three
// select the same 50 rows and all three try to update them: duplicated work,
// and lock contention between our own replicas.
//
//	FOR UPDATE            -> "lock the rows I selected"
//	FOR UPDATE SKIP LOCKED -> "...and IGNORE rows someone else has locked"
//
// So replica A claims rows 1-50, replica B instantly skips those and claims
// 51-100, replica C takes 101-150. Three disjoint batches, full parallelism,
// zero coordination service, and no row is ever processed twice.
//
// Without SKIP LOCKED, B and C would BLOCK waiting for A — you would get the
// correctness but none of the parallelism, and your job would take 3x longer
// for no reason.
//
// This single clause is why we do not need Kafka, Celery or a job runner for
// this workload. It is also what backs the outbox publisher.
// ============================================================================
func (r *PostgresRepository) AdvanceStalePending(ctx context.Context, olderThan time.Time, limit int) (int, error) {
	if limit <= 0 {
		limit = 100
	}
	var count int

	err := r.db.InTx(ctx, func(tx pgx.Tx) error {
		// A CTE does claim-and-update in ONE statement, so there is no window
		// between selecting and updating in which another worker could
		// interfere. RETURNING gives us the ids for the outbox events.
		rows, err := tx.Query(ctx, `
			WITH claimed AS (
				SELECT id FROM orders
				WHERE status = $1 AND status_changed_at <= $2
				ORDER BY status_changed_at
				LIMIT $3
				FOR UPDATE SKIP LOCKED
			)
			UPDATE orders o
			SET status = $4, status_changed_at = $5, updated_at = $5, version = o.version + 1
			FROM claimed c
			WHERE o.id = c.id
			RETURNING o.id`,
			string(domain.StatusPending), olderThan, limit,
			string(domain.StatusProcessing), time.Now().UTC())
		if err != nil {
			return postgres.TranslateError(err)
		}

		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return postgres.TranslateError(err)
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return postgres.TranslateError(err)
		}
		count = len(ids)

		// Emit one outbox event per advanced order, in the SAME transaction,
		// so the state change and its notification stay atomic.
		if len(ids) > 0 {
			now := time.Now().UTC()
			batch := &pgx.Batch{}
			for _, id := range ids {
				payload, _ := json.Marshal(map[string]any{
					"order_id":   id,
					"from":       domain.StatusPending,
					"to":         domain.StatusProcessing,
					"actor":      "auto-advance-worker",
					"changed_at": now,
				})
				// A DETERMINISTIC event id (order id + the target status)
				// makes the whole job idempotent: if it runs twice, ON
				// CONFLICT DO NOTHING collapses the duplicate events.
				eventID := fmt.Sprintf("%s:%s:auto", id, domain.StatusProcessing)
				batch.Queue(`
					INSERT INTO outbox (id, aggregate_type, aggregate_id, event_type, payload, created_at)
					VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (id) DO NOTHING`,
					eventID, "order", id, domain.EventOrderStatusChanged, payload, now)
			}
			if err := tx.SendBatch(ctx, batch).Close(); err != nil {
				return postgres.TranslateError(err)
			}
		}
		return nil
	})
	return count, err
}

func (r *PostgresRepository) Ping(ctx context.Context) error { return r.db.Ping(ctx) }

// ---------------------------------------------------------------------------
// Outbox repository
// ---------------------------------------------------------------------------

type PostgresOutboxRepository struct {
	db  *postgres.Pool
	log *slog.Logger
}

func NewPostgresOutboxRepository(db *postgres.Pool, log *slog.Logger) *PostgresOutboxRepository {
	return &PostgresOutboxRepository{db: db, log: log}
}

// FetchUnpublished claims a batch for this worker using SKIP LOCKED.
//
// IMPORTANT SUBTLETY: the rows are claimed inside a transaction that we COMMIT
// before returning. So the locks are released immediately, and two workers
// could in principle fetch the same event if their timing overlaps exactly.
//
// WHY THAT IS ACCEPTABLE: this gives AT-LEAST-ONCE delivery, which is the
// guarantee the outbox pattern promises anyway, and which our consumers
// already handle by being idempotent on EventID. The alternative — holding
// the transaction open for the entire publish — means a slow broker keeps a
// database transaction and its connection open, which is a far worse failure
// mode. Choosing at-least-once here is deliberate, not an oversight.
func (r *PostgresOutboxRepository) FetchUnpublished(ctx context.Context, limit int) ([]domain.OutboxEvent, error) {
	if limit <= 0 {
		limit = 50
	}
	var events []domain.OutboxEvent

	err := r.db.InTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, aggregate_type, aggregate_id, event_type, payload, created_at, attempts, COALESCE(last_error, '')
			FROM outbox
			WHERE published_at IS NULL
			  AND attempts < 10
			ORDER BY created_at
			LIMIT $1
			FOR UPDATE SKIP LOCKED`, limit)
		if err != nil {
			return postgres.TranslateError(err)
		}
		defer rows.Close()
		for rows.Next() {
			var e domain.OutboxEvent
			if err := rows.Scan(&e.ID, &e.AggregateType, &e.AggregateID, &e.EventType,
				&e.Payload, &e.CreatedAt, &e.Attempts, &e.LastError); err != nil {
				return postgres.TranslateError(err)
			}
			events = append(events, e)
		}
		return postgres.TranslateError(rows.Err())
	})
	// `attempts < 10` is a POISON-MESSAGE GUARD. An event that can never be
	// published (malformed payload, a deleted downstream topic) would
	// otherwise be retried forever, consuming the whole batch on every cycle
	// and starving every healthy event behind it. After 10 attempts it is
	// parked for a human — which is what a dead-letter queue is for.
	return events, err
}

func (r *PostgresOutboxRepository) MarkPublished(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	// ONE statement for the whole batch, via = ANY($1). A loop would be one
	// round-trip per event.
	_, err := r.db.Exec(ctx, `UPDATE outbox SET published_at = NOW() WHERE id = ANY($1)`, ids)
	return postgres.TranslateError(err)
}

func (r *PostgresOutboxRepository) MarkFailed(ctx context.Context, id, reason string) error {
	_, err := r.db.Exec(ctx, `
		UPDATE outbox
		SET attempts = attempts + 1, last_error = $2
		WHERE id = $1`, id, truncate(reason, 500))
	// Truncating the stored error matters: a 2MB stack trace per failed event,
	// multiplied by thousands of events, is a genuine way to fill a disk.
	return postgres.TranslateError(err)
}

func (r *PostgresOutboxRepository) PurgePublished(ctx context.Context, olderThan time.Time) (int, error) {
	tag, err := r.db.Exec(ctx, `DELETE FROM outbox WHERE published_at IS NOT NULL AND published_at < $1`, olderThan)
	if err != nil {
		return 0, postgres.TranslateError(err)
	}
	return int(tag.RowsAffected()), nil
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

// nullable converts "" to a real SQL NULL.
//
// WHY IT MATTERS: storing "" and NULL in the same column means two different
// representations of "absent", and every query then needs
// `WHERE (x IS NULL OR x = ”)`. Normalising at the boundary keeps the data
// clean and the queries simple.
func nullable(s string) *string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return &s
}

// deref converts a NULL back to "". Go has no nil string, and forcing every
// consumer to nil-check a *string would be noise in exchange for nothing.
func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

var (
	_ domain.OrderRepository  = (*PostgresRepository)(nil)
	_ domain.OutboxRepository = (*PostgresOutboxRepository)(nil)
)
