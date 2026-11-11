package domain_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments/domain"
)

var eat = time.FixedZone("EAT", 3*60*60)

const header = `"Receipt No.","Completion Time","Initiation Time","Details","Transaction Status","Paid In","Withdrawn","Balance","Balance Confirmed","Reason Type","Other Party Info","Linked Transaction ID","A/C No."`

func TestParseOrgStatement(t *testing.T) {
	csv := header + "\n" +
		`RKTQDM7W6S,11-11-2026 09:15:00,11-11-2026 09:15:00,Pay Bill from 254708374149,Completed,1500.00,,1500.00,true,,254708374149 - Test Payer,,ABC23456` + "\n" +
		`RKTQDM7W6T,11-11-2026 10:00:00,11-11-2026 10:00:00,Pay Bill,Failed,200.00,,1500.00,true,,,,` + "\n" +
		`RKTQDM7W6U,11-11-2026 11:30:05,11-11-2026 11:30:05,Charge,Completed,,30.50,1469.50,true,,,,` + "\n"
	lines, err := domain.ParseOrgStatement([]byte(csv), eat)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 3 {
		t.Fatalf("%d lines", len(lines))
	}
	l := lines[0]
	if l.Row != 2 || l.Receipt != "RKTQDM7W6S" || l.Direction != 'I' || l.AmountMinor != 150000 || !l.Completed || l.AccountNo != "ABC23456" ||
		!l.At.Equal(time.Date(2026, 11, 11, 6, 15, 0, 0, time.UTC)) {
		t.Fatalf("line 1 %+v", l)
	}
	if lines[1].Completed || lines[2].Direction != 'O' || lines[2].AmountMinor != 3050 {
		t.Fatalf("lines 2 and 3 %+v %+v", lines[1], lines[2])
	}

	for name, tc := range map[string]struct {
		csv string
		row string
	}{
		"another export":     {`Receipt,Time,Amount` + "\n" + `X,1,2`, "row 1"},
		"empty":              {"", "row 1"},
		"a bad amount":       {header + "\n" + `R1,11-11-2026 09:15:00,,x,Completed,15OO,,,,,,,`, "row 2"},
		"cents beyond two":   {header + "\n" + `R1,11-11-2026 09:15:00,,x,Completed,1.005,,,,,,,`, "row 2"},
		"a bad time":         {header + "\n" + `R1,yesterday,,x,Completed,10.00,,,,,,,`, "row 2"},
		"both in and out":    {header + "\n" + `R1,11-11-2026 09:15:00,,x,Completed,10.00,5.00,,,,,,`, "row 2"},
		"neither in nor out": {header + "\n" + `R1,11-11-2026 09:15:00,,x,Completed,,,,,,,,`, "row 2"},
		"no receipt":         {header + "\n" + `,11-11-2026 09:15:00,,x,Completed,10.00,,,,,,,`, "row 2"},
	} {
		_, err := domain.ParseOrgStatement([]byte(tc.csv), eat)
		if !errors.Is(err, domain.ErrBadStatement) || !strings.Contains(err.Error(), tc.row) {
			t.Errorf("%s: %v, want ErrBadStatement naming %s", name, err, tc.row)
		}
	}
}

func TestDayBounds(t *testing.T) {
	start, end, err := domain.DayBounds("2026-11-11", eat)
	if err != nil {
		t.Fatal(err)
	}
	if !start.Equal(time.Date(2026, 11, 10, 21, 0, 0, 0, time.UTC)) || !end.Equal(time.Date(2026, 11, 11, 21, 0, 0, 0, time.UTC)) {
		t.Fatalf("bounds %v %v", start, end)
	}
	for _, bad := range []string{"", "2026-11-31", "11-11-2026", "2026-11-11T00:00:00Z"} {
		if _, _, err := domain.DayBounds(bad, eat); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// TestReconcile: every exception kind, and a clean day.
func TestReconcile(t *testing.T) {
	start := time.Date(2026, 11, 10, 21, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	in := start.Add(time.Hour)
	n1, n2 := uuid.New(), uuid.New()
	line := func(row int, receipt string, amount int64) domain.StatementLine {
		return domain.StatementLine{Row: row, Receipt: receipt, At: in, Direction: 'I', AmountMinor: amount, Completed: true}
	}
	clearing := func(notice uuid.UUID, amount int64, at time.Time) domain.ClearingEntry {
		return domain.ClearingEntry{EntryID: 1, TransactionID: uuid.New(), Direction: "D", AmountMinor: amount, ReferenceType: "collection_notice", ReferenceID: notice, PostedAt: at}
	}
	clean := domain.ReconInput{
		DayStart: start, DayEnd: end,
		Lines:    []domain.StatementLine{line(2, "R1", 1000), line(3, "R2", 2000)},
		Notices:  []domain.DayNotice{{ID: n1, Receipt: "R1", AmountMinor: 1000}, {ID: n2, Receipt: "R2", AmountMinor: 2000}},
		Clearing: []domain.ClearingEntry{clearing(n1, 1000, in), clearing(n2, 2000, end.Add(2*time.Hour))}, // the second posted late, the next day
		Known:    map[uuid.UUID]bool{},
	}
	kinds := func(r domain.ReconResult) string {
		var out []string
		for _, e := range r.Exceptions {
			out = append(out, e.Kind)
		}
		return strings.Join(out, ",")
	}

	r := domain.Reconcile(clean)
	if len(r.Exceptions) != 0 || r.Lines[0].NoticeID == nil || *r.Lines[0].NoticeID != n1 || r.Lines[1].Status != "matched" {
		t.Fatalf("a clean day: %+v", r)
	}

	for name, tc := range map[string]struct {
		mutate func(*domain.ReconInput)
		want   string
	}{
		"a line VASPRO has no notice for": {func(in *domain.ReconInput) {
			in.Lines = append(in.Lines, line(4, "R9", 500))
		}, "missing_in_vaspro"},
		"a notice the rail doesn't have": {func(in *domain.ReconInput) { in.Lines = in.Lines[:1] }, "missing_on_rail"},
		"amounts disagree":               {func(in *domain.ReconInput) { in.Lines[1].AmountMinor = 2500 }, "amount_mismatch"},
		"a receipt twice on the statement": {func(in *domain.ReconInput) {
			in.Lines = append(in.Lines, line(4, "R2", 2000))
		}, "duplicate"},
		"a notice the ledger never posted": {func(in *domain.ReconInput) { in.Clearing = in.Clearing[:1] }, "ledger_mismatch"},
		"the ledger posted another amount": {func(in *domain.ReconInput) { in.Clearing[0].AmountMinor = 999 }, "ledger_mismatch"},
		"a clearing debit for no notice": {func(in *domain.ReconInput) {
			in.Clearing = append(in.Clearing, clearing(uuid.New(), 700, start.Add(2*time.Hour)))
		}, "ledger_mismatch"},
		"a clearing debit that isn't a notice's": {func(in *domain.ReconInput) {
			e := clearing(uuid.New(), 700, start.Add(2*time.Hour))
			e.ReferenceType = "reversal"
			in.Clearing = append(in.Clearing, e)
		}, "ledger_mismatch"},
	} {
		c := clean
		c.Lines = append([]domain.StatementLine{}, clean.Lines...)
		c.Clearing = append([]domain.ClearingEntry{}, clean.Clearing...)
		tc.mutate(&c)
		if got := kinds(domain.Reconcile(c)); got != tc.want {
			t.Errorf("%s: %q, want %q", name, got, tc.want)
		}
	}

	t.Run("outside this task: not flagged", func(t *testing.T) {
		c := clean
		c.Lines = append(append([]domain.StatementLine{}, clean.Lines...),
			domain.StatementLine{Row: 4, Receipt: "W1", At: in, Direction: 'O', AmountMinor: 300, Completed: true},
			domain.StatementLine{Row: 5, Receipt: "F1", At: in, Direction: 'I', AmountMinor: 300, Completed: false})
		other := uuid.New()
		c.Clearing = append(append([]domain.ClearingEntry{}, clean.Clearing...),
			domain.ClearingEntry{TransactionID: uuid.New(), Direction: "C", AmountMinor: 300, ReferenceType: "reversal", ReferenceID: uuid.New(), PostedAt: in},
			clearing(other, 400, in)) // a notice from another day, known
		c.Known = map[uuid.UUID]bool{other: true}
		r := domain.Reconcile(c)
		if len(r.Exceptions) != 0 || r.Lines[2].Status != "ignored" || r.Lines[3].Status != "ignored" {
			t.Fatalf("got %+v", r)
		}
	})
}
