//go:build integration

package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/accounts"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/accounts/domain"
	vasdb "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/testkit"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/identityusers"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/kycvendor"
)

func newRouter(t *testing.T) (http.Handler, *vasdb.Pool) {
	t.Helper()
	r, pool, _ := newRouterWithIdentity(t)
	return r, pool
}

// newRouterWithIdentity is newRouter that also returns the identity
// fake behind invitations (task 2.3b).
func newRouterWithIdentity(t *testing.T) (http.Handler, *vasdb.Pool, *identityusers.Fake) {
	t.Helper()
	r, pool, identity, _ := newRouterWithFakes(t)
	return r, pool, identity
}

const testKYCSecret = "kyc-webhook-test-secret" //nolint:gosec // G101: a fixture secret shared with the fake vendor, not a credential

// newRouterWithFakes is newRouter that also returns the identity fake
// (task 2.3b) and the fake KYC vendor (task 2.6).
func newRouterWithFakes(t *testing.T) (http.Handler, *vasdb.Pool, *identityusers.Fake, *kycvendor.Vendor) {
	t.Helper()
	identity := &identityusers.Fake{}
	vendor := kycvendor.New([]byte(testKYCSecret))
	t.Cleanup(vendor.Close)
	ctx := context.Background()
	pg := testkit.StartPostgres(t, testkit.WithMigrations("../../../migrations/accounts"))
	pool, err := vasdb.NewPool(ctx, pg.DSN()+"&search_path=accounts,public")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	svc, err := accounts.New(accounts.Config{
		PacksDir: "../../../packs", KYCProvider: "fake", KYCFakeURL: vendor.URL(), KYCWebhookSecret: testKYCSecret,
	}, pool, nil, identity)
	if err != nil {
		t.Fatalf("accounts.New: %v", err)
	}
	return svc.Router(), pool, identity, vendor
}

func do(t *testing.T, r http.Handler, method, path string, user uuid.UUID, key string, body any) (int, map[string]any) {
	t.Helper()
	return doInSession(t, r, method, path, user, uuid.Nil, key, body)
}

// doInSession is do with the session the gateway would also forward.
func doInSession(t *testing.T, r http.Handler, method, path string, user, session uuid.UUID, key string, body any) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if user != uuid.Nil {
		req.Header.Set("X-Vaspro-User-Id", user.String())
	}
	if session != uuid.Nil {
		req.Header.Set("X-Vaspro-Session-Id", session.String())
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	var decoded map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &decoded)
	return rec.Code, decoded
}

func TestAccountsAPI(t *testing.T) {
	r, pool := newRouter(t)
	ada, bob := uuid.New(), uuid.New()
	body := map[string]string{"display_name": "Acme", "country_code": "KE"}

	t.Run("without the gateway's user header it is 401 AUTH-4010", func(t *testing.T) {
		status, resp := do(t, r, http.MethodPost, "/v1/accounts/tenants", uuid.Nil, "k0", body)
		if status != http.StatusUnauthorized || resp["code"] != "AUTH-4010" {
			t.Fatalf("got %d %v", status, resp)
		}
	})

	status, created := do(t, r, http.MethodPost, "/v1/accounts/tenants", ada, "create-1", body)
	if status != http.StatusCreated || created["tier"] != "sandbox" || created["type"] != "direct" {
		t.Fatalf("create: got %d %v", status, created)
	}
	id, _ := created["id"].(string)

	t.Run("replaying the Idempotency-Key returns the same account, not a second one", func(t *testing.T) {
		status, again := do(t, r, http.MethodPost, "/v1/accounts/tenants", ada, "create-1", body)
		if status != http.StatusCreated || again["id"] != id {
			t.Fatalf("got %d %v, want the first account %s", status, again, id)
		}
		var n int
		if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM tenants`).Scan(&n); err != nil || n != 1 {
			t.Fatalf("%d tenants, want 1 (%v)", n, err)
		}
	})
	t.Run("another user's identical key is their own", func(t *testing.T) {
		status, other := do(t, r, http.MethodPost, "/v1/accounts/tenants", bob, "create-1", body)
		if status != http.StatusCreated || other["id"] == id {
			t.Fatalf("got %d %v, want a new account for bob", status, other)
		}
	})
	t.Run("the member reads it", func(t *testing.T) {
		status, got := do(t, r, http.MethodGet, "/v1/accounts/tenants/"+id, ada, "", nil)
		if status != http.StatusOK || got["id"] != id {
			t.Fatalf("got %d %v", status, got)
		}
	})
	t.Run("a non-member gets 404 VAL-4003", func(t *testing.T) {
		status, got := do(t, r, http.MethodGet, "/v1/accounts/tenants/"+id, bob, "", nil)
		if status != http.StatusNotFound || got["code"] != "VAL-4003" {
			t.Fatalf("got %d %v", status, got)
		}
	})
	t.Run("an unsupported country is 400 VAL-4000", func(t *testing.T) {
		status, got := do(t, r, http.MethodPost, "/v1/accounts/tenants", ada, "create-zz",
			map[string]string{"display_name": "Acme", "country_code": "ZZ"})
		if status != http.StatusBadRequest || got["code"] != "VAL-4000" {
			t.Fatalf("got %d %v", status, got)
		}
	})
}

// makeReseller stands in for staff (task 2.9 adds the route): it moves
// tenant to financial with its tier_changes row, then to reseller, in
// one transaction as the tier trigger requires.
func makeReseller(t *testing.T, pool *vasdb.Pool, tenant string) {
	t.Helper()
	err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(context.Background(), `UPDATE tenants SET tier = 'financial', type = 'reseller' WHERE id = $1`, tenant); err != nil {
			return err
		}
		_, err := tx.Exec(context.Background(), `
			INSERT INTO tier_changes (id, tenant_id, from_tier, to_tier, reason, changed_by)
			VALUES ($1, $2, 'sandbox', 'financial', 'fixture', $3)`, uuid.New(), tenant, uuid.New())
		return err
	})
	if err != nil {
		t.Fatalf("making %s a reseller: %v", tenant, err)
	}
}

func TestSubAccountsAPI(t *testing.T) {
	r, pool := newRouter(t)
	ada, bob := uuid.New(), uuid.New()

	_, reseller := do(t, r, http.MethodPost, "/v1/accounts/tenants", ada, "reseller", map[string]string{"display_name": "Agency", "country_code": "KE"})
	resellerID, _ := reseller["id"].(string)
	makeReseller(t, pool, resellerID)
	_, direct := do(t, r, http.MethodPost, "/v1/accounts/tenants", ada, "direct", map[string]string{"display_name": "Plain", "country_code": "KE"})
	directID, _ := direct["id"].(string)
	subsPath := "/v1/accounts/tenants/" + resellerID + "/sub-accounts"
	body := map[string]string{"display_name": "Kilimo Traders"}

	status, sub := do(t, r, http.MethodPost, subsPath, ada, "sub-1", body)
	if status != http.StatusCreated || sub["type"] != "sub_account" || sub["parent_tenant_id"] != resellerID ||
		sub["tier"] != "sandbox" || sub["default_currency"] != "KES" {
		t.Fatalf("create sub-account: got %d %v", status, sub)
	}
	subID, _ := sub["id"].(string)

	t.Run("replaying the Idempotency-Key returns the same sub-account", func(t *testing.T) {
		status, again := do(t, r, http.MethodPost, subsPath, ada, "sub-1", body)
		if status != http.StatusCreated || again["id"] != subID {
			t.Fatalf("got %d %v, want %s", status, again, subID)
		}
	})
	t.Run("the reseller's member reads the sub-account", func(t *testing.T) {
		status, got := do(t, r, http.MethodGet, "/v1/accounts/tenants/"+subID, ada, "", nil)
		if status != http.StatusOK || got["id"] != subID {
			t.Fatalf("got %d %v", status, got)
		}
	})
	t.Run("listing returns it", func(t *testing.T) {
		status, got := do(t, r, http.MethodGet, subsPath, ada, "", nil)
		list, _ := got["sub_accounts"].([]any)
		if status != http.StatusOK || len(list) != 1 || list[0].(map[string]any)["id"] != subID {
			t.Fatalf("got %d %v", status, got)
		}
	})
	t.Run("a non-member gets 404 VAL-4003 for create, list and read", func(t *testing.T) {
		for _, c := range []struct{ method, path, key string }{
			{http.MethodPost, subsPath, "bob-sub"},
			{http.MethodGet, subsPath, ""},
			{http.MethodGet, "/v1/accounts/tenants/" + subID, ""},
		} {
			var b any
			if c.method == http.MethodPost {
				b = body
			}
			if status, got := do(t, r, c.method, c.path, bob, c.key, b); status != http.StatusNotFound || got["code"] != "VAL-4003" {
				t.Fatalf("%s %s: got %d %v", c.method, c.path, status, got)
			}
		}
	})
	t.Run("a direct account is 403 KYC-4021", func(t *testing.T) {
		status, got := do(t, r, http.MethodPost, "/v1/accounts/tenants/"+directID+"/sub-accounts", ada, "direct-sub", body)
		if status != http.StatusForbidden || got["code"] != "KYC-4021" {
			t.Fatalf("got %d %v", status, got)
		}
	})
	t.Run("a blank display name is 400 VAL-4000", func(t *testing.T) {
		status, got := do(t, r, http.MethodPost, subsPath, ada, "no-name", map[string]string{"display_name": " "})
		if status != http.StatusBadRequest || got["code"] != "VAL-4000" {
			t.Fatalf("got %d %v", status, got)
		}
	})
	t.Run("a reseller below financial is 403 KYC-4020", func(t *testing.T) {
		err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(context.Background(), `UPDATE tenants SET tier = 'business' WHERE id = $1`, resellerID); err != nil {
				return err
			}
			_, err := tx.Exec(context.Background(), `
				INSERT INTO tier_changes (id, tenant_id, from_tier, to_tier, reason, changed_by)
				VALUES ($1, $2, 'financial', 'business', 'fixture', $3)`, uuid.New(), resellerID, uuid.New())
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		status, got := do(t, r, http.MethodPost, subsPath, ada, "sub-after-demotion", body)
		if status != http.StatusForbidden || got["code"] != "KYC-4020" {
			t.Fatalf("got %d %v", status, got)
		}
	})
}

func addMember(t *testing.T, pool *vasdb.Pool, tenant string, user uuid.UUID, role uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO memberships (tenant_id, user_id, role_id, status) VALUES ($1, $2, $3, 'active')`, tenant, user, role); err != nil {
		t.Fatal(err)
	}
}

func TestMembersAPI(t *testing.T) {
	r, pool := newRouter(t)
	admin, viewer, dev, stranger := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	_, created := do(t, r, http.MethodPost, "/v1/accounts/tenants", admin, "create", map[string]string{"display_name": "Team", "country_code": "KE"})
	tenant, _ := created["id"].(string)
	addMember(t, pool, tenant, viewer, domain.BuiltinViewerRoleID)
	addMember(t, pool, tenant, dev, domain.BuiltinDeveloperRoleID)
	membersPath := "/v1/accounts/tenants/" + tenant + "/members"

	t.Run("roles lists the four built-in roles", func(t *testing.T) {
		status, got := do(t, r, http.MethodGet, "/v1/accounts/roles", viewer, "", nil)
		roles, _ := got["roles"].([]any)
		if status != http.StatusOK || len(roles) != 4 {
			t.Fatalf("got %d %v", status, got)
		}
	})
	t.Run("a Viewer lists the members", func(t *testing.T) {
		status, got := do(t, r, http.MethodGet, membersPath, viewer, "", nil)
		members, _ := got["members"].([]any)
		if status != http.StatusOK || len(members) != 3 {
			t.Fatalf("got %d %v", status, got)
		}
	})
	t.Run("a stranger gets 404 VAL-4003", func(t *testing.T) {
		if status, got := do(t, r, http.MethodGet, membersPath, stranger, "", nil); status != http.StatusNotFound || got["code"] != "VAL-4003" {
			t.Fatalf("got %d %v", status, got)
		}
	})
	t.Run("a Viewer can't change roles: 403 AUTH-4024", func(t *testing.T) {
		status, got := do(t, r, http.MethodPatch, membersPath+"/"+dev.String(), viewer, "viewer-patch",
			map[string]string{"role_id": domain.BuiltinAdminRoleID.String()})
		if status != http.StatusForbidden || got["code"] != "AUTH-4024" {
			t.Fatalf("got %d %v", status, got)
		}
	})
	t.Run("the last Admin can't step down: 409 VAL-4004", func(t *testing.T) {
		status, got := do(t, r, http.MethodPatch, membersPath+"/"+admin.String(), admin, "admin-demote",
			map[string]string{"role_id": domain.BuiltinViewerRoleID.String()})
		if status != http.StatusConflict || got["code"] != "VAL-4004" {
			t.Fatalf("got %d %v", status, got)
		}
		if status, got := do(t, r, http.MethodDelete, membersPath+"/"+admin.String(), admin, "admin-leave", nil); status != http.StatusConflict || got["code"] != "VAL-4004" {
			t.Fatalf("leaving: got %d %v", status, got)
		}
	})
	t.Run("a role that isn't built in is 400 VAL-4000", func(t *testing.T) {
		status, got := do(t, r, http.MethodPatch, membersPath+"/"+dev.String(), admin, "custom-role",
			map[string]string{"role_id": uuid.NewString()})
		if status != http.StatusBadRequest || got["code"] != "VAL-4000" {
			t.Fatalf("got %d %v", status, got)
		}
	})
	t.Run("an Admin makes the Developer a Viewer", func(t *testing.T) {
		status, got := do(t, r, http.MethodPatch, membersPath+"/"+dev.String(), admin, "dev-to-viewer",
			map[string]string{"role_id": domain.BuiltinViewerRoleID.String()})
		if status != http.StatusOK || got["role_id"] != domain.BuiltinViewerRoleID.String() {
			t.Fatalf("got %d %v", status, got)
		}
	})
	t.Run("a member leaves, then can't read the account", func(t *testing.T) {
		if status, got := do(t, r, http.MethodDelete, membersPath+"/"+viewer.String(), viewer, "viewer-leave", nil); status != http.StatusNoContent {
			t.Fatalf("got %d %v", status, got)
		}
		if status, _ := do(t, r, http.MethodGet, "/v1/accounts/tenants/"+tenant, viewer, "", nil); status != http.StatusNotFound {
			t.Fatalf("after leaving: got %d", status)
		}
	})
	t.Run("my memberships", func(t *testing.T) {
		status, got := do(t, r, http.MethodGet, "/v1/accounts/me/memberships", admin, "", nil)
		list, _ := got["memberships"].([]any)
		if status != http.StatusOK || len(list) != 1 || list[0].(map[string]any)["tenant_id"] != tenant ||
			list[0].(map[string]any)["role_id"] != domain.BuiltinAdminRoleID.String() {
			t.Fatalf("got %d %v", status, got)
		}
	})
}

func TestInvitationsAPI(t *testing.T) {
	r, pool, identity := newRouterWithIdentity(t)
	admin := identity.AddUser("ada@example.com", "Ada Lovelace")
	viewer := identity.AddUser("vic@example.com", "Vic")
	_, created := do(t, r, http.MethodPost, "/v1/accounts/tenants", admin, "create", map[string]string{"display_name": "Team", "country_code": "KE"})
	tenant, _ := created["id"].(string)
	addMember(t, pool, tenant, viewer, domain.BuiltinViewerRoleID)
	invitePath := "/v1/accounts/tenants/" + tenant + "/invitations"
	invite := map[string]string{"email": "bob@example.com", "role_id": domain.BuiltinDeveloperRoleID.String()}

	t.Run("without a fresh step-up it is 401 AUTH-4012", func(t *testing.T) {
		status, got := doInSession(t, r, http.MethodPost, invitePath, admin, uuid.New(), "stale", invite)
		if status != http.StatusUnauthorized || got["code"] != "AUTH-4012" {
			t.Fatalf("got %d %v", status, got)
		}
	})
	session := uuid.New()
	identity.StepUp(admin, session)
	t.Run("a Viewer is 403 AUTH-4024", func(t *testing.T) {
		vs := uuid.New()
		identity.StepUp(viewer, vs)
		status, got := doInSession(t, r, http.MethodPost, invitePath, viewer, vs, "viewer-invite", invite)
		if status != http.StatusForbidden || got["code"] != "AUTH-4024" {
			t.Fatalf("got %d %v", status, got)
		}
	})
	t.Run("an Admin invites: 201, invited, with the email", func(t *testing.T) {
		status, got := doInSession(t, r, http.MethodPost, invitePath, admin, session, "invite-bob", invite)
		if status != http.StatusCreated || got["status"] != "invited" || got["email"] != "bob@example.com" {
			t.Fatalf("got %d %v", status, got)
		}
	})
	t.Run("an active member is 409 VAL-4005", func(t *testing.T) {
		status, got := doInSession(t, r, http.MethodPost, invitePath, admin, session, "invite-vic",
			map[string]string{"email": "vic@example.com", "role_id": domain.BuiltinViewerRoleID.String()})
		if status != http.StatusConflict || got["code"] != "VAL-4005" {
			t.Fatalf("got %d %v", status, got)
		}
	})
	t.Run("a malformed address is 400 VAL-4000", func(t *testing.T) {
		status, got := doInSession(t, r, http.MethodPost, invitePath, admin, session, "invite-bad",
			map[string]string{"email": "nope", "role_id": domain.BuiltinViewerRoleID.String()})
		if status != http.StatusBadRequest || got["code"] != "VAL-4000" {
			t.Fatalf("got %d %v", status, got)
		}
	})
	t.Run("the invitee accepts, then reads the account; accepting again is 404", func(t *testing.T) {
		_, mine := do(t, r, http.MethodGet, "/v1/accounts/tenants/"+tenant+"/members", admin, "", nil)
		var bobID string
		for _, m := range mine["members"].([]any) {
			if m.(map[string]any)["email"] == "bob@example.com" {
				bobID, _ = m.(map[string]any)["user_id"].(string)
			}
		}
		bobUUID := uuid.MustParse(bobID)
		acceptPath := "/v1/accounts/me/memberships/" + tenant + "/acceptance"
		if status, got := do(t, r, http.MethodPost, acceptPath, bobUUID, "accept", nil); status != http.StatusOK || got["status"] != "active" {
			t.Fatalf("got %d %v", status, got)
		}
		if status, _ := do(t, r, http.MethodGet, "/v1/accounts/tenants/"+tenant, bobUUID, "", nil); status != http.StatusOK {
			t.Fatalf("reading the account after accepting: %d", status)
		}
		if status, got := do(t, r, http.MethodPost, acceptPath, bobUUID, "accept-again", nil); status != http.StatusNotFound || got["code"] != "VAL-4003" {
			t.Fatalf("accepting again: got %d %v", status, got)
		}
	})
	t.Run("members carry names and emails", func(t *testing.T) {
		_, got := do(t, r, http.MethodGet, "/v1/accounts/tenants/"+tenant+"/members", admin, "", nil)
		for _, m := range got["members"].([]any) {
			if m.(map[string]any)["email"] == nil {
				t.Fatalf("a member without an email: %v", got)
			}
		}
	})
}

func TestServicesAPI(t *testing.T) {
	r, pool := newRouter(t)
	admin, viewer, stranger := uuid.New(), uuid.New(), uuid.New()
	_, created := do(t, r, http.MethodPost, "/v1/accounts/tenants", admin, "create", map[string]string{"display_name": "Team", "country_code": "KE"})
	tenant, _ := created["id"].(string)
	addMember(t, pool, tenant, viewer, domain.BuiltinViewerRoleID)
	servicesPath := "/v1/accounts/tenants/" + tenant + "/services"

	status, got := do(t, r, http.MethodPut, servicesPath+"/sms", admin, "select-sms", nil)
	if status != http.StatusOK || got["selected"] != true || got["locked"] != true || got["min_tier"] != "starter" || got["status"] != "selected" {
		t.Fatalf("selecting sms in the sandbox: got %d %v", status, got)
	}
	status, got = do(t, r, http.MethodGet, servicesPath, viewer, "", nil)
	services, _ := got["services"].([]any)
	if status != http.StatusOK || got["tier"] != "sandbox" || got["required_tier"] != "starter" || len(services) != 7 {
		t.Fatalf("the catalogue: got %d %v", status, got)
	}
	for _, s := range services {
		if m := s.(map[string]any); m["code"] == "ussd" && (m["selected"] != false || m["status"] != nil) {
			t.Fatalf("an unselected service: %v", m)
		}
	}
	cases := []struct {
		name   string
		method string
		path   string
		user   uuid.UUID
		status int
		code   string
	}{
		{"a Viewer can't select", http.MethodPut, servicesPath + "/ussd", viewer, http.StatusForbidden, "AUTH-4024"},
		{"an unknown service", http.MethodPut, servicesPath + "/voice", admin, http.StatusNotFound, "VAL-4003"},
		{"a stranger", http.MethodGet, servicesPath, stranger, http.StatusNotFound, "VAL-4003"},
		{"deselecting what isn't selected", http.MethodDelete, servicesPath + "/ussd", admin, http.StatusNotFound, "VAL-4003"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if status, got := do(t, r, tc.method, tc.path, tc.user, "k-"+tc.name, nil); status != tc.status || got["code"] != tc.code {
				t.Fatalf("got %d %v, want %d %s", status, got, tc.status, tc.code)
			}
		})
	}
	if status, _ := do(t, r, http.MethodDelete, servicesPath+"/sms", admin, "deselect-sms", nil); status != http.StatusNoContent {
		t.Fatalf("deselecting sms: got %d", status)
	}
}

func TestKYCAPI(t *testing.T) {
	r, pool, _, vendor := newRouterWithFakes(t)
	admin, viewer := uuid.New(), uuid.New()
	_, created := do(t, r, http.MethodPost, "/v1/accounts/tenants", admin, "create", map[string]string{"display_name": "Team", "country_code": "KE"})
	tenant, _ := created["id"].(string)
	addMember(t, pool, tenant, viewer, domain.BuiltinViewerRoleID)
	checksPath := "/v1/accounts/tenants/" + tenant + "/kyc-checks"

	if status, got := do(t, r, http.MethodPost, checksPath, viewer, "viewer-kyc", map[string]string{"kind": "individual"}); status != http.StatusForbidden || got["code"] != "AUTH-4024" {
		t.Fatalf("a Viewer starting KYC: got %d %v", status, got)
	}
	status, check := do(t, r, http.MethodPost, checksPath, admin, "start-kyc", map[string]string{"kind": "individual"})
	if status != http.StatusCreated || check["status"] != "pending" || check["url"] == nil {
		t.Fatalf("starting KYC: got %d %v", status, check)
	}
	checkID, _ := check["id"].(string)
	var ref string
	if err := pool.QueryRow(context.Background(), `SELECT provider_ref FROM kyc_checks WHERE id = $1`, checkID).Scan(&ref); err != nil {
		t.Fatal(err)
	}

	// The webhook route: no user header and no Idempotency-Key, as a
	// vendor sends it.
	post := func(provider string, tamper kycvendor.Tamper) (int, map[string]any) {
		t.Helper()
		header, body := vendor.Webhook(ref, "passed", tamper)
		req := httptest.NewRequest(http.MethodPost, "/v1/accounts/kyc/webhooks/"+provider, bytes.NewReader(body))
		req.Header = header
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		var decoded map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &decoded)
		return rec.Code, decoded
	}
	if status, got := post("fake", kycvendor.TamperBody); status != http.StatusUnauthorized || got["code"] != "AUTH-4013" {
		t.Fatalf("a tampered webhook: got %d %v, want 401 AUTH-4013", status, got)
	}
	if status, got := post("acme-kyc", kycvendor.NoTamper); status != http.StatusNotFound {
		t.Fatalf("another provider: got %d %v, want 404", status, got)
	}
	if status, got := post("fake", kycvendor.NoTamper); status != http.StatusOK {
		t.Fatalf("a signed webhook: got %d %v, want 200", status, got)
	}
	if status, got := post("fake", kycvendor.NoTamper); status != http.StatusOK {
		t.Fatalf("the same webhook again: got %d %v, want 200", status, got)
	}

	status, got := do(t, r, http.MethodGet, checksPath, viewer, "", nil)
	checks, _ := got["checks"].([]any)
	if status != http.StatusOK || len(checks) != 1 || checks[0].(map[string]any)["status"] != "passed" || checks[0].(map[string]any)["url"] != nil {
		t.Fatalf("listing checks: got %d %v", status, got)
	}
	if _, account := do(t, r, http.MethodGet, "/v1/accounts/tenants/"+tenant, admin, "", nil); account["tier"] != "starter" {
		t.Fatalf("the account after a passed check: %v, want starter", account)
	}
}
