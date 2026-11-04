// Package tenantaccess fakes accounts' CheckTenantAccess for identity's
// tests (adapters.TenantAccess, tasks 2.2c and 2.3a).
package tenantaccess

import (
	"context"
	"slices"
	"sync"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/adapters"
)

// Fake grants the (user, account) pairs it was given, with every
// permission (Grant) or only some (GrantOnly); AllowAll grants every
// pair every permission. Err, when set, is returned for every check,
// standing in for accounts being unreachable.
type Fake struct {
	AllowAll bool

	mu      sync.Mutex
	granted map[[2]uuid.UUID][]string // nil slice: every permission
	err     error
}

// Grant lets user work in tenant with every permission; uuid.Nil as
// user means anyone, for a test that can't learn a user's ID
// (identity's HTTP API never returns it).
func (f *Fake) Grant(user, tenant uuid.UUID) { f.set(user, tenant, nil) }

// GrantOnly lets user work in tenant with just perms, as a role without
// the others would.
func (f *Fake) GrantOnly(user, tenant uuid.UUID, perms ...string) {
	f.set(user, tenant, append([]string{}, perms...))
}

func (f *Fake) set(user, tenant uuid.UUID, perms []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.granted == nil {
		f.granted = map[[2]uuid.UUID][]string{}
	}
	f.granted[[2]uuid.UUID{user, tenant}] = perms
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
func (f *Fake) CheckTenantAccess(_ context.Context, user, tenant uuid.UUID, permission string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	if f.AllowAll {
		return nil
	}
	perms, ok := f.granted[[2]uuid.UUID{user, tenant}]
	if !ok {
		perms, ok = f.granted[[2]uuid.UUID{uuid.Nil, tenant}]
	}
	if !ok {
		return adapters.ErrNoTenantAccess
	}
	if permission != "" && perms != nil && !slices.Contains(perms, permission) {
		return adapters.ErrPermissionDenied
	}
	return nil
}
