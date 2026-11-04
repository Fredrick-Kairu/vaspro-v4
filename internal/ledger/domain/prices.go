package domain

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// ErrInvalidPrice covers a token price staff can't enter as given.
var ErrInvalidPrice = errors.New("ledger: invalid token price")

// NewPrice is a token price as staff enter it (task 3.4b): a list price
// (no tenant) or a client's deal, with decimals as written.
type NewPrice struct {
	TenantID      *uuid.UUID
	ServiceCode   string
	Currency      string
	MinTokens     string // empty means 0
	MinorPerToken string
	DealReference string
	From          time.Time
	To            *time.Time
}

// ValidPrice is a NewPrice's decimals, parsed.
type ValidPrice struct {
	MinTokens     decimal.Decimal
	MinorPerToken decimal.Decimal
}

// maxPrice keeps minor_per_token inside numeric(18,6).
var maxPrice = decimal.New(1, 12)

// ValidateNewPrice checks a price before it's stored (OQ-15): a
// positive price and a non-negative tier with at most six decimals
// (numeric(18,6)); a deal names the signed quote behind it and a list
// price doesn't; an end, if any, comes after the start.
func ValidateNewPrice(p NewPrice) (ValidPrice, error) {
	if err := ValidateAccountKey(AccountKey{Type: ServiceWallet, ServiceCode: p.ServiceCode, Currency: p.Currency}); err != nil {
		return ValidPrice{}, fmt.Errorf("%w: %v", ErrInvalidPrice, err)
	}
	price, err := scaledDecimal(p.MinorPerToken)
	if err != nil || !price.IsPositive() || !price.LessThan(maxPrice) {
		return ValidPrice{}, fmt.Errorf("%w: minor_per_token must be a positive decimal with at most %d places", ErrInvalidPrice, TokenScale)
	}
	minTokens := decimal.Zero
	if p.MinTokens != "" {
		minTokens, err = scaledDecimal(p.MinTokens)
		if err != nil || minTokens.GreaterThan(MaxTokens) {
			return ValidPrice{}, fmt.Errorf("%w: min_tokens must be a decimal from 0 with at most %d places", ErrInvalidPrice, TokenScale)
		}
	}
	switch {
	case p.TenantID != nil && p.DealReference == "":
		return ValidPrice{}, fmt.Errorf("%w: a deal needs the reference of its signed quote or agreement", ErrInvalidPrice)
	case p.TenantID == nil && p.DealReference != "":
		return ValidPrice{}, fmt.Errorf("%w: a list price has no deal reference", ErrInvalidPrice)
	case len(p.DealReference) > 200:
		return ValidPrice{}, fmt.Errorf("%w: the deal reference is too long", ErrInvalidPrice)
	case p.From.IsZero():
		return ValidPrice{}, fmt.Errorf("%w: effective_from is required", ErrInvalidPrice)
	case p.To != nil && !p.To.After(p.From):
		return ValidPrice{}, fmt.Errorf("%w: effective_to must be after effective_from", ErrInvalidPrice)
	}
	return ValidPrice{MinTokens: minTokens, MinorPerToken: price}, nil
}

// scaledDecimal reads a non-negative plain decimal with at most
// TokenScale places.
func scaledDecimal(s string) (decimal.Decimal, error) {
	if !plainDecimal.MatchString(s) {
		return decimal.Zero, fmt.Errorf("%q is not a plain decimal", s)
	}
	d, err := decimal.NewFromString(s)
	if err != nil {
		return decimal.Zero, err
	}
	if !d.Equal(d.Truncate(TokenScale)) {
		return decimal.Zero, fmt.Errorf("%q has more than %d places", s, TokenScale)
	}
	return d, nil
}
