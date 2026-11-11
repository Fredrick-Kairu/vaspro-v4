//go:build integration

package app_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments/app"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments/domain"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/packs"
)

func TestSyncRails(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	if err := h.svc.SyncRails(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.SyncRails(ctx); err != nil {
		t.Fatalf("a second sync: %v", err)
	}
	if n := count(t, h, `SELECT count(*) FROM rails WHERE code = 'mpesa_ke' AND kind = 'mobile_money' AND status = 'live' AND 'c2b' = ANY (capabilities)`); n != 1 {
		t.Fatalf("mpesa_ke rows %d, want 1", n)
	}

	if _, err := h.super.Exec(ctx, `INSERT INTO rails (code, country_code, kind, capabilities, status) VALUES ('old_rail', 'KE', 'bank', '{}', 'live')`); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.SyncRails(ctx); err != nil {
		t.Fatal(err)
	}
	if n := count(t, h, `SELECT count(*) FROM rails WHERE code = 'old_rail' AND status = 'retired'`); n != 1 {
		t.Fatal("a rail no pack lists any more is retired, not left live")
	}

	other := *h.ke
	other.CountryCode = "TZ"
	dup := newHarness(t, func(d *app.Deps) { d.Packs = map[string]*packs.Pack{"KE": h.ke, "TZ": &other} })
	wantErr(t, dup.svc.SyncRails(ctx), app.ErrValidation)
}

func TestTopupDetails(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	pb := seedPaybill(t, h, "active")
	_ = pb
	ada, tenant := uuid.New(), uuid.New()
	h.accounts.Grant(ada, tenant, "KE", "KES", app.PermBillingRead)

	got, err := h.svc.TopupDetails(ctx, ada, tenant)
	if err != nil {
		t.Fatal(err)
	}
	if got.Rail != "mpesa_ke" || got.PaybillNumber != darajaApp.ShortCode || got.PaybillName != "VASPRO" || got.Currency != "KES" || !domain.ValidAccountNumber(got.AccountNumber) {
		t.Fatalf("details %+v", got)
	}
	again, err := h.svc.TopupDetails(ctx, ada, tenant)
	if err != nil || again != got {
		t.Fatalf("asked again: %+v %v, want the same number %s", again, err, got.AccountNumber)
	}

	bob, other := uuid.New(), uuid.New()
	h.accounts.Grant(bob, other, "KE", "KES", app.PermBillingRead, app.PermBillingManage)
	theirs, err := h.svc.TopupDetails(ctx, bob, other)
	if err != nil || theirs.AccountNumber == got.AccountNumber {
		t.Fatalf("another account: %+v %v", theirs, err)
	}

	t.Run("20 concurrent first calls issue one number", func(t *testing.T) {
		user, fresh := uuid.New(), uuid.New()
		h.accounts.Grant(user, fresh, "KE", "KES", app.PermBillingRead)
		var wg sync.WaitGroup
		numbers := make([]string, 20)
		errs := make([]error, 20)
		for i := range 20 {
			wg.Go(func() {
				d, err := h.svc.TopupDetails(ctx, user, fresh)
				numbers[i], errs[i] = d.AccountNumber, err
			})
		}
		wg.Wait()
		for i := range 20 {
			if errs[i] != nil || numbers[i] != numbers[0] {
				t.Fatalf("call %d: %q %v, want %q", i, numbers[i], errs[i], numbers[0])
			}
		}
		if n := count(t, h, `SELECT count(*) FROM topup_accounts WHERE tenant_id = $1`, fresh); n != 1 {
			t.Fatalf("%d numbers issued, want 1", n)
		}
	})

	t.Run("a number already taken is retried", func(t *testing.T) {
		seed := bytes.Repeat([]byte{42}, 6)
		taken, err := domain.NewAccountNumber(bytes.NewReader(seed))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.super.Exec(ctx, `INSERT INTO topup_accounts (id, tenant_id, rail_account_id, account_number) VALUES ($1, $2, $3, $4)`,
			uuid.New(), uuid.New(), pb, taken); err != nil {
			t.Fatal(err)
		}
		r := newHarnessOnSame(h, io.MultiReader(bytes.NewReader(seed), rand.Reader))
		user, fresh := uuid.New(), uuid.New()
		h.accounts.Grant(user, fresh, "KE", "KES", app.PermBillingRead)
		d, err := r.TopupDetails(ctx, user, fresh)
		if err != nil || d.AccountNumber == taken || !domain.ValidAccountNumber(d.AccountNumber) {
			t.Fatalf("got %+v %v; want a fresh number, not %s", d, err, taken)
		}
	})

	t.Run("access", func(t *testing.T) {
		member, stranger := uuid.New(), uuid.New()
		h.accounts.Grant(member, tenant, "KE", "KES") // no billing permission
		before := count(t, h, `SELECT count(*) FROM topup_accounts`)
		_, err := h.svc.TopupDetails(ctx, member, tenant)
		wantErr(t, err, app.ErrPermissionDenied)
		_, err = h.svc.TopupDetails(ctx, stranger, uuid.New())
		wantErr(t, err, app.ErrNotFound)
		if after := count(t, h, `SELECT count(*) FROM topup_accounts`); after != before {
			t.Fatal("a refused call issued a number")
		}
	})
}

// newHarnessOnSame is h's service with another random source.
func newHarnessOnSame(h harness, r io.Reader) *app.Service {
	return app.New(app.Deps{Pool: h.app, Clock: h.clk, Accounts: h.accounts, Packs: map[string]*packs.Pack{"KE": h.ke}, Rand: r})
}

func TestTopupDetails_Unavailable(t *testing.T) {
	ctx := context.Background()

	t.Run("a pending paybill isn't offered", func(t *testing.T) {
		h := newHarness(t)
		seedPaybill(t, h, "pending_verification")
		user, tenant := uuid.New(), uuid.New()
		h.accounts.Grant(user, tenant, "KE", "KES", app.PermBillingRead)
		_, err := h.svc.TopupDetails(ctx, user, tenant)
		wantErr(t, err, app.ErrTopupUnavailable)
		if n := count(t, h, `SELECT count(*) FROM topup_accounts`); n != 0 {
			t.Fatal("a number was issued with no paybill to use it on")
		}
	})
	t.Run("a country with no pack or no top-up rail", func(t *testing.T) {
		h := newHarness(t)
		seedPaybill(t, h, "active")
		user, tenant := uuid.New(), uuid.New()
		h.accounts.Grant(user, tenant, "UG", "UGX", app.PermBillingRead)
		_, err := h.svc.TopupDetails(ctx, user, tenant)
		wantErr(t, err, app.ErrTopupUnavailable)
	})
	t.Run("accounts down fails closed", func(t *testing.T) {
		h := newHarness(t)
		seedPaybill(t, h, "active")
		user, tenant := uuid.New(), uuid.New()
		h.accounts.Grant(user, tenant, "KE", "KES", app.PermBillingRead)
		h.accounts.Fail()
		if _, err := h.svc.TopupDetails(ctx, user, tenant); err == nil {
			t.Fatal("accounts down must not answer")
		}
		if n := count(t, h, `SELECT count(*) FROM topup_accounts`); n != 0 {
			t.Fatal("a number was issued while accounts was down")
		}
	})
}
