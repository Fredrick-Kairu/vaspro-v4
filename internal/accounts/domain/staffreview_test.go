package domain_test

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/accounts/domain"
)

func TestValidateStaffDecision(t *testing.T) {
	a, b, other := uuid.New(), uuid.New(), uuid.New()
	submitted := []uuid.UUID{a, b}
	cases := []struct {
		name string
		d    domain.StaffDecision
		want error
	}{
		{"pass with a reason", domain.StaffDecision{Decision: domain.KYCPassed, Reason: "matches the registry"}, nil},
		{"fail with a reason", domain.StaffDecision{Decision: domain.KYCFailed, Reason: "blurred certificate"}, nil},
		{"fail rejecting submitted documents", domain.StaffDecision{Decision: domain.KYCFailed, Reason: "blurred", RejectedDocumentIDs: []uuid.UUID{b}}, nil},
		{"a reason is required", domain.StaffDecision{Decision: domain.KYCPassed, Reason: "  "}, domain.ErrInvalidDecision},
		{"review isn't a decision", domain.StaffDecision{Decision: domain.KYCReview, Reason: "later"}, domain.ErrInvalidDecision},
		{"expired isn't a staff decision", domain.StaffDecision{Decision: domain.KYCExpired, Reason: "old"}, domain.ErrInvalidDecision},
		{"an unknown decision", domain.StaffDecision{Decision: "maybe", Reason: "x"}, domain.ErrInvalidDecision},
		{"a pass can't reject documents", domain.StaffDecision{Decision: domain.KYCPassed, Reason: "ok", RejectedDocumentIDs: []uuid.UUID{a}}, domain.ErrInvalidDecision},
		{"only the check's own documents can be rejected", domain.StaffDecision{Decision: domain.KYCFailed, Reason: "x", RejectedDocumentIDs: []uuid.UUID{other}}, domain.ErrInvalidDecision},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := domain.ValidateStaffDecision(tc.d, submitted)
			if !errors.Is(err, tc.want) || (tc.want == nil && err != nil) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}
