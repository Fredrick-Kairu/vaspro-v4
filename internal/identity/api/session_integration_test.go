//go:build integration

package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequireSession_RejectsTotpEnrollmentWithoutBearer(t *testing.T) {
	r, _ := newTestRouter(t)

	req := httptest.NewRequest(http.MethodPost, "/v1/auth/totp-enrollments", nil)
	req.Header.Set("Idempotency-Key", "test-key-1")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("got status %d, want 401 (body: %s)", rec.Code, rec.Body.String())
	}
}

func TestRequireSession_RejectsTotpEnrollmentWithGarbageBearer(t *testing.T) {
	r, _ := newTestRouter(t)

	req := httptest.NewRequest(http.MethodPost, "/v1/auth/totp-enrollments", nil)
	req.Header.Set("Idempotency-Key", "test-key-2")
	req.Header.Set("Authorization", "Bearer not-a-real-session")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("got status %d, want 401 (body: %s)", rec.Code, rec.Body.String())
	}
}

func TestRequireSession_RejectsTotpEnrollmentConfirmationWithoutBearer(t *testing.T) {
	r, _ := newTestRouter(t)

	body, _ := json.Marshal(map[string]string{"code": "000000"})
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/totp-enrollments/confirmation", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "test-key-3")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("got status %d, want 401 (body: %s)", rec.Code, rec.Body.String())
	}
}

func TestRequireSession_RejectsPasskeyManagementRoutesWithoutBearer(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
		body   []byte
	}{
		{"createPasskeyRegistration", http.MethodPost, "/v1/auth/passkey-registrations", nil},
		{"createPasskeyRegistrationConfirmation", http.MethodPost, "/v1/auth/passkey-registrations/confirmation", mustJSON(t, map[string]any{"session": "x", "credential": map[string]any{}})},
		{"listPasskeys", http.MethodGet, "/v1/auth/passkeys", nil},
		{"updatePasskey", http.MethodPatch, "/v1/auth/passkeys/" + "00000000-0000-0000-0000-000000000000", mustJSON(t, map[string]any{"name": "x"})},
		{"deletePasskey", http.MethodDelete, "/v1/auth/passkeys/" + "00000000-0000-0000-0000-000000000000", nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := newTestRouter(t)

			var body *bytes.Reader
			if tc.body != nil {
				body = bytes.NewReader(tc.body)
			} else {
				body = bytes.NewReader(nil)
			}
			req := httptest.NewRequest(tc.method, tc.path, body)
			req.Header.Set("Content-Type", "application/json")
			if tc.method != http.MethodGet {
				req.Header.Set("Idempotency-Key", "test-key-"+tc.name)
			}
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("got status %d, want 401 (body: %s)", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestRequireSession_RejectsSessionManagementRoutesWithoutBearer(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
		body   []byte
	}{
		{"listSessions", http.MethodGet, "/v1/auth/sessions", nil},
		{"deleteSession", http.MethodDelete, "/v1/auth/sessions/00000000-0000-0000-0000-000000000000", nil},
		{"deleteSessions", http.MethodDelete, "/v1/auth/sessions", nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := newTestRouter(t)

			var body *bytes.Reader
			if tc.body != nil {
				body = bytes.NewReader(tc.body)
			} else {
				body = bytes.NewReader(nil)
			}
			req := httptest.NewRequest(tc.method, tc.path, body)
			req.Header.Set("Content-Type", "application/json")
			if tc.method != http.MethodGet {
				req.Header.Set("Idempotency-Key", "test-key-"+tc.name)
			}
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("got status %d, want 401 (body: %s)", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestRequireSession_RejectsApiClientRoutesWithoutBearer(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
		body   []byte
	}{
		{"createApiClient", http.MethodPost, "/v1/auth/api-clients", mustJSON(t, map[string]any{"name": "x", "environment": "sandbox", "scopes": []string{"sms:send"}})},
		{"listApiClients", http.MethodGet, "/v1/auth/api-clients", nil},
		{"getApiClient", http.MethodGet, "/v1/auth/api-clients/00000000-0000-0000-0000-000000000000", nil},
		{"revokeApiClient", http.MethodDelete, "/v1/auth/api-clients/00000000-0000-0000-0000-000000000000", nil},
		{"createApiClientSecret", http.MethodPost, "/v1/auth/api-clients/00000000-0000-0000-0000-000000000000/secrets", nil},
		{"revokeApiClientSecret", http.MethodDelete, "/v1/auth/api-clients/00000000-0000-0000-0000-000000000000/secrets/00000000-0000-0000-0000-000000000000", nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := newTestRouter(t)

			var body *bytes.Reader
			if tc.body != nil {
				body = bytes.NewReader(tc.body)
			} else {
				body = bytes.NewReader(nil)
			}
			req := httptest.NewRequest(tc.method, tc.path, body)
			req.Header.Set("Content-Type", "application/json")
			if tc.method != http.MethodGet {
				req.Header.Set("Idempotency-Key", "test-key-"+tc.name)
			}
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("got status %d, want 401 (body: %s)", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestRequireSession_RejectsStepUpRoutesWithoutBearer(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
		body   []byte
	}{
		{"createStepUpTotpVerification", http.MethodPost, "/v1/auth/step-up/totp", mustJSON(t, map[string]any{"code": "000000"})},
		{"createStepUpEmail", http.MethodPost, "/v1/auth/step-up/email", nil},
		{"createStepUpEmailConfirmation", http.MethodPost, "/v1/auth/step-up/email/confirmation", mustJSON(t, map[string]any{"code": "000000"})},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := newTestRouter(t)

			var body *bytes.Reader
			if tc.body != nil {
				body = bytes.NewReader(tc.body)
			} else {
				body = bytes.NewReader(nil)
			}
			req := httptest.NewRequest(tc.method, tc.path, body)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Idempotency-Key", "test-key-"+tc.name)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("got status %d, want 401 (body: %s)", rec.Code, rec.Body.String())
			}
		})
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshaling test body: %v", err)
	}
	return b
}

// TestRequireSession_LeavesUnprotectedRoutesAlone proves requireSession
// is gated on operation ID, not applied globally: a write with no
// Authorization header at all still has to pass (the existing
// anonymous sign-up tests already cover this for /v1/auth/signups —
// this one exercises a second anonymous route, password-resets, to
// show the gating isn't accidentally specific to signups).
func TestRequireSession_LeavesUnprotectedRoutesAlone(t *testing.T) {
	r, _ := newTestRouter(t)

	body, _ := json.Marshal(map[string]string{"email": "ada@example.com"})
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/password-resets", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "test-key-4")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("got status %d, want 202 (body: %s)", rec.Code, rec.Body.String())
	}
}

// TestRequireSession_SessionIDIsNotABearer: a session's id is its
// public handle (listSessions returns it; deleteSession takes it in
// the URL), so it must never authenticate. Only the session_token
// sign-in returns does.
func TestRequireSession_SessionIDIsNotABearer(t *testing.T) {
	r, sender := newTestRouter(t)
	signUpVerifyAndSetPassword(t, r, sender, "ada@example.com", "a-strong-password")

	code, signIn := doJSON(t, r, http.MethodPost, "/v1/auth/sessions", "",
		map[string]string{"email": "ada@example.com", "password": "a-strong-password"}, "signin-key")
	if code != http.StatusOK {
		t.Fatalf("sign in: got status %d", code)
	}
	sessionID, _ := signIn["session_id"].(string)
	if sessionID == "" {
		t.Fatalf("expected a session_id, got %v", signIn)
	}

	code, body := doJSON(t, r, http.MethodGet, "/v1/auth/sessions", sessionID, nil, "")
	if code != http.StatusUnauthorized || body["code"] != "AUTH-4010" {
		t.Fatalf("session id as bearer: got status %d, code %v, want 401 AUTH-4010", code, body["code"])
	}
}

// TestListSessions_NeverReturnsATokenAndRevokeTakesTheID: the list shows
// each session's public id (what deleteSession takes) and never a
// session_token, so holding one session can't hand out the others.
func TestListSessions_NeverReturnsATokenAndRevokeTakesTheID(t *testing.T) {
	r, sender := newTestRouter(t)
	signUpVerifyAndSetPassword(t, r, sender, "ada@example.com", "a-strong-password")

	signIn := func(key string) (id, token string) {
		code, body := doJSON(t, r, http.MethodPost, "/v1/auth/sessions", "",
			map[string]string{"email": "ada@example.com", "password": "a-strong-password"}, key)
		if code != http.StatusOK {
			t.Fatalf("sign in: got status %d", code)
		}
		id, _ = body["session_id"].(string)
		token, _ = body["session_token"].(string)
		if id == "" || token == "" {
			t.Fatalf("expected a session_id and session_token, got %v", body)
		}
		return id, token
	}
	firstID, firstToken := signIn("signin-key-1")
	_, secondToken := signIn("signin-key-2")

	req := httptest.NewRequest(http.MethodGet, "/v1/auth/sessions", nil)
	req.Header.Set("Authorization", "Bearer "+secondToken)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list sessions: got status %d", rec.Code)
	}
	raw := rec.Body.String()
	for _, token := range []string{firstToken, secondToken} {
		if strings.Contains(raw, token) {
			t.Fatalf("the session list contains a session token: %s", raw)
		}
	}
	if !strings.Contains(raw, firstID) {
		t.Fatalf("expected the first session's id in the list, got %s", raw)
	}

	code, _ := doJSON(t, r, http.MethodDelete, "/v1/auth/sessions/"+firstID, secondToken, nil, "revoke-first")
	if code != http.StatusNoContent {
		t.Fatalf("revoke by id: got status %d", code)
	}
	code, body := doJSON(t, r, http.MethodGet, "/v1/auth/sessions", firstToken, nil, "")
	if code != http.StatusUnauthorized || body["code"] != "AUTH-4010" {
		t.Fatalf("revoked token: got status %d, code %v, want 401 AUTH-4010", code, body["code"])
	}
}
