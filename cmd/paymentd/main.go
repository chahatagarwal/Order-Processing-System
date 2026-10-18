// Command paymentd is the PAYMENT SERVICE.
//
// It is the saga's most consequential participant, because its action moves
// real money and cannot be rolled back — only compensated with a refund.
// Everything about it is organised around ONE guarantee: an idempotency key
// means a charge happens exactly once, no matter how many times the caller
// retries. See internal/payment for the full reasoning.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/chahat/order-processing-system/internal/payment"
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
	cfg, err := config.Load(constants.ServicePayment, ":8082")
	if err != nil {
		return err
	}
	log := logging.New(cfg.ServiceName, cfg.LogLevel, cfg.LogFormat)
	mtx := metrics.NewRegistry()
	log.Info("starting payment service", slog.String("version", version), slog.String("addr", cfg.HTTPAddr))

	rootCtx := logging.WithLogger(context.Background(), log)
	checker := health.NewChecker(cfg.ServiceName, version)

	store := payment.NewStore(log, mtx)

	mux := http.NewServeMux()
	payment.NewHandler(store, log).Register(mux)
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
	log.Info("payment service is ready",
		slog.String("note", "any amount whose cents end in 66 is declined, so saga compensation can be demonstrated deterministically"))

	serverCfg := httpx.DefaultServerConfig(cfg.HTTPAddr)
	serverCfg.ShutdownGrace = cfg.ShutdownGracePeriod
	return httpx.RunServer(rootCtx, log, serverCfg, handler, func(context.Context) error {
		checker.SetReady(false)
		return nil
	})
}
