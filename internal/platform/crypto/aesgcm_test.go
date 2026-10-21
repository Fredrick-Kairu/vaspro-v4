package crypto

import (
	"bytes"
	"testing"
)

func key32(b byte) []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = b
	}
	return k
}

func TestAESGCM_RoundTrip(t *testing.T) {
	key := key32(0x01)
	plaintext := []byte("a rail credential secret")

	ciphertext, err := sealAESGCM(key, plaintext)
	if err != nil {
		t.Fatalf("sealAESGCM: %v", err)
	}
	if bytes.Equal(ciphertext, plaintext) {
		t.Fatal("ciphertext must not equal plaintext")
	}

	got, err := openAESGCM(key, ciphertext)
	if err != nil {
		t.Fatalf("openAESGCM: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("got %q, want %q", got, plaintext)
	}
}

func TestAESGCM_DifferentNoncePerSeal(t *testing.T) {
	key := key32(0x02)
	plaintext := []byte("same plaintext every time")

	a, err := sealAESGCM(key, plaintext)
	if err != nil {
		t.Fatalf("sealAESGCM: %v", err)
	}
	b, err := sealAESGCM(key, plaintext)
	if err != nil {
		t.Fatalf("sealAESGCM: %v", err)
	}
	if bytes.Equal(a, b) {
		t.Fatal("two seals of the same plaintext must not produce the same ciphertext")
	}
}

func TestAESGCM_TamperedCiphertextFails(t *testing.T) {
	key := key32(0x03)
	ciphertext, err := sealAESGCM(key, []byte("tamper me"))
	if err != nil {
		t.Fatalf("sealAESGCM: %v", err)
	}
	tampered := bytes.Clone(ciphertext)
	tampered[len(tampered)-1] ^= 0xFF

	if _, err := openAESGCM(key, tampered); err == nil {
		t.Fatal("expected tampered ciphertext to fail authentication")
	}
}

func TestAESGCM_WrongKeyFails(t *testing.T) {
	ciphertext, err := sealAESGCM(key32(0x04), []byte("secret"))
	if err != nil {
		t.Fatalf("sealAESGCM: %v", err)
	}
	if _, err := openAESGCM(key32(0x05), ciphertext); err == nil {
		t.Fatal("expected the wrong key to fail authentication")
	}
}

func TestAESGCM_WrongKeyLengthErrors(t *testing.T) {
	if _, err := sealAESGCM([]byte("too-short"), []byte("x")); err == nil {
		t.Fatal("expected a non-32-byte key to error")
	}
}

func TestAESGCM_TruncatedCiphertextErrors(t *testing.T) {
	if _, err := openAESGCM(key32(0x06), []byte("short")); err == nil {
		t.Fatal("expected ciphertext shorter than a nonce to error")
	}
}
