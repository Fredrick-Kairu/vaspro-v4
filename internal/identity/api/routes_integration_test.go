//go:build integration

package api_test

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	vasdb "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/testkit"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/email"
)

func randomBase64Key(t *testing.T) string {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("generating random key: %v", err)
	}
	return base64.StdEncoding.EncodeToString(key)
}

// newTestRouter builds a real identity.Service's router against a real
// Postgres container. nil for the bus is safe here: Router() never
// touches it, only Run() does. Returns the fake email sender too, so a
// test that needs to read an emailed code (step-up, verification,
// password reset) doesn't have to reach around the router for it.
func newTestRouter(t *testing.T) (http.Handler, *email.Sender) {
	t.Helper()
	ctx := t.Context()

	pg := testkit.StartPostgres(t, testkit.WithMigrations("../../../migrations/identity"))
	pool, err := vasdb.NewPool(ctx, pg.DSN()+"&search_path=identity,public")
	if err != nil {
		t.Fatalf("failed to connect with search_path=identity,public: %v", err)
	}
	t.Cleanup(pool.Close)

	cfg := identity.Config{
		TOTPKMSKey:          randomBase64Key(t),
		ChallengeSigningKey: randomBase64Key(t),
		WebAuthnRPID:        "localhost",
		WebAuthnOrigins:     []string{"https://localhost"},
		RedisURL:            testkit.StartRedisURL(t),
	}
	sender := &email.Sender{}
	svc, err := identity.New(cfg, pool, nil, sender)
	if err != nil {
		t.Fatalf("building identity service: %v", err)
	}
	return svc.Router(), sender
}

func TestCreateSignup_MissingIdempotencyKey_Rejected(t *testing.T) {
	r, _ := newTestRouter(t)

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
	r, _ := newTestRouter(t)

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
	r, _ := newTestRouter(t)

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
	r, _ := newTestRouter(t)

	for _, path := range []string{"/healthz", "/readyz"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: got status %d", path, rec.Code)
		}
	}
}
