package domain

import (
	"crypto/subtle"
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

// MatchTOTPStep reports whether code is valid for secret at at,
// allowing totpSkew periods of drift either side, and returns the time
// step (unix seconds / totpPeriod) the code belongs to. The caller
// records that step so the same code — or one from an earlier step —
// is never accepted twice (RFC 6238 §5.2); validity alone can't say
// whether a code was already used.
func MatchTOTPStep(secret, code string, at time.Time) (step int64, ok bool) {
	if len(code) != totpDigits.Length() {
		return 0, false
	}
	opts := totp.ValidateOpts{Period: totpPeriod, Digits: totpDigits, Algorithm: otp.AlgorithmSHA1}
	now := at.Unix() / totpPeriod
	for candidate := now - totpSkew; candidate <= now+totpSkew; candidate++ {
		want, err := totp.GenerateCodeCustom(secret, time.Unix(candidate*totpPeriod, 0).UTC(), opts)
		if err != nil {
			return 0, false
		}
		if subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 {
			return candidate, true
		}
	}
	return 0, false
}
