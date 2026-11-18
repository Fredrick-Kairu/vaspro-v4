package domain

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
)

// Purposes, consent states and the reasons a send is blocked (data
// model §10).
const (
	PurposeTransactional = "transactional"
	PurposePromotional   = "promotional"

	ConsentGranted   = "granted"
	ConsentWithdrawn = "withdrawn"

	ReasonSuppressed    = "suppressed"
	ReasonOptedOut      = "opted_out"
	ReasonNoConsent     = "no_consent"
	ReasonInvalidNumber = "invalid_number"
)

var (
	// Channels are consents.channel's values, in the enum's order.
	Channels = []string{"sms", "whatsapp", "email", "telegram", "rcs"}
	// Purposes are consents.purpose's values.
	Purposes = []string{PurposeTransactional, PurposePromotional}

	// ErrSubjectWithdrew: the person withdrew this consent themselves,
	// and only they can give it back (VAL-4026).
	ErrSubjectWithdrew = errors.New("the person withdrew this consent themselves")
)

// ValidChannel reports whether c is a channel.
func ValidChannel(c string) bool { return slices.Contains(Channels, c) }

// ValidPurpose reports whether p is a purpose.
func ValidPurpose(p string) bool { return slices.Contains(Purposes, p) }

// DecisionInput is what one send's decision depends on.
type DecisionInput struct {
	ValidNumber      bool
	Channel, Purpose string
	// Status is the recorded consent for the channel and purpose:
	// granted, withdrawn, or "" for none.
	Status string
	// Suppressions are the number's suppressed channels; "" is every
	// channel.
	Suppressions []string
	// RequiresOptIn is the pack's opt_out.promotional_requires_opt_in.
	RequiresOptIn bool
}

// Decision is whether a send may go, and why not.
type Decision struct {
	Allowed bool
	Reason  string
}

// DecideConsent applies the decision table: an invalid number, then a
// suppression on the channel or every channel, then a withdrawal, then
// promotional sends with no consent where the country needs opt-in.
func DecideConsent(in DecisionInput) Decision {
	switch {
	case !in.ValidNumber:
		return Decision{Reason: ReasonInvalidNumber}
	case slices.Contains(in.Suppressions, "") || slices.Contains(in.Suppressions, in.Channel):
		return Decision{Reason: ReasonSuppressed}
	case in.Status == ConsentWithdrawn:
		return Decision{Reason: ReasonOptedOut}
	case in.Status == "" && in.Purpose == PurposePromotional && in.RequiresOptIn:
		return Decision{Reason: ReasonNoConsent}
	}
	return Decision{Allowed: true}
}

// ClientConsentChange decides whether the client (API, dashboard) may
// record newStatus over the current actor and status ("" for none). A
// person's own withdrawal stands: withdrawing again writes nothing, and
// granting is ErrSubjectWithdrew (the trigger refuses it too).
func ClientConsentChange(actor, status, newStatus string) (bool, error) {
	if actor == "subject" && status == ConsentWithdrawn {
		if newStatus == ConsentWithdrawn {
			return false, nil
		}
		return false, ErrSubjectWithdrew
	}
	return true, nil
}

// WithdrawalIsNews reports whether a change from before to after is a
// withdrawal, the moment consent.withdrawn is published.
func WithdrawalIsNews(before, after string) bool {
	return after == ConsentWithdrawn && before != ConsentWithdrawn
}

// KeywordRules are a pack's opt-out words, upper case.
type KeywordRules struct{ Stop, Start []string }

// MatchKeyword says whether a reply is only a stop or start word:
// "stop", "start" or "". Case, surrounding and repeated spaces, and one
// trailing . , ! or ? are ignored; anything more is no match.
func MatchKeyword(rules KeywordRules, text string) string {
	words := strings.Fields(text)
	if len(words) == 0 {
		return ""
	}
	last := words[len(words)-1]
	if trimmed := strings.TrimRight(last, ".,!?"); len(last)-len(trimmed) == 1 {
		words[len(words)-1] = trimmed
	}
	reply := strings.ToUpper(strings.Join(words, " "))
	switch {
	case reply == "":
		return ""
	case slices.Contains(rules.Stop, reply):
		return "stop"
	case slices.Contains(rules.Start, reply):
		return "start"
	}
	return ""
}

// PreferenceTokenLength is a preference link's token: 12 characters from
// 62, about 71 bits (data model §10).
const PreferenceTokenLength = 12

const tokenAlphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// NewPreferenceToken draws a token from r (crypto/rand in production),
// refusing bytes of 62 and above so every character is equally likely.
func NewPreferenceToken(r io.Reader) (string, error) {
	out := make([]byte, 0, PreferenceTokenLength)
	buf := make([]byte, 1)
	for len(out) < PreferenceTokenLength {
		if _, err := io.ReadFull(r, buf); err != nil {
			return "", fmt.Errorf("preference token: %w", err)
		}
		if int(buf[0]) < len(tokenAlphabet) {
			out = append(out, tokenAlphabet[buf[0]])
		}
	}
	return string(out), nil
}

// HashPreferenceToken is the SHA-256 stored for a token; false for
// anything that isn't a token's shape.
func HashPreferenceToken(token string) ([]byte, bool) {
	if len(token) != PreferenceTokenLength {
		return nil, false
	}
	for i := 0; i < len(token); i++ {
		if !strings.ContainsRune(tokenAlphabet, rune(token[i])) {
			return nil, false
		}
	}
	sum := sha256.Sum256([]byte(token))
	return sum[:], true
}
