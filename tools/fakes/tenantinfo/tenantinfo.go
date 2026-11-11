// Package tenantinfo fakes accounts' CheckTenantAccess for payments'
// tests (payments' adapters.Accounts, task 4.3), with the country and
// currency accounts now returns. The ledger's tenantcheck implements
// the ledger's adapter, which payments can't import. Nobody has access
// until a test grants it; Fail makes every check error, as accounts
// being down would.
package tenantinfo

import (
	"context"
	"errors"
	"slices"
	"sync"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments/adapters"
)

// ErrDown is what every check returns after Fail.
var ErrDown = errors.New("tenantinfo: accounts unavailable")

type grant struct {
	info  adapters.TenantInfo
	perms []string
}

// Fake answers from what a test granted.
type Fake struct {
	mu      sync.Mutex
	granted map[[2]uuid.UUID]grant
	down    bool
}

// New returns a Fake granting nothing.
func New() *Fake { return &Fake{granted: map[[2]uuid.UUID]grant{}} }

// Grant lets user work in tenant, an account in country with currency,
// with exactly perms (none: membership only).
func (f *Fake) Grant(user, tenant uuid.UUID, country, currency string, perms ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.granted[[2]uuid.UUID{user, tenant}] = grant{info: adapters.TenantInfo{CountryCode: country, Currency: currency}, perms: slices.Clone(perms)}
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

// TenantAccess implements adapters.Accounts.
func (f *Fake) TenantAccess(_ context.Context, user, tenant uuid.UUID, permission string) (adapters.TenantInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return adapters.TenantInfo{}, ErrDown
	}
	g, ok := f.granted[[2]uuid.UUID{user, tenant}]
	if !ok {
		return adapters.TenantInfo{}, adapters.ErrNoTenantAccess
	}
	if permission != "" && !slices.Contains(g.perms, permission) {
		return adapters.TenantInfo{}, adapters.ErrPermissionDenied
	}
	return g.info, nil
}
