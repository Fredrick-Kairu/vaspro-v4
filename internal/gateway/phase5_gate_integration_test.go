//go:build integration

package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/clock"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/testkit"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/daraja"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/kycvendor"
)

// gateDaraja is the Daraja fake's app: test-only values, nothing real.
var gateDaraja = daraja.App{ConsumerKey: "gate-consumer-key", ConsumerSecret: "gate-consumer-secret", ShortCode: "600100", Passkey: "gate-passkey"}

// startService builds ./cmd/<name>, runs it with env, and waits until
// httpAddr answers /healthz. The subprocess is killed at cleanup.
func startService(t *testing.T, name, httpAddr string, env ...string) {
	t.Helper()
	root := repoRoot(t)
	bin := filepath.Join(t.TempDir(), name)
	build := exec.Command("go", "build", "-o", bin, "./cmd/"+name) //nolint:gosec // fixed "go build" into our own t.TempDir()
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/%s failed: %v\n%s", name, err, out)
	}
	cmd := exec.Command(bin) //nolint:gosec // the binary we just built
	cmd.Dir = root
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting %s: %v", name, err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	waitForHealthy(t, "http://"+httpAddr, 30*time.Second)
}

// gateBackends are the services the gate's gateway routes to.
type gateBackends struct {
	identity, accounts, ledger, payments, webhooks string
}

// newGateGateway is the gateway in front of every real backend.
func newGateGateway(t *testing.T, b gateBackends, logger *slog.Logger) http.Handler {
	t.Helper()
	root := repoRoot(t)
	grpcAddr, ok := identityGRPCAddrs.Load(b.identity)
	if !ok {
		t.Fatalf("no gRPC address recorded for identity at %s", b.identity)
	}
	sessions, err := DialGRPCSessions(grpcAddr.(string))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sessions.Close() })
	svc, err := New(Config{
		IdentityBaseURL: b.identity, IdentityOpenAPIPath: filepath.Join(root, "docs/api/identity.yaml"),
		AccountsBaseURL: b.accounts, AccountsOpenAPIPath: filepath.Join(root, "docs/api/accounts.yaml"),
		LedgerBaseURL: b.ledger, LedgerOpenAPIPath: filepath.Join(root, "docs/api/ledger.yaml"),
		PaymentsBaseURL: b.payments, PaymentsOpenAPIPath: filepath.Join(root, "docs/api/payments.yaml"),
		WebhooksBaseURL: b.webhooks, WebhooksOpenAPIPath: filepath.Join(root, "docs/api/webhooks.yaml"),
		ContactsBaseURL: "http://127.0.0.1:1", ContactsOpenAPIPath: filepath.Join(root, "docs/api/contacts.yaml"),
	}, testkit.StartRedis(t), clock.Real{}, logger, nil, sessions)
	if err != nil {
		t.Fatalf("gateway.New: %v", err)
	}
	return svc.Router()
}

// runPaymentsCLI runs `vaspro payments <args>` as an operator would,
// with stdin, and returns its output.
func runPaymentsCLI(t *testing.T, bin string, env []string, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.Command(bin, append([]string{"payments"}, args...)...) //nolint:gosec // the binary we built; args are the test's own
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("vaspro payments %v failed: %v\n%s", args, err, out)
	}
	return string(out)
}

// gateReceived is what the test endpoint verified.
type gateReceived struct {
	Verified []struct{ ID, Type string } `json:"verified"`
	Rejected int                         `json:"rejected"`
}

func readReceiver(t *testing.T, addr string) gateReceived {
	t.Helper()
	var got gateReceived
	resp, err := http.Get("http://" + addr + "/received") //nolint:gosec,noctx // the test's own receiver
	if err != nil {
		return got
	}
	defer func() { _ = resp.Body.Close() }()
	_ = json.NewDecoder(resp.Body).Decode(&got)
	return got
}

func eventually(t *testing.T, what string, timeout time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestGateway_Phase5ExitGate_EndToEnd is Phase 5's exit gate: with real
// identity, accounts, ledger, payments and webhooks behind the gateway,
// sharing one bus, events from Phases 2–4 reach a client's endpoint
// signed, and that endpoint, built on the Go verification snippet
// (docs/webhooks/verify/go), accepts every one. The delivery log shows
// every attempt with its response, a failed first attempt and its retry
// included, and the daily activity report counts the money once.
func TestGateway_Phase5ExitGate_EndToEnd(t *testing.T) {
	root := repoRoot(t)
	const kycSecret = "gate-kyc-webhook-secret" //nolint:gosec // G101: a fixture secret shared with the fake vendor
	smtp := startFakeSMTPServer(t)
	vendor := kycvendor.New([]byte(kycSecret))
	t.Cleanup(vendor.Close)
	rail := daraja.New(gateDaraja)
	t.Cleanup(rail.Close)
	natsURL := testkit.StartNATSURL(t) // one bus for every service but identity

	identityGRPC, accountsGRPC, ledgerGRPC := freeTCPAddr(t), freeTCPAddr(t), freeTCPAddr(t)
	accountsHTTP, ledgerHTTP, paymentsHTTP, webhooksHTTP := freeTCPAddr(t), freeTCPAddr(t), freeTCPAddr(t), freeTCPAddr(t)
	packsDir := filepath.Join(root, "packs")
	dsn := func(schema string) string {
		pg := testkit.StartPostgres(t, testkit.WithMigrations(filepath.Join(root, "migrations", schema)))
		return pg.DSN() + "&search_path=" + schema + ",public"
	}

	startService(t, "accounts", accountsHTTP,
		"ACCOUNTS_DATABASE_URL="+dsn("accounts"), "ACCOUNTS_NATS_URL="+natsURL, "ACCOUNTS_HTTP_ADDR="+accountsHTTP,
		"ACCOUNTS_GRPC_ADDR="+accountsGRPC, "ACCOUNTS_PACKS_DIR="+packsDir, "ACCOUNTS_IDENTITY_GRPC_ADDR="+identityGRPC,
		"ACCOUNTS_KYC_PROVIDER=fake", "ACCOUNTS_KYC_FAKE_URL="+vendor.URL(), "ACCOUNTS_KYC_WEBHOOK_SECRET="+kycSecret)
	identityURL := startRealIdentityWithLogs(t, smtp.Addr(), nil,
		"IDENTITY_ACCOUNTS_GRPC_ADDR="+accountsGRPC, "IDENTITY_GRPC_ADDR="+identityGRPC)
	startService(t, "ledger", ledgerHTTP,
		"LEDGER_DATABASE_URL="+dsn("ledger"), "LEDGER_NATS_URL="+natsURL, "LEDGER_HTTP_ADDR="+ledgerHTTP,
		"LEDGER_GRPC_ADDR="+ledgerGRPC, "LEDGER_IDENTITY_GRPC_ADDR="+identityGRPC, "LEDGER_ACCOUNTS_GRPC_ADDR="+accountsGRPC,
		"LEDGER_PACKS_DIR="+packsDir, "LEDGER_DEV_TOPUPS=true")

	gwLogs := &syncBuffer{}
	r := newGateGateway(t, gateBackends{
		identity: identityURL, accounts: "http://" + accountsHTTP, ledger: "http://" + ledgerHTTP,
		payments: "http://" + paymentsHTTP, webhooks: "http://" + webhooksHTTP,
	}, slog.New(slog.NewJSONHandler(gwLogs, nil)))
	gw := httptest.NewServer(r)
	t.Cleanup(gw.Close)
	vendor.SetWebhookURL(gw.URL + "/v1/accounts/kyc/webhooks/fake")

	paymentsDSN, paymentsKMS := dsn("payments"), randomBase64Key(t)
	startService(t, "payments", paymentsHTTP,
		"PAYMENTS_DATABASE_URL="+paymentsDSN, "PAYMENTS_NATS_URL="+natsURL, "PAYMENTS_HTTP_ADDR="+paymentsHTTP,
		"PAYMENTS_ACCOUNTS_GRPC_ADDR="+accountsGRPC, "PAYMENTS_IDENTITY_GRPC_ADDR="+identityGRPC,
		"PAYMENTS_LEDGER_GRPC_ADDR="+ledgerGRPC, "PAYMENTS_PACKS_DIR="+packsDir, "PAYMENTS_REDIS_URL="+testkit.StartRedisURL(t),
		"DARAJA_BASE_URL="+rail.URL(), "PAYMENTS_CREDENTIALS_KMS_KEY="+paymentsKMS,
		"PAYMENTS_CALLBACK_BASE_URL="+gw.URL, "PAYMENTS_CALLBACK_SECRET="+randomBase64Key(t),
		"PAYMENTS_DARAJA_CALLBACK_CIDRS=127.0.0.1/32", "PAYMENTS_CHECKOUT_SECRET="+randomBase64Key(t),
		"PAYMENTS_SWEEP_INTERVAL=500ms")
	startService(t, "webhooks", webhooksHTTP,
		"WEBHOOKS_DATABASE_URL="+dsn("webhooks"), "WEBHOOKS_NATS_URL="+natsURL, "WEBHOOKS_HTTP_ADDR="+webhooksHTTP,
		"WEBHOOKS_ACCOUNTS_GRPC_ADDR="+accountsGRPC, "WEBHOOKS_IDENTITY_GRPC_ADDR="+identityGRPC,
		"WEBHOOKS_SECRETS_KMS_KEY="+randomBase64Key(t), "WEBHOOKS_SECRET_FINGERPRINT_KEY="+randomBase64Key(t),
		"WEBHOOKS_ALLOW_INSECURE_URLS=true", "WEBHOOKS_PACKS_DIR="+packsDir,
		"WEBHOOKS_CLICKHOUSE_URL="+testkit.StartClickHouse(t, testkit.WithClickHouseMigrations(filepath.Join(root, "migrations/clickhouse"))),
		"WEBHOOKS_RETRY_SCHEDULE=1s,1s", "WEBHOOKS_POLL_INTERVAL=200ms", "WEBHOOKS_STREAM_RETRY=200ms", "WEBHOOKS_SINK_WAIT=200ms")

	// --- the client signs up and creates a Kenyan account ---
	_, ada := signUpVerifySetPasswordAndSignIn(t, r, smtp, "ada@example.com")
	status, account := gwDoJSON(t, r, http.MethodPost, "/v1/accounts/tenants", ada,
		map[string]string{"display_name": "Acme Deliveries", "country_code": "KE"}, "create-account")
	if status != http.StatusCreated {
		t.Fatalf("create account: got %d %v", status, account)
	}
	tenant := account["id"].(string)

	// --- two staff members (maker and checker), granted with the ops CLI ---
	_, staff1 := signUpVerifySetPasswordAndSignIn(t, r, smtp, "ops1@example.com")
	_, staff2 := signUpVerifySetPasswordAndSignIn(t, r, smtp, "ops2@example.com")
	runVasproCLI(t, identityURL, "identity", "grant-staff", "-by", "gate-operator", "ops1@example.com")
	runVasproCLI(t, identityURL, "identity", "grant-staff", "-by", "gate-operator", "ops2@example.com")

	// --- VASPRO's paybill, set up with the real ops CLI (task 4.3) ---
	vaspro := filepath.Join(t.TempDir(), "vaspro")
	build := exec.Command("go", "build", "-o", vaspro, "./tools/vaspro") //nolint:gosec // fixed "go build" into our own t.TempDir()
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build ./tools/vaspro: %v\n%s", err, out)
	}
	cliEnv := []string{
		"PAYMENTS_DATABASE_URL=" + paymentsDSN, "PAYMENTS_IDENTITY_GRPC_ADDR=" + identityGRPC,
		"PAYMENTS_CREDENTIALS_KMS_KEY=" + paymentsKMS, "PAYMENTS_CREDENTIAL_FINGERPRINT_KEY=" + randomBase64Key(t),
		"DARAJA_BASE_URL=" + rail.URL(),
	}
	out := runPaymentsCLI(t, vaspro, cliEnv, "", "add-rail-account", "-by", "ops1@example.com",
		"-rail", "mpesa_ke", "-number", gateDaraja.ShortCode, "-name", "VASPRO")
	m := regexp.MustCompile(`id ([0-9a-f-]{36})`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no paybill ID in %q", out)
	}
	for kind, secret := range map[string]string{"app_key": gateDaraja.ConsumerKey, "app_secret": gateDaraja.ConsumerSecret, "passkey": gateDaraja.Passkey} {
		runPaymentsCLI(t, vaspro, cliEnv, secret, "set-rail-credential", "-by", "ops1@example.com", "-account", m[1], "-kind", kind)
	}
	runPaymentsCLI(t, vaspro, cliEnv, "", "verify-rail-account", "-by", "ops1@example.com", "-account", m[1])

	// --- the client registers its endpoint (secret shown once), starts
	// it with that secret, and passes the challenge ---
	stepUp(t, r, smtp, ada)
	receiverAddr := freeTCPAddr(t)
	status, endpoint := gwDoJSON(t, r, http.MethodPost, "/v1/webhooks/tenants/"+tenant+"/endpoints", ada,
		map[string]any{"url": "http://" + receiverAddr + "/hook", "events": []string{"*"}}, "create-endpoint")
	if status != http.StatusCreated {
		t.Fatalf("create endpoint: got %d %v", status, endpoint)
	}
	endpointSecret := endpoint["secret"].(string)
	receiver := filepath.Join(t.TempDir(), "testreceiver")
	build = exec.Command("go", "build", "-o", receiver, "./testreceiver") //nolint:gosec // fixed "go build" into our own t.TempDir()
	build.Dir = filepath.Join(root, "docs/webhooks/verify/go")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build the snippet's testreceiver: %v\n%s", err, out)
	}
	rcv := exec.Command(receiver, "-addr", receiverAddr, "-secret", endpointSecret, "-fail-first", "wallet.credited") //nolint:gosec // our own binary
	rcv.Stdout, rcv.Stderr = os.Stdout, os.Stderr
	if err := rcv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = rcv.Process.Kill()
		_ = rcv.Wait()
	})
	eventually(t, "the receiver to listen", 10*time.Second, func() bool {
		resp, err := http.Get("http://" + receiverAddr + "/received") //nolint:gosec,noctx // the test's own receiver
		if err == nil {
			_ = resp.Body.Close()
		}
		return err == nil
	})
	endpointID := endpoint["id"].(string)
	if status, got := gwDoJSON(t, r, http.MethodPost, "/v1/webhooks/tenants/"+tenant+"/endpoints/"+endpointID+"/verification", ada, nil, "verify-endpoint"); status != http.StatusOK || got["status"] != "active" {
		t.Fatalf("verify endpoint: got %d %v", status, got)
	}

	// --- Phase 2: individual KYC passes through the fake vendor ---
	status, check := gwDoJSON(t, r, http.MethodPost, "/v1/accounts/tenants/"+tenant+"/kyc-checks", ada,
		map[string]string{"kind": "individual"}, "start-kyc")
	if status != http.StatusCreated {
		t.Fatalf("starting KYC: got %d %v", status, check)
	}
	flow := check["url"].(string)
	if code, err := vendor.Decide(flow[strings.LastIndex(flow, "/")+1:], "passed", kycvendor.NoTamper); err != nil || code != http.StatusOK {
		t.Fatalf("the vendor's webhook: %d, %v", code, err)
	}

	// --- Phase 3: a dev top-up by staff, a list price by two staff, and a
	// token purchase by the client ---
	if status, got := gwDoJSON(t, r, http.MethodPost, "/v1/ledger/staff/dev-top-ups", staff1,
		map[string]any{"tenant_id": tenant, "currency": "KES", "amount_minor": 1300000}, "dev-top-up"); status != http.StatusCreated {
		t.Fatalf("dev top-up: got %d %v", status, got)
	}
	status, price := gwDoJSON(t, r, http.MethodPost, "/v1/ledger/staff/token-prices", staff1,
		map[string]any{"service_code": "sms", "currency": "KES", "minor_per_token": "80", "effective_from": "2026-01-01T00:00:00Z"}, "list-price")
	if status != http.StatusCreated {
		t.Fatalf("list price: got %d %v", status, price)
	}
	stepUp(t, r, smtp, staff2)
	if status, got := gwDoJSON(t, r, http.MethodPost, "/v1/ledger/staff/token-prices/"+price["id"].(string)+"/approval", staff2, nil, "approve-price"); status != http.StatusOK {
		t.Fatalf("approving the price: got %d %v", status, got)
	}
	if status, got := gwDoJSON(t, r, http.MethodPost, "/v1/ledger/tenants/"+tenant+"/token-purchases", ada,
		map[string]any{"service_code": "sms", "currency": "KES", "tokens": "10000"}, "buy-tokens"); status != http.StatusCreated || got["cost_minor"] != float64(800000) {
		t.Fatalf("token purchase: got %d %v", status, got)
	}

	// --- Phase 4: an STK top-up, called back through the gateway ---
	status, topup := gwDoJSON(t, r, http.MethodPost, "/v1/payments/tenants/"+tenant+"/topups", ada,
		map[string]any{"amount_minor": 150000, "payer_phone": "+" + daraja.SandboxMSISDN}, "stk-top-up")
	if status != http.StatusCreated {
		t.Fatalf("STK top-up: got %d %v", status, topup)
	}
	rail.Wait()
	eventually(t, "the STK top-up to succeed", 20*time.Second, func() bool {
		_, got := gwDoJSON(t, r, http.MethodGet, "/v1/payments/tenants/"+tenant+"/topups/"+topup["id"].(string), ada, nil, "")
		return got["status"] == "succeeded"
	})

	// --- the endpoint, verifying with the Go snippet, got every event ---
	want := map[string]int{
		"kyc.check_decided": 1, "tenant.tier_changed": 1, "wallet.credited": 2, "tokens.purchased": 1, "payment_intent.succeeded": 1,
	}
	var got gateReceived
	eventually(t, "every event at the endpoint", 60*time.Second, func() bool {
		got = readReceiver(t, receiverAddr)
		counts := map[string]int{}
		for _, v := range got.Verified {
			counts[v.Type]++
		}
		for typ, n := range want {
			if counts[typ] < n {
				return false
			}
		}
		return true
	})
	if got.Rejected != 0 {
		t.Fatalf("the snippet refused %d requests' signatures", got.Rejected)
	}

	// --- the delivery log shows every attempt with its response ---
	status, log := gwDoJSON(t, r, http.MethodGet, "/v1/webhooks/tenants/"+tenant+"/deliveries?limit=100", ada, nil, "")
	if status != http.StatusOK {
		t.Fatalf("delivery log: got %d %v", status, log)
	}
	rails := map[string]bool{}
	retried := 0
	for _, d := range log["deliveries"].([]any) {
		delivery := d.(map[string]any)
		if delivery["status"] != "succeeded" {
			t.Fatalf("delivery %v isn't succeeded", delivery)
		}
		status, detail := gwDoJSON(t, r, http.MethodGet, "/v1/webhooks/tenants/"+tenant+"/deliveries/"+delivery["id"].(string), ada, nil, "")
		attempts, _ := detail["attempt_log"].([]any)
		if status != http.StatusOK || len(attempts) == 0 || len(attempts) != int(detail["attempts"].(float64)) {
			t.Fatalf("delivery detail: %d %v", status, detail)
		}
		for i, a := range attempts {
			at := a.(map[string]any)
			last := i == len(attempts)-1
			if at["response_status"] == nil || (last && at["outcome"] != "succeeded") || (!last && at["response_status"] != float64(500)) {
				t.Fatalf("attempt %d of %s: %v", i+1, detail["event_type"], at)
			}
		}
		if len(attempts) > 1 {
			retried++
		}
		if detail["event_type"] == "wallet.credited" {
			var body struct {
				Data struct {
					Rail string `json:"rail"`
				} `json:"data"`
			}
			_ = json.Unmarshal([]byte(detail["body"].(string)), &body)
			rails[body.Data.Rail] = true
		}
	}
	if retried != 1 || !rails["sandbox"] || !rails["mpesa_ke"] {
		t.Fatalf("%d deliveries retried (want the first wallet.credited, once), rails %v (want sandbox and mpesa_ke)", retried, rails)
	}

	// --- the daily activity report counts the money once (task 5.6) ---
	nairobi, err := time.LoadLocation("Africa/Nairobi")
	if err != nil {
		t.Fatal(err)
	}
	today := time.Now().In(nairobi).Format(time.DateOnly)
	eventually(t, "the report", 30*time.Second, func() bool {
		_, report := gwDoJSON(t, r, http.MethodGet, fmt.Sprintf("/v1/webhooks/tenants/%s/reports/daily-activity?from=%s&to=%s", tenant, today, today), ada, nil, "")
		days, _ := report["days"].([]any)
		if len(days) != 1 {
			return false
		}
		day := days[0].(map[string]any)
		return day["credited_minor"] == float64(1450000) && day["topups"] == float64(2) && day["token_purchases_minor"] == float64(800000)
	})

	// --- no secret in the gateway's log ---
	for _, secret := range []string{endpointSecret, ada, staff1, staff2} {
		if bytes.Contains([]byte(gwLogs.String()), []byte(secret)) {
			t.Fatal("the gateway logged a secret or session token")
		}
	}
}
