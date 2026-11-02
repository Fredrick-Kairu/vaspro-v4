//go:build integration

package gateway

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/kycvendor"
)

// TestGateway_OnboardingThroughKYC_EndToEnd is Phase 2's onboarding
// e2e (ROADMAP Phase 2 tests and exit gate): sign up → pick SMS and
// USSD → Tier 1 KYC through the fake vendor → account promoted →
// tenant.tier_changed published. Real identity and accounts processes;
// the fake vendor posts its signed result to the gateway, as a vendor
// would.
func TestGateway_OnboardingThroughKYC_EndToEnd(t *testing.T) {
	const secret = "e2e-kyc-webhook-secret" //nolint:gosec // G101: a fixture secret shared with the fake vendor, not a credential
	smtp := startFakeSMTPServer(t)
	vendor := kycvendor.New([]byte(secret))
	t.Cleanup(vendor.Close)

	identityGRPC := freeTCPAddr(t)
	acc := startRealAccountsWith(t, identityGRPC,
		"ACCOUNTS_KYC_PROVIDER=fake", "ACCOUNTS_KYC_FAKE_URL="+vendor.URL(), "ACCOUNTS_KYC_WEBHOOK_SECRET="+secret)
	identityBaseURL := startRealIdentityWithLogs(t, smtp.Addr(), nil,
		"IDENTITY_ACCOUNTS_GRPC_ADDR="+acc.grpcAddr, "IDENTITY_GRPC_ADDR="+identityGRPC)
	r := newTestGatewayWithAccounts(t, identityBaseURL, acc.baseURL, nil, slog.New(slog.NewJSONHandler(&syncBuffer{}, nil)))
	gw := httptest.NewServer(r)
	t.Cleanup(gw.Close)
	vendor.SetWebhookURL(gw.URL + "/v1/accounts/kyc/webhooks/fake")

	// --- sign up and create the account ---
	_, ada := signUpVerifySetPasswordAndSignIn(t, r, smtp, "ada@example.com")
	status, account := gwDoJSON(t, r, http.MethodPost, "/v1/accounts/tenants", ada,
		map[string]string{"display_name": "Acme Deliveries", "country_code": "KE"}, "create-account")
	if status != http.StatusCreated || account["tier"] != "sandbox" {
		t.Fatalf("create account: got %d %v", status, account)
	}
	id, _ := account["id"].(string)

	// --- pick SMS and USSD: locked in the sandbox, needing starter ---
	for _, code := range []string{"sms", "ussd"} {
		if status, got := gwDoJSON(t, r, http.MethodPut, "/v1/accounts/tenants/"+id+"/services/"+code, ada, nil, "select-"+code); status != http.StatusOK || got["locked"] != true {
			t.Fatalf("selecting %s: got %d %v", code, status, got)
		}
	}

	// --- Tier 1 KYC through the fake vendor ---
	status, check := gwDoJSON(t, r, http.MethodPost, "/v1/accounts/tenants/"+id+"/kyc-checks", ada,
		map[string]string{"kind": "individual"}, "start-kyc")
	if status != http.StatusCreated || check["status"] != "pending" {
		t.Fatalf("starting KYC: got %d %v", status, check)
	}
	flow, _ := check["url"].(string)
	ref := flow[strings.LastIndex(flow, "/")+1:]

	events := subscribe(t, acc.natsURL, "accounts.tenant.tier_changed")
	if code, err := vendor.Decide(ref, "passed", kycvendor.NoTamper); err != nil || code != http.StatusOK {
		t.Fatalf("the vendor's webhook through the gateway: %d, %v", code, err)
	}

	// --- promoted, and the services it picked are live ---
	status, account = gwDoJSON(t, r, http.MethodGet, "/v1/accounts/tenants/"+id, ada, nil, "")
	if status != http.StatusOK || account["tier"] != "starter" {
		t.Fatalf("after the check: got %d %v, want starter", status, account)
	}
	status, catalogue := gwDoJSON(t, r, http.MethodGet, "/v1/accounts/tenants/"+id+"/services", ada, nil, "")
	if status != http.StatusOK {
		t.Fatalf("services: got %d %v", status, catalogue)
	}
	for _, s := range catalogue["services"].([]any) {
		m := s.(map[string]any)
		if (m["code"] == "sms" || m["code"] == "ussd") && (m["status"] != "enabled" || m["locked"] != false) {
			t.Fatalf("%s after promotion: %v, want enabled", m["code"], m)
		}
	}

	// --- tenant.tier_changed reached the bus ---
	msg, err := events.Next(jetstream.FetchMaxWait(15 * time.Second))
	if err != nil {
		t.Fatalf("no tenant.tier_changed on accounts' stream: %v", err)
	}
	var env struct {
		TenantID string          `json:"tenant_id"`
		Data     json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(msg.Data(), &env); err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	_ = json.Unmarshal(env.Data, &data)
	if env.TenantID != id || data["from_tier"] != "sandbox" || data["to_tier"] != "starter" {
		t.Fatalf("got tenant %s, data %v", env.TenantID, data)
	}
}

// subscribe reads subject from accounts' event stream from the start.
func subscribe(t *testing.T, natsURL, subject string) jetstream.Consumer {
	t.Helper()
	nc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	cons, err := js.OrderedConsumer(context.Background(), "ACCOUNTS_EVENTS", jetstream.OrderedConsumerConfig{FilterSubjects: []string{subject}})
	if err != nil {
		t.Fatalf("subscribing to %s: %v", subject, err)
	}
	return cons
}
