package domain

import "github.com/chahat/order-processing-system/internal/platform/errs"

// Domain-level sentinel errors.
//
// WHY PRE-BUILT VALUES RATHER THAN CONSTRUCTING THEM AT EACH THROW SITE:
// the repository, the service and the saga all need to signal "version
// conflict", and they must signal it IDENTICALLY so that one `errs.Is` check
// in the retry loop catches all of them. Declaring the values once makes that
// guaranteed rather than hoped for.
var (
	// ErrVersionConflict is returned by UpdateOptimistic when the stored
	// version no longer matches ours. It carries KindConcurrency, which makes
	// errs.IsRetryable(err) == true, which is what drives the automatic retry.
	ErrVersionConflict = errs.Concurrency("version_conflict",
		"this order was modified by another request; please retry")

	// ErrOrderNotFound -> 404.
	ErrOrderNotFound = errs.NotFound("order_not_found", "no order exists with that identifier")

	// ErrDuplicateOrderNumber indicates our random order number collided with
	// an existing one. Astronomically unlikely (32^8 possibilities), but the
	// UNIQUE constraint is what makes it IMPOSSIBLE rather than merely
	// improbable — and the caller simply generates a new number and retries.
	ErrDuplicateOrderNumber = errs.Conflict("duplicate_order_number", "that order number is already in use")

	// ErrInsufficientStock is a BUSINESS failure, not a technical one. It is
	// KindUnprocessable (422), not KindUnavailable (503), because retrying
	// will not create stock. Classifying it correctly is what stops the saga
	// from pointlessly retrying a guaranteed failure.
	ErrInsufficientStock = errs.Unprocessable("insufficient_stock", "there is not enough stock to fulfil this order")

	// ErrPaymentDeclined — likewise a business outcome. The card was declined;
	// trying again in 20ms will not change the bank's mind.
	ErrPaymentDeclined = errs.Unprocessable("payment_declined", "the payment was declined")
)
