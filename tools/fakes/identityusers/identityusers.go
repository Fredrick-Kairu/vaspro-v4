// Package identityusers fakes identity for accounts' tests
// (adapters.Identity, task 2.3b): users by email, invitations sent,
// step-ups and member names.
package identityusers

import (
	"context"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/accounts/adapters"
)

// Invitation is one SendInvitation call.
type Invitation struct {
	InviteeID, InviterID uuid.UUID
	AccountName          string
}

// Fake keeps users by email, creating one for a new address as
// identity does; addresses without "@" are refused. Unreachable, when
// set, makes GetUsers fail, standing in for identity being down.
type Fake struct {
	mu          sync.Mutex
	byEmail     map[string]uuid.UUID
	users       map[uuid.UUID]adapters.User
	fresh       map[[2]uuid.UUID]bool // {user, session}
	invitations []Invitation
	Unreachable bool
}

// AddUser registers an existing user and returns their ID.
func (f *Fake) AddUser(email, fullName string) uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addLocked(email, fullName)
}

func (f *Fake) addLocked(email, fullName string) uuid.UUID {
	if f.byEmail == nil {
		f.byEmail, f.users = map[string]uuid.UUID{}, map[uuid.UUID]adapters.User{}
	}
	id := uuid.New()
	f.byEmail[strings.ToLower(email)] = id
	f.users[id] = adapters.User{ID: id, Email: strings.ToLower(email), FullName: fullName}
	return id
}

// StepUp marks session as freshly stepped up for user.
func (f *Fake) StepUp(user, session uuid.UUID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fresh == nil {
		f.fresh = map[[2]uuid.UUID]bool{}
	}
	f.fresh[[2]uuid.UUID{user, session}] = true
}

// Invitations returns every SendInvitation call so far.
func (f *Fake) Invitations() []Invitation {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Invitation{}, f.invitations...)
}

func (f *Fake) EnsureInvitee(_ context.Context, email string) (uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	email = strings.ToLower(strings.TrimSpace(email))
	if !strings.Contains(email, "@") {
		return uuid.Nil, adapters.ErrInvalidEmail
	}
	if id, ok := f.byEmail[email]; ok {
		return id, nil
	}
	return f.addLocked(email, ""), nil
}

func (f *Fake) SendInvitation(_ context.Context, inviteeID, inviterID uuid.UUID, accountName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.invitations = append(f.invitations, Invitation{InviteeID: inviteeID, InviterID: inviterID, AccountName: accountName})
	return nil
}

func (f *Fake) IsStepUpFresh(_ context.Context, userID, sessionID uuid.UUID) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fresh[[2]uuid.UUID{userID, sessionID}], nil
}

func (f *Fake) GetUsers(_ context.Context, userIDs []uuid.UUID) ([]adapters.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Unreachable {
		return nil, context.DeadlineExceeded
	}
	var out []adapters.User
	for _, id := range userIDs {
		if u, ok := f.users[id]; ok {
			out = append(out, u)
		}
	}
	return out, nil
}
