package domain_test

import (
	"testing"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/domain"
)

func TestExpireHold(t *testing.T) {
	cases := []struct {
		name string
		hold domain.Hold
		want domain.HoldChange
	}{
		{"an active hold expires and gives everything back", active(400),
			domain.HoldChange{Status: domain.HoldExpired, Release: 400, Settle: true}},
		{"a captured hold is left for settlement", domain.Hold{Status: domain.HoldCaptured, Amount: 400, Captured: 380},
			domain.HoldChange{Replay: true}},
		{"a released hold stays released", domain.Hold{Status: domain.HoldReleased, Amount: 400, Settled: true},
			domain.HoldChange{Replay: true}},
		{"an expired hold stays expired", domain.Hold{Status: domain.HoldExpired, Amount: 400, Settled: true},
			domain.HoldChange{Replay: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := domain.ExpireHold(tc.hold); got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestAutoBuy: system design §4.2, "when tokens drop below the client's
// level, the rule buys a set amount", inside the hold that needs it
// (OQ-17). A purchase that wouldn't let the hold through isn't made.
func TestAutoBuy(t *testing.T) {
	rule := domain.AutoBuyRule{Enabled: true, BelowTokens: dec("100"), BuyTokens: dec("1000")}
	disabled := rule
	disabled.Enabled = false

	cases := []struct {
		name                 string
		rule                 domain.AutoBuyRule
		available, requested string
		buy                  bool
		reason               string
	}{
		{"a hold leaving 50 of a level of 100 buys first", rule, "150", "100", true, domain.AutoBuyBelowLevel},
		{"a hold beyond what's available buys to cover it", rule, "150", "900", true, domain.AutoBuyCoverHold},
		{"a hold leaving exactly the level doesn't buy", rule, "200", "100", false, ""},
		{"a hold leaving more doesn't buy", rule, "5000", "100", false, ""},
		{"a purchase that wouldn't cover the hold isn't made", rule, "150", "1150.000001", false, ""},
		{"a purchase that covers it exactly is made", rule, "150", "1150", true, domain.AutoBuyCoverHold},
		{"a disabled rule never buys", disabled, "0", "1", false, ""},
		{"a level of 0 buys only to cover", domain.AutoBuyRule{Enabled: true, BelowTokens: dec("0"), BuyTokens: dec("10")}, "5", "5", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buy, reason := domain.AutoBuy(tc.rule, dec(tc.available), dec(tc.requested))
			if buy != tc.buy || reason != tc.reason {
				t.Fatalf("got %v %q, want %v %q", buy, reason, tc.buy, tc.reason)
			}
		})
	}
}
