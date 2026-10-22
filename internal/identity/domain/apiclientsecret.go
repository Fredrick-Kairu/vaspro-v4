package domain

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
)

// GenerateApiClientSecret returns a fresh high-entropy secret for an
// API client, prefixed sk_<environment>_ (environment is "sandbox" or
// "live", already validated by the caller) — a leaked key's prefix
// alone says which environment to worry about, matching real-world
// API-key conventions (e.g. Stripe's sk_live_/sk_test_). last4 is the
// literal last four characters of the returned secret, for a
// dashboard to show ("...X9zK") without ever re-displaying the whole
// thing.
func GenerateApiClientSecret(environment string) (secret, last4 string, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("domain: generating api client secret: %w", err)
	}
	secret = "sk_" + environment + "_" + base64.RawURLEncoding.EncodeToString(raw)
	last4 = secret[len(secret)-4:]
	return secret, last4, nil
}
