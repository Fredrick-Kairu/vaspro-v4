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

// staffMember is a staff user with a freshly stepped-up session.
type staffMember struct{ user, session uuid.UUID }

func newStaff(h harness) staffMember {
	s := staffMember{uuid.New(), uuid.New()}
	h.staff.SetStaff(s.user, true)
	h.staff.StepUp(s.user, s.session)
	return s
}

func dealFor(tenant uuid.UUID, price string) domain.NewPrice {
	return domain.NewPrice{TenantID: &tenant, ServiceCode: "sms", Currency: "KES", MinorPerToken: price, DealReference: "Q-" + price, From: jan1}
}

// TestTokenPrices_MakerChecker: a deal staff enter is never used until
// a different staff member with a fresh step-up approves it (task 3.4b,
// OQ-15); withdrawing it puts the client back on the list price.
func TestTokenPrices_MakerChecker(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	maker, checker := newStaff(h), newStaff(h)
	tenant := uuid.New()
	topUp(t, h, tenant, 1000000)
	buy := func() app.PurchaseResult {
		t.Helper()
		res, err := h.svc.PurchaseTokens(ctx, purchase(tenant, "sms", "KES", "100"))
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	price, err := h.svc.CreateTokenPrice(ctx, maker.user, dealFor(tenant, "50"))
	if err != nil {
		t.Fatal(err)
	}
	if price.Status != "pending_approval" || price.CreatedBy != maker.user || price.ApprovedBy != nil || *price.TenantID != tenant {
		t.Fatalf("created %+v", price)
	}
	if res := buy(); !res.MinorPerToken.Equal(tok("80")) {
		t.Fatalf("a pending deal was used: bought at %s, want the list's 80", res.MinorPerToken)
	}

	t.Run("the maker can't approve it", func(t *testing.T) {
		_, err := h.svc.ApproveTokenPrice(ctx, maker.user, maker.session, price.ID)
		wantErr(t, err, app.ErrMakerCannotApprove)
	})

	t.Run("approving needs a fresh step-up", func(t *testing.T) {
		stale := uuid.New()
		_, err := h.svc.ApproveTokenPrice(ctx, checker.user, stale, price.ID)
		wantErr(t, err, app.ErrStepUpRequired)
	})

	t.Run("someone who isn't staff can't approve it", func(t *testing.T) {
		outsider := uuid.New()
		_, err := h.svc.ApproveTokenPrice(ctx, outsider, uuid.New(), price.ID)
		wantErr(t, err, app.ErrNotStaff)
	})

	approved, err := h.svc.ApproveTokenPrice(ctx, checker.user, checker.session, price.ID)
	if err != nil {
		t.Fatal(err)
	}
	if approved.Status != "active" || approved.ApprovedBy == nil || *approved.ApprovedBy != checker.user {
		t.Fatalf("approved %+v", approved)
	}
	if res := buy(); res.TokenPriceID != price.ID || !res.MinorPerToken.Equal(tok("50")) {
		t.Fatalf("after approval bought at %s from %s, want the deal's 50", res.MinorPerToken, res.TokenPriceID)
	}

	t.Run("a second approval is refused", func(t *testing.T) {
		_, err := h.svc.ApproveTokenPrice(ctx, checker.user, checker.session, price.ID)
		wantErr(t, err, app.ErrPriceDecided)
	})

	t.Run("an overlapping deal for the same tier can't be approved", func(t *testing.T) {
		other, err := h.svc.CreateTokenPrice(ctx, maker.user, dealFor(tenant, "45"))
		if err != nil {
			t.Fatal(err)
		}
		_, err = h.svc.ApproveTokenPrice(ctx, checker.user, checker.session, other.ID)
		wantErr(t, err, app.ErrPriceOverlap)
	})

	t.Run("withdrawing needs a fresh step-up", func(t *testing.T) {
		_, err := h.svc.WithdrawTokenPrice(ctx, maker.user, uuid.New(), price.ID)
		wantErr(t, err, app.ErrStepUpRequired)
	})

	withdrawn, err := h.svc.WithdrawTokenPrice(ctx, maker.user, maker.session, price.ID)
	if err != nil || withdrawn.Status != "withdrawn" {
		t.Fatalf("withdraw: %+v, %v", withdrawn, err)
	}
	if res := buy(); !res.MinorPerToken.Equal(tok("80")) {
		t.Fatalf("after the withdrawal bought at %s, want the list's 80", res.MinorPerToken)
	}
	if lots := lotsOf(t, h, tenant); len(lots) != 3 || lots[1].price != "50.000000" {
		t.Fatalf("lots %+v: the lot bought at the deal must keep 50", lots)
	}

	t.Run("a withdrawn price stays withdrawn", func(t *testing.T) {
		_, err := h.svc.WithdrawTokenPrice(ctx, maker.user, maker.session, price.ID)
		wantErr(t, err, app.ErrPriceDecided)
		_, err = h.svc.ApproveTokenPrice(ctx, checker.user, checker.session, price.ID)
		wantErr(t, err, app.ErrPriceDecided)
	})
}

// TestTokenPrices_ListPrice: a list price (no client) is written as
// ledger_system and applies to every client without a deal.
func TestTokenPrices_ListPrice(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	maker, checker := newStaff(h), newStaff(h)
	end := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)

	list, err := h.svc.CreateTokenPrice(ctx, maker.user, domain.NewPrice{ServiceCode: "whatsapp", Currency: "KES", MinTokens: "0", MinorPerToken: "120.5", From: jan1, To: &end})
	if err != nil {
		t.Fatal(err)
	}
	if list.TenantID != nil || !list.MinorPerToken.Equal(tok("120.5")) || list.To == nil {
		t.Fatalf("created %+v", list)
	}
	if _, err := h.svc.ApproveTokenPrice(ctx, checker.user, checker.session, list.ID); err != nil {
		t.Fatal(err)
	}
	tenant := uuid.New()
	topUp(t, h, tenant, 10000)
	res, err := h.svc.PurchaseTokens(ctx, purchase(tenant, "whatsapp", "KES", "10"))
	if err != nil || res.TokenPriceID != list.ID || res.CostMinor != 1205 {
		t.Fatalf("bought %+v, %v; want 10 at the new list's 120.5", res, err)
	}
}

func TestTokenPrices_ListAndRefusals(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	maker := newStaff(h)
	a, b := uuid.New(), uuid.New()
	var ids []uuid.UUID
	for _, p := range []domain.NewPrice{dealFor(a, "50"), dealFor(b, "60"), dealFor(a, "40")} {
		created, err := h.svc.CreateTokenPrice(ctx, maker.user, p)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, created.ID)
	}

	t.Run("the pending queue across clients, oldest first, paged", func(t *testing.T) {
		var got []uuid.UUID
		filter := app.PriceFilter{Status: "pending_approval", Limit: 2}
		for {
			page, err := h.svc.ListTokenPrices(ctx, maker.user, filter)
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range page.Prices {
				got = append(got, p.ID)
			}
			if page.NextCursor == "" {
				break
			}
			filter.Cursor = page.NextCursor
		}
		if len(got) != 3 || got[0] != ids[0] || got[1] != ids[1] || got[2] != ids[2] {
			t.Fatalf("queue %v, want %v", got, ids)
		}
	})

	t.Run("one client's deals", func(t *testing.T) {
		page, err := h.svc.ListTokenPrices(ctx, maker.user, app.PriceFilter{TenantID: &a})
		if err != nil || len(page.Prices) != 2 {
			t.Fatalf("client A's deals: %+v, %v", page, err)
		}
	})

	t.Run("refusals write nothing", func(t *testing.T) {
		outsider := uuid.New()
		_, err := h.svc.CreateTokenPrice(ctx, outsider, dealFor(a, "1"))
		wantErr(t, err, app.ErrNotStaff)
		_, err = h.svc.ListTokenPrices(ctx, outsider, app.PriceFilter{})
		wantErr(t, err, app.ErrNotStaff)

		h.staff.SetStaff(maker.user, false) // revoked: takes effect at once
		_, err = h.svc.CreateTokenPrice(ctx, maker.user, dealFor(a, "1"))
		wantErr(t, err, app.ErrNotStaff)
		h.staff.SetStaff(maker.user, true)

		bad := dealFor(a, "1")
		bad.DealReference = ""
		_, err = h.svc.CreateTokenPrice(ctx, maker.user, bad)
		wantErr(t, err, app.ErrValidation)

		unknownCurrency := dealFor(a, "1")
		unknownCurrency.Currency = "USD"
		_, err = h.svc.CreateTokenPrice(ctx, maker.user, unknownCurrency)
		wantErr(t, err, app.ErrValidation)

		_, err = h.svc.ApproveTokenPrice(ctx, newStaff(h).user, uuid.Nil, uuid.New())
		wantErr(t, err, app.ErrStepUpRequired)
		checker := newStaff(h)
		_, err = h.svc.ApproveTokenPrice(ctx, checker.user, checker.session, uuid.New())
		wantErr(t, err, app.ErrNotFound)

		_, err = h.svc.ListTokenPrices(ctx, maker.user, app.PriceFilter{Cursor: "not-a-cursor"})
		wantErr(t, err, app.ErrValidation)

		if n := count(t, h, `SELECT count(*) FROM token_prices WHERE tenant_id IS NOT NULL`); n != 3 {
			t.Fatalf("%d deals, want the 3 created", n)
		}
	})

	t.Run("identity down fails closed", func(t *testing.T) {
		h.staff.Fail()
		_, err := h.svc.CreateTokenPrice(ctx, maker.user, dealFor(a, "1"))
		if err == nil {
			t.Fatal("a staff action went ahead without identity")
		}
	})
}
