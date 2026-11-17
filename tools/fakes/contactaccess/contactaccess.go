// Package contactaccess fakes accounts' CheckTenantAccess for contacts'
// tests (contacts' adapters.Accounts, task 6.2), which can't import
// another service's adapters and their errors. Nobody has access
// until a test grants it; Fail makes every check error, as accounts
// being down would.
package contactaccess

import (
	"context"
	"errors"
	"slices"
	"sync"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/contacts/adapters"
)

// ErrDown is what every check returns after Fail.
var ErrDown = errors.New("contactaccess: accounts unavailable")

// Fake answers from what a test granted. Every account is in Kenya
// unless SetCountry says otherwise.
type Fake struct {
	mu        sync.Mutex
	granted   map[[2]uuid.UUID][]string
	countries map[uuid.UUID]string
	down      bool
}

// New returns a Fake granting nothing.
func New() *Fake {
	return &Fake{granted: map[[2]uuid.UUID][]string{}, countries: map[uuid.UUID]string{}}
}

// SetCountry puts tenant in country (ISO 3166-1 alpha-2).
func (f *Fake) SetCountry(tenant uuid.UUID, country string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.countries[tenant] = country
}

// Grant lets user work in tenant with exactly perms (none: membership
// only).
func (f *Fake) Grant(user, tenant uuid.UUID, perms ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.granted[[2]uuid.UUID{user, tenant}] = slices.Clone(perms)
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
	perms, ok := f.granted[[2]uuid.UUID{user, tenant}]
	if !ok {
		return adapters.TenantInfo{}, adapters.ErrNoTenantAccess
	}
	if permission != "" && !slices.Contains(perms, permission) {
		return adapters.TenantInfo{}, adapters.ErrPermissionDenied
	}
	country := f.countries[tenant]
	if country == "" {
		country = "KE"
	}
	return adapters.TenantInfo{CountryCode: country}, nil
}
