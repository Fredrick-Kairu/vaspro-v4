//go:build integration

package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/api"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/app"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/domain"
	vasdb "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/idempotency"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/testkit"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/staffcheck"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/tenantcheck"
)

type harness struct {
	router http.Handler
	staff  *staffcheck.Fake
	access *tenantcheck.Fake
	svc    *app.Service
}

func newHarness(t *testing.T) harness {
	t.Helper()
	ctx := context.Background()
	pg := testkit.StartPostgres(t, testkit.WithMigrations("../../../migrations/ledger"))
	if _, err := pg.Pool.Exec(ctx, `CREATE ROLE ledger_app LOGIN PASSWORD 'ledger_app'; GRANT ledger_service TO ledger_app;`); err != nil {
		t.Fatal(err)
	}
	pool, err := vasdb.NewPool(ctx, pg.RoleDSN("ledger_app", "ledger_app")+"&search_path=ledger,public")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pg.Pool.Exec(ctx, `
		INSERT INTO ledger.token_prices (id, service_code, currency, minor_per_token, status, effective_from, created_by, approved_by)
		VALUES ($1, 'sms', 'KES', 80, 'active', '2026-01-01', $2, $3)`, uuid.New(), uuid.New(), uuid.New()); err != nil {
		t.Fatal(err)
	}
	staff, access := staffcheck.New(), tenantcheck.New()
	svc := app.New(app.Deps{Pool: pool, Clock: testkit.NewClock(time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)), Identity: staff, Accounts: access})
	return harness{router: api.Routes(api.NewHandlers(svc), slog.Default(), nil, idempotency.NewPostgresStore(pool)), staff: staff, access: access, svc: svc}
}

type caller struct{ user, session uuid.UUID }

func (h harness) staffMember() caller {
	c := caller{uuid.New(), uuid.New()}
	h.staff.SetStaff(c.user, true)
	h.staff.StepUp(c.user, c.session)
	return c
}

func (h harness) do(t *testing.T, method, path string, who *caller, body any, key string) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if who != nil {
		req.Header.Set(api.HeaderUserID, who.user.String())
		req.Header.Set(api.HeaderSessionID, who.session.String())
	}
	if method != http.MethodGet {
		if key == "" {
			key = uuid.NewString()
		}
		req.Header.Set("Idempotency-Key", key)
	}
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func wantProblem(t *testing.T, status int, body map[string]any, wantStatus int, code string) {
	t.Helper()
	if status != wantStatus || body["code"] != code {
		t.Fatalf("got %d %v, want %d %s", status, body["code"], wantStatus, code)
	}
}

func deal(tenant uuid.UUID, price string) map[string]any {
	return map[string]any{
		"tenant_id": tenant.String(), "service_code": "sms", "currency": "KES",
		"minor_per_token": price, "deal_reference": "Q-" + price, "effective_from": "2026-01-01T00:00:00Z",
	}
}

func TestTokenPricesAPI(t *testing.T) {
	h := newHarness(t)
	maker, checker := h.staffMember(), h.staffMember()
	tenant := uuid.New()
	const base = "/v1/ledger/staff/token-prices"

	key := uuid.NewString()
	status, created := h.do(t, http.MethodPost, base, &maker, deal(tenant, "50"), key)
	if status != http.StatusCreated || created["status"] != "pending_approval" || created["minor_per_token"] != "50" ||
		created["tenant_id"] != tenant.String() || created["created_by"] != maker.user.String() {
		t.Fatalf("create: %d %v", status, created)
	}
	id := created["id"].(string)

	t.Run("the same key replays the creation", func(t *testing.T) {
		status, again := h.do(t, http.MethodPost, base, &maker, deal(tenant, "50"), key)
		if status != http.StatusCreated || again["id"] != id {
			t.Fatalf("replay: %d %v, want the same price", status, again)
		}
	})

	t.Run("a deal without its reference", func(t *testing.T) {
		bad := deal(tenant, "50")
		delete(bad, "deal_reference")
		status, body := h.do(t, http.MethodPost, base, &maker, bad, "")
		wantProblem(t, status, body, http.StatusBadRequest, "VAL-4000")
	})

	t.Run("a tenant that isn't a UUID", func(t *testing.T) {
		bad := deal(tenant, "50")
		bad["tenant_id"] = "acme"
		status, body := h.do(t, http.MethodPost, base, &maker, bad, "")
		wantProblem(t, status, body, http.StatusBadRequest, "VAL-4000")
	})

	t.Run("the maker can't approve", func(t *testing.T) {
		status, body := h.do(t, http.MethodPost, base+"/"+id+"/approval", &maker, nil, "")
		wantProblem(t, status, body, http.StatusForbidden, "VAL-4013")
	})

	t.Run("approval needs step-up", func(t *testing.T) {
		stale := caller{checker.user, uuid.New()}
		status, body := h.do(t, http.MethodPost, base+"/"+id+"/approval", &stale, nil, "")
		wantProblem(t, status, body, http.StatusUnauthorized, "AUTH-4012")
	})

	status, approved := h.do(t, http.MethodPost, base+"/"+id+"/approval", &checker, nil, "")
	if status != http.StatusOK || approved["status"] != "active" || approved["approved_by"] != checker.user.String() {
		t.Fatalf("approve: %d %v", status, approved)
	}

	t.Run("approving again", func(t *testing.T) {
		status, body := h.do(t, http.MethodPost, base+"/"+id+"/approval", &checker, nil, "")
		wantProblem(t, status, body, http.StatusConflict, "VAL-4014")
	})

	t.Run("an overlapping deal", func(t *testing.T) {
		_, other := h.do(t, http.MethodPost, base, &maker, deal(tenant, "45"), "")
		status, body := h.do(t, http.MethodPost, base+"/"+other["id"].(string)+"/approval", &checker, nil, "")
		wantProblem(t, status, body, http.StatusConflict, "VAL-4012")
	})

	t.Run("an unknown price", func(t *testing.T) {
		status, body := h.do(t, http.MethodPost, base+"/"+uuid.NewString()+"/approval", &checker, nil, "")
		wantProblem(t, status, body, http.StatusNotFound, "VAL-4003")
		status, body = h.do(t, http.MethodPost, base+"/not-a-uuid/withdrawal", &checker, nil, "")
		wantProblem(t, status, body, http.StatusNotFound, "VAL-4003")
	})

	t.Run("the list, paged", func(t *testing.T) {
		status, page := h.do(t, http.MethodGet, base+"?tenant_id="+tenant.String()+"&limit=1", &maker, nil, "")
		if status != http.StatusOK || len(page["prices"].([]any)) != 1 || page["next_cursor"] == nil {
			t.Fatalf("first page: %d %v", status, page)
		}
		status, next := h.do(t, http.MethodGet, base+"?tenant_id="+tenant.String()+"&limit=1&cursor="+page["next_cursor"].(string), &maker, nil, "")
		if status != http.StatusOK || len(next["prices"].([]any)) != 1 || next["next_cursor"] != nil {
			t.Fatalf("second page: %d %v", status, next)
		}
		status, body := h.do(t, http.MethodGet, base+"?cursor=@@", &maker, nil, "")
		wantProblem(t, status, body, http.StatusBadRequest, "VAL-4000")
	})

	status, withdrawn := h.do(t, http.MethodPost, base+"/"+id+"/withdrawal", &maker, nil, "")
	if status != http.StatusOK || withdrawn["status"] != "withdrawn" {
		t.Fatalf("withdraw: %d %v", status, withdrawn)
	}
}

// TestStaffOperations_RefuseEveryoneElse is spec-driven: every
// operation under /v1/ledger/staff/ in docs/api/ledger.yaml is 401
// AUTH-4010 without a signed-in user and 403 AUTH-4025 for a user who
// isn't staff. It fails if an operation has no case.
func TestStaffOperations_RefuseEveryoneElse(t *testing.T) {
	h := newHarness(t)
	spec, err := openapi3.NewLoader().LoadFromFile("../../../docs/api/ledger.yaml")
	if err != nil {
		t.Fatal(err)
	}
	bodies := map[string]any{
		"createTokenPrice": deal(uuid.New(), "50"),
		"createReversal":   reversal(uuid.New(), uuid.New(), 100),
		"rejectReversal":   map[string]any{"reason": "no"},
		"createDevTopUp":   map[string]any{"tenant_id": uuid.NewString(), "currency": "KES", "amount_minor": 100},
	}

	var ops []string
	for path, item := range spec.Paths.Map() {
		if !strings.HasPrefix(path, "/v1/ledger/staff/") {
			continue
		}
		for method, op := range item.Operations() {
			ops = append(ops, op.OperationID)
			concrete := strings.NewReplacer("{priceId}", uuid.NewString(), "{reversalId}", uuid.NewString()).Replace(path)
			t.Run(op.OperationID, func(t *testing.T) {
				status, body := h.do(t, method, concrete, nil, bodies[op.OperationID], "")
				wantProblem(t, status, body, http.StatusUnauthorized, "AUTH-4010")

				outsider := caller{uuid.New(), uuid.New()}
				h.staff.StepUp(outsider.user, outsider.session) // stepped up, but not staff
				status, body = h.do(t, method, concrete, &outsider, bodies[op.OperationID], "")
				wantProblem(t, status, body, http.StatusForbidden, "AUTH-4025")
			})
		}
	}
	sort.Strings(ops)
	want := []string{"approveReversal", "approveTokenPrice", "createDevTopUp", "createReversal", "createTokenPrice", "getReversal",
		"listReversals", "listTokenPrices", "rejectReversal", "withdrawTokenPrice"}
	if strings.Join(ops, ",") != strings.Join(want, ",") {
		t.Fatalf("staff operations in the spec %v, cases written for %v: add the new one's case", ops, want)
	}
}

func reversal(tenant, original uuid.UUID, amount int64) map[string]any {
	return map[string]any{
		"tenant_id": tenant.String(), "original_transaction_id": original.String(), "kind": "payback",
		"amount_minor": amount, "reason": "paid twice",
	}
}

// TestReversalsAPI covers the reversal routes' codes (task 3.7). This
// service has no country pack, so every reversal needs a finance lead
// (fails closed).
func TestReversalsAPI(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	maker, checker, lead := h.staffMember(), h.staffMember(), h.staffMember()
	h.staff.SetFinanceLead(lead.user, true)
	tenant := uuid.New()
	main := domain.AccountKey{Type: domain.MainWallet, Currency: "KES"}
	mpesa := domain.AccountKey{Type: domain.ProviderClearing, Rail: "mpesa_ke", Currency: "KES"}
	top, err := h.svc.Post(ctx, app.PostRequest{TenantID: &tenant, IdempotencyKey: "t1", Type: domain.TxnTopup, Description: "top-up",
		Lines: []domain.Line{{Account: mpesa, Direction: domain.Debit, AmountMinor: 50_000}, {Account: main, Direction: domain.Credit, AmountMinor: 50_000}}})
	if err != nil {
		t.Fatal(err)
	}
	const base = "/v1/ledger/staff/reversals"

	key := uuid.NewString()
	status, created := h.do(t, http.MethodPost, base, &maker, reversal(tenant, top.TransactionID, 30_000), key)
	if status != http.StatusCreated || created["status"] != "requested" || created["currency"] != "KES" || created["requested_by"] != maker.user.String() {
		t.Fatalf("create: %d %v", status, created)
	}
	id := created["id"].(string)
	if status, again := h.do(t, http.MethodPost, base, &maker, reversal(tenant, top.TransactionID, 30_000), key); status != http.StatusCreated || again["id"] != id {
		t.Fatalf("replay: %d %v", status, again)
	}

	t.Run("refusals at request", func(t *testing.T) {
		status, body := h.do(t, http.MethodPost, base, &maker, reversal(tenant, uuid.New(), 1), "")
		wantProblem(t, status, body, http.StatusNotFound, "VAL-4003")
		status, body = h.do(t, http.MethodPost, base, &maker, reversal(tenant, top.TransactionID, 20_001), "")
		wantProblem(t, status, body, http.StatusConflict, "VAL-4015")
		bad := reversal(tenant, top.TransactionID, 1)
		bad["kind"] = "refund"
		status, body = h.do(t, http.MethodPost, base, &maker, bad, "")
		wantProblem(t, status, body, http.StatusBadRequest, "VAL-4000")
	})

	t.Run("the preview", func(t *testing.T) {
		status, got := h.do(t, http.MethodGet, base+"/"+id, &checker, nil, "")
		preview, _ := got["preview"].(map[string]any)
		if status != http.StatusOK || preview == nil || preview["covered"] != true || preview["from_main_minor"] != float64(30_000) {
			t.Fatalf("get: %d %v", status, got)
		}
	})

	t.Run("who may approve", func(t *testing.T) {
		status, body := h.do(t, http.MethodPost, base+"/"+id+"/approval", &maker, nil, "")
		wantProblem(t, status, body, http.StatusForbidden, "VAL-4013")
		status, body = h.do(t, http.MethodPost, base+"/"+id+"/approval", &checker, nil, "")
		wantProblem(t, status, body, http.StatusForbidden, "AUTH-4026")
	})

	status, approved := h.do(t, http.MethodPost, base+"/"+id+"/approval", &lead, nil, "")
	if status != http.StatusOK || approved["status"] != "payback_pending" || approved["reversal_transaction_id"] == nil {
		t.Fatalf("approve: %d %v", status, approved)
	}
	status, body := h.do(t, http.MethodPost, base+"/"+id+"/approval", &lead, nil, "")
	wantProblem(t, status, body, http.StatusConflict, "VAL-4014")

	t.Run("not covered, then rejected", func(t *testing.T) {
		_, second := h.do(t, http.MethodPost, base, &maker, reversal(tenant, top.TransactionID, 20_000), "")
		if _, err := h.svc.PlaceHold(ctx, app.HoldRequest{TenantID: tenant, Account: main, ReferenceType: "fee", ReferenceID: uuid.New(),
			AmountMinor: 1, ExpiresAt: time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)}); err != nil {
			t.Fatal(err)
		}
		id := second["id"].(string)
		status, body := h.do(t, http.MethodPost, base+"/"+id+"/approval", &lead, nil, "")
		wantProblem(t, status, body, http.StatusConflict, "BAL-4007")
		status, body = h.do(t, http.MethodPost, base+"/"+id+"/rejection", &checker, map[string]any{"reason": " "}, "")
		wantProblem(t, status, body, http.StatusBadRequest, "VAL-4000")
		status, rejected := h.do(t, http.MethodPost, base+"/"+id+"/rejection", &checker, map[string]any{"reason": "the client kept the money"}, "")
		if status != http.StatusOK || rejected["status"] != "rejected" || rejected["rejection_reason"] != "the client kept the money" {
			t.Fatalf("reject: %d %v", status, rejected)
		}
	})

	t.Run("the list by status", func(t *testing.T) {
		status, page := h.do(t, http.MethodGet, base+"?status=rejected", &checker, nil, "")
		if status != http.StatusOK || len(page["reversals"].([]any)) != 1 {
			t.Fatalf("list: %d %v", status, page)
		}
	})
}

// tenantOperationBodies are valid bodies for the tenant routes' writes.
var tenantOperationBodies = map[string]any{
	"createTokenPurchase":  map[string]any{"service_code": "sms", "currency": "KES", "tokens": "10"},
	"setTenantAutoBuyRule": map[string]any{"below_tokens": "10", "buy_tokens": "100", "enabled": true},
}

// tenantPath fills a tenant route's parameters.
func tenantPath(path string, tenant, account uuid.UUID) string {
	return strings.NewReplacer("{tenantId}", tenant.String(), "{accountId}", account.String(),
		"{serviceCode}", "sms", "{currency}", "KES").Replace(path) + map[bool]string{true: "?service_code=sms&currency=KES&tokens=1"}[strings.HasSuffix(path, "/token-quote")]
}

// TestTenantOperations_NeedBillingPermissions is spec-driven (task 3.9,
// OQ-20): every /v1/ledger/tenants/ operation is 401 without a user,
// VAL-4003 for someone outside the account, and AUTH-4024 for a member
// without the billing permission it needs; writes need billing:manage,
// so a read-only member gets AUTH-4024 there too. It fails if an
// operation has no case.
func TestTenantOperations_NeedBillingPermissions(t *testing.T) {
	h := newHarness(t)
	spec, err := openapi3.NewLoader().LoadFromFile("../../../docs/api/ledger.yaml")
	if err != nil {
		t.Fatal(err)
	}
	tenant := uuid.New()
	main := domain.AccountKey{Type: domain.MainWallet, Currency: "KES"}
	top, err := h.svc.Post(context.Background(), app.PostRequest{TenantID: &tenant, IdempotencyKey: "t1", Type: domain.TxnTopup, Description: "top-up",
		Lines: []domain.Line{{Account: domain.AccountKey{Type: domain.ProviderClearing, Rail: "mpesa_ke", Currency: "KES"}, Direction: domain.Debit, AmountMinor: 100}, {Account: main, Direction: domain.Credit, AmountMinor: 100}}})
	if err != nil {
		t.Fatal(err)
	}
	var account uuid.UUID
	for _, l := range top.Lines {
		if l.Line.Account == main {
			account = l.AccountID
		}
	}
	stranger, member, reader := caller{uuid.New(), uuid.New()}, caller{uuid.New(), uuid.New()}, caller{uuid.New(), uuid.New()}
	h.access.Grant(member.user, tenant)
	h.access.Grant(reader.user, tenant, app.PermBillingRead)

	writes := map[string]bool{"createTokenPurchase": true, "setTenantAutoBuyRule": true}
	var ops []string
	for path, item := range spec.Paths.Map() {
		if !strings.HasPrefix(path, "/v1/ledger/tenants/") {
			continue
		}
		for method, op := range item.Operations() {
			ops = append(ops, op.OperationID)
			concrete := tenantPath(path, tenant, account)
			body := tenantOperationBodies[op.OperationID]
			t.Run(op.OperationID, func(t *testing.T) {
				status, got := h.do(t, method, concrete, nil, body, "")
				wantProblem(t, status, got, http.StatusUnauthorized, "AUTH-4010")
				status, got = h.do(t, method, concrete, &stranger, body, "")
				wantProblem(t, status, got, http.StatusNotFound, "VAL-4003")
				status, got = h.do(t, method, concrete, &member, body, "")
				wantProblem(t, status, got, http.StatusForbidden, "AUTH-4024")
				status, got = h.do(t, method, concrete, &reader, body, "")
				if writes[op.OperationID] {
					wantProblem(t, status, got, http.StatusForbidden, "AUTH-4024")
				} else if status != http.StatusOK {
					t.Fatalf("billing:read on a read: %d %v", status, got)
				}
			})
		}
	}
	sort.Strings(ops)
	want := []string{"createTokenPurchase", "getStatement", "listBalances", "listTenantAutoBuyRules", "listTenantTokenPrices",
		"quoteTokenPurchase", "setTenantAutoBuyRule"}
	if strings.Join(ops, ",") != strings.Join(want, ",") {
		t.Fatalf("tenant operations in the spec %v, cases written for %v: add the new one's case", ops, want)
	}
}

// TestDashboardAPI: the dashboard's wallet flow over HTTP (task 3.9).
func TestDashboardAPI(t *testing.T) {
	h := newHarness(t)
	tenant := uuid.New()
	finance := caller{uuid.New(), uuid.New()}
	h.access.Grant(finance.user, tenant, app.PermBillingRead, app.PermBillingManage)
	base := "/v1/ledger/tenants/" + tenant.String()

	staff := h.staffMember()
	status, body := h.do(t, http.MethodPost, "/v1/ledger/staff/dev-top-ups", &staff, map[string]any{"tenant_id": tenant.String(), "currency": "KES", "amount_minor": 100_000}, "")
	wantProblem(t, status, body, http.StatusNotFound, "VAL-4003") // off in this service
	if _, err := h.svc.Post(context.Background(), app.PostRequest{TenantID: &tenant, IdempotencyKey: "t1", Type: domain.TxnTopup, Description: "top-up",
		Lines: []domain.Line{{Account: domain.AccountKey{Type: domain.ProviderClearing, Rail: "mpesa_ke", Currency: "KES"}, Direction: domain.Debit, AmountMinor: 100_000},
			{Account: domain.AccountKey{Type: domain.MainWallet, Currency: "KES"}, Direction: domain.Credit, AmountMinor: 100_000}}}); err != nil {
		t.Fatal(err)
	}

	status, quote := h.do(t, http.MethodGet, base+"/token-quote?service_code=sms&currency=KES&tokens=1000", &finance, nil, "")
	if status != http.StatusOK || quote["cost_minor"] != float64(80_000) || quote["affordable"] != true || quote["source"] != "list" {
		t.Fatalf("quote: %d %v", status, quote)
	}
	key := uuid.NewString()
	status, bought := h.do(t, http.MethodPost, base+"/token-purchases", &finance, map[string]any{"service_code": "sms", "currency": "KES", "tokens": "1000"}, key)
	if status != http.StatusCreated || bought["cost_minor"] != float64(80_000) {
		t.Fatalf("purchase: %d %v", status, bought)
	}
	if status, again := h.do(t, http.MethodPost, base+"/token-purchases", &finance, map[string]any{"service_code": "sms", "currency": "KES", "tokens": "1000"}, key); status != http.StatusCreated || again["lot_id"] != bought["lot_id"] {
		t.Fatalf("replay: %d %v", status, again)
	}
	status, body = h.do(t, http.MethodPost, base+"/token-purchases", &finance, map[string]any{"service_code": "sms", "currency": "KES", "tokens": "1000"}, "")
	wantProblem(t, status, body, http.StatusPaymentRequired, "BAL-4001")
	status, body = h.do(t, http.MethodGet, base+"/token-quote?service_code=whatsapp&currency=KES&tokens=1", &finance, nil, "")
	wantProblem(t, status, body, http.StatusConflict, "BAL-4005")

	status, bals := h.do(t, http.MethodGet, base+"/balances", &finance, nil, "")
	if status != http.StatusOK {
		t.Fatalf("balances: %d %v", status, bals)
	}
	var mainID, smsTokens string
	for _, b := range bals["balances"].([]any) {
		b := b.(map[string]any)
		switch b["type"] {
		case "main_wallet":
			mainID = b["account_id"].(string)
		case "service_wallet":
			smsTokens, _ = b["tokens_remaining"].(string)
		}
	}
	if smsTokens != "1000" {
		t.Fatalf("SMS tokens %q, want 1000", smsTokens)
	}
	status, st := h.do(t, http.MethodGet, base+"/accounts/"+mainID+"/statement?limit=1", &finance, nil, "")
	lines, _ := st["lines"].([]any)
	if status != http.StatusOK || len(lines) != 1 || st["next_cursor"] == nil {
		t.Fatalf("statement: %d %v", status, st)
	}
	if l := lines[0].(map[string]any); l["direction"] != "debit" || l["balance_after_minor"] != float64(20_000) || l["type"] != "token_purchase" {
		t.Fatalf("newest line %v", l)
	}

	status, rule := h.do(t, http.MethodPut, base+"/auto-buy-rules/sms/KES", &finance, map[string]any{"below_tokens": "100", "buy_tokens": "500", "enabled": true}, "")
	if status != http.StatusOK || rule["buy_tokens"] != "500" {
		t.Fatalf("rule: %d %v", status, rule)
	}
	status, body = h.do(t, http.MethodPut, base+"/auto-buy-rules/sms/KES", &finance, map[string]any{"below_tokens": "100", "buy_tokens": "0", "enabled": true}, "")
	wantProblem(t, status, body, http.StatusBadRequest, "VAL-4000")
	status, prices := h.do(t, http.MethodGet, base+"/token-prices", &finance, nil, "")
	if status != http.StatusOK || len(prices["prices"].([]any)) != 1 {
		t.Fatalf("prices: %d %v", status, prices)
	}
}
