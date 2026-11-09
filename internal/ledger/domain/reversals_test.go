package domain_test

import (
	"errors"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/domain"
)

func TestValidateNewReversal(t *testing.T) {
	ok := domain.NewReversal{Kind: domain.ReversalPayback, Currency: "KES", AmountMinor: 100, Reason: "duplicate payment", ClawbackServices: []string{"sms", "whatsapp"}}
	tests := []struct {
		name   string
		mutate func(*domain.NewReversal)
		valid  bool
	}{
		{"a pay-back with two wallets to claw back from", func(*domain.NewReversal) {}, true},
		{"a chargeback with no wallets", func(r *domain.NewReversal) { r.Kind = domain.ReversalChargeback; r.ClawbackServices = nil }, true},
		{"an operator reversal", func(r *domain.NewReversal) { r.Kind = domain.ReversalOperator }, true},
		{"an unknown kind", func(r *domain.NewReversal) { r.Kind = "refund" }, false},
		{"a zero amount", func(r *domain.NewReversal) { r.AmountMinor = 0 }, false},
		{"a negative amount", func(r *domain.NewReversal) { r.AmountMinor = -1 }, false},
		{"no reason", func(r *domain.NewReversal) { r.Reason = "  " }, false},
		{"a wallet named twice", func(r *domain.NewReversal) { r.ClawbackServices = []string{"sms", "sms"} }, false},
		{"a malformed service code", func(r *domain.NewReversal) { r.ClawbackServices = []string{"SMS!"} }, false},
		{"a malformed currency", func(r *domain.NewReversal) { r.Currency = "kes" }, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := ok
			r.ClawbackServices = append([]string(nil), ok.ClawbackServices...)
			tc.mutate(&r)
			err := domain.ValidateNewReversal(r)
			if tc.valid && err != nil {
				t.Fatalf("refused: %v", err)
			}
			if !tc.valid && !errors.Is(err, domain.ErrInvalidReversal) {
				t.Fatalf("err = %v, want ErrInvalidReversal", err)
			}
		})
	}
}

func TestCheckReversalCap(t *testing.T) {
	tests := []struct {
		name                   string
		original, live, amount int64
		ok                     bool
	}{
		{"the whole top-up", 1_300_000, 0, 1_300_000, true},
		{"what's left after an earlier part", 1_300_000, 300_000, 1_000_000, true},
		{"one minor unit over what's left", 1_300_000, 300_000, 1_000_001, false},
		{"anything once it's all reversed", 1_300_000, 1_300_000, 1, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := domain.CheckReversalCap(tc.original, tc.live, tc.amount)
			if tc.ok != (err == nil) {
				t.Fatalf("err = %v, want ok=%v", err, tc.ok)
			}
			if err != nil && !errors.Is(err, domain.ErrReversalExceedsOriginal) {
				t.Fatalf("err = %v, want ErrReversalExceedsOriginal", err)
			}
		})
	}
}

func TestNeedsFinanceLead(t *testing.T) {
	threshold := int64(200_000) // KES 2,000
	tests := []struct {
		name      string
		amount    int64
		threshold *int64
		want      bool
	}{
		{"just below the threshold", 199_999, &threshold, false},
		{"at the threshold", 200_000, &threshold, true},
		{"above it", 1_300_000, &threshold, true},
		{"no threshold in the pack: always", 1, nil, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := domain.NeedsFinanceLead(tc.amount, tc.threshold); got != tc.want {
				t.Fatalf("NeedsFinanceLead(%d) = %v, want %v", tc.amount, got, tc.want)
			}
		})
	}
}

// The worked example reversed: 13,000.00 topped up, 8,000.00 of it in
// 10,000 SMS tokens at 0.80. The main wallet gives its 5,000.00 and the
// SMS wallet the rest, all 10,000 tokens.
func TestPlanReversal_WorkedExample(t *testing.T) {
	a := lot("10000", "10000", "80")
	plan, err := domain.PlanReversal(1_300_000, 500_000, []domain.ClawbackWallet{{ServiceCode: "sms", Lots: []domain.Lot{a}}})
	if err != nil {
		t.Fatal(err)
	}
	if plan.MainMinor != 500_000 || len(plan.Wallets) != 1 {
		t.Fatalf("plan = %+v, want 500,000 from main and one wallet", plan)
	}
	w := plan.Wallets[0]
	if w.ServiceCode != "sms" || w.ValueMinor != 800_000 || !w.Tokens.Equal(dec("10000")) {
		t.Fatalf("wallet = %+v, want 10,000 tokens worth 800,000", w)
	}
}

func TestPlanReversal(t *testing.T) {
	t.Run("the main wallet covers it: nothing clawed back", func(t *testing.T) {
		plan, err := domain.PlanReversal(100_000, 500_000, []domain.ClawbackWallet{{ServiceCode: "sms", Lots: []domain.Lot{lot("10", "10", "80")}}})
		if err != nil || plan.MainMinor != 100_000 || len(plan.Wallets) != 0 {
			t.Fatalf("plan = %+v, %v; want all from main", plan, err)
		}
	})

	t.Run("newest lot first, and the fewest tokens covering the shortfall", func(t *testing.T) {
		older, newer := lot("100", "100", "80"), lot("100", "100", "50")
		plan, err := domain.PlanReversal(1_000, 0, []domain.ClawbackWallet{{ServiceCode: "sms", Lots: []domain.Lot{older, newer}}})
		if err != nil {
			t.Fatal(err)
		}
		w := plan.Wallets[0]
		if len(w.Draws) != 1 || w.Draws[0].LotID != newer.ID || !w.Draws[0].Tokens.Equal(dec("19.99")) || w.ValueMinor != 1_000 {
			t.Fatalf("draws = %+v, want 19.99 tokens (999.5 rounds up to 1,000) from the newer lot", w.Draws)
		}
		if plan.MainMinor != 0 {
			t.Fatalf("main = %d, want 0", plan.MainMinor)
		}
	})

	t.Run("spills into the older lot, then the next wallet, in the order named", func(t *testing.T) {
		smsOld, smsNew := lot("10", "10", "100"), lot("10", "10", "100")
		wa := lot("100", "100", "100")
		plan, err := domain.PlanReversal(2_500, 0, []domain.ClawbackWallet{
			{ServiceCode: "sms", Lots: []domain.Lot{smsOld, smsNew}},
			{ServiceCode: "whatsapp", Lots: []domain.Lot{wa}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.Wallets) != 2 || plan.Wallets[0].ValueMinor != 2_000 || plan.Wallets[1].ValueMinor != 500 {
			t.Fatalf("plan = %+v, want 2,000 from sms and 500 from whatsapp", plan)
		}
		sms := plan.Wallets[0].Draws
		if len(sms) != 2 || sms[0].LotID != smsNew.ID || sms[1].LotID != smsOld.ID {
			t.Fatalf("sms draws = %+v, want the newer lot then the older", sms)
		}
	})

	t.Run("held tokens are never clawed back", func(t *testing.T) {
		// 100 tokens, 60 held: only 40 (4,000) can come back, from the
		// newest end; the held 60 stay for the oldest-first settlement.
		w := []domain.ClawbackWallet{{ServiceCode: "sms", Lots: []domain.Lot{lot("100", "100", "100")}, Held: dec("60")}}
		if _, err := domain.PlanReversal(4_001, 0, w); !errors.Is(err, domain.ErrReversalNotCovered) {
			t.Fatalf("err = %v, want ErrReversalNotCovered", err)
		}
		plan, err := domain.PlanReversal(4_000, 0, w)
		if err != nil || !plan.Wallets[0].Tokens.Equal(dec("40")) {
			t.Fatalf("plan = %+v, %v; want 40 tokens", plan, err)
		}
	})

	t.Run("held tokens use the oldest lots, so a newer lot is still clawable", func(t *testing.T) {
		older, newer := lot("50", "50", "100"), lot("50", "50", "200")
		plan, err := domain.PlanReversal(10_000, 0, []domain.ClawbackWallet{{ServiceCode: "sms", Lots: []domain.Lot{older, newer}, Held: dec("50")}})
		if err != nil {
			t.Fatal(err)
		}
		if d := plan.Wallets[0].Draws; len(d) != 1 || d[0].LotID != newer.ID || d[0].ValueMinor != 10_000 {
			t.Fatalf("draws = %+v, want all of the newer lot", d)
		}
	})

	t.Run("not covered by main plus every named wallet", func(t *testing.T) {
		_, err := domain.PlanReversal(10_000, 2_000, []domain.ClawbackWallet{{ServiceCode: "sms", Lots: []domain.Lot{lot("10", "10", "100")}}})
		if !errors.Is(err, domain.ErrReversalNotCovered) {
			t.Fatalf("err = %v, want ErrReversalNotCovered", err)
		}
	})

	t.Run("no wallet named: the main wallet alone must cover it", func(t *testing.T) {
		if _, err := domain.PlanReversal(10_000, 9_999, nil); !errors.Is(err, domain.ErrReversalNotCovered) {
			t.Fatalf("err = %v, want ErrReversalNotCovered", err)
		}
	})

	t.Run("rounding overshoot comes back off the main wallet's line", func(t *testing.T) {
		// A lot at 1.5 per token with 1 token used (worth 2 after
		// rounding): main plus clawed must still be exactly the amount,
		// and main never gives more than it has.
		l := lot("4", "3", "1.5")
		plan, err := domain.PlanReversal(3, 2, []domain.ClawbackWallet{{ServiceCode: "sms", Lots: []domain.Lot{l}}})
		if err != nil {
			t.Fatal(err)
		}
		total := plan.MainMinor
		for _, w := range plan.Wallets {
			total += w.ValueMinor
		}
		if total != 3 {
			t.Fatalf("main %d + clawed must equal the amount 3, got %d (%+v)", plan.MainMinor, total, plan)
		}
		if plan.MainMinor > 2 {
			t.Fatalf("main %d exceeds what it has (2)", plan.MainMinor)
		}
	})

	t.Run("main wallet can be credited back when one step overshoots the whole amount", func(t *testing.T) {
		// One micro-token is worth 2 minor units at 2,000,000 per token,
		// and the shortfall is 1: the clawback is worth 2 and the main
		// wallet's line is a credit of 1.
		plan, err := domain.PlanReversal(1, 0, []domain.ClawbackWallet{{ServiceCode: "sms", Lots: []domain.Lot{lot("1", "1", "2000000")}}})
		if err != nil {
			t.Fatal(err)
		}
		if plan.Wallets[0].ValueMinor != 2 || plan.MainMinor != -1 {
			t.Fatalf("plan = %+v, want 2 clawed and the main wallet credited 1", plan)
		}
	})
}

// A lot used partly by settlement (oldest first) and partly clawed back
// still adds up to exactly what it cost, whatever the split.
func TestPlanReversal_LotStillSumsToItsCost(t *testing.T) {
	for _, price := range []string{"0.8", "0.333333", "1.5", "7", "0.000001"} {
		for _, usedStr := range []string{"0", "1", "3.5", "999.999999"} {
			l := lot("1000", "1000", price)
			cost, err := domain.CostMinor(dec("1000"), dec(price))
			if err != nil {
				t.Fatal(err)
			}
			used := dec(usedStr)
			draws, usedValue, err := domain.DrawFIFO([]domain.Lot{l}, used)
			if err != nil && used.IsPositive() {
				t.Fatal(err)
			}
			_ = draws
			l.Remaining = l.Remaining.Sub(used)
			// Claw back everything left: ask for more than it's worth
			// from main 0, so it takes the whole lot, and check coverage.
			rest := cost - usedValue
			if rest == 0 {
				continue
			}
			plan, err := domain.PlanReversal(rest, 0, []domain.ClawbackWallet{{ServiceCode: "sms", Lots: []domain.Lot{l}}})
			if err != nil {
				t.Fatalf("price %s used %s: %v", price, usedStr, err)
			}
			if got := usedValue + plan.Wallets[0].ValueMinor; got != cost {
				t.Fatalf("price %s used %s: used %d + clawed %d = %d, want the lot's cost %d", price, usedStr, usedValue, plan.Wallets[0].ValueMinor, got, cost)
			}
			if !plan.Wallets[0].Tokens.Equal(l.Remaining) {
				t.Fatalf("price %s used %s: clawed %s tokens, want all %s left", price, usedStr, plan.Wallets[0].Tokens, l.Remaining)
			}
		}
	}
}

// The fewest tokens: one micro-token fewer would not cover the need.
func TestPlanReversal_FewestTokens(t *testing.T) {
	micro := decimal.New(1, -6)
	for _, price := range []string{"0.8", "0.333333", "1.5", "33"} {
		for _, need := range []int64{1, 2, 7, 100, 12_345} {
			l := lot("100000", "100000", price)
			plan, err := domain.PlanReversal(need, 0, []domain.ClawbackWallet{{ServiceCode: "sms", Lots: []domain.Lot{l}}})
			if err != nil {
				t.Fatal(err)
			}
			w := plan.Wallets[0]
			if w.ValueMinor < need {
				t.Fatalf("price %s need %d: clawed %d, under the need", price, need, w.ValueMinor)
			}
			less, err := domain.CostMinor(w.Tokens.Sub(micro), dec(price))
			if err != nil {
				t.Fatal(err)
			}
			if less >= need {
				t.Fatalf("price %s need %d: %s tokens taken, but one micro-token fewer is worth %d", price, need, w.Tokens, less)
			}
		}
	}
}

func TestReversalStateMachine(t *testing.T) {
	t.Run("only a requested reversal can be decided", func(t *testing.T) {
		for _, s := range []domain.ReversalStatus{domain.ReversalRequested, domain.ReversalRejected, domain.ReversalPosted, domain.ReversalPaybackPending, domain.ReversalPaidBack, domain.ReversalFailed} {
			err := domain.CanDecideReversal(s)
			if (s == domain.ReversalRequested) != (err == nil) {
				t.Fatalf("%s: err = %v", s, err)
			}
			if err != nil && !errors.Is(err, domain.ErrReversalDecided) {
				t.Fatalf("%s: err = %v, want ErrReversalDecided", s, err)
			}
		}
	})

	t.Run("a pay-back waits for the rail; the others are done once posted", func(t *testing.T) {
		if got := domain.StatusAfterPosting(domain.ReversalPayback); got != domain.ReversalPaybackPending {
			t.Fatalf("payback → %s", got)
		}
		for _, k := range []domain.ReversalKind{domain.ReversalChargeback, domain.ReversalOperator} {
			if got := domain.StatusAfterPosting(k); got != domain.ReversalPosted {
				t.Fatalf("%s → %s", k, got)
			}
		}
	})

	tests := []struct {
		name      string
		status    domain.ReversalStatus
		succeeded bool
		want      domain.PaybackChange
		err       error
	}{
		{"pending and paid back", domain.ReversalPaybackPending, true, domain.PaybackChange{Status: domain.ReversalPaidBack, Post: true}, nil},
		{"pending and failed: stays pending for a retry", domain.ReversalPaybackPending, false, domain.PaybackChange{Status: domain.ReversalPaybackPending, Attempt: true}, nil},
		{"paid back, told again: a replay", domain.ReversalPaidBack, true, domain.PaybackChange{Status: domain.ReversalPaidBack, Replay: true}, nil},
		{"paid back, then a failure", domain.ReversalPaidBack, false, domain.PaybackChange{}, domain.ErrNotAwaitingPayback},
		{"a chargeback has no pay-back", domain.ReversalPosted, true, domain.PaybackChange{}, domain.ErrNotAwaitingPayback},
		{"still requested", domain.ReversalRequested, true, domain.PaybackChange{}, domain.ErrNotAwaitingPayback},
		{"rejected", domain.ReversalRejected, false, domain.PaybackChange{}, domain.ErrNotAwaitingPayback},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := domain.RecordPayback(tc.status, tc.succeeded)
			if !errors.Is(err, tc.err) || (tc.err == nil && err != nil) {
				t.Fatalf("err = %v, want %v", err, tc.err)
			}
			if got != tc.want {
				t.Fatalf("change = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestClawableValue(t *testing.T) {
	w := domain.ClawbackWallet{ServiceCode: "sms", Lots: []domain.Lot{lot("100", "100", "80"), lot("100", "40", "50")}, Held: dec("30")}
	got, err := domain.ClawableValue(w)
	if err != nil {
		t.Fatal(err)
	}
	// Held 30 come off the oldest lot: 70 at 80 (5,600) and 40 at 50 (2,000).
	if got != 7_600 {
		t.Fatalf("clawable %d, want 7,600", got)
	}
}
