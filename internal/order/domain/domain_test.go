package domain

import (
	"testing"
	"time"

	"github.com/chahat/order-processing-system/internal/platform/errs"
)

// ============================================================================
// DOMAIN TESTS
//
// Note what these tests DO NOT need: no database, no Docker, no HTTP server,
// no mocks. They run in microseconds. That is not a happy accident — it is the
// direct payoff of the domain layer importing nothing but the standard library.
//
// A test suite that needs infrastructure gets run rarely. One that runs in
// 10ms gets run on every save, which is how it actually catches bugs.
// ============================================================================

var testNow = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

func validInput() NewOrderInput {
	return NewOrderInput{
		CustomerID:      "cust-1001",
		CustomerEmail:   "john.doe@example.com",
		CustomerName:    "John Doe",
		ShippingAddress: "123 Main St, New York, NY 10001",
		Currency:        "USD",
		Items: []NewOrderItemInput{
			{ProductID: "SKU-HEADPHONES", ProductName: "Wireless Headphones", Quantity: 2, UnitPriceMinor: 4999},
		},
	}
}

// ---------------------------------------------------------------------------
// Money — the arithmetic that must never be wrong
// ---------------------------------------------------------------------------

func TestMoney_ArithmeticIsExact(t *testing.T) {
	// THE TEST THAT JUSTIFIES THE WHOLE int64 DESIGN.
	//
	// In float64, 0.1 + 0.2 != 0.3. In minor units, 10 + 20 == 30, exactly,
	// every time. This test would be impossible to write against a float
	// implementation, which is the point.
	a := NewMoney(10, "USD") // $0.10
	b := NewMoney(20, "USD") // $0.20
	sum, err := a.Add(b)
	if err != nil {
		t.Fatalf("Add returned an unexpected error: %v", err)
	}
	if sum.Minor != 30 {
		t.Errorf("0.10 + 0.20: got %d minor units, want 30", sum.Minor)
	}
	if got := sum.String(); got != "0.30 USD" {
		t.Errorf("formatted sum: got %q, want %q", got, "0.30 USD")
	}
}

func TestMoney_RejectsMixedCurrencies(t *testing.T) {
	// Adding USD to EUR is always a bug. Making it an error means it surfaces
	// in a test rather than on a customer's invoice.
	_, err := NewMoney(100, "USD").Add(NewMoney(100, "EUR"))
	if err == nil {
		t.Fatal("expected adding EUR to USD to fail, but it succeeded")
	}
	if !errs.Is(err, errs.KindInvalid) {
		t.Errorf("currency mismatch should be KindInvalid, got %s", errs.KindOf(err))
	}
}

func TestMoney_DetectsOverflow(t *testing.T) {
	// int64 addition WRAPS SILENTLY: huge positive + huge positive becomes
	// negative, turning a charge into a refund. This must be caught.
	const nearMax = int64(9_223_372_036_854_775_807)
	_, err := NewMoney(nearMax, "USD").Add(NewMoney(1000, "USD"))
	if err == nil {
		t.Fatal("expected an overflow error, got none")
	}
}

func TestMoney_MulQty(t *testing.T) {
	line, err := NewMoney(4999, "USD").MulQty(3)
	if err != nil {
		t.Fatalf("MulQty: %v", err)
	}
	if line.Minor != 14997 {
		t.Errorf("49.99 * 3: got %d, want 14997", line.Minor)
	}
}

func TestMoney_ParseIsExact(t *testing.T) {
	cases := []struct {
		in       string
		currency string
		want     int64
	}{
		{"19.99", "USD", 1999},
		{"0.01", "USD", 1},
		{"1000", "USD", 100000},
		{"19.9", "USD", 1990},  // right-padded: .9 is 90 cents, not 9
		{"-5.00", "USD", -500}, // refunds are negative
		{"100", "JPY", 100},    // JPY has ZERO decimal places
	}
	for _, c := range cases {
		got, err := ParseMoney(c.in, c.currency)
		if err != nil {
			t.Errorf("ParseMoney(%q, %q): unexpected error %v", c.in, c.currency, err)
			continue
		}
		if got.Minor != c.want {
			t.Errorf("ParseMoney(%q, %q): got %d minor, want %d", c.in, c.currency, got.Minor, c.want)
		}
	}
}

func TestMoney_ParseRejectsExcessPrecision(t *testing.T) {
	// We REJECT rather than round. Silently turning a customer's "10.999" into
	// "11.00" changes what they agreed to pay.
	if _, err := ParseMoney("10.999", "USD"); err == nil {
		t.Fatal("expected 3 decimal places to be rejected for USD")
	}
}

func TestMoney_FormatsZeroDecimalCurrencies(t *testing.T) {
	// Formatting ¥100 as "1.00" is a visible, embarrassing bug.
	if got := NewMoney(100, "JPY").String(); got != "100 JPY" {
		t.Errorf("JPY formatting: got %q, want %q", got, "100 JPY")
	}
}

func TestMoney_AllocateLosesNoCents(t *testing.T) {
	// THE PENNY PROBLEM. $10.00 split three ways must still total $10.00.
	// Naive rounding gives 333+333+333 = 999 and one cent vanishes.
	total := NewMoney(1000, "USD")
	parts, err := total.Allocate(3)
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	var sum int64
	for _, p := range parts {
		sum += p.Minor
	}
	if sum != total.Minor {
		t.Errorf("allocation lost money: parts sum to %d, want %d", sum, total.Minor)
	}
	// The leftover cents go to the earliest parts: [334, 333, 333].
	if parts[0].Minor != 334 || parts[1].Minor != 333 || parts[2].Minor != 333 {
		t.Errorf("unexpected distribution: %d, %d, %d", parts[0].Minor, parts[1].Minor, parts[2].Minor)
	}
}

func TestMoney_AllocateIsExhaustive(t *testing.T) {
	// Property-style check: for many totals and many split counts, the parts
	// must ALWAYS sum back to the original. A single hand-picked example can
	// pass by luck; sweeping the space cannot.
	for total := int64(1); total <= 200; total++ {
		for n := 1; n <= 7; n++ {
			parts, err := NewMoney(total, "USD").Allocate(n)
			if err != nil {
				t.Fatalf("Allocate(%d) for %d: %v", n, total, err)
			}
			var sum int64
			for _, p := range parts {
				sum += p.Minor
			}
			if sum != total {
				t.Fatalf("Allocate(%d) of %d summed to %d", n, total, sum)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// The status state machine
// ---------------------------------------------------------------------------

func TestStatus_LegalTransitions(t *testing.T) {
	legal := []struct{ from, to Status }{
		{StatusPending, StatusProcessing},
		{StatusPending, StatusCancelled},
		{StatusPending, StatusFailed},
		{StatusProcessing, StatusShipped},
		{StatusProcessing, StatusCancelled},
		{StatusShipped, StatusDelivered},
	}
	for _, c := range legal {
		if !c.from.CanTransitionTo(c.to) {
			t.Errorf("%s -> %s should be legal but was rejected", c.from, c.to)
		}
	}
}

func TestStatus_IllegalTransitionsAreBlocked(t *testing.T) {
	// These are the ones that matter: each would be a real data-integrity bug.
	illegal := []struct {
		from, to Status
		why      string
	}{
		{StatusShipped, StatusPending, "an order cannot un-ship"},
		{StatusDelivered, StatusProcessing, "a delivered order is final"},
		{StatusCancelled, StatusProcessing, "a cancelled order cannot be revived"},
		{StatusDelivered, StatusCancelled, "you cannot cancel what was delivered"},
		{StatusPending, StatusShipped, "an order cannot skip PROCESSING"},
		{StatusPending, StatusDelivered, "an order cannot skip the whole pipeline"},
		{StatusFailed, StatusPending, "a failed order is final"},
	}
	for _, c := range illegal {
		if c.from.CanTransitionTo(c.to) {
			t.Errorf("%s -> %s was allowed, but %s", c.from, c.to, c.why)
		}
		if err := c.from.ValidateTransition(c.to); err == nil {
			t.Errorf("ValidateTransition(%s -> %s) returned nil; %s", c.from, c.to, c.why)
		}
	}
}

func TestStatus_SameStatusIsIdempotent(t *testing.T) {
	// A client retrying "set to SHIPPED" after a timeout must get success, not
	// a 409. Otherwise every network blip becomes a support ticket.
	if err := StatusShipped.ValidateTransition(StatusShipped); err != nil {
		t.Errorf("a no-op transition should succeed, got %v", err)
	}
}

func TestStatus_TerminalStates(t *testing.T) {
	terminal := []Status{StatusDelivered, StatusCancelled, StatusFailed}
	for _, s := range terminal {
		if !s.IsTerminal() {
			t.Errorf("%s should be terminal", s)
		}
	}
	for _, s := range []Status{StatusPending, StatusProcessing, StatusShipped} {
		if s.IsTerminal() {
			t.Errorf("%s should not be terminal", s)
		}
	}
}

func TestStatus_TerminalTransitionReturnsUnprocessable(t *testing.T) {
	// 422, not 400: the request was well-formed, it is just impossible.
	err := StatusDelivered.ValidateTransition(StatusCancelled)
	if !errs.Is(err, errs.KindUnprocessable) {
		t.Errorf("expected KindUnprocessable, got %s", errs.KindOf(err))
	}
}

func TestStatus_InvalidStatusIsRejected(t *testing.T) {
	// This is what stops a client PATCHing {"status":"DELETED"} into the DB.
	if Status("DELETED").IsValid() {
		t.Error("an unknown status must not be valid")
	}
	if err := StatusPending.ValidateTransition(Status("GIBBERISH")); err == nil {
		t.Error("expected a transition to an unknown status to fail")
	}
}

func TestStatus_CustomerCancellationIsStricterThanTheMachine(t *testing.T) {
	// The state machine PERMITS PROCESSING -> CANCELLED (for admins and failed
	// sagas). The BUSINESS RULE forbids a customer doing it. Conflating
	// "technically possible" with "allowed for this actor" is how
	// authorisation bugs are born, so the two are tested separately.
	if !StatusProcessing.CanTransitionTo(StatusCancelled) {
		t.Error("the machine should permit PROCESSING -> CANCELLED")
	}
	if StatusProcessing.CanBeCancelledByCustomer() {
		t.Error("a customer must not be able to cancel a PROCESSING order")
	}
	if !StatusPending.CanBeCancelledByCustomer() {
		t.Error("a customer must be able to cancel a PENDING order")
	}
}

func TestStatus_ErrorNamesTheAllowedNextStates(t *testing.T) {
	// Good error messages are a feature: "invalid transition" costs a
	// developer 20 minutes; naming the allowed set costs them 5 seconds.
	err := StatusShipped.ValidateTransition(StatusPending)
	e, ok := errs.As(err)
	if !ok {
		t.Fatalf("expected a structured error, got %T", err)
	}
	if _, present := e.Details["allowed_next"]; !present {
		t.Error("the error should list the allowed next statuses")
	}
}

// ---------------------------------------------------------------------------
// The order factory
// ---------------------------------------------------------------------------

func TestNewOrder_HappyPath(t *testing.T) {
	order, err := NewOrder(validInput(), testNow)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	if order.Status != StatusPending {
		t.Errorf("a new order must start PENDING, got %s", order.Status)
	}
	// 2 x 49.99 = 99.98. The SERVER computed this, not the client.
	if order.TotalAmount.Minor != 9998 {
		t.Errorf("total: got %d, want 9998", order.TotalAmount.Minor)
	}
	if order.Version != 1 {
		t.Errorf("version should start at 1, got %d", order.Version)
	}
	if order.OrderNumber == "" || order.ID == "" {
		t.Error("the factory must assign both an id and an order number")
	}
	if order.Source != SourceNew {
		t.Errorf("source: got %q, want %q", order.Source, SourceNew)
	}
}

func TestNewOrder_NormalisesInput(t *testing.T) {
	in := validInput()
	in.CustomerEmail = "  John.DOE@Example.COM  "
	in.ShippingAddress = "  123 Main St  "
	order, err := NewOrder(in, testNow)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	// Normalising at the boundary means every later comparison and lookup is
	// reliable, instead of depending on how the client formatted its input.
	if order.CustomerEmail != "john.doe@example.com" {
		t.Errorf("email should be trimmed and lowercased, got %q", order.CustomerEmail)
	}
	if order.ShippingAddress != "123 Main St" {
		t.Errorf("address should be trimmed, got %q", order.ShippingAddress)
	}
}

func TestNewOrder_ComputesTotalFromItemsNotFromTheClient(t *testing.T) {
	// Trusting a client-supplied total is how you get charged $0.01 for a
	// television. The input type has no total field at all — the type system
	// enforces the rule, so this test documents it rather than guarding it.
	in := validInput()
	in.Items = []NewOrderItemInput{
		{ProductID: "A", ProductName: "A", Quantity: 3, UnitPriceMinor: 1000},
		{ProductID: "B", ProductName: "B", Quantity: 1, UnitPriceMinor: 2500},
	}
	order, err := NewOrder(in, testNow)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	if order.TotalAmount.Minor != 5500 { // 3*1000 + 2500
		t.Errorf("total: got %d, want 5500", order.TotalAmount.Minor)
	}
}

func TestNewOrder_MergesDuplicateProductLines(t *testing.T) {
	// A double-click or a retried partial request should produce quantity 4 on
	// one line, not two lines the warehouse picks twice.
	in := validInput()
	in.Items = []NewOrderItemInput{
		{ProductID: "SKU-X", ProductName: "X", Quantity: 1, UnitPriceMinor: 1000},
		{ProductID: "SKU-X", ProductName: "X", Quantity: 3, UnitPriceMinor: 1000},
	}
	order, err := NewOrder(in, testNow)
	if err != nil {
		t.Fatalf("NewOrder: %v", err)
	}
	if len(order.Items) != 1 {
		t.Fatalf("duplicate products should merge into one line, got %d lines", len(order.Items))
	}
	if order.Items[0].Quantity != 4 {
		t.Errorf("merged quantity: got %d, want 4", order.Items[0].Quantity)
	}
	if order.TotalAmount.Minor != 4000 {
		t.Errorf("total after merge: got %d, want 4000", order.TotalAmount.Minor)
	}
}

func TestNewOrder_ValidationFailures(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*NewOrderInput)
		code   string
	}{
		{"no items", func(in *NewOrderInput) { in.Items = nil }, "no_items"},
		{"missing customer id", func(in *NewOrderInput) { in.CustomerID = "" }, "missing_customer_id"},
		{"missing email", func(in *NewOrderInput) { in.CustomerEmail = "" }, "missing_email"},
		{"malformed email", func(in *NewOrderInput) { in.CustomerEmail = "not-an-email" }, "invalid_email"},
		{"missing address", func(in *NewOrderInput) { in.ShippingAddress = "" }, "missing_address"},
		{
			// THE NEGATIVE-QUANTITY EXPLOIT: it would make the line total
			// negative and reduce the order total, so the attacker pays less
			// than the real price.
			"negative quantity",
			func(in *NewOrderInput) { in.Items[0].Quantity = -5 },
			"invalid_quantity",
		},
		{"zero quantity", func(in *NewOrderInput) { in.Items[0].Quantity = 0 }, "invalid_quantity"},
		{"zero price", func(in *NewOrderInput) { in.Items[0].UnitPriceMinor = 0 }, "invalid_price"},
		{"negative price", func(in *NewOrderInput) { in.Items[0].UnitPriceMinor = -100 }, "invalid_price"},
		{"missing product name", func(in *NewOrderInput) { in.Items[0].ProductName = "" }, "missing_product_name"},
		{
			"address too long",
			func(in *NewOrderInput) { in.ShippingAddress = string(make([]byte, 600)) },
			"address_too_long",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := validInput()
			c.mutate(&in)
			_, err := NewOrder(in, testNow)
			if err == nil {
				t.Fatalf("expected %s to be rejected", c.name)
			}
			e, ok := errs.As(err)
			if !ok {
				t.Fatalf("expected a structured error, got %T", err)
			}
			if e.Code != c.code {
				t.Errorf("error code: got %q, want %q", e.Code, c.code)
			}
			if !errs.Is(err, errs.KindInvalid) {
				t.Errorf("validation failures should be KindInvalid, got %s", errs.KindOf(err))
			}
		})
	}
}

func TestNewOrder_RejectsTooManyItems(t *testing.T) {
	in := validInput()
	in.Items = nil
	for i := 0; i < 100; i++ {
		in.Items = append(in.Items, NewOrderItemInput{
			// Distinct product ids, so the duplicate-merging logic does not
			// collapse them and mask the limit being tested.
			ProductID:   "SKU-" + string(rune('A'+i%26)) + string(rune('a'+i/26)),
			ProductName: "Item", Quantity: 1, UnitPriceMinor: 100,
		})
	}
	if _, err := NewOrder(in, testNow); err == nil {
		t.Fatal("expected an order with 100 items to be rejected")
	}
}

func TestNewOrder_ErrorNamesTheOffendingItem(t *testing.T) {
	// On a 20-line order, "invalid quantity" without an index is guesswork.
	in := validInput()
	in.Items = append(in.Items, NewOrderItemInput{
		ProductID: "SKU-BAD", ProductName: "Bad", Quantity: 0, UnitPriceMinor: 100,
	})
	_, err := NewOrder(in, testNow)
	e, ok := errs.As(err)
	if !ok {
		t.Fatalf("expected a structured error, got %T", err)
	}
	if field, _ := e.Details["field"].(string); field != "items[1].quantity" {
		t.Errorf("error should identify items[1].quantity, got %v", e.Details["field"])
	}
}

// ---------------------------------------------------------------------------
// Behaviour on the aggregate
// ---------------------------------------------------------------------------

func TestOrder_TransitionTo(t *testing.T) {
	order, _ := NewOrder(validInput(), testNow)
	later := testNow.Add(time.Minute)

	if err := order.TransitionTo(StatusProcessing, later); err != nil {
		t.Fatalf("PENDING -> PROCESSING: %v", err)
	}
	if order.Status != StatusProcessing {
		t.Errorf("status: got %s, want PROCESSING", order.Status)
	}
	if !order.StatusChangedAt.Equal(later) {
		t.Error("StatusChangedAt should be updated by a transition")
	}
}

func TestOrder_CancelOnlyWhilePending(t *testing.T) {
	order, _ := NewOrder(validInput(), testNow)

	if err := order.Cancel("changed my mind", testNow); err != nil {
		t.Fatalf("cancelling a PENDING order should succeed: %v", err)
	}
	if order.Status != StatusCancelled {
		t.Errorf("status: got %s, want CANCELLED", order.Status)
	}
	if order.CancelReason != "changed my mind" {
		t.Errorf("cancel reason: got %q", order.CancelReason)
	}
}

func TestOrder_CancelIsIdempotent(t *testing.T) {
	// A double-click must not error. This is what lets the service treat a
	// repeat cancellation as a no-op instead of a 422.
	order, _ := NewOrder(validInput(), testNow)
	_ = order.Cancel("first", testNow)
	if err := order.Cancel("second", testNow); err != nil {
		t.Fatalf("cancelling an already-cancelled order should be a no-op, got %v", err)
	}
	if order.CancelReason != "first" {
		t.Errorf("the original reason should be preserved, got %q", order.CancelReason)
	}
}

func TestOrder_CancelRejectedAfterProcessing(t *testing.T) {
	order, _ := NewOrder(validInput(), testNow)
	_ = order.TransitionTo(StatusProcessing, testNow)

	err := order.Cancel("too late", testNow)
	if err == nil {
		t.Fatal("expected cancelling a PROCESSING order to be rejected")
	}
	if !errs.Is(err, errs.KindUnprocessable) {
		t.Errorf("expected KindUnprocessable, got %s", errs.KindOf(err))
	}
}

func TestOrder_CancelGetsADefaultReason(t *testing.T) {
	order, _ := NewOrder(validInput(), testNow)
	_ = order.Cancel("", testNow)
	// The database CHECK constraint requires a reason on a cancelled order, so
	// the domain must always supply one.
	if order.CancelReason == "" {
		t.Error("a cancelled order must always carry a reason")
	}
}

func TestOrder_FailDoesNotOverwriteATerminalState(t *testing.T) {
	// Compensation WILL run twice in a distributed system. The second run must
	// be a no-op rather than mangling the record.
	order, _ := NewOrder(validInput(), testNow)
	_ = order.Cancel("customer cancelled", testNow)

	if err := order.Fail("saga failed", testNow); err != nil {
		t.Fatalf("Fail on a terminal order should be a no-op, got %v", err)
	}
	if order.Status != StatusCancelled {
		t.Errorf("status should remain CANCELLED, got %s", order.Status)
	}
}

func TestOrder_ItemCountCountsUnitsNotLines(t *testing.T) {
	// The warehouse cares about units; the invoice shows lines. Naming it
	// precisely avoids a family of off-by-one reporting bugs.
	in := validInput()
	in.Items = []NewOrderItemInput{
		{ProductID: "A", ProductName: "A", Quantity: 2, UnitPriceMinor: 100},
		{ProductID: "B", ProductName: "B", Quantity: 5, UnitPriceMinor: 100},
	}
	order, _ := NewOrder(in, testNow)
	if got := order.ItemCount(); got != 7 {
		t.Errorf("ItemCount: got %d, want 7 units across 2 lines", got)
	}
	if len(order.Items) != 2 {
		t.Errorf("line count: got %d, want 2", len(order.Items))
	}
}

func TestOrder_RecalculateTotalDetectsCorruption(t *testing.T) {
	order, _ := NewOrder(validInput(), testNow)

	_, consistent, err := order.RecalculateTotal()
	if err != nil {
		t.Fatalf("RecalculateTotal: %v", err)
	}
	if !consistent {
		t.Error("a freshly built order should be internally consistent")
	}

	// Simulate corruption, as a bad migration or a second writer might cause.
	order.TotalAmount = NewMoney(1, "USD")
	if _, consistent, _ := order.RecalculateTotal(); consistent {
		t.Error("a mismatched total should be reported as inconsistent")
	}
}

func TestOrder_CacheTTLAdaptsToMutability(t *testing.T) {
	// A terminal order is IMMUTABLE, so it is safe to cache for a long time.
	// A PENDING order could change in a second. Letting the domain answer this
	// keeps the rule with the knowledge that justifies it.
	order, _ := NewOrder(validInput(), testNow)
	short := 5 * time.Minute

	if ttl := order.CacheTTL(short); ttl != short {
		t.Errorf("a PENDING order should use the default TTL, got %s", ttl)
	}
	_ = order.Cancel("done", testNow)
	if ttl := order.CacheTTL(short); ttl <= short {
		t.Errorf("a terminal order should get a longer TTL, got %s", ttl)
	}
}
