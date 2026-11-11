// Package app holds payments' use cases: validate through domain/, then
// write through store/ and platform/outbox inside one database
// transaction.
package app

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/clock"
	vascrypto "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/crypto"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/packs"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments/adapters"
)

// Roles a transaction takes for rows with no tenant, or across tenants
// (migration 20261111090000).
const systemRole = "payments_system"

// CredentialsKMSKeyID is the KMS key rail credentials are sealed under.
const CredentialsKMSKeyID = "payments-rail-credentials" //nolint:gosec // G101: the KMS key's name, not a key

// Billing permissions in accounts' catalogue (task 2.3a).
const (
	PermBillingRead   = "billing:read"
	PermBillingManage = "billing:manage"
)

var (
	// ErrValidation wraps every rejected input (VAL-4000).
	ErrValidation = errors.New("app: validation failed")
	// ErrNotFound: no such account, or no access to it (VAL-4003).
	ErrNotFound = errors.New("app: not found")
	// ErrPermissionDenied: a dashboard user whose role lacks the
	// permission (AUTH-4024).
	ErrPermissionDenied = errors.New("app: your role doesn't allow this")
	// ErrNotStaff: a staff action by someone who isn't staff (AUTH-4025).
	ErrNotStaff = errors.New("app: staff only")
	// ErrTopupUnavailable: no verified VASPRO paybill takes top-ups in
	// the account's country (PAY-6507).
	ErrTopupUnavailable = errors.New("app: top-ups aren't available for this account's country")
	// ErrVerificationFailed: the rail refused a rail account's
	// credentials, or couldn't be reached, so it stays pending.
	ErrVerificationFailed = errors.New("app: the rail didn't accept the account's credentials")
)

// RailChecker proves a rail account's stored credentials with a test
// call (adapters.Daraja.CheckCredentials).
type RailChecker interface {
	CheckCredentials(ctx context.Context, railAccountID uuid.UUID) error
}

// Deps are the Service's dependencies.
type Deps struct {
	Pool   *db.Pool
	Clock  clock.Clock
	Logger *slog.Logger
	// Accounts answers dashboard access checks; nil fails them closed.
	Accounts adapters.Accounts
	// Identity finds staff by email for the ops CLI; nil fails closed.
	Identity adapters.Identity
	// Packs are the country packs, by country code.
	Packs map[string]*packs.Pack
	// KMS seals rail credentials (CredentialsKMSKeyID); FingerprintKey
	// keys their fingerprints. Only the ops CLI writes credentials.
	KMS            vascrypto.KMS
	FingerprintKey []byte
	// Rail proves credentials when an account is verified.
	Rail RailChecker
	// Rand feeds account numbers; crypto/rand when nil.
	Rand io.Reader
}

// Service holds payments' use cases.
type Service struct {
	pool     *db.Pool
	clk      clock.Clock
	log      *slog.Logger
	accounts adapters.Accounts
	identity adapters.Identity
	packs    map[string]*packs.Pack
	kms      vascrypto.KMS
	fpKey    []byte
	rail     RailChecker
	rand     io.Reader
}

// New builds a Service.
func New(d Deps) *Service {
	if d.Clock == nil {
		d.Clock = clock.Real{}
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Rand == nil {
		d.Rand = rand.Reader
	}
	return &Service{
		pool: d.Pool, clk: d.Clock, log: d.Logger, accounts: d.Accounts, identity: d.Identity,
		packs: d.Packs, kms: d.KMS, fpKey: d.FingerprintKey, rail: d.Rail, rand: d.Rand,
	}
}
