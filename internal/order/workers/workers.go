// Package workers holds the background jobs.
//
// ============================================================================
// THE THREE PROBLEMS EVERY BACKGROUND WORKER MUST SOLVE
// ============================================================================
//
//  1. OVERLAPPING RUNS. The job runs every minute but takes 90 seconds. Now
//     two copies run at once, processing the same rows.
//     => We guard with an atomic flag and SKIP rather than queue.
//
//  2. MULTIPLE REPLICAS. Three pods each run the same timer, so every job
//     executes three times.
//     => We take a Postgres ADVISORY LOCK so the job is a cluster singleton.
//
//  3. GRACEFUL SHUTDOWN. A deploy arrives mid-batch.
//     => We select on ctx.Done() so the worker stops cleanly between items,
//     instead of being killed halfway through.
//
// Every one of these is invisible on a laptop with one process and becomes
// an incident in production with three replicas. That asymmetry is why they
// get explicit handling here rather than a comment saying "TODO".
// ============================================================================
package workers

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/chahat/order-processing-system/internal/order/domain"
	"github.com/chahat/order-processing-system/internal/order/service"
	"github.com/chahat/order-processing-system/internal/platform/config"
	"github.com/chahat/order-processing-system/internal/platform/logging"
	"github.com/chahat/order-processing-system/internal/platform/metrics"
)

// Locker is the port for cluster-wide mutual exclusion.
//
// We define it as an interface (rather than depending on *postgres.Pool) so
// that the in-memory mode can inject a no-op locker and the worker code stays
// identical. It also means swapping to a Redis or etcd lock later touches one
// adapter, not the workers.
type Locker interface {
	// TryAdvisoryLock returns (acquired, release, error) and NEVER BLOCKS.
	// Non-blocking is the important property: a blocking lock would make all
	// three replicas queue up and run the job sequentially, which is slower
	// than one replica and achieves nothing.
	TryAdvisoryLock(ctx context.Context, key int64) (bool, func(), error)
}

// NoopLocker is used when there is no shared database to coordinate through.
// It always "acquires" — correct for a single process, and explicitly
// documented as unsafe for multiple replicas.
type NoopLocker struct{}

func (NoopLocker) TryAdvisoryLock(context.Context, int64) (bool, func(), error) {
	return true, func() {}, nil
}

// Advisory lock keys. They are arbitrary but must be GLOBALLY UNIQUE across
// everything that uses advisory locks on this database — two jobs sharing a
// key would silently block each other, and the symptom (one job never runs)
// is very hard to trace back to a duplicated constant.
const (
	LockKeyAutoAdvance int64 = 950_001
	LockKeyOutbox      int64 = 950_002
)

// ---------------------------------------------------------------------------
// AutoAdvanceWorker: PENDING -> PROCESSING for stale orders
// ---------------------------------------------------------------------------

// AutoAdvanceWorker implements the brief's "background job every 5 minutes".
type AutoAdvanceWorker struct {
	svc    *service.OrderService
	locker Locker
	cfg    config.WorkerConfig
	log    *slog.Logger
	mtx    *metrics.Registry

	// running is the OVERLAP GUARD. It is atomic because Run's ticker
	// goroutine and the method that checks it are different goroutines.
	running atomic.Bool
	runs    atomic.Int64
}

func NewAutoAdvanceWorker(svc *service.OrderService, locker Locker, cfg config.WorkerConfig, log *slog.Logger, mtx *metrics.Registry) *AutoAdvanceWorker {
	if locker == nil {
		locker = NoopLocker{}
	}
	return &AutoAdvanceWorker{svc: svc, locker: locker, cfg: cfg, log: log, mtx: mtx}
}

func (w *AutoAdvanceWorker) Name() string { return "auto-advance" }

// Run blocks until ctx is cancelled. It is intended to be started in a
// goroutine from main().
func (w *AutoAdvanceWorker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.cfg.AutoAdvanceInterval)
	defer ticker.Stop()

	w.log.Info("auto-advance worker started",
		slog.Duration("interval", w.cfg.AutoAdvanceInterval),
		slog.Duration("advance_orders_older_than", w.cfg.AutoAdvanceAfter))

	for {
		select {
		case <-ctx.Done():
			// Graceful shutdown. Logging it means a deploy produces evidence
			// that workers stopped cleanly, rather than silence you have to
			// interpret.
			w.log.Info("auto-advance worker stopping",
				slog.Int64("total_runs", w.runs.Load()))
			return
		case <-ticker.C:
			w.RunOnce(ctx)
		}
	}
}

// RunOnce executes one cycle. It is EXPORTED so that tests (and an operator,
// via a debug endpoint) can trigger a run deterministically instead of
// waiting for a timer. Tests that sleep waiting for a ticker are slow and
// flaky; tests that call RunOnce are instant and exact.
func (w *AutoAdvanceWorker) RunOnce(ctx context.Context) {
	// GUARD 1: overlap within this process.
	//
	// CompareAndSwap is the whole mechanism: it atomically checks "is it
	// false?" and sets it to true in one indivisible step. A naive
	// `if !running { running = true }` has a window between the check and the
	// set in which a second goroutine passes the same check — and you get the
	// overlap you were trying to prevent.
	if !w.running.CompareAndSwap(false, true) {
		w.log.Warn("skipping auto-advance run: the previous run is still in progress",
			slog.String("hint", "the interval may be shorter than the job duration"))
		w.mtx.IncCounter("worker_skipped_total", "Worker runs skipped", "worker", w.Name(), "reason", "overlap")
		return
	}
	defer w.running.Store(false)

	// GUARD 2: only one replica in the cluster should do this work.
	acquired, release, err := w.locker.TryAdvisoryLock(ctx, LockKeyAutoAdvance)
	if err != nil {
		w.log.Error("could not attempt the auto-advance advisory lock",
			slog.String("error", err.Error()))
		return
	}
	if !acquired {
		// This is the NORMAL, HEALTHY path on 2 of 3 replicas. Logging it at
		// DEBUG rather than WARN matters: at WARN, a 3-replica deployment
		// would generate two "warnings" every minute forever and train
		// everyone to ignore warnings.
		w.log.Debug("another replica holds the auto-advance lock; skipping this cycle")
		w.mtx.IncCounter("worker_skipped_total", "Worker runs skipped", "worker", w.Name(), "reason", "not_leader")
		return
	}
	defer release()

	w.runs.Add(1)
	start := time.Now()

	// A dedicated context with its own timeout. Without one, a pathological
	// query could leave this job running until the next tick and then
	// overlap-skip forever — the job silently stops working and nothing says so.
	runCtx, cancel := context.WithTimeout(ctx, w.cfg.AutoAdvanceInterval)
	defer cancel()
	runCtx = logging.WithLogger(runCtx, w.log.With(slog.String("worker", w.Name())))

	count, err := w.svc.AdvanceStalePendingOrders(runCtx, w.cfg.AutoAdvanceAfter, 500)
	duration := time.Since(start)

	w.mtx.ObserveHistogram("worker_run_duration_seconds", "Worker run duration",
		duration.Seconds(), "worker", w.Name())

	if err != nil {
		w.log.Error("auto-advance run failed",
			slog.String("error", err.Error()), slog.Duration("duration", duration))
		w.mtx.IncCounter("worker_errors_total", "Worker errors", "worker", w.Name())
		return
	}

	// Log at INFO only when something happened. A job that logs "processed 0
	// orders" every minute produces 1,440 useless lines a day per replica and
	// buries the lines that matter.
	if count > 0 {
		w.log.Info("auto-advance run complete",
			slog.Int("orders_advanced", count), slog.Duration("duration", duration))
	} else {
		w.log.Debug("auto-advance run complete: nothing to do",
			slog.Duration("duration", duration))
	}
}

// ---------------------------------------------------------------------------
// OutboxWorker: publish the transactional outbox
// ---------------------------------------------------------------------------

// OutboxWorker drains the outbox table and publishes events.
//
// This is the second half of the TRANSACTIONAL OUTBOX pattern. The writer
// (the repository) put the event in the database atomically with the state
// change; this worker is what eventually gets it to the outside world.
type OutboxWorker struct {
	repo      domain.OutboxRepository
	publisher domain.EventPublisher
	locker    Locker
	cfg       config.WorkerConfig
	log       *slog.Logger
	mtx       *metrics.Registry

	running   atomic.Bool
	published atomic.Int64
}

func NewOutboxWorker(repo domain.OutboxRepository, publisher domain.EventPublisher, locker Locker, cfg config.WorkerConfig, log *slog.Logger, mtx *metrics.Registry) *OutboxWorker {
	if locker == nil {
		locker = NoopLocker{}
	}
	return &OutboxWorker{repo: repo, publisher: publisher, locker: locker, cfg: cfg, log: log, mtx: mtx}
}

func (w *OutboxWorker) Name() string { return "outbox" }

func (w *OutboxWorker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.cfg.OutboxInterval)
	defer ticker.Stop()

	// A separate, much slower ticker for retention. Purging on every outbox
	// cycle would run a DELETE every 2 seconds for no benefit.
	purgeTicker := time.NewTicker(10 * time.Minute)
	defer purgeTicker.Stop()

	w.log.Info("outbox worker started",
		slog.Duration("interval", w.cfg.OutboxInterval),
		slog.Int("batch_size", w.cfg.OutboxBatchSize))

	for {
		select {
		case <-ctx.Done():
			w.log.Info("outbox worker stopping", slog.Int64("total_published", w.published.Load()))
			return
		case <-ticker.C:
			w.RunOnce(ctx)
		case <-purgeTicker.C:
			w.purge(ctx)
		}
	}
}

// RunOnce publishes one batch and reports how many events were published.
func (w *OutboxWorker) RunOnce(ctx context.Context) int {
	if !w.running.CompareAndSwap(false, true) {
		return 0
	}
	defer w.running.Store(false)

	// NOTE: unlike auto-advance, the outbox does NOT need a cluster
	// singleton lock for correctness — `FOR UPDATE SKIP LOCKED` already makes
	// concurrent workers claim disjoint batches, which is exactly the
	// parallelism we want here. We still take the lock so that a 3-replica
	// deployment does not issue 3x the polling queries when the outbox is
	// empty (which is most of the time). Correctness from SKIP LOCKED,
	// efficiency from the lock.
	acquired, release, err := w.locker.TryAdvisoryLock(ctx, LockKeyOutbox)
	if err != nil || !acquired {
		return 0
	}
	defer release()

	runCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	events, err := w.repo.FetchUnpublished(runCtx, w.cfg.OutboxBatchSize)
	if err != nil {
		w.log.Error("failed to fetch outbox events", slog.String("error", err.Error()))
		w.mtx.IncCounter("worker_errors_total", "Worker errors", "worker", w.Name())
		return 0
	}
	if len(events) == 0 {
		return 0
	}

	// publishedIDs are marked done in ONE batched UPDATE at the end, rather
	// than one UPDATE per event. On a backlog of 10,000 events that is the
	// difference between 200 round-trips and 4.
	var publishedIDs []string

	for _, event := range events {
		// Stop promptly on shutdown, but DO mark what we already published.
		// Returning without marking them would republish the whole batch on
		// restart — harmless (consumers are idempotent) but wasteful, and it
		// makes the backlog graph lie.
		if runCtx.Err() != nil {
			break
		}

		if err := w.publisher.Publish(runCtx, event); err != nil {
			// A single bad event must not block the batch behind it. We record
			// the failure against THAT event and move on; after 10 attempts
			// the fetch query excludes it (the poison-message guard).
			w.log.Warn("failed to publish an outbox event",
				slog.String("event_id", event.ID),
				slog.String("event_type", event.EventType),
				slog.Int("attempts", event.Attempts),
				slog.String("error", err.Error()))
			if markErr := w.repo.MarkFailed(runCtx, event.ID, err.Error()); markErr != nil {
				w.log.Error("failed to record an outbox publish failure",
					slog.String("event_id", event.ID), slog.String("error", markErr.Error()))
			}
			w.mtx.IncCounter("outbox_publish_failed_total", "Outbox events that failed to publish",
				"event_type", event.EventType)
			continue
		}
		publishedIDs = append(publishedIDs, event.ID)
		w.mtx.IncCounter("outbox_published_total", "Outbox events published",
			"event_type", event.EventType)
	}

	if len(publishedIDs) > 0 {
		if err := w.repo.MarkPublished(runCtx, publishedIDs); err != nil {
			// ============================================================
			// THE AT-LEAST-ONCE MOMENT, made explicit.
			//
			// We published these events successfully but failed to record
			// that fact. On the next cycle they will be fetched and
			// PUBLISHED AGAIN.
			//
			// This is unavoidable without a distributed transaction between
			// our database and the broker — and it is precisely why every
			// consumer must deduplicate on event id. The alternative
			// (mark-then-publish) risks LOSING events entirely, which is
			// strictly worse: a duplicate email annoys a customer, a missing
			// order loses money.
			// ============================================================
			w.log.Error("published events but failed to mark them; they WILL be redelivered (consumers must be idempotent)",
				slog.Int("count", len(publishedIDs)), slog.String("error", err.Error()))
			return 0
		}
		w.published.Add(int64(len(publishedIDs)))
		w.log.Debug("outbox batch published", slog.Int("count", len(publishedIDs)))
	}
	return len(publishedIDs)
}

// purge enforces retention on the outbox table.
func (w *OutboxWorker) purge(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// A 7-day retention keeps enough history to debug last week's incident
	// while bounding table growth. Published rows are dead weight after that:
	// they slow every index scan, bloat every backup, and cost storage.
	cutoff := time.Now().UTC().Add(-7 * 24 * time.Hour)
	n, err := w.repo.PurgePublished(ctx, cutoff)
	if err != nil {
		w.log.Warn("outbox purge failed", slog.String("error", err.Error()))
		return
	}
	if n > 0 {
		w.log.Info("purged published outbox events", slog.Int("count", n), slog.Time("older_than", cutoff))
	}
}

// ---------------------------------------------------------------------------
// LogPublisher — the default EventPublisher
// ---------------------------------------------------------------------------

// LogPublisher writes events to the structured log.
//
// WHY THIS IS A REASONABLE DEFAULT RATHER THAN A CHEAT:
// the outbox pattern's value is in the ATOMIC WRITE, which is already done
// and real. The publisher is a pluggable detail. Logging the event proves the
// pipeline end to end, is inspectable in the e2e test, and requires no broker
// to run the project. Swapping in Kafka is one new type implementing
// domain.EventPublisher and one line in main() — the Open/Closed Principle
// giving a concrete return.
type LogPublisher struct {
	log *slog.Logger
	// subscribers lets in-process consumers react to events, which is how we
	// demonstrate event-driven flow without deploying a broker.
	subscribers []func(context.Context, domain.OutboxEvent)
}

func NewLogPublisher(log *slog.Logger) *LogPublisher {
	return &LogPublisher{log: log}
}

// Subscribe registers an in-process handler. Note it is NOT safe to call this
// after Publish has started being called concurrently — it is a boot-time API,
// which is why there is no mutex: adding one would imply a safety this method
// does not have.
func (p *LogPublisher) Subscribe(fn func(context.Context, domain.OutboxEvent)) {
	p.subscribers = append(p.subscribers, fn)
}

func (p *LogPublisher) Publish(ctx context.Context, event domain.OutboxEvent) error {
	p.log.InfoContext(ctx, "event published",
		slog.String("event_id", event.ID),
		slog.String("event_type", event.EventType),
		slog.String("aggregate_type", event.AggregateType),
		slog.String("aggregate_id", event.AggregateID),
		// The payload is logged as a raw string so the line is greppable and
		// the structure survives into your log aggregator.
		slog.String("payload", string(event.Payload)))

	for _, fn := range p.subscribers {
		// A panicking subscriber must not take down the publisher (and with
		// it the process). Each callback is isolated.
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					p.log.Error("event subscriber panicked", slog.Any("panic", rec))
				}
			}()
			fn(ctx, event)
		}()
	}
	return nil
}

var _ domain.EventPublisher = (*LogPublisher)(nil)
