package domain

import (
	"fmt"

	"github.com/chahat/order-processing-system/internal/platform/errs"
)

// Status is an order's position in its lifecycle.
//
// ============================================================================
// THE STATE PATTERN (behavioural, GoF) — and why it lives HERE, in the domain
// ============================================================================
// The naive implementation scatters checks through the service layer:
//
//	if order.Status == "SHIPPED" && newStatus == "PENDING" { return err }
//	if order.Status == "DELIVERED" { return err }
//	... repeated in cancel(), in the worker, in the admin endpoint ...
//
// That fails in three ways:
//  1. Someone adds a new status and forgets one of the five call sites, so
//     invalid transitions become possible in exactly one code path.
//  2. The rules are not discoverable — you must read every caller to learn
//     the state machine.
//  3. It cannot be unit-tested on its own.
//
// Here, the transition table is ONE map in ONE place. Every caller asks
// `CanTransitionTo`. Adding a RETURNED status means editing one map, and the
// compiler plus tests tell you what else to consider.
//
// THE STATE DIAGRAM:
//
//	                    +-----------+
//	                    |  PENDING  |  <- created here
//	                    +-----+-----+
//	         cancel          |            auto-advance (worker, 5 min)
//	      +------------------+------------------+
//	      v                                     v
//	+-----------+                        +------------+
//	| CANCELLED |                        | PROCESSING |
//	+-----------+                        +-----+------+
//	  (terminal)                               |
//	                        +------------------+--------+
//	                        v                           v
//	                  +---------+                  +--------+
//	                  | SHIPPED |                  | FAILED |
//	                  +----+----+                  +--------+
//	                       |                        (terminal)
//	                       v
//	                 +-----------+
//	                 | DELIVERED |
//	                 +-----------+
//	                  (terminal)
//
// ============================================================================
type Status string

const (
	// StatusPending — accepted and persisted, not yet worked on. This is the
	// only state from which a customer may cancel freely.
	StatusPending Status = "PENDING"

	// StatusProcessing — the warehouse is picking it. Stock is reserved and
	// payment is captured, so cancelling now requires a refund and a stock
	// release, which is why we treat it as a different (privileged) operation.
	StatusProcessing Status = "PROCESSING"

	StatusShipped   Status = "SHIPPED"
	StatusDelivered Status = "DELIVERED" // terminal: happy ending
	StatusCancelled Status = "CANCELLED" // terminal: deliberate stop
	// StatusFailed — a saga could not complete and compensations ran. Kept as
	// a distinct state from CANCELLED because the CAUSE differs (system
	// failure vs customer choice) and support needs to tell them apart.
	StatusFailed Status = "FAILED"
)

// transitions is the single source of truth for the state machine.
//
// A nil/empty slice means TERMINAL — no transition out. Encoding terminality
// as "no outgoing edges" rather than a separate boolean means it is
// impossible for the two to disagree.
var transitions = map[Status][]Status{
	StatusPending:    {StatusProcessing, StatusCancelled, StatusFailed},
	StatusProcessing: {StatusShipped, StatusCancelled, StatusFailed},
	StatusShipped:    {StatusDelivered},
	StatusDelivered:  {},
	StatusCancelled:  {},
	StatusFailed:     {},
}

// AllStatuses is used by validation and by the API documentation, so the two
// can never list different values.
func AllStatuses() []Status {
	return []Status{StatusPending, StatusProcessing, StatusShipped, StatusDelivered, StatusCancelled, StatusFailed}
}

// IsValid reports whether s is a status we know about. This is what stops a
// client PATCHing `{"status": "DELETED"}` into the database.
func (s Status) IsValid() bool {
	_, ok := transitions[s]
	return ok
}

// IsTerminal reports whether the order's lifecycle is over.
//
// WHY THIS MATTERS OPERATIONALLY: terminal orders are immutable, which means
// they are SAFE TO CACHE FOR A LONG TIME (nothing can change them) and safe
// to archive to cold storage. A single predicate drives both decisions —
// see the cache TTL logic in the service layer.
func (s Status) IsTerminal() bool {
	next, ok := transitions[s]
	return ok && len(next) == 0
}

// CanTransitionTo reports whether s -> target is legal.
func (s Status) CanTransitionTo(target Status) bool {
	for _, allowed := range transitions[s] {
		if allowed == target {
			return true
		}
	}
	return false
}

// ValidateTransition returns a descriptive domain error instead of a bare
// bool, so the API can tell the caller WHY and WHAT IS allowed.
//
// GOOD ERROR MESSAGES ARE A FEATURE: "invalid transition" costs a developer
// 20 minutes; "cannot move from SHIPPED to PENDING; allowed: [DELIVERED]"
// costs them 5 seconds.
func (s Status) ValidateTransition(target Status) error {
	if !s.IsValid() {
		return errs.Invalid("unknown_status", fmt.Sprintf("%q is not a recognised order status", s))
	}
	if !target.IsValid() {
		return errs.Invalid("unknown_status", fmt.Sprintf("%q is not a recognised order status", target)).
			WithDetail("allowed_statuses", AllStatuses())
	}
	if s == target {
		// An IDEMPOTENT no-op, not an error. A client retrying "set to
		// SHIPPED" after a timeout should get success, not a 409 — otherwise
		// every network blip becomes a support ticket.
		return nil
	}
	if s.IsTerminal() {
		return errs.Unprocessable("order_is_final",
			fmt.Sprintf("order is %s, which is final and cannot be changed", s))
	}
	if !s.CanTransitionTo(target) {
		return errs.Unprocessable("invalid_transition",
			fmt.Sprintf("cannot change status from %s to %s", s, target)).
			WithDetail("current_status", string(s)).
			WithDetail("requested_status", string(target)).
			WithDetail("allowed_next", transitions[s])
	}
	return nil
}

// CanBeCancelledByCustomer encodes the BUSINESS rule, which is deliberately
// stricter than the technical state machine.
//
// The machine permits PROCESSING -> CANCELLED (an admin or a failed saga may
// need it). The business rule is that a CUSTOMER may only cancel while
// PENDING, because once we are picking stock there is a real cost.
//
// WHY SEPARATE THE TWO: conflating "technically possible" with "allowed for
// this actor" is how authorisation bugs are born. Keeping them separate means
// an admin tool can do the former without us weakening the latter.
func (s Status) CanBeCancelledByCustomer() bool {
	return s == StatusPending
}

// NextAutoStatus powers the background worker: PENDING auto-advances to
// PROCESSING, and nothing else auto-advances.
//
// Returning (Status, bool) rather than a sentinel keeps the worker honest —
// it has to handle "nothing to do" explicitly.
func (s Status) NextAutoStatus() (Status, bool) {
	if s == StatusPending {
		return StatusProcessing, true
	}
	return "", false
}
