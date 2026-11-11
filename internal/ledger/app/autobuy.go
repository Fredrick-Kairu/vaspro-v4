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
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/outbox"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/domain"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/store"
)

// maxExpiredPerRun bounds one expiry run; the next window's run takes
// the rest.
const maxExpiredPerRun = 1000

// autoBuy runs a token hold's auto-buy rule inside the hold's
// transaction (task 3.6, OQ-17), with the wallet and main wallet
// balances already locked: when domain.AutoBuy says so, it buys the
// rule's set amount at the client's price, marks the rule and writes
// autobuy.triggered. A purchase the main wallet can't pay, or one with
// no price, is refused before it writes anything, so the hold carries
// on without it and bought is false.
func (s *Service) autoBuy(ctx context.Context, tx pgx.Tx, req HoldRequest, rule store.LedgerAutoBuyRule, available decimal.Decimal) (bought bool, err error) {
	buy, reason := domain.AutoBuy(domain.AutoBuyRule{
		Enabled: rule.Enabled, BelowTokens: toDecimal(rule.BelowTokens), BuyTokens: toDecimal(rule.BuyTokens),
	}, available, req.Tokens)
	if !buy {
		return false, nil
	}
	purchase, err := s.purchaseInTx(ctx, tx, PurchaseRequest{
		TenantID: req.TenantID, ServiceCode: req.Account.ServiceCode, Currency: req.Account.Currency,
		Tokens:         toDecimal(rule.BuyTokens),
		IdempotencyKey: "autobuy:" + req.ReferenceType + ":" + req.ReferenceID.String(),
	})
	switch {
	case errors.Is(err, ErrInsufficientBalance), errors.Is(err, ErrNoPrice):
		s.log.InfoContext(ctx, "ledger: auto-buy couldn't buy; the hold goes on without it",
			"tenant_id", req.TenantID, "service_code", req.Account.ServiceCode, "error", err)
		return false, nil
	case err != nil:
		return false, err
	}

	q := store.New(tx)
	now := s.clk.Now()
	if err := q.MarkAutoBuyTriggered(ctx, store.MarkAutoBuyTriggeredParams{ID: rule.ID, LastTriggeredAt: pgtype.Timestamptz{Time: now, Valid: true}}); err != nil {
		return false, err
	}
	eventID, err := ids.New()
	if err != nil {
		return false, err
	}
	env, err := platformevents.New(eventID, "autobuy.triggered", 1, req.TenantID, now, "", map[string]any{
		"tenant_id": req.TenantID, "rule_id": rule.ID, "service_code": rule.ServiceCode, "currency": rule.Currency,
		"tokens": toDecimal(rule.BuyTokens).String(), "lot_id": purchase.LotID, "transaction_id": purchase.TransactionID,
		"reason": reason, "hold_reference_type": req.ReferenceType, "hold_reference_id": req.ReferenceID,
	})
	if err != nil {
		return false, fmt.Errorf("app: building autobuy.triggered: %w", err)
	}
	return true, outbox.Write(ctx, tx, "auto_buy_rule", rule.ID, env)
}

// ExpireHolds expires active holds past their expiry plus a grace of
// one settlement window, as of now (task 3.6, OQ-17), and gives back
// what they held. Holds are found across clients as ledger_settlement
// and expired one by one in their tenant's transaction; one that fails
// is logged and retried next run. A capture after expiry is refused
// (BAL-4003): the caller sets expires_at beyond the validity period and
// its delivery reports.
func (s *Service) ExpireHolds(ctx context.Context, now time.Time) (ExpirySummary, error) {
	var sum ExpirySummary
	var holds []store.ListExpiredHoldsRow
	err := s.pool.WithRoleTx(ctx, settlementRole, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		holds, err = store.New(tx).ListExpiredHolds(ctx, store.ListExpiredHoldsParams{Cutoff: now.Add(-s.window), MaxRows: maxExpiredPerRun})
		return err
	})
	if err != nil {
		return sum, fmt.Errorf("app: finding expired holds: %w", err)
	}
	for _, h := range holds {
		expired, err := s.expireHold(ctx, h.TenantID, h.ReferenceType, h.ReferenceID)
		switch {
		case err != nil:
			sum.Failed++
			s.log.Error("ledger: expiring a hold failed; it is retried next run", "tenant_id", h.TenantID, "hold_id", h.ID, "error", err)
		case expired:
			sum.Expired++
		}
	}
	return sum, nil
}

// expireHold expires one hold in its tenant's transaction; false when
// another run, a capture or a release got to it first.
func (s *Service) expireHold(ctx context.Context, tenant uuid.UUID, referenceType string, referenceID uuid.UUID) (bool, error) {
	var expired bool
	err := s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		hold, acct, err := lockHold(ctx, q, referenceType, referenceID)
		if err != nil {
			return err
		}
		change := domain.ExpireHold(domainHold(hold))
		if change.Replay {
			return nil
		}
		if _, err := lockBalances(ctx, q, []uuid.UUID{acct.ID}); err != nil {
			return err
		}
		if _, err := applyHoldChange(ctx, q, hold, change, s.clk.Now()); err != nil {
			return err
		}
		expired = true
		return nil
	})
	return expired, err
}

// SetAutoBuyRule sets the tenant's rule for a service and currency,
// replacing any rule already there (task 3.6).
func (s *Service) SetAutoBuyRule(ctx context.Context, tenant uuid.UUID, r AutoBuyRule) (AutoBuyRule, error) {
	if err := domain.ValidateAutoBuyRule(r.ServiceCode, r.Currency, r.BelowTokens, r.BuyTokens); err != nil {
		return AutoBuyRule{}, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	id, err := ids.New()
	if err != nil {
		return AutoBuyRule{}, err
	}
	var row store.LedgerAutoBuyRule
	err = s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		row, err = store.New(tx).UpsertAutoBuyRule(ctx, store.UpsertAutoBuyRuleParams{
			ID: id, TenantID: tenant, ServiceCode: r.ServiceCode, Currency: r.Currency,
			BelowTokens: toNumeric(r.BelowTokens), BuyTokens: toNumeric(r.BuyTokens), Enabled: r.Enabled,
		})
		return err
	})
	if err != nil {
		return AutoBuyRule{}, fmt.Errorf("app: setting the auto-buy rule: %w", err)
	}
	return autoBuyRule(row), nil
}

// ListAutoBuyRules lists the tenant's rules.
func (s *Service) ListAutoBuyRules(ctx context.Context, tenant uuid.UUID) ([]AutoBuyRule, error) {
	var out []AutoBuyRule
	err := s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := store.New(tx).ListAutoBuyRules(ctx, tenant)
		for _, r := range rows {
			out = append(out, autoBuyRule(r))
		}
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("app: listing auto-buy rules: %w", err)
	}
	return out, nil
}

func autoBuyRule(r store.LedgerAutoBuyRule) AutoBuyRule {
	out := AutoBuyRule{
		ID: r.ID, ServiceCode: r.ServiceCode, Currency: r.Currency,
		BelowTokens: toDecimal(r.BelowTokens), BuyTokens: toDecimal(r.BuyTokens), Enabled: r.Enabled,
	}
	if r.LastTriggeredAt.Valid {
		at := r.LastTriggeredAt.Time
		out.LastTriggeredAt = &at
	}
	return out
}
