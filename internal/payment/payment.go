// Package payment is the PAYMENT MICROSERVICE.
//
// ============================================================================
// ITS ONE JOB: take money exactly once, and give it back when asked.
//
// "EXACTLY ONCE" is the hard part, and it cannot be achieved by being careful.
// Consider the unavoidable scenario:
//
//	order service -> POST /payments        (we charge the card: SUCCESS)
//	              <- ...network timeout... (the response is lost)
//
// The order service now CANNOT KNOW whether the charge happened. Its only
// options are to retry (risking a double charge) or to give up (risking a
// charge for an order that is never created). Both are wrong.
//
// THE ONLY CORRECT ANSWER IS AN IDEMPOTENCY KEY, and this service is built
// around one. The caller invents a stable key for the LOGICAL operation; we
// store key -> result. A repeat of that key returns the stored result and
// moves no money.
//
// This is exactly how Stripe, Adyen and every serious payment API work, and
// it is the single most important thing to understand about payment
// integration.
// ============================================================================
package payment

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/chahat/order-processing-system/internal/platform/constants"
	"github.com/chahat/order-processing-system/internal/platform/errs"
	"github.com/chahat/order-processing-system/internal/platform/httpx"
	"github.com/chahat/order-processing-system/internal/platform/idgen"
	"github.com/chahat/order-processing-system/internal/platform/logging"
	"github.com/chahat/order-processing-system/internal/platform/metrics"
)

type Payment struct {
	ID           string     `json:"payment_id"`
	OrderID      string     `json:"order_id"`
	CustomerID   string     `json:"customer_id"`
	AmountMinor  int64      `json:"amount_minor"`
	Currency     string     `json:"currency"`
	Captured     bool       `json:"captured"`
	Refunded     bool       `json:"refunded"`
	CreatedAt    time.Time  `json:"created_at"`
	RefundedAt   *time.Time `json:"refunded_at,omitempty"`
	RefundReason string     `json:"refund_reason,omitempty"`
}

// idempotencyRecord stores the OUTCOME of an operation against its key.
//
// NOTE IT STORES THE RESULT, not just "we saw this key". Returning the
// ORIGINAL RESULT is what makes a retry transparent to the caller: they get
// the same payment id they would have got the first time, so their own
// bookkeeping stays consistent.
type idempotencyRecord struct {
	PaymentID string
	Captured  bool
	Reason    string
	// RequestFingerprint detects KEY REUSE WITH DIFFERENT DATA, which is
	// almost always a client bug: the same key sent with a different amount.
	// Silently returning the first result would mean the second, different
	// charge never happens and nobody notices. We reject it loudly instead.
	RequestFingerprint string
	CreatedAt          time.Time
}

type Store struct {
	mu       sync.Mutex
	payments map[string]*Payment
	byKey    map[string]*idempotencyRecord
	log      *slog.Logger
	mtx      *metrics.Registry
}

func NewStore(log *slog.Logger, mtx *metrics.Registry) *Store {
	return &Store{
		payments: map[string]*Payment{},
		byKey:    map[string]*idempotencyRecord{},
		log:      log,
		mtx:      mtx,
	}
}

type ChargeInput struct {
	OrderID        string
	CustomerID     string
	CustomerEmail  string
	AmountMinor    int64
	Currency       string
	IdempotencyKey string
}

// Charge captures a payment, exactly once per idempotency key.
func (s *Store) Charge(ctx context.Context, in ChargeInput) (*Payment, error) {
	log := logging.FromContext(ctx)

	if in.AmountMinor <= 0 {
		return nil, errs.Invalid("invalid_amount", "the charge amount must be positive")
	}
	if in.IdempotencyKey == "" {
		// We REFUSE to process a charge without a key. This is a deliberate
		// hard requirement rather than a convenience default: accepting the
		// request would mean accepting that a retry could double-charge, and
		// there is no amount of care elsewhere that fixes that.
		return nil, errs.Invalid("missing_idempotency_key",
			"an "+constants.HeaderIdempotencyKey+" header is required for charges")
	}

	fingerprint := fingerprintOf(in)

	s.mu.Lock()
	defer s.mu.Unlock()

	// ----------------------------------------------------------------
	// THE IDEMPOTENCY CHECK. This happens FIRST, before any state change.
	// ----------------------------------------------------------------
	if rec, ok := s.byKey[in.IdempotencyKey]; ok {
		if rec.RequestFingerprint != fingerprint {
			// Same key, different request. A 409 is the honest answer: we
			// cannot tell which of the two the caller meant, and guessing
			// risks either a missing charge or a wrong amount.
			log.WarnContext(ctx, "idempotency key reused with different request data",
				slog.String("idempotency_key", in.IdempotencyKey))
			s.mtx.IncCounter("payment_idempotency_mismatch_total",
				"Idempotency keys reused with different payloads")
			return nil, errs.Conflict("idempotency_key_reuse",
				"this idempotency key was already used with different request data")
		}
		log.InfoContext(ctx, "replaying the stored result for a repeated idempotency key",
			slog.String("idempotency_key", in.IdempotencyKey),
			slog.String("payment_id", rec.PaymentID))
		s.mtx.IncCounter("payment_idempotent_replays_total", "Charges served from an idempotency record")

		if !rec.Captured {
			// We replay FAILURES too. A declined card stays declined; charging
			// on the retry would mean the same key produced two different
			// outcomes, which defeats the entire guarantee.
			return nil, errs.Unprocessable("payment_declined", rec.Reason)
		}
		return s.payments[rec.PaymentID], nil
	}

	// ----------------------------------------------------------------
	// THE DEMO DECLINE RULE.
	//
	// Any amount whose cents end in 66 is declined. This gives the e2e test a
	// deterministic way to force a saga failure and prove that compensation
	// runs — a demo you cannot make fail only ever proves the happy path.
	// It is documented in the README so it is a feature, not a mystery.
	// ----------------------------------------------------------------
	if in.AmountMinor%100 == 66 {
		reason := "card declined: insufficient funds"
		// We RECORD THE DECLINE against the key, so a retry replays the
		// decline instead of re-attempting. Not recording failures is a
		// classic idempotency bug: the caller's retry succeeds where the
		// original failed, and the two systems disagree about what happened.
		s.byKey[in.IdempotencyKey] = &idempotencyRecord{
			Captured: false, Reason: reason,
			RequestFingerprint: fingerprint, CreatedAt: time.Now().UTC(),
		}
		log.InfoContext(ctx, "charge declined",
			slog.String("order_id", in.OrderID), slog.Int64("amount_minor", in.AmountMinor))
		s.mtx.IncCounter("payment_declined_total", "Charges declined")
		return nil, errs.Unprocessable("payment_declined", reason)
	}

	pay := &Payment{
		ID:          "pay-" + idgen.NewUUID(),
		OrderID:     in.OrderID,
		CustomerID:  in.CustomerID,
		AmountMinor: in.AmountMinor,
		Currency:    in.Currency,
		Captured:    true,
		CreatedAt:   time.Now().UTC(),
	}
	s.payments[pay.ID] = pay
	// Index by order id as well, so a refund can be issued knowing only the
	// order (which is all the cancellation path has).
	s.payments["order:"+in.OrderID] = pay
	s.byKey[in.IdempotencyKey] = &idempotencyRecord{
		PaymentID: pay.ID, Captured: true,
		RequestFingerprint: fingerprint, CreatedAt: time.Now().UTC(),
	}

	log.InfoContext(ctx, "payment captured",
		slog.String("payment_id", pay.ID),
		slog.String("order_id", in.OrderID),
		slog.Int64("amount_minor", in.AmountMinor),
		slog.String("currency", in.Currency))
	s.mtx.IncCounter("payment_captured_total", "Payments captured", "currency", in.Currency)
	s.mtx.ObserveHistogramWithBuckets("payment_amount_minor", "Captured payment amounts",
		[]float64{500, 2000, 10000, 50000, 250000}, float64(in.AmountMinor))
	return pay, nil
}

// Refund reverses a capture. It is the saga's compensating action for Charge.
func (s *Store) Refund(ctx context.Context, paymentID, idempotencyKey, reason string) error {
	log := logging.FromContext(ctx)

	s.mu.Lock()
	defer s.mu.Unlock()

	pay, ok := s.payments[paymentID]
	if !ok {
		// Nothing to refund. As with inventory's Release, the desired end
		// state already holds, so this is a SUCCESS, not an error. Reporting
		// it as a failure would make correct compensations look broken.
		log.DebugContext(ctx, "refund requested for an unknown payment; treating as a no-op",
			slog.String("payment_id", paymentID))
		return nil
	}

	if pay.Refunded {
		// THE DOUBLE-REFUND GUARD. Without it, a retried compensation gives
		// the customer their money back twice — the mirror image of the
		// double-charge problem, and just as real.
		log.InfoContext(ctx, "payment was already refunded; no action taken",
			slog.String("payment_id", pay.ID))
		s.mtx.IncCounter("payment_refund_replays_total", "Duplicate refund requests ignored")
		return nil
	}

	now := time.Now().UTC()
	pay.Refunded = true
	pay.RefundedAt = &now
	pay.RefundReason = reason

	log.InfoContext(ctx, "payment refunded (saga compensation)",
		slog.String("payment_id", pay.ID),
		slog.String("order_id", pay.OrderID),
		slog.Int64("amount_minor", pay.AmountMinor),
		slog.String("reason", reason))
	s.mtx.IncCounter("payment_refunded_total", "Payments refunded")
	return nil
}

func (s *Store) Get(paymentID string) (*Payment, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.payments[paymentID]
	return p, ok
}

// fingerprintOf builds a stable signature of the request's meaningful fields.
//
// It deliberately EXCLUDES the idempotency key itself (which would make every
// fingerprint trivially match) and any field that may legitimately differ
// between retries.
func fingerprintOf(in ChargeInput) string {
	return in.OrderID + "|" + in.CustomerID + "|" + in.Currency + "|" + itoa64(in.AmountMinor)
}

func itoa64(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [24]byte
	pos := len(b)
	for v > 0 {
		pos--
		b[pos] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}

// ---------------------------------------------------------------------------
// HTTP transport
// ---------------------------------------------------------------------------

type Handler struct {
	store *Store
	log   *slog.Logger
}

func NewHandler(store *Store, log *slog.Logger) *Handler {
	return &Handler{store: store, log: log}
}

func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST "+constants.APIVersion+"/payments", h.charge)
	mux.HandleFunc("POST "+constants.APIVersion+"/payments/{id}/refund", h.refund)
	mux.HandleFunc("GET "+constants.APIVersion+"/payments/{id}", h.get)
}

type chargeRequest struct {
	OrderID       string `json:"order_id"`
	CustomerID    string `json:"customer_id"`
	CustomerEmail string `json:"customer_email"`
	AmountMinor   int64  `json:"amount_minor"`
	Currency      string `json:"currency"`
}

type chargeResponse struct {
	PaymentID string `json:"payment_id"`
	Captured  bool   `json:"captured"`
	Reason    string `json:"reason,omitempty"`
}

func (h *Handler) charge(w http.ResponseWriter, r *http.Request) {
	var req chargeRequest
	if err := httpx.DecodeJSON(w, r, &req, 8<<10); err != nil {
		httpx.Error(w, r, err)
		return
	}

	key := r.Header.Get(constants.HeaderIdempotencyKey)
	if key == "" {
		key = "order:" + req.OrderID + ":charge"
	}

	pay, err := h.store.Charge(r.Context(), ChargeInput{
		OrderID:        req.OrderID,
		CustomerID:     req.CustomerID,
		CustomerEmail:  req.CustomerEmail,
		AmountMinor:    req.AmountMinor,
		Currency:       req.Currency,
		IdempotencyKey: key,
	})
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, chargeResponse{PaymentID: pay.ID, Captured: true})
}

func (h *Handler) refund(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Reason string `json:"reason"`
	}
	if r.ContentLength > 0 {
		if err := httpx.DecodeJSON(w, r, &body, 4<<10); err != nil {
			httpx.Error(w, r, err)
			return
		}
	}
	key := r.Header.Get(constants.HeaderIdempotencyKey)
	if err := h.store.Refund(r.Context(), r.PathValue("id"), key, body.Reason); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.NoContent(w)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	pay, ok := h.store.Get(r.PathValue("id"))
	if !ok {
		httpx.Error(w, r, errs.NotFound("payment_not_found", "no payment exists with that id"))
		return
	}
	httpx.JSON(w, http.StatusOK, pay)
}
