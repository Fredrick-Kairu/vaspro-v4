//go:build integration

package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments/adapters"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments/app"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments/domain"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/clock"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/testkit"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/daraja"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/ledgerposter"
)

const payer = "+" + daraja.SandboxMSISDN

var callbackSecret = bytes.Repeat([]byte{5}, 32)

// stkHarness is a harness whose service pushes to the Daraja fake,
// receives its callbacks on a local server standing in for the gateway,
// and posts to a fake ledger.
type stkHarness struct {
	harness
	ledger    *ledgerposter.Fake
	paybill   uuid.UUID
	mu        sync.Mutex
	callbacks [][]byte // bodies the callback server received
}

func newSTKHarness(t *testing.T, opts ...option) *stkHarness {
	t.Helper()
	sh := &stkHarness{ledger: ledgerposter.New()}
	var svc *app.Service
	cb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/payments/callbacks/daraja/"), "/")
		body, _ := io.ReadAll(r.Body)
		sh.mu.Lock()
		sh.callbacks = append(sh.callbacks, body)
		sh.mu.Unlock()
		acct, _ := uuid.Parse(parts[0])
		local := netip.MustParseAddr("127.0.0.1")
		var err error
		accept, code := true, ""
		switch strings.Join(parts[2:], "/") {
		case "stk":
			err = svc.HandleSTKCallback(r.Context(), acct, parts[1], local, body)
		case "c2b/validation":
			accept, code, err = svc.HandleC2BValidation(r.Context(), acct, parts[1], local, body)
		case "c2b/confirmation":
			err = svc.HandleC2BConfirmation(r.Context(), acct, parts[1], local, body)
		default:
			http.NotFound(w, r)
			return
		}
		switch {
		case errors.Is(err, app.ErrBadCallback):
			w.WriteHeader(http.StatusUnauthorized)
		case err != nil:
			w.WriteHeader(http.StatusBadRequest)
		default:
			_, _ = w.Write(adapters.C2BResponse(accept, code))
		}
	}))
	t.Cleanup(cb.Close)
	all := append([]option{func(d *app.Deps) {
		d.Ledger = sh.ledger
		d.CallbackBaseURL = cb.URL
		d.CallbackSecret = callbackSecret
		d.CallbackCIDRs = []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}
	}}, opts...)
	sh.harness = newHarness(t, all...)
	svc = sh.svc
	sh.paybill = credentialedPaybill(t, sh.harness)
	return sh
}

// credentialedPaybill adds VASPRO's paybill with the fake's credentials
// and marks it active as the owner, so tests don't depend on a
// verification call.
func credentialedPaybill(t *testing.T, h harness) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	if err := h.svc.SyncRails(ctx); err != nil {
		t.Fatal(err)
	}
	h.staff.Add("ops@example.com")
	acct, err := h.svc.AddRailAccount(ctx, app.AddRailAccountRequest{StaffEmail: "ops@example.com", RailCode: "mpesa_ke", Number: darajaApp.ShortCode, DisplayName: "VASPRO"})
	if err != nil {
		t.Fatal(err)
	}
	for kind, secret := range map[string]string{"app_key": darajaApp.ConsumerKey, "app_secret": darajaApp.ConsumerSecret, "passkey": darajaApp.Passkey} {
		if err := h.svc.SetRailCredential(ctx, app.SetRailCredentialRequest{StaffEmail: "ops@example.com", RailAccountID: acct.ID, Kind: kind, Secret: []byte(secret)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.super.Exec(ctx, `UPDATE rail_accounts SET status = 'active', verified_at = now() WHERE id = $1`, acct.ID); err != nil {
		t.Fatal(err)
	}
	return acct.ID
}

// client grants a new user billing:read and :manage in a new tenant.
func (sh *stkHarness) client() (user, tenant uuid.UUID) {
	user, tenant = uuid.New(), uuid.New()
	sh.accounts.Grant(user, tenant, "KE", "KES", app.PermBillingRead, app.PermBillingManage)
	return user, tenant
}

func (sh *stkHarness) start(t *testing.T, user, tenant uuid.UUID, amount int64) app.Topup {
	t.Helper()
	top, err := sh.svc.StartTopup(context.Background(), user, tenant, uuid.NewString(), amount, payer)
	if err != nil {
		t.Fatalf("StartTopup: %v", err)
	}
	return top
}

func (sh *stkHarness) get(t *testing.T, user, tenant, id uuid.UUID) app.Topup {
	t.Helper()
	top, err := sh.svc.GetTopup(context.Background(), user, tenant, id)
	if err != nil {
		t.Fatalf("GetTopup: %v", err)
	}
	return top
}

func (sh *stkHarness) lastCallback(t *testing.T) []byte {
	t.Helper()
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if len(sh.callbacks) == 0 {
		t.Fatal("no callback received")
	}
	return sh.callbacks[len(sh.callbacks)-1]
}

func (sh *stkHarness) notices(t *testing.T, intent uuid.UUID) int {
	t.Helper()
	return count(t, sh.harness, `SELECT count(*) FROM collection_notices WHERE payment_intent_id = $1`, intent)
}

func (sh *stkHarness) exceptions(t *testing.T, intent uuid.UUID, kind string) int {
	t.Helper()
	return count(t, sh.harness, `SELECT count(*) FROM recon_exceptions WHERE reference_type = 'payment_intent' AND reference_id = $1 AND kind = $2`, intent, kind)
}

func TestSTKTopup_Outcomes(t *testing.T) {
	sh := newSTKHarness(t)
	for name, tc := range map[string]struct {
		code      int
		status    string
		errorCode string
	}{
		"success":            {daraja.ResultSuccess, "succeeded", ""},
		"cancelled by user":  {daraja.ResultCancelled, "cancelled", "PAY-6503"},
		"timeout":            {daraja.ResultTimeout, "failed", "PAY-6504"},
		"insufficient funds": {daraja.ResultInsufficientFunds, "failed", "PAY-6505"},
		"wrong PIN":          {daraja.ResultWrongPIN, "failed", "PAY-6506"},
	} {
		t.Run(name, func(t *testing.T) {
			user, tenant := sh.client()
			sh.daraja.Next(daraja.Outcome{ResultCode: tc.code})
			top := sh.start(t, user, tenant, 150000)
			// Pending, or already settled: the fake's callback runs in the
			// background and can win the race with StartTopup's reload.
			if top.Status == "created" || !domain.ValidAccountNumber(top.AccountReference) || top.PayerPhone != payer {
				t.Fatalf("started %+v", top)
			}
			sh.daraja.Wait()
			got := sh.get(t, user, tenant, top.ID)
			if got.Status != tc.status || got.ErrorCode != tc.errorCode || (got.CompletedAt == nil) {
				t.Fatalf("got %s %s, want %s %s", got.Status, got.ErrorCode, tc.status, tc.errorCode)
			}
			if tc.code == daraja.ResultSuccess {
				if got.Receipt == "" || sh.ledger.Credited(tenant) != 150000 || sh.notices(t, top.ID) != 1 {
					t.Fatalf("success: receipt %q credited %d notices %d", got.Receipt, sh.ledger.Credited(tenant), sh.notices(t, top.ID))
				}
				ev := assertEventValid(t, sh.harness, "payment_intent.succeeded", top.ID)
				if ev["paid_minor"] != float64(150000) || ev["receipt"] != got.Receipt {
					t.Fatalf("event %v", ev)
				}
				return
			}
			if sh.ledger.Credited(tenant) != 0 || sh.notices(t, top.ID) != 0 {
				t.Fatal("a failure credited the wallet")
			}
			ev := assertEventValid(t, sh.harness, "payment_intent.failed", top.ID)
			if ev["error_code"] != tc.errorCode {
				t.Fatalf("event %v", ev)
			}
		})
	}
	t.Run("the posting is a top-up of the notice", func(t *testing.T) {
		postings := sh.ledger.Postings()
		if len(postings) != 1 {
			t.Fatalf("%d postings, want the one success's", len(postings))
		}
		p := postings[0]
		if p.Rail != "mpesa_ke" || p.Currency != "KES" || p.AmountMinor != 150000 {
			t.Fatalf("posting %+v", p)
		}
		if n := count(t, sh.harness, `SELECT count(*) FROM collection_notices WHERE id = $1 AND ledger_transaction_id IS NOT NULL AND match_status = 'matched' AND matched_tenant_id = $2`, p.NoticeID, p.TenantID); n != 1 {
			t.Fatal("the notice doesn't record its posting")
		}
	})
	t.Run("no secret, token or payer number in the logs", func(t *testing.T) {
		logs := sh.logs.String()
		for _, s := range []string{darajaApp.ConsumerSecret, darajaApp.Passkey, daraja.SandboxMSISDN[3:], domain.CallbackToken(callbackSecret, sh.paybill)} {
			if strings.Contains(logs, s) {
				t.Fatalf("logged %q", s)
			}
		}
	})
}

func TestSTKTopup_RepeatedAndEarlyCallbacks(t *testing.T) {
	sh := newSTKHarness(t)
	ctx := context.Background()

	t.Run("a duplicate callback is ignored", func(t *testing.T) {
		user, tenant := sh.client()
		sh.daraja.Next(daraja.Outcome{Delivery: daraja.DeliverTwice})
		top := sh.start(t, user, tenant, 150000)
		sh.daraja.Wait()
		if sh.get(t, user, tenant, top.ID).Status != "succeeded" || sh.ledger.Credited(tenant) != 150000 || sh.notices(t, top.ID) != 1 {
			t.Fatalf("credited %d with %d notices", sh.ledger.Credited(tenant), sh.notices(t, top.ID))
		}
	})
	t.Run("20 concurrent copies credit once", func(t *testing.T) {
		user, tenant := sh.client()
		top := sh.start(t, user, tenant, 150000)
		sh.daraja.Wait()
		body := sh.lastCallback(t)
		token := domain.CallbackToken(callbackSecret, sh.paybill)
		warmPool(t, sh.app) // so the copies overlap (see TestC2B_StaffMatch)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for range 20 {
			wg.Go(func() {
				<-start
				if err := sh.svc.HandleSTKCallback(ctx, sh.paybill, token, netip.MustParseAddr("127.0.0.1"), body); err != nil {
					t.Errorf("replay: %v", err)
				}
			})
		}
		close(start)
		wg.Wait()
		if sh.ledger.Credited(tenant) != 150000 || sh.notices(t, top.ID) != 1 {
			t.Fatalf("credited %d with %d notices", sh.ledger.Credited(tenant), sh.notices(t, top.ID))
		}
		if n := count(t, sh.harness, `SELECT count(*) FROM outbox_events WHERE event_type = 'payment_intent.succeeded' AND aggregate_id = $1`, top.ID); n != 1 {
			t.Fatalf("%d success events, want 1: the intent must be decided once", n)
		}
	})
	t.Run("a callback that arrives before the push returns is still applied", func(t *testing.T) {
		user, tenant := sh.client()
		sh.daraja.Next(daraja.Outcome{Delivery: daraja.DeliverBeforeResponse})
		top := sh.start(t, user, tenant, 150000)
		if top.Status != "succeeded" || sh.ledger.Credited(tenant) != 150000 {
			t.Fatalf("got %s, credited %d", top.Status, sh.ledger.Credited(tenant))
		}
		if n := count(t, sh.harness, `SELECT count(*) FROM stk_callbacks WHERE payment_intent_id = $1 AND applied_at IS NOT NULL`, top.ID); n != 1 {
			t.Fatal("the parked callback isn't marked applied")
		}
	})
}

func TestSTKTopup_AmountMismatch(t *testing.T) {
	sh := newSTKHarness(t)
	user, tenant := sh.client()
	sh.daraja.Next(daraja.Outcome{PaidMinor: 100000})
	top := sh.start(t, user, tenant, 150000)
	sh.daraja.Wait()
	if sh.get(t, user, tenant, top.ID).Status != "succeeded" || sh.ledger.Credited(tenant) != 100000 {
		t.Fatalf("credited %d, want the callback's 100000", sh.ledger.Credited(tenant))
	}
	if sh.exceptions(t, top.ID, "amount_mismatch") != 1 {
		t.Fatal("the mismatch isn't flagged")
	}
	if ev := assertEventValid(t, sh.harness, "payment_intent.succeeded", top.ID); ev["paid_minor"] != float64(100000) || ev["amount_minor"] != float64(150000) {
		t.Fatalf("event %v", ev)
	}
}

// stkCallbackBody is Daraja's success callback for checkout, built by
// hand for a callback the fake never sends.
func stkCallbackBody(merchant, checkout, receipt string, amountMinor int64, at time.Time) []byte {
	return fmt.Appendf(nil, `{"Body":{"stkCallback":{"MerchantRequestID":%q,"CheckoutRequestID":%q,"ResultCode":0,"ResultDesc":"The service request is processed successfully.","CallbackMetadata":{"Item":[{"Name":"Amount","Value":%d.%02d},{"Name":"MpesaReceiptNumber","Value":%q},{"Name":"TransactionDate","Value":%s},{"Name":"PhoneNumber","Value":%s}]}}}}`,
		merchant, checkout, amountMinor/100, amountMinor%100, receipt, at.In(daraja.EAT).Format("20060102150405"), daraja.SandboxMSISDN)
}

func (sh *stkHarness) checkout(t *testing.T, intent uuid.UUID) string {
	t.Helper()
	var id string
	if err := sh.super.QueryRow(context.Background(), `SELECT rail_request_id FROM payment_intents WHERE id = $1`, intent).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func (sh *stkHarness) sweep(t *testing.T) app.SweepSummary {
	t.Helper()
	sum, err := sh.svc.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	return sum
}

func TestSTKTopup_LostCallbacks(t *testing.T) {
	sh := newSTKHarness(t)
	ctx := context.Background()
	token := domain.CallbackToken(callbackSecret, sh.paybill)

	t.Run("nothing happens before expiry", func(t *testing.T) {
		user, tenant := sh.client()
		sh.daraja.Next(daraja.Outcome{ResultCode: daraja.ResultCancelled, Delivery: daraja.NoCallback})
		top := sh.start(t, user, tenant, 150000)
		sh.sweep(t)
		if sh.get(t, user, tenant, top.ID).Status != "pending" {
			t.Fatal("the sweep settled a top-up before it expired")
		}
		sh.clk.Advance(app.DefaultSTKExpiry + time.Second)
		if sum := sh.sweep(t); sum.Queried != 1 {
			t.Fatalf("queried %d, want 1", sum.Queried)
		}
		if got := sh.get(t, user, tenant, top.ID); got.Status != "cancelled" || got.ErrorCode != "PAY-6503" {
			t.Fatalf("a failure found by query: %s %s", got.Status, got.ErrorCode)
		}
	})

	t.Run("a payment found only by query isn't credited, and a later callback is", func(t *testing.T) {
		user, tenant := sh.client()
		sh.daraja.Next(daraja.Outcome{ResultCode: daraja.ResultSuccess, Delivery: daraja.NoCallback})
		top := sh.start(t, user, tenant, 150000)
		sh.clk.Advance(app.DefaultSTKExpiry + time.Second)
		sh.sweep(t)
		got := sh.get(t, user, tenant, top.ID)
		if got.Status != "pending" || sh.ledger.Credited(tenant) != 0 || sh.exceptions(t, top.ID, "status_mismatch") != 1 {
			t.Fatalf("status %s, credited %d, exceptions %d", got.Status, sh.ledger.Credited(tenant), sh.exceptions(t, top.ID, "status_mismatch"))
		}
		sh.clk.Advance(app.DefaultSTKGiveUp)
		if sum := sh.sweep(t); sum.Queried != 0 {
			t.Fatal("it was queried again")
		}
		if sh.get(t, user, tenant, top.ID).Status != "pending" || sh.exceptions(t, top.ID, "status_mismatch") != 1 {
			t.Fatal("a payment the rail reported was expired, or flagged twice")
		}
		body := stkCallbackBody("m-late", sh.checkout(t, top.ID), "LATE000001", 150000, sh.clk.Now())
		if err := sh.svc.HandleSTKCallback(ctx, sh.paybill, token, netip.MustParseAddr("127.0.0.1"), body); err != nil {
			t.Fatal(err)
		}
		if got := sh.get(t, user, tenant, top.ID); got.Status != "succeeded" || got.Receipt != "LATE000001" || sh.ledger.Credited(tenant) != 150000 {
			t.Fatalf("the confirmation: %s %q credited %d", got.Status, got.Receipt, sh.ledger.Credited(tenant))
		}
	})

	t.Run("no answer at all expires, and a late success is still credited", func(t *testing.T) {
		user, tenant := sh.client()
		sh.daraja.Next(daraja.Outcome{ResultCode: daraja.ResultSuccess, Delivery: daraja.Manual})
		top := sh.start(t, user, tenant, 150000)
		sh.clk.Advance(app.DefaultSTKExpiry + time.Second)
		sh.sweep(t)
		if sh.get(t, user, tenant, top.ID).Status != "pending" {
			t.Fatal("expired before giving up")
		}
		sh.clk.Advance(app.DefaultSTKGiveUp)
		sh.sweep(t)
		got := sh.get(t, user, tenant, top.ID)
		if got.Status != "expired" || got.ErrorCode != "PAY-6504" {
			t.Fatalf("got %s %s, want expired PAY-6504", got.Status, got.ErrorCode)
		}
		assertEventValid(t, sh.harness, "payment_intent.failed", top.ID)
		if _, err := sh.daraja.Release(sh.checkout(t, top.ID)); err != nil {
			t.Fatal(err)
		}
		got = sh.get(t, user, tenant, top.ID)
		if got.Status != "succeeded" || sh.ledger.Credited(tenant) != 150000 || sh.exceptions(t, top.ID, "status_mismatch") != 1 {
			t.Fatalf("late success: %s credited %d exceptions %d", got.Status, sh.ledger.Credited(tenant), sh.exceptions(t, top.ID, "status_mismatch"))
		}
		if ev := assertEventValid(t, sh.harness, "payment_intent.succeeded", top.ID); ev["late"] != true {
			t.Fatalf("event %v", ev)
		}
	})
}

func TestSTKTopup_LedgerDown(t *testing.T) {
	sh := newSTKHarness(t)
	user, tenant := sh.client()
	sh.ledger.Fail()
	top := sh.start(t, user, tenant, 150000)
	sh.daraja.Wait()
	if sh.get(t, user, tenant, top.ID).Status != "succeeded" || sh.ledger.Credited(tenant) != 0 {
		t.Fatal("the top-up must be recorded but not yet credited")
	}
	if n := count(t, sh.harness, `SELECT count(*) FROM collection_notices WHERE payment_intent_id = $1 AND ledger_transaction_id IS NULL`, top.ID); n != 1 {
		t.Fatal("the notice should wait for its posting")
	}
	sh.ledger.Recover()
	if sum := sh.sweep(t); sum.Posted != 1 {
		t.Fatalf("posted %d, want 1", sum.Posted)
	}
	if sum := sh.sweep(t); sum.Posted != 0 {
		t.Fatalf("posted %d again", sum.Posted)
	}
	if sh.ledger.Credited(tenant) != 150000 {
		t.Fatalf("credited %d", sh.ledger.Credited(tenant))
	}
}

func TestSTKTopup_StartRules(t *testing.T) {
	sh := newSTKHarness(t)
	ctx := context.Background()
	user, tenant := sh.client()

	t.Run("a replayed key returns the same top-up without prompting again", func(t *testing.T) {
		first, err := sh.svc.StartTopup(ctx, user, tenant, "k1", 150000, payer)
		if err != nil {
			t.Fatal(err)
		}
		sh.daraja.Wait()
		pushes := sh.daraja.Pushes()
		again, err := sh.svc.StartTopup(ctx, user, tenant, "k1", 150000, payer)
		if err != nil || again.ID != first.ID || sh.daraja.Pushes() != pushes {
			t.Fatalf("replay: %v %v, pushes %d then %d", again.ID, err, pushes, sh.daraja.Pushes())
		}
		_, err = sh.svc.StartTopup(ctx, user, tenant, "k1", 200000, payer)
		wantErr(t, err, app.ErrIdempotencyConflict)
	})
	t.Run("refusals write nothing", func(t *testing.T) {
		before := count(t, sh.harness, `SELECT count(*) FROM payment_intents`)
		reader, other := uuid.New(), uuid.New()
		sh.accounts.Grant(reader, tenant, "KE", "KES", app.PermBillingRead)
		for name, tc := range map[string]struct {
			user, tenant uuid.UUID
			amount       int64
			phone        string
			want         error
		}{
			"billing:read can't start one": {reader, tenant, 150000, payer, app.ErrPermissionDenied},
			"a stranger":                   {other, tenant, 150000, payer, app.ErrNotFound},
			"cents":                        {user, tenant, 150050, payer, app.ErrValidation},
			"a foreign number":             {user, tenant, 150000, "+255708374149", app.ErrValidation},
		} {
			_, err := sh.svc.StartTopup(ctx, tc.user, tc.tenant, uuid.NewString(), tc.amount, tc.phone)
			if !errors.Is(err, tc.want) {
				t.Errorf("%s: got %v, want %v", name, err, tc.want)
			}
		}
		if after := count(t, sh.harness, `SELECT count(*) FROM payment_intents`); after != before {
			t.Fatal("a refused top-up was written")
		}
	})
	t.Run("a number the rail refuses fails the top-up", func(t *testing.T) {
		top, err := sh.svc.StartTopup(ctx, user, tenant, uuid.NewString(), 150000, "+254208374149") // a landline
		wantErr(t, err, app.ErrValidation)
		_ = top
		if n := count(t, sh.harness, `SELECT count(*) FROM payment_intents WHERE tenant_id = $1 AND status = 'failed' AND error_code = 'VAL-4000'`, tenant); n != 1 {
			t.Fatal("the refused push isn't recorded as failed")
		}
	})
	t.Run("reading", func(t *testing.T) {
		top := sh.start(t, user, tenant, 150000)
		reader := uuid.New()
		sh.accounts.Grant(reader, tenant, "KE", "KES", app.PermBillingRead)
		if _, err := sh.svc.GetTopup(ctx, reader, tenant, top.ID); err != nil {
			t.Fatalf("billing:read reads: %v", err)
		}
		otherUser, otherTenant := sh.client()
		_, err := sh.svc.GetTopup(ctx, otherUser, otherTenant, top.ID)
		wantErr(t, err, app.ErrNotFound)
	})
}

func TestSTKTopup_PushTimeout(t *testing.T) {
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	t.Cleanup(hang.Close)
	var sh *stkHarness
	sh = newSTKHarness(t, func(d *app.Deps) {
		d.Rails = map[string]app.RailClient{"mpesa_ke": adapters.NewDaraja(adapters.DarajaConfig{
			BaseURL: hang.URL, Cache: testkit.StartRedis(t), Credentials: app.NewCredentialSource(d.Pool, d.KMS),
			Location: daraja.EAT, Clock: clock.Real{}, HTTPClient: &http.Client{Timeout: 200 * time.Millisecond},
		})}
	})
	user, tenant := sh.client()
	top := sh.start(t, user, tenant, 150000)
	if top.Status != "failed" || top.ErrorCode != "PAY-6502" {
		t.Fatalf("got %s %s, want failed PAY-6502", top.Status, top.ErrorCode)
	}
	assertEventValid(t, sh.harness, "payment_intent.failed", top.ID)
}

func TestSTKCallback_Refused(t *testing.T) {
	sh := newSTKHarness(t)
	ctx := context.Background()
	user, tenant := sh.client()
	sh.daraja.Next(daraja.Outcome{Delivery: daraja.Manual})
	top := sh.start(t, user, tenant, 150000)
	body := stkCallbackBody("m", sh.checkout(t, top.ID), "FORGED0001", 150000, sh.clk.Now())
	token := domain.CallbackToken(callbackSecret, sh.paybill)
	local := netip.MustParseAddr("127.0.0.1")

	for name, call := range map[string]func() error{
		"a wrong token":   func() error { return sh.svc.HandleSTKCallback(ctx, sh.paybill, strings.Repeat("0", 64), local, body) },
		"another account": func() error { return sh.svc.HandleSTKCallback(ctx, uuid.New(), token, local, body) },
		"an outside address": func() error {
			return sh.svc.HandleSTKCallback(ctx, sh.paybill, token, netip.MustParseAddr("8.8.8.8"), body)
		},
		"no address": func() error { return sh.svc.HandleSTKCallback(ctx, sh.paybill, token, netip.Addr{}, body) },
	} {
		wantErr(t, call(), app.ErrBadCallback)
		_ = name
	}
	wantErr(t, sh.svc.HandleSTKCallback(ctx, sh.paybill, token, local, []byte(`{"Body":{}}`)), app.ErrValidation)
	if n := count(t, sh.harness, `SELECT count(*) FROM stk_callbacks`); n != 0 {
		t.Fatalf("%d refused callbacks were recorded", n)
	}
	if sh.get(t, user, tenant, top.ID).Status != "pending" || sh.ledger.Credited(tenant) != 0 {
		t.Fatal("a refused callback changed the top-up")
	}
	var dummy map[string]any
	_ = json.Unmarshal(body, &dummy)
}
