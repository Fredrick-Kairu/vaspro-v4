package crypto

import (
	"bytes"
	"context"
	"testing"
)

func testEnvelope(t *testing.T) *Envelope {
	t.Helper()
	kms, err := NewLocalKMS(map[string][]byte{
		"dev-kek":   key32(0x20),
		"other-kek": key32(0x21),
	})
	if err != nil {
		t.Fatalf("NewLocalKMS: %v", err)
	}
	return NewEnvelope(kms)
}

func TestEnvelope_RoundTrip(t *testing.T) {
	env := testEnvelope(t)
	ctx := context.Background()
	plaintext := []byte("Daraja initiator password")

	blob, err := env.Seal(ctx, "dev-kek", plaintext)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Contains(blob, plaintext) {
		t.Fatal("sealed blob must not contain the plaintext")
	}

	got, err := env.Open(ctx, "dev-kek", blob)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("got %q, want %q", got, plaintext)
	}
}

func TestEnvelope_DifferentCiphertextPerSeal(t *testing.T) {
	env := testEnvelope(t)
	ctx := context.Background()
	plaintext := []byte("same secret every time")

	a, err := env.Seal(ctx, "dev-kek", plaintext)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	b, err := env.Seal(ctx, "dev-kek", plaintext)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Equal(a, b) {
		t.Fatal("two seals of the same plaintext must not produce the same blob")
	}
}

func TestEnvelope_OpenUnderWrongKeyIDFails(t *testing.T) {
	env := testEnvelope(t)
	ctx := context.Background()

	blob, err := env.Seal(ctx, "dev-kek", []byte("secret"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if _, err := env.Open(ctx, "other-kek", blob); err == nil {
		t.Fatal("expected Open under the wrong key ID to fail")
	}
}

func TestEnvelope_OpenTamperedBlobFails(t *testing.T) {
	env := testEnvelope(t)
	ctx := context.Background()

	blob, err := env.Seal(ctx, "dev-kek", []byte("secret"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	tampered := bytes.Clone(blob)
	tampered[len(tampered)-1] ^= 0xFF

	if _, err := env.Open(ctx, "dev-kek", tampered); err == nil {
		t.Fatal("expected Open on a tampered blob to fail")
	}
}

func TestEnvelope_OpenTruncatedBlobErrors(t *testing.T) {
	env := testEnvelope(t)
	ctx := context.Background()

	if _, err := env.Open(ctx, "dev-kek", []byte("short")); err == nil {
		t.Fatal("expected Open on a truncated blob to error, not panic")
	}
}
