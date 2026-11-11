// Package domain holds payments' rules: pure Go, no I/O.
package domain

import (
	"errors"
	"fmt"
	"io"
	"strings"
)

// AccountNumberAlphabet is the 32 characters account numbers use: no 0,
// 1, I or O, which are misread on a phone. topup_accounts'
// account_number CHECK allows exactly these.
const AccountNumberAlphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"

// ErrInvalidAccountNumber means the text isn't a VASPRO account number.
var ErrInvalidAccountNumber = errors.New("domain: not a valid account number")

// An account number is 6 random characters and 2 check characters
// (OQ-22). The check is c = Σ (i+1)·vᵢ mod 1021 over the six values,
// written as two base-32 digits. 1021 is a prime larger than every
// weight times every difference of two values (6·31), so any single
// changed character, any swap of neighbours (across the check
// characters too), and any wrong check character changes the check: a
// payer's typo lands in suspense, never on another client's wallet.
const (
	accountDataLen  = 6
	accountNumLen   = accountDataLen + 2
	accountCheckMod = 1021
)

// NewAccountNumber makes a fresh number from rand (crypto/rand.Reader
// in production).
func NewAccountNumber(rand io.Reader) (string, error) {
	b := make([]byte, accountDataLen)
	if _, err := io.ReadFull(rand, b); err != nil {
		return "", fmt.Errorf("domain: reading randomness: %w", err)
	}
	vals := make([]int, accountDataLen)
	for i, x := range b {
		vals[i] = int(x) & 31 // 32 divides 256, so every character is equally likely
	}
	return encodeAccountNumber(vals), nil
}

func encodeAccountNumber(vals []int) string {
	var sb strings.Builder
	for _, v := range vals {
		sb.WriteByte(AccountNumberAlphabet[v])
	}
	c := accountCheck(vals)
	sb.WriteByte(AccountNumberAlphabet[c/32])
	sb.WriteByte(AccountNumberAlphabet[c%32])
	return sb.String()
}

func accountCheck(vals []int) int {
	sum := 0
	for i, v := range vals {
		sum += (i + 1) * v
	}
	return sum % accountCheckMod
}

// ValidAccountNumber reports whether s, exactly as given, is a number
// with correct check characters.
func ValidAccountNumber(s string) bool {
	if len(s) != accountNumLen {
		return false
	}
	vals := make([]int, accountNumLen)
	for i := range accountNumLen {
		v := strings.IndexByte(AccountNumberAlphabet, s[i])
		if v < 0 {
			return false
		}
		vals[i] = v
	}
	return accountCheck(vals[:accountDataLen]) == vals[accountDataLen]*32+vals[accountDataLen+1]
}

// NormalizeAccountNumber reads a number as a payer typed it: upper or
// lower case, with spaces or hyphens anywhere.
func NormalizeAccountNumber(s string) (string, error) {
	n := strings.ToUpper(strings.NewReplacer(" ", "", "-", "").Replace(s))
	if !ValidAccountNumber(n) {
		return "", ErrInvalidAccountNumber
	}
	return n, nil
}
