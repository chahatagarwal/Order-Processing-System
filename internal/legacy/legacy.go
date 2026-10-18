// Package legacy is the "OLD" ORDER SYSTEM — the one being strangled.
//
// ============================================================================
// WHY WE BUILT A FAKE LEGACY SYSTEM
// ============================================================================
// The Strangler Fig pattern is meaningless without something to strangle. A
// facade that falls back to nothing cannot be demonstrated, tested, or
// honestly explained.
//
// So this service deliberately exhibits the traits of a real 12-year-old
// system, and each one is a translation problem the adapter has to solve:
//
//   - A DIFFERENT DATA SHAPE: `order_ref` not `id`, `cust_ref` not
//     `customer_id`, `lines` not `items`.
//   - MONEY AS A DECIMAL STRING IN DOLLARS ("1299.00"), not integer cents.
//   - ITS OWN STATUS VOCABULARY: NEW / PICKING / DISPATCHED / COMPLETE / VOID,
//     plus states nobody remembers the meaning of ("AWAIT_FAX").
//   - A NON-RFC3339 TIMESTAMP FORMAT ("2024-03-11 14:22:09").
//   - NO IDEMPOTENCY SUPPORT AT ALL, which is why the adapter must not retry
//     writes against it.
//   - A URL PREFIX OF ITS OWN (/legacy/...), because it predates your API
//     conventions and nobody is going to change it now.
//
// It is pre-seeded with historical orders that exist ONLY here. That is what
// makes the read-fallback demonstrable: GET an old order from the new service
// and watch X-Served-By: legacy come back, then watch it get backfilled.
//
// This service has no cache, no saga, no outbox and no locking — on purpose.
// Its simplicity is the contrast that makes the new system's design visible.
// ============================================================================
package legacy

import (
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/chahat/order-processing-system/internal/platform/errs"
	"github.com/chahat/order-processing-system/internal/platform/httpx"
	"github.com/chahat/order-processing-system/internal/platform/logging"
)

// LegacyOrder is the old system's record shape, warts intact.
type LegacyOrder struct {
	OrderRef      string       `json:"order_ref"`
	CustRef       string       `json:"cust_ref"`
	CustEmail     string       `json:"cust_email"`
	CustName      string       `json:"cust_name"`
	OrderState    string       `json:"order_state"`
	AmountDollars string       `json:"amount_dollars"`
	ShipTo        string       `json:"ship_to"`
	Created       string       `json:"created"`
	Lines         []LegacyLine `json:"lines"`
}

type LegacyLine struct {
	Sku       string `json:"sku"`
	Desc      string `json:"desc"`
	Qty       int    `json:"qty"`
	PriceEach string `json:"price_each"`
}

type Store struct {
	mu     sync.RWMutex
	orders map[string]*LegacyOrder
	log    *slog.Logger
}

func NewStore(log *slog.Logger) *Store {
	s := &Store{orders: map[string]*LegacyOrder{}, log: log}
	s.seed()
	return s
}

// seed loads historical orders that exist ONLY in the legacy system.
//
// These fixed ids are what the e2e test and the README use to demonstrate the
// read-fallback. Using stable, documented ids (rather than random ones) is
// what makes the demo reproducible.
func (s *Store) seed() {
	historical := []*LegacyOrder{
		{
			OrderRef: "LEG-2019-0001", CustRef: "cust-legacy-1",
			CustEmail: "early.adopter@example.com", CustName: "Early Adopter",
			OrderState: "COMPLETE", AmountDollars: "1299.00",
			ShipTo: "1 Old Street, London, EC1V 9HL",
			// Deliberately in the legacy format, to exercise the adapter's
			// timestamp translation.
			Created: "2019-04-02 09:13:44",
			Lines: []LegacyLine{
				{Sku: "SKU-LAPTOP", Desc: "ThinkPad X1 (2019 model)", Qty: 1, PriceEach: "1299.00"},
			},
		},
		{
			OrderRef: "LEG-2021-0042", CustRef: "cust-legacy-2",
			CustEmail: "long.time@example.com", CustName: "Long Time Customer",
			OrderState: "DISPATCHED", AmountDollars: "249.98",
			ShipTo:  "42 Legacy Lane, Manchester, M1 1AA",
			Created: "2021-11-19 16:02:11",
			Lines: []LegacyLine{
				{Sku: "SKU-HEADPHONES", Desc: "Noise Cancelling Headphones", Qty: 2, PriceEach: "124.99"},
			},
		},
		{
			OrderRef: "LEG-2020-0777", CustRef: "cust-legacy-3",
			CustEmail: "mystery@example.com", CustName: "Mystery Order",
			// AN UNRECOGNISED STATE, on purpose. The adapter's default case
			// maps it conservatively to PENDING rather than crashing or
			// hiding the order — which is exactly the defensive behaviour a
			// real migration needs.
			OrderState: "AWAIT_FAX", AmountDollars: "89.50",
			ShipTo:  "7 Forgotten Road, Leeds, LS1 4AB",
			Created: "2020-07-07 07:07:07",
			Lines: []LegacyLine{
				{Sku: "SKU-KEYBOARD", Desc: "Mechanical Keyboard", Qty: 1, PriceEach: "89.50"},
			},
		},
	}
	for _, o := range historical {
		s.orders[o.OrderRef] = o
	}
}

func (s *Store) Get(ref string) (*LegacyOrder, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	// The legacy system was case-insensitive about refs (a behaviour nobody
	// documented but several clients rely on). Preserving such quirks is
	// often necessary during a migration — and finding them is what phase 2
	// dual-running is for.
	o, ok := s.orders[strings.ToUpper(ref)]
	return o, ok
}

func (s *Store) Put(o *LegacyOrder) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.orders[strings.ToUpper(o.OrderRef)] = o
}

func (s *Store) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.orders)
}

// ---------------------------------------------------------------------------
// HTTP transport — note the /legacy prefix, not /api/v1
// ---------------------------------------------------------------------------

type Handler struct {
	store *Store
	log   *slog.Logger
	// latency simulates an old, slow system, so the circuit breaker and
	// timeouts in the calling adapter are exercised by something real rather
	// than only by unit tests.
	latency time.Duration
}

func NewHandler(store *Store, log *slog.Logger, latency time.Duration) *Handler {
	return &Handler{store: store, log: log, latency: latency}
}

func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /legacy/orders/{ref}", h.get)
	mux.HandleFunc("GET /legacy/orders/by-ref/{ref}", h.get)
	mux.HandleFunc("POST /legacy/orders", h.create)
	mux.HandleFunc("GET /legacy/orders", h.list)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	// Simulated legacy slowness.
	if h.latency > 0 {
		select {
		case <-time.After(h.latency):
		case <-r.Context().Done():
			// Honour cancellation even while pretending to be slow, so a
			// client timeout does not leave this handler running.
			return
		}
	}

	ref := r.PathValue("ref")
	order, ok := h.store.Get(ref)
	if !ok {
		httpx.Error(w, r, errs.NotFound("legacy_order_not_found",
			"no order with that reference exists in the legacy system"))
		return
	}
	logging.FromContext(r.Context()).InfoContext(r.Context(), "legacy order served",
		slog.String("order_ref", ref))
	httpx.JSON(w, http.StatusOK, order)
}

// create supports STRANGLER_WRITE_MODE=dual and =legacy.
//
// IT HAS NO IDEMPOTENCY HANDLING, faithfully. That absence is WHY the
// adapter sets Retry:false for this call — a timeout plus a retry here would
// create two orders. The limitation is the lesson.
func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	var o LegacyOrder
	if err := httpx.DecodeJSON(w, r, &o, 32<<10); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if strings.TrimSpace(o.OrderRef) == "" {
		httpx.Error(w, r, errs.Invalid("missing_order_ref", "order_ref is required"))
		return
	}
	h.store.Put(&o)
	h.log.Info("order written to the legacy system (dual-write mode)",
		slog.String("order_ref", o.OrderRef))
	httpx.JSON(w, http.StatusCreated, o)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	h.store.mu.RLock()
	defer h.store.mu.RUnlock()
	out := make([]*LegacyOrder, 0, len(h.store.orders))
	for _, o := range h.store.orders {
		out = append(out, o)
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"orders": out, "count": len(out)})
}
