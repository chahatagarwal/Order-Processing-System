package transport

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/chahat/order-processing-system/internal/order/domain"
	"github.com/chahat/order-processing-system/internal/order/service"
	"github.com/chahat/order-processing-system/internal/platform/constants"
	"github.com/chahat/order-processing-system/internal/platform/errs"
	"github.com/chahat/order-processing-system/internal/platform/httpx"
	"github.com/chahat/order-processing-system/internal/platform/logging"
)

// Handler holds the HTTP endpoints for the order service.
//
// ============================================================================
// WHAT A HANDLER SHOULD AND SHOULD NOT DO
// ============================================================================
// A handler has exactly four jobs:
//
//  1. PARSE    the HTTP request into a DTO.
//  2. CONVERT  the DTO into domain input.
//  3. CALL     one service method.
//  4. RENDER   the result (or the error) as HTTP.
//
// If a handler contains an `if` about BUSINESS RULES, it is in the wrong
// place. "Can this order be cancelled?" is a domain question, not an HTTP one.
//
// THE TEST FOR WHETHER YOU GOT THIS RIGHT: could you add a CLI or a gRPC API
// without duplicating logic? Here, yes — because all the logic is in the
// service and the domain, and this file is a thin translation shell.
// ============================================================================
type Handler struct {
	svc *service.OrderService
	// strangler is optional. When present, reads fall back to the legacy
	// system; when nil, we read only the new store.
	strangler *service.StranglerFacade
	log       *slog.Logger
}

func NewHandler(svc *service.OrderService, strangler *service.StranglerFacade, log *slog.Logger) *Handler {
	return &Handler{svc: svc, strangler: strangler, log: log}
}

// Register wires routes onto the mux.
//
// WE USE THE STANDARD LIBRARY'S ServeMux (Go 1.22+), which now supports
// method-aware patterns and path variables:
//
//	mux.HandleFunc("PATCH /api/v1/orders/{id}/status", h.updateStatus)
//
// WHY NOT chi / gin / echo: for ~12 routes they would add a dependency, a
// different idiom to learn, and a layer between you and net/http — for
// features we do not use. The stdlib router is enough, and "enough" is the
// YAGNI answer. If we needed per-route middleware groups or regex
// constraints, a router would start to pay for itself.
func (h *Handler) Register(mux *http.ServeMux) {
	base := constants.APIVersion + "/orders"

	mux.HandleFunc("POST "+base, h.createOrder)
	mux.HandleFunc("GET "+base, h.listOrders)

	// ROUTE ORDERING NOTE: Go's ServeMux matches the MOST SPECIFIC pattern,
	// not the first registered one. So "/orders/stats" wins over
	// "/orders/{id}" even though it is registered after it. With older
	// routers (and with regex routers) a literal path must be registered
	// BEFORE the wildcard, or "stats" is parsed as an order id — a classic
	// routing bug worth knowing about.
	mux.HandleFunc("GET "+base+"/stats", h.stats)
	mux.HandleFunc("GET "+base+"/number/{number}", h.getByOrderNumber)
	mux.HandleFunc("GET "+base+"/{id}", h.getOrder)
	mux.HandleFunc("PATCH "+base+"/{id}/status", h.updateStatus)
	mux.HandleFunc("POST "+base+"/{id}/cancel", h.cancelOrder)

	// Admin/diagnostic endpoints. In production these sit behind
	// authentication; they are open here so the demo and the e2e test can
	// inspect internal state, which is called out in the README rather than
	// left as a surprise.
	mux.HandleFunc("GET "+constants.APIVersion+"/admin/strangler", h.stranglerStats)
	mux.HandleFunc("GET "+constants.APIVersion+"/admin/sagas/{orderID}", h.sagaState)
}

// ---------------------------------------------------------------------------
// POST /api/v1/orders
// ---------------------------------------------------------------------------

func (h *Handler) createOrder(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req CreateOrderRequest
	// 64KiB is generous for an order (50 items max) and bounds what a
	// malicious client can make us allocate.
	if err := httpx.DecodeJSON(w, r, &req, 64<<10); err != nil {
		httpx.Error(w, r, err)
		return
	}

	in, err := req.ToDomain()
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	order, err := h.svc.Create(ctx, in)
	if err != nil {
		// IMPORTANT SUBTLETY: when the SAGA fails, Create returns BOTH a
		// non-nil order and an error. The order row exists (as FAILED) and
		// the client needs its id to query or reference it. So we include the
		// order in the error response rather than discarding it — otherwise
		// the customer is told "something failed" with no reference at all.
		if order != nil {
			w.Header().Set("X-Order-Id", order.ID)
		}
		httpx.Error(w, r, err)
		return
	}

	// 201 Created + a Location header is the correct REST answer for a
	// successful creation; it tells the client exactly where the new resource
	// lives without them having to construct the URL.
	w.Header().Set("Location", constants.APIVersion+"/orders/"+order.ID)
	httpx.JSON(w, http.StatusCreated, NewOrderResponse(order))
}

// ---------------------------------------------------------------------------
// GET /api/v1/orders/{id}
// ---------------------------------------------------------------------------

func (h *Handler) getOrder(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")

	// Route through the Strangler Fig facade when it is enabled, so a
	// historical order still resolves from the legacy system.
	if h.strangler != nil {
		order, servedBy, err := h.strangler.GetByIDTraced(ctx, id)
		if err != nil {
			httpx.Error(w, r, err)
			return
		}
		// Surfacing WHICH backend answered makes the migration testable from
		// the outside — the e2e script asserts on this header.
		httpx.SetServedBy(w, string(servedBy))
		h.setCacheHeaders(w, order)
		httpx.JSON(w, http.StatusOK, NewOrderResponse(order))
		return
	}

	order, err := h.svc.GetByID(ctx, id)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	h.setCacheHeaders(w, order)
	httpx.JSON(w, http.StatusOK, NewOrderResponse(order))
}

func (h *Handler) getByOrderNumber(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	number := r.PathValue("number")

	if h.strangler != nil {
		order, servedBy, err := h.strangler.GetByOrderNumberTraced(ctx, number)
		if err != nil {
			httpx.Error(w, r, err)
			return
		}
		httpx.SetServedBy(w, string(servedBy))
		h.setCacheHeaders(w, order)
		httpx.JSON(w, http.StatusOK, NewOrderResponse(order))
		return
	}

	order, err := h.svc.GetByOrderNumber(ctx, number)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	h.setCacheHeaders(w, order)
	httpx.JSON(w, http.StatusOK, NewOrderResponse(order))
}

// setCacheHeaders adds HTTP-level caching instructions.
//
// ============================================================================
// THIS IS A THIRD LAYER OF CACHING, and it is the cheapest one of all.
//
//	LAYER 1  HTTP caching (here)      — the browser/CDN does not even call us.
//	LAYER 2  Redis (shared)           — we do not call the database.
//	LAYER 3  Postgres                 — the source of truth.
//
// A TERMINAL order is immutable, so we can tell the client to cache it for an
// hour and make the request disappear entirely. A PENDING order could change
// in a second, so `no-store` keeps the customer from seeing a stale status.
//
// The ETag enables CONDITIONAL REQUESTS: a client sends
// If-None-Match: "v3" and we can answer 304 Not Modified with an empty body,
// saving the bandwidth but not the round-trip. Version makes a perfect ETag
// because it changes on exactly every mutation.
// ============================================================================
func (h *Handler) setCacheHeaders(w http.ResponseWriter, o *domain.Order) {
	w.Header().Set("ETag", `"`+strconv.Itoa(o.Version)+`"`)
	if o.Status.IsTerminal() {
		// private, not public: an order contains personal data and must never
		// be cached in a SHARED CDN cache where another user could be served
		// someone else's order. Getting this directive wrong is a genuine
		// data-leak mechanism.
		w.Header().Set("Cache-Control", "private, max-age=3600")
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
}

// ---------------------------------------------------------------------------
// GET /api/v1/orders
// ---------------------------------------------------------------------------

func (h *Handler) listOrders(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()

	filter := domain.ListFilter{
		CustomerID: strings.TrimSpace(q.Get("customer_id")),
		SortBy:     q.Get("sort_by"),
		SortDir:    q.Get("sort_dir"),
	}

	if raw := strings.TrimSpace(q.Get("status")); raw != "" {
		status := domain.Status(strings.ToUpper(raw))
		if !status.IsValid() {
			// A clear 400 that LISTS the valid values. "Invalid status" alone
			// makes the client guess; including the allowed set means they fix
			// it in one attempt.
			httpx.Error(w, r, errs.Invalid("invalid_status",
				"the status filter is not a recognised order status").
				WithDetail("provided", raw).
				WithDetail("allowed", domain.AllStatuses()))
			return
		}
		filter.Status = &status
	}

	// Accept both `limit/offset` and the friendlier `page/size`, because
	// different clients expect different conventions and translating here is
	// cheaper than arguing about it.
	filter.Limit = parseIntDefault(q.Get("limit"), parseIntDefault(q.Get("size"), constants.DefaultPageSize))
	filter.Offset = parseIntDefault(q.Get("offset"), 0)
	if page := parseIntDefault(q.Get("page"), -1); page >= 0 && q.Get("offset") == "" {
		filter.Offset = page * filter.Limit
	}

	if raw := q.Get("created_after"); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			httpx.Error(w, r, errs.Invalid("invalid_date",
				"created_after must be an RFC3339 timestamp, e.g. 2026-01-15T10:00:00Z").
				WithDetail("provided", raw))
			return
		}
		filter.CreatedAfter = &t
	}
	if raw := q.Get("created_before"); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			httpx.Error(w, r, errs.Invalid("invalid_date",
				"created_before must be an RFC3339 timestamp").WithDetail("provided", raw))
			return
		}
		filter.CreatedBefore = &t
	}

	result, err := h.svc.List(ctx, filter)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, NewListOrdersResponse(result))
}

// parseIntDefault parses an integer, falling back to def.
//
// It IGNORES malformed input rather than erroring. For pagination that is the
// right call: `?limit=abc` from a buggy client should still return a usable
// first page, not a 400 that breaks their whole screen. (Contrast with
// `?status=abc`, which we DO reject — silently ignoring a filter would return
// data the caller did not ask for, which is far worse than a default page size.)
func parseIntDefault(raw string, def int) int {
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	return n
}

// ---------------------------------------------------------------------------
// PATCH /api/v1/orders/{id}/status
// ---------------------------------------------------------------------------

func (h *Handler) updateStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")

	var req UpdateStatusRequest
	if err := httpx.DecodeJSON(w, r, &req, 8<<10); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if strings.TrimSpace(req.Status) == "" {
		httpx.Error(w, r, errs.Invalid("missing_status", "status is required").
			WithDetail("allowed", domain.AllStatuses()))
		return
	}

	// Uppercase the input so "shipped" works as well as "SHIPPED". Being
	// lenient about case costs nothing and removes a whole category of
	// support question.
	target := domain.Status(strings.ToUpper(strings.TrimSpace(req.Status)))

	order, err := h.svc.UpdateStatus(ctx, id, target, req.Reason)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, NewOrderResponse(order))
}

// ---------------------------------------------------------------------------
// POST /api/v1/orders/{id}/cancel
// ---------------------------------------------------------------------------

// WHY POST AND NOT DELETE: cancelling is a STATE TRANSITION, not a deletion.
// The order row must survive for accounting, audit and support. `DELETE`
// would imply the resource is gone, and a client library might reasonably
// stop showing it. POST to a /cancel sub-resource says exactly what happens.
func (h *Handler) cancelOrder(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")

	// The body is OPTIONAL here. A cancel with no reason is perfectly valid,
	// so an empty body must not be an error.
	var req CancelOrderRequest
	if r.ContentLength > 0 {
		if err := httpx.DecodeJSON(w, r, &req, 8<<10); err != nil {
			httpx.Error(w, r, err)
			return
		}
	}

	order, err := h.svc.Cancel(ctx, id, req.Reason)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, NewOrderResponse(order))
}

// ---------------------------------------------------------------------------
// GET /api/v1/orders/stats
// ---------------------------------------------------------------------------

func (h *Handler) stats(w http.ResponseWriter, r *http.Request) {
	stats, err := h.svc.Stats(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, NewStatsResponse(stats))
}

// ---------------------------------------------------------------------------
// Admin / diagnostics
// ---------------------------------------------------------------------------

// stranglerStats answers "how far through the migration are we?".
func (h *Handler) stranglerStats(w http.ResponseWriter, r *http.Request) {
	if h.strangler == nil {
		httpx.JSON(w, http.StatusOK, map[string]any{
			"enabled": false,
			"note":    "The Strangler Fig facade is disabled; all reads go to the new store.",
		})
		return
	}
	httpx.JSON(w, http.StatusOK, h.strangler.MigrationStats())
}

// sagaState shows which saga step an order died on — the first thing anyone
// wants to know about a stuck order, and the thing that is otherwise buried
// in logs.
func (h *Handler) sagaState(w http.ResponseWriter, r *http.Request) {
	orderID := r.PathValue("orderID")
	states := h.svc.SagaStatesForOrder(orderID)
	if len(states) == 0 {
		httpx.JSON(w, http.StatusOK, map[string]any{
			"order_id": orderID,
			"sagas":    []any{},
			"note":     "No saga recorded. Either sagas are disabled, or this order predates the current process (saga state is in-memory).",
		})
		return
	}
	logging.FromContext(r.Context()).DebugContext(r.Context(), "saga state inspected",
		slog.String("order_id", orderID), slog.Int("sagas", len(states)))
	httpx.JSON(w, http.StatusOK, map[string]any{"order_id": orderID, "sagas": states})
}
