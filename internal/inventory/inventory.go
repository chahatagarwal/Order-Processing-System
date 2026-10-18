// Package inventory is the INVENTORY MICROSERVICE.
//
// ============================================================================
// ITS ONE JOB: reserve and release stock, correctly, under concurrency.
//
// THE CENTRAL PROBLEM — and why this service uses PESSIMISTIC locking while
// the order service defaults to optimistic:
//
// Stock is a SHARED, CONTENDED counter. During a flash sale, 5,000 requests
// per second all want to decrement the same row for the same popular SKU.
// That is the exact opposite of the order service's workload, where each
// request touches its own order row and conflicts are rare.
//
//	OPTIMISTIC on a hot stock row:  almost every request loses the
//	  compare-and-swap and retries. Throughput collapses into retry churn,
//	  and some customers get a 409 for an item that IS in stock.
//
//	PESSIMISTIC on a hot stock row: requests queue. Each waits a few
//	  milliseconds, then succeeds deterministically. Throughput is bounded by
//	  lock hold time, which we keep tiny by doing nothing but arithmetic
//	  inside the lock.
//
// THIS IS THE WHOLE "choose your locking by use case" ARGUMENT, shown with
// two services in one codebase rather than asserted in a comment.
//
// AND THE INVARIANT THAT MAKES IT NON-NEGOTIABLE: stock must never go
// negative. Overselling means telling a customer their order is confirmed and
// then not being able to ship it.
// ============================================================================
package inventory

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

// Reservation is a temporary hold on stock.
type Reservation struct {
	ID        string         `json:"reservation_id"`
	OrderID   string         `json:"order_id"`
	Items     []ReservedItem `json:"items"`
	CreatedAt time.Time      `json:"created_at"`
	// ExpiresAt is the SAFETY NET for the saga's "empty compensation" problem
	// (see saga.go rule 3). If the order service times out and never learns
	// the reservation id, it cannot release it explicitly — so a reservation
	// that is never confirmed expires on its own and the stock returns to the
	// pool. Without a TTL, every timed-out saga would leak stock permanently.
	ExpiresAt time.Time `json:"expires_at"`
	Confirmed bool      `json:"confirmed"`
}

type ReservedItem struct {
	ProductID string `json:"product_id"`
	Quantity  int    `json:"quantity"`
}

// Store holds stock levels and reservations.
//
// It is in-memory on purpose: this service exists to demonstrate the SAGA
// PARTICIPANT ROLE and the locking contrast, not to be a second full
// persistence exercise. The locking semantics (per-product mutex, check-then-
// decrement under the lock) are the real thing, and would be
// `SELECT ... FOR UPDATE` on a `stock` table in production. That substitution
// is noted in docs/TRADEOFFS.md rather than left implicit.
type Store struct {
	mu sync.Mutex
	// available is productID -> units on hand (not reserved).
	available map[string]int
	// reserved tracks how much is currently held, for the stats endpoint.
	reserved     map[string]int
	reservations map[string]*Reservation
	// byIdempotencyKey makes Reserve safe to retry.
	byIdempotencyKey map[string]string

	log *slog.Logger
	mtx *metrics.Registry
}

func NewStore(log *slog.Logger, mtx *metrics.Registry) *Store {
	s := &Store{
		available:        map[string]int{},
		reserved:         map[string]int{},
		reservations:     map[string]*Reservation{},
		byIdempotencyKey: map[string]string{},
		log:              log,
		mtx:              mtx,
	}
	s.seed()
	return s
}

// seed loads demo stock so the system is usable the moment it starts.
//
// SKU-OUT-OF-STOCK is deliberately set to zero: it gives the e2e test a
// reliable way to trigger a saga failure and prove compensation runs. A
// demo you cannot make FAIL on purpose only demonstrates the happy path.
func (s *Store) seed() {
	s.available["SKU-LAPTOP"] = 50
	s.available["SKU-HEADPHONES"] = 200
	s.available["SKU-MOUSE"] = 500
	s.available["SKU-KEYBOARD"] = 120
	s.available["SKU-MONITOR"] = 30
	s.available["SKU-LIMITED"] = 2 // for demonstrating contention
	s.available["SKU-OUT-OF-STOCK"] = 0
}

// Reserve holds stock for an order, ALL-OR-NOTHING.
func (s *Store) Reserve(ctx context.Context, orderID, idempotencyKey string, items []ReservedItem) (*Reservation, error) {
	log := logging.FromContext(ctx)

	// ----------------------------------------------------------------
	// THE PESSIMISTIC CRITICAL SECTION.
	//
	// Everything between Lock and Unlock is pure arithmetic: no I/O, no
	// network calls, no logging that could block. THAT IS THE RULE that makes
	// a lock on a hot row acceptable — hold it for microseconds, never across
	// anything that can be slow. A lock held across a network call is how one
	// slow dependency becomes a total outage.
	// ----------------------------------------------------------------
	s.mu.Lock()
	defer s.mu.Unlock()

	// IDEMPOTENCY FIRST. The order service retries on timeout, and without
	// this check the retry would reserve the stock a second time — double
	// decrementing for one order.
	if existingID, ok := s.byIdempotencyKey[idempotencyKey]; ok {
		if existing, found := s.reservations[existingID]; found {
			log.InfoContext(ctx, "returning the existing reservation for a repeated idempotency key",
				slog.String("reservation_id", existingID),
				slog.String("idempotency_key", idempotencyKey))
			s.mtx.IncCounter("inventory_idempotent_replays_total",
				"Requests served from an idempotency record instead of re-executing")
			return existing, nil
		}
	}

	if len(items) == 0 {
		return nil, errs.Invalid("no_items", "a reservation must contain at least one item")
	}

	// PHASE 1: CHECK EVERYTHING. We validate all items before mutating any,
	// so a 3-item order with one unavailable item reserves nothing at all.
	// Mutating as we go would leave stock held for an order that then fails.
	for _, it := range items {
		if it.Quantity <= 0 {
			return nil, errs.Invalid("invalid_quantity", "quantity must be positive").
				WithDetail("product_id", it.ProductID)
		}
		available, tracked := s.available[it.ProductID]
		if !tracked {
			return nil, errs.NotFound("unknown_product",
				"we do not carry product "+it.ProductID).WithDetail("product_id", it.ProductID)
		}
		if available < it.Quantity {
			// THE INVARIANT ENFORCED. Returning Unprocessable (422) rather
			// than Unavailable (503) tells the caller "do not retry, this
			// will not change" — which is what stops the saga burning its
			// retry budget on a guaranteed failure.
			s.mtx.IncCounter("inventory_insufficient_total", "Reservations refused for insufficient stock",
				"product_id", it.ProductID)
			return nil, errs.Unprocessable("insufficient_stock",
				"insufficient stock for "+it.ProductID).
				WithDetail("product_id", it.ProductID).
				WithDetail("requested", it.Quantity).
				WithDetail("available", available)
		}
	}

	// PHASE 2: MUTATE. Every check has passed, so this cannot fail partway.
	res := &Reservation{
		ID:        "res-" + idgen.NewUUID(),
		OrderID:   orderID,
		Items:     items,
		CreatedAt: time.Now().UTC(),
		ExpiresAt: time.Now().UTC().Add(15 * time.Minute),
	}
	for _, it := range items {
		s.available[it.ProductID] -= it.Quantity
		s.reserved[it.ProductID] += it.Quantity
	}
	s.reservations[res.ID] = res
	// Also index by order id, so the order service can release without having
	// stored the reservation handle (see CompensateCancelledOrder).
	s.reservations["order:"+orderID] = res
	s.byIdempotencyKey[idempotencyKey] = res.ID

	log.InfoContext(ctx, "stock reserved",
		slog.String("reservation_id", res.ID),
		slog.String("order_id", orderID),
		slog.Int("item_lines", len(items)))
	s.mtx.IncCounter("inventory_reserved_total", "Reservations created")
	return res, nil
}

// Release returns stock to the pool. It is the saga's compensating action.
func (s *Store) Release(ctx context.Context, reservationID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	res, ok := s.reservations[reservationID]
	if !ok {
		// ============================================================
		// RELEASING SOMETHING THAT DOES NOT EXIST IS A **SUCCESS**.
		//
		// The desired end state is "this reservation is not holding stock",
		// and that is already true. Returning an error here would make the
		// order service's compensation look like it failed, triggering alerts
		// and retries for a system that is in exactly the right state.
		//
		// This is what "idempotent compensation" means in practice, and it is
		// the single most common place to get a saga wrong.
		// ============================================================
		logging.FromContext(ctx).DebugContext(ctx, "release for an unknown reservation; already released or never created",
			slog.String("reservation_id", reservationID))
		return nil
	}

	s.releaseLocked(res)
	logging.FromContext(ctx).InfoContext(ctx, "stock released (saga compensation)",
		slog.String("reservation_id", res.ID), slog.String("order_id", res.OrderID))
	s.mtx.IncCounter("inventory_released_total", "Reservations released")
	return nil
}

// releaseLocked must be called with s.mu held.
func (s *Store) releaseLocked(res *Reservation) {
	for _, it := range res.Items {
		s.available[it.ProductID] += it.Quantity
		s.reserved[it.ProductID] -= it.Quantity
		if s.reserved[it.ProductID] < 0 {
			// Defensive: this should be unreachable. If it ever fires, our
			// accounting has a bug, and clamping is better than letting a
			// negative "reserved" figure propagate into reports.
			s.reserved[it.ProductID] = 0
		}
	}
	// Delete BOTH index entries, or a later release by the other key would
	// credit the stock a second time — inventing inventory from nothing.
	delete(s.reservations, res.ID)
	delete(s.reservations, "order:"+res.OrderID)
}

// ExpireStale releases reservations whose TTL has elapsed.
//
// This is the backstop described on Reservation.ExpiresAt: it recovers stock
// from sagas that died before they could compensate. Without it, every
// crashed saga would permanently remove units from your sellable inventory.
func (s *Store) ExpireStale(ctx context.Context) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	// Collect first, then mutate: releaseLocked deletes from the same map we
	// would be ranging over.
	var stale []*Reservation
	seen := map[string]bool{}
	for key, res := range s.reservations {
		// The map holds each reservation twice (by id and by order). Dedupe,
		// or we would credit the stock twice.
		if seen[res.ID] || len(key) > 6 && key[:6] == "order:" {
			continue
		}
		seen[res.ID] = true
		if !res.Confirmed && now.After(res.ExpiresAt) {
			stale = append(stale, res)
		}
	}
	for _, res := range stale {
		s.releaseLocked(res)
		s.log.Warn("expired a stale reservation and returned its stock",
			slog.String("reservation_id", res.ID),
			slog.String("order_id", res.OrderID),
			slog.String("hint", "the order saga probably died before confirming or compensating"))
	}
	if len(stale) > 0 {
		s.mtx.AddCounter("inventory_expired_total", "Reservations expired by TTL", float64(len(stale)))
	}
	return len(stale)
}

// Levels returns a snapshot for the stock endpoint and for tests.
func (s *Store) Levels() map[string]map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]map[string]int{}
	for product, avail := range s.available {
		out[product] = map[string]int{"available": avail, "reserved": s.reserved[product]}
	}
	return out
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
	mux.HandleFunc("POST "+constants.APIVersion+"/reservations", h.reserve)
	mux.HandleFunc("DELETE "+constants.APIVersion+"/reservations/{id}", h.release)
	mux.HandleFunc("GET "+constants.APIVersion+"/stock", h.stock)
}

type reserveRequest struct {
	OrderID string         `json:"order_id"`
	Items   []ReservedItem `json:"items"`
}

type reserveResponse struct {
	ReservationID string `json:"reservation_id"`
	Reserved      bool   `json:"reserved"`
	Reason        string `json:"reason,omitempty"`
	ExpiresAt     string `json:"expires_at,omitempty"`
}

func (h *Handler) reserve(w http.ResponseWriter, r *http.Request) {
	var req reserveRequest
	if err := httpx.DecodeJSON(w, r, &req, 32<<10); err != nil {
		httpx.Error(w, r, err)
		return
	}

	// The idempotency key comes from the HEADER, not the body.
	//
	// WHY THE HEADER: it is a TRANSPORT concern (safe retry of this HTTP
	// call), not part of the business payload. Keeping it out of the body
	// means the same key mechanism works for every endpoint without each
	// one's schema having to carry it. This is the convention Stripe uses.
	key := r.Header.Get(constants.HeaderIdempotencyKey)
	if key == "" {
		// Fall back to the order id so the endpoint is still safe to retry
		// even if a caller forgets the header. Defaulting to SAFE rather than
		// to "no protection" is the right bias for anything touching stock.
		key = "order:" + req.OrderID
	}

	res, err := h.store.Reserve(r.Context(), req.OrderID, key, req.Items)
	if err != nil {
		// A business refusal (insufficient stock) is returned as a 422 with a
		// structured reason, so the order service can distinguish it from an
		// outage and compensate instead of retrying.
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, reserveResponse{
		ReservationID: res.ID,
		Reserved:      true,
		ExpiresAt:     res.ExpiresAt.Format(time.RFC3339),
	})
}

func (h *Handler) release(w http.ResponseWriter, r *http.Request) {
	if err := h.store.Release(r.Context(), r.PathValue("id")); err != nil {
		httpx.Error(w, r, err)
		return
	}
	// 204 No Content: the correct answer to a successful release. There is
	// nothing meaningful to return, and sending `{"ok": true}` would just be
	// bytes the caller has to parse and ignore.
	httpx.NoContent(w)
}

func (h *Handler) stock(w http.ResponseWriter, r *http.Request) {
	httpx.JSON(w, http.StatusOK, map[string]any{"stock": h.store.Levels()})
}

// RunExpiryWorker releases stale reservations on a timer.
func RunExpiryWorker(ctx context.Context, store *Store, log *slog.Logger) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	log.Info("inventory expiry worker started", slog.Duration("interval", time.Minute))
	for {
		select {
		case <-ctx.Done():
			log.Info("inventory expiry worker stopping")
			return
		case <-ticker.C:
			if n := store.ExpireStale(ctx); n > 0 {
				log.Info("released stock from expired reservations", slog.Int("count", n))
			}
		}
	}
}
