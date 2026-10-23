//go:build integration

// This file proves the gateway against a REAL identity process — not
// identity's Go package (internal/gateway must never import
// internal/identity, the same boundary .golangci.yml's new "gateway:"
// depguard block now enforces) but a real `cmd/identity` binary,
// built and run as a subprocess, exactly the way
// tools/scaffold/scaffold_integration_test.go already proves a
// generated service end to end without importing it. Reading a
// verification/step-up code means capturing a real email: a minimal
// SMTP server (just enough of RFC 5321 for net/smtp.SendMail, the
// same "no auth, no TLS" shape adapters.SMTPSender already uses)
// stands in for Mailpit and hands the test the raw message body to
// pull a 6-digit code out of — the same generic, documented shape
// every one of identity's own code emails uses (1.2's
// domain.GenerateVerificationCode), not anything internal.
package gateway

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/clock"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/testkit"
)

// --- a minimal SMTP catcher ---

// fakeSMTPServer implements just enough of RFC 5321 to satisfy
// net/smtp.SendMail with a nil Auth (adapters.SMTPSender's exact
// usage) — EHLO/HELO, MAIL, RCPT, DATA, QUIT. No STARTTLS/AUTH
// extensions are advertised, so SendMail never tries either.
type fakeSMTPServer struct {
	ln net.Listener

	mu       sync.Mutex
	messages []string
}

func startFakeSMTPServer(t *testing.T) *fakeSMTPServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	s := &fakeSMTPServer{ln: ln}
	go s.serve()
	t.Cleanup(func() { _ = ln.Close() })
	return s
}

func (s *fakeSMTPServer) Addr() string { return s.ln.Addr().String() }

func (s *fakeSMTPServer) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *fakeSMTPServer) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	write := func(line string) {
		_, _ = conn.Write([]byte(line + "\r\n"))
	}
	write("220 localhost ESMTP")

	r := bufio.NewReader(conn)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		upper := strings.ToUpper(line)

		switch {
		case strings.HasPrefix(upper, "EHLO"), strings.HasPrefix(upper, "HELO"):
			write("250 localhost")
		case strings.HasPrefix(upper, "MAIL FROM:"):
			write("250 OK")
		case strings.HasPrefix(upper, "RCPT TO:"):
			write("250 OK")
		case upper == "DATA":
			write("354 Go ahead")
			var body strings.Builder
			for {
				dataLine, err := r.ReadString('\n')
				if err != nil {
					return
				}
				dataLine = strings.TrimRight(dataLine, "\r\n")
				if dataLine == "." {
					break
				}
				body.WriteString(strings.TrimPrefix(dataLine, "."))
				body.WriteString("\n")
			}
			s.mu.Lock()
			s.messages = append(s.messages, body.String())
			s.mu.Unlock()
			write("250 OK")
		case upper == "QUIT":
			write("221 Bye")
			return
		default:
			write("500 unrecognized command")
		}
	}
}

// count returns how many emails have been captured so far.
func (s *fakeSMTPServer) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.messages)
}

// waitForNext waits for a message past index since (the count()
// observed right before whatever action was expected to trigger one)
// to arrive, and returns it — signup/step-up emails are sent
// synchronously inside the handler, so the message exists by the time
// the HTTP response the test is waiting on comes back, but a small
// poll covers any scheduling slack. Indexed rather than "latest" so
// two sequential sends can never be confused with each other.
func (s *fakeSMTPServer) waitForNext(t *testing.T, since int) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		if len(s.messages) > since {
			msg := s.messages[since]
			s.mu.Unlock()
			return msg
		}
		s.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected a new captured email past index %d, got none", since)
	return ""
}

var sixDigitCode = regexp.MustCompile(`\b(\d{6})\b`)

func extractCode(t *testing.T, body string) string {
	t.Helper()
	m := sixDigitCode.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no 6-digit code found in email body: %q", body)
	}
	return m[1]
}

// --- building and running the real identity subprocess ---

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolving repo root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("expected %s to contain go.mod: %v", root, err)
	}
	return root
}

func freeTCPAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("finding a free port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func waitForHealthy(t *testing.T, baseURL string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(baseURL + "/healthz") //nolint:gosec // fixed localhost test address
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("identity subprocess never became healthy within %s", timeout)
}

func randomBase64Key(t *testing.T) string {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("generating random key: %v", err)
	}
	return base64.StdEncoding.EncodeToString(key)
}

// startRealIdentity builds cmd/identity once per test binary run and
// starts it as a real subprocess against real Postgres/NATS/Redis
// containers and smtp (the fake SMTP server), returning its base URL.
func startRealIdentity(t *testing.T, smtpAddr string) string {
	t.Helper()
	root := repoRoot(t)

	binPath := filepath.Join(t.TempDir(), "identity")
	cmd := exec.Command("go", "build", "-o", binPath, "./cmd/identity") //nolint:gosec // fixed "go build" invocation; binPath is our own t.TempDir() path
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/identity failed: %v\n%s", err, out)
	}

	pg := testkit.StartPostgres(t, testkit.WithMigrations(filepath.Join(root, "migrations/identity")))
	natsURL := testkit.StartNATSURL(t)
	redisURL := testkit.StartRedisURL(t)
	httpAddr := freeTCPAddr(t)

	runCmd := exec.Command(binPath) //nolint:gosec // binPath is the binary we just built into our own t.TempDir()
	runCmd.Dir = root
	runCmd.Env = append(os.Environ(),
		"IDENTITY_DATABASE_URL="+pg.DSN()+"&search_path=identity,public",
		"IDENTITY_NATS_URL="+natsURL,
		"IDENTITY_HTTP_ADDR="+httpAddr,
		"IDENTITY_SMTP_ADDR="+smtpAddr,
		"IDENTITY_TOTP_KMS_KEY="+randomBase64Key(t),
		"IDENTITY_SIGNING_KEYS_KMS_KEY="+randomBase64Key(t),
		"IDENTITY_CHALLENGE_SIGNING_KEY="+randomBase64Key(t),
		"IDENTITY_WEBAUTHN_RP_ID=localhost",
		"IDENTITY_WEBAUTHN_ORIGINS=https://localhost",
		"IDENTITY_REDIS_URL="+redisURL,
	)
	runCmd.Stdout = os.Stdout
	runCmd.Stderr = os.Stderr
	if err := runCmd.Start(); err != nil {
		t.Fatalf("starting identity subprocess: %v", err)
	}
	t.Cleanup(func() {
		_ = runCmd.Process.Kill()
		_ = runCmd.Wait()
	})

	baseURL := "http://" + httpAddr
	waitForHealthy(t, baseURL, 20*time.Second)
	return baseURL
}

// --- a tiny HTTP JSON helper, local to this test (identity/api's own
// doJSON isn't importable: it's an unexported test helper in a
// different package) ---

func gwDoJSON(t *testing.T, h http.Handler, method, path, bearer string, body any, idempotencyKey string) (int, map[string]any) {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshaling request body: %v", err)
		}
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var decoded map[string]any
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &decoded)
	}
	return rec.Code, decoded
}

func newTestGateway(t *testing.T, identityBaseURL string, protected []ProtectedRoute) http.Handler {
	t.Helper()
	root := repoRoot(t)
	cfg := Config{
		IdentityBaseURL:     identityBaseURL,
		IdentityOpenAPIPath: filepath.Join(root, "docs/api/identity.yaml"),
	}
	svc, err := New(cfg, testkit.StartRedis(t), clock.Real{}, protected)
	if err != nil {
		t.Fatalf("gateway.New: %v", err)
	}
	return svc.Router()
}

func TestGateway_SignupThroughApiClientAndToken_EndToEnd(t *testing.T) {
	smtp := startFakeSMTPServer(t)
	identityBaseURL := startRealIdentity(t, smtp.Addr())

	// The synthetic protected route this task's own tests use to prove
	// the JWT/scope/env/ip-allowlist pipeline: a real, already-existing
	// anonymous identity route (jwks.json) layered under a requirement
	// no production route carries today (package doc: no scope-gated
	// business endpoint exists yet in this codebase).
	protected := []ProtectedRoute{
		{Method: http.MethodGet, Pattern: "/.well-known/jwks.json", Scope: "reports:read"},
	}
	r := newTestGateway(t, identityBaseURL, protected)

	email := "ada@example.com"

	// --- sign up (pass-through proxy, no auth) ---
	before := smtp.count()
	status, _ := gwDoJSON(t, r, http.MethodPost, "/v1/auth/signups", "",
		map[string]string{"email": email, "full_name": "Ada Lovelace"}, "signup-key")
	if status != http.StatusAccepted {
		t.Fatalf("signup: got status %d", status)
	}
	signupCode := extractCode(t, smtp.waitForNext(t, before))

	// --- verify email ---
	status, _ = gwDoJSON(t, r, http.MethodPost, "/v1/auth/email-verifications", "",
		map[string]string{"email": email, "code": signupCode}, "verify-key")
	if status != http.StatusOK {
		t.Fatalf("verify email: got status %d", status)
	}

	// --- set a password (same mechanism as "forgot password", 1.3) ---
	before = smtp.count()
	status, _ = gwDoJSON(t, r, http.MethodPost, "/v1/auth/password-resets", "",
		map[string]string{"email": email}, "pwreset-request-key")
	if status != http.StatusAccepted {
		t.Fatalf("password reset request: got status %d", status)
	}
	pwResetCode := extractCode(t, smtp.waitForNext(t, before))

	status, _ = gwDoJSON(t, r, http.MethodPost, "/v1/auth/password-reset-confirmations", "",
		map[string]string{"email": email, "code": pwResetCode, "new_password": "a-strong-password"}, "pwreset-confirm-key")
	if status != http.StatusOK {
		t.Fatalf("password reset confirmation: got status %d", status)
	}

	// --- sign in ---
	status, signInBody := gwDoJSON(t, r, http.MethodPost, "/v1/auth/sessions", "",
		map[string]string{"email": email, "password": "a-strong-password"}, "signin-key")
	if status != http.StatusOK {
		t.Fatalf("sign in: got status %d (body: %v)", status, signInBody)
	}
	sessionID, _ := signInBody["session_id"].(string)
	if sessionID == "" {
		t.Fatalf("expected a session_id, got %v", signInBody)
	}

	// --- step up via email (no TOTP enrolled) ---
	before = smtp.count()
	status, _ = gwDoJSON(t, r, http.MethodPost, "/v1/auth/step-up/email", sessionID, nil, "stepup-request-key")
	if status != http.StatusAccepted {
		t.Fatalf("step-up email request: got status %d", status)
	}
	stepUpCode := extractCode(t, smtp.waitForNext(t, before))

	status, _ = gwDoJSON(t, r, http.MethodPost, "/v1/auth/step-up/email/confirmation", sessionID,
		map[string]string{"code": stepUpCode}, "stepup-confirm-key")
	if status != http.StatusOK {
		t.Fatalf("step-up email confirmation: got status %d", status)
	}

	// --- create an API client (step-up-gated) ---
	status, clientBody := gwDoJSON(t, r, http.MethodPost, "/v1/auth/api-clients", sessionID,
		map[string]any{
			"name": "gateway test client", "environment": "sandbox",
			"scopes": []string{"reports:read"}, "ip_allowlist": []string{"203.0.113.0/24"},
		}, "create-client-key")
	if status != http.StatusOK {
		t.Fatalf("create api client: got status %d (body: %v)", status, clientBody)
	}
	clientID, _ := clientBody["id"].(string)
	secretObj, _ := clientBody["secret"].(map[string]any)
	clientSecret, _ := secretObj["secret"].(string)
	if clientID == "" || clientSecret == "" {
		t.Fatalf("expected an id and secret, got %v", clientBody)
	}

	// --- exchange for a JWT (form-encoded, proxied straight through —
	// this path is exempt from the gateway's own idempotency check too,
	// mirroring identity's own exemption) ---
	exchangeToken := func(scope string) (int, map[string]any) {
		form := url.Values{
			"grant_type":    {"client_credentials"},
			"client_id":     {clientID},
			"client_secret": {clientSecret},
		}
		if scope != "" {
			form.Set("scope", scope)
		}
		req := httptest.NewRequest(http.MethodPost, "/v1/oauth/token", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		var decoded map[string]any
		if rec.Body.Len() > 0 {
			_ = json.Unmarshal(rec.Body.Bytes(), &decoded)
		}
		return rec.Code, decoded
	}

	status, tokenBody := exchangeToken("")
	if status != http.StatusOK {
		t.Fatalf("token exchange: got status %d (body: %v)", status, tokenBody)
	}
	accessToken, _ := tokenBody["access_token"].(string)
	if accessToken == "" {
		t.Fatalf("expected an access_token, got %v", tokenBody)
	}

	callProtected := func(bearer, remoteAddr string) (int, map[string]any) {
		req := httptest.NewRequest(http.MethodGet, "/.well-known/jwks.json", nil)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		if remoteAddr != "" {
			req.RemoteAddr = remoteAddr
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		var decoded map[string]any
		if rec.Body.Len() > 0 {
			_ = json.Unmarshal(rec.Body.Bytes(), &decoded)
		}
		return rec.Code, decoded
	}

	// --- the full happy path: valid JWT, right scope, allowed IP ---
	status, protectedBody := callProtected(accessToken, "203.0.113.42:1234")
	if status != http.StatusOK {
		t.Fatalf("protected route: got status %d (body: %v)", status, protectedBody)
	}
	if _, ok := protectedBody["keys"]; !ok {
		t.Fatalf("expected the real jwks response to be proxied through, got %v", protectedBody)
	}

	// --- no Authorization header at all ---
	status, body := callProtected("", "203.0.113.42:1234")
	if status != http.StatusUnauthorized || body["code"] != "AUTH-4010" {
		t.Fatalf("missing bearer: got status %d, code %v", status, body["code"])
	}

	// --- valid JWT but narrowed to a scope the route doesn't require,
	// not the one it does ---
	_, narrowTokenBody := exchangeToken("reports:read")
	narrowToken, _ := narrowTokenBody["access_token"].(string)
	status, body = callProtected(narrowToken, "203.0.113.42:1234")
	if status != http.StatusOK {
		t.Fatalf("narrowed-but-sufficient scope: got status %d (body: %v)", status, body)
	}

	// --- caller IP outside the credential's own ip_allowlist claim ---
	status, body = callProtected(accessToken, "198.51.100.9:1234")
	if status != http.StatusForbidden || body["code"] != "AUTH-4015" {
		t.Fatalf("disallowed IP: got status %d, code %v", status, body["code"])
	}

	// --- revoke the client: a NEW token exchange fails. An
	// already-issued token keeps working until its own 1-hour expiry
	// (task 1.8's own documented behavior — revoking doesn't recall
	// it), so this deliberately checks the exchange, not the token
	// that's already out. ---
	status, _ = gwDoJSON(t, r, http.MethodDelete, "/v1/auth/api-clients/"+clientID, sessionID, nil, "revoke-key")
	if status != http.StatusNoContent {
		t.Fatalf("revoke api client: got status %d", status)
	}
	status, errBody := exchangeToken("")
	if status != http.StatusUnauthorized {
		t.Fatalf("token exchange after revoke: got status %d (body: %v)", status, errBody)
	}
	if errBody["error"] != "invalid_client" {
		t.Fatalf("got oauth error %v, want invalid_client", errBody["error"])
	}
}

func TestGateway_MissingScope_Returns403(t *testing.T) {
	smtp := startFakeSMTPServer(t)
	identityBaseURL := startRealIdentity(t, smtp.Addr())
	protected := []ProtectedRoute{
		{Method: http.MethodGet, Pattern: "/.well-known/jwks.json", Scope: "sms:send"},
	}
	r := newTestGateway(t, identityBaseURL, protected)

	_, sessionID := signUpVerifySetPasswordAndSignIn(t, r, smtp, "bob@example.com")
	stepUp(t, r, smtp, sessionID)
	clientID, clientSecret := createApiClient(t, r, sessionID, []string{"reports:read"}, nil)

	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {clientID}, "client_secret": {clientSecret}}
	req := httptest.NewRequest(http.MethodPost, "/v1/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	var tokenBody map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &tokenBody)
	accessToken, _ := tokenBody["access_token"].(string)
	if accessToken == "" {
		t.Fatalf("expected an access token, got %v", tokenBody)
	}

	req2 := httptest.NewRequest(http.MethodGet, "/.well-known/jwks.json", nil)
	req2.Header.Set("Authorization", "Bearer "+accessToken)
	req2.RemoteAddr = "203.0.113.1:1"
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, req2)
	var body map[string]any
	_ = json.Unmarshal(rec2.Body.Bytes(), &body)
	if rec2.Code != http.StatusForbidden || body["code"] != "AUTH-4014" {
		t.Fatalf("got status %d, code %v, want 403 AUTH-4014", rec2.Code, body["code"])
	}
}

func TestGateway_SandboxCredentialCannotReachLiveOnlyRoute(t *testing.T) {
	smtp := startFakeSMTPServer(t)
	identityBaseURL := startRealIdentity(t, smtp.Addr())
	protected := []ProtectedRoute{
		{Method: http.MethodGet, Pattern: "/.well-known/jwks.json", LiveOnly: true},
	}
	r := newTestGateway(t, identityBaseURL, protected)

	_, sessionID := signUpVerifySetPasswordAndSignIn(t, r, smtp, "carol@example.com")
	stepUp(t, r, smtp, sessionID)
	clientID, clientSecret := createApiClient(t, r, sessionID, []string{"reports:read"}, nil) // sandbox, 1.7's own default

	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {clientID}, "client_secret": {clientSecret}}
	req := httptest.NewRequest(http.MethodPost, "/v1/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	var tokenBody map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &tokenBody)
	accessToken, _ := tokenBody["access_token"].(string)

	req2 := httptest.NewRequest(http.MethodGet, "/.well-known/jwks.json", nil)
	req2.Header.Set("Authorization", "Bearer "+accessToken)
	req2.RemoteAddr = "203.0.113.1:1"
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, req2)
	var body map[string]any
	_ = json.Unmarshal(rec2.Body.Bytes(), &body)
	if rec2.Code != http.StatusForbidden || body["code"] != "AUTH-4016" {
		t.Fatalf("got status %d, code %v, want 403 AUTH-4016", rec2.Code, body["code"])
	}
}

// --- shared onboarding helpers for the smaller, targeted tests above
// (the first, bigger test spells every step out inline, since it's
// the one place proving the whole mechanism end to end matters most
// as plainly-readable steps) ---

func signUpVerifySetPasswordAndSignIn(t *testing.T, r http.Handler, smtp *fakeSMTPServer, email string) (string, string) {
	t.Helper()

	before := smtp.count()
	status, _ := gwDoJSON(t, r, http.MethodPost, "/v1/auth/signups", "",
		map[string]string{"email": email, "full_name": "Test User"}, fmt.Sprintf("signup-%s", email))
	if status != http.StatusAccepted {
		t.Fatalf("signup: got status %d", status)
	}
	code := extractCode(t, smtp.waitForNext(t, before))
	status, _ = gwDoJSON(t, r, http.MethodPost, "/v1/auth/email-verifications", "",
		map[string]string{"email": email, "code": code}, "verify-"+email)
	if status != http.StatusOK {
		t.Fatalf("verify: got status %d", status)
	}

	before = smtp.count()
	status, _ = gwDoJSON(t, r, http.MethodPost, "/v1/auth/password-resets", "",
		map[string]string{"email": email}, "pwreset-"+email)
	if status != http.StatusAccepted {
		t.Fatalf("password reset request: got status %d", status)
	}
	code = extractCode(t, smtp.waitForNext(t, before))
	status, _ = gwDoJSON(t, r, http.MethodPost, "/v1/auth/password-reset-confirmations", "",
		map[string]string{"email": email, "code": code, "new_password": "a-strong-password"}, "pwconfirm-"+email)
	if status != http.StatusOK {
		t.Fatalf("password reset confirmation: got status %d", status)
	}

	status, signInBody := gwDoJSON(t, r, http.MethodPost, "/v1/auth/sessions", "",
		map[string]string{"email": email, "password": "a-strong-password"}, "signin-"+email)
	if status != http.StatusOK {
		t.Fatalf("sign in: got status %d", status)
	}
	sessionID, _ := signInBody["session_id"].(string)
	if sessionID == "" {
		t.Fatalf("expected a session_id, got %v", signInBody)
	}
	return email, sessionID
}

func stepUp(t *testing.T, r http.Handler, smtp *fakeSMTPServer, sessionID string) {
	t.Helper()
	before := smtp.count()
	status, _ := gwDoJSON(t, r, http.MethodPost, "/v1/auth/step-up/email", sessionID, nil, "stepup-req-"+sessionID)
	if status != http.StatusAccepted {
		t.Fatalf("step-up request: got status %d", status)
	}
	code := extractCode(t, smtp.waitForNext(t, before))
	status, _ = gwDoJSON(t, r, http.MethodPost, "/v1/auth/step-up/email/confirmation", sessionID,
		map[string]string{"code": code}, "stepup-confirm-"+sessionID)
	if status != http.StatusOK {
		t.Fatalf("step-up confirmation: got status %d", status)
	}
}

func createApiClient(t *testing.T, r http.Handler, sessionID string, scopes, ipAllowlist []string) (string, string) {
	t.Helper()
	body := map[string]any{"name": "test client", "environment": "sandbox", "scopes": scopes}
	if ipAllowlist != nil {
		body["ip_allowlist"] = ipAllowlist
	}
	status, clientBody := gwDoJSON(t, r, http.MethodPost, "/v1/auth/api-clients", sessionID, body, "create-client-"+sessionID)
	if status != http.StatusOK {
		t.Fatalf("create api client: got status %d (body: %v)", status, clientBody)
	}
	clientID, _ := clientBody["id"].(string)
	secretObj, _ := clientBody["secret"].(map[string]any)
	clientSecret, _ := secretObj["secret"].(string)
	if clientID == "" || clientSecret == "" {
		t.Fatalf("expected an id and secret, got %v", clientBody)
	}
	return clientID, clientSecret
}
