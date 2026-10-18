package httpx

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/chahat/order-processing-system/internal/platform/constants"
	"github.com/chahat/order-processing-system/internal/platform/idgen"
	"github.com/chahat/order-processing-system/internal/platform/logging"
	"github.com/chahat/order-processing-system/internal/platform/metrics"
)

// Middleware is a function that wraps a handler to add one cross-cutting
// concern. This is the DECORATOR pattern (structural, GoF) expressed in Go.
//
// WHY DECORATORS INSTEAD OF PUTTING THIS IN EVERY HANDLER:
// Logging, panic recovery, timeouts and metrics apply to ALL 12 endpoints.
// Copy-pasted into each one, they would drift and some handler would forget
// panic recovery. As middleware, each concern is written once, tested once,
// and provably applied everywhere — Single Responsibility at the HTTP layer.
type Middleware func(http.Handler) http.Handler

// Chain composes middleware so that Chain(h, A, B, C) executes A -> B -> C -> h.
//
// Note the REVERSE iteration: wrapping must be applied inside-out for the
// outermost listed middleware to run first. Getting this backwards puts your
// panic-recovery middleware INSIDE the thing that panics, which silently
// defeats it. A subtle bug worth understanding.
func Chain(h http.Handler, mws ...Middleware) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

// responseRecorder wraps http.ResponseWriter so we can observe the status
// code and byte count after the handler has run. The standard
// ResponseWriter is write-only — there is no w.Status() — so recording it
// ourselves is the only way to log or meter it.
type responseRecorder struct {
	http.ResponseWriter
	status      int
	bytes       int
	wroteHeader bool
}

func (r *responseRecorder) WriteHeader(status int) {
	if r.wroteHeader {
		// Guard against a double WriteHeader, which makes net/http log
		// "superfluous WriteHeader call" and leaves confusing noise.
		return
	}
	r.status = status
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(status)
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		// A handler that calls Write without WriteHeader implicitly sends 200.
		// We must record that, or every successful request logs status=0.
		r.status = http.StatusOK
		r.wroteHeader = true
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

// Unwrap lets Go 1.20+ http.ResponseController reach the real writer, so
// wrapping does not break flushing or hijacking (used by SSE/WebSockets).
func (r *responseRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// RequestID ensures every request has a correlation id, and that it is
// attached to the context logger AND echoed back in the response header.
//
// WHY WE HONOUR AN INBOUND HEADER: in a microservice call chain
// (gateway -> order -> inventory), reusing the caller's id lets you trace one
// customer action across four services' logs. Generating a fresh id at each
// hop throws that away.
func RequestID() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := strings.TrimSpace(r.Header.Get(constants.HeaderRequestID))
			// Bound the length: an attacker sending a 1MB header value would
			// otherwise get it written into every one of our log lines.
			if id == "" || len(id) > 128 {
				id = idgen.NewRequestID()
			}
			ctx := logging.WithRequestID(r.Context(), id)
			w.Header().Set(constants.HeaderRequestID, id)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// Logger logs one structured line per request, after it completes.
//
// WHY ONE LINE AT THE END, NOT TWO (start + end):
// Two lines doubles log volume and cost for little benefit. The completion
// line can carry everything including duration and status. We only log at
// start in DEBUG, for latency debugging.
func Logger(log *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Health probes hit every few seconds forever. Logging them would
			// bury real traffic and inflate your logging bill for zero value.
			if isProbe(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}

			start := time.Now()
			reqLog := log.With(slog.String("request_id", logging.RequestIDFromContext(r.Context())))
			ctx := logging.WithLogger(r.Context(), reqLog)
			rec := &responseRecorder{ResponseWriter: w, status: http.StatusOK}

			next.ServeHTTP(rec, r.WithContext(ctx))

			elapsed := time.Since(start)
			attrs := []any{
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", rec.status),
				slog.Int("bytes", rec.bytes),
				slog.Duration("duration", elapsed),
				slog.String("cache", w.Header().Get(constants.HeaderCacheStatus)),
			}

			// LOG LEVEL BY OUTCOME, so that "level>=warn" is a meaningful
			// alerting filter rather than noise:
			//   5xx -> ERROR (our fault, page someone)
			//   4xx -> WARN  (their fault, watch the rate)
			//   2xx -> INFO  (normal)
			switch {
			case rec.status >= 500:
				reqLog.ErrorContext(ctx, "http request", attrs...)
			case rec.status >= 400:
				reqLog.WarnContext(ctx, "http request", attrs...)
			default:
				reqLog.InfoContext(ctx, "http request", attrs...)
			}
		})
	}
}

// Recover turns a panic into a 500 instead of killing the whole server.
//
// WHY THIS IS NOT OPTIONAL: in Go, an unrecovered panic in ANY goroutine
// terminates the ENTIRE PROCESS. Without this middleware, one nil-map write
// in one rarely-used handler takes down every in-flight request for every
// customer. This is the single highest-value middleware in the stack.
func Recover(log *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					// http.ErrAbortHandler is net/http's documented way to
					// abort a response on purpose. Treating it as a crash
					// would produce fake alerts.
					if rec == http.ErrAbortHandler {
						panic(rec)
					}
					log.ErrorContext(r.Context(), "panic recovered",
						slog.Any("panic", rec),
						slog.String("path", r.URL.Path),
						slog.String("request_id", logging.RequestIDFromContext(r.Context())),
						// The stack trace is the only thing that makes the
						// panic debuggable. It goes to logs, never to the client.
						slog.String("stack", string(debug.Stack())),
					)
					JSON(w, http.StatusInternalServerError, ErrorBody{})
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// Metrics records request counts and a latency histogram.
//
// CARDINALITY WARNING (a genuine production trap): we label by the ROUTE
// PATTERN ("/api/v1/orders/{id}"), never the raw URL. Labelling by raw URL
// creates one time series per order id — millions of series, and you have
// taken down your own Prometheus. Keep label values low-cardinality.
func Metrics(reg *metrics.Registry) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isProbe(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			start := time.Now()
			rec := &responseRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)

			// r.Pattern (Go 1.23+) gives the matched route template, which is
			// exactly the low-cardinality label we want.
			route := r.Pattern
			if route == "" {
				route = "unmatched"
			}
			statusClass := statusClass(rec.status)

			reg.IncCounter("http_requests_total", "Total HTTP requests",
				"method", r.Method, "route", route, "status", statusClass)
			reg.ObserveHistogram("http_request_duration_seconds", "HTTP request latency in seconds",
				time.Since(start).Seconds(), "method", r.Method, "route", route)
		})
	}
}

// Timeout bounds how long a handler may run.
//
// WHY EVERY REQUEST NEEDS A DEADLINE: without one, a slow database query
// holds a goroutine, a connection and the client socket indefinitely. Enough
// of those and you run out of all three — a "resource leak by patience".
// The deadline propagates through context, so pgx cancels the query too.
func Timeout(d time.Duration) Middleware {
	return func(next http.Handler) http.Handler {
		return http.TimeoutHandler(next, d, `{"error":{"code":"timeout","message":"request timed out"}}`)
	}
}

// NormalizeErrors guarantees that EVERY error response is our JSON envelope.
//
// ============================================================================
// THE BUG THIS FIXES (found by a test, not by inspection)
// ============================================================================
// net/http's ServeMux answers a request it cannot route by writing PLAIN TEXT
// directly:
//
//	GET /api/v1/nonsense   ->  404  "404 page not found\n"
//	DELETE /api/v1/orders  ->  405  "Method Not Allowed\n"
//
// Those bypass our handlers entirely, so a client that carefully implemented
// one error parser against our documented envelope gets a parse failure the
// first time somebody fat-fingers a URL. The API had TWO error shapes and the
// documentation only admitted to one.
//
// This middleware buffers any 4xx/5xx that was NOT written as JSON and
// replaces it with the standard envelope. Successful responses and responses
// our own handlers produced are passed through untouched, so it costs nothing
// on the happy path.
//
// WHY MIDDLEWARE RATHER THAN A CATCH-ALL ROUTE: a catch-all `mux.Handle("/")`
// fixes 404 but NOT 405 — ServeMux generates the method-mismatch response
// itself, before any handler of ours runs. Intercepting at the writer is the
// only place that catches both.
// ============================================================================
func NormalizeErrors() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isProbe(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}

			nw := &normalizingWriter{ResponseWriter: w, request: r}
			next.ServeHTTP(nw, r)
			nw.finish()
		})
	}
}

// normalizingWriter withholds the body of a non-JSON error until we can see
// whether it needs replacing.
type normalizingWriter struct {
	http.ResponseWriter
	request *http.Request

	status      int
	wroteHeader bool
	// intercepting is set when we have decided to swallow this response and
	// substitute our own.
	intercepting  bool
	captured      []byte
	passedThrough bool
}

func (w *normalizingWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.status = status
	w.wroteHeader = true

	// Intercept only an ERROR whose content type is not already JSON. That is
	// precisely the "framework wrote it, not us" case.
	contentType := w.Header().Get("Content-Type")
	if status >= 400 && !strings.Contains(contentType, "application/json") {
		w.intercepting = true
		// Do NOT forward the header yet: we are about to change the
		// Content-Type and the body, and headers can only be written once.
		return
	}
	w.passedThrough = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *normalizingWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		// An implicit 200 from a handler that only called Write.
		w.WriteHeader(http.StatusOK)
	}
	if w.intercepting {
		// Capture and discard the framework's text body. We cap what we keep:
		// we only need it to decide, not to echo it back.
		if len(w.captured) < 512 {
			w.captured = append(w.captured, b...)
		}
		return len(b), nil // report success so the caller is none the wiser
	}
	return w.ResponseWriter.Write(b)
}

// finish emits the substituted response, if we intercepted one.
func (w *normalizingWriter) finish() {
	if !w.intercepting {
		return
	}

	code, message := "not_found", "the requested resource does not exist"
	if w.status == http.StatusMethodNotAllowed {
		code = "method_not_allowed"
		// The Allow header that ServeMux set is genuinely useful, so it is
		// preserved — we are changing the body's shape, not hiding information.
		message = "that HTTP method is not supported for this path"
		if allow := w.Header().Get("Allow"); allow != "" {
			message += " (allowed: " + allow + ")"
		}
	} else if w.status >= 500 {
		code, message = "internal_error", "an unexpected error occurred"
	} else if w.status == http.StatusRequestEntityTooLarge {
		code, message = "payload_too_large", "the request body is too large"
	}

	var body ErrorBody
	body.Error.Code = code
	body.Error.Message = message
	body.Error.RequestID = logging.RequestIDFromContext(w.request.Context())

	raw, err := json.Marshal(body)
	if err != nil {
		// Should be impossible for a fixed struct, but we must still send
		// SOMETHING or the client gets an empty response with no status.
		raw = []byte(`{"error":{"code":"internal_error","message":"an unexpected error occurred"}}`)
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
	w.ResponseWriter.WriteHeader(w.status)
	_, _ = w.ResponseWriter.Write(raw)
}

// Unwrap keeps http.ResponseController working through the wrapper.
func (w *normalizingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// isProbe matches the operational endpoints we exclude from logs and metrics.
func isProbe(path string) bool {
	return path == constants.PathHealthz || path == constants.PathReadyz || path == constants.PathMetrics
}

// statusClass buckets 200->2xx, 404->4xx. This keeps metric cardinality at 5
// series instead of one per distinct status code.
func statusClass(status int) string {
	switch {
	case status < 200:
		return "1xx"
	case status < 300:
		return "2xx"
	case status < 400:
		return "3xx"
	case status < 500:
		return "4xx"
	default:
		return "5xx"
	}
}
