package domain

import (
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/shopspring/decimal"
)

var (
	// ErrInvalidReversal covers a reversal request that can't be raised
	// as given.
	ErrInvalidReversal = errors.New("ledger: invalid reversal")
	// ErrReversalExceedsOriginal: this reversal and the live ones of the
	// same top-up would be more than the top-up.
	ErrReversalExceedsOriginal = errors.New("ledger: reversal exceeds what's left of the original")
	// ErrReversalNotCovered: the main wallet plus the named clawback
	// can't cover the reversal (OQ-18: no debt is recorded).
	ErrReversalNotCovered = errors.New("ledger: reversal not covered by the main wallet and the named clawback")
	// ErrReversalDecided: only a requested reversal can be approved or
	// rejected.
	ErrReversalDecided = errors.New("ledger: reversal already decided")
	// ErrNotAwaitingPayback: only a pay-back reversal that's posted and
	// not yet paid back can record a pay-back.
	ErrNotAwaitingPayback = errors.New("ledger: reversal isn't awaiting pay-back")
)

// ReversalKind is how the money goes back (OQ-18).
type ReversalKind string

const (
	// ReversalPayback: VASPRO pays the money back through the rail; it
	// waits in reversal_clearing until the pay-back succeeds.
	ReversalPayback ReversalKind = "payback"
	// ReversalChargeback and ReversalOperator: the rail already moved the
	// money, so only the ledger entries are posted (system design §4.3).
	ReversalChargeback ReversalKind = "chargeback"
	ReversalOperator   ReversalKind = "operator"
)

// ReversalStatus is ledger.reversals.status.
type ReversalStatus string

const (
	ReversalRequested      ReversalStatus = "requested"
	ReversalApproved       ReversalStatus = "approved"
	ReversalRejected       ReversalStatus = "rejected"
	ReversalPosted         ReversalStatus = "posted"
	ReversalPaybackPending ReversalStatus = "payback_pending"
	ReversalPaidBack       ReversalStatus = "paid_back"
	ReversalFailed         ReversalStatus = "failed"
)

// NewReversal is what a staff member raises.
type NewReversal struct {
	Kind             ReversalKind
	Currency         string
	AmountMinor      int64
	Reason           string
	ClawbackServices []string // service wallets to claw unused tokens back from, in order
}

// ValidateNewReversal checks a reversal before it's raised.
func ValidateNewReversal(r NewReversal) error {
	switch r.Kind {
	case ReversalPayback, ReversalChargeback, ReversalOperator:
	default:
		return fmt.Errorf("%w: kind must be payback, chargeback or operator", ErrInvalidReversal)
	}
	if r.AmountMinor <= 0 {
		return fmt.Errorf("%w: amount_minor must be positive", ErrInvalidReversal)
	}
	if strings.TrimSpace(r.Reason) == "" {
		return fmt.Errorf("%w: a reason is required", ErrInvalidReversal)
	}
	if err := ValidateAccountKey(AccountKey{Type: MainWallet, Currency: r.Currency}); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidReversal, err)
	}
	seen := map[string]bool{}
	for _, code := range r.ClawbackServices {
		if seen[code] {
			return fmt.Errorf("%w: service %s is named twice", ErrInvalidReversal, code)
		}
		seen[code] = true
		if err := ValidateAccountKey(AccountKey{Type: ServiceWallet, ServiceCode: code, Currency: r.Currency}); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidReversal, err)
		}
	}
	return nil
}

// CheckReversalCap refuses a reversal that, with the live (not rejected)
// reversals of the same top-up, would reverse more than it paid in.
func CheckReversalCap(original, live, amount int64) error {
	if amount > original-live {
		return fmt.Errorf("%w: %d of %d left, %d asked", ErrReversalExceedsOriginal, original-live, original, amount)
	}
	return nil
}

// NeedsFinanceLead: a reversal at or above the pack's threshold needs a
// finance lead's approval; with no threshold, every reversal does
// (system design §4.3, OQ-18).
func NeedsFinanceLead(amount int64, threshold *int64) bool {
	return threshold == nil || amount >= *threshold
}

// CanDecideReversal: only a requested reversal is approved or rejected.
func CanDecideReversal(s ReversalStatus) error {
	if s != ReversalRequested {
		return fmt.Errorf("%w: it is %s", ErrReversalDecided, s)
	}
	return nil
}

// StatusAfterPosting is where an approved reversal lands once posted.
func StatusAfterPosting(k ReversalKind) ReversalStatus {
	if k == ReversalPayback {
		return ReversalPaybackPending
	}
	return ReversalPosted
}

// PaybackChange is what a pay-back report does to a reversal.
type PaybackChange struct {
	Status  ReversalStatus
	Post    bool // post the pay-back transaction
	Attempt bool // record a failed attempt; the money stays in clearing
	Replay  bool // already paid back; nothing changes
}

// RecordPayback applies the rail's report on a pay-back (OQ-18): a
// failure leaves the reversal pending for a retry, so the money is
// never lost or paid twice.
func RecordPayback(s ReversalStatus, succeeded bool) (PaybackChange, error) {
	switch {
	case s == ReversalPaybackPending && succeeded:
		return PaybackChange{Status: ReversalPaidBack, Post: true}, nil
	case s == ReversalPaybackPending:
		return PaybackChange{Status: ReversalPaybackPending, Attempt: true}, nil
	case s == ReversalPaidBack && succeeded:
		return PaybackChange{Status: ReversalPaidBack, Replay: true}, nil
	default:
		return PaybackChange{}, fmt.Errorf("%w: it is %s", ErrNotAwaitingPayback, s)
	}
}

// ClawbackWallet is a service wallet the maker named: its open lots in
// purchase order, and the tokens holds reserve in it.
type ClawbackWallet struct {
	ServiceCode string
	Lots        []Lot
	Held        decimal.Decimal
}

// WalletClawback is what a reversal takes back from one wallet.
type WalletClawback struct {
	ServiceCode string
	Draws       []Draw
	Tokens      decimal.Decimal
	ValueMinor  int64
}

// ReversalPlan is a reversal's lines: MainMinor from the main wallet
// (negative: a credit back to it, when a clawback step overshoots), and
// the clawback per wallet. MainMinor plus the clawed values is always
// the amount.
type ReversalPlan struct {
	MainMinor int64
	Wallets   []WalletClawback
}

// PlanReversal works out where a reversal's amount comes from (system
// design §4.3, OQ-18): the main wallet up to its available balance,
// then unused tokens from the named wallets in order, newest lot first.
// Held tokens are never clawed back; settlement draws oldest first, so
// they are reserved from the oldest lots. Each draw is valued by the
// lot's cumulative rounding (DrawFIFO), and takes the fewest tokens
// covering what's still needed, or the whole lot when it's worth no
// more. ErrReversalNotCovered when it all falls short.
func PlanReversal(amount, mainAvailable int64, wallets []ClawbackWallet) (ReversalPlan, error) {
	need := amount - max(mainAvailable, 0)
	var plan ReversalPlan
	var clawed int64
	for _, w := range wallets {
		if need <= 0 {
			break
		}
		wc, err := clawBackWallet(w, need)
		if err != nil {
			return ReversalPlan{}, err
		}
		if len(wc.Draws) == 0 {
			continue
		}
		plan.Wallets = append(plan.Wallets, wc)
		need -= wc.ValueMinor
		clawed += wc.ValueMinor
	}
	if need > 0 {
		return ReversalPlan{}, fmt.Errorf("%w: %d minor units short", ErrReversalNotCovered, need)
	}
	plan.MainMinor = amount - clawed
	return plan, nil
}

func clawBackWallet(w ClawbackWallet, need int64) (WalletClawback, error) {
	out := WalletClawback{ServiceCode: w.ServiceCode, Tokens: decimal.Zero}
	clawable := clawableByLot(w.Lots, w.Held)
	for i := len(w.Lots) - 1; i >= 0 && need > 0; i-- {
		l, avail := w.Lots[i], clawable[i]
		if !avail.IsPositive() {
			continue
		}
		before := l.Bought.Sub(l.Remaining)
		take, value, err := fewestCovering(before, avail, l.Price, need)
		if err != nil {
			return WalletClawback{}, err
		}
		if !take.IsPositive() || value == 0 {
			continue // tokens worth under a minor unit stay: a line can't be zero
		}
		out.Draws = append(out.Draws, Draw{LotID: l.ID, Tokens: take, ValueMinor: value})
		out.Tokens = out.Tokens.Add(take)
		out.ValueMinor += value
		need -= value
	}
	return out, nil
}

// ClawableValue is what a wallet could give back: its unheld tokens'
// value, for a reversal's preview.
func ClawableValue(w ClawbackWallet) (int64, error) {
	wc, err := clawBackWallet(w, math.MaxInt64)
	return wc.ValueMinor, err
}

// clawableByLot is each lot's remaining tokens less the held tokens,
// which are reserved from the oldest lots first.
func clawableByLot(lots []Lot, held decimal.Decimal) []decimal.Decimal {
	out := make([]decimal.Decimal, len(lots))
	for i, l := range lots {
		reserved := decimal.Min(l.Remaining, decimal.Max(held, decimal.Zero))
		held = held.Sub(reserved)
		out[i] = l.Remaining.Sub(reserved)
	}
	return out
}

// fewestCovering takes from a lot, after before tokens already drawn,
// the fewest tokens (to TokenScale places, at most avail) whose
// cumulative-rounding value covers need; all of avail when it's worth
// no more than need.
func fewestCovering(before, avail, price decimal.Decimal, need int64) (decimal.Decimal, int64, error) {
	valueOf := func(take decimal.Decimal) (int64, error) {
		lo, err := CostMinor(before, price)
		if err != nil {
			return 0, err
		}
		hi, err := CostMinor(before.Add(take), price)
		return hi - lo, err
	}
	whole, err := valueOf(avail)
	if err != nil || whole <= need {
		return avail, whole, err
	}
	// Binary search over micro-tokens: value is non-decreasing in take.
	lo, hi := int64(0), avail.Shift(TokenScale).IntPart()
	for lo < hi {
		mid := lo + (hi-lo)/2
		v, err := valueOf(decimal.New(mid, -TokenScale))
		if err != nil {
			return decimal.Zero, 0, err
		}
		if v >= need {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	take := decimal.New(lo, -TokenScale)
	v, err := valueOf(take)
	return take, v, err
}
