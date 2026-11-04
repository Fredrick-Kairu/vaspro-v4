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

	"github.com/google/uuid"

	vasdb "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/testkit"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/adapters"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/email"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/tenantaccess"
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
	return newTestRouterWith(t, nil)
}

// newTestRouterWith is newTestRouter with configure applied to the
// Config first (task 1.16's Google tests set the GOOGLE_* fields).
func newTestRouterWith(t *testing.T, configure func(*identity.Config)) (http.Handler, *email.Sender) {
	t.Helper()
	return newTestRouterWithAccess(t, configure, &tenantaccess.Fake{AllowAll: true})
}

// newTestRouterWithAccess is newTestRouterWith with accounts' access
// check answered by access (task 2.2c); the other constructors let
// every user into every account, so a test selects any account ID.
func newTestRouterWithAccess(t *testing.T, configure func(*identity.Config), access adapters.TenantAccess) (http.Handler, *email.Sender) {
	t.Helper()
	r, sender, _ := newTestRouterWithPool(t, configure, access)
	return r, sender
}

// newTestRouterWithPool is newTestRouterWithAccess that also returns
// the database pool, for a test that must do what only the ops CLI
// does (granting staff, task 2.9).
func newTestRouterWithPool(t *testing.T, configure func(*identity.Config), access adapters.TenantAccess) (http.Handler, *email.Sender, *vasdb.Pool) {
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
		SigningKeysKMSKey:   randomBase64Key(t),
		ChallengeSigningKey: randomBase64Key(t),
		WebAuthnRPID:        "localhost",
		// Set explicitly: a literal Config skips the env loader's
		// "VASPRO" default, and go-webauthn only rejects an empty
		// display name at BeginRegistration, not at construction.
		WebAuthnRPDisplayName: "VASPRO Test",
		WebAuthnOrigins:       []string{"https://localhost"},
		RedisURL:              testkit.StartRedisURL(t),
	}
	if configure != nil {
		configure(&cfg)
	}
	sender := &email.Sender{}
	svc, err := identity.New(cfg, pool, nil, sender, access)
	if err != nil {
		t.Fatalf("building identity service: %v", err)
	}
	// newTestRouter never calls Run(), which is where this normally
	// happens at startup — the OAuth2 token endpoint and JWKS tests
	// both need a real active key to exist.
	if err := svc.EnsureActiveSigningKey(ctx); err != nil {
		t.Fatalf("ensuring an active signing key: %v", err)
	}
	return svc.Router(), sender, pool
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

// selectTenant has sessionToken's session select a new account, which
// the allow-all fake lets anyone into, and returns its ID (task 2.2c:
// API clients and SMPP accounts belong to the selected account).
func selectTenant(t *testing.T, r http.Handler, sessionToken string) string {
	t.Helper()
	tenantID := uuid.NewString()
	status, body := doJSON(t, r, http.MethodPut, "/v1/auth/sessions/current/tenant", sessionToken,
		map[string]string{"tenant_id": tenantID}, "select-"+tenantID)
	if status != http.StatusOK || body["tenant_id"] != tenantID {
		t.Fatalf("selecting an account: got %d %v", status, body)
	}
	return tenantID
}
