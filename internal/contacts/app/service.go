// Package app holds contacts' use cases: validate through domain/, then
// write through store/ inside one database transaction.
package app

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/clock"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/packs"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/uploads"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/contacts/adapters"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/contacts/domain"
)

// systemRole claims imports and sweeps their files across clients
// (migration 20261117090000).
const systemRole = "contacts_system"

// Permissions in accounts' catalogue (task 6.2, OQ-26).
const (
	PermRead   = "contacts:read"
	PermManage = "contacts:manage"
)

var (
	// ErrValidation wraps every rejected input (VAL-4000).
	ErrValidation = errors.New("app: validation failed")
	// ErrNotFound: no such account, contact, group or import, or no
	// access to it (VAL-4003).
	ErrNotFound = errors.New("app: not found")
	// ErrPermissionDenied: the user's role lacks the permission
	// (AUTH-4024).
	ErrPermissionDenied = errors.New("app: your role doesn't allow this")
	// ErrPhone: the number isn't valid (VAL-4020); it wraps the
	// domain's reason.
	ErrPhone = errors.New("app: phone number not valid")
	// ErrValue: a field or attribute value doesn't suit it, or names an
	// unknown attribute (VAL-4021).
	ErrValue = errors.New("app: field value not valid")
	// ErrImportState: the import isn't awaiting its upload, or its form
	// expired (VAL-4023).
	ErrImportState = errors.New("app: the import isn't awaiting its upload")
	// ErrFileDeleted: the import's files are gone (VAL-4024).
	ErrFileDeleted = errors.New("app: the file has been deleted")
)

// Deps are the Service's dependencies.
type Deps struct {
	Pool   *db.Pool
	Clock  clock.Clock
	Logger *slog.Logger
	// Accounts answers access checks; nil fails them closed.
	Accounts adapters.Accounts
	// Packs are the country packs by country code: an account's reads
	// its numbers.
	Packs map[string]*packs.Pack
	// Uploads stores imported files (platform/uploads, D-23).
	Uploads *uploads.Uploads
	// DownloadBaseURL is the public base download links start with:
	// the gateway's public URL.
	DownloadBaseURL string
	// BatchSize is how many rows an import writes per transaction
	// (1,000 by default, data model §10).
	BatchSize int
	// MaxRows is the most data rows one import reads
	// (domain.MaxImportRows by default).
	MaxRows int
	// AfterBatch, if set, runs after each committed batch of an import;
	// an error stops the import there, as a worker dying would. Tests
	// only.
	AfterBatch func(importID uuid.UUID, batch int) error
}

// Service holds contacts' use cases.
type Service struct {
	pool       *db.Pool
	clk        clock.Clock
	log        *slog.Logger
	accounts   adapters.Accounts
	packs      map[string]*packs.Pack
	uploads    *uploads.Uploads
	baseURL    string
	batchSize  int
	afterBatch func(uuid.UUID, int) error
	maxRows    int
}

// New builds a Service.
func New(d Deps) *Service {
	if d.Clock == nil {
		d.Clock = clock.Real{}
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.BatchSize <= 0 {
		d.BatchSize = 1000
	}
	if d.MaxRows <= 0 {
		d.MaxRows = domain.MaxImportRows
	}
	return &Service{
		pool: d.Pool, clk: d.Clock, log: d.Logger, accounts: d.Accounts, packs: d.Packs,
		uploads: d.Uploads, baseURL: strings.TrimSuffix(d.DownloadBaseURL, "/"), batchSize: d.BatchSize, afterBatch: d.AfterBatch, maxRows: d.MaxRows,
	}
}

// requireTenant asks accounts, on every call, whether user may act in
// tenant with perm, and returns the account's country. Accounts being
// down fails closed.
func (s *Service) requireTenant(ctx context.Context, user, tenant uuid.UUID, perm string) (string, error) {
	if s.accounts == nil {
		return "", errors.New("app: no accounts adapter for access checks")
	}
	info, err := s.accounts.TenantAccess(ctx, user, tenant, perm)
	switch {
	case errors.Is(err, adapters.ErrNoTenantAccess):
		return "", fmt.Errorf("%w: no account %s", ErrNotFound, tenant)
	case errors.Is(err, adapters.ErrPermissionDenied):
		return "", fmt.Errorf("%w: %s", ErrPermissionDenied, perm)
	case err != nil:
		return "", fmt.Errorf("app: checking account access: %w", err)
	}
	return info.CountryCode, nil
}

// phoneRules are the numbering rules of country's pack.
func (s *Service) phoneRules(country string) (domain.PhoneRules, error) {
	p, ok := s.packs[country]
	if !ok {
		return domain.PhoneRules{}, fmt.Errorf("app: no pack for country %q", country)
	}
	return domain.PhoneRules{
		CountryCode: p.Numbering.CountryCode, TrunkPrefix: p.Numbering.TrunkPrefix,
		NSNLength: p.Numbering.NationalSignificantNumberLength, MobilePrefixes: p.Numbering.MobilePrefixes,
	}, nil
}

// Page asks for one page of a list.
type Page struct {
	Cursor string // the next_cursor of the page before; "" for the first
	Limit  int    // 1 to 100; 50 when 0
}

func (p Page) size() (int32, error) {
	switch {
	case p.Limit == 0:
		return 50, nil
	case p.Limit < 1 || p.Limit > 100:
		return 0, fmt.Errorf("%w: limit must be 1 to 100", ErrValidation)
	}
	return int32(p.Limit), nil //nolint:gosec // G115: checked to be at most 100
}

// timeCursor encodes a (created_at, id) position.
func timeCursor(at time.Time, id uuid.UUID) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(at.UnixNano(), 10) + "." + id.String()))
}

func parseTimeCursor(c string) (*time.Time, uuid.UUID, error) {
	if c == "" {
		return nil, uuid.Nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return nil, uuid.Nil, fmt.Errorf("%w: bad cursor", ErrValidation)
	}
	ns, idText, ok := strings.Cut(string(raw), ".")
	n, nerr := strconv.ParseInt(ns, 10, 64)
	id, ierr := uuid.Parse(idText)
	if !ok || nerr != nil || ierr != nil {
		return nil, uuid.Nil, fmt.Errorf("%w: bad cursor", ErrValidation)
	}
	at := time.Unix(0, n).UTC()
	return &at, id, nil
}
