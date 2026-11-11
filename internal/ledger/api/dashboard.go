package api

import (
	"context"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/problem"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/app"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/domain"
)

// The dashboard's wallet and token routes (task 3.9, OQ-20). A tenant
// or account ID that isn't a UUID names nothing: uuid.Nil goes on, so
// the access check answers it as not found.

func (h *Handlers) ListBalances(ctx context.Context, request ListBalancesRequestObject) (ListBalancesResponseObject, error) {
	bals, err := h.app.TenantBalances(ctx, callerFromContext(ctx), parseOrNil(request.TenantId))
	if err != nil {
		return nil, toProblem(err)
	}
	out := ListBalances200JSONResponse{Balances: make([]Balance, len(bals))}
	for i, b := range bals {
		out.Balances[i] = balanceResponse(b)
	}
	return out, nil
}

func (h *Handlers) GetStatement(ctx context.Context, request GetStatementRequestObject) (GetStatementResponseObject, error) {
	p := request.Params
	q := app.StatementQuery{AccountID: parseOrNil(request.AccountId), From: p.From, To: p.To}
	if p.Cursor != nil {
		q.Cursor = *p.Cursor
	}
	if p.Limit != nil {
		q.Limit = *p.Limit
	}
	st, err := h.app.Statement(ctx, callerFromContext(ctx), parseOrNil(request.TenantId), q)
	if err != nil {
		return nil, toProblem(err)
	}
	out := GetStatement200JSONResponse{
		AccountId: st.AccountID.String(), Type: string(st.Account.Type), Currency: st.Account.Currency,
		ServiceCode: nonEmpty(st.Account.ServiceCode), Lines: make([]StatementLine, len(st.Lines)),
	}
	for i, l := range st.Lines {
		line := StatementLine{
			EntryId: l.EntryID, TransactionId: l.TransactionID.String(), PostedAt: l.PostedAt, Type: string(l.Type),
			Description: l.Description, ReferenceType: nonEmpty(l.ReferenceType), ReferenceId: idString(l.ReferenceID),
			Direction: Credit, AmountMinor: l.AmountMinor, BalanceAfterMinor: l.BalanceAfterMinor,
			Tokens: decimalString(l.Tokens), TokensAfter: decimalString(l.TokensAfter),
		}
		if l.Direction == domain.Debit {
			line.Direction = Debit
		}
		out.Lines[i] = line
	}
	if st.NextCursor != "" {
		out.NextCursor = &st.NextCursor
	}
	return out, nil
}

func (h *Handlers) ListTenantTokenPrices(ctx context.Context, request ListTenantTokenPricesRequestObject) (ListTenantTokenPricesResponseObject, error) {
	prices, err := h.app.TenantPrices(ctx, callerFromContext(ctx), parseOrNil(request.TenantId))
	if err != nil {
		return nil, toProblem(err)
	}
	out := ListTenantTokenPrices200JSONResponse{Prices: make([]TenantTokenPrice, len(prices))}
	for i, p := range prices {
		out.Prices[i] = TenantTokenPrice{
			ServiceCode: p.ServiceCode, Currency: p.Currency, Source: TenantTokenPriceSource(p.Source),
			MinTokens: p.MinTokens.String(), MinorPerToken: p.MinorPerToken.String(),
			DealReference: nonEmpty(p.DealReference), EffectiveTo: p.EffectiveTo,
		}
	}
	return out, nil
}

func (h *Handlers) QuoteTokenPurchase(ctx context.Context, request QuoteTokenPurchaseRequestObject) (QuoteTokenPurchaseResponseObject, error) {
	p := request.Params
	tokens, err := decimal.NewFromString(p.Tokens)
	if err != nil {
		return nil, problem.New(problem.VAL4000, "tokens must be a decimal")
	}
	q, err := h.app.QuoteTokens(ctx, callerFromContext(ctx), parseOrNil(request.TenantId), p.ServiceCode, p.Currency, tokens)
	if err != nil {
		return nil, toProblem(err)
	}
	return QuoteTokenPurchase200JSONResponse{
		ServiceCode: q.ServiceCode, Currency: q.Currency, Tokens: q.Tokens.String(), Source: TokenQuoteSource(q.Source),
		MinTokens: q.MinTokens.String(), MinorPerToken: q.MinorPerToken.String(), CostMinor: q.CostMinor,
		MainAvailableMinor: q.MainAvailableMinor, Affordable: q.Affordable,
	}, nil
}

func (h *Handlers) CreateTokenPurchase(ctx context.Context, request CreateTokenPurchaseRequestObject) (CreateTokenPurchaseResponseObject, error) {
	b := request.Body
	tokens, err := decimal.NewFromString(b.Tokens)
	if err != nil {
		return nil, problem.New(problem.VAL4000, "tokens must be a decimal")
	}
	tenant := parseOrNil(request.TenantId)
	res, err := h.app.BuyTokens(ctx, callerFromContext(ctx), app.PurchaseRequest{
		TenantID: tenant, ServiceCode: b.ServiceCode, Currency: b.Currency, Tokens: tokens,
		// The ledger key follows the client's: a retry with the same key
		// is the same purchase even if the HTTP replay has expired.
		IdempotencyKey: "dashboard:" + request.Params.IdempotencyKey,
	})
	if err != nil {
		return nil, toProblem(err)
	}
	return CreateTokenPurchase201JSONResponse{
		TransactionId: res.TransactionID.String(), LotId: res.LotID.String(), ServiceCode: b.ServiceCode, Currency: b.Currency,
		Tokens: tokens.String(), MinorPerToken: res.MinorPerToken.String(), CostMinor: res.CostMinor,
	}, nil
}

func (h *Handlers) ListTenantAutoBuyRules(ctx context.Context, request ListTenantAutoBuyRulesRequestObject) (ListTenantAutoBuyRulesResponseObject, error) {
	rules, err := h.app.TenantAutoBuyRules(ctx, callerFromContext(ctx), parseOrNil(request.TenantId))
	if err != nil {
		return nil, toProblem(err)
	}
	out := ListTenantAutoBuyRules200JSONResponse{Rules: make([]AutoBuyRule, len(rules))}
	for i, r := range rules {
		out.Rules[i] = autoBuyRuleResponse(r)
	}
	return out, nil
}

func (h *Handlers) SetTenantAutoBuyRule(ctx context.Context, request SetTenantAutoBuyRuleRequestObject) (SetTenantAutoBuyRuleResponseObject, error) {
	b := request.Body
	below, err1 := decimal.NewFromString(b.BelowTokens)
	buy, err2 := decimal.NewFromString(b.BuyTokens)
	if err1 != nil || err2 != nil {
		return nil, problem.New(problem.VAL4000, "below_tokens and buy_tokens must be decimals")
	}
	rule, err := h.app.SetTenantAutoBuyRule(ctx, callerFromContext(ctx), parseOrNil(request.TenantId), app.AutoBuyRule{
		ServiceCode: request.ServiceCode, Currency: request.Currency, BelowTokens: below, BuyTokens: buy, Enabled: b.Enabled,
	})
	if err != nil {
		return nil, toProblem(err)
	}
	return SetTenantAutoBuyRule200JSONResponse(autoBuyRuleResponse(rule)), nil
}

func (h *Handlers) CreateDevTopUp(ctx context.Context, request CreateDevTopUpRequestObject) (CreateDevTopUpResponseObject, error) {
	b := request.Body
	tenant, err := uuid.Parse(b.TenantId)
	if err != nil {
		return nil, problem.New(problem.VAL4000, "tenant_id must be a UUID")
	}
	res, err := h.app.DevTopUp(ctx, callerFromContext(ctx), app.DevTopUpRequest{
		TenantID: tenant, Currency: b.Currency, AmountMinor: b.AmountMinor, IdempotencyKey: request.Params.IdempotencyKey,
	})
	if err != nil {
		return nil, toProblem(err)
	}
	return CreateDevTopUp201JSONResponse{TransactionId: res.TransactionID.String()}, nil
}

func balanceResponse(b app.Balance) Balance {
	out := Balance{
		AccountId: b.AccountID.String(), Type: BalanceType(b.Account.Type), Currency: b.Account.Currency, Status: b.Status,
		ServiceCode: nonEmpty(b.Account.ServiceCode), BalanceMinor: b.BalanceMinor, HeldMinor: b.HeldMinor, AvailableMinor: b.AvailableMinor,
	}
	if b.Account.Type == domain.ServiceWallet {
		out.TokensRemaining = decimalString(&b.TokensRemaining)
		out.HeldTokens = decimalString(&b.HeldTokens)
		out.TokensAvailable = decimalString(&b.TokensAvailable)
	}
	return out
}

func autoBuyRuleResponse(r app.AutoBuyRule) AutoBuyRule {
	return AutoBuyRule{
		Id: r.ID.String(), ServiceCode: r.ServiceCode, Currency: r.Currency, BelowTokens: r.BelowTokens.String(),
		BuyTokens: r.BuyTokens.String(), Enabled: r.Enabled, LastTriggeredAt: r.LastTriggeredAt,
	}
}

func decimalString(d *decimal.Decimal) *string {
	if d == nil {
		return nil
	}
	s := d.String()
	return &s
}
