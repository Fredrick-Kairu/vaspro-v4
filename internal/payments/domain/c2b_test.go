package domain_test

import (
	"crypto/rand"
	"strings"
	"testing"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments/domain"
)

// TestMatchC2B: which account number a paybill payment names, or none
// (suspense).
func TestMatchC2B(t *testing.T) {
	n, err := domain.NewAccountNumber(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	typo := n[:3] + string(other(n[3])) + n[4:]
	swapped := n[:2] + string(n[3]) + string(n[2]) + n[4:]
	if n[2] == n[3] {
		swapped = n // nothing to swap; covered by other runs
	}
	for name, tc := range map[string]struct {
		billRef, shortCode string
		want               string // "" is suspense
	}{
		"as issued":             {n, "600100", n},
		"lower case and spaced": {" " + strings.ToLower(n[:4]) + " " + n[4:] + " ", "600100", n},
		"a one-character typo":  {typo, "600100", ""},
		"blank":                 {"", "600100", ""},
		"an invoice number":     {"INV-2026-0042", "600100", ""},
		"a phone number":        {"0708374149", "600100", ""},
		"another paybill":       {n, "600200", ""},
	} {
		got, ok := domain.MatchC2B(tc.billRef, tc.shortCode, "600100")
		if (tc.want == "") == ok || got != tc.want {
			t.Errorf("%s: got %q %v, want %q", name, got, ok, tc.want)
		}
	}
	if swapped != n {
		if _, ok := domain.MatchC2B(swapped, "600100", "600100"); ok {
			t.Errorf("a swap of neighbours matched: %s for %s", swapped, n)
		}
	}
}

// other is a different character of the alphabet.
func other(c byte) byte {
	for i := range len(domain.AccountNumberAlphabet) {
		if domain.AccountNumberAlphabet[i] != c {
			return domain.AccountNumberAlphabet[i]
		}
	}
	return c
}
