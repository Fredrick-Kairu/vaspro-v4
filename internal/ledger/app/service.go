// Package app holds the ledger's use cases (task 3.3): validate
// through domain/, then write through store/ and platform/outbox
// inside one database transaction. The database guards (3.2) back
// every rule checked here.
package app

import (
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/clock"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/domain"
)

// systemRole writes VASPRO's own rows (migration 20261106090000).
const systemRole = "ledger_system"

var (
	// ErrValidation wraps every rejected input (VAL-4000).
	ErrValidation = errors.New("app: validation failed")
	// ErrNotFound: no such hold for this tenant (VAL-4003).
	ErrNotFound = errors.New("app: not found")
	// ErrIdempotencyConflict: an idempotency key or hold reference
	// reused with different content (VAL-4001).
	ErrIdempotencyConflict = errors.New("app: key reused with a different request")
	// ErrInsufficientBalance: a wallet's available balance doesn't cover
	// the request (BAL-4001).
	ErrInsufficientBalance = errors.New("app: insufficient balance")
	// ErrAccountNotOpen: a frozen or closed account (BAL-4002).
	ErrAccountNotOpen = errors.New("app: account not open")
)

// domain.ErrHoldState (BAL-4003) and domain.ErrAllocationFinal
// (BAL-4004) are returned as they are.

// Service holds the ledger's use cases.
type Service struct {
	pool *db.Pool
	clk  clock.Clock
	log  *slog.Logger
}

// Deps is everything New needs. Pool must connect as a role granted
// ledger_service (and through it ledger_system), never the tables'
// owner (migration 20261106090000).
type Deps struct {
	Pool   *db.Pool
	Clock  clock.Clock
	Logger *slog.Logger
}

// New builds a Service.
func New(d Deps) *Service {
	log := d.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Service{pool: d.Pool, clk: d.Clock, log: log}
}

// PostRequest asks for one balanced transaction. TenantID nil is a
// system posting, which may use VASPRO's accounts only.
type PostRequest struct {
	TenantID       *uuid.UUID
	IdempotencyKey string
	Type           domain.TxnType
	Description    string
	ReferenceType  string
	ReferenceID    *uuid.UUID
	CreatedBy      *uuid.UUID
	Lines          []domain.Line
}

// PostedLine is a line as posted, with its account's ID.
type PostedLine struct {
	AccountID uuid.UUID
	Line      domain.Line
}

// PostResult is a posting's outcome; Replayed means the key had been
// used for this same posting before and nothing new was written.
type PostResult struct {
	TransactionID uuid.UUID
	Lines         []PostedLine
	Replayed      bool
}

// HoldRequest reserves AmountMinor in one of the tenant's wallets.
type HoldRequest struct {
	TenantID      uuid.UUID
	Account       domain.AccountKey
	ReferenceType string
	ReferenceID   uuid.UUID
	AmountMinor   int64
	ExpiresAt     time.Time
}

// CaptureRequest captures CapturedMinor of a hold. Posting, when set,
// is posted in the same transaction and must take exactly
// CapturedMinor, net, from the held account; its TenantID is ignored
// (the hold's tenant is used).
type CaptureRequest struct {
	TenantID      uuid.UUID
	ReferenceType string
	ReferenceID   uuid.UUID
	CapturedMinor int64
	Posting       *PostRequest
}

// HoldView is a hold as the API shows it.
type HoldView struct {
	ID            uuid.UUID
	AccountID     uuid.UUID
	ReferenceType string
	ReferenceID   uuid.UUID
	AmountMinor   int64
	CapturedMinor int64
	Status        domain.HoldStatus
	Settled       bool
	// TransactionID is the capture's posting, when it carried one.
	TransactionID *uuid.UUID
}

// HoldResult is a hold request's outcome.
type HoldResult struct {
	Hold     HoldView
	Replayed bool
}

// Balance is one account's balance. For VASPRO's accounts HeldMinor is
// 0 and BalanceMinor is summed from the journal (ADR 0010).
type Balance struct {
	AccountID      uuid.UUID
	Account        domain.AccountKey
	Status         string
	BalanceMinor   int64
	HeldMinor      int64
	AvailableMinor int64
}
