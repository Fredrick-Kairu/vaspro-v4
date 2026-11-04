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

func newBackendsGateway(t *testing.T, identity, accounts *recordingBackend, sessions SessionResolver) http.Handler {
	t.Helper()
	root := repoRoot(t)
	cfg := Config{
		IdentityBaseURL:     identity.URL,
		IdentityOpenAPIPath: filepath.Join(root, "docs/api/identity.yaml"),
		AccountsBaseURL:     accounts.URL,
		AccountsOpenAPIPath: filepath.Join(root, "docs/api/accounts.yaml"),
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
