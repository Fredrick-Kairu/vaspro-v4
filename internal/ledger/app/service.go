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
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/packs"

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
	// ErrInsufficientBalance: a wallet's available money doesn't cover
	// the request (BAL-4001).
	ErrInsufficientBalance = errors.New("app: insufficient balance")
	// ErrInsufficientTokens: a service wallet doesn't have enough tokens
	// left beyond those held (BAL-4002, system design §8: "no tokens left
	// for this service; buy tokens or turn on auto-buy").
	ErrInsufficientTokens = errors.New("app: no tokens left for this service")
	// ErrAccountNotOpen: a frozen or closed account (BAL-4006).
	ErrAccountNotOpen = errors.New("app: account not open")
	// ErrNotStaff: a staff action by someone who isn't staff (AUTH-4025).
	ErrNotStaff = errors.New("app: staff only")
	// ErrStepUpRequired: the session's step-up isn't fresh (AUTH-4012).
	ErrStepUpRequired = errors.New("app: step-up required")
	// ErrMakerCannotApprove: maker-checker, a price's or reversal's maker
	// deciding it (VAL-4013).
	ErrMakerCannotApprove = errors.New("app: the maker can't decide their own request")
	// ErrPriceDecided: the price was already approved or withdrawn
	// (VAL-4014).
	ErrPriceDecided = errors.New("app: price already decided")
	// ErrPriceOverlap: approving would overlap an active price (VAL-4012).
	ErrPriceOverlap = errors.New("app: price would overlap an active one")
	// ErrPermissionDenied: a dashboard user whose role lacks the
	// permission (AUTH-4024).
	ErrPermissionDenied = errors.New("app: your role doesn't allow this")
	// ErrFinanceLeadRequired: a reversal at or above the pack's threshold
	// approved by staff who aren't a finance lead (AUTH-4026).
	ErrFinanceLeadRequired = errors.New("app: a finance lead must approve this reversal")
)

// domain.ErrHoldState (BAL-4003) and domain.ErrAllocationFinal
// (BAL-4004) are returned as they are, as are the reversal errors
// domain.ErrReversalDecided (VAL-4014), ErrReversalExceedsOriginal
// (VAL-4015), ErrNotAwaitingPayback (VAL-4016) and
// ErrReversalNotCovered (BAL-4007).

// Service holds the ledger's use cases.
type Service struct {
	pool *db.Pool
	clk  clock.Clock
	log  *slog.Logger
	// identity answers staff and step-up checks (task 3.4b); nil makes
	// every staff action fail closed.
	identity adapters.Identity
	// accounts answers whether a dashboard user may work in a tenant
	// with a permission (task 3.9); nil fails every dashboard call
	// closed.
	accounts adapters.Accounts
	// devTopUps lets staff credit main wallets from the sandbox rail
	// (task 3.9, OQ-20): dev environments only.
	devTopUps bool
	// packs are the country packs by country code: their VAT rates and
	// whether prices include VAT (task 3.5). A wallet's country is the
	// one pack with its currency.
	packs  map[string]*packs.Pack
	window time.Duration
}

// Deps is everything New needs. Pool must connect as a role granted
// ledger_service (and through it ledger_system), never the tables'
// owner (migration 20261106090000).
type Deps struct {
	Pool     *db.Pool
	Clock    clock.Clock
	Logger   *slog.Logger
	Identity adapters.Identity
	// Accounts checks dashboard callers' access (task 3.9).
	Accounts adapters.Accounts
	// DevTopUps turns on staff's sandbox top-ups (LEDGER_DEV_TOPUPS).
	DevTopUps bool
	// Packs are every loaded country pack, by country code.
	Packs map[string]*packs.Pack
	// SettlementWindow is OD-07's settlement window; 0 is the default,
	// 5 minutes.
	SettlementWindow time.Duration
}

// DefaultSettlementWindow is OD-07's default.
const DefaultSettlementWindow = 5 * time.Minute

// New builds a Service.
func New(d Deps) *Service {
	log := d.Logger
	if log == nil {
		log = slog.Default()
	}
	window := d.SettlementWindow
	if window == 0 {
		window = DefaultSettlementWindow
	}
	return &Service{pool: d.Pool, clk: d.Clock, log: log, identity: d.Identity, accounts: d.Accounts, devTopUps: d.DevTopUps,
		packs: d.Packs, window: window}
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
	// reverses is the transaction a reversal or pay-back undoes
	// (transactions.reverses_id, task 3.7).
	reverses *uuid.UUID
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
//
// A money hold is captured only with its posting: a plain money capture
// would have nothing to settle into (OQ-16). Events, for a token hold,
// is how many billable events (delivered messages) the capture covers;
// 0 means 1. Settlement sums them into its batch.
type CaptureRequest struct {
	TenantID       uuid.UUID
	ReferenceType  string
	ReferenceID    uuid.UUID
	CapturedMinor  int64
	CapturedTokens decimal.Decimal
	Events         int64
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

// SettlementSummary is what one settlement run did, for its window.
type SettlementSummary struct {
	WindowStart time.Time
	WindowEnd   time.Time
	// Batches are the usage batches posted, one per wallet settled.
	Batches []uuid.UUID
	// Skipped wallets had a batch for the window already, or captured
	// tokens worth less than one minor unit; Failed ones are logged and
	// retried next window.
	Skipped int
	Failed  int
}

// ExpirySummary is what one expiry run did.
type ExpirySummary struct {
	Expired int
	Failed  int
}

// AutoBuyRule is a tenant's auto-buy rule for one service and currency
// (task 3.6): when a token hold would leave fewer than BelowTokens, or
// couldn't be covered, BuyTokens are bought from the main wallet first.
type AutoBuyRule struct {
	ID              uuid.UUID
	ServiceCode     string
	Currency        string
	BelowTokens     decimal.Decimal
	BuyTokens       decimal.Decimal
	Enabled         bool
	LastTriggeredAt *time.Time
}
