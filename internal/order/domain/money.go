package domain

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/chahat/order-processing-system/internal/platform/constants"
	"github.com/chahat/order-processing-system/internal/platform/errs"
)

// Money represents an exact monetary amount in MINOR UNITS (cents for USD).
//
// ============================================================================
// WHY NOT float64 — THE SINGLE MOST IMPORTANT DECISION IN THIS FILE
// ============================================================================
// Binary floating point cannot represent 0.1 exactly, the same way decimal
// cannot represent 1/3 exactly. So in every language with IEEE-754 floats:
//
//	0.1 + 0.2        == 0.30000000000000004
//	19.99 * 3        == 59.97000000000001
//	0.1 + 0.2 == 0.3 -> FALSE
//
// Multiply that by a million orders and you get an accounting discrepancy no
// auditor will accept, and which you cannot reconstruct after the fact.
//
// THE FIX used by Stripe, every bank, and every payment processor: store an
// INTEGER COUNT OF THE SMALLEST UNIT. $19.99 is the integer 1999. Addition and
// multiplication by an integer quantity are then exact. Division is the only
// operation needing care (see Allocate below).
//
// WHY int64 AND NOT A DECIMAL LIBRARY: int64 holds ±9.2e18, i.e. ±92 quadrillion
// dollars in cents. No dependency, no precision questions, hardware-speed
// arithmetic. A decimal type earns its keep for tax rates and FX, not for
// order totals.
// ============================================================================
type Money struct {
	// Minor is the amount in the currency's smallest unit.
	Minor int64
	// Currency is an ISO-4217 code. It travels WITH the amount, because
	// "1000" is meaningless alone — 1000 JPY and 1000 USD differ 150x, and
	// JPY has zero decimal places, so it is not even the same scale.
	Currency string
}

func NewMoney(minor int64, currency string) Money {
	if currency == "" {
		currency = constants.CurrencyDefault
	}
	return Money{Minor: minor, Currency: strings.ToUpper(currency)}
}

// Zero is the additive identity, used as the starting value when summing.
func Zero(currency string) Money { return NewMoney(0, currency) }

// Add returns the sum, refusing to mix currencies.
//
// WHY RETURN AN ERROR RATHER THAN SILENTLY ADDING: adding USD to EUR is
// always a bug. Making it a compile-time-adjacent runtime error means that
// bug surfaces in a test, not in a customer's invoice.
func (m Money) Add(other Money) (Money, error) {
	if m.Currency != other.Currency {
		return Money{}, errs.Invalid("currency_mismatch",
			fmt.Sprintf("cannot add %s to %s", other.Currency, m.Currency))
	}
	sum := m.Minor + other.Minor
	// OVERFLOW CHECK. int64 addition wraps silently: a huge positive plus a
	// huge positive becomes NEGATIVE, turning a charge into a refund. The
	// sign test below is the standard, cheap way to detect that.
	if (m.Minor > 0 && other.Minor > 0 && sum < 0) || (m.Minor < 0 && other.Minor < 0 && sum > 0) {
		return Money{}, errs.Invalid("amount_overflow", "the monetary amount overflowed int64")
	}
	return Money{Minor: sum, Currency: m.Currency}, nil
}

// MulQty multiplies a unit price by an integer quantity (price * count).
// Integer * integer stays exact — this is the whole benefit of minor units.
func (m Money) MulQty(qty int) (Money, error) {
	if qty < 0 {
		return Money{}, errs.Invalid("negative_quantity", "quantity cannot be negative")
	}
	if qty != 0 && m.Minor != 0 {
		// Detect overflow BEFORE it happens by checking the division inverse.
		if m.Minor > constants.MaxOrderAmountMinor/int64(qty) {
			return Money{}, errs.Invalid("amount_overflow", "unit price times quantity exceeds the maximum order amount")
		}
	}
	return Money{Minor: m.Minor * int64(qty), Currency: m.Currency}, nil
}

// IsZero / IsPositive are small, named predicates.
//
// WHY NAME THEM instead of writing `m.Minor > 0` inline: the business rule
// "an order must have a positive total" should read as one, and if the rule
// ever changes (e.g. zero-value promotional orders become legal) it changes
// in one place.
func (m Money) IsZero() bool     { return m.Minor == 0 }
func (m Money) IsPositive() bool { return m.Minor > 0 }

// Validate enforces the business bounds on an order amount.
func (m Money) Validate() error {
	if m.Minor < constants.MinOrderAmountMinor {
		return errs.Invalid("amount_too_small",
			fmt.Sprintf("order total must be at least %s", NewMoney(constants.MinOrderAmountMinor, m.Currency)))
	}
	if m.Minor > constants.MaxOrderAmountMinor {
		// An upper bound is FRAUD PROTECTION as much as validation: a
		// $4,000,000 order is far more likely to be a bug or an attack than
		// a real customer, and it should be reviewed by a human.
		return errs.Invalid("amount_too_large",
			fmt.Sprintf("order total must not exceed %s", NewMoney(constants.MaxOrderAmountMinor, m.Currency)))
	}
	return nil
}

// decimalPlaces returns how many decimal digits a currency uses.
//
// MOST currencies use 2 (USD, EUR, GBP). JPY and KRW use ZERO — ¥100 is one
// hundred yen, not one yen. A few (BHD, KWD) use 3. Formatting ¥100 as "1.00"
// is a visible, embarrassing bug, and it is why this function exists.
func decimalPlaces(currency string) int {
	switch strings.ToUpper(currency) {
	case "JPY", "KRW", "VND", "CLP", "ISK":
		return 0
	case "BHD", "KWD", "OMR", "TND", "JOD":
		return 3
	default:
		return 2
	}
}

// String renders the amount for humans: "19.99 USD".
func (m Money) String() string {
	places := decimalPlaces(m.Currency)
	if places == 0 {
		return fmt.Sprintf("%d %s", m.Minor, m.Currency)
	}
	divisor := int64(1)
	for i := 0; i < places; i++ {
		divisor *= 10
	}
	neg := m.Minor < 0
	abs := m.Minor
	if neg {
		abs = -abs
	}
	whole := abs / divisor
	frac := abs % divisor
	sign := ""
	if neg {
		sign = "-"
	}
	return fmt.Sprintf("%s%d.%0*d %s", sign, whole, places, frac, m.Currency)
}

// MajorString renders just the numeric part ("19.99"), for API responses that
// carry the currency in a separate field.
func (m Money) MajorString() string {
	s := m.String()
	return strings.TrimSuffix(s, " "+m.Currency)
}

// ParseMoney converts a decimal string like "19.99" into minor units.
//
// WHY WE PARSE STRINGS AND DO NOT ACCEPT JSON NUMBERS FOR MONEY:
// `{"price": 19.99}` is parsed by every JSON library into a float64 — so the
// precision is already lost before our code ever sees it. Accepting
// `{"price": "19.99"}` as a string keeps it exact end to end. Our API accepts
// BOTH a string and an integer minor-unit field, and this is the string path.
func ParseMoney(s, currency string) (Money, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Money{}, errs.Invalid("invalid_amount", "amount must not be empty")
	}
	places := decimalPlaces(currency)

	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(strings.TrimPrefix(s, "-"), "+")

	whole, frac, hasFrac := strings.Cut(s, ".")
	if whole == "" {
		whole = "0"
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return Money{}, errs.Invalid("invalid_amount", fmt.Sprintf("%q is not a valid amount", s)).Wrap(err)
	}

	var f int64
	if hasFrac {
		if len(frac) > places {
			// We REJECT rather than round. Silently rounding a customer's
			// "10.999" to "11.00" changes what they agreed to pay; making it
			// an error forces the caller to be explicit.
			return Money{}, errs.Invalid("too_many_decimals",
				fmt.Sprintf("%s supports at most %d decimal places", currency, places))
		}
		// Right-pad: "19.9" with 2 places means 90 cents, not 9.
		for len(frac) < places {
			frac += "0"
		}
		if frac != "" {
			if f, err = strconv.ParseInt(frac, 10, 64); err != nil {
				return Money{}, errs.Invalid("invalid_amount", fmt.Sprintf("%q is not a valid amount", s)).Wrap(err)
			}
		}
	}

	divisor := int64(1)
	for i := 0; i < places; i++ {
		divisor *= 10
	}
	minor := w*divisor + f
	if neg {
		minor = -minor
	}
	return NewMoney(minor, currency), nil
}

// Allocate splits an amount into n parts WITHOUT LOSING A SINGLE CENT.
//
// ============================================================================
// THE "PENNY PROBLEM" — a classic interview question
// ============================================================================
// Split $10.00 three ways. 1000 / 3 = 333.33, so naive rounding gives three
// parts of 333, totalling 999. ONE CENT HAS VANISHED. Do that on every split
// invoice and your ledger stops balancing.
//
// The fix: integer-divide to get the base share, then distribute the remainder
// one minor unit at a time to the first `remainder` parts. Result:
// [334, 333, 333] — which sums to exactly 1000. Always.
//
// This same algorithm is what you use for splitting a discount across line
// items, or a shipping fee across packages.
// ============================================================================
func (m Money) Allocate(n int) ([]Money, error) {
	if n <= 0 {
		return nil, errs.Invalid("invalid_allocation", "the number of parts must be positive")
	}
	base := m.Minor / int64(n)
	remainder := m.Minor % int64(n)

	out := make([]Money, n)
	for i := 0; i < n; i++ {
		share := base
		if int64(i) < remainder {
			share++ // hand out the leftover cents, one each
		}
		out[i] = Money{Minor: share, Currency: m.Currency}
	}
	return out, nil
}
