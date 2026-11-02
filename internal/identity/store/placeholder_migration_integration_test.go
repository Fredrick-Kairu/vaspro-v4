//go:build integration

package store_test

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib" // the "pgx" database/sql driver goose runs on
	"github.com/pressly/goose/v3"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/testkit"
)

// placeholderMigration is task 2.2c's revocation of Phase 1's
// placeholder credentials.
const placeholderMigration = 20261102120000

// TestRevokePlaceholderCredentials_RevokesEveryRowCreatedBefore applies
// identity's migrations up to the one before 2.2c's, inserts active
// credentials the way Phase 1 did (tenant_id = the user's ID), then
// runs 2.2c's migration as the tables' owner, a role that isn't a
// superuser, the way a production migration role might be. FORCE ROW
// LEVEL SECURITY would make its updates match nothing for that role if
// the migration didn't lift it.
func TestRevokePlaceholderCredentials_RevokesEveryRowCreatedBefore(t *testing.T) {
	ctx := context.Background()
	pg := testkit.StartPostgres(t)

	superDB, err := sql.Open("pgx", pg.DSN())
	if err != nil {
		t.Fatal(err)
	}
	defer superDB.Close()
	provider, err := goose.NewProvider(goose.DialectPostgres, superDB, os.DirFS("../../../migrations/identity"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, placeholderMigration-1); err != nil {
		t.Fatalf("UpTo before 2.2c: %v", err)
	}

	user, client, secret, smpp := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	revokedClient := uuid.New()
	setup := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO identity.api_clients (id, tenant_id, name, environment, scopes, created_by) VALUES ($1, $2, 'placeholder', 'sandbox', '{}', $2)`, []any{client, user}},
		{`INSERT INTO identity.api_clients (id, tenant_id, name, environment, scopes, created_by, revoked_at) VALUES ($1, $2, 'already revoked', 'sandbox', '{}', $2, '2026-10-01T00:00:00Z')`, []any{revokedClient, user}},
		{`INSERT INTO identity.api_client_secrets (id, api_client_id, secret_hash, last4) VALUES ($1, $2, 'hash', 'abcd')`, []any{secret, client}},
		{`INSERT INTO identity.smpp_accounts (id, tenant_id, system_id, password_hash, ip_allowlist, max_binds, max_tps, environment, status) VALUES ($1, $2, 'sys1', 'hash', '{10.0.0.0/8}', 1, 10, 'sandbox', 'active')`, []any{smpp, user}},
		{`CREATE ROLE identity_owner LOGIN PASSWORD 'identity_owner'`, nil},
		{`GRANT USAGE, CREATE ON SCHEMA identity TO identity_owner`, nil},
		{`ALTER TABLE identity.api_clients OWNER TO identity_owner`, nil},
		{`ALTER TABLE identity.api_client_secrets OWNER TO identity_owner`, nil},
		{`ALTER TABLE identity.smpp_accounts OWNER TO identity_owner`, nil},
		{`GRANT ALL ON public.goose_db_version TO identity_owner`, nil},
		{`GRANT USAGE ON ALL SEQUENCES IN SCHEMA public TO identity_owner`, nil},
	}
	for _, s := range setup {
		if _, err := superDB.ExecContext(ctx, s.sql, s.args...); err != nil {
			t.Fatalf("%s: %v", s.sql, err)
		}
	}

	ownerDB, err := sql.Open("pgx", pg.RoleDSN("identity_owner", "identity_owner"))
	if err != nil {
		t.Fatal(err)
	}
	defer ownerDB.Close()
	ownerProvider, err := goose.NewProvider(goose.DialectPostgres, ownerDB, os.DirFS("../../../migrations/identity"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ownerProvider.UpTo(ctx, placeholderMigration); err != nil {
		t.Fatalf("2.2c's migration as the owner: %v", err)
	}

	var clientRevoked, secretRevoked bool
	var smppStatus, revokedAt string
	checks := []struct {
		sql  string
		arg  uuid.UUID
		dest any
	}{
		{`SELECT revoked_at IS NOT NULL FROM identity.api_clients WHERE id = $1`, client, &clientRevoked},
		{`SELECT revoked_at IS NOT NULL FROM identity.api_client_secrets WHERE id = $1`, secret, &secretRevoked},
		{`SELECT status FROM identity.smpp_accounts WHERE id = $1`, smpp, &smppStatus},
		{`SELECT revoked_at::text FROM identity.api_clients WHERE id = $1`, revokedClient, &revokedAt},
	}
	for _, c := range checks {
		if err := superDB.QueryRowContext(ctx, c.sql, c.arg).Scan(c.dest); err != nil {
			t.Fatalf("%s: %v", c.sql, err)
		}
	}
	if !clientRevoked || !secretRevoked || smppStatus != "suspended" {
		t.Fatalf("client revoked %v, secret revoked %v, smpp %q; want all revoked and suspended", clientRevoked, secretRevoked, smppStatus)
	}
	if revokedAt != "2026-10-01 00:00:00+00" {
		t.Fatalf("an already-revoked client's revoked_at changed to %s", revokedAt)
	}

	var forced int
	if err := superDB.QueryRowContext(ctx, `SELECT count(*) FROM pg_class WHERE relname IN ('api_clients', 'smpp_accounts') AND relforcerowsecurity`).Scan(&forced); err != nil {
		t.Fatal(err)
	}
	if forced != 2 {
		t.Fatalf("FORCE ROW LEVEL SECURITY is back on %d of 2 tables", forced)
	}
}
