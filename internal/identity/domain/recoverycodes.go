package domain

import (
	"crypto/rand"
	"fmt"
	"strings"
)

// RecoveryCodeCount is how many codes TOTP enrollment generates at once.
// RecoveryCodeLength is each code's length.
const (
	RecoveryCodeCount  = 8
	RecoveryCodeLength = 10
)

// recoveryCodeAlphabet excludes visually ambiguous characters (0/O, 1/I/l)
// — these are meant to be read off a screen and typed back in once,
// usually by hand.
const recoveryCodeAlphabet = "23456789ABCDEFGHJKMNPQRSTUVWXYZ"

// GenerateRecoveryCodes returns RecoveryCodeCount fresh, high-entropy
// recovery codes (crypto/rand) — generated and shown exactly once, at
// TOTP enrollment.
func GenerateRecoveryCodes() ([]string, error) {
	codes := make([]string, RecoveryCodeCount)
	for i := range codes {
		code, err := randomRecoveryCode()
		if err != nil {
			return nil, fmt.Errorf("domain: generating recovery code: %w", err)
		}
		codes[i] = code
	}
	return codes, nil
}

func randomRecoveryCode() (string, error) {
	buf := make([]byte, RecoveryCodeLength)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	var b strings.Builder
	b.Grow(RecoveryCodeLength)
	for _, v := range buf {
		b.WriteByte(recoveryCodeAlphabet[int(v)%len(recoveryCodeAlphabet)])
	}
	return b.String(), nil
}
