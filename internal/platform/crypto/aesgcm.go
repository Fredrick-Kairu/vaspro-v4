// Package crypto holds the secret-handling primitives named in ROADMAP
// §6's tech stack table: a KMS interface with a local, development-only
// implementation; envelope encryption built on top of it; HMAC helpers;
// and Argon2id password/secret hashing. Nothing here is specific to one
// service — identity hashes passwords and API-client secrets with it,
// payments envelope-encrypts rail credentials with it.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"fmt"
)

// sealAESGCM encrypts plaintext under a 32-byte (AES-256) key, returning
// nonce||ciphertext+tag as one blob so openAESGCM needs nothing but the
// key to reverse it.
func sealAESGCM(key, plaintext []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("crypto: generating nonce: %w", err)
	}

	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// openAESGCM reverses sealAESGCM. It fails closed on a tampered blob or
// the wrong key: GCM's authentication tag covers both.
func openAESGCM(key, blob []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}

	if len(blob) < gcm.NonceSize() {
		return nil, fmt.Errorf("crypto: ciphertext shorter than a nonce")
	}
	nonce, ciphertext := blob[:gcm.NonceSize()], blob[gcm.NonceSize():]

	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("crypto: decrypting: %w", err)
	}
	return plaintext, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("crypto: key must be 32 bytes (AES-256), got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("crypto: building AES cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("crypto: building GCM: %w", err)
	}
	return gcm, nil
}
