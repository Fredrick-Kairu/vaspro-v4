package domain

import (
	"encoding/base32"
	"testing"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
)

// TestTOTP_RFC6238Vectors proves pquerna/otp itself computes RFC 6238's
// own published SHA-1 test vectors correctly (https://tools.ietf.org/html/rfc6238#appendix-B).
// This is deliberately 8-digit/SHA-1-at-a-fixed-time, not our own
// 6-digit/skew=1 configuration — that gets its own round-trip test
// below. ROADMAP names this test explicitly ("TOTP against the RFC
// 6238 test vectors").
func TestTOTP_RFC6238Vectors(t *testing.T) {
	secret := base32.StdEncoding.EncodeToString([]byte("12345678901234567890"))

	cases := []struct {
		unixTime int64
		want     string
	}{
		{59, "94287082"},
		{1111111109, "07081804"},
		{1111111111, "14050471"},
		{1234567890, "89005924"},
		{2000000000, "69279037"},
		{20000000000, "65353130"},
	}
	for _, c := range cases {
		ok, err := totp.ValidateCustom(c.want, secret, time.Unix(c.unixTime, 0).UTC(), totp.ValidateOpts{
			Digits:    otp.DigitsEight,
			Algorithm: otp.AlgorithmSHA1,
		})
		if err != nil {
			t.Fatalf("unixTime=%d: unexpected error: %v", c.unixTime, err)
		}
		if !ok {
			t.Fatalf("unixTime=%d: code %q did not validate against the RFC 6238 vector", c.unixTime, c.want)
		}
	}
}

// TestMatchTOTPStep_ReturnsTheCodesOwnTimeStep covers this codebase's
// own 6-digit, ±1-step configuration (ROADMAP: "including time-step
// drift of ±1"). The step returned is
// the one the code was generated for, wherever in the ±1 window it's
// presented — that's what the replay check (RFC 6238 §5.2) records.
func TestMatchTOTPStep_ReturnsTheCodesOwnTimeStep(t *testing.T) {
	secret, _, err := GenerateTOTPSecret("VASPRO", "ada@example.com")
	if err != nil {
		t.Fatalf("GenerateTOTPSecret: %v", err)
	}
	generatedAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	wantStep := generatedAt.Unix() / 30
	code, err := totp.GenerateCodeCustom(secret, generatedAt, totp.ValidateOpts{Period: 30, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1})
	if err != nil {
		t.Fatalf("generating a code to test with: %v", err)
	}

	cases := []struct {
		name   string
		at     time.Time
		wantOK bool
	}{
		{"presented in its own step", generatedAt, true},
		{"presented one step late", generatedAt.Add(30 * time.Second), true},
		{"presented one step early", generatedAt.Add(-30 * time.Second), true},
		{"presented two steps late", generatedAt.Add(60 * time.Second), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			step, ok := MatchTOTPStep(secret, code, c.at)
			if ok != c.wantOK {
				t.Fatalf("got ok=%v, want %v", ok, c.wantOK)
			}
			if ok && step != wantStep {
				t.Fatalf("got step %d, want %d", step, wantStep)
			}
		})
	}

	if _, ok := MatchTOTPStep(secret, "not-a-code", generatedAt); ok {
		t.Fatal("expected a malformed code not to match")
	}
}

func TestGenerateTOTPSecret_ProducesAScannableURL(t *testing.T) {
	secret, url, err := GenerateTOTPSecret("VASPRO", "ada@example.com")
	if err != nil {
		t.Fatalf("GenerateTOTPSecret: %v", err)
	}
	if secret == "" {
		t.Fatal("expected a non-empty secret")
	}
	if _, err := base32.StdEncoding.DecodeString(secret); err != nil {
		t.Fatalf("expected the secret to be valid base32: %v", err)
	}
	if url == "" || url[:7] != "otpauth" {
		t.Fatalf("expected an otpauth:// URL, got %q", url)
	}
}
