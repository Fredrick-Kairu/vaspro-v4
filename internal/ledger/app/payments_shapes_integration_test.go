//go:build integration

package app_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/app"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/domain"
)

// TestPost_PaymentsTopupShapes pins the three postings payments sends
// for money arriving on VASPRO's paybill (tasks 4.4 and 4.5), as the
// ledger's own contract: a direct top-up (provider clearing → main
// wallet), a system posting into suspense, and the staff match out of
// suspense into the main wallet. Suspense goes up and back to 0, and
// wallet.credited is written only for the client postings.
func TestPost_PaymentsTopupShapes(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tenant := uuid.New()
	clearing := domain.AccountKey{Type: domain.ProviderClearing, Rail: "mpesa_ke", Currency: "KES"}
	suspense := domain.AccountKey{Type: domain.Suspense, Currency: "KES"}
	main := domain.AccountKey{Type: domain.MainWallet, Currency: "KES"}
	post := func(tenant *uuid.UUID, key string, notice uuid.UUID, from, to domain.AccountKey, amount int64) app.PostResult {
		t.Helper()
		res, err := h.svc.Post(ctx, app.PostRequest{
			TenantID: tenant, IdempotencyKey: key, Type: domain.TxnTopup, Description: "M-Pesa top-up",
			ReferenceType: "collection_notice", ReferenceID: &notice,
			Lines: []domain.Line{{Account: from, Direction: domain.Debit, AmountMinor: amount}, {Account: to, Direction: domain.Credit, AmountMinor: amount}},
		})
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		return res
	}
	balance := func(tenant *uuid.UUID, key domain.AccountKey) int64 {
		t.Helper()
		bals, err := h.svc.GetBalances(ctx, tenant)
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range bals {
			if b.Account == key {
				return b.BalanceMinor
			}
		}
		return 0
	}

	direct := uuid.New()
	d := post(&tenant, "topup:"+direct.String(), direct, clearing, main, 150000)
	assertEventValid(t, h, "wallet.credited", d.Lines[1].AccountID)

	unmatched := uuid.New()
	post(nil, "suspense:"+unmatched.String(), unmatched, clearing, suspense, 50000)
	if got := balance(nil, suspense); got != 50000 {
		t.Fatalf("suspense %d after the payment, want 50000", got)
	}
	if n := count(t, h, `SELECT count(*) FROM outbox_events WHERE event_type = 'wallet.credited'`); n != 1 {
		t.Fatalf("%d wallet.credited events, want only the direct top-up's", n)
	}

	post(&tenant, "match:"+unmatched.String(), unmatched, suspense, main, 50000)
	if got := balance(nil, suspense); got != 0 {
		t.Fatalf("suspense %d after the match, want 0", got)
	}
	if got := balance(&tenant, main); got != 200000 {
		t.Fatalf("main wallet %d, want 200000", got)
	}
	if got := balance(nil, clearing); got != 200000 {
		t.Fatalf("provider clearing %d, want 200000", got)
	}
	if n := count(t, h, `SELECT count(*) FROM outbox_events WHERE event_type = 'wallet.credited'`); n != 2 {
		t.Fatalf("%d wallet.credited events, want 2", n)
	}
	// A repeated match key is a replay, not a second credit.
	post(&tenant, "match:"+unmatched.String(), unmatched, suspense, main, 50000)
	if got := balance(&tenant, main); got != 200000 {
		t.Fatalf("main wallet %d after a replay", got)
	}
}
