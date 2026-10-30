// Package app holds accounts' use cases: validate through domain/,
// then write through store/ and platform/outbox inside one
// transaction.
package app

import (
	"errors"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/clock"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/packs"
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
}

// Deps is everything New needs. Packs is every loaded country pack,
// keyed by country code: a new account takes its currency, time zone
// and data region from its country's pack.
type Deps struct {
	Pool  *db.Pool
	Clock clock.Clock
	Packs map[string]*packs.Pack
}

// New builds a Service.
func New(d Deps) *Service {
	return &Service{pool: d.Pool, clk: d.Clock, packs: d.Packs}
}
