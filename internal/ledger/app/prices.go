package app

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/ids"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/domain"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/store"
)

// priceReviewRole reads token prices across clients (migration
// 20261106130000).
const priceReviewRole = "ledger_price_review"

const (
	defaultPricePage = 50
	maxPricePage     = 100
)

// requireStaff asks identity on every staff action, so a revocation
// takes effect at once (2.9). Identity being unreachable, or not wired,
// fails closed.
func (s *Service) requireStaff(ctx context.Context, userID uuid.UUID) error {
	if s.identity == nil {
		return errors.New("app: no identity adapter for staff checks")
	}
	staff, err := s.identity.IsStaff(ctx, userID)
	if err != nil {
		return fmt.Errorf("app: checking staff: %w", err)
	}
	if !staff {
		return ErrNotStaff
	}
	return nil
}

// requireStaffStepUp is requireStaff plus a fresh step-up on the
// caller's session, for actions that set what clients pay (OQ-15).
func (s *Service) requireStaffStepUp(ctx context.Context, userID, sessionID uuid.UUID) error {
	if err := s.requireStaff(ctx, userID); err != nil {
		return err
	}
	fresh, err := s.identity.IsStepUpFresh(ctx, userID, sessionID)
	if err != nil {
		return fmt.Errorf("app: checking step-up: %w", err)
	}
	if !fresh {
		return ErrStepUpRequired
	}
	return nil
}

// inPriceOwnerTx runs fn in the transaction that owns a price: the
// client's for a deal, ledger_system's for a list price. Staff act
// through the owner's transaction, never across clients (2.9).
func (s *Service) inPriceOwnerTx(ctx context.Context, tenant *uuid.UUID, fn func(ctx context.Context, q *store.Queries) error) error {
	return s.inOwnerTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error { return fn(ctx, store.New(tx)) })
}

// CreateTokenPrice enters a price as pending_approval (task 3.4b): it
// is never used until another staff member approves it.
func (s *Service) CreateTokenPrice(ctx context.Context, staffID uuid.UUID, p domain.NewPrice) (TokenPrice, error) {
	if err := s.requireStaff(ctx, staffID); err != nil {
		return TokenPrice{}, err
	}
	valid, err := domain.ValidateNewPrice(p)
	if err != nil {
		return TokenPrice{}, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	id, err := ids.New()
	if err != nil {
		return TokenPrice{}, err
	}
	params := store.CreateTokenPriceParams{
		ID: id, TenantID: pgUUID(p.TenantID), ServiceCode: p.ServiceCode, Currency: p.Currency,
		MinTokens: toNumeric(valid.MinTokens), MinorPerToken: toNumeric(valid.MinorPerToken),
		DealReference: text(p.DealReference), Status: "pending_approval",
		EffectiveFrom: p.From, CreatedBy: staffID,
	}
	if p.To != nil {
		params.EffectiveTo = pgtype.Timestamptz{Time: *p.To, Valid: true}
	}
	var row store.LedgerTokenPrice
	err = s.inPriceOwnerTx(ctx, p.TenantID, func(ctx context.Context, q *store.Queries) error {
		row, err = q.CreateTokenPrice(ctx, params)
		return err
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23503" {
		return TokenPrice{}, fmt.Errorf("%w: unknown currency %s", ErrValidation, p.Currency)
	}
	if err != nil {
		return TokenPrice{}, fmt.Errorf("app: writing the price: %w", err)
	}
	return tokenPrice(row), nil
}

// ListTokenPrices is staff's list across clients, oldest first, read as
// ledger_price_review.
func (s *Service) ListTokenPrices(ctx context.Context, staffID uuid.UUID, f PriceFilter) (TokenPricePage, error) {
	if err := s.requireStaff(ctx, staffID); err != nil {
		return TokenPricePage{}, err
	}
	limit := f.Limit
	if limit == 0 {
		limit = defaultPricePage
	}
	if limit < 1 || limit > maxPricePage {
		return TokenPricePage{}, fmt.Errorf("%w: limit is 1 to %d", ErrValidation, maxPricePage)
	}
	params := store.ListTokenPricesPageParams{
		TenantID: pgUUID(f.TenantID), ServiceCode: text(f.ServiceCode), Status: text(f.Status),
		MaxRows: int32(limit + 1), //nolint:gosec // limit is 1 to 100
	}
	if f.Cursor != "" {
		after, err := decodePriceCursor(f.Cursor)
		if err != nil {
			return TokenPricePage{}, err
		}
		params.After = pgUUID(&after)
	}
	var rows []store.LedgerTokenPrice
	err := s.pool.WithRoleTx(ctx, priceReviewRole, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		rows, err = store.New(tx).ListTokenPricesPage(ctx, params)
		return err
	})
	if err != nil {
		return TokenPricePage{}, fmt.Errorf("app: listing prices: %w", err)
	}
	var page TokenPricePage
	if len(rows) > limit {
		rows = rows[:limit]
		page.NextCursor = base64.RawURLEncoding.EncodeToString(rows[limit-1].ID[:])
	}
	for _, r := range rows {
		page.Prices = append(page.Prices, tokenPrice(r))
	}
	return page, nil
}

func decodePriceCursor(cursor string) (uuid.UUID, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil || len(raw) != 16 {
		return uuid.Nil, fmt.Errorf("%w: malformed cursor", ErrValidation)
	}
	return uuid.UUID(raw), nil
}

// ApproveTokenPrice is the checker's step of maker-checker: another
// staff member than the maker, with a fresh step-up, approves a pending
// price. The database's CHECK and EXCLUDE back the maker and overlap
// rules.
func (s *Service) ApproveTokenPrice(ctx context.Context, staffID, sessionID, priceID uuid.UUID) (TokenPrice, error) {
	return s.decidePrice(ctx, staffID, sessionID, priceID, func(ctx context.Context, q *store.Queries, row store.LedgerTokenPrice) (store.LedgerTokenPrice, error) {
		if row.Status != "pending_approval" {
			return row, fmt.Errorf("%w: the price is %s", ErrPriceDecided, row.Status)
		}
		if row.CreatedBy == staffID {
			return row, ErrMakerCannotApprove
		}
		approved, err := q.ApproveTokenPrice(ctx, store.ApproveTokenPriceParams{ID: row.ID, ApprovedBy: pgUUID(&staffID)})
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23P01" {
			return row, ErrPriceOverlap
		}
		return approved, err
	})
}

// WithdrawTokenPrice stops a pending or active price applying from now
// on; lots already bought keep their price.
func (s *Service) WithdrawTokenPrice(ctx context.Context, staffID, sessionID, priceID uuid.UUID) (TokenPrice, error) {
	return s.decidePrice(ctx, staffID, sessionID, priceID, func(ctx context.Context, q *store.Queries, row store.LedgerTokenPrice) (store.LedgerTokenPrice, error) {
		if row.Status == "withdrawn" {
			return row, fmt.Errorf("%w: the price is already withdrawn", ErrPriceDecided)
		}
		return q.WithdrawTokenPrice(ctx, row.ID)
	})
}

// decidePrice checks the staff member and step-up, finds the price
// across clients, then locks and changes it in its owner's transaction.
func (s *Service) decidePrice(ctx context.Context, staffID, sessionID, priceID uuid.UUID,
	decide func(ctx context.Context, q *store.Queries, row store.LedgerTokenPrice) (store.LedgerTokenPrice, error)) (TokenPrice, error) {
	if err := s.requireStaffStepUp(ctx, staffID, sessionID); err != nil {
		return TokenPrice{}, err
	}
	var found store.LedgerTokenPrice
	err := s.pool.WithRoleTx(ctx, priceReviewRole, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		found, err = store.New(tx).GetTokenPrice(ctx, priceID)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return TokenPrice{}, fmt.Errorf("%w: no price %s", ErrNotFound, priceID)
	}
	if err != nil {
		return TokenPrice{}, err
	}
	var decided store.LedgerTokenPrice
	err = s.inPriceOwnerTx(ctx, optionalID(found.TenantID), func(ctx context.Context, q *store.Queries) error {
		row, err := q.LockTokenPrice(ctx, priceID)
		if err != nil {
			return err
		}
		decided, err = decide(ctx, q, row)
		return err
	})
	if err != nil {
		return TokenPrice{}, err
	}
	return tokenPrice(decided), nil
}

func tokenPrice(r store.LedgerTokenPrice) TokenPrice {
	p := TokenPrice{
		ID: r.ID, TenantID: optionalID(r.TenantID), ServiceCode: r.ServiceCode, Currency: r.Currency,
		MinTokens: toDecimal(r.MinTokens), MinorPerToken: toDecimal(r.MinorPerToken), DealReference: r.DealReference.String,
		Status: r.Status, From: r.EffectiveFrom, CreatedBy: r.CreatedBy, ApprovedBy: optionalID(r.ApprovedBy),
	}
	if r.EffectiveTo.Valid {
		to := r.EffectiveTo.Time
		p.To = &to
	}
	return p
}
