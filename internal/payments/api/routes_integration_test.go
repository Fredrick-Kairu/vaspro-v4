//go:build integration

package api_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments/api"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments/app"
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
	svc := app.New(app.Deps{Pool: pool, Accounts: access, Packs: map[string]*packs.Pack{"KE": ke}})
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
	req, err := http.NewRequest(http.MethodGet, h.srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if user != uuid.Nil {
		req.Header.Set(api.HeaderUserID, user.String())
	}
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: the test's own server
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body
}

func wantProblem(t *testing.T, status int, body map[string]any, wantStatus int, code string) {
	t.Helper()
	if status != wantStatus || body["code"] != code {
		t.Fatalf("got %d %v, want %d %s", status, body, wantStatus, code)
	}
}

// TestTenantOperations_NeedBillingRead: every /v1/payments/tenants/
// operation is 401 without a user, VAL-4003 for someone outside the
// account, and AUTH-4024 for a member without billing:read. It fails if
// an operation has no case.
func TestTenantOperations_NeedBillingRead(t *testing.T) {
	h := newHarness(t)
	h.seedPaybill(t, "active")
	spec, err := openapi3.NewLoader().LoadFromFile("../../../docs/api/payments.yaml")
	if err != nil {
		t.Fatal(err)
	}
	tenant, member, reader := uuid.New(), uuid.New(), uuid.New()
	h.access.Grant(member, tenant, "KE", "KES")
	h.access.Grant(reader, tenant, "KE", "KES", app.PermBillingRead)

	var ops []string
	for path, item := range spec.Paths.Map() {
		if !strings.HasPrefix(path, "/v1/payments/tenants/") {
			continue
		}
		for method, op := range item.Operations() {
			if method != http.MethodGet {
				t.Fatalf("%s %s: a write needs its own cases here", method, path)
			}
			ops = append(ops, op.OperationID)
			concrete := strings.ReplaceAll(path, "{tenantId}", tenant.String())
			t.Run(op.OperationID, func(t *testing.T) {
				status, got := h.get(t, concrete, uuid.Nil)
				wantProblem(t, status, got, http.StatusUnauthorized, "AUTH-4010")
				status, got = h.get(t, concrete, uuid.New())
				wantProblem(t, status, got, http.StatusNotFound, "VAL-4003")
				status, got = h.get(t, concrete, member)
				wantProblem(t, status, got, http.StatusForbidden, "AUTH-4024")
				if status, got = h.get(t, concrete, reader); status != http.StatusOK {
					t.Fatalf("billing:read: %d %v", status, got)
				}
			})
		}
	}
	sort.Strings(ops)
	if strings.Join(ops, ",") != "getTopupDetails" {
		t.Fatalf("tenant operations in the spec %v, cases written for [getTopupDetails]: add the new one's case", ops)
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
