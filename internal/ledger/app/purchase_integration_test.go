//go:build integration

package app_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/app"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/domain"
)

type lotRow struct {
	bought, remaining, price string
	priceID                  uuid.UUID
}

func lotsOf(t *testing.T, h harness, tenant uuid.UUID) []lotRow {
	t.Helper()
	rows, err := h.super.Query(context.Background(), `
		SELECT tokens_bought::text, tokens_remaining::text, minor_per_token::text, token_price_id
		FROM token_lots WHERE tenant_id = $1 ORDER BY purchased_at, id`, tenant)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (lotRow, error) {
		var l lotRow
		err := r.Scan(&l.bought, &l.remaining, &l.price, &l.priceID)
		return l, err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestPurchaseTokens_WorkedExample: data model §5 steps 1, 2 and 5. A
// client with a KES 0.80 deal tops up 13,000.00 and buys 10,000 SMS
// units (8,000.00, lot A). A new deal of 0.50 starts on 1 Nov; the next
// 10,000 cost 5,000.00 (lot B), and lot A keeps its 0.80.
func TestPurchaseTokens_WorkedExample(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	tenant := uuid.New()
	nov1 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	oldDeal := seedPrice(t, h, &tenant, "sms", "KES", "0", "80", jan1, &nov1)
	newDeal := seedPrice(t, h, &tenant, "sms", "KES", "0", "50", nov1, nil)

	h.clk.Set(time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC))
	topUp(t, h, tenant, 1300000)
	first, err := h.svc.PurchaseTokens(ctx, purchase(tenant, "sms", "KES", "10000"))
	if err != nil {
		t.Fatal(err)
	}
	if first.CostMinor != 800000 || first.TokenPriceID != oldDeal || !first.MinorPerToken.Equal(tok("80")) {
		t.Fatalf("first purchase %+v, want 8,000.00 at the 0.80 deal", first)
	}

	h.clk.Set(testNow)
	second, err := h.svc.PurchaseTokens(ctx, purchase(tenant, "sms", "KES", "10000"))
	if err != nil {
		t.Fatal(err)
	}
	if second.CostMinor != 500000 || second.TokenPriceID != newDeal {
		t.Fatalf("second purchase %+v, want 5,000.00 at the 0.50 deal", second)
	}

	lots := lotsOf(t, h, tenant)
	if len(lots) != 2 || lots[0].price != "80.000000" || lots[1].price != "50.000000" ||
		lots[0].remaining != "10000.000000" || lots[0].priceID != oldDeal {
		t.Fatalf("lots %+v, want lot A at 80 then lot B at 50", lots)
	}
	b := balanceOf(t, h, tenant, smsKES)
	if b.BalanceMinor != 1300000 || !b.TokensRemaining.Equal(tok("20000")) {
		t.Fatalf("SMS wallet %+v, want 13,000.00 and 20,000 tokens", b)
	}
	if m := balanceOf(t, h, tenant, mainKES); m.BalanceMinor != 0 {
		t.Fatalf("main wallet %d, want 0", m.BalanceMinor)
	}

	data := assertEventValid(t, h, "tokens.purchased", second.LotID)
	if data["tokens"] != "10000" || data["minor_per_token"] != "50" || data["cost_minor"].(float64) != 500000 {
		t.Fatalf("tokens.purchased = %v", data)
	}
	var entryTokens string
	if err := h.super.QueryRow(ctx, `SELECT tokens::text FROM entries WHERE transaction_id = $1 AND tokens IS NOT NULL`, second.TransactionID).Scan(&entryTokens); err != nil || entryTokens != "10000.000000" {
		t.Fatalf("the service wallet line carries %q tokens (%v), want 10000", entryTokens, err)
	}

	t.Run("a replay after the price changed returns the original lot", func(t *testing.T) {
		req := purchase(tenant, "sms", "KES", "10")
		topUp(t, h, tenant, 500)
		res, err := h.svc.PurchaseTokens(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		seedPrice(t, h, nil, "whatsapp", "KES", "0", "1", jan1, nil) // an unrelated change
		if _, err := h.super.Exec(ctx, `UPDATE token_prices SET status = 'withdrawn' WHERE id = $1`, newDeal); err != nil {
			t.Fatal(err)
		}
		again, err := h.svc.PurchaseTokens(ctx, req)
		if err != nil || !again.Replayed || again.LotID != res.LotID || again.CostMinor != 500 || again.TokenPriceID != newDeal {
			t.Fatalf("replay %+v, %v; want the original lot at 0.50", again, err)
		}
	})
}

func TestPurchaseTokens_Pricing(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	ussd := domain.AccountKey{Type: domain.ServiceWallet, ServiceCode: "ussd", Currency: "KES"}
	list := seedPrice(t, h, nil, "ussd", "KES", "0", "100", jan1, nil)
	listBulk := seedPrice(t, h, nil, "ussd", "KES", "100000", "90", jan1, nil)

	buy := func(tenant uuid.UUID, tokens string) (app.PurchaseResult, error) {
		return h.svc.PurchaseTokens(ctx, purchase(tenant, "ussd", "KES", tokens))
	}

	t.Run("without a deal, the list tier matching the quantity", func(t *testing.T) {
		tenant := uuid.New()
		topUp(t, h, tenant, 100000000)
		small, err := buy(tenant, "99999")
		if err != nil || small.TokenPriceID != list || small.CostMinor != 9999900 {
			t.Fatalf("99,999 tokens: %+v, %v", small, err)
		}
		bulk, err := buy(tenant, "100000")
		if err != nil || bulk.TokenPriceID != listBulk || bulk.CostMinor != 9000000 {
			t.Fatalf("100,000 tokens: %+v, %v", bulk, err)
		}
	})

	t.Run("a deal in force always gets the deal price", func(t *testing.T) {
		tenant := uuid.New()
		deal := seedPrice(t, h, &tenant, "ussd", "KES", "0", "95", jan1, nil)
		topUp(t, h, tenant, 100000000)
		res, err := buy(tenant, "150000")
		if err != nil || res.TokenPriceID != deal {
			t.Fatalf("got %+v, %v; want the deal even above the list's bulk tier", res, err)
		}
	})

	t.Run("an ended deal falls back to the list", func(t *testing.T) {
		tenant := uuid.New()
		ended := time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC)
		seedPrice(t, h, &tenant, "ussd", "KES", "0", "10", jan1, &ended)
		topUp(t, h, tenant, 10000)
		res, err := buy(tenant, "10")
		if err != nil || res.TokenPriceID != list {
			t.Fatalf("got %+v, %v; want the list price", res, err)
		}
	})

	t.Run("an unapproved deal is never used", func(t *testing.T) {
		tenant := uuid.New()
		if _, err := h.super.Exec(ctx, `
			INSERT INTO token_prices (id, tenant_id, service_code, currency, minor_per_token, deal_reference, status, effective_from, created_by)
			VALUES ($1, $2, 'ussd', 'KES', 1, 'q-1', 'pending_approval', $3, $4)`, uuid.New(), tenant, jan1, uuid.New()); err != nil {
			t.Fatal(err)
		}
		topUp(t, h, tenant, 10000)
		res, err := buy(tenant, "10")
		if err != nil || res.TokenPriceID != list {
			t.Fatalf("got %+v, %v; want the list price", res, err)
		}
	})

	t.Run("lots keep their price after it changes", func(t *testing.T) {
		tenant := uuid.New()
		deal := seedPrice(t, h, &tenant, "ussd", "KES", "0", "70", jan1, nil)
		topUp(t, h, tenant, 100000)
		if _, err := buy(tenant, "100"); err != nil {
			t.Fatal(err)
		}
		if _, err := h.super.Exec(ctx, `UPDATE token_prices SET status = 'withdrawn' WHERE id = $1`, deal); err != nil {
			t.Fatal(err)
		}
		if _, err := buy(tenant, "100"); err != nil {
			t.Fatal(err)
		}
		lots := lotsOf(t, h, tenant)
		if len(lots) != 2 || lots[0].price != "70.000000" || lots[1].price != "100.000000" {
			t.Fatalf("lots %+v, want 70 kept and the new one at the list's 100", lots)
		}
		if b := balanceOf(t, h, tenant, ussd); b.BalanceMinor != 17000 {
			t.Fatalf("USSD wallet %d, want 17000", b.BalanceMinor)
		}
	})

	t.Run("fractional tokens at a fractional price round half-up", func(t *testing.T) {
		tenant := uuid.New()
		seedPrice(t, h, &tenant, "ussd", "KES", "0", "40.2", jan1, nil)
		topUp(t, h, tenant, 1000)
		res, err := buy(tenant, "2.5")
		if err != nil || res.CostMinor != 101 {
			t.Fatalf("2.5 tokens at 40.2: %+v, %v; want 101 (100.5 up)", res, err)
		}
	})

	t.Run("UGX costs whole shillings", func(t *testing.T) {
		tenant := uuid.New()
		if _, err := h.svc.Post(ctx, post(tenant, domain.TxnTopup,
			d(domain.AccountKey{Type: domain.ProviderClearing, Rail: "mtn_ug", Currency: "UGX"}, 1000),
			c(domain.AccountKey{Type: domain.MainWallet, Currency: "UGX"}, 1000))); err != nil {
			t.Fatal(err)
		}
		res, err := h.svc.PurchaseTokens(ctx, purchase(tenant, "sms", "UGX", "7"))
		if err != nil || res.CostMinor != 88 {
			t.Fatalf("7 tokens at UGX 12.5: %+v, %v; want 88 (87.5 up)", res, err)
		}
	})

	t.Run("refusals", func(t *testing.T) {
		tenant := uuid.New()
		topUp(t, h, tenant, 1000)

		_, err := h.svc.PurchaseTokens(ctx, purchase(tenant, "whatsapp", "KES", "1"))
		wantErr(t, err, app.ErrNoPrice)

		_, err = buy(tenant, "10.01")
		wantErr(t, err, app.ErrInsufficientBalance)

		_, err = buy(tenant, "0.004") // 0.4 minor rounds to nothing
		wantErr(t, err, app.ErrValidation)

		req := purchase(tenant, "ussd", "KES", "1")
		if _, err := h.svc.PurchaseTokens(ctx, req); err != nil {
			t.Fatal(err)
		}
		req.Tokens = tok("2")
		_, err = h.svc.PurchaseTokens(ctx, req)
		wantErr(t, err, app.ErrIdempotencyConflict)

		if n := count(t, h, `SELECT count(*) FROM token_lots WHERE tenant_id = $1`, tenant); n != 1 {
			t.Fatalf("%d lots after the refusals, want 1", n)
		}
	})
}
