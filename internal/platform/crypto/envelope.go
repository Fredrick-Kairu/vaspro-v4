package crypto

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
)

const dekSize = 32 // AES-256

// Envelope performs envelope encryption on top of a KMS: a fresh data key
// (DEK) encrypts the payload locally; the KMS only ever sees the DEK, not
// the payload itself. This is the "envelope encryption in platform/crypto"
// line in ROADMAP §6's tech stack table, and the shape
// payments.rail_credentials expects — one ciphertext blob per secret,
// with the caller tracking which KMS key ID protected it (kms_key_id).
type Envelope struct {
	kms KMS
}

// NewEnvelope builds an Envelope over kms.
func NewEnvelope(kms KMS) *Envelope {
	return &Envelope{kms: kms}
}

// Seal encrypts plaintext under a fresh data key, which is itself
// encrypted via e's KMS under keyID. The caller stores keyID alongside the
// returned blob (e.g. rail_credentials.kms_key_id) and must pass the same
// value back into Open.
func (e *Envelope) Seal(ctx context.Context, keyID string, plaintext []byte) ([]byte, error) {
	dek := make([]byte, dekSize)
	if _, err := rand.Read(dek); err != nil {
		return nil, fmt.Errorf("crypto: generating data key: %w", err)
	}
	defer zero(dek)

	encDEK, err := e.kms.Encrypt(ctx, keyID, dek)
	if err != nil {
		return nil, fmt.Errorf("crypto: encrypting data key: %w", err)
	}

	payload, err := sealAESGCM(dek, plaintext)
	if err != nil {
		return nil, fmt.Errorf("crypto: sealing payload: %w", err)
	}

	return packEnvelope(encDEK, payload), nil
}

// Open reverses Seal. keyID must be the same value passed to Seal — the
// wrong keyID (or a tampered blob) fails closed, since GCM's
// authentication tag covers both the data key and the payload.
func (e *Envelope) Open(ctx context.Context, keyID string, blob []byte) ([]byte, error) {
	encDEK, payload, err := unpackEnvelope(blob)
	if err != nil {
		return nil, err
	}

	dek, err := e.kms.Decrypt(ctx, keyID, encDEK)
	if err != nil {
		return nil, fmt.Errorf("crypto: decrypting data key: %w", err)
	}
	defer zero(dek)

	plaintext, err := openAESGCM(dek, payload)
	if err != nil {
		return nil, fmt.Errorf("crypto: opening payload: %w", err)
	}
	return plaintext, nil
}

// packEnvelope frames [uint32 big-endian length][encDEK][payload] into one
// blob, so Open needs nothing but keyID and the blob itself.
func packEnvelope(encDEK, payload []byte) []byte {
	out := make([]byte, 4+len(encDEK)+len(payload))
	binary.BigEndian.PutUint32(out, uint32(len(encDEK))) //nolint:gosec // encDEK is a KMS-wrapped 32-byte key; always far under 2^32
	copy(out[4:], encDEK)
	copy(out[4+len(encDEK):], payload)
	return out
}

func unpackEnvelope(blob []byte) (encDEK, payload []byte, err error) {
	if len(blob) < 4 {
		return nil, nil, fmt.Errorf("crypto: envelope blob too short")
	}
	n := int(binary.BigEndian.Uint32(blob))
	if n < 0 || n > len(blob)-4 {
		return nil, nil, fmt.Errorf("crypto: envelope blob malformed: encrypted data key length out of range")
	}
	return blob[4 : 4+n], blob[4+n:], nil
}

// zero is a best-effort attempt to clear a data key from memory once it's
// no longer needed. Not a hard guarantee under a garbage-collected
// runtime, but cheap and worth doing for the one place in this package
// that holds a raw symmetric key.
func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
