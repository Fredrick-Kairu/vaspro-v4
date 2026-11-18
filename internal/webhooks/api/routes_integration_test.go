//go:build integration

package api_test

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/bus"
	vascrypto "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/crypto"
	vasdb "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/events"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/idempotency"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/packs"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/testkit"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/staffcheck"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/tenantperms"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/webhooks/adapters"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/webhooks/api"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/webhooks/app"
)

// receiver is a client's endpoint over TLS: it echoes a challenge, or
// answers wrongly once told to.
type receiver struct {
	srv   *httptest.Server
	mu    sync.Mutex
	wrong bool
	hits  int
}

func newReceiver(t *testing.T) *receiver {
	r := &receiver{}
	r.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		var env struct {
			Data struct {
				Challenge string `json:"challenge"`
			} `json:"data"`
		}
		_ = json.Unmarshal(body, &env)
		r.mu.Lock()
		r.hits++
		wrong := r.wrong
		r.mu.Unlock()
		if wrong {
			_, _ = w.Write([]byte(`{"challenge":"nope"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"challenge": env.Data.Challenge})
	}))
	t.Cleanup(r.srv.Close)
	return r
}

type harness struct {
	srv      *httptest.Server
	svc      *app.Service
	super    *vasdb.Pool
	access   *tenantperms.Fake
	identity *staffcheck.Fake
	recv     *receiver
	clk      *testkit.Clock
}

func newHarness(t *testing.T) harness {
	t.Helper()
	ctx := context.Background()
	pg := testkit.StartPostgres(t, testkit.WithMigrations("../../../migrations/webhooks"))
	if _, err := pg.Pool.Exec(ctx, `CREATE ROLE webhooks_app LOGIN PASSWORD 'webhooks_app'; GRANT webhooks_service TO webhooks_app;`); err != nil {
		t.Fatal(err)
	}
	super, err := vasdb.NewPool(ctx, pg.DSN()+"&search_path=webhooks,public")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(super.Close)
	pool, err := vasdb.NewPool(ctx, pg.RoleDSN("webhooks_app", "webhooks_app")+"&search_path=webhooks,public")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	kms, err := vascrypto.NewLocalKMS(map[string][]byte{app.SecretsKMSKeyID: bytes.Repeat([]byte{9}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	recv := newReceiver(t)
	roots := x509.NewCertPool()
	roots.AddCert(recv.srv.Certificate())
	clk := testkit.NewClock(time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC))
	access, identity := tenantperms.New(), staffcheck.New()
	ch, err := adapters.OpenClickHouse(testkit.StartClickHouse(t, testkit.WithClickHouseMigrations("../../../migrations/clickhouse")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ch.Close() })
	ke, err := packs.Load("../../../packs/schema.json", "../../../packs/ke/pack.yaml")
	if err != nil {
		t.Fatal(err)
	}
	svc := app.New(app.Deps{
		Pool: pool, Clock: clk, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Accounts: access, Identity: identity,
		KMS: kms, FingerprintKey: bytes.Repeat([]byte{7}, 32), AllowInsecureURLs: true,
		Sender: adapters.NewSender(adapters.SenderConfig{AllowInsecure: true, RootCAs: roots, Timeout: time.Second}),
		Events: ch, Packs: map[string]*packs.Pack{"KE": ke},
	})
	srv := httptest.NewServer(api.Routes(api.NewHandlers(svc), slog.New(slog.NewTextHandler(io.Discard, nil)), nil, idempotency.NewPostgresStore(pool)))
	t.Cleanup(srv.Close)
	return harness{srv: srv, svc: svc, super: super, access: access, identity: identity, recv: recv, clk: clk}
}

// caller is a dashboard user and session the gateway would forward.
type caller struct{ user, session uuid.UUID }

func (h harness) member(tenant uuid.UUID, perms ...string) caller {
	c := caller{uuid.New(), uuid.New()}
	h.access.Grant(c.user, tenant, perms...)
	h.identity.StepUp(c.user, c.session)
	return c
}

// do sends method path as c (a zero caller sends no user) with body
// (JSON); a write gets key as its Idempotency-Key, or a fresh one.
func (h harness) do(t *testing.T, method, path string, c caller, body any, key string) (int, map[string]any) {
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
		if key == "" {
			key = uuid.NewString()
		}
		req.Header.Set("Idempotency-Key", key)
	}
	if c.user != uuid.Nil {
		req.Header.Set(api.HeaderUserID, c.user.String())
		req.Header.Set(api.HeaderSessionID, c.session.String())
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

func wantStatus(t *testing.T, status int, body map[string]any, want int) {
	t.Helper()
	if status != want {
		t.Fatalf("got %d %v, want %d", status, body, want)
	}
}

// seed makes an active endpoint and one delivered event in tenant, and
// returns their IDs.
func (h harness) seed(t *testing.T, tenant uuid.UUID) (endpoint, delivery string) {
	t.Helper()
	ctx := context.Background()
	admin := h.member(tenant, app.PermCredentialsRead, app.PermCredentialsManage)
	ep, err := h.svc.CreateEndpoint(ctx, admin.user, admin.session, tenant, app.NewEndpoint{URL: h.recv.srv.URL + "/hooks", Events: []string{"*"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.VerifyEndpoint(ctx, admin.user, tenant, ep.ID); err != nil {
		t.Fatal(err)
	}
	env, _ := events.New(uuid.New(), "wallet.credited", 1, tenant, h.clk.Now(), "", map[string]any{"tenant_id": tenant})
	raw, _ := json.Marshal(env)
	if err := h.svc.HandleEvent(ctx, raw); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.DeliverDue(ctx); err != nil {
		t.Fatal(err)
	}
	ds, _, err := h.svc.ListDeliveries(ctx, admin.user, tenant, app.DeliveryFilter{}, "", 1)
	if err != nil || len(ds) != 1 {
		t.Fatalf("seeded deliveries %v, %v", ds, err)
	}
	return ep.ID.String(), ds[0].ID.String()
}

// TestTenantOperations_NeedCredentialPermissions: every
// /v1/webhooks/tenants/ operation is 401 without a user, VAL-4003 for
// someone outside the account and AUTH-4024 for a member without the
// credentials permissions; a reader may read but not write. Reports are
// billing:read instead (task 5.6): a credentials reader is refused them
// and a billing reader isn't. It fails if an operation has no case.
func TestTenantOperations_NeedCredentialPermissions(t *testing.T) {
	h := newHarness(t)
	spec, err := openapi3.NewLoader().LoadFromFile("../../../docs/api/webhooks.yaml")
	if err != nil {
		t.Fatal(err)
	}
	tenant := uuid.New()
	endpoint, delivery := h.seed(t, tenant)
	member := h.member(tenant)
	reader := h.member(tenant, app.PermCredentialsRead)
	billing := h.member(tenant, app.PermBillingRead)
	reports := map[string]bool{"getDailyActivity": true}
	queries := map[string]string{"getDailyActivity": "?from=2026-11-01&to=2026-11-30"}

	bodies := map[string]any{
		"createWebhookEndpoint": map[string]any{"url": h.recv.srv.URL + "/new", "events": []string{"*"}},
		"updateWebhookEndpoint": map[string]any{"description": "renamed"},
	}
	var ops []string
	for path, item := range spec.Paths.Map() {
		if !strings.HasPrefix(path, "/v1/webhooks/tenants/") {
			continue
		}
		for method, op := range item.Operations() {
			ops = append(ops, op.OperationID)
			concrete := strings.NewReplacer("{tenantId}", tenant.String(), "{endpointId}", endpoint, "{deliveryId}", delivery).Replace(path) +
				queries[op.OperationID]
			body := bodies[op.OperationID]
			t.Run(op.OperationID, func(t *testing.T) {
				status, got := h.do(t, method, concrete, caller{}, body, "")
				wantProblem(t, status, got, http.StatusUnauthorized, "AUTH-4010")
				status, got = h.do(t, method, concrete, caller{uuid.New(), uuid.New()}, body, "")
				wantProblem(t, status, got, http.StatusNotFound, "VAL-4003")
				status, got = h.do(t, method, concrete, member, body, "")
				wantProblem(t, status, got, http.StatusForbidden, "AUTH-4024")
				status, got = h.do(t, method, concrete, reader, body, "")
				switch {
				case reports[op.OperationID]:
					wantProblem(t, status, got, http.StatusForbidden, "AUTH-4024")
					status, got = h.do(t, method, concrete, billing, body, "")
					wantStatus(t, status, got, http.StatusOK)
				case method == http.MethodGet:
					wantStatus(t, status, got, http.StatusOK)
				default:
					wantProblem(t, status, got, http.StatusForbidden, "AUTH-4024")
				}
			})
		}
	}
	sort.Strings(ops)
	want := "createWebhookEndpoint,deleteWebhookEndpoint,disableWebhookEndpoint,getDailyActivity,getWebhookDelivery,getWebhookEndpoint," +
		"listWebhookDeliveries,listWebhookEndpoints,replayWebhookDelivery,rotateWebhookEndpointSecret,sendWebhookTestEvent," +
		"updateWebhookEndpoint,verifyWebhookEndpoint"
	if strings.Join(ops, ",") != want {
		t.Fatalf("tenant operations in the spec %v, cases written for %s: add the new one's case", ops, want)
	}
}

func TestStepUpOperations(t *testing.T) {
	h := newHarness(t)
	tenant := uuid.New()
	endpoint, _ := h.seed(t, tenant)
	admin := h.member(tenant, app.PermCredentialsRead, app.PermCredentialsManage)
	h.identity.Expire(admin.user, admin.session)
	base := "/v1/webhooks/tenants/" + tenant.String() + "/endpoints"

	for name, c := range map[string]struct {
		method, path string
		body         any
	}{
		"create":         {http.MethodPost, base, map[string]any{"url": h.recv.srv.URL + "/x", "events": []string{"*"}}},
		"change the URL": {http.MethodPatch, base + "/" + endpoint, map[string]any{"url": h.recv.srv.URL + "/y"}},
		"rotate":         {http.MethodPost, base + "/" + endpoint + "/secret-rotations", nil},
	} {
		t.Run(name, func(t *testing.T) {
			status, got := h.do(t, c.method, c.path, admin, c.body, "")
			wantProblem(t, status, got, http.StatusUnauthorized, "AUTH-4012")
		})
	}
	status, got := h.do(t, http.MethodPatch, base+"/"+endpoint, admin, map[string]any{"description": "no step-up needed"}, "")
	wantStatus(t, status, got, http.StatusOK)
}

func TestWebhooksAPI_EndpointToDeliveryLog(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tenant := uuid.New()
	admin := h.member(tenant, app.PermCredentialsRead, app.PermCredentialsManage)
	base := "/v1/webhooks/tenants/" + tenant.String()

	status, created := h.do(t, http.MethodPost, base+"/endpoints", admin, map[string]any{
		"url": h.recv.srv.URL + "/hooks", "description": "orders", "events": []string{"wallet.credited"},
	}, "")
	wantStatus(t, status, created, http.StatusCreated)
	id, _ := created["id"].(string)
	if created["status"] != "pending_verification" || !strings.HasPrefix(created["secret"].(string), "whsec_") {
		t.Fatalf("created %v", created)
	}

	status, got := h.do(t, http.MethodPost, base+"/endpoints/"+id+"/verification", admin, nil, "")
	if status != http.StatusOK || got["status"] != "active" {
		t.Fatalf("verify: %d %v", status, got)
	}
	status, got = h.do(t, http.MethodPost, base+"/endpoints/"+id+"/test-events", admin, nil, "")
	if status != http.StatusAccepted || got["event_type"] != "webhook.test" || got["status"] != "pending" {
		t.Fatalf("test event: %d %v", status, got)
	}
	testDelivery := got["id"].(string)
	if _, err := h.svc.DeliverDue(ctx); err != nil {
		t.Fatal(err)
	}

	status, got = h.do(t, http.MethodGet, base+"/deliveries/"+testDelivery, admin, nil, "")
	wantStatus(t, status, got, http.StatusOK)
	attempts, _ := got["attempt_log"].([]any)
	if got["status"] != "succeeded" || len(attempts) != 1 || !strings.Contains(got["body"].(string), `"webhook.test"`) {
		t.Fatalf("delivery detail %v", got)
	}
	first := attempts[0].(map[string]any)
	headers := first["request_headers"].(map[string]any)
	if first["outcome"] != "succeeded" || first["response_status"] != float64(200) || first["response_body_encoding"] != "utf8" ||
		headers["Vaspro-Webhook-Signature"] == nil || first["response_headers"] == nil {
		t.Fatalf("attempt %v", first)
	}

	status, got = h.do(t, http.MethodPost, base+"/deliveries/"+testDelivery+"/replays", admin, nil, "")
	if status != http.StatusAccepted || got["replay_of"] != testDelivery || got["event_id"] == nil {
		t.Fatalf("replay: %d %v", status, got)
	}
	status, got = h.do(t, http.MethodGet, base+"/deliveries?event_type=webhook.test&limit=1", admin, nil, "")
	list, _ := got["deliveries"].([]any)
	if status != http.StatusOK || len(list) != 1 || got["next_cursor"] == nil {
		t.Fatalf("page one: %d %v", status, got)
	}
	status, got = h.do(t, http.MethodGet, base+"/deliveries?event_type=webhook.test&limit=1&cursor="+got["next_cursor"].(string), admin, nil, "")
	list, _ = got["deliveries"].([]any)
	if status != http.StatusOK || len(list) != 1 || got["next_cursor"] != nil {
		t.Fatalf("page two: %d %v", status, got)
	}

	status, got = h.do(t, http.MethodPost, base+"/endpoints/"+id+"/secret-rotations", admin, nil, "")
	if status != http.StatusCreated || !strings.HasPrefix(got["secret"].(string), "whsec_") || got["previous_valid_until"] == nil {
		t.Fatalf("rotate: %d %v", status, got)
	}
	status, got = h.do(t, http.MethodPost, base+"/endpoints/"+id+"/disable", admin, nil, "")
	if status != http.StatusOK || got["status"] != "disabled" || got["disabled_reason"] != "client" {
		t.Fatalf("disable: %d %v", status, got)
	}
	status, got = h.do(t, http.MethodDelete, base+"/endpoints/"+id, admin, nil, "")
	wantStatus(t, status, got, http.StatusNoContent)
	status, got = h.do(t, http.MethodGet, base+"/endpoints/"+id, admin, nil, "")
	wantProblem(t, status, got, http.StatusNotFound, "VAL-4003")
	status, got = h.do(t, http.MethodGet, base+"/deliveries/"+testDelivery, admin, nil, "")
	wantStatus(t, status, got, http.StatusOK) // the log outlives the endpoint
}

func TestWebhooksAPI_Codes(t *testing.T) {
	h := newHarness(t)
	tenant := uuid.New()
	admin := h.member(tenant, app.PermCredentialsRead, app.PermCredentialsManage)
	base := "/v1/webhooks/tenants/" + tenant.String()

	status, got := h.do(t, http.MethodPost, base+"/endpoints", admin, map[string]any{"url": "https://user:pass@hooks.example.com/x", "events": []string{"*"}}, "")
	wantProblem(t, status, got, http.StatusUnprocessableEntity, "VAL-4019")
	status, got = h.do(t, http.MethodPost, base+"/endpoints", admin, map[string]any{"url": h.recv.srv.URL, "events": []string{"ledger.rebuild_failed"}}, "")
	wantProblem(t, status, got, http.StatusBadRequest, "VAL-4000")

	status, created := h.do(t, http.MethodPost, base+"/endpoints", admin, map[string]any{"url": h.recv.srv.URL + "/hooks", "events": []string{"*"}}, "")
	wantStatus(t, status, created, http.StatusCreated)
	h.recv.mu.Lock()
	h.recv.wrong = true
	h.recv.mu.Unlock()
	status, got = h.do(t, http.MethodPost, base+"/endpoints/"+created["id"].(string)+"/verification", admin, nil, "")
	wantProblem(t, status, got, http.StatusUnprocessableEntity, "VAL-4018")
	if strings.Contains(got["detail"].(string), h.recv.srv.URL) {
		t.Fatalf("the problem names the URL: %v", got["detail"])
	}
	status, got = h.do(t, http.MethodPost, base+"/endpoints/"+created["id"].(string)+"/test-events", admin, nil, "")
	wantProblem(t, status, got, http.StatusBadRequest, "VAL-4000")

	for _, q := range []string{"cursor=garbage", "limit=101", "status=lost", "endpoint_id=nope", "event_id=nope"} {
		status, got = h.do(t, http.MethodGet, base+"/deliveries?"+q, admin, nil, "")
		wantProblem(t, status, got, http.StatusBadRequest, "VAL-4000")
	}
}

// TestWebhooksAPI_SecretsAreNotKeptForReplays: a retried create or
// rotation with the same Idempotency-Key does nothing twice and shows
// no secret, and the stored idempotency record never held one.
func TestWebhooksAPI_SecretsAreNotKeptForReplays(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tenant := uuid.New()
	admin := h.member(tenant, app.PermCredentialsRead, app.PermCredentialsManage)
	base := "/v1/webhooks/tenants/" + tenant.String() + "/endpoints"
	body := map[string]any{"url": h.recv.srv.URL + "/hooks", "events": []string{"*"}}

	status, first := h.do(t, http.MethodPost, base, admin, body, "create-1")
	wantStatus(t, status, first, http.StatusCreated)
	status, again := h.do(t, http.MethodPost, base, admin, body, "create-1")
	wantStatus(t, status, again, http.StatusCreated)
	if again["id"] != first["id"] || again["secret"] != nil {
		t.Fatalf("the retry: %v, want the same endpoint with secret null", again)
	}
	id := first["id"].(string)

	status, rot := h.do(t, http.MethodPost, base+"/"+id+"/secret-rotations", admin, nil, "rotate-1")
	wantStatus(t, status, rot, http.StatusCreated)
	status, rotAgain := h.do(t, http.MethodPost, base+"/"+id+"/secret-rotations", admin, nil, "rotate-1")
	if status != http.StatusCreated || rotAgain["secret"] != nil || rotAgain["previous_valid_until"] != rot["previous_valid_until"] {
		t.Fatalf("the retried rotation: %d %v", status, rotAgain)
	}

	var endpoints, secrets int
	if err := h.super.QueryRow(ctx, `SELECT (SELECT count(*) FROM endpoints), (SELECT count(*) FROM endpoint_secrets)`).Scan(&endpoints, &secrets); err != nil {
		t.Fatal(err)
	}
	if endpoints != 1 || secrets != 2 {
		t.Fatalf("%d endpoints and %d secrets, want 1 and 2 (one create, one rotation)", endpoints, secrets)
	}
	var stored int
	if err := h.super.QueryRow(ctx, `SELECT count(*) FROM idempotency_keys WHERE response::text LIKE '%whsec_%'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != 0 {
		t.Fatalf("%d idempotency records hold a secret", stored)
	}
}

func TestGetDailyActivity(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tenant := uuid.New()
	billing := h.member(tenant, app.PermBillingRead)
	at := time.Date(2026, 11, 10, 22, 30, 0, 0, time.UTC) // 01:30 on the 11th in Nairobi
	env, _ := events.New(uuid.New(), "wallet.credited", 1, tenant, at, "", map[string]any{
		"tenant_id": tenant, "transaction_type": "topup", "currency": "KES", "amount_minor": 150000, "rail": "mpesa_ke",
	})
	raw, _ := json.Marshal(env)
	if err := h.svc.SinkEvents(ctx, []bus.Message{{Subject: "ledger.wallet.credited", Data: raw}}); err != nil {
		t.Fatal(err)
	}
	base := "/v1/webhooks/tenants/" + tenant.String() + "/reports/daily-activity"

	status, got := h.do(t, http.MethodGet, base+"?from=2026-11-01&to=2026-11-30", billing, nil, "")
	wantStatus(t, status, got, http.StatusOK)
	days, _ := got["days"].([]any)
	if got["time_zone"] != "Africa/Nairobi" || len(days) != 1 {
		t.Fatalf("report %v", got)
	}
	day := days[0].(map[string]any)
	if day["date"] != "2026-11-11" || day["currency"] != "KES" || day["credited_minor"] != float64(150000) ||
		day["topups"] != float64(1) || day["usage_minor"] != float64(0) {
		t.Fatalf("day %v", day)
	}
	for _, q := range []string{"?from=2026-11-30&to=2026-11-01", "?from=2026-01-01&to=2026-12-31", "?from=yesterday&to=2026-11-01", "?from=2026-11-01"} {
		status, got := h.do(t, http.MethodGet, base+q, billing, nil, "")
		wantProblem(t, status, got, http.StatusBadRequest, "VAL-4000")
	}
}
