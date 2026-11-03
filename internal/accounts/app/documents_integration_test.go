//go:build integration

package app_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/accounts/app"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/accounts/domain"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/objectstore"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/clamd"
)

// pdf is a small PDF whose text is easy to look for in storage.
func pdf(text string) []byte { return []byte("%PDF-1.4\n% " + text + "\n%%EOF\n") }

func (h harness) objectFor(t *testing.T, documentID uuid.UUID) []byte {
	t.Helper()
	var key string
	if err := h.super.QueryRow(context.Background(), `SELECT storage_key FROM documents WHERE id = $1`, documentID).Scan(&key); err != nil {
		t.Fatal(err)
	}
	data, err := h.objects.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("reading the stored object: %v", err)
	}
	return data
}

func TestUploadDocument_StoresEncryptedAndReturnsTheOriginal(t *testing.T) {
	h := newHarnessWithDocuments(t)
	ctx := context.Background()
	tenant, admin, _, viewer := h.newTeam(t)
	original := pdf("Certificate of incorporation for Acme Deliveries Limited")

	doc, created, err := h.svc.UploadDocument(ctx, admin, tenant, domain.DocIncorporationCert, original)
	if err != nil || !created || doc.MIMEType != "application/pdf" || doc.SizeBytes != int64(len(original)) || doc.Status != domain.DocumentReceived {
		t.Fatalf("UploadDocument: %+v, %v, %v", doc, created, err)
	}
	stored := h.objectFor(t, doc.ID)
	if bytes.Contains(stored, []byte("Acme Deliveries")) || bytes.Contains(stored, []byte("%PDF")) {
		t.Fatal("the object store holds the plaintext; documents must be encrypted before they're stored")
	}
	got, mime, err := h.svc.DownloadDocument(ctx, admin, tenant, doc.ID)
	if err != nil || mime != "application/pdf" || !bytes.Equal(got, original) {
		t.Fatalf("DownloadDocument: %q, %v", mime, err)
	}

	t.Run("the same file again is the same document, stored once", func(t *testing.T) {
		scans := h.scanner.Scans()
		again, created, err := h.svc.UploadDocument(ctx, admin, tenant, domain.DocIncorporationCert, original)
		if h.scanner.Scans() != scans {
			t.Fatal("the duplicate was scanned (and so stored) again; it should be recognised by its hash first")
		}
		if err != nil || created || again.ID != doc.ID {
			t.Fatalf("got %+v, created %v, %v; want the first document", again, created, err)
		}
		if n := h.count(t, `SELECT count(*) FROM documents WHERE tenant_id = $1`, tenant); n != 1 {
			t.Fatalf("%d document rows, want 1", n)
		}
	})
	t.Run("a Viewer lists but can't upload or download", func(t *testing.T) {
		docs, err := h.svc.ListDocuments(ctx, viewer, tenant)
		if err != nil || len(docs) != 1 {
			t.Fatalf("ListDocuments: %+v, %v", docs, err)
		}
		if _, _, err := h.svc.DownloadDocument(ctx, viewer, tenant, doc.ID); !errors.Is(err, domain.ErrPermissionDenied) {
			t.Fatalf("download: got %v", err)
		}
		if _, _, err := h.svc.UploadDocument(ctx, viewer, tenant, domain.DocPassport, pdf("x")); !errors.Is(err, domain.ErrPermissionDenied) {
			t.Fatalf("upload: got %v", err)
		}
	})
	t.Run("another account's document is not found", func(t *testing.T) {
		other, otherAdmin, _, _ := h.newTeam(t)
		if _, _, err := h.svc.DownloadDocument(ctx, otherAdmin, other, doc.ID); !errors.Is(err, app.ErrNotFound) {
			t.Fatalf("got %v", err)
		}
		// The same file in another account is that account's own document.
		theirs, created, err := h.svc.UploadDocument(ctx, otherAdmin, other, domain.DocIncorporationCert, original)
		if err != nil || !created || theirs.ID == doc.ID {
			t.Fatalf("got %+v, %v, %v; want a separate document", theirs, created, err)
		}
	})
}

func TestUploadDocument_Refusals(t *testing.T) {
	h := newHarnessWithDocuments(t)
	ctx := context.Background()
	tenant, admin, _, _ := h.newTeam(t)
	infected := append(pdf("looks like a PDF"), clamd.EICAR()...)
	cases := []struct {
		name string
		data []byte
		want error
	}{
		{"a wrong type", []byte("GIF89a\x01\x00\x01\x00"), domain.ErrDocumentTypeNotAllowed},
		{"too large", append(pdf("big"), make([]byte, domain.MaxDocumentBytes)...), domain.ErrDocumentTooLarge},
		{"infected", infected, app.ErrInfected},
	}
	for _, tc := range cases {
		t.Run(tc.name+" stores nothing", func(t *testing.T) {
			if _, _, err := h.svc.UploadDocument(ctx, admin, tenant, domain.DocTaxCertificate, tc.data); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			if n := h.count(t, `SELECT count(*) FROM documents`); n != 0 {
				t.Fatalf("%d document rows", n)
			}
		})
	}
	if _, _, err := h.svc.UploadDocument(ctx, admin, tenant, "selfie", pdf("x")); !errors.Is(err, domain.ErrUnknownDocumentType) {
		t.Fatalf("an unknown type: got %v", err)
	}
	if _, err := h.objects.Get(ctx, "tenants/"+tenant.String()+"/documents/"); !errors.Is(err, objectstore.ErrNotFound) {
		t.Fatalf("something was stored: %v", err)
	}
}

func TestDocuments_NotConfigured(t *testing.T) {
	h := newHarness(t)
	tenant, admin, _, _ := h.newTeam(t)
	if _, _, err := h.svc.UploadDocument(context.Background(), admin, tenant, domain.DocPassport, pdf("x")); !errors.Is(err, app.ErrValidation) {
		t.Fatalf("got %v, want ErrValidation (documents aren't configured)", err)
	}
}
