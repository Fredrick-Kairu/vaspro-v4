// Package staffcheck is a fake of the identity questions a staff action
// asks (ledger's adapters.Identity): who is staff, who is a finance
// lead, and whose step-up is fresh. Nothing is staff or stepped up until a test says so; Fail makes
// every question error, as identity being down would.
package staffcheck

import (
	"context"
	"errors"
	"sync"

	"github.com/google/uuid"
)

// ErrDown is what every question returns after Fail.
var ErrDown = errors.New("staffcheck: identity unavailable")

// Fake answers IsStaff and IsStepUpFresh from what a test set.
type Fake struct {
	mu        sync.Mutex
	staff     map[uuid.UUID]bool
	leads     map[uuid.UUID]bool
	steppedUp map[[2]uuid.UUID]bool
	down      bool
}

// New returns a Fake with no staff.
func New() *Fake {
	return &Fake{staff: map[uuid.UUID]bool{}, leads: map[uuid.UUID]bool{}, steppedUp: map[[2]uuid.UUID]bool{}}
}

// SetStaff grants or revokes staff.
func (f *Fake) SetStaff(user uuid.UUID, staff bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.staff[user] = staff
}

// SetFinanceLead sets the finance-lead flag; it counts only for staff,
// as in identity.
func (f *Fake) SetFinanceLead(user uuid.UUID, lead bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.leads[user] = lead
}

// StepUp marks the user's session as freshly stepped up; Expire undoes it.
func (f *Fake) StepUp(user, session uuid.UUID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.steppedUp[[2]uuid.UUID{user, session}] = true
}

// Expire makes the session's step-up stale.
func (f *Fake) Expire(user, session uuid.UUID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.steppedUp, [2]uuid.UUID{user, session})
}

// Fail makes every question error.
func (f *Fake) Fail() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down = true
}

func (f *Fake) IsStaff(_ context.Context, userID uuid.UUID) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return false, ErrDown
	}
	return f.staff[userID], nil
}

func (f *Fake) IsFinanceLead(_ context.Context, userID uuid.UUID) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return false, ErrDown
	}
	return f.staff[userID] && f.leads[userID], nil
}

func (f *Fake) IsStepUpFresh(_ context.Context, userID, sessionID uuid.UUID) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return false, ErrDown
	}
	return f.steppedUp[[2]uuid.UUID{userID, sessionID}], nil
}
