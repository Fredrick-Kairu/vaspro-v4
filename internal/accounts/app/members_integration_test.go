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

// addMemberWithRole gives user an active membership of tenant with role.
func (h harness) addMemberWithRole(t *testing.T, tenant, user, role uuid.UUID) {
	t.Helper()
	if _, err := h.super.Exec(context.Background(),
		`INSERT INTO memberships (tenant_id, user_id, role_id, status) VALUES ($1, $2, $3, 'active')`,
		tenant, user, role); err != nil {
		t.Fatalf("insert membership: %v", err)
	}
}

func (h harness) roleOf(t *testing.T, tenant, user uuid.UUID) (role uuid.UUID, status string) {
	t.Helper()
	if err := h.super.QueryRow(context.Background(),
		`SELECT role_id, status FROM memberships WHERE tenant_id = $1 AND user_id = $2`, tenant, user).Scan(&role, &status); err != nil {
		t.Fatal(err)
	}
	return role, status
}

// newTeam is an account created by admin, plus a Developer and a Viewer.
func (h harness) newTeam(t *testing.T) (tenant, admin, dev, viewer uuid.UUID) {
	t.Helper()
	admin, dev, viewer = uuid.New(), uuid.New(), uuid.New()
	created, err := h.svc.CreateTenant(context.Background(), admin, domain.NewTenantInput{DisplayName: "Team", CountryCode: "KE"})
	if err != nil {
		t.Fatal(err)
	}
	h.addMemberWithRole(t, created.ID, dev, domain.BuiltinDeveloperRoleID)
	h.addMemberWithRole(t, created.ID, viewer, domain.BuiltinViewerRoleID)
	return created.ID, admin, dev, viewer
}

func TestListMembers(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tenant, admin, dev, viewer := h.newTeam(t)

	members, err := h.svc.ListMembers(ctx, viewer, tenant)
	if err != nil {
		t.Fatalf("a Viewer lists members: %v", err)
	}
	got := map[uuid.UUID]uuid.UUID{}
	for _, m := range members {
		got[m.UserID] = m.RoleID
	}
	if len(members) != 3 || got[admin] != domain.BuiltinAdminRoleID || got[dev] != domain.BuiltinDeveloperRoleID || got[viewer] != domain.BuiltinViewerRoleID {
		t.Fatalf("got %+v", members)
	}
	if _, err := h.svc.ListMembers(ctx, uuid.New(), tenant); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("a stranger: got %v, want ErrNotFound", err)
	}
}

func TestChangeMemberRole(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tenant, admin, dev, viewer := h.newTeam(t)

	t.Run("a Developer can't change roles", func(t *testing.T) {
		if _, err := h.svc.ChangeMemberRole(ctx, dev, tenant, viewer, domain.BuiltinDeveloperRoleID); !errors.Is(err, domain.ErrPermissionDenied) {
			t.Fatalf("got %v, want ErrPermissionDenied", err)
		}
	})
	t.Run("the last Admin can't be demoted, not even by themselves", func(t *testing.T) {
		if _, err := h.svc.ChangeMemberRole(ctx, admin, tenant, admin, domain.BuiltinViewerRoleID); !errors.Is(err, domain.ErrLastAdmin) {
			t.Fatalf("got %v, want ErrLastAdmin", err)
		}
	})
	t.Run("only built-in roles", func(t *testing.T) {
		if _, err := h.svc.ChangeMemberRole(ctx, admin, tenant, viewer, uuid.New()); !errors.Is(err, domain.ErrNotBuiltinRole) {
			t.Fatalf("got %v, want ErrNotBuiltinRole", err)
		}
	})
	t.Run("a non-member target is not found", func(t *testing.T) {
		if _, err := h.svc.ChangeMemberRole(ctx, admin, tenant, uuid.New(), domain.BuiltinViewerRoleID); !errors.Is(err, app.ErrNotFound) {
			t.Fatalf("got %v, want ErrNotFound", err)
		}
	})
	t.Run("an Admin promotes the Viewer to Admin, then can step down", func(t *testing.T) {
		m, err := h.svc.ChangeMemberRole(ctx, admin, tenant, viewer, domain.BuiltinAdminRoleID)
		if err != nil || m.RoleID != domain.BuiltinAdminRoleID {
			t.Fatalf("got %+v, %v", m, err)
		}
		data := assertEventValid(t, h, "membership.role_changed", tenant)
		if data["user_id"] != viewer.String() || data["to_role_id"] != domain.BuiltinAdminRoleID.String() || data["changed_by"] != admin.String() {
			t.Fatalf("got event data %v", data)
		}
		if _, err := h.svc.ChangeMemberRole(ctx, admin, tenant, admin, domain.BuiltinDeveloperRoleID); err != nil {
			t.Fatalf("one of two Admins stepping down: %v", err)
		}
		if role, _ := h.roleOf(t, tenant, admin); role != domain.BuiltinDeveloperRoleID {
			t.Fatalf("role %s, want Developer", role)
		}
	})
}

func TestRemoveMember(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tenant, admin, dev, viewer := h.newTeam(t)

	if err := h.svc.RemoveMember(ctx, dev, tenant, viewer); !errors.Is(err, domain.ErrPermissionDenied) {
		t.Fatalf("a Developer removing someone: got %v, want ErrPermissionDenied", err)
	}
	if err := h.svc.RemoveMember(ctx, admin, tenant, admin); !errors.Is(err, domain.ErrLastAdmin) {
		t.Fatalf("the last Admin leaving: got %v, want ErrLastAdmin", err)
	}
	if err := h.svc.RemoveMember(ctx, admin, tenant, viewer); err != nil {
		t.Fatalf("an Admin removing the Viewer: %v", err)
	}
	if _, status := h.roleOf(t, tenant, viewer); status != "removed" {
		t.Fatalf("status %q, want removed", status)
	}
	data := assertEventValid(t, h, "membership.removed", tenant)
	if data["user_id"] != viewer.String() || data["removed_by"] != admin.String() {
		t.Fatalf("got event data %v", data)
	}
	if _, err := h.svc.GetTenant(ctx, viewer, tenant); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("a removed member reading the account: got %v, want ErrNotFound", err)
	}
	if err := h.svc.RemoveMember(ctx, admin, tenant, viewer); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("removing them again: got %v, want ErrNotFound", err)
	}
	if err := h.svc.RemoveMember(ctx, dev, tenant, dev); err != nil {
		t.Fatalf("a Developer leaving: %v", err)
	}
	if n := h.count(t, `SELECT count(*) FROM memberships WHERE tenant_id = $1 AND status = 'active'`, tenant); n != 1 {
		t.Fatalf("%d active members, want only the Admin", n)
	}
}

func TestPermissionsOnAccountsAndSubAccounts(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	reseller, admin := h.newReseller(t)
	viewer := uuid.New()
	h.addMemberWithRole(t, reseller, viewer, domain.BuiltinViewerRoleID)

	sub, err := h.svc.CreateSubAccount(ctx, admin, reseller, app.SubAccountInput{DisplayName: "Client"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.CreateSubAccount(ctx, viewer, reseller, app.SubAccountInput{DisplayName: "X"}); !errors.Is(err, domain.ErrPermissionDenied) {
		t.Fatalf("a Viewer creating a sub-account: got %v, want ErrPermissionDenied", err)
	}
	if _, err := h.svc.GetTenant(ctx, viewer, sub.ID); err != nil {
		t.Fatalf("a Viewer of the reseller reading the sub-account: %v", err)
	}
	if _, err := h.svc.ListSubAccounts(ctx, viewer, reseller); err != nil {
		t.Fatalf("a Viewer listing sub-accounts: %v", err)
	}

	t.Run("CheckAccess", func(t *testing.T) {
		cases := []struct {
			name   string
			user   uuid.UUID
			tenant uuid.UUID
			perm   string
			want   error
		}{
			{"any access", viewer, reseller, "", nil},
			{"a Viewer reads credentials", viewer, reseller, domain.PermCredentialsRead, nil},
			{"a Viewer can't manage credentials", viewer, reseller, domain.PermCredentialsManage, domain.ErrPermissionDenied},
			{"the reseller's role applies to its sub-account", viewer, sub.ID, domain.PermCredentialsManage, domain.ErrPermissionDenied},
			{"an Admin manages credentials of the sub-account", admin, sub.ID, domain.PermCredentialsManage, nil},
			{"an unknown permission", admin, reseller, "payouts:approve", app.ErrUnknownPermission},
			{"a stranger", uuid.New(), reseller, "", app.ErrNotFound},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				err := h.svc.CheckAccess(ctx, tc.user, tc.tenant, tc.perm)
				if !errors.Is(err, tc.want) || (tc.want == nil && err != nil) {
					t.Fatalf("got %v, want %v", err, tc.want)
				}
			})
		}
	})
}

func TestMyMemberships(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	ada := uuid.New()
	first, err := h.svc.CreateTenant(ctx, ada, domain.NewTenantInput{DisplayName: "First", CountryCode: "KE"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := h.svc.CreateTenant(ctx, uuid.New(), domain.NewTenantInput{DisplayName: "Second", CountryCode: "KE"})
	if err != nil {
		t.Fatal(err)
	}
	h.addMemberWithRole(t, second.ID, ada, domain.BuiltinViewerRoleID)
	left, err := h.svc.CreateTenant(ctx, uuid.New(), domain.NewTenantInput{DisplayName: "Left", CountryCode: "KE"})
	if err != nil {
		t.Fatal(err)
	}
	h.addMemberWithRole(t, left.ID, ada, domain.BuiltinViewerRoleID)
	if err := h.svc.RemoveMember(ctx, ada, left.ID, ada); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.CreateTenant(ctx, uuid.New(), domain.NewTenantInput{DisplayName: "Not ada's", CountryCode: "KE"}); err != nil {
		t.Fatal(err)
	}

	got, err := h.svc.MyMemberships(ctx, ada)
	if err != nil {
		t.Fatalf("MyMemberships: %v", err)
	}
	if len(got) != 2 || got[0].TenantID != first.ID || got[0].RoleID != domain.BuiltinAdminRoleID || got[0].DisplayName != "First" ||
		got[1].TenantID != second.ID || got[1].RoleID != domain.BuiltinViewerRoleID {
		t.Fatalf("got %+v, want First as Admin and Second as Viewer", got)
	}
	if none, err := h.svc.MyMemberships(ctx, uuid.New()); err != nil || len(none) != 0 {
		t.Fatalf("a user with no accounts: got %+v, %v", none, err)
	}
}
