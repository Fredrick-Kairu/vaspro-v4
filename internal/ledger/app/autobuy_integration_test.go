//go:build integration

package app_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/app"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/domain"
)

// expire runs the expiry job as of the clock plus d.
func expire(t *testing.T, h harness, d time.Duration) app.ExpirySummary {
	t.Helper()
	sum, err := h.svc.ExpireHolds(context.Background(), h.clk.Now().Add(d))
	if err != nil {
		t.Fatal(err)
	}
	if sum.Failed != 0 {
		t.Fatalf("%d holds failed to expire", sum.Failed)
	}
	return sum
}

func holdStatus(t *testing.T, h harness, ref uuid.UUID) string {
	t.Helper()
	var status string
	if err := h.super.QueryRow(context.Background(), `SELECT status FROM holds WHERE reference_id = $1`, ref).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

// TestExpireHolds (task 3.6, OQ-17): an active hold past its expiry,
// plus a grace of one settlement window, is expired and gives back what
// it held; a captured one is left for settlement.
func TestExpireHolds(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	a, b := uuid.New(), uuid.New()
	fundSMS(t, h, a, 100)
	fundSMS(t, h, b, 100)
	topUp(t, h, a, 1000)
	expiry := h.clk.Now().Add(time.Hour)

	place := func(tenant uuid.UUID, tokens string) app.HoldRequest {
		req := holdReq(tenant, tokens)
		req.ExpiresAt = expiry
		if _, err := h.svc.PlaceHold(ctx, req); err != nil {
			t.Fatal(err)
		}
		return req
	}
	tokenA, tokenB := place(a, "40"), place(b, "30")
	captured := place(a, "10")
	cap := capture(a, captured, "10")
	if _, err := h.svc.CaptureHold(ctx, cap); err != nil {
		t.Fatal(err)
	}
	money := app.HoldRequest{TenantID: a, Account: mainKES, ReferenceType: "payout", ReferenceID: uuid.New(), AmountMinor: 600, ExpiresAt: expiry}
	if _, err := h.svc.PlaceHold(ctx, money); err != nil {
		t.Fatal(err)
	}

	t.Run("within the grace nothing expires", func(t *testing.T) {
		if sum := expire(t, h, time.Hour+app.DefaultSettlementWindow-time.Second); sum.Expired != 0 {
			t.Fatalf("expired %d holds inside the grace", sum.Expired)
		}
	})

	sum := expire(t, h, time.Hour+app.DefaultSettlementWindow+time.Second)
	if sum.Expired != 3 {
		t.Fatalf("expired %d holds, want both clients' token holds and the money hold (3)", sum.Expired)
	}
	for _, ref := range []uuid.UUID{tokenA.ReferenceID, tokenB.ReferenceID, money.ReferenceID} {
		if got := holdStatus(t, h, ref); got != "expired" {
			t.Fatalf("hold %s is %s, want expired", ref, got)
		}
	}
	if got := holdStatus(t, h, captured.ReferenceID); got != "captured" {
		t.Fatalf("the captured hold is %s, want it left for settlement", got)
	}
	if bal := balanceOf(t, h, a, smsKES); !bal.HeldTokens.Equal(tok("10")) || !bal.TokensAvailable.Equal(tok("90")) {
		t.Fatalf("tenant A's SMS wallet %+v: only the captured 10 should still be held", bal)
	}
	if bal := balanceOf(t, h, a, mainKES); bal.HeldMinor != 0 {
		t.Fatalf("main wallet still holds %d", bal.HeldMinor)
	}

	t.Run("an expired hold can't be captured, and releasing it is a replay", func(t *testing.T) {
		_, err := h.svc.CaptureHold(ctx, capture(b, tokenB, "30"))
		wantErr(t, err, domain.ErrHoldState)
		res, err := h.svc.ReleaseHold(ctx, b, tokenB.ReferenceType, tokenB.ReferenceID)
		if err != nil || !res.Replayed {
			t.Fatalf("release: %+v, %v", res, err)
		}
	})

	t.Run("a second run expires nothing more", func(t *testing.T) {
		if sum := expire(t, h, 2*time.Hour); sum.Expired != 0 {
			t.Fatalf("expired %d holds again", sum.Expired)
		}
	})

	t.Run("two runs at once expire each hold once", func(t *testing.T) {
		later := h.clk.Now().Add(time.Hour)
		for range 5 {
			req := holdReq(a, "1")
			req.ExpiresAt = later
			if _, err := h.svc.PlaceHold(ctx, req); err != nil {
				t.Fatal(err)
			}
		}
		var total atomic.Int64
		var wg sync.WaitGroup
		for range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				sum, err := h.svc.ExpireHolds(ctx, later.Add(time.Hour))
				if err != nil || sum.Failed != 0 {
					t.Errorf("run: %+v, %v", sum, err)
				}
				total.Add(int64(sum.Expired))
			}()
		}
		wg.Wait()
		if total.Load() != 5 {
			t.Fatalf("the runs expired %d holds between them, want 5", total.Load())
		}
		if bal := balanceOf(t, h, a, smsKES); !bal.HeldTokens.Equal(tok("10")) {
			t.Fatalf("held %s, want the captured 10", bal.HeldTokens)
		}
	})
}

// TestAutoBuy (task 3.6, OQ-17): a rule buys its set amount inside the
// token hold that needs it, at the client's price.
func TestAutoBuy(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	rule := app.AutoBuyRule{ServiceCode: "sms", Currency: "KES", BelowTokens: tok("100"), BuyTokens: tok("1000"), Enabled: true}

	newClient := func(t *testing.T, tokens int64, mainMinor int64) uuid.UUID {
		t.Helper()
		tenant := uuid.New()
		fundSMS(t, h, tenant, tokens)
		if mainMinor > 0 {
			topUp(t, h, tenant, mainMinor)
		}
		if _, err := h.svc.SetAutoBuyRule(ctx, tenant, rule); err != nil {
			t.Fatal(err)
		}
		return tenant
	}
	lots := func(tenant uuid.UUID) int {
		return count(t, h, `SELECT count(*) FROM token_lots WHERE tenant_id = $1`, tenant)
	}

	t.Run("a hold leaving fewer than the level buys first", func(t *testing.T) {
		tenant := newClient(t, 150, 80000)
		req := holdReq(tenant, "100")
		if _, err := h.svc.PlaceHold(ctx, req); err != nil {
			t.Fatal(err)
		}
		b := balanceOf(t, h, tenant, smsKES)
		if !b.TokensRemaining.Equal(tok("1150")) || !b.TokensAvailable.Equal(tok("1050")) {
			t.Fatalf("wallet %+v, want 1,150 tokens after buying 1,000", b)
		}
		if m := balanceOf(t, h, tenant, mainKES); m.BalanceMinor != 0 {
			t.Fatalf("main wallet %d, want the 800.00 spent", m.BalanceMinor)
		}
		var ruleID uuid.UUID
		if err := h.super.QueryRow(ctx, `SELECT id FROM auto_buy_rules WHERE tenant_id = $1 AND last_triggered_at IS NOT NULL`, tenant).Scan(&ruleID); err != nil {
			t.Fatalf("the rule wasn't marked triggered: %v", err)
		}
		data := assertEventValid(t, h, "autobuy.triggered", ruleID)
		if data["reason"] != domain.AutoBuyBelowLevel || data["tokens"] != "1000" || data["hold_reference_id"] != req.ReferenceID.String() {
			t.Fatalf("autobuy.triggered %v", data)
		}
		assertEventValid(t, h, "tokens.purchased", uuid.MustParse(data["lot_id"].(string)))

		t.Run("the same hold again doesn't buy again", func(t *testing.T) {
			again, err := h.svc.PlaceHold(ctx, req)
			if err != nil || !again.Replayed || lots(tenant) != 2 {
				t.Fatalf("replay: %+v, %v, %d lots", again, err, lots(tenant))
			}
		})
	})

	t.Run("a hold beyond what's available is covered by the purchase", func(t *testing.T) {
		tenant := newClient(t, 150, 80000)
		if _, err := h.svc.PlaceHold(ctx, holdReq(tenant, "900")); err != nil {
			t.Fatal(err)
		}
		if b := balanceOf(t, h, tenant, smsKES); !b.HeldTokens.Equal(tok("900")) || !b.TokensRemaining.Equal(tok("1150")) {
			t.Fatalf("wallet %+v", b)
		}
	})

	t.Run("a hold too large even after buying buys nothing", func(t *testing.T) {
		tenant := newClient(t, 150, 80000)
		_, err := h.svc.PlaceHold(ctx, holdReq(tenant, "1150.000001"))
		wantErr(t, err, app.ErrInsufficientTokens)
		if lots(tenant) != 1 || balanceOf(t, h, tenant, mainKES).BalanceMinor != 80000 {
			t.Fatal("a purchase was made for a hold that was refused")
		}
	})

	t.Run("a main wallet that can't pay: no purchase, the hold goes ahead if it fits", func(t *testing.T) {
		tenant := newClient(t, 150, 79999)
		if _, err := h.svc.PlaceHold(ctx, holdReq(tenant, "100")); err != nil {
			t.Fatalf("a hold that fits without buying: %v", err)
		}
		if lots(tenant) != 1 {
			t.Fatal("a purchase the main wallet couldn't pay was made")
		}
		_, err := h.svc.PlaceHold(ctx, holdReq(tenant, "51"))
		wantErr(t, err, app.ErrInsufficientTokens)
	})

	t.Run("a disabled rule, or no price, buys nothing", func(t *testing.T) {
		tenant := newClient(t, 150, 80000)
		off := rule
		off.Enabled = false
		if _, err := h.svc.SetAutoBuyRule(ctx, tenant, off); err != nil {
			t.Fatal(err)
		}
		if _, err := h.svc.PlaceHold(ctx, holdReq(tenant, "100")); err != nil || lots(tenant) != 1 {
			t.Fatalf("disabled: %v, %d lots", err, lots(tenant))
		}

		priceless := uuid.New()
		ussd := domain.AccountKey{Type: domain.ServiceWallet, ServiceCode: "ussd", Currency: "KES"}
		ussdPrice := seedPrice(t, h, nil, "ussd", "KES", "0", "100", jan1, nil)
		topUp(t, h, priceless, 20000)
		if _, err := h.svc.PurchaseTokens(ctx, purchase(priceless, "ussd", "KES", "150")); err != nil {
			t.Fatal(err)
		}
		if _, err := h.super.Exec(ctx, `UPDATE token_prices SET status = 'withdrawn' WHERE id = $1`, ussdPrice); err != nil {
			t.Fatal(err)
		}
		ussdRule := rule
		ussdRule.ServiceCode = "ussd"
		if _, err := h.svc.SetAutoBuyRule(ctx, priceless, ussdRule); err != nil {
			t.Fatal(err)
		}
		req := holdReq(priceless, "100")
		req.Account = ussd
		if _, err := h.svc.PlaceHold(ctx, req); err != nil || lots(priceless) != 1 {
			t.Fatalf("no price: %v, %d lots", err, lots(priceless))
		}
	})

	// Auto-buy locks the wallet and the main wallet together, in order,
	// like a manual purchase; otherwise the two could deadlock.
	t.Run("holds that auto-buy and manual purchases at once never deadlock", func(t *testing.T) {
		tenant := newClient(t, 20, 4000000)
		var wg sync.WaitGroup
		for i := range 40 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				var err error
				if i%2 == 0 {
					_, err = h.svc.PlaceHold(ctx, holdReq(tenant, "30"))
				} else {
					_, err = h.svc.PurchaseTokens(ctx, purchase(tenant, "sms", "KES", "10"))
				}
				if err != nil && !errors.Is(err, app.ErrInsufficientTokens) {
					t.Errorf("unexpected: %v", err)
				}
			}()
		}
		wg.Wait()
	})

	t.Run("50 holds at once never overspend and buy only as needed", func(t *testing.T) {
		tenant := newClient(t, 150, 160000) // enough for two purchases
		var placed, refused atomic.Int64
		var wg sync.WaitGroup
		for range 50 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := h.svc.PlaceHold(ctx, holdReq(tenant, "50"))
				switch {
				case err == nil:
					placed.Add(1)
				case errors.Is(err, app.ErrInsufficientTokens):
					refused.Add(1)
				default:
					t.Errorf("unexpected: %v", err)
				}
			}()
		}
		wg.Wait()
		b := balanceOf(t, h, tenant, smsKES)
		if lots(tenant) != 3 || !b.TokensRemaining.Equal(tok("2150")) {
			t.Fatalf("%d lots, %s tokens; want 2 purchases (2,150 tokens)", lots(tenant), b.TokensRemaining)
		}
		if b.HeldTokens.GreaterThan(b.TokensRemaining) || !b.HeldTokens.Equal(decimal.NewFromInt(50*placed.Load())) {
			t.Fatalf("held %s for %d holds placed", b.HeldTokens, placed.Load())
		}
		if placed.Load() != 43 || refused.Load() != 7 {
			t.Fatalf("placed %d, refused %d; want 43 (2,150 / 50) and 7", placed.Load(), refused.Load())
		}
	})
}

func TestAutoBuyRules(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	tenant := uuid.New()

	set, err := h.svc.SetAutoBuyRule(ctx, tenant, app.AutoBuyRule{ServiceCode: "sms", Currency: "KES", BelowTokens: tok("100"), BuyTokens: tok("1000"), Enabled: true})
	if err != nil || set.ID == uuid.Nil {
		t.Fatalf("set: %+v, %v", set, err)
	}
	changed, err := h.svc.SetAutoBuyRule(ctx, tenant, app.AutoBuyRule{ServiceCode: "sms", Currency: "KES", BelowTokens: tok("0"), BuyTokens: tok("2.5"), Enabled: false})
	if err != nil || changed.ID != set.ID || changed.Enabled || !changed.BuyTokens.Equal(tok("2.5")) {
		t.Fatalf("changing it: %+v, %v; want the same rule changed", changed, err)
	}
	rules, err := h.svc.ListAutoBuyRules(ctx, tenant)
	if err != nil || len(rules) != 1 {
		t.Fatalf("list: %+v, %v", rules, err)
	}
	if other, err := h.svc.ListAutoBuyRules(ctx, uuid.New()); err != nil || len(other) != 0 {
		t.Fatalf("another tenant's list: %+v, %v", other, err)
	}

	for name, r := range map[string]app.AutoBuyRule{
		"buying nothing":      {ServiceCode: "sms", Currency: "KES", BelowTokens: tok("1"), BuyTokens: tok("0"), Enabled: true},
		"a negative level":    {ServiceCode: "sms", Currency: "KES", BelowTokens: tok("-1"), BuyTokens: tok("1"), Enabled: true},
		"too many decimals":   {ServiceCode: "sms", Currency: "KES", BelowTokens: tok("1"), BuyTokens: tok("0.0000001"), Enabled: true},
		"a bad service code":  {ServiceCode: "SMS", Currency: "KES", BelowTokens: tok("1"), BuyTokens: tok("1"), Enabled: true},
		"an unknown currency": {ServiceCode: "sms", Currency: "kes", BelowTokens: tok("1"), BuyTokens: tok("1"), Enabled: true},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := h.svc.SetAutoBuyRule(ctx, tenant, r)
			wantErr(t, err, app.ErrValidation)
		})
	}
}
