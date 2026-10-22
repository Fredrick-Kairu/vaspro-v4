//go:build integration

package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
