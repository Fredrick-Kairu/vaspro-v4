package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	vascrypto "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/crypto"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/ids"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/accounts/adapters"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/accounts/domain"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/accounts/store"
)

// ErrInfected means the virus scanner found something; nothing was
// stored. api/ maps it to VAL-4009.
var ErrInfected = errors.New("app: the file failed the virus scan")

// ObjectStore is where documents' sealed bytes live
// (platform/objectstore.Store).
type ObjectStore interface {
	Put(ctx context.Context, key string, data []byte) error
	Get(ctx context.Context, key string) ([]byte, error)
	Delete(ctx context.Context, key string) error
}

// DocumentDeps is what documents need (task 2.8, OQ-09): object
// storage, a virus scanner, and the envelope and KMS key that seal each
// file before it's stored. Without them, document use cases return
// ErrValidation.
type DocumentDeps struct {
	Store    ObjectStore
	Scanner  adapters.Scanner
	Envelope *vascrypto.Envelope
	KeyID    string
}

// Document is a stored document's metadata.
type Document struct {
	ID        uuid.UUID
	Type      string
	MIMEType  string
	SizeBytes int64
	SHA256    string // hex
	Status    string
	CreatedAt time.Time
}

func documentFromRow(r store.AccountsDocument) Document {
	return Document{
		ID: r.ID, Type: r.Type, MIMEType: r.MimeType, SizeBytes: int64(r.SizeBytes),
		SHA256: hex.EncodeToString(r.Sha256), Status: r.Status, CreatedAt: r.CreatedAt,
	}
}

func (s *Service) documentsConfigured() error {
	if s.documents == nil {
		return fmt.Errorf("%w: documents aren't configured", ErrValidation)
	}
	return nil
}

// UploadDocument stores a document for tenantID (account:manage; task
// 2.8, OQ-09). In order: the type, size and file type are checked
// (domain.CheckUpload, from the bytes, never the client's word); a file
// this account already uploaded returns that document (created false),
// so the same file is stored once; the file is virus-scanned and
// refused if anything is found (ErrInfected) or the scan can't run;
// then it's sealed with the documents key so object storage only ever
// holds ciphertext, stored, and recorded. Nothing is kept for a refused
// upload, and the object is removed if recording it fails.
func (s *Service) UploadDocument(ctx context.Context, userID, tenantID uuid.UUID, docType string, data []byte) (*Document, bool, error) {
	if _, _, err := s.authorize(ctx, userID, tenantID, domain.PermAccountManage); err != nil {
		return nil, false, err
	}
	if err := s.documentsConfigured(); err != nil {
		return nil, false, err
	}
	mime, err := domain.CheckUpload(docType, data)
	if err != nil {
		return nil, false, err
	}
	sum := sha256.Sum256(data)

	if existing, err := s.documentByHash(ctx, tenantID, sum[:]); err != nil || existing != nil {
		return existing, false, err
	}

	verdict, err := s.documents.Scanner.Scan(ctx, data)
	if err != nil {
		return nil, false, fmt.Errorf("app: scanning the upload: %w", err)
	}
	if !verdict.Clean {
		return nil, false, ErrInfected
	}

	id, err := ids.New()
	if err != nil {
		return nil, false, fmt.Errorf("app: generating document id: %w", err)
	}
	sealed, err := s.documents.Envelope.Seal(ctx, s.documents.KeyID, data)
	if err != nil {
		return nil, false, fmt.Errorf("app: encrypting the document: %w", err)
	}
	key := fmt.Sprintf("tenants/%s/documents/%s", tenantID, id)
	if err := s.documents.Store.Put(ctx, key, sealed); err != nil {
		return nil, false, err
	}

	var doc Document
	err = s.pool.WithTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		row, err := store.New(tx).CreateDocument(ctx, store.CreateDocumentParams{
			ID: id, TenantID: tenantID, Type: docType, StorageKey: key, Sha256: sum[:], MimeType: mime,
			SizeBytes: int32(len(data)), Status: domain.DocumentReceived, UploadedBy: userID, //nolint:gosec // G115: at most domain.MaxDocumentBytes (10 MB)
		})
		if err != nil {
			return fmt.Errorf("app: recording the document: %w", err)
		}
		doc = documentFromRow(row)
		return nil
	})
	if err != nil {
		_ = s.documents.Store.Delete(context.WithoutCancel(ctx), key)
		if isUniqueViolation(err) {
			// The same file landed concurrently: that one is the document.
			existing, findErr := s.documentByHash(ctx, tenantID, sum[:])
			if findErr == nil && existing != nil {
				return existing, false, nil
			}
		}
		return nil, false, err
	}
	return &doc, true, nil
}

// documentByHash is tenantID's document with this hash, or nil.
func (s *Service) documentByHash(ctx context.Context, tenantID uuid.UUID, sum []byte) (*Document, error) {
	var found *Document
	err := s.pool.WithTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		row, err := store.New(tx).GetDocumentBySHA256(ctx, store.GetDocumentBySHA256Params{TenantID: tenantID, Sha256: sum})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("app: looking for the same file: %w", err)
		}
		d := documentFromRow(row)
		found = &d
		return nil
	})
	return found, err
}

// ListDocuments returns tenantID's documents' metadata, newest first
// (account:read).
func (s *Service) ListDocuments(ctx context.Context, userID, tenantID uuid.UUID) ([]Document, error) {
	if _, _, err := s.authorize(ctx, userID, tenantID, domain.PermAccountRead); err != nil {
		return nil, err
	}
	var docs []Document
	err := s.pool.WithTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := store.New(tx).ListDocuments(ctx, tenantID)
		if err != nil {
			return fmt.Errorf("app: listing documents: %w", err)
		}
		docs = make([]Document, len(rows))
		for i, r := range rows {
			docs[i] = documentFromRow(r)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return docs, nil
}

// DownloadDocument returns a document's decrypted bytes and MIME type
// (account:manage: identity and company documents are sensitive, so
// read-only roles can list them but not open them).
func (s *Service) DownloadDocument(ctx context.Context, userID, tenantID, documentID uuid.UUID) ([]byte, string, error) {
	if _, _, err := s.authorize(ctx, userID, tenantID, domain.PermAccountManage); err != nil {
		return nil, "", err
	}
	if err := s.documentsConfigured(); err != nil {
		return nil, "", err
	}
	var row store.AccountsDocument
	err := s.pool.WithTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		row, err = store.New(tx).GetDocumentByID(ctx, store.GetDocumentByIDParams{ID: documentID, TenantID: tenantID})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	})
	if err != nil {
		return nil, "", err
	}
	sealed, err := s.documents.Store.Get(ctx, row.StorageKey)
	if err != nil {
		return nil, "", fmt.Errorf("app: reading the document: %w", err)
	}
	data, err := s.documents.Envelope.Open(ctx, s.documents.KeyID, sealed)
	if err != nil {
		return nil, "", fmt.Errorf("app: decrypting the document: %w", err)
	}
	return data, row.MimeType, nil
}

// isUniqueViolation reports a Postgres unique-constraint violation.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
