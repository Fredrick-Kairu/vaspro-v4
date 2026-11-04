package domain

import (
	"errors"
	"fmt"

	"github.com/shopspring/decimal"
)

// ErrInvalidAutoBuyRule covers a rule that can't be set as given.
var ErrInvalidAutoBuyRule = errors.New("ledger: invalid auto-buy rule")

// ValidateAutoBuyRule checks a rule before it's stored: a service and
// currency as for a service wallet, a level of 0 or more and a positive
// amount to buy, each with at most six decimals (numeric(18,6)).
func ValidateAutoBuyRule(serviceCode, currency string, below, buy decimal.Decimal) error {
	if err := ValidateAccountKey(AccountKey{Type: ServiceWallet, ServiceCode: serviceCode, Currency: currency}); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidAutoBuyRule, err)
	}
	if _, err := ParseTokens(buy.String()); err != nil {
		return fmt.Errorf("%w: buy_tokens: %v", ErrInvalidAutoBuyRule, err)
	}
	if below.IsNegative() || !below.Equal(below.Truncate(TokenScale)) || below.GreaterThan(MaxTokens) {
		return fmt.Errorf("%w: below_tokens must be 0 or more with at most %d places", ErrInvalidAutoBuyRule, TokenScale)
	}
	return nil
}

// Why an auto-buy rule bought (autobuy.triggered's reason).
const (
	// AutoBuyBelowLevel: the hold would have left fewer tokens than the
	// rule's level.
	AutoBuyBelowLevel = "below_level"
	// AutoBuyCoverHold: the hold couldn't have been placed without the
	// purchase.
	AutoBuyCoverHold = "cover_hold"
)

// AutoBuyRule is the part of an auto_buy_rules row the decision needs.
type AutoBuyRule struct {
	Enabled     bool
	BelowTokens decimal.Decimal
	BuyTokens   decimal.Decimal
}

// ExpireHold is what the expiry job (task 3.6) does to a hold: an
// active one expires and gives everything back; a captured one is left
// for settlement, and a released or expired one has nothing left to
// give, so both are replays.
func ExpireHold(h Hold) HoldChange {
	if h.Status != HoldActive {
		return HoldChange{Replay: true}
	}
	return HoldChange{Status: HoldExpired, Release: h.Amount, Settle: true}
}

// AutoBuy decides whether a token hold of requested, against available
// tokens, first buys the rule's set amount (system design §4.2, OQ-17):
// when the hold can't be covered, or would leave fewer than the rule's
// level. A purchase that still wouldn't cover the hold isn't made, so
// a refused send never leaves a purchase behind.
func AutoBuy(r AutoBuyRule, available, requested decimal.Decimal) (buy bool, reason string) {
	if !r.Enabled {
		return false, ""
	}
	after := available.Sub(requested)
	switch {
	case after.IsNegative():
		if available.Add(r.BuyTokens).LessThan(requested) {
			return false, ""
		}
		return true, AutoBuyCoverHold
	case after.LessThan(r.BelowTokens):
		return true, AutoBuyBelowLevel
	default:
		return false, ""
	}
}
