//go:build integration

package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/accounts/store"
)

// TestCreateTenant_ThroughGeneratedQueries_UnderRLS runs the sqlc
// queries the way app/ will: as the non-superuser role, inside
// WithTenantTx for the tenant being created, so the tenants policy's
// WITH CHECK (id = app.tenant_id) is what lets the insert through.
func TestCreateTenant_ThroughGeneratedQueries_UnderRLS(t *testing.T) {
	db := newAccountsDB(t)
	ctx := context.Background()
	tenantID := uuid.New()

	err := db.app.WithTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		created, err := q.CreateTenant(ctx, store.CreateTenantParams{
			ID:              tenantID,
			Type:            store.AccountsTenantTypeDirect,
			DisplayName:     "Acme",
			CountryCode:     "KE",
			Timezone:        "Africa/Nairobi",
			DefaultCurrency: "KES",
			Region:          "ke",
			BillingEmail:    pgtype.Text{String: "Billing@Acme.example", Valid: true},
		})
		if err != nil {
			return err
		}
		if created.Tier != store.AccountsTierSandbox || created.Status != "active" || created.Version != 1 {
			t.Fatalf("defaults: got tier=%s status=%s version=%d, want sandbox/active/1",
				created.Tier, created.Status, created.Version)
		}

		got, err := q.GetTenantByID(ctx, tenantID)
		if err != nil {
			return err
		}
		if got.DisplayName != "Acme" {
			t.Fatalf("got display_name %q, want Acme", got.DisplayName)
		}

		if _, err := q.CreateTierChange(ctx, store.CreateTierChangeParams{
			ID:       uuid.New(),
			TenantID: tenantID,
			FromTier: store.AccountsTierSandbox,
			ToTier:   store.AccountsTierStarter,
			Reason:   "test",
		}); err != nil {
			return err
		}
		changes, err := q.ListTierChangesByTenant(ctx, tenantID)
		if err != nil {
			return err
		}
		if len(changes) != 1 || changes[0].ToTier != store.AccountsTierStarter {
			t.Fatalf("got tier changes %+v, want one sandbox→starter", changes)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
}

// TestListSubAccounts_UnderRLS: a reseller's transaction lists its own
// sub-accounts, and naming another reseller's id finds nothing.
func TestListSubAccounts_UnderRLS(t *testing.T) {
	db := newAccountsDB(t)
	ctx := context.Background()
	reseller := mustTenant(t, db.super)
	other := mustTenant(t, db.super)
	subs := []uuid.UUID{uuid.New(), uuid.New()}
	for _, id := range subs {
		if err := insertTenant(ctx, db.super, id, &reseller); err != nil {
			t.Fatal(err)
		}
	}
	otherSub := uuid.New()
	if err := insertTenant(ctx, db.super, otherSub, &other); err != nil {
		t.Fatal(err)
	}

	list := func(as, parent uuid.UUID) []store.AccountsTenant {
		t.Helper()
		var rows []store.AccountsTenant
		err := db.app.WithTenantTx(ctx, as, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			rows, err = store.New(tx).ListSubAccounts(ctx, pgtype.UUID{Bytes: parent, Valid: true})
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	if got := list(reseller, reseller); len(got) != 2 {
		t.Fatalf("reseller listed %d sub-accounts, want 2", len(got))
	}
	if got := list(reseller, other); len(got) != 0 {
		t.Fatalf("reseller listed %d of another reseller's sub-accounts, want 0", len(got))
	}
}
