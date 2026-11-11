package domain

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ErrBadStatement means the file isn't the org-portal statement export,
// or a row can't be read (VAL-4000). The error names the row.
var ErrBadStatement = errors.New("domain: not a statement")

// StatementHeader is the M-Pesa org portal's statement export, as
// assumed until an anonymised export confirms it (OQ-22).
var StatementHeader = []string{
	"Receipt No.", "Completion Time", "Initiation Time", "Details", "Transaction Status",
	"Paid In", "Withdrawn", "Balance", "Balance Confirmed", "Reason Type",
	"Other Party Info", "Linked Transaction ID", "A/C No.",
}

const statementTimeFmt = "02-01-2006 15:04:05"

var shillingsPattern = regexp.MustCompile(`^\d{1,15}(\.\d{1,2})?$`)

// StatementLine is one row of the statement.
type StatementLine struct {
	Row         int // the row in the file; the header is row 1
	Receipt     string
	At          time.Time // completion time, UTC
	Direction   byte      // 'I' paid in, 'O' withdrawn
	AmountMinor int64
	Completed   bool
	Details     string
	AccountNo   string
}

// ParseOrgStatement reads a statement export (task 4.6), its times in
// loc, the rail's country's time zone. Amounts are read as decimal
// text into minor units, never through a float.
func ParseOrgStatement(data []byte, loc *time.Location) ([]StatementLine, error) {
	r := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))))
	r.FieldsPerRecord = -1
	header, err := r.Read()
	if err != nil || !slices.Equal(header, StatementHeader) {
		return nil, fmt.Errorf("%w: row 1: the header isn't the org portal's statement export", ErrBadStatement)
	}
	var out []StatementLine
	for row := 2; ; row++ {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil || len(rec) != len(StatementHeader) {
			return nil, fmt.Errorf("%w: row %d: %d columns, want %d", ErrBadStatement, row, len(rec), len(StatementHeader))
		}
		line, err := statementLine(row, rec, loc)
		if err != nil {
			return nil, err
		}
		out = append(out, line)
	}
}

func statementLine(row int, rec []string, loc *time.Location) (StatementLine, error) {
	bad := func(why string) (StatementLine, error) {
		return StatementLine{}, fmt.Errorf("%w: row %d: %s", ErrBadStatement, row, why)
	}
	l := StatementLine{
		Row: row, Receipt: strings.TrimSpace(rec[0]), Details: rec[3],
		Completed: strings.TrimSpace(rec[4]) == "Completed", AccountNo: strings.TrimSpace(rec[12]),
	}
	if l.Receipt == "" {
		return bad("no receipt")
	}
	at, err := time.ParseInLocation(statementTimeFmt, strings.TrimSpace(rec[1]), loc)
	if err != nil {
		return bad("the completion time isn't " + statementTimeFmt)
	}
	l.At = at.UTC()
	in, out := strings.TrimSpace(rec[5]), strings.TrimSpace(rec[6])
	switch {
	case in != "" && out == "":
		l.Direction = 'I'
		l.AmountMinor, err = shillingsToMinor(in)
	case out != "" && in == "":
		l.Direction = 'O'
		l.AmountMinor, err = shillingsToMinor(out)
	default:
		return bad("one of Paid In and Withdrawn must be given")
	}
	if err != nil {
		return bad(err.Error())
	}
	return l, nil
}

// shillingsToMinor reads "1500", "1,500.5" or "1500.00" as minor units.
func shillingsToMinor(s string) (int64, error) {
	s = strings.ReplaceAll(s, ",", "")
	if !shillingsPattern.MatchString(s) {
		return 0, fmt.Errorf("amount %q isn't money", s)
	}
	whole, frac, _ := strings.Cut(s, ".")
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("amount %q is too large", s)
	}
	f, _ := strconv.ParseInt((frac + "00")[:2], 10, 64)
	return w*100 + f, nil
}

// DayBounds is the local day date names (YYYY-MM-DD) in loc, as UTC
// instants: [start, end).
func DayBounds(date string, loc *time.Location) (time.Time, time.Time, error) {
	d, err := time.ParseInLocation(time.DateOnly, date, loc)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("%w: %q isn't a date (YYYY-MM-DD)", ErrBadStatement, date)
	}
	return d.UTC(), d.AddDate(0, 0, 1).UTC(), nil
}

// DayNotice is a payment VASPRO recorded with the day's paid_at.
type DayNotice struct {
	ID          uuid.UUID
	Receipt     string
	AmountMinor int64
}

// ClearingEntry is one line on the rail's provider clearing account in
// the ledger.
type ClearingEntry struct {
	EntryID       int64
	TransactionID uuid.UUID
	Direction     string // D or C
	AmountMinor   int64
	ReferenceType string
	ReferenceID   uuid.UUID
	PostedAt      time.Time
}

// ReconInput is one day's three sources. Clearing covers the day and
// the next (a late sweep posts after midnight); Known holds the notices
// from other days that clearing debits in the day refer to.
type ReconInput struct {
	DayStart, DayEnd time.Time
	Lines            []StatementLine
	Notices          []DayNotice
	Clearing         []ClearingEntry
	Known            map[uuid.UUID]bool
}

// LineMatch is what a statement line matched: matched (NoticeID set),
// unmatched, or ignored (not a completed payment in, outside this task).
type LineMatch struct {
	Row      int
	Status   string
	NoticeID *uuid.UUID
}

// Discrepancy is one exception to open (recon_exceptions.kind).
type Discrepancy struct {
	Kind          string
	Row           int // the statement line, if any
	NoticeID      *uuid.UUID
	TransactionID *uuid.UUID // a ledger transaction, for a stray clearing line
}

// ReconResult is a day's reconciliation.
type ReconResult struct {
	Lines      []LineMatch
	Exceptions []Discrepancy
}

// Reconcile compares a day's statement, VASPRO's notices and provider
// clearing in the ledger (task 4.6, data model §7). Every completed
// payment in on the statement must be a notice with the same amount,
// and every notice of the day a statement line; every notice must have
// exactly its amount debited to provider clearing, by reference; and
// every clearing debit posted in the day must be a notice's. Money out
// (withdrawals, pay-backs) is Phase 13's: recorded, not flagged.
func Reconcile(in ReconInput) ReconResult {
	var res ReconResult
	byReceipt := make(map[string]DayNotice, len(in.Notices))
	for _, n := range in.Notices {
		byReceipt[n.Receipt] = n
	}
	onStatement := map[string]bool{}
	for _, l := range in.Lines {
		if !l.Completed || l.Direction != 'I' {
			res.Lines = append(res.Lines, LineMatch{Row: l.Row, Status: "ignored"})
			continue
		}
		if onStatement[l.Receipt] {
			res.Lines = append(res.Lines, LineMatch{Row: l.Row, Status: "unmatched"})
			res.Exceptions = append(res.Exceptions, Discrepancy{Kind: "duplicate", Row: l.Row})
			continue
		}
		onStatement[l.Receipt] = true
		n, ok := byReceipt[l.Receipt]
		if !ok {
			res.Lines = append(res.Lines, LineMatch{Row: l.Row, Status: "unmatched"})
			res.Exceptions = append(res.Exceptions, Discrepancy{Kind: "missing_in_vaspro", Row: l.Row})
			continue
		}
		id := n.ID
		res.Lines = append(res.Lines, LineMatch{Row: l.Row, Status: "matched", NoticeID: &id})
		if l.AmountMinor != n.AmountMinor {
			res.Exceptions = append(res.Exceptions, Discrepancy{Kind: "amount_mismatch", Row: l.Row, NoticeID: &id})
		}
	}
	for _, n := range in.Notices {
		if !onStatement[n.Receipt] {
			id := n.ID
			res.Exceptions = append(res.Exceptions, Discrepancy{Kind: "missing_on_rail", NoticeID: &id})
		}
	}

	debited := map[uuid.UUID]int64{}
	dayNotice := make(map[uuid.UUID]bool, len(in.Notices))
	for _, n := range in.Notices {
		dayNotice[n.ID] = true
	}
	for _, e := range in.Clearing {
		if e.Direction != "D" {
			continue
		}
		if e.ReferenceType == "collection_notice" {
			debited[e.ReferenceID] += e.AmountMinor
		}
		inDay := !e.PostedAt.Before(in.DayStart) && e.PostedAt.Before(in.DayEnd)
		if inDay && (e.ReferenceType != "collection_notice" || (!dayNotice[e.ReferenceID] && !in.Known[e.ReferenceID])) {
			txn := e.TransactionID
			res.Exceptions = append(res.Exceptions, Discrepancy{Kind: "ledger_mismatch", TransactionID: &txn})
		}
	}
	for _, n := range in.Notices {
		if debited[n.ID] != n.AmountMinor {
			id := n.ID
			res.Exceptions = append(res.Exceptions, Discrepancy{Kind: "ledger_mismatch", NoticeID: &id})
		}
	}
	return res
}
