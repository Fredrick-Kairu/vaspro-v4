// Package domain holds identity's business rules — pure Go, no I/O: no
// imports of store/, adapters/ or net/http (.claude/rules/go.md).
package domain

import (
	"fmt"
	"net/mail"
	"regexp"
	"strings"
)

// localPartRE matches a dot-atom local part: the only form a real
// sign-up email ever needs. Anything outside it (a quoted string, a
// literal space, parentheses...) is RFC 5322-legal but round-trips
// inconsistently through net/mail depending on how it was spelled
// (found by fuzzing: a quoted local part containing a space comes back
// out of mail.Address unquoted, and re-parsing that unquoted form
// sometimes succeeds with yet another result, sometimes doesn't) — so
// it's rejected outright rather than chasing every corner of that
// mismatch.
var localPartRE = regexp.MustCompile(`^[a-z0-9!#$%&'*+\-/=?^_` + "`" + `{|}~]+(\.[a-z0-9!#$%&'*+\-/=?^_` + "`" + `{|}~]+)*$`)

// NormalizeEmail validates raw as a single bare email address and
// returns it lower-cased. Email is the sign-in identity (system design
// §2) and already case-insensitive at the database level (citext), but
// the application still normalizes before ever reaching the database —
// net/mail.ParseAddress also accepts a "Display Name <addr>" form and
// comma-separated lists, neither of which is a sign-in identity.
func NormalizeEmail(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("domain: email must not be empty")
	}

	addr, err := mail.ParseAddress(raw)
	if err != nil {
		return "", fmt.Errorf("domain: invalid email address: %w", err)
	}
	if addr.Name != "" {
		return "", fmt.Errorf("domain: email address must not include a display name")
	}

	normalized := strings.ToLower(addr.Address)

	at := strings.LastIndexByte(normalized, '@')
	if at < 0 {
		return "", fmt.Errorf("domain: invalid email address")
	}
	if !localPartRE.MatchString(normalized[:at]) {
		return "", fmt.Errorf("domain: email address uses a form we don't support")
	}

	return normalized, nil
}
