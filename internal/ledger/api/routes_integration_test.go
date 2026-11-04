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
	vasdb "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/idempotency"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/testkit"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/staffcheck"
)

type harness struct {
	router http.Handler
	staff  *staffcheck.Fake
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
	staff := staffcheck.New()
	svc := app.New(app.Deps{Pool: pool, Clock: testkit.NewClock(time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)), Identity: staff})
	return harness{router: api.Routes(api.NewHandlers(svc), slog.Default(), nil, idempotency.NewPostgresStore(pool)), staff: staff}
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
	bodies := map[string]any{"createTokenPrice": deal(uuid.New(), "50")}

	var ops []string
	for path, item := range spec.Paths.Map() {
		if !strings.HasPrefix(path, "/v1/ledger/staff/") {
			continue
		}
		for method, op := range item.Operations() {
			ops = append(ops, op.OperationID)
			concrete := strings.ReplaceAll(path, "{priceId}", uuid.NewString())
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
	want := []string{"approveTokenPrice", "createTokenPrice", "listTokenPrices", "withdrawTokenPrice"}
	if strings.Join(ops, ",") != strings.Join(want, ",") {
		t.Fatalf("staff operations in the spec %v, cases written for %v: add the new one's case", ops, want)
	}
}
