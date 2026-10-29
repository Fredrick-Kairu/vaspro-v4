package domain

import (
	"crypto/rand"
	"fmt"
	"math/big"
)

// GenerateVerificationCode returns a fresh 6-digit numeric code
// ("000000"-"999999"), crypto/rand-backed.
func GenerateVerificationCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", fmt.Errorf("domain: generating verification code: %w", err)
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}
