package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/shopspring/decimal"

	platformevents "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/events"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/ids"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/money"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/outbox"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/packs"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/domain"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/store"
)

// settlementRole finds the wallets settlement must visit across clients
// (migration 20261106140000).
const settlementRole = "ledger_settlement"

// vatCode is the tax every country pack carries today (data model §5's
// tax_rates.tax_code).
const vatCode = "VAT"

// errNothingToSettle: the wallet's batch for the window exists already,
// its holds were settled by another run, or what was captured is worth
// less than one minor unit. The wallet's transaction rolls back and its
// holds wait for a later window.
var errNothingToSettle = errors.New("app: nothing to settle")

// SyncTaxRates copies each country pack's VAT rate into tax_rates
// (task 3.5, OQ-16), as ledger_system: a rate from a date not yet known
// is a new row; the same date with a different rate stops startup,
// because a rate is never edited. Two packs with one currency also stop
// startup: a wallet's country is found by its currency.
func (s *Service) SyncTaxRates(ctx context.Context) error {
	seen := map[string]string{}
	for country, p := range s.packs {
		if other, ok := seen[p.Currency.Code]; ok {
			return fmt.Errorf("app: packs %s and %s share the currency %s", other, country, p.Currency.Code)
		}
		seen[p.Currency.Code] = country
	}
	return s.pool.WithRoleTx(ctx, systemRole, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		for country, p := range s.packs {
			rate := decimal.New(int64(p.Tax.VATRateBasisPoints), -4)
			from := pgtype.Date{Time: p.Tax.VATEffectiveFrom, Valid: true}
			if err := q.CreateTaxRateIfNew(ctx, store.CreateTaxRateIfNewParams{
				CountryCode: country, TaxCode: vatCode, Rate: toNumeric(rate), EffectiveFrom: from,
			}); err != nil {
				return fmt.Errorf("app: syncing %s's VAT rate: %w", country, err)
			}
			stored, err := q.GetTaxRate(ctx, store.GetTaxRateParams{CountryCode: country, TaxCode: vatCode, EffectiveFrom: from})
			if err != nil {
				return err
			}
			if !toDecimal(stored.Rate).Equal(rate) {
				return fmt.Errorf("app: %s's VAT from %s is %s, the pack says %s: a new rate needs a new date",
					country, p.Tax.VATEffectiveFrom.Format(time.DateOnly), toDecimal(stored.Rate), rate)
			}
		}
		return nil
	})
}

// packForCurrency is the one pack whose currency a wallet is in.
func (s *Service) packForCurrency(code string) (*packs.Pack, error) {
	for _, p := range s.packs {
		if p.Currency.Code == code {
			return p, nil
		}
	}
	return nil, fmt.Errorf("app: no country pack uses %s", code)
}

// SettleUsage settles the window that ended most recently at at (task
// 3.5, OQ-16): for every service wallet with captured, unsettled token
// holds, one usage batch and one usage posting that draws the captured
// tokens from the lots, oldest first, and splits the value drawn into
// revenue and VAT. A wallet already settled for the window, or worth
// less than a minor unit, is skipped; one that fails is logged and
// retried next window, without stopping the others.
func (s *Service) SettleUsage(ctx context.Context, at time.Time) (SettlementSummary, error) {
	start, end := domain.WindowFor(at, s.window)
	sum := SettlementSummary{WindowStart: start, WindowEnd: end}
	var wallets []store.ListSettleableWalletsRow
	err := s.pool.WithRoleTx(ctx, settlementRole, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		wallets, err = store.New(tx).ListSettleableWallets(ctx)
		return err
	})
	if err != nil {
		return sum, fmt.Errorf("app: finding wallets to settle: %w", err)
	}
	for _, w := range wallets {
		batch, err := s.settleWallet(ctx, w.TenantID, w.AccountID, start, end)
		switch {
		case errors.Is(err, errNothingToSettle):
			sum.Skipped++
		case err != nil:
			sum.Failed++
			s.log.Error("ledger: settling a wallet failed; it is retried next window",
				"tenant_id", w.TenantID, "account_id", w.AccountID, "error", s.guardError(err))
		default:
			sum.Batches = append(sum.Batches, batch)
		}
	}
	return sum, nil
}

// settleWallet settles one wallet in its tenant's transaction.
func (s *Service) settleWallet(ctx context.Context, tenant, walletID uuid.UUID, start, end time.Time) (uuid.UUID, error) {
	var wallet store.LedgerAccount
	if err := s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		wallet, err = store.New(tx).GetAccount(ctx, walletID)
		return err
	}); err != nil {
		return uuid.Nil, fmt.Errorf("app: reading the wallet: %w", err)
	}
	key := accountKey(wallet)
	vaspro := []domain.AccountKey{
		{Type: domain.Revenue, ServiceCode: key.ServiceCode, Currency: key.Currency},
		{Type: domain.TaxPayable, Currency: key.Currency},
	}
	var batch uuid.UUID
	err := s.withSystemAccounts(ctx, vaspro, func() error {
		return s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			batch, err = s.settleWalletInTx(ctx, tx, tenant, wallet, start, end)
			return err
		})
	})
	return batch, err
}

func (s *Service) settleWalletInTx(ctx context.Context, tx pgx.Tx, tenant uuid.UUID, wallet store.LedgerAccount, start, end time.Time) (uuid.UUID, error) {
	q := store.New(tx)
	key := accountKey(wallet)

	// Holds first, as a capture locks its hold before its balance.
	holds, err := q.LockCapturedHolds(ctx, wallet.ID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("app: locking captured holds: %w", err)
	}
	if len(holds) == 0 {
		return uuid.Nil, errNothingToSettle // another run settled them
	}
	tokens, events := decimal.Zero, int64(0)
	holdIDs := make([]uuid.UUID, len(holds))
	refs := make([]map[string]any, len(holds))
	for i, h := range holds {
		tokens = tokens.Add(toDecimal(h.CapturedTokens))
		events += h.CapturedEvents.Int64
		holdIDs[i] = h.ID
		refs[i] = map[string]any{"reference_type": h.ReferenceType, "reference_id": h.ReferenceID}
	}

	pack, err := s.packForCurrency(key.Currency)
	if err != nil {
		return uuid.Nil, err
	}
	taxRate, err := q.GetTaxRateAt(ctx, store.GetTaxRateAtParams{CountryCode: pack.CountryCode, TaxCode: vatCode, EffectiveFrom: pgtype.Date{Time: end, Valid: true}})
	if err != nil {
		return uuid.Nil, fmt.Errorf("app: no %s VAT rate for %s: %w", pack.CountryCode, end.Format(time.DateOnly), err)
	}
	rate := money.VATRateBasisPoints(toDecimal(taxRate.Rate).Shift(4).IntPart())
	inclusive := pack.Tax.VATInclusiveByDefault
	revenue := domain.AccountKey{Type: domain.Revenue, ServiceCode: key.ServiceCode, Currency: key.Currency}
	taxPayable := domain.AccountKey{Type: domain.TaxPayable, Currency: key.Currency}
	mainKey := domain.AccountKey{Type: domain.MainWallet, Currency: key.Currency}
	receivableKey := domain.AccountKey{Type: domain.Receivable, Currency: key.Currency}

	// Every client balance the posting may touch, locked together in
	// account_id order (ADR 0010); then the lots, in drawLots.
	toLock := []uuid.UUID{wallet.ID}
	var main store.LedgerAccount
	if !inclusive {
		if main, err = resolveClientAccount(ctx, q, tenant, mainKey); err != nil {
			return uuid.Nil, err
		}
		receivable, err := resolveClientAccount(ctx, q, tenant, receivableKey)
		if err != nil {
			return uuid.Nil, err
		}
		toLock = append(toLock, main.ID, receivable.ID)
	}
	locked, err := lockBalances(ctx, q, toLock)
	if err != nil {
		return uuid.Nil, err
	}

	batchID, err := ids.New()
	if err != nil {
		return uuid.Nil, err
	}
	if _, err := q.CreateUsageBatchIfNew(ctx, store.CreateUsageBatchIfNewParams{
		ID: batchID, TenantID: tenant, AccountID: wallet.ID, WindowStart: start, WindowEnd: end,
		Events: events, Tokens: toNumeric(tokens),
	}); errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, errNothingToSettle // the window's batch exists: these wait for the next
	} else if err != nil {
		return uuid.Nil, fmt.Errorf("app: writing the usage batch: %w", err)
	}

	draws, value, err := s.drawLots(ctx, q, wallet, tokens, tokens)
	if err != nil {
		return uuid.Nil, err
	}
	if value == 0 {
		return uuid.Nil, errNothingToSettle // under one minor unit: nothing to post yet
	}
	var mainAvailable int64
	if !inclusive {
		mainAvailable = locked[main.ID].BalanceMinor - locked[main.ID].HeldMinor
	}
	split, err := domain.SplitUsage(value, rate, inclusive, mainAvailable, pack.Currency)
	if err != nil {
		return uuid.Nil, err
	}

	lines := []domain.Line{{Account: key, Direction: domain.Debit, AmountMinor: value, Tokens: tokens.String()}}
	for _, l := range []domain.Line{
		{Account: mainKey, Direction: domain.Debit, AmountMinor: split.VATFromMain},
		{Account: receivableKey, Direction: domain.Debit, AmountMinor: split.VATToReceivable},
		{Account: revenue, Direction: domain.Credit, AmountMinor: split.Revenue},
		{Account: taxPayable, Direction: domain.Credit, AmountMinor: split.VAT, TaxCode: vatCode},
	} {
		if l.AmountMinor > 0 {
			lines = append(lines, l)
		}
	}
	posted, err := s.postInTx(ctx, tx, PostRequest{
		TenantID: &tenant, IdempotencyKey: "usage:" + batchID.String(), Type: domain.TxnUsage,
		Description:   fmt.Sprintf("%s usage %s to %s", key.ServiceCode, start.Format(time.RFC3339), end.Format(time.RFC3339)),
		ReferenceType: "usage_batch", ReferenceID: &batchID, Lines: lines, internal: true,
	})
	if err != nil {
		return uuid.Nil, err
	}
	if err := recordDraws(ctx, q, draws, posted.TransactionID); err != nil {
		return uuid.Nil, err
	}
	if err := q.SetUsageBatchTransaction(ctx, store.SetUsageBatchTransactionParams{ID: batchID, TransactionID: pgUUID(&posted.TransactionID)}); err != nil {
		return uuid.Nil, err
	}
	now := s.clk.Now()
	if err := q.SettleHolds(ctx, store.SettleHoldsParams{
		SettledAt: pgtype.Timestamptz{Time: now, Valid: true}, UsageBatchID: pgUUID(&batchID), Ids: holdIDs,
	}); err != nil {
		return uuid.Nil, fmt.Errorf("app: settling the holds: %w", err)
	}

	eventID, err := ids.New()
	if err != nil {
		return uuid.Nil, err
	}
	env, err := platformevents.New(eventID, "usage.posted", 1, tenant, now, "", map[string]any{
		"tenant_id": tenant, "account_id": wallet.ID, "batch_id": batchID, "transaction_id": posted.TransactionID,
		"service_code": key.ServiceCode, "currency": key.Currency,
		"window_start": start.Format(time.RFC3339), "window_end": end.Format(time.RFC3339),
		"events": events, "tokens": tokens.String(), "value_minor": value,
		"revenue_minor": split.Revenue, "vat_minor": split.VAT, "vat_inclusive": inclusive,
		"vat_from_main_minor": split.VATFromMain, "vat_to_receivable_minor": split.VATToReceivable,
		"holds": refs,
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("app: building usage.posted: %w", err)
	}
	if err := outbox.Write(ctx, tx, "usage_batch", batchID, env); err != nil {
		return uuid.Nil, err
	}
	return batchID, nil
}
