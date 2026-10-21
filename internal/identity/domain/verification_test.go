package domain

import (
	"regexp"
	"testing"
)

var sixDigits = regexp.MustCompile(`^[0-9]{6}$`)

func TestGenerateVerificationCode_SixDigits(t *testing.T) {
	for range 100 {
		code, err := GenerateVerificationCode()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !sixDigits.MatchString(code) {
			t.Fatalf("code %q is not exactly 6 digits", code)
		}
	}
}

func TestGenerateVerificationCode_ZeroPadded(t *testing.T) {
	// Over enough draws, a code starting with '0' must show up —
	// otherwise the code isn't actually zero-padded to 6 digits.
	for range 10000 {
		code, err := GenerateVerificationCode()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if code[0] == '0' {
			return
		}
	}
	t.Fatal("never saw a code starting with 0 in 10000 draws; GenerateVerificationCode may not be zero-padding")
}
