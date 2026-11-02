// Package tenantaccess fakes accounts' CheckTenantAccess for identity's
// tests (adapters.TenantAccess, task 2.2c).
package tenantaccess

import (
	"context"
	"sync"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/adapters"
)

// Fake grants exactly the (user, account) pairs it was given, or every
// pair when AllowAll is set. Err, when set, is returned for every
// check, standing in for accounts being unreachable.
type Fake struct {
	AllowAll bool

	mu      sync.Mutex
	granted map[[2]uuid.UUID]bool
	err     error
}

// Grant lets user work in tenant; uuid.Nil as user means anyone, for a
// test that can't learn a user's ID (identity's HTTP API never returns
// it).
func (f *Fake) Grant(user, tenant uuid.UUID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.granted == nil {
		f.granted = map[[2]uuid.UUID]bool{}
	}
	f.granted[[2]uuid.UUID{user, tenant}] = true
}

// Revoke removes user from tenant, as removing a member would.
func (f *Fake) Revoke(user, tenant uuid.UUID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.granted, [2]uuid.UUID{user, tenant})
}

// FailWith makes every check return err (nil to stop).
func (f *Fake) FailWith(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// CheckTenantAccess implements adapters.TenantAccess.
func (f *Fake) CheckTenantAccess(_ context.Context, user, tenant uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	if f.AllowAll || f.granted[[2]uuid.UUID{user, tenant}] || f.granted[[2]uuid.UUID{uuid.Nil, tenant}] {
		return nil
	}
	return adapters.ErrNoTenantAccess
}
