//go:build integration

package db_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	vasdb "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"
)

// appRoleDSN starts a postgres container as the test superuser, creates a
// sample table with row-level security exactly as data model §1 specifies
// (ENABLE *and* FORCE, so even the table owner can't bypass it, plus a
// dedicated low-privilege role), and returns a DSN for that low-privilege
// role — standing in for what a real service would connect as.
func appRoleDSN(t *testing.T) string {
	t.Helper()
	ctx := context.Background()

	// The official postgres image logs "database system is ready to accept
	// connections" once for a temporary setup server and again for the
	// real one; waiting for only the first occurrence races the restart
	// in between and produces "connection reset by peer".
	ctr, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("vaspro_test"),
		postgres.WithUsername("super"),
		postgres.WithPassword("super"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		t.Fatalf("failed to start postgres container: %v", err)
	}
	testcontainers.CleanupContainer(t, ctr)

	superDSN, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("failed to get connection string: %v", err)
	}

	host, err := ctr.Host(ctx)
	if err != nil {
		t.Fatalf("failed to get container host: %v", err)
	}
	port, err := ctr.MappedPort(ctx, "5432/tcp")
	if err != nil {
		t.Fatalf("failed to get container port: %v", err)
	}

	superPool, err := vasdb.NewPool(ctx, superDSN)
	if err != nil {
		t.Fatalf("failed to connect as superuser: %v", err)
	}
	defer superPool.Close()

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
	if _, err := superPool.Exec(ctx, setup); err != nil {
		t.Fatalf("failed to set up the sample table: %v", err)
	}

	return fmt.Sprintf("postgres://app_role:app_role@%s:%s/vaspro_test?sslmode=disable", host, port.Port())
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
