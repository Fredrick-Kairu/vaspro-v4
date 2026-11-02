package adapters

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// ErrBadWebhookSignature means a vendor webhook's signature didn't
// verify, or its timestamp is outside the window: nothing in it can be
// trusted.
var ErrBadWebhookSignature = errors.New("adapters: KYC webhook signature invalid or expired")

// KYCStart asks a vendor to verify a subject.
type KYCStart struct {
	CheckID   uuid.UUID // our kyc_checks.id, sent as the vendor's reference
	Kind      string    // individual, business, screening
	SubjectID uuid.UUID
}

// KYCStarted is the vendor's side of a started check.
type KYCStarted struct {
	ProviderRef string
	URL         string // the vendor's hosted flow for the subject
}

// KYCResult is a verified vendor result.
type KYCResult struct {
	ProviderRef string
	Status      string         // passed, review, failed, expired
	Summary     map[string]any // decision details only; never documents or images
}

// KYCProvider is the one internal KYC interface (system design §2:
// start check, receive result, fetch report; ADR 0008). Each vendor is
// an adapter; the "fake" one stands in until OD-03 picks a vendor.
type KYCProvider interface {
	Name() string
	StartCheck(ctx context.Context, req KYCStart) (KYCStarted, error)
	// ParseWebhook verifies the vendor's signature before reading
	// anything, returning ErrBadWebhookSignature when it doesn't hold.
	ParseWebhook(header http.Header, body []byte, now time.Time) (KYCResult, error)
	FetchReport(ctx context.Context, providerRef string) (map[string]any, error)
}
