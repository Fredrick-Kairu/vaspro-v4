//go:build integration

package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestTenantRegion_FixedAndSharedWithTheParent covers migration
// 20261105110000 (task 2.10): an account's data residency tag never
// changes after creation, and a sub-account is always in its
// reseller's region. Runs as the non-superuser role, inside
// WithTenantTx, the way app/ does.
func TestTenantRegion_FixedAndSharedWithTheParent(t *testing.T) {
	db := newAccountsDB(t)
	ctx := context.Background()

	newTenant := func(t *testing.T) uuid.UUID {
		t.Helper()
		id := uuid.New()
		if err := db.app.WithTenantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
			return insertTenant(ctx, tx, id, nil)
		}); err != nil {
			t.Fatalf("insert tenant: %v", err)
		}
		return id
	}
	insertSub := func(ctx context.Context, tx pgx.Tx, parent uuid.UUID, region string) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO tenants (id, type, parent_tenant_id, display_name, country_code, timezone, default_currency, region)
			VALUES ($1, 'sub_account', $2, 'Sub', 'KE', 'Africa/Nairobi', 'KES', $3)`, uuid.New(), parent, region)
		return err
	}

	t.Run("the region can't change", func(t *testing.T) {
		id := newTenant(t)
		err := db.app.WithTenantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE tenants SET region = 'tz' WHERE id = $1`, id)
			return err
		})
		if pgCode(err) != "23514" {
			t.Fatalf("got %v, want a check violation (23514)", err)
		}
		var region string
		if err := db.super.QueryRow(ctx, `SELECT region FROM tenants WHERE id = $1`, id).Scan(&region); err != nil || region != "ke" {
			t.Fatalf("region is %q (%v), want ke", region, err)
		}
	})
	t.Run("setting it to the same value and updating other columns still work", func(t *testing.T) {
		id := newTenant(t)
		if err := db.app.WithTenantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE tenants SET region = 'ke', display_name = 'Renamed' WHERE id = $1`, id)
			return err
		}); err != nil {
			t.Fatalf("got %v, want success", err)
		}
	})
	t.Run("a sub-account in its reseller's region is accepted", func(t *testing.T) {
		parent := newTenant(t)
		if err := db.app.WithTenantTx(ctx, parent, func(ctx context.Context, tx pgx.Tx) error {
			return insertSub(ctx, tx, parent, "ke")
		}); err != nil {
			t.Fatalf("got %v, want success", err)
		}
	})
	t.Run("a sub-account in another region is refused", func(t *testing.T) {
		parent := newTenant(t)
		err := db.app.WithTenantTx(ctx, parent, func(ctx context.Context, tx pgx.Tx) error {
			return insertSub(ctx, tx, parent, "tz")
		})
		if pgCode(err) != "23514" {
			t.Fatalf("got %v, want a check violation (23514)", err)
		}
	})
	t.Run("a sub-account whose parent can't be seen is refused", func(t *testing.T) {
		parent := newTenant(t)
		// In the sub-account's own transaction the parent's row is
		// hidden by RLS, so its region can't be checked.
		sub := uuid.New()
		err := db.app.WithTenantTx(ctx, sub, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				INSERT INTO tenants (id, type, parent_tenant_id, display_name, country_code, timezone, default_currency, region)
				VALUES ($1, 'sub_account', $2, 'Sub', 'KE', 'Africa/Nairobi', 'KES', 'ke')`, sub, parent)
			return err
		})
		if pgCode(err) != "23514" {
			t.Fatalf("got %v, want a check violation (23514)", err)
		}
	})
}
