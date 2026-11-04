//go:build integration

package api_test

import (
	"net/http"
	"testing"
)

func TestSmppAccounts_CreateSuspendReactivateRotateEndToEnd(t *testing.T) {
	r, sender := newTestRouter(t)
	signUpVerifyAndSetPassword(t, r, sender, "ada@example.com", "a-strong-password")

	_, signInBody := doJSON(t, r, http.MethodPost, "/v1/auth/sessions", "",
		map[string]string{"email": "ada@example.com", "password": "a-strong-password"}, "signin-key")
	sessionToken, _ := signInBody["session_token"].(string)
	selectTenant(t, r, sessionToken)
	if sessionToken == "" {
		t.Fatalf("expected a session_token, got %v", signInBody)
	}

	createReq := map[string]any{
		"system_id":    "ACME01",
		"environment":  "sandbox",
		"ip_allowlist": []string{"203.0.113.0/24"},
		"max_binds":    2,
		"max_tps":      50,
	}

	status, problemBody := doJSON(t, r, http.MethodPost, "/v1/auth/smpp-accounts", sessionToken, createReq, "create-key-1")
	if status != http.StatusUnauthorized {
		t.Fatalf("got status %d, want 401 before stepping up (body: %v)", status, problemBody)
	}
	if problemBody["code"] != "AUTH-4012" {
		t.Fatalf("got code %v, want AUTH-4012", problemBody["code"])
	}

	stepUpViaEmail(t, r, sender, sessionToken)

	status, createBody := doJSON(t, r, http.MethodPost, "/v1/auth/smpp-accounts", sessionToken, createReq, "create-key-2")
	if status != http.StatusOK {
		t.Fatalf("create smpp account: got status %d (body: %v)", status, createBody)
	}
	accountID, _ := createBody["id"].(string)
	if accountID == "" {
		t.Fatalf("expected an id in the create response, got %v", createBody)
	}
	firstPassword, _ := createBody["password"].(string)
	if firstPassword == "" {
		t.Fatalf("expected a password in the create response, got %v", createBody)
	}

	status, listBody := doJSON(t, r, http.MethodGet, "/v1/auth/smpp-accounts", sessionToken, nil, "")
	if status != http.StatusOK {
		t.Fatalf("list smpp accounts: got status %d", status)
	}
	accounts, _ := listBody["accounts"].([]any)
	if len(accounts) != 1 {
		t.Fatalf("got %d accounts, want 1 (body: %v)", len(accounts), listBody)
	}

	// Rotation is also step-up-gated, but the step-up that just
	// unlocked create is still fresh (well within the 5-minute
	// window), same reasoning as the API clients test.
	status, rotateBody := doJSON(t, r, http.MethodPost, "/v1/auth/smpp-accounts/"+accountID+"/password", sessionToken, nil, "rotate-key-1")
	if status != http.StatusOK {
		t.Fatalf("rotate password: got status %d (body: %v)", status, rotateBody)
	}
	secondPassword, _ := rotateBody["password"].(string)
	if secondPassword == "" || secondPassword == firstPassword {
		t.Fatalf("expected a fresh password on rotation, got %v", rotateBody)
	}

	status, suspendBody := doJSON(t, r, http.MethodPatch, "/v1/auth/smpp-accounts/"+accountID, sessionToken,
		map[string]any{"status": "suspended"}, "suspend-key")
	if status != http.StatusOK {
		t.Fatalf("suspend: got status %d (body: %v)", status, suspendBody)
	}
	if suspendBody["status"] != "suspended" {
		t.Fatalf("got status %v, want suspended", suspendBody["status"])
	}

	status, reactivateBody := doJSON(t, r, http.MethodPatch, "/v1/auth/smpp-accounts/"+accountID, sessionToken,
		map[string]any{"status": "active"}, "reactivate-key")
	if status != http.StatusOK {
		t.Fatalf("reactivate: got status %d (body: %v)", status, reactivateBody)
	}
	if reactivateBody["status"] != "active" {
		t.Fatalf("got status %v, want active", reactivateBody["status"])
	}
}

func TestSmppAccounts_EmptyIPAllowlist_ReturnsValidationError(t *testing.T) {
	r, sender := newTestRouter(t)
	signUpVerifyAndSetPassword(t, r, sender, "ada@example.com", "a-strong-password")

	_, signInBody := doJSON(t, r, http.MethodPost, "/v1/auth/sessions", "",
		map[string]string{"email": "ada@example.com", "password": "a-strong-password"}, "signin-key")
	sessionToken, _ := signInBody["session_token"].(string)
	selectTenant(t, r, sessionToken)
	stepUpViaEmail(t, r, sender, sessionToken)

	status, body := doJSON(t, r, http.MethodPost, "/v1/auth/smpp-accounts", sessionToken,
		map[string]any{"system_id": "ACME01", "environment": "sandbox", "max_binds": 2, "max_tps": 50}, "create-key")
	if status != http.StatusBadRequest {
		t.Fatalf("got status %d, want 400 (body: %v)", status, body)
	}
	if body["code"] != "VAL-4000" {
		t.Fatalf("got code %v, want VAL-4000", body["code"])
	}
}

func TestSmppAccounts_MissingBearer_Returns401(t *testing.T) {
	r, _ := newTestRouter(t)

	status, _ := doJSON(t, r, http.MethodGet, "/v1/auth/smpp-accounts", "", nil, "")
	if status != http.StatusUnauthorized {
		t.Fatalf("got status %d, want 401", status)
	}
}
