// Package errs defines the system's error vocabulary.
//
// THE PROBLEM IT SOLVES:
// Deep inside the repository we know "this order does not exist". The HTTP
// layer needs to turn that into 404. The naive fix is to return an HTTP
// status from the repository — but then your database code depends on
// net/http, and you can never reuse it from a CLI or a gRPC server.
//
// THE FIX (Dependency Inversion):
// The domain returns a transport-agnostic *Error carrying a Kind. ONE place
// (internal/platform/httpx) maps Kind -> HTTP status. Swap in gRPC tomorrow
// and you write one new mapping function; no business logic changes.
package errs

import (
	"errors"
	"fmt"
)

// Kind is the category of failure. Categories, not individual errors, because
// the caller cares "is this my fault or theirs?", not the exact message.
type Kind string

const (
	KindInvalid       Kind = "invalid"       // -> 400 Bad request: caller sent nonsense
	KindNotFound      Kind = "not_found"     // -> 404
	KindConflict      Kind = "conflict"      // -> 409 Business rule says no (bad state transition)
	KindConcurrency   Kind = "concurrency"   // -> 409 Someone else changed it; safe to retry
	KindUnprocessable Kind = "unprocessable" // -> 422 Well-formed but semantically impossible
	KindUnavailable   Kind = "unavailable"   // -> 503 A dependency is down; retry later
	KindTimeout       Kind = "timeout"       // -> 504
	KindInternal      Kind = "internal"      // -> 500 Our bug. Never leak details to the client.
)

// Error is our error type. It carries:
//   - Kind:    how the transport layer should respond
//   - Code:    a stable machine-readable string for API clients
//   - Message: a safe, human-readable sentence we are happy to show a user
//   - Details: structured extras (which field was wrong, etc.)
//   - wrapped: the underlying cause, for logs only — never sent to clients
type Error struct {
	Kind    Kind
	Code    string
	Message string
	Details map[string]any
	wrapped error
}

// Error implements the error interface.
func (e *Error) Error() string {
	if e.wrapped != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.wrapped)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Unwrap lets errors.Is / errors.As walk the chain. Without this, wrapping
// would hide the cause from everything downstream.
func (e *Error) Unwrap() error { return e.wrapped }

// WithDetail attaches structured context. Returns the same error so it chains:
//
//	errs.Invalid("bad_qty", "quantity must be positive").WithDetail("field", "quantity")
func (e *Error) WithDetail(key string, val any) *Error {
	if e.Details == nil {
		e.Details = map[string]any{}
	}
	e.Details[key] = val
	return e
}

// Wrap attaches an underlying cause (logged, never returned to the client).
func (e *Error) Wrap(cause error) *Error {
	e.wrapped = cause
	return e
}

// ---------------------------------------------------------------------------
// Constructors — one per Kind. Short names keep call sites readable.
// ---------------------------------------------------------------------------

func New(kind Kind, code, msg string) *Error {
	return &Error{Kind: kind, Code: code, Message: msg}
}

func Invalid(code, msg string) *Error       { return New(KindInvalid, code, msg) }
func NotFound(code, msg string) *Error      { return New(KindNotFound, code, msg) }
func Conflict(code, msg string) *Error      { return New(KindConflict, code, msg) }
func Concurrency(code, msg string) *Error   { return New(KindConcurrency, code, msg) }
func Unprocessable(code, msg string) *Error { return New(KindUnprocessable, code, msg) }
func Unavailable(code, msg string) *Error   { return New(KindUnavailable, code, msg) }
func Timeout(code, msg string) *Error       { return New(KindTimeout, code, msg) }
func Internal(code, msg string) *Error      { return New(KindInternal, code, msg) }

// ---------------------------------------------------------------------------
// Inspection helpers
// ---------------------------------------------------------------------------

// KindOf extracts the Kind from any error. An unrecognised error is treated
// as KindInternal: if we did not deliberately classify it, it is our bug and
// it should page us, not blame the customer with a 400.
func KindOf(err error) Kind {
	if err == nil {
		return ""
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return KindInternal
}

// Is reports whether err has the given Kind.
func Is(err error, kind Kind) bool { return KindOf(err) == kind }

// IsRetryable answers "would trying this again plausibly work?"
//
// This single function is why the Kind taxonomy earns its keep: retry logic,
// saga compensation and circuit breakers all consult it instead of
// re-implementing their own string matching on error messages.
func IsRetryable(err error) bool {
	switch KindOf(err) {
	case KindConcurrency, KindUnavailable, KindTimeout:
		return true // transient — the world may differ in 20ms
	default:
		return false // deterministic — retrying gets the identical failure
	}
}

// As is a thin, typed wrapper over errors.As for our Error type.
func As(err error) (*Error, bool) {
	var e *Error
	ok := errors.As(err, &e)
	return e, ok
}
