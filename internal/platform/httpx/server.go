package httpx

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/chahat/order-processing-system/internal/platform/constants"
)

// ServerConfig holds the timeouts that keep a Go HTTP server safe.
type ServerConfig struct {
	Addr         string
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	IdleTimeout  time.Duration
	// ShutdownGrace: how long we wait for in-flight requests to finish before
	// forcing exit.
	ShutdownGrace time.Duration
}

func DefaultServerConfig(addr string) ServerConfig {
	return ServerConfig{
		Addr:          addr,
		ReadTimeout:   constants.DefaultHTTPReadTimeout,
		WriteTimeout:  constants.DefaultHTTPWriteTimeout,
		IdleTimeout:   constants.DefaultHTTPIdleTimeout,
		ShutdownGrace: constants.DefaultShutdownGracePeriod,
	}
}

// RunServer starts an HTTP server and blocks until the process is asked to
// stop, then shuts down GRACEFULLY.
//
// ============================================================================
// WHY GRACEFUL SHUTDOWN IS A CORRECTNESS FEATURE, NOT A NICETY
// ============================================================================
// When you deploy, Kubernetes sends SIGTERM. If the process exits instantly:
//   - Every in-flight request dies mid-response. A customer who just clicked
//     "Place order" gets a connection reset and does not know whether they
//     were charged.
//   - A saga halfway through its steps is abandoned with stock reserved and
//     no order — which is exactly the inconsistency sagas exist to avoid.
//
// With graceful shutdown we:
//  1. Stop ACCEPTING new connections (the load balancer notices readyz
//     failing and routes elsewhere).
//  2. Let in-flight requests FINISH, up to the grace period.
//  3. Run cleanup hooks (close DB pool, flush the outbox, close Redis).
//
// The grace period must be LONGER than your slowest normal request and
// SHORTER than Kubernetes' terminationGracePeriodSeconds (default 30s), or
// the kernel SIGKILLs you mid-cleanup and you are back where you started.
// ============================================================================
func RunServer(ctx context.Context, log *slog.Logger, cfg ServerConfig, handler http.Handler, onShutdown ...func(context.Context) error) error {
	// NotifyContext cancels ctx on SIGINT (Ctrl-C) or SIGTERM (orchestrator).
	// Handling BOTH matters: SIGINT is your terminal, SIGTERM is production.
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{
		Addr:         cfg.Addr,
		Handler:      handler,
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
		IdleTimeout:  cfg.IdleTimeout,
		// ReadHeaderTimeout specifically defends against Slowloris: an
		// attacker opens thousands of connections and dribbles headers one
		// byte at a time, holding a goroutine each, until you are out of
		// memory. Go's default is NO timeout, so this line is load-bearing.
		ReadHeaderTimeout: 5 * time.Second,
		// BaseContext ties every request's context to the server lifecycle.
		BaseContext: func(net.Listener) context.Context { return context.Background() },
	}

	// errCh is buffered so that if ListenAndServe fails AFTER we have already
	// returned, the sending goroutine does not block forever and leak.
	errCh := make(chan error, 1)
	go func() {
		log.Info("http server listening", slog.String("addr", cfg.Addr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			// ErrServerClosed is the EXPECTED result of a clean Shutdown, so
			// treating it as a failure would make every deploy look like a crash.
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		// Could not bind the port (already in use, permission denied).
		return err
	case <-ctx.Done():
		log.Info("shutdown signal received, draining in-flight requests",
			slog.Duration("grace", cfg.ShutdownGrace))
	}

	// A FRESH context for shutdown. Reusing the cancelled ctx would make
	// Shutdown return immediately and defeat the entire purpose — an easy
	// mistake that silently removes your graceful shutdown.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownGrace)
	defer cancel()

	shutdownErr := srv.Shutdown(shutdownCtx)
	if shutdownErr != nil {
		log.Error("graceful shutdown exceeded grace period; some requests were cut off",
			slog.String("error", shutdownErr.Error()))
	}

	// Run cleanup hooks in REVERSE registration order (last opened, first
	// closed) — the same discipline as defer, for the same reason: a resource
	// may depend on one registered before it.
	for i := len(onShutdown) - 1; i >= 0; i-- {
		if err := onShutdown[i](shutdownCtx); err != nil {
			log.Error("shutdown hook failed", slog.Int("hook", i), slog.String("error", err.Error()))
		}
	}

	log.Info("shutdown complete")
	return shutdownErr
}
