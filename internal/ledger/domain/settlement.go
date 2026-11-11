package domain

import (
	"errors"
	"time"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/money"
)

// UsageSplit is how a settlement's drawn value becomes revenue and VAT
// (task 3.5, OQ-16). VATFromMain and VATToReceivable are non-zero only
// when prices exclude VAT: the VAT then comes on top of the value, from
// the client's main wallet up to its available balance, the rest as a
// debt on its receivable.
type UsageSplit struct {
	Revenue         int64
	VAT             int64
	VATFromMain     int64
	VATToReceivable int64
}

// SplitUsage splits value, the minor units drawn from a service
// wallet's lots, at rate (data model §5 step 4). Inclusive: revenue and
// VAT add up to value (304.00 → 262.07 + 41.93). Exclusive: revenue is
// value and VAT is added (48.64), so the usage posting's debits are the
// value plus the VAT's sources and its credits revenue plus VAT.
func SplitUsage(value int64, rate money.VATRateBasisPoints, inclusive bool, mainAvailable int64, currency money.Currency) (UsageSplit, error) {
	if value <= 0 {
		return UsageSplit{}, errors.New("ledger: only a positive usage value is split")
	}
	if inclusive {
		revenue, vat, err := money.SplitVATInclusive(money.New(value, currency), rate)
		if err != nil {
			return UsageSplit{}, err
		}
		return UsageSplit{Revenue: revenue.Minor(), VAT: vat.Minor()}, nil
	}
	_, vat, err := money.AddVATExclusive(money.New(value, currency), rate)
	if err != nil {
		return UsageSplit{}, err
	}
	fromMain := min(max(mainAvailable, 0), vat.Minor())
	return UsageSplit{Revenue: value, VAT: vat.Minor(), VATFromMain: fromMain, VATToReceivable: vat.Minor() - fromMain}, nil
}

// WindowFor is the settlement window that ended most recently at now:
// [start, end) on a grid of length from the zero time, so every
// replica of the ledger agrees on window boundaries (OD-07).
func WindowFor(now time.Time, length time.Duration) (start, end time.Time) {
	end = now.UTC().Truncate(length)
	return end.Add(-length), end
}
