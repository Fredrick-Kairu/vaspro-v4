//go:build integration

package store_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"sort"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib" // "pgx" database/sql driver, for goose's Down round trip only
	"github.com/pressly/goose/v3"

	vasdb "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/testkit"
)

const migrationsDir = "../../../migrations/accounts"

// accountsDB is one migrated Postgres with two pools: super (the
// migration-running superuser, which bypasses RLS — used to arrange
// fixtures and to test plain constraints) and app (a non-superuser
// role, the only kind RLS actually applies to). Both have public on
// search_path so citext's operators resolve (STATUS notes, 1.1).
type accountsDB struct {
	pg    *testkit.Postgres
	super *vasdb.Pool
	app   *vasdb.Pool
}

func newAccountsDB(t *testing.T) accountsDB {
	t.Helper()
	ctx := context.Background()

	pg := testkit.StartPostgres(t, testkit.WithMigrations(migrationsDir))

	const setup = `
		CREATE ROLE accounts_role LOGIN PASSWORD 'accounts_role';
		GRANT USAGE ON SCHEMA accounts TO accounts_role;
		GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA accounts TO accounts_role;
		GRANT accounts_membership_lookup TO accounts_role WITH INHERIT FALSE;
		GRANT accounts_kyc_webhook TO accounts_role WITH INHERIT FALSE;
		GRANT accounts_registration_lookup TO accounts_role WITH INHERIT FALSE;
	`
	if _, err := pg.Pool.Exec(ctx, setup); err != nil {
		t.Fatalf("failed to set up accounts_role: %v", err)
	}

	super, err := vasdb.NewPool(ctx, pg.DSN()+"&search_path=accounts,public")
	if err != nil {
		t.Fatalf("failed to connect as superuser: %v", err)
	}
	t.Cleanup(super.Close)

	app, err := vasdb.NewPool(ctx, pg.RoleDSN("accounts_role", "accounts_role")+"&search_path=accounts,public")
	if err != nil {
		t.Fatalf("failed to connect as accounts_role: %v", err)
	}
	t.Cleanup(app.Close)

	return accountsDB{pg: pg, super: super, app: app}
}

// execer is satisfied by both a pool and a transaction.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func insertTenant(ctx context.Context, db execer, id uuid.UUID, parent *uuid.UUID) error {
	_, err := db.Exec(ctx, `
		INSERT INTO tenants (id, type, parent_tenant_id, display_name, country_code, timezone, default_currency, region)
		VALUES ($1, CASE WHEN $2::uuid IS NULL THEN 'direct' ELSE 'sub_account' END::tenant_type, $2, 'Test tenant', 'KE', 'Africa/Nairobi', 'KES', 'ke')`, id, parent)
	return err
}

func mustTenant(t *testing.T, db execer) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if err := insertTenant(context.Background(), db, id, nil); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}
	return id
}

func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func countRows(ctx context.Context, tx pgx.Tx, query string, args ...any) (int, error) {
	var n int
	err := tx.QueryRow(ctx, query, args...).Scan(&n)
	return n, err
}

// TestMigration_DownThenUp proves the Down section really undoes Up:
// after DownTo(0) the schema is gone, and Up applies cleanly again.
func TestMigration_DownThenUp(t *testing.T) {
	ctx := context.Background()
	db := newAccountsDB(t)

	sqlDB, err := sql.Open("pgx", db.pg.DSN())
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer sqlDB.Close()

	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, os.DirFS(migrationsDir))
	if err != nil {
		t.Fatalf("goose.NewProvider: %v", err)
	}
	if _, err := provider.DownTo(ctx, 0); err != nil {
		t.Fatalf("DownTo(0): %v", err)
	}

	var exists bool
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = 'accounts')`).Scan(&exists); err != nil {
		t.Fatalf("checking schema: %v", err)
	}
	if exists {
		t.Fatal("schema accounts still exists after DownTo(0)")
	}

	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("Up after Down: %v", err)
	}
}

// TestRowLevelSecurity_TenantOwnedTables runs the canonical RLS check
// (data model §1) over every table with tenant_id NOT NULL. Each insert
// creates the row's own tenant first (tenants' own policy allows a
// tenant to insert itself) plus whatever the row's foreign keys need.
func TestRowLevelSecurity_TenantOwnedTables(t *testing.T) {
	db := newAccountsDB(t)
	ctx := context.Background()

	// Global fixtures, inserted as superuser: a service code and a
	// built-in role (tenant_id NULL), which every tenant can reference.
	if _, err := db.super.Exec(ctx, `INSERT INTO services (code, name, default_min_tier) VALUES ('fixture_service', 'Fixture service', 'starter')`); err != nil {
		t.Fatalf("insert service: %v", err)
	}
	builtinRole := uuid.New()
	if _, err := db.super.Exec(ctx, `INSERT INTO roles (id, tenant_id, name, permissions) VALUES ($1, NULL, 'Fixture built-in', '{sms:send}')`, builtinRole); err != nil {
		t.Fatalf("insert built-in role: %v", err)
	}

	insertBusinessProfile := func(ctx context.Context, tx pgx.Tx, tenant uuid.UUID) (uuid.UUID, error) {
		id := uuid.New()
		_, err := tx.Exec(ctx, `
			INSERT INTO business_profiles (id, tenant_id, registry, registration_number, registered_name)
			VALUES ($1, $2, 'BRS', 'PVT-123', 'Test Ltd')`, id, tenant)
		return id, err
	}

	cases := []struct {
		table  string
		insert func(ctx context.Context, tx pgx.Tx, tenant uuid.UUID) error
	}{
		{"memberships", func(ctx context.Context, tx pgx.Tx, tenant uuid.UUID) error {
			_, err := tx.Exec(ctx, `INSERT INTO memberships (tenant_id, user_id, role_id, status) VALUES ($1, $2, $3, 'active')`,
				tenant, uuid.New(), builtinRole)
			return err
		}},
		{"tenant_services", func(ctx context.Context, tx pgx.Tx, tenant uuid.UUID) error {
			_, err := tx.Exec(ctx, `INSERT INTO tenant_services (tenant_id, service_code, status) VALUES ($1, 'fixture_service', 'selected')`, tenant)
			return err
		}},
		{"business_profiles", func(ctx context.Context, tx pgx.Tx, tenant uuid.UUID) error {
			_, err := insertBusinessProfile(ctx, tx, tenant)
			return err
		}},
		{"beneficial_owners", func(ctx context.Context, tx pgx.Tx, tenant uuid.UUID) error {
			bp, err := insertBusinessProfile(ctx, tx, tenant)
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `
				INSERT INTO beneficial_owners (id, tenant_id, business_profile_id, full_name, role)
				VALUES ($1, $2, $3, 'Jane Director', 'director')`, uuid.New(), tenant, bp)
			return err
		}},
		{"documents", func(ctx context.Context, tx pgx.Tx, tenant uuid.UUID) error {
			_, err := tx.Exec(ctx, `
				INSERT INTO documents (id, tenant_id, type, storage_key, sha256, mime_type, size_bytes, status, uploaded_by)
				VALUES ($1, $2, 'national_id', 'k', '\x00', 'application/pdf', 1, 'received', $3)`, uuid.New(), tenant, uuid.New())
			return err
		}},
		{"kyc_checks", func(ctx context.Context, tx pgx.Tx, tenant uuid.UUID) error {
			_, err := tx.Exec(ctx, `
				INSERT INTO kyc_checks (id, tenant_id, kind, subject_id, provider, status)
				VALUES ($1, $2, 'individual', $3, 'fake', 'pending')`, uuid.New(), tenant, uuid.New())
			return err
		}},
		{"tier_changes", func(ctx context.Context, tx pgx.Tx, tenant uuid.UUID) error {
			_, err := tx.Exec(ctx, `
				INSERT INTO tier_changes (id, tenant_id, from_tier, to_tier, reason)
				VALUES ($1, $2, 'sandbox', 'starter', 'test')`, uuid.New(), tenant)
			return err
		}},
	}

	for _, tc := range cases {
		t.Run(tc.table, func(t *testing.T) {
			testkit.AssertRowLevelSecurity(t, db.app,
				func(ctx context.Context, tx pgx.Tx, tenant uuid.UUID) error {
					if err := insertTenant(ctx, tx, tenant, nil); err != nil {
						return err
					}
					return tc.insert(ctx, tx, tenant)
				},
				func(ctx context.Context, tx pgx.Tx) (int, error) {
					return countRows(ctx, tx, `SELECT count(*) FROM `+pgx.Identifier{tc.table}.Sanitize())
				},
			)
		})
	}
}

// TestRowLevelSecurity_Tenants covers the tenants policy, which keys on
// the row's own id (tenants has no tenant_id): a tenant sees itself and
// its sub-accounts, nothing else, and nothing outside a tenant transaction.
func TestRowLevelSecurity_Tenants(t *testing.T) {
	db := newAccountsDB(t)
	ctx := context.Background()

	reseller := mustTenant(t, db.super)
	sub := uuid.New()
	if err := insertTenant(ctx, db.super, sub, &reseller); err != nil {
		t.Fatalf("insert sub-account: %v", err)
	}
	unrelated := mustTenant(t, db.super)

	visible := func(t *testing.T, tenant uuid.UUID) []uuid.UUID {
		t.Helper()
		var ids []uuid.UUID
		err := db.app.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `SELECT id FROM tenants`)
			if err != nil {
				return err
			}
			ids, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
			return err
		})
		if err != nil {
			t.Fatalf("listing tenants as %s: %v", tenant, err)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
		return ids
	}
	sorted := func(ids ...uuid.UUID) []uuid.UUID {
		sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
		return ids
	}
	equal := func(a, b []uuid.UUID) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}

	t.Run("reseller sees itself and its sub-account, not an unrelated tenant", func(t *testing.T) {
		if got, want := visible(t, reseller), sorted(reseller, sub); !equal(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})
	t.Run("sub-account sees only itself", func(t *testing.T) {
		if got, want := visible(t, sub), []uuid.UUID{sub}; !equal(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})
	t.Run("unrelated tenant sees only itself", func(t *testing.T) {
		if got, want := visible(t, unrelated), []uuid.UUID{unrelated}; !equal(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})
	t.Run("nothing is visible outside a tenant transaction", func(t *testing.T) {
		err := pgx.BeginFunc(ctx, db.app, func(tx pgx.Tx) error {
			n, err := countRows(ctx, tx, `SELECT count(*) FROM tenants`)
			if err != nil {
				return err
			}
			if n != 0 {
				t.Fatalf("saw %d tenants with no tenant set, want 0", n)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})
	t.Run("a tenant cannot insert an unrelated tenant", func(t *testing.T) {
		err := db.app.WithTenantTx(ctx, reseller, func(ctx context.Context, tx pgx.Tx) error {
			return insertTenant(ctx, tx, uuid.New(), nil)
		})
		if pgCode(err) != "42501" {
			t.Fatalf("got %v, want an RLS violation (42501)", err)
		}
	})
	t.Run("a reseller can insert its own sub-account", func(t *testing.T) {
		err := db.app.WithTenantTx(ctx, reseller, func(ctx context.Context, tx pgx.Tx) error {
			return insertTenant(ctx, tx, uuid.New(), &reseller)
		})
		if err != nil {
			t.Fatalf("got %v, want the insert to succeed", err)
		}
	})
}

// TestRowLevelSecurity_Roles covers roles' nullable tenant_id: built-in
// roles (NULL) are readable by every tenant but writable by none.
func TestRowLevelSecurity_Roles(t *testing.T) {
	db := newAccountsDB(t)
	ctx := context.Background()

	if _, err := db.super.Exec(ctx, `INSERT INTO roles (id, tenant_id, name, permissions) VALUES ($1, NULL, 'Fixture built-in', '{}')`, uuid.New()); err != nil {
		t.Fatalf("insert built-in role: %v", err)
	}
	tenantA := mustTenant(t, db.super)
	tenantB := mustTenant(t, db.super)

	if err := db.app.WithTenantTx(ctx, tenantA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO roles (id, tenant_id, name, permissions) VALUES ($1, $2, 'Campaigns', '{}')`, uuid.New(), tenantA)
		return err
	}); err != nil {
		t.Fatalf("tenant A inserting its own role: %v", err)
	}

	count := func(t *testing.T, tenant uuid.UUID, where string) int {
		t.Helper()
		var n int
		if err := db.app.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			n, err = countRows(ctx, tx, `SELECT count(*) FROM roles WHERE `+where)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}

	t.Run("a built-in role is visible to every tenant", func(t *testing.T) {
		for _, tenant := range []uuid.UUID{tenantA, tenantB} {
			if n := count(t, tenant, `tenant_id IS NULL AND name = 'Fixture built-in'`); n != 1 {
				t.Fatalf("tenant %s sees %d built-in roles, want 1", tenant, n)
			}
		}
	})
	t.Run("a tenant's own role is invisible to another tenant", func(t *testing.T) {
		if n := count(t, tenantA, `tenant_id IS NOT NULL`); n != 1 {
			t.Fatalf("tenant A sees %d own roles, want 1", n)
		}
		if n := count(t, tenantB, `tenant_id IS NOT NULL`); n != 0 {
			t.Fatalf("tenant B sees %d tenant roles, want 0", n)
		}
	})
	t.Run("a tenant cannot create a built-in role", func(t *testing.T) {
		err := db.app.WithTenantTx(ctx, tenantA, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO roles (id, tenant_id, name, permissions) VALUES ($1, NULL, 'Sneaky', '{}')`, uuid.New())
			return err
		})
		if pgCode(err) != "42501" {
			t.Fatalf("got %v, want an RLS violation (42501)", err)
		}
	})
	t.Run("a tenant cannot edit a built-in role", func(t *testing.T) {
		err := db.app.WithTenantTx(ctx, tenantA, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE roles SET permissions = '{payouts:approve}' WHERE tenant_id IS NULL`)
			return err
		})
		if pgCode(err) != "42501" {
			t.Fatalf("got %v, want an RLS violation (42501)", err)
		}
	})
}

// TestConstraints_RejectInvalidValues checks every CHECK in the schema.
// Run as superuser: these are plain constraints, not policies.
func TestConstraints_RejectInvalidValues(t *testing.T) {
	db := newAccountsDB(t)
	ctx := context.Background()

	tenant := mustTenant(t, db.super)
	bp := uuid.New()
	if _, err := db.super.Exec(ctx, `
		INSERT INTO business_profiles (id, tenant_id, registry, registration_number, registered_name)
		VALUES ($1, $2, 'BRS', 'PVT-1', 'Test Ltd')`, bp, tenant); err != nil {
		t.Fatalf("insert business profile: %v", err)
	}
	role := uuid.New()
	if _, err := db.super.Exec(ctx, `INSERT INTO roles (id, name, permissions) VALUES ($1, 'Fixture built-in', '{}')`, role); err != nil {
		t.Fatalf("insert role: %v", err)
	}
	if _, err := db.super.Exec(ctx, `INSERT INTO services (code, name, default_min_tier) VALUES ('fixture_service', 'Fixture service', 'starter')`); err != nil {
		t.Fatalf("insert service: %v", err)
	}

	cases := []struct {
		name string
		sql  string
		args []any
	}{
		{"tenants.status", `UPDATE tenants SET status = 'deleted' WHERE id = $1`, []any{tenant}},
		{"tenants: a sub-account needs a parent", `UPDATE tenants SET type = 'sub_account' WHERE id = $1`, []any{tenant}},
		{"tenants: only a sub-account has a parent", `INSERT INTO tenants (id, type, parent_tenant_id, display_name, country_code, timezone, default_currency, region) VALUES ($1, 'reseller', $2, 'X', 'KE', 'Africa/Nairobi', 'KES', 'ke')`, []any{uuid.New(), tenant}},
		{"memberships.status", `INSERT INTO memberships (tenant_id, user_id, role_id, status) VALUES ($1, $2, $3, 'pending')`, []any{tenant, uuid.New(), role}},
		{"tenant_services.status", `INSERT INTO tenant_services (tenant_id, service_code, status) VALUES ($1, 'fixture_service', 'disabled')`, []any{tenant}},
		{"tier_caps.period", `INSERT INTO tier_caps (country_code, tier, metric, period, value) VALUES ('KE', 'starter', 'sms_per_day', 'week', 100)`, nil},
		{"beneficial_owners.role", `INSERT INTO beneficial_owners (id, tenant_id, business_profile_id, full_name, role) VALUES ($1, $2, $3, 'X', 'shareholder')`, []any{uuid.New(), tenant, bp}},
		{"beneficial_owners.screening_result", `INSERT INTO beneficial_owners (id, tenant_id, business_profile_id, full_name, role, screening_result) VALUES ($1, $2, $3, 'X', 'owner', 'unknown')`, []any{uuid.New(), tenant, bp}},
		{"documents.status", `INSERT INTO documents (id, tenant_id, type, storage_key, sha256, mime_type, size_bytes, status, uploaded_by) VALUES ($1, $2, 'national_id', 'k', '\x00', 'application/pdf', 1, 'scanning', $3)`, []any{uuid.New(), tenant, uuid.New()}},
		{"kyc_checks.kind", `INSERT INTO kyc_checks (id, tenant_id, kind, subject_id, provider, status) VALUES ($1, $2, 'device', $3, 'fake', 'pending')`, []any{uuid.New(), tenant, uuid.New()}},
		{"kyc_checks.status", `INSERT INTO kyc_checks (id, tenant_id, kind, subject_id, provider, status) VALUES ($1, $2, 'individual', $3, 'fake', 'approved')`, []any{uuid.New(), tenant, uuid.New()}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := db.super.Exec(ctx, tc.sql, tc.args...)
			if pgCode(err) != "23514" {
				t.Fatalf("got %v, want a check violation (23514)", err)
			}
		})
	}
}

// TestConstraints_Uniqueness checks the unique keys later tasks rely on.
func TestConstraints_Uniqueness(t *testing.T) {
	db := newAccountsDB(t)
	ctx := context.Background()

	tenantA := mustTenant(t, db.super)
	tenantB := mustTenant(t, db.super)
	role := uuid.New()
	if _, err := db.super.Exec(ctx, `INSERT INTO roles (id, name, permissions) VALUES ($1, 'Fixture built-in', '{}')`, role); err != nil {
		t.Fatalf("insert role: %v", err)
	}

	exec := func(sql string, args ...any) error {
		_, err := db.super.Exec(ctx, sql, args...)
		return err
	}
	wantOK := func(t *testing.T, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("got %v, want success", err)
		}
	}
	wantDuplicate := func(t *testing.T, err error) {
		t.Helper()
		if pgCode(err) != "23505" {
			t.Fatalf("got %v, want a unique violation (23505)", err)
		}
	}

	t.Run("a duplicate built-in role name is rejected (NULLS NOT DISTINCT)", func(t *testing.T) {
		wantDuplicate(t, exec(`INSERT INTO roles (id, name, permissions) VALUES ($1, 'Fixture built-in', '{}')`, uuid.New()))
	})
	t.Run("the same role name under two tenants is allowed", func(t *testing.T) {
		wantOK(t, exec(`INSERT INTO roles (id, tenant_id, name, permissions) VALUES ($1, $2, 'Ops', '{}')`, uuid.New(), tenantA))
		wantOK(t, exec(`INSERT INTO roles (id, tenant_id, name, permissions) VALUES ($1, $2, 'Ops', '{}')`, uuid.New(), tenantB))
	})

	t.Run("one user belongs to several tenants, but only once to each", func(t *testing.T) {
		user := uuid.New()
		const q = `INSERT INTO memberships (tenant_id, user_id, role_id, status) VALUES ($1, $2, $3, 'active')`
		wantOK(t, exec(q, tenantA, user, role))
		wantOK(t, exec(q, tenantB, user, role))
		wantDuplicate(t, exec(q, tenantA, user, role))
	})

	t.Run("kyc_checks: a provider reference is unique per provider; NULL refs are not", func(t *testing.T) {
		const q = `INSERT INTO kyc_checks (id, tenant_id, kind, subject_id, provider, provider_ref, status) VALUES ($1, $2, 'individual', $3, 'fake', $4, 'pending')`
		ref := "ref-1"
		wantOK(t, exec(q, uuid.New(), tenantA, uuid.New(), &ref))
		wantDuplicate(t, exec(q, uuid.New(), tenantB, uuid.New(), &ref))
		wantOK(t, exec(q, uuid.New(), tenantA, uuid.New(), nil))
		wantOK(t, exec(q, uuid.New(), tenantA, uuid.New(), nil))
	})

	t.Run("business_profiles: one per tenant, but a registration number may repeat across tenants", func(t *testing.T) {
		const q = `INSERT INTO business_profiles (id, tenant_id, registry, registration_number, registered_name) VALUES ($1, $2, 'BRS', 'PVT-9', 'Shared Ltd')`
		wantOK(t, exec(q, uuid.New(), tenantA))
		wantOK(t, exec(q, uuid.New(), tenantB))
		wantDuplicate(t, exec(q, uuid.New(), tenantA))
	})
}

// TestTierEnum_OrderedLowestToHighest pins the enum's declaration order,
// which later tasks use to compare tiers ("the lowest tier that unlocks").
func TestTierEnum_OrderedLowestToHighest(t *testing.T) {
	db := newAccountsDB(t)
	var ordered bool
	err := db.super.QueryRow(context.Background(), `
		SELECT 'sandbox'::tier < 'starter'::tier
		   AND 'starter'::tier < 'business'::tier
		   AND 'business'::tier < 'financial'::tier`).Scan(&ordered)
	if err != nil {
		t.Fatal(err)
	}
	if !ordered {
		t.Fatal("tier enum is not ordered sandbox < starter < business < financial")
	}
}

func TestTenants_BillingEmailIsCaseInsensitive(t *testing.T) {
	db := newAccountsDB(t)
	ctx := context.Background()

	tenant := mustTenant(t, db.super)
	if _, err := db.super.Exec(ctx, `UPDATE tenants SET billing_email = 'Billing@Example.com' WHERE id = $1`, tenant); err != nil {
		t.Fatal(err)
	}
	var got uuid.UUID
	if err := db.super.QueryRow(ctx, `SELECT id FROM tenants WHERE billing_email = 'billing@example.com'`).Scan(&got); err != nil {
		t.Fatalf("case-insensitive lookup: %v", err)
	}
	if got != tenant {
		t.Fatalf("got %s, want %s", got, tenant)
	}
}
