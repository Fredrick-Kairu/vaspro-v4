package crypto

import (
	"bytes"
	"context"
	"testing"
)

func TestLocalKMS_RoundTrip(t *testing.T) {
	kms, err := NewLocalKMS(map[string][]byte{"dev-kek": key32(0x10)})
	if err != nil {
		t.Fatalf("NewLocalKMS: %v", err)
	}

	ctx := context.Background()
	plaintext := []byte("a 32-byte data encryption key...")

	ciphertext, err := kms.Encrypt(ctx, "dev-kek", plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	got, err := kms.Decrypt(ctx, "dev-kek", ciphertext)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("got %q, want %q", got, plaintext)
	}
}

func TestLocalKMS_UnknownKeyIDErrors(t *testing.T) {
	kms, err := NewLocalKMS(map[string][]byte{"dev-kek": key32(0x11)})
	if err != nil {
		t.Fatalf("NewLocalKMS: %v", err)
	}
	ctx := context.Background()

	if _, err := kms.Encrypt(ctx, "no-such-key", []byte("x")); err == nil {
		t.Fatal("expected Encrypt under an unknown key ID to error")
	}
	if _, err := kms.Decrypt(ctx, "no-such-key", []byte("x")); err == nil {
		t.Fatal("expected Decrypt under an unknown key ID to error")
	}
}

func TestNewLocalKMS_RejectsWrongLengthKey(t *testing.T) {
	if _, err := NewLocalKMS(map[string][]byte{"bad": []byte("too-short")}); err == nil {
		t.Fatal("expected a non-32-byte master key to be rejected")
	}
}

func TestLocalKMS_DecryptUnderWrongKeyFails(t *testing.T) {
	kms, err := NewLocalKMS(map[string][]byte{
		"key-a": key32(0x12),
		"key-b": key32(0x13),
	})
	if err != nil {
		t.Fatalf("NewLocalKMS: %v", err)
	}
	ctx := context.Background()

	ciphertext, err := kms.Encrypt(ctx, "key-a", []byte("secret"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if _, err := kms.Decrypt(ctx, "key-b", ciphertext); err == nil {
		t.Fatal("expected decrypting under a different master key to fail")
	}
}

func TestNewLocalKMS_DoesNotAliasCallerMap(t *testing.T) {
	keys := map[string][]byte{"dev-kek": key32(0x14)}
	kms, err := NewLocalKMS(keys)
	if err != nil {
		t.Fatalf("NewLocalKMS: %v", err)
	}

	keys["dev-kek"] = key32(0x15) // mutate the caller's copy after construction

	ctx := context.Background()
	ciphertext, err := kms.Encrypt(ctx, "dev-kek", []byte("secret"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if _, err := kms.Decrypt(ctx, "dev-kek", ciphertext); err != nil {
		t.Fatalf("Decrypt: %v (KMS must not have aliased the caller's map)", err)
	}
}
