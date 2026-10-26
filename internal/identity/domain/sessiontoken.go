package domain

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

// GenerateSessionToken returns a fresh dashboard session bearer token:
// 32 random bytes, base64url-encoded, prefixed sess_ so a leaked one is
// recognisable on sight. It is a separate value from the session's id,
// which is a public handle (listSessions returns it) and must never
// authenticate.
func GenerateSessionToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("domain: generating session token: %w", err)
	}
	return "sess_" + base64.RawURLEncoding.EncodeToString(raw), nil
}

// HashSessionToken is how a session token is stored and looked up
// (identity.sessions.token_hash). Plain SHA-256, not Argon2id: the
// token is random rather than human-chosen, so there's nothing for a
// slow hash to protect, and it's checked on every request.
func HashSessionToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}
