package domain_test

import (
	"errors"
	"testing"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/accounts/domain"
)

var (
	tenantTypes    = []string{domain.TenantDirect, domain.TenantReseller, domain.TenantSubAccount}
	allTiers       = []domain.Tier{sandbox, starter, business, financial}
	tenantStatuses = []string{domain.TenantActive, domain.TenantSuspended, domain.TenantArchived}
)

func TestCanBecomeReseller(t *testing.T) {
	cases := []struct {
		name   string
		facts  domain.TenantFacts
		reason string
		want   error
	}{
		{"an active direct account on financial can", domain.TenantFacts{Type: domain.TenantDirect, Tier: financial, Status: domain.TenantActive}, "contract signed", nil},
		{"a direct account below financial cannot", domain.TenantFacts{Type: domain.TenantDirect, Tier: business, Status: domain.TenantActive}, "contract signed", domain.ErrResellerNeedsFinancial},
		{"a sandbox account cannot", domain.TenantFacts{Type: domain.TenantDirect, Tier: sandbox, Status: domain.TenantActive}, "contract signed", domain.ErrResellerNeedsFinancial},
		{"a suspended account cannot", domain.TenantFacts{Type: domain.TenantDirect, Tier: financial, Status: domain.TenantSuspended}, "contract signed", domain.ErrTenantNotActive},
		{"an archived account cannot", domain.TenantFacts{Type: domain.TenantDirect, Tier: financial, Status: domain.TenantArchived}, "contract signed", domain.ErrTenantNotActive},
		{"a reseller is already one", domain.TenantFacts{Type: domain.TenantReseller, Tier: financial, Status: domain.TenantActive}, "contract signed", domain.ErrNotDirect},
		{"a sub-account never becomes one (two levels only)", domain.TenantFacts{Type: domain.TenantSubAccount, Tier: financial, Status: domain.TenantActive}, "contract signed", domain.ErrNotDirect},
		{"a change needs a reason", domain.TenantFacts{Type: domain.TenantDirect, Tier: financial, Status: domain.TenantActive}, " ", domain.ErrNoReason},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := domain.CanBecomeReseller(tc.facts, tc.reason)
			if !errors.Is(err, tc.want) || (tc.want == nil && err != nil) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

func TestCanCreateSubAccount(t *testing.T) {
	cases := []struct {
		name   string
		parent domain.TenantFacts
		want   error
	}{
		{"an active reseller on financial can", domain.TenantFacts{Type: domain.TenantReseller, Tier: financial, Status: domain.TenantActive}, nil},
		{"a direct account cannot, even on financial", domain.TenantFacts{Type: domain.TenantDirect, Tier: financial, Status: domain.TenantActive}, domain.ErrNotReseller},
		{"a sub-account cannot (two levels only)", domain.TenantFacts{Type: domain.TenantSubAccount, Tier: financial, Status: domain.TenantActive}, domain.ErrNotReseller},
		{"a reseller demoted below financial cannot", domain.TenantFacts{Type: domain.TenantReseller, Tier: business, Status: domain.TenantActive}, domain.ErrResellerNeedsFinancial},
		{"a suspended reseller cannot", domain.TenantFacts{Type: domain.TenantReseller, Tier: financial, Status: domain.TenantSuspended}, domain.ErrTenantNotActive},
		{"an archived reseller cannot", domain.TenantFacts{Type: domain.TenantReseller, Tier: financial, Status: domain.TenantArchived}, domain.ErrTenantNotActive},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := domain.CanCreateSubAccount(tc.parent)
			if !errors.Is(err, tc.want) || (tc.want == nil && err != nil) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

// TestResellerRules_EveryCombination: across every type × tier ×
// status, exactly one combination may become a reseller and exactly
// one may create sub-accounts.
func TestResellerRules_EveryCombination(t *testing.T) {
	for _, typ := range tenantTypes {
		for _, tier := range allTiers {
			for _, status := range tenantStatuses {
				facts := domain.TenantFacts{Type: typ, Tier: tier, Status: status}
				qualifies := tier == financial && status == domain.TenantActive

				err := domain.CanBecomeReseller(facts, "reason")
				if want := qualifies && typ == domain.TenantDirect; (err == nil) != want {
					t.Errorf("CanBecomeReseller(%+v) = %v, want allowed=%v", facts, err, want)
				}
				err = domain.CanCreateSubAccount(facts)
				if want := qualifies && typ == domain.TenantReseller; (err == nil) != want {
					t.Errorf("CanCreateSubAccount(%+v) = %v, want allowed=%v", facts, err, want)
				}
			}
		}
	}
}
