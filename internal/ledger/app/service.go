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
	"github.com/shopspring/decimal"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/clock"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/adapters"
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
	// ErrNotStaff: a staff action by someone who isn't staff (AUTH-4025).
	ErrNotStaff = errors.New("app: staff only")
	// ErrStepUpRequired: the session's step-up isn't fresh (AUTH-4012).
	ErrStepUpRequired = errors.New("app: step-up required")
	// ErrMakerCannotApprove: maker-checker, the price's maker approving
	// it (VAL-4013).
	ErrMakerCannotApprove = errors.New("app: a price's maker can't approve it")
	// ErrPriceDecided: the price was already approved or withdrawn
	// (VAL-4014).
	ErrPriceDecided = errors.New("app: price already decided")
	// ErrPriceOverlap: approving would overlap an active price (VAL-4012).
	ErrPriceOverlap = errors.New("app: price would overlap an active one")
)

// domain.ErrHoldState (BAL-4003) and domain.ErrAllocationFinal
// (BAL-4004) are returned as they are.

// Service holds the ledger's use cases.
type Service struct {
	pool *db.Pool
	clk  clock.Clock
	log  *slog.Logger
	// identity answers staff and step-up checks (task 3.4b); nil makes
	// every staff action fail closed.
	identity adapters.Identity
}

// Deps is everything New needs. Pool must connect as a role granted
// ledger_service (and through it ledger_system), never the tables'
// owner (migration 20261106090000).
type Deps struct {
	Pool     *db.Pool
	Clock    clock.Clock
	Logger   *slog.Logger
	Identity adapters.Identity
}

// New builds a Service.
func New(d Deps) *Service {
	log := d.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Service{pool: d.Pool, clk: d.Clock, log: log, identity: d.Identity}
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
	// internal marks a posting from the ledger's own token use cases,
	// the only ones that may touch a service wallet (OQ-15).
	internal bool
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

// HoldRequest reserves part of one of the tenant's wallets: Tokens in a
// service wallet (OQ-15), AmountMinor in a main or bonus wallet.
type HoldRequest struct {
	TenantID      uuid.UUID
	Account       domain.AccountKey
	ReferenceType string
	ReferenceID   uuid.UUID
	AmountMinor   int64
	Tokens        decimal.Decimal
	ExpiresAt     time.Time
}

// CaptureRequest captures CapturedMinor of a hold. Posting, when set,
// is posted in the same transaction and must take exactly
// CapturedMinor, net, from the held account; its TenantID is ignored
// (the hold's tenant is used).
//
// A token hold is captured in CapturedTokens and can't carry a posting
// yet: its usage posting, with VAT, comes with settlement (3.5).
type CaptureRequest struct {
	TenantID       uuid.UUID
	ReferenceType  string
	ReferenceID    uuid.UUID
	CapturedMinor  int64
	CapturedTokens decimal.Decimal
	Posting        *PostRequest
}

// HoldView is a hold as the API shows it. For a token hold, Tokens and
// CapturedTokens are what's reserved and used; AmountMinor and
// CapturedMinor are only their estimated value (OQ-15).
type HoldView struct {
	ID             uuid.UUID
	AccountID      uuid.UUID
	ReferenceType  string
	ReferenceID    uuid.UUID
	AmountMinor    int64
	CapturedMinor  int64
	Tokens         decimal.Decimal
	CapturedTokens decimal.Decimal
	Status         domain.HoldStatus
	Settled        bool
	// TransactionID is the capture's posting, when it carried one.
	TransactionID *uuid.UUID
}

// HoldResult is a hold request's outcome.
type HoldResult struct {
	Hold     HoldView
	Replayed bool
}

// Balance is one account's balance. For VASPRO's accounts HeldMinor is
// 0 and BalanceMinor is summed from the journal (ADR 0010). A service
// wallet also has tokens: those left in its lots, those held, and those
// available for new holds and use.
type Balance struct {
	AccountID       uuid.UUID
	Account         domain.AccountKey
	Status          string
	BalanceMinor    int64
	HeldMinor       int64
	AvailableMinor  int64
	TokensRemaining decimal.Decimal
	HeldTokens      decimal.Decimal
	TokensAvailable decimal.Decimal
}

// PurchaseRequest buys Tokens for a service from the tenant's main
// wallet in the same currency, at the tenant's price (OQ-15).
type PurchaseRequest struct {
	TenantID       uuid.UUID
	IdempotencyKey string
	ServiceCode    string
	Currency       string
	Tokens         decimal.Decimal
	CreatedBy      *uuid.UUID
}

// PurchaseResult is a purchase's lot and what it cost.
type PurchaseResult struct {
	TransactionID uuid.UUID
	LotID         uuid.UUID
	TokenPriceID  uuid.UUID
	MinorPerToken decimal.Decimal
	CostMinor     int64
	Replayed      bool
}

// TokenPrice is a token_prices row as staff see it (task 3.4b).
type TokenPrice struct {
	ID            uuid.UUID
	TenantID      *uuid.UUID
	ServiceCode   string
	Currency      string
	MinTokens     decimal.Decimal
	MinorPerToken decimal.Decimal
	DealReference string
	Status        string
	From          time.Time
	To            *time.Time
	CreatedBy     uuid.UUID
	ApprovedBy    *uuid.UUID
}

// PriceFilter narrows staff's list of prices. Limit 0 is the default
// page (50); at most 100.
type PriceFilter struct {
	TenantID    *uuid.UUID
	ServiceCode string
	Status      string
	Cursor      string
	Limit       int
}

// TokenPricePage is one page of prices, oldest first.
type TokenPricePage struct {
	Prices     []TokenPrice
	NextCursor string
}
