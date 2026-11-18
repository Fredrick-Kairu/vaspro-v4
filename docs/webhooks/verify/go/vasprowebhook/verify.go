// Package vasprowebhook verifies the signature on a VASPRO webhook.
// It uses only the standard library: copy this file into your project.
//
//	body, _ := io.ReadAll(r.Body) // the raw bytes, before any JSON decoding
//	err := vasprowebhook.Verify(secret,
//		r.Header.Get("Vaspro-Webhook-Timestamp"),
//		r.Header.Get("Vaspro-Webhook-Signature"),
//		body, time.Now())
//	if err != nil {
//		http.Error(w, "bad signature", http.StatusBadRequest)
//		return
//	}
package vasprowebhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Tolerance is how far the timestamp may be from your clock, either way.
const Tolerance = 5 * time.Minute

var (
	// ErrBadTimestamp: the timestamp is missing, malformed, or more than
	// Tolerance from now (a replayed or delayed request).
	ErrBadTimestamp = errors.New("vaspro webhook: timestamp missing, malformed or too old")
	// ErrBadSignature: no v1 signature matches the body under the secret.
	ErrBadSignature = errors.New("vaspro webhook: signature does not match")
)

var digits = regexp.MustCompile(`^[0-9]+$`)

// Verify checks a webhook request. secret is the endpoint's secret
// exactly as shown (whsec_…, used whole as the HMAC key); timestamp and
// signature are the Vaspro-Webhook-Timestamp and -Signature headers;
// body is the raw request body.
func Verify(secret, timestamp, signature string, body []byte, now time.Time) error {
	if !digits.MatchString(timestamp) {
		return ErrBadTimestamp
	}
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return ErrBadTimestamp
	}
	if d := now.Sub(time.Unix(ts, 0)); d > Tolerance || d < -Tolerance {
		return ErrBadTimestamp
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp + "."))
	mac.Write(body)
	expected := []byte(hex.EncodeToString(mac.Sum(nil)))

	// During a secret rotation there are two values, space-separated.
	// Values with other schemes are for later versions; skip them.
	for _, v := range strings.Split(signature, " ") {
		if h, ok := strings.CutPrefix(v, "v1="); ok && hmac.Equal([]byte(strings.ToLower(h)), expected) {
			return nil
		}
	}
	return ErrBadSignature
}
