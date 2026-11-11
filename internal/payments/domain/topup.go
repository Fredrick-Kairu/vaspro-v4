package domain

import (
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"github.com/google/uuid"

	vascrypto "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/crypto"
)

// ErrInvalidTopup wraps every refused top-up request (VAL-4000).
var ErrInvalidTopup = errors.New("domain: invalid top-up")

// TopupRules are a country's rules for an STK top-up, from its pack.
type TopupRules struct {
	CurrencyExponent     int
	CountryCode          string // E.164 country code, e.g. 254
	NationalNumberLength int
}

// ValidateTopup checks an STK top-up (task 4.4): a positive amount in
// whole units of the currency, since STK pushes whole shillings, and
// the payer's number in E.164 for the country. Which mobile prefixes a
// rail takes is the rail adapter's check.
func ValidateTopup(amountMinor int64, phone string, r TopupRules) error {
	unit := int64(1)
	for range r.CurrencyExponent {
		unit *= 10
	}
	if amountMinor <= 0 || amountMinor%unit != 0 {
		return fmt.Errorf("%w: the amount must be a positive whole number of the currency's units", ErrInvalidTopup)
	}
	national, ok := strings.CutPrefix(phone, "+"+r.CountryCode)
	if !ok || len(national) != r.NationalNumberLength || strings.Trim(national, "0123456789") != "" {
		return fmt.Errorf("%w: the payer's number must be +%s and %d digits", ErrInvalidTopup, r.CountryCode, r.NationalNumberLength)
	}
	return nil
}

// CallbackToken is the path token a rail account's callback URLs carry
// (OQ-22): HMAC-SHA256 of its ID under the callback secret, in hex.
// Daraja can't sign callbacks, so the token, with the source address,
// is what proves a callback came from the URL VASPRO gave Daraja.
func CallbackToken(secret []byte, railAccountID uuid.UUID) string {
	return hex.EncodeToString(vascrypto.HMACSign(secret, railAccountID[:]))
}

// ValidCallbackToken checks token in constant time.
func ValidCallbackToken(secret []byte, railAccountID uuid.UUID, token string) bool {
	sig, err := hex.DecodeString(token)
	if err != nil || len(sig) == 0 {
		return false
	}
	return vascrypto.HMACVerify(secret, railAccountID[:], sig)
}

// ParseCIDRs reads a comma-separated list of CIDRs; blanks are skipped.
func ParseCIDRs(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for part := range strings.SplitSeq(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		p, err := netip.ParsePrefix(part)
		if err != nil {
			return nil, fmt.Errorf("domain: %q isn't a CIDR: %w", part, err)
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

// SourceAllowed reports whether a callback's source address is in the
// rail's published ranges. An empty list allows nothing.
func SourceAllowed(ip netip.Addr, allowed []netip.Prefix) bool {
	if !ip.IsValid() {
		return false
	}
	ip = ip.Unmap()
	for _, p := range allowed {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// IntentStatus is payments.intent_status.
type IntentStatus string

const (
	IntentCreated   IntentStatus = "created"
	IntentPending   IntentStatus = "pending"
	IntentSucceeded IntentStatus = "succeeded"
	IntentFailed    IntentStatus = "failed"
	IntentExpired   IntentStatus = "expired"
	IntentCancelled IntentStatus = "cancelled"
)

// Final reports whether the status ends the intent.
func (s IntentStatus) Final() bool {
	return s == IntentSucceeded || s == IntentFailed || s == IntentExpired || s == IntentCancelled
}

// IntentState is what ApplySTKResult needs of an intent.
type IntentState struct {
	Status      IntentStatus
	AmountMinor int64
	Receipt     string
}

// STKOutcome is a final STK result from the rail.
type STKOutcome struct {
	ResultCode  int
	ErrorCode   string // the PAY code for a failure
	Receipt     string
	AmountMinor int64
}

// STKDecision is what to do with an outcome.
type STKDecision struct {
	// Apply: the intent takes Status (and, for a success, the receipt
	// and a credit).
	Apply  bool
	Status IntentStatus
	// Late: a success after the intent was reported failed, expired or
	// cancelled. It is still credited (the money left the payer) and
	// flagged (OQ-22).
	Late bool
	// AmountMismatch: the rail's amount differs from the intent's; the
	// rail's amount is credited and the difference flagged.
	AmountMismatch bool
	// Duplicate: a second, different receipt for an intent already
	// paid; never applied over the first, only reported.
	Duplicate bool
}

// cancelledByPayer is Daraja's "request cancelled by user".
const cancelledByPayer = 1032

// ApplySTKResult decides an STK outcome against the intent (task 4.4).
// A success always wins, even late, because the money has left the
// payer; a failure never overrides a final status.
func ApplySTKResult(cur IntentState, out STKOutcome) STKDecision {
	if out.ResultCode == 0 {
		if cur.Status == IntentSucceeded {
			return STKDecision{Status: cur.Status, Duplicate: cur.Receipt != out.Receipt}
		}
		return STKDecision{
			Apply: true, Status: IntentSucceeded,
			Late:           cur.Status.Final(),
			AmountMismatch: out.AmountMinor != cur.AmountMinor,
		}
	}
	if cur.Status.Final() {
		return STKDecision{Status: cur.Status}
	}
	if out.ResultCode == cancelledByPayer {
		return STKDecision{Apply: true, Status: IntentCancelled}
	}
	return STKDecision{Apply: true, Status: IntentFailed}
}
