package domain

import (
	"errors"
	"strings"
)

// TenantFacts is what the reseller rules look at.
type TenantFacts struct {
	Type   string
	Tier   Tier
	Status string
}

var (
	ErrNotDirect              = errors.New("domain: only a direct account can become a reseller")
	ErrNotReseller            = errors.New("domain: only a reseller can create sub-accounts")
	ErrResellerNeedsFinancial = errors.New("domain: reseller accounts need the financial tier")
	ErrTenantNotActive        = errors.New("domain: the account is suspended or archived")
)

// CanBecomeReseller reports whether staff may make t a reseller
// (requirements: the Financial tier unlocks reseller accounts; agreed
// 2 Nov, OQ-04): only an active direct account on the financial tier,
// with a reason. A sub-account never becomes one, so there are only two
// levels.
func CanBecomeReseller(t TenantFacts, reason string) error {
	if strings.TrimSpace(reason) == "" {
		return ErrNoReason
	}
	if t.Type != TenantDirect {
		return ErrNotDirect
	}
	return activeOnFinancial(t)
}

// CanCreateSubAccount reports whether parent may create a sub-account:
// only an active reseller still on the financial tier. A reseller that
// drops below it, or is suspended or archived, keeps its existing
// sub-accounts but can't add new ones.
func CanCreateSubAccount(parent TenantFacts) error {
	if parent.Type != TenantReseller {
		return ErrNotReseller
	}
	return activeOnFinancial(parent)
}

func activeOnFinancial(t TenantFacts) error {
	if t.Status != TenantActive {
		return ErrTenantNotActive
	}
	if t.Tier != TierFinancial {
		return ErrResellerNeedsFinancial
	}
	return nil
}
