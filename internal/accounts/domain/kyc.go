package domain

// KYC check statuses (accounts.kyc_checks.status).
const (
	KYCPending = "pending"
	KYCPassed  = "passed"
	KYCReview  = "review"
	KYCFailed  = "failed"
	KYCExpired = "expired"
)

// NextKYCStatus is what a check in current becomes when the vendor
// reports incoming, and whether that's a change (agreed 2 Nov, OQ-07):
// a pending check takes any result; a check in review takes a decision
// (passed, failed or expired); a decided check is final. So a repeated
// or out-of-order vendor result never changes a decision, and with it
// never moves the account's tier. An unknown incoming status changes
// nothing.
func NextKYCStatus(current, incoming string) (string, bool) {
	decision := incoming == KYCPassed || incoming == KYCFailed || incoming == KYCExpired
	switch {
	case current == KYCPending && (decision || incoming == KYCReview):
		return incoming, true
	case current == KYCReview && decision:
		return incoming, true
	default:
		return current, false
	}
}
