package domain

import (
	"errors"
	"slices"

	"github.com/google/uuid"
)

// Permission names (OQ-05).
const (
	PermAll               = "*"
	PermAccountRead       = "account:read"
	PermAccountManage     = "account:manage"
	PermMembersRead       = "members:read"
	PermMembersManage     = "members:manage"
	PermCredentialsRead   = "credentials:read"
	PermCredentialsManage = "credentials:manage"
	PermBillingRead       = "billing:read"
	PermBillingManage     = "billing:manage"
)

// Built-in role IDs.
var (
	BuiltinDeveloperRoleID = uuid.MustParse("019a0000-0000-7000-8000-000000000002")
	BuiltinFinanceRoleID   = uuid.MustParse("019a0000-0000-7000-8000-000000000003")
	BuiltinViewerRoleID    = uuid.MustParse("019a0000-0000-7000-8000-000000000004")
)

// Role is a named set of permissions.
type Role struct {
	ID          uuid.UUID
	Name        string
	Permissions []string
}

var (
	ErrPermissionDenied = errors.New("domain: the role doesn't allow this")
	ErrLastAdmin        = errors.New("domain: the account would have no active Admin")
	ErrNotBuiltinRole   = errors.New("domain: only built-in roles can be assigned")
)

// catalogue is every permission accounts knows (agreed 2 Nov, OQ-05).
// Only account:*, members:* and credentials:* are enforced today;
// billing:* is reserved for Phase 3+, and later services add their own
// names (sms:send ...) here as they're built.
var catalogue = []string{
	PermAccountRead, PermAccountManage,
	PermMembersRead, PermMembersManage,
	PermCredentialsRead, PermCredentialsManage,
	PermBillingRead, PermBillingManage,
}

// builtinRoles must match what migrations seed (20261030160000,
// 20261103090000); a store test checks they do.
var builtinRoles = []Role{
	{ID: BuiltinAdminRoleID, Name: "Admin", Permissions: []string{PermAll}},
	{ID: BuiltinDeveloperRoleID, Name: "Developer", Permissions: []string{PermAccountRead, PermMembersRead, PermCredentialsRead, PermCredentialsManage}},
	{ID: BuiltinFinanceRoleID, Name: "Finance", Permissions: []string{PermAccountRead, PermMembersRead, PermBillingRead, PermBillingManage}},
	{ID: BuiltinViewerRoleID, Name: "Viewer", Permissions: []string{PermAccountRead, PermMembersRead, PermCredentialsRead, PermBillingRead}},
}

// Catalogue returns every known permission name.
func Catalogue() []string { return slices.Clone(catalogue) }

// IsPermission reports whether p is in the catalogue.
func IsPermission(p string) bool { return slices.Contains(catalogue, p) }

// BuiltinRoles returns the built-in roles, Admin first.
func BuiltinRoles() []Role {
	roles := make([]Role, len(builtinRoles))
	for i, r := range builtinRoles {
		roles[i] = Role{ID: r.ID, Name: r.Name, Permissions: slices.Clone(r.Permissions)}
	}
	return roles
}

// IsBuiltinRole reports whether id is a built-in role.
func IsBuiltinRole(id uuid.UUID) bool {
	return slices.ContainsFunc(builtinRoles, func(r Role) bool { return r.ID == id })
}

// Allows reports whether a role with perms may do p. "*" allows every
// catalogue permission; a name outside the catalogue is never allowed,
// so a typo fails closed.
func Allows(perms []string, p string) bool {
	if !IsPermission(p) {
		return false
	}
	return slices.Contains(perms, PermAll) || slices.Contains(perms, p)
}

// MembershipChange is a requested role change (NewRoleID set) or
// removal (NewRoleID nil) of Target by Actor.
type MembershipChange struct {
	Actor, Target  uuid.UUID
	ActorCanManage bool       // the actor's role allows members:manage
	TargetRoleID   uuid.UUID  // the target's current role
	TargetActive   bool       // the target is active (not just invited)
	NewRoleID      *uuid.UUID // nil for a removal
	ActiveAdmins   int        // active members with the Admin role now
}

// CheckMembershipChange reports whether c is allowed (agreed 2 Nov):
//   - changing roles and removing others needs members:manage; anyone
//     may remove themselves (leave);
//   - only built-in roles can be assigned;
//   - an account always keeps an active Admin, so the last one can't be
//     demoted, removed or leave.
func CheckMembershipChange(c MembershipChange) error {
	leaving := c.NewRoleID == nil && c.Actor == c.Target
	if !leaving && !c.ActorCanManage {
		return ErrPermissionDenied
	}
	if c.NewRoleID != nil && !IsBuiltinRole(*c.NewRoleID) {
		return ErrNotBuiltinRole
	}
	losesAdmin := c.TargetRoleID == BuiltinAdminRoleID && c.TargetActive &&
		(c.NewRoleID == nil || *c.NewRoleID != BuiltinAdminRoleID)
	if losesAdmin && c.ActiveAdmins <= 1 {
		return ErrLastAdmin
	}
	return nil
}
