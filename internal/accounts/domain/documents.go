package domain

import (
	"errors"
	"net/http"
	"slices"
)

// Document types (accounts.documents.type). proof_of_address is in the
// requirements' Tier 2 list though not in data model §4's comment.
const (
	DocNationalID          = "national_id"
	DocPassport            = "passport"
	DocIncorporationCert   = "incorporation_cert"
	DocTaxCertificate      = "tax_certificate"
	DocProofOfAddress      = "proof_of_address"
	DocAuthorisationLetter = "authorisation_letter"
)

// Document statuses (accounts.documents.status).
const (
	DocumentReceived = "received"
	DocumentAccepted = "accepted"
	DocumentRejected = "rejected"
	DocumentExpired  = "expired"
)

// MaxDocumentBytes is the largest upload (agreed 3 Nov, OQ-09).
const MaxDocumentBytes = 10 << 20

var documentTypes = []string{DocNationalID, DocPassport, DocIncorporationCert, DocTaxCertificate, DocProofOfAddress, DocAuthorisationLetter}

var allowedMIME = []string{"application/pdf", "image/jpeg", "image/png"}

var (
	ErrUnknownDocumentType    = errors.New("domain: unknown document type")
	ErrDocumentTypeNotAllowed = errors.New("domain: only PDF, JPEG and PNG files are accepted")
	ErrDocumentTooLarge       = errors.New("domain: files are limited to 10 MB")
)

// IsDocumentType reports whether t is a known document type.
func IsDocumentType(t string) bool { return slices.Contains(documentTypes, t) }

// CheckUpload checks an upload before anything else happens to it
// (task 2.8, OQ-09): a known document type, at most 10 MB, and a PDF,
// JPEG or PNG judged from the file's own bytes, never from its name or
// the Content-Type the client sent. It returns the MIME type it found.
func CheckUpload(docType string, data []byte) (string, error) {
	if !IsDocumentType(docType) {
		return "", ErrUnknownDocumentType
	}
	if len(data) > MaxDocumentBytes {
		return "", ErrDocumentTooLarge
	}
	if len(data) == 0 {
		return "", ErrDocumentTypeNotAllowed
	}
	mime := http.DetectContentType(data)
	if !slices.Contains(allowedMIME, mime) {
		return "", ErrDocumentTypeNotAllowed
	}
	return mime, nil
}

// MissingDocuments returns the required document types the account has
// no usable document for, in required's order. have maps a document
// type to the status of the account's document of that type; a
// rejected or expired one doesn't count.
func MissingDocuments(required []string, have map[string]string) []string {
	var missing []string
	for _, t := range required {
		if s, ok := have[t]; !ok || s == DocumentRejected || s == DocumentExpired {
			missing = append(missing, t)
		}
	}
	return missing
}
