// Package staffdirectory fakes the staff questions payments asks
// identity (payments' adapters.Identity): staff by email (task 4.3),
// whether a user is staff, and whether a session's step-up is fresh
// (task 4.5). Nobody is staff or stepped up until a test says so; Fail
// makes every question error, as identity being down would.
package staffdirectory

import (
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments/adapters"
)

// ErrDown is what every lookup returns after Fail.
var ErrDown = errors.New("staffdirectory: identity unavailable")

// Fake answers from the staff a test added.
type Fake struct {
	mu        sync.Mutex
	staff     map[string]uuid.UUID
	steppedUp map[[2]uuid.UUID]bool
	down      bool
}

// New returns a Fake with no staff.
func New() *Fake { return &Fake{staff: map[string]uuid.UUID{}, steppedUp: map[[2]uuid.UUID]bool{}} }

// StepUp marks user's session as freshly stepped up; Expire undoes it.
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

// IsStaff implements adapters.Identity.
func (f *Fake) IsStaff(_ context.Context, user uuid.UUID) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return false, ErrDown
	}
	for _, id := range f.staff {
		if id == user {
			return true, nil
		}
	}
	return false, nil
}

// IsStepUpFresh implements adapters.Identity.
func (f *Fake) IsStepUpFresh(_ context.Context, user, session uuid.UUID) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return false, ErrDown
	}
	return f.steppedUp[[2]uuid.UUID{user, session}], nil
}

// Add makes email a staff member and returns their user ID.
func (f *Fake) Add(email string) uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := uuid.New()
	f.staff[strings.ToLower(strings.TrimSpace(email))] = id
	return id
}

// Remove stops email being staff.
func (f *Fake) Remove(email string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.staff, strings.ToLower(strings.TrimSpace(email)))
}

// Fail makes every lookup error.
func (f *Fake) Fail() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down = true
}

// StaffByEmail implements adapters.Identity.
func (f *Fake) StaffByEmail(_ context.Context, email string) (uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return uuid.Nil, ErrDown
	}
	id, ok := f.staff[strings.ToLower(strings.TrimSpace(email))]
	if !ok {
		return uuid.Nil, adapters.ErrNotStaff
	}
	return id, nil
}
