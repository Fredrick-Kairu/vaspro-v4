package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/shopspring/decimal"

	platformevents "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/events"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/ids"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/outbox"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/domain"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/store"
)

// ErrNoPrice: no active price applies to the service (BAL-4005).
var ErrNoPrice = domain.ErrNoPrice

// PurchaseTokens buys tokens for a service from the tenant's main
// wallet (task 3.4, OQ-15): at the tenant's deal if one is in force,
// otherwise the list price's tier for the quantity; the cost is rounded
// half-up; the price is locked in a new lot. A repeated key returns the
// original purchase, even if the price has changed since.
func (s *Service) PurchaseTokens(ctx context.Context, req PurchaseRequest) (PurchaseResult, error) {
	if err := validatePurchase(req); err != nil {
		return PurchaseResult{}, err
	}
	var res PurchaseResult
	run := func() error {
		return s.pool.WithTenantTx(ctx, req.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			r, err := s.purchaseInTx(ctx, tx, req)
			res = r
			return err
		})
	}
	err := run()
	if uniqueViolationOn(err, transactionsKey) {
		err = run() // a concurrent purchase with this key committed first
	}
	return res, s.guardError(err)
}

func validatePurchase(req PurchaseRequest) error {
	if req.IdempotencyKey == "" || len(req.IdempotencyKey) > 255 {
		return fmt.Errorf("%w: an idempotency key of 1 to 255 characters is required", ErrValidation)
	}
	if _, err := domain.ParseTokens(req.Tokens.String()); err != nil {
		return fmt.Errorf("%w: %v", ErrValidation, err)
	}
	if err := domain.ValidateAccountKey(serviceWallet(req)); err != nil {
		return fmt.Errorf("%w: %v", ErrValidation, err)
	}
	return nil
}

func serviceWallet(req PurchaseRequest) domain.AccountKey {
	return domain.AccountKey{Type: domain.ServiceWallet, ServiceCode: req.ServiceCode, Currency: req.Currency}
}

func (s *Service) purchaseInTx(ctx context.Context, tx pgx.Tx, req PurchaseRequest) (PurchaseResult, error) {
	q := store.New(tx)
	tenant := req.TenantID

	existing, err := q.GetClientTransactionByKey(ctx, store.GetClientTransactionByKeyParams{TenantID: pgUUID(&tenant), IdempotencyKey: req.IdempotencyKey})
	if err == nil {
		return replayPurchase(ctx, q, existing, req)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return PurchaseResult{}, fmt.Errorf("app: looking up the idempotency key: %w", err)
	}

	rows, err := q.ListActivePrices(ctx, store.ListActivePricesParams{ServiceCode: req.ServiceCode, Currency: req.Currency, TenantID: pgUUID(&tenant)})
	if err != nil {
		return PurchaseResult{}, fmt.Errorf("app: reading prices: %w", err)
	}
	now := s.clk.Now()
	price, err := domain.SelectPrice(priceRows(rows), tenant, now, req.Tokens)
	if err != nil {
		return PurchaseResult{}, fmt.Errorf("%w: %s in %s", err, req.ServiceCode, req.Currency)
	}
	cost, err := domain.CostMinor(req.Tokens, price.MinorPerToken)
	if err != nil {
		return PurchaseResult{}, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	if cost <= 0 {
		return PurchaseResult{}, fmt.Errorf("%w: %s tokens at %s cost less than one minor unit", ErrValidation, req.Tokens, price.MinorPerToken)
	}

	wallet := serviceWallet(req)
	posted, err := s.postInTx(ctx, tx, PostRequest{
		TenantID: &tenant, IdempotencyKey: req.IdempotencyKey, Type: domain.TxnTokenPurchase,
		Description: fmt.Sprintf("%s %s tokens", req.Tokens, req.ServiceCode), CreatedBy: req.CreatedBy,
		Lines: []domain.Line{
			{Account: domain.AccountKey{Type: domain.MainWallet, Currency: req.Currency}, Direction: domain.Debit, AmountMinor: cost},
			{Account: wallet, Direction: domain.Credit, AmountMinor: cost, Tokens: req.Tokens.String()},
		},
		internal: true,
	})
	if err != nil {
		return PurchaseResult{}, err
	}
	var walletID uuid.UUID
	for _, l := range posted.Lines {
		if l.Line.Account == wallet {
			walletID = l.AccountID
		}
	}

	lotID, err := ids.New()
	if err != nil {
		return PurchaseResult{}, err
	}
	if _, err := q.CreateTokenLot(ctx, store.CreateTokenLotParams{
		ID: lotID, TenantID: tenant, AccountID: walletID, TransactionID: posted.TransactionID,
		TokensBought: toNumeric(req.Tokens), TokensRemaining: toNumeric(req.Tokens),
		MinorPerToken: toNumeric(price.MinorPerToken), TokenPriceID: price.ID, PurchasedAt: now,
	}); err != nil {
		return PurchaseResult{}, fmt.Errorf("app: writing the lot: %w", err)
	}

	eventID, err := ids.New()
	if err != nil {
		return PurchaseResult{}, err
	}
	env, err := platformevents.New(eventID, "tokens.purchased", 1, tenant, now, "", map[string]any{
		"tenant_id": tenant, "account_id": walletID, "lot_id": lotID, "transaction_id": posted.TransactionID,
		"service_code": req.ServiceCode, "currency": req.Currency, "tokens": req.Tokens.String(),
		"minor_per_token": price.MinorPerToken.String(), "cost_minor": cost, "token_price_id": price.ID,
	})
	if err != nil {
		return PurchaseResult{}, fmt.Errorf("app: building tokens.purchased: %w", err)
	}
	if err := outbox.Write(ctx, tx, "lot", lotID, env); err != nil {
		return PurchaseResult{}, err
	}
	return PurchaseResult{
		TransactionID: posted.TransactionID, LotID: lotID, TokenPriceID: price.ID,
		MinorPerToken: price.MinorPerToken, CostMinor: cost,
	}, nil
}

// replayPurchase answers a repeated key from the lot it created: the
// same service, currency and tokens is the same purchase, at the price
// it was made at.
func replayPurchase(ctx context.Context, q *store.Queries, existing store.LedgerTransaction, req PurchaseRequest) (PurchaseResult, error) {
	conflict := fmt.Errorf("%w: idempotency key %q was used for a different purchase", ErrIdempotencyConflict, req.IdempotencyKey)
	if domain.TxnType(existing.Type) != domain.TxnTokenPurchase {
		return PurchaseResult{}, conflict
	}
	lot, err := q.GetLotByTransaction(ctx, existing.ID)
	if err != nil {
		return PurchaseResult{}, fmt.Errorf("app: reading the purchase's lot: %w", err)
	}
	acct, err := q.GetAccount(ctx, lot.AccountID)
	if err != nil {
		return PurchaseResult{}, err
	}
	bought, price := toDecimal(lot.TokensBought), toDecimal(lot.MinorPerToken)
	if accountKey(acct) != serviceWallet(req) || !bought.Equal(req.Tokens) {
		return PurchaseResult{}, conflict
	}
	cost, err := domain.CostMinor(bought, price)
	if err != nil {
		return PurchaseResult{}, err
	}
	return PurchaseResult{
		TransactionID: existing.ID, LotID: lot.ID, TokenPriceID: lot.TokenPriceID,
		MinorPerToken: price, CostMinor: cost, Replayed: true,
	}, nil
}

func priceRows(rows []store.LedgerTokenPrice) []domain.PriceRow {
	out := make([]domain.PriceRow, 0, len(rows))
	for _, r := range rows {
		p := domain.PriceRow{
			ID: r.ID, TenantID: optionalID(r.TenantID), MinTokens: toDecimal(r.MinTokens),
			MinorPerToken: toDecimal(r.MinorPerToken), Status: r.Status, From: r.EffectiveFrom,
		}
		if r.EffectiveTo.Valid {
			to := r.EffectiveTo.Time
			p.To = &to
		}
		out = append(out, p)
	}
	return out
}

func optionalID(u pgtype.UUID) *uuid.UUID {
	if !u.Valid {
		return nil
	}
	id := uuid.UUID(u.Bytes)
	return &id
}

// drawLots takes tokens from the wallet's open lots, oldest first
// (domain.DrawFIFO), in the caller's tenant transaction: settlement
// calls it with the captured tokens it settles as releaseHeld,
// which leave held_tokens. Other tokens a hold reserves are never
// drawn. The caller posts the returned value from the wallet and then
// records the draws against that posting (recordDraws).
func (s *Service) drawLots(ctx context.Context, q *store.Queries, wallet store.LedgerAccount, tokens, releaseHeld decimal.Decimal) ([]domain.Draw, int64, error) {
	locked, err := lockBalances(ctx, q, []uuid.UUID{wallet.ID})
	if err != nil {
		return nil, 0, err
	}
	rows, err := q.LockOpenLots(ctx, wallet.ID)
	if err != nil {
		return nil, 0, fmt.Errorf("app: locking lots: %w", err)
	}
	lots := domainLots(rows)
	available := tokensLeft(lots).Sub(toDecimal(locked[wallet.ID].HeldTokens)).Add(releaseHeld)
	if tokens.GreaterThan(available) {
		return nil, 0, fmt.Errorf("%w: %s tokens available, %s needed", ErrInsufficientBalance, available, tokens)
	}
	draws, value, err := domain.DrawFIFO(lots, tokens)
	if err != nil {
		return nil, 0, err
	}
	for _, d := range draws {
		if err := q.DrawFromLot(ctx, store.DrawFromLotParams{ID: d.LotID, Tokens: toNumeric(d.Tokens)}); err != nil {
			return nil, 0, fmt.Errorf("app: drawing from a lot: %w", err)
		}
	}
	if releaseHeld.IsPositive() {
		if err := q.AdjustHeldTokens(ctx, store.AdjustHeldTokensParams{Delta: toNumeric(releaseHeld.Neg()), AccountID: wallet.ID}); err != nil {
			return nil, 0, err
		}
	}
	return draws, value, nil
}

// recordDraws writes the lot consumptions behind a usage posting.
func recordDraws(ctx context.Context, q *store.Queries, draws []domain.Draw, txnID uuid.UUID) error {
	for _, d := range draws {
		if _, err := q.CreateLotConsumption(ctx, store.CreateLotConsumptionParams{
			LotID: d.LotID, TransactionID: txnID, Tokens: toNumeric(d.Tokens), ValueMinor: d.ValueMinor,
		}); err != nil {
			return fmt.Errorf("app: recording a lot consumption: %w", err)
		}
	}
	return nil
}

func domainLots(rows []store.LedgerTokenLot) []domain.Lot {
	lots := make([]domain.Lot, len(rows))
	for i, r := range rows {
		lots[i] = domain.Lot{ID: r.ID, Bought: toDecimal(r.TokensBought), Remaining: toDecimal(r.TokensRemaining), Price: toDecimal(r.MinorPerToken)}
	}
	return lots
}

func tokensLeft(lots []domain.Lot) decimal.Decimal {
	total := decimal.Zero
	for _, l := range lots {
		total = total.Add(l.Remaining)
	}
	return total
}
