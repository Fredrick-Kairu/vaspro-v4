//go:build integration

package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/shopspring/decimal"

	vasdb "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"
	platformevents "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/events"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/testkit"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/app"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/domain"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/staffcheck"
)

// harness is one migrated Postgres, the service connected as
// ledger_app (privileges only through ledger_service, as in
// production), and a superuser pool for arranging and inspecting.
type harness struct {
	super *vasdb.Pool
	app   *vasdb.Pool
	svc   *app.Service
	clk   *testkit.Clock
	staff *staffcheck.Fake
}

var testNow = time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)

func newHarness(t testing.TB) harness {
	t.Helper()
	ctx := context.Background()
	pg := testkit.StartPostgres(t, testkit.WithMigrations("../../../migrations/ledger"))
	if _, err := pg.Pool.Exec(ctx, `
		CREATE ROLE ledger_app LOGIN PASSWORD 'ledger_app';
		GRANT ledger_service TO ledger_app;`); err != nil {
		t.Fatalf("set up ledger_app: %v", err)
	}
	super, err := vasdb.NewPool(ctx, pg.DSN()+"&search_path=ledger,public")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(super.Close)
	appPool, err := vasdb.NewPool(ctx, pg.RoleDSN("ledger_app", "ledger_app")+"&search_path=ledger,public&pool_max_conns=40")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(appPool.Close)
	clk := testkit.NewClock(testNow)
	staff := staffcheck.New()
	h := harness{super: super, app: appPool, svc: app.New(app.Deps{Pool: appPool, Clock: clk, Identity: staff}), clk: clk, staff: staff}
	// List prices every test can buy at: SMS at KES 0.80 and UGX 12.5.
	seedPrice(t, h, nil, "sms", "KES", "0", "80", jan1, nil)
	seedPrice(t, h, nil, "sms", "UGX", "0", "12.5", jan1, nil)
	return h
}

var jan1 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// seedPrice inserts an approved, active token price as the superuser
// (staff entry and approval are 3.4b's API). tenant nil is a list
// price.
func seedPrice(t testing.TB, h harness, tenant *uuid.UUID, service, currency, minTokens, price string, from time.Time, to *time.Time) uuid.UUID {
	t.Helper()
	id := uuid.New()
	ref := ""
	if tenant != nil {
		ref = "deal-" + id.String()[:8]
	}
	if _, err := h.super.Exec(context.Background(), `
		INSERT INTO token_prices (id, tenant_id, service_code, currency, min_tokens, minor_per_token, deal_reference,
		                          status, effective_from, effective_to, created_by, approved_by)
		VALUES ($1, $2, $3, $4, $5, $6, nullif($7, ''), 'active', $8, $9, $10, $11)`,
		id, tenant, service, currency, minTokens, price, ref, from, to, uuid.New(), uuid.New()); err != nil {
		t.Fatalf("seed price: %v", err)
	}
	return id
}

func tok(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func purchase(tenant uuid.UUID, service, currency, tokens string) app.PurchaseRequest {
	return app.PurchaseRequest{
		TenantID: tenant, IdempotencyKey: uuid.NewString(), ServiceCode: service,
		Currency: currency, Tokens: tok(tokens),
	}
}

var (
	mainKES    = domain.AccountKey{Type: domain.MainWallet, Currency: "KES"}
	smsKES     = domain.AccountKey{Type: domain.ServiceWallet, ServiceCode: "sms", Currency: "KES"}
	recvKES    = domain.AccountKey{Type: domain.Receivable, Currency: "KES"}
	mpesaKES   = domain.AccountKey{Type: domain.ProviderClearing, Rail: "mpesa_ke", Currency: "KES"}
	revenueKES = domain.AccountKey{Type: domain.Revenue, ServiceCode: "sms", Currency: "KES"}
	vatKES     = domain.AccountKey{Type: domain.TaxPayable, Currency: "KES"}
	suspKES    = domain.AccountKey{Type: domain.Suspense, Currency: "KES"}
)

func d(k domain.AccountKey, amount int64) domain.Line {
	return domain.Line{Account: k, Direction: domain.Debit, AmountMinor: amount}
}
func c(k domain.AccountKey, amount int64) domain.Line {
	return domain.Line{Account: k, Direction: domain.Credit, AmountMinor: amount}
}

func post(tenant uuid.UUID, typ domain.TxnType, lines ...domain.Line) app.PostRequest {
	return app.PostRequest{
		TenantID: &tenant, IdempotencyKey: uuid.NewString(), Type: typ,
		Description: "test", Lines: lines,
	}
}

// topUp credits amount to tenant's KES main wallet from M-Pesa.
func topUp(t testing.TB, h harness, tenant uuid.UUID, amount int64) app.PostResult {
	t.Helper()
	res, err := h.svc.Post(context.Background(), post(tenant, domain.TxnTopup, d(mpesaKES, amount), c(mainKES, amount)))
	if err != nil {
		t.Fatalf("top-up: %v", err)
	}
	return res
}

// fundSMS tops up and buys tokens SMS units at the KES list price
// (0.80), for a tenant with no deal.
func fundSMS(t testing.TB, h harness, tenant uuid.UUID, tokens int64) {
	t.Helper()
	topUp(t, h, tenant, tokens*80)
	if _, err := h.svc.PurchaseTokens(context.Background(), purchase(tenant, "sms", "KES", decimal.NewFromInt(tokens).String())); err != nil {
		t.Fatalf("token purchase: %v", err)
	}
}

func balanceOf(t testing.TB, h harness, tenant uuid.UUID, key domain.AccountKey) app.Balance {
	t.Helper()
	bals, err := h.svc.GetBalances(context.Background(), &tenant)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range bals {
		if b.Account == key {
			return b
		}
	}
	t.Fatalf("tenant %s has no %+v account", tenant, key)
	return app.Balance{}
}

func systemBalance(t testing.TB, h harness, key domain.AccountKey) int64 {
	t.Helper()
	bals, err := h.svc.GetBalances(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range bals {
		if b.Account == key {
			return b.BalanceMinor
		}
	}
	return 0
}

func count(t testing.TB, h harness, query string, args ...any) int {
	t.Helper()
	var n int
	if err := h.super.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func wantErr(t testing.TB, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("got %v, want %v", err, want)
	}
}

// assertEventValid checks the newest outbox event of eventType for
// aggregate against its schema in docs/events and returns its data.
func assertEventValid(t *testing.T, h harness, eventType string, aggregate uuid.UUID) map[string]any {
	t.Helper()
	var payload []byte
	err := h.super.QueryRow(context.Background(),
		`SELECT payload FROM outbox_events WHERE event_type = $1 AND aggregate_id = $2 ORDER BY occurred_at DESC LIMIT 1`,
		eventType, aggregate).Scan(&payload)
	if errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("no %s event for %s in the outbox", eventType, aggregate)
	}
	if err != nil {
		t.Fatal(err)
	}
	var env platformevents.Envelope
	if err := json.Unmarshal(payload, &env); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../../../docs/events/" + eventType + ".schema.json") //nolint:gosec // a fixed name from the test
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	comp := jsonschema.NewCompiler()
	if err := comp.AddResource(eventType+".schema.json", doc); err != nil {
		t.Fatal(err)
	}
	schema, err := comp.Compile(eventType + ".schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var data any
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(data); err != nil {
		t.Fatalf("%s doesn't match its schema: %v", eventType, err)
	}
	return data.(map[string]any)
}
