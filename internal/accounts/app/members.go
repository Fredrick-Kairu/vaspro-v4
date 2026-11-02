package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	platformevents "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/events"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/ids"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/outbox"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/accounts/domain"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/accounts/store"
)

// ErrUnknownPermission means a caller asked about a name outside the
// permission catalogue: a programming error, not a denial.
var ErrUnknownPermission = errors.New("app: unknown permission")

// membershipLookupRole may read one user's memberships across accounts
// (migration 20261103090000), and only inside WithRoleTx.
const membershipLookupRole = "accounts_membership_lookup"

// Member is one person's membership of an account.
type Member struct {
	UserID    uuid.UUID
	RoleID    uuid.UUID
	Status    string
	InvitedBy *uuid.UUID
	CreatedAt time.Time
}

func memberFromRow(row store.AccountsMembership) Member {
	m := Member{UserID: row.UserID, RoleID: row.RoleID, Status: row.Status, CreatedAt: row.CreatedAt}
	if row.InvitedBy.Valid {
		id := uuid.UUID(row.InvitedBy.Bytes)
		m.InvitedBy = &id
	}
	return m
}

// MyMembership is one account the caller belongs to.
type MyMembership struct {
	TenantID    uuid.UUID
	DisplayName string
	Type        string
	Tier        string
	RoleID      uuid.UUID
	Status      string
}

// ListRoles returns the built-in roles as stored (task 2.3a).
func (s *Service) ListRoles(ctx context.Context) ([]domain.Role, error) {
	rows, err := store.New(s.pool).ListBuiltinRoles(ctx)
	if err != nil {
		return nil, fmt.Errorf("app: listing roles: %w", err)
	}
	roles := make([]domain.Role, len(rows))
	for i, r := range rows {
		roles[i] = domain.Role{ID: r.ID, Name: r.Name, Permissions: r.Permissions}
	}
	return roles, nil
}

// ListMembers returns tenantID's active and invited members if userID's
// role allows members:read.
func (s *Service) ListMembers(ctx context.Context, userID, tenantID uuid.UUID) ([]Member, error) {
	if _, _, err := s.authorize(ctx, userID, tenantID, domain.PermMembersRead); err != nil {
		return nil, err
	}
	var members []Member
	err := s.pool.WithTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := store.New(tx).ListMembers(ctx, tenantID)
		if err != nil {
			return fmt.Errorf("app: listing members: %w", err)
		}
		members = make([]Member, len(rows))
		for i, row := range rows {
			members[i] = memberFromRow(row)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return members, nil
}

// ChangeMemberRole gives target a built-in role, writing
// membership.role_changed (domain.CheckMembershipChange: members:manage,
// built-in roles only, and an account always keeps an active Admin).
func (s *Service) ChangeMemberRole(ctx context.Context, actor, tenantID, target, roleID uuid.UUID) (*Member, error) {
	return s.changeMembership(ctx, actor, tenantID, target, &roleID)
}

// RemoveMember removes target from tenantID, or lets a member leave
// (actor == target), writing membership.removed. The last active Admin
// can't be removed or leave.
func (s *Service) RemoveMember(ctx context.Context, actor, tenantID, target uuid.UUID) error {
	_, err := s.changeMembership(ctx, actor, tenantID, target, nil)
	return err
}

// changeMembership is ChangeMemberRole (newRole set) and RemoveMember
// (newRole nil). The actor's role comes from their own membership or,
// for a sub-account, the reseller's. The account row is locked so two
// Admins demoting each other at once can't leave it with none.
func (s *Service) changeMembership(ctx context.Context, actor, tenantID, target uuid.UUID, newRole *uuid.UUID) (*Member, error) {
	_, perms, err := s.authorize(ctx, actor, tenantID, "")
	if err != nil {
		return nil, err
	}
	canManage := domain.Allows(perms, domain.PermMembersManage)
	leaving := newRole == nil && actor == target
	if !leaving && !canManage {
		// Before looking the target up, so a member without the
		// permission learns nothing about who else is in the account.
		return nil, domain.ErrPermissionDenied
	}

	var result *Member
	err = s.pool.WithTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		if _, err := q.GetTenantForUpdate(ctx, tenantID); err != nil {
			return fmt.Errorf("app: locking tenant: %w", err)
		}
		current, err := q.GetMembership(ctx, store.GetMembershipParams{TenantID: tenantID, UserID: target})
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && current.Status == "removed") {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("app: loading membership: %w", err)
		}
		admins, err := q.CountActiveMembersWithRole(ctx, store.CountActiveMembersWithRoleParams{TenantID: tenantID, RoleID: domain.BuiltinAdminRoleID})
		if err != nil {
			return fmt.Errorf("app: counting Admins: %w", err)
		}
		if err := domain.CheckMembershipChange(domain.MembershipChange{
			Actor: actor, Target: target, ActorCanManage: canManage,
			TargetRoleID: current.RoleID, TargetActive: current.Status == "active",
			NewRoleID: newRole, ActiveAdmins: int(admins),
		}); err != nil {
			return err
		}

		var (
			eventType string
			data      map[string]any
		)
		if newRole == nil {
			if err := q.RemoveMembership(ctx, store.RemoveMembershipParams{TenantID: tenantID, UserID: target}); err != nil {
				return fmt.Errorf("app: removing member: %w", err)
			}
			eventType = "membership.removed"
			data = map[string]any{"tenant_id": tenantID, "user_id": target, "removed_by": actor}
			current.Status = "removed"
		} else {
			if *newRole == current.RoleID {
				m := memberFromRow(current)
				result = &m
				return nil
			}
			if err := q.UpdateMembershipRole(ctx, store.UpdateMembershipRoleParams{TenantID: tenantID, UserID: target, RoleID: *newRole}); err != nil {
				return fmt.Errorf("app: changing role: %w", err)
			}
			eventType = "membership.role_changed"
			data = map[string]any{"tenant_id": tenantID, "user_id": target, "from_role_id": current.RoleID, "to_role_id": *newRole, "changed_by": actor}
			current.RoleID = *newRole
		}

		eventID, err := ids.New()
		if err != nil {
			return fmt.Errorf("app: generating event id: %w", err)
		}
		env, err := platformevents.New(eventID, eventType, 1, tenantID, s.clk.Now(), "", data)
		if err != nil {
			return fmt.Errorf("app: building %s event: %w", eventType, err)
		}
		if err := outbox.Write(ctx, tx, "tenant", tenantID, env); err != nil {
			return err
		}
		m := memberFromRow(current)
		result = &m
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// MyMemberships returns every account userID is an active or invited
// member of ("one email, several accounts", system design §2). It reads
// across accounts as the narrow accounts_membership_lookup role, whose
// policies only show app.user_id's own memberships.
func (s *Service) MyMemberships(ctx context.Context, userID uuid.UUID) ([]MyMembership, error) {
	var out []MyMembership
	err := s.pool.WithRoleTx(ctx, membershipLookupRole, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT set_config('app.user_id', $1, true)", userID.String()); err != nil {
			return fmt.Errorf("app: setting the user for the lookup: %w", err)
		}
		rows, err := store.New(tx).ListMyMemberships(ctx, userID)
		if err != nil {
			return fmt.Errorf("app: listing memberships: %w", err)
		}
		out = make([]MyMembership, len(rows))
		for i, r := range rows {
			out[i] = MyMembership{TenantID: r.TenantID, DisplayName: r.DisplayName, Type: string(r.Type), Tier: string(r.Tier), RoleID: r.RoleID, Status: r.Status}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
