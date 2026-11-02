package domain

// Statuses of an account's service (accounts.tenant_services.status).
const (
	ServiceSelected  = "selected"
	ServiceEnabled   = "enabled"
	ServiceSuspended = "suspended"
)

// Unlocked reports whether an account on tier may use, live, a service
// whose minimum tier in its country is minTier. An unknown tier unlocks
// nothing.
func Unlocked(tier, minTier Tier) bool {
	have, okHave := tier.Rank()
	need, okNeed := minTier.Rank()
	return okHave && okNeed && have >= need
}

// RequiredTier is the lowest tier that unlocks every service with these
// minimum tiers: verification "stops at the lowest tier that unlocks
// everything the client picked" (system design §2). Nothing selected
// needs nothing beyond the sandbox.
func RequiredTier(minTiers []Tier) Tier {
	required, rank := TierSandbox, 0
	for _, t := range minTiers {
		if r, ok := t.Rank(); ok && r > rank {
			required, rank = t, r
		}
	}
	return required
}

// ServiceStatusFor is what a selected service's status should be for an
// account on tier (agreed 2 Nov, OQ-06): enabled once the tier reaches
// the service's minimum, back to selected (locked) below it. A
// suspended service stays suspended: only staff or billing lift that.
func ServiceStatusFor(tier, minTier Tier, current string) string {
	switch {
	case current == ServiceSuspended:
		return ServiceSuspended
	case Unlocked(tier, minTier):
		return ServiceEnabled
	default:
		return ServiceSelected
	}
}
