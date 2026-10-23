package crypto

import (
	"crypto/md5" //nolint:gosec // building a v3-shaped test fixture hash, not production use
	"encoding/hex"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

// v3StyleHash reproduces exactly what v3 itself is documented to
// store (data model §3: "v3 passwords are bcrypt of MD5") — a test
// fixture, not production code (BcryptMD5Verify never hashes, only
// verifies).
func v3StyleHash(t *testing.T, password string) []byte {
	t.Helper()
	sum := md5.Sum([]byte(password)) //nolint:gosec // see v3StyleHash's own doc comment
	hash, err := bcrypt.GenerateFromPassword([]byte(hex.EncodeToString(sum[:])), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("bcrypt.GenerateFromPassword: %v", err)
	}
	return hash
}

func TestBcryptMD5Verify_CorrectPassword(t *testing.T) {
	hash := v3StyleHash(t, "the-v3-password")
	if !BcryptMD5Verify(hash, "the-v3-password") {
		t.Fatal("expected the correct v3 password to verify")
	}
}

func TestBcryptMD5Verify_WrongPassword(t *testing.T) {
	hash := v3StyleHash(t, "the-v3-password")
	if BcryptMD5Verify(hash, "not-the-v3-password") {
		t.Fatal("expected a wrong password to fail verification")
	}
}

func TestIsBcryptHash(t *testing.T) {
	tests := []struct {
		name    string
		encoded string
		want    bool
	}{
		{"v3-style bcrypt hash", string(v3StyleHash(t, "x")), true},
		{"argon2id hash", mustArgon2Hash(t, "x"), false},
		{"empty string", "", false},
		{"garbage", "not-a-hash-at-all", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsBcryptHash(tc.encoded); got != tc.want {
				t.Fatalf("IsBcryptHash(%q) = %v, want %v", tc.encoded, got, tc.want)
			}
		})
	}
}

func mustArgon2Hash(t *testing.T, secret string) string {
	t.Helper()
	encoded, err := Argon2Hash(secret)
	if err != nil {
		t.Fatalf("Argon2Hash: %v", err)
	}
	return encoded
}
