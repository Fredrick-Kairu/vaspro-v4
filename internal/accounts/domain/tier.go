// Package domain holds accounts' business rules. Pure Go, no I/O.
package domain

import (
	"errors"
	"strings"
)

// Tier is an account's verification tier (data model §4,
// accounts.tier), lowest first.
type Tier string

const (
	TierSandbox   Tier = "sandbox"
	TierStarter   Tier = "starter"
	TierBusiness  Tier = "business"
	TierFinancial Tier = "financial"
)

// Kinds of KYC check (accounts.kyc_checks.kind).
const (
	KYCKindIndividual = "individual"
	KYCKindBusiness   = "business"
	KYCKindScreening  = "screening"
)

// Tenant statuses (accounts.tenants.status).
const (
	TenantActive    = "active"
	TenantSuspended = "suspended"
	TenantArchived  = "archived"
)

// Evidence is what backs a tier change: a passed KYC check of some
// kind, a staff decision, or both (staff deciding a check sent to
// review).
type Evidence struct {
	KYCCheckKind string // empty when no check backs the change
	ByStaff      bool
}

// TierChange is a requested change of a tenant's tier.
type TierChange struct {
	From         Tier
	To           Tier
	TenantStatus string
	Reason       string
	Evidence     Evidence
}

var (
	ErrUnknownTier            = errors.New("domain: unknown tier")
	ErrNoReason               = errors.New("domain: a tier change needs a reason")
	ErrSameTier               = errors.New("domain: the tenant is already on that tier")
	ErrNoEvidence             = errors.New("domain: a tier change needs a KYC check or a staff decision behind it")
	ErrPromotionWhileInactive = errors.New("domain: a suspended or archived tenant can only be demoted")
	ErrFinancialNeedsStaff    = errors.New("domain: only staff can promote to the financial tier")
	ErrSkipsTiers             = errors.New("domain: a KYC check promotes one tier at a time")
	ErrCheckKindMismatch      = errors.New("domain: this kind of KYC check doesn't unlock that tier")
)

// Rank orders tiers, sandbox lowest. ok is false for an unknown tier.
func (t Tier) Rank() (rank int, ok bool) {
	switch t {
	case TierSandbox:
		return 0, true
	case TierStarter:
		return 1, true
	case TierBusiness:
		return 2, true
	case TierFinancial:
		return 3, true
	}
	return 0, false
}

// unlockedBy is the one tier each kind of check promotes to, one step
// at a time. Screening backs financial, which only staff can grant, so
// it unlocks nothing on its own.
var unlockedBy = map[string]Tier{
	KYCKindIndividual: TierStarter,
	KYCKindBusiness:   TierBusiness,
}

// ValidateTierChange reports whether c is allowed (agreed 30 Oct):
//   - every change needs a reason and evidence (a KYC check or staff);
//   - a check promotes exactly one tier, to the tier its kind unlocks;
//   - staff promote any number of tiers, and only staff reach financial;
//   - demotions go any number of tiers down;
//   - a suspended or archived tenant can only be demoted.
func ValidateTierChange(c TierChange) error {
	from, okFrom := c.From.Rank()
	to, okTo := c.To.Rank()
	if !okFrom || !okTo {
		return ErrUnknownTier
	}
	if strings.TrimSpace(c.Reason) == "" {
		return ErrNoReason
	}
	if from == to {
		return ErrSameTier
	}
	if c.Evidence.KYCCheckKind == "" && !c.Evidence.ByStaff {
		return ErrNoEvidence
	}
	if to < from {
		return nil
	}

	if c.TenantStatus != TenantActive {
		return ErrPromotionWhileInactive
	}
	if c.Evidence.ByStaff {
		return nil
	}
	if c.To == TierFinancial {
		return ErrFinancialNeedsStaff
	}
	if to-from != 1 {
		return ErrSkipsTiers
	}
	if unlockedBy[c.Evidence.KYCCheckKind] != c.To {
		return ErrCheckKindMismatch
	}
	return nil
}
