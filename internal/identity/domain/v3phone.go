package domain

import (
	"regexp"
	"strings"
)

// v3LocalKEMSISDNRE matches a Kenyan MSISDN written in its common
// local forms: a leading 0 or 254, then a 9-digit subscriber number
// starting with 7 or 1 (Safaricom/Airtel/Telkom's actual ranges).
var v3LocalKEMSISDNRE = regexp.MustCompile(`^(?:0|254)?([71][0-9]{8})$`)

// NormalizeV3MSISDN converts a v3 profile.msisdn value to E.164, or
// reports ok=false if it can't confidently do so. v3 is Kenya-only
// (requirements.md), so this is deliberately Kenya-specific — unlike
// the country-pack-driven rules CLAUDE.md means by "country rules
// live in packs," this is one-time v3 import glue (task 1.13), not an
// ongoing runtime business rule, so it doesn't belong in packs/.
// A number this can't normalize isn't a reason to reject the whole
// user record: phone is optional profile data (system design §2), so
// the importer just leaves phone_e164 unset rather than flagging the
// account over it.
func NormalizeV3MSISDN(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	raw = strings.ReplaceAll(raw, " ", "")
	raw = strings.ReplaceAll(raw, "-", "")

	if e164, err := ValidatePhoneE164(raw); err == nil {
		return e164, true
	}

	m := v3LocalKEMSISDNRE.FindStringSubmatch(raw)
	if m == nil {
		return "", false
	}
	return "+254" + m[1], true
}
