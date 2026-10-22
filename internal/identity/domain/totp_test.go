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

func TestValidateTOTPCode_AllowsOneStepOfDrift(t *testing.T) {
	secret, _, err := GenerateTOTPSecret("VASPRO", "ada@example.com")
	if err != nil {
		t.Fatalf("GenerateTOTPSecret: %v", err)
	}

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	code, err := totp.GenerateCodeCustom(secret, now, totp.ValidateOpts{Period: 30, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1})
	if err != nil {
		t.Fatalf("generating a code to test with: %v", err)
	}

	if !ValidateTOTPCode(secret, code, now) {
		t.Fatal("expected the code to validate at the exact time it was generated for")
	}
	if !ValidateTOTPCode(secret, code, now.Add(30*time.Second)) {
		t.Fatal("expected the code to still validate one period (30s) later (skew=1)")
	}
	if !ValidateTOTPCode(secret, code, now.Add(-30*time.Second)) {
		t.Fatal("expected the code to still validate one period (30s) earlier (skew=1)")
	}
	if ValidateTOTPCode(secret, code, now.Add(90*time.Second)) {
		t.Fatal("expected the code to no longer validate three periods later")
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
