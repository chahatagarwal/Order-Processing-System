// Package logging gives every service one consistent, structured logger.
//
// WHY STRUCTURED LOGGING (key=value) AND NOT fmt.Printf:
// A line like "order 7 failed" is unsearchable at scale. A line like
//
//	level=ERROR msg="order transition rejected" order_id=7 from=SHIPPED to=PENDING request_id=abc
//
// can be filtered, counted and alerted on. Every log line in this system
// carries a request_id, so one customer complaint = one query.
//
// We use log/slog from the Go standard library — no third-party logging
// dependency to audit, upgrade or argue about.
package logging

import (
	"context"
	"log/slog"
	"os"
	"strings"
)

// contextKey is a private type so that nobody outside this package can
// accidentally collide with our context keys. This is the standard Go idiom.
type contextKey string

const (
	ctxKeyRequestID contextKey = "request_id"
	ctxKeyLogger    contextKey = "logger"
)

// New builds the root logger for a service.
//
// format "json" is for production (machine-readable, ships to Datadog/Loki).
// format "text" is for your terminal (human-readable while you read the code).
func New(serviceName, level, format string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: parseLevel(level)}

	var handler slog.Handler
	if strings.EqualFold(format, "json") {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		handler = slog.NewTextHandler(os.Stdout, opts)
	}

	// Every single line from this service is tagged with its name. In a
	// multi-service log stream that is the difference between usable and not.
	return slog.New(handler).With(slog.String("service", serviceName))
}

func parseLevel(level string) slog.Level {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// ---------------------------------------------------------------------------
// Carrying the logger through the call stack via context
// ---------------------------------------------------------------------------
//
// The alternative — passing *slog.Logger as a parameter to every function —
// pollutes every signature. The alternative-alternative — a global logger —
// means you cannot attach per-request fields. Context is the Go answer.

// WithLogger stores a logger in the context.
func WithLogger(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKeyLogger, l)
}

// FromContext retrieves the request-scoped logger. It NEVER returns nil —
// a logger that panics is worse than no logs, so we degrade to the default.
func FromContext(ctx context.Context) *slog.Logger {
	if ctx != nil {
		if l, ok := ctx.Value(ctxKeyLogger).(*slog.Logger); ok && l != nil {
			return l
		}
	}
	return slog.Default()
}

// WithRequestID tags the context (and its logger) with a request id.
func WithRequestID(ctx context.Context, id string) context.Context {
	ctx = context.WithValue(ctx, ctxKeyRequestID, id)
	return WithLogger(ctx, FromContext(ctx).With(slog.String("request_id", id)))
}

// RequestIDFromContext returns the request id, or "" if unset.
func RequestIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if id, ok := ctx.Value(ctxKeyRequestID).(string); ok {
		return id
	}
	return ""
}

// Discard returns a logger that writes nothing. Tests use it to keep output
// clean while still exercising all the logging code paths.
func Discard() *slog.Logger {
	return slog.New(slog.NewTextHandler(discardWriter{}, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
