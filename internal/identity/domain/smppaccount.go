package domain

import (
	"crypto/rand"
	"fmt"
	"strings"
)

// SMPPAccountPasswordLength is the longest password SMPP 3.4 itself
// allows (data model §3's own comment: "SMPP passwords are limited to
// 8 characters by the SMPP 3.4 protocol, so SMPP accounts always
// require an IP allowlist and TLS") — generated at the maximum the
// protocol permits, to get as much entropy as that ceiling allows.
const SMPPAccountPasswordLength = 8

// smppAccountPasswordAlphabet is plain alphanumeric: SMPP binds are
// configured once into a client's own SMSC stack, not hand-typed
// repeatedly, so there's no need to exclude visually similar
// characters the way GenerateRecoveryCodes does for a human reading
// codes off a screen.
const smppAccountPasswordAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789" // gitleaks:allow (a character set, not a password)

// GenerateSMPPAccountPassword returns a fresh, high-entropy password
// for an SMPP account, shown exactly once at creation or rotation and
// stored only as its Argon2id hash — the same "shown once, hashed
// after" shape as GenerateApiClientSecret.
func GenerateSMPPAccountPassword() (string, error) {
	buf := make([]byte, SMPPAccountPasswordLength)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("domain: generating smpp account password: %w", err)
	}
	var b strings.Builder
	b.Grow(SMPPAccountPasswordLength)
	for _, v := range buf {
		b.WriteByte(smppAccountPasswordAlphabet[int(v)%len(smppAccountPasswordAlphabet)])
	}
	return b.String(), nil
}
