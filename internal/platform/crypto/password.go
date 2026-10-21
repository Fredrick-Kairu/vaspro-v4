package crypto

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters. Nothing in the design docs fixes a specific cost —
// these are reasonable defaults (OWASP's current minimums), tunable later
// against a real latency/throughput measurement.
const (
	argon2Time    = 1
	argon2Memory  = 64 * 1024 // KiB (64 MiB)
	argon2Threads = 4
	argon2SaltLen = 16
	argon2KeyLen  = 32
)

// Argon2Hash hashes secret with Argon2id into the standard, self-
// describing $argon2id$v=19$m=...,t=...,p=...$salt$hash encoding (the
// same shape other Argon2id libraries produce), so Argon2Verify needs
// nothing but the encoded string back. Used for both
// identity.users.password_hash and identity.api_client_secrets.secret_hash
// — the data model gives both the same "Argon2id" treatment.
func Argon2Hash(secret string) (string, error) {
	salt := make([]byte, argon2SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("crypto: generating salt: %w", err)
	}

	hash := argon2.IDKey([]byte(secret), salt, argon2Time, argon2Memory, argon2Threads, argon2KeyLen)

	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s",
		argon2Memory, argon2Time, argon2Threads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(hash),
	), nil
}

// Argon2Verify reports whether secret matches the Argon2id hash encoded
// in encoded (as produced by Argon2Hash). A malformed encoding is an
// error, not a false result, so a corrupt stored hash can't be mistaken
// for "verification failed."
func Argon2Verify(encoded, secret string) (bool, error) {
	memory, time, threads, salt, hash, err := decodeArgon2id(encoded)
	if err != nil {
		return false, err
	}

	candidate := argon2.IDKey([]byte(secret), salt, time, memory, threads, uint32(len(hash))) //nolint:gosec // len(hash) comes from our own encoding, always small
	return subtle.ConstantTimeCompare(candidate, hash) == 1, nil
}

func decodeArgon2id(encoded string) (memory uint32, time uint32, threads uint8, salt, hash []byte, err error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return 0, 0, 0, nil, nil, fmt.Errorf("crypto: not a recognized argon2id encoding")
	}
	if parts[2] != "v=19" {
		return 0, 0, 0, nil, nil, fmt.Errorf("crypto: unsupported argon2 version %q", parts[2])
	}

	var m uint32
	var t uint32
	var p uint8
	if _, scanErr := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); scanErr != nil {
		return 0, 0, 0, nil, nil, fmt.Errorf("crypto: malformed argon2id parameters %q: %w", parts[3], scanErr)
	}

	salt, err = base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return 0, 0, 0, nil, nil, fmt.Errorf("crypto: decoding salt: %w", err)
	}
	hash, err = base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return 0, 0, 0, nil, nil, fmt.Errorf("crypto: decoding hash: %w", err)
	}

	return m, t, p, salt, hash, nil
}
