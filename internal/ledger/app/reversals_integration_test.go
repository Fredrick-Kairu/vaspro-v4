//go:build integration

package app_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/app"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/domain"
)

var clearingKES = domain.AccountKey{Type: domain.ReversalClearing, Currency: "KES"}

// financeLead is a staff member with the finance-lead flag and a fresh
// step-up (task 3.7, OQ-18).
func financeLead(h harness) staffMember {
	s := newStaff(h)
	h.staff.SetFinanceLead(s.user, true)
	return s
}

func reversalOf(tenant, original uuid.UUID, kind domain.ReversalKind, amount int64, services ...string) app.ReversalRequest {
	return app.ReversalRequest{
		TenantID: tenant, OriginalTransactionID: original, Kind: kind, AmountMinor: amount,
		Reason: "paid twice by mistake", ClawbackServices: services,
	}
}

func raise(t *testing.T, h harness, maker staffMember, req app.ReversalRequest) app.Reversal {
	t.Helper()
	r, err := h.svc.RequestReversal(context.Background(), maker.user, maker.session, req)
	if err != nil {
		t.Fatalf("RequestReversal: %v", err)
	}
	return r
}

func approve(t *testing.T, h harness, checker staffMember, id uuid.UUID) app.Reversal {
	t.Helper()
	r, err := h.svc.ApproveReversal(context.Background(), checker.user, checker.session, id)
	if err != nil {
		t.Fatalf("ApproveReversal: %v", err)
	}
	return r
}

// TestReversal_WorkedExample: the worked example's client tops up
// 13,000.00 and buys 10,000 SMS units for 8,000.00; then the whole
// top-up is reversed and paid back. The main wallet gives its 5,000.00
// and the SMS wallet's unused 10,000 units give 8,000.00, the money
// waits in reversal clearing, and once the rail pays it back M-Pesa's
// clearing is where it started.
func TestReversal_WorkedExample(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	maker, lead := newStaff(h), financeLead(h)
	tenant := uuid.New()
	original := topUp(t, h, tenant, 1_300_000)
	if _, err := h.svc.PurchaseTokens(ctx, purchase(tenant, "sms", "KES", "10000")); err != nil {
		t.Fatal(err)
	}

	r := raise(t, h, maker, reversalOf(tenant, original.TransactionID, domain.ReversalPayback, 1_300_000, "sms"))
	if r.Status != domain.ReversalRequested || r.Currency != "KES" || r.RequestedBy != maker.user {
		t.Fatalf("requested %+v", r)
	}
	if got := count(t, h, `SELECT count(*) FROM transactions WHERE type = 'reversal'`); got != 0 {
		t.Fatalf("a request posted %d reversal transactions, want none before approval", got)
	}
	assertEventValid(t, h, "reversal.requested", r.ID)

	r = approve(t, h, lead, r.ID)
	if r.Status != domain.ReversalPaybackPending || r.ApprovedBy == nil || *r.ApprovedBy != lead.user || r.ReversalTransactionID == nil {
		t.Fatalf("approved %+v", r)
	}
	if got := balanceOf(t, h, tenant, mainKES).BalanceMinor; got != 0 {
		t.Fatalf("main wallet %d, want 0 (5,000.00 reversed)", got)
	}
	sms := balanceOf(t, h, tenant, smsKES)
	if sms.BalanceMinor != 0 || !sms.TokensRemaining.IsZero() {
		t.Fatalf("SMS wallet %d with %s tokens, want 0 and 0 (10,000 units clawed back)", sms.BalanceMinor, sms.TokensRemaining)
	}
	if got := systemBalance(t, h, clearingKES); got != 1_300_000 {
		t.Fatalf("reversal clearing %d, want 1,300,000", got)
	}
	var reverses uuid.UUID
	if err := h.super.QueryRow(ctx, `SELECT reverses_id FROM transactions WHERE id = $1`, *r.ReversalTransactionID).Scan(&reverses); err != nil {
		t.Fatal(err)
	}
	if reverses != original.TransactionID {
		t.Fatalf("the reversal reverses %s, want the top-up %s", reverses, original.TransactionID)
	}
	if got := count(t, h, `SELECT coalesce(sum(value_minor), 0)::int FROM lot_consumptions WHERE transaction_id = $1`, *r.ReversalTransactionID); got != 800_000 {
		t.Fatalf("lot consumptions worth %d, want 800,000", got)
	}
	assertEventValid(t, h, "reversal.approved", r.ID)
	posted := assertEventValid(t, h, "reversal.posted", r.ID)
	if posted["from_main_minor"] != float64(500_000) {
		t.Fatalf("reversal.posted from_main_minor = %v, want 500,000", posted["from_main_minor"])
	}

	t.Run("a failed pay-back keeps the money in clearing for a retry", func(t *testing.T) {
		failed, err := h.svc.RecordPayback(ctx, app.PaybackReport{TenantID: tenant, ReversalID: r.ID, Succeeded: false, Reference: "b2c-1", Error: "receiver unreachable"})
		if err != nil {
			t.Fatal(err)
		}
		if failed.Status != domain.ReversalPaybackPending || failed.PaybackAttempts != 1 || failed.LastPaybackError != "receiver unreachable" {
			t.Fatalf("after a failure %+v", failed)
		}
		if got := systemBalance(t, h, clearingKES); got != 1_300_000 {
			t.Fatalf("reversal clearing %d after a failure, want 1,300,000", got)
		}
	})

	t.Run("paid back: clearing empties and M-Pesa's clearing is back to zero", func(t *testing.T) {
		paid, err := h.svc.RecordPayback(ctx, app.PaybackReport{TenantID: tenant, ReversalID: r.ID, Succeeded: true, Reference: "b2c-2"})
		if err != nil {
			t.Fatal(err)
		}
		if paid.Status != domain.ReversalPaidBack || paid.PaybackTransactionID == nil || paid.PaybackReference != "b2c-2" {
			t.Fatalf("paid back %+v", paid)
		}
		if got := systemBalance(t, h, clearingKES); got != 0 {
			t.Fatalf("reversal clearing %d, want 0", got)
		}
		if got := systemBalance(t, h, mpesaKES); got != 0 {
			t.Fatalf("M-Pesa clearing %d, want 0", got)
		}
		assertEventValid(t, h, "reversal.paid_back", r.ID)

		again, err := h.svc.RecordPayback(ctx, app.PaybackReport{TenantID: tenant, ReversalID: r.ID, Succeeded: true, Reference: "b2c-2"})
		if err != nil || *again.PaybackTransactionID != *paid.PaybackTransactionID {
			t.Fatalf("a repeated success: %+v, %v; want the same pay-back", again, err)
		}
		if got := count(t, h, `SELECT count(*) FROM transactions WHERE type = 'payback'`); got != 1 {
			t.Fatalf("%d pay-back transactions, want 1", got)
		}
		_, err = h.svc.RecordPayback(ctx, app.PaybackReport{TenantID: tenant, ReversalID: r.ID, Succeeded: false, Error: "late"})
		wantErr(t, err, domain.ErrNotAwaitingPayback)
	})
}

// TestReversal_Chargeback: the rail already took the money back, so the
// reversal credits M-Pesa's clearing directly and is done once posted.
func TestReversal_Chargeback(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	maker, checker := newStaff(h), newStaff(h)
	tenant := uuid.New()
	original := topUp(t, h, tenant, 50_000)

	r := approve(t, h, checker, raise(t, h, maker, reversalOf(tenant, original.TransactionID, domain.ReversalChargeback, 50_000)).ID)
	if r.Status != domain.ReversalPosted {
		t.Fatalf("status %s, want posted", r.Status)
	}
	if got := systemBalance(t, h, mpesaKES); got != 0 {
		t.Fatalf("M-Pesa clearing %d, want 0", got)
	}
	if got := systemBalance(t, h, clearingKES); got != 0 {
		t.Fatalf("reversal clearing %d, want 0: a chargeback never passes through it", got)
	}
	if got := balanceOf(t, h, tenant, mainKES).BalanceMinor; got != 0 {
		t.Fatalf("main wallet %d, want 0", got)
	}
	_, err := h.svc.RecordPayback(ctx, app.PaybackReport{TenantID: tenant, ReversalID: r.ID, Succeeded: true})
	wantErr(t, err, domain.ErrNotAwaitingPayback)
}

// TestReversal_Cap: reversals of one top-up never add up to more than
// it; a rejected one frees its share.
func TestReversal_Cap(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	maker, checker := newStaff(h), newStaff(h)
	tenant := uuid.New()
	original := topUp(t, h, tenant, 100_000)

	first := raise(t, h, maker, reversalOf(tenant, original.TransactionID, domain.ReversalOperator, 60_000))
	_, err := h.svc.RequestReversal(ctx, maker.user, maker.session, reversalOf(tenant, original.TransactionID, domain.ReversalOperator, 40_001))
	wantErr(t, err, domain.ErrReversalExceedsOriginal)
	second := raise(t, h, maker, reversalOf(tenant, original.TransactionID, domain.ReversalOperator, 40_000))

	rejected, err := h.svc.RejectReversal(ctx, checker.user, checker.session, first.ID, "not a duplicate")
	if err != nil {
		t.Fatal(err)
	}
	if rejected.Status != domain.ReversalRejected || *rejected.RejectedBy != checker.user || rejected.RejectionReason != "not a duplicate" {
		t.Fatalf("rejected %+v", rejected)
	}
	assertEventValid(t, h, "reversal.rejected", first.ID)
	raise(t, h, maker, reversalOf(tenant, original.TransactionID, domain.ReversalOperator, 60_000))
	approve(t, h, checker, second.ID)
	_, err = h.svc.RequestReversal(ctx, maker.user, maker.session, reversalOf(tenant, original.TransactionID, domain.ReversalOperator, 1))
	wantErr(t, err, domain.ErrReversalExceedsOriginal)

	t.Run("only the tenant's own top-ups", func(t *testing.T) {
		buy, err := h.svc.PurchaseTokens(ctx, purchase(tenant, "sms", "KES", "10"))
		if err != nil {
			t.Fatal(err)
		}
		other := uuid.New()
		otherTopUp := topUp(t, h, other, 10_000)
		for name, req := range map[string]app.ReversalRequest{
			"a token purchase":            reversalOf(tenant, buy.TransactionID, domain.ReversalPayback, 1),
			"another tenant's top-up":     reversalOf(tenant, otherTopUp.TransactionID, domain.ReversalPayback, 1),
			"a transaction that's absent": reversalOf(tenant, uuid.New(), domain.ReversalPayback, 1),
		} {
			_, err := h.svc.RequestReversal(ctx, maker.user, maker.session, req)
			if !errors.Is(err, app.ErrNotFound) {
				t.Fatalf("%s: got %v, want ErrNotFound", name, err)
			}
		}
	})
}

// TestReversal_Concurrent: ten staff raising 20,000 each against one
// 100,000 top-up at once: exactly five go through.
func TestReversal_Concurrent(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	maker := newStaff(h)
	tenant := uuid.New()
	original := topUp(t, h, tenant, 100_000)

	var wg sync.WaitGroup
	errs := make([]error, 10)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = h.svc.RequestReversal(ctx, maker.user, maker.session, reversalOf(tenant, original.TransactionID, domain.ReversalOperator, 20_000))
		}()
	}
	wg.Wait()
	var ok, over int
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, domain.ErrReversalExceedsOriginal):
			over++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok != 5 || over != 5 {
		t.Fatalf("%d raised and %d refused, want 5 and 5", ok, over)
	}
}

// TestReversal_Clawback: the main wallet first, then unused, unheld
// tokens from the named wallets; a reversal they can't cover is refused
// and writes nothing (OQ-18: no debt).
func TestReversal_Clawback(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	maker, checker := newStaff(h), newStaff(h)
	tenant := uuid.New()
	original := topUp(t, h, tenant, 100_000)
	if _, err := h.svc.PurchaseTokens(ctx, purchase(tenant, "sms", "KES", "1000")); err != nil { // 80,000
		t.Fatal(err)
	}
	if _, err := h.svc.PlaceHold(ctx, app.HoldRequest{
		TenantID: tenant, Account: smsKES, ReferenceType: "campaign", ReferenceID: uuid.New(),
		Tokens: tok("600"), ExpiresAt: testNow.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	// Available: main 20,000; SMS 400 unheld tokens worth 32,000.

	t.Run("no wallet named: the main wallet alone falls short", func(t *testing.T) {
		r := raise(t, h, maker, reversalOf(tenant, original.TransactionID, domain.ReversalPayback, 20_001))
		_, err := h.svc.ApproveReversal(ctx, checker.user, checker.session, r.ID)
		wantErr(t, err, domain.ErrReversalNotCovered)
		got, err := h.svc.GetReversal(ctx, checker.user, r.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != domain.ReversalRequested || got.Preview == nil || got.Preview.Covered || got.Preview.MainAvailableMinor != 20_000 {
			t.Fatalf("after a refusal: %+v (preview %+v)", got, got.Preview)
		}
		if _, err := h.svc.RejectReversal(ctx, checker.user, checker.session, r.ID, "not covered"); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("held tokens are never clawed back", func(t *testing.T) {
		r := raise(t, h, maker, reversalOf(tenant, original.TransactionID, domain.ReversalPayback, 52_001, "sms"))
		_, err := h.svc.ApproveReversal(ctx, checker.user, checker.session, r.ID)
		wantErr(t, err, domain.ErrReversalNotCovered)
		if got := count(t, h, `SELECT count(*) FROM transactions WHERE type = 'reversal'`); got != 0 {
			t.Fatalf("%d reversal transactions after refusals, want 0", got)
		}
		if _, err := h.svc.RejectReversal(ctx, checker.user, checker.session, r.ID, "not covered"); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("exactly what's free: main 20,000 and 400 tokens", func(t *testing.T) {
		r := raise(t, h, maker, reversalOf(tenant, original.TransactionID, domain.ReversalPayback, 52_000, "sms"))
		preview, err := h.svc.GetReversal(ctx, checker.user, r.ID)
		if err != nil {
			t.Fatal(err)
		}
		p := preview.Preview
		if p == nil || !p.Covered || p.FromMainMinor != 20_000 || len(p.Clawback) != 1 || p.Clawback[0].ValueMinor != 32_000 || !p.Clawback[0].Tokens.Equal(tok("400")) {
			t.Fatalf("preview %+v", p)
		}
		approve(t, h, checker, r.ID)
		sms := balanceOf(t, h, tenant, smsKES)
		if sms.BalanceMinor != 48_000 || !sms.TokensRemaining.Equal(tok("600")) || !sms.HeldTokens.Equal(tok("600")) {
			t.Fatalf("SMS wallet %d, %s tokens, %s held; want 48,000, 600, 600", sms.BalanceMinor, sms.TokensRemaining, sms.HeldTokens)
		}
		if got := balanceOf(t, h, tenant, mainKES).BalanceMinor; got != 0 {
			t.Fatalf("main wallet %d, want 0", got)
		}
	})
}

// TestReversal_NewestLotFirst: with two lots, a clawback takes from the
// newest, so the older lot's price is the one the client keeps using.
func TestReversal_NewestLotFirst(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	maker, checker := newStaff(h), newStaff(h)
	tenant := uuid.New()
	original := topUp(t, h, tenant, 13_000)
	if _, err := h.svc.PurchaseTokens(ctx, purchase(tenant, "sms", "KES", "100")); err != nil { // lot A at 80: 8,000
		t.Fatal(err)
	}
	h.clk.Advance(time.Hour)
	seedPrice(t, h, &tenant, "sms", "KES", "0", "50", jan1, nil)
	if _, err := h.svc.PurchaseTokens(ctx, purchase(tenant, "sms", "KES", "100")); err != nil { // lot B at 50: 5,000
		t.Fatal(err)
	}

	// 2,500 from lot B at 50 is 49.99 tokens: 2,499.5 rounds half up to
	// 2,500, and the fewest tokens covering the amount are taken.
	approve(t, h, checker, raise(t, h, maker, reversalOf(tenant, original.TransactionID, domain.ReversalPayback, 2_500, "sms")).ID)
	lots := lotsOf(t, h, tenant)
	if lots[0].remaining != "100.000000" || lots[1].remaining != "50.010000" {
		t.Fatalf("lots left %s and %s, want lot A untouched (100) and 49.99 clawed from lot B", lots[0].remaining, lots[1].remaining)
	}
	if got := balanceOf(t, h, tenant, smsKES).BalanceMinor; got != 10_500 {
		t.Fatalf("SMS wallet %d, want 10,500 (8,000 in lot A and 2,500 left in lot B)", got)
	}
}

// TestReversal_MakerChecker: one staff member raises, a different one
// approves or rejects, with a fresh step-up; at or above the pack's
// KES 2,000 threshold only a finance lead may approve.
func TestReversal_MakerChecker(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	maker, checker, lead := newStaff(h), newStaff(h), financeLead(h)
	tenant := uuid.New()
	original := topUp(t, h, tenant, 1_000_000)

	t.Run("staff and a fresh step-up to raise", func(t *testing.T) {
		stranger := staffMember{uuid.New(), uuid.New()}
		h.staff.StepUp(stranger.user, stranger.session)
		_, err := h.svc.RequestReversal(ctx, stranger.user, stranger.session, reversalOf(tenant, original.TransactionID, domain.ReversalPayback, 1))
		wantErr(t, err, app.ErrNotStaff)
		stale := newStaff(h)
		h.staff.Expire(stale.user, stale.session)
		_, err = h.svc.RequestReversal(ctx, stale.user, stale.session, reversalOf(tenant, original.TransactionID, domain.ReversalPayback, 1))
		wantErr(t, err, app.ErrStepUpRequired)
		for name, req := range map[string]app.ReversalRequest{
			"an unknown kind": reversalOf(tenant, original.TransactionID, "refund", 1),
			"a zero amount":   reversalOf(tenant, original.TransactionID, domain.ReversalPayback, 0),
			"no reason":       {TenantID: tenant, OriginalTransactionID: original.TransactionID, Kind: domain.ReversalPayback, AmountMinor: 1},
			"a wallet twice":  reversalOf(tenant, original.TransactionID, domain.ReversalPayback, 1, "sms", "sms"),
		} {
			if _, err := h.svc.RequestReversal(ctx, maker.user, maker.session, req); !errors.Is(err, app.ErrValidation) {
				t.Fatalf("%s: got %v, want ErrValidation", name, err)
			}
		}
		if got := count(t, h, `SELECT count(*) FROM reversals`); got != 0 {
			t.Fatalf("refused requests wrote %d reversals", got)
		}
	})

	below := raise(t, h, maker, reversalOf(tenant, original.TransactionID, domain.ReversalOperator, 199_999))
	at := raise(t, h, maker, reversalOf(tenant, original.TransactionID, domain.ReversalOperator, 200_000))

	t.Run("the maker can't decide their own", func(t *testing.T) {
		_, err := h.svc.ApproveReversal(ctx, maker.user, maker.session, below.ID)
		wantErr(t, err, app.ErrMakerCannotApprove)
		_, err = h.svc.RejectReversal(ctx, maker.user, maker.session, below.ID, "changed my mind")
		wantErr(t, err, app.ErrMakerCannotApprove)
	})

	t.Run("a stale step-up or a non-staff checker", func(t *testing.T) {
		stale := newStaff(h)
		h.staff.Expire(stale.user, stale.session)
		_, err := h.svc.ApproveReversal(ctx, stale.user, stale.session, below.ID)
		wantErr(t, err, app.ErrStepUpRequired)
		revoked := newStaff(h)
		h.staff.SetStaff(revoked.user, false)
		_, err = h.svc.ApproveReversal(ctx, revoked.user, revoked.session, below.ID)
		wantErr(t, err, app.ErrNotStaff)
	})

	t.Run("at the threshold only a finance lead approves", func(t *testing.T) {
		_, err := h.svc.ApproveReversal(ctx, checker.user, checker.session, at.ID)
		wantErr(t, err, app.ErrFinanceLeadRequired)
		approve(t, h, lead, at.ID)
		approve(t, h, checker, below.ID)
	})

	t.Run("a decided reversal can't be decided again", func(t *testing.T) {
		_, err := h.svc.ApproveReversal(ctx, lead.user, lead.session, at.ID)
		wantErr(t, err, domain.ErrReversalDecided)
		_, err = h.svc.RejectReversal(ctx, lead.user, lead.session, at.ID, "too late")
		wantErr(t, err, domain.ErrReversalDecided)
	})

	t.Run("a rejection needs a reason", func(t *testing.T) {
		r := raise(t, h, maker, reversalOf(tenant, original.TransactionID, domain.ReversalOperator, 1))
		_, err := h.svc.RejectReversal(ctx, checker.user, checker.session, r.ID, " ")
		wantErr(t, err, app.ErrValidation)
	})

	t.Run("unknown reversals", func(t *testing.T) {
		_, err := h.svc.ApproveReversal(ctx, checker.user, checker.session, uuid.New())
		wantErr(t, err, app.ErrNotFound)
		_, err = h.svc.RecordPayback(ctx, app.PaybackReport{TenantID: uuid.New(), ReversalID: at.ID, Succeeded: true})
		wantErr(t, err, app.ErrNotFound)
	})

	t.Run("identity down fails closed", func(t *testing.T) {
		r := raise(t, h, maker, reversalOf(tenant, original.TransactionID, domain.ReversalOperator, 1))
		h.staff.Fail()
		if _, err := h.svc.ApproveReversal(ctx, checker.user, checker.session, r.ID); err == nil {
			t.Fatal("approved with identity down")
		}
		var status string
		if err := h.super.QueryRow(ctx, `SELECT status FROM reversals WHERE id = $1`, r.ID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != "requested" {
			t.Fatalf("status %s, want still requested", status)
		}
	})
}

// TestReversal_List: staff's queue across clients, oldest first, by
// status, paged.
func TestReversal_List(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	maker, checker := newStaff(h), newStaff(h)
	var ids []uuid.UUID
	for range 3 {
		tenant := uuid.New()
		original := topUp(t, h, tenant, 10_000)
		ids = append(ids, raise(t, h, maker, reversalOf(tenant, original.TransactionID, domain.ReversalOperator, 100)).ID)
	}
	approve(t, h, checker, ids[1])

	page, err := h.svc.ListReversals(ctx, checker.user, app.ReversalFilter{Status: "requested", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Reversals) != 1 || page.Reversals[0].ID != ids[0] || page.NextCursor == "" {
		t.Fatalf("first page %+v", page)
	}
	page, err = h.svc.ListReversals(ctx, checker.user, app.ReversalFilter{Status: "requested", Limit: 1, Cursor: page.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Reversals) != 1 || page.Reversals[0].ID != ids[2] {
		t.Fatalf("second page %+v", page)
	}
	all, err := h.svc.ListReversals(ctx, checker.user, app.ReversalFilter{})
	if err != nil || len(all.Reversals) != 3 {
		t.Fatalf("all: %d, %v", len(all.Reversals), err)
	}
	stranger := uuid.New()
	_, err = h.svc.ListReversals(ctx, stranger, app.ReversalFilter{})
	wantErr(t, err, app.ErrNotStaff)
	_, err = h.svc.ListReversals(ctx, checker.user, app.ReversalFilter{Cursor: "!!"})
	wantErr(t, err, app.ErrValidation)
}
