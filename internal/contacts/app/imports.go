package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/ids"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/uploads"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/contacts/domain"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/contacts/store"
)

// Import is an import as the API shows it.
type Import struct {
	ID                    uuid.UUID
	Status                domain.ImportStatus
	FileName, ContentType string
	GroupID               *uuid.UUID
	SizeBytes             *int64
	RowsTotal             *int
	RowsValid, RowsFailed int
	UploadExpiresAt       time.Time
	FinishedAt, DeletedAt *time.Time
	DeleteAfter           time.Time
	HasErrorReport        bool
	CreatedAt             time.Time
}

// ImportInput starts an import.
type ImportInput struct {
	FileName, ContentType string
	GroupID               *uuid.UUID
}

// ImportError is one rejected row or column.
type ImportError struct {
	Row              int
	Field, Code, Msg string
}

// Download is a file a download link opens; the caller closes Body.
type Download struct {
	Body                  io.ReadCloser
	ContentType, FileName string
}

func toImport(r store.ContactsImport) Import {
	imp := Import{
		ID: r.ID, Status: domain.ImportStatus(r.Status), FileName: r.FileName, ContentType: r.ContentType,
		RowsValid: int(r.RowsValid), RowsFailed: int(r.RowsFailed), UploadExpiresAt: r.UploadExpiresAt,
		DeleteAfter: r.DeleteAfter, HasErrorReport: r.ErrorReportKey.Valid, CreatedAt: r.CreatedAt,
	}
	if r.GroupID.Valid {
		g := uuid.UUID(r.GroupID.Bytes)
		imp.GroupID = &g
	}
	if r.SizeBytes.Valid {
		imp.SizeBytes = &r.SizeBytes.Int64
	}
	if r.RowsTotal.Valid {
		n := int(r.RowsTotal.Int32)
		imp.RowsTotal = &n
	}
	if r.FinishedAt.Valid {
		imp.FinishedAt = &r.FinishedAt.Time
	}
	if r.DeletedAt.Valid {
		imp.DeletedAt = &r.DeletedAt.Time
	}
	return imp
}

// CreateImport starts an import and signs its upload form (data model
// §8, flow step 1). The file name is only shown back; keys come from IDs.
func (s *Service) CreateImport(ctx context.Context, user, tenant uuid.UUID, in ImportInput) (Import, uploads.Upload, error) {
	if _, err := s.requireTenant(ctx, user, tenant, PermManage); err != nil {
		return Import{}, uploads.Upload{}, err
	}
	if t := uploads.ContentType(in.ContentType); t != uploads.CSV && t != uploads.XLSX {
		return Import{}, uploads.Upload{}, fmt.Errorf("%w: content_type is text/csv or the XLSX type", ErrValidation)
	}
	if in.FileName == "" || utf8.RuneCountInString(in.FileName) > 255 || strings.ContainsFunc(in.FileName, unicode.IsControl) {
		return Import{}, uploads.Upload{}, fmt.Errorf("%w: file_name is 1 to 255 characters", ErrValidation)
	}
	id, err := ids.New()
	if err != nil {
		return Import{}, uploads.Upload{}, err
	}
	up, err := s.uploads.IssueUpload(ctx, tenant, id)
	if err != nil {
		return Import{}, uploads.Upload{}, err
	}
	params := store.CreateImportParams{
		ID: id, TenantID: tenant, FileName: in.FileName, ContentType: in.ContentType,
		ObjectKey: uploads.FileKey(uploads.Transient, tenant, id), UploadExpiresAt: up.ExpiresAt,
		DeleteAfter: up.DeleteAfter, CreatedBy: user,
	}
	if in.GroupID != nil {
		params.GroupID = pgtype.UUID{Bytes: *in.GroupID, Valid: true}
	}
	var row store.ContactsImport
	err = s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		if in.GroupID != nil {
			if _, err := q.GetGroup(ctx, *in.GroupID); err != nil {
				return notFound(err, "group")
			}
		}
		var err error
		row, err = q.CreateImport(ctx, params)
		return err
	})
	if err != nil {
		return Import{}, uploads.Upload{}, err
	}
	return toImport(row), up, nil
}

// ConfirmImportUpload moves an import to scanning, for the worker.
func (s *Service) ConfirmImportUpload(ctx context.Context, user, tenant, id uuid.UUID) (Import, error) {
	if _, err := s.requireTenant(ctx, user, tenant, PermManage); err != nil {
		return Import{}, err
	}
	var row store.ContactsImport
	err := s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		var err error
		row, err = q.ConfirmImportUpload(ctx, store.ConfirmImportUploadParams{ID: id, At: s.clk.Now()})
		if errors.Is(err, pgx.ErrNoRows) {
			if _, gerr := q.GetImport(ctx, id); gerr != nil {
				return notFound(gerr, "import")
			}
			return ErrImportState
		}
		return err
	})
	return toImport(row), err
}

// GetImport reads one import with its progress.
func (s *Service) GetImport(ctx context.Context, user, tenant, id uuid.UUID) (Import, error) {
	if _, err := s.requireTenant(ctx, user, tenant, PermRead); err != nil {
		return Import{}, err
	}
	var row store.ContactsImport
	err := s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		row, err = store.New(tx).GetImport(ctx, id)
		return notFound(err, "import")
	})
	return toImport(row), err
}

// ListImports pages the account's imports, newest first.
func (s *Service) ListImports(ctx context.Context, user, tenant uuid.UUID, page Page) ([]Import, string, error) {
	if _, err := s.requireTenant(ctx, user, tenant, PermRead); err != nil {
		return nil, "", err
	}
	size, err := page.size()
	if err != nil {
		return nil, "", err
	}
	after, afterID, err := parseTimeCursor(page.Cursor)
	if err != nil {
		return nil, "", err
	}
	params := store.ListImportsParams{PageSize: size + 1}
	if after != nil {
		params.AfterCreated = pgtype.Timestamptz{Time: *after, Valid: true}
		params.AfterID = pgtype.UUID{Bytes: afterID, Valid: true}
	}
	var rows []store.ContactsImport
	err = s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		rows, err = store.New(tx).ListImports(ctx, params)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(rows) > int(size) {
		rows = rows[:size]
		next = timeCursor(rows[len(rows)-1].CreatedAt, rows[len(rows)-1].ID)
	}
	out := make([]Import, len(rows))
	for i, r := range rows {
		out[i] = toImport(r)
	}
	return out, next, nil
}

type errorCursor struct {
	Row   int32  `json:"r"`
	Field string `json:"f"`
	Code  string `json:"c"`
}

// ListImportErrors pages an import's errors by row.
func (s *Service) ListImportErrors(ctx context.Context, user, tenant, id uuid.UUID, page Page) ([]ImportError, string, error) {
	if _, err := s.requireTenant(ctx, user, tenant, PermRead); err != nil {
		return nil, "", err
	}
	size, err := page.size()
	if err != nil {
		return nil, "", err
	}
	params := store.ListImportErrorsPageParams{ImportID: id, PageSize: size + 1}
	if page.Cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(page.Cursor)
		var c errorCursor
		if err != nil || json.Unmarshal(raw, &c) != nil {
			return nil, "", fmt.Errorf("%w: bad cursor", ErrValidation)
		}
		params.AfterRow = pgtype.Int4{Int32: c.Row, Valid: true}
		params.AfterField = pgtype.Text{String: c.Field, Valid: true}
		params.AfterCode = pgtype.Text{String: c.Code, Valid: true}
	}
	var rows []store.ContactsImportError
	err = s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		if _, err := q.GetImport(ctx, id); err != nil {
			return notFound(err, "import")
		}
		var err error
		rows, err = q.ListImportErrorsPage(ctx, params)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(rows) > int(size) {
		rows = rows[:size]
		last := rows[len(rows)-1]
		raw, _ := json.Marshal(errorCursor{Row: last.RowNo, Field: last.Field, Code: last.ErrorCode})
		next = base64.RawURLEncoding.EncodeToString(raw)
	}
	out := make([]ImportError, len(rows))
	for i, r := range rows {
		out[i] = ImportError{Row: int(r.RowNo), Field: r.Field, Code: r.ErrorCode, Msg: r.Message}
	}
	return out, next, nil
}

// CreateDownloadLink signs a 5-minute link to an import's original file
// or its error report, answered by OpenDownload (ADR 0012: storage holds
// only ciphertext, so files come back through the service).
func (s *Service) CreateDownloadLink(ctx context.Context, user, tenant, id uuid.UUID, errorReport bool) (string, time.Time, error) {
	if _, err := s.requireTenant(ctx, user, tenant, PermRead); err != nil {
		return "", time.Time{}, err
	}
	var row store.ContactsImport
	err := s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		row, err = store.New(tx).GetImport(ctx, id)
		return notFound(err, "import")
	})
	if err != nil {
		return "", time.Time{}, err
	}
	if row.DeletedAt.Valid || !s.clk.Now().Before(row.DeleteAfter) {
		return "", time.Time{}, ErrFileDeleted
	}
	key := row.ObjectKey
	switch {
	case errorReport && !row.ErrorReportKey.Valid:
		return "", time.Time{}, fmt.Errorf("%w: this import has no error report", ErrNotFound)
	case errorReport:
		key = row.ErrorReportKey.String
	case !row.SizeBytes.Valid && !row.FinishedAt.Valid:
		return "", time.Time{}, fmt.Errorf("%w: no file yet", ErrNotFound)
	}
	token, expires, err := s.uploads.SignDownload(tenant, key)
	if err != nil {
		return "", time.Time{}, err
	}
	return s.baseURL + "/v1/contacts/downloads/" + token, expires, nil
}

// OpenDownload answers a download link: ErrNotFound for a link that
// isn't valid or has expired, ErrFileDeleted once the file is gone.
func (s *Service) OpenDownload(ctx context.Context, token string) (Download, error) {
	d, err := s.uploads.VerifyDownload(token)
	if err != nil {
		return Download{}, fmt.Errorf("%w: download link", ErrNotFound)
	}
	var row store.ContactsImport
	err = s.pool.WithTenantTx(ctx, d.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		row, err = store.New(tx).GetImportByObjectKey(ctx, d.Key)
		return notFound(err, "file")
	})
	if err != nil {
		return Download{}, err
	}
	if row.DeletedAt.Valid {
		return Download{}, ErrFileDeleted
	}
	body, err := s.uploads.Open(ctx, d.Key)
	if errors.Is(err, uploads.ErrDeleted) {
		return Download{}, ErrFileDeleted
	}
	if err != nil {
		return Download{}, err
	}
	if row.ErrorReportKey.Valid && d.Key == row.ErrorReportKey.String {
		return Download{Body: body, ContentType: string(uploads.CSV), FileName: strings.TrimSuffix(row.FileName, extOf(row.FileName)) + "-errors.csv"}, nil
	}
	return Download{Body: body, ContentType: row.ContentType, FileName: row.FileName}, nil
}

func extOf(name string) string {
	if i := strings.LastIndexByte(name, '.'); i > 0 {
		return name[i:]
	}
	return ""
}
