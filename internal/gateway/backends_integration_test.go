//go:build integration

package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/clock"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/testkit"
)

// fakeSessions resolves the tokens it holds and rejects the rest, like
// identity's ResolveSession.
type fakeSessions map[string][2]string // token → {session id, user id}

func (f fakeSessions) ResolveSession(_ context.Context, token string) (string, string, error) {
	v, ok := f[token]
	if !ok {
		return "", "", errInvalidSession
	}
	return v[0], v[1], nil
}

// recordingBackend stands in for a backend service and records what
// the gateway forwarded to it.
type recordingBackend struct {
	*httptest.Server
	mu    sync.Mutex
	calls []*http.Request
}

func newRecordingBackend(t *testing.T) *recordingBackend {
	t.Helper()
	b := &recordingBackend{}
	b.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		b.calls = append(b.calls, r.Clone(context.Background()))
		b.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(b.Close)
	return b
}

func (b *recordingBackend) last(t *testing.T) *http.Request {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.calls) == 0 {
		t.Fatal("the backend was never called")
	}
	return b.calls[len(b.calls)-1]
}

func (b *recordingBackend) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.calls)
}

// newBackendsGateway routes to fake backends. others are the ledger's
// and payments', in that order, both optional: a fresh recording
// backend stands in for each one not given.
func newBackendsGateway(t *testing.T, identity, accounts *recordingBackend, sessions SessionResolver, others ...*recordingBackend) http.Handler {
	t.Helper()
	root := repoRoot(t)
	for len(others) < 2 {
		others = append(others, newRecordingBackend(t))
	}
	ledger, payments := others[:1], others[1]
	cfg := Config{
		IdentityBaseURL:     identity.URL,
		IdentityOpenAPIPath: filepath.Join(root, "docs/api/identity.yaml"),
		AccountsBaseURL:     accounts.URL,
		AccountsOpenAPIPath: filepath.Join(root, "docs/api/accounts.yaml"),
		LedgerBaseURL:       ledger[0].URL,
		LedgerOpenAPIPath:   filepath.Join(root, "docs/api/ledger.yaml"),
		PaymentsBaseURL:     payments.URL,
		PaymentsOpenAPIPath: filepath.Join(root, "docs/api/payments.yaml"),
	}
	svc, err := New(cfg, testkit.StartRedis(t), clock.Real{}, slog.Default(), nil, sessions)
	if err != nil {
		t.Fatalf("gateway.New: %v", err)
	}
	return svc.Router()
}

func send(t *testing.T, h http.Handler, method, path string, headers map[string]string, body any) (int, map[string]any) {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var decoded map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &decoded)
	return rec.Code, decoded
}

// TestGatewayBackends_Ledger: /v1/ledger/ (task 3.4b) is routed like
// /v1/accounts/: a session is required, the user and session are
// forwarded and spoofed copies replaced, and requests are validated
// against the ledger's own spec before the ledger sees them.
func TestGatewayBackends_Ledger(t *testing.T) {
	identity, accounts, ledger := newRecordingBackend(t), newRecordingBackend(t), newRecordingBackend(t)
	sessions := fakeSessions{"sess_good": {"11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222"}}
	gw := newBackendsGateway(t, identity, accounts, sessions, ledger)
	price := map[string]string{"service_code": "sms", "currency": "KES", "minor_per_token": "80", "effective_from": "2026-01-01T00:00:00Z"}

	t.Run("a staff price call reaches the ledger with the session's user, not the token or a spoofed user", func(t *testing.T) {
		status, _ := send(t, gw, http.MethodPost, "/v1/ledger/staff/token-prices",
			map[string]string{"Authorization": "Bearer sess_good", "Idempotency-Key": "kl1", headerUserID: "33333333-3333-3333-3333-333333333333"}, price)
		if status != http.StatusOK {
			t.Fatalf("got status %d", status)
		}
		req := ledger.last(t)
		if req.Header.Get(headerUserID) != "22222222-2222-2222-2222-222222222222" ||
			req.Header.Get(headerSessionID) != "11111111-1111-1111-1111-111111111111" || req.Header.Get("Authorization") != "" {
			t.Fatalf("forwarded user %q session %q auth %q", req.Header.Get(headerUserID), req.Header.Get(headerSessionID), req.Header.Get("Authorization"))
		}
	})
	t.Run("no session is 401 and never reaches the ledger", func(t *testing.T) {
		before := ledger.count()
		status, body := send(t, gw, http.MethodGet, "/v1/ledger/staff/token-prices", nil, nil)
		if status != http.StatusUnauthorized || body["code"] != "AUTH-4010" || ledger.count() != before {
			t.Fatalf("got %d %v", status, body)
		}
	})
	t.Run("validated against the ledger's spec", func(t *testing.T) {
		before := ledger.count()
		bad := map[string]string{"service_code": "sms", "currency": "KES"}
		status, body := send(t, gw, http.MethodPost, "/v1/ledger/staff/token-prices",
			map[string]string{"Authorization": "Bearer sess_good", "Idempotency-Key": "kl2"}, bad)
		if status != http.StatusBadRequest || body["code"] != "VAL-4000" || ledger.count() != before {
			t.Fatalf("got %d %v, want 400 VAL-4000 before the ledger", status, body)
		}
	})
	t.Run("a write without an Idempotency-Key is refused at the edge", func(t *testing.T) {
		before := ledger.count()
		status, _ := send(t, gw, http.MethodPost, "/v1/ledger/staff/token-prices/"+uuid.NewString()+"/approval",
			map[string]string{"Authorization": "Bearer sess_good"}, nil)
		if status != http.StatusBadRequest || ledger.count() != before {
			t.Fatalf("got %d", status)
		}
	})
	t.Run("a dashboard wallet route reaches the ledger with the session's user (task 3.9)", func(t *testing.T) {
		tenant := uuid.NewString()
		status, _ := send(t, gw, http.MethodGet, "/v1/ledger/tenants/"+tenant+"/token-quote?service_code=sms&currency=KES&tokens=100",
			map[string]string{"Authorization": "Bearer sess_good"}, nil)
		if status != http.StatusOK {
			t.Fatalf("got status %d", status)
		}
		req := ledger.last(t)
		if req.URL.Path != "/v1/ledger/tenants/"+tenant+"/token-quote" || req.Header.Get(headerUserID) != "22222222-2222-2222-2222-222222222222" {
			t.Fatalf("forwarded %s as %q", req.URL.Path, req.Header.Get(headerUserID))
		}
		before := ledger.count()
		status, body := send(t, gw, http.MethodGet, "/v1/ledger/tenants/"+tenant+"/token-quote?service_code=sms",
			map[string]string{"Authorization": "Bearer sess_good"}, nil)
		if status != http.StatusBadRequest || body["code"] != "VAL-4000" || ledger.count() != before {
			t.Fatalf("a quote without tokens: %d %v, want 400 before the ledger", status, body)
		}
	})
	t.Run("ledger paths never fall through to identity", func(t *testing.T) {
		before := identity.count()
		_, _ = send(t, gw, http.MethodGet, "/v1/ledger/unknown", map[string]string{"Authorization": "Bearer sess_good"}, nil)
		if identity.count() != before {
			t.Fatal("a /v1/ledger/ request reached identity")
		}
	})
}

func TestGatewayBackends(t *testing.T) {
	identity := newRecordingBackend(t)
	accounts := newRecordingBackend(t)
	sessions := fakeSessions{"sess_good": {"11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222"}}
	gw := newBackendsGateway(t, identity, accounts, sessions)
	createTenant := map[string]string{"display_name": "Acme", "country_code": "KE"}

	t.Run("an accounts call with a valid session reaches accounts with the user, not the token", func(t *testing.T) {
		status, _ := send(t, gw, http.MethodPost, "/v1/accounts/tenants",
			map[string]string{"Authorization": "Bearer sess_good", "Idempotency-Key": "k1"}, createTenant)
		if status != http.StatusOK {
			t.Fatalf("got status %d", status)
		}
		req := accounts.last(t)
		if req.Header.Get(headerUserID) != "22222222-2222-2222-2222-222222222222" ||
			req.Header.Get(headerSessionID) != "11111111-1111-1111-1111-111111111111" {
			t.Fatalf("forwarded user %q session %q", req.Header.Get(headerUserID), req.Header.Get(headerSessionID))
		}
		if req.Header.Get("Authorization") != "" {
			t.Fatal("the session token was forwarded to accounts")
		}
	})
	t.Run("a client cannot spoof the forwarded user", func(t *testing.T) {
		_, _ = send(t, gw, http.MethodGet, "/v1/accounts/tenants/abc",
			map[string]string{"Authorization": "Bearer sess_good", headerUserID: "33333333-3333-3333-3333-333333333333"}, nil)
		if got := accounts.last(t).Header.Get(headerUserID); got != "22222222-2222-2222-2222-222222222222" {
			t.Fatalf("accounts saw user %q, want the session's", got)
		}
	})
	t.Run("identity never receives client-sent user headers", func(t *testing.T) {
		_, _ = send(t, gw, http.MethodGet, "/v1/auth/sessions",
			map[string]string{"Authorization": "Bearer sess_good", headerUserID: "33333333-3333-3333-3333-333333333333", headerSessionID: "x"}, nil)
		req := identity.last(t)
		if req.Header.Get(headerUserID) != "" || req.Header.Get(headerSessionID) != "" {
			t.Fatal("identity received client-sent user headers")
		}
		if req.Header.Get("Authorization") != "Bearer sess_good" {
			t.Fatal("identity's own routes must still get the bearer token")
		}
	})
	for name, auth := range map[string]string{
		"no session token":    "",
		"an unknown token":    "Bearer sess_bad",
		"a non-bearer header": "Basic abc",
	} {
		t.Run("accounts with "+name+" is 401 AUTH-4010 and never reaches accounts", func(t *testing.T) {
			before := accounts.count()
			headers := map[string]string{"Idempotency-Key": "k-" + name}
			if auth != "" {
				headers["Authorization"] = auth
			}
			status, body := send(t, gw, http.MethodPost, "/v1/accounts/tenants", headers, createTenant)
			if status != http.StatusUnauthorized || body["code"] != "AUTH-4010" {
				t.Fatalf("got %d %v, want 401 AUTH-4010", status, body)
			}
			if accounts.count() != before {
				t.Fatal("accounts was called")
			}
		})
	}
	t.Run("an accounts request is validated against accounts' own spec", func(t *testing.T) {
		before := accounts.count()
		status, body := send(t, gw, http.MethodPost, "/v1/accounts/tenants",
			map[string]string{"Authorization": "Bearer sess_good", "Idempotency-Key": "k-bad"}, map[string]string{"country_code": "KE"})
		if status != http.StatusBadRequest || body["code"] != "VAL-4000" {
			t.Fatalf("got %d %v, want 400 VAL-4000", status, body)
		}
		if accounts.count() != before {
			t.Fatal("an invalid request reached accounts")
		}
	})
	t.Run("a KYC vendor's webhook reaches accounts with no session and no Idempotency-Key", func(t *testing.T) {
		before := accounts.count()
		status, body := send(t, gw, http.MethodPost, "/v1/accounts/kyc/webhooks/fake",
			map[string]string{headerUserID: "33333333-3333-3333-3333-333333333333"}, map[string]string{"provider_ref": "fkyc_1", "status": "passed"})
		if status != http.StatusOK || accounts.count() != before+1 {
			t.Fatalf("got %d %v, accounts calls %d → %d", status, body, before, accounts.count())
		}
		if got := accounts.last(t).Header.Get(headerUserID); got != "" {
			t.Fatalf("a client-sent user header reached accounts on the public route: %q", got)
		}
	})
	t.Run("only operations the spec marks public skip the session", func(t *testing.T) {
		before := accounts.count()
		status, body := send(t, gw, http.MethodPost, "/v1/accounts/tenants/"+uuid.NewString()+"/kyc-checks",
			map[string]string{"Idempotency-Key": "k-kyc"}, map[string]string{"kind": "individual"})
		if status != http.StatusUnauthorized || body["code"] != "AUTH-4010" || accounts.count() != before {
			t.Fatalf("starting KYC without a session: got %d %v", status, body)
		}
	})
	t.Run("a 5 MB document upload passes the gateway; 1 MB still bounds everything else", func(t *testing.T) {
		uploadPath := "/v1/accounts/tenants/" + uuid.NewString() + "/documents"
		big := append([]byte("%PDF-1.4\n"), make([]byte, 5<<20)...)
		before := accounts.count()
		if status, body := sendMultipart(t, gw, uploadPath, "sess_good", "incorporation_cert", big); status != http.StatusOK || accounts.count() != before+1 {
			t.Fatalf("5 MB upload: got %d %v", status, body)
		}
		huge := append([]byte("%PDF-1.4\n"), make([]byte, 12<<20)...)
		if status, body := sendMultipart(t, gw, uploadPath, "sess_good", "incorporation_cert", huge); status != http.StatusRequestEntityTooLarge || body["code"] != "VAL-4008" || accounts.count() != before+1 {
			t.Fatalf("12 MB upload: got %d %v, want 413 VAL-4008 before accounts", status, body)
		}
		status, _ := send(t, gw, http.MethodPost, "/v1/accounts/tenants",
			map[string]string{"Authorization": "Bearer sess_good", "Idempotency-Key": "k-big"},
			map[string]string{"display_name": strings.Repeat("x", 2<<20), "country_code": "KE"})
		if status < 400 || accounts.count() != before+1 {
			t.Fatalf("a 2 MB JSON body: got %d, accounts calls %d; want it refused at the gateway", status, accounts.count()-before)
		}
	})
	t.Run("identity routes still go to identity", func(t *testing.T) {
		before := identity.count()
		status, _ := send(t, gw, http.MethodPost, "/v1/auth/signups",
			map[string]string{"Idempotency-Key": "k-signup"}, map[string]string{"email": "ada@example.com", "full_name": "Ada"})
		if status != http.StatusOK || identity.count() != before+1 {
			t.Fatalf("got %d, identity calls %d → %d", status, before, identity.count())
		}
	})
}

// sendMultipart posts a document upload as the dashboard would.
func sendMultipart(t *testing.T, h http.Handler, path, token, docType string, data []byte) (int, map[string]any) {
	t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	_ = form.WriteField("type", docType)
	part, _ := form.CreateFormFile("file", "document.pdf")
	_, _ = part.Write(data)
	_ = form.Close()
	req := httptest.NewRequest(http.MethodPost, path, &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Idempotency-Key", "k-"+docType+strconv.Itoa(len(data)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var decoded map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &decoded)
	return rec.Code, decoded
}

// TestGatewayBackends_Payments: /v1/payments/ (task 4.3) is routed like
// the ledger: a session is required, the user is forwarded and a
// spoofed copy replaced, the token isn't forwarded, and payments paths
// never fall through to identity.
func TestGatewayBackends_Payments(t *testing.T) {
	identity, accounts, ledger, payments := newRecordingBackend(t), newRecordingBackend(t), newRecordingBackend(t), newRecordingBackend(t)
	sessions := fakeSessions{"sess_good": {"11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222"}}
	gw := newBackendsGateway(t, identity, accounts, sessions, ledger, payments)
	path := "/v1/payments/tenants/" + uuid.NewString() + "/topup-details"

	t.Run("top-up details reach payments with the session's user", func(t *testing.T) {
		status, _ := send(t, gw, http.MethodGet, path,
			map[string]string{"Authorization": "Bearer sess_good", headerUserID: "33333333-3333-3333-3333-333333333333"}, nil)
		if status != http.StatusOK {
			t.Fatalf("got status %d", status)
		}
		req := payments.last(t)
		if req.URL.Path != path || req.Header.Get(headerUserID) != "22222222-2222-2222-2222-222222222222" || req.Header.Get("Authorization") != "" {
			t.Fatalf("forwarded %s as %q with auth %q", req.URL.Path, req.Header.Get(headerUserID), req.Header.Get("Authorization"))
		}
	})
	t.Run("no session is 401 and never reaches payments", func(t *testing.T) {
		before := payments.count()
		status, body := send(t, gw, http.MethodGet, path, nil, nil)
		if status != http.StatusUnauthorized || body["code"] != "AUTH-4010" || payments.count() != before {
			t.Fatalf("got %d %v", status, body)
		}
	})
	t.Run("payments paths never fall through to identity", func(t *testing.T) {
		before := identity.count()
		status, _ := send(t, gw, http.MethodGet, "/v1/payments/not-an-operation", map[string]string{"Authorization": "Bearer sess_good"}, nil)
		if status == http.StatusOK || identity.count() != before {
			t.Fatalf("got %d, identity called %d times", status, identity.count()-before)
		}
	})
}

// TestGatewayBackends_PaymentsCallbacks (task 4.4): a rail callback
// reaches payments with no session and no Idempotency-Key, and every
// forwarded request carries the address the gateway saw in
// X-Vaspro-Client-Ip, never one the caller sent.
func TestGatewayBackends_PaymentsCallbacks(t *testing.T) {
	identity, accounts, ledger, payments := newRecordingBackend(t), newRecordingBackend(t), newRecordingBackend(t), newRecordingBackend(t)
	sessions := fakeSessions{"sess_good": {"11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222"}}
	gw := newBackendsGateway(t, identity, accounts, sessions, ledger, payments)
	const clientIPHeader = "X-Vaspro-Client-Ip"

	callback := "/v1/payments/callbacks/daraja/" + uuid.NewString() + "/" + strings.Repeat("ab", 32) + "/stk"
	status, body := send(t, gw, http.MethodPost, callback, map[string]string{clientIPHeader: "196.201.214.200"},
		map[string]any{"Body": map[string]any{"stkCallback": map[string]any{"ResultCode": 0}}})
	if status != http.StatusOK {
		t.Fatalf("callback: %d %v", status, body)
	}
	req := payments.last(t)
	if req.URL.Path != callback || req.Header.Get(clientIPHeader) != "192.0.2.1" {
		t.Fatalf("forwarded %s with client IP %q, want the connection's 192.0.2.1", req.URL.Path, req.Header.Get(clientIPHeader))
	}

	confirmation := "/v1/payments/callbacks/daraja/" + uuid.NewString() + "/" + strings.Repeat("ab", 32) + "/c2b/confirmation"
	if status, body := send(t, gw, http.MethodPost, confirmation, nil, map[string]any{"TransID": "RKTQDM7W6S"}); status != http.StatusOK || payments.last(t).URL.Path != confirmation {
		t.Fatalf("a C2B confirmation (task 4.5): %d %v", status, body)
	}

	status, _ = send(t, gw, http.MethodGet, "/v1/payments/tenants/"+uuid.NewString()+"/topup-details",
		map[string]string{"Authorization": "Bearer sess_good", clientIPHeader: "10.0.0.1"}, nil)
	if status != http.StatusOK || payments.last(t).Header.Get(clientIPHeader) != "192.0.2.1" {
		t.Fatalf("a dashboard call: %d, client IP %q", status, payments.last(t).Header.Get(clientIPHeader))
	}
}

// TestGatewayBackends_StatementImportLimit (task 4.6): a statement
// import may carry up to 10 MB, like a document upload; other payments
// writes keep the 1 MB limit.
func TestGatewayBackends_StatementImportLimit(t *testing.T) {
	identity, accounts, ledger, payments := newRecordingBackend(t), newRecordingBackend(t), newRecordingBackend(t), newRecordingBackend(t)
	sessions := fakeSessions{"sess_good": {"11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222"}}
	gw := newBackendsGateway(t, identity, accounts, sessions, ledger, payments)
	headers := map[string]string{"Authorization": "Bearer sess_good", "Idempotency-Key": "si1"}
	body := func(size int) map[string]any {
		return map[string]any{"rail_account_id": uuid.NewString(), "date": "2026-11-11", "csv": strings.Repeat("x", size)}
	}
	if status, got := send(t, gw, http.MethodPost, "/v1/payments/staff/statement-imports", headers, body(5<<20)); status != http.StatusOK {
		t.Fatalf("a 5 MB statement: %d %v", status, got)
	}
	before := payments.count()
	headers["Idempotency-Key"] = "si2"
	if status, got := send(t, gw, http.MethodPost, "/v1/payments/staff/statement-imports", headers, body(12<<20)); status != http.StatusRequestEntityTooLarge || got["code"] != "VAL-4008" || payments.count() != before {
		t.Fatalf("a 12 MB statement: %d %v", status, got)
	}
	headers["Idempotency-Key"] = "si3"
	big := map[string]any{"account_number": strings.Repeat("x", 2<<20), "reason": "x"}
	if status, _ := send(t, gw, http.MethodPost, "/v1/payments/staff/collection-notices/"+uuid.NewString()+"/match", headers, big); status == http.StatusOK || payments.count() != before {
		t.Fatalf("2 MB elsewhere: %d", status)
	}
}

// TestGatewayBackends_Checkout (task 4.8): the hosted page and the
// public checkout API reach payments with no session, the page without
// spec validation; a client-sent user header is still stripped.
func TestGatewayBackends_Checkout(t *testing.T) {
	identity, accounts, ledger, payments := newRecordingBackend(t), newRecordingBackend(t), newRecordingBackend(t), newRecordingBackend(t)
	gw := newBackendsGateway(t, identity, accounts, fakeSessions{}, ledger, payments)
	token := "eyJsIjoiMSJ9.c2lnbmF0dXJl" // gitleaks:allow -- a made-up token: base64 of {"l":"1"} and "signature"; the recording backend never checks it
	spoof := map[string]string{headerUserID: "33333333-3333-3333-3333-333333333333"}

	if status, _ := send(t, gw, http.MethodGet, "/pay/"+token, spoof, nil); status != http.StatusOK {
		t.Fatalf("the page: %d", status)
	}
	if req := payments.last(t); req.URL.Path != "/pay/"+token || req.Header.Get(headerUserID) != "" {
		t.Fatalf("forwarded %s with user %q", req.URL.Path, req.Header.Get(headerUserID))
	}
	if status, _ := send(t, gw, http.MethodGet, "/pay/static/checkout.js", nil, nil); status != http.StatusOK || payments.last(t).URL.Path != "/pay/static/checkout.js" {
		t.Fatalf("the script: %d", status)
	}
	if status, body := send(t, gw, http.MethodGet, "/v1/payments/checkout/"+token, nil, nil); status != http.StatusOK {
		t.Fatalf("the public API: %d %v", status, body)
	}
	if status, body := send(t, gw, http.MethodPost, "/v1/payments/checkout/"+token+"/payments", map[string]string{"Idempotency-Key": "c1"},
		map[string]any{"payer_phone": "+254708374149"}); status != http.StatusOK {
		t.Fatalf("a payment: %d %v", status, body)
	}
	before := payments.count()
	if status, _ := send(t, gw, http.MethodGet, "/v1/payments/tenants/"+uuid.NewString()+"/topup-details", nil, nil); status != http.StatusUnauthorized || payments.count() != before {
		t.Fatalf("a dashboard route still needs a session: %d", status)
	}
}
