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
	vasdb "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/testkit"
)

func newRouter(t *testing.T) (http.Handler, *vasdb.Pool) {
	t.Helper()
	ctx := context.Background()
	pg := testkit.StartPostgres(t, testkit.WithMigrations("../../../migrations/accounts"))
	pool, err := vasdb.NewPool(ctx, pg.DSN()+"&search_path=accounts,public")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	svc, err := accounts.New(accounts.Config{PacksDir: "../../../packs"}, pool, nil)
	if err != nil {
		t.Fatalf("accounts.New: %v", err)
	}
	return svc.Router(), pool
}

func do(t *testing.T, r http.Handler, method, path string, user uuid.UUID, key string, body any) (int, map[string]any) {
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
