package domain

import (
	"fmt"
	"regexp"
	"strings"
)

// phoneRE matches the exact CHECK constraint on identity.users.phone_e164
// (data model §3) — kept identical so a bad phone fails here, with a
// clear VAL- error, rather than as an opaque constraint violation from
// the database.
var phoneRE = regexp.MustCompile(`^\+[1-9][0-9]{6,14}$`)

// ValidatePhoneE164 validates raw as an E.164 phone number and returns
// it trimmed. Phone is optional profile data, never the sign-in
// identity (system design §2) — callers only call this when a phone
// was actually given.
func ValidatePhoneE164(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if !phoneRE.MatchString(raw) {
		return "", fmt.Errorf("domain: phone number must be E.164 (e.g. +254700000000)")
	}
	return raw, nil
}
