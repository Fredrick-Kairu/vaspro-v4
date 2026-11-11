package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	platformevents "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/events"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/ids"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/outbox"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments/domain"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments/store"
)

var (
	// ErrStatementImported: the day's statement is already imported
	// (VAL-4017).
	ErrStatementImported = errors.New("app: statement already imported")
	// ErrExceptionClosed: the exception is already resolved or written
	// off (VAL-4014).
	ErrExceptionClosed = errors.New("app: the exception is already closed")
)

// DefaultStatementDue is when, after local midnight, yesterday's
// statement is due.
const DefaultStatementDue = 12 * time.Hour

// clearingLate is how long after the day its payments' postings are
// looked for: a sweep can post a late payment after midnight.
const clearingLate = 24 * time.Hour

// ImportSummary is what an import found.
type ImportSummary struct {
	ID                         uuid.UUID
	Lines, Matched, Exceptions int
}

// ReconException is an exception as staff see it.
type ReconException struct {
	ID              uuid.UUID
	RailAccountID   uuid.UUID
	Kind            string
	Status          string
	TenantID        *uuid.UUID
	StatementLineID *int64
	ReferenceType   string
	ReferenceID     *uuid.UUID
	Resolution      string
	ResolvedBy      *uuid.UUID
	OpenedAt        time.Time
	ResolvedAt      *time.Time
}

func reconExceptionOf(r store.PaymentsReconException) ReconException {
	e := ReconException{
		ID: r.ID, RailAccountID: r.RailAccountID, Kind: r.Kind, Status: r.Status,
		ReferenceType: r.ReferenceType.String, Resolution: r.Resolution.String, OpenedAt: r.OpenedAt,
	}
	if r.TenantID.Valid {
		id := uuid.UUID(r.TenantID.Bytes)
		e.TenantID = &id
	}
	if r.StatementLineID.Valid {
		e.StatementLineID = &r.StatementLineID.Int64
	}
	if r.ReferenceID.Valid {
		id := uuid.UUID(r.ReferenceID.Bytes)
		e.ReferenceID = &id
	}
	if r.ResolvedBy.Valid {
		id := uuid.UUID(r.ResolvedBy.Bytes)
		e.ResolvedBy = &id
	}
	if r.ResolvedAt.Valid {
		at := r.ResolvedAt.Time
		e.ResolvedAt = &at
	}
	return e
}

// ImportStatement imports one local day's statement for one of VASPRO's
// paybills and reconciles it against the day's payments and the
// ledger's provider clearing (task 4.6): every gap opens an exception,
// with recon.exception_opened, and the import ends with
// recon.statement_imported. A day is imported once (ErrStatementImported);
// a file that isn't the export is ErrValidation; either way nothing is
// recorded.
func (s *Service) ImportStatement(ctx context.Context, staff, railAccountID uuid.UUID, date string, csv []byte) (ImportSummary, error) {
	if err := s.requireStaffID(ctx, staff); err != nil {
		return ImportSummary{}, err
	}
	if s.ledger == nil {
		return ImportSummary{}, errors.New("app: no ledger to reconcile against")
	}
	var acct store.PaymentsRailAccount
	err := s.pool.WithRoleTx(ctx, systemRole, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		acct, err = store.New(tx).GetRailAccount(ctx, railAccountID)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: no VASPRO paybill %s", ErrNotFound, railAccountID)
		}
		return err
	})
	if err != nil {
		return ImportSummary{}, err
	}
	pack, err := s.railPack(acct.RailCode)
	if err != nil {
		return ImportSummary{}, err
	}
	loc, err := time.LoadLocation(pack.Timezone)
	if err != nil {
		return ImportSummary{}, fmt.Errorf("app: %s's time zone: %w", pack.CountryCode, err)
	}
	start, end, err := domain.DayBounds(date, loc)
	if err != nil {
		return ImportSummary{}, fmt.Errorf("%w: %w", ErrValidation, err)
	}
	lines, err := domain.ParseOrgStatement(csv, loc)
	if err != nil {
		return ImportSummary{}, fmt.Errorf("%w: %w", ErrValidation, err)
	}

	in, err := s.reconInput(ctx, acct, pack.Currency.Code, start, end)
	if err != nil {
		return ImportSummary{}, err
	}
	in.Lines = lines
	res := domain.Reconcile(in)

	var sum ImportSummary
	err = s.pool.WithRoleTx(ctx, systemRole, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		id, err := ids.New()
		if err != nil {
			return err
		}
		if _, err := q.CreateStatementImport(ctx, store.CreateStatementImportParams{
			ID: id, RailAccountID: acct.ID, PeriodStart: start, PeriodEnd: end, Source: "file",
		}); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return fmt.Errorf("%w: %s on %s", ErrStatementImported, date, acct.Number)
			}
			return fmt.Errorf("app: recording the import: %w", err)
		}
		sum.ID = id
		lineIDs := make(map[int]int64, len(lines))
		for i, l := range lines {
			m := res.Lines[i]
			p := store.InsertStatementLineParams{
				ImportID: id, RailReceipt: l.Receipt, Direction: string(l.Direction), AmountMinor: l.AmountMinor,
				OccurredAt: l.At, MatchStatus: m.Status,
			}
			if m.NoticeID != nil {
				p.MatchedType, p.MatchedID = text("collection_notice"), pgtype.UUID{Bytes: *m.NoticeID, Valid: true}
				sum.Matched++
			}
			lineID, err := q.InsertStatementLine(ctx, p)
			if err != nil {
				return fmt.Errorf("app: recording statement row %d: %w", l.Row, err)
			}
			lineIDs[l.Row] = lineID
		}
		sum.Lines = len(lines)
		for _, d := range res.Exceptions {
			if err := s.openReconException(ctx, tx, acct.ID, id, d, lineIDs); err != nil {
				return err
			}
		}
		sum.Exceptions = len(res.Exceptions)
		if err := q.FinishStatementImport(ctx, store.FinishStatementImportParams{ID: id, LineCount: int32(len(lines))}); err != nil { //nolint:gosec // a day's rows fit in int32
			return err
		}
		return s.writeReconEvent(ctx, tx, "recon.statement_imported", "statement_import", id, map[string]any{
			"import_id": id, "rail_account_id": acct.ID, "date": date, "lines": sum.Lines,
			"matched": sum.Matched, "exceptions_opened": sum.Exceptions, "imported_by": staff,
		})
	})
	if err != nil {
		return ImportSummary{}, err
	}
	s.log.InfoContext(ctx, "payments: statement reconciled", "import_id", sum.ID, "lines", sum.Lines,
		"matched", sum.Matched, "exceptions", sum.Exceptions)
	return sum, nil
}

// reconInput gathers the day's payments to the paybill and the ledger's
// provider clearing lines from the day's start to a day after its end,
// and which notices from other days in-day clearing lines refer to.
func (s *Service) reconInput(ctx context.Context, acct store.PaymentsRailAccount, currency string, start, end time.Time) (domain.ReconInput, error) {
	in := domain.ReconInput{DayStart: start, DayEnd: end, Known: map[uuid.UUID]bool{}}
	lines, err := s.ledger.ClearingEntries(ctx, acct.RailCode, currency, start, end.Add(clearingLate))
	if err != nil {
		return in, fmt.Errorf("app: reading provider clearing: %w", err)
	}
	err = s.pool.WithRoleTx(ctx, systemRole, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		notices, err := q.ListNoticesPaidBetween(ctx, store.ListNoticesPaidBetweenParams{RailAccountID: acct.ID, PaidFrom: start, PaidTo: end})
		if err != nil {
			return err
		}
		day := map[uuid.UUID]bool{}
		for _, n := range notices {
			in.Notices = append(in.Notices, domain.DayNotice{ID: n.ID, Receipt: n.RailReceipt, AmountMinor: n.AmountMinor})
			day[n.ID] = true
		}
		var others []uuid.UUID
		for _, l := range lines {
			in.Clearing = append(in.Clearing, domain.ClearingEntry{
				EntryID: l.EntryID, TransactionID: l.TransactionID, Direction: l.Direction, AmountMinor: l.AmountMinor,
				ReferenceType: l.ReferenceType, ReferenceID: l.ReferenceID, PostedAt: l.PostedAt,
			})
			if l.ReferenceType == "collection_notice" && !day[l.ReferenceID] {
				others = append(others, l.ReferenceID)
			}
		}
		if len(others) == 0 {
			return nil
		}
		known, err := q.ListNoticeIDsIn(ctx, others)
		for _, id := range known {
			in.Known[id] = true
		}
		return err
	})
	if err != nil {
		return in, fmt.Errorf("app: loading the day's payments: %w", err)
	}
	return in, nil
}

// openReconException records one discrepancy on VASPRO's paybill (no
// tenant) and writes recon.exception_opened.
func (s *Service) openReconException(ctx context.Context, tx pgx.Tx, railAccountID, importID uuid.UUID, d domain.Discrepancy, lineIDs map[int]int64) error {
	id, err := ids.New()
	if err != nil {
		return err
	}
	p := store.CreateReconExceptionParams{ID: id, RailAccountID: railAccountID, Kind: d.Kind}
	data := map[string]any{"exception_id": id, "rail_account_id": railAccountID, "kind": d.Kind, "statement_import_id": importID}
	if d.Row != 0 {
		p.StatementLineID = pgtype.Int8{Int64: lineIDs[d.Row], Valid: true}
		data["statement_line_id"] = lineIDs[d.Row]
	}
	switch {
	case d.NoticeID != nil:
		p.ReferenceType, p.ReferenceID = text("collection_notice"), pgtype.UUID{Bytes: *d.NoticeID, Valid: true}
	case d.TransactionID != nil:
		p.ReferenceType, p.ReferenceID = text("ledger_transaction"), pgtype.UUID{Bytes: *d.TransactionID, Valid: true}
	}
	if p.ReferenceID.Valid {
		data["reference_type"], data["reference_id"] = p.ReferenceType.String, uuid.UUID(p.ReferenceID.Bytes)
	}
	if _, err := store.New(tx).CreateReconException(ctx, p); err != nil {
		return fmt.Errorf("app: opening a %s exception: %w", d.Kind, err)
	}
	return s.writeReconEvent(ctx, tx, "recon.exception_opened", "recon_exception", id, data)
}

func (s *Service) writeReconEvent(ctx context.Context, tx pgx.Tx, eventType, aggregate string, aggregateID uuid.UUID, data map[string]any) error {
	eventID, err := ids.New()
	if err != nil {
		return err
	}
	env, err := platformevents.New(eventID, eventType, 1, uuid.Nil, s.clk.Now(), "", data)
	if err != nil {
		return fmt.Errorf("app: building %s: %w", eventType, err)
	}
	return outbox.Write(ctx, tx, aggregate, aggregateID, env)
}

// ListReconExceptions is the staff queue of exceptions, oldest first,
// optionally by status, paged by an opaque cursor.
func (s *Service) ListReconExceptions(ctx context.Context, staff uuid.UUID, status, cursor string, limit int) ([]ReconException, string, error) {
	if err := s.requireStaffID(ctx, staff); err != nil {
		return nil, "", err
	}
	switch status {
	case "", "open", "resolved", "written_off":
	default:
		return nil, "", fmt.Errorf("%w: status %q", ErrValidation, status)
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 100 {
		return nil, "", fmt.Errorf("%w: limit is at most 100", ErrValidation)
	}
	afterAt, afterID, err := decodeNoticeCursor(cursor)
	if err != nil {
		return nil, "", err
	}
	var rows []store.PaymentsReconException
	err = s.pool.WithRoleTx(ctx, operationsRole, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		rows, err = store.New(tx).ListReconExceptions(ctx, store.ListReconExceptionsParams{
			Status: status, AfterOpenedAt: afterAt, AfterID: afterID, MaxRows: int32(limit + 1), //nolint:gosec // limit is 1..100
		})
		return err
	})
	if err != nil {
		return nil, "", fmt.Errorf("app: listing exceptions: %w", err)
	}
	next := ""
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[len(rows)-1]
		next = encodeNoticeCursor(last.OpenedAt, last.ID)
	}
	out := make([]ReconException, len(rows))
	for i, r := range rows {
		out[i] = reconExceptionOf(r)
	}
	return out, next, nil
}

// ResolveReconException closes an open exception, resolved or written
// off, with how (task 4.6): staff with a fresh step-up.
func (s *Service) ResolveReconException(ctx context.Context, staff, session, id uuid.UUID, status, resolution string) (ReconException, error) {
	if err := s.requireStaffID(ctx, staff); err != nil {
		return ReconException{}, err
	}
	fresh, err := s.identity.IsStepUpFresh(ctx, staff, session)
	if err != nil {
		return ReconException{}, fmt.Errorf("app: checking step-up: %w", err)
	}
	if !fresh {
		return ReconException{}, ErrStepUpRequired
	}
	resolution = strings.TrimSpace(resolution)
	if (status != "resolved" && status != "written_off") || resolution == "" {
		return ReconException{}, fmt.Errorf("%w: resolved or written_off, with a resolution", ErrValidation)
	}
	var out ReconException
	err = s.pool.WithRoleTx(ctx, operationsRole, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		row, err := q.ResolveReconException(ctx, store.ResolveReconExceptionParams{
			ID: id, Status: status, Resolution: text(resolution), ResolvedBy: pgtype.UUID{Bytes: staff, Valid: true},
			ResolvedAt: pgtype.Timestamptz{Time: s.clk.Now(), Valid: true},
		})
		if errors.Is(err, pgx.ErrNoRows) {
			if _, gerr := q.GetReconException(ctx, id); errors.Is(gerr, pgx.ErrNoRows) {
				return fmt.Errorf("%w: no exception %s", ErrNotFound, id)
			}
			return ErrExceptionClosed
		}
		if err != nil {
			return fmt.Errorf("app: resolving the exception: %w", err)
		}
		out = reconExceptionOf(row)
		return nil
	})
	return out, err
}

// CheckStatements reports each active VASPRO paybill whose yesterday
// has no statement once its due time has passed (task 4.6):
// recon.statement_missing, once per paybill and day, so an unreconciled
// day is never silent. It returns how many it reported.
func (s *Service) CheckStatements(ctx context.Context) (int, error) {
	var paybills []store.PaymentsRailAccount
	err := s.pool.WithRoleTx(ctx, systemRole, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		paybills, err = store.New(tx).ListActiveTopupRailAccounts(ctx)
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("app: listing paybills: %w", err)
	}
	reported := 0
	for _, pb := range paybills {
		pack, err := s.railPack(pb.RailCode)
		if err != nil {
			return reported, err
		}
		loc, err := time.LoadLocation(pack.Timezone)
		if err != nil {
			return reported, err
		}
		now := s.clk.Now().In(loc)
		today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
		if now.Before(today.Add(s.stmtDue)) {
			continue
		}
		day := today.AddDate(0, 0, -1)
		date := day.Format(time.DateOnly)
		// One deterministic aggregate per paybill and day, so a second
		// check (on this replica or another) finds the first event.
		agg := uuid.NewSHA1(pb.ID, []byte(date))
		wrote := false
		err = s.pool.WithRoleTx(ctx, systemRole, func(ctx context.Context, tx pgx.Tx) error {
			q := store.New(tx)
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, agg.String()); err != nil {
				return err
			}
			exists, err := q.StatementImportExists(ctx, store.StatementImportExistsParams{RailAccountID: pb.ID, PeriodStart: day.UTC()})
			if err != nil || exists {
				return err
			}
			n, err := q.CountOutboxEvents(ctx, store.CountOutboxEventsParams{EventType: "recon.statement_missing", AggregateID: agg})
			if err != nil || n > 0 {
				return err
			}
			wrote = true
			return s.writeReconEvent(ctx, tx, "recon.statement_missing", "rail_account_day", agg, map[string]any{
				"rail_account_id": pb.ID, "date": date,
			})
		})
		if err != nil {
			return reported, fmt.Errorf("app: checking %s's statement: %w", date, err)
		}
		if wrote {
			reported++
			s.log.WarnContext(ctx, "payments: no statement imported", "rail_account_id", pb.ID, "date", date)
		}
	}
	return reported, nil
}
