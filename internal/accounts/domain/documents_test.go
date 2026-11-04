package domain_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/accounts/domain"
)

var (
	pdfBytes  = []byte("%PDF-1.4\n% a test document\n")
	pngBytes  = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	jpegBytes = []byte("\xff\xd8\xff\xe0\x00\x10JFIF\x00")
	gifBytes  = []byte("GIF89a\x01\x00\x01\x00")
	exeBytes  = []byte("MZ\x90\x00\x03\x00\x00\x00")
)

func TestCheckUpload(t *testing.T) {
	exactly := append(append([]byte{}, pdfBytes...), bytes.Repeat([]byte("a"), domain.MaxDocumentBytes-len(pdfBytes))...)
	cases := []struct {
		name     string
		docType  string
		data     []byte
		wantMIME string
		want     error
	}{
		{"a PDF", domain.DocIncorporationCert, pdfBytes, "application/pdf", nil},
		{"a PNG", domain.DocNationalID, pngBytes, "image/png", nil},
		{"a JPEG", domain.DocPassport, jpegBytes, "image/jpeg", nil},
		{"proof of address", domain.DocProofOfAddress, pdfBytes, "application/pdf", nil},
		{"exactly 10 MB", domain.DocTaxCertificate, exactly, "application/pdf", nil},
		{"one byte over 10 MB", domain.DocTaxCertificate, append(exactly, 'a'), "", domain.ErrDocumentTooLarge},
		{"empty", domain.DocTaxCertificate, nil, "", domain.ErrDocumentTypeNotAllowed},
		{"a GIF", domain.DocNationalID, gifBytes, "", domain.ErrDocumentTypeNotAllowed},
		{"an executable", domain.DocNationalID, exeBytes, "", domain.ErrDocumentTypeNotAllowed},
		{"plain text", domain.DocNationalID, []byte("just some text"), "", domain.ErrDocumentTypeNotAllowed},
		{"an unknown document type", "selfie", pdfBytes, "", domain.ErrUnknownDocumentType},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mime, err := domain.CheckUpload(tc.docType, tc.data)
			if !errors.Is(err, tc.want) || (tc.want == nil && err != nil) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			if mime != tc.wantMIME {
				t.Fatalf("got MIME %q, want %q", mime, tc.wantMIME)
			}
		})
	}
}

func TestMissingKYBDocuments(t *testing.T) {
	required := []string{domain.DocIncorporationCert, domain.DocTaxCertificate, domain.DocProofOfAddress}
	have := map[string]string{
		domain.DocIncorporationCert: domain.DocumentReceived,
		domain.DocTaxCertificate:    domain.DocumentRejected,
		domain.DocNationalID:        domain.DocumentReceived,
	}
	got := domain.MissingDocuments(required, have)
	if len(got) != 2 || got[0] != domain.DocTaxCertificate || got[1] != domain.DocProofOfAddress {
		t.Fatalf("got %v, want tax_certificate (only rejected) and proof_of_address", got)
	}
	if got := domain.MissingDocuments(required, map[string]string{
		domain.DocIncorporationCert: domain.DocumentReceived, domain.DocTaxCertificate: domain.DocumentAccepted, domain.DocProofOfAddress: domain.DocumentReceived,
	}); len(got) != 0 {
		t.Fatalf("got %v, want none missing", got)
	}
}
