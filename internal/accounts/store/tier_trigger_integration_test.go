//go:build integration

package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestTenantTier_OnlyChangesWithATierChangesRow covers migration
// 20261030140000: data model §4's "a tenant's tier only changes
// through tier_changes, written in the same transaction" is enforced
// by the database, not just by app code. Runs as the non-superuser
// role, inside WithTenantTx, the way app/ does.
func TestTenantTier_OnlyChangesWithATierChangesRow(t *testing.T) {
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
	setTier := func(ctx context.Context, tx pgx.Tx, id uuid.UUID, tier string) error {
		_, err := tx.Exec(ctx, `UPDATE tenants SET tier = $2 WHERE id = $1`, id, tier)
		return err
	}
	recordChange := func(ctx context.Context, tx pgx.Tx, id uuid.UUID, from, to string) error {
		_, err := tx.Exec(ctx, `INSERT INTO tier_changes (id, tenant_id, from_tier, to_tier, reason) VALUES ($1, $2, $3, $4, 'test')`,
			uuid.New(), id, from, to)
		return err
	}
	tierOf := func(t *testing.T, id uuid.UUID) string {
		t.Helper()
		var tier string
		if err := db.super.QueryRow(ctx, `SELECT tier FROM tenants WHERE id = $1`, id).Scan(&tier); err != nil {
			t.Fatal(err)
		}
		return tier
	}

	t.Run("a tier change with no tier_changes row fails at commit", func(t *testing.T) {
		id := newTenant(t)
		err := db.app.WithTenantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
			return setTier(ctx, tx, id, "starter")
		})
		if pgCode(err) != "23514" {
			t.Fatalf("got %v, want a check violation (23514)", err)
		}
		if got := tierOf(t, id); got != "sandbox" {
			t.Fatalf("tier is %q after the failed change, want sandbox", got)
		}
	})
	t.Run("a tier_changes row for a different change doesn't count", func(t *testing.T) {
		id := newTenant(t)
		err := db.app.WithTenantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
			if err := recordChange(ctx, tx, id, "sandbox", "business"); err != nil {
				return err
			}
			return setTier(ctx, tx, id, "starter")
		})
		if pgCode(err) != "23514" {
			t.Fatalf("got %v, want a check violation (23514)", err)
		}
	})
	t.Run("a tier_changes row from an earlier transaction doesn't count", func(t *testing.T) {
		id := newTenant(t)
		if err := db.app.WithTenantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
			return recordChange(ctx, tx, id, "sandbox", "starter")
		}); err != nil {
			t.Fatal(err)
		}
		err := db.app.WithTenantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
			return setTier(ctx, tx, id, "starter")
		})
		if pgCode(err) != "23514" {
			t.Fatalf("got %v, want a check violation (23514)", err)
		}
	})
	t.Run("the change and its row in one transaction succeed, in either order", func(t *testing.T) {
		id := newTenant(t)
		if err := db.app.WithTenantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
			if err := setTier(ctx, tx, id, "starter"); err != nil {
				return err
			}
			return recordChange(ctx, tx, id, "sandbox", "starter")
		}); err != nil {
			t.Fatalf("got %v, want success", err)
		}
		if got := tierOf(t, id); got != "starter" {
			t.Fatalf("tier is %q, want starter", got)
		}
	})
	t.Run("updating other columns needs no tier_changes row", func(t *testing.T) {
		id := newTenant(t)
		if err := db.app.WithTenantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE tenants SET display_name = 'Renamed', status = 'suspended' WHERE id = $1`, id)
			return err
		}); err != nil {
			t.Fatalf("got %v, want success", err)
		}
	})
	t.Run("a tenant cannot be created above sandbox", func(t *testing.T) {
		id := uuid.New()
		err := db.app.WithTenantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				INSERT INTO tenants (id, display_name, country_code, timezone, default_currency, region, tier)
				VALUES ($1, 'Pre-promoted', 'KE', 'Africa/Nairobi', 'KES', 'ke', 'business')`, id)
			return err
		})
		if pgCode(err) != "23514" {
			t.Fatalf("got %v, want a check violation (23514)", err)
		}
	})
}
