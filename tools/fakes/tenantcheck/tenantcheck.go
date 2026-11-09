// Package tenantcheck fakes accounts' CheckTenantAccess for the
// ledger's tests (ledger's adapters.Accounts, task 3.9). identity's
// tenantaccess fake implements identity's adapter, which the ledger
// can't import. Nobody has access until a test grants it; Fail makes
// every check error, as accounts being down would.
package tenantcheck

import (
	"context"
	"errors"
	"slices"
	"sync"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/adapters"
)

// ErrDown is what every check returns after Fail.
var ErrDown = errors.New("tenantcheck: accounts unavailable")

// Fake answers from what a test granted.
type Fake struct {
	mu      sync.Mutex
	granted map[[2]uuid.UUID][]string
	down    bool
}

// New returns a Fake granting nothing.
func New() *Fake { return &Fake{granted: map[[2]uuid.UUID][]string{}} }

// Grant lets user work in tenant with exactly perms (none: membership
// only), as their role would.
func (f *Fake) Grant(user, tenant uuid.UUID, perms ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.granted[[2]uuid.UUID{user, tenant}] = append([]string{}, perms...)
}

// Revoke removes user from tenant.
func (f *Fake) Revoke(user, tenant uuid.UUID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.granted, [2]uuid.UUID{user, tenant})
}

// Fail makes every check error.
func (f *Fake) Fail() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down = true
}

// CheckTenantAccess implements adapters.Accounts.
func (f *Fake) CheckTenantAccess(_ context.Context, user, tenant uuid.UUID, permission string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return ErrDown
	}
	perms, ok := f.granted[[2]uuid.UUID{user, tenant}]
	if !ok {
		return adapters.ErrNoTenantAccess
	}
	if permission != "" && !slices.Contains(perms, permission) {
		return adapters.ErrPermissionDenied
	}
	return nil
}
