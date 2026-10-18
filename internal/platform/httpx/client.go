package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/chahat/order-processing-system/internal/platform/constants"
	"github.com/chahat/order-processing-system/internal/platform/errs"
	"github.com/chahat/order-processing-system/internal/platform/logging"
	"github.com/chahat/order-processing-system/internal/platform/retry"
)

// Client is a JSON-over-HTTP client for calling our own microservices.
//
// WHY NOT JUST http.Get / http.Post:
//  1. http.DefaultClient HAS NO TIMEOUT. A hung dependency hangs your request
//     forever, and goroutines accumulate until the process dies. This is the
//     most common Go production bug there is.
//  2. Every caller would re-implement retries, idempotency keys, request-id
//     propagation and error translation — four chances to get it wrong.
//  3. A shared client REUSES CONNECTIONS. Creating a client per call forces a
//     new TCP + TLS handshake every time and leaks file descriptors.
type Client struct {
	baseURL string
	http    *http.Client
	log     *slog.Logger
	retry   retry.Policy
	breaker *CircuitBreaker
}

func NewClient(baseURL string, timeout time.Duration, log *slog.Logger) *Client {
	return &Client{
		baseURL: baseURL,
		http: &http.Client{
			// This is the OVERALL deadline: DNS + connect + send + wait + read.
			Timeout: timeout,
			Transport: &http.Transport{
				// Pool sizing: without MaxIdleConnsPerHost (default 2), a
				// burst of concurrent calls to one service opens and discards
				// connections continuously, and you spend all your latency on
				// TCP handshakes.
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 20,
				IdleConnTimeout:     90 * time.Second,
				DialContext: (&net.Dialer{
					Timeout:   2 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				// Fail fast if the remote accepts the TCP connection but then
				// never sends response headers.
				ResponseHeaderTimeout: timeout,
			},
		},
		log:     log,
		retry:   retry.DefaultPolicy(3, 50*time.Millisecond),
		breaker: NewCircuitBreaker(5, 10*time.Second),
	}
}

// CallOptions describes one request.
type CallOptions struct {
	Method string
	Path   string
	Body   any
	// IdempotencyKey makes a retried POST safe. See the long note below.
	IdempotencyKey string
	// Out receives the decoded response body (pass nil to ignore it).
	Out any
	// Retry: set false for operations that are NOT safe to repeat without an
	// idempotency key.
	Retry bool
}

// Do executes the call, with retries, circuit breaking and error translation.
//
// ============================================================================
// IDEMPOTENCY: WHY RETRYING A POST IS DANGEROUS WITHOUT A KEY
// ============================================================================
// We POST /charge. The payment service charges the card, then the response is
// lost to a network timeout. We do not know whether it succeeded, so we retry
// — and the customer is charged TWICE.
//
// The fix is an Idempotency-Key header: a unique id for this LOGICAL
// operation, reused across retries. The server records "key K -> result R".
// On seeing K again it returns the stored R WITHOUT re-charging.
//
// This is exactly how Stripe's API works, and it is the only correct way to
// retry a non-idempotent operation. See internal/payment/service.
// ============================================================================
func (c *Client) Do(ctx context.Context, opts CallOptions) error {
	// CIRCUIT BREAKER: if this dependency has been failing, stop calling it.
	// Hammering a service that is already overloaded prevents it recovering,
	// and makes OUR latency terrible while we wait for timeouts that we can
	// already predict. Failing instantly is kinder to both sides.
	if !c.breaker.Allow() {
		return errs.Unavailable("circuit_open",
			fmt.Sprintf("circuit breaker is open for %s; not attempting the call", c.baseURL))
	}

	policy := c.retry
	if !opts.Retry {
		policy.MaxAttempts = 1
	}

	err := retry.Do(ctx, policy, errs.IsRetryable, func(ctx context.Context, attempt int) error {
		return c.doOnce(ctx, opts, attempt)
	})

	// Feed the outcome back into the breaker. Note we only count
	// AVAILABILITY failures: a 400 means WE sent a bad request, which says
	// nothing about the remote service's health and must not open the circuit.
	if err != nil && (errs.Is(err, errs.KindUnavailable) || errs.Is(err, errs.KindTimeout)) {
		c.breaker.RecordFailure()
	} else {
		c.breaker.RecordSuccess()
	}
	return err
}

func (c *Client) doOnce(ctx context.Context, opts CallOptions, attempt int) error {
	var bodyReader io.Reader
	if opts.Body != nil {
		raw, err := json.Marshal(opts.Body)
		if err != nil {
			return errs.Internal("marshal_failed", "failed to encode request body").Wrap(err)
		}
		// A fresh reader PER ATTEMPT. Reusing one would make retry #2 send an
		// empty body, because the first attempt already drained it.
		bodyReader = bytes.NewReader(raw)
	}

	url := c.baseURL + opts.Path
	req, err := http.NewRequestWithContext(ctx, opts.Method, url, bodyReader)
	if err != nil {
		return errs.Internal("bad_request_build", "failed to build request").Wrap(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	// DISTRIBUTED TRACING, the cheap version: forward our request id so the
	// downstream service logs the same id. One grep across all services then
	// reconstructs the whole call chain.
	if rid := logging.RequestIDFromContext(ctx); rid != "" {
		req.Header.Set(constants.HeaderRequestID, rid)
	}
	if opts.IdempotencyKey != "" {
		req.Header.Set(constants.HeaderIdempotencyKey, opts.IdempotencyKey)
	}

	log := logging.FromContext(ctx)
	start := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		// A transport error: DNS failure, connection refused, or timeout.
		// We cannot tell whether the server processed the request — which is
		// precisely why the idempotency key exists.
		log.WarnContext(ctx, "downstream call failed at transport level",
			slog.String("url", url), slog.Int("attempt", attempt), slog.String("error", err.Error()))
		if ctx.Err() != nil {
			return errs.Timeout("downstream_timeout", "downstream call timed out").Wrap(err)
		}
		return errs.Unavailable("downstream_unreachable", "downstream service is unreachable").Wrap(err)
	}
	// MUST close the body, and MUST drain it, or the connection cannot be
	// returned to the idle pool and is thrown away. Silent performance killer.
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		_ = resp.Body.Close()
	}()

	// Bound the response we are willing to read, so a misbehaving dependency
	// cannot exhaust our memory.
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if readErr != nil {
		return errs.Unavailable("downstream_read_failed", "failed to read downstream response").Wrap(readErr)
	}

	log.DebugContext(ctx, "downstream call complete",
		slog.String("url", url), slog.Int("status", resp.StatusCode),
		slog.Duration("duration", time.Since(start)), slog.Int("attempt", attempt))

	if resp.StatusCode >= 400 {
		return c.translateError(resp.StatusCode, raw)
	}
	if opts.Out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, opts.Out); err != nil {
			return errs.Internal("downstream_decode_failed", "failed to decode downstream response").Wrap(err)
		}
	}
	return nil
}

// translateError maps a downstream HTTP status back into our error vocabulary,
// preserving the remote error code so logs stay meaningful across services.
func (c *Client) translateError(status int, raw []byte) error {
	var body ErrorBody
	_ = json.Unmarshal(raw, &body) // best-effort; the body may not be our shape
	code := body.Error.Code
	if code == "" {
		code = fmt.Sprintf("downstream_%d", status)
	}
	msg := body.Error.Message
	if msg == "" {
		msg = fmt.Sprintf("downstream returned %d", status)
	}

	switch {
	case status == http.StatusNotFound:
		return errs.NotFound(code, msg)
	case status == http.StatusConflict:
		return errs.Conflict(code, msg)
	case status == http.StatusUnprocessableEntity:
		return errs.Unprocessable(code, msg)
	case status == http.StatusTooManyRequests:
		// 429 is explicitly "come back later" — retryable by definition.
		return errs.Unavailable(code, msg)
	case status == http.StatusGatewayTimeout:
		return errs.Timeout(code, msg)
	case status >= 500:
		// 5xx from a dependency is RETRYABLE (their transient problem).
		return errs.Unavailable(code, msg)
	default:
		// 4xx is NOT retryable: we sent something invalid and will keep
		// sending the same invalid thing. Classifying this correctly is what
		// stops a validation bug becoming a retry storm.
		return errs.Invalid(code, msg)
	}
}

// ---------------------------------------------------------------------------
// CircuitBreaker — a small, explicit state machine (behavioural: State pattern)
// ---------------------------------------------------------------------------
//
// THE THREE STATES:
//
//	CLOSED    normal. Calls pass through. We count consecutive failures.
//	OPEN      tripped. Calls fail INSTANTLY without touching the network.
//	          This is "load shedding": it protects the struggling dependency
//	          from our traffic, and protects us from waiting on timeouts.
//	HALF-OPEN after a cooldown, we let ONE probe through. Success -> CLOSED,
//	          failure -> OPEN again.
//
// WHY HALF-OPEN EXISTS: without a probe state, recovery requires either a
// human or a flood of traffic to discover the service is healthy again. One
// cheap probe answers the question without re-overloading the dependency.
type CircuitBreaker struct {
	mu               sync.Mutex
	failures         int
	threshold        int
	cooldown         time.Duration
	openedAt         time.Time
	state            string
	halfOpenInFlight bool
}

const (
	breakerClosed   = "closed"
	breakerOpen     = "open"
	breakerHalfOpen = "half-open"
)

func NewCircuitBreaker(threshold int, cooldown time.Duration) *CircuitBreaker {
	if threshold <= 0 {
		threshold = 5
	}
	return &CircuitBreaker{threshold: threshold, cooldown: cooldown, state: breakerClosed}
}

// Allow reports whether a call may proceed, advancing the state machine.
func (b *CircuitBreaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case breakerOpen:
		if time.Since(b.openedAt) >= b.cooldown {
			// Cooldown elapsed: move to half-open and allow exactly ONE probe.
			b.state = breakerHalfOpen
			b.halfOpenInFlight = true
			return true
		}
		return false
	case breakerHalfOpen:
		// Admit only a single probe. Without this guard, 1,000 queued
		// requests would all be "the probe" and we would re-flood the
		// dependency the instant the cooldown expired.
		if b.halfOpenInFlight {
			return false
		}
		b.halfOpenInFlight = true
		return true
	default:
		return true
	}
}

func (b *CircuitBreaker) RecordSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures = 0
	b.state = breakerClosed
	b.halfOpenInFlight = false
}

func (b *CircuitBreaker) RecordFailure() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures++
	b.halfOpenInFlight = false
	if b.failures >= b.threshold || b.state == breakerHalfOpen {
		// A failed probe sends us straight back to OPEN and restarts the
		// cooldown, rather than retrying immediately.
		b.state = breakerOpen
		b.openedAt = time.Now()
	}
}

// State is exported for /readyz reporting and tests.
func (b *CircuitBreaker) State() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}
