package domain_test

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/accounts/domain"
)

// TestBuiltinRoles_PermissionMatrix is Phase 2's role × permission
// matrix: every built-in role against every permission in the
// catalogue, each expectation written out (agreed 2 Nov, OQ-05).
func TestBuiltinRoles_PermissionMatrix(t *testing.T) {
	const (
		y = true
		n = false
	)
	perms := []string{
		domain.PermAccountRead, domain.PermAccountManage,
		domain.PermMembersRead, domain.PermMembersManage,
		domain.PermCredentialsRead, domain.PermCredentialsManage,
		domain.PermBillingRead, domain.PermBillingManage,
	}
	want := map[uuid.UUID][]bool{
		//                              acct r  acct m  mem r  mem m  cred r  cred m  bill r  bill m
		domain.BuiltinAdminRoleID:     {y, y, y, y, y, y, y, y},
		domain.BuiltinDeveloperRoleID: {y, n, y, n, y, y, n, n},
		domain.BuiltinFinanceRoleID:   {y, n, y, n, n, n, y, y},
		domain.BuiltinViewerRoleID:    {y, n, y, n, y, n, y, n},
	}
	if len(domain.Catalogue()) != len(perms) {
		t.Fatalf("the catalogue has %d permissions, this matrix covers %d: add the new ones here", len(domain.Catalogue()), len(perms))
	}
	if len(domain.BuiltinRoles()) != len(want) {
		t.Fatalf("%d built-in roles, this matrix covers %d", len(domain.BuiltinRoles()), len(want))
	}
	for _, role := range domain.BuiltinRoles() {
		row, ok := want[role.ID]
		if !ok {
			t.Fatalf("role %s (%s) isn't in the matrix", role.Name, role.ID)
		}
		for i, p := range perms {
			if got := domain.Allows(role.Permissions, p); got != row[i] {
				t.Errorf("%s × %s = %v, want %v", role.Name, p, got, row[i])
			}
		}
	}
}

func TestAllows(t *testing.T) {
	cases := []struct {
		name  string
		perms []string
		p     string
		want  bool
	}{
		{"a listed permission", []string{domain.PermAccountRead}, domain.PermAccountRead, true},
		{"an unlisted permission", []string{domain.PermAccountRead}, domain.PermAccountManage, false},
		{"* allows a catalogue permission", []string{domain.PermAll}, domain.PermCredentialsManage, true},
		{"* never allows a name outside the catalogue", []string{domain.PermAll}, "payouts:approve", false},
		{"a listed name outside the catalogue is still not allowed", []string{"payouts:approve"}, "payouts:approve", false},
		{"the empty name is never allowed", []string{domain.PermAll}, "", false},
		{"no permissions allow nothing", nil, domain.PermAccountRead, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := domain.Allows(tc.perms, tc.p); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCheckMembershipChange(t *testing.T) {
	admin, dev := domain.BuiltinAdminRoleID, domain.BuiltinDeveloperRoleID
	actor, other := uuid.New(), uuid.New()
	custom := uuid.New()
	cases := []struct {
		name string
		c    domain.MembershipChange
		want error
	}{
		{"an Admin changes another member's role", domain.MembershipChange{Actor: actor, Target: other, ActorCanManage: true, TargetRoleID: dev, NewRoleID: &admin, ActiveAdmins: 1}, nil},
		{"an Admin demotes one of two Admins", domain.MembershipChange{Actor: actor, Target: other, ActorCanManage: true, TargetRoleID: admin, TargetActive: true, NewRoleID: &dev, ActiveAdmins: 2}, nil},
		{"the last active Admin can't be demoted", domain.MembershipChange{Actor: actor, Target: actor, ActorCanManage: true, TargetRoleID: admin, TargetActive: true, NewRoleID: &dev, ActiveAdmins: 1}, domain.ErrLastAdmin},
		{"the last active Admin can't be removed", domain.MembershipChange{Actor: other, Target: actor, ActorCanManage: true, TargetRoleID: admin, TargetActive: true, ActiveAdmins: 1}, domain.ErrLastAdmin},
		{"the last active Admin can't leave", domain.MembershipChange{Actor: actor, Target: actor, TargetRoleID: admin, TargetActive: true, ActiveAdmins: 1}, domain.ErrLastAdmin},
		{"an invited Admin isn't the one keeping the account", domain.MembershipChange{Actor: actor, Target: other, ActorCanManage: true, TargetRoleID: admin, TargetActive: false, ActiveAdmins: 1}, nil},
		{"one of two Admins may leave", domain.MembershipChange{Actor: actor, Target: actor, TargetRoleID: admin, TargetActive: true, ActiveAdmins: 2}, nil},
		{"a member may leave without members:manage", domain.MembershipChange{Actor: actor, Target: actor, TargetRoleID: dev, TargetActive: true, ActiveAdmins: 1}, nil},
		{"removing someone else needs members:manage", domain.MembershipChange{Actor: actor, Target: other, TargetRoleID: dev, TargetActive: true, ActiveAdmins: 1}, domain.ErrPermissionDenied},
		{"changing your own role needs members:manage too", domain.MembershipChange{Actor: actor, Target: actor, TargetRoleID: dev, NewRoleID: &admin, ActiveAdmins: 1}, domain.ErrPermissionDenied},
		{"only built-in roles can be assigned", domain.MembershipChange{Actor: actor, Target: other, ActorCanManage: true, TargetRoleID: dev, NewRoleID: &custom, ActiveAdmins: 1}, domain.ErrNotBuiltinRole},
		{"assigning the same role is allowed (no change)", domain.MembershipChange{Actor: actor, Target: other, ActorCanManage: true, TargetRoleID: dev, NewRoleID: &dev, ActiveAdmins: 1}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := domain.CheckMembershipChange(tc.c)
			if !errors.Is(err, tc.want) || (tc.want == nil && err != nil) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}
