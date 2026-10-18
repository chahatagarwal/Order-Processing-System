// Package postgres owns the connection pool, migrations, transaction helper
// and Postgres-specific error translation.
//
// WHY WE CHOSE POSTGRESQL OVER MONGODB FOR THIS SYSTEM
// (the full comparison is in docs/TRADEOFFS.md; the short version:)
//
//  1. An order is a TRANSACTION. "Insert the order, insert 3 items, append an
//     outbox event" must be all-or-nothing. Postgres gives that in one BEGIN/
//     COMMIT. MongoDB has multi-document transactions, but only on a replica
//     set, and they are a bolt-on rather than the native model.
//
//  2. PESSIMISTIC LOCKING IS FIRST-CLASS: `SELECT ... FOR UPDATE` is exactly
//     the primitive we need. In MongoDB you emulate it with findAndModify
//     lease fields — which works, but you are rebuilding a database feature
//     in application code.
//
//  3. `FOR UPDATE SKIP LOCKED` turns a table into a safe work queue for N
//     concurrent workers with no extra infrastructure. This is what makes our
//     transactional outbox (see docs/SAGA.md) a dozen lines instead of a
//     Kafka deployment.
//
//  4. The data is RELATIONAL and the shape is stable. Orders have items; items
//     reference products. Schema-on-write catches bad data at the door.
//
// WHEN MONGO WOULD WIN: a product catalogue where every category has
// different attributes, or an event store you only ever append to and read by
// id. Different problem, different tool.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/chahat/order-processing-system/internal/platform/errs"
)

// Pool wraps pgxpool so the rest of the code does not import pgx directly
// outside the repository layer.
type Pool struct {
	*pgxpool.Pool
	log *slog.Logger
}

type Options struct {
	DSN             string
	MaxConns        int32
	MinConns        int32
	MaxConnLifetime time.Duration
	ConnectTimeout  time.Duration
}

// Connect opens the pool and verifies it with a ping.
//
// WHY WE PING AT BOOT: pgxpool is lazy, so a wrong password would otherwise
// surface as a 500 on the first customer request rather than as a startup
// failure in your deploy logs. Fail fast, fail visibly.
func Connect(ctx context.Context, log *slog.Logger, opts Options) (*Pool, error) {
	cfg, err := pgxpool.ParseConfig(opts.DSN)
	if err != nil {
		return nil, fmt.Errorf("postgres: invalid DSN: %w", err)
	}

	// POOL SIZING IS A REAL TRADE-OFF, not a number to leave at default:
	//   too small -> requests queue waiting for a connection, and your p99
	//                latency becomes "time spent waiting for the pool".
	//   too large -> you exhaust Postgres's max_connections (default 100),
	//                and because each backend is an OS process, hundreds of
	//                them thrash the server. More connections past the point
	//                of saturation makes throughput WORSE, not better.
	// Rule of thumb: start near (cpu_cores * 2) + effective_spindles, measure,
	// then adjust. For N replicas, remember N * MaxConns is the real total.
	if opts.MaxConns > 0 {
		cfg.MaxConns = opts.MaxConns
	}
	if opts.MinConns > 0 {
		// MinConns keeps warm connections so a traffic spike does not pay
		// connection-establishment cost on the critical path.
		cfg.MinConns = opts.MinConns
	}
	if opts.MaxConnLifetime > 0 {
		// Recycling connections prevents unbounded server-side memory growth
		// per backend, and lets a connection pick up DNS/failover changes.
		cfg.MaxConnLifetime = opts.MaxConnLifetime
	}
	cfg.MaxConnIdleTime = 5 * time.Minute
	// HealthCheckPeriod proactively discards connections killed by a network
	// device or a failover, so a request does not discover it the hard way.
	cfg.HealthCheckPeriod = 30 * time.Second

	connectTimeout := opts.ConnectTimeout
	if connectTimeout <= 0 {
		connectTimeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("postgres: failed to create pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: ping failed (is the database up and the DSN correct?): %w", err)
	}

	log.Info("postgres connected",
		slog.Int("max_conns", int(cfg.MaxConns)),
		slog.Int("min_conns", int(cfg.MinConns)))
	return &Pool{Pool: pool, log: log}, nil
}

// Ping is the probe used by /readyz.
func (p *Pool) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	return p.Pool.Ping(ctx)
}

// ---------------------------------------------------------------------------
// Transaction helper
// ---------------------------------------------------------------------------

// InTx runs fn inside a transaction, committing on success and rolling back
// on error OR panic.
//
// WHY A HELPER INSTEAD OF HAND-WRITTEN BEGIN/COMMIT EVERYWHERE:
// The hand-written version has three classic bugs, and this helper fixes all
// three for every caller at once:
//
//  1. A FORGOTTEN ROLLBACK on an early return leaks the transaction. It holds
//     its locks and its connection until the pool recycles it — and if it was
//     holding a row lock, every other request for that row blocks. One
//     forgotten rollback can stall a whole endpoint.
//  2. A PANIC skips the rollback entirely, with the same consequence.
//  3. AN IGNORED COMMIT ERROR means you report success for data that was
//     never durably written. The customer sees "order placed"; the order
//     does not exist.
func (p *Pool) InTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	return p.InTxOpts(ctx, pgx.TxOptions{}, fn)
}

// InTxOpts is InTx with an explicit isolation level.
//
// POSTGRES ISOLATION LEVELS, AND WHY WE DEFAULT TO READ COMMITTED:
//
//	READ COMMITTED (default) — each STATEMENT sees a fresh snapshot. Two
//	  statements in one transaction can see different data. Combined with
//	  explicit SELECT ... FOR UPDATE this is correct AND fast, which is why
//	  it is the right default for an OLTP order system.
//	REPEATABLE READ — the whole transaction sees one snapshot. Prevents
//	  non-repeatable reads, but a conflicting write makes COMMIT fail with a
//	  serialization error that YOU must catch and retry.
//	SERIALIZABLE — as if transactions ran one at a time. Strongest guarantee,
//	  highest abort rate under contention; every transaction needs retry logic.
//
// Our approach: READ COMMITTED + explicit row locks where an invariant
// demands it. Predictable, no surprise aborts, and the locking is visible in
// the code rather than implied by a session setting.
func (p *Pool) InTxOpts(ctx context.Context, opts pgx.TxOptions, fn func(tx pgx.Tx) error) error {
	tx, err := p.Pool.BeginTx(ctx, opts)
	if err != nil {
		return TranslateError(err)
	}

	committed := false
	defer func() {
		if committed {
			return
		}
		// A fresh context: if the request context was cancelled (client hung
		// up), a rollback on the cancelled context would ALSO fail, leaving
		// the transaction open. This is a real and frequently-missed bug.
		rbCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if rbErr := tx.Rollback(rbCtx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
			p.log.Error("transaction rollback failed", slog.String("error", rbErr.Error()))
		}
	}()

	if err := fn(tx); err != nil {
		return err // the deferred rollback handles cleanup
	}

	if err := tx.Commit(ctx); err != nil {
		return TranslateError(err)
	}
	committed = true
	return nil
}

// ---------------------------------------------------------------------------
// Advisory locks — Postgres as a distributed lock manager
// ---------------------------------------------------------------------------

// TryAdvisoryLock attempts to take a session-level advisory lock, returning
// immediately (it never blocks).
//
// WHAT AN ADVISORY LOCK IS: a named lock that means whatever YOU decide, and
// is not attached to any row. Perfect for "only one replica should run the
// nightly job".
//
// WHY WE USE IT FOR OUR BACKGROUND WORKERS:
// With 3 replicas of the order service, all 3 run the same cron loop. Without
// coordination, all 3 would process the same batch of pending orders at the
// same instant — triple the work, and triple the chance of a duplicated side
// effect. One advisory lock makes the job a singleton across the cluster.
//
// WHY NOT A REDIS LOCK (SETNX + TTL): Redis locks are not safe under network
// partitions without Redlock-style complexity, and a TTL that expires while
// the job is still running hands the lock to a second worker. A Postgres
// advisory lock is released AUTOMATICALLY when the session dies — no TTL to
// tune, no split-brain. We already depend on Postgres, so it adds nothing new.
func (p *Pool) TryAdvisoryLock(ctx context.Context, key int64) (bool, func(), error) {
	conn, err := p.Pool.Acquire(ctx)
	if err != nil {
		return false, func() {}, TranslateError(err)
	}

	var acquired bool
	// pg_try_advisory_lock returns immediately: true if taken, false if held
	// elsewhere. The blocking variant (pg_advisory_lock) would make every
	// replica queue up and run the job sequentially — exactly what we are
	// trying to avoid.
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&acquired); err != nil {
		conn.Release()
		return false, func() {}, TranslateError(err)
	}
	if !acquired {
		conn.Release()
		return false, func() {}, nil
	}

	// The release function must unlock AND return the connection. The lock is
	// tied to this specific session, so we must hold the same connection for
	// the lock's lifetime — hence acquiring from the pool rather than using
	// pool.Exec (which may use a different connection each call).
	release := func() {
		relCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := conn.Exec(relCtx, `SELECT pg_advisory_unlock($1)`, key); err != nil {
			p.log.Warn("advisory unlock failed; lock will be freed when the session ends",
				slog.Int64("key", key), slog.String("error", err.Error()))
		}
		conn.Release()
	}
	return true, release, nil
}

// ---------------------------------------------------------------------------
// Error translation
// ---------------------------------------------------------------------------

// Postgres SQLSTATE codes we care about. Matching on these numeric codes is
// mandatory — matching on English error text breaks the day someone changes
// the server locale.
const (
	sqlStateUniqueViolation      = "23505"
	sqlStateForeignKeyViolation  = "23503"
	sqlStateCheckViolation       = "23514"
	sqlStateNotNullViolation     = "23502"
	sqlStateSerializationFailure = "40001"
	sqlStateDeadlockDetected     = "40P01"
	sqlStateLockNotAvailable     = "55P03"
	sqlStateQueryCanceled        = "57014"
	sqlStateTooManyConnections   = "53300"
)

// TranslateError converts a driver error into our domain error vocabulary, so
// that the service layer never needs to know what SQLSTATE means.
func TranslateError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return errs.NotFound("not_found", "the requested record does not exist").Wrap(err)
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case sqlStateUniqueViolation:
			// The caller is trying to create something that already exists.
			// For an idempotency key, this is actually the EXPECTED outcome of
			// a duplicate request, which the service layer handles.
			return errs.Conflict("duplicate", "a record with these unique values already exists").
				WithDetail("constraint", pgErr.ConstraintName).Wrap(err)

		case sqlStateForeignKeyViolation:
			return errs.Invalid("invalid_reference", "referenced record does not exist").
				WithDetail("constraint", pgErr.ConstraintName).Wrap(err)

		case sqlStateCheckViolation, sqlStateNotNullViolation:
			// Reaching this means our application-level validation missed
			// something and the DATABASE caught it. That is defence in depth
			// working as intended — the constraint is the last line, not the
			// first.
			return errs.Invalid("constraint_violation", "the data violates a database constraint").
				WithDetail("constraint", pgErr.ConstraintName).Wrap(err)

		case sqlStateSerializationFailure, sqlStateDeadlockDetected:
			// Both are RETRYABLE and both are NORMAL under concurrency.
			// Postgres detected that committing would break isolation (or that
			// two transactions were waiting on each other) and aborted one.
			// Retrying the loser almost always succeeds.
			return errs.Concurrency("serialization_failure",
				"the record was modified concurrently; please retry").Wrap(err)

		case sqlStateLockNotAvailable:
			// Raised by SELECT ... FOR UPDATE NOWAIT when the row is locked.
			return errs.Concurrency("lock_unavailable",
				"the record is locked by another operation; please retry").Wrap(err)

		case sqlStateQueryCanceled:
			// Usually our own statement_timeout or lock_timeout firing.
			return errs.Timeout("query_timeout", "the database query timed out").Wrap(err)

		case sqlStateTooManyConnections:
			return errs.Unavailable("db_overloaded", "the database is at its connection limit").Wrap(err)
		}
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return errs.Timeout("db_timeout", "the database operation timed out").Wrap(err)
	}
	if errors.Is(err, context.Canceled) {
		// The client went away. Not our bug, and not worth paging anyone.
		return errs.Timeout("request_canceled", "the request was canceled").Wrap(err)
	}
	return errs.Internal("db_error", "a database error occurred").Wrap(err)
}
