package crypto

import (
	"crypto/md5" //nolint:gosec // v3's own legacy pre-hash step, verify-only — see BcryptMD5Verify's doc comment
	"encoding/hex"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

// v3BcryptPrefixes are every prefix golang.org/x/crypto/bcrypt itself
// produces ($2a$ is what x/crypto/bcrypt.GenerateFromPassword writes
// today; $2b$/$2y$ are other bcrypt implementations' own variants,
// including whatever PHP's password_hash/crypt produced for v3 — kept
// all three so detection doesn't depend on exactly which one v3 used).
var v3BcryptPrefixes = []string{"$2a$", "$2b$", "$2y$"}

// IsBcryptHash reports whether encoded looks like a bcrypt hash rather
// than this package's own Argon2id encoding ($argon2id$...,
// Argon2Hash's exact prefix). identity.users.password_hash holds
// either shape after task 1.13's v3 import: a freshly-imported v3
// hash is bcrypt; everything hashed by v4 itself (signup, password
// reset, and 1.13's own one-time upgrade below) is Argon2id.
func IsBcryptHash(encoded string) bool {
	for _, prefix := range v3BcryptPrefixes {
		if strings.HasPrefix(encoded, prefix) {
			return true
		}
	}
	return false
}

// BcryptMD5Verify checks secret against hash using v3's own scheme —
// bcrypt of the hex-encoded MD5 of the plaintext password, not of the
// password directly (task 1.13: "v3 passwords are bcrypt of MD5").
// MD5 is broken for collision resistance, but that's irrelevant here:
// it's v3's own legacy pre-hash step, already baked into every
// imported hash, and this function only ever verifies an existing one
// — it never produces a new hash this way. A caller that verifies
// successfully is expected to re-hash the same secret with Argon2Hash
// and store that instead, so this check only ever has to succeed once
// per account.
func BcryptMD5Verify(hash []byte, secret string) bool {
	sum := md5.Sum([]byte(secret)) //nolint:gosec // see package-level note above
	hexSum := hex.EncodeToString(sum[:])
	return bcrypt.CompareHashAndPassword(hash, []byte(hexSum)) == nil
}
