package app

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/shopspring/decimal"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/adapters"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/domain"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/store"
)

const (
	defaultStatementPage = 50
	maxStatementPage     = 100
)

// Accounts' permissions the dashboard routes need (task 3.9, OQ-20).
const (
	PermBillingRead   = "billing:read"
	PermBillingManage = "billing:manage"
)

// StatementQuery picks an account's statement page.
type StatementQuery struct {
	AccountID uuid.UUID
	From, To  *time.Time
	Cursor    string
	Limit     int
}

// StatementLine is one line, with the balance after it; for a service
// wallet, Tokens and TokensAfter too.
type StatementLine struct {
	EntryID           int64
	TransactionID     uuid.UUID
	PostedAt          time.Time
	Type              domain.TxnType
	Description       string
	ReferenceType     string
	ReferenceID       *uuid.UUID
	Direction         domain.Side
	AmountMinor       int64
	Tokens            *decimal.Decimal
	BalanceAfterMinor int64
	TokensAfter       *decimal.Decimal
}

// Statement is one page of an account's lines, newest first.
type Statement struct {
	AccountID  uuid.UUID
	Account    domain.AccountKey
	Lines      []StatementLine
	NextCursor string
}

// TenantPrice is one tier the client buys at now: its deal's, or the
// list's when it has none.
type TenantPrice struct {
	ServiceCode   string
	Currency      string
	Source        string // deal or list
	MinTokens     decimal.Decimal
	MinorPerToken decimal.Decimal
	DealReference string
	EffectiveTo   *time.Time
}

// Quote is what buying Tokens would cost now.
type Quote struct {
	ServiceCode        string
	Currency           string
	Tokens             decimal.Decimal
	Source             string
	MinTokens          decimal.Decimal
	MinorPerToken      decimal.Decimal
	CostMinor          int64
	MainAvailableMinor int64
	Affordable         bool
}

// DevTopUpRequest credits a tenant's main wallet from the sandbox rail.
type DevTopUpRequest struct {
	TenantID       uuid.UUID
	Currency       string
	AmountMinor    int64
	IdempotencyKey string
}

// requireTenant asks accounts, on every call, whether user may work in
// tenant with perm (task 3.9, OQ-20). No access and no such account are
// ErrNotFound alike; a role without perm is ErrPermissionDenied;
// accounts unreachable, or not wired, fails closed.
func (s *Service) requireTenant(ctx context.Context, user, tenant uuid.UUID, perm string) error {
	if s.accounts == nil {
		return errors.New("app: no accounts adapter for access checks")
	}
	err := s.accounts.CheckTenantAccess(ctx, user, tenant, perm)
	switch {
	case errors.Is(err, adapters.ErrNoTenantAccess):
		return fmt.Errorf("%w: no account %s", ErrNotFound, tenant)
	case errors.Is(err, adapters.ErrPermissionDenied):
		return fmt.Errorf("%w: %s", ErrPermissionDenied, perm)
	case err != nil:
		return fmt.Errorf("app: checking account access: %w", err)
	}
	return nil
}

// TenantBalances is the dashboard's wallet list: every account of the
// tenant with its balance, held amount and tokens.
func (s *Service) TenantBalances(ctx context.Context, user, tenant uuid.UUID) ([]Balance, error) {
	if err := s.requireTenant(ctx, user, tenant, PermBillingRead); err != nil {
		return nil, err
	}
	return s.GetBalances(ctx, &tenant)
}

// Statement is one page of one of the tenant's accounts, newest first,
// each line with the balance (and, for a service wallet, the tokens)
// after it, read in one snapshot (task 3.9).
func (s *Service) Statement(ctx context.Context, user, tenant uuid.UUID, sq StatementQuery) (Statement, error) {
	if err := s.requireTenant(ctx, user, tenant, PermBillingRead); err != nil {
		return Statement{}, err
	}
	limit := sq.Limit
	if limit == 0 {
		limit = defaultStatementPage
	}
	if limit < 1 || limit > maxStatementPage {
		return Statement{}, fmt.Errorf("%w: limit is 1 to %d", ErrValidation, maxStatementPage)
	}
	params := store.StatementPageParams{AccountID: sq.AccountID, MaxRows: int32(limit + 1)} //nolint:gosec // limit is at most maxStatementPage
	if sq.Cursor != "" {
		before, err := decodeStatementCursor(sq.Cursor)
		if err != nil {
			return Statement{}, err
		}
		params.Before = pgtype.Int8{Int64: before, Valid: true}
	}
	if sq.From != nil {
		params.FromAt = pgtype.Timestamptz{Time: *sq.From, Valid: true}
	}
	if sq.To != nil {
		params.ToAt = pgtype.Timestamptz{Time: *sq.To, Valid: true}
	}
	var out Statement
	err := s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		acct, err := q.GetAccount(ctx, sq.AccountID)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && (!acct.TenantID.Valid || uuid.UUID(acct.TenantID.Bytes) != tenant)) {
			return fmt.Errorf("%w: no account %s", ErrNotFound, sq.AccountID) // VASPRO's accounts are readable, never statements
		}
		if err != nil {
			return err
		}
		out.AccountID, out.Account = acct.ID, accountKey(acct)
		rows, err := q.StatementPage(ctx, params)
		if err != nil {
			return fmt.Errorf("app: reading the statement: %w", err)
		}
		if len(rows) > limit {
			rows = rows[:limit]
			out.NextCursor = base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(rows[limit-1].EntryID, 10)))
		}
		for _, r := range rows {
			out.Lines = append(out.Lines, statementLine(r, out.Account.Type == domain.ServiceWallet))
		}
		return nil
	})
	if err != nil {
		return Statement{}, err
	}
	return out, nil
}

func statementLine(r store.StatementPageRow, tokens bool) StatementLine {
	l := StatementLine{
		EntryID: r.EntryID, TransactionID: r.TransactionID, PostedAt: r.PostedAt, Type: domain.TxnType(r.Type),
		Description: r.Description, ReferenceType: r.ReferenceType.String, ReferenceID: optionalID(r.ReferenceID),
		Direction: domain.Side(r.Direction[0]), AmountMinor: r.AmountMinor, BalanceAfterMinor: r.BalanceAfter,
	}
	if r.Tokens != "" {
		t := decimal.RequireFromString(r.Tokens)
		l.Tokens = &t
	}
	if tokens {
		t := decimal.RequireFromString(r.TokensAfter)
		l.TokensAfter = &t
	}
	return l
}

func decodeStatementCursor(cursor string) (int64, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err == nil {
		if id, perr := strconv.ParseInt(string(raw), 10, 64); perr == nil && id > 0 {
			return id, nil
		}
	}
	return 0, fmt.Errorf("%w: malformed cursor", ErrValidation)
}

// TenantPrices is what the client buys at now, per service and
// currency: its deal's tiers while one is in force, else the list's
// (domain.PricesInForce, the rows SelectPrice chooses from).
func (s *Service) TenantPrices(ctx context.Context, user, tenant uuid.UUID) ([]TenantPrice, error) {
	if err := s.requireTenant(ctx, user, tenant, PermBillingRead); err != nil {
		return nil, err
	}
	var rows []store.LedgerTokenPrice
	err := s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		rows, err = store.New(tx).ListTenantPricesInForce(ctx, pgUUID(&tenant))
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("app: reading prices: %w", err)
	}
	type group struct{ service, currency string }
	var order []group
	byGroup := map[group][]store.LedgerTokenPrice{}
	for _, r := range rows {
		g := group{r.ServiceCode, r.Currency}
		if _, seen := byGroup[g]; !seen {
			order = append(order, g)
		}
		byGroup[g] = append(byGroup[g], r)
	}
	now := s.clk.Now()
	var out []TenantPrice
	for _, g := range order {
		for _, p := range domain.PricesInForce(priceRows(byGroup[g]), tenant, now) {
			out = append(out, tenantPrice(g.service, g.currency, p, byGroup[g]))
		}
	}
	return out, nil
}

func tenantPrice(service, currency string, p domain.PriceRow, rows []store.LedgerTokenPrice) TenantPrice {
	out := TenantPrice{ServiceCode: service, Currency: currency, Source: "list", MinTokens: p.MinTokens, MinorPerToken: p.MinorPerToken, EffectiveTo: p.To}
	if p.TenantID != nil {
		out.Source = "deal"
		for _, r := range rows {
			if r.ID == p.ID {
				out.DealReference = r.DealReference.String
			}
		}
	}
	return out
}

// QuoteTokens is what buying tokens would cost now, at the price a
// purchase would use (domain.SelectPrice), and whether the main
// wallet's available money covers it. Nothing is reserved.
func (s *Service) QuoteTokens(ctx context.Context, user, tenant uuid.UUID, serviceCode, currency string, tokens decimal.Decimal) (Quote, error) {
	if err := s.requireTenant(ctx, user, tenant, PermBillingRead); err != nil {
		return Quote{}, err
	}
	qty, err := domain.ParseTokens(tokens.String())
	if err != nil {
		return Quote{}, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	out := Quote{ServiceCode: serviceCode, Currency: currency, Tokens: qty}
	err = s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		rows, err := q.ListActivePrices(ctx, store.ListActivePricesParams{ServiceCode: serviceCode, Currency: currency, TenantID: pgUUID(&tenant)})
		if err != nil {
			return fmt.Errorf("app: reading prices: %w", err)
		}
		price, err := domain.SelectPrice(priceRows(rows), tenant, s.clk.Now(), qty)
		if err != nil {
			return err
		}
		if out.CostMinor, err = domain.CostMinor(qty, price.MinorPerToken); err != nil {
			return fmt.Errorf("%w: %v", ErrValidation, err)
		}
		out.MinTokens, out.MinorPerToken, out.Source = price.MinTokens, price.MinorPerToken, "list"
		if price.TenantID != nil {
			out.Source = "deal"
		}
		main, err := q.FindClientAccount(ctx, store.FindClientAccountParams{
			TenantID: pgUUID(&tenant), Type: store.LedgerAccountType(domain.MainWallet), Currency: currency,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // no main wallet yet: nothing available
		}
		if err != nil {
			return err
		}
		b, err := q.GetBalance(ctx, main.ID)
		if err != nil {
			return err
		}
		out.MainAvailableMinor = b.BalanceMinor - b.HeldMinor
		return nil
	})
	if err != nil {
		return Quote{}, err
	}
	out.Affordable = out.CostMinor > 0 && out.CostMinor <= out.MainAvailableMinor
	return out, nil
}

// BuyTokens is the dashboard's purchase: PurchaseTokens for a user with
// billing:manage in the tenant.
func (s *Service) BuyTokens(ctx context.Context, user uuid.UUID, req PurchaseRequest) (PurchaseResult, error) {
	if err := s.requireTenant(ctx, user, req.TenantID, PermBillingManage); err != nil {
		return PurchaseResult{}, err
	}
	return s.PurchaseTokens(ctx, req)
}

// TenantAutoBuyRules lists the tenant's auto-buy rules for the dashboard.
func (s *Service) TenantAutoBuyRules(ctx context.Context, user, tenant uuid.UUID) ([]AutoBuyRule, error) {
	if err := s.requireTenant(ctx, user, tenant, PermBillingRead); err != nil {
		return nil, err
	}
	return s.ListAutoBuyRules(ctx, tenant)
}

// SetTenantAutoBuyRule sets one of the tenant's auto-buy rules from the
// dashboard (billing:manage).
func (s *Service) SetTenantAutoBuyRule(ctx context.Context, user, tenant uuid.UUID, r AutoBuyRule) (AutoBuyRule, error) {
	if err := s.requireTenant(ctx, user, tenant, PermBillingManage); err != nil {
		return AutoBuyRule{}, err
	}
	return s.SetAutoBuyRule(ctx, tenant, r)
}

// sandboxRail is the provider clearing dev top-ups come from.
const sandboxRail = "sandbox"

// DevTopUp lets staff credit a tenant's main wallet from the sandbox
// rail, so testers have money before Payments exists (task 3.9,
// OQ-20). Staff only, like every staff route; and only where
// LEDGER_DEV_TOPUPS is on, elsewhere not found, as if the route weren't
// there. It's an ordinary topup posting, so
// wallet.credited goes out and the nightly rebuild sees it like any
// other.
func (s *Service) DevTopUp(ctx context.Context, staffID uuid.UUID, req DevTopUpRequest) (PostResult, error) {
	if err := s.requireStaff(ctx, staffID); err != nil {
		return PostResult{}, err
	}
	if !s.devTopUps {
		return PostResult{}, fmt.Errorf("%w: dev top-ups are off", ErrNotFound)
	}
	if req.AmountMinor <= 0 || req.IdempotencyKey == "" {
		return PostResult{}, fmt.Errorf("%w: a positive amount_minor and an idempotency key are required", ErrValidation)
	}
	clearing := domain.AccountKey{Type: domain.ProviderClearing, Rail: sandboxRail, Currency: req.Currency}
	main := domain.AccountKey{Type: domain.MainWallet, Currency: req.Currency}
	return s.Post(ctx, PostRequest{
		TenantID: &req.TenantID, IdempotencyKey: "dev-topup:" + req.IdempotencyKey, Type: domain.TxnTopup,
		Description: "dev top-up (sandbox)", CreatedBy: &staffID,
		Lines: []domain.Line{
			{Account: clearing, Direction: domain.Debit, AmountMinor: req.AmountMinor},
			{Account: main, Direction: domain.Credit, AmountMinor: req.AmountMinor},
		},
	})
}
