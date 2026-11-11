//go:build integration

package app_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments/app"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments/domain"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/daraja"
)

var checkoutSecret = bytes.Repeat([]byte{6}, 32)

func newCheckoutHarness(t *testing.T) *stkHarness {
	t.Helper()
	return newSTKHarness(t, func(d *app.Deps) {
		d.CheckoutSecret = checkoutSecret
		d.CheckoutBaseURL = "https://pay.example.test/"
	})
}

func TestCheckout(t *testing.T) {
	sh := newCheckoutHarness(t)
	ctx := context.Background()
	user, tenant := sh.client()

	link, err := sh.svc.CreateCheckoutLink(ctx, user, tenant, 500000, 0)
	if err != nil {
		t.Fatal(err)
	}
	if link.URL != "https://pay.example.test/pay/"+link.Token || link.Currency != "KES" || link.AmountMinor != 500000 ||
		!link.ExpiresAt.Equal(sh.clk.Now().Add(24*time.Hour).Truncate(time.Second)) {
		t.Fatalf("link %+v", link)
	}

	t.Run("creating needs billing:manage, whole units and an available paybill", func(t *testing.T) {
		reader := uuid.New()
		sh.accounts.Grant(reader, tenant, "KE", "KES", app.PermBillingRead)
		_, err := sh.svc.CreateCheckoutLink(ctx, reader, tenant, 500000, 0)
		wantErr(t, err, app.ErrPermissionDenied)
		_, err = sh.svc.CreateCheckoutLink(ctx, user, tenant, 500050, 0)
		wantErr(t, err, app.ErrValidation)
		_, err = sh.svc.CreateCheckoutLink(ctx, user, tenant, 500000, 8*24*time.Hour)
		wantErr(t, err, app.ErrValidation)
	})

	t.Run("the link is open, with the payee and the account's number", func(t *testing.T) {
		v, err := sh.svc.GetCheckout(ctx, link.Token)
		if err != nil || v.Status != "open" || v.AmountMinor != 500000 || v.Payee != "VASPRO" || !domain.ValidAccountNumber(v.AccountReference) {
			t.Fatalf("view %+v %v", v, err)
		}
	})

	t.Run("a cancelled attempt leaves the link open", func(t *testing.T) {
		sh.daraja.Next(daraja.Outcome{ResultCode: daraja.ResultCancelled})
		p, err := sh.svc.PayCheckout(ctx, link.Token, "k1", payer)
		if err != nil {
			t.Fatal(err)
		}
		sh.daraja.Wait()
		got, err := sh.svc.GetCheckoutPayment(ctx, link.Token, p.ID)
		if err != nil || got.Status != "cancelled" {
			t.Fatalf("payment %+v %v", got, err)
		}
		if v, _ := sh.svc.GetCheckout(ctx, link.Token); v.Status != "open" {
			t.Fatalf("status %s", v.Status)
		}
	})

	t.Run("a replayed key doesn't prompt again", func(t *testing.T) {
		pushes := sh.daraja.Pushes()
		first, _ := sh.svc.GetCheckout(ctx, link.Token)
		_ = first
		p, err := sh.svc.PayCheckout(ctx, link.Token, "k1", payer)
		if err != nil || sh.daraja.Pushes() != pushes || p.Status != "cancelled" {
			t.Fatalf("replay: %+v %v, pushes %d then %d", p, err, pushes, sh.daraja.Pushes())
		}
	})

	t.Run("paying the link credits the tenant once, then it's paid", func(t *testing.T) {
		p, err := sh.svc.PayCheckout(ctx, link.Token, "k2", payer)
		if err != nil {
			t.Fatal(err)
		}
		sh.daraja.Wait()
		if got, _ := sh.svc.GetCheckoutPayment(ctx, link.Token, p.ID); got.Status != "succeeded" || sh.ledger.Credited(tenant) != 500000 {
			t.Fatalf("payment %+v, credited %d", got, sh.ledger.Credited(tenant))
		}
		if v, _ := sh.svc.GetCheckout(ctx, link.Token); v.Status != "paid" {
			t.Fatalf("status %s", v.Status)
		}
		_, err = sh.svc.PayCheckout(ctx, link.Token, "k3", payer)
		wantErr(t, err, app.ErrCheckoutPaid)
	})

	t.Run("five attempts at most", func(t *testing.T) {
		other, err := sh.svc.CreateCheckoutLink(ctx, user, tenant, 100000, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		for i := range 5 {
			sh.daraja.Next(daraja.Outcome{ResultCode: daraja.ResultCancelled})
			if _, err := sh.svc.PayCheckout(ctx, other.Token, uuid.NewString(), payer); err != nil {
				t.Fatalf("attempt %d: %v", i+1, err)
			}
			sh.daraja.Wait()
		}
		before := count(t, sh.harness, `SELECT count(*) FROM payment_intents`)
		_, err = sh.svc.PayCheckout(ctx, other.Token, uuid.NewString(), payer)
		wantErr(t, err, app.ErrTooManyAttempts)
		if count(t, sh.harness, `SELECT count(*) FROM payment_intents`) != before {
			t.Fatal("the sixth attempt was recorded")
		}
	})

	t.Run("refusals", func(t *testing.T) {
		before := count(t, sh.harness, `SELECT count(*) FROM payment_intents`)
		tampered := link.Token[:5] + strings.ToUpper(link.Token[5:6]) + link.Token[6:]
		if tampered == link.Token {
			tampered = link.Token[:5] + "A" + link.Token[6:]
		}
		_, err := sh.svc.GetCheckout(ctx, tampered)
		wantErr(t, err, app.ErrNotFound)
		_, err = sh.svc.PayCheckout(ctx, tampered, "k9", payer)
		wantErr(t, err, app.ErrNotFound)
		_, err = sh.svc.PayCheckout(ctx, link.Token, "k10", "+255708374149")
		if err == nil {
			t.Fatal("a paid link took a payment")
		}
		short, err := sh.svc.CreateCheckoutLink(ctx, user, tenant, 100000, 5*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		_, err = sh.svc.GetCheckoutPayment(ctx, short.Token, uuid.New())
		wantErr(t, err, app.ErrNotFound)
		sh.clk.Advance(6 * time.Minute)
		if v, err := sh.svc.GetCheckout(ctx, short.Token); err != nil || v.Status != "expired" {
			t.Fatalf("expired view: %+v %v", v, err)
		}
		_, err = sh.svc.PayCheckout(ctx, short.Token, "k11", payer)
		wantErr(t, err, app.ErrNotFound)
		if count(t, sh.harness, `SELECT count(*) FROM payment_intents`) != before {
			t.Fatal("a refused payment was recorded")
		}
	})
}
