package crypto

import (
	"context"
	"fmt"
)

// KMS encrypts and decrypts short values — in this codebase, always a
// data-encryption key, never an arbitrary-size payload — under a named
// master key it holds. In production this is Cloud KMS or Vault (ROADMAP
// §6); LocalKMS is development's stand-in, named in the same table.
type KMS interface {
	Encrypt(ctx context.Context, keyID string, plaintext []byte) ([]byte, error)
	Decrypt(ctx context.Context, keyID string, ciphertext []byte) ([]byte, error)
}

// LocalKMS implements KMS with in-process master keys. Development only —
// nothing here is a substitute for an actual KMS's access control or
// audit trail.
type LocalKMS struct {
	keys map[string][]byte // keyID -> 32-byte master key
}

// NewLocalKMS builds a LocalKMS from named master keys, each exactly 32
// bytes (AES-256).
func NewLocalKMS(keys map[string][]byte) (*LocalKMS, error) {
	cp := make(map[string][]byte, len(keys))
	for id, key := range keys {
		if len(key) != 32 {
			return nil, fmt.Errorf("crypto: local KMS key %q: want 32 bytes, got %d", id, len(key))
		}
		cp[id] = key
	}
	return &LocalKMS{keys: cp}, nil
}

func (k *LocalKMS) Encrypt(_ context.Context, keyID string, plaintext []byte) ([]byte, error) {
	key, ok := k.keys[keyID]
	if !ok {
		return nil, fmt.Errorf("crypto: local KMS: unknown key %q", keyID)
	}
	return sealAESGCM(key, plaintext)
}

func (k *LocalKMS) Decrypt(_ context.Context, keyID string, ciphertext []byte) ([]byte, error) {
	key, ok := k.keys[keyID]
	if !ok {
		return nil, fmt.Errorf("crypto: local KMS: unknown key %q", keyID)
	}
	return openAESGCM(key, ciphertext)
}
