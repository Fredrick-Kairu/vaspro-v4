//go:build integration

package payments_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments/api"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments/app"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/bus"
	vascrypto "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/crypto"
	vasdb "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"
	platformevents "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/events"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/testkit"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/daraja"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/ledgerposter"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/staffdirectory"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/tenantinfo"
)

// Test-only Daraja app for the fake; nothing real.
var darajaApp = daraja.App{ConsumerKey: "fake-service-key", ConsumerSecret: "fake-service-secret", ShortCode: "600100", Passkey: "fake-service-passkey"}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestService_TopsUpByStkEndToEnd runs the service as production wires
// it (as a role granted payments_service, the rails synced from the
// real packs at startup) and tops a wallet up over HTTP: the push goes
// to the Daraja fake, whose callback comes back through a stand-in for
// the gateway, and with the ledger down at first, the service's own
// sweep loop posts the top-up once it's back. payment_intent.succeeded
// reaches NATS through the outbox relay.
func TestService_TopsUpByStkEndToEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pg := testkit.StartPostgres(t, testkit.WithMigrations("../../migrations/payments"))
	if _, err := pg.Pool.Exec(ctx, `CREATE ROLE payments_app LOGIN PASSWORD 'payments_app'; GRANT payments_service TO payments_app;`); err != nil {
		t.Fatal(err)
	}
	pool, err := vasdb.NewPool(ctx, pg.RoleDSN("payments_app", "payments_app")+"&search_path=payments,public")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	natsURL := testkit.StartNATSURL(t)
	b, err := bus.Connect(ctx, natsURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	loaded, err := payments.LoadPacks("../../packs")
	if err != nil {
		t.Fatal(err)
	}
	fake := daraja.New(darajaApp)
	t.Cleanup(fake.Close)

	kmsKey := bytes.Repeat([]byte{9}, 32)
	httpAddr := freeAddr(t)
	// gateway stands in for the gateway: it forwards to payments and
	// sets the client address it saw.
	target, _ := url.Parse("http://" + httpAddr)
	proxy := httputil.NewSingleHostReverseProxy(target)
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set(api.HeaderClientIP, "127.0.0.1")
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(gateway.Close)

	cfg := payments.Config{
		HTTPAddr: httpAddr, DarajaBaseURL: fake.URL(),
		CredentialsKMSKey: base64.StdEncoding.EncodeToString(kmsKey),
		CallbackBaseURL:   gateway.URL, CallbackSecret: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{5}, 32)),
		DarajaCallbackCIDRs: "127.0.0.1/32", SweepInterval: 200 * time.Millisecond,
		CheckoutSecret: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{6}, 32)),
	}
	accounts, ledger := tenantinfo.New(), ledgerposter.New()
	svc, err := payments.New(cfg, pool, b, payments.Clients{Accounts: accounts, Identity: staffdirectory.New(), Ledger: ledger, Cache: testkit.StartRedis(t)}, loaded)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- svc.Run(ctx) }()
	waitFor(t, "/readyz", func() bool {
		resp, err := http.Get("http://" + httpAddr + "/readyz") //nolint:gosec,noctx // the test's own server
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})

	// VASPRO's paybill, set up as the ops CLI would and marked verified.
	kms, err := vascrypto.NewLocalKMS(map[string][]byte{app.CredentialsKMSKeyID: kmsKey})
	if err != nil {
		t.Fatal(err)
	}
	staff := staffdirectory.New()
	staff.Add("ops@example.com")
	ops := app.New(app.Deps{Pool: pool, Identity: staff, Packs: loaded, KMS: kms, FingerprintKey: bytes.Repeat([]byte{7}, 32)})
	paybill, err := ops.AddRailAccount(ctx, app.AddRailAccountRequest{StaffEmail: "ops@example.com", RailCode: "mpesa_ke", Number: darajaApp.ShortCode, DisplayName: "VASPRO"})
	if err != nil {
		t.Fatal(err)
	}
	for kind, secret := range map[string]string{"app_key": darajaApp.ConsumerKey, "app_secret": darajaApp.ConsumerSecret, "passkey": darajaApp.Passkey} {
		if err := ops.SetRailCredential(ctx, app.SetRailCredentialRequest{StaffEmail: "ops@example.com", RailAccountID: paybill.ID, Kind: kind, Secret: []byte(secret)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pg.Pool.Exec(ctx, `UPDATE payments.rail_accounts SET status = 'active', verified_at = now() WHERE id = $1`, paybill.ID); err != nil {
		t.Fatal(err)
	}

	nc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	sub, err := nc.SubscribeSync("payments.payment_intent.succeeded")
	if err != nil {
		t.Fatal(err)
	}

	user, tenant := uuid.New(), uuid.New()
	accounts.Grant(user, tenant, "KE", "KES", app.PermBillingRead, app.PermBillingManage)
	ledger.Fail()
	body, _ := json.Marshal(map[string]any{"amount_minor": 150000, "payer_phone": "+" + daraja.SandboxMSISDN})
	req, _ := http.NewRequest(http.MethodPost, gateway.URL+"/v1/payments/tenants/"+tenant.String()+"/topups", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", uuid.NewString())
	req.Header.Set(api.HeaderUserID, user.String())
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // the test's own server
	if err != nil {
		t.Fatal(err)
	}
	var created map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&created)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d %v", resp.StatusCode, created)
	}
	fake.Wait()

	getTopup := func() map[string]any {
		req, _ := http.NewRequest(http.MethodGet, gateway.URL+"/v1/payments/tenants/"+tenant.String()+"/topups/"+created["id"].(string), nil)
		req.Header.Set(api.HeaderUserID, user.String())
		resp, err := http.DefaultClient.Do(req) //nolint:gosec // the test's own server
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var got map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&got)
		return got
	}
	if got := getTopup(); got["status"] != "succeeded" || got["receipt"] == nil {
		t.Fatalf("after the callback: %v (callbacks: %+v)", got, fake.Callbacks())
	}
	if ledger.Credited(tenant) != 0 {
		t.Fatal("credited while the ledger was down")
	}
	ledger.Recover()
	waitFor(t, "the sweep to post the top-up", func() bool { return ledger.Credited(tenant) == 150000 })

	msg, err := sub.NextMsg(15 * time.Second)
	if err != nil {
		t.Fatalf("payment_intent.succeeded never reached NATS: %v", err)
	}
	var env platformevents.Envelope
	if err := json.Unmarshal(msg.Data, &env); err != nil || env.TenantID != tenant {
		t.Fatalf("event %s %v", msg.Data, err)
	}

	// A checkout link (task 4.8): created by the client, opened and paid
	// with no session, credited once, then shown paid.
	linkBody, _ := json.Marshal(map[string]any{"amount_minor": 200000})
	req, _ = http.NewRequest(http.MethodPost, gateway.URL+"/v1/payments/tenants/"+tenant.String()+"/checkout-links", bytes.NewReader(linkBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", uuid.NewString())
	req.Header.Set(api.HeaderUserID, user.String())
	resp, err = http.DefaultClient.Do(req) //nolint:gosec // the test's own server
	if err != nil {
		t.Fatal(err)
	}
	var link map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&link)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("checkout link: %d %v", resp.StatusCode, link)
	}
	token := link["token"].(string)
	page, err := http.Get(gateway.URL + "/pay/" + token) //nolint:gosec,noctx // the test's own server
	if err != nil {
		t.Fatal(err)
	}
	_ = page.Body.Close()
	if page.StatusCode != http.StatusOK || page.Header.Get("Content-Security-Policy") == "" {
		t.Fatalf("the page: %d", page.StatusCode)
	}
	payBody, _ := json.Marshal(map[string]any{"payer_phone": "+" + daraja.SandboxMSISDN})
	req, _ = http.NewRequest(http.MethodPost, gateway.URL+"/v1/payments/checkout/"+token+"/payments", bytes.NewReader(payBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", uuid.NewString())
	resp, err = http.DefaultClient.Do(req) //nolint:gosec // the test's own server
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("paying the link: %d", resp.StatusCode)
	}
	fake.Wait()
	waitFor(t, "the checkout to be credited", func() bool { return ledger.Credited(tenant) == 350000 })
	var view map[string]any
	viewResp, err := http.Get(gateway.URL + "/v1/payments/checkout/" + token) //nolint:gosec,noctx // the test's own server
	if err != nil {
		t.Fatal(err)
	}
	_ = json.NewDecoder(viewResp.Body).Decode(&view)
	_ = viewResp.Body.Close()
	if view["status"] != "paid" {
		t.Fatalf("the link after paying: %v", view)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run didn't return after cancel")
	}
	var live int
	if err := pg.Pool.QueryRow(context.Background(), `SELECT count(*) FROM payments.rails WHERE code = 'mpesa_ke' AND status = 'live'`).Scan(&live); err != nil || live != 1 {
		t.Fatalf("mpesa_ke synced at startup: %d %v", live, err)
	}
}
