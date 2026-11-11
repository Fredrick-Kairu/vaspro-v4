//go:build integration

package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sort"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments/api"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments/app"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments/domain"
	vasdb "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/idempotency"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/packs"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/testkit"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/tenantinfo"
)

type harness struct {
	srv    *httptest.Server
	super  *vasdb.Pool
	access *tenantinfo.Fake
}

var callbackSecret = bytes.Repeat([]byte{5}, 32)

func newHarness(t *testing.T) harness {
	t.Helper()
	ctx := context.Background()
	pg := testkit.StartPostgres(t, testkit.WithMigrations("../../../migrations/payments"))
	if _, err := pg.Pool.Exec(ctx, `CREATE ROLE payments_app LOGIN PASSWORD 'payments_app'; GRANT payments_service TO payments_app;`); err != nil {
		t.Fatal(err)
	}
	super, err := vasdb.NewPool(ctx, pg.DSN()+"&search_path=payments,public")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(super.Close)
	pool, err := vasdb.NewPool(ctx, pg.RoleDSN("payments_app", "payments_app")+"&search_path=payments,public")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	ke, err := packs.Load("../../../packs/schema.json", "../../../packs/ke/pack.yaml")
	if err != nil {
		t.Fatal(err)
	}
	access := tenantinfo.New()
	svc := app.New(app.Deps{
		Pool: pool, Accounts: access, Packs: map[string]*packs.Pack{"KE": ke},
		CallbackSecret: callbackSecret, CallbackCIDRs: []netip.Prefix{netip.MustParsePrefix("196.201.214.0/24")},
	})
	if err := svc.SyncRails(ctx); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(api.Routes(api.NewHandlers(svc), slog.New(slog.NewTextHandler(io.Discard, nil)), nil, idempotency.NewPostgresStore(pool)))
	t.Cleanup(srv.Close)
	return harness{srv: srv, super: super, access: access}
}

func (h harness) seedPaybill(t *testing.T, status string) {
	t.Helper()
	verified := "NULL"
	if status == "active" {
		verified = "now()"
	}
	if _, err := h.super.Exec(context.Background(), `
		INSERT INTO rail_accounts (id, rail_code, account_type, number, display_name, uses, status, verified_at)
		VALUES (gen_random_uuid(), 'mpesa_ke', 'paybill', '600100', 'VASPRO', '{vaspro_topups}', $1, `+verified+`)`, status); err != nil {
		t.Fatal(err)
	}
}

// get calls path as user (uuid.Nil: no user header) and decodes the
// JSON body.
func (h harness) get(t *testing.T, path string, user uuid.UUID) (int, map[string]any) {
	t.Helper()
	return h.do(t, http.MethodGet, path, user, nil, nil)
}

// do sends method path as user with body (JSON) and extra headers; a
// write gets a fresh Idempotency-Key unless headers give one.
func (h harness) do(t *testing.T, method, path string, user uuid.UUID, body any, headers map[string]string) (int, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, h.srv.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if method != http.MethodGet {
		req.Header.Set("Idempotency-Key", uuid.NewString())
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if user != uuid.Nil {
		req.Header.Set(api.HeaderUserID, user.String())
	}
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: the test's own server
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var decoded map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&decoded)
	return resp.StatusCode, decoded
}

func wantProblem(t *testing.T, status int, body map[string]any, wantStatus int, code string) {
	t.Helper()
	if status != wantStatus || body["code"] != code {
		t.Fatalf("got %d %v, want %d %s", status, body, wantStatus, code)
	}
}

// TestTenantOperations_NeedBillingPermissions: every
// /v1/payments/tenants/ operation is 401 without a user, VAL-4003 for
// someone outside the account, and AUTH-4024 for a member without
// billing:read; writes need billing:manage, so a reader gets AUTH-4024
// there too. It fails if an operation has no case.
func TestTenantOperations_NeedBillingPermissions(t *testing.T) {
	h := newHarness(t)
	h.seedPaybill(t, "active")
	spec, err := openapi3.NewLoader().LoadFromFile("../../../docs/api/payments.yaml")
	if err != nil {
		t.Fatal(err)
	}
	tenant, member, reader := uuid.New(), uuid.New(), uuid.New()
	h.access.Grant(member, tenant, "KE", "KES")
	h.access.Grant(reader, tenant, "KE", "KES", app.PermBillingRead)
	topup := h.seedTopup(t, tenant)

	writes := map[string]bool{"createTopup": true}
	bodies := map[string]any{"createTopup": map[string]any{"amount_minor": 150000, "payer_phone": "+254708374149"}}
	var ops []string
	for path, item := range spec.Paths.Map() {
		if !strings.HasPrefix(path, "/v1/payments/tenants/") {
			continue
		}
		for method, op := range item.Operations() {
			ops = append(ops, op.OperationID)
			concrete := strings.NewReplacer("{tenantId}", tenant.String(), "{topupId}", topup.String()).Replace(path)
			body := bodies[op.OperationID]
			t.Run(op.OperationID, func(t *testing.T) {
				status, got := h.do(t, method, concrete, uuid.Nil, body, nil)
				wantProblem(t, status, got, http.StatusUnauthorized, "AUTH-4010")
				status, got = h.do(t, method, concrete, uuid.New(), body, nil)
				wantProblem(t, status, got, http.StatusNotFound, "VAL-4003")
				status, got = h.do(t, method, concrete, member, body, nil)
				wantProblem(t, status, got, http.StatusForbidden, "AUTH-4024")
				status, got = h.do(t, method, concrete, reader, body, nil)
				if writes[op.OperationID] {
					wantProblem(t, status, got, http.StatusForbidden, "AUTH-4024")
				} else if status != http.StatusOK {
					t.Fatalf("billing:read: %d %v", status, got)
				}
			})
		}
	}
	sort.Strings(ops)
	if want := "createTopup,getTopup,getTopupDetails"; strings.Join(ops, ",") != want {
		t.Fatalf("tenant operations in the spec %v, cases written for %s: add the new one's case", ops, want)
	}
}

// seedTopup inserts a pending top-up for tenant as the owner.
func (h harness) seedTopup(t *testing.T, tenant uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := h.super.Exec(context.Background(), `
		INSERT INTO payment_intents (id, tenant_id, purpose, rail_account_id, method, amount_minor, currency, payer_phone_e164,
		  account_reference, status, idempotency_key, expires_at)
		VALUES ($1, $2, 'wallet_topup', (SELECT id FROM rail_accounts LIMIT 1), 'stk_push', 150000, 'KES', '+254708374149',
		  'ABC23456', 'pending', $3, now() + interval '3 minutes')`, id, tenant, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	return id
}

// TestReceiveStkCallback: Daraja's callback needs no session and no
// Idempotency-Key, and is refused (AUTH-4013) from outside Safaricom's
// ranges or without the paybill's token; a malformed body is VAL-4000.
func TestReceiveStkCallback(t *testing.T) {
	h := newHarness(t)
	h.seedPaybill(t, "active")
	var paybill uuid.UUID
	if err := h.super.QueryRow(context.Background(), `SELECT id FROM rail_accounts LIMIT 1`).Scan(&paybill); err != nil {
		t.Fatal(err)
	}
	path := "/v1/payments/callbacks/daraja/" + paybill.String() + "/" + domain.CallbackToken(callbackSecret, paybill) + "/stk"
	safaricom := map[string]string{api.HeaderClientIP: "196.201.214.200", "Idempotency-Key": ""}
	body := map[string]any{"Body": map[string]any{}}

	status, got := h.do(t, http.MethodPost, path, uuid.Nil, body, safaricom)
	wantProblem(t, status, got, http.StatusBadRequest, "VAL-4000")

	bad := "/v1/payments/callbacks/daraja/" + paybill.String() + "/" + strings.Repeat("0", 64) + "/stk"
	status, got = h.do(t, http.MethodPost, bad, uuid.Nil, body, safaricom)
	wantProblem(t, status, got, http.StatusUnauthorized, "AUTH-4013")
	status, got = h.do(t, http.MethodPost, path, uuid.Nil, body, map[string]string{api.HeaderClientIP: "8.8.8.8", "Idempotency-Key": ""})
	wantProblem(t, status, got, http.StatusUnauthorized, "AUTH-4013")
	status, got = h.do(t, http.MethodPost, path, uuid.Nil, body, map[string]string{"Idempotency-Key": ""})
	wantProblem(t, status, got, http.StatusUnauthorized, "AUTH-4013")

	var n int
	if err := h.super.QueryRow(context.Background(), `SELECT count(*) FROM stk_callbacks`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d callbacks recorded, %v", n, err)
	}
}

func TestGetTopupDetails(t *testing.T) {
	h := newHarness(t)
	tenant, reader := uuid.New(), uuid.New()
	h.access.Grant(reader, tenant, "KE", "KES", app.PermBillingRead)
	path := "/v1/payments/tenants/" + tenant.String() + "/topup-details"

	status, body := h.get(t, path, reader)
	wantProblem(t, status, body, http.StatusServiceUnavailable, "PAY-6507")

	h.seedPaybill(t, "active")
	status, body = h.get(t, path, reader)
	if status != http.StatusOK || body["rail"] != "mpesa_ke" || body["paybill_number"] != "600100" || body["paybill_name"] != "VASPRO" ||
		body["currency"] != "KES" || len(body["account_number"].(string)) != 8 {
		t.Fatalf("got %d %v", status, body)
	}
	_, again := h.get(t, path, reader)
	if again["account_number"] != body["account_number"] {
		t.Fatalf("the number changed: %v then %v", body["account_number"], again["account_number"])
	}

	status, body = h.get(t, "/v1/payments/tenants/not-a-uuid/topup-details", reader)
	wantProblem(t, status, body, http.StatusNotFound, "VAL-4003")
}
