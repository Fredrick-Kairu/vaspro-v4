// Package money represents amounts as bigint minor units plus a currency
// (data model §1, D-05) — never a float, anywhere, including a VAT rate or
// an allocation weight. Every multiply-then-divide here goes through
// math/big so overflow is never a question worth reasoning about under
// time pressure later.
package money

import (
	"errors"
	"fmt"
	"math/big"
)

// Currency is an ISO 4217 code plus the number of minor-unit decimal
// places it uses (data model §1: KES/TZS 2, UGX 0).
type Currency struct {
	Code     string
	Exponent int
}

var (
	KES = Currency{Code: "KES", Exponent: 2}
	TZS = Currency{Code: "TZS", Exponent: 2}
	UGX = Currency{Code: "UGX", Exponent: 0}
)

// Money is an exact amount: minor units of a currency. Never construct one
// from a float.
type Money struct {
	minor    int64
	currency Currency
}

// New returns minor units of currency.
func New(minor int64, currency Currency) Money {
	return Money{minor: minor, currency: currency}
}

func (m Money) Minor() int64         { return m.minor }
func (m Money) Currency() Currency   { return m.currency }

// String renders the amount in major units, e.g. "190.00 KES" or,
// for a zero-exponent currency, "190 UGX".
func (m Money) String() string {
	if m.currency.Exponent == 0 {
		return fmt.Sprintf("%d %s", m.minor, m.currency.Code)
	}
	divisor := pow10(m.currency.Exponent)
	major := m.minor / divisor
	frac := m.minor % divisor
	if frac < 0 {
		frac = -frac
	}
	return fmt.Sprintf("%d.%0*d %s", major, m.currency.Exponent, frac, m.currency.Code)
}

func (m Money) requireSameCurrency(other Money) error {
	if m.currency != other.currency {
		return fmt.Errorf("money: currency mismatch: %s vs %s", m.currency.Code, other.currency.Code)
	}
	return nil
}

// Add returns m + other. Both must be the same currency.
func (m Money) Add(other Money) (Money, error) {
	if err := m.requireSameCurrency(other); err != nil {
		return Money{}, err
	}
	return Money{minor: m.minor + other.minor, currency: m.currency}, nil
}

// Sub returns m - other. Both must be the same currency.
func (m Money) Sub(other Money) (Money, error) {
	if err := m.requireSameCurrency(other); err != nil {
		return Money{}, err
	}
	return Money{minor: m.minor - other.minor, currency: m.currency}, nil
}

func pow10(n int) int64 {
	v := int64(1)
	for range n {
		v *= 10
	}
	return v
}

// Allocate splits total into len(weights) parts proportional to weights,
// such that the parts always sum exactly back to total: each share is
// floor(total*weight/sum(weights)), then the leftover minor units (there
// are always fewer than len(weights) of them) go one each to the shares
// with the largest fractional remainder, largest first, ties broken by
// input order.
func Allocate(total Money, weights []int64) ([]Money, error) {
	if total.minor < 0 {
		return nil, errors.New("money: Allocate: total must not be negative")
	}
	if len(weights) == 0 {
		return nil, errors.New("money: Allocate: at least one weight is required")
	}

	sumWeights := big.NewInt(0)
	for i, w := range weights {
		if w < 0 {
			return nil, fmt.Errorf("money: Allocate: weight[%d] must not be negative", i)
		}
		sumWeights.Add(sumWeights, big.NewInt(w))
	}
	if sumWeights.Sign() == 0 {
		return nil, errors.New("money: Allocate: weights must not all be zero")
	}

	totalBig := big.NewInt(total.minor)
	type share struct {
		idx       int
		base      int64
		remainder *big.Int // total*weight mod sumWeights, for ranking
	}
	shares := make([]share, len(weights))

	allocated := big.NewInt(0)
	for i, w := range weights {
		numerator := new(big.Int).Mul(totalBig, big.NewInt(w))
		base, remainder := new(big.Int).QuoRem(numerator, sumWeights, new(big.Int))
		shares[i] = share{idx: i, base: base.Int64(), remainder: remainder}
		allocated.Add(allocated, base)
	}

	leftover := total.minor - allocated.Int64()

	// Largest remainder first; input order breaks ties (stable sort over
	// an already-input-ordered slice achieves that for free).
	ordered := make([]int, len(shares))
	for i := range ordered {
		ordered[i] = i
	}
	for i := 1; i < len(ordered); i++ {
		for j := i; j > 0 && shares[ordered[j]].remainder.Cmp(shares[ordered[j-1]].remainder) > 0; j-- {
			ordered[j], ordered[j-1] = ordered[j-1], ordered[j]
		}
	}

	parts := make([]Money, len(weights))
	for i, s := range shares {
		parts[i] = New(s.base, total.currency)
	}
	for i := range leftover {
		idx := ordered[i]
		parts[idx] = New(parts[idx].minor+1, total.currency)
	}
	return parts, nil
}

// VATRateBasisPoints is a tax rate in basis points: 1600 = 16.00%. An
// integer, never a float, for the same reason Money.minor is an int64.
type VATRateBasisPoints int64

const basisPointsDenominator = 10000

// SplitVATInclusive treats total as already including tax at rate and
// returns (revenue, vat) summing back exactly to total (round-half-up on
// the unrounded revenue; data model's worked example: 190.00 at 16%
// inclusive → 163.79 revenue, 26.21 VAT — OD-06's default).
func SplitVATInclusive(total Money, rate VATRateBasisPoints) (revenue, vat Money, err error) {
	if total.minor < 0 {
		return Money{}, Money{}, errors.New("money: SplitVATInclusive: total must not be negative")
	}
	if rate < 0 {
		return Money{}, Money{}, errors.New("money: SplitVATInclusive: rate must not be negative")
	}

	numerator := new(big.Int).Mul(big.NewInt(total.minor), big.NewInt(basisPointsDenominator))
	denominator := big.NewInt(basisPointsDenominator + int64(rate))
	revenueMinor := roundHalfUp(numerator, denominator)

	revenue = New(revenueMinor, total.currency)
	vat, err = total.Sub(revenue)
	if err != nil {
		return Money{}, Money{}, err
	}
	return revenue, vat, nil
}

// AddVATExclusive treats base as pre-tax and returns (total, vat) where
// total = base + vat (the tax-exclusive variant: 190.00 base + 16% →
// 220.40 total, 30.40 VAT).
func AddVATExclusive(base Money, rate VATRateBasisPoints) (total, vat Money, err error) {
	if base.minor < 0 {
		return Money{}, Money{}, errors.New("money: AddVATExclusive: base must not be negative")
	}
	if rate < 0 {
		return Money{}, Money{}, errors.New("money: AddVATExclusive: rate must not be negative")
	}

	numerator := new(big.Int).Mul(big.NewInt(base.minor), big.NewInt(int64(rate)))
	vatMinor := roundHalfUp(numerator, big.NewInt(basisPointsDenominator))

	vat = New(vatMinor, base.currency)
	total, err = base.Add(vat)
	if err != nil {
		return Money{}, Money{}, err
	}
	return total, vat, nil
}

// roundHalfUp returns round(numerator/denominator), rounding exact halves
// away from zero. Both arguments must be non-negative.
func roundHalfUp(numerator, denominator *big.Int) int64 {
	doubled := new(big.Int).Lsh(numerator, 1) // 2*numerator
	doubled.Add(doubled, denominator)         // 2*numerator + denominator
	result := doubled.Div(doubled, new(big.Int).Lsh(denominator, 1))
	return result.Int64()
}
