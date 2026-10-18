// Package transport is the HTTP layer: routing, DTOs, and nothing else.
//
// ============================================================================
// WHY DTOs EXIST AT ALL — "can't we just return the domain struct?"
// ============================================================================
// You can, and it is the most common shortcut in web services. Here is what
// it costs:
//
//  1. YOU LEAK DATA. Add an `InternalRiskScore` or `SupplierCost` field to
//     the domain and it appears in the public API instantly, with no review.
//     A DTO means exposing a field is an EXPLICIT, visible decision.
//
//  2. YOUR API BREAKS WHEN YOU REFACTOR. Rename a domain field for clarity
//     and every client integration breaks. The DTO decouples the two: the
//     wire contract is stable while the internals are free to change.
//
//  3. YOU CANNOT SHAPE THE RESPONSE. The domain stores money as int64 cents;
//     an API should offer both `total_minor: 1999` (for machines) and
//     `total: "19.99"` (for humans). That is a presentation concern, and it
//     does not belong in the domain.
//
//  4. YOU LOSE INPUT SAFETY. A request DTO can only express the fields a
//     client is ALLOWED to set. Binding straight onto a domain Order means a
//     crafted request could set `status: DELIVERED` or `version: 99`.
//     This is "mass assignment", a real and recurring vulnerability class.
//
// The cost of DTOs is mapping code. That is a genuine cost, and this file is
// the price. It is worth paying at every public boundary.
// ============================================================================
package transport

import (
	"strings"
	"time"

	"github.com/chahat/order-processing-system/internal/order/domain"
	"github.com/chahat/order-processing-system/internal/platform/errs"
)

// ---------------------------------------------------------------------------
// Requests
// ---------------------------------------------------------------------------

// CreateOrderRequest is the POST /api/v1/orders body.
//
// NOTE WHAT IS ABSENT: no id, no status, no version, no total. The server
// owns all of those. A client cannot set them because the type cannot hold
// them — the compiler enforces the security rule.
type CreateOrderRequest struct {
	CustomerID      string                   `json:"customer_id"`
	CustomerEmail   string                   `json:"customer_email"`
	CustomerName    string                   `json:"customer_name"`
	ShippingAddress string                   `json:"shipping_address"`
	Notes           string                   `json:"notes,omitempty"`
	Currency        string                   `json:"currency,omitempty"`
	Items           []CreateOrderItemRequest `json:"items"`
}

type CreateOrderItemRequest struct {
	ProductID   string `json:"product_id"`
	ProductName string `json:"product_name"`
	ProductSKU  string `json:"product_sku,omitempty"`
	Quantity    int    `json:"quantity"`

	// TWO WAYS TO SEND A PRICE, and the reason is precision:
	//
	//	UnitPriceMinor: 1999      <- integer cents. EXACT. Preferred.
	//	UnitPrice:      "19.99"   <- decimal STRING. Also exact.
	//
	// What we deliberately do NOT accept is a JSON number like 19.99, because
	// every JSON parser turns that into a float64 and the precision is gone
	// before our code runs. See money.go for why that matters.
	UnitPriceMinor int64  `json:"unit_price_minor,omitempty"`
	UnitPrice      string `json:"unit_price,omitempty"`
}

// ToDomain converts the request into the domain's input type.
//
// THIS IS THE ONLY PLACE the two vocabularies meet. Note that it does NOT
// validate business rules — that is the domain factory's job, and duplicating
// it here would mean two sets of rules that drift apart.
func (r CreateOrderRequest) ToDomain() (domain.NewOrderInput, error) {
	currency := strings.ToUpper(strings.TrimSpace(r.Currency))
	if currency == "" {
		currency = "USD"
	}

	in := domain.NewOrderInput{
		CustomerID:      r.CustomerID,
		CustomerEmail:   r.CustomerEmail,
		CustomerName:    r.CustomerName,
		ShippingAddress: r.ShippingAddress,
		Notes:           r.Notes,
		Currency:        currency,
	}

	for i, item := range r.Items {
		minor := item.UnitPriceMinor

		// Accept the decimal-string form, converting exactly.
		if minor == 0 && item.UnitPrice != "" {
			money, err := domain.ParseMoney(item.UnitPrice, currency)
			if err != nil {
				return domain.NewOrderInput{}, errs.Invalid("invalid_price",
					"the unit price could not be parsed").
					WithDetail("field", itemField(i, "unit_price")).
					WithDetail("value", item.UnitPrice).Wrap(err)
			}
			minor = money.Minor
		}

		// REJECT AMBIGUITY: if a client sends both forms and they disagree,
		// guessing which one they meant is how you charge the wrong amount.
		// An explicit error forces them to be unambiguous.
		if item.UnitPriceMinor != 0 && item.UnitPrice != "" {
			money, err := domain.ParseMoney(item.UnitPrice, currency)
			if err == nil && money.Minor != item.UnitPriceMinor {
				return domain.NewOrderInput{}, errs.Invalid("conflicting_price",
					"unit_price and unit_price_minor disagree; send only one").
					WithDetail("field", itemField(i, "unit_price"))
			}
		}

		in.Items = append(in.Items, domain.NewOrderItemInput{
			ProductID:      item.ProductID,
			ProductName:    item.ProductName,
			ProductSKU:     item.ProductSKU,
			Quantity:       item.Quantity,
			UnitPriceMinor: minor,
		})
	}
	return in, nil
}

func itemField(i int, name string) string {
	return "items[" + itoa(i) + "]." + name
}

// itoa avoids importing strconv for one call; small and obvious.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}

// UpdateStatusRequest is the PATCH /orders/{id}/status body.
type UpdateStatusRequest struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// CancelOrderRequest is the POST /orders/{id}/cancel body.
type CancelOrderRequest struct {
	Reason string `json:"reason,omitempty"`
}

// ---------------------------------------------------------------------------
// Responses
// ---------------------------------------------------------------------------

// OrderResponse is the public representation of an order.
type OrderResponse struct {
	ID          string `json:"id"`
	OrderNumber string `json:"order_number"`

	CustomerID    string `json:"customer_id"`
	CustomerEmail string `json:"customer_email"`
	CustomerName  string `json:"customer_name,omitempty"`

	Status string `json:"status"`
	// StatusDescription spells out what the status MEANS. An API that returns
	// only "PROCESSING" forces every frontend to hardcode its own glossary,
	// and they all end up slightly different. Sending it once, from the
	// server, keeps every client consistent.
	StatusDescription string `json:"status_description"`
	// IsCancellable saves clients from reimplementing our business rule.
	// If they reimplement it, they WILL get it wrong, and their "Cancel"
	// button will 422 in the user's face. Server-computed capability flags
	// are a small kindness with a large payoff.
	IsCancellable bool `json:"is_cancellable"`
	IsFinal       bool `json:"is_final"`

	TotalMinor int64  `json:"total_minor"` // exact, for machines
	Total      string `json:"total"`       // formatted, for humans
	Currency   string `json:"currency"`

	Items     []OrderItemResponse `json:"items"`
	ItemCount int                 `json:"item_count"` // total UNITS, not lines

	ShippingAddress string `json:"shipping_address"`
	Notes           string `json:"notes,omitempty"`
	CancelReason    string `json:"cancel_reason,omitempty"`
	FailureReason   string `json:"failure_reason,omitempty"`

	// Version is EXPOSED on purpose: it lets a client do its own optimistic
	// concurrency control ("update only if still at version 3"), which is how
	// you stop two admin tabs silently overwriting each other.
	Version int `json:"version"`

	// Source tells you which system owns this record — the Strangler Fig
	// migration made visible in the API.
	Source string `json:"source"`

	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
	StatusChangedAt time.Time `json:"status_changed_at"`
}

type OrderItemResponse struct {
	ID          string `json:"id"`
	ProductID   string `json:"product_id"`
	ProductName string `json:"product_name"`
	ProductSKU  string `json:"product_sku,omitempty"`
	Quantity    int    `json:"quantity"`

	UnitPriceMinor int64  `json:"unit_price_minor"`
	UnitPrice      string `json:"unit_price"`
	LineTotalMinor int64  `json:"line_total_minor"`
	LineTotal      string `json:"line_total"`
}

// NewOrderResponse maps domain -> wire.
func NewOrderResponse(o *domain.Order) OrderResponse {
	resp := OrderResponse{
		ID:                o.ID,
		OrderNumber:       o.OrderNumber,
		CustomerID:        o.CustomerID,
		CustomerEmail:     o.CustomerEmail,
		CustomerName:      o.CustomerName,
		Status:            string(o.Status),
		StatusDescription: statusDescription(o.Status),
		IsCancellable:     o.Status.CanBeCancelledByCustomer(),
		IsFinal:           o.Status.IsTerminal(),
		TotalMinor:        o.TotalAmount.Minor,
		Total:             o.TotalAmount.MajorString(),
		Currency:          o.TotalAmount.Currency,
		ItemCount:         o.ItemCount(),
		ShippingAddress:   o.ShippingAddress,
		Notes:             o.Notes,
		CancelReason:      o.CancelReason,
		FailureReason:     o.FailureReason,
		Version:           o.Version,
		Source:            o.Source,
		CreatedAt:         o.CreatedAt,
		UpdatedAt:         o.UpdatedAt,
		StatusChangedAt:   o.StatusChangedAt,
	}

	// Initialise to an EMPTY SLICE, not nil.
	//
	// WHY THIS ONE LINE MATTERS: a nil slice marshals to `null`, an empty
	// slice marshals to `[]`. Clients then crash on `order.items.length` or
	// `for item in order.items`. Returning `[]` for "no items" is the correct,
	// kind behaviour, and it is a two-minute bug that reaches production
	// constantly.
	resp.Items = make([]OrderItemResponse, 0, len(o.Items))
	for _, it := range o.Items {
		resp.Items = append(resp.Items, OrderItemResponse{
			ID:             it.ID,
			ProductID:      it.ProductID,
			ProductName:    it.ProductName,
			ProductSKU:     it.ProductSKU,
			Quantity:       it.Quantity,
			UnitPriceMinor: it.UnitPrice.Minor,
			UnitPrice:      it.UnitPrice.MajorString(),
			LineTotalMinor: it.LineTotal.Minor,
			LineTotal:      it.LineTotal.MajorString(),
		})
	}
	return resp
}

// statusDescription is the server-side glossary referenced above.
func statusDescription(s domain.Status) string {
	switch s {
	case domain.StatusPending:
		return "Order received and awaiting processing. It can still be cancelled."
	case domain.StatusProcessing:
		return "Stock is reserved, payment is captured, and the warehouse is preparing your order."
	case domain.StatusShipped:
		return "Your order has left the warehouse and is on its way."
	case domain.StatusDelivered:
		return "Your order has been delivered. This order is complete."
	case domain.StatusCancelled:
		return "This order was cancelled. Any payment taken has been refunded."
	case domain.StatusFailed:
		return "This order could not be completed. Any stock and payment have been released."
	default:
		return "Unknown status."
	}
}

// ListOrdersResponse wraps a page of results.
//
// WHY AN ENVELOPE AND NOT A BARE JSON ARRAY:
//  1. There is nowhere to put pagination metadata in a bare array.
//  2. You can never add a field later without breaking every client.
//  3. A top-level JSON array is a (historical) XSS vector in some browsers.
//
// An envelope costs one extra level of nesting and buys permanent extensibility.
type ListOrdersResponse struct {
	Orders     []OrderResponse `json:"orders"`
	Pagination Pagination      `json:"pagination"`
}

type Pagination struct {
	Total   int  `json:"total"`
	Limit   int  `json:"limit"`
	Offset  int  `json:"offset"`
	HasMore bool `json:"has_more"`
	// NextOffset is pre-computed so a client does not have to do arithmetic
	// (and get it wrong). It is null on the last page, which makes "stop
	// paging" unambiguous.
	NextOffset *int `json:"next_offset"`
}

func NewListOrdersResponse(result domain.ListResult) ListOrdersResponse {
	resp := ListOrdersResponse{
		Orders: make([]OrderResponse, 0, len(result.Orders)),
		Pagination: Pagination{
			Total:   result.Total,
			Limit:   result.Limit,
			Offset:  result.Offset,
			HasMore: result.HasMore,
		},
	}
	for i := range result.Orders {
		resp.Orders = append(resp.Orders, NewOrderResponse(&result.Orders[i]))
	}
	if result.HasMore {
		next := result.Offset + result.Limit
		resp.Pagination.NextOffset = &next
	}
	return resp
}

// StatsResponse is the dashboard payload.
type StatsResponse struct {
	TotalOrders   int            `json:"total_orders"`
	CountByStatus map[string]int `json:"count_by_status"`
	RevenueMinor  int64          `json:"revenue_minor"`
	Revenue       string         `json:"revenue"`
	AverageMinor  int64          `json:"average_order_minor"`
	Average       string         `json:"average_order"`
	Currency      string         `json:"currency"`
	GeneratedAt   time.Time      `json:"generated_at"`
	// Note explains what the numbers mean. A dashboard that shows "revenue"
	// without saying whether cancelled orders are included invites someone to
	// misread it in a meeting.
	Note string `json:"note"`
}

func NewStatsResponse(s domain.Stats) StatsResponse {
	counts := make(map[string]int, len(s.CountByStatus))
	for k, v := range s.CountByStatus {
		counts[string(k)] = v
	}
	return StatsResponse{
		TotalOrders:   s.TotalOrders,
		CountByStatus: counts,
		RevenueMinor:  s.TotalRevenue.Minor,
		Revenue:       s.TotalRevenue.MajorString(),
		AverageMinor:  s.AverageOrder.Minor,
		Average:       s.AverageOrder.MajorString(),
		Currency:      s.TotalRevenue.Currency,
		GeneratedAt:   s.GeneratedAt,
		Note:          "Revenue excludes CANCELLED and FAILED orders.",
	}
}
