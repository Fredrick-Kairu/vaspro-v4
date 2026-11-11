package domain

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	vascrypto "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/crypto"
)

var (
	// ErrBadCheckout means the text isn't a checkout link VASPRO signed:
	// forged, altered or truncated. It reads as not found (VAL-4003).
	ErrBadCheckout = errors.New("domain: not a checkout link")
	// ErrCheckoutExpired means the link was signed but has expired.
	ErrCheckoutExpired = errors.New("domain: the checkout link has expired")
)

// Checkout is what a checkout link carries (task 4.8): who is paid,
// how much, until when. Links are stateless: their payments are found
// by LinkID.
type Checkout struct {
	LinkID      uuid.UUID
	TenantID    uuid.UUID
	AmountMinor int64
	Currency    string
	Expires     time.Time
}

type checkoutPayload struct {
	L uuid.UUID `json:"l"`
	T uuid.UUID `json:"t"`
	A int64     `json:"a"`
	C string    `json:"c"`
	E int64     `json:"e"`
}

// tokenEncoding refuses non-canonical text, so no two strings decode
// to the same signature.
var tokenEncoding = base64.RawURLEncoding.Strict()

// SignCheckout makes a checkout link's token: its payload and an
// HMAC-SHA256 over the payload's text, both base64url.
func SignCheckout(secret []byte, c Checkout) string {
	payload, _ := json.Marshal(checkoutPayload{L: c.LinkID, T: c.TenantID, A: c.AmountMinor, C: c.Currency, E: c.Expires.Unix()})
	body := tokenEncoding.EncodeToString(payload)
	return body + "." + tokenEncoding.EncodeToString(vascrypto.HMACSign(secret, []byte(body)))
}

// ParseCheckout checks a token's signature in constant time, then its
// expiry at now.
func ParseCheckout(secret []byte, token string, now time.Time) (Checkout, error) {
	body, sigText, ok := strings.Cut(token, ".")
	if !ok || strings.Contains(sigText, ".") {
		return Checkout{}, ErrBadCheckout
	}
	sig, err := tokenEncoding.DecodeString(sigText)
	if err != nil || !vascrypto.HMACVerify(secret, []byte(body), sig) {
		return Checkout{}, ErrBadCheckout
	}
	raw, err := tokenEncoding.DecodeString(body)
	if err != nil {
		return Checkout{}, ErrBadCheckout
	}
	var p checkoutPayload
	if err := json.Unmarshal(raw, &p); err != nil || p.L == uuid.Nil || p.T == uuid.Nil || p.A <= 0 || p.C == "" {
		return Checkout{}, ErrBadCheckout
	}
	c := Checkout{LinkID: p.L, TenantID: p.T, AmountMinor: p.A, Currency: p.C, Expires: time.Unix(p.E, 0).UTC()}
	if !now.Before(c.Expires) {
		return c, ErrCheckoutExpired
	}
	return c, nil
}
