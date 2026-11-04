package domain

import (
	"errors"
	"fmt"
	"regexp"
)

// TxnType is a transaction's type (data model §5, ledger.txn_type).
type TxnType string

const (
	TxnTopup          TxnType = "topup"
	TxnTokenPurchase  TxnType = "token_purchase"
	TxnUsage          TxnType = "usage"
	TxnRefund         TxnType = "refund"
	TxnBonusGrant     TxnType = "bonus_grant"
	TxnInvoicePayment TxnType = "invoice_payment"
	TxnReversal       TxnType = "reversal"
	TxnPayback        TxnType = "payback"
	TxnAdjustment     TxnType = "adjustment"
	TxnRevenueShare   TxnType = "revenue_share"
	TxnMigration      TxnType = "migration"
)

var txnTypes = map[TxnType]bool{
	TxnTopup: true, TxnTokenPurchase: true, TxnUsage: true, TxnRefund: true, TxnBonusGrant: true,
	TxnInvoicePayment: true, TxnReversal: true, TxnPayback: true, TxnAdjustment: true,
	TxnRevenueShare: true, TxnMigration: true,
}

// ErrInvalidPosting covers a malformed posting or account key.
var ErrInvalidPosting = errors.New("ledger: invalid posting")

// ErrAllocationFinal: no posting moves money from a service or bonus
// wallet back to a main wallet, except a reversal (data model §5,
// "Allocation is final").
var ErrAllocationFinal = errors.New("ledger: allocation is final")

// AccountKey names a ledger account by data model §5's unique key,
// without its owner: client types belong to the posting's tenant,
// VASPRO's types to VASPRO.
type AccountKey struct {
	Type        AccountType
	ServiceCode string
	Rail        string
	Currency    string
}

// Line is one line of a posting.
type Line struct {
	Account     AccountKey
	Direction   Side
	AmountMinor int64
}

// Posting is a transaction as a caller asks for it. System postings
// (no tenant) may use VASPRO's accounts only.
type Posting struct {
	Type   TxnType
	System bool
	Lines  []Line
}

var (
	codePattern     = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	currencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)
)

// ValidateAccountKey applies the account CHECKs (3.1) before the
// database sees the key: a known type, a service where the type needs
// one, a rail on provider clearing only, and well-formed codes.
func ValidateAccountKey(k AccountKey) error {
	if _, err := NormalSide(k.Type); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidPosting, err)
	}
	if !currencyPattern.MatchString(k.Currency) {
		return fmt.Errorf("%w: currency %q is not an ISO 4217 code", ErrInvalidPosting, k.Currency)
	}
	if k.ServiceCode != "" && !codePattern.MatchString(k.ServiceCode) {
		return fmt.Errorf("%w: service code %q", ErrInvalidPosting, k.ServiceCode)
	}
	if (k.Type == ServiceWallet || k.Type == Revenue) && k.ServiceCode == "" {
		return fmt.Errorf("%w: a %s account needs a service code", ErrInvalidPosting, k.Type)
	}
	if (k.Type == ProviderClearing) != (k.Rail != "") {
		return fmt.Errorf("%w: only provider clearing names a rail, and it must", ErrInvalidPosting)
	}
	if k.Rail != "" && !codePattern.MatchString(k.Rail) {
		return fmt.Errorf("%w: rail %q", ErrInvalidPosting, k.Rail)
	}
	return nil
}

// ValidatePosting checks a posting's shape, that it balances per
// currency, and the design's rules (OQ-14). The database's deferred
// check (3.2) is the backstop for the balance.
func ValidatePosting(p Posting) error {
	if !txnTypes[p.Type] {
		return fmt.Errorf("%w: unknown transaction type %q", ErrInvalidPosting, p.Type)
	}
	if len(p.Lines) < 2 {
		return fmt.Errorf("%w: a posting needs at least two lines", ErrInvalidPosting)
	}
	for i, l := range p.Lines {
		if err := ValidateAccountKey(l.Account); err != nil {
			return fmt.Errorf("line %d: %w", i, err)
		}
		if l.Direction != Debit && l.Direction != Credit {
			return fmt.Errorf("%w: line %d: direction %q is neither D nor C", ErrInvalidPosting, i, l.Direction)
		}
		if l.AmountMinor <= 0 {
			return fmt.Errorf("%w: line %d: amount must be positive", ErrInvalidPosting, i)
		}
		if p.System && IsClientOwned(l.Account.Type) {
			return fmt.Errorf("%w: line %d: a system posting can't use a client's %s", ErrInvalidPosting, i, l.Account.Type)
		}
	}

	perCurrency := map[string]int64{}
	for _, l := range p.Lines {
		if l.Direction == Debit {
			perCurrency[l.Account.Currency] += l.AmountMinor
		} else {
			perCurrency[l.Account.Currency] -= l.AmountMinor
		}
	}
	for cur, diff := range perCurrency {
		if diff != 0 {
			return fmt.Errorf("%w: debits and credits in %s differ by %d", ErrInvalidPosting, cur, diff)
		}
	}

	if p.Type != TxnReversal {
		deltas, err := NetDeltas(p.Lines)
		if err != nil {
			return err
		}
		var mainUp, allocatedDown bool
		for k, delta := range deltas {
			switch {
			case k.Type == MainWallet && delta > 0:
				mainUp = true
			case (k.Type == ServiceWallet || k.Type == Bonus) && delta < 0:
				allocatedDown = true
			}
		}
		if mainUp && allocatedDown {
			return ErrAllocationFinal
		}
	}
	return nil
}

// NetDeltas is how a posting moves each account's balance, on its
// normal side (3.1's BalanceDelta), summed per account.
func NetDeltas(lines []Line) (map[AccountKey]int64, error) {
	deltas := make(map[AccountKey]int64, len(lines))
	for _, l := range lines {
		delta, err := BalanceDelta(l.Account.Type, l.Direction, l.AmountMinor)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidPosting, err)
		}
		deltas[l.Account] += delta
	}
	return deltas, nil
}
