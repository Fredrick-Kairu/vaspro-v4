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
	"io"
	"log/slog"
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

	"github.com/google/uuid"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"

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
	return startRealIdentityWithLogs(t, smtpAddr, nil)
}

// startRealIdentityWithLogs is startRealIdentity that also copies the
// subprocess's stdout and stderr into logs, for a test that needs to
// read back what identity logged. extraEnv is appended to the
// subprocess's environment (task 1.16's e2e sets IDENTITY_GOOGLE_*;
// an e2e with real accounts sets IDENTITY_ACCOUNTS_GRPC_ADDR, otherwise
// identity asks an allow-all fake, task 2.2c).
func startRealIdentityWithLogs(t *testing.T, smtpAddr string, logs io.Writer, extraEnv ...string) string {
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
	grpcAddr := envValue(extraEnv, "IDENTITY_GRPC_ADDR")
	if grpcAddr == "" {
		grpcAddr = freeTCPAddr(t)
	}

	runCmd := exec.Command(binPath) //nolint:gosec // binPath is the binary we just built into our own t.TempDir()
	runCmd.Dir = root
	runCmd.Env = append(os.Environ(),
		"IDENTITY_DATABASE_URL="+pg.DSN()+"&search_path=identity,public",
		"IDENTITY_NATS_URL="+natsURL,
		"IDENTITY_HTTP_ADDR="+httpAddr,
		"IDENTITY_GRPC_ADDR="+grpcAddr,
		"IDENTITY_SMTP_ADDR="+smtpAddr,
		"IDENTITY_TOTP_KMS_KEY="+randomBase64Key(t),
		"IDENTITY_SIGNING_KEYS_KMS_KEY="+randomBase64Key(t),
		"IDENTITY_CHALLENGE_SIGNING_KEY="+randomBase64Key(t),
		"IDENTITY_WEBAUTHN_RP_ID=localhost",
		"IDENTITY_WEBAUTHN_ORIGINS=https://localhost",
		"IDENTITY_REDIS_URL="+redisURL,
	)
	if !hasEnv(extraEnv, "IDENTITY_ACCOUNTS_GRPC_ADDR") {
		runCmd.Env = append(runCmd.Env, "IDENTITY_ACCOUNTS_GRPC_ADDR="+startAllowAllAccountsGRPC(t))
	}
	runCmd.Env = append(runCmd.Env, extraEnv...)
	runCmd.Stdout = os.Stdout
	runCmd.Stderr = os.Stderr
	if logs != nil {
		runCmd.Stdout = io.MultiWriter(os.Stdout, logs)
		runCmd.Stderr = io.MultiWriter(os.Stderr, logs)
	}
	if err := runCmd.Start(); err != nil {
		t.Fatalf("starting identity subprocess: %v", err)
	}
	t.Cleanup(func() {
		_ = runCmd.Process.Kill()
		_ = runCmd.Wait()
	})

	baseURL := "http://" + httpAddr
	waitForHealthy(t, baseURL, 20*time.Second)
	identityGRPCAddrs.Store(baseURL, grpcAddr)
	return baseURL
}

func hasEnv(env []string, key string) bool { return envValue(env, key) != "" }

// envValue is key's value in env, or "".
func envValue(env []string, key string) string {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, key+"="); ok {
			return v
		}
	}
	return ""
}

// identityGRPCAddrs maps each identity subprocess's base URL to its gRPC
// address, so newTestGateway can check sessions against it.
var identityGRPCAddrs sync.Map

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
	return newTestGatewayWithLogger(t, identityBaseURL, protected, slog.Default())
}

// newTestGatewayWithLogger is newTestGateway with the gateway's logger
// supplied, for a test that needs to read back what the gateway logged.
func newTestGatewayWithLogger(t *testing.T, identityBaseURL string, protected []ProtectedRoute, logger *slog.Logger) http.Handler {
	t.Helper()
	return newTestGatewayWithAccounts(t, identityBaseURL, "http://127.0.0.1:1", protected, logger)
}

// newTestGatewayWithAccounts is newTestGatewayWithLogger with a real
// accounts backend; sessions are checked against the identity
// subprocess's gRPC API.
func newTestGatewayWithAccounts(t *testing.T, identityBaseURL, accountsBaseURL string, protected []ProtectedRoute, logger *slog.Logger) http.Handler {
	t.Helper()
	root := repoRoot(t)
	grpcAddr, ok := identityGRPCAddrs.Load(identityBaseURL)
	if !ok {
		t.Fatalf("no gRPC address recorded for identity at %s", identityBaseURL)
	}
	sessions, err := DialGRPCSessions(grpcAddr.(string))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sessions.Close() })
	cfg := Config{
		IdentityBaseURL:     identityBaseURL,
		IdentityOpenAPIPath: filepath.Join(root, "docs/api/identity.yaml"),
		AccountsBaseURL:     accountsBaseURL,
		AccountsOpenAPIPath: filepath.Join(root, "docs/api/accounts.yaml"),
	}
	svc, err := New(cfg, testkit.StartRedis(t), clock.Real{}, logger, protected, sessions)
	if err != nil {
		t.Fatalf("gateway.New: %v", err)
	}
	return svc.Router()
}

// syncBuffer is a bytes.Buffer safe to write from the gateway's
// handlers and the identity subprocess's output copiers at once.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// totpCodes hands out TOTP codes the way identity accepts them: each
// from a 30-second time step later than any used before (RFC 6238 §5.2
// replay protection), taken oldest-first from the current step and its
// neighbours (identity accepts ±1 step of drift) — so three codes fit
// in one window without waiting for the clock.
type totpCodes struct {
	secret   string
	lastStep int64
	issued   []string
}

func (c *totpCodes) next(t *testing.T) string {
	t.Helper()
	now := time.Now().Unix() / 30
	step := max(now-1, c.lastStep+1)
	if step > now+1 {
		t.Fatal("no unused TOTP time step left in the drift window")
	}
	code, err := totp.GenerateCodeCustom(c.secret, time.Unix(step*30, 0), totp.ValidateOpts{
		Period: 30, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1,
	})
	if err != nil {
		t.Fatalf("generating a TOTP code: %v", err)
	}
	c.lastStep = step
	c.issued = append(c.issued, code)
	return code
}

// TestGateway_SignupThroughApiClientAndToken_EndToEnd is Phase 1's
// exit-gate e2e scenario, step for step: sign up → verify email →
// enrol TOTP → sign in → create API client → get a token → call a
// protected endpoint → revoke the client → the call fails. It also
// carries the gate's "no secret, password or token in any log line"
// check: every secret the flow handles is grepped for in what both
// the gateway and the identity subprocess logged.
func TestGateway_SignupThroughApiClientAndToken_EndToEnd(t *testing.T) {
	smtp := startFakeSMTPServer(t)
	var identityLogs, gatewayLogs syncBuffer
	identityGRPC := freeTCPAddr(t)
	accountsBaseURL, accountsGRPC := startRealAccounts(t, identityGRPC)
	identityBaseURL := startRealIdentityWithLogs(t, smtp.Addr(), &identityLogs,
		"IDENTITY_ACCOUNTS_GRPC_ADDR="+accountsGRPC, "IDENTITY_GRPC_ADDR="+identityGRPC)

	// The synthetic protected route this task's own tests use to prove
	// the JWT/scope/env/ip-allowlist pipeline: a real, already-existing
	// anonymous identity route (jwks.json) layered under a requirement
	// no production route carries today (package doc: no scope-gated
	// business endpoint exists yet in this codebase).
	protected := []ProtectedRoute{
		{Method: http.MethodGet, Pattern: "/.well-known/jwks.json", Scope: "reports:read"},
	}
	// JSON, minus the timestamp: its microsecond field is a run of six
	// digits that could match a 6-digit code by chance.
	gatewayLogger := slog.New(slog.NewJSONHandler(&gatewayLogs, &slog.HandlerOptions{
		Level: slog.LevelDebug,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	}))
	r := newTestGatewayWithAccounts(t, identityBaseURL, accountsBaseURL, protected, gatewayLogger)

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
	sessionToken, _ := signInBody["session_token"].(string)
	if sessionToken == "" {
		t.Fatalf("expected a session_token, got %v", signInBody)
	}

	// --- step up via email (no TOTP enrolled) ---
	before = smtp.count()
	status, _ = gwDoJSON(t, r, http.MethodPost, "/v1/auth/step-up/email", sessionToken, nil, "stepup-request-key")
	if status != http.StatusAccepted {
		t.Fatalf("step-up email request: got status %d", status)
	}
	stepUpCode := extractCode(t, smtp.waitForNext(t, before))

	status, _ = gwDoJSON(t, r, http.MethodPost, "/v1/auth/step-up/email/confirmation", sessionToken,
		map[string]string{"code": stepUpCode}, "stepup-confirm-key")
	if status != http.StatusOK {
		t.Fatalf("step-up email confirmation: got status %d", status)
	}

	// --- enrol TOTP (step-up-gated; the email step-up above covers it,
	// since an account with no TOTP yet has no other way to step up) ---
	status, enrolBody := gwDoJSON(t, r, http.MethodPost, "/v1/auth/totp-enrollments", sessionToken, nil, "totp-enrol-key")
	if status != http.StatusOK {
		t.Fatalf("totp enrollment: got status %d (body: %v)", status, enrolBody)
	}
	otpURL, _ := enrolBody["url"].(string)
	otpKey, err := otp.NewKeyFromURL(otpURL)
	if err != nil {
		t.Fatalf("parsing the otpauth url: %v", err)
	}
	totpSecret := otpKey.Secret()
	var recoveryCodes []string
	if raw, ok := enrolBody["recovery_codes"].([]any); ok {
		for _, c := range raw {
			if s, ok := c.(string); ok {
				recoveryCodes = append(recoveryCodes, s)
			}
		}
	}
	if totpSecret == "" || len(recoveryCodes) == 0 {
		t.Fatalf("expected a TOTP secret and recovery codes, got %v", enrolBody)
	}
	codes := &totpCodes{secret: totpSecret}

	status, _ = gwDoJSON(t, r, http.MethodPost, "/v1/auth/totp-enrollments/confirmation", sessionToken,
		map[string]string{"code": codes.next(t)}, "totp-confirm-key")
	if status != http.StatusOK {
		t.Fatalf("totp enrollment confirmation: got status %d", status)
	}

	// --- sign out everywhere, then sign in again: TOTP is now required ---
	firstSessionToken := sessionToken
	status, _ = gwDoJSON(t, r, http.MethodDelete, "/v1/auth/sessions", sessionToken, nil, "signout-key")
	if status != http.StatusNoContent {
		t.Fatalf("sign out everywhere: got status %d", status)
	}
	status, body := gwDoJSON(t, r, http.MethodGet, "/v1/auth/sessions", firstSessionToken, nil, "")
	if status != http.StatusUnauthorized || body["code"] != "AUTH-4010" {
		t.Fatalf("signed-out session: got status %d, code %v, want 401 AUTH-4010", status, body["code"])
	}

	status, signInBody = gwDoJSON(t, r, http.MethodPost, "/v1/auth/sessions", "",
		map[string]string{"email": email, "password": "a-strong-password"}, "signin-totp-key")
	if status != http.StatusOK {
		t.Fatalf("sign in with TOTP enrolled: got status %d (body: %v)", status, signInBody)
	}
	challenge, _ := signInBody["challenge"].(string)
	if challenge == "" || signInBody["session_token"] != nil {
		t.Fatalf("expected a TOTP challenge and no session yet, got %v", signInBody)
	}
	status, challengeBody := gwDoJSON(t, r, http.MethodPost, "/v1/auth/totp-challenges/"+challenge+"/verification", "",
		map[string]string{"code": codes.next(t)}, "totp-challenge-key")
	if status != http.StatusOK {
		t.Fatalf("totp challenge: got status %d (body: %v)", status, challengeBody)
	}
	sessionToken, _ = challengeBody["session_token"].(string)
	if sessionToken == "" {
		t.Fatalf("expected a session_token after the TOTP challenge, got %v", challengeBody)
	}

	// --- step up with TOTP: the new session needs its own fresh step-up ---
	status, _ = gwDoJSON(t, r, http.MethodPost, "/v1/auth/step-up/totp", sessionToken,
		map[string]string{"code": codes.next(t)}, "stepup-totp-key")
	if status != http.StatusOK {
		t.Fatalf("step-up totp: got status %d", status)
	}

	// --- create the client account and select it (task 2.2c): the API
	// client belongs to the account, not the user ---
	status, account := gwDoJSON(t, r, http.MethodPost, "/v1/accounts/tenants", sessionToken,
		map[string]string{"display_name": "Ada's agency", "country_code": "KE"}, "create-account-key")
	if status != http.StatusCreated {
		t.Fatalf("create account: got status %d (body: %v)", status, account)
	}
	accountID, _ := account["id"].(string)
	status, selected := gwDoJSON(t, r, http.MethodPut, "/v1/auth/sessions/current/tenant", sessionToken,
		map[string]string{"tenant_id": accountID}, "select-account-key")
	if status != http.StatusOK {
		t.Fatalf("select account: got status %d (body: %v)", status, selected)
	}

	// --- create an API client (step-up-gated) ---
	status, clientBody := gwDoJSON(t, r, http.MethodPost, "/v1/auth/api-clients", sessionToken,
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
	if tid := unverifiedClaim(t, accessToken, "tid"); tid != accountID {
		t.Fatalf("the token's tid is %v, want the selected account %s", tid, accountID)
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
	status, body = callProtected("", "203.0.113.42:1234")
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

	// --- an SMPP account too, so its generated password is one more
	// secret the log check below looks for ---
	status, smppBody := gwDoJSON(t, r, http.MethodPost, "/v1/auth/smpp-accounts", sessionToken,
		map[string]any{
			"system_id": "gwtest01", "environment": "sandbox",
			"ip_allowlist": []string{"203.0.113.0/24"}, "max_binds": 1, "max_tps": 10,
		}, "create-smpp-key")
	if status != http.StatusOK {
		t.Fatalf("create smpp account: got status %d (body: %v)", status, smppBody)
	}
	smppPassword, _ := smppBody["password"].(string)

	// --- revoke the client: a NEW token exchange fails. An
	// already-issued token keeps working until its own 1-hour expiry
	// (task 1.8's own documented behavior — revoking doesn't recall
	// it), so this deliberately checks the exchange, not the token
	// that's already out. ---
	status, _ = gwDoJSON(t, r, http.MethodDelete, "/v1/auth/api-clients/"+clientID, sessionToken, nil, "revoke-key")
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

	// --- exit gate: no secret, password or token in any log line ---
	secrets := map[string]string{
		"password":            "a-strong-password",
		"signup code":         signupCode,
		"password reset code": pwResetCode,
		"step-up email code":  stepUpCode,
		"totp secret":         totpSecret,
		"first session token": firstSessionToken,
		"session token":       sessionToken,
		"totp challenge":      challenge,
		"client secret":       clientSecret,
		"access token":        accessToken,
		"narrowed token":      narrowToken,
		"smpp password":       smppPassword,
	}
	for i, c := range codes.issued {
		secrets[fmt.Sprintf("totp code %d", i)] = c
	}
	for i, c := range recoveryCodes {
		secrets[fmt.Sprintf("recovery code %d", i)] = c
	}
	logs := map[string]string{"gateway": gatewayLogs.String(), "identity": identityLogs.String()}
	if !strings.Contains(logs["gateway"], `"msg":"gateway: request"`) {
		t.Fatalf("expected the gateway's audit lines in its log, got %q", logs["gateway"])
	}
	for name, secret := range secrets {
		if secret == "" {
			t.Fatalf("secret %q is empty: the flow above didn't capture it", name)
		}
		for source, text := range logs {
			if strings.Contains(text, secret) {
				t.Errorf("the %s log contains the %s", source, name)
			}
		}
	}
}

func TestGateway_MissingScope_Returns403(t *testing.T) {
	smtp := startFakeSMTPServer(t)
	identityBaseURL := startRealIdentity(t, smtp.Addr())
	protected := []ProtectedRoute{
		{Method: http.MethodGet, Pattern: "/.well-known/jwks.json", Scope: "sms:send"},
	}
	r := newTestGateway(t, identityBaseURL, protected)

	_, sessionToken := signUpVerifySetPasswordAndSignIn(t, r, smtp, "bob@example.com")
	stepUp(t, r, smtp, sessionToken)
	clientID, clientSecret := createApiClient(t, r, sessionToken, []string{"reports:read"}, nil)

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

	_, sessionToken := signUpVerifySetPasswordAndSignIn(t, r, smtp, "carol@example.com")
	stepUp(t, r, smtp, sessionToken)
	clientID, clientSecret := createApiClient(t, r, sessionToken, []string{"reports:read"}, nil) // sandbox, 1.7's own default

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
	sessionToken, _ := signInBody["session_token"].(string)
	if sessionToken == "" {
		t.Fatalf("expected a session_token, got %v", signInBody)
	}
	return email, sessionToken
}

func stepUp(t *testing.T, r http.Handler, smtp *fakeSMTPServer, sessionToken string) {
	t.Helper()
	before := smtp.count()
	status, _ := gwDoJSON(t, r, http.MethodPost, "/v1/auth/step-up/email", sessionToken, nil, "stepup-req-"+sessionToken)
	if status != http.StatusAccepted {
		t.Fatalf("step-up request: got status %d", status)
	}
	code := extractCode(t, smtp.waitForNext(t, before))
	status, _ = gwDoJSON(t, r, http.MethodPost, "/v1/auth/step-up/email/confirmation", sessionToken,
		map[string]string{"code": code}, "stepup-confirm-"+sessionToken)
	if status != http.StatusOK {
		t.Fatalf("step-up confirmation: got status %d", status)
	}
}

// unverifiedClaim reads one claim from a JWT's payload without checking
// its signature; the gateway's own checks verify it.
func unverifiedClaim(t *testing.T, token, name string) any {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("not a JWT: %d parts", len(parts))
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}
	return claims[name]
}

// selectAnyAccount has the session select a new account ID, which the
// allow-all accounts fake lets anyone into (task 2.2c).
func selectAnyAccount(t *testing.T, r http.Handler, sessionToken string) {
	t.Helper()
	tenantID := uuid.NewString()
	status, body := gwDoJSON(t, r, http.MethodPut, "/v1/auth/sessions/current/tenant", sessionToken,
		map[string]string{"tenant_id": tenantID}, "select-"+tenantID)
	if status != http.StatusOK {
		t.Fatalf("selecting an account: got %d %v", status, body)
	}
}

func createApiClient(t *testing.T, r http.Handler, sessionToken string, scopes, ipAllowlist []string) (string, string) {
	t.Helper()
	selectAnyAccount(t, r, sessionToken)
	body := map[string]any{"name": "test client", "environment": "sandbox", "scopes": scopes}
	if ipAllowlist != nil {
		body["ip_allowlist"] = ipAllowlist
	}
	status, clientBody := gwDoJSON(t, r, http.MethodPost, "/v1/auth/api-clients", sessionToken, body, "create-client-"+sessionToken)
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
