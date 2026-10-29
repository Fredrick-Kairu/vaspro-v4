package domain

import (
	"fmt"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
)

// totpPeriod/totpDigits/totpSkew fix this codebase's TOTP configuration
// in one place: 6-digit codes, 30-second periods, ±1 period of clock
// drift tolerated (ROADMAP's own Phase 1 test: "TOTP against the RFC
// 6238 test vectors, including time-step drift of ±1").
const (
	totpPeriod = 30
	totpDigits = otp.DigitsSix
	totpSkew   = 1
)

// GenerateTOTPSecret returns a fresh base32 secret and its otpauth://
// URL (for a QR code) for accountName under issuer.
func GenerateTOTPSecret(issuer, accountName string) (secret, url string, err error) {
	key, err := totp.Generate(totp.GenerateOpts{
		Issuer:      issuer,
		AccountName: accountName,
		Period:      totpPeriod,
		Digits:      totpDigits,
		Algorithm:   otp.AlgorithmSHA1,
	})
	if err != nil {
		return "", "", fmt.Errorf("domain: generating TOTP secret: %w", err)
	}
	return key.Secret(), key.URL(), nil
}

// ValidateTOTPCode reports whether code is valid for secret at at,
// allowing totpSkew periods of drift either side.
func ValidateTOTPCode(secret, code string, at time.Time) bool {
	ok, err := totp.ValidateCustom(code, secret, at, totp.ValidateOpts{
		Period:    totpPeriod,
		Skew:      totpSkew,
		Digits:    totpDigits,
		Algorithm: otp.AlgorithmSHA1,
	})
	return err == nil && ok
}
