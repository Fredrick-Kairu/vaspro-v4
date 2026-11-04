package domain_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/accounts/domain"
)

var (
	staff      = domain.Evidence{ByStaff: true}
	individual = domain.Evidence{KYCCheckKind: domain.KYCKindIndividual}
	kyb        = domain.Evidence{KYCCheckKind: domain.KYCKindBusiness}
	screening  = domain.Evidence{KYCCheckKind: domain.KYCKindScreening}
	none       = domain.Evidence{}
)

const (
	sandbox   = domain.TierSandbox
	starter   = domain.TierStarter
	business  = domain.TierBusiness
	financial = domain.TierFinancial
)

func TestValidateTierChange(t *testing.T) {
	cases := []struct {
		name     string
		from, to domain.Tier
		status   string
		reason   string
		evidence domain.Evidence
		want     error
	}{
		// Promotions backed by a passed check: one step, matching kind.
		{"individual KYC promotes sandbox to starter", sandbox, starter, domain.TenantActive, "kyc passed", individual, nil},
		{"KYB promotes starter to business", starter, business, domain.TenantActive, "kyb passed", kyb, nil},
		{"KYB cannot skip from sandbox to business", sandbox, business, domain.TenantActive, "kyb passed", kyb, domain.ErrSkipsTiers},
		{"individual KYC cannot promote to business", starter, business, domain.TenantActive, "kyc passed", individual, domain.ErrCheckKindMismatch},
		{"KYB cannot promote to starter", sandbox, starter, domain.TenantActive, "kyb passed", kyb, domain.ErrCheckKindMismatch},
		{"screening promotes nothing below financial", starter, business, domain.TenantActive, "screened", screening, domain.ErrCheckKindMismatch},
		{"a check never promotes to financial", business, financial, domain.TenantActive, "screened", screening, domain.ErrFinancialNeedsStaff},

		// Promotions by staff: any number of steps.
		{"staff promote one step", sandbox, starter, domain.TenantActive, "reviewed", staff, nil},
		{"staff promote sandbox straight to financial", sandbox, financial, domain.TenantActive, "existing client, documents on file", staff, nil},
		{"staff promote business to financial", business, financial, domain.TenantActive, "contract signed", staff, nil},
		{"staff deciding a reviewed check may skip", sandbox, business, domain.TenantActive, "review approved", domain.Evidence{KYCCheckKind: domain.KYCKindBusiness, ByStaff: true}, nil},

		// Demotions: any number of steps, staff or a check.
		{"staff demote financial to sandbox", financial, sandbox, domain.TenantActive, "fraud", staff, nil},
		{"re-verification demotes business to starter", business, starter, domain.TenantActive, "director changed", kyb, nil},
		{"a demotion needs evidence too", business, starter, domain.TenantActive, "no reason given", none, domain.ErrNoEvidence},

		// Status.
		{"a suspended tenant cannot be promoted", sandbox, starter, domain.TenantSuspended, "kyc passed", individual, domain.ErrPromotionWhileInactive},
		{"staff cannot promote an archived tenant", sandbox, financial, domain.TenantArchived, "reviewed", staff, domain.ErrPromotionWhileInactive},
		{"a suspended tenant can be demoted", business, sandbox, domain.TenantSuspended, "fraud", staff, nil},

		// Always.
		{"a promotion needs evidence", sandbox, starter, domain.TenantActive, "because", none, domain.ErrNoEvidence},
		{"the same tier is not a change", starter, starter, domain.TenantActive, "again", staff, domain.ErrSameTier},
		{"a change needs a reason", sandbox, starter, domain.TenantActive, "  ", staff, domain.ErrNoReason},
		{"an unknown tier is rejected", sandbox, "platinum", domain.TenantActive, "x", staff, domain.ErrUnknownTier},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := domain.ValidateTierChange(domain.TierChange{
				From: tc.from, To: tc.to, TenantStatus: tc.status, Reason: tc.reason, Evidence: tc.evidence,
			})
			if !errors.Is(err, tc.want) || (tc.want == nil && err != nil) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

// TestValidateTierChange_Invariants runs every from × to × evidence ×
// status combination and checks the rules that must hold whatever the
// combination, so no transition is left untested.
func TestValidateTierChange_Invariants(t *testing.T) {
	tiers := []domain.Tier{sandbox, starter, business, financial}
	evidences := map[string]domain.Evidence{"none": none, "individual": individual, "kyb": kyb, "screening": screening, "staff": staff}
	statuses := []string{domain.TenantActive, domain.TenantSuspended, domain.TenantArchived}
	rank := map[domain.Tier]int{sandbox: 0, starter: 1, business: 2, financial: 3}

	for _, from := range tiers {
		for _, to := range tiers {
			for evName, ev := range evidences {
				for _, status := range statuses {
					name := fmt.Sprintf("%s→%s by %s while %s", from, to, evName, status)
					err := domain.ValidateTierChange(domain.TierChange{From: from, To: to, TenantStatus: status, Reason: "r", Evidence: ev})
					up := rank[to] - rank[from]
					switch {
					case up == 0 && err == nil:
						t.Errorf("%s: a same-tier change was allowed", name)
					case evName == "none" && err == nil:
						t.Errorf("%s: a change with no evidence was allowed", name)
					case up > 0 && status != domain.TenantActive && err == nil:
						t.Errorf("%s: an inactive tenant was promoted", name)
					case up > 0 && to == financial && !ev.ByStaff && err == nil:
						t.Errorf("%s: financial was reached without staff", name)
					case up > 1 && !ev.ByStaff && err == nil:
						t.Errorf("%s: a check skipped tiers", name)
					case up < 0 && evName != "none" && err != nil:
						t.Errorf("%s: a backed demotion was refused: %v", name, err)
					case up > 0 && ev.ByStaff && status == domain.TenantActive && err != nil:
						t.Errorf("%s: an active staff promotion was refused: %v", name, err)
					}
				}
			}
		}
	}
}
