// Package httpx holds reusable HTTP building blocks: JSON responses, error
// translation, middleware, a server with graceful shutdown, and a typed
// outbound client.
//
// It is the ONE place that knows about HTTP status codes. The service and
// domain layers never import net/http — that is what keeps the business
// logic reusable from a CLI, a queue consumer or a gRPC server.
package httpx

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/chahat/order-processing-system/internal/platform/constants"
	"github.com/chahat/order-processing-system/internal/platform/errs"
	"github.com/chahat/order-processing-system/internal/platform/logging"
)

// ErrorBody is the single error shape every endpoint returns.
//
// WHY A CONSISTENT ENVELOPE MATTERS: a client can write ONE error handler.
// APIs that return a bare string here, an object there, and HTML from the
// load balancer force every consumer to write defensive parsing code.
type ErrorBody struct {
	Error struct {
		Code      string         `json:"code"`                 // stable, machine-readable
		Message   string         `json:"message"`              // safe for humans
		Details   map[string]any `json:"details,omitempty"`    // which field, etc.
		RequestID string         `json:"request_id,omitempty"` // quote this to support
	} `json:"error"`
}

// JSON writes a successful response.
func JSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	// WHY WE DO NOT STREAM DIRECTLY TO w:
	// json.NewEncoder(w).Encode(body) writes a 200 header, then may fail
	// halfway through marshalling — leaving the client with truncated JSON and
	// a success status. Marshalling to a buffer first means an encoding error
	// still produces a clean 500.
	if body == nil {
		w.WriteHeader(status)
		return
	}
	buf, err := json.Marshal(body)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":"encoding_failed","message":"failed to encode response"}}`))
		return
	}
	w.WriteHeader(status)
	_, _ = w.Write(buf)
}

// NoContent is the correct response to a successful DELETE or a no-op PUT.
func NoContent(w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) }

// Error translates a domain error into an HTTP response.
//
// THIS FUNCTION IS THE BOUNDARY. Everything below it speaks errs.Kind;
// everything above it speaks HTTP. One mapping table, one place to change.
func Error(w http.ResponseWriter, r *http.Request, err error) {
	ctx := r.Context()
	log := logging.FromContext(ctx)

	status := statusForKind(errs.KindOf(err))

	code, message := "internal_error", "an unexpected error occurred"
	var details map[string]any
	if e, ok := errs.As(err); ok {
		code = e.Code
		message = e.Message
		details = e.Details
	}

	// SECURITY: for a 5xx we deliberately DISCARD the real message and send a
	// generic one. Internal errors carry SQL fragments, hostnames, and stack
	// context — handing those to an attacker is information disclosure. The
	// full detail goes to our logs, keyed by request_id, so support can still
	// find it in one query.
	if status >= 500 {
		log.ErrorContext(ctx, "request failed",
			slog.String("error", err.Error()),
			slog.String("code", code),
			slog.Int("status", status),
		)
		code, message, details = "internal_error", "an unexpected error occurred", nil
	} else {
		// 4xx is the CALLER's mistake, not an incident. Logging it at ERROR
		// would drown your real alerts in validation noise — a genuine
		// operational problem ("alert fatigue"), so this is WARN.
		log.WarnContext(ctx, "request rejected",
			slog.String("error", err.Error()),
			slog.String("code", code),
			slog.Int("status", status),
		)
	}

	var body ErrorBody
	body.Error.Code = code
	body.Error.Message = message
	body.Error.Details = details
	body.Error.RequestID = logging.RequestIDFromContext(ctx)

	// Retry-After tells a well-behaved client WHEN to come back, instead of
	// leaving it to hammer us immediately. Cheap, and it protects us.
	if status == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", "2")
	}
	JSON(w, status, body)
}

// statusForKind is the complete Kind -> HTTP mapping.
func statusForKind(kind errs.Kind) int {
	switch kind {
	case errs.KindInvalid:
		return http.StatusBadRequest // 400 — malformed/unparseable input
	case errs.KindNotFound:
		return http.StatusNotFound // 404
	case errs.KindConflict:
		return http.StatusConflict // 409 — business rule forbids it
	case errs.KindConcurrency:
		// Also 409, but SEMANTICALLY DIFFERENT: a conflict means "never going
		// to work"; a concurrency error means "someone beat you, try again".
		// We expose that difference via the error code so a client can retry
		// one and not the other.
		return http.StatusConflict
	case errs.KindUnprocessable:
		// 422 — syntactically valid, semantically impossible (e.g. cancelling
		// an already-delivered order). Distinct from 400 on purpose.
		return http.StatusUnprocessableEntity
	case errs.KindUnavailable:
		return http.StatusServiceUnavailable // 503 — dependency down, retry
	case errs.KindTimeout:
		return http.StatusGatewayTimeout // 504
	default:
		return http.StatusInternalServerError // 500 — our bug
	}
}

// DecodeJSON reads and validates a request body.
//
// THREE PROTECTIONS, ALL OF WHICH MATTER:
//  1. MaxBytesReader — without it, a client streaming a 10GB body makes the
//     server allocate until the OOM killer takes it down. A one-line DoS.
//  2. DisallowUnknownFields — catches `{"quantiy": 5}`. Without it that typo
//     is silently ignored and the order is created with quantity 0.
//  3. A single trailing-token check — rejects `{}{}`, i.e. smuggled payloads.
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any, maxBytes int64) error {
	if maxBytes <= 0 {
		maxBytes = 1 << 20 // 1 MiB default
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		return errs.Invalid("invalid_json", "request body is not valid JSON or contains unknown fields").Wrap(err)
	}
	// A second Decode must fail with io.EOF; anything else means extra data.
	if err := dec.Decode(&struct{}{}); err == nil {
		return errs.Invalid("invalid_json", "request body must contain exactly one JSON object")
	}
	return nil
}

// SetCacheStatus records HIT / MISS / BYPASS on the response so the cache is
// observable with `curl -i` alone.
func SetCacheStatus(w http.ResponseWriter, status string) {
	w.Header().Set(constants.HeaderCacheStatus, status)
}

// SetServedBy records which backend produced the response ("new" or "legacy").
// During a Strangler Fig migration this header is how you PROVE traffic has
// shifted, rather than hoping it has.
func SetServedBy(w http.ResponseWriter, who string) {
	w.Header().Set(constants.HeaderServedBy, who)
}
