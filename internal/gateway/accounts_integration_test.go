//go:build integration

package gateway

import (
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/testkit"
)

// startRealAccounts builds cmd/accounts and runs it as a subprocess
// against its own Postgres and NATS, pointed at identity's gRPC API on
// identityGRPCAddr (task 2.3b), returning its HTTP base URL and its
// gRPC address (identity's IDENTITY_ACCOUNTS_GRPC_ADDR, task 2.2c).
// The two services dial each other lazily, so either may start first.
func startRealAccounts(t *testing.T, identityGRPCAddr string) (baseURL, grpcAddr string) {
	t.Helper()
	root := repoRoot(t)

	binPath := filepath.Join(t.TempDir(), "accounts")
	build := exec.Command("go", "build", "-o", binPath, "./cmd/accounts") //nolint:gosec // fixed "go build" invocation; binPath is our own t.TempDir() path
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/accounts failed: %v\n%s", err, out)
	}

	pg := testkit.StartPostgres(t, testkit.WithMigrations(filepath.Join(root, "migrations/accounts")))
	natsURL := testkit.StartNATSURL(t)
	httpAddr := freeTCPAddr(t)
	grpcAddr = freeTCPAddr(t)

	cmd := exec.Command(binPath) //nolint:gosec // binPath is the binary we just built into our own t.TempDir()
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"ACCOUNTS_DATABASE_URL="+pg.DSN()+"&search_path=accounts,public",
		"ACCOUNTS_NATS_URL="+natsURL,
		"ACCOUNTS_HTTP_ADDR="+httpAddr,
		"ACCOUNTS_GRPC_ADDR="+grpcAddr,
		"ACCOUNTS_PACKS_DIR="+filepath.Join(root, "packs"),
		"ACCOUNTS_IDENTITY_GRPC_ADDR="+identityGRPCAddr,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting accounts subprocess: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	baseURL = "http://" + httpAddr
	waitForHealthy(t, baseURL, 20*time.Second)
	return baseURL, grpcAddr
}

// TestGateway_CreateAccountThroughGateway_EndToEnd is task 2.2a's e2e:
// a user signs up and signs in through identity, then creates their
// client account through the gateway, which checks the session over
// identity's gRPC API and forwards the user to accounts. A second
// user can't read the first user's account.
func TestGateway_CreateAccountThroughGateway_EndToEnd(t *testing.T) {
	smtp := startFakeSMTPServer(t)
	var gatewayLogs syncBuffer
	identityGRPC := freeTCPAddr(t)
	accountsBaseURL, accountsGRPC := startRealAccounts(t, identityGRPC)
	identityBaseURL := startRealIdentityWithLogs(t, smtp.Addr(), nil,
		"IDENTITY_ACCOUNTS_GRPC_ADDR="+accountsGRPC, "IDENTITY_GRPC_ADDR="+identityGRPC)
	r := newTestGatewayWithAccounts(t, identityBaseURL, accountsBaseURL, nil,
		slog.New(slog.NewJSONHandler(&gatewayLogs, nil)))

	_, adaToken := signUpVerifySetPasswordAndSignIn(t, r, smtp, "ada@example.com")
	_, bobToken := signUpVerifySetPasswordAndSignIn(t, r, smtp, "bob@example.com")

	status, created := gwDoJSON(t, r, http.MethodPost, "/v1/accounts/tenants", adaToken,
		map[string]string{"display_name": "Acme Deliveries", "country_code": "KE", "billing_email": "billing@acme.example"}, "create-account")
	if status != http.StatusCreated {
		t.Fatalf("create account: got %d %v", status, created)
	}
	if created["tier"] != "sandbox" || created["type"] != "direct" || created["timezone"] != "Africa/Nairobi" ||
		created["default_currency"] != "KES" || created["region"] != "ke" {
		t.Fatalf("got account %v", created)
	}
	id, _ := created["id"].(string)

	status, got := gwDoJSON(t, r, http.MethodGet, "/v1/accounts/tenants/"+id, adaToken, nil, "")
	if status != http.StatusOK || got["display_name"] != "Acme Deliveries" {
		t.Fatalf("read own account: got %d %v", status, got)
	}
	status, got = gwDoJSON(t, r, http.MethodGet, "/v1/accounts/tenants/"+id, bobToken, nil, "")
	if status != http.StatusNotFound || got["code"] != "VAL-4003" {
		t.Fatalf("another user's account: got %d %v, want 404 VAL-4003", status, got)
	}
	// Task 2.2c: identity asks the real accounts service who may select
	// the account for a session.
	status, got = gwDoJSON(t, r, http.MethodPut, "/v1/auth/sessions/current/tenant", adaToken,
		map[string]string{"tenant_id": id}, "ada-select")
	if status != http.StatusOK || got["tenant_id"] != id {
		t.Fatalf("ada selecting her account: got %d %v", status, got)
	}
	status, got = gwDoJSON(t, r, http.MethodPut, "/v1/auth/sessions/current/tenant", bobToken,
		map[string]string{"tenant_id": id}, "bob-select")
	if status != http.StatusNotFound || got["code"] != "VAL-4003" {
		t.Fatalf("bob selecting ada's account: got %d %v, want 404 VAL-4003", status, got)
	}

	// Task 2.3a: the creator is the account's only member and its Admin,
	// and can't leave it without one.
	status, got = gwDoJSON(t, r, http.MethodGet, "/v1/accounts/tenants/"+id+"/members", adaToken, nil, "")
	members, _ := got["members"].([]any)
	if status != http.StatusOK || len(members) != 1 || members[0].(map[string]any)["role_id"] != "019a0000-0000-7000-8000-000000000001" {
		t.Fatalf("members: got %d %v, want ada alone as Admin", status, got)
	}
	adaID, _ := members[0].(map[string]any)["user_id"].(string)
	status, got = gwDoJSON(t, r, http.MethodDelete, "/v1/accounts/tenants/"+id+"/members/"+adaID, adaToken, nil, "ada-leave")
	if status != http.StatusConflict || got["code"] != "VAL-4004" {
		t.Fatalf("the last Admin leaving: got %d %v, want 409 VAL-4004", status, got)
	}
	status, got = gwDoJSON(t, r, http.MethodGet, "/v1/accounts/me/memberships", adaToken, nil, "")
	if mine, _ := got["memberships"].([]any); status != http.StatusOK || len(mine) != 1 || mine[0].(map[string]any)["tenant_id"] != id {
		t.Fatalf("ada's memberships: got %d %v", status, got)
	}

	status, got = gwDoJSON(t, r, http.MethodGet, "/v1/accounts/tenants/"+id, "sess_not-a-token", nil, "")
	if status != http.StatusUnauthorized || got["code"] != "AUTH-4010" {
		t.Fatalf("garbage session: got %d %v, want 401 AUTH-4010", status, got)
	}

	for name, secret := range map[string]string{"ada's session token": adaToken, "bob's session token": bobToken} {
		if strings.Contains(gatewayLogs.String(), secret) {
			t.Errorf("the gateway log contains %s", name)
		}
	}
}

// TestGateway_InviteThroughGateway_EndToEnd is task 2.3b's e2e, with
// real identity and accounts: ada invites bob (who has a login) and
// carol (who doesn't); both get the same invitation email; bob accepts
// and uses the account's credentials; carol signs up with the invited
// address and accepts.
func TestGateway_InviteThroughGateway_EndToEnd(t *testing.T) {
	smtp := startFakeSMTPServer(t)
	identityGRPC := freeTCPAddr(t)
	accountsBaseURL, accountsGRPC := startRealAccounts(t, identityGRPC)
	var identityLogs, gatewayLogs syncBuffer
	identityBaseURL := startRealIdentityWithLogs(t, smtp.Addr(), &identityLogs,
		"IDENTITY_ACCOUNTS_GRPC_ADDR="+accountsGRPC, "IDENTITY_GRPC_ADDR="+identityGRPC)
	r := newTestGatewayWithAccounts(t, identityBaseURL, accountsBaseURL, nil,
		slog.New(slog.NewJSONHandler(&gatewayLogs, nil)))

	_, ada := signUpVerifySetPasswordAndSignIn(t, r, smtp, "ada@example.com")
	_, bob := signUpVerifySetPasswordAndSignIn(t, r, smtp, "bob@example.com")
	status, account := gwDoJSON(t, r, http.MethodPost, "/v1/accounts/tenants", ada,
		map[string]string{"display_name": "Acme Deliveries", "country_code": "KE"}, "create-account")
	if status != http.StatusCreated {
		t.Fatalf("create account: %d %v", status, account)
	}
	id, _ := account["id"].(string)
	invitePath := "/v1/accounts/tenants/" + id + "/invitations"
	const developer = "019a0000-0000-7000-8000-000000000002"

	status, got := gwDoJSON(t, r, http.MethodPost, invitePath, ada,
		map[string]string{"email": "bob@example.com", "role_id": developer}, "invite-bob-stale")
	if status != http.StatusUnauthorized || got["code"] != "AUTH-4012" {
		t.Fatalf("inviting without a step-up: got %d %v, want 401 AUTH-4012", status, got)
	}
	stepUp(t, r, smtp, ada)

	invitations := map[string]string{}
	for _, who := range []string{"bob@example.com", "carol@example.com"} {
		before := smtp.count()
		status, got := gwDoJSON(t, r, http.MethodPost, invitePath, ada,
			map[string]string{"email": who, "role_id": developer}, "invite-"+who)
		if status != http.StatusCreated || got["status"] != "invited" {
			t.Fatalf("inviting %s: got %d %v", who, status, got)
		}
		invitations[who] = smtp.waitForNext(t, before)
		if !strings.Contains(invitations[who], "Acme Deliveries") || !strings.Contains(invitations[who], "To: "+who) {
			t.Fatalf("the invitation to %s: %s", who, invitations[who])
		}
	}

	// bob, who already has a login, sees the invitation and accepts.
	status, got = gwDoJSON(t, r, http.MethodGet, "/v1/accounts/me/memberships", bob, nil, "")
	if mine, _ := got["memberships"].([]any); status != http.StatusOK || len(mine) != 1 || mine[0].(map[string]any)["status"] != "invited" {
		t.Fatalf("bob's memberships: got %d %v", status, got)
	}
	if status, got := gwDoJSON(t, r, http.MethodPost, "/v1/accounts/me/memberships/"+id+"/acceptance", bob, nil, "bob-accept"); status != http.StatusOK || got["status"] != "active" {
		t.Fatalf("bob accepting: got %d %v", status, got)
	}
	if status, got := gwDoJSON(t, r, http.MethodPut, "/v1/auth/sessions/current/tenant", bob,
		map[string]string{"tenant_id": id}, "bob-select"); status != http.StatusOK {
		t.Fatalf("bob selecting the account: got %d %v", status, got)
	}
	stepUp(t, r, smtp, bob)
	if status, got := gwDoJSON(t, r, http.MethodPost, "/v1/auth/api-clients", bob,
		map[string]any{"name": "bob's client", "environment": "sandbox", "scopes": []string{"sms:send"}}, "bob-client"); status != http.StatusOK {
		t.Fatalf("bob, a Developer, creating an API client: got %d %v", status, got)
	}

	// carol, who had no login, signs up with the invited address.
	_, carol := signUpVerifySetPasswordAndSignIn(t, r, smtp, "carol@example.com")
	if status, got := gwDoJSON(t, r, http.MethodPost, "/v1/accounts/me/memberships/"+id+"/acceptance", carol, nil, "carol-accept"); status != http.StatusOK {
		t.Fatalf("carol accepting: got %d %v", status, got)
	}

	status, got = gwDoJSON(t, r, http.MethodGet, "/v1/accounts/tenants/"+id+"/members", ada, nil, "")
	members, _ := got["members"].([]any)
	emails := map[string]string{}
	for _, m := range members {
		mm := m.(map[string]any)
		email, _ := mm["email"].(string)
		emails[email], _ = mm["status"].(string)
	}
	if status != http.StatusOK || len(members) != 3 || emails["ada@example.com"] != "active" ||
		emails["bob@example.com"] != "active" || emails["carol@example.com"] != "active" {
		t.Fatalf("ada's member list: got %d %v", status, got)
	}

	for name, secret := range map[string]string{"ada's session token": ada, "bob's session token": bob, "carol's session token": carol} {
		if strings.Contains(gatewayLogs.String(), secret) || strings.Contains(identityLogs.String(), secret) {
			t.Errorf("a log contains %s", name)
		}
	}
}
