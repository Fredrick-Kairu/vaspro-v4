//go:build integration

package app_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/app"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/domain"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/packs"
)

// holdAndCapture holds tokens in the tenant's wallet and captures used
// of them for events delivered.
func holdAndCapture(t *testing.T, h harness, tenant uuid.UUID, wallet domain.AccountKey, tokens, used string, events int64) app.HoldRequest {
	t.Helper()
	ctx := context.Background()
	req := holdReq(tenant, tokens)
	req.Account = wallet
	req.ExpiresAt = h.clk.Now().Add(time.Hour)
	if _, err := h.svc.PlaceHold(ctx, req); err != nil {
		t.Fatalf("hold: %v", err)
	}
	cap := capture(tenant, req, used)
	cap.Events = events
	if _, err := h.svc.CaptureHold(ctx, cap); err != nil {
		t.Fatalf("capture: %v", err)
	}
	return req
}

// settle runs settlement for the window ending at the next grid line
// after the clock, then moves the clock past it.
func settle(t *testing.T, h harness) app.SettlementSummary {
	t.Helper()
	_, end := domain.WindowFor(h.clk.Now(), app.DefaultSettlementWindow)
	end = end.Add(app.DefaultSettlementWindow)
	sum, err := h.svc.SettleUsage(context.Background(), end)
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if sum.Failed != 0 {
		t.Fatalf("settlement failed for %d wallets", sum.Failed)
	}
	h.clk.Set(end.Add(time.Second))
	return sum
}

type usageLine struct {
	account domain.AccountType
	dir     string
	amount  int64
	tokens  *string
	taxCode *string
}

func usageLines(t *testing.T, h harness, batch uuid.UUID) []usageLine {
	t.Helper()
	rows, err := h.super.Query(context.Background(), `
		SELECT a.type, e.direction, e.amount_minor, e.tokens::text, e.tax_code
		FROM usage_batches b JOIN entries e ON e.transaction_id = b.transaction_id JOIN accounts a ON a.id = e.account_id
		WHERE b.id = $1 ORDER BY a.type, e.direction`, batch)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (usageLine, error) {
		var l usageLine
		var typ string
		err := r.Scan(&typ, &l.dir, &l.amount, &l.tokens, &l.taxCode)
		l.account = domain.AccountType(typ)
		return l, err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func lineAmount(lines []usageLine, typ domain.AccountType, dir string) int64 {
	var n int64
	for _, l := range lines {
		if l.account == typ && l.dir == dir {
			n += l.amount
		}
	}
	return n
}

// TestSettlement_WorkedExample is Phase 3's golden test, end to end
// (data model §5): client deal KES 0.80 per SMS unit; KES 13,000 top-up;
// buy 10,000 units (8,000.00); a 400-SMS campaign at 1 unit each, 380
// delivered → the SMS wallet pays 304.00, revenue 262.07, VAT 41.93,
// 20 units released. Then a new deal of KES 0.50: the next 10,000 units
// cost 5,000.00, and the 9,620 units left at 80 cents are used first.
func TestSettlement_WorkedExample(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	tenant := uuid.New()
	nov1 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	seedPrice(t, h, &tenant, "sms", "KES", "0", "80", jan1, &nov1)
	seedPrice(t, h, &tenant, "sms", "KES", "0", "50", nov1, nil)
	h.clk.Set(time.Date(2026, 10, 20, 9, 1, 0, 0, time.UTC))

	topUp(t, h, tenant, 1300000)
	lotA, err := h.svc.PurchaseTokens(ctx, purchase(tenant, "sms", "KES", "10000"))
	if err != nil || lotA.CostMinor != 800000 {
		t.Fatalf("buying 10,000 at 0.80: %+v, %v", lotA, err)
	}
	campaign := holdAndCapture(t, h, tenant, smsKES, "400", "380", 380)
	if b := balanceOf(t, h, tenant, smsKES); !b.HeldTokens.Equal(tok("380")) {
		t.Fatalf("held %s before settlement, want the 380 captured (20 released)", b.HeldTokens)
	}

	sum := settle(t, h)
	if len(sum.Batches) != 1 || sum.Skipped != 0 {
		t.Fatalf("summary %+v, want one batch", sum)
	}
	batch := sum.Batches[0]
	lines := usageLines(t, h, batch)
	if got := lineAmount(lines, domain.ServiceWallet, "D"); got != 30400 {
		t.Fatalf("SMS wallet debited %d, want 30400 (304.00)", got)
	}
	if got := lineAmount(lines, domain.Revenue, "C"); got != 26207 {
		t.Fatalf("revenue %d, want 26207 (262.07)", got)
	}
	if got := lineAmount(lines, domain.TaxPayable, "C"); got != 4193 {
		t.Fatalf("VAT %d, want 4193 (41.93)", got)
	}
	for _, l := range lines {
		if l.account == domain.ServiceWallet && (l.tokens == nil || *l.tokens != "380.000000") {
			t.Fatalf("the wallet line carries tokens %v, want 380", l.tokens)
		}
		if l.account == domain.TaxPayable && (l.taxCode == nil || *l.taxCode != "VAT") {
			t.Fatalf("the VAT line's tax code is %v", l.taxCode)
		}
	}

	var events int64
	var tokens string
	if err := h.super.QueryRow(ctx, `SELECT events, tokens::text FROM usage_batches WHERE id = $1`, batch).Scan(&events, &tokens); err != nil {
		t.Fatal(err)
	}
	if events != 380 || tokens != "380.000000" {
		t.Fatalf("batch events %d tokens %s, want 380 and 380", events, tokens)
	}
	var settled bool
	var holdBatch *uuid.UUID
	if err := h.super.QueryRow(ctx, `SELECT settled_at IS NOT NULL, usage_batch_id FROM holds WHERE reference_id = $1`, campaign.ReferenceID).Scan(&settled, &holdBatch); err != nil {
		t.Fatal(err)
	}
	if !settled || holdBatch == nil || *holdBatch != batch {
		t.Fatalf("the hold is settled %v with batch %v, want %s", settled, holdBatch, batch)
	}
	b := balanceOf(t, h, tenant, smsKES)
	if b.BalanceMinor != 769600 || !b.HeldTokens.IsZero() || !b.TokensRemaining.Equal(tok("9620")) {
		t.Fatalf("SMS wallet %+v, want 7,696.00 with 9,620 tokens and nothing held", b)
	}
	data := assertEventValid(t, h, "usage.posted", batch)
	if data["revenue_minor"].(float64) != 26207 || data["vat_minor"].(float64) != 4193 || data["vat_inclusive"] != true {
		t.Fatalf("usage.posted %v", data)
	}
	if refs := data["holds"].([]any); len(refs) != 1 || refs[0].(map[string]any)["reference_id"] != campaign.ReferenceID.String() {
		t.Fatalf("usage.posted holds %v", data["holds"])
	}

	// Step 5: a new deal of 0.50 from 1 Nov; 10,000 more units.
	h.clk.Set(time.Date(2026, 11, 4, 9, 1, 0, 0, time.UTC))
	lotB, err := h.svc.PurchaseTokens(ctx, purchase(tenant, "sms", "KES", "10000"))
	if err != nil || lotB.CostMinor != 500000 {
		t.Fatalf("buying 10,000 at 0.50: %+v, %v", lotB, err)
	}
	holdAndCapture(t, h, tenant, smsKES, "9620", "9620", 9620)
	sum = settle(t, h)
	if got := lineAmount(usageLines(t, h, sum.Batches[0]), domain.ServiceWallet, "D"); got != 769600 {
		t.Fatalf("the 9,620 units cost %d, want 769600: all from lot A at 80 cents", got)
	}
	holdAndCapture(t, h, tenant, smsKES, "100", "100", 100)
	sum = settle(t, h)
	if got := lineAmount(usageLines(t, h, sum.Batches[0]), domain.ServiceWallet, "D"); got != 5000 {
		t.Fatalf("the next 100 units cost %d, want 5000: lot B at 50 cents", got)
	}
	if got := systemBalance(t, h, revenueKES); got <= 0 {
		t.Fatalf("SMS revenue %d", got)
	}
}

// TestSettlement_VATExclusive: data model §5's variant (OQ-16). The SMS
// wallet pays the lots' 304.00, the main wallet the 48.64 VAT; what the
// main wallet can't cover becomes a debt on the receivable.
func TestSettlement_VATExclusive(t *testing.T) {
	h := newHarness(t)
	smsTZS := domain.AccountKey{Type: domain.ServiceWallet, ServiceCode: "sms", Currency: "TZS"}
	mainTZS := domain.AccountKey{Type: domain.MainWallet, Currency: "TZS"}
	clearing := domain.AccountKey{Type: domain.ProviderClearing, Rail: "mpesa_tz", Currency: "TZS"}
	seedPrice(t, h, nil, "sms", "TZS", "0", "80", jan1, nil)

	run := func(t *testing.T, topUpMinor int64) []usageLine {
		t.Helper()
		ctx := context.Background()
		tenant := uuid.New()
		if _, err := h.svc.Post(ctx, post(tenant, domain.TxnTopup, d(clearing, topUpMinor), c(mainTZS, topUpMinor))); err != nil {
			t.Fatal(err)
		}
		if _, err := h.svc.PurchaseTokens(ctx, purchase(tenant, "sms", "TZS", "380")); err != nil {
			t.Fatal(err)
		}
		holdAndCapture(t, h, tenant, smsTZS, "380", "380", 380)
		sum := settle(t, h)
		if len(sum.Batches) != 1 {
			t.Fatalf("summary %+v", sum)
		}
		return usageLines(t, h, sum.Batches[0])
	}

	t.Run("the main wallet pays the VAT", func(t *testing.T) {
		lines := run(t, 30400+10000)
		if lineAmount(lines, domain.ServiceWallet, "D") != 30400 || lineAmount(lines, domain.MainWallet, "D") != 4864 ||
			lineAmount(lines, domain.Revenue, "C") != 30400 || lineAmount(lines, domain.TaxPayable, "C") != 4864 ||
			lineAmount(lines, domain.Receivable, "D") != 0 {
			t.Fatalf("lines %+v, want 304.00 from the SMS wallet and 48.64 from the main wallet; revenue 304.00, VAT 48.64", lines)
		}
	})

	t.Run("a main wallet short of the VAT leaves a debt", func(t *testing.T) {
		lines := run(t, 30400+1000)
		if lineAmount(lines, domain.MainWallet, "D") != 1000 || lineAmount(lines, domain.Receivable, "D") != 3864 ||
			lineAmount(lines, domain.TaxPayable, "C") != 4864 {
			t.Fatalf("lines %+v, want 10.00 from the main wallet and 38.64 to the receivable", lines)
		}
	})
}

func TestSettlement_Windows(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	a, b := uuid.New(), uuid.New()
	fundSMS(t, h, a, 1000)
	fundSMS(t, h, b, 1000)

	t.Run("one run settles every client's wallets", func(t *testing.T) {
		holdAndCapture(t, h, a, smsKES, "10", "10", 10)
		holdAndCapture(t, h, b, smsKES, "20", "20", 20)
		if sum := settle(t, h); len(sum.Batches) != 2 {
			t.Fatalf("summary %+v, want two batches", sum)
		}
	})

	t.Run("a rerun of a window changes nothing, and a later capture waits for the next window", func(t *testing.T) {
		_, end := domain.WindowFor(h.clk.Now(), app.DefaultSettlementWindow)
		end = end.Add(app.DefaultSettlementWindow)
		holdAndCapture(t, h, a, smsKES, "5", "5", 5)
		first, err := h.svc.SettleUsage(ctx, end)
		if err != nil || len(first.Batches) != 1 {
			t.Fatalf("first run %+v, %v", first, err)
		}
		holdAndCapture(t, h, a, smsKES, "6", "6", 6)
		again, err := h.svc.SettleUsage(ctx, end)
		if err != nil || len(again.Batches) != 0 || again.Skipped != 1 {
			t.Fatalf("rerun %+v, %v; want the wallet skipped", again, err)
		}
		if b := balanceOf(t, h, a, smsKES); !b.HeldTokens.Equal(tok("6")) {
			t.Fatalf("held %s, want the 6 captured after the run still held", b.HeldTokens)
		}
		h.clk.Set(end.Add(time.Second))
		if next := settle(t, h); len(next.Batches) != 1 {
			t.Fatalf("next window %+v", next)
		}
	})

	t.Run("two runs at once make one batch per wallet", func(t *testing.T) {
		holdAndCapture(t, h, a, smsKES, "1", "1", 1)
		holdAndCapture(t, h, b, smsKES, "1", "1", 1)
		_, end := domain.WindowFor(h.clk.Now(), app.DefaultSettlementWindow)
		end = end.Add(app.DefaultSettlementWindow)
		var wg sync.WaitGroup
		results := make([]app.SettlementSummary, 2)
		for i := range results {
			wg.Add(1)
			go func() {
				defer wg.Done()
				sum, err := h.svc.SettleUsage(ctx, end)
				if err != nil {
					t.Error(err)
				}
				results[i] = sum
			}()
		}
		wg.Wait()
		if n := len(results[0].Batches) + len(results[1].Batches); n != 2 || results[0].Failed+results[1].Failed != 0 {
			t.Fatalf("runs %+v; want 2 batches between them, no failures", results)
		}
		var batches int
		if err := h.super.QueryRow(ctx, `SELECT count(*) FROM usage_batches WHERE window_end = $1`, end).Scan(&batches); err != nil {
			t.Fatal(err)
		}
		if batches != 2 {
			t.Fatalf("%d batches for the window, want 2", batches)
		}
		h.clk.Set(end.Add(time.Second))
	})

	t.Run("a capture worth under one minor unit waits until it is worth one", func(t *testing.T) {
		tiny := holdAndCapture(t, h, a, smsKES, "0.001", "0.001", 1)
		if sum := settle(t, h); len(sum.Batches) != 0 || sum.Skipped != 1 {
			t.Fatalf("summary %+v, want the wallet skipped", sum)
		}
		holdAndCapture(t, h, a, smsKES, "1", "1", 1)
		sum := settle(t, h)
		if len(sum.Batches) != 1 {
			t.Fatalf("summary %+v", sum)
		}
		if got := lineAmount(usageLines(t, h, sum.Batches[0]), domain.ServiceWallet, "D"); got != 80 {
			t.Fatalf("1.001 units cost %d, want 80", got)
		}
		var settled bool
		if err := h.super.QueryRow(ctx, `SELECT settled_at IS NOT NULL FROM holds WHERE reference_id = $1`, tiny.ReferenceID).Scan(&settled); err != nil || !settled {
			t.Fatalf("the tiny hold settled %v (%v)", settled, err)
		}
	})
}

func TestCapture_EventsAndMoneyHolds(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	tenant := uuid.New()
	fundSMS(t, h, tenant, 100)
	topUp(t, h, tenant, 1000)

	t.Run("a token capture without events counts one", func(t *testing.T) {
		req := holdAndCapture(t, h, tenant, smsKES, "3", "2", 0)
		var events int64
		if err := h.super.QueryRow(ctx, `SELECT captured_events FROM holds WHERE reference_id = $1`, req.ReferenceID).Scan(&events); err != nil || events != 1 {
			t.Fatalf("captured_events %d (%v), want 1", events, err)
		}
	})

	t.Run("a money hold can't be captured without its posting", func(t *testing.T) {
		req := app.HoldRequest{TenantID: tenant, Account: mainKES, ReferenceType: "payout", ReferenceID: uuid.New(), AmountMinor: 100, ExpiresAt: testNow.Add(time.Hour)}
		if _, err := h.svc.PlaceHold(ctx, req); err != nil {
			t.Fatal(err)
		}
		_, err := h.svc.CaptureHold(ctx, app.CaptureRequest{TenantID: tenant, ReferenceType: req.ReferenceType, ReferenceID: req.ReferenceID, CapturedMinor: 100})
		wantErr(t, err, app.ErrValidation)
	})

	t.Run("events on a money hold are refused", func(t *testing.T) {
		req := app.HoldRequest{TenantID: tenant, Account: mainKES, ReferenceType: "payout", ReferenceID: uuid.New(), AmountMinor: 100, ExpiresAt: testNow.Add(time.Hour)}
		if _, err := h.svc.PlaceHold(ctx, req); err != nil {
			t.Fatal(err)
		}
		fee := post(tenant, domain.TxnUsage, d(mainKES, 100), c(revenueKES, 100))
		_, err := h.svc.CaptureHold(ctx, app.CaptureRequest{TenantID: tenant, ReferenceType: req.ReferenceType, ReferenceID: req.ReferenceID, CapturedMinor: 100, Events: 3, Posting: &fee})
		wantErr(t, err, app.ErrValidation)
	})
}

func TestSyncTaxRates(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t) // syncs once
	if err := h.svc.SyncTaxRates(ctx); err != nil {
		t.Fatalf("a second sync: %v", err)
	}
	var rates int
	var rate string
	if err := h.super.QueryRow(ctx, `SELECT count(*), max(rate)::text FROM tax_rates WHERE country_code = 'KE'`).Scan(&rates, &rate); err != nil {
		t.Fatal(err)
	}
	if rates != 1 || rate != "0.1600" {
		t.Fatalf("KE has %d rates (%s), want one at 0.1600", rates, rate)
	}

	t.Run("a changed rate for the same date stops startup", func(t *testing.T) {
		changed := testPacks(t)
		ke := *changed["KE"]
		ke.Tax.VATRateBasisPoints = 1500
		changed["KE"] = &ke
		svc := app.New(app.Deps{Pool: h.app, Clock: h.clk, Packs: changed})
		if err := svc.SyncTaxRates(ctx); err == nil {
			t.Fatal("a different rate for an existing date was accepted")
		}
	})

	t.Run("a new rate from a new date is a new row", func(t *testing.T) {
		changed := testPacks(t)
		ke := *changed["KE"]
		ke.Tax.VATRateBasisPoints = 1500
		ke.Tax.VATEffectiveFrom = time.Date(2027, 7, 1, 0, 0, 0, 0, time.UTC)
		changed["KE"] = &ke
		if err := app.New(app.Deps{Pool: h.app, Clock: h.clk, Packs: changed}).SyncTaxRates(ctx); err != nil {
			t.Fatal(err)
		}
		if err := h.super.QueryRow(ctx, `SELECT count(*) FROM tax_rates WHERE country_code = 'KE'`).Scan(&rates); err != nil || rates != 2 {
			t.Fatalf("KE has %d rates, want 2", rates)
		}
	})

	t.Run("two packs with one currency stop startup", func(t *testing.T) {
		twins := testPacks(t)
		clone := *twins["KE"]
		clone.CountryCode = "ZZ"
		twins["ZZ"] = &clone
		if err := app.New(app.Deps{Pool: h.app, Clock: h.clk, Packs: map[string]*packs.Pack(twins)}).SyncTaxRates(ctx); err == nil {
			t.Fatal("two packs sharing KES were accepted")
		}
	})
}
