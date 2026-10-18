package domain

import (
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/chahat/order-processing-system/internal/platform/constants"
	"github.com/chahat/order-processing-system/internal/platform/errs"
	"github.com/chahat/order-processing-system/internal/platform/idgen"
)

// ============================================================================
// THE DOMAIN LAYER: the innermost ring of the architecture
// ============================================================================
// Notice what this file DOES NOT import: no net/http, no pgx, no redis, no
// JSON tags. That is not an accident, it is the whole design.
//
// DEPENDENCIES POINT INWARDS (the Dependency Rule):
//
//	transport -> service -> domain <- repository
//
// The domain depends on NOTHING. Everything depends on the domain. The
// consequences are concrete:
//   - You can unit-test every business rule with no database and no server.
//   - Swapping Postgres for MongoDB does not touch a single line in here.
//   - Adding a gRPC API does not touch a single line in here.
//
// This is the Dependency Inversion Principle (the "D" in SOLID) applied at
// the architectural scale, and it is why the test suite runs in milliseconds.
// ============================================================================

// Order is the AGGREGATE ROOT.
//
// WHAT "AGGREGATE ROOT" MEANS, PRACTICALLY: Items are part of the Order and
// have no independent life — you never load, change or delete an OrderItem on
// its own. So all access goes THROUGH the Order, the Order enforces the
// invariants for the whole cluster (e.g. "total equals the sum of items"), and
// the Order is the unit of transactional consistency and of locking.
//
// Getting this boundary right is what makes locking tractable: we lock one
// order row, not an unpredictable graph of rows.
type Order struct {
	ID            string // UUID, our internal primary key
	OrderNumber   string // ORD-7K2M9QX4, the human-facing reference
	CustomerID    string
	CustomerEmail string
	CustomerName  string

	Status      Status
	Items       []OrderItem
	TotalAmount Money

	ShippingAddress string
	Notes           string

	// ------------------------------------------------------------------
	// Version drives OPTIMISTIC LOCKING.
	// ------------------------------------------------------------------
	// Every UPDATE carries `WHERE id = $1 AND version = $2` and sets
	// `version = version + 1`. If another transaction committed in between,
	// our version is stale, zero rows match, and we KNOW we would have
	// overwritten someone's change — so we retry instead of losing it.
	//
	// THE BUG THIS PREVENTS (the "lost update"):
	//   T1 reads order (notes="")          T2 reads order (notes="")
	//   T1 writes notes="call customer"
	//                                      T2 writes notes="gift wrap"
	//   Result: T1's note is GONE, with no error and no trace.
	// With versions, T2's write matches 0 rows and it must re-read first.
	Version int

	// CancelReason / FailureReason exist separately because the two outcomes
	// are operationally different: one is a customer choice, one is a system
	// failure that someone may need to investigate.
	CancelReason  string
	FailureReason string

	// SagaID links this order to its distributed transaction, so that a
	// stuck order can be traced to the exact step that failed.
	SagaID string

	CreatedAt       time.Time
	UpdatedAt       time.Time
	StatusChangedAt time.Time

	// Source records WHICH system owns this record. During the Strangler Fig
	// migration, "legacy" means it came from the old service. This is how we
	// measure migration progress with a GROUP BY instead of a guess.
	Source string
}

const (
	SourceNew    = "new"
	SourceLegacy = "legacy"
)

// OrderItem is one line of an order.
//
// NOTE THE DENORMALISATION: we copy ProductName and UnitPrice onto the item
// instead of joining to a products table at read time.
//
// WHY, AND WHY IT IS NOT A MISTAKE: an order is a HISTORICAL RECORD of what
// was agreed. If the product's price changes next week, or the product is
// renamed, or it is deleted from the catalogue entirely, last month's invoice
// must still show what the customer actually bought and paid. Joining live
// catalogue data would silently rewrite history — and break your accounting.
//
// This is "denormalise for correctness", which is a stronger reason than the
// usual "denormalise for performance" (though we get that too: no join).
type OrderItem struct {
	ID          string
	OrderID     string
	ProductID   string
	ProductName string // snapshot at purchase time
	ProductSKU  string
	Quantity    int
	UnitPrice   Money // snapshot at purchase time
	LineTotal   Money // UnitPrice * Quantity, stored for auditability
}

// NewOrderInput is the validated input for creating an order.
//
// WHY A SEPARATE INPUT TYPE instead of accepting a half-built Order:
// if the constructor took an *Order, a caller could pass one with
// Status="DELIVERED", Version=99 and a forged ID. By accepting only the
// fields a client is ALLOWED to set, those attacks are not expressible.
// The type system enforces the rule, so no reviewer has to remember it.
type NewOrderInput struct {
	CustomerID      string
	CustomerEmail   string
	CustomerName    string
	ShippingAddress string
	Notes           string
	Currency        string
	Items           []NewOrderItemInput
}

type NewOrderItemInput struct {
	ProductID   string
	ProductName string
	ProductSKU  string
	Quantity    int
	// UnitPriceMinor is in cents. The transport layer accepts either a
	// decimal string or this integer and normalises to this field.
	UnitPriceMinor int64
}

// NewOrder is the FACTORY METHOD (creational, GoF) for an Order.
//
// ============================================================================
// WHY A FACTORY AND NOT JUST `&Order{...}`
// ============================================================================
// A struct literal lets any caller build an INVALID order: no items, a
// negative total, a bogus status, a missing id. Then every consumer must
// defensively re-validate, and one that forgets corrupts the database.
//
// A factory gives us a single guarantee that is worth a great deal:
//
//	IF YOU HOLD AN *Order RETURNED FROM HERE, IT IS VALID.
//
// Everything downstream can stop checking. That is the real payoff of the
// pattern — not object creation, but the elimination of a whole class of
// defensive code.
//
// We also compute the total HERE rather than trusting the client. Trusting a
// client-supplied total is a straightforward way to be charged $0.01 for a
// television.
// ============================================================================
func NewOrder(in NewOrderInput, now time.Time) (*Order, error) {
	if err := validateCustomer(in); err != nil {
		return nil, err
	}
	if len(in.Items) == 0 {
		return nil, errs.Invalid("no_items", "an order must contain at least one item")
	}
	if len(in.Items) > constants.MaxItemsPerOrder {
		return nil, errs.Invalid("too_many_items",
			fmt.Sprintf("an order may contain at most %d items", constants.MaxItemsPerOrder)).
			WithDetail("max_items", constants.MaxItemsPerOrder)
	}

	currency := strings.ToUpper(strings.TrimSpace(in.Currency))
	if currency == "" {
		currency = constants.CurrencyDefault
	}

	orderID := idgen.NewUUID()
	items := make([]OrderItem, 0, len(in.Items))
	total := Zero(currency)

	// Merge duplicate product lines. A client sending the same product twice
	// (double-click, or a retried partial request) should get quantity 4, not
	// two separate lines that confuse the warehouse and the invoice.
	seen := map[string]int{} // productID -> index in items

	for i, itemIn := range in.Items {
		if err := validateItem(itemIn, i); err != nil {
			return nil, err
		}
		unit := NewMoney(itemIn.UnitPriceMinor, currency)

		if idx, dup := seen[itemIn.ProductID]; dup {
			merged := items[idx].Quantity + itemIn.Quantity
			if merged > constants.MaxItemsPerOrder*1000 {
				return nil, errs.Invalid("quantity_too_large", "merged quantity is unreasonably large")
			}
			lineTotal, err := unit.MulQty(merged)
			if err != nil {
				return nil, err
			}
			items[idx].Quantity = merged
			items[idx].LineTotal = lineTotal
			continue
		}

		lineTotal, err := unit.MulQty(itemIn.Quantity)
		if err != nil {
			return nil, err
		}
		seen[itemIn.ProductID] = len(items)
		items = append(items, OrderItem{
			ID:          idgen.NewUUID(),
			OrderID:     orderID,
			ProductID:   itemIn.ProductID,
			ProductName: strings.TrimSpace(itemIn.ProductName),
			ProductSKU:  strings.TrimSpace(itemIn.ProductSKU),
			Quantity:    itemIn.Quantity,
			UnitPrice:   unit,
			LineTotal:   lineTotal,
		})
	}

	// Recompute the total from the merged lines, so it cannot disagree with them.
	for _, it := range items {
		var err error
		if total, err = total.Add(it.LineTotal); err != nil {
			return nil, err
		}
	}
	if err := total.Validate(); err != nil {
		return nil, err
	}

	return &Order{
		ID:              orderID,
		OrderNumber:     idgen.NewOrderNumber(),
		CustomerID:      strings.TrimSpace(in.CustomerID),
		CustomerEmail:   strings.ToLower(strings.TrimSpace(in.CustomerEmail)),
		CustomerName:    strings.TrimSpace(in.CustomerName),
		Status:          StatusPending, // every order starts here, always
		Items:           items,
		TotalAmount:     total,
		ShippingAddress: strings.TrimSpace(in.ShippingAddress),
		Notes:           strings.TrimSpace(in.Notes),
		Version:         1,
		CreatedAt:       now,
		UpdatedAt:       now,
		StatusChangedAt: now,
		Source:          SourceNew,
	}, nil
}

func validateCustomer(in NewOrderInput) error {
	if strings.TrimSpace(in.CustomerID) == "" {
		return errs.Invalid("missing_customer_id", "customer_id is required").WithDetail("field", "customer_id")
	}
	email := strings.TrimSpace(in.CustomerEmail)
	if email == "" {
		return errs.Invalid("missing_email", "customer_email is required").WithDetail("field", "customer_email")
	}
	// net/mail is the standard library's RFC-5322 parser. A hand-written
	// email regex is famously, unfixably wrong (it rejects valid addresses
	// and accepts invalid ones); using the parser is both shorter and correct.
	if _, err := mail.ParseAddress(email); err != nil {
		return errs.Invalid("invalid_email", fmt.Sprintf("%q is not a valid email address", email)).
			WithDetail("field", "customer_email")
	}
	if strings.TrimSpace(in.ShippingAddress) == "" {
		return errs.Invalid("missing_address", "shipping_address is required").WithDetail("field", "shipping_address")
	}
	// Bound every free-text field. Unbounded strings mean a client can store
	// a 50MB "note" and turn your row size, your backups and your JSON
	// responses into a problem.
	if len(in.ShippingAddress) > 500 {
		return errs.Invalid("address_too_long", "shipping_address must be at most 500 characters")
	}
	if len(in.Notes) > 1000 {
		return errs.Invalid("notes_too_long", "notes must be at most 1000 characters")
	}
	if len(in.CustomerName) > 200 {
		return errs.Invalid("name_too_long", "customer_name must be at most 200 characters")
	}
	return nil
}

func validateItem(it NewOrderItemInput, index int) error {
	// Include the index in the error so a 20-line order tells the client
	// WHICH line is wrong. Without it, debugging is guesswork.
	field := fmt.Sprintf("items[%d]", index)
	if strings.TrimSpace(it.ProductID) == "" {
		return errs.Invalid("missing_product_id", "each item requires a product_id").WithDetail("field", field+".product_id")
	}
	if strings.TrimSpace(it.ProductName) == "" {
		return errs.Invalid("missing_product_name", "each item requires a product_name").WithDetail("field", field+".product_name")
	}
	if it.Quantity <= 0 {
		// Rejecting <= 0 blocks the classic exploit: a negative quantity makes
		// the line total negative and reduces the order total, so an attacker
		// pays less than the real price, or is owed money.
		return errs.Invalid("invalid_quantity", "quantity must be a positive whole number").
			WithDetail("field", field+".quantity")
	}
	if it.Quantity > 10_000 {
		return errs.Invalid("quantity_too_large", "quantity must be at most 10000").WithDetail("field", field+".quantity")
	}
	if it.UnitPriceMinor <= 0 {
		return errs.Invalid("invalid_price", "unit price must be greater than zero").
			WithDetail("field", field+".unit_price")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Behaviour on the aggregate
// ---------------------------------------------------------------------------
//
// These methods mutate the order AND enforce the rules. They are the reason
// this is a "rich domain model" rather than an anaemic bag of fields.
//
// THE ALTERNATIVE (an "anaemic model") puts all the rules in the service
// layer and leaves the struct as plain data. The cost is that nothing stops
// a future caller from doing `order.Status = StatusDelivered` directly and
// bypassing every rule. Here, the only way to change status is to go through
// a method that validates the transition.

// TransitionTo changes the status after validating the move.
func (o *Order) TransitionTo(target Status, now time.Time) error {
	if err := o.Status.ValidateTransition(target); err != nil {
		return err
	}
	if o.Status == target {
		return nil // idempotent no-op; see ValidateTransition
	}
	o.Status = target
	o.StatusChangedAt = now
	o.UpdatedAt = now
	return nil
}

// Cancel applies the CUSTOMER cancellation rule.
func (o *Order) Cancel(reason string, now time.Time) error {
	if !o.Status.CanBeCancelledByCustomer() {
		if o.Status == StatusCancelled {
			return nil // already cancelled: idempotent, so a retry is fine
		}
		return errs.Unprocessable("cannot_cancel",
			fmt.Sprintf("an order can only be cancelled while PENDING; this order is %s", o.Status)).
			WithDetail("current_status", string(o.Status))
	}
	if err := o.TransitionTo(StatusCancelled, now); err != nil {
		return err
	}
	o.CancelReason = strings.TrimSpace(reason)
	if o.CancelReason == "" {
		o.CancelReason = "cancelled by customer"
	}
	return nil
}

// Fail marks the order as failed after a saga's compensations have run.
func (o *Order) Fail(reason string, now time.Time) error {
	if o.Status.IsTerminal() {
		// Do not overwrite a terminal state. If compensation runs twice (and
		// in a distributed system it will), the second run must be a no-op
		// rather than mangling the record.
		return nil
	}
	if err := o.TransitionTo(StatusFailed, now); err != nil {
		return err
	}
	o.FailureReason = strings.TrimSpace(reason)
	return nil
}

// ItemCount is the total number of physical units (not the number of lines).
// The warehouse cares about units; the invoice shows lines. Naming it
// precisely avoids a whole family of off-by-one reporting bugs.
func (o *Order) ItemCount() int {
	n := 0
	for _, it := range o.Items {
		n += it.Quantity
	}
	return n
}

// RecalculateTotal recomputes the total from the line items and reports
// whether it disagreed with the stored value.
//
// WHY WE KEEP THIS: it is used by an integrity check. A stored total that
// disagrees with the sum of its items means data corruption or a bug, and
// you want to find that in a nightly job, not from a customer complaint.
func (o *Order) RecalculateTotal() (Money, bool, error) {
	total := Zero(o.TotalAmount.Currency)
	for _, it := range o.Items {
		var err error
		if total, err = total.Add(it.LineTotal); err != nil {
			return Money{}, false, err
		}
	}
	return total, total.Minor == o.TotalAmount.Minor, nil
}

// CacheTTL decides how long this order may be cached.
//
// ============================================================================
// AN ADAPTIVE TTL — a small idea with a large effect
// ============================================================================
// A DELIVERED or CANCELLED order is IMMUTABLE: nothing can ever change it.
// So it can be cached for an hour, with zero staleness risk.
//
// A PENDING order could be cancelled a second from now, so it gets a short
// TTL and the window of staleness stays small.
//
// Letting the DOMAIN answer this (rather than hardcoding one TTL in the cache
// layer) is the Single Responsibility Principle paying off: the rule lives
// with the knowledge that justifies it, and the cache just does as it is told.
// ============================================================================
func (o *Order) CacheTTL(defaultTTL time.Duration) time.Duration {
	if o.Status.IsTerminal() {
		return time.Hour
	}
	return defaultTTL
}
