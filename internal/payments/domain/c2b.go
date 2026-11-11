package domain

import "strings"

// MatchC2B reads which account number a payment to VASPRO's paybill
// names (task 4.5): the number as the payer typed it, normalised. A
// payment to another shortcode, a blank reference, or one that isn't a
// valid number (any typo fails the check characters) names none, and
// goes to suspense for staff (OQ-22). Whether anyone holds the number
// is the caller's lookup.
func MatchC2B(billRef, shortCode, paybillNumber string) (string, bool) {
	if strings.TrimSpace(shortCode) != paybillNumber {
		return "", false
	}
	n, err := NormalizeAccountNumber(billRef)
	if err != nil {
		return "", false
	}
	return n, true
}
