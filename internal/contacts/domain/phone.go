// Package domain holds contacts' rules (data model §10): pure Go, no I/O.
package domain

import (
	"errors"
	"regexp"
	"slices"
	"strings"
)

// Why a number was refused; an import reports these with the row.
var (
	ErrPhoneEmpty     = errors.New("no number")
	ErrPhoneNotDigits = errors.New("not a number: only digits, spaces, hyphens, dots, brackets and a leading + or 00 are allowed")
	ErrPhoneLength    = errors.New("the number has the wrong length")
	ErrPhoneNotMobile = errors.New("not a mobile number")
)

// PhoneRules are a country's numbering rules (its pack's numbering).
type PhoneRules struct {
	CountryCode    string // e.g. 254
	TrunkPrefix    string // e.g. 0
	NSNLength      int    // the national significant number's length
	MobilePrefixes []string
}

var e164 = regexp.MustCompile(`^\+[1-9][0-9]{6,14}$`)

// ValidE164 reports whether s is an E.164 number: + and 7 to 15 digits.
func ValidE164(s string) bool { return e164.MatchString(s) }

// NormalizePhone reads raw as a number in the rules' country, unless it
// starts with + or 00, and returns it in E.164 (data model §10).
// Separators (spaces, hyphens, dots, brackets) and a "(0)" after the
// country code are dropped. A local number must have the country's
// length and start with one of its mobile prefixes; a number in another
// country only has to be valid E.164.
func NormalizePhone(r PhoneRules, raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", ErrPhoneEmpty
	}
	international := false
	switch {
	case strings.HasPrefix(s, "+"):
		international, s = true, s[1:]
	case strings.HasPrefix(s, "00"):
		international, s = true, s[2:]
	}
	if international {
		s = strings.Replace(s, "(0)", "", 1)
	}
	s = strings.Map(func(c rune) rune {
		if strings.ContainsRune(" -.()", c) {
			return -1
		}
		return c
	}, s)
	if s == "" || strings.IndexFunc(s, func(c rune) bool { return c < '0' || c > '9' }) >= 0 {
		return "", ErrPhoneNotDigits
	}
	if international {
		if s[0] == '0' {
			return "", ErrPhoneNotDigits
		}
		if rest, ok := strings.CutPrefix(s, r.CountryCode); ok && r.CountryCode != "" {
			return r.local(rest)
		}
		if !ValidE164("+" + s) {
			return "", ErrPhoneLength
		}
		return "+" + s, nil
	}
	switch {
	case r.TrunkPrefix != "" && strings.HasPrefix(s, r.TrunkPrefix) && len(s) == len(r.TrunkPrefix)+r.NSNLength:
		return r.local(s[len(r.TrunkPrefix):])
	case r.CountryCode != "" && strings.HasPrefix(s, r.CountryCode) && len(s) == len(r.CountryCode)+r.NSNLength:
		return r.local(s[len(r.CountryCode):])
	case len(s) == r.NSNLength:
		return r.local(s)
	}
	return "", ErrPhoneLength
}

// local checks a national significant number and makes it E.164.
func (r PhoneRules) local(nsn string) (string, error) {
	if len(nsn) != r.NSNLength {
		return "", ErrPhoneLength
	}
	if !slices.ContainsFunc(r.MobilePrefixes, func(p string) bool { return strings.HasPrefix(nsn, p) }) {
		return "", ErrPhoneNotMobile
	}
	return "+" + r.CountryCode + nsn, nil
}
