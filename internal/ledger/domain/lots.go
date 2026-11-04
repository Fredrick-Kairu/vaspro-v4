package domain

import (
	"fmt"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// Lot is the part of a token lot FIFO use needs: tokens bought,
// tokens left, and the price locked at purchase (data model §5).
type Lot struct {
	ID        uuid.UUID
	Bought    decimal.Decimal
	Remaining decimal.Decimal
	Price     decimal.Decimal
}

// Draw is how many tokens one use takes from one lot, and their value.
type Draw struct {
	LotID      uuid.UUID
	Tokens     decimal.Decimal
	ValueMinor int64
}

// DrawFIFO takes tokens from lots, oldest first (lots must be in
// purchase order), so revenue and VAT are always worked out on the
// price actually paid.
//
// A draw is valued by cumulative rounding (OQ-15): the cost of
// everything drawn from the lot so far, minus the cost of what was
// drawn before this draw, each rounded as a purchase is (CostMinor).
// So a lot's draws always add up to exactly what it cost, the draw that
// empties it takes the rest, and no draw is ever negative. Rounding
// each draw on its own would drift: 5 tokens at 0.5 cost 3, but five
// draws of one token would take 5.
func DrawFIFO(lots []Lot, tokens decimal.Decimal) ([]Draw, int64, error) {
	need := tokens
	var draws []Draw
	var total int64
	for _, l := range lots {
		if !need.IsPositive() {
			break
		}
		take := decimal.Min(l.Remaining, need)
		if !take.IsPositive() {
			continue
		}
		before := l.Bought.Sub(l.Remaining)
		costBefore, err := CostMinor(before, l.Price)
		if err != nil {
			return nil, 0, err
		}
		costAfter, err := CostMinor(before.Add(take), l.Price)
		if err != nil {
			return nil, 0, err
		}
		value := costAfter - costBefore
		draws = append(draws, Draw{LotID: l.ID, Tokens: take, ValueMinor: value})
		total += value
		need = need.Sub(take)
	}
	if need.IsPositive() {
		return nil, 0, fmt.Errorf("%w: %s more needed", ErrInsufficientTokens, need)
	}
	return draws, total, nil
}

// ValueAfter is the FIFO value of tokens drawn after skip tokens: what
// a new token hold is shown as worth, behind the holds already placed.
// It's an estimate; settlement draws the real value.
func ValueAfter(lots []Lot, skip, tokens decimal.Decimal) (int64, error) {
	rest := make([]Lot, len(lots))
	copy(rest, lots)
	if skip.IsPositive() {
		draws, _, err := DrawFIFO(rest, skip)
		if err != nil {
			return 0, err
		}
		taken := map[uuid.UUID]decimal.Decimal{}
		for _, d := range draws {
			taken[d.LotID] = d.Tokens
		}
		for i := range rest {
			rest[i].Remaining = rest[i].Remaining.Sub(taken[rest[i].ID])
		}
	}
	_, value, err := DrawFIFO(rest, tokens)
	return value, err
}
