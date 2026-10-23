//go:build integration

package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/email"
)

// doJSON sends method/path with an optional JSON body and bearer token,
// returning the decoded response and status code.
func doJSON(t *testing.T, r http.Handler, method, path, bearer string, body any, idempotencyKey string) (int, map[string]any) {
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
	r.ServeHTTP(rec, req)

	var decoded map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
			t.Fatalf("decoding response body %q: %v", rec.Body.String(), err)
		}
	}
	return rec.Code, decoded
}

// signUpVerifyAndSetPassword drives the full anonymous onboarding flow
// over HTTP and returns the account's email. Every Idempotency-Key is
// suffixed with rawEmail — every pre-auth call in this flow shares one
// anonymous tenant (uuid.Nil, no gateway/tenant context yet), so a
// bare "signup-key" collides across two different calls to this helper
// in the same test (two different bodies claiming the same key is
// VAL-4001, found by testing once a test needed two accounts).
// Collision-free for every existing single-account call site too,
// since rawEmail was already distinct there.
func signUpVerifyAndSetPassword(t *testing.T, r http.Handler, sender *email.Sender, rawEmail, password string) {
	t.Helper()

	code, _ := doJSON(t, r, http.MethodPost, "/v1/auth/signups", "",
		map[string]string{"email": rawEmail, "full_name": "Ada Lovelace"}, "signup-key-"+rawEmail)
	if code != http.StatusAccepted {
		t.Fatalf("signup: got status %d", code)
	}
	sent, ok := sender.LastOfKind(email.KindVerification)
	if !ok {
		t.Fatal("expected a verification email")
	}
	code, _ = doJSON(t, r, http.MethodPost, "/v1/auth/email-verifications", "",
		map[string]string{"email": rawEmail, "code": sent.Code}, "verify-key-"+rawEmail)
	if code != http.StatusOK {
		t.Fatalf("email verification: got status %d", code)
	}

	code, _ = doJSON(t, r, http.MethodPost, "/v1/auth/password-resets", "",
		map[string]string{"email": rawEmail}, "pwreset-key-"+rawEmail)
	if code != http.StatusAccepted {
		t.Fatalf("password reset request: got status %d", code)
	}
	sent, ok = sender.LastOfKind(email.KindPasswordReset)
	if !ok {
		t.Fatal("expected a password reset email")
	}
	code, _ = doJSON(t, r, http.MethodPost, "/v1/auth/password-reset-confirmations", "",
		map[string]string{"email": rawEmail, "code": sent.Code, "new_password": password}, "pwconfirm-key-"+rawEmail)
	if code != http.StatusOK {
		t.Fatalf("password reset confirmation: got status %d", code)
	}
}

func TestStepUp_GatesTotpEnrollmentUntilSteppedUp(t *testing.T) {
	r, sender := newTestRouter(t)
	signUpVerifyAndSetPassword(t, r, sender, "ada@example.com", "a-strong-password")

	status, signInBody := doJSON(t, r, http.MethodPost, "/v1/auth/sessions", "",
		map[string]string{"email": "ada@example.com", "password": "a-strong-password"}, "signin-key")
	if status != http.StatusOK {
		t.Fatalf("sign-in: got status %d (body: %v)", status, signInBody)
	}
	sessionID, _ := signInBody["session_id"].(string)
	if sessionID == "" {
		t.Fatalf("expected a session_id in the sign-in response, got %v", signInBody)
	}

	status, problemBody := doJSON(t, r, http.MethodPost, "/v1/auth/totp-enrollments", sessionID, nil, "enroll-key-1")
	if status != http.StatusUnauthorized {
		t.Fatalf("got status %d, want 401 before stepping up (body: %v)", status, problemBody)
	}
	if problemBody["code"] != "AUTH-4012" {
		t.Fatalf("got code %v, want AUTH-4012", problemBody["code"])
	}

	status, _ = doJSON(t, r, http.MethodPost, "/v1/auth/step-up/email", sessionID, nil, "stepup-request-key")
	if status != http.StatusAccepted {
		t.Fatalf("step-up email request: got status %d", status)
	}
	sent, ok := sender.LastOfKind(email.KindStepUp)
	if !ok {
		t.Fatal("expected a step-up email")
	}
	status, _ = doJSON(t, r, http.MethodPost, "/v1/auth/step-up/email/confirmation", sessionID,
		map[string]string{"code": sent.Code}, "stepup-confirm-key")
	if status != http.StatusOK {
		t.Fatalf("step-up email confirmation: got status %d", status)
	}

	status, enrollBody := doJSON(t, r, http.MethodPost, "/v1/auth/totp-enrollments", sessionID, nil, "enroll-key-2")
	if status != http.StatusOK {
		t.Fatalf("got status %d, want 200 after stepping up (body: %v)", status, enrollBody)
	}
	if enrollBody["url"] == nil {
		t.Fatalf("expected an enrollment url in the response, got %v", enrollBody)
	}
}

func TestStepUp_GatesPasskeyRegistrationUntilSteppedUp(t *testing.T) {
	r, sender := newTestRouter(t)
	signUpVerifyAndSetPassword(t, r, sender, "ada@example.com", "a-strong-password")

	_, signInBody := doJSON(t, r, http.MethodPost, "/v1/auth/sessions", "",
		map[string]string{"email": "ada@example.com", "password": "a-strong-password"}, "signin-key")
	sessionID, _ := signInBody["session_id"].(string)
	if sessionID == "" {
		t.Fatalf("expected a session_id in the sign-in response, got %v", signInBody)
	}

	status, problemBody := doJSON(t, r, http.MethodPost, "/v1/auth/passkey-registrations", sessionID, nil, "passkey-reg-key")
	if status != http.StatusUnauthorized {
		t.Fatalf("got status %d, want 401 before stepping up (body: %v)", status, problemBody)
	}
	if problemBody["code"] != "AUTH-4012" {
		t.Fatalf("got code %v, want AUTH-4012", problemBody["code"])
	}
}
