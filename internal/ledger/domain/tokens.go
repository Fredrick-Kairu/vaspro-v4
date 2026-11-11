package domain

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// TokenScale is numeric(18,6)'s scale: tokens and prices carry up to
// six decimal places (data model §5, "Fractional tokens are allowed").
const TokenScale = 6

var (
	// ErrInvalidTokens: a token quantity that isn't positive, has more
	// than six decimals, or exceeds MaxTokens.
	ErrInvalidTokens = errors.New("ledger: invalid token quantity")
	// ErrNoPrice: no active price row applies (BAL-4005).
	ErrNoPrice = errors.New("ledger: no token price for this service")
	// ErrInsufficientTokens: the wallet's open lots don't hold enough
	// tokens.
	ErrInsufficientTokens = errors.New("ledger: not enough tokens")
)

// MaxTokens bounds one quantity: numeric(18,6) holds just under 10^12,
// and 10^12 micro-tokens fit an int64 with room to spare.
var MaxTokens = decimal.New(1, 12)

var plainDecimal = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)

// ParseTokens reads a token quantity written as a plain decimal.
func ParseTokens(s string) (decimal.Decimal, error) {
	if !plainDecimal.MatchString(s) {
		return decimal.Zero, fmt.Errorf("%w: %q is not a plain decimal", ErrInvalidTokens, s)
	}
	d, err := decimal.NewFromString(s)
	if err != nil {
		return decimal.Zero, fmt.Errorf("%w: %v", ErrInvalidTokens, err)
	}
	switch {
	case !d.IsPositive():
		return decimal.Zero, fmt.Errorf("%w: must be positive", ErrInvalidTokens)
	case !d.Equal(d.Truncate(TokenScale)):
		return decimal.Zero, fmt.Errorf("%w: at most %d decimal places", ErrInvalidTokens, TokenScale)
	case d.GreaterThan(MaxTokens):
		return decimal.Zero, fmt.Errorf("%w: at most %s", ErrInvalidTokens, MaxTokens)
	}
	return d, nil
}

// MicroTokens is a validated quantity in millionths of a token: the
// integer unit the hold state machine counts token holds in.
func MicroTokens(d decimal.Decimal) int64 { return d.Shift(TokenScale).IntPart() }

// TokensFromMicro is MicroTokens' inverse.
func TokensFromMicro(n int64) decimal.Decimal { return decimal.New(n, -TokenScale) }

// CostMinor is what tokens cost at price minor units per token, rounded
// half-up to whole minor units (OQ-15). It may be 0 for a tiny
// quantity; a purchase refuses that.
func CostMinor(tokens, price decimal.Decimal) (int64, error) {
	cost := tokens.Mul(price).Round(0) // halves away from zero; both are positive
	if cost.GreaterThan(decimal.NewFromInt(math.MaxInt64)) {
		return 0, fmt.Errorf("ledger: %s tokens at %s is too large an amount", tokens, price)
	}
	return cost.IntPart(), nil
}

// PriceRow is the part of a token_prices row SelectPrice needs, for one
// service and currency.
type PriceRow struct {
	ID            uuid.UUID
	TenantID      *uuid.UUID // nil: the list price
	MinTokens     decimal.Decimal
	MinorPerToken decimal.Decimal
	Status        string
	From          time.Time
	To            *time.Time
}

func (p PriceRow) inForce(at time.Time) bool {
	return p.Status == "active" && !at.Before(p.From) && (p.To == nil || at.Before(*p.To))
}

// SelectPrice applies data model §5's rule (OQ-15): the client's own
// active rows in force at, if it has any (a deal), else the list rows;
// of those, the row with the highest min_tokens not above qty. A deal
// in force always applies: below all its tiers, its lowest tier does.
func SelectPrice(rows []PriceRow, tenant uuid.UUID, at time.Time, qty decimal.Decimal) (PriceRow, error) {
	var deal, list []PriceRow
	for _, r := range rows {
		if !r.inForce(at) {
			continue
		}
		switch {
		case r.TenantID == nil:
			list = append(list, r)
		case *r.TenantID == tenant:
			deal = append(deal, r)
		}
	}
	if len(deal) > 0 {
		if best, ok := highestTierWithin(deal, qty); ok {
			return best, nil
		}
		lowest := deal[0]
		for _, r := range deal[1:] {
			if r.MinTokens.LessThan(lowest.MinTokens) {
				lowest = r
			}
		}
		return lowest, nil
	}
	if best, ok := highestTierWithin(list, qty); ok {
		return best, nil
	}
	return PriceRow{}, ErrNoPrice
}

func highestTierWithin(rows []PriceRow, qty decimal.Decimal) (PriceRow, bool) {
	var best PriceRow
	found := false
	for _, r := range rows {
		if r.MinTokens.GreaterThan(qty) {
			continue
		}
		if !found || r.MinTokens.GreaterThan(best.MinTokens) {
			best, found = r, true
		}
	}
	return best, found
}
