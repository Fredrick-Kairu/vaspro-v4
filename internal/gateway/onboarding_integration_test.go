//go:build integration

package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/testkit"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/clamd"
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

	storage := testkit.StartObjectStorage(t)
	scanner, err := clamd.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(scanner.Close)

	identityGRPC := freeTCPAddr(t)
	acc := startRealAccountsWith(t, identityGRPC,
		"ACCOUNTS_KYC_PROVIDER=fake", "ACCOUNTS_KYC_FAKE_URL="+vendor.URL(), "ACCOUNTS_KYC_WEBHOOK_SECRET="+secret,
		"ACCOUNTS_OBJECT_STORAGE_ENDPOINT="+storage.Endpoint, "ACCOUNTS_OBJECT_STORAGE_ACCESS_KEY="+storage.AccessKey,
		"ACCOUNTS_OBJECT_STORAGE_SECRET_KEY="+storage.SecretKey, "ACCOUNTS_CLAMAV_ADDR="+scanner.Addr(),
		"ACCOUNTS_DOCUMENTS_KMS_KEY="+randomBase64Key(t))
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

	// --- Tier 2 starts: the business profile, a director, and KYB goes
	// to the staff review queue (task 2.7, OQ-08) ---
	profilePath := "/v1/accounts/tenants/" + id + "/business-profile"
	if status, got := gwDoJSON(t, r, http.MethodPut, profilePath, ada,
		map[string]string{"registration_number": "PVT-AB1CDEF2", "registered_name": "Acme Deliveries Limited", "tax_id": "P051234567X"}, "kyb-profile"); status != http.StatusOK || got["registry"] != "BRS" {
		t.Fatalf("business profile: got %d %v", status, got)
	}
	if status, got := gwDoJSON(t, r, http.MethodPost, profilePath+"/owners", ada,
		map[string]string{"full_name": "Ada Lovelace", "role": "director", "ownership_pct": "100"}, "kyb-director"); status != http.StatusCreated {
		t.Fatalf("director: got %d %v", status, got)
	}
	if status, got := gwDoJSON(t, r, http.MethodPost, "/v1/accounts/tenants/"+id+"/kyb-submissions", ada, nil, "kyb-too-early"); status != http.StatusBadRequest || got["code"] != "VAL-4000" {
		t.Fatalf("KYB before the documents: got %d %v, want 400 naming them", status, got)
	}
	// The documents Kenya's KYB needs (packs/ke), each scanned, encrypted
	// and stored in object storage by the real accounts process (2.8).
	for _, docType := range []string{"incorporation_cert", "tax_certificate", "proof_of_address"} {
		file := []byte("%PDF-1.4\n% " + docType + " for Acme Deliveries Limited\n%%EOF\n")
		if status, got := sendMultipart(t, r, "/v1/accounts/tenants/"+id+"/documents", ada, docType, file); status != http.StatusCreated {
			t.Fatalf("uploading %s: got %d %v", docType, status, got)
		}
	}
	status, kyb := gwDoJSON(t, r, http.MethodPost, "/v1/accounts/tenants/"+id+"/kyb-submissions", ada, nil, "kyb-submit")
	if status != http.StatusCreated || kyb["status"] != "review" {
		t.Fatalf("KYB submission: got %d %v, want a check in review", status, kyb)
	}
	kybID, _ := kyb["id"].(string)

	// --- tenant.tier_changed reached the bus ---
	expectTierChanged(t, events, id, "sandbox", "starter")

	// --- staff review (task 2.9): an operator grants staff with the real
	// ops CLI; the client's own Admin can't reach the queue ---
	_, staff := signUpVerifySetPasswordAndSignIn(t, r, smtp, "staff@example.com")
	runVasproCLI(t, identityBaseURL, "identity", "grant-staff", "-by", "e2e-operator", "staff@example.com")
	if status, got := gwDoJSON(t, r, http.MethodGet, "/v1/accounts/staff/kyc-reviews", ada, nil, ""); status != http.StatusForbidden || got["code"] != "AUTH-4025" {
		t.Fatalf("the client's Admin on the queue: got %d %v, want 403 AUTH-4025", status, got)
	}
	status, queue := gwDoJSON(t, r, http.MethodGet, "/v1/accounts/staff/kyc-reviews", staff, nil, "")
	reviews, _ := queue["reviews"].([]any)
	if status != http.StatusOK || len(reviews) != 1 || reviews[0].(map[string]any)["check_id"] != kybID {
		t.Fatalf("the staff queue: got %d %v, want the KYB check", status, queue)
	}
	status, review := gwDoJSON(t, r, http.MethodGet, "/v1/accounts/staff/kyc-reviews/"+kybID, staff, nil, "")
	docs, _ := review["documents"].([]any)
	if status != http.StatusOK || len(docs) != 3 {
		t.Fatalf("the review: got %d %v, want its three documents", status, review)
	}
	for _, d := range docs {
		doc := d.(map[string]any)
		if doc["type"] != "incorporation_cert" {
			continue
		}
		req := httptest.NewRequest(http.MethodGet, "/v1/accounts/staff/tenants/"+id+"/documents/"+doc["id"].(string)+"/content", nil)
		req.Header.Set("Authorization", "Bearer "+staff)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "incorporation_cert for Acme Deliveries Limited") {
			t.Fatalf("staff opening the certificate: got %d %q", rec.Code, rec.Body.String())
		}
	}
	if status, got := gwDoJSON(t, r, http.MethodPost, "/v1/accounts/staff/kyc-reviews/"+kybID+"/decision", staff,
		map[string]string{"decision": "passed", "reason": "Certificate matches the registry extract"}, "kyb-decision"); status != http.StatusOK || got["status"] != "passed" {
		t.Fatalf("passing KYB: got %d %v", status, got)
	}
	status, account = gwDoJSON(t, r, http.MethodGet, "/v1/accounts/tenants/"+id, ada, nil, "")
	if status != http.StatusOK || account["tier"] != "business" {
		t.Fatalf("after KYB: got %d %v, want business", status, account)
	}
	expectTierChanged(t, events, id, "starter", "business")

	// --- staff move it to financial and make it a reseller; the client
	// then creates a sub-account ---
	if status, got := gwDoJSON(t, r, http.MethodPost, "/v1/accounts/staff/tenants/"+id+"/tier-changes", staff,
		map[string]string{"to": "financial", "reason": "Signed financial services agreement"}, "to-financial"); status != http.StatusOK || got["tier"] != "financial" {
		t.Fatalf("to financial: got %d %v", status, got)
	}
	if status, got := gwDoJSON(t, r, http.MethodPost, "/v1/accounts/staff/tenants/"+id+"/reseller", staff,
		map[string]string{"reason": "Signed reseller agreement"}, "make-reseller"); status != http.StatusOK || got["type"] != "reseller" {
		t.Fatalf("making a reseller: got %d %v", status, got)
	}
	if status, got := gwDoJSON(t, r, http.MethodPost, "/v1/accounts/tenants/"+id+"/sub-accounts", ada,
		map[string]string{"display_name": "Acme Client One"}, "sub-account"); status != http.StatusCreated || got["type"] != "sub_account" {
		t.Fatalf("the reseller's first sub-account: got %d %v", status, got)
	}

	// --- unlock by staff (task 2.9, carried from Phase 1) ---
	signUpVerifySetPasswordAndSignIn(t, r, smtp, "locked@example.com")
	signIn := func(password, key string) int {
		status, _ := gwDoJSON(t, r, http.MethodPost, "/v1/auth/sessions", "",
			map[string]string{"email": "locked@example.com", "password": password}, key)
		return status
	}
	for i := 0; i < 5; i++ {
		signIn("wrong-password", fmt.Sprintf("locked-wrong-%d", i))
	}
	if status := signIn("a-strong-password", "locked-right-1"); status != http.StatusUnauthorized {
		t.Fatalf("expected the lockout, got %d", status)
	}
	if status, got := gwDoJSON(t, r, http.MethodPost, "/v1/auth/staff/unlocks", ada,
		map[string]string{"email": "locked@example.com"}, "unlock-not-staff"); status != http.StatusForbidden || got["code"] != "AUTH-4025" {
		t.Fatalf("a client unlocking: got %d %v", status, got)
	}
	if status, got := gwDoJSON(t, r, http.MethodPost, "/v1/auth/staff/unlocks", staff,
		map[string]string{"email": "locked@example.com"}, "unlock"); status != http.StatusNoContent {
		t.Fatalf("staff unlocking: got %d %v", status, got)
	}
	if status := signIn("a-strong-password", "locked-right-2"); status != http.StatusOK {
		t.Fatalf("after the unlock: got %d, want 200", status)
	}
}

// expectTierChanged reads the next tenant.tier_changed off events and
// checks it moved tenant from one tier to another.
func expectTierChanged(t *testing.T, events jetstream.Consumer, tenant, from, to string) {
	t.Helper()
	msg, err := events.Next(jetstream.FetchMaxWait(15 * time.Second))
	if err != nil {
		t.Fatalf("no tenant.tier_changed (%s → %s) on accounts' stream: %v", from, to, err)
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
	if env.TenantID != tenant || data["from_tier"] != from || data["to_tier"] != to {
		t.Fatalf("got tenant %s, data %v; want %s → %s", env.TenantID, data, from, to)
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
