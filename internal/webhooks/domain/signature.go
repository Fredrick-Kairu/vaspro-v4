// Package domain holds the webhooks service's rules. It is pure Go and
// does no I/O.
package domain

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	vascrypto "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/crypto"
)

// The headers every webhook request carries (data model §8).
const (
	HeaderID        = "Vaspro-Webhook-Id"
	HeaderTimestamp = "Vaspro-Webhook-Timestamp"
	HeaderSignature = "Vaspro-Webhook-Signature"
)

// SecretPrefix starts every endpoint secret.
const SecretPrefix = "whsec_"

// SignatureTolerance is how far a timestamp may be from the receiver's
// clock, either way.
const SignatureTolerance = 5 * time.Minute

const (
	secretBytes   = 32
	secretLength  = len(SecretPrefix) + 43 // base64url of 32 bytes, unpadded
	schemeV1      = "v1="
	maxSecretsPer = 2 // the current one, and the previous during its overlap
)

var (
	// ErrInvalidSecret: not a secret GenerateSecret could have made.
	ErrInvalidSecret = errors.New("webhooks: invalid endpoint secret")
	// ErrBadSignature: no listed v1 signature matches.
	ErrBadSignature = errors.New("webhooks: signature does not match")
	// ErrBadTimestamp: missing, malformed, or too far from now.
	ErrBadTimestamp = errors.New("webhooks: timestamp missing, malformed or outside the tolerance")
)

// Signature is the timestamp and signature headers of one attempt.
type Signature struct {
	Timestamp string
	Value     string
}

// GenerateSecret makes an endpoint secret: SecretPrefix and 32 bytes
// from r (crypto/rand when nil), base64url without padding. The whole
// string, prefix included, is the HMAC key (OQ-23), so a client uses it
// exactly as shown.
func GenerateSecret(r io.Reader) (string, error) {
	if r == nil {
		r = rand.Reader
	}
	b := make([]byte, secretBytes)
	if _, err := io.ReadFull(r, b); err != nil {
		return "", fmt.Errorf("webhooks: generating a secret: %w", err)
	}
	return SecretPrefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// ValidSecret reports whether s has the shape GenerateSecret makes.
func ValidSecret(s string) bool {
	rest, ok := strings.CutPrefix(s, SecretPrefix)
	if !ok || len(s) != secretLength {
		return false
	}
	b, err := base64.RawURLEncoding.Strict().DecodeString(rest)
	return err == nil && len(b) == secretBytes
}

// Sign signs body for one attempt at time at: a v1 HMAC-SHA256 of
// "<unix seconds>.<body>" under each secret, the current one first.
// During a rotation two secrets give two space-separated values.
func Sign(secrets []string, at time.Time, body []byte) (Signature, error) {
	if len(secrets) == 0 || len(secrets) > maxSecretsPer {
		return Signature{}, fmt.Errorf("%w: %d secrets, want 1 or %d", ErrInvalidSecret, len(secrets), maxSecretsPer)
	}
	if len(secrets) == 2 && secrets[0] == secrets[1] {
		return Signature{}, fmt.Errorf("%w: the same secret twice", ErrInvalidSecret)
	}
	ts := strconv.FormatInt(at.Unix(), 10)
	values := make([]string, 0, len(secrets))
	for _, s := range secrets {
		if !ValidSecret(s) {
			return Signature{}, ErrInvalidSecret
		}
		values = append(values, schemeV1+hex.EncodeToString(vascrypto.HMACSign([]byte(s), signedPayload(ts, body))))
	}
	return Signature{Timestamp: ts, Value: strings.Join(values, " ")}, nil
}

// Verify is the receiver's check, which the 5.7 snippets mirror: the
// timestamp is decimal Unix seconds within SignatureTolerance of now,
// and some v1 value in signature matches under secret, compared in
// constant time. Other schemes are ignored.
func Verify(secret, timestamp, signature string, body []byte, now time.Time) error {
	if !ValidSecret(secret) {
		return ErrInvalidSecret
	}
	if timestamp == "" || strings.Trim(timestamp, "0123456789") != "" {
		return ErrBadTimestamp
	}
	unix, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return ErrBadTimestamp
	}
	if d := now.Sub(time.Unix(unix, 0)); d > SignatureTolerance || d < -SignatureTolerance {
		return ErrBadTimestamp
	}
	payload := signedPayload(timestamp, body)
	for v := range strings.SplitSeq(signature, " ") {
		h, ok := strings.CutPrefix(v, schemeV1)
		if !ok {
			continue
		}
		mac, err := hex.DecodeString(h)
		if err != nil {
			continue
		}
		if vascrypto.HMACVerify([]byte(secret), payload, mac) {
			return nil
		}
	}
	return ErrBadSignature
}

func signedPayload(timestamp string, body []byte) []byte {
	p := make([]byte, 0, len(timestamp)+1+len(body))
	p = append(p, timestamp...)
	p = append(p, '.')
	return append(p, body...)
}
