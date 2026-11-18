package domain_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/contacts/domain"
)

// keRules are KE's pack values (packs/ke/pack.yaml).
var keRules = domain.PhoneRules{CountryCode: "254", TrunkPrefix: "0", NSNLength: 9, MobilePrefixes: []string{"1", "7"}}

// The numbers are ROADMAP §8.4's reserved test block (+254 700 000 0xx)
// and its mirror for the 1 prefix (+254 100 000 0xx); never real ones.
func TestNormalizePhone_KEFormats(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"0700000001", "+254700000001"},
		{"0100000001", "+254100000001"},
		{"700000001", "+254700000001"},
		{"100000001", "+254100000001"},
		{"254700000001", "+254700000001"},
		{"254100000001", "+254100000001"},
		{"+254700000001", "+254700000001"},
		{"+254100000001", "+254100000001"},
		{"00254700000001", "+254700000001"},
		{"0700 000 001", "+254700000001"},
		{"0700-000-001", "+254700000001"},
		{"(0700) 000.001", "+254700000001"},
		{"+254 (0)700 000 001", "+254700000001"},
		{"  +254 700 000 001  ", "+254700000001"},
		{"+999 00000001", "+99900000001"},      // another country kept as E.164
		{"0044 999 000 0001", "+449990000001"}, // 00 means international
	} {
		got, err := domain.NormalizePhone(keRules, tc.in)
		if err != nil || got != tc.want {
			t.Errorf("NormalizePhone(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
}

func TestNormalizePhone_Refusals(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want error
	}{
		{"", domain.ErrPhoneEmpty},
		{"   ", domain.ErrPhoneEmpty},
		{"07000000O1", domain.ErrPhoneNotDigits}, // a letter O
		{"0700-000-001 ext 2", domain.ErrPhoneNotDigits},
		{"+254+700000001", domain.ErrPhoneNotDigits},
		{"07000000", domain.ErrPhoneLength},
		{"07000000011", domain.ErrPhoneLength},
		{"25470000000", domain.ErrPhoneLength},
		{"+25470000000", domain.ErrPhoneLength},
		{"0200000001", domain.ErrPhoneNotMobile}, // a Nairobi landline
		{"+254200000001", domain.ErrPhoneNotMobile},
		{"0800000001", domain.ErrPhoneNotMobile},
		{"+12", domain.ErrPhoneLength},               // too short for E.164
		{"+1234567890123456", domain.ErrPhoneLength}, // 16 digits
		{"+0700000001", domain.ErrPhoneNotDigits},    // no country code starts with 0
		{"٠٧٠٠٠٠٠٠٠١", domain.ErrPhoneNotDigits},     // Arabic-Indic digits
		{strings.Repeat("7", 200), domain.ErrPhoneLength},
	} {
		got, err := domain.NormalizePhone(keRules, tc.in)
		if !errors.Is(err, tc.want) {
			t.Errorf("NormalizePhone(%q) = %q, %v; want %v", tc.in, got, err, tc.want)
		}
	}
}

func TestNormalizePhone_NoMobilePrefixesRefusesLocalNumbers(t *testing.T) {
	rules := keRules
	rules.MobilePrefixes = nil
	if _, err := domain.NormalizePhone(rules, "0700000001"); !errors.Is(err, domain.ErrPhoneNotMobile) {
		t.Fatalf("a country with no mobile prefixes accepted a local number: %v", err)
	}
}

func FuzzNormalizePhone_Idempotent(f *testing.F) {
	for _, s := range []string{"0700000001", "+254 700 000 001", "00447700900000", "abc", ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		once, err := domain.NormalizePhone(keRules, s)
		if err != nil {
			return
		}
		twice, err := domain.NormalizePhone(keRules, once)
		if err != nil || twice != once {
			t.Fatalf("NormalizePhone(%q) = %q, then %q, %v", s, once, twice, err)
		}
		if !domain.ValidE164(once) {
			t.Fatalf("NormalizePhone(%q) = %q, not E.164", s, once)
		}
	})
}
