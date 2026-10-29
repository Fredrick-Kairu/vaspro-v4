//go:build integration

package api_test

import (
	"net/http"
	"testing"

	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/email"
)

// stepUpViaEmail completes an email step-up for sessionToken, assuming
// the account has no TOTP enrolled (the simplest path — this task
// doesn't need to prove the TOTP step-up path again, 1.6's own tests
// already cover that).
func stepUpViaEmail(t *testing.T, r http.Handler, sender *email.Sender, sessionToken string) {
	t.Helper()
	status, _ := doJSON(t, r, http.MethodPost, "/v1/auth/step-up/email", sessionToken, nil, "stepup-request-"+sessionToken)
	if status != http.StatusAccepted {
		t.Fatalf("step-up email request: got status %d", status)
	}
	sent, ok := sender.LastOfKind(email.KindStepUp)
	if !ok {
		t.Fatal("expected a step-up email")
	}
	status, _ = doJSON(t, r, http.MethodPost, "/v1/auth/step-up/email/confirmation", sessionToken,
		map[string]string{"code": sent.Code}, "stepup-confirm-"+sessionToken)
	if status != http.StatusOK {
		t.Fatalf("step-up email confirmation: got status %d", status)
	}
}

func TestApiClients_CreateRotateEndToEnd(t *testing.T) {
	r, sender := newTestRouter(t)
	signUpVerifyAndSetPassword(t, r, sender, "ada@example.com", "a-strong-password")

	_, signInBody := doJSON(t, r, http.MethodPost, "/v1/auth/sessions", "",
		map[string]string{"email": "ada@example.com", "password": "a-strong-password"}, "signin-key")
	sessionToken, _ := signInBody["session_token"].(string)
	if sessionToken == "" {
		t.Fatalf("expected a session_token, got %v", signInBody)
	}

	status, problemBody := doJSON(t, r, http.MethodPost, "/v1/auth/api-clients", sessionToken,
		map[string]any{"name": "Reporting", "environment": "sandbox", "scopes": []string{"reports:read"}}, "create-key-1")
	if status != http.StatusUnauthorized {
		t.Fatalf("got status %d, want 401 before stepping up (body: %v)", status, problemBody)
	}
	if problemBody["code"] != "AUTH-4012" {
		t.Fatalf("got code %v, want AUTH-4012", problemBody["code"])
	}

	stepUpViaEmail(t, r, sender, sessionToken)

	status, createBody := doJSON(t, r, http.MethodPost, "/v1/auth/api-clients", sessionToken,
		map[string]any{"name": "Reporting", "environment": "sandbox", "scopes": []string{"reports:read"}}, "create-key-2")
	if status != http.StatusOK {
		t.Fatalf("create api client: got status %d (body: %v)", status, createBody)
	}
	clientID, _ := createBody["id"].(string)
	if clientID == "" {
		t.Fatalf("expected an id in the create response, got %v", createBody)
	}
	secretObj, _ := createBody["secret"].(map[string]any)
	firstSecret, _ := secretObj["secret"].(string)
	if firstSecret == "" {
		t.Fatalf("expected a secret in the create response, got %v", createBody)
	}

	status, listBody := doJSON(t, r, http.MethodGet, "/v1/auth/api-clients", sessionToken, nil, "")
	if status != http.StatusOK {
		t.Fatalf("list api clients: got status %d", status)
	}
	clients, _ := listBody["clients"].([]any)
	if len(clients) != 1 {
		t.Fatalf("got %d clients, want 1 (body: %v)", len(clients), listBody)
	}

	// Rotation is also step-up-gated, but the step-up that just
	// unlocked create is still fresh (well within the 5-minute
	// window) — one step-up covers every gated action until it
	// expires, not one step-up per action, so this succeeds without
	// stepping up again.
	status, rotateBody := doJSON(t, r, http.MethodPost, "/v1/auth/api-clients/"+clientID+"/secrets", sessionToken, nil, "rotate-key-1")
	if status != http.StatusOK {
		t.Fatalf("rotate: got status %d (body: %v)", status, rotateBody)
	}
	secondSecret, _ := rotateBody["secret"].(string)
	if secondSecret == "" || secondSecret == firstSecret {
		t.Fatalf("expected a fresh secret on rotation, got %v", rotateBody)
	}

	status, detailBody := doJSON(t, r, http.MethodGet, "/v1/auth/api-clients/"+clientID, sessionToken, nil, "")
	if status != http.StatusOK {
		t.Fatalf("get api client: got status %d", status)
	}
	secrets, _ := detailBody["secrets"].([]any)
	if len(secrets) != 2 {
		t.Fatalf("got %d active secrets after one rotation, want 2 (body: %v)", len(secrets), detailBody)
	}

	status, _ = doJSON(t, r, http.MethodDelete, "/v1/auth/api-clients/"+clientID, sessionToken, nil, "revoke-key")
	if status != http.StatusNoContent {
		t.Fatalf("revoke api client: got status %d", status)
	}

	status, detailBody = doJSON(t, r, http.MethodGet, "/v1/auth/api-clients/"+clientID, sessionToken, nil, "")
	if status != http.StatusOK {
		t.Fatalf("get api client after revoke: got status %d", status)
	}
	if detailBody["revoked_at"] == nil {
		t.Fatalf("expected revoked_at to be set, got %v", detailBody)
	}
}
