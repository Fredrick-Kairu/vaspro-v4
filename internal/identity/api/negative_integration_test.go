//go:build integration

// Phase 1's exit-gate negative pass: every endpoint without a token,
// and with another tenant's IDs. Both tests range over the operations
// in docs/api/identity.yaml (api.LoadSpecOperations), so an endpoint
// added to the contract is covered — or fails the coverage check —
// the moment it exists. "With a wrong scope" has no identity
// counterpart: no identity operation is scope-gated; scopes are
// checked at the gateway (internal/gateway's TestRequireScope and
// TestGateway_MissingScope_Returns403).
package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/descope/virtualwebauthn"
	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/api"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/email"
)

var pathParam = regexp.MustCompile(`\{[^}]+\}`)

// signInForTokens signs rawEmail in and returns the new session's
// public id and its bearer token.
func signInForTokens(t *testing.T, r http.Handler, rawEmail, password, idempotencyKey string) (id, token string) {
	t.Helper()
	code, body := doJSON(t, r, http.MethodPost, "/v1/auth/sessions", "",
		map[string]string{"email": rawEmail, "password": password}, idempotencyKey)
	if code != http.StatusOK {
		t.Fatalf("sign in %s: got status %d (body: %v)", rawEmail, code, body)
	}
	id, _ = body["session_id"].(string)
	token, _ = body["session_token"].(string)
	if id == "" || token == "" {
		t.Fatalf("sign in %s: expected a session_id and session_token, got %v", rawEmail, body)
	}
	return id, token
}

// doRaw sends one request with the Authorization header exactly as
// given (empty means none), returning the status and the problem code.
func doRaw(t *testing.T, r http.Handler, method, path, authorization string, body []byte) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	if method != http.MethodGet {
		req.Header.Set("Idempotency-Key", uuid.NewString())
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	var problem struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &problem)
	return rec.Code, problem.Code
}

func TestEverySessionBearerOperation_RejectsMissingGarbageAndRevokedTokens(t *testing.T) {
	r, sender := newTestRouter(t)
	signUpVerifyAndSetPassword(t, r, sender, "ada@example.com", "a-strong-password")
	_, revoked := signInForTokens(t, r, "ada@example.com", "a-strong-password", "signin-key")
	if code, _ := doJSON(t, r, http.MethodDelete, "/v1/auth/sessions", revoked, nil, "signout-key"); code != http.StatusNoContent {
		t.Fatalf("sign out everywhere: got status %d", code)
	}

	credentials := []struct {
		name          string
		authorization string
	}{
		{"no token", ""},
		{"garbage token", "Bearer garbage"},
		{"empty bearer", "Bearer "},
		{"revoked token", "Bearer " + revoked},
	}

	checked := 0
	for _, op := range api.LoadSpecOperations(t) {
		if !op.SessionBearer {
			continue
		}
		checked++
		// Any well-formed id: the request must be rejected before
		// anything looks it up. {} gets a body past the strict server's
		// JSON decoding to the session check.
		path := pathParam.ReplaceAllStringFunc(op.Path, func(string) string { return uuid.NewString() })
		var body []byte
		if op.HasBody {
			body = []byte("{}")
		}
		for _, c := range credentials {
			t.Run(op.OperationID+"/"+c.name, func(t *testing.T) {
				status, code := doRaw(t, r, op.Method, path, c.authorization, body)
				if status != http.StatusUnauthorized || code != "AUTH-4010" {
					t.Fatalf("%s %s: got status %d, code %q, want 401 AUTH-4010", op.Method, path, status, code)
				}
			})
		}
	}
	if checked == 0 {
		t.Fatal("no sessionBearer operation found in the spec")
	}
}

func TestEveryIDTakingOperation_RejectsAnotherTenantsIDs(t *testing.T) {
	r, sender := newTestRouter(t)

	// --- bob owns one of everything an id can name ---
	signUpVerifyAndSetPassword(t, r, sender, "bob@example.com", "bobs-strong-password")
	_, bob := signInForTokens(t, r, "bob@example.com", "bobs-strong-password", "bob-signin-1")
	bobOtherSessionID, bobOther := signInForTokens(t, r, "bob@example.com", "bobs-strong-password", "bob-signin-2")
	stepUpViaEmail(t, r, sender, bob)
	selectTenant(t, r, bob)

	passkeyID := registerPasskey(t, r, bob, "Bob's laptop")

	code, client := doJSON(t, r, http.MethodPost, "/v1/auth/api-clients", bob, map[string]any{
		"name": "bob's client", "environment": "sandbox", "scopes": []string{"reports:read"},
	}, "bob-create-client")
	if code != http.StatusOK {
		t.Fatalf("create api client: got status %d (body: %v)", code, client)
	}
	clientID, _ := client["id"].(string)
	secret, _ := client["secret"].(map[string]any)
	secretID, _ := secret["id"].(string)

	code, smpp := doJSON(t, r, http.MethodPost, "/v1/auth/smpp-accounts", bob, map[string]any{
		"system_id": "bobsmpp1", "environment": "sandbox",
		"ip_allowlist": []string{"203.0.113.0/24"}, "max_binds": 1, "max_tps": 10,
	}, "bob-create-smpp")
	if code != http.StatusOK {
		t.Fatalf("create smpp account: got status %d (body: %v)", code, smpp)
	}
	smppID, _ := smpp["id"].(string)
	if clientID == "" || secretID == "" || smppID == "" {
		t.Fatalf("expected ids for bob's api client, secret and smpp account, got %v / %v", client, smpp)
	}

	// --- ada, fully signed in and stepped up, aims at bob's ids ---
	signUpVerifyAndSetPassword(t, r, sender, "ada@example.com", "a-strong-password")
	_, ada := signInForTokens(t, r, "ada@example.com", "a-strong-password", "ada-signin")
	stepUpViaEmail(t, r, sender, ada)
	selectTenant(t, r, ada) // a different account from bob's

	attempts := map[string]struct {
		path string
		body any
	}{
		"deleteSession":             {"/v1/auth/sessions/" + bobOtherSessionID, nil},
		"updatePasskey":             {"/v1/auth/passkeys/" + passkeyID, map[string]string{"name": "taken over"}},
		"deletePasskey":             {"/v1/auth/passkeys/" + passkeyID, nil},
		"getApiClient":              {"/v1/auth/api-clients/" + clientID, nil},
		"revokeApiClient":           {"/v1/auth/api-clients/" + clientID, nil},
		"createApiClientSecret":     {"/v1/auth/api-clients/" + clientID + "/secrets", nil},
		"revokeApiClientSecret":     {"/v1/auth/api-clients/" + clientID + "/secrets/" + secretID, nil},
		"getSmppAccount":            {"/v1/auth/smpp-accounts/" + smppID, nil},
		"updateSmppAccountStatus":   {"/v1/auth/smpp-accounts/" + smppID, map[string]string{"status": "suspended"}},
		"createSmppAccountPassword": {"/v1/auth/smpp-accounts/" + smppID + "/password", nil},
	}

	covered := map[string]bool{}
	for _, op := range api.LoadSpecOperations(t) {
		// {challenge} operations are anonymous sign-in steps: the path
		// carries a signed ceremony token, not a resource id.
		if !strings.Contains(op.Path, "{id}") && !strings.Contains(op.Path, "{secretId}") {
			continue
		}
		attempt, ok := attempts[op.OperationID]
		if !ok {
			t.Errorf("%s %s takes an id but has no cross-tenant case here", op.Method, op.Path)
			continue
		}
		covered[op.OperationID] = true
		t.Run(op.OperationID, func(t *testing.T) {
			code, body := doJSON(t, r, op.Method, attempt.path, ada, attempt.body, "ada-"+op.OperationID)
			if code != http.StatusNotFound || body["code"] != "VAL-4003" {
				t.Fatalf("%s %s with bob's id: got status %d, code %v, want 404 VAL-4003", op.Method, attempt.path, code, body["code"])
			}
		})
	}
	for id := range attempts {
		if !covered[id] {
			t.Errorf("cross-tenant case %s matches no id-taking operation in the spec", id)
		}
	}

	// --- and bob's things are exactly as he left them ---
	if code, _ := doJSON(t, r, http.MethodGet, "/v1/auth/sessions", bobOther, nil, ""); code != http.StatusOK {
		t.Fatalf("bob's other session: got status %d, want it still active", code)
	}
	code, passkeys := doJSON(t, r, http.MethodGet, "/v1/auth/passkeys", bob, nil, "")
	if code != http.StatusOK || !strings.Contains(mustMarshal(t, passkeys), `"Bob's laptop"`) {
		t.Fatalf("bob's passkey: got status %d, body %v, want it present and unrenamed", code, passkeys)
	}
	code, detail := doJSON(t, r, http.MethodGet, "/v1/auth/api-clients/"+clientID, bob, nil, "")
	secrets, _ := detail["secrets"].([]any)
	if code != http.StatusOK || detail["revoked_at"] != nil || len(secrets) != 1 {
		t.Fatalf("bob's api client: got status %d, body %v, want it unrevoked with its one secret", code, detail)
	}
	code, account := doJSON(t, r, http.MethodGet, "/v1/auth/smpp-accounts/"+smppID, bob, nil, "")
	if code != http.StatusOK || account["status"] != "active" {
		t.Fatalf("bob's smpp account: got status %d, body %v, want it still active", code, account)
	}
}

// registerPasskey runs a full WebAuthn registration over HTTP with a
// software authenticator and returns the new passkey's id.
func registerPasskey(t *testing.T, r http.Handler, sessionToken, name string) string {
	t.Helper()
	code, begin := doJSON(t, r, http.MethodPost, "/v1/auth/passkey-registrations", sessionToken, nil, "passkey-begin-"+name)
	if code != http.StatusOK {
		t.Fatalf("begin passkey registration: got status %d (body: %v)", code, begin)
	}
	ceremony, _ := begin["session"].(string)
	options, err := virtualwebauthn.ParseAttestationOptions(mustMarshal(t, begin["options"]))
	if err != nil {
		t.Fatalf("parsing attestation options: %v", err)
	}
	rp := virtualwebauthn.RelyingParty{ID: "localhost", Name: "VASPRO Test", Origin: "https://localhost"}
	authenticator := virtualwebauthn.NewAuthenticator()
	cred := virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)
	authenticator.AddCredential(cred)
	attestation := virtualwebauthn.CreateAttestationResponse(rp, authenticator, cred, *options)

	code, summary := doJSON(t, r, http.MethodPost, "/v1/auth/passkey-registrations/confirmation", sessionToken, map[string]any{
		"session": ceremony, "name": name, "credential": json.RawMessage(attestation),
	}, "passkey-confirm-"+name)
	if code != http.StatusOK {
		t.Fatalf("confirm passkey registration: got status %d (body: %v)", code, summary)
	}
	id, _ := summary["id"].(string)
	if id == "" {
		t.Fatalf("expected a passkey id, got %v", summary)
	}
	return id
}

func mustMarshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshaling: %v", err)
	}
	return string(b)
}

var _ = email.KindStepUp // the fake sender's kinds, used via stepUpViaEmail
