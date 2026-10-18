// Command orderd is the ORDER SERVICE.
//
// ============================================================================
// THIS FILE IS THE "COMPOSITION ROOT" — and it is the single best place to
// start reading the codebase.
//
// WHAT A COMPOSITION ROOT IS: the one place where abstractions are bound to
// concrete implementations. Nothing below this file knows whether the cache is
// Redis or a map, or whether the repository is Postgres or in-memory. They
// receive an interface and use it.
//
// WHY ALL THE WIRING LIVES IN ONE PLACE:
//   - You can read the ENTIRE dependency graph of the system in one screen.
//   - Swapping any component is a one-line change HERE, with zero risk to
//     business logic.
//   - There is no dependency-injection framework, no reflection, no magic
//     container. Dependencies are passed as arguments, which means the
//     compiler verifies the whole graph. (This is the "poor man's DI" that
//     Go culture prefers, and for good reason: a missing dependency is a
//     compile error, not a 3am runtime panic.)
//
// READ THE main() FUNCTION TOP TO BOTTOM AND YOU WILL UNDERSTAND THE SYSTEM'S
// ARCHITECTURE. Everything else is detail.
// ============================================================================
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/chahat/order-processing-system/internal/order/domain"
	"github.com/chahat/order-processing-system/internal/order/repository"
	"github.com/chahat/order-processing-system/internal/order/service"
	"github.com/chahat/order-processing-system/internal/order/transport"
	"github.com/chahat/order-processing-system/internal/order/workers"
	"github.com/chahat/order-processing-system/internal/platform/cache"
	"github.com/chahat/order-processing-system/internal/platform/config"
	"github.com/chahat/order-processing-system/internal/platform/constants"
	"github.com/chahat/order-processing-system/internal/platform/health"
	"github.com/chahat/order-processing-system/internal/platform/httpx"
	"github.com/chahat/order-processing-system/internal/platform/logging"
	"github.com/chahat/order-processing-system/internal/platform/metrics"
	"github.com/chahat/order-processing-system/internal/platform/postgres"
)

// version is stamped at build time with -ldflags "-X main.version=...".
// Knowing EXACTLY which build is running is the first question in any
// incident, and /readyz reports it.
var version = "dev"

func main() {
	// main() does nothing but call run() and handle its error.
	//
	// WHY: os.Exit skips every deferred function. If main contained the logic,
	// `defer pool.Close()` would never run on the error path. Keeping main
	// trivial means every defer in run() is honoured before we exit.
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// ---------------------------------------------------------------
	// 1. CONFIGURATION — loaded and validated first, before anything else.
	//    A bad config must fail HERE, loudly, not later under load.
	// ---------------------------------------------------------------
	cfg, err := config.Load(constants.ServiceOrder, ":8080")
	if err != nil {
		return err
	}

	// ---------------------------------------------------------------
	// 2. OBSERVABILITY — a logger and a metrics registry, built early so that
	//    every subsequent step can report what it is doing.
	// ---------------------------------------------------------------
	log := logging.New(cfg.ServiceName, cfg.LogLevel, cfg.LogFormat)
	mtx := metrics.NewRegistry()

	log.Info("starting order service",
		slog.String("version", version),
		slog.String("env", cfg.Env),
		slog.String("addr", cfg.HTTPAddr),
		// Logging the effective strategy means you never have to guess which
		// mode a running instance is in.
		slog.String("lock_strategy", string(cfg.Locking.Strategy)),
		slog.String("strangler_write_mode", string(cfg.Strangler.WriteMode)),
		slog.Bool("postgres", cfg.Postgres.Enabled),
		slog.Bool("redis", cfg.Redis.Enabled),
		slog.Bool("saga", cfg.Saga.Enabled))

	rootCtx := logging.WithLogger(context.Background(), log)
	checker := health.NewChecker(cfg.ServiceName, version)

	// ---------------------------------------------------------------
	// 3. CACHE — Redis if configured, else an in-process LRU, else a no-op.
	//
	//    THE SWAP IS THREE LINES. Nothing downstream changes, because
	//    everything downstream depends on the cache.Cache INTERFACE.
	// ---------------------------------------------------------------
	var cacheImpl cache.Cache
	switch {
	case !cfg.Cache.Enabled:
		// The Null Object pattern: caching "off" needs no if-statements
		// anywhere else in the system.
		cacheImpl = cache.NoopCache{}
		log.Warn("caching is DISABLED; every read will hit the database")
	case cfg.Redis.Enabled:
		rc := cache.NewRedis(cache.DefaultRedisOptions(cfg.Redis.Addr, cfg.Redis.Password, cfg.Redis.DB))
		// We PROBE Redis at boot but DO NOT fail if it is down.
		//
		// WHY: the cache is an optimisation. Refusing to start without it
		// would mean a Redis outage takes the whole order system offline — a
		// strictly worse outcome than running slower. This is the same
		// "fail open" reasoning as in the Get path.
		probeCtx, cancel := context.WithTimeout(rootCtx, 3*time.Second)
		if err := rc.Ping(probeCtx); err != nil {
			log.Warn("redis is not reachable at startup; continuing in degraded (uncached) mode",
				slog.String("addr", cfg.Redis.Addr), slog.String("error", err.Error()))
		} else {
			log.Info("redis connected", slog.String("addr", cfg.Redis.Addr))
		}
		cancel()
		cacheImpl = rc
		// NOT CRITICAL: a readiness failure here reports "degraded" and keeps
		// us in the load balancer. See the Critical flag's comment in health.go.
		checker.Register(health.Check{Name: "redis", Critical: false, Probe: rc.Ping})
	default:
		cacheImpl = cache.NewMemory(cfg.Cache.MaxEntries, time.Minute)
		log.Warn("using the in-process cache; with multiple replicas each one caches independently and invalidation does NOT propagate",
			slog.Int("max_entries", cfg.Cache.MaxEntries))
	}
	defer func() { _ = cacheImpl.Close() }()

	// ---------------------------------------------------------------
	// 4. REPOSITORY — Postgres or in-memory, then WRAPPED with the cache
	//    decorator.
	//
	//    NOTE THE LAYERING, which is the whole Decorator story in four lines:
	//        CachedRepository -> PostgresRepository -> database
	//    The service receives one interface and is unaware of either layer.
	// ---------------------------------------------------------------
	var (
		baseRepo   domain.OrderRepository
		outboxRepo domain.OutboxRepository
		locker     workers.Locker = workers.NoopLocker{}
	)

	if cfg.Postgres.Enabled {
		pool, err := postgres.Connect(rootCtx, log, postgres.Options{
			DSN:             cfg.Postgres.DSN,
			MaxConns:        cfg.Postgres.MaxConns,
			MinConns:        cfg.Postgres.MinConns,
			MaxConnLifetime: cfg.Postgres.MaxConnLifetime,
		})
		if err != nil {
			// Postgres, unlike Redis, IS fatal. It is the source of truth:
			// without it we cannot serve a single correct response, so
			// starting up would only produce 500s and confuse everyone.
			return fmt.Errorf("the database is required but unavailable: %w", err)
		}
		defer pool.Close()

		if err := postgres.Migrate(rootCtx, pool, log); err != nil {
			return fmt.Errorf("database migration failed: %w", err)
		}

		baseRepo = repository.NewPostgresRepository(pool, log)
		outboxRepo = repository.NewPostgresOutboxRepository(pool, log)
		// Postgres doubles as our distributed lock manager for the workers.
		locker = pool
		checker.Register(health.Check{Name: "postgres", Critical: true, Probe: pool.Ping})
		log.Info("using the postgres repository")
	} else {
		memRepo := repository.NewMemoryRepository()
		baseRepo = memRepo
		outboxRepo = memRepo
		log.Warn("using the IN-MEMORY repository; ALL DATA IS LOST ON RESTART",
			slog.String("hint", "set POSTGRES_ENABLED=true for durable storage"))
		if cfg.IsProd() {
			// A guard rail: an in-memory database in production is never
			// intentional, and failing to start is far kinder than silently
			// losing every order.
			return fmt.Errorf("refusing to start in prod with POSTGRES_ENABLED=false")
		}
	}

	// THE DECORATOR IS APPLIED HERE. One line adds cache-aside reads,
	// write-invalidation, negative caching and stampede protection to
	// whatever repository we just chose.
	repo := domain.OrderRepository(repository.NewCachedRepository(
		baseRepo, cacheImpl, log, cfg.Cache.TTL, cfg.Cache.NegativeTTL))

	// ---------------------------------------------------------------
	// 5. SAGA PARTICIPANTS — real HTTP clients, or in-process stubs.
	// ---------------------------------------------------------------
	clock := domain.SystemClock{}

	var saga *service.SagaOrchestrator
	if cfg.Saga.Enabled {
		var inventoryPort domain.InventoryPort
		var paymentPort domain.PaymentPort

		if cfg.Deps.InventoryURL != "" && cfg.Deps.PaymentURL != "" {
			inventoryPort = service.NewHTTPInventoryAdapter(cfg.Deps.InventoryURL, cfg.Deps.Timeout, log)
			paymentPort = service.NewHTTPPaymentAdapter(cfg.Deps.PaymentURL, cfg.Deps.Timeout, log)
			log.Info("saga participants configured over HTTP",
				slog.String("inventory", cfg.Deps.InventoryURL),
				slog.String("payment", cfg.Deps.PaymentURL))
		} else {
			// In-process stubs let the order service run alone. The saga code
			// under test is IDENTICAL either way — only the adapter differs.
			inventoryPort = service.NewStubInventory(log)
			paymentPort = service.NewStubPayment(log)
			log.Warn("saga participants are in-process stubs (no inventory/payment URLs configured)")
		}
		saga = service.NewSagaOrchestrator(repo, inventoryPort, paymentPort, clock, log, cfg.Saga, mtx)
	} else {
		log.Warn("sagas are DISABLED; orders will be created as PENDING with no stock reservation or payment")
	}

	// ---------------------------------------------------------------
	// 6. SERVICE LAYER
	// ---------------------------------------------------------------
	orderSvc := service.NewOrderService(repo, clock, log, cfg.Locking, mtx, saga)

	// ---------------------------------------------------------------
	// 7. STRANGLER FIG FACADE — the migration router.
	// ---------------------------------------------------------------
	var strangler *service.StranglerFacade
	if cfg.Strangler.Enabled && cfg.Deps.LegacyURL != "" {
		legacyAdapter := service.NewHTTPLegacyAdapter(cfg.Deps.LegacyURL, cfg.Deps.Timeout, log)
		strangler = service.NewStranglerFacade(repo, legacyAdapter, cfg.Strangler, log, mtx)
		log.Info("strangler fig facade enabled",
			slog.String("legacy_url", cfg.Deps.LegacyURL),
			slog.Bool("read_fallback", cfg.Strangler.ReadFallbackToLegacy),
			slog.Bool("backfill_on_read", cfg.Strangler.BackfillOnRead),
			slog.String("write_mode", string(cfg.Strangler.WriteMode)))
	}

	// ---------------------------------------------------------------
	// 8. BACKGROUND WORKERS
	// ---------------------------------------------------------------
	// A cancellable context so shutdown stops the workers before we close
	// the database pool they are using.
	workerCtx, stopWorkers := context.WithCancel(rootCtx)
	defer stopWorkers()

	publisher := workers.NewLogPublisher(log)

	if cfg.Worker.Enabled {
		advanceWorker := workers.NewAutoAdvanceWorker(orderSvc, locker, cfg.Worker, log, mtx)
		outboxWorker := workers.NewOutboxWorker(outboxRepo, publisher, locker, cfg.Worker, log, mtx)
		go advanceWorker.Run(workerCtx)
		go outboxWorker.Run(workerCtx)
	} else {
		log.Warn("background workers are DISABLED; pending orders will not auto-advance and outbox events will not publish")
	}

	// ---------------------------------------------------------------
	// 9. HTTP ROUTING
	// ---------------------------------------------------------------
	mux := http.NewServeMux()

	orderHandler := transport.NewHandler(orderSvc, strangler, log)
	orderHandler.Register(mux)

	// Operational endpoints, deliberately OUTSIDE /api/v1 and outside the
	// request-timeout middleware (a probe must answer even when the service
	// is struggling — that is the whole point of a probe).
	mux.HandleFunc("GET "+constants.PathHealthz, checker.LivenessHandler())
	mux.HandleFunc("GET "+constants.PathReadyz, checker.ReadinessHandler())
	mux.HandleFunc("GET "+constants.PathMetrics, func(w http.ResponseWriter, r *http.Request) {
		// Report cache effectiveness on every scrape, so hit ratio is a graph
		// rather than a thing you have to go and measure.
		if cr, ok := repo.(*repository.CachedRepository); ok {
			st := cr.CacheStats()
			mtx.SetGauge("cache_hits_total", "Cache hits", float64(st.Hits))
			mtx.SetGauge("cache_misses_total", "Cache misses", float64(st.Misses))
			mtx.SetGauge("cache_evictions_total", "Cache evictions (capacity)", float64(st.Evictions))
			mtx.SetGauge("cache_expired_total", "Cache entries expired (TTL)", float64(st.Expired))
			mtx.SetGauge("cache_hit_ratio", "Cache hit ratio 0..1", st.HitRatio())
			mtx.SetGauge("cache_entries", "Entries currently cached", float64(st.Entries))
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(mtx.Render()))
	})

	// API documentation, served from the binary's own working directory.
	registerDocs(mux, log)

	// ---------------------------------------------------------------
	// 10. MIDDLEWARE — the order here is deliberate and worth reading.
	// ---------------------------------------------------------------
	handler := httpx.Chain(mux,
		// Recover is OUTERMOST so it catches a panic from ANY inner
		// middleware, not just from the handler. If it were inner, a panic in
		// the metrics middleware would kill the process.
		httpx.Recover(log),
		// NormalizeErrors sits high in the chain so it also catches the 404 and
		// 405 responses the ROUTER generates before any handler of ours runs.
		httpx.NormalizeErrors(),
		// RequestID next, so every later layer (including the logger) has an
		// id to attach.
		httpx.RequestID(),
		httpx.Logger(log),
		httpx.Metrics(mtx),
		// Timeout INNERMOST, so the logger still records the request that
		// timed out. Outside the logger, a timeout would produce no log line
		// at all — and an unlogged timeout is an invisible outage.
		httpx.Timeout(20*time.Second),
	)

	// ---------------------------------------------------------------
	// 11. OPEN THE READINESS GATE, then serve.
	//
	//     We flip this only now: migrations have run, workers are started,
	//     and dependencies are probed. Until this line /readyz returns 503
	//     and the load balancer correctly withholds traffic.
	// ---------------------------------------------------------------
	checker.SetReady(true)
	log.Info("order service is ready", slog.String("docs", "http://localhost"+cfg.HTTPAddr+constants.PathDocs))

	serverCfg := httpx.DefaultServerConfig(cfg.HTTPAddr)
	serverCfg.ShutdownGrace = cfg.ShutdownGracePeriod

	return httpx.RunServer(rootCtx, log, serverCfg, handler,
		// Shutdown hooks run in REVERSE order. Note the sequencing: we mark
		// ourselves NOT ready first (so the load balancer stops sending new
		// work), and only then stop the workers.
		func(ctx context.Context) error {
			checker.SetReady(false)
			return nil
		},
		func(ctx context.Context) error {
			log.Info("stopping background workers")
			stopWorkers()
			// A brief pause so a worker mid-batch can finish before the
			// database pool closes underneath it. Without it you get a flurry
			// of "connection closed" errors in your shutdown logs that look
			// like a crash but are purely self-inflicted.
			time.Sleep(200 * time.Millisecond)
			return nil
		},
	)
}

// registerDocs serves the OpenAPI spec and a Swagger UI page.
//
// We load Swagger UI from a CDN rather than vendoring it: ~2MB of third-party
// JavaScript in the repository would dwarf our own code and make the project
// harder to read, for a documentation convenience. The trade-off (docs need
// internet access) is explicit and acceptable — the spec file itself is local
// and complete.
func registerDocs(mux *http.ServeMux, log *slog.Logger) {
	mux.HandleFunc("GET "+constants.PathOpenAPI, func(w http.ResponseWriter, r *http.Request) {
		spec, err := os.ReadFile("docs/openapi.yaml")
		if err != nil {
			http.Error(w, "the OpenAPI spec was not found (run the service from the repository root)", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write(spec)
	})

	mux.HandleFunc("GET "+constants.PathDocs, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(swaggerUIPage))
	})
}

const swaggerUIPage = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Order Processing API</title>
  <link rel="stylesheet" href="https://cdnjs.cloudflare.com/ajax/libs/swagger-ui/5.17.14/swagger-ui.min.css">
  <style>body{margin:0}</style>
</head>
<body>
  <div id="swagger-ui"></div>
  <script src="https://cdnjs.cloudflare.com/ajax/libs/swagger-ui/5.17.14/swagger-ui-bundle.min.js"></script>
  <script>
    window.onload = function () {
      SwaggerUIBundle({ url: '/openapi.yaml', dom_id: '#swagger-ui', deepLinking: true });
    };
  </script>
</body>
</html>`
