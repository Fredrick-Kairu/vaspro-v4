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
	r := newTestRouter(t)

	req := httptest.NewRequest(http.MethodPost, "/v1/auth/totp-enrollments", nil)
	req.Header.Set("Idempotency-Key", "test-key-1")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("got status %d, want 401 (body: %s)", rec.Code, rec.Body.String())
	}
}

func TestRequireSession_RejectsTotpEnrollmentWithGarbageBearer(t *testing.T) {
	r := newTestRouter(t)

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
	r := newTestRouter(t)

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

// TestRequireSession_LeavesUnprotectedRoutesAlone proves requireSession
// is gated on operation ID, not applied globally: a write with no
// Authorization header at all still has to pass (the existing
// anonymous sign-up tests already cover this for /v1/auth/signups —
// this one exercises a second anonymous route, password-resets, to
// show the gating isn't accidentally specific to signups).
func TestRequireSession_LeavesUnprotectedRoutesAlone(t *testing.T) {
	r := newTestRouter(t)

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
