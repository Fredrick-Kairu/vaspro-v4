//go:build integration

package app_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/accounts/app"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/accounts/domain"
)

// addMember gives user an active Admin membership of tenant.
func (h harness) addMember(t *testing.T, tenant, user uuid.UUID) {
	t.Helper()
	if _, err := h.super.Exec(context.Background(),
		`INSERT INTO memberships (tenant_id, user_id, role_id, status) VALUES ($1, $2, $3, 'active')`,
		tenant, user, domain.BuiltinAdminRoleID); err != nil {
		t.Fatalf("insert membership: %v", err)
	}
}

// promoteToFinancial has staff move tenant straight to financial.
func (h harness) promoteToFinancial(t *testing.T, tenant uuid.UUID) {
	t.Helper()
	if err := h.svc.ChangeTier(context.Background(), app.ChangeTierRequest{
		TenantID: tenant, To: domain.TierFinancial, Reason: "contract signed", ChangedBy: ptr(uuid.New()),
	}); err != nil {
		t.Fatalf("promoting to financial: %v", err)
	}
}

// newReseller returns an active financial reseller and one active
// member of it.
func (h harness) newReseller(t *testing.T) (tenant, member uuid.UUID) {
	t.Helper()
	tenant = h.newTenant(t, domain.TenantActive)
	h.promoteToFinancial(t, tenant)
	if err := h.svc.MakeReseller(context.Background(), app.MakeResellerRequest{
		TenantID: tenant, ChangedBy: uuid.New(), Reason: "reseller agreement signed",
	}); err != nil {
		t.Fatalf("MakeReseller: %v", err)
	}
	member = uuid.New()
	h.addMember(t, tenant, member)
	return tenant, member
}

func (h harness) typeOf(t *testing.T, tenant uuid.UUID) string {
	t.Helper()
	var typ string
	if err := h.super.QueryRow(context.Background(), `SELECT type FROM tenants WHERE id = $1`, tenant).Scan(&typ); err != nil {
		t.Fatal(err)
	}
	return typ
}

func TestMakeReseller_WritesTypeAndEventTogether(t *testing.T) {
	h := newHarness(t)
	tenant := h.newTenant(t, domain.TenantActive)
	h.promoteToFinancial(t, tenant)
	staff := uuid.New()

	if err := h.svc.MakeReseller(context.Background(), app.MakeResellerRequest{
		TenantID: tenant, ChangedBy: staff, Reason: "reseller agreement signed",
	}); err != nil {
		t.Fatalf("MakeReseller: %v", err)
	}
	if got := h.typeOf(t, tenant); got != "reseller" {
		t.Fatalf("type %q, want reseller", got)
	}
	data := assertEventValid(t, h, "tenant.type_changed", tenant)
	if data["from_type"] != "direct" || data["to_type"] != "reseller" || data["changed_by"] != staff.String() {
		t.Fatalf("got event data %v", data)
	}
}

func TestMakeReseller_RefusedChangesWriteNothing(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	belowFinancial := h.newTenant(t, domain.TenantActive)
	suspended := h.newTenant(t, domain.TenantActive)
	h.promoteToFinancial(t, suspended)
	if _, err := h.super.Exec(ctx, `UPDATE tenants SET status = 'suspended' WHERE id = $1`, suspended); err != nil {
		t.Fatal(err)
	}
	reseller, member := h.newReseller(t)
	sub, err := h.svc.CreateSubAccount(ctx, member, reseller, app.SubAccountInput{DisplayName: "Client"})
	if err != nil {
		t.Fatalf("CreateSubAccount: %v", err)
	}
	h.promoteToFinancial(t, sub.ID)
	financialDirect := h.newTenant(t, domain.TenantActive)
	h.promoteToFinancial(t, financialDirect)

	cases := []struct {
		name   string
		tenant uuid.UUID
		reason string
		want   error
	}{
		{"a direct account below financial", belowFinancial, "agreement", domain.ErrResellerNeedsFinancial},
		{"a suspended account", suspended, "agreement", domain.ErrTenantNotActive},
		{"an existing reseller", reseller, "agreement", domain.ErrNotDirect},
		{"a sub-account, even on financial", sub.ID, "agreement", domain.ErrNotDirect},
		{"no reason", financialDirect, " ", domain.ErrNoReason},
		{"an unknown tenant", uuid.New(), "agreement", app.ErrNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := h.count(t, `SELECT count(*) FROM outbox_events WHERE event_type = 'tenant.type_changed'`)
			var typeBefore string
			_ = h.super.QueryRow(ctx, `SELECT type FROM tenants WHERE id = $1`, tc.tenant).Scan(&typeBefore)

			err := h.svc.MakeReseller(ctx, app.MakeResellerRequest{TenantID: tc.tenant, ChangedBy: uuid.New(), Reason: tc.reason})
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			var typeAfter string
			_ = h.super.QueryRow(ctx, `SELECT type FROM tenants WHERE id = $1`, tc.tenant).Scan(&typeAfter)
			if typeAfter != typeBefore {
				t.Fatalf("type changed %q → %q", typeBefore, typeAfter)
			}
			if after := h.count(t, `SELECT count(*) FROM outbox_events WHERE event_type = 'tenant.type_changed'`); after != before {
				t.Fatal("a refused change wrote an event")
			}
		})
	}
}

func TestCreateSubAccount_WritesSandboxSubAccountUnderTheReseller(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	reseller, member := h.newReseller(t)

	sub, err := h.svc.CreateSubAccount(ctx, member, reseller, app.SubAccountInput{
		DisplayName: " Kilimo Traders ", LegalName: "Kilimo Traders Limited", BillingEmail: "accounts@kilimo.example",
	})
	if err != nil {
		t.Fatalf("CreateSubAccount: %v", err)
	}
	if sub.Type != "sub_account" || sub.ParentTenantID == nil || *sub.ParentTenantID != reseller ||
		sub.Tier != domain.TierSandbox || sub.Status != "active" || sub.DisplayName != "Kilimo Traders" ||
		sub.CountryCode != "KE" || sub.Timezone != "Africa/Nairobi" || sub.DefaultCurrency != "KES" || sub.Region != "ke" {
		t.Fatalf("got %+v", sub)
	}
	if n := h.count(t, `SELECT count(*) FROM memberships WHERE tenant_id = $1`, sub.ID); n != 0 {
		t.Fatalf("got %d memberships on the sub-account, want none (access is through the reseller)", n)
	}
	data := assertEventValid(t, h, "tenant.created", sub.ID)
	if data["type"] != "sub_account" || data["parent_tenant_id"] != reseller.String() || data["created_by"] != member.String() ||
		data["region"] != "ke" {
		t.Fatalf("got event data %v", data)
	}
}

func TestCreateSubAccount_Refusals(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	reseller, member := h.newReseller(t)

	direct := h.newTenant(t, domain.TenantActive)
	h.promoteToFinancial(t, direct)
	directMember := uuid.New()
	h.addMember(t, direct, directMember)

	demoted, demotedMember := h.newReseller(t)
	if err := h.svc.ChangeTier(ctx, app.ChangeTierRequest{
		TenantID: demoted, To: domain.TierBusiness, Reason: "contract ended", ChangedBy: ptr(uuid.New()),
	}); err != nil {
		t.Fatal(err)
	}
	suspended, suspendedMember := h.newReseller(t)
	if _, err := h.super.Exec(ctx, `UPDATE tenants SET status = 'suspended' WHERE id = $1`, suspended); err != nil {
		t.Fatal(err)
	}
	sub, err := h.svc.CreateSubAccount(ctx, member, reseller, app.SubAccountInput{DisplayName: "Client"})
	if err != nil {
		t.Fatal(err)
	}
	subMember := uuid.New()
	h.addMember(t, sub.ID, subMember)

	cases := []struct {
		name   string
		user   uuid.UUID
		parent uuid.UUID
		in     app.SubAccountInput
		want   error
	}{
		{"a non-member of the reseller", uuid.New(), reseller, app.SubAccountInput{DisplayName: "X"}, app.ErrNotFound},
		{"an unknown parent", member, uuid.New(), app.SubAccountInput{DisplayName: "X"}, app.ErrNotFound},
		{"a direct account, even on financial", directMember, direct, app.SubAccountInput{DisplayName: "X"}, domain.ErrNotReseller},
		{"a sub-account (two levels only)", subMember, sub.ID, app.SubAccountInput{DisplayName: "X"}, domain.ErrNotReseller},
		{"a reseller demoted below financial", demotedMember, demoted, app.SubAccountInput{DisplayName: "X"}, domain.ErrResellerNeedsFinancial},
		{"a suspended reseller", suspendedMember, suspended, app.SubAccountInput{DisplayName: "X"}, domain.ErrTenantNotActive},
		{"no display name", member, reseller, app.SubAccountInput{DisplayName: " "}, app.ErrValidation},
		{"a bad billing email", member, reseller, app.SubAccountInput{DisplayName: "X", BillingEmail: "nope"}, app.ErrValidation},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := h.count(t, `SELECT count(*) FROM tenants`)
			if _, err := h.svc.CreateSubAccount(ctx, tc.user, tc.parent, tc.in); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			if after := h.count(t, `SELECT count(*) FROM tenants`); after != before {
				t.Fatal("a refused sub-account was written")
			}
		})
	}
}

func TestSubAccountAccess(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	reseller, member := h.newReseller(t)
	otherReseller, otherMember := h.newReseller(t)

	sub, err := h.svc.CreateSubAccount(ctx, member, reseller, app.SubAccountInput{DisplayName: "Client A"})
	if err != nil {
		t.Fatal(err)
	}
	sibling, err := h.svc.CreateSubAccount(ctx, member, reseller, app.SubAccountInput{DisplayName: "Client B"})
	if err != nil {
		t.Fatal(err)
	}
	otherSub, err := h.svc.CreateSubAccount(ctx, otherMember, otherReseller, app.SubAccountInput{DisplayName: "Other client"})
	if err != nil {
		t.Fatal(err)
	}
	subMember := uuid.New()
	h.addMember(t, sub.ID, subMember)

	canRead := func(user, tenant uuid.UUID) bool {
		t.Helper()
		got, err := h.svc.GetTenant(ctx, user, tenant)
		if errors.Is(err, app.ErrNotFound) {
			return false
		}
		if err != nil {
			t.Fatalf("GetTenant: %v", err)
		}
		if got.ID != tenant {
			t.Fatalf("asked for %s, got %s", tenant, got.ID)
		}
		return true
	}
	cases := []struct {
		name   string
		user   uuid.UUID
		tenant uuid.UUID
		want   bool
	}{
		{"a reseller member reads its sub-account through the parent", member, sub.ID, true},
		{"a reseller member reads the reseller itself", member, reseller, true},
		{"another reseller's member can't read it", otherMember, sub.ID, false},
		{"a reseller member can't read another reseller's sub-account", member, otherSub.ID, false},
		{"a sub-account's own member reads it", subMember, sub.ID, true},
		{"a sub-account's member can't read its reseller", subMember, reseller, false},
		{"a sub-account's member can't read a sibling", subMember, sibling.ID, false},
		{"a stranger can't read it", uuid.New(), sub.ID, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := canRead(tc.user, tc.tenant); got != tc.want {
				t.Fatalf("can read = %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("listing shows only that reseller's sub-accounts, oldest first", func(t *testing.T) {
		got, err := h.svc.ListSubAccounts(ctx, member, reseller)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[0].ID != sub.ID || got[1].ID != sibling.ID {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("a non-member can't list them", func(t *testing.T) {
		if _, err := h.svc.ListSubAccounts(ctx, otherMember, reseller); !errors.Is(err, app.ErrNotFound) {
			t.Fatalf("got %v, want ErrNotFound", err)
		}
	})
	t.Run("a sub-account's member can't list the reseller's sub-accounts", func(t *testing.T) {
		if _, err := h.svc.ListSubAccounts(ctx, subMember, reseller); !errors.Is(err, app.ErrNotFound) {
			t.Fatalf("got %v, want ErrNotFound", err)
		}
	})
}
