//go:build integration

package api_test

import (
	"net/http"
	"testing"
)

func TestListSecurityEvents_RequiresBearerAndScopesToOwnUser(t *testing.T) {
	r, sender := newTestRouter(t)
	signUpVerifyAndSetPassword(t, r, sender, "ada@example.com", "a-strong-password")
	signUpVerifyAndSetPassword(t, r, sender, "bob@example.com", "a-strong-password")

	status, _ := doJSON(t, r, http.MethodGet, "/v1/auth/security-events", "", nil, "")
	if status != http.StatusUnauthorized {
		t.Fatalf("got status %d, want 401 with no bearer", status)
	}

	_, adaSignIn := doJSON(t, r, http.MethodPost, "/v1/auth/sessions", "",
		map[string]string{"email": "ada@example.com", "password": "a-strong-password"}, "ada-signin")
	adaSessionToken, _ := adaSignIn["session_token"].(string)
	_, bobSignIn := doJSON(t, r, http.MethodPost, "/v1/auth/sessions", "",
		map[string]string{"email": "bob@example.com", "password": "a-strong-password"}, "bob-signin")
	bobSessionToken, _ := bobSignIn["session_token"].(string)

	status, adaBody := doJSON(t, r, http.MethodGet, "/v1/auth/security-events", adaSessionToken, nil, "")
	if status != http.StatusOK {
		t.Fatalf("list security events (ada): got status %d", status)
	}
	adaEvents, _ := adaBody["events"].([]any)
	if len(adaEvents) == 0 {
		t.Fatal("expected at least ada's own sign-in event")
	}
	for _, raw := range adaEvents {
		ev, _ := raw.(map[string]any)
		if ev["type"] != "sign_in" {
			t.Fatalf("got event type %v, want sign_in", ev["type"])
		}
	}

	status, bobBody := doJSON(t, r, http.MethodGet, "/v1/auth/security-events", bobSessionToken, nil, "")
	if status != http.StatusOK {
		t.Fatalf("list security events (bob): got status %d", status)
	}
	bobEvents, _ := bobBody["events"].([]any)
	if len(bobEvents) != 1 {
		t.Fatalf("got %d events for bob, want exactly 1 (ada's own sign-in must not leak into bob's list)", len(bobEvents))
	}
}
