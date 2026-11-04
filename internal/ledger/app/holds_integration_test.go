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

func holdReq(tenant uuid.UUID, amount int64) app.HoldRequest {
	return app.HoldRequest{
		TenantID: tenant, Account: smsKES, ReferenceType: "campaign", ReferenceID: uuid.New(),
		AmountMinor: amount, ExpiresAt: testNow.Add(time.Hour),
	}
}

// TestHolds_WorkedExample: data model §5 steps 3 and 4. A 400-SMS
// campaign holds 320.00; 380 are delivered, so 304.00 is captured and
// the 16.00 for the 20 failed is released at once. The 304.00 stays
// held until settlement posts it (OQ-14): the wallet's available
// balance never shows it as spendable.
func TestHolds_WorkedExample(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	tenant := uuid.New()
	fundSMS(t, h, tenant, 800000)

	req := holdReq(tenant, 32000)
	placed, err := h.svc.PlaceHold(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if placed.Replayed || placed.Hold.Status != domain.HoldActive {
		t.Fatalf("placed %+v", placed)
	}
	if b := balanceOf(t, h, tenant, smsKES); b.BalanceMinor != 800000 || b.HeldMinor != 32000 || b.AvailableMinor != 768000 {
		t.Fatalf("after the hold: %+v", b)
	}
	if n := count(t, h, `SELECT count(*) FROM entries`); n != 4 {
		t.Fatalf("a hold posted journal lines: %d lines, want the 4 from funding", n)
	}

	captured, err := h.svc.CaptureHold(ctx, app.CaptureRequest{
		TenantID: tenant, ReferenceType: req.ReferenceType, ReferenceID: req.ReferenceID, CapturedMinor: 30400,
	})
	if err != nil {
		t.Fatal(err)
	}
	if captured.Hold.Status != domain.HoldCaptured || captured.Hold.CapturedMinor != 30400 || captured.Hold.Settled {
		t.Fatalf("captured %+v", captured)
	}
	if b := balanceOf(t, h, tenant, smsKES); b.BalanceMinor != 800000 || b.HeldMinor != 30400 || b.AvailableMinor != 769600 {
		t.Fatalf("after the capture: %+v, want 304.00 still held and 7,696.00 available", b)
	}

	t.Run("the same capture again changes nothing", func(t *testing.T) {
		again, err := h.svc.CaptureHold(ctx, app.CaptureRequest{
			TenantID: tenant, ReferenceType: req.ReferenceType, ReferenceID: req.ReferenceID, CapturedMinor: 30400,
		})
		if err != nil || !again.Replayed {
			t.Fatalf("replay: %+v, %v", again, err)
		}
		if b := balanceOf(t, h, tenant, smsKES); b.HeldMinor != 30400 {
			t.Fatalf("held %d after a replay", b.HeldMinor)
		}
	})

	t.Run("a different capture is refused", func(t *testing.T) {
		_, err := h.svc.CaptureHold(ctx, app.CaptureRequest{
			TenantID: tenant, ReferenceType: req.ReferenceType, ReferenceID: req.ReferenceID, CapturedMinor: 30000,
		})
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
		if b := balanceOf(t, h, tenant, smsKES); b.HeldMinor != 30400 {
			t.Fatalf("held %d after a replayed hold", b.HeldMinor)
		}
	})

	t.Run("the same reference with another amount is a conflict", func(t *testing.T) {
		other := req
		other.AmountMinor = 1
		_, err := h.svc.PlaceHold(ctx, other)
		wantErr(t, err, app.ErrIdempotencyConflict)
	})
}

func TestHolds_ReleaseAndRefusals(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	tenant := uuid.New()
	fundSMS(t, h, tenant, 10000)

	t.Run("a hold beyond the available balance", func(t *testing.T) {
		_, err := h.svc.PlaceHold(ctx, holdReq(tenant, 10001))
		wantErr(t, err, app.ErrInsufficientBalance)
	})

	t.Run("release gives it all back, once", func(t *testing.T) {
		req := holdReq(tenant, 6000)
		if _, err := h.svc.PlaceHold(ctx, req); err != nil {
			t.Fatal(err)
		}
		if _, err := h.svc.PlaceHold(ctx, holdReq(tenant, 4001)); err != nil {
			wantErr(t, err, app.ErrInsufficientBalance)
		} else {
			t.Fatal("a second hold beyond what's left was placed")
		}
		rel, err := h.svc.ReleaseHold(ctx, tenant, req.ReferenceType, req.ReferenceID)
		if err != nil || rel.Hold.Status != domain.HoldReleased || !rel.Hold.Settled {
			t.Fatalf("release: %+v, %v", rel, err)
		}
		again, err := h.svc.ReleaseHold(ctx, tenant, req.ReferenceType, req.ReferenceID)
		if err != nil || !again.Replayed {
			t.Fatalf("second release: %+v, %v", again, err)
		}
		if b := balanceOf(t, h, tenant, smsKES); b.HeldMinor != 0 || b.AvailableMinor != 10000 {
			t.Fatalf("after release: %+v", b)
		}
		_, err = h.svc.CaptureHold(ctx, app.CaptureRequest{TenantID: tenant, ReferenceType: req.ReferenceType, ReferenceID: req.ReferenceID, CapturedMinor: 1})
		wantErr(t, err, domain.ErrHoldState)
	})

	t.Run("capturing more than the hold", func(t *testing.T) {
		req := holdReq(tenant, 500)
		if _, err := h.svc.PlaceHold(ctx, req); err != nil {
			t.Fatal(err)
		}
		_, err := h.svc.CaptureHold(ctx, app.CaptureRequest{TenantID: tenant, ReferenceType: req.ReferenceType, ReferenceID: req.ReferenceID, CapturedMinor: 501})
		wantErr(t, err, domain.ErrHoldState)
		if b := balanceOf(t, h, tenant, smsKES); b.HeldMinor != 500 {
			t.Fatalf("held %d after a refused capture", b.HeldMinor)
		}
	})

	t.Run("an unknown hold, or another tenant's", func(t *testing.T) {
		_, err := h.svc.ReleaseHold(ctx, tenant, "campaign", uuid.New())
		wantErr(t, err, app.ErrNotFound)

		req := holdReq(tenant, 100)
		if _, err := h.svc.PlaceHold(ctx, req); err != nil {
			t.Fatal(err)
		}
		_, err = h.svc.ReleaseHold(ctx, uuid.New(), req.ReferenceType, req.ReferenceID)
		wantErr(t, err, app.ErrNotFound)
	})

	t.Run("bad requests", func(t *testing.T) {
		zero := holdReq(tenant, 0)
		_, err := h.svc.PlaceHold(ctx, zero)
		wantErr(t, err, app.ErrValidation)

		past := holdReq(tenant, 1)
		past.ExpiresAt = testNow.Add(-time.Second)
		_, err = h.svc.PlaceHold(ctx, past)
		wantErr(t, err, app.ErrValidation)

		notWallet := holdReq(tenant, 1)
		notWallet.Account = recvKES
		_, err = h.svc.PlaceHold(ctx, notWallet)
		wantErr(t, err, app.ErrValidation)

		vaspro := holdReq(tenant, 1)
		vaspro.Account = revenueKES
		_, err = h.svc.PlaceHold(ctx, vaspro)
		wantErr(t, err, app.ErrValidation)
	})
}

// TestHolds_CaptureWithPosting: an airtime-style item posts its own
// transaction when captured; the hold is settled and its whole amount
// un-held in the same transaction.
func TestHolds_CaptureWithPosting(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	tenant := uuid.New()
	airtime := domain.AccountKey{Type: domain.ServiceWallet, ServiceCode: "airtime", Currency: "KES"}
	airtimeRevenue := domain.AccountKey{Type: domain.Revenue, ServiceCode: "airtime", Currency: "KES"}
	topUp(t, h, tenant, 10000)
	if _, err := h.svc.Post(ctx, post(tenant, domain.TxnTokenPurchase, d(mainKES, 10000), c(airtime, 10000))); err != nil {
		t.Fatal(err)
	}

	req := holdReq(tenant, 5000)
	req.Account = airtime
	if _, err := h.svc.PlaceHold(ctx, req); err != nil {
		t.Fatal(err)
	}
	usage := post(tenant, domain.TxnUsage, d(airtime, 4800), c(airtimeRevenue, 4138), c(vatKES, 662))
	capture := app.CaptureRequest{TenantID: tenant, ReferenceType: req.ReferenceType, ReferenceID: req.ReferenceID, CapturedMinor: 4800, Posting: &usage}

	t.Run("a posting that doesn't take exactly the captured amount is refused", func(t *testing.T) {
		wrong := capture
		bad := post(tenant, domain.TxnUsage, d(airtime, 4700), c(airtimeRevenue, 4700))
		wrong.Posting = &bad
		_, err := h.svc.CaptureHold(ctx, wrong)
		wantErr(t, err, app.ErrValidation)
		if b := balanceOf(t, h, tenant, airtime); b.HeldMinor != 5000 {
			t.Fatalf("held %d after a refused capture", b.HeldMinor)
		}
	})

	res, err := h.svc.CaptureHold(ctx, capture)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Hold.Settled || res.Hold.TransactionID == nil {
		t.Fatalf("captured with posting: %+v", res.Hold)
	}
	if b := balanceOf(t, h, tenant, airtime); b.BalanceMinor != 5200 || b.HeldMinor != 0 || b.AvailableMinor != 5200 {
		t.Fatalf("after the capture: %+v, want 52.00 left, nothing held", b)
	}

	t.Run("the same capture again replays the posting", func(t *testing.T) {
		again, err := h.svc.CaptureHold(ctx, capture)
		if err != nil || !again.Replayed || *again.Hold.TransactionID != *res.Hold.TransactionID {
			t.Fatalf("replay: %+v, %v", again, err)
		}
		if b := balanceOf(t, h, tenant, airtime); b.BalanceMinor != 5200 {
			t.Fatalf("balance %d after a replay", b.BalanceMinor)
		}
	})

	t.Run("a plain capture later sent with a posting doesn't post", func(t *testing.T) {
		plain := holdReq(tenant, 100)
		plain.Account = airtime
		if _, err := h.svc.PlaceHold(ctx, plain); err != nil {
			t.Fatal(err)
		}
		if _, err := h.svc.CaptureHold(ctx, app.CaptureRequest{TenantID: tenant, ReferenceType: plain.ReferenceType, ReferenceID: plain.ReferenceID, CapturedMinor: 100}); err != nil {
			t.Fatal(err)
		}
		late := post(tenant, domain.TxnUsage, d(airtime, 100), c(airtimeRevenue, 100))
		_, err := h.svc.CaptureHold(ctx, app.CaptureRequest{TenantID: tenant, ReferenceType: plain.ReferenceType, ReferenceID: plain.ReferenceID, CapturedMinor: 100, Posting: &late})
		wantErr(t, err, domain.ErrHoldState)
	})

	t.Run("a posting key already used elsewhere is a conflict, and nothing is un-held", func(t *testing.T) {
		other := holdReq(tenant, 200)
		other.Account = airtime
		if _, err := h.svc.PlaceHold(ctx, other); err != nil {
			t.Fatal(err)
		}
		reused := usage // its key was used by the first capture
		reused.Lines = []domain.Line{d(airtime, 200), c(airtimeRevenue, 200)}
		_, err := h.svc.CaptureHold(ctx, app.CaptureRequest{TenantID: tenant, ReferenceType: other.ReferenceType, ReferenceID: other.ReferenceID, CapturedMinor: 200, Posting: &reused})
		wantErr(t, err, app.ErrIdempotencyConflict)
		if b := balanceOf(t, h, tenant, airtime); b.HeldMinor != 300 {
			t.Fatalf("held %d, want 300 (100 captured unsettled + 200 active)", b.HeldMinor)
		}
	})
}
