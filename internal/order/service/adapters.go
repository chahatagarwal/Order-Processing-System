package service

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/chahat/order-processing-system/internal/order/domain"
	"github.com/chahat/order-processing-system/internal/platform/errs"
	"github.com/chahat/order-processing-system/internal/platform/httpx"
	"github.com/chahat/order-processing-system/internal/platform/idgen"
)

// ============================================================================
// ADAPTERS: the outbound half of Ports & Adapters.
//
// The domain declared WHAT it needs (domain.InventoryPort, domain.PaymentPort).
// This file provides HOW, over HTTP. The ADAPTER PATTERN (structural, GoF)
// in its textbook form: it translates between the interface our domain wants
// and the interface the remote service actually offers.
//
// WHY THE TRANSLATION LAYER EARNS ITS KEEP: if the inventory team renames
// `/reserve` to `/v2/reservations`, or returns `available: false` instead of
// `reserved: true`, we change THIS FILE ONLY. The saga, the service and the
// domain do not know the remote API exists.
// ============================================================================

// ---------------------------------------------------------------------------
// Inventory adapter
// ---------------------------------------------------------------------------

type HTTPInventoryAdapter struct {
	client *httpx.Client
	log    *slog.Logger
}

func NewHTTPInventoryAdapter(baseURL string, timeout time.Duration, log *slog.Logger) *HTTPInventoryAdapter {
	return &HTTPInventoryAdapter{client: httpx.NewClient(baseURL, timeout, log), log: log}
}

// The wire DTOs are PRIVATE to this file.
//
// WHY: if they were exported, the saga would be tempted to use them directly
// and the decoupling would quietly evaporate. Private wire types make the
// boundary enforceable by the compiler rather than by discipline.
type reserveRequestDTO struct {
	OrderID string `json:"order_id"`
	Items   []struct {
		ProductID string `json:"product_id"`
		Quantity  int    `json:"quantity"`
	} `json:"items"`
}

type reserveResponseDTO struct {
	ReservationID string `json:"reservation_id"`
	Reserved      bool   `json:"reserved"`
	Reason        string `json:"reason,omitempty"`
}

func (a *HTTPInventoryAdapter) Reserve(ctx context.Context, req domain.ReservationRequest) (domain.ReservationResult, error) {
	body := reserveRequestDTO{OrderID: req.OrderID}
	for _, it := range req.Items {
		body.Items = append(body.Items, struct {
			ProductID string `json:"product_id"`
			Quantity  int    `json:"quantity"`
		}{ProductID: it.ProductID, Quantity: it.Quantity})
	}

	var out reserveResponseDTO
	err := a.client.Do(ctx, httpx.CallOptions{
		Method:         http.MethodPost,
		Path:           "/api/v1/reservations",
		Body:           body,
		IdempotencyKey: req.IdempotencyKey,
		Out:            &out,
		// Retry is SAFE here precisely BECAUSE we send an idempotency key.
		// Without the key this must be false, or a retry after a timeout
		// double-reserves the stock.
		Retry: true,
	})
	if err != nil {
		// A 422 from inventory means "not enough stock" — a business answer.
		// We convert it into a successful call with Reserved=false so the saga
		// can distinguish "the service is broken" (retry) from "we cannot
		// fulfil this" (compensate and stop). Collapsing those two into one
		// error is how you end up retrying a guaranteed failure 3 times.
		if errs.Is(err, errs.KindUnprocessable) {
			e, _ := errs.As(err)
			return domain.ReservationResult{Reserved: false, Reason: e.Message}, nil
		}
		return domain.ReservationResult{}, err
	}
	return domain.ReservationResult{
		ReservationID: out.ReservationID,
		Reserved:      out.Reserved,
		Reason:        out.Reason,
	}, nil
}

func (a *HTTPInventoryAdapter) Release(ctx context.Context, reservationID, idempotencyKey string) error {
	err := a.client.Do(ctx, httpx.CallOptions{
		Method:         http.MethodDelete,
		Path:           "/api/v1/reservations/" + reservationID,
		IdempotencyKey: idempotencyKey,
		// A compensation MUST retry hard: see rule 4 in saga.go. There is
		// nothing to compensate a failed compensation with.
		Retry: true,
	})
	if err != nil && errs.Is(err, errs.KindNotFound) {
		// Releasing a reservation that does not exist is the DESIRED END
		// STATE, so it is a success. Reporting it as an error would make a
		// correct cancellation look like a failure.
		return nil
	}
	return err
}

// ---------------------------------------------------------------------------
// Payment adapter
// ---------------------------------------------------------------------------

type HTTPPaymentAdapter struct {
	client *httpx.Client
	log    *slog.Logger
}

func NewHTTPPaymentAdapter(baseURL string, timeout time.Duration, log *slog.Logger) *HTTPPaymentAdapter {
	return &HTTPPaymentAdapter{client: httpx.NewClient(baseURL, timeout, log), log: log}
}

type chargeRequestDTO struct {
	OrderID       string `json:"order_id"`
	CustomerID    string `json:"customer_id"`
	CustomerEmail string `json:"customer_email"`
	AmountMinor   int64  `json:"amount_minor"`
	Currency      string `json:"currency"`
}

type chargeResponseDTO struct {
	PaymentID string `json:"payment_id"`
	Captured  bool   `json:"captured"`
	Reason    string `json:"reason,omitempty"`
}

func (a *HTTPPaymentAdapter) Charge(ctx context.Context, req domain.ChargeRequest) (domain.ChargeResult, error) {
	var out chargeResponseDTO
	err := a.client.Do(ctx, httpx.CallOptions{
		Method: http.MethodPost,
		Path:   "/api/v1/payments",
		Body: chargeRequestDTO{
			OrderID:       req.OrderID,
			CustomerID:    req.CustomerID,
			CustomerEmail: req.CustomerEmail,
			AmountMinor:   req.AmountMinor,
			Currency:      req.Currency,
		},
		IdempotencyKey: req.IdempotencyKey,
		Out:            &out,
		// THE MOST IMPORTANT Retry:true IN THE CODEBASE. It is only safe
		// because of the deterministic idempotency key; see the long comment
		// on idempotencyKey() in saga.go for what happens without it.
		Retry: true,
	})
	if err != nil {
		if errs.Is(err, errs.KindUnprocessable) {
			e, _ := errs.As(err)
			// A declined card is the bank's decision. Retrying in 50ms will
			// not change it, so we report it as a business outcome.
			return domain.ChargeResult{Captured: false, Reason: e.Message}, nil
		}
		return domain.ChargeResult{}, err
	}
	return domain.ChargeResult{PaymentID: out.PaymentID, Captured: out.Captured, Reason: out.Reason}, nil
}

func (a *HTTPPaymentAdapter) Refund(ctx context.Context, paymentID, idempotencyKey, reason string) error {
	err := a.client.Do(ctx, httpx.CallOptions{
		Method:         http.MethodPost,
		Path:           "/api/v1/payments/" + paymentID + "/refund",
		Body:           map[string]string{"reason": reason},
		IdempotencyKey: idempotencyKey,
		Retry:          true,
	})
	if err != nil && errs.Is(err, errs.KindNotFound) {
		return nil // nothing to refund == already in the desired state
	}
	return err
}

// ---------------------------------------------------------------------------
// In-process adapters (used when the collaborators are not deployed)
// ---------------------------------------------------------------------------
//
// WHY THESE EXIST: they let the order service run and be demonstrated on its
// own, and they are what the unit tests use. Because they satisfy the same
// ports, the saga code under test is EXACTLY the code that runs in production
// — only the adapter differs. A test that exercises a different code path
// than production is a test that can pass while production is broken.

// StubInventory is an in-memory inventory with real reservation semantics.
type StubInventory struct {
	mu sync.Mutex
	// stock is productID -> available units.
	stock map[string]int
	// reservations is reservationID -> the items it holds.
	reservations map[string][]domain.ReservationItem
	// byKey implements idempotency: the same key returns the same result.
	byKey map[string]string
	log   *slog.Logger
	// FailMode lets a test force a specific failure, which is how we prove
	// the saga compensates. Scenarios that never occur naturally in dev are
	// exactly the ones that must be tested.
	FailMode string // "", "insufficient", "unavailable"
}

func NewStubInventory(log *slog.Logger) *StubInventory {
	return &StubInventory{
		stock:        map[string]int{},
		reservations: map[string][]domain.ReservationItem{},
		byKey:        map[string]string{},
		log:          log,
	}
}

// SetStock seeds availability. An unseeded product is treated as having
// plenty, so demos do not need to pre-register every SKU.
func (s *StubInventory) SetStock(productID string, qty int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stock[productID] = qty
}

func (s *StubInventory) Reserve(ctx context.Context, req domain.ReservationRequest) (domain.ReservationResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch s.FailMode {
	case "insufficient":
		return domain.ReservationResult{Reserved: false, Reason: "insufficient stock (forced by test)"}, nil
	case "unavailable":
		return domain.ReservationResult{}, errs.Unavailable("inventory_down", "the inventory service is unavailable")
	}

	// IDEMPOTENCY CHECK FIRST, before any state change. A retry must observe
	// the original outcome, not create a second reservation.
	if existing, ok := s.byKey[req.IdempotencyKey]; ok {
		return domain.ReservationResult{ReservationID: existing, Reserved: true}, nil
	}

	// Check ALL items before reserving ANY. A partial reservation would leave
	// stock held for an order that cannot be fulfilled — the classic
	// all-or-nothing requirement, enforced by validating first and mutating
	// second.
	for _, it := range req.Items {
		available, tracked := s.stock[it.ProductID]
		if tracked && available < it.Quantity {
			return domain.ReservationResult{
				Reserved: false,
				Reason:   "insufficient stock for product " + it.ProductID,
			}, nil
		}
	}

	resID := "res-" + idgen.NewUUID()
	for _, it := range req.Items {
		if _, tracked := s.stock[it.ProductID]; tracked {
			s.stock[it.ProductID] -= it.Quantity
		}
	}
	s.reservations[resID] = req.Items
	// Index by order id too, so CompensateCancelledOrder can release using
	// "order:<id>" without having stored the reservation handle.
	s.reservations["order:"+req.OrderID] = req.Items
	s.byKey[req.IdempotencyKey] = resID

	return domain.ReservationResult{ReservationID: resID, Reserved: true}, nil
}

func (s *StubInventory) Release(ctx context.Context, reservationID, idempotencyKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	items, ok := s.reservations[reservationID]
	if !ok {
		// Already released, or never existed. Idempotent success.
		return nil
	}
	for _, it := range items {
		if _, tracked := s.stock[it.ProductID]; tracked {
			s.stock[it.ProductID] += it.Quantity
		}
	}
	delete(s.reservations, reservationID)
	return nil
}

// Available reports current stock; used by tests to assert that a
// compensation genuinely restored the units.
func (s *StubInventory) Available(productID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stock[productID]
}

// StubPayment is an in-memory payment processor with idempotency.
type StubPayment struct {
	mu       sync.Mutex
	captured map[string]int64  // paymentID -> amount
	byKey    map[string]string // idempotency key -> paymentID
	refunded map[string]bool
	log      *slog.Logger
	FailMode string // "", "declined", "unavailable"
}

func NewStubPayment(log *slog.Logger) *StubPayment {
	return &StubPayment{
		captured: map[string]int64{},
		byKey:    map[string]string{},
		refunded: map[string]bool{},
		log:      log,
	}
}

func (p *StubPayment) Charge(ctx context.Context, req domain.ChargeRequest) (domain.ChargeResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	switch p.FailMode {
	case "declined":
		return domain.ChargeResult{Captured: false, Reason: "card declined (forced by test)"}, nil
	case "unavailable":
		return domain.ChargeResult{}, errs.Unavailable("payment_down", "the payment service is unavailable")
	}

	if existing, ok := p.byKey[req.IdempotencyKey]; ok {
		// THE IDEMPOTENCY GUARANTEE, demonstrated: the same key returns the
		// original payment and charges nothing more.
		return domain.ChargeResult{PaymentID: existing, Captured: true}, nil
	}

	// A DETERMINISTIC demo rule so the e2e test can force a decline without
	// special configuration: any total ending in .66 is declined. Documented
	// in the README so it is a feature, not a mystery.
	if req.AmountMinor%100 == 66 {
		return domain.ChargeResult{Captured: false, Reason: "card declined (insufficient funds)"}, nil
	}

	payID := "pay-" + idgen.NewUUID()
	p.captured[payID] = req.AmountMinor
	p.captured["order:"+req.OrderID] = req.AmountMinor
	p.byKey[req.IdempotencyKey] = payID
	return domain.ChargeResult{PaymentID: payID, Captured: true}, nil
}

func (p *StubPayment) Refund(ctx context.Context, paymentID, idempotencyKey, reason string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if _, ok := p.captured[paymentID]; !ok {
		return nil // nothing captured: idempotent success
	}
	if p.refunded[paymentID] {
		return nil // DOUBLE-REFUND GUARD. Refunding twice gives money away.
	}
	p.refunded[paymentID] = true
	return nil
}

// WasRefunded lets tests assert that compensation actually issued the refund.
func (p *StubPayment) WasRefunded(handle string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.refunded[handle]
}

// CapturedTotal is used by tests to prove no double charge occurred.
func (p *StubPayment) CapturedCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for k := range p.captured {
		if len(k) > 4 && k[:4] == "pay-" {
			n++
		}
	}
	return n
}

var (
	_ domain.InventoryPort = (*HTTPInventoryAdapter)(nil)
	_ domain.PaymentPort   = (*HTTPPaymentAdapter)(nil)
	_ domain.InventoryPort = (*StubInventory)(nil)
	_ domain.PaymentPort   = (*StubPayment)(nil)
)
