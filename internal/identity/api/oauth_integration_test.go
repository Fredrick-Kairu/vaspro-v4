//go:build integration

package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// doForm posts application/x-www-form-urlencoded body to path — the
// one operation in this API (task 1.8's OAuth2 token endpoint) that
// isn't JSON, so doJSON doesn't fit. Deliberately sets no
// Idempotency-Key: that's the point of the test using this helper.
func doForm(t *testing.T, r http.Handler, path string, form url.Values) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
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

func TestCreateOAuthToken_HappyPathAndExemptions(t *testing.T) {
	r, sender := newTestRouter(t)
	signUpVerifyAndSetPassword(t, r, sender, "oauth-http@example.com", "a-strong-password")

	_, signInBody := doJSON(t, r, http.MethodPost, "/v1/auth/sessions", "",
		map[string]string{"email": "oauth-http@example.com", "password": "a-strong-password"}, "signin-key")
	sessionToken, _ := signInBody["session_token"].(string)
	selectTenant(t, r, sessionToken)
	if sessionToken == "" {
		t.Fatalf("expected a session_token, got %v", signInBody)
	}
	stepUpViaEmail(t, r, sender, sessionToken)

	status, createBody := doJSON(t, r, http.MethodPost, "/v1/auth/api-clients", sessionToken,
		map[string]any{"name": "OAuth test client", "environment": "sandbox", "scopes": []string{"sms:send", "reports:read"}}, "create-oauth-client")
	if status != http.StatusOK {
		t.Fatalf("create api client: got status %d (body: %v)", status, createBody)
	}
	clientID, _ := createBody["id"].(string)
	secretObj, _ := createBody["secret"].(map[string]any)
	secret, _ := secretObj["secret"].(string)
	if clientID == "" || secret == "" {
		t.Fatalf("expected an id and secret, got %v", createBody)
	}

	// No Idempotency-Key header at all — proves routes.go's exemption,
	// since every other write in this API rejects that.
	status, tokenBody := doForm(t, r, "/v1/oauth/token", url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {clientID},
		"client_secret": {secret},
	})
	if status != http.StatusOK {
		t.Fatalf("token exchange: got status %d (body: %v)", status, tokenBody)
	}
	if tokenBody["token_type"] != "Bearer" {
		t.Fatalf("got token_type %v, want Bearer", tokenBody["token_type"])
	}
	accessToken, _ := tokenBody["access_token"].(string)
	if accessToken == "" {
		t.Fatalf("expected an access_token, got %v", tokenBody)
	}
	if tokenBody["scope"] != "sms:send reports:read" {
		t.Fatalf("got scope %v, want every client scope", tokenBody["scope"])
	}
}

func TestCreateOAuthToken_UnsupportedGrantType_ReturnsOAuthShapedError(t *testing.T) {
	r, _ := newTestRouter(t)

	status, body := doForm(t, r, "/v1/oauth/token", url.Values{
		"grant_type":    {"password"},
		"client_id":     {"whatever"},
		"client_secret": {"whatever"},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("got status %d, want 400", status)
	}
	if body["error"] != "unsupported_grant_type" {
		t.Fatalf("got body %v, want error=unsupported_grant_type", body)
	}
	// RFC 6749 §5.2's shape, not this API's usual problem+json —
	// "code"/"type"/"title" would mean it fell through to toProblem
	// instead of this operation's own OAuth2-shaped response.
	if _, hasProblemCode := body["code"]; hasProblemCode {
		t.Fatalf("got a problem+json-shaped body, want RFC 6749's {error: ...}: %v", body)
	}
}

func TestCreateOAuthToken_BadSecret_ReturnsInvalidClient(t *testing.T) {
	r, sender := newTestRouter(t)
	signUpVerifyAndSetPassword(t, r, sender, "oauth-badsecret@example.com", "a-strong-password")

	_, signInBody := doJSON(t, r, http.MethodPost, "/v1/auth/sessions", "",
		map[string]string{"email": "oauth-badsecret@example.com", "password": "a-strong-password"}, "signin-key-2")
	sessionToken, _ := signInBody["session_token"].(string)
	selectTenant(t, r, sessionToken)
	stepUpViaEmail(t, r, sender, sessionToken)

	status, createBody := doJSON(t, r, http.MethodPost, "/v1/auth/api-clients", sessionToken,
		map[string]any{"name": "OAuth bad secret client", "environment": "sandbox", "scopes": []string{"sms:send"}}, "create-oauth-client-2")
	if status != http.StatusOK {
		t.Fatalf("create api client: got status %d (body: %v)", status, createBody)
	}
	clientID, _ := createBody["id"].(string)

	status, body := doForm(t, r, "/v1/oauth/token", url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {clientID},
		"client_secret": {"definitely-not-the-real-secret"},
	})
	if status != http.StatusUnauthorized {
		t.Fatalf("got status %d, want 401", status)
	}
	if body["error"] != "invalid_client" {
		t.Fatalf("got body %v, want error=invalid_client", body)
	}
}

func TestGetJwks_PublishesTheActiveKey(t *testing.T) {
	r, _ := newTestRouter(t)

	status, body := doJSON(t, r, http.MethodGet, "/.well-known/jwks.json", "", nil, "")
	if status != http.StatusOK {
		t.Fatalf("got status %d, want 200", status)
	}
	keys, _ := body["keys"].([]any)
	if len(keys) != 1 {
		t.Fatalf("got %d keys, want 1 (newTestRouter's own EnsureActiveSigningKey call)", len(keys))
	}
	key, _ := keys[0].(map[string]any)
	if key["kty"] != "OKP" || key["crv"] != "Ed25519" || key["use"] != "sig" || key["alg"] != "EdDSA" {
		t.Fatalf("got key %v, want a well-formed Ed25519 OKP JWK", key)
	}
	if key["kid"] == "" || key["kid"] == nil {
		t.Fatalf("expected a non-empty kid, got %v", key)
	}
}
