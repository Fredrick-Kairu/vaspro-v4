package crypto

import (
	"crypto/hmac"
	"crypto/sha256"
)

// HMACSign returns the HMAC-SHA256 of data under key. Two uses named in
// the design docs: system design §3's high-value-call request signature
// (over method, path, body and timestamp — task 1.9 builds the actual
// signing on top of this) and payments.rail_credentials.fingerprint, a
// keyed hash that lets a stored credential be compared without
// decrypting it.
func HMACSign(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return mac.Sum(nil)
}

// HMACVerify reports whether sig is the correct HMAC-SHA256 of data under
// key, comparing in constant time.
func HMACVerify(key, data, sig []byte) bool {
	return hmac.Equal(HMACSign(key, data), sig)
}
