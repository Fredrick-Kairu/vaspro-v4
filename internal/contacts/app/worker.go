package app

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/events"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/ids"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/outbox"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/problem"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/uploads"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/contacts/domain"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/contacts/store"
)

// claimBatch is how many imports one ProcessImports pass looks at.
const claimBatch = 10

// ProcessImports works on imports that are scanning, validating or
// processing (data model §8), each under a session advisory lock so two
// workers never take one import, and returns how many moved on. An
// import whose scanner is down is left for the next pass. Errors are
// logged, never returned for one import, so the others still go.
func (s *Service) ProcessImports(ctx context.Context) (int, error) {
	var todo []store.ListImportsToProcessRow
	if err := s.pool.WithRoleTx(ctx, systemRole, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		todo, err = store.New(tx).ListImportsToProcess(ctx, claimBatch)
		return err
	}); err != nil {
		return 0, fmt.Errorf("app: listing imports to process: %w", err)
	}
	moved := 0
	for _, imp := range todo {
		ok, err := s.withImportLock(ctx, imp.ID, func() (bool, error) { return s.processImport(ctx, imp.ID, imp.TenantID) })
		if err != nil {
			s.log.ErrorContext(ctx, "contacts: processing an import", "import_id", imp.ID, "tenant_id", imp.TenantID, "error", err)
		}
		if ok {
			moved++
		}
	}
	return moved, nil
}

// withImportLock runs fn holding id's advisory lock on one connection;
// if another worker holds it, it does nothing. The lock ends with the
// connection's session, so a worker that dies releases it.
func (s *Service) withImportLock(ctx context.Context, id uuid.UUID, fn func() (bool, error)) (bool, error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Release()
	q := store.New(conn)
	locked, err := q.TryLockImport(ctx, id)
	if err != nil || !locked {
		return false, err
	}
	defer func() {
		if _, err := q.UnlockImport(context.WithoutCancel(ctx), id); err != nil {
			// A lock that can't be released dies with the session.
			_ = conn.Conn().Close(context.WithoutCancel(ctx))
		}
	}()
	return fn()
}

// processImport moves one import on as far as it can: scanning, then
// validating, then processing, to a final status.
func (s *Service) processImport(ctx context.Context, id, tenant uuid.UUID) (bool, error) {
	moved := false
	for {
		imp, err := s.loadImport(ctx, tenant, id)
		if err != nil {
			return moved, err
		}
		var step bool
		switch domain.ImportStatus(imp.Status) {
		case domain.Scanning:
			step, err = s.scanImport(ctx, imp)
		case domain.Validating:
			step, err = s.validateImport(ctx, imp)
		case domain.Processing:
			step, err = s.processRows(ctx, imp)
		default:
			return moved, nil
		}
		moved = moved || step
		if err != nil || !step {
			return moved, err
		}
	}
}

func (s *Service) loadImport(ctx context.Context, tenant, id uuid.UUID) (store.ContactsImport, error) {
	var row store.ContactsImport
	err := s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		row, err = store.New(tx).GetImport(ctx, id)
		return err
	})
	return row, err
}

// fileFailures maps platform/uploads' refusals to the code an import
// fails with.
var fileFailures = []struct {
	err  error
	code problem.Code
	msg  string
}{
	{uploads.ErrWrongType, problem.VAL4007, "the file's content isn't the declared CSV or XLSX"},
	{uploads.ErrTooLarge, problem.VAL4008, "the file is over 20 MB"},
	{uploads.ErrEmpty, problem.VAL4008, "the file is empty"},
	{uploads.ErrInfected, problem.VAL4009, "the file contains malware and was deleted"},
	{uploads.ErrChanged, problem.VAL4022, "the file changed while it was being checked; upload it again"},
	{uploads.ErrNotUploaded, problem.VAL4022, "no file was uploaded"},
}

// scanImport is §8's scanning step: platform/uploads checks, scans and
// encrypts the file. A scanner that can't answer leaves it for later.
func (s *Service) scanImport(ctx context.Context, imp store.ContactsImport) (bool, error) {
	file, err := s.uploads.Finalize(ctx, uploads.Transient, imp.TenantID, imp.ID, uploads.ContentType(imp.ContentType))
	switch {
	case errors.Is(err, uploads.ErrScanUnavailable):
		s.log.WarnContext(ctx, "contacts: an import waits for the virus scanner", "import_id", imp.ID, "error", err)
		return false, nil
	case errors.Is(err, uploads.ErrAlreadyFinalized):
		// Stored by a worker that stopped before recording it.
	case err != nil:
		for _, f := range fileFailures {
			if errors.Is(err, f.err) {
				return true, s.failImport(ctx, imp, f.code, f.msg)
			}
		}
		return false, err
	}
	params := store.SetImportScannedParams{ID: imp.ID}
	if file.Size > 0 {
		params.SizeBytes = pgtype.Int8{Int64: file.Size, Valid: true}
		params.Sha256 = file.SHA256
	}
	return true, s.pool.WithTenantTx(ctx, imp.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		return store.New(tx).SetImportScanned(ctx, params)
	})
}

// openImport opens the stored file's rows and matches its header.
func (s *Service) openImport(ctx context.Context, imp store.ContactsImport) (rowReader, io.Closer, domain.HeaderMap, []string, []int, error) {
	body, err := s.uploads.Open(ctx, imp.ObjectKey)
	if err != nil {
		return nil, nil, domain.HeaderMap{}, nil, nil, err
	}
	fail := func(err error) (rowReader, io.Closer, domain.HeaderMap, []string, []int, error) {
		_ = body.Close()
		return nil, nil, domain.HeaderMap{}, nil, nil, err
	}
	rows, err := openRows(imp.ContentType, body)
	if err != nil {
		return fail(err)
	}
	header, err := rows.Next()
	if errors.Is(err, io.EOF) {
		return fail(fmt.Errorf("%w: the file has no header row", errUnreadable))
	}
	if err != nil {
		return fail(err)
	}
	var defs map[string]domain.AttributeDef
	if err := s.pool.WithTenantTx(ctx, imp.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		defs, err = loadDefs(ctx, store.New(tx))
		return err
	}); err != nil {
		return fail(err)
	}
	list := make([]domain.AttributeDef, 0, len(defs))
	for _, d := range defs {
		list = append(list, d)
	}
	m, unknown, err := domain.MatchHeaders(header, list)
	if err != nil {
		return fail(fmt.Errorf("%w: %v", errUnreadable, err))
	}
	return rows, body, m, header, unknown, nil
}

// validateImport is §8's validating step: headers matched to fields.
// Columns that match nothing are reported (row 1) and ignored.
func (s *Service) validateImport(ctx context.Context, imp store.ContactsImport) (bool, error) {
	_, closer, _, header, unknown, err := s.openImport(ctx, imp)
	if errors.Is(err, errUnreadable) {
		return true, s.failImport(ctx, imp, problem.VAL4022, err.Error())
	}
	if err != nil {
		return false, err
	}
	_ = closer.Close()
	return true, s.pool.WithTenantTx(ctx, imp.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		for _, col := range unknown {
			name := header[col]
			if utf8Len := []rune(name); len(utf8Len) > 100 {
				name = string(utf8Len[:100])
			}
			if err := q.InsertImportError(ctx, store.InsertImportErrorParams{
				ImportID: imp.ID, TenantID: imp.TenantID, RowNo: 1, Field: name, ErrorCode: string(problem.VAL4021),
				Message: "not a contact field or one of the account's attributes; the column was ignored",
			}); err != nil {
				return err
			}
		}
		return q.SetImportProcessing(ctx, imp.ID)
	})
}

// batchRow is one contact for ImportContacts, merged within the batch.
type batchRow struct {
	ID         uuid.UUID                  `json:"id"`
	Phone      string                     `json:"phone"`
	FirstName  string                     `json:"first_name"`
	LastName   string                     `json:"last_name"`
	Email      string                     `json:"email"`
	Attributes map[string]json.RawMessage `json:"attributes"`
}

// merge applies a later row: its non-empty cells win (data model §10).
func (b *batchRow) merge(later batchRow) {
	for _, f := range []struct {
		dst *string
		src string
	}{
		{&b.FirstName, later.FirstName}, {&b.LastName, later.LastName}, {&b.Email, later.Email},
	} {
		if f.src != "" {
			*f.dst = f.src
		}
	}
	for k, v := range later.Attributes {
		b.Attributes[k] = v
	}
}

type batch struct {
	rows    []batchRow
	byPhone map[string]int
	errs    []store.InsertImportErrorParams
	valid   int
	failed  int
	read    int
}

func newBatch() *batch { return &batch{byPhone: map[string]int{}} }

// processRows is §8's processing step: rows read as a stream, written in
// batches, each batch and its counts in one transaction. A resumed
// import skips the rows already counted (rows_total).
func (s *Service) processRows(ctx context.Context, imp store.ContactsImport) (bool, error) {
	country, err := s.tenantCountry(ctx, imp)
	if err != nil {
		return false, err
	}
	rules, err := s.phoneRules(country)
	if err != nil {
		return false, err
	}
	rows, closer, hm, _, _, err := s.openImport(ctx, imp)
	if errors.Is(err, errUnreadable) {
		return true, s.failImport(ctx, imp, problem.VAL4022, err.Error())
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = closer.Close() }()

	group := s.importGroup(ctx, imp)
	skip := int(imp.RowsTotal.Int32)
	dataRow, batchNo := 0, 0
	b := newBatch()
	overLimit := false
	for {
		rec, err := rows.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return true, s.failImport(ctx, imp, problem.VAL4022, fmt.Sprintf("row %d: %v", dataRow+2, err))
		}
		dataRow++
		if dataRow <= skip {
			continue
		}
		if dataRow > s.maxRows {
			overLimit = true
			break
		}
		s.readRow(b, imp, rules, hm, dataRow+1, rec)
		if b.read == s.batchSize {
			batchNo++
			if err := s.writeBatch(ctx, imp, group, b); err != nil {
				return true, err
			}
			if s.afterBatch != nil {
				if err := s.afterBatch(imp.ID, batchNo); err != nil {
					return true, err
				}
			}
			b = newBatch()
		}
	}
	if err := s.writeBatch(ctx, imp, group, b); err != nil {
		return true, err
	}
	return true, s.finishImport(ctx, imp.TenantID, imp.ID, overLimit)
}

// tenantCountry is the country whose pack reads an import's numbers.
// The worker acts for no user, so it can't ask accounts' access check;
// with one pack loaded (Kenya's, until OD-02 adds regions) it's that
// one. A second country needs an accounts call for an account's
// country, or the country stored with the import.
func (s *Service) tenantCountry(_ context.Context, _ store.ContactsImport) (string, error) {
	if len(s.packs) != 1 {
		return "", errors.New("app: imports read numbers with the one loaded pack; more than one needs the account's country")
	}
	for code := range s.packs {
		return code, nil
	}
	return "", nil
}

// importGroup is the import's group, or nil if it's gone since.
func (s *Service) importGroup(ctx context.Context, imp store.ContactsImport) *uuid.UUID {
	if !imp.GroupID.Valid {
		return nil
	}
	id := uuid.UUID(imp.GroupID.Bytes)
	err := s.pool.WithTenantTx(ctx, imp.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := store.New(tx).GetGroup(ctx, id)
		return err
	})
	if err != nil {
		return nil
	}
	return &id
}

// readRow checks one data row (spreadsheet row rowNo) into the batch.
func (s *Service) readRow(b *batch, imp store.ContactsImport, rules domain.PhoneRules, hm domain.HeaderMap, rowNo int, rec []string) {
	b.read++
	cell := func(i int) string {
		if i < 0 || i >= len(rec) {
			return ""
		}
		return rec[i]
	}
	blank := true
	for _, c := range rec {
		if strings.TrimSpace(c) != "" {
			blank = false
			break
		}
	}
	if blank {
		return
	}
	fail := func(field string, code problem.Code, msg string) {
		b.errs = append(b.errs, store.InsertImportErrorParams{
			ImportID: imp.ID, TenantID: imp.TenantID, RowNo: int32(rowNo), Field: field, ErrorCode: string(code), Message: msg, //nolint:gosec // G115: at most MaxImportRows+1
		})
	}
	if !validUTF8(rec) {
		fail("", problem.VAL4021, "the row isn't UTF-8 text; save the file as CSV UTF-8")
		b.failed++
		return
	}
	before := len(b.errs)
	phone, err := domain.NormalizePhone(rules, cell(hm.Phone))
	if err != nil {
		fail("phone", problem.VAL4020, err.Error())
	}
	row := batchRow{Phone: phone, Attributes: map[string]json.RawMessage{}}
	for _, f := range []struct {
		name string
		col  int
		dst  *string
		in   domain.ContactFields
	}{
		{"first_name", hm.FirstName, &row.FirstName, domain.ContactFields{FirstName: cell(hm.FirstName)}},
		{"last_name", hm.LastName, &row.LastName, domain.ContactFields{LastName: cell(hm.LastName)}},
		{"email", hm.Email, &row.Email, domain.ContactFields{Email: cell(hm.Email)}},
	} {
		if f.col < 0 {
			continue
		}
		out, err := domain.NormalizeContactFields(f.in)
		if err != nil {
			fail(f.name, problem.VAL4021, err.Error())
			continue
		}
		*f.dst = out.FirstName + out.LastName + out.Email
	}
	for col, def := range hm.Attributes {
		v, err := domain.AttributeValue(def.Type, cell(col))
		if err != nil {
			fail(def.Key, problem.VAL4021, err.Error())
			continue
		}
		if v != nil {
			row.Attributes[def.Key] = v
		}
	}
	if len(b.errs) > before {
		b.failed++
		return
	}
	b.valid++
	if i, ok := b.byPhone[phone]; ok {
		b.rows[i].merge(row)
		return
	}
	row.ID = ids.MustNew()
	b.byPhone[phone] = len(b.rows)
	b.rows = append(b.rows, row)
}

// writeBatch writes a batch's contacts, memberships, errors and counts
// in one transaction, so a resumed import counts nothing twice.
func (s *Service) writeBatch(ctx context.Context, imp store.ContactsImport, group *uuid.UUID, b *batch) error {
	if b.read == 0 {
		return nil
	}
	return s.pool.WithTenantTx(ctx, imp.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		if len(b.rows) > 0 {
			raw, err := json.Marshal(b.rows)
			if err != nil {
				return err
			}
			if _, err := q.ImportContacts(ctx, store.ImportContactsParams{
				TenantID: imp.TenantID, CreatedBy: imp.CreatedBy, Batch: raw, At: s.clk.Now(),
			}); err != nil {
				return fmt.Errorf("app: writing a batch: %w", err)
			}
			if group != nil {
				phones := make([]string, len(b.rows))
				for i, r := range b.rows {
					phones[i] = r.Phone
				}
				if _, err := q.AddContactsToGroupByPhone(ctx, store.AddContactsToGroupByPhoneParams{GroupID: *group, Phones: phones}); err != nil {
					return err
				}
			}
		}
		for _, e := range b.errs {
			if err := q.InsertImportError(ctx, e); err != nil {
				return err
			}
		}
		return q.AddImportProgress(ctx, store.AddImportProgressParams{
			ID: imp.ID, Valid: int32(b.valid), Failed: int32(b.failed), Read: int32(b.read), //nolint:gosec // G115: at most a batch
		})
	})
}

// finishImport writes the error report, if there are errors, then the
// final status, the clock for deleting the files and the event.
func (s *Service) finishImport(ctx context.Context, tenant, id uuid.UUID, overLimit bool) error {
	if overLimit {
		if err := s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
			return store.New(tx).InsertImportError(ctx, store.InsertImportErrorParams{
				ImportID: id, TenantID: tenant, RowNo: 0, ErrorCode: string(problem.VAL4022),
				Message: fmt.Sprintf("the file has more than %d rows; the rest weren't read", s.maxRows),
			})
		}); err != nil {
			return err
		}
	}
	var n int64
	if err := s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		n, err = store.New(tx).CountImportErrors(ctx, id)
		return err
	}); err != nil {
		return err
	}
	status, report := domain.Completed, pgtype.Text{}
	if n > 0 {
		status = domain.CompletedWithErrors
		key := uploads.ErrorReportKey(uploads.Transient, tenant, id)
		if err := s.writeErrorReport(ctx, tenant, id, key); err != nil {
			return err
		}
		report = pgtype.Text{String: key, Valid: true}
	}
	return s.finish(ctx, tenant, id, status, report, "")
}

// failImport ends an import as failed, with a row-0 error saying why.
func (s *Service) failImport(ctx context.Context, imp store.ContactsImport, code problem.Code, msg string) error {
	if err := s.pool.WithTenantTx(ctx, imp.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		return store.New(tx).InsertImportError(ctx, store.InsertImportErrorParams{
			ImportID: imp.ID, TenantID: imp.TenantID, RowNo: 0, ErrorCode: string(code), Message: msg,
		})
	}); err != nil {
		return err
	}
	return s.finish(ctx, imp.TenantID, imp.ID, domain.Failed, pgtype.Text{}, code)
}

// finish sets a final status and publishes contact_import.finished, in
// one transaction (data model §8, flow step 4).
func (s *Service) finish(ctx context.Context, tenant, id uuid.UUID, status domain.ImportStatus, report pgtype.Text, code problem.Code) error {
	now := s.clk.Now()
	return s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		row, err := store.New(tx).FinishImport(ctx, store.FinishImportParams{
			ID: id, Status: store.ContactsImportStatus(status), ErrorReportKey: report,
			At: now, DeleteAfter: s.uploads.DeleteAfter(uploads.Transient, now),
		})
		if err != nil {
			return fmt.Errorf("app: finishing an import: %w", err)
		}
		return s.publishFinished(ctx, tx, row, code)
	})
}

func (s *Service) publishFinished(ctx context.Context, tx pgx.Tx, row store.ContactsImport, code problem.Code) error {
	data := map[string]any{
		"tenant_id": row.TenantID, "import_id": row.ID, "status": row.Status,
		"rows_total": row.RowsTotal.Int32, "rows_valid": row.RowsValid, "rows_failed": row.RowsFailed,
		"finished_at": row.FinishedAt.Time.UTC(), "delete_after": row.DeleteAfter.UTC(),
	}
	if code != "" {
		data["error_code"] = string(code)
	}
	env, err := events.New(ids.MustNew(), "contact_import.finished", 1, row.TenantID, row.FinishedAt.Time, "", data)
	if err != nil {
		return err
	}
	return outbox.Write(ctx, tx, "contact_import", row.ID, env)
}

// writeErrorReport streams the errors, a page at a time, into a CSV
// sealed at key. Cells a spreadsheet would run as a formula are
// prefixed with an apostrophe.
func (s *Service) writeErrorReport(ctx context.Context, tenant, id uuid.UUID, key string) error {
	pr, pw := io.Pipe()
	go func() {
		w := csv.NewWriter(pw)
		err := w.Write([]string{"row", "field", "code", "message"})
		params := store.ListImportErrorsPageParams{ImportID: id, PageSize: 5000}
		for err == nil {
			var page []store.ContactsImportError
			err = s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
				var err error
				page, err = store.New(tx).ListImportErrorsPage(ctx, params)
				return err
			})
			for _, e := range page {
				if err == nil {
					err = w.Write([]string{strconv.Itoa(int(e.RowNo)), safeCell(e.Field), e.ErrorCode, safeCell(e.Message)})
				}
			}
			if err != nil || len(page) < int(params.PageSize) {
				break
			}
			last := page[len(page)-1]
			params.AfterRow = pgtype.Int4{Int32: last.RowNo, Valid: true}
			params.AfterField = pgtype.Text{String: last.Field, Valid: true}
			params.AfterCode = pgtype.Text{String: last.ErrorCode, Valid: true}
		}
		w.Flush()
		if err == nil {
			err = w.Error()
		}
		_ = pw.CloseWithError(err)
	}()
	if err := s.uploads.PutSealed(ctx, key, pr); err != nil {
		_ = pr.CloseWithError(err)
		return fmt.Errorf("app: writing the error report: %w", err)
	}
	return nil
}

func safeCell(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

// ExpireImports ends imports whose upload form ran out unconfirmed;
// their files go 2 days on (data model §8).
func (s *Service) ExpireImports(ctx context.Context) (int, error) {
	now := s.clk.Now()
	var n int
	err := s.pool.WithRoleTx(ctx, systemRole, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := store.New(tx).ExpireImports(ctx, store.ExpireImportsParams{At: now, DeleteAfter: s.uploads.DeleteAfter(uploads.Transient, now)})
		if err != nil {
			return err
		}
		for _, r := range rows {
			if err := s.publishFinished(ctx, tx, r, ""); err != nil {
				return err
			}
		}
		n = len(rows)
		return nil
	})
	return n, err
}

// SweepImportFiles deletes the files of imports past delete_after, then
// marks them (data model §8, flow step 6); never one still processing.
func (s *Service) SweepImportFiles(ctx context.Context) (uploads.SweepResult, error) {
	return s.uploads.Sweep(ctx, sweepTarget{s}, 100)
}

type sweepTarget struct{ s *Service }

func (t sweepTarget) Due(ctx context.Context, now time.Time, limit int) ([]uploads.Due, error) {
	var due []uploads.Due
	err := t.s.pool.WithRoleTx(ctx, systemRole, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := store.New(tx).DueImportFiles(ctx, store.DueImportFilesParams{At: now, PageSize: int32(limit)}) //nolint:gosec // G115: a small page
		for _, r := range rows {
			keys := []string{uploads.QuarantineKey(r.TenantID, r.ID), r.ObjectKey}
			if r.ErrorReportKey.Valid {
				keys = append(keys, r.ErrorReportKey.String)
			}
			due = append(due, uploads.Due{ID: r.ID, Keys: keys})
		}
		return err
	})
	return due, err
}

func (t sweepTarget) MarkDeleted(ctx context.Context, id uuid.UUID, at time.Time) error {
	return t.s.pool.WithRoleTx(ctx, systemRole, func(ctx context.Context, tx pgx.Tx) error {
		return store.New(tx).MarkImportFilesDeleted(ctx, store.MarkImportFilesDeletedParams{ID: id, At: at})
	})
}
