//go:build integration

package app_test

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments/adapters"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments/app"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments/domain"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/daraja"
)

// reconHarness is a c2bHarness whose Daraja fake and fake ledger run on
// the harness clock, so a "day" is the clock's day.
type reconHarness struct {
	*c2bHarness
}

func newReconHarness(t *testing.T) *reconHarness {
	t.Helper()
	ch := newC2BHarness(t)
	ch.daraja.SetNow(ch.clk.Now)
	ch.ledger.SetNow(ch.clk.Now)
	return &reconHarness{ch}
}

// day is the clock's local day in Nairobi, as the import names it.
func (rh *reconHarness) day() string { return rh.clk.Now().In(daraja.EAT).Format(time.DateOnly) }

func (rh *reconHarness) importDay(t *testing.T, statement []byte) (app.ImportSummary, error) {
	t.Helper()
	return rh.svc.ImportStatement(context.Background(), rh.staffID, rh.paybill, rh.day(), statement)
}

func (rh *reconHarness) statement(opts daraja.StatementOptions) []byte {
	return rh.daraja.Statement(darajaApp.ShortCode, rh.clk.Now(), opts)
}

// nextDay moves the clock to noon of the next local day.
func (rh *reconHarness) nextDay() {
	rh.clk.Advance(24 * time.Hour)
}

func (rh *reconHarness) exceptionKinds(t *testing.T, importID uuid.UUID) map[string]int {
	t.Helper()
	rows, err := rh.super.Query(context.Background(), `
		SELECT e.kind FROM recon_exceptions e
		LEFT JOIN statement_lines l ON l.id = e.statement_line_id
		WHERE e.opened_at >= now() - interval '1 hour' AND e.tenant_id IS NULL
		  AND (l.import_id = $1 OR e.statement_line_id IS NULL)`, importID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatal(err)
		}
		out[k]++
	}
	return out
}

// TestReconcile_DayOfFakeTraffic is Phase 4's exit gate: a day of every
// kind of traffic the Daraja fake can make, then the fake's own
// statement for the day, reconciles with zero exceptions and every
// statement line matched.
func TestReconcile_DayOfFakeTraffic(t *testing.T) {
	rh := newReconHarness(t)
	ctx := context.Background()
	u1, t1 := rh.client()
	u2, t2 := rh.client()
	number1 := rh.accountNumber(t, u1, t1)
	number2 := rh.accountNumber(t, u2, t2)

	// STK: a success, a duplicate callback, an early callback, a short
	// payment, a cancellation and insufficient funds.
	for _, o := range []daraja.Outcome{
		{}, {Delivery: daraja.DeliverTwice}, {Delivery: daraja.DeliverBeforeResponse},
		{PaidMinor: 90000}, {ResultCode: daraja.ResultCancelled}, {ResultCode: daraja.ResultInsufficientFunds},
	} {
		rh.daraja.Next(o)
		rh.start(t, u1, t1, 100000)
		rh.daraja.Wait()
		rh.clk.Advance(time.Minute)
	}
	// C2B: two clients paying their own numbers, one typed messily; one
	// payment to suspense matched by staff; one left in suspense.
	rh.pay(t, number1, 30000)
	rh.pay(t, " "+number2+" ", 45000)
	res := rh.pay(t, "", 40000)
	id, _ := rh.notice(t, res.TransID)
	if _, err := rh.svc.MatchNotice(ctx, rh.staffID, rh.session, id, number2, "payer called"); err != nil {
		t.Fatal(err)
	}
	rh.pay(t, "WRONG", 10000)
	rh.sweep(t)

	sum, err := rh.importDay(t, rh.statement(daraja.StatementOptions{}))
	if err != nil {
		t.Fatal(err)
	}
	// 4 STK successes (one short) and 4 C2B payments.
	if sum.Lines != 8 || sum.Matched != 8 || sum.Exceptions != 0 {
		t.Fatalf("a clean day: %+v, exceptions %v", sum, rh.exceptionKinds(t, sum.ID))
	}
	if n := count(t, rh.harness, `SELECT count(*) FROM statement_lines WHERE import_id = $1 AND match_status = 'matched' AND matched_type = 'collection_notice'`, sum.ID); n != 8 {
		t.Fatalf("%d lines matched to notices, want 8", n)
	}
	if ev := assertEventValid(t, rh.harness, "recon.statement_imported", sum.ID); ev["exceptions_opened"] != float64(0) {
		t.Fatalf("event %v", ev)
	}

	t.Run("a day is imported once", func(t *testing.T) {
		before := count(t, rh.harness, `SELECT count(*) FROM statement_lines`)
		_, err := rh.importDay(t, rh.statement(daraja.StatementOptions{}))
		wantErr(t, err, app.ErrStatementImported)
		if count(t, rh.harness, `SELECT count(*) FROM statement_lines`) != before {
			t.Fatal("a refused import wrote lines")
		}
	})
}

// statementCSV writes lines as the org portal's export.
func statementCSV(t *testing.T, lines ...[]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	if err := w.Write(domain.StatementHeader); err != nil {
		t.Fatal(err)
	}
	for _, l := range lines {
		if err := w.Write(l); err != nil {
			t.Fatal(err)
		}
	}
	w.Flush()
	return buf.Bytes()
}

func paidIn(receipt string, at time.Time, amountMinor int64) []string {
	stamp := at.In(daraja.EAT).Format("02-01-2006 15:04:05")
	return []string{receipt, stamp, stamp, "Pay Bill", "Completed", fmt.Sprintf("%d.%02d", amountMinor/100, amountMinor%100), "", "", "true", "", "", "", ""}
}

func TestReconcile_Exceptions(t *testing.T) {
	rh := newReconHarness(t)
	u, tenant := rh.client()
	number := rh.accountNumber(t, u, tenant)

	cases := []struct {
		name string
		day  func(t *testing.T) []byte
		want map[string]int
	}{
		{"a VASPRO record missing from the rail", func(t *testing.T) []byte {
			omitted := rh.pay(t, number, 10000)
			rh.pay(t, number, 20000)
			return rh.statement(daraja.StatementOptions{Omit: []string{omitted.TransID}})
		}, map[string]int{"missing_on_rail": 1}},
		{"a rail line missing from VASPRO", func(t *testing.T) []byte {
			rh.pay(t, number, 10000)
			return rh.statement(daraja.StatementOptions{Extra: []daraja.StatementLine{{Receipt: "EXTRA00001", At: rh.clk.Now(), PaidInMinor: 5000}}})
		}, map[string]int{"missing_in_vaspro": 1}},
		{"the amounts disagree", func(t *testing.T) []byte {
			res := rh.pay(t, number, 10000)
			return statementCSV(t, paidIn(res.TransID, rh.clk.Now(), 10100))
		}, map[string]int{"amount_mismatch": 1}},
		{"a payment the ledger never posted", func(t *testing.T) []byte {
			rh.ledger.Fail()
			rh.pay(t, number, 10000)
			rh.ledger.Recover()
			return rh.statement(daraja.StatementOptions{})
		}, map[string]int{"ledger_mismatch": 1}},
		{"a clearing line VASPRO has no record of", func(t *testing.T) []byte {
			rh.pay(t, number, 10000)
			rh.ledger.Inject(adapters.ClearingLine{TransactionID: uuid.New(), Direction: "D", AmountMinor: 777,
				ReferenceType: "collection_notice", ReferenceID: uuid.New(), PostedAt: rh.clk.Now()})
			return rh.statement(daraja.StatementOptions{})
		}, map[string]int{"ledger_mismatch": 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rh.nextDay()
			statement := tc.day(t)
			before := count(t, rh.harness, `SELECT count(*) FROM recon_exceptions WHERE tenant_id IS NULL`)
			sum, err := rh.importDay(t, statement)
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]int{}
			rows, err := rh.super.Query(context.Background(), `SELECT id, kind FROM recon_exceptions WHERE tenant_id IS NULL ORDER BY opened_at, id OFFSET $1`, before)
			if err != nil {
				t.Fatal(err)
			}
			var ids []uuid.UUID
			for rows.Next() {
				var id uuid.UUID
				var k string
				if err := rows.Scan(&id, &k); err != nil {
					t.Fatal(err)
				}
				got[k]++
				ids = append(ids, id)
			}
			rows.Close()
			if fmt.Sprint(got) != fmt.Sprint(tc.want) || sum.Exceptions != len(ids) {
				t.Fatalf("exceptions %v (summary %d), want %v", got, sum.Exceptions, tc.want)
			}
			for _, id := range ids {
				assertEventValid(t, rh.harness, "recon.exception_opened", id)
			}
		})
	}

	t.Run("a file that isn't the export writes nothing", func(t *testing.T) {
		rh.nextDay()
		before := count(t, rh.harness, `SELECT count(*) FROM statement_imports`)
		_, err := rh.importDay(t, []byte("Receipt,Amount\nX,1\n"))
		wantErr(t, err, app.ErrValidation)
		_, err = rh.svc.ImportStatement(context.Background(), rh.staffID, rh.paybill, "2026-13-01", rh.statement(daraja.StatementOptions{}))
		wantErr(t, err, app.ErrValidation)
		if count(t, rh.harness, `SELECT count(*) FROM statement_imports`) != before {
			t.Fatal("a refused import was recorded")
		}
	})
	t.Run("staff only", func(t *testing.T) {
		_, err := rh.svc.ImportStatement(context.Background(), uuid.New(), rh.paybill, rh.day(), rh.statement(daraja.StatementOptions{}))
		wantErr(t, err, app.ErrNotStaff)
		_, err = rh.svc.ImportStatement(context.Background(), rh.staffID, uuid.New(), rh.day(), rh.statement(daraja.StatementOptions{}))
		wantErr(t, err, app.ErrNotFound)
	})

	t.Run("listing and resolving", func(t *testing.T) {
		ctx := context.Background()
		open, _, err := rh.svc.ListReconExceptions(ctx, rh.staffID, "open", "", 100)
		if err != nil || len(open) == 0 {
			t.Fatalf("open exceptions: %d %v", len(open), err)
		}
		first, next, err := rh.svc.ListReconExceptions(ctx, rh.staffID, "open", "", 1)
		if err != nil || len(first) != 1 || next == "" {
			t.Fatalf("a page of one: %+v %q %v", first, next, err)
		}
		target := first[0].ID
		_, err = rh.svc.ResolveReconException(ctx, rh.staffID, uuid.New(), target, "resolved", "explained")
		wantErr(t, err, app.ErrStepUpRequired)
		_, err = rh.svc.ResolveReconException(ctx, rh.staffID, rh.session, target, "written_off", " ")
		wantErr(t, err, app.ErrValidation)
		_, err = rh.svc.ResolveReconException(ctx, rh.staffID, rh.session, target, "open", "explained")
		wantErr(t, err, app.ErrValidation)
		got, err := rh.svc.ResolveReconException(ctx, rh.staffID, rh.session, target, "written_off", "a test payment")
		if err != nil || got.Status != "written_off" || got.ResolvedBy == nil || *got.ResolvedBy != rh.staffID || got.ResolvedAt == nil {
			t.Fatalf("resolve: %+v %v", got, err)
		}
		_, err = rh.svc.ResolveReconException(ctx, rh.staffID, rh.session, target, "resolved", "again")
		wantErr(t, err, app.ErrExceptionClosed)
		_, err = rh.svc.ResolveReconException(ctx, rh.staffID, rh.session, uuid.New(), "resolved", "x")
		wantErr(t, err, app.ErrNotFound)
		_, _, err = rh.svc.ListReconExceptions(ctx, uuid.New(), "", "", 10)
		wantErr(t, err, app.ErrNotStaff)
	})
}

// TestCheckStatements: a day with no statement by the due time
// (12:00 the next day) is reported once; a day with one never is.
func TestCheckStatements(t *testing.T) {
	rh := newReconHarness(t)
	ctx := context.Background()
	missing := func() int {
		return count(t, rh.harness, `SELECT count(*) FROM outbox_events WHERE event_type = 'recon.statement_missing'`)
	}
	// The harness clock is noon in Nairobi; move to 11:00 the next day.
	imported := rh.day()
	if _, err := rh.importDay(t, rh.statement(daraja.StatementOptions{})); err != nil {
		t.Fatal(err)
	}
	rh.clk.Advance(23 * time.Hour)
	if n, err := rh.svc.CheckStatements(ctx); err != nil || n != 0 || missing() != 0 {
		t.Fatalf("an imported day: %d %v", n, err)
	}
	// The next day isn't imported: before its due time, nothing; after,
	// one event, once.
	rh.clk.Advance(24 * time.Hour) // 11:00, two days on
	if n, err := rh.svc.CheckStatements(ctx); err != nil || n != 0 {
		t.Fatalf("before due: %d %v", n, err)
	}
	rh.clk.Advance(90 * time.Minute) // 12:30
	if n, err := rh.svc.CheckStatements(ctx); err != nil || n != 1 || missing() != 1 {
		t.Fatalf("after due: %d %v", n, err)
	}
	if n, err := rh.svc.CheckStatements(ctx); err != nil || n != 0 || missing() != 1 {
		t.Fatalf("again: %d %v", n, err)
	}
	var id uuid.UUID
	if err := rh.super.QueryRow(ctx, `SELECT aggregate_id FROM outbox_events WHERE event_type = 'recon.statement_missing'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	ev := assertEventValid(t, rh.harness, "recon.statement_missing", id)
	if want := rh.clk.Now().In(daraja.EAT).AddDate(0, 0, -1).Format(time.DateOnly); ev["date"] != want || ev["date"] == imported {
		t.Fatalf("event %v, want the unimported %s", ev, want)
	}
}
