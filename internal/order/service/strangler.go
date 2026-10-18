package service

import (
	"context"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/chahat/order-processing-system/internal/order/domain"
	"github.com/chahat/order-processing-system/internal/platform/config"
	"github.com/chahat/order-processing-system/internal/platform/errs"
	"github.com/chahat/order-processing-system/internal/platform/httpx"
	"github.com/chahat/order-processing-system/internal/platform/idgen"
	"github.com/chahat/order-processing-system/internal/platform/logging"
	"github.com/chahat/order-processing-system/internal/platform/metrics"
)

// ============================================================================
// THE STRANGLER FIG PATTERN
// ============================================================================
//
// THE NAME: a strangler fig seeds in the canopy of a host tree and grows
// roots downward around its trunk. Over years it takes over the host's
// structure; eventually the host dies and rots away, leaving a
// self-supporting fig in exactly the host's shape. Martin Fowler borrowed it
// for incremental migration.
//
// THE PROBLEM IT SOLVES: you have a 12-year-old order system. 400 endpoints,
// no tests, three people who understand it, and it is load-bearing for
// revenue. You cannot stop shipping features for 18 months to rewrite it, and
// A BIG-BANG REWRITE-AND-CUTOVER IS THE MOST RELIABLE WAY TO DESTROY A
// BUSINESS — on cutover day you discover the 200 undocumented behaviours the
// old system had, all at once, in production.
//
// THE PATTERN: put a FACADE in front of the legacy system. Route one slice of
// traffic to a new implementation. Then another. The legacy system shrinks
// until nothing reaches it, then you delete it.
//
// WHAT THIS FILE IMPLEMENTS:
//
//	                    +-------------------+
//	   client --------> |  StranglerFacade  |
//	                    +---------+---------+
//	                              |
//	              new store?  ----+----  legacy service?
//	                  |                        |
//	           +------v------+          +------v-------+
//	           | Postgres    |   miss   | legacy-order |
//	           | (new)       | -------> | service      |
//	           +-------------+          +--------------+
//	                  ^                        |
//	                  +------- backfill -------+
//	                        ("migrate on read")
//
// THE FOUR MIGRATION PHASES, each a config change and NOT a deploy:
//
//	PHASE 1  STRANGLER_WRITE_MODE=legacy
//	         All writes go to legacy. We read from legacy. The new system is
//	         deployed but idle. Zero risk; proves the facade works.
//
//	PHASE 2  STRANGLER_WRITE_MODE=dual
//	         Writes go to BOTH. Reads still prefer legacy (or are shadowed and
//	         compared). This is where you find the behaviour differences, with
//	         legacy still authoritative so a mismatch is a bug report, not an
//	         outage. THE MOST VALUABLE AND MOST OFTEN SKIPPED PHASE.
//
//	PHASE 3  STRANGLER_WRITE_MODE=new, READ_FALLBACK=true   <-- our default
//	         New system is authoritative. Historical orders still live in
//	         legacy, so a miss falls back and (optionally) BACKFILLS, letting
//	         ordinary traffic perform the migration for you.
//
//	PHASE 4  STRANGLER_READ_FALLBACK=false
//	         Nothing reaches legacy. Turn it off, keep a database snapshot,
//	         delete the code. The fig stands alone.
//
// WHY THIS IS A FEATURE FLAG AND NOT A CODE BRANCH: every phase transition is
// an env var change, so ROLLBACK IS INSTANT. If phase 3 goes wrong at 2am you
// set one variable back, rather than reverting a deploy under pressure.
// ============================================================================

// StranglerFacade routes reads and writes between the new store and legacy.
//
// It implements domain.OrderReader, so the HTTP layer cannot tell whether an
// order came from Postgres or from a 12-year-old PHP service. That opacity is
// the entire value of the facade — it is the FACADE PATTERN (structural, GoF)
// doing its actual job rather than just wrapping something for the sake of it.
type StranglerFacade struct {
	newStore domain.OrderRepository
	legacy   domain.LegacyOrderPort
	cfg      config.StranglerConfig
	log      *slog.Logger
	mtx      *metrics.Registry

	// Counters let you WATCH the migration progress on a dashboard. Without
	// them, "are we done?" is a guess. With them it is a graph that trends to
	// zero, and you know exactly when phase 4 is safe.
	servedByNew    atomic.Int64
	servedByLegacy atomic.Int64
	backfilled     atomic.Int64
}

func NewStranglerFacade(
	newStore domain.OrderRepository,
	legacy domain.LegacyOrderPort,
	cfg config.StranglerConfig,
	log *slog.Logger,
	mtx *metrics.Registry,
) *StranglerFacade {
	return &StranglerFacade{newStore: newStore, legacy: legacy, cfg: cfg, log: log, mtx: mtx}
}

// ServedBy reports which backend answered the last call, for the X-Served-By
// response header. Returning this to the client makes the migration
// OBSERVABLE FROM OUTSIDE — an e2e test can assert that a given order came
// from legacy, which is how we prove the fallback works rather than assuming.
type ServedBy string

const (
	ServedByNew    ServedBy = "new"
	ServedByLegacy ServedBy = "legacy"
)

// GetByIDTraced is GetByID plus the routing decision.
func (f *StranglerFacade) GetByIDTraced(ctx context.Context, id string) (*domain.Order, ServedBy, error) {
	log := logging.FromContext(ctx)

	// ALWAYS TRY THE NEW SYSTEM FIRST. It is faster, it is authoritative in
	// phase 3, and as the migration progresses this path answers an
	// ever-larger share of traffic with no code change.
	order, err := f.newStore.GetByID(ctx, id)
	if err == nil {
		f.servedByNew.Add(1)
		f.mtx.IncCounter("strangler_requests_total", "Requests by backend", "backend", "new", "operation", "get_by_id")
		return order, ServedByNew, nil
	}

	// ONLY a not-found justifies trying legacy. A timeout or a connection
	// error means the new system is SICK, and falling back to legacy then
	// would silently double the load on the system we are trying to retire —
	// at exactly the worst moment. Distinguishing "absent" from "broken" is
	// what keeps this fallback safe.
	if !errs.Is(err, errs.KindNotFound) {
		return nil, ServedByNew, err
	}
	if !f.cfg.Enabled || !f.cfg.ReadFallbackToLegacy || f.legacy == nil {
		return nil, ServedByNew, err // phase 4: legacy is not consulted at all
	}

	log.DebugContext(ctx, "order not in the new store; falling back to legacy",
		slog.String("order_id", id))

	legacyOrder, legacyErr := f.legacy.GetByID(ctx, id)
	if legacyErr != nil {
		if errs.Is(legacyErr, errs.KindNotFound) {
			// Neither system has it, so it genuinely does not exist. Return
			// the ORIGINAL not-found error, so the client sees a clean 404
			// rather than a confusing message about a legacy system it should
			// never have to know about.
			return nil, ServedByLegacy, err
		}
		// Legacy is broken. Report a 503 rather than a 404: telling a
		// customer their real order "does not exist" because a backend is
		// down is far worse than telling them to try again.
		f.mtx.IncCounter("strangler_legacy_errors_total", "Legacy backend errors")
		return nil, ServedByLegacy, errs.Unavailable("legacy_unavailable",
			"the historical order system is temporarily unavailable").Wrap(legacyErr)
	}

	f.servedByLegacy.Add(1)
	f.mtx.IncCounter("strangler_requests_total", "Requests by backend", "backend", "legacy", "operation", "get_by_id")
	log.InfoContext(ctx, "order served from the legacy system",
		slog.String("order_id", id), slog.String("order_number", legacyOrder.OrderNumber))

	// ------------------------------------------------------------------
	// BACKFILL ON READ ("migrate on read" / lazy migration).
	//
	// THE IDEA: the orders customers actually look at are the ones worth
	// migrating. Ordinary traffic does the migration for you, warmest records
	// first, with no big batch job and no maintenance window.
	//
	// IT IS DELIBERATELY FIRE-AND-FORGET: a failed backfill must NEVER fail
	// the customer's read. They asked for their order; they get their order.
	// The backfill is an optimisation, and treating it as optional is what
	// keeps it safe.
	//
	// THE TRADE-OFF (honestly): cold records are never migrated, so you still
	// need a batch job for the long tail before phase 4. Backfill-on-read
	// reduces that job's size and risk; it does not eliminate it.
	// ------------------------------------------------------------------
	if f.cfg.BackfillOnRead {
		// A DETACHED context: the request may well end before the backfill
		// does, and a cancelled parent context would abort every backfill.
		go f.backfill(context.WithoutCancel(ctx), legacyOrder)
	}

	return legacyOrder, ServedByLegacy, nil
}

// isUUID reports whether s has the shape of a UUID.
//
// A SHAPE CHECK, not a validation: we only need to know whether this
// identifier can go into a `uuid` column. Parsing it properly would need a
// dependency to answer a question that length and hyphen positions already
// settle. Deliberately narrow, and the comment says so, so nobody mistakes it
// for a validator later.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
			if !isHex {
				return false
			}
		}
	}
	return true
}

func (f *StranglerFacade) backfill(ctx context.Context, order *domain.Order) {
	// A goroutine panicking takes the WHOLE PROCESS down in Go, and this one
	// is not protected by the HTTP middleware's recover. So it needs its own.
	// A background goroutine without panic recovery is a time bomb.
	defer func() {
		if rec := recover(); rec != nil {
			f.log.Error("panic during strangler backfill", slog.Any("panic", rec))
		}
	}()

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// Mark the provenance so a GROUP BY source tells you migration progress,
	// and so anyone debugging an odd record knows where it came from.
	copied := *order
	copied.Source = domain.SourceLegacy

	// ------------------------------------------------------------------
	// IDENTIFIER TRANSLATION — a bug found by running the system, not by
	// reading the code.
	//
	// THE PROBLEM: the legacy system's identifier is "LEG-2019-0001". Our
	// orders.id column is a UUID. Inserting a legacy reference straight into
	// it fails with SQLSTATE 22P02 (invalid input syntax for type uuid), so
	// EVERY backfill failed — silently, because a failed backfill is
	// deliberately fire-and-forget and must never break the customer's read.
	//
	// The read kept working, the metric said "0 backfilled", and only the
	// WARN log revealed why. (The e2e test passed because it asserted on the
	// read succeeding, which it did. The lesson: assert on the OUTCOME you
	// care about — the record landing in the new store — not just on the
	// request not failing.)
	//
	// THE FIX: mint a NEW internal UUID and keep the legacy reference as the
	// ORDER NUMBER, which is the customer-facing identifier anyway.
	//
	// WHY THIS IS THE RIGHT FIX RATHER THAN WIDENING THE COLUMN TO TEXT:
	//   * The legacy reference is what the CUSTOMER quotes, and order_number
	//     is exactly the field for that. Lookups by "LEG-2019-0001" keep
	//     working, which is the behaviour that matters.
	//   * Our internal id stays a real UUID, so it keeps the properties we
	//     chose it for: no volume leak, not guessable, and indexable with a
	//     single type.
	//   * Changing orders.id to TEXT to accommodate a system we are actively
	//     retiring would let the legacy system's design dictate ours
	//     permanently — the exact outcome the Strangler Fig exists to avoid.
	//
	// Item ids need the same treatment, and they must point at the NEW
	// order id or the foreign key would be orphaned.
	// ------------------------------------------------------------------
	if !isUUID(copied.ID) {
		newID := idgen.NewUUID()
		f.log.Debug("translating a legacy identifier for backfill",
			slog.String("legacy_ref", copied.ID),
			slog.String("new_internal_id", newID),
			slog.String("order_number", copied.OrderNumber))

		copied.ID = newID
		// Copy the items slice before rewriting it: `copied := *order` copied
		// the slice HEADER, so both still share one backing array. Mutating
		// in place would corrupt the caller's order — which is the very
		// object we are about to return to the customer.
		items := make([]domain.OrderItem, len(order.Items))
		copy(items, order.Items)
		for i := range items {
			items[i].ID = idgen.NewUUID()
			items[i].OrderID = newID
		}
		copied.Items = items
	}

	if err := f.newStore.Create(ctx, &copied, domain.NoEvent); err != nil {
		// A DUPLICATE here is the EXPECTED outcome under concurrency: two
		// simultaneous reads of the same legacy order both try to backfill,
		// and one loses. That is success, not failure — the record is in the
		// new store, which is all we wanted.
		if errs.Is(err, errs.KindConflict) {
			return
		}
		f.log.Warn("strangler backfill failed; the order remains in the legacy system only",
			slog.String("order_id", order.ID), slog.String("error", err.Error()))
		f.mtx.IncCounter("strangler_backfill_failed_total", "Failed backfills")
		return
	}

	f.backfilled.Add(1)
	f.mtx.IncCounter("strangler_backfilled_total", "Orders migrated from legacy by read traffic")
	f.log.Info("order backfilled from legacy into the new store",
		slog.String("order_id", order.ID),
		slog.String("order_number", order.OrderNumber))
	// NOTE: we deliberately emit NO outbox event for a backfill. A backfill is
	// a MIGRATION, not a business event — publishing order.created for a
	// three-year-old order would re-trigger confirmation emails to customers
	// for orders they received years ago. This is the single most common and
	// most embarrassing migration mistake, and one line of comment is cheaper
	// than finding out the hard way.
}

// GetByOrderNumberTraced mirrors GetByIDTraced for the customer-facing lookup.
func (f *StranglerFacade) GetByOrderNumberTraced(ctx context.Context, number string) (*domain.Order, ServedBy, error) {
	order, err := f.newStore.GetByOrderNumber(ctx, number)
	if err == nil {
		f.servedByNew.Add(1)
		return order, ServedByNew, nil
	}
	if !errs.Is(err, errs.KindNotFound) {
		return nil, ServedByNew, err
	}
	if !f.cfg.Enabled || !f.cfg.ReadFallbackToLegacy || f.legacy == nil {
		return nil, ServedByNew, err
	}

	legacyOrder, legacyErr := f.legacy.GetByOrderNumber(ctx, number)
	if legacyErr != nil {
		if errs.Is(legacyErr, errs.KindNotFound) {
			return nil, ServedByLegacy, err
		}
		return nil, ServedByLegacy, errs.Unavailable("legacy_unavailable",
			"the historical order system is temporarily unavailable").Wrap(legacyErr)
	}
	f.servedByLegacy.Add(1)
	if f.cfg.BackfillOnRead {
		go f.backfill(context.WithoutCancel(ctx), legacyOrder)
	}
	return legacyOrder, ServedByLegacy, nil
}

// MigrationStats powers a progress dashboard and the /api/v1/admin/strangler
// endpoint.
func (f *StranglerFacade) MigrationStats() map[string]any {
	servedNew := f.servedByNew.Load()
	servedLegacy := f.servedByLegacy.Load()
	total := servedNew + servedLegacy

	// THE NUMBER THAT ANSWERS "CAN WE DELETE THE LEGACY SYSTEM YET?"
	// When legacy_share reaches 0 and stays there across a full business
	// cycle (including month-end and any annual reporting job), phase 4 is
	// safe. Deciding that from a graph rather than from confidence is the
	// difference between a calm decommission and an incident.
	legacyShare := 0.0
	if total > 0 {
		legacyShare = float64(servedLegacy) / float64(total)
	}

	return map[string]any{
		"write_mode":            string(f.cfg.WriteMode),
		"read_fallback_enabled": f.cfg.ReadFallbackToLegacy,
		"backfill_enabled":      f.cfg.BackfillOnRead,
		"served_by_new":         servedNew,
		"served_by_legacy":      servedLegacy,
		"legacy_share":          legacyShare,
		"orders_backfilled":     f.backfilled.Load(),
		"phase":                 f.phase(),
	}
}

// phase derives the human-readable migration phase from the live config, so
// the dashboard cannot disagree with reality.
func (f *StranglerFacade) phase() string {
	switch {
	case !f.cfg.Enabled:
		return "disabled (strangler off; new system only)"
	case f.cfg.WriteMode == config.WriteModeLegacy:
		return "phase 1: legacy authoritative, new system shadowing"
	case f.cfg.WriteMode == config.WriteModeDual:
		return "phase 2: dual write, legacy still the source of truth"
	case f.cfg.WriteMode == config.WriteModeNew && f.cfg.ReadFallbackToLegacy:
		return "phase 3: new system authoritative, legacy read-fallback for history"
	default:
		return "phase 4: new system only, legacy ready to decommission"
	}
}

// ---------------------------------------------------------------------------
// The legacy client adapter
// ---------------------------------------------------------------------------

// HTTPLegacyAdapter talks to the old order service and TRANSLATES its ugly
// response shape into our clean domain model.
//
// This is the Adapter pattern earning its keep for real: the legacy service
// returns snake_case keys, a string amount in dollars, a different status
// vocabulary, and no concept of order items. None of that leaks past this file.
type HTTPLegacyAdapter struct {
	client *httpx.Client
	log    *slog.Logger
}

func NewHTTPLegacyAdapter(baseURL string, timeout time.Duration, log *slog.Logger) *HTTPLegacyAdapter {
	return &HTTPLegacyAdapter{client: httpx.NewClient(baseURL, timeout, log), log: log}
}

// legacyOrderDTO is the OLD system's wire format, warts and all.
type legacyOrderDTO struct {
	OrderRef      string `json:"order_ref"` // their id
	CustRef       string `json:"cust_ref"`  // their customer id
	CustEmail     string `json:"cust_email"`
	CustName      string `json:"cust_name"`
	OrderState    string `json:"order_state"`    // their status vocabulary
	AmountDollars string `json:"amount_dollars"` // a STRING, in dollars
	ShipTo        string `json:"ship_to"`
	Created       string `json:"created"` // a non-RFC3339 timestamp format
	Lines         []struct {
		Sku       string `json:"sku"`
		Desc      string `json:"desc"`
		Qty       int    `json:"qty"`
		PriceEach string `json:"price_each"`
	} `json:"lines"`
}

func (a *HTTPLegacyAdapter) GetByID(ctx context.Context, id string) (*domain.Order, error) {
	var dto legacyOrderDTO
	if err := a.client.Do(ctx, httpx.CallOptions{
		Method: http.MethodGet,
		Path:   "/legacy/orders/" + id,
		Out:    &dto,
		// A read is naturally idempotent, so retrying is always safe.
		Retry: true,
	}); err != nil {
		return nil, err
	}
	return a.translate(dto)
}

func (a *HTTPLegacyAdapter) GetByOrderNumber(ctx context.Context, number string) (*domain.Order, error) {
	var dto legacyOrderDTO
	if err := a.client.Do(ctx, httpx.CallOptions{
		Method: http.MethodGet,
		Path:   "/legacy/orders/by-ref/" + number,
		Out:    &dto,
		Retry:  true,
	}); err != nil {
		return nil, err
	}
	return a.translate(dto)
}

// Create supports STRANGLER_WRITE_MODE=dual and =legacy (phases 1 and 2).
func (a *HTTPLegacyAdapter) Create(ctx context.Context, order *domain.Order) error {
	body := legacyOrderDTO{
		OrderRef:      order.OrderNumber,
		CustRef:       order.CustomerID,
		CustEmail:     order.CustomerEmail,
		CustName:      order.CustomerName,
		OrderState:    toLegacyState(order.Status),
		AmountDollars: order.TotalAmount.MajorString(),
		ShipTo:        order.ShippingAddress,
		Created:       order.CreatedAt.Format("2006-01-02 15:04:05"),
	}
	return a.client.Do(ctx, httpx.CallOptions{
		Method: http.MethodPost,
		Path:   "/legacy/orders",
		Body:   body,
		// Retry is FALSE: the legacy system has no idempotency support, so a
		// retry after a timeout would create a DUPLICATE ORDER. When you
		// cannot make a call idempotent, you must not retry it. Recognising
		// that constraint is more important than the convenience of retrying.
		Retry: false,
	})
}

// translate converts the legacy shape into our domain model.
//
// THIS FUNCTION IS WHERE THE TWO WORLDS MEET, and it is the only place that
// needs to know the legacy system's quirks.
func (a *HTTPLegacyAdapter) translate(dto legacyOrderDTO) (*domain.Order, error) {
	// Their amount is a decimal STRING in dollars; ours is integer cents.
	// ParseMoney does the exact conversion — doing it with a float here would
	// reintroduce the precision bug money.go exists to prevent.
	total, err := domain.ParseMoney(dto.AmountDollars, "USD")
	if err != nil {
		return nil, errs.Internal("legacy_translate_failed",
			"the legacy system returned an amount we could not parse").Wrap(err)
	}

	// Their timestamp format is not RFC3339. A parse failure must NOT lose
	// the order — a wrong created_at is a cosmetic problem, a dropped order is
	// a revenue problem. So we degrade to the zero time and carry on.
	created, err := time.Parse("2006-01-02 15:04:05", dto.Created)
	if err != nil {
		a.log.Warn("could not parse the legacy created timestamp; leaving it empty",
			slog.String("raw", dto.Created))
	}

	o := &domain.Order{
		ID:              dto.OrderRef,
		OrderNumber:     dto.OrderRef,
		CustomerID:      dto.CustRef,
		CustomerEmail:   dto.CustEmail,
		CustomerName:    dto.CustName,
		Status:          fromLegacyState(dto.OrderState),
		TotalAmount:     total,
		ShippingAddress: dto.ShipTo,
		Version:         1,
		CreatedAt:       created,
		UpdatedAt:       created,
		StatusChangedAt: created,
		Source:          domain.SourceLegacy,
	}

	for i, line := range dto.Lines {
		unit, err := domain.ParseMoney(line.PriceEach, "USD")
		if err != nil {
			// Skip an unparseable line rather than dropping the whole order.
			a.log.Warn("skipping an unparseable legacy order line",
				slog.String("order", dto.OrderRef), slog.Int("line", i))
			continue
		}
		lineTotal, _ := unit.MulQty(line.Qty)
		o.Items = append(o.Items, domain.OrderItem{
			ID:          dto.OrderRef + "-line-" + line.Sku,
			OrderID:     o.ID,
			ProductID:   line.Sku,
			ProductName: line.Desc,
			ProductSKU:  line.Sku,
			Quantity:    line.Qty,
			UnitPrice:   unit,
			LineTotal:   lineTotal,
		})
	}
	return o, nil
}

// fromLegacyState maps their status vocabulary onto ours.
//
// THE DEFAULT CASE IS THE INTERESTING ONE: an unknown legacy state becomes
// PENDING rather than panicking or returning an error. A 12-year-old system
// WILL have states nobody remembers ("AWAIT_FAX"), and refusing to display
// those orders at all would be a worse outcome than showing a conservative
// status. Defensive translation is a requirement, not paranoia.
func fromLegacyState(state string) domain.Status {
	switch state {
	case "NEW", "AWAITING", "HOLD":
		return domain.StatusPending
	case "PICKING", "PACKED", "IN_PROGRESS":
		return domain.StatusProcessing
	case "DISPATCHED", "IN_TRANSIT":
		return domain.StatusShipped
	case "COMPLETE", "CLOSED":
		return domain.StatusDelivered
	case "VOID", "ABANDONED":
		return domain.StatusCancelled
	default:
		return domain.StatusPending
	}
}

func toLegacyState(s domain.Status) string {
	switch s {
	case domain.StatusPending:
		return "NEW"
	case domain.StatusProcessing:
		return "PICKING"
	case domain.StatusShipped:
		return "DISPATCHED"
	case domain.StatusDelivered:
		return "COMPLETE"
	case domain.StatusCancelled, domain.StatusFailed:
		return "VOID"
	default:
		return "NEW"
	}
}

var _ domain.LegacyOrderPort = (*HTTPLegacyAdapter)(nil)
