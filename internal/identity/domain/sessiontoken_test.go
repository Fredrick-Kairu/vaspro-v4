package domain_test

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/domain"
)

func TestGenerateSessionToken_PrefixedFull32ByteSecret(t *testing.T) {
	token, err := domain.GenerateSessionToken()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(token, "sess_") {
		t.Fatalf("token %q doesn't have the sess_ prefix", token)
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(token, "sess_"))
	if err != nil {
		t.Fatalf("token body isn't raw base64url: %v", err)
	}
	if len(raw) != 32 {
		t.Fatalf("got %d random bytes, want 32", len(raw))
	}
}

func TestGenerateSessionToken_DiffersAcrossCalls(t *testing.T) {
	first, err := domain.GenerateSessionToken()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	second, err := domain.GenerateSessionToken()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if first == second {
		t.Fatal("two generated session tokens were identical")
	}
}

func TestHashSessionToken_DeterministicAndDistinct(t *testing.T) {
	a := domain.HashSessionToken("sess_a")
	if !bytes.Equal(a, domain.HashSessionToken("sess_a")) {
		t.Fatal("hashing the same token twice gave different results")
	}
	if bytes.Equal(a, domain.HashSessionToken("sess_b")) {
		t.Fatal("two different tokens hashed the same")
	}
	if len(a) != 32 {
		t.Fatalf("got a %d-byte hash, want 32 (SHA-256)", len(a))
	}
}
