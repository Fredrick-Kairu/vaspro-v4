// Package app holds accounts' use cases: validate through domain/,
// then write through store/ and platform/outbox inside one
// transaction.
package app

import (
	"errors"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/clock"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/packs"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/accounts/adapters"
)

// ErrNotFound covers a tenant that doesn't exist and one the caller's
// tenant can't see alike.
var ErrNotFound = errors.New("app: not found")

// ErrValidation wraps every rejected input; api/ maps it to VAL-4000.
var ErrValidation = errors.New("app: validation failed")

// Service holds accounts' use cases.
type Service struct {
	pool  *db.Pool
	clk   clock.Clock
	packs map[string]*packs.Pack
	// identity finds invitees, sends invitations, checks step-up and
	// names members (task 2.3b).
	identity adapters.Identity
	// kyc is the KYC vendor's adapter (task 2.6, ADR 0008); nil when
	// KYC isn't configured.
	kyc adapters.KYCProvider
}

// Deps is everything New needs. Packs is every loaded country pack,
// keyed by country code: a new account takes its currency, time zone
// and data region from its country's pack.
type Deps struct {
	Pool     *db.Pool
	Clock    clock.Clock
	Packs    map[string]*packs.Pack
	Identity adapters.Identity
	KYC      adapters.KYCProvider
}

// New builds a Service.
func New(d Deps) *Service {
	return &Service{pool: d.Pool, clk: d.Clock, packs: d.Packs, identity: d.Identity, kyc: d.KYC}
}
