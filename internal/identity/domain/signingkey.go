package domain

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/ids"
)

// SigningKeyAlgorithm is the one value identity.signing_keys.algorithm
// has ever held (1.1's CHECK constraint doesn't enumerate alternatives,
// but nothing in this codebase generates anything else).
const SigningKeyAlgorithm = "EdDSA"

// SigningKeyPair is a freshly generated Ed25519 key pair for JWT
// signing, plus the kid that will identify it in both
// identity.signing_keys and the published JWKS.
type SigningKeyPair struct {
	Kid        string
	PublicKey  ed25519.PublicKey
	PrivateKey ed25519.PrivateKey
}

// GenerateSigningKeyPair creates a new Ed25519 key pair with a fresh
// kid (a plain UUID, like every other identity entity's ID — 1.7's own
// "no prefixed-ID convention for just one entity" reasoning applies
// here too).
func GenerateSigningKeyPair() (SigningKeyPair, error) {
	kid, err := ids.New()
	if err != nil {
		return SigningKeyPair{}, fmt.Errorf("domain: generating signing key id: %w", err)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return SigningKeyPair{}, fmt.Errorf("domain: generating ed25519 key pair: %w", err)
	}
	return SigningKeyPair{Kid: kid.String(), PublicKey: pub, PrivateKey: priv}, nil
}
