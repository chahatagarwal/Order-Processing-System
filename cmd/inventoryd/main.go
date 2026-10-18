// Command inventoryd is the INVENTORY SERVICE.
//
// It is a SAGA PARTICIPANT: it exposes one forward action (reserve) and its
// compensating action (release). Its locking strategy is deliberately the
// OPPOSITE of the order service's — see internal/inventory for why a
// contended stock counter wants pessimistic locking while an order row does
// not. That contrast is the point of running it as a separate service.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/chahat/order-processing-system/internal/inventory"
	"github.com/chahat/order-processing-system/internal/platform/config"
	"github.com/chahat/order-processing-system/internal/platform/constants"
	"github.com/chahat/order-processing-system/internal/platform/health"
	"github.com/chahat/order-processing-system/internal/platform/httpx"
	"github.com/chahat/order-processing-system/internal/platform/logging"
	"github.com/chahat/order-processing-system/internal/platform/metrics"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(constants.ServiceInventory, ":8081")
	if err != nil {
		return err
	}
	log := logging.New(cfg.ServiceName, cfg.LogLevel, cfg.LogFormat)
	mtx := metrics.NewRegistry()
	log.Info("starting inventory service", slog.String("version", version), slog.String("addr", cfg.HTTPAddr))

	rootCtx := logging.WithLogger(context.Background(), log)
	checker := health.NewChecker(cfg.ServiceName, version)

	store := inventory.NewStore(log, mtx)

	// The expiry worker is the backstop for the saga's "empty compensation"
	// problem: it returns stock from reservations whose order never confirmed
	// or compensated. Without it, every crashed saga would permanently remove
	// units from sellable inventory.
	workerCtx, stopWorkers := context.WithCancel(rootCtx)
	defer stopWorkers()
	go inventory.RunExpiryWorker(workerCtx, store, log)

	mux := http.NewServeMux()
	inventory.NewHandler(store, log).Register(mux)
	mux.HandleFunc("GET "+constants.PathHealthz, checker.LivenessHandler())
	mux.HandleFunc("GET "+constants.PathReadyz, checker.ReadinessHandler())
	mux.HandleFunc("GET "+constants.PathMetrics, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(mtx.Render()))
	})

	handler := httpx.Chain(mux,
		httpx.Recover(log),
		// NormalizeErrors sits high in the chain so it also catches the 404 and
		// 405 responses the ROUTER generates before any handler of ours runs.
		httpx.NormalizeErrors(),
		httpx.RequestID(),
		httpx.Logger(log),
		httpx.Metrics(mtx),
		httpx.Timeout(10*time.Second),
	)

	checker.SetReady(true)
	log.Info("inventory service is ready")

	serverCfg := httpx.DefaultServerConfig(cfg.HTTPAddr)
	serverCfg.ShutdownGrace = cfg.ShutdownGracePeriod
	return httpx.RunServer(rootCtx, log, serverCfg, handler, func(context.Context) error {
		checker.SetReady(false)
		stopWorkers()
		return nil
	})
}
