package domain

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
)

// ErrInvalidDecision means a staff decision on a check under review is
// malformed (task 2.9).
var ErrInvalidDecision = errors.New("domain: invalid staff decision")

// StaffDecision is a staff member's decision on a check in review.
type StaffDecision struct {
	Decision            string // KYCPassed or KYCFailed
	Reason              string
	RejectedDocumentIDs []uuid.UUID
}

// ValidateStaffDecision checks d against the documents the check was
// submitted with: staff pass or fail (expiry is the vendor's or
// re-verification's), always with a reason the client will see, and
// only a failure may reject documents, each one the check's own.
func ValidateStaffDecision(d StaffDecision, submitted []uuid.UUID) error {
	if d.Decision != KYCPassed && d.Decision != KYCFailed {
		return fmt.Errorf("%w: the decision must be passed or failed", ErrInvalidDecision)
	}
	if strings.TrimSpace(d.Reason) == "" {
		return fmt.Errorf("%w: a reason is required", ErrInvalidDecision)
	}
	if len(d.RejectedDocumentIDs) > 0 && d.Decision != KYCFailed {
		return fmt.Errorf("%w: only a failed check rejects documents", ErrInvalidDecision)
	}
	for _, id := range d.RejectedDocumentIDs {
		if !slices.Contains(submitted, id) {
			return fmt.Errorf("%w: document %s isn't one the check was submitted with", ErrInvalidDecision, id)
		}
	}
	return nil
}
