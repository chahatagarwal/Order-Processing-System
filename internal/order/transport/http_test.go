package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chahat/order-processing-system/internal/order/domain"
	"github.com/chahat/order-processing-system/internal/order/repository"
	"github.com/chahat/order-processing-system/internal/order/service"
	"github.com/chahat/order-processing-system/internal/platform/cache"
	"github.com/chahat/order-processing-system/internal/platform/config"
	"github.com/chahat/order-processing-system/internal/platform/constants"
	"github.com/chahat/order-processing-system/internal/platform/httpx"
	"github.com/chahat/order-processing-system/internal/platform/logging"
	"github.com/chahat/order-processing-system/internal/platform/metrics"
)

// ============================================================================
// HTTP INTEGRATION TESTS
//
// These drive the system through its REAL HTTP surface: the real router, the
// real middleware chain, the real DTO mapping, the real cache decorator, the
// real service, the real saga and the real domain. Only Postgres and the two
// remote participants are in-process substitutes.
//
// WHY THIS LAYER OF TEST EARNS ITS KEEP ON TOP OF THE UNIT TESTS:
// the unit tests prove the logic is right. These prove the WIRING is right —
// status codes, JSON field names, route patterns, header behaviour, middleware
// ordering. Those are exactly the things that are correct in isolation and
// wrong when assembled, and they are what a client actually consumes.
//
// They use httptest.Server, so there is no port to bind, nothing to clean up,
// and they run in parallel with everything else.
// ============================================================================

type harness struct {
	server *httptest.Server
	repo   *repository.MemoryRepository
	cache  *cache.MemoryCache
	inv    *service.StubInventory
	pay    *service.StubPayment
	legacy *legacyFake
}

func (h *harness) Close() { h.server.Close() }

func newHarness(t *testing.T, withStrangler bool) *harness {
	t.Helper()

	log := logging.Discard()
	mtx := metrics.NewRegistry()
	clock := domain.SystemClock{}

	memRepo := repository.NewMemoryRepository()
	memCache := cache.NewMemory(100, 0) // no janitor: tests control time explicitly

	// THE REAL DECORATOR CHAIN, as assembled in main().
	repo := domain.OrderRepository(repository.NewCachedRepository(
		memRepo, memCache, log, 5*time.Minute, 30*time.Second))

	inv := service.NewStubInventory(log)
	pay := service.NewStubPayment(log)

	saga := service.NewSagaOrchestrator(repo, inv, pay, clock, log,
		config.SagaConfig{Enabled: true, StepTimeout: 2 * time.Second}, mtx)

	svc := service.NewOrderService(repo, clock, log, config.LockingConfig{
		Strategy:           config.LockHybrid,
		MaxRetries:         3,
		RetryBase:          time.Millisecond,
		PessimisticTimeout: 2 * time.Second,
	}, mtx, saga)

	// legacyFake stands in for the legacy order service, so the Strangler Fig
	// read-fallback is exercised without running a second process.
	var strangler *service.StranglerFacade
	legacy := &legacyFake{orders: map[string]*domain.Order{}}
	if withStrangler {
		strangler = service.NewStranglerFacade(repo, legacy, config.StranglerConfig{
			Enabled:              true,
			ReadFallbackToLegacy: true,
			BackfillOnRead:       true,
			WriteMode:            config.WriteModeNew,
		}, log, mtx)
	}

	mux := http.NewServeMux()
	NewHandler(svc, strangler, log).Register(mux)

	// The REAL middleware chain, in the real order. Testing without it would
	// miss panic recovery, request ids and the timeout — the cross-cutting
	// behaviour clients depend on.
	handler := httpx.Chain(mux,
		httpx.Recover(log),
		httpx.NormalizeErrors(),
		httpx.RequestID(),
		httpx.Logger(log),
		httpx.Metrics(mtx),
		httpx.Timeout(10*time.Second),
	)

	h := &harness{
		server: httptest.NewServer(handler),
		repo:   memRepo,
		cache:  memCache,
		inv:    inv,
		pay:    pay,
		legacy: legacy,
	}
	t.Cleanup(h.Close)
	return h
}

// ---------------------------------------------------------------------------
// Small HTTP helpers
// ---------------------------------------------------------------------------

func (h *harness) do(t *testing.T, method, path string, body any) (*http.Response, map[string]any) {
	t.Helper()

	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}

	req, err := http.NewRequest(method, h.server.URL+path, reader)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()

	var decoded map[string]any
	if resp.StatusCode != http.StatusNoContent {
		if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil && err.Error() != "EOF" {
			t.Fatalf("decode response from %s %s: %v", method, path, err)
		}
	}
	return resp, decoded
}

func validOrderBody() map[string]any {
	return map[string]any{
		"customer_id":      "cust-1001",
		"customer_email":   "john.doe@example.com",
		"customer_name":    "John Doe",
		"shipping_address": "123 Main St, New York, NY 10001",
		"currency":         "USD",
		"items": []map[string]any{
			{
				"product_id":       "SKU-HEADPHONES",
				"product_name":     "Wireless Headphones",
				"product_sku":      "WH-001",
				"quantity":         2,
				"unit_price_minor": 4999,
			},
		},
	}
}

func (h *harness) createOrder(t *testing.T) map[string]any {
	t.Helper()
	resp, body := h.do(t, http.MethodPost, constants.APIVersion+"/orders", validOrderBody())
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create order: got %d, want 201 (body: %v)", resp.StatusCode, body)
	}
	return body
}

func errorCode(body map[string]any) string {
	if e, ok := body["error"].(map[string]any); ok {
		if code, ok := e["code"].(string); ok {
			return code
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// POST /api/v1/orders
// ---------------------------------------------------------------------------

func TestHTTP_CreateOrder(t *testing.T) {
	h := newHarness(t, false)

	resp, body := h.do(t, http.MethodPost, constants.APIVersion+"/orders", validOrderBody())

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status: got %d, want 201 (body %v)", resp.StatusCode, body)
	}
	// 201 must carry a Location header pointing at the new resource.
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, "/orders/") {
		t.Errorf("Location header: got %q", loc)
	}
	// Every response must carry a request id, for support correlation.
	if resp.Header.Get(constants.HeaderRequestID) == "" {
		t.Error("the response should carry an X-Request-Id header")
	}

	// The server computed the total; the client never sent one.
	if got := body["total_minor"].(float64); got != 9998 {
		t.Errorf("total_minor: got %v, want 9998", got)
	}
	if got := body["total"].(string); got != "99.98" {
		t.Errorf("total: got %q, want %q", got, "99.98")
	}
	// The saga ran, so the order is PROCESSING rather than PENDING.
	if got := body["status"].(string); got != "PROCESSING" {
		t.Errorf("status: got %q, want PROCESSING", got)
	}
	// The server-side glossary and capability flags must be present.
	if body["status_description"].(string) == "" {
		t.Error("status_description should be populated")
	}
	if body["order_number"].(string) == "" {
		t.Error("order_number should be assigned by the server")
	}
	if !strings.HasPrefix(body["order_number"].(string), "ORD-") {
		t.Errorf("order_number should start with ORD-, got %q", body["order_number"])
	}
}

func TestHTTP_CreateOrder_AcceptsDecimalStringPrices(t *testing.T) {
	// `"19.99"` as a STRING is exact; `19.99` as a JSON number would already
	// have lost precision before our code saw it.
	h := newHarness(t, false)

	body := validOrderBody()
	body["items"] = []map[string]any{
		{"product_id": "SKU-A", "product_name": "A", "quantity": 1, "unit_price": "19.99"},
	}

	resp, decoded := h.do(t, http.MethodPost, constants.APIVersion+"/orders", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status: got %d, want 201 (%v)", resp.StatusCode, decoded)
	}
	if got := decoded["total_minor"].(float64); got != 1999 {
		t.Errorf("total_minor: got %v, want 1999", got)
	}
}

func TestHTTP_CreateOrder_RejectsConflictingPriceFields(t *testing.T) {
	// If the two price forms disagree, guessing which one the client meant is
	// how you charge the wrong amount.
	h := newHarness(t, false)

	body := validOrderBody()
	body["items"] = []map[string]any{
		{"product_id": "SKU-A", "product_name": "A", "quantity": 1,
			"unit_price": "19.99", "unit_price_minor": 500},
	}

	resp, decoded := h.do(t, http.MethodPost, constants.APIVersion+"/orders", body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", resp.StatusCode)
	}
	if code := errorCode(decoded); code != "conflicting_price" {
		t.Errorf("error code: got %q, want conflicting_price", code)
	}
}

func TestHTTP_CreateOrder_ValidationErrors(t *testing.T) {
	h := newHarness(t, false)

	cases := []struct {
		name   string
		mutate func(map[string]any)
		status int
		code   string
	}{
		{"no items", func(b map[string]any) { b["items"] = []map[string]any{} }, 400, "no_items"},
		{"bad email", func(b map[string]any) { b["customer_email"] = "nope" }, 400, "invalid_email"},
		{"missing customer", func(b map[string]any) { delete(b, "customer_id") }, 400, "missing_customer_id"},
		{
			"negative quantity",
			func(b map[string]any) {
				b["items"] = []map[string]any{{"product_id": "A", "product_name": "A", "quantity": -1, "unit_price_minor": 100}}
			},
			400, "invalid_quantity",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := validOrderBody()
			c.mutate(body)
			resp, decoded := h.do(t, http.MethodPost, constants.APIVersion+"/orders", body)
			if resp.StatusCode != c.status {
				t.Errorf("status: got %d, want %d (%v)", resp.StatusCode, c.status, decoded)
			}
			if code := errorCode(decoded); code != c.code {
				t.Errorf("error code: got %q, want %q", code, c.code)
			}
		})
	}
}

func TestHTTP_CreateOrder_RejectsUnknownFields(t *testing.T) {
	// DisallowUnknownFields catches `{"quantiy": 5}`. Without it, that typo is
	// silently ignored and the order is created with quantity 0.
	h := newHarness(t, false)

	body := validOrderBody()
	body["totl_amount"] = 100 // a typo a real client would make

	resp, decoded := h.do(t, http.MethodPost, constants.APIVersion+"/orders", body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("an unknown field should be rejected, got %d", resp.StatusCode)
	}
	if code := errorCode(decoded); code != "invalid_json" {
		t.Errorf("error code: got %q", code)
	}
}

func TestHTTP_CreateOrder_SagaFailureReturnsTheOrderReference(t *testing.T) {
	// A declined payment must still give the client something to quote to
	// support. "Something went wrong" with no reference is a dead end.
	h := newHarness(t, false)
	h.pay.FailMode = "declined"

	resp, decoded := h.do(t, http.MethodPost, constants.APIVersion+"/orders", validOrderBody())
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422 (%v)", resp.StatusCode, decoded)
	}
	if resp.Header.Get("X-Order-Id") == "" {
		t.Error("the failed order's id should be returned in X-Order-Id")
	}
	if code := errorCode(decoded); code != "payment_declined" {
		t.Errorf("error code: got %q, want payment_declined", code)
	}
}

// ---------------------------------------------------------------------------
// GET /api/v1/orders/{id} and caching
// ---------------------------------------------------------------------------

func TestHTTP_GetOrderByID(t *testing.T) {
	h := newHarness(t, false)
	created := h.createOrder(t)
	id := created["id"].(string)

	resp, body := h.do(t, http.MethodGet, constants.APIVersion+"/orders/"+id, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	if body["id"].(string) != id {
		t.Errorf("id: got %v, want %v", body["id"], id)
	}
	// The version is exposed so clients can do their own optimistic
	// concurrency control.
	if _, ok := body["version"]; !ok {
		t.Error("the response should expose version")
	}
	// An ETag enables conditional requests.
	if resp.Header.Get("ETag") == "" {
		t.Error("the response should carry an ETag")
	}
}

func TestHTTP_GetOrder_NotFound(t *testing.T) {
	h := newHarness(t, false)

	resp, decoded := h.do(t, http.MethodGet, constants.APIVersion+"/orders/does-not-exist", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", resp.StatusCode)
	}
	if code := errorCode(decoded); code != "order_not_found" {
		t.Errorf("error code: got %q", code)
	}
}

func TestHTTP_GetOrderByNumber(t *testing.T) {
	// Customers quote the number on their receipt, not a UUID.
	h := newHarness(t, false)
	created := h.createOrder(t)
	number := created["order_number"].(string)

	resp, body := h.do(t, http.MethodGet, constants.APIVersion+"/orders/number/"+number, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	if body["id"].(string) != created["id"].(string) {
		t.Error("lookup by number should return the same order as lookup by id")
	}
}

func TestHTTP_CacheIsPopulatedAndInvalidated(t *testing.T) {
	// ========================================================================
	// THE CACHE-ASIDE + INVALIDATION TEST, end to end through HTTP.
	//
	// It asserts on the CACHE'S OWN COUNTERS rather than on timing, which is
	// what makes it deterministic rather than flaky.
	// ========================================================================
	h := newHarness(t, false)
	created := h.createOrder(t)
	id := created["id"].(string)

	// First read: a MISS that populates the cache.
	h.do(t, http.MethodGet, constants.APIVersion+"/orders/"+id, nil)
	afterFirst := h.cache.Stats()

	// Second read: must be served from the cache.
	h.do(t, http.MethodGet, constants.APIVersion+"/orders/"+id, nil)
	afterSecond := h.cache.Stats()

	if afterSecond.Hits <= afterFirst.Hits {
		t.Errorf("the second read should have hit the cache (hits %d -> %d)",
			afterFirst.Hits, afterSecond.Hits)
	}

	// A WRITE must invalidate the entry.
	resp, _ := h.do(t, http.MethodPatch, constants.APIVersion+"/orders/"+id+"/status",
		map[string]any{"status": "SHIPPED", "reason": "handed to carrier"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status update: got %d, want 200", resp.StatusCode)
	}

	// The next read must therefore see the NEW status, not the cached old one.
	_, body := h.do(t, http.MethodGet, constants.APIVersion+"/orders/"+id, nil)
	if got := body["status"].(string); got != "SHIPPED" {
		t.Errorf("after invalidation the read returned stale data: got %q, want SHIPPED", got)
	}
	if h.cache.Stats().Deletes == 0 {
		t.Error("the write should have deleted the cached entry")
	}
}

func TestHTTP_NegativeCachingForMissingOrders(t *testing.T) {
	// Caching absence is what stops a bot probing random ids from hitting the
	// database on every request (cache penetration).
	h := newHarness(t, false)

	for i := 0; i < 3; i++ {
		resp, _ := h.do(t, http.MethodGet, constants.APIVersion+"/orders/ghost-order", nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("read %d: got %d, want 404", i, resp.StatusCode)
		}
	}
	// The second and third lookups were answered from the negative entry.
	if h.cache.Stats().Hits == 0 {
		t.Error("repeated lookups of a missing order should hit the negative cache")
	}
}

func TestHTTP_TerminalOrdersAreCacheableByTheClient(t *testing.T) {
	// A terminal order is immutable, so we can let the browser cache it. A
	// PENDING one must not be cached, or a customer sees a stale status.
	h := newHarness(t, false)

	order, err := domain.NewOrder(domain.NewOrderInput{
		CustomerID: "c1", CustomerEmail: "a@b.com", ShippingAddress: "x",
		Currency: "USD",
		Items:    []domain.NewOrderItemInput{{ProductID: "P", ProductName: "P", Quantity: 1, UnitPriceMinor: 100}},
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	_ = order.Cancel("test", time.Now().UTC())
	h.repo.SeedOrder(order)

	resp, _ := h.do(t, http.MethodGet, constants.APIVersion+"/orders/"+order.ID, nil)
	cc := resp.Header.Get("Cache-Control")
	if !strings.Contains(cc, "max-age") {
		t.Errorf("a terminal order should be client-cacheable, got Cache-Control %q", cc)
	}
	// It must be PRIVATE: an order contains personal data and must never sit
	// in a shared CDN cache where another user could be served it.
	if !strings.Contains(cc, "private") {
		t.Errorf("order responses must be private, got %q", cc)
	}
}

// ---------------------------------------------------------------------------
// GET /api/v1/orders (list)
// ---------------------------------------------------------------------------

func TestHTTP_ListOrders(t *testing.T) {
	h := newHarness(t, false)
	for i := 0; i < 3; i++ {
		h.createOrder(t)
	}

	resp, body := h.do(t, http.MethodGet, constants.APIVersion+"/orders", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}

	orders := body["orders"].([]any)
	if len(orders) != 3 {
		t.Errorf("got %d orders, want 3", len(orders))
	}
	pagination := body["pagination"].(map[string]any)
	if pagination["total"].(float64) != 3 {
		t.Errorf("pagination total: got %v, want 3", pagination["total"])
	}
	if pagination["has_more"].(bool) {
		t.Error("has_more should be false when everything fits on one page")
	}
}

func TestHTTP_ListOrders_Pagination(t *testing.T) {
	h := newHarness(t, false)
	for i := 0; i < 5; i++ {
		h.createOrder(t)
	}

	_, body := h.do(t, http.MethodGet, constants.APIVersion+"/orders?limit=2&offset=0", nil)
	orders := body["orders"].([]any)
	if len(orders) != 2 {
		t.Errorf("got %d orders, want 2", len(orders))
	}

	pagination := body["pagination"].(map[string]any)
	if !pagination["has_more"].(bool) {
		t.Error("has_more should be true")
	}
	// next_offset is pre-computed so the client does not have to do (and get
	// wrong) the arithmetic.
	if pagination["next_offset"].(float64) != 2 {
		t.Errorf("next_offset: got %v, want 2", pagination["next_offset"])
	}
}

func TestHTTP_ListOrders_EmptyResultIsAnArrayNotNull(t *testing.T) {
	// `null` breaks `orders.length` on the client; `[]` does not.
	h := newHarness(t, false)

	_, body := h.do(t, http.MethodGet, constants.APIVersion+"/orders", nil)
	orders, ok := body["orders"].([]any)
	if !ok {
		t.Fatalf("orders should be a JSON array even when empty, got %T", body["orders"])
	}
	if len(orders) != 0 {
		t.Errorf("got %d orders, want 0", len(orders))
	}
}

func TestHTTP_ListOrders_FilterByStatus(t *testing.T) {
	h := newHarness(t, false)
	h.createOrder(t) // PROCESSING after the saga

	_, body := h.do(t, http.MethodGet, constants.APIVersion+"/orders?status=PROCESSING", nil)
	if len(body["orders"].([]any)) != 1 {
		t.Error("expected the PROCESSING filter to match the order")
	}

	_, body = h.do(t, http.MethodGet, constants.APIVersion+"/orders?status=DELIVERED", nil)
	if len(body["orders"].([]any)) != 0 {
		t.Error("expected the DELIVERED filter to match nothing")
	}
}

func TestHTTP_ListOrders_LowercaseStatusIsAccepted(t *testing.T) {
	// Being lenient about case costs nothing and removes a support question.
	h := newHarness(t, false)
	h.createOrder(t)

	resp, body := h.do(t, http.MethodGet, constants.APIVersion+"/orders?status=processing", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	if len(body["orders"].([]any)) != 1 {
		t.Error("a lowercase status filter should work")
	}
}

func TestHTTP_ListOrders_InvalidStatusListsTheValidOnes(t *testing.T) {
	h := newHarness(t, false)

	resp, decoded := h.do(t, http.MethodGet, constants.APIVersion+"/orders?status=NONSENSE", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", resp.StatusCode)
	}
	e := decoded["error"].(map[string]any)
	details, ok := e["details"].(map[string]any)
	if !ok || details["allowed"] == nil {
		t.Error("the error should list the allowed statuses so the client can fix it in one attempt")
	}
}

func TestHTTP_ListOrders_MalformedLimitFallsBackToTheDefault(t *testing.T) {
	// `?limit=abc` from a buggy client should still return a usable page,
	// rather than breaking the client's whole screen with a 400.
	h := newHarness(t, false)
	h.createOrder(t)

	resp, body := h.do(t, http.MethodGet, constants.APIVersion+"/orders?limit=abc", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	if body["pagination"].(map[string]any)["limit"].(float64) != 20 {
		t.Error("a malformed limit should fall back to the default of 20")
	}
}

// ---------------------------------------------------------------------------
// PATCH status / POST cancel
// ---------------------------------------------------------------------------

func TestHTTP_UpdateStatus(t *testing.T) {
	h := newHarness(t, false)
	created := h.createOrder(t)
	id := created["id"].(string)

	resp, body := h.do(t, http.MethodPatch, constants.APIVersion+"/orders/"+id+"/status",
		map[string]any{"status": "SHIPPED", "reason": "handed to carrier"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (%v)", resp.StatusCode, body)
	}
	if got := body["status"].(string); got != "SHIPPED" {
		t.Errorf("status: got %q, want SHIPPED", got)
	}
	// The version must have advanced.
	if body["version"].(float64) <= created["version"].(float64) {
		t.Error("the version should advance on a write")
	}
}

func TestHTTP_UpdateStatus_IllegalTransitionIs422(t *testing.T) {
	// 422, not 400: the request was well-formed, the transition is impossible.
	h := newHarness(t, false)
	created := h.createOrder(t)
	id := created["id"].(string)

	resp, decoded := h.do(t, http.MethodPatch, constants.APIVersion+"/orders/"+id+"/status",
		map[string]any{"status": "PENDING"})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422", resp.StatusCode)
	}
	if code := errorCode(decoded); code != "invalid_transition" {
		t.Errorf("error code: got %q, want invalid_transition", code)
	}
}

func TestHTTP_UpdateStatus_MissingStatusIs400(t *testing.T) {
	h := newHarness(t, false)
	created := h.createOrder(t)
	id := created["id"].(string)

	resp, decoded := h.do(t, http.MethodPatch, constants.APIVersion+"/orders/"+id+"/status",
		map[string]any{"reason": "no status given"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", resp.StatusCode)
	}
	if code := errorCode(decoded); code != "missing_status" {
		t.Errorf("error code: got %q", code)
	}
}

func TestHTTP_CancelOrder(t *testing.T) {
	h := newHarness(t, false)

	// Seed a PENDING order directly: the saga would have moved a created one
	// to PROCESSING, which a customer may not cancel.
	order, err := domain.NewOrder(domain.NewOrderInput{
		CustomerID: "c1", CustomerEmail: "a@b.com", ShippingAddress: "x", Currency: "USD",
		Items: []domain.NewOrderItemInput{{ProductID: "P", ProductName: "P", Quantity: 1, UnitPriceMinor: 500}},
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	h.repo.SeedOrder(order)

	resp, body := h.do(t, http.MethodPost, constants.APIVersion+"/orders/"+order.ID+"/cancel",
		map[string]any{"reason": "changed my mind"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (%v)", resp.StatusCode, body)
	}
	if got := body["status"].(string); got != "CANCELLED" {
		t.Errorf("status: got %q, want CANCELLED", got)
	}
	if got := body["cancel_reason"].(string); got != "changed my mind" {
		t.Errorf("cancel_reason: got %q", got)
	}
	// A cancelled order is terminal and no longer cancellable.
	if body["is_cancellable"].(bool) {
		t.Error("is_cancellable should be false once cancelled")
	}
	if !body["is_final"].(bool) {
		t.Error("is_final should be true once cancelled")
	}
}

func TestHTTP_CancelOrder_WithNoBody(t *testing.T) {
	// A cancel with no reason is perfectly valid, so an empty body must not
	// be an error.
	h := newHarness(t, false)

	order, _ := domain.NewOrder(domain.NewOrderInput{
		CustomerID: "c1", CustomerEmail: "a@b.com", ShippingAddress: "x", Currency: "USD",
		Items: []domain.NewOrderItemInput{{ProductID: "P", ProductName: "P", Quantity: 1, UnitPriceMinor: 500}},
	}, time.Now().UTC())
	h.repo.SeedOrder(order)

	resp, body := h.do(t, http.MethodPost, constants.APIVersion+"/orders/"+order.ID+"/cancel", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (%v)", resp.StatusCode, body)
	}
	// The domain supplies a default, because the DB constraint requires one.
	if body["cancel_reason"].(string) == "" {
		t.Error("a default cancel reason should be applied")
	}
}

func TestHTTP_CancelOrder_ProcessingIs422(t *testing.T) {
	h := newHarness(t, false)
	created := h.createOrder(t) // PROCESSING after the saga
	id := created["id"].(string)

	resp, decoded := h.do(t, http.MethodPost, constants.APIVersion+"/orders/"+id+"/cancel", nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422", resp.StatusCode)
	}
	if code := errorCode(decoded); code != "cannot_cancel" {
		t.Errorf("error code: got %q, want cannot_cancel", code)
	}
}

// ---------------------------------------------------------------------------
// GET /api/v1/orders/stats
// ---------------------------------------------------------------------------

func TestHTTP_Stats(t *testing.T) {
	h := newHarness(t, false)
	h.createOrder(t)
	h.createOrder(t)

	resp, body := h.do(t, http.MethodGet, constants.APIVersion+"/orders/stats", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	if body["total_orders"].(float64) != 2 {
		t.Errorf("total_orders: got %v, want 2", body["total_orders"])
	}
	if body["revenue_minor"].(float64) != 19996 {
		t.Errorf("revenue_minor: got %v, want 19996", body["revenue_minor"])
	}
	// The note documents what the number means, so nobody misreads it.
	if body["note"].(string) == "" {
		t.Error("the stats response should explain what revenue excludes")
	}

	// Every bucket must be present, so charts have no gaps.
	counts := body["count_by_status"].(map[string]any)
	for _, s := range domain.AllStatuses() {
		if _, ok := counts[string(s)]; !ok {
			t.Errorf("count_by_status is missing %s", s)
		}
	}
}

func TestHTTP_StatsRouteBeatsTheWildcard(t *testing.T) {
	// /orders/stats must not be parsed as /orders/{id} with id="stats".
	// Go 1.22+ ServeMux prefers the more specific pattern, but asserting it
	// documents the intent and catches a future router change.
	h := newHarness(t, false)

	resp, body := h.do(t, http.MethodGet, constants.APIVersion+"/orders/stats", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	if _, isOrder := body["order_number"]; isOrder {
		t.Error("/orders/stats was routed to the order-by-id handler")
	}
}

// ---------------------------------------------------------------------------
// Strangler Fig
// ---------------------------------------------------------------------------

// legacyFake implements domain.LegacyOrderPort for the facade test.
type legacyFake struct {
	orders map[string]*domain.Order
	calls  int
}

func (l *legacyFake) GetByID(_ context.Context, id string) (*domain.Order, error) {
	l.calls++
	if o, ok := l.orders[id]; ok {
		copied := *o
		return &copied, nil
	}
	return nil, domain.ErrOrderNotFound
}

func (l *legacyFake) GetByOrderNumber(_ context.Context, number string) (*domain.Order, error) {
	l.calls++
	for _, o := range l.orders {
		if o.OrderNumber == number {
			copied := *o
			return &copied, nil
		}
	}
	return nil, domain.ErrOrderNotFound
}

func (l *legacyFake) Create(_ context.Context, o *domain.Order) error {
	l.orders[o.ID] = o
	return nil
}

func TestHTTP_StranglerStatsEndpoint(t *testing.T) {
	h := newHarness(t, true)

	resp, body := h.do(t, http.MethodGet, constants.APIVersion+"/admin/strangler", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	// The phase string is what tells an operator where the migration stands.
	if phase, ok := body["phase"].(string); !ok || phase == "" {
		t.Error("the strangler stats should report the migration phase")
	}
	if _, ok := body["legacy_share"]; !ok {
		t.Error("legacy_share is the number that answers 'can we decommission yet?'")
	}
}

func TestHTTP_NewStoreServesItsOwnOrders(t *testing.T) {
	// With the facade enabled, an order in the NEW store must be served by the
	// new store and must not consult legacy at all.
	h := newHarness(t, true)
	created := h.createOrder(t)
	id := created["id"].(string)

	resp, _ := h.do(t, http.MethodGet, constants.APIVersion+"/orders/"+id, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	if served := resp.Header.Get(constants.HeaderServedBy); served != "new" {
		t.Errorf("X-Served-By: got %q, want %q", served, "new")
	}
}

// ---------------------------------------------------------------------------
// Middleware behaviour
// ---------------------------------------------------------------------------

func TestHTTP_RequestIDIsPropagated(t *testing.T) {
	// Honouring an inbound id is what lets one customer action be traced
	// across every service's logs.
	h := newHarness(t, false)

	req, _ := http.NewRequest(http.MethodGet, h.server.URL+constants.APIVersion+"/orders", nil)
	req.Header.Set(constants.HeaderRequestID, "trace-me-12345")

	resp, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get(constants.HeaderRequestID); got != "trace-me-12345" {
		t.Errorf("the inbound request id should be echoed: got %q", got)
	}
}

func TestHTTP_UnknownRouteIs404(t *testing.T) {
	h := newHarness(t, false)

	resp, _ := h.do(t, http.MethodGet, "/api/v1/nonexistent", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", resp.StatusCode)
	}
}

func TestHTTP_WrongMethodIsRejected(t *testing.T) {
	// Method-aware routing means DELETE /orders is a 405, not an accidental
	// match on another handler.
	h := newHarness(t, false)

	resp, _ := h.do(t, http.MethodDelete, constants.APIVersion+"/orders", nil)
	if resp.StatusCode != http.StatusMethodNotAllowed && resp.StatusCode != http.StatusNotFound {
		t.Errorf("status: got %d, want 405 or 404", resp.StatusCode)
	}
}

func TestHTTP_OversizedBodyIsRejected(t *testing.T) {
	// Without MaxBytesReader, a client streaming a huge body makes the server
	// allocate until it is killed — a one-line denial of service.
	h := newHarness(t, false)

	body := validOrderBody()
	body["notes"] = strings.Repeat("x", 200_000) // far beyond the 64KiB cap

	resp, _ := h.do(t, http.MethodPost, constants.APIVersion+"/orders", body)
	if resp.StatusCode != http.StatusBadRequest && resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("an oversized body should be rejected, got %d", resp.StatusCode)
	}
}

func TestHTTP_ConcurrentRequestsAreSafe(t *testing.T) {
	// Run with -race. This is the normal operating condition of an HTTP
	// server, not an edge case.
	h := newHarness(t, false)

	done := make(chan struct{})
	for i := 0; i < 20; i++ {
		go func(n int) {
			defer func() { done <- struct{}{} }()
			body := validOrderBody()
			body["customer_id"] = fmt.Sprintf("cust-%d", n)
			resp, _ := h.server.Client().Post(
				h.server.URL+constants.APIVersion+"/orders",
				"application/json",
				bytes.NewReader(mustJSON(body)),
			)
			if resp != nil {
				resp.Body.Close()
			}
		}(i)
	}
	for i := 0; i < 20; i++ {
		<-done
	}

	_, body := h.do(t, http.MethodGet, constants.APIVersion+"/orders?limit=100", nil)
	if total := body["pagination"].(map[string]any)["total"].(float64); total != 20 {
		t.Errorf("expected all 20 concurrent orders to be stored, got %v", total)
	}
}

func mustJSON(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return raw
}
