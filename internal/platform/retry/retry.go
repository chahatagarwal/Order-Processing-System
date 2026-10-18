// Package retry implements bounded retries with exponential backoff + jitter.
//
// WHY THIS IS ITS OWN PACKAGE (DRY + Single Responsibility):
// Retry logic is needed by the optimistic-locking path, the saga participant
// calls, and the outbox publisher. Written three times, it would be subtly
// wrong in at least two of them. Written once, it is tested once.
//
// THE THREE THINGS NAIVE RETRY GETS WRONG:
//
//  1. RETRYING NON-RETRYABLE ERRORS. Retrying "quantity must be positive"
//     3 times just wastes 3x the resources and returns the same 400.
//     => We consult a predicate (errs.IsRetryable) instead of retrying blindly.
//
//  2. NO BACKOFF. Retrying instantly, in a tight loop, during an overload is
//     how you turn a slow database into a dead database. This is a
//     "retry storm" and it is a classic cause of cascading failure.
//     => We back off exponentially: 20ms, 40ms, 80ms...
//
//  3. NO JITTER. If 500 requests all fail at once and all back off exactly
//     40ms, they all retry at exactly the same instant — the "thundering
//     herd". The spike repeats forever, just 40ms later each time.
//     => We add randomness so the herd spreads out.
package retry

import (
	"context"
	"math/rand"
	"time"
)

// Policy describes how hard to try.
type Policy struct {
	// MaxAttempts counts the FIRST try. MaxAttempts=3 means 1 try + 2 retries.
	// Naming this clearly prevents the classic off-by-one in retry code.
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	// JitterFraction 0.0..1.0 — 0.5 means "randomise the delay by ±50%".
	JitterFraction float64
}

// DefaultPolicy is a sane starting point: 3 attempts, 20ms base, ±50% jitter.
func DefaultPolicy(maxAttempts int, base time.Duration) Policy {
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	if base <= 0 {
		base = 20 * time.Millisecond
	}
	return Policy{
		MaxAttempts:    maxAttempts,
		BaseDelay:      base,
		MaxDelay:       2 * time.Second,
		JitterFraction: 0.5,
	}
}

// Do runs fn until it succeeds, until shouldRetry says stop, until attempts
// run out, or until the context is cancelled — whichever comes first.
//
// `attempt` is 1-based so log lines read "attempt 1 of 3", not "attempt 0".
func Do(ctx context.Context, p Policy, shouldRetry func(error) bool, fn func(ctx context.Context, attempt int) error) error {
	var lastErr error

	for attempt := 1; attempt <= p.MaxAttempts; attempt++ {
		// Check cancellation BEFORE doing work. If the customer closed their
		// browser, doing attempt 3 of 3 is pure waste.
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return lastErr // report the real cause, not "context canceled"
			}
			return err
		}

		lastErr = fn(ctx, attempt)
		if lastErr == nil {
			return nil
		}
		if shouldRetry != nil && !shouldRetry(lastErr) {
			return lastErr // permanent failure: stop immediately
		}
		if attempt == p.MaxAttempts {
			break // out of budget; fall through and return lastErr
		}

		// Sleep, but stay responsive to cancellation. A bare time.Sleep here
		// would ignore ctx and hold the request open past its deadline.
		select {
		case <-ctx.Done():
			return lastErr
		case <-time.After(p.delayFor(attempt)):
		}
	}
	return lastErr
}

// delayFor computes delay = base * 2^(attempt-1), capped, then jittered.
func (p Policy) delayFor(attempt int) time.Duration {
	delay := p.BaseDelay
	for i := 1; i < attempt; i++ {
		delay *= 2
		if p.MaxDelay > 0 && delay > p.MaxDelay {
			delay = p.MaxDelay
			break
		}
	}
	if p.JitterFraction > 0 {
		// Spread uniformly across [delay*(1-f), delay*(1+f)].
		span := float64(delay) * p.JitterFraction
		// #nosec G404 -- math/rand is correct here: jitter needs to be
		// unpredictable to *other retriers*, not cryptographically secure.
		// Using crypto/rand for this would be slower for no benefit.
		delay = time.Duration(float64(delay) - span + rand.Float64()*2*span)
	}
	if delay < 0 {
		delay = p.BaseDelay
	}
	return delay
}
