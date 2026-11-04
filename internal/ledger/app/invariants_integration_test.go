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

	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/app"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/domain"
)

// TestHolds_200ConcurrentHoldsNeverOverspend is Phase 3's concurrency
// test: 200 goroutines hold against one wallet that covers only half
// of them. Exactly the affordable number succeed, the rest are refused
// for balance, and held_minor is exactly the sum of the holds placed.
// Run with -race.
func TestHolds_200ConcurrentHoldsNeverOverspend(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	tenant := uuid.New()
	fundSMS(t, h, tenant, 10000)

	var placed, refused atomic.Int64
	var wg sync.WaitGroup
	for range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := h.svc.PlaceHold(ctx, holdReq(tenant, 100))
			switch {
			case err == nil:
				placed.Add(1)
			case errors.Is(err, app.ErrInsufficientBalance):
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
	if b.HeldMinor != 10000 || b.AvailableMinor != 0 {
		t.Fatalf("wallet %+v, want all 100.00 held", b)
	}
	if n := count(t, h, `SELECT count(*) FROM holds WHERE status = 'active'`); n != 100 {
		t.Fatalf("%d active holds, want 100", n)
	}
}

// TestLedger_RandomOperationsKeepInvariants is Phase 3's property test:
// a seeded random sequence of top-ups, token purchases, usage, holds,
// captures (with and without a posting) and releases across tenants
// and two currencies. After every step: debits equal credits per
// currency, every balance equals the replay of its entries, held_minor
// equals the sum of its holds' contributions, and no wallet's available
// balance is below zero. LEDGER_PROPERTY_SEED reruns a failing seed.
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
		amount int64
		wallet domain.AccountKey
	}
	var active []openHold
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

		var op string
		var err error
		switch rng.IntN(7) {
		case 0, 1:
			op = "top-up"
			amount *= 10
			_, err = h.svc.Post(ctx, post(tenant, domain.TxnTopup, d(cur.clear, amount), c(main, amount)))
		case 2:
			op = "token purchase"
			_, err = h.svc.Post(ctx, post(tenant, domain.TxnTokenPurchase, d(main, amount), c(sms, amount)))
		case 3:
			op = "usage"
			lines := []domain.Line{d(sms, amount)}
			if tax := amount * 16 / 116; tax > 0 { // a tiny usage has no VAT line, not a zero one
				lines = append(lines, c(revenue, amount-tax), c(vat, tax))
			} else {
				lines = append(lines, c(revenue, amount))
			}
			_, err = h.svc.Post(ctx, post(tenant, domain.TxnUsage, lines...))
		case 4:
			op = "hold"
			ref := uuid.New()
			_, err = h.svc.PlaceHold(ctx, app.HoldRequest{
				TenantID: tenant, Account: sms, ReferenceType: "campaign", ReferenceID: ref,
				AmountMinor: amount, ExpiresAt: testNow.Add(time.Hour),
			})
			if err == nil {
				active = append(active, openHold{tenant, ref, amount, sms})
			}
		case 5, 6:
			if len(active) == 0 {
				continue
			}
			i := rng.IntN(len(active))
			hd := active[i]
			active = append(active[:i], active[i+1:]...)
			used := int64(rng.IntN(int(hd.amount)) + 1)
			switch rng.IntN(3) {
			case 0:
				op = "release"
				_, err = h.svc.ReleaseHold(ctx, hd.tenant, "campaign", hd.ref)
			case 1:
				op = "capture"
				_, err = h.svc.CaptureHold(ctx, app.CaptureRequest{TenantID: hd.tenant, ReferenceType: "campaign", ReferenceID: hd.ref, CapturedMinor: used})
			default:
				op = "capture with posting"
				rev := domain.AccountKey{Type: domain.Revenue, ServiceCode: "sms", Currency: hd.wallet.Currency}
				p := post(hd.tenant, domain.TxnUsage, d(hd.wallet, used), c(rev, used))
				_, err = h.svc.CaptureHold(ctx, app.CaptureRequest{TenantID: hd.tenant, ReferenceType: "campaign", ReferenceID: hd.ref, CapturedMinor: used, Posting: &p})
			}
		}
		switch {
		case err == nil:
			succeeded[op]++
		case errors.Is(err, app.ErrInsufficientBalance):
			refused[op]++
		default:
			t.Fatalf("seed %d step %d %s: %v", seed, step, op, err)
		}
		if msg := checkInvariants(ctx, h); msg != "" {
			t.Fatalf("seed %d step %d after %s: %s", seed, step, op, msg)
		}
	}
	t.Logf("succeeded %v; refused for balance %v", succeeded, refused)
	for _, op := range []string{"top-up", "token purchase", "usage", "hold", "release", "capture", "capture with posting"} {
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
		{"held_minor that differs from its holds", `
			SELECT count(*) FROM balances b
			WHERE b.held_minor <> (
			  SELECT coalesce(sum(CASE
			    WHEN h.status = 'active' THEN h.amount_minor
			    WHEN h.status = 'captured' AND h.settled_at IS NULL THEN h.captured_minor
			    ELSE 0 END), 0)
			  FROM holds h WHERE h.account_id = b.account_id)`},
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

// BenchmarkHoldCapture measures a hold followed by a capture, the
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
				req := holdReq(tenant, 100)
				if _, err := h.svc.PlaceHold(ctx, req); err != nil {
					b.Error(err)
					return
				}
				if _, err := h.svc.CaptureHold(ctx, app.CaptureRequest{TenantID: tenant, ReferenceType: req.ReferenceType, ReferenceID: req.ReferenceID, CapturedMinor: 100}); err != nil {
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
			fundSMS(b, h, tenant, 1<<40)
			return tenant
		})
	})
	b.Run("one wallet", func(b *testing.B) {
		tenant := uuid.New()
		fundSMS(b, h, tenant, 1<<40)
		run(b, func() uuid.UUID { return tenant })
	})
}
