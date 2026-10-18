// Command legacyd is the "OLD" ORDER SYSTEM — the one being strangled.
//
// It exists so the Strangler Fig pattern can be demonstrated against
// something real: it holds historical orders that exist NOWHERE ELSE, in an
// awkward legacy format, with no idempotency support and deliberate latency.
// See internal/legacy for the list of quirks and why each one matters.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/chahat/order-processing-system/internal/legacy"
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
	cfg, err := config.Load(constants.ServiceLegacy, ":8083")
	if err != nil {
		return err
	}
	log := logging.New(cfg.ServiceName, cfg.LogLevel, cfg.LogFormat)
	mtx := metrics.NewRegistry()

	rootCtx := logging.WithLogger(context.Background(), log)
	checker := health.NewChecker(cfg.ServiceName, version)

	store := legacy.NewStore(log)
	log.Info("starting legacy order service",
		slog.String("version", version),
		slog.String("addr", cfg.HTTPAddr),
		slog.Int("historical_orders", store.Count()),
		slog.String("note", "these orders exist only here, which is what makes the strangler read-fallback demonstrable"))

	// Simulated latency, so the calling adapter's timeouts and circuit
	// breaker are exercised by something real rather than only by unit tests.
	latency := 120 * time.Millisecond

	mux := http.NewServeMux()
	legacy.NewHandler(store, log, latency).Register(mux)
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
	serverCfg := httpx.DefaultServerConfig(cfg.HTTPAddr)
	serverCfg.ShutdownGrace = cfg.ShutdownGracePeriod
	return httpx.RunServer(rootCtx, log, serverCfg, handler, func(context.Context) error {
		checker.SetReady(false)
		return nil
	})
}
