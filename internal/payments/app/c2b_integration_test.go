//go:build integration

package app_test

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments/app"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments/domain"
	vasdb "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/daraja"
)

// c2bHarness is an stkHarness whose paybill has its C2B URLs registered
// with the Daraja fake, so fake.Pay reaches the service, plus a
// stepped-up staff member.
type c2bHarness struct {
	*stkHarness
	staffID, session uuid.UUID
}

func newC2BHarness(t *testing.T) *c2bHarness {
	t.Helper()
	sh := newSTKHarness(t)
	if err := sh.svc.RegisterC2B(context.Background(), "ops@example.com", sh.paybill); err != nil {
		t.Fatalf("RegisterC2B: %v", err)
	}
	staffID, err := sh.staff.StaffByEmail(context.Background(), "ops@example.com")
	if err != nil {
		t.Fatal(err)
	}
	session := uuid.New()
	sh.staff.StepUp(staffID, session)
	return &c2bHarness{stkHarness: sh, staffID: staffID, session: session}
}

// accountNumber is tenant's number on the paybill.
func (ch *c2bHarness) accountNumber(t *testing.T, user, tenant uuid.UUID) string {
	t.Helper()
	d, err := ch.svc.TopupDetails(context.Background(), user, tenant)
	if err != nil {
		t.Fatal(err)
	}
	return d.AccountNumber
}

func (ch *c2bHarness) pay(t *testing.T, billRef string, amount int64) daraja.C2BResult {
	t.Helper()
	res, err := ch.daraja.Pay(daraja.C2BPayment{ShortCode: darajaApp.ShortCode, BillRef: billRef, AmountMinor: amount, Phone: daraja.SandboxMSISDN})
	if err != nil || !res.Accepted || res.ConfirmationStatus != 200 {
		t.Fatalf("pay %q: %+v %v", billRef, res, err)
	}
	return res
}

func (ch *c2bHarness) notice(t *testing.T, receipt string) (id uuid.UUID, status string) {
	t.Helper()
	if err := ch.super.QueryRow(context.Background(), `SELECT id, match_status FROM collection_notices WHERE rail_receipt = $1`, receipt).Scan(&id, &status); err != nil {
		t.Fatalf("notice %s: %v", receipt, err)
	}
	return id, status
}

func TestC2B_Confirmations(t *testing.T) {
	ch := newC2BHarness(t)
	ctx := context.Background()
	user, tenant := ch.client()
	number := ch.accountNumber(t, user, tenant)

	t.Run("registration", func(t *testing.T) {
		if n := count(t, ch.harness, `SELECT count(*) FROM rail_accounts WHERE id = $1 AND c2b_registered_at IS NOT NULL`, ch.paybill); n != 1 {
			t.Fatal("c2b_registered_at isn't set")
		}
	})
	t.Run("a valid number credits the client", func(t *testing.T) {
		res := ch.pay(t, number, 100000)
		if _, status := ch.notice(t, res.TransID); status != "matched" || ch.ledger.Credited(tenant) != 100000 {
			t.Fatalf("status %s, credited %d", status, ch.ledger.Credited(tenant))
		}
	})
	t.Run("typed in lower case with spaces, it still credits the client", func(t *testing.T) {
		before := ch.ledger.Credited(tenant)
		ch.pay(t, " "+number[:4]+" "+fmt.Sprintf("%s", number[4:])+" ", 5000)
		if ch.ledger.Credited(tenant) != before+5000 {
			t.Fatal("not credited")
		}
	})
	t.Run("an unmatched payment lands in suspense", func(t *testing.T) {
		suspenseBefore := ch.ledger.Suspense()
		res := ch.pay(t, "INV-2026-0042", 70000)
		id, status := ch.notice(t, res.TransID)
		if status != "suspense" || ch.ledger.Suspense() != suspenseBefore+70000 {
			t.Fatalf("status %s, suspense %d", status, ch.ledger.Suspense())
		}
		if ev := assertEventValid(t, ch.harness, "topup.unmatched", id); ev["amount_minor"] != float64(70000) {
			t.Fatalf("event %v", ev)
		}
	})
	t.Run("a typo lands in suspense, never on the client it nearly matches", func(t *testing.T) {
		before := ch.ledger.Credited(tenant)
		typo := number[:5] + string(other(number[5])) + number[6:]
		res := ch.pay(t, typo, 30000)
		if _, status := ch.notice(t, res.TransID); status != "suspense" || ch.ledger.Credited(tenant) != before {
			t.Fatalf("status %s, credited %d (was %d)", status, ch.ledger.Credited(tenant), before)
		}
	})
	t.Run("a valid number nobody holds lands in suspense", func(t *testing.T) {
		unissued, err := domain.NewAccountNumber(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		res := ch.pay(t, unissued, 30000)
		if _, status := ch.notice(t, res.TransID); status != "suspense" {
			t.Fatalf("status %s", status)
		}
	})
	t.Run("a repeated confirmation is one notice and one posting", func(t *testing.T) {
		before, calls := ch.ledger.Credited(tenant), len(ch.ledger.Postings())
		res, err := ch.daraja.Pay(daraja.C2BPayment{ShortCode: darajaApp.ShortCode, BillRef: number, AmountMinor: 20000, Phone: daraja.SandboxMSISDN, Duplicate: true})
		if err != nil || !res.Accepted {
			t.Fatal(err)
		}
		if n := count(t, ch.harness, `SELECT count(*) FROM collection_notices WHERE rail_receipt = $1`, res.TransID); n != 1 ||
			ch.ledger.Credited(tenant) != before+20000 || len(ch.ledger.Postings()) != calls+1 {
			t.Fatalf("%d notices, credited %d, postings %d", n, ch.ledger.Credited(tenant)-before, len(ch.ledger.Postings())-calls)
		}
	})
	t.Run("an STK payment confirmed again by C2B credits once", func(t *testing.T) {
		u2, t2 := ch.client()
		top := ch.start(t, u2, t2, 150000)
		ch.daraja.Wait()
		got := ch.get(t, u2, t2, top.ID)
		body := c2bBody(got.Receipt, darajaApp.ShortCode, got.AccountReference, 150000, ch.clk.Now())
		token := domain.CallbackToken(callbackSecret, ch.paybill)
		if err := ch.svc.HandleC2BConfirmation(ctx, ch.paybill, token, netip.MustParseAddr("127.0.0.1"), body); err != nil {
			t.Fatal(err)
		}
		if ch.ledger.Credited(t2) != 150000 || count(t, ch.harness, `SELECT count(*) FROM collection_notices WHERE rail_receipt = $1`, got.Receipt) != 1 {
			t.Fatalf("credited %d", ch.ledger.Credited(t2))
		}
	})
	t.Run("validation accepts, refusing only another shortcode, and writes nothing", func(t *testing.T) {
		token := domain.CallbackToken(callbackSecret, ch.paybill)
		local := netip.MustParseAddr("127.0.0.1")
		before := count(t, ch.harness, `SELECT count(*) FROM collection_notices`)
		accept, _, err := ch.svc.HandleC2BValidation(ctx, ch.paybill, token, local, c2bBody("VAL0000001", darajaApp.ShortCode, "anything", 1000, ch.clk.Now()))
		if err != nil || !accept {
			t.Fatalf("an unknown reference: %v %v", accept, err)
		}
		accept, code, err := ch.svc.HandleC2BValidation(ctx, ch.paybill, token, local, c2bBody("VAL0000002", "600200", number, 1000, ch.clk.Now()))
		if err != nil || accept || code != "C2B00016" {
			t.Fatalf("another shortcode: %v %q %v", accept, code, err)
		}
		if after := count(t, ch.harness, `SELECT count(*) FROM collection_notices`); after != before {
			t.Fatal("validation wrote a notice")
		}
	})
	t.Run("a bad token or source is refused and nothing written", func(t *testing.T) {
		before := count(t, ch.harness, `SELECT count(*) FROM collection_notices`)
		body := c2bBody("BAD0000001", darajaApp.ShortCode, number, 1000, ch.clk.Now())
		token := domain.CallbackToken(callbackSecret, ch.paybill)
		wantErr(t, ch.svc.HandleC2BConfirmation(ctx, ch.paybill, "00", netip.MustParseAddr("127.0.0.1"), body), app.ErrBadCallback)
		wantErr(t, ch.svc.HandleC2BConfirmation(ctx, ch.paybill, token, netip.MustParseAddr("8.8.8.8"), body), app.ErrBadCallback)
		_, _, err := ch.svc.HandleC2BValidation(ctx, ch.paybill, token, netip.MustParseAddr("8.8.8.8"), body)
		wantErr(t, err, app.ErrBadCallback)
		if after := count(t, ch.harness, `SELECT count(*) FROM collection_notices`); after != before {
			t.Fatal("a refused confirmation was recorded")
		}
	})
}

// c2bBody is a C2B confirmation or validation body, built by hand.
func c2bBody(receipt, shortCode, billRef string, amountMinor int64, at time.Time) []byte {
	return fmt.Appendf(nil, `{"TransactionType":"Pay Bill","TransID":%q,"TransTime":%q,"TransAmount":"%d.%02d","BusinessShortCode":%q,"BillRefNumber":%q,"InvoiceNumber":"","OrgAccountBalance":"","ThirdPartyTransID":"","MSISDN":%q,"FirstName":"Test","MiddleName":"","LastName":"Payer"}`,
		receipt, at.In(daraja.EAT).Format("20060102150405"), amountMinor/100, amountMinor%100, shortCode, billRef, daraja.SandboxMSISDN)
}

// other is a different character of the account number alphabet.
func other(c byte) byte {
	for i := range len(domain.AccountNumberAlphabet) {
		if domain.AccountNumberAlphabet[i] != c {
			return domain.AccountNumberAlphabet[i]
		}
	}
	return c
}

func TestC2B_StaffMatch(t *testing.T) {
	ch := newC2BHarness(t)
	ctx := context.Background()
	user, tenant := ch.client()
	number := ch.accountNumber(t, user, tenant)

	res := ch.pay(t, "nothing useful", 80000)
	noticeID, _ := ch.notice(t, res.TransID)

	t.Run("refusals change nothing", func(t *testing.T) {
		outsider := uuid.New()
		_, err := ch.svc.MatchNotice(ctx, outsider, uuid.New(), noticeID, number, "payer called")
		wantErr(t, err, app.ErrNotStaff)
		_, err = ch.svc.MatchNotice(ctx, ch.staffID, uuid.New(), noticeID, number, "payer called")
		wantErr(t, err, app.ErrStepUpRequired)
		_, err = ch.svc.MatchNotice(ctx, ch.staffID, ch.session, noticeID, number, "  ")
		wantErr(t, err, app.ErrValidation)
		unissued, _ := domain.NewAccountNumber(rand.Reader)
		_, err = ch.svc.MatchNotice(ctx, ch.staffID, ch.session, noticeID, unissued, "payer called")
		wantErr(t, err, app.ErrNotFound)
		_, err = ch.svc.MatchNotice(ctx, ch.staffID, ch.session, uuid.New(), number, "payer called")
		wantErr(t, err, app.ErrNotFound)
		if _, status := ch.notice(t, res.TransID); status != "suspense" || ch.ledger.Credited(tenant) != 0 {
			t.Fatal("a refused match changed the payment")
		}
	})

	t.Run("a match credits the client once", func(t *testing.T) {
		n, err := ch.svc.MatchNotice(ctx, ch.staffID, ch.session, noticeID, number, "payer called; typed the wrong number")
		if err != nil || n.Status != "matched" || n.TenantID == nil || *n.TenantID != tenant {
			t.Fatalf("match: %+v %v", n, err)
		}
		if ch.ledger.Credited(tenant) != 80000 || ch.ledger.Suspense() != 0 {
			t.Fatalf("credited %d, suspense %d", ch.ledger.Credited(tenant), ch.ledger.Suspense())
		}
		if ev := assertEventValid(t, ch.harness, "collection_notice.matched", noticeID); ev["matched_by"] != ch.staffID.String() || ev["tenant_id"] != tenant.String() {
			t.Fatalf("event %v", ev)
		}
		_, err = ch.svc.MatchNotice(ctx, ch.staffID, ch.session, noticeID, number, "again")
		wantErr(t, err, app.ErrNoticeDecided)
	})

	t.Run("20 concurrent matches credit once", func(t *testing.T) {
		res := ch.pay(t, "", 40000)
		id, _ := ch.notice(t, res.TransID)
		var wg sync.WaitGroup
		var mu sync.Mutex
		won, decided := 0, 0
		// Open every pooled connection first: otherwise the first match
		// commits on the one idle connection while the rest are still
		// connecting, and the matches never overlap.
		warmPool(t, ch.app)
		start := make(chan struct{}) // released together, so the matches race
		for range 20 {
			wg.Go(func() {
				<-start
				_, err := ch.svc.MatchNotice(ctx, ch.staffID, ch.session, id, number, "payer called")
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err == nil:
					won++
				case errors.Is(err, app.ErrNoticeDecided):
					decided++
				default:
					t.Errorf("match: %v", err)
				}
			})
		}
		close(start)
		wg.Wait()
		if won != 1 || decided != 19 || ch.ledger.Credited(tenant) != 120000 || ch.ledger.Suspense() != 0 {
			t.Fatalf("won %d, decided %d, credited %d, suspense %d", won, decided, ch.ledger.Credited(tenant), ch.ledger.Suspense())
		}
		if n := count(t, ch.harness, `SELECT count(*) FROM outbox_events WHERE event_type = 'collection_notice.matched' AND aggregate_id = $1`, id); n != 1 {
			t.Fatalf("%d match events, want 1", n)
		}
	})

	t.Run("a match waits for the suspense posting", func(t *testing.T) {
		ch.ledger.Fail()
		res := ch.pay(t, "", 25000)
		id, _ := ch.notice(t, res.TransID)
		ch.ledger.Recover()
		_, err := ch.svc.MatchNotice(ctx, ch.staffID, ch.session, id, number, "payer called")
		wantErr(t, err, app.ErrSuspenseNotPosted)
		if sum := ch.sweep(t); sum.Posted != 1 {
			t.Fatalf("the sweep posted %d, want the suspense posting", sum.Posted)
		}
		if _, err := ch.svc.MatchNotice(ctx, ch.staffID, ch.session, id, number, "payer called"); err != nil {
			t.Fatal(err)
		}
		if ch.ledger.Credited(tenant) != 145000 || ch.ledger.Suspense() != 0 {
			t.Fatalf("credited %d, suspense %d", ch.ledger.Credited(tenant), ch.ledger.Suspense())
		}
	})

	t.Run("a matched payment the ledger missed is posted by the sweep, once", func(t *testing.T) {
		ch.ledger.Fail()
		ch.pay(t, number, 5000)
		ch.ledger.Recover()
		if ch.ledger.Credited(tenant) != 145000 {
			t.Fatal("credited while the ledger was down")
		}
		ch.sweep(t)
		ch.sweep(t)
		if ch.ledger.Credited(tenant) != 150000 {
			t.Fatalf("credited %d", ch.ledger.Credited(tenant))
		}
	})

	t.Run("the queue", func(t *testing.T) {
		for range 2 {
			ch.pay(t, "", 1000)
		}
		page, next, err := ch.svc.ListSuspense(ctx, ch.staffID, "", 1)
		if err != nil || len(page) != 1 || next == "" || page[0].Status != "suspense" {
			t.Fatalf("first page: %+v %q %v", page, next, err)
		}
		rest, last, err := ch.svc.ListSuspense(ctx, ch.staffID, next, 100)
		if err != nil || len(rest) != 1 || last != "" || !rest[0].PaidAt.After(page[0].PaidAt.Add(-time.Nanosecond)) || rest[0].ID == page[0].ID {
			t.Fatalf("second page: %+v %q %v", rest, last, err)
		}
		_, _, err = ch.svc.ListSuspense(ctx, uuid.New(), "", 10)
		wantErr(t, err, app.ErrNotStaff)
		_, _, err = ch.svc.ListSuspense(ctx, ch.staffID, "garbage", 10)
		wantErr(t, err, app.ErrValidation)
	})

	t.Run("identity down fails closed", func(t *testing.T) {
		ch.staff.Fail()
		_, _, err := ch.svc.ListSuspense(ctx, ch.staffID, "", 10)
		if err == nil {
			t.Fatal("identity down must not answer")
		}
	})
}

// warmPool opens every connection pool may hold.
func warmPool(t *testing.T, pool *vasdb.Pool) {
	t.Helper()
	ctx := context.Background()
	conns := make([]*pgxpool.Conn, 0, pool.Stat().MaxConns())
	for range pool.Stat().MaxConns() {
		c, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
	}
	for _, c := range conns {
		c.Release()
	}
}
