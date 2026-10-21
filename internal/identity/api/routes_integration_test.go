//go:build integration

package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	vasdb "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/testkit"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/email"
)

// newTestRouter builds a real identity.Service's router against a real
// Postgres container. nil for the bus is safe here: Router() never
// touches it, only Run() does.
func newTestRouter(t *testing.T) http.Handler {
	t.Helper()
	ctx := t.Context()

	pg := testkit.StartPostgres(t, testkit.WithMigrations("../../../migrations/identity"))
	pool, err := vasdb.NewPool(ctx, pg.DSN()+"&search_path=identity,public")
	if err != nil {
		t.Fatalf("failed to connect with search_path=identity,public: %v", err)
	}
	t.Cleanup(pool.Close)

	svc := identity.New(identity.Config{}, pool, nil, &email.Sender{})
	return svc.Router()
}

func TestCreateSignup_MissingIdempotencyKey_Rejected(t *testing.T) {
	r := newTestRouter(t)

	body, _ := json.Marshal(map[string]string{"email": "ada@example.com", "full_name": "Ada Lovelace"})
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/signups", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code == http.StatusAccepted {
		t.Fatalf("expected a request with no Idempotency-Key to be rejected, got 202")
	}
}

func TestCreateSignup_WithIdempotencyKey_Returns202(t *testing.T) {
	r := newTestRouter(t)

	body, _ := json.Marshal(map[string]string{"email": "ada@example.com", "full_name": "Ada Lovelace"})
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/signups", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "test-key-1")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("got status %d, want 202 (body: %s)", rec.Code, rec.Body.String())
	}
}

func TestCreateSignup_MalformedEmail_ReturnsVAL4000ProblemJSON(t *testing.T) {
	r := newTestRouter(t)

	body, _ := json.Marshal(map[string]string{"email": "not-an-email", "full_name": "Ada Lovelace"})
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/signups", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "test-key-2")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got status %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("got Content-Type %q, want application/problem+json", ct)
	}

	var problem struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
		t.Fatalf("unexpected error decoding problem body: %v", err)
	}
	if problem.Code != "VAL-4000" {
		t.Fatalf("got code %q, want VAL-4000 (body: %s)", problem.Code, rec.Body.String())
	}
}

func TestHealthzAndReadyz(t *testing.T) {
	r := newTestRouter(t)

	for _, path := range []string{"/healthz", "/readyz"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: got status %d", path, rec.Code)
		}
	}
}
