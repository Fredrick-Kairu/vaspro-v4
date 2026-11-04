//go:build integration

package store_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/accounts/domain"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/accounts/store"
)

// TestBuiltinRoles_SeedMatchesDomain: the roles the migrations seed are
// exactly domain.BuiltinRoles, which the permission matrix tests.
func TestBuiltinRoles_SeedMatchesDomain(t *testing.T) {
	db := newAccountsDB(t)
	rows, err := store.New(db.app).ListBuiltinRoles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := domain.BuiltinRoles()
	if len(rows) != len(want) {
		t.Fatalf("%d built-in roles seeded, domain has %d", len(rows), len(want))
	}
	for i, r := range rows {
		if r.ID != want[i].ID || r.Name != want[i].Name || !slices.Equal(r.Permissions, want[i].Permissions) {
			t.Errorf("seeded %s %q %v, domain %s %q %v", r.ID, r.Name, r.Permissions, want[i].ID, want[i].Name, want[i].Permissions)
		}
	}
}

// TestMembershipLookupRole_OnlyTheCallersOwnMemberships: the cross-
// account "my accounts" read sees one user's memberships and their
// accounts, only inside WithRoleTx with app.user_id set.
func TestMembershipLookupRole_OnlyTheCallersOwnMemberships(t *testing.T) {
	db := newAccountsDB(t)
	ctx := context.Background()
	ada, bob := uuid.New(), uuid.New()
	adaA, adaB, bobOnly := mustTenant(t, db.super), mustTenant(t, db.super), mustTenant(t, db.super)
	for _, m := range []struct{ tenant, user uuid.UUID }{{adaA, ada}, {adaB, ada}, {bobOnly, bob}, {adaA, bob}} {
		if _, err := db.super.Exec(ctx, `INSERT INTO memberships (tenant_id, user_id, role_id, status) VALUES ($1, $2, $3, 'active')`,
			m.tenant, m.user, domain.BuiltinAdminRoleID); err != nil {
			t.Fatal(err)
		}
	}

	lookup := func(as uuid.UUID, setUser bool) (memberships, tenants int) {
		t.Helper()
		err := db.app.WithRoleTx(ctx, "accounts_membership_lookup", func(ctx context.Context, tx pgx.Tx) error {
			if setUser {
				if _, err := tx.Exec(ctx, "SELECT set_config('app.user_id', $1, true)", as.String()); err != nil {
					return err
				}
			}
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM memberships`).Scan(&memberships); err != nil {
				return err
			}
			return tx.QueryRow(ctx, `SELECT count(*) FROM tenants`).Scan(&tenants)
		})
		if err != nil {
			t.Fatal(err)
		}
		return memberships, tenants
	}
	if m, tn := lookup(ada, true); m != 2 || tn != 2 {
		t.Fatalf("ada sees %d memberships and %d accounts, want 2 and 2", m, tn)
	}
	if m, tn := lookup(bob, true); m != 2 || tn != 2 {
		t.Fatalf("bob sees %d memberships and %d accounts, want 2 and 2", m, tn)
	}
	if m, tn := lookup(ada, false); m != 0 || tn != 0 {
		t.Fatalf("with no user set the role sees %d memberships and %d accounts, want none", m, tn)
	}

	var n int
	if err := pgx.BeginFunc(ctx, db.app, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT set_config('app.user_id', $1, true)", ada.String()); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM memberships`).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("outside WithRoleTx the app role sees %d memberships with app.user_id set, want 0 (WITH INHERIT FALSE)", n)
	}
}

// TestKYCWebhookRole_FindsChecksOnlyInsideWithRoleTx: a vendor webhook
// names a check by reference across accounts, only as the narrow role,
// and that role reads kyc_checks and nothing else.
func TestKYCWebhookRole_FindsChecksOnlyInsideWithRoleTx(t *testing.T) {
	db := newAccountsDB(t)
	ctx := context.Background()
	tenant := mustTenant(t, db.super)
	check := uuid.New()
	if _, err := db.super.Exec(ctx, `INSERT INTO kyc_checks (id, tenant_id, kind, subject_id, provider, provider_ref, status)
		VALUES ($1, $2, 'individual', $3, 'fake', 'fkyc_1', 'pending')`, check, tenant, uuid.New()); err != nil {
		t.Fatal(err)
	}
	params := store.FindKycCheckByProviderRefParams{Provider: "fake", ProviderRef: pgtype.Text{String: "fkyc_1", Valid: true}}

	var found store.FindKycCheckByProviderRefRow
	if err := db.app.WithRoleTx(ctx, "accounts_kyc_webhook", func(ctx context.Context, tx pgx.Tx) error {
		var err error
		found, err = store.New(tx).FindKycCheckByProviderRef(ctx, params)
		return err
	}); err != nil || found.ID != check || found.TenantID != tenant {
		t.Fatalf("inside WithRoleTx: got %+v, %v", found, err)
	}
	err := pgx.BeginFunc(ctx, db.app, func(tx pgx.Tx) error {
		_, err := store.New(tx).FindKycCheckByProviderRef(ctx, params)
		return err
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("outside WithRoleTx: got %v, want no rows (WITH INHERIT FALSE)", err)
	}
	err = db.app.WithRoleTx(ctx, "accounts_kyc_webhook", func(ctx context.Context, tx pgx.Tx) error {
		var n int
		return tx.QueryRow(ctx, `SELECT count(*) FROM tenants`).Scan(&n)
	})
	if pgCode(err) != "42501" {
		t.Fatalf("the role reading tenants: got %v, want permission denied (42501)", err)
	}
}

// TestRegistrationLookupRole_CountsAcrossAccountsOnlyInsideWithRoleTx:
// KYB's duplicate flag counts other accounts' profiles with the same
// registration number, only as the narrow role.
func TestRegistrationLookupRole_CountsAcrossAccountsOnlyInsideWithRoleTx(t *testing.T) {
	db := newAccountsDB(t)
	ctx := context.Background()
	mine, theirs := mustTenant(t, db.super), mustTenant(t, db.super)
	for _, tenant := range []uuid.UUID{mine, theirs} {
		if _, err := db.super.Exec(ctx, `INSERT INTO business_profiles (id, tenant_id, registry, registration_number, registered_name)
			VALUES ($1, $2, 'BRS', 'PVT-SHARED', 'Acme')`, uuid.New(), tenant); err != nil {
			t.Fatal(err)
		}
	}
	params := store.CountOtherProfilesWithRegistrationParams{Registry: "BRS", RegistrationNumber: "PVT-SHARED", TenantID: mine}

	var n int64
	if err := db.app.WithRoleTx(ctx, "accounts_registration_lookup", func(ctx context.Context, tx pgx.Tx) error {
		var err error
		n, err = store.New(tx).CountOtherProfilesWithRegistration(ctx, params)
		return err
	}); err != nil || n != 1 {
		t.Fatalf("inside WithRoleTx: got %d, %v; want 1 other account", n, err)
	}
	if err := pgx.BeginFunc(ctx, db.app, func(tx pgx.Tx) error {
		var err error
		n, err = store.New(tx).CountOtherProfilesWithRegistration(ctx, params)
		return err
	}); err != nil || n != 0 {
		t.Fatalf("outside WithRoleTx: got %d, %v; want 0 (WITH INHERIT FALSE)", n, err)
	}
}

// TestStaffReviewRole_ReadsChecksAcrossAccountsOnlyInsideWithRoleTx:
// the staff queue (task 2.9) reads every account's checks, and the
// accounts that have one, only as the narrow role, and writes nothing.
func TestStaffReviewRole_ReadsChecksAcrossAccountsOnlyInsideWithRoleTx(t *testing.T) {
	db := newAccountsDB(t)
	ctx := context.Background()
	a, b, noChecks := mustTenant(t, db.super), mustTenant(t, db.super), mustTenant(t, db.super)
	checkA, checkB := uuid.New(), uuid.New()
	for _, c := range []struct {
		id, tenant uuid.UUID
		status     string
	}{{checkA, a, "review"}, {checkB, b, "passed"}} {
		if _, err := db.super.Exec(ctx, `INSERT INTO kyc_checks (id, tenant_id, kind, subject_id, provider, status)
			VALUES ($1, $2, 'business', $3, 'manual', $4)`, c.id, c.tenant, uuid.New(), c.status); err != nil {
			t.Fatal(err)
		}
	}
	list := store.ListKycReviewsParams{MaxRows: 10}

	if err := db.app.WithRoleTx(ctx, "accounts_staff_review", func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		rows, err := q.ListKycReviews(ctx, list)
		if err != nil || len(rows) != 1 || rows[0].ID != checkA || rows[0].TenantID != a {
			t.Fatalf("the queue: got %+v, %v; want only the check in review, with its account", rows, err)
		}
		if tenant, err := q.FindKycCheckTenant(ctx, checkB); err != nil || tenant != b {
			t.Fatalf("a decided check's account: got %s, %v", tenant, err)
		}
		var visible int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM tenants WHERE id = ANY($1)`, []uuid.UUID{a, b, noChecks}).Scan(&visible); err != nil || visible != 2 {
			t.Fatalf("accounts visible: %d, %v; want the 2 with checks", visible, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := pgx.BeginFunc(ctx, db.app, func(tx pgx.Tx) error {
		rows, err := store.New(tx).ListKycReviews(ctx, list)
		if err != nil || len(rows) != 0 {
			t.Fatalf("outside WithRoleTx: got %d rows, %v; want none (WITH INHERIT FALSE)", len(rows), err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for name, stmt := range map[string]string{
		"decide a check":   `UPDATE kyc_checks SET status = 'passed' WHERE id = '` + checkA.String() + `'`,
		"change a tier":    `UPDATE tenants SET display_name = 'x' WHERE id = '` + a.String() + `'`,
		"read memberships": `SELECT count(*) FROM memberships`,
	} {
		err := db.app.WithRoleTx(ctx, "accounts_staff_review", func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, stmt)
			return err
		})
		if pgCode(err) != "42501" {
			t.Fatalf("%s as the role: got %v, want permission denied (42501)", name, err)
		}
	}
}
