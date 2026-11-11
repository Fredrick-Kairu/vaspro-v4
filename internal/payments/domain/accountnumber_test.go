package domain_test

import (
	"bytes"
	"crypto/rand"
	"errors"
	"regexp"
	"testing"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments/domain"
)

// dbPattern is topup_accounts.account_number's CHECK (migration
// 20261111090000).
var dbPattern = regexp.MustCompile(`^[2-9A-HJ-NP-Z]{8}$`)

func TestAccountNumberAlphabet_MatchesTheDatabase(t *testing.T) {
	if len(domain.AccountNumberAlphabet) != 32 {
		t.Fatalf("alphabet has %d characters, want 32", len(domain.AccountNumberAlphabet))
	}
	for _, c := range domain.AccountNumberAlphabet {
		if !regexp.MustCompile(`^[2-9A-HJ-NP-Z]$`).MatchString(string(c)) {
			t.Fatalf("%q is outside the database's pattern", c)
		}
	}
}

func TestNewAccountNumber_IsValid(t *testing.T) {
	for range 2000 {
		n, err := domain.NewAccountNumber(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		if !dbPattern.MatchString(n) || !domain.ValidAccountNumber(n) {
			t.Fatalf("%q isn't a valid account number", n)
		}
	}
}

func TestNewAccountNumber_NoRandomness(t *testing.T) {
	if _, err := domain.NewAccountNumber(bytes.NewReader(nil)); err == nil {
		t.Fatal("an empty random source must fail, not produce a number")
	}
}

// Every typo a payer can make on one character, and every swap of two
// neighbours, is caught, so a mistyped number goes to suspense and
// never to another client.
func TestValidAccountNumber_CatchesTyposAndSwaps(t *testing.T) {
	alphabet := domain.AccountNumberAlphabet
	for range 2000 {
		n, err := domain.NewAccountNumber(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		for i := range len(n) {
			for j := range len(alphabet) {
				c := alphabet[j]
				if c == n[i] {
					continue
				}
				typo := n[:i] + string(c) + n[i+1:]
				if domain.ValidAccountNumber(typo) {
					t.Fatalf("%q with position %d changed to %q still validates", n, i, typo)
				}
			}
		}
		for i := range len(n) - 1 {
			if n[i] == n[i+1] {
				continue
			}
			swapped := n[:i] + string(n[i+1]) + string(n[i]) + n[i+2:]
			if domain.ValidAccountNumber(swapped) {
				t.Fatalf("%q with positions %d and %d swapped (%q) still validates", n, i, i+1, swapped)
			}
		}
	}
}

func TestNormalizeAccountNumber(t *testing.T) {
	n, err := domain.NewAccountNumber(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	lower := bytes.ToLower([]byte(n))
	for name, in := range map[string]string{
		"as issued":         n,
		"lower case":        string(lower),
		"spaces around":     "  " + n + " ",
		"grouped by spaces": n[:4] + " " + n[4:],
		"grouped by hyphen": n[:4] + "-" + n[4:],
	} {
		got, err := domain.NormalizeAccountNumber(in)
		if err != nil || got != n {
			t.Errorf("%s: %q → %q, %v; want %q", name, in, got, err, n)
		}
	}
	for name, in := range map[string]string{
		"empty":             "",
		"too short":         n[:7],
		"too long":          n + "2",
		"a zero":            "0" + n[1:],
		"a letter O":        "O" + n[1:],
		"punctuation":       n[:7] + ".",
		"an invoice number": "INV-2026-0042",
	} {
		if _, err := domain.NormalizeAccountNumber(in); !errors.Is(err, domain.ErrInvalidAccountNumber) {
			t.Errorf("%s: %q gave %v, want ErrInvalidAccountNumber", name, in, err)
		}
	}
}
