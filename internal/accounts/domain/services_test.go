package domain_test

import (
	"testing"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/accounts/domain"
)

func TestUnlocked(t *testing.T) {
	tiers := []domain.Tier{sandbox, starter, business, financial}
	for i, tier := range tiers {
		for j, minTier := range tiers {
			if got, want := domain.Unlocked(tier, minTier), i >= j; got != want {
				t.Errorf("Unlocked(%s, %s) = %v, want %v", tier, minTier, got, want)
			}
		}
	}
	if domain.Unlocked(financial, "platinum") {
		t.Error("an unknown minimum tier is never unlocked")
	}
}

func TestRequiredTier(t *testing.T) {
	cases := []struct {
		name string
		mins []domain.Tier
		want domain.Tier
	}{
		{"nothing selected needs nothing beyond the sandbox", nil, sandbox},
		{"one starter service", []domain.Tier{starter}, starter},
		{"the highest of several wins", []domain.Tier{starter, business, starter}, business},
		{"financial over everything", []domain.Tier{financial, starter, business}, financial},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := domain.RequiredTier(tc.mins); got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
}

func TestServiceStatusFor(t *testing.T) {
	tiers := []domain.Tier{sandbox, starter, business, financial}
	statuses := []string{domain.ServiceSelected, domain.ServiceEnabled, domain.ServiceSuspended}
	for _, tier := range tiers {
		for _, minTier := range tiers {
			for _, current := range statuses {
				got := domain.ServiceStatusFor(tier, minTier, current)
				want := domain.ServiceSelected
				switch {
				case current == domain.ServiceSuspended:
					want = domain.ServiceSuspended // only staff or billing lift a suspension
				case domain.Unlocked(tier, minTier):
					want = domain.ServiceEnabled
				}
				if got != want {
					t.Errorf("ServiceStatusFor(%s, %s, %s) = %s, want %s", tier, minTier, current, got, want)
				}
			}
		}
	}
}
