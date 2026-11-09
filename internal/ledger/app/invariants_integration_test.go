//go:build integration

package app_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/app"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/domain"
)

// TestHolds_200ConcurrentHoldsNeverOverspend is Phase 3's concurrency
// test: 200 goroutines each hold one SMS unit against a wallet with
// 100. Exactly 100 succeed, the rest are refused for balance, and
// held_tokens is exactly what was placed. Run with -race.
func TestHolds_200ConcurrentHoldsNeverOverspend(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	tenant := uuid.New()
	fundSMS(t, h, tenant, 100)

	var placed, refused atomic.Int64
	var wg sync.WaitGroup
	for range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := h.svc.PlaceHold(ctx, holdReq(tenant, "1"))
			switch {
			case err == nil:
				placed.Add(1)
			case errors.Is(err, app.ErrInsufficientTokens):
				refused.Add(1)
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()

	if placed.Load() != 100 || refused.Load() != 100 {
		t.Fatalf("placed %d, refused %d; want 100 and 100", placed.Load(), refused.Load())
	}
	b := balanceOf(t, h, tenant, smsKES)
	if !b.HeldTokens.Equal(tok("100")) || !b.TokensAvailable.IsZero() {
		t.Fatalf("wallet %+v, want all 100 units held", b)
	}
	if n := count(t, h, `SELECT count(*) FROM holds WHERE status = 'active'`); n != 100 {
		t.Fatalf("%d active holds, want 100", n)
	}
}

// TestLedger_RandomOperationsKeepInvariants is Phase 3's property test:
// a seeded random sequence of top-ups, token purchases (at a list price
// and, for one tenant, a deal), main-wallet charges, token holds, money
// holds, captures (money holds with and without a posting) and releases
// across tenants and two currencies. After every step the invariants in
// checkInvariants hold. LEDGER_PROPERTY_SEED reruns a failing seed.
func TestLedger_RandomOperationsKeepInvariants(t *testing.T) {
	seed := uint64(time.Now().UnixNano())
	if s := os.Getenv("LEDGER_PROPERTY_SEED"); s != "" {
		v, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		seed = v
	}
	t.Logf("seed %d (rerun with LEDGER_PROPERTY_SEED=%d)", seed, seed)
	rng := rand.New(rand.NewPCG(seed, seed)) //nolint:gosec // a reproducible test sequence, not a secret

	ctx := context.Background()
	h := newHarness(t)
	tenants := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}
	seedPrice(t, h, &tenants[0], "sms", "KES", "0", "50.5", jan1, nil) // one tenant buys at a deal
	// Auto-buy is on for every tenant in both currencies (task 3.6), so
	// token holds often buy first; one rule is off, so some don't.
	for i, tenant := range tenants {
		for _, cur := range []string{"KES", "UGX"} {
			if _, err := h.svc.SetAutoBuyRule(ctx, tenant, app.AutoBuyRule{ServiceCode: "sms", Currency: cur,
				BelowTokens: tok("20"), BuyTokens: tok("25"), Enabled: i != 2 || cur != "KES"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	currencies := []struct {
		code  string
		clear domain.AccountKey
	}{
		{"KES", mpesaKES},
		{"UGX", domain.AccountKey{Type: domain.ProviderClearing, Rail: "mtn_ug", Currency: "UGX"}},
	}
	type openHold struct {
		tenant uuid.UUID
		ref    uuid.UUID
		tokens decimal.Decimal // zero for a money hold
		amount int64
		wallet domain.AccountKey
	}
	var active []openHold
	// Top-ups and pending pay-backs, for the reversal steps (task 3.7).
	type topUpRef struct {
		tenant   uuid.UUID
		id       uuid.UUID
		amount   int64
		currency string
	}
	var topUps []topUpRef
	var pendingPaybacks []app.Reversal
	reversed := map[uuid.UUID]int64{} // what each top-up's live reversals take
	maker, lead := newStaff(h), financeLead(h)
	// Top-ups are larger than spends, so wallets have money and the
	// hold, capture and release paths are reached often.
	const steps = 400
	succeeded, refused := map[string]int{}, map[string]int{}

	for step := range steps {
		tenant := tenants[rng.IntN(len(tenants))]
		cur := currencies[rng.IntN(len(currencies))]
		main := domain.AccountKey{Type: domain.MainWallet, Currency: cur.code}
		sms := domain.AccountKey{Type: domain.ServiceWallet, ServiceCode: "sms", Currency: cur.code}
		revenue := domain.AccountKey{Type: domain.Revenue, ServiceCode: "sms", Currency: cur.code}
		vat := domain.AccountKey{Type: domain.TaxPayable, Currency: cur.code}
		amount := int64(rng.IntN(2000) + 1)
		// Up to 25 tokens with up to 2 decimals: fractional purchases
		// and holds at fractional prices.
		tokens := decimal.New(int64(rng.IntN(2500)+1), -2)

		var op string
		var err error
		switch rng.IntN(13) {
		case 0, 1:
			op = "top-up"
			amount *= 10
			var res app.PostResult
			res, err = h.svc.Post(ctx, post(tenant, domain.TxnTopup, d(cur.clear, amount), c(main, amount)))
			if err == nil {
				topUps = append(topUps, topUpRef{tenant: tenant, id: res.TransactionID, amount: amount, currency: cur.code})
			}
		case 11:
			// A reversal of part of a top-up, sometimes clawing tokens
			// back, approved at once; one that isn't covered is rejected.
			if len(topUps) == 0 {
				continue
			}
			op = "reversal"
			top := topUps[rng.IntN(len(topUps))]
			kind := []domain.ReversalKind{domain.ReversalPayback, domain.ReversalChargeback, domain.ReversalOperator}[rng.IntN(3)]
			left := top.amount - reversed[top.id]
			if left <= 0 {
				continue
			}
			reverse := rng.Int64N(left) + 1
			var services []string
			if rng.IntN(2) == 0 {
				// The client turned most of its main wallet into tokens,
				// then the top-up is reversed: aim just past what the main
				// wallet has left, so the SMS wallet's tokens are clawed back.
				services = []string{"sms"}
				mainKey := domain.AccountKey{Type: domain.MainWallet, Currency: top.currency}
				perToken := map[string]int64{"KES": 81, "UGX": 13}[top.currency] // just over each list price
				if spend := balanceOf(t, h, top.tenant, mainKey).AvailableMinor / perToken; spend > 0 {
					if _, perr := h.svc.PurchaseTokens(ctx, purchase(top.tenant, "sms", top.currency, decimal.NewFromInt(spend).String())); perr != nil {
						t.Fatalf("seed %d step %d: spending the main wallet: %v", seed, step, perr)
					}
				}
				reverse = min(left, max(balanceOf(t, h, top.tenant, mainKey).AvailableMinor, 0)+rng.Int64N(1500)+1)
			}
			var requested, approved app.Reversal
			if requested, err = h.svc.RequestReversal(ctx, maker.user, maker.session, reversalOf(top.tenant, top.id, kind, reverse, services...)); err == nil {
				reversed[top.id] += reverse
				approved, err = h.svc.ApproveReversal(ctx, lead.user, lead.session, requested.ID)
				switch {
				case errors.Is(err, domain.ErrReversalNotCovered):
					if _, rerr := h.svc.RejectReversal(ctx, lead.user, lead.session, requested.ID, "not covered"); rerr != nil {
						t.Fatalf("seed %d step %d: rejecting: %v", seed, step, rerr)
					}
					reversed[top.id] -= reverse
				case err == nil && approved.Status == domain.ReversalPaybackPending:
					pendingPaybacks = append(pendingPaybacks, approved)
				}
			}
		case 12:
			if len(pendingPaybacks) == 0 {
				continue
			}
			op = "pay-back"
			i := rng.IntN(len(pendingPaybacks))
			r := pendingPaybacks[i]
			ok := rng.IntN(3) != 0
			if ok {
				pendingPaybacks = append(pendingPaybacks[:i], pendingPaybacks[i+1:]...)
			}
			_, err = h.svc.RecordPayback(ctx, app.PaybackReport{TenantID: r.TenantID, ReversalID: r.ID, Succeeded: ok, Error: "retry"})
		case 2:
			op = "token purchase"
			_, err = h.svc.PurchaseTokens(ctx, purchase(tenant, "sms", cur.code, tokens.String()))
		case 3:
			op = "main-wallet charge"
			lines := []domain.Line{d(main, amount)}
			if tax := amount * 16 / 116; tax > 0 { // a tiny charge has no VAT line, not a zero one
				lines = append(lines, c(revenue, amount-tax), c(vat, tax))
			} else {
				lines = append(lines, c(revenue, amount))
			}
			_, err = h.svc.Post(ctx, post(tenant, domain.TxnAdjustment, lines...))
		case 4:
			op = "token hold"
			ref := uuid.New()
			_, err = h.svc.PlaceHold(ctx, app.HoldRequest{
				TenantID: tenant, Account: sms, ReferenceType: "campaign", ReferenceID: ref,
				Tokens: tokens, ExpiresAt: h.clk.Now().Add(time.Hour),
			})
			if err == nil {
				active = append(active, openHold{tenant: tenant, ref: ref, tokens: tokens, wallet: sms})
			}
		case 5:
			op = "money hold"
			ref := uuid.New()
			_, err = h.svc.PlaceHold(ctx, app.HoldRequest{
				TenantID: tenant, Account: main, ReferenceType: "payout", ReferenceID: ref,
				AmountMinor: amount, ExpiresAt: h.clk.Now().Add(time.Hour),
			})
			if err == nil {
				active = append(active, openHold{tenant: tenant, ref: ref, amount: amount, wallet: main})
			}
		case 10:
			// Jump past every open hold's expiry plus the grace (task 3.6).
			op = "expiry"
			h.clk.Set(h.clk.Now().Add(time.Hour + app.DefaultSettlementWindow + time.Second))
			var sum app.ExpirySummary
			sum, err = h.svc.ExpireHolds(ctx, h.clk.Now())
			if err == nil && sum.Failed > 0 {
				err = fmt.Errorf("%d holds failed to expire", sum.Failed)
			}
			active = nil // every hold still open has expired
		case 9:
			op = "settlement"
			_, end := domain.WindowFor(h.clk.Now(), app.DefaultSettlementWindow)
			end = end.Add(app.DefaultSettlementWindow)
			var sum app.SettlementSummary
			sum, err = h.svc.SettleUsage(ctx, end)
			if err == nil && sum.Failed > 0 {
				err = fmt.Errorf("%d wallets failed to settle", sum.Failed)
			}
			h.clk.Set(end.Add(time.Second))
		default:
			if len(active) == 0 {
				continue
			}
			i := rng.IntN(len(active))
			hd := active[i]
			active = append(active[:i], active[i+1:]...)
			refType := "payout"
			if hd.wallet.Type == domain.ServiceWallet {
				refType = "campaign"
			}
			switch choice := rng.IntN(3); {
			case choice == 0:
				op = "release"
				_, err = h.svc.ReleaseHold(ctx, hd.tenant, refType, hd.ref)
			case hd.wallet.Type == domain.ServiceWallet:
				op = "token capture"
				used := decimal.New(rng.Int64N(hd.tokens.Shift(2).IntPart())+1, -2)
				_, err = h.svc.CaptureHold(ctx, app.CaptureRequest{TenantID: hd.tenant, ReferenceType: refType, ReferenceID: hd.ref, CapturedTokens: used})
			default:
				op = "money capture with posting"
				used := rng.Int64N(hd.amount) + 1
				rev := domain.AccountKey{Type: domain.Revenue, ServiceCode: "payments", Currency: hd.wallet.Currency}
				p := post(hd.tenant, domain.TxnUsage, d(hd.wallet, used), c(rev, used))
				_, err = h.svc.CaptureHold(ctx, app.CaptureRequest{TenantID: hd.tenant, ReferenceType: refType, ReferenceID: hd.ref, CapturedMinor: used, Posting: &p})
			}
		}
		switch {
		case err == nil:
			succeeded[op]++
		case errors.Is(err, app.ErrInsufficientBalance), errors.Is(err, app.ErrInsufficientTokens):
			refused[op]++
		case op == "reversal" && (errors.Is(err, domain.ErrReversalExceedsOriginal) || errors.Is(err, domain.ErrReversalNotCovered)):
			refused[op]++
		case op == "token purchase" && errors.Is(err, app.ErrValidation):
			// A tiny quantity can cost under one minor unit (0.02 UGX tokens
			// at 12.5 is 0.25 shillings), which a purchase rightly refuses.
			refused[op]++
		default:
			t.Fatalf("seed %d step %d %s: %v", seed, step, op, err)
		}
		if msg := checkInvariants(ctx, h); msg != "" {
			t.Fatalf("seed %d step %d after %s: %s", seed, step, op, msg)
		}
	}
	batches := count(t, h, `SELECT count(*) FROM usage_batches WHERE transaction_id IS NOT NULL`)
	autoBuys := count(t, h, `SELECT count(*) FROM outbox_events WHERE event_type = 'autobuy.triggered'`)
	expired := count(t, h, `SELECT count(*) FROM holds WHERE status = 'expired'`)
	clawbacks := count(t, h, `SELECT count(DISTINCT lc.transaction_id) FROM lot_consumptions lc JOIN transactions t ON t.id = lc.transaction_id WHERE t.type = 'reversal'`)
	t.Logf("succeeded %v; refused for balance %v; %d usage batches, %d auto-buys, %d holds expired, %d reversals clawed tokens back",
		succeeded, refused, batches, autoBuys, expired, clawbacks)
	if clawbacks == 0 {
		t.Error("no reversal clawed tokens back: the sequence isn't exercising it")
	}
	if autoBuys == 0 || expired == 0 {
		t.Errorf("%d auto-buys and %d expiries: the sequence isn't exercising both", autoBuys, expired)
	}
	if batches == 0 {
		t.Error("no settlement posted a usage batch: the sequence isn't exercising it")
	}
	for _, op := range []string{"top-up", "token purchase", "main-wallet charge", "token hold", "money hold",
		"release", "token capture", "money capture with posting", "settlement", "reversal", "pay-back"} {
		if succeeded[op] == 0 {
			t.Errorf("no %s succeeded in %d steps: the sequence isn't exercising it", op, steps)
		}
	}
}

// checkInvariants returns "" when the ledger holds together, or what
// broke.
func checkInvariants(ctx context.Context, h harness) string {
	var n int
	queries := []struct {
		what, sql string
	}{
		{"a currency whose debits and credits differ", `
			SELECT count(*) FROM (
			  SELECT currency FROM entries GROUP BY currency
			  HAVING sum(CASE direction WHEN 'D' THEN amount_minor ELSE -amount_minor END) <> 0) x`},
		{"a balance that differs from the replay of its entries", `
			SELECT count(*) FROM balances b JOIN accounts a ON a.id = b.account_id
			WHERE b.balance_minor <> (
			  SELECT coalesce(sum(CASE WHEN e.direction = ledger.normal_side(a.type) THEN e.amount_minor ELSE -e.amount_minor END), 0)
			  FROM entries e WHERE e.account_id = b.account_id)`},
		{"held_minor that differs from its money holds", `
			SELECT count(*) FROM balances b
			WHERE b.held_minor <> (
			  SELECT coalesce(sum(CASE
			    WHEN h.status = 'active' THEN h.amount_minor
			    WHEN h.status = 'captured' AND h.settled_at IS NULL THEN h.captured_minor
			    ELSE 0 END), 0)
			  FROM holds h WHERE h.account_id = b.account_id AND h.tokens IS NULL)`},
		{"held_tokens that differ from its token holds", `
			SELECT count(*) FROM balances b
			WHERE b.held_tokens <> (
			  SELECT coalesce(sum(CASE
			    WHEN h.status = 'active' THEN h.tokens
			    WHEN h.status = 'captured' AND h.settled_at IS NULL THEN h.captured_tokens
			    ELSE 0 END), 0)
			  FROM holds h WHERE h.account_id = b.account_id AND h.tokens IS NOT NULL)`},
		{"a service wallet holding more tokens than its lots have left", `
			SELECT count(*) FROM balances b
			WHERE b.held_tokens > (SELECT coalesce(sum(l.tokens_remaining), 0) FROM token_lots l WHERE l.account_id = b.account_id)`},
		{"a service wallet whose money differs from its lots' remaining value", `
			SELECT count(*) FROM balances b JOIN accounts a ON a.id = b.account_id
			WHERE a.type = 'service_wallet' AND b.balance_minor <> (
			  SELECT coalesce(sum(round(l.tokens_bought * l.minor_per_token)
			    - (SELECT coalesce(sum(lc.value_minor), 0) FROM lot_consumptions lc WHERE lc.lot_id = l.id)), 0)
			  FROM token_lots l WHERE l.account_id = b.account_id)`},
		{"reversal clearing that differs from the pay-backs still owed", `
			SELECT count(*) FROM currencies cur
			WHERE (SELECT coalesce(sum(CASE e.direction WHEN 'C' THEN e.amount_minor ELSE -e.amount_minor END), 0)
			       FROM entries e JOIN accounts a ON a.id = e.account_id
			       WHERE a.type = 'reversal_clearing' AND a.currency = cur.code)
			   <> (SELECT coalesce(sum(r.amount_minor), 0) FROM reversals r
			       WHERE r.status = 'payback_pending' AND r.currency = cur.code)`},
		{"a top-up reversed beyond what it paid in", `
			SELECT count(*) FROM transactions t
			WHERE t.type = 'topup' AND (SELECT coalesce(sum(r.amount_minor), 0) FROM reversals r
			                            WHERE r.original_transaction_id = t.id AND r.status <> 'rejected')
			  > (SELECT sum(e.amount_minor) FROM entries e JOIN accounts a ON a.id = e.account_id
			     WHERE e.transaction_id = t.id AND a.type = 'main_wallet' AND e.direction = 'C')`},
		{"a wallet below zero available", `
			SELECT count(*) FROM balances b JOIN accounts a ON a.id = b.account_id
			WHERE ledger.is_client_wallet(a.type) AND b.balance_minor - b.held_minor < 0`},
	}
	for _, q := range queries {
		if err := h.super.QueryRow(ctx, q.sql).Scan(&n); err != nil {
			return err.Error()
		}
		if n != 0 {
			return fmt.Sprintf("%d × %s", n, q.what)
		}
	}
	return ""
}

// BenchmarkHoldCapture measures a token hold followed by a capture, the
// exit gate's "holds and captures per second": on separate wallets
// (one tenant per goroutine) and contended on one wallet.
func BenchmarkHoldCapture(b *testing.B) {
	ctx := context.Background()
	h := newHarness(b)

	run := func(b *testing.B, tenantFor func() uuid.UUID) {
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			tenant := tenantFor()
			for pb.Next() {
				req := holdReq(tenant, "1")
				if _, err := h.svc.PlaceHold(ctx, req); err != nil {
					b.Error(err)
					return
				}
				if _, err := h.svc.CaptureHold(ctx, capture(tenant, req, "1")); err != nil {
					b.Error(err)
					return
				}
			}
		})
		b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "hold+capture/s")
	}

	b.Run("separate wallets", func(b *testing.B) {
		run(b, func() uuid.UUID {
			tenant := uuid.New()
			fundSMS(b, h, tenant, 1<<30)
			return tenant
		})
	})
	b.Run("one wallet", func(b *testing.B) {
		tenant := uuid.New()
		fundSMS(b, h, tenant, 1<<30)
		run(b, func() uuid.UUID { return tenant })
	})
}
