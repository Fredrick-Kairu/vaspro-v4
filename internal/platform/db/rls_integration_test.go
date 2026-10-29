//go:build integration

package db_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	vasdb "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/testkit"
)

// appRoleDSN starts a postgres container as the test superuser, creates a
// sample table with row-level security exactly as data model §1 specifies
// (ENABLE *and* FORCE, so even the table owner can't bypass it, plus a
// dedicated low-privilege role), and returns a DSN for that low-privilege
// role — standing in for what a real service would connect as.
func appRoleDSN(t *testing.T) string {
	t.Helper()
	ctx := context.Background()

	pg := testkit.StartPostgres(t)

	const setup = `
		CREATE TABLE sample (
			tenant_id uuid NOT NULL,
			value     text NOT NULL
		);
		ALTER TABLE sample ENABLE ROW LEVEL SECURITY;
		ALTER TABLE sample FORCE ROW LEVEL SECURITY;
		-- nullif(..., '') matters: on a pooled connection that previously
		-- ran a WithTenantTx for some tenant, current_setting(..., true)
		-- returns '' (not NULL) once that transaction's SET LOCAL value
		-- has reverted, and casting '' to uuid errors instead of failing
		-- closed to "no rows" the way a truly never-set GUC does.
		CREATE POLICY tenant_isolation ON sample
			USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
			WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

		CREATE ROLE app_role LOGIN PASSWORD 'app_role';
		GRANT SELECT, INSERT, UPDATE, DELETE ON sample TO app_role;
	`
	if _, err := pg.Pool.Exec(ctx, setup); err != nil {
		t.Fatalf("failed to set up the sample table: %v", err)
	}

	return pg.RoleDSN("app_role", "app_role")
}

// TestAssertRowLevelSecurity_OnTheSampleTable dogfoods
// testkit.AssertRowLevelSecurity against the same sample table/policy the
// tests below exercise by hand — proof the generic helper holds up
// against a real RLS policy, not just its own throwaway scratch table.
func TestAssertRowLevelSecurity_OnTheSampleTable(t *testing.T) {
	ctx := context.Background()
	pool, err := vasdb.NewPool(ctx, appRoleDSN(t))
	if err != nil {
		t.Fatalf("failed to connect as app_role: %v", err)
	}
	defer pool.Close()

	insert := func(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) error {
		_, err := tx.Exec(ctx, "INSERT INTO sample (tenant_id, value) VALUES ($1, 'x')", tenantID)
		return err
	}
	count := func(ctx context.Context, tx pgx.Tx) (int, error) {
		var n int
		err := tx.QueryRow(ctx, "SELECT count(*) FROM sample").Scan(&n)
		return n, err
	}

	testkit.AssertRowLevelSecurity(t, pool, insert, count)
}

func TestWithTenantTx_ReadIsolation(t *testing.T) {
	ctx := context.Background()
	pool, err := vasdb.NewPool(ctx, appRoleDSN(t))
	if err != nil {
		t.Fatalf("failed to connect as app_role: %v", err)
	}
	defer pool.Close()

	tenantA := uuid.New()
	tenantB := uuid.New()

	insert := func(tenantID uuid.UUID, value string) error {
		return pool.WithTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, "INSERT INTO sample (tenant_id, value) VALUES ($1, $2)", tenantID, value)
			return err
		})
	}
	if err := insert(tenantA, "a-row"); err != nil {
		t.Fatalf("failed to insert tenant A's row: %v", err)
	}
	if err := insert(tenantB, "b-row"); err != nil {
		t.Fatalf("failed to insert tenant B's row: %v", err)
	}

	var seen []string
	err = pool.WithTenantTx(ctx, tenantA, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, "SELECT value FROM sample")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				return err
			}
			seen = append(seen, v)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("failed to query as tenant A: %v", err)
	}

	if len(seen) != 1 || seen[0] != "a-row" {
		t.Fatalf("tenant A should see only its own row, got %v", seen)
	}
}

func TestWithTenantTx_WriteIsolation(t *testing.T) {
	ctx := context.Background()
	pool, err := vasdb.NewPool(ctx, appRoleDSN(t))
	if err != nil {
		t.Fatalf("failed to connect as app_role: %v", err)
	}
	defer pool.Close()

	tenantA := uuid.New()
	tenantB := uuid.New()

	// Seed tenant B's row from within tenant B's own transaction (the
	// legitimate way), so it exists for the cross-tenant update/delete
	// checks below to target.
	err = pool.WithTenantTx(ctx, tenantB, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "INSERT INTO sample (tenant_id, value) VALUES ($1, $2)", tenantB, "b-row")
		return err
	})
	if err != nil {
		t.Fatalf("failed to seed tenant B's row: %v", err)
	}

	t.Run("mismatched insert is rejected", func(t *testing.T) {
		err := pool.WithTenantTx(ctx, tenantA, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, "INSERT INTO sample (tenant_id, value) VALUES ($1, $2)", tenantB, "smuggled-row")
			return err
		})
		if err == nil {
			t.Fatal("expected inserting a row for tenant B while scoped to tenant A to fail")
		}
	})

	t.Run("cross-tenant update affects nothing", func(t *testing.T) {
		var rowsAffected int64
		err := pool.WithTenantTx(ctx, tenantA, func(ctx context.Context, tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, "UPDATE sample SET value = 'tampered' WHERE tenant_id = $1", tenantB)
			if err != nil {
				return err
			}
			rowsAffected = tag.RowsAffected()
			return nil
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if rowsAffected != 0 {
			t.Fatalf("expected the cross-tenant update to affect 0 rows, affected %d", rowsAffected)
		}
	})

	t.Run("cross-tenant delete affects nothing", func(t *testing.T) {
		var rowsAffected int64
		err := pool.WithTenantTx(ctx, tenantA, func(ctx context.Context, tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, "DELETE FROM sample WHERE tenant_id = $1", tenantB)
			if err != nil {
				return err
			}
			rowsAffected = tag.RowsAffected()
			return nil
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if rowsAffected != 0 {
			t.Fatalf("expected the cross-tenant delete to affect 0 rows, affected %d", rowsAffected)
		}
	})
}

// elevatedRoleDSN mirrors appRoleDSN's shape but adds a second,
// NOLOGIN role that can see a table app_role itself has no grant on
// at all — standing in for identity_token_exchange's real shape (1.8):
// a narrow, elevated-but-not-superuser role reached only via SET
// LOCAL ROLE from an already-authenticated connection, never its own
// login credential.
func elevatedRoleDSN(t *testing.T) string {
	t.Helper()
	ctx := context.Background()

	pg := testkit.StartPostgres(t)

	const setup = `
		CREATE TABLE restricted (
			value text NOT NULL
		);
		INSERT INTO restricted (value) VALUES ('secret-row');

		CREATE ROLE app_role LOGIN PASSWORD 'app_role';
		CREATE ROLE elevated_role NOLOGIN;
		GRANT SELECT ON restricted TO elevated_role;
		-- WITH INHERIT FALSE matters: a default (inheriting) GRANT would
		-- make elevated_role's SELECT ambiently active on every query
		-- app_role's connection ever runs, not just inside WithRoleTx —
		-- found by testing this exact migration pattern against
		-- identity.api_clients (store_integration_test.go).
		GRANT elevated_role TO app_role WITH INHERIT FALSE;
	`
	if _, err := pg.Pool.Exec(ctx, setup); err != nil {
		t.Fatalf("failed to set up restricted table and roles: %v", err)
	}

	return pg.RoleDSN("app_role", "app_role")
}

func TestWithRoleTx_GrantsAndRevertsElevatedAccess(t *testing.T) {
	ctx := context.Background()
	pool, err := vasdb.NewPool(ctx, elevatedRoleDSN(t))
	if err != nil {
		t.Fatalf("failed to connect as app_role: %v", err)
	}
	defer pool.Close()

	t.Run("plain app_role cannot see the restricted table", func(t *testing.T) {
		rows, err := pool.Query(ctx, "SELECT value FROM restricted")
		if err == nil {
			for rows.Next() {
			}
			err = rows.Err()
			rows.Close()
		}
		if err == nil {
			t.Fatal("expected a permission error querying restricted as app_role")
		}
	})

	t.Run("WithRoleTx grants access for the one transaction", func(t *testing.T) {
		var seen []string
		err := pool.WithRoleTx(ctx, "elevated_role", func(ctx context.Context, tx pgx.Tx) error {
			rows, err := tx.Query(ctx, "SELECT value FROM restricted")
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var v string
				if err := rows.Scan(&v); err != nil {
					return err
				}
				seen = append(seen, v)
			}
			return rows.Err()
		})
		if err != nil {
			t.Fatalf("unexpected error inside WithRoleTx: %v", err)
		}
		if len(seen) != 1 || seen[0] != "secret-row" {
			t.Fatalf("got %v, want [secret-row]", seen)
		}
	})

	t.Run("the role reverts once the transaction ends", func(t *testing.T) {
		// Pool-level, not tx-level: proves SET LOCAL's effect didn't
		// leak onto a reused pooled connection after the transaction
		// above committed.
		rows, err := pool.Query(ctx, "SELECT value FROM restricted")
		if err == nil {
			for rows.Next() {
			}
			err = rows.Err()
			rows.Close()
		}
		if err == nil {
			t.Fatal("expected app_role to have lost elevated access after WithRoleTx returned")
		}
	})
}

func TestWithoutTenantTx_FailsClosedToNoRows(t *testing.T) {
	ctx := context.Background()
	pool, err := vasdb.NewPool(ctx, appRoleDSN(t))
	if err != nil {
		t.Fatalf("failed to connect as app_role: %v", err)
	}
	defer pool.Close()

	tenantA := uuid.New()
	err = pool.WithTenantTx(ctx, tenantA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "INSERT INTO sample (tenant_id, value) VALUES ($1, $2)", tenantA, "a-row")
		return err
	})
	if err != nil {
		t.Fatalf("failed to seed a row: %v", err)
	}

	// A plain query, outside WithTenantTx: app.tenant_id is unset (or, on
	// a pooled connection reused from a prior transaction, reverted to
	// '' — see the nullif in the policy above), so every row's
	// tenant_id = NULL comparison is NULL (not true) — fail closed to
	// zero rows, not an error and not everything.
	rows, err := pool.Query(ctx, "SELECT value FROM sample")
	if err != nil {
		t.Fatalf("unexpected query error: %v", err)
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		count++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("unexpected row iteration error: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected 0 rows visible without a tenant set, got %d", count)
	}
}
