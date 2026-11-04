package domain_test

import (
	"testing"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/accounts/domain"
)

// TestNextKYCStatus covers every current × incoming status: a decided
// check is final, so a repeated or out-of-order vendor result changes
// nothing (agreed 2 Nov, OQ-07).
func TestNextKYCStatus(t *testing.T) {
	const (
		pending  = domain.KYCPending
		passed   = domain.KYCPassed
		review   = domain.KYCReview
		failed   = domain.KYCFailed
		expired  = domain.KYCExpired
		noChange = ""
	)
	all := []string{pending, passed, review, failed, expired}
	want := map[string]map[string]string{
		pending: {pending: noChange, passed: passed, review: review, failed: failed, expired: expired},
		review:  {pending: noChange, passed: passed, review: noChange, failed: failed, expired: expired},
		passed:  {pending: noChange, passed: noChange, review: noChange, failed: noChange, expired: noChange},
		failed:  {pending: noChange, passed: noChange, review: noChange, failed: noChange, expired: noChange},
		expired: {pending: noChange, passed: noChange, review: noChange, failed: noChange, expired: noChange},
	}
	for _, current := range all {
		for _, incoming := range all {
			next, changed := domain.NextKYCStatus(current, incoming)
			w := want[current][incoming]
			if changed != (w != noChange) || (changed && next != w) {
				t.Errorf("NextKYCStatus(%s, %s) = %q, %v; want %q", current, incoming, next, changed, w)
			}
			if !changed && next != current {
				t.Errorf("NextKYCStatus(%s, %s) changed nothing but returned %q", current, incoming, next)
			}
		}
	}
	if _, changed := domain.NextKYCStatus(pending, "approved"); changed {
		t.Error("an unknown incoming status changed a pending check")
	}
}
