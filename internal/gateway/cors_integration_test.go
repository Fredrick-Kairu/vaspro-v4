//go:build integration

package gateway

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/clock"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/testkit"
)

// newCORSGateway routes to fake backends with the given allow-list.
func newCORSGateway(t *testing.T, origins []string) (http.Handler, *recordingBackend) {
	t.Helper()
	root := repoRoot(t)
	identity := newRecordingBackend(t)
	cfg := Config{
		IdentityBaseURL:     identity.URL,
		IdentityOpenAPIPath: filepath.Join(root, "docs/api/identity.yaml"),
		AccountsBaseURL:     newRecordingBackend(t).URL,
		AccountsOpenAPIPath: filepath.Join(root, "docs/api/accounts.yaml"),
		LedgerBaseURL:       newRecordingBackend(t).URL,
		LedgerOpenAPIPath:   filepath.Join(root, "docs/api/ledger.yaml"),
		CORSAllowedOrigins:  origins,
	}
	svc, err := New(cfg, testkit.StartRedis(t), clock.Real{}, slog.Default(), nil, fakeSessions{})
	if err != nil {
		t.Fatalf("gateway.New: %v", err)
	}
	return svc.Router(), identity
}

func corsRequest(h http.Handler, method, origin string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/.well-known/jwks.json", nil)
	req.Header.Set("Origin", origin)
	if method == http.MethodOptions {
		req.Header.Set("Access-Control-Request-Method", http.MethodPost)
		req.Header.Set("Access-Control-Request-Headers", "Authorization, Idempotency-Key")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestCORS_AllowListedOrigin(t *testing.T) {
	const origin = "http://localhost:5173"
	h, identity := newCORSGateway(t, []string{origin})

	t.Run("preflight is answered by the gateway", func(t *testing.T) {
		rec := corsRequest(h, http.MethodOptions, origin)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204", rec.Code)
		}
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != origin {
			t.Errorf("Allow-Origin = %q, want %q", got, origin)
		}
		for _, want := range []string{"Authorization", "Idempotency-Key", "Content-Type"} {
			if !containsToken(rec.Header().Get("Access-Control-Allow-Headers"), want) {
				t.Errorf("Allow-Headers %q lacks %s", rec.Header().Get("Access-Control-Allow-Headers"), want)
			}
		}
		if !containsToken(rec.Header().Get("Access-Control-Allow-Methods"), http.MethodPatch) {
			t.Errorf("Allow-Methods %q lacks PATCH", rec.Header().Get("Access-Control-Allow-Methods"))
		}
		if identity.count() != 0 {
			t.Error("a preflight reached the backend")
		}
	})

	t.Run("a request carries the header and is proxied", func(t *testing.T) {
		rec := corsRequest(h, http.MethodGet, origin)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != origin {
			t.Errorf("Allow-Origin = %q, want %q", got, origin)
		}
		if identity.count() != 1 {
			t.Errorf("backend calls = %d, want 1", identity.count())
		}
	})

	t.Run("another origin gets no CORS headers", func(t *testing.T) {
		for _, method := range []string{http.MethodOptions, http.MethodGet} {
			rec := corsRequest(h, method, "https://evil.example")
			if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
				t.Errorf("%s: Allow-Origin = %q, want none", method, got)
			}
		}
	})
}

func TestCORS_EmptyAllowListChangesNothing(t *testing.T) {
	h, _ := newCORSGateway(t, nil)
	get := corsRequest(h, http.MethodGet, "http://localhost:5173")
	if got := get.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("GET: Allow-Origin = %q, want none", got)
	}
	// Without an allow-list the gateway answers no preflights: OPTIONS
	// is refused by request validation (no such operation), as before.
	pre := corsRequest(h, http.MethodOptions, "http://localhost:5173")
	if pre.Code != http.StatusBadRequest {
		t.Errorf("OPTIONS: status = %d, want 400", pre.Code)
	}
	if got := pre.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("OPTIONS: Allow-Origin = %q, want none", got)
	}
}

func containsToken(list, want string) bool {
	for _, part := range strings.Split(list, ",") {
		if strings.TrimSpace(part) == want {
			return true
		}
	}
	return false
}
