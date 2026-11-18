// Package app holds webhooks' use cases: validate through domain/, then
// write through store/ inside one database transaction.
package app

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/clock"
	vascrypto "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/crypto"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/packs"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/webhooks/adapters"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/webhooks/domain"
)

// systemRole is the delivery worker's role across clients (migration
// 20261112090000).
const systemRole = "webhooks_system"

// SecretsKMSKeyID is the KMS key endpoint secrets are sealed under.
const SecretsKMSKeyID = "webhooks-endpoint-secrets" //nolint:gosec // G101: the KMS key's name, not a key

// Permissions in accounts' catalogue (task 2.3a). Endpoints are
// credentials: data model §8.
const (
	PermCredentialsRead   = "credentials:read"
	PermCredentialsManage = "credentials:manage"
	PermBillingRead       = "billing:read"
)

// DefaultMaxEndpoints is how many undeleted endpoints a client may have
// (data model §8).
const DefaultMaxEndpoints = 16

var (
	// ErrValidation wraps every rejected input (VAL-4000).
	ErrValidation = errors.New("app: validation failed")
	// ErrNotFound: no such account or endpoint, or no access to it
	// (VAL-4003).
	ErrNotFound = errors.New("app: not found")
	// ErrPermissionDenied: the user's role lacks the permission
	// (AUTH-4024).
	ErrPermissionDenied = errors.New("app: your role doesn't allow this")
	// ErrStepUpRequired: the session's step-up isn't fresh (AUTH-4012).
	ErrStepUpRequired = errors.New("app: step-up verification required")
	// ErrURLNotAllowed: not a URL VASPRO calls (VAL-4019).
	ErrURLNotAllowed = domain.ErrURLNotAllowed
	// ErrVerificationFailed: the endpoint didn't echo the challenge
	// (VAL-4018). The error is a *VerificationFailure.
	ErrVerificationFailed = errors.New("app: the endpoint didn't answer the verification challenge")
)

// VerificationFailure says how a challenge failed, without the URL, the
// secret or the challenge.
type VerificationFailure struct {
	Outcome adapters.Outcome // "" when the answer came but was wrong
	Status  int              // the answer's status, if there was one
	Reason  string
}

func (f *VerificationFailure) Error() string {
	return fmt.Sprintf("%v: %s", ErrVerificationFailed, f.Reason)
}

func (f *VerificationFailure) Unwrap() error { return ErrVerificationFailed }

// Sender POSTs to endpoints (adapters.Sender).
type Sender interface {
	Send(ctx context.Context, req adapters.Request) adapters.Response
}

// Deps are the Service's dependencies.
type Deps struct {
	Pool   *db.Pool
	Clock  clock.Clock
	Logger *slog.Logger
	// Accounts answers access checks; nil fails them closed.
	Accounts adapters.Accounts
	// Identity answers step-up checks; nil fails them closed.
	Identity adapters.Identity
	// KMS seals endpoint secrets under SecretsKMSKeyID; FingerprintKey
	// keys their fingerprints.
	KMS            vascrypto.KMS
	FingerprintKey []byte
	// Sender calls endpoints.
	Sender Sender
	// AllowInsecureURLs allows plain http and private addresses, for
	// development only. It must match the Sender's setting.
	AllowInsecureURLs bool
	// MaxEndpoints caps a client's undeleted endpoints;
	// DefaultMaxEndpoints when zero.
	MaxEndpoints int
	// Rand feeds secrets and challenges; crypto/rand when nil.
	Rand io.Reader

	// Delivery (task 5.4). Schedule is the wait before each retry
	// (domain.DefaultSchedule when nil); Jitter varies each wait by up to
	// that fraction of itself, drawn from RandFloat (math/rand when nil).
	Schedule  []time.Duration
	Jitter    float64
	RandFloat func() float64
	// DisableAfter is how long an endpoint fails before it is disabled
	// (domain.DefaultDisableAfter when zero).
	DisableAfter time.Duration
	// Workers bounds the requests in flight per DeliverDue, PerEndpoint
	// those to any one endpoint, and BatchSize the deliveries claimed at
	// once. Lease is how long a claim holds before another worker may
	// take the delivery; HoldFor how long a delivery to an endpoint
	// waiting for verification is put off.
	Workers     int
	PerEndpoint int
	BatchSize   int
	Lease       time.Duration
	HoldFor     time.Duration

	// Reports (task 5.6). Events is the events table in ClickHouse; Packs
	// are the country packs by country code, whose time zone a client's
	// report days are in.
	Events EventStore
	Packs  map[string]*packs.Pack
}

// EventStore is the events table and its reports (adapters.ClickHouse).
type EventStore interface {
	InsertEvents(ctx context.Context, rows []adapters.EventRow) error
	DailyActivity(ctx context.Context, tenant uuid.UUID, from, to time.Time, zone string) ([]adapters.DailyActivity, error)
	TopupsByRail(ctx context.Context, from, to time.Time, zone string) ([]adapters.TopupsByRail, error)
	EventsPerType(ctx context.Context, from, to time.Time) ([]adapters.EventsPerType, error)
}

// Delivery defaults.
const (
	DefaultWorkers     = 16
	DefaultPerEndpoint = 4
	DefaultBatchSize   = 100
	DefaultLease       = 5 * time.Minute
	DefaultHoldFor     = time.Minute
)

// Service holds webhooks' use cases.
type Service struct {
	pool     *db.Pool
	clk      clock.Clock
	log      *slog.Logger
	accounts adapters.Accounts
	identity adapters.Identity
	envelope *vascrypto.Envelope
	fpKey    []byte
	sender   Sender
	insecure bool
	maxEnd   int
	rand     io.Reader

	schedule    []time.Duration
	jitter      float64
	randFloat   func() float64
	disableAt   time.Duration
	workers     int
	perEndpoint int
	batch       int
	lease       time.Duration
	holdFor     time.Duration

	events EventStore
	packs  map[string]*packs.Pack
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
	if d.MaxEndpoints <= 0 {
		d.MaxEndpoints = DefaultMaxEndpoints
	}
	if d.Schedule == nil {
		d.Schedule = domain.DefaultSchedule
	}
	if d.DisableAfter <= 0 {
		d.DisableAfter = domain.DefaultDisableAfter
	}
	d.Workers = orDefault(d.Workers, DefaultWorkers)
	d.PerEndpoint = orDefault(d.PerEndpoint, DefaultPerEndpoint)
	d.BatchSize = orDefault(d.BatchSize, DefaultBatchSize)
	if d.Lease <= 0 {
		d.Lease = DefaultLease
	}
	if d.HoldFor <= 0 {
		d.HoldFor = DefaultHoldFor
	}
	s := &Service{
		pool: d.Pool, clk: d.Clock, log: d.Logger, accounts: d.Accounts, identity: d.Identity,
		fpKey: d.FingerprintKey, sender: d.Sender, insecure: d.AllowInsecureURLs, maxEnd: d.MaxEndpoints, rand: d.Rand,
		schedule: d.Schedule, jitter: d.Jitter, randFloat: d.RandFloat, disableAt: d.DisableAfter,
		workers: d.Workers, perEndpoint: d.PerEndpoint, batch: d.BatchSize, lease: d.Lease, holdFor: d.HoldFor,
		events: d.Events, packs: d.Packs,
	}
	if d.KMS != nil {
		s.envelope = vascrypto.NewEnvelope(d.KMS)
	}
	return s
}

func orDefault(n, def int) int {
	if n <= 0 {
		return def
	}
	return n
}

// requireTenant asks accounts, on every call, whether user may act in
// tenant with perm. Accounts being down fails closed.
func (s *Service) requireTenant(ctx context.Context, user, tenant uuid.UUID, perm string) error {
	_, err := s.tenantInfo(ctx, user, tenant, perm)
	return err
}

// tenantInfo is requireTenant that also returns what accounts says about
// the account.
func (s *Service) tenantInfo(ctx context.Context, user, tenant uuid.UUID, perm string) (adapters.TenantInfo, error) {
	if s.accounts == nil {
		return adapters.TenantInfo{}, errors.New("app: no accounts adapter for access checks")
	}
	info, err := s.accounts.TenantAccess(ctx, user, tenant, perm)
	switch {
	case errors.Is(err, adapters.ErrNoTenantAccess):
		return adapters.TenantInfo{}, fmt.Errorf("%w: no account %s", ErrNotFound, tenant)
	case errors.Is(err, adapters.ErrPermissionDenied):
		return adapters.TenantInfo{}, fmt.Errorf("%w: %s", ErrPermissionDenied, perm)
	case err != nil:
		return adapters.TenantInfo{}, fmt.Errorf("app: checking account access: %w", err)
	}
	return info, nil
}

// requireStepUp asks identity whether the session's step-up is fresh.
// Identity being down fails closed.
func (s *Service) requireStepUp(ctx context.Context, user, session uuid.UUID) error {
	if s.identity == nil {
		return errors.New("app: no identity adapter for step-up checks")
	}
	fresh, err := s.identity.IsStepUpFresh(ctx, user, session)
	if err != nil {
		return fmt.Errorf("app: checking step-up: %w", err)
	}
	if !fresh {
		return ErrStepUpRequired
	}
	return nil
}
