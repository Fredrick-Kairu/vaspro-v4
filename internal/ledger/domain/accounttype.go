// Package domain holds the ledger's rules: pure Go, no I/O.
package domain

import "fmt"

// AccountType is a ledger account's type (data model §5,
// ledger.account_type).
type AccountType string

const (
	MainWallet          AccountType = "main_wallet"
	ServiceWallet       AccountType = "service_wallet"
	Bonus               AccountType = "bonus"
	Receivable          AccountType = "receivable"
	RevenueSharePayable AccountType = "revenue_share_payable"
	CommissionPayable   AccountType = "commission_payable"
	ProviderClearing    AccountType = "provider_clearing"
	Suspense            AccountType = "suspense"
	ReversalClearing    AccountType = "reversal_clearing"
	Revenue             AccountType = "revenue"
	TaxPayable          AccountType = "tax_payable"
)

// AccountTypes lists every type in the enum's order. The store tests
// check it against ledger.account_type and ledger.normal_side.
var AccountTypes = []AccountType{
	MainWallet, ServiceWallet, Bonus, Receivable, RevenueSharePayable, CommissionPayable,
	ProviderClearing, Suspense, ReversalClearing, Revenue, TaxPayable,
}

// Side is a journal line's direction, as stored in entries.direction.
type Side byte

const (
	Debit  Side = 'D'
	Credit Side = 'C'
)

type accountTypeRule struct {
	normal       Side
	clientOwned  bool
	clientWallet bool
}

// accountTypeRules is the one place the sign convention lives (OQ-13):
// balances are kept on each account's normal side, so every balance
// reads positive in normal use. Receivable and provider clearing are
// what others owe VASPRO (debit-normal); everything else is what VASPRO
// owes or has earned (credit-normal). Only main, service and bonus
// wallets are spendable and must never go below zero.
var accountTypeRules = map[AccountType]accountTypeRule{
	MainWallet:          {Credit, true, true},
	ServiceWallet:       {Credit, true, true},
	Bonus:               {Credit, true, true},
	Receivable:          {Debit, true, false},
	RevenueSharePayable: {Credit, true, false},
	CommissionPayable:   {Credit, true, false},
	ProviderClearing:    {Debit, false, false},
	Suspense:            {Credit, false, false},
	ReversalClearing:    {Credit, false, false},
	Revenue:             {Credit, false, false},
	TaxPayable:          {Credit, false, false},
}

// NormalSide is the side on which a line raises the account's balance.
func NormalSide(t AccountType) (Side, error) {
	r, ok := accountTypeRules[t]
	if !ok {
		return 0, fmt.Errorf("ledger: unknown account type %q", t)
	}
	return r.normal, nil
}

// IsClientOwned reports whether accounts of this type belong to a
// client (tenant_id set) rather than to VASPRO (tenant_id NULL).
func IsClientOwned(t AccountType) bool { return accountTypeRules[t].clientOwned }

// IsClientWallet reports whether the type is a spendable client wallet,
// which the database keeps at or above zero available balance.
func IsClientWallet(t AccountType) bool { return accountTypeRules[t].clientWallet }

// BalanceDelta is how a line of amountMinor on side dir moves the
// balance of an account of type t.
func BalanceDelta(t AccountType, dir Side, amountMinor int64) (int64, error) {
	normal, err := NormalSide(t)
	if err != nil {
		return 0, err
	}
	switch dir {
	case normal:
		return amountMinor, nil
	case Debit, Credit:
		return -amountMinor, nil
	default:
		return 0, fmt.Errorf("ledger: direction %q is neither D nor C", dir)
	}
}
