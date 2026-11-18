// Package uploads is the mechanism for files clients upload (task 6.2a,
// D-23, data model §8, ADR 0012): signed upload forms to a quarantine
// prefix, the move from quarantine to an encrypted final object after
// type, size and virus checks, signed download links, and a sweeper
// that deletes files once their retention ends. It holds no table and
// no status rules: each service keeps its own upload table
// (contacts.imports first) and decides what each step means for it.
package uploads

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/clamav"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/clock"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/crypto"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/objectstore"
)

// Errors a service maps to an upload's status. Finalize's refusals
// (ErrEmpty, ErrTooLarge, ErrWrongType, ErrInfected, ErrChanged) are
// final: the file is already deleted. ErrScanUnavailable isn't: the
// quarantine copy is kept, so the step can be retried.
var (
	ErrNotUploaded      = errors.New("uploads: no file was uploaded")
	ErrAlreadyFinalized = errors.New("uploads: the file was already checked and stored")
	ErrEmpty            = errors.New("uploads: the file is empty")
	ErrTooLarge         = errors.New("uploads: the file is too large")
	ErrWrongType        = errors.New("uploads: the file's content isn't the declared type")
	ErrInfected         = errors.New("uploads: the file contains malware")
	ErrChanged          = errors.New("uploads: the file changed while it was being checked")
	ErrScanUnavailable  = errors.New("uploads: the virus scanner couldn't scan the file")
	ErrDeleted          = errors.New("uploads: the file has been deleted")
	ErrLinkInvalid      = errors.New("uploads: the download link isn't valid")
	ErrLinkExpired      = errors.New("uploads: the download link has expired")
)

// Class is a retention class (data model §8).
type Class string

const (
	// Transient: contact imports, campaign recipient files and their
	// error reports; deleted 2 days after a final status by default.
	Transient Class = "transient"
	// Financial: payout files and result reports; 30 days by default.
	Financial Class = "financial"
)

// ContentType is a file type an upload may declare.
type ContentType string

const (
	CSV  ContentType = "text/csv"
	XLSX ContentType = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
)

// ObjectStore is the part of platform/objectstore uploads uses.
type ObjectStore interface {
	Stat(ctx context.Context, key string) (objectstore.ObjectInfo, error)
	PutStream(ctx context.Context, key string, r io.Reader, size int64) error
	GetStream(ctx context.Context, key string) (io.ReadCloser, error)
	Delete(ctx context.Context, key string) error
	PresignUpload(ctx context.Context, key string, maxBytes int64, expiresAt time.Time) (objectstore.UploadForm, error)
}

// Scanner scans a file for malware (platform/clamav).
type Scanner interface {
	Scan(ctx context.Context, r io.Reader) (clamav.Result, error)
}

// Config is a service's upload settings.
type Config struct {
	KMSKeyID           string        // envelope key for stored files
	DownloadKey        []byte        // 32 bytes, signs download links
	MaxBytes           int64         // largest file accepted
	UploadLinkTTL      time.Duration // how long an upload form works
	DownloadLinkTTL    time.Duration // how long a download link works
	TransientRetention time.Duration
	FinancialRetention time.Duration
}

// DefaultConfig is data model §8's defaults, without keys. The 20 MB
// cap (decided 17 Nov) stays under clamd's default StreamMaxLength of
// 25 MB, so ClamAV needs no change to scan any accepted file.
func DefaultConfig() Config {
	return Config{
		MaxBytes:           20 << 20,
		UploadLinkTTL:      15 * time.Minute,
		DownloadLinkTTL:    5 * time.Minute,
		TransientRetention: 48 * time.Hour,
		FinancialRetention: 30 * 24 * time.Hour,
	}
}

// Uploads does the work for one service.
type Uploads struct {
	store    ObjectStore
	scanner  Scanner
	envelope *crypto.Envelope
	clock    clock.Clock
	cfg      Config
}

// New checks cfg and builds Uploads.
func New(store ObjectStore, scanner Scanner, envelope *crypto.Envelope, clk clock.Clock, cfg Config) (*Uploads, error) {
	switch {
	case cfg.KMSKeyID == "":
		return nil, errors.New("uploads: a KMS key ID is required")
	case len(cfg.DownloadKey) != 32:
		return nil, errors.New("uploads: the download signing key must be 32 bytes")
	case cfg.MaxBytes <= 0:
		return nil, errors.New("uploads: the size cap must be positive")
	case cfg.UploadLinkTTL <= 0 || cfg.DownloadLinkTTL <= 0:
		return nil, errors.New("uploads: link lifetimes must be positive")
	case cfg.TransientRetention <= 0:
		return nil, errors.New("uploads: transient retention must be positive")
	case cfg.FinancialRetention < cfg.TransientRetention:
		return nil, errors.New("uploads: financial retention can't be shorter than transient")
	}
	return &Uploads{store: store, scanner: scanner, envelope: envelope, clock: clk, cfg: cfg}, nil
}

// QuarantineKey is where an upload lands before it's checked. Every key
// is built from IDs only, never from anything the client sends.
func QuarantineKey(tenant, id uuid.UUID) string {
	return fmt.Sprintf("quarantine/%s/%s", tenant, id)
}

// FileKey is where a checked file is stored, encrypted.
func FileKey(class Class, tenant, id uuid.UUID) string {
	return fmt.Sprintf("uploads/%s/%s/%s", class, tenant, id)
}

// ErrorReportKey is where the error report made from an upload goes; it
// shares the upload's class and retention.
func ErrorReportKey(class Class, tenant, id uuid.UUID) string {
	return FileKey(class, tenant, id) + ".errors"
}

// DeleteAfter is when an upload's files go, given when it reached a
// final status.
func (u *Uploads) DeleteAfter(class Class, finishedAt time.Time) time.Time {
	if class == Financial {
		return finishedAt.Add(u.cfg.FinancialRetention)
	}
	return finishedAt.Add(u.cfg.TransientRetention)
}

// Upload is a signed form for one file, and the dates a service records.
type Upload struct {
	Form        objectstore.UploadForm
	ExpiresAt   time.Time // the form stops working
	DeleteAfter time.Time // the link's end plus transient retention, for an upload that never happens
	MaxBytes    int64
}

// IssueUpload signs a form for one file to the quarantine key of id.
func (u *Uploads) IssueUpload(ctx context.Context, tenant, id uuid.UUID) (Upload, error) {
	expires := u.clock.Now().Add(u.cfg.UploadLinkTTL)
	form, err := u.store.PresignUpload(ctx, QuarantineKey(tenant, id), u.cfg.MaxBytes, expires)
	if err != nil {
		return Upload{}, fmt.Errorf("uploads: signing the upload form: %w", err)
	}
	return Upload{Form: form, ExpiresAt: expires, DeleteAfter: expires.Add(u.cfg.TransientRetention), MaxBytes: u.cfg.MaxBytes}, nil
}

// Open returns a reader of the decrypted file at key, or ErrDeleted.
// Any read error means the file is corrupt: discard what was read.
func (u *Uploads) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	obj, err := u.store.GetStream(ctx, key)
	if errors.Is(err, objectstore.ErrNotFound) {
		return nil, ErrDeleted
	}
	if err != nil {
		return nil, fmt.Errorf("uploads: opening %s: %w", key, err)
	}
	plain, err := u.envelope.OpenStream(ctx, u.cfg.KMSKeyID, obj)
	if err != nil {
		_ = obj.Close()
		return nil, fmt.Errorf("uploads: opening %s: %w", key, err)
	}
	return readCloser{Reader: plain, Closer: obj}, nil
}

type readCloser struct {
	io.Reader
	io.Closer
}

// PutSealed encrypts what r yields and stores it at key, for files a
// service makes itself, such as error reports.
func (u *Uploads) PutSealed(ctx context.Context, key string, r io.Reader) error {
	sealer, err := u.envelope.NewStreamSealer(ctx, u.cfg.KMSKeyID)
	if err != nil {
		return fmt.Errorf("uploads: sealing %s: %w", key, err)
	}
	return u.putSealed(ctx, key, sealer, r, -1)
}

// putSealed pipes r through sealer into the store. size is r's exact
// length, or -1 if unknown.
func (u *Uploads) putSealed(ctx context.Context, key string, sealer *crypto.StreamSealer, r io.Reader, size int64) error {
	pr, pw := io.Pipe()
	go func() {
		w := sealer.Writer(pw)
		_, err := io.Copy(w, r)
		if cerr := w.Close(); err == nil {
			err = cerr
		}
		_ = pw.CloseWithError(err)
	}()
	sealedSize := int64(-1)
	if size >= 0 {
		sealedSize = sealer.SealedSize(size)
	}
	err := u.store.PutStream(ctx, key, pr, sealedSize)
	_ = pr.CloseWithError(errors.New("uploads: upload ended"))
	if err != nil {
		return fmt.Errorf("uploads: storing %s: %w", key, err)
	}
	return nil
}
