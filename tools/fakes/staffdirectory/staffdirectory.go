// Package staffdirectory fakes identity's GetStaffByEmail for payments'
// tests (payments' adapters.Identity, task 4.3). Nobody is staff until a
// test adds them; Fail makes every lookup error, as identity being down
// would.
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
	mu    sync.Mutex
	staff map[string]uuid.UUID
	down  bool
}

// New returns a Fake with no staff.
func New() *Fake { return &Fake{staff: map[string]uuid.UUID{}} }

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
