package domain_test

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments/domain"
)

func TestCheckoutToken(t *testing.T) {
	secret := bytes.Repeat([]byte{3}, 32)
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	c := domain.Checkout{LinkID: uuid.New(), TenantID: uuid.New(), AmountMinor: 150000, Currency: "KES", Expires: now.Add(time.Hour).Truncate(time.Second)}
	tok := domain.SignCheckout(secret, c)

	got, err := domain.ParseCheckout(secret, tok, now)
	if err != nil || got != c {
		t.Fatalf("round trip: %+v %v, want %+v", got, err, c)
	}
	if _, err := domain.ParseCheckout(secret, tok, c.Expires); !errors.Is(err, domain.ErrCheckoutExpired) {
		t.Fatalf("at expiry: %v, want ErrCheckoutExpired", err)
	}

	// Every single-character change, anywhere in the token, is refused.
	alphabet := "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_."
	for i := range len(tok) {
		for _, c := range []byte{alphabet[0], alphabet[30], alphabet[63]} {
			if c == tok[i] {
				continue
			}
			bad := tok[:i] + string(c) + tok[i+1:]
			if _, err := domain.ParseCheckout(secret, bad, now); !errors.Is(err, domain.ErrBadCheckout) {
				t.Fatalf("position %d changed: %v, want ErrBadCheckout", i, err)
			}
		}
	}
	for name, bad := range map[string]string{
		"another secret": domain.SignCheckout(bytes.Repeat([]byte{4}, 32), c),
		"truncated":      tok[:len(tok)-3],
		"no signature":   tok[:bytes.IndexByte([]byte(tok), '.')],
		"empty":          "",
		"garbage":        "not.a-token",
	} {
		if _, err := domain.ParseCheckout(secret, bad, now); !errors.Is(err, domain.ErrBadCheckout) {
			t.Errorf("%s: %v, want ErrBadCheckout", name, err)
		}
	}
}
