//go:build integration

package app_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/app"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/domain"
)

// holdReq holds tokens SMS units in the tenant's SMS wallet.
func holdReq(tenant uuid.UUID, tokens string) app.HoldRequest {
	return app.HoldRequest{
		TenantID: tenant, Account: smsKES, ReferenceType: "campaign", ReferenceID: uuid.New(),
		Tokens: tok(tokens), ExpiresAt: testNow.Add(time.Hour),
	}
}

func capture(tenant uuid.UUID, req app.HoldRequest, tokens string) app.CaptureRequest {
	return app.CaptureRequest{TenantID: tenant, ReferenceType: req.ReferenceType, ReferenceID: req.ReferenceID, CapturedTokens: tok(tokens)}
}

// TestHolds_WorkedExample: data model §5 steps 3 and 4. A 400-SMS
// campaign holds 400 units (shown as 320.00); 380 are delivered, so 380
// are captured and the 20 for the failed sends are released at once.
// The 380 stay held until settlement (3.5) draws them: the wallet's
// tokens available never count them as free (OQ-14, OQ-15).
func TestHolds_WorkedExample(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	tenant := uuid.New()
	fundSMS(t, h, tenant, 10000)

	req := holdReq(tenant, "400")
	placed, err := h.svc.PlaceHold(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if placed.Replayed || placed.Hold.Status != domain.HoldActive || !placed.Hold.Tokens.Equal(tok("400")) || placed.Hold.AmountMinor != 32000 {
		t.Fatalf("placed %+v, want 400 tokens shown as 320.00", placed.Hold)
	}
	b := balanceOf(t, h, tenant, smsKES)
	if b.BalanceMinor != 800000 || b.HeldMinor != 0 || !b.HeldTokens.Equal(tok("400")) || !b.TokensAvailable.Equal(tok("9600")) {
		t.Fatalf("after the hold: %+v", b)
	}
	if n := count(t, h, `SELECT count(*) FROM entries`); n != 4 {
		t.Fatalf("a hold posted journal lines: %d lines, want the 4 from funding", n)
	}

	captured, err := h.svc.CaptureHold(ctx, capture(tenant, req, "380"))
	if err != nil {
		t.Fatal(err)
	}
	if captured.Hold.Status != domain.HoldCaptured || !captured.Hold.CapturedTokens.Equal(tok("380")) || captured.Hold.Settled {
		t.Fatalf("captured %+v", captured.Hold)
	}
	b = balanceOf(t, h, tenant, smsKES)
	if b.BalanceMinor != 800000 || !b.HeldTokens.Equal(tok("380")) || !b.TokensAvailable.Equal(tok("9620")) {
		t.Fatalf("after the capture: %+v, want 380 still held and 9,620 available", b)
	}

	t.Run("the same capture again changes nothing", func(t *testing.T) {
		again, err := h.svc.CaptureHold(ctx, capture(tenant, req, "380"))
		if err != nil || !again.Replayed {
			t.Fatalf("replay: %+v, %v", again, err)
		}
		if b := balanceOf(t, h, tenant, smsKES); !b.HeldTokens.Equal(tok("380")) {
			t.Fatalf("held %s after a replay", b.HeldTokens)
		}
	})

	t.Run("a different capture is refused", func(t *testing.T) {
		_, err := h.svc.CaptureHold(ctx, capture(tenant, req, "379"))
		wantErr(t, err, domain.ErrHoldState)
	})

	t.Run("a captured hold can't be released", func(t *testing.T) {
		_, err := h.svc.ReleaseHold(ctx, tenant, req.ReferenceType, req.ReferenceID)
		wantErr(t, err, domain.ErrHoldState)
	})

	t.Run("placing the same hold again is a replay", func(t *testing.T) {
		again, err := h.svc.PlaceHold(ctx, req)
		if err != nil || !again.Replayed || again.Hold.ID != placed.Hold.ID {
			t.Fatalf("replay: %+v, %v", again, err)
		}
		if b := balanceOf(t, h, tenant, smsKES); !b.HeldTokens.Equal(tok("380")) {
			t.Fatalf("held %s after a replayed hold", b.HeldTokens)
		}
	})

	t.Run("the same reference for other tokens is a conflict", func(t *testing.T) {
		other := req
		other.Tokens = tok("1")
		_, err := h.svc.PlaceHold(ctx, other)
		wantErr(t, err, app.ErrIdempotencyConflict)
	})
}

// TestHolds_ValuedAfterEarlierHolds: a token hold is shown at the FIFO
// value of the tokens after those already held, here across a lot at
// 0.80 and a lot at 0.50 (display only).
func TestHolds_ValuedAfterEarlierHolds(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	tenant := uuid.New()
	fundSMS(t, h, tenant, 100)
	seedPrice(t, h, &tenant, "sms", "KES", "0", "50", jan1, nil)
	topUp(t, h, tenant, 5000)
	if _, err := h.svc.PurchaseTokens(ctx, purchase(tenant, "sms", "KES", "100")); err != nil {
		t.Fatal(err)
	}

	first, err := h.svc.PlaceHold(ctx, holdReq(tenant, "100"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := h.svc.PlaceHold(ctx, holdReq(tenant, "100"))
	if err != nil {
		t.Fatal(err)
	}
	if first.Hold.AmountMinor != 8000 || second.Hold.AmountMinor != 5000 {
		t.Fatalf("holds shown as %d and %d, want 8000 (lot at 0.80) and 5000 (lot at 0.50)", first.Hold.AmountMinor, second.Hold.AmountMinor)
	}
}

func TestHolds_ReleaseAndRefusals(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	tenant := uuid.New()
	fundSMS(t, h, tenant, 100)

	t.Run("a hold beyond the tokens left", func(t *testing.T) {
		_, err := h.svc.PlaceHold(ctx, holdReq(tenant, "100.000001"))
		wantErr(t, err, app.ErrInsufficientBalance)
	})

	t.Run("release gives it all back, once", func(t *testing.T) {
		req := holdReq(tenant, "60")
		if _, err := h.svc.PlaceHold(ctx, req); err != nil {
			t.Fatal(err)
		}
		_, err := h.svc.PlaceHold(ctx, holdReq(tenant, "40.5"))
		wantErr(t, err, app.ErrInsufficientBalance)
		rel, err := h.svc.ReleaseHold(ctx, tenant, req.ReferenceType, req.ReferenceID)
		if err != nil || rel.Hold.Status != domain.HoldReleased || !rel.Hold.Settled {
			t.Fatalf("release: %+v, %v", rel, err)
		}
		again, err := h.svc.ReleaseHold(ctx, tenant, req.ReferenceType, req.ReferenceID)
		if err != nil || !again.Replayed {
			t.Fatalf("second release: %+v, %v", again, err)
		}
		if b := balanceOf(t, h, tenant, smsKES); !b.HeldTokens.IsZero() || !b.TokensAvailable.Equal(tok("100")) {
			t.Fatalf("after release: %+v", b)
		}
		_, err = h.svc.CaptureHold(ctx, capture(tenant, req, "1"))
		wantErr(t, err, domain.ErrHoldState)
	})

	t.Run("capturing more than the hold", func(t *testing.T) {
		req := holdReq(tenant, "5")
		if _, err := h.svc.PlaceHold(ctx, req); err != nil {
			t.Fatal(err)
		}
		_, err := h.svc.CaptureHold(ctx, capture(tenant, req, "5.000001"))
		wantErr(t, err, domain.ErrHoldState)
		if b := balanceOf(t, h, tenant, smsKES); !b.HeldTokens.Equal(tok("5")) {
			t.Fatalf("held %s after a refused capture", b.HeldTokens)
		}
	})

	t.Run("a fractional capture", func(t *testing.T) {
		req := holdReq(tenant, "2.5")
		if _, err := h.svc.PlaceHold(ctx, req); err != nil {
			t.Fatal(err)
		}
		if _, err := h.svc.CaptureHold(ctx, capture(tenant, req, "1.25")); err != nil {
			t.Fatal(err)
		}
		if b := balanceOf(t, h, tenant, smsKES); !b.HeldTokens.Equal(tok("6.25")) {
			t.Fatalf("held %s, want 6.25 (5 active + 1.25 captured)", b.HeldTokens)
		}
	})

	t.Run("an unknown hold, or another tenant's", func(t *testing.T) {
		_, err := h.svc.ReleaseHold(ctx, tenant, "campaign", uuid.New())
		wantErr(t, err, app.ErrNotFound)

		req := holdReq(tenant, "1")
		if _, err := h.svc.PlaceHold(ctx, req); err != nil {
			t.Fatal(err)
		}
		_, err = h.svc.ReleaseHold(ctx, uuid.New(), req.ReferenceType, req.ReferenceID)
		wantErr(t, err, app.ErrNotFound)
	})

	t.Run("bad requests", func(t *testing.T) {
		for name, req := range map[string]app.HoldRequest{
			"a service wallet hold without tokens": {TenantID: tenant, Account: smsKES, ReferenceType: "campaign", ReferenceID: uuid.New(), AmountMinor: 100, ExpiresAt: testNow.Add(time.Hour)},
			"a main wallet hold in tokens":         {TenantID: tenant, Account: mainKES, ReferenceType: "payout", ReferenceID: uuid.New(), Tokens: tok("1"), ExpiresAt: testNow.Add(time.Hour)},
			"a zero money hold":                    {TenantID: tenant, Account: mainKES, ReferenceType: "payout", ReferenceID: uuid.New(), ExpiresAt: testNow.Add(time.Hour)},
			"too many decimals":                    {TenantID: tenant, Account: smsKES, ReferenceType: "campaign", ReferenceID: uuid.New(), Tokens: tok("0.0000001"), ExpiresAt: testNow.Add(time.Hour)},
			"in the past":                          {TenantID: tenant, Account: smsKES, ReferenceType: "campaign", ReferenceID: uuid.New(), Tokens: tok("1"), ExpiresAt: testNow.Add(-time.Second)},
			"a receivable":                         {TenantID: tenant, Account: recvKES, ReferenceType: "payout", ReferenceID: uuid.New(), AmountMinor: 1, ExpiresAt: testNow.Add(time.Hour)},
			"a VASPRO account":                     {TenantID: tenant, Account: revenueKES, ReferenceType: "payout", ReferenceID: uuid.New(), AmountMinor: 1, ExpiresAt: testNow.Add(time.Hour)},
		} {
			t.Run(name, func(t *testing.T) {
				_, err := h.svc.PlaceHold(ctx, req)
				wantErr(t, err, app.ErrValidation)
			})
		}
	})

	t.Run("a token hold can't be captured with a posting yet", func(t *testing.T) {
		req := holdReq(tenant, "1")
		if _, err := h.svc.PlaceHold(ctx, req); err != nil {
			t.Fatal(err)
		}
		usage := post(tenant, domain.TxnUsage, d(mainKES, 80), c(revenueKES, 80))
		withPosting := capture(tenant, req, "1")
		withPosting.Posting = &usage
		_, err := h.svc.CaptureHold(ctx, withPosting)
		wantErr(t, err, app.ErrValidation)
	})
}

// TestHolds_MoneyHoldCaptureWithPosting: a money hold on a main wallet
// (a payout's fee, say) posts its own transaction when captured; the
// hold is settled and its whole amount un-held in the same transaction.
func TestHolds_MoneyHoldCaptureWithPosting(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	tenant := uuid.New()
	payments := domain.AccountKey{Type: domain.Revenue, ServiceCode: "payments", Currency: "KES"}
	topUp(t, h, tenant, 10000)

	req := app.HoldRequest{TenantID: tenant, Account: mainKES, ReferenceType: "payout", ReferenceID: uuid.New(), AmountMinor: 5000, ExpiresAt: testNow.Add(time.Hour)}
	if _, err := h.svc.PlaceHold(ctx, req); err != nil {
		t.Fatal(err)
	}
	fee := post(tenant, domain.TxnUsage, d(mainKES, 4800), c(payments, 4138), c(vatKES, 662))
	cap := app.CaptureRequest{TenantID: tenant, ReferenceType: req.ReferenceType, ReferenceID: req.ReferenceID, CapturedMinor: 4800, Posting: &fee}

	t.Run("a posting that doesn't take exactly the captured amount is refused", func(t *testing.T) {
		wrong := cap
		bad := post(tenant, domain.TxnUsage, d(mainKES, 4700), c(payments, 4700))
		wrong.Posting = &bad
		_, err := h.svc.CaptureHold(ctx, wrong)
		wantErr(t, err, app.ErrValidation)
		if b := balanceOf(t, h, tenant, mainKES); b.HeldMinor != 5000 {
			t.Fatalf("held %d after a refused capture", b.HeldMinor)
		}
	})

	res, err := h.svc.CaptureHold(ctx, cap)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Hold.Settled || res.Hold.TransactionID == nil {
		t.Fatalf("captured with posting: %+v", res.Hold)
	}
	if b := balanceOf(t, h, tenant, mainKES); b.BalanceMinor != 5200 || b.HeldMinor != 0 || b.AvailableMinor != 5200 {
		t.Fatalf("after the capture: %+v, want 52.00 left, nothing held", b)
	}

	t.Run("the same capture again replays the posting", func(t *testing.T) {
		again, err := h.svc.CaptureHold(ctx, cap)
		if err != nil || !again.Replayed || *again.Hold.TransactionID != *res.Hold.TransactionID {
			t.Fatalf("replay: %+v, %v", again, err)
		}
		if b := balanceOf(t, h, tenant, mainKES); b.BalanceMinor != 5200 {
			t.Fatalf("balance %d after a replay", b.BalanceMinor)
		}
	})

	t.Run("a posting key already used elsewhere is a conflict, and nothing is un-held", func(t *testing.T) {
		other := app.HoldRequest{TenantID: tenant, Account: mainKES, ReferenceType: "payout", ReferenceID: uuid.New(), AmountMinor: 200, ExpiresAt: testNow.Add(time.Hour)}
		if _, err := h.svc.PlaceHold(ctx, other); err != nil {
			t.Fatal(err)
		}
		reused := fee // its key was used by the first capture
		reused.Lines = []domain.Line{d(mainKES, 200), c(payments, 200)}
		_, err := h.svc.CaptureHold(ctx, app.CaptureRequest{TenantID: tenant, ReferenceType: other.ReferenceType, ReferenceID: other.ReferenceID, CapturedMinor: 200, Posting: &reused})
		wantErr(t, err, app.ErrIdempotencyConflict)
		if b := balanceOf(t, h, tenant, mainKES); b.HeldMinor != 200 {
			t.Fatalf("held %d, want the 200 still active", b.HeldMinor)
		}
	})
}
