package testkit

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	vasdb "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"
)

// AssertRowLevelSecurity verifies that a tenant-scoped table follows the
// canonical RLS pattern (data model §1): a row inserted under one tenant
// is invisible when queried under a different tenant, and invisible to
// any query that runs outside WithTenantTx altogether — RLS must fail
// closed, not fail open.
//
// pool must already be connected as the low-privilege role the policy is
// meant to apply to (a superuser bypasses RLS regardless of FORCE ROW
// LEVEL SECURITY). insertRow inserts exactly one row for tenantID.
// countVisible counts the rows visible in the current transaction's
// scope. Both run against pool via WithTenantTx, except the final
// no-tenant check, which runs in a plain transaction.
func AssertRowLevelSecurity(
	t *testing.T,
	pool *vasdb.Pool,
	insertRow func(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) error,
	countVisible func(ctx context.Context, tx pgx.Tx) (int, error),
) {
	t.Helper()
	ctx := context.Background()

	tenantA := uuid.New()
	tenantB := uuid.New()

	if err := pool.WithTenantTx(ctx, tenantA, func(ctx context.Context, tx pgx.Tx) error {
		return insertRow(ctx, tx, tenantA)
	}); err != nil {
		t.Fatalf("testkit: inserting tenant A's row: %v", err)
	}
	if err := pool.WithTenantTx(ctx, tenantB, func(ctx context.Context, tx pgx.Tx) error {
		return insertRow(ctx, tx, tenantB)
	}); err != nil {
		t.Fatalf("testkit: inserting tenant B's row: %v", err)
	}

	var seenByA int
	err := pool.WithTenantTx(ctx, tenantA, func(ctx context.Context, tx pgx.Tx) error {
		n, err := countVisible(ctx, tx)
		seenByA = n
		return err
	})
	if err != nil {
		t.Fatalf("testkit: counting rows visible to tenant A: %v", err)
	}
	if seenByA != 1 {
		t.Fatalf("testkit: tenant A should see exactly its own row, saw %d", seenByA)
	}

	var seenWithNoTenant int
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		n, err := countVisible(ctx, tx)
		seenWithNoTenant = n
		return err
	})
	if err != nil {
		t.Fatalf("testkit: counting rows visible without a tenant set: %v", err)
	}
	if seenWithNoTenant != 0 {
		t.Fatalf("testkit: expected 0 rows visible without a tenant set, saw %d (RLS must fail closed)", seenWithNoTenant)
	}
}
