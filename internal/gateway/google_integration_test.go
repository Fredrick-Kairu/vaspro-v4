//go:build integration

package gateway

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/pquerna/otp"

	fakeoidc "github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/oidc"
)

// TestGateway_GoogleSignUpThroughTOTPSignIn_EndToEnd is task 1.16's
// e2e, step for step from the Phase 1 test list: sign up with Google →
// enrol TOTP → sign out → sign in with Google → TOTP → a session with
// auth_method google. Everything goes through the gateway to a real
// cmd/identity subprocess, with tools/fakes/oidc standing in for
// Google. Like the main e2e, it greps both processes' logs for every
// secret the flow handled.
func TestGateway_GoogleSignUpThroughTOTPSignIn_EndToEnd(t *testing.T) {
	const clientID = "vaspro-e2e-client"
	idp, err := fakeoidc.New(clientID)
	if err != nil {
		t.Fatalf("starting fake OIDC provider: %v", err)
	}
	t.Cleanup(idp.Close)

	smtp := startFakeSMTPServer(t)
	var identityLogs, gatewayLogs syncBuffer
	identityBaseURL := startRealIdentityWithLogs(t, smtp.Addr(), &identityLogs,
		"IDENTITY_GOOGLE_CLIENT_ID="+clientID,
		"IDENTITY_GOOGLE_CLIENT_SECRET=e2e-client-secret",
		"IDENTITY_GOOGLE_REDIRECT_URL=https://localhost/auth/google/callback",
		"IDENTITY_GOOGLE_ISSUER="+idp.Issuer(),
	)
	gatewayLogger := slog.New(slog.NewJSONHandler(&gatewayLogs, &slog.HandlerOptions{
		Level: slog.LevelDebug,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	}))
	r := newTestGatewayWithLogger(t, identityBaseURL, nil, gatewayLogger)

	user := fakeoidc.User{Subject: "google-sub-e2e", Email: "grace@example.com", EmailVerified: true, Name: "Grace Hopper"}
	var googleCodes, states []string

	// googleStep runs one Google round trip: start, "sign in" at the
	// fake provider, callback.
	googleStep := func(key string) map[string]any {
		t.Helper()
		status, auth := gwDoJSON(t, r, http.MethodPost, "/v1/auth/google/authorizations", "", nil, "google-auth-"+key)
		if status != http.StatusOK {
			t.Fatalf("google authorization: got %d %v", status, auth)
		}
		authURL, _ := auth["authorization_url"].(string)
		code, state, err := idp.Authorize(authURL, user, fakeoidc.Tamper{})
		if err != nil {
			t.Fatalf("Authorize: %v", err)
		}
		if state != auth["state"] {
			t.Fatalf("state in the URL %q, want %v", state, auth["state"])
		}
		googleCodes = append(googleCodes, code)
		states = append(states, state)
		status, body := gwDoJSON(t, r, http.MethodPost, "/v1/auth/google/sessions", "",
			map[string]string{"code": code, "state": state}, "google-session-"+key)
		if status != http.StatusOK {
			t.Fatalf("google callback: got %d %v", status, body)
		}
		if body["session_token"] != nil {
			t.Fatalf("Google alone must never create a session, got %v", body)
		}
		return body
	}

	// --- sign up with Google: no second factor yet, so an enrolment token ---
	signUp := googleStep("signup")
	enrollmentToken, _ := signUp["enrollment_token"].(string)
	if enrollmentToken == "" {
		t.Fatalf("expected an enrollment_token, got %v", signUp)
	}

	// --- enrol TOTP; confirming it creates the first session ---
	status, enrolBody := gwDoJSON(t, r, http.MethodPost, "/v1/auth/google/enrollments/totp", "",
		map[string]string{"enrollment_token": enrollmentToken}, "google-totp-enrol")
	if status != http.StatusOK {
		t.Fatalf("google totp enrollment: got %d %v", status, enrolBody)
	}
	otpURL, _ := enrolBody["url"].(string)
	otpKey, err := otp.NewKeyFromURL(otpURL)
	if err != nil {
		t.Fatalf("parsing the otpauth url: %v", err)
	}
	var recoveryCodes []string
	if raw, ok := enrolBody["recovery_codes"].([]any); ok {
		for _, c := range raw {
			if s, ok := c.(string); ok {
				recoveryCodes = append(recoveryCodes, s)
			}
		}
	}
	codes := &totpCodes{secret: otpKey.Secret()}
	status, firstSession := gwDoJSON(t, r, http.MethodPost, "/v1/auth/google/enrollments/totp/confirmation", "",
		map[string]string{"enrollment_token": enrollmentToken, "code": codes.next(t)}, "google-totp-confirm")
	firstSessionToken, _ := firstSession["session_token"].(string)
	if status != http.StatusOK || firstSessionToken == "" {
		t.Fatalf("google totp confirmation: got %d %v, want a session", status, firstSession)
	}

	// --- sign out everywhere ---
	status, _ = gwDoJSON(t, r, http.MethodDelete, "/v1/auth/sessions", firstSessionToken, nil, "google-signout")
	if status != http.StatusNoContent {
		t.Fatalf("sign out everywhere: got status %d", status)
	}

	// --- sign in with Google again: a TOTP challenge, not a session ---
	signIn := googleStep("signin")
	challenge, _ := signIn["challenge"].(string)
	if challenge == "" || signIn["enrollment_token"] != nil {
		t.Fatalf("expected a challenge only, got %v", signIn)
	}
	status, verified := gwDoJSON(t, r, http.MethodPost, "/v1/auth/totp-challenges/"+challenge+"/verification", "",
		map[string]string{"code": codes.next(t)}, "google-totp-challenge")
	sessionToken, _ := verified["session_token"].(string)
	if status != http.StatusOK || sessionToken == "" {
		t.Fatalf("totp challenge: got %d %v, want a session", status, verified)
	}

	// --- the session records that it signed in with Google ---
	status, list := gwDoJSON(t, r, http.MethodGet, "/v1/auth/sessions", sessionToken, nil, "")
	if status != http.StatusOK {
		t.Fatalf("list sessions: got %d %v", status, list)
	}
	sessions, _ := list["sessions"].([]any)
	if len(sessions) != 1 {
		t.Fatalf("got %d active sessions, want 1: %v", len(sessions), list)
	}
	current, _ := sessions[0].(map[string]any)
	if current["auth_method"] != "google" || current["current"] != true {
		t.Fatalf("got session %v, want the current session with auth_method google", current)
	}

	// --- no secret, code or token in any log line ---
	secrets := map[string]string{
		"enrollment token":    enrollmentToken,
		"totp secret":         codes.secret,
		"first session token": firstSessionToken,
		"session token":       sessionToken,
		"totp challenge":      challenge,
	}
	for i, c := range googleCodes {
		secrets[fmt.Sprintf("google code %d", i)] = c
	}
	for i, s := range states {
		secrets[fmt.Sprintf("google state %d", i)] = s
	}
	for i, tok := range idp.IDTokens {
		secrets[fmt.Sprintf("google ID token %d", i)] = tok
	}
	for i, c := range codes.issued {
		secrets[fmt.Sprintf("totp code %d", i)] = c
	}
	for i, c := range recoveryCodes {
		secrets[fmt.Sprintf("recovery code %d", i)] = c
	}
	if len(idp.IDTokens) != 2 || len(recoveryCodes) == 0 {
		t.Fatalf("expected 2 ID tokens and some recovery codes, got %d and %d", len(idp.IDTokens), len(recoveryCodes))
	}
	logs := map[string]string{"gateway": gatewayLogs.String(), "identity": identityLogs.String()}
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
