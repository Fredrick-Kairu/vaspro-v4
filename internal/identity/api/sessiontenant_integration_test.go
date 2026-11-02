//go:build integration

package api_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/api"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/tenantaccess"
)

func TestSelectSessionTenant(t *testing.T) {
	access := &tenantaccess.Fake{}
	r, sender := newTestRouterWithAccess(t, nil, access)
	signUpVerifyAndSetPassword(t, r, sender, "ada@example.com", "a-strong-password")
	_, token := signInForTokens(t, r, "ada@example.com", "a-strong-password", "ada-signin")
	tenant := uuid.NewString()

	status, body := doJSON(t, r, http.MethodPut, "/v1/auth/sessions/current/tenant", token,
		map[string]string{"tenant_id": tenant}, "select-1")
	if status != http.StatusNotFound || body["code"] != "VAL-4003" {
		t.Fatalf("an account ada isn't in: got %d %v, want 404 VAL-4003", status, body)
	}

	access.Grant(uuid.Nil, uuid.MustParse(tenant))
	status, body = doJSON(t, r, http.MethodPut, "/v1/auth/sessions/current/tenant", token,
		map[string]string{"tenant_id": tenant}, "select-2")
	if status != http.StatusOK || body["tenant_id"] != tenant {
		t.Fatalf("an account ada is in: got %d %v", status, body)
	}

	_, list := doJSON(t, r, http.MethodGet, "/v1/auth/sessions", token, nil, "")
	sessions, _ := list["sessions"].([]any)
	if len(sessions) != 1 || sessions[0].(map[string]any)["tenant_id"] != tenant {
		t.Fatalf("listSessions doesn't show the selected account: %v", list)
	}

	t.Run("removing ada from the account takes effect at once", func(t *testing.T) {
		access.Revoke(uuid.Nil, uuid.MustParse(tenant))
		status, body := doJSON(t, r, http.MethodGet, "/v1/auth/api-clients", token, nil, "")
		if status != http.StatusForbidden || body["code"] != "AUTH-4023" {
			t.Fatalf("got %d %v, want 403 AUTH-4023", status, body)
		}
	})
}

// TestEveryCredentialOperation_RequiresASelectedAccount ranges over the
// spec's API client and SMPP account operations, so one added later
// without the check fails here.
func TestEveryCredentialOperation_RequiresASelectedAccount(t *testing.T) {
	r, sender := newTestRouter(t)
	signUpVerifyAndSetPassword(t, r, sender, "ada@example.com", "a-strong-password")
	_, token := signInForTokens(t, r, "ada@example.com", "a-strong-password", "ada-signin")
	stepUpViaEmail(t, r, sender, token) // so step-up-gated operations reach the account check

	const someID = "00000000-0000-0000-0000-000000000000"
	checked := 0
	for _, op := range api.LoadSpecOperations(t) {
		if !strings.HasPrefix(op.Path, "/v1/auth/api-clients") && !strings.HasPrefix(op.Path, "/v1/auth/smpp-accounts") {
			continue
		}
		checked++
		path := strings.NewReplacer("{id}", someID, "{secretId}", someID).Replace(op.Path)
		var body []byte
		if op.HasBody {
			body = []byte(`{}`)
		}
		t.Run(op.OperationID, func(t *testing.T) {
			status, code := doRaw(t, r, op.Method, path, "Bearer "+token, body)
			if status != http.StatusForbidden || code != "AUTH-4023" {
				t.Fatalf("%s %s: got %d %q, want 403 AUTH-4023", op.Method, path, status, code)
			}
		})
	}
	if checked < 11 {
		t.Fatalf("checked %d credential operations, want all 11", checked)
	}
}

// TestCredentialsBelongToTheAccount: members of one account share its
// API clients; a member of another account can't see them.
func TestCredentialsBelongToTheAccount(t *testing.T) {
	r, sender := newTestRouter(t)
	tokens := map[string]string{}
	for _, name := range []string{"ada", "bob", "cy"} {
		signUpVerifyAndSetPassword(t, r, sender, name+"@example.com", "a-strong-password")
		_, tokens[name] = signInForTokens(t, r, name+"@example.com", "a-strong-password", name+"-signin")
	}
	shared := uuid.NewString()
	for _, name := range []string{"ada", "bob"} {
		if status, body := doJSON(t, r, http.MethodPut, "/v1/auth/sessions/current/tenant", tokens[name],
			map[string]string{"tenant_id": shared}, name+"-select"); status != http.StatusOK {
			t.Fatalf("%s selecting the shared account: %d %v", name, status, body)
		}
	}
	selectTenant(t, r, tokens["cy"])

	stepUpViaEmail(t, r, sender, tokens["ada"])
	status, client := doJSON(t, r, http.MethodPost, "/v1/auth/api-clients", tokens["ada"],
		map[string]any{"name": "Shared", "environment": "sandbox", "scopes": []string{"sms:send"}}, "ada-create")
	if status != http.StatusOK {
		t.Fatalf("create: %d %v", status, client)
	}
	clientID, _ := client["id"].(string)

	_, list := doJSON(t, r, http.MethodGet, "/v1/auth/api-clients", tokens["bob"], nil, "")
	if clients, _ := list["clients"].([]any); len(clients) != 1 || clients[0].(map[string]any)["id"] != clientID {
		t.Fatalf("bob, in the same account, sees %v; want ada's client", list)
	}
	if status, body := doJSON(t, r, http.MethodGet, "/v1/auth/api-clients/"+clientID, tokens["cy"], nil, ""); status != http.StatusNotFound {
		t.Fatalf("cy, in another account, got %d %v; want 404", status, body)
	}
}
