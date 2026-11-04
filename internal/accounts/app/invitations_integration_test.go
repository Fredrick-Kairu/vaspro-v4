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

// steppedUpSession is a session of user whose step-up is fresh.
func (h harness) steppedUpSession(user uuid.UUID) uuid.UUID {
	session := uuid.New()
	h.identity.StepUp(user, session)
	return session
}

func TestInvite(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tenant, admin, dev, _ := h.newTeam(t)
	session := h.steppedUpSession(admin)
	bob := h.identity.AddUser("bob@example.com", "Bob")

	m, err := h.svc.Invite(ctx, admin, session, tenant, "Bob@Example.com", domain.BuiltinDeveloperRoleID)
	if err != nil {
		t.Fatalf("Invite: %v", err)
	}
	if m.UserID != bob || m.Status != "invited" || m.RoleID != domain.BuiltinDeveloperRoleID || m.InvitedBy == nil || *m.InvitedBy != admin || m.Email != "bob@example.com" {
		t.Fatalf("got %+v", m)
	}
	data := assertEventValid(t, h, "membership.invited", tenant)
	if data["user_id"] != bob.String() || data["invited_by"] != admin.String() {
		t.Fatalf("got event data %v", data)
	}
	sent := h.identity.Invitations()
	if len(sent) != 1 || sent[0].InviteeID != bob || sent[0].InviterID != admin || sent[0].AccountName != "Team" {
		t.Fatalf("invitations sent: %+v", sent)
	}

	t.Run("an invitee can't use the account before accepting", func(t *testing.T) {
		if _, err := h.svc.GetTenant(ctx, bob, tenant); !errors.Is(err, app.ErrNotFound) {
			t.Fatalf("got %v, want ErrNotFound", err)
		}
	})
	t.Run("inviting again re-sends with the new role", func(t *testing.T) {
		m, err := h.svc.Invite(ctx, admin, session, tenant, "bob@example.com", domain.BuiltinViewerRoleID)
		if err != nil || m.RoleID != domain.BuiltinViewerRoleID || m.Status != "invited" {
			t.Fatalf("got %+v, %v", m, err)
		}
		if len(h.identity.Invitations()) != 2 {
			t.Fatal("the invitation wasn't sent again")
		}
	})
	t.Run("a new address is invited the same way", func(t *testing.T) {
		m, err := h.svc.Invite(ctx, admin, session, tenant, "carol@example.com", domain.BuiltinViewerRoleID)
		if err != nil || m.Status != "invited" {
			t.Fatalf("got %+v, %v", m, err)
		}
	})

	refusals := []struct {
		name    string
		actor   uuid.UUID
		session uuid.UUID
		email   string
		role    uuid.UUID
		want    error
	}{
		{"a Developer can't invite", dev, h.steppedUpSession(dev), "x@example.com", domain.BuiltinViewerRoleID, domain.ErrPermissionDenied},
		{"an Admin without a fresh step-up", admin, uuid.New(), "x@example.com", domain.BuiltinViewerRoleID, app.ErrStepUpRequired},
		{"a stranger", uuid.New(), uuid.New(), "x@example.com", domain.BuiltinViewerRoleID, app.ErrNotFound},
		{"an active member", admin, session, "", domain.BuiltinViewerRoleID, app.ErrAlreadyMember},
		{"a role that isn't built in", admin, session, "x@example.com", uuid.New(), domain.ErrNotBuiltinRole},
		{"a malformed address", admin, session, "not-an-email", domain.BuiltinViewerRoleID, app.ErrValidation},
	}
	for _, tc := range refusals {
		t.Run(tc.name, func(t *testing.T) {
			email := tc.email
			if email == "" {
				var devEmail = "dev@example.com"
				devID := h.identity.AddUser(devEmail, "Dev")
				h.addMemberWithRole(t, tenant, devID, domain.BuiltinViewerRoleID)
				email = devEmail
			}
			before := h.count(t, `SELECT count(*) FROM outbox_events WHERE event_type = 'membership.invited'`)
			if _, err := h.svc.Invite(ctx, tc.actor, tc.session, tenant, email, tc.role); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			if after := h.count(t, `SELECT count(*) FROM outbox_events WHERE event_type = 'membership.invited'`); after != before {
				t.Fatal("a refused invitation wrote an event")
			}
		})
	}
}

func TestAcceptInvitation(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tenant, admin, _, viewer := h.newTeam(t)
	session := h.steppedUpSession(admin)
	bob := h.identity.AddUser("bob@example.com", "Bob")
	if _, err := h.svc.Invite(ctx, admin, session, tenant, "bob@example.com", domain.BuiltinAdminRoleID); err != nil {
		t.Fatal(err)
	}

	t.Run("an invited Admin doesn't keep the account until they accept", func(t *testing.T) {
		if _, err := h.svc.ChangeMemberRole(ctx, admin, tenant, admin, domain.BuiltinViewerRoleID); !errors.Is(err, domain.ErrLastAdmin) {
			t.Fatalf("got %v, want ErrLastAdmin", err)
		}
	})
	t.Run("only an invitation can be accepted", func(t *testing.T) {
		if _, err := h.svc.AcceptInvitation(ctx, viewer, tenant); !errors.Is(err, app.ErrNotFound) {
			t.Fatalf("an active member: got %v, want ErrNotFound", err)
		}
		if _, err := h.svc.AcceptInvitation(ctx, uuid.New(), tenant); !errors.Is(err, app.ErrNotFound) {
			t.Fatalf("a stranger: got %v, want ErrNotFound", err)
		}
	})
	t.Run("the invitation is in my memberships before accepting", func(t *testing.T) {
		mine, err := h.svc.MyMemberships(ctx, bob)
		if err != nil || len(mine) != 1 || mine[0].Status != "invited" {
			t.Fatalf("got %+v, %v", mine, err)
		}
	})

	m, err := h.svc.AcceptInvitation(ctx, bob, tenant)
	if err != nil || m.Status != "active" || m.RoleID != domain.BuiltinAdminRoleID {
		t.Fatalf("AcceptInvitation: %+v, %v", m, err)
	}
	data := assertEventValid(t, h, "membership.joined", tenant)
	if data["user_id"] != bob.String() {
		t.Fatalf("got event data %v", data)
	}
	if _, err := h.svc.GetTenant(ctx, bob, tenant); err != nil {
		t.Fatalf("after accepting: %v", err)
	}
	if _, err := h.svc.AcceptInvitation(ctx, bob, tenant); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("accepting twice: got %v, want ErrNotFound", err)
	}
	t.Run("with a second active Admin the first may step down", func(t *testing.T) {
		if _, err := h.svc.ChangeMemberRole(ctx, admin, tenant, admin, domain.BuiltinViewerRoleID); err != nil {
			t.Fatal(err)
		}
	})
}

func TestDeclineInvitation_IsLeaving(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tenant, admin, _, _ := h.newTeam(t)
	bob := h.identity.AddUser("bob@example.com", "Bob")
	if _, err := h.svc.Invite(ctx, admin, h.steppedUpSession(admin), tenant, "bob@example.com", domain.BuiltinViewerRoleID); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.RemoveMember(ctx, bob, tenant, bob); err != nil {
		t.Fatalf("declining: %v", err)
	}
	if _, err := h.svc.AcceptInvitation(ctx, bob, tenant); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("accepting a declined invitation: got %v, want ErrNotFound", err)
	}
	if _, err := h.svc.Invite(ctx, admin, h.steppedUpSession(admin), tenant, "bob@example.com", domain.BuiltinViewerRoleID); err != nil {
		t.Fatalf("inviting again after a decline: %v", err)
	}
	if err := h.svc.RemoveMember(ctx, bob, uuid.New(), bob); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("leaving an account that doesn't exist: got %v, want ErrNotFound", err)
	}
}

func TestListMembers_WithNamesAndEmails(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	admin := h.identity.AddUser("ada@example.com", "Ada Lovelace")
	created, err := h.svc.CreateTenant(ctx, admin, domain.NewTenantInput{DisplayName: "Team", CountryCode: "KE"})
	if err != nil {
		t.Fatal(err)
	}
	members, err := h.svc.ListMembers(ctx, admin, created.ID)
	if err != nil || len(members) != 1 || members[0].Email != "ada@example.com" || members[0].FullName != "Ada Lovelace" {
		t.Fatalf("got %+v, %v", members, err)
	}
	h.identity.Unreachable = true
	members, err = h.svc.ListMembers(ctx, admin, created.ID)
	if err != nil || len(members) != 1 || members[0].Email != "" {
		t.Fatalf("with identity down: got %+v, %v; want the list without emails", members, err)
	}
}
