//go:build integration

package testkit_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	vasdb "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/testkit"
)

func TestAssertRowLevelSecurity_OnAnIsolatedTable(t *testing.T) {
	pg := testkit.StartPostgres(t)
	ctx := context.Background()

	// A superuser bypasses RLS outright, FORCE ROW LEVEL SECURITY or not
	// (FORCE only binds a non-superuser table owner) — the assertion must
	// run as an ordinary role, the same way db/rls_integration_test.go's
	// app_role stands in for what a real service connects as.
	const setup = `
		CREATE TABLE rls_sample (
			tenant_id uuid NOT NULL,
			value     text NOT NULL
		);
		ALTER TABLE rls_sample ENABLE ROW LEVEL SECURITY;
		ALTER TABLE rls_sample FORCE ROW LEVEL SECURITY;
		CREATE POLICY tenant_isolation ON rls_sample
			USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
			WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
		CREATE ROLE rls_sample_role LOGIN PASSWORD 'rls_sample_role';
		GRANT SELECT, INSERT, UPDATE, DELETE ON rls_sample TO rls_sample_role;
	`
	if _, err := pg.Pool.Exec(ctx, setup); err != nil {
		t.Fatalf("failed to set up rls_sample: %v", err)
	}

	pool, err := vasdb.NewPool(ctx, pg.RoleDSN("rls_sample_role", "rls_sample_role"))
	if err != nil {
		t.Fatalf("failed to connect as rls_sample_role: %v", err)
	}
	defer pool.Close()

	insert := func(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) error {
		_, err := tx.Exec(ctx, "INSERT INTO rls_sample (tenant_id, value) VALUES ($1, 'x')", tenantID)
		return err
	}
	count := func(ctx context.Context, tx pgx.Tx) (int, error) {
		var n int
		err := tx.QueryRow(ctx, "SELECT count(*) FROM rls_sample").Scan(&n)
		return n, err
	}

	testkit.AssertRowLevelSecurity(t, pool, insert, count)
}
