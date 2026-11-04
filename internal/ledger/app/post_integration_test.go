//go:build integration

package app_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/app"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/domain"
)

func TestPost_TopUpAndReplay(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	tenant := uuid.New()

	req := post(tenant, domain.TxnTopup, d(mpesaKES, 1300000), c(mainKES, 1300000))
	first, err := h.svc.Post(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if first.Replayed || len(first.Lines) != 2 {
		t.Fatalf("first post: %+v", first)
	}
	if b := balanceOf(t, h, tenant, mainKES); b.BalanceMinor != 1300000 || b.AvailableMinor != 1300000 {
		t.Fatalf("main wallet %+v, want 13,000.00", b)
	}
	if got := systemBalance(t, h, mpesaKES); got != 1300000 {
		t.Fatalf("M-Pesa clearing = %d, want 1300000 (debit-normal)", got)
	}

	data := assertEventValid(t, h, "wallet.credited", first.Lines[1].AccountID)
	if data["amount_minor"].(float64) != 1300000 || data["transaction_type"] != "topup" {
		t.Fatalf("wallet.credited = %v", data)
	}

	t.Run("the same request again is a replay and writes nothing", func(t *testing.T) {
		again, err := h.svc.Post(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		if !again.Replayed || again.TransactionID != first.TransactionID {
			t.Fatalf("replay = %+v, want transaction %s replayed", again, first.TransactionID)
		}
		if n := count(t, h, `SELECT count(*) FROM transactions`); n != 1 {
			t.Fatalf("%d transactions after a replay, want 1", n)
		}
		if n := count(t, h, `SELECT count(*) FROM outbox_events`); n != 1 {
			t.Fatalf("%d events after a replay, want 1", n)
		}
		if b := balanceOf(t, h, tenant, mainKES); b.BalanceMinor != 1300000 {
			t.Fatalf("main wallet %d after a replay", b.BalanceMinor)
		}
	})

	t.Run("lines in another order are the same posting", func(t *testing.T) {
		swapped := req
		swapped.Lines = []domain.Line{c(mainKES, 1300000), d(mpesaKES, 1300000)}
		again, err := h.svc.Post(ctx, swapped)
		if err != nil {
			t.Fatal(err)
		}
		if !again.Replayed {
			t.Fatal("not a replay")
		}
	})

	t.Run("the same key with different lines is a conflict", func(t *testing.T) {
		other := req
		other.Lines = []domain.Line{d(mpesaKES, 1300001), c(mainKES, 1300001)}
		_, err := h.svc.Post(ctx, other)
		wantErr(t, err, app.ErrIdempotencyConflict)
	})

	t.Run("the same key with another type is a conflict", func(t *testing.T) {
		other := req
		other.Type = domain.TxnRefund
		_, err := h.svc.Post(ctx, other)
		wantErr(t, err, app.ErrIdempotencyConflict)
	})

	t.Run("another tenant may use the same key", func(t *testing.T) {
		other := req
		someone := uuid.New()
		other.TenantID = &someone
		res, err := h.svc.Post(ctx, other)
		if err != nil || res.Replayed {
			t.Fatalf("another tenant's post: %+v, %v", res, err)
		}
	})
}

func TestPost_Refusals(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	tenant := uuid.New()
	fundSMS(t, h, tenant, 100000)
	topUp(t, h, tenant, 50000)

	t.Run("spending more than the wallet holds", func(t *testing.T) {
		_, err := h.svc.Post(ctx, post(tenant, domain.TxnTokenPurchase, d(mainKES, 50001), c(smsKES, 50001)))
		wantErr(t, err, app.ErrInsufficientBalance)
	})

	t.Run("spending held money", func(t *testing.T) {
		if _, err := h.svc.PlaceHold(ctx, app.HoldRequest{
			TenantID: tenant, Account: smsKES, ReferenceType: "campaign", ReferenceID: uuid.New(),
			AmountMinor: 60000, ExpiresAt: testNow.Add(time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
		_, err := h.svc.Post(ctx, post(tenant, domain.TxnUsage, d(smsKES, 40001), c(revenueKES, 40001)))
		wantErr(t, err, app.ErrInsufficientBalance)
		if _, err := h.svc.Post(ctx, post(tenant, domain.TxnUsage, d(smsKES, 40000), c(revenueKES, 40000))); err != nil {
			t.Fatalf("spending exactly the unheld part: %v", err)
		}
	})

	t.Run("allocation is final", func(t *testing.T) {
		_, err := h.svc.Post(ctx, post(tenant, domain.TxnRefund, d(smsKES, 1), c(mainKES, 1)))
		wantErr(t, err, domain.ErrAllocationFinal)
	})

	t.Run("a reversal may move a service wallet back", func(t *testing.T) {
		tenant := uuid.New()
		fundSMS(t, h, tenant, 1000)
		if _, err := h.svc.Post(ctx, post(tenant, domain.TxnReversal, d(smsKES, 1000), c(mainKES, 1000))); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a frozen account takes nothing", func(t *testing.T) {
		tenant := uuid.New()
		topUp(t, h, tenant, 1000)
		if _, err := h.super.Exec(ctx, `UPDATE accounts SET status = 'frozen' WHERE tenant_id = $1 AND type = 'main_wallet'`, tenant); err != nil {
			t.Fatal(err)
		}
		_, err := h.svc.Post(ctx, post(tenant, domain.TxnTopup, d(mpesaKES, 5), c(mainKES, 5)))
		wantErr(t, err, app.ErrAccountNotOpen)
		_, err = h.svc.PlaceHold(ctx, app.HoldRequest{
			TenantID: tenant, Account: mainKES, ReferenceType: "payout", ReferenceID: uuid.New(),
			AmountMinor: 1, ExpiresAt: testNow.Add(time.Hour),
		})
		wantErr(t, err, app.ErrAccountNotOpen)
	})

	t.Run("a frozen VASPRO account takes nothing", func(t *testing.T) {
		tenant := uuid.New()
		if _, err := h.svc.Post(ctx, post(tenant, domain.TxnAdjustment, d(recvKES, 5), c(suspKES, 5))); err != nil {
			t.Fatal(err)
		}
		if _, err := h.super.Exec(ctx, `UPDATE accounts SET status = 'frozen' WHERE tenant_id IS NULL AND type = 'suspense'`); err != nil {
			t.Fatal(err)
		}
		_, err := h.svc.Post(ctx, post(tenant, domain.TxnAdjustment, d(recvKES, 5), c(suspKES, 5)))
		wantErr(t, err, app.ErrAccountNotOpen)
	})

	t.Run("an unbalanced posting is refused before the database", func(t *testing.T) {
		_, err := h.svc.Post(ctx, post(tenant, domain.TxnAdjustment, d(recvKES, 5), c(revenueKES, 4)))
		wantErr(t, err, app.ErrValidation)
	})

	t.Run("a system posting may not name a client account", func(t *testing.T) {
		req := post(tenant, domain.TxnAdjustment, d(mpesaKES, 5), c(mainKES, 5))
		req.TenantID = nil
		_, err := h.svc.Post(ctx, req)
		wantErr(t, err, app.ErrValidation)
	})

	t.Run("no idempotency key", func(t *testing.T) {
		req := post(tenant, domain.TxnTopup, d(mpesaKES, 5), c(mainKES, 5))
		req.IdempotencyKey = ""
		_, err := h.svc.Post(ctx, req)
		wantErr(t, err, app.ErrValidation)
	})

	t.Run("refusals write nothing", func(t *testing.T) {
		// Top-up, purchase, top-up, and the one spend of unheld money.
		if n := count(t, h, `SELECT count(*) FROM transactions WHERE tenant_id = $1`, tenant); n != 4 {
			t.Fatalf("%d transactions for the tenant, want 4", n)
		}
	})
}

// TestPost_SystemPosting: with no tenant, a posting between VASPRO's
// accounts runs as ledger_system.
func TestPost_SystemPosting(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	req := app.PostRequest{
		IdempotencyKey: "mpesa:QWE123", Type: domain.TxnAdjustment, Description: "unmatched M-Pesa payment",
		Lines: []domain.Line{d(mpesaKES, 2500), c(suspKES, 2500)},
	}
	first, err := h.svc.Post(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	again, err := h.svc.Post(ctx, req)
	if err != nil || !again.Replayed || again.TransactionID != first.TransactionID {
		t.Fatalf("system replay: %+v, %v", again, err)
	}
	if got := systemBalance(t, h, suspKES); got != 2500 {
		t.Fatalf("suspense = %d, want 2500", got)
	}
}

// TestPost_OpensAccountsOnce: accounts are opened by key on first use,
// once, even when many postings race to open them.
func TestPost_OpensAccountsOnce(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	tenant := uuid.New()

	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := h.svc.Post(ctx, post(tenant, domain.TxnTopup, d(mpesaKES, 100), c(mainKES, 100)))
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := count(t, h, `SELECT count(*) FROM accounts WHERE tenant_id = $1`, tenant); n != 1 {
		t.Fatalf("%d client accounts, want 1 main wallet", n)
	}
	if n := count(t, h, `SELECT count(*) FROM accounts WHERE tenant_id IS NULL`); n != 1 {
		t.Fatalf("%d VASPRO accounts, want 1 M-Pesa clearing", n)
	}
	if b := balanceOf(t, h, tenant, mainKES); b.BalanceMinor != 2000 {
		t.Fatalf("main wallet = %d, want 2000", b.BalanceMinor)
	}
}

// TestPost_ConcurrentSameKey: two concurrent posts with one key make
// one transaction; the other is answered as a replay.
func TestPost_ConcurrentSameKey(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	tenant := uuid.New()
	topUp(t, h, tenant, 1) // open the accounts first, so the race is on the key alone
	req := post(tenant, domain.TxnTopup, d(mpesaKES, 700), c(mainKES, 700))

	results := make(chan app.PostResult, 10)
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := h.svc.Post(ctx, req)
			if err != nil {
				t.Error(err)
			}
			results <- res
		}()
	}
	wg.Wait()
	close(results)
	var fresh int
	for r := range results {
		if !r.Replayed {
			fresh++
		}
	}
	if fresh != 1 {
		t.Fatalf("%d posts weren't replays, want exactly 1", fresh)
	}
	if b := balanceOf(t, h, tenant, mainKES); b.BalanceMinor != 701 {
		t.Fatalf("main wallet = %d, want 701", b.BalanceMinor)
	}
}

func TestGetBalances_WorkedExample(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	tenant := uuid.New()

	topUp(t, h, tenant, 1300000)
	if _, err := h.svc.Post(ctx, post(tenant, domain.TxnTokenPurchase, d(mainKES, 800000), c(smsKES, 800000))); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Post(ctx, post(tenant, domain.TxnUsage, d(smsKES, 30400), c(revenueKES, 26207), c(vatKES, 4193))); err != nil {
		t.Fatal(err)
	}

	bals, err := h.svc.GetBalances(ctx, &tenant)
	if err != nil {
		t.Fatal(err)
	}
	want := map[domain.AccountKey]int64{mainKES: 500000, smsKES: 769600}
	if len(bals) != len(want) {
		t.Fatalf("tenant has %d accounts, want %d", len(bals), len(want))
	}
	for _, b := range bals {
		if b.BalanceMinor != want[b.Account] || b.AvailableMinor != want[b.Account] || b.Status != "open" {
			t.Errorf("%+v, want balance %d", b, want[b.Account])
		}
	}
	for key, w := range map[domain.AccountKey]int64{mpesaKES: 1300000, revenueKES: 26207, vatKES: 4193} {
		if got := systemBalance(t, h, key); got != w {
			t.Errorf("VASPRO %+v = %d, want %d", key, got, w)
		}
	}

	t.Run("another tenant sees none of it", func(t *testing.T) {
		other := uuid.New()
		bals, err := h.svc.GetBalances(ctx, &other)
		if err != nil || len(bals) != 0 {
			t.Fatalf("another tenant: %v, %v", bals, err)
		}
	})
}
