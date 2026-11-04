package domain_test

import (
	"errors"
	"testing"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/accounts/domain"
)

func TestNormalizeBusinessProfile(t *testing.T) {
	got, err := domain.NormalizeBusinessProfile(domain.BusinessProfileInput{
		RegistrationNumber: " pvt-ab1cdef2 ", RegisteredName: " Acme Deliveries Limited ", TaxID: " p051234567x ", RegisteredAddress: " Moi Avenue ",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.RegistrationNumber != "PVT-AB1CDEF2" || got.RegisteredName != "Acme Deliveries Limited" || got.TaxID != "P051234567X" || got.RegisteredAddress != "Moi Avenue" {
		t.Fatalf("got %+v", got)
	}
	for name, in := range map[string]domain.BusinessProfileInput{
		"no registration number": {RegisteredName: "Acme"},
		"no registered name":     {RegistrationNumber: "PVT-1"},
		"a name over 200":        {RegistrationNumber: "PVT-1", RegisteredName: string(make([]byte, 201))},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := domain.NormalizeBusinessProfile(in); !errors.Is(err, domain.ErrInvalidBusinessProfile) {
				t.Fatalf("got %v, want ErrInvalidBusinessProfile", err)
			}
		})
	}
}

func TestNormalizeOwner_OwnershipPercentage(t *testing.T) {
	cases := []struct {
		pct     string
		wantBP  int64 // hundredths of a percent; -1 when absent
		wantErr bool
	}{
		{"", -1, false},
		{"0", 0, false},
		{"25.5", 2550, false},
		{"25.50", 2550, false},
		{"100", 10000, false},
		{"100.00", 10000, false},
		{"100.01", 0, true},
		{"101", 0, true},
		{"-1", 0, true},
		{"12.345", 0, true},
		{"abc", 0, true},
		{"1e2", 0, true},
		{".5", 0, true},
	}
	for _, tc := range cases {
		t.Run("pct "+tc.pct, func(t *testing.T) {
			got, err := domain.NormalizeOwner(domain.OwnerInput{FullName: "Grace", Role: domain.OwnerDirector, OwnershipPct: tc.pct})
			if tc.wantErr {
				if !errors.Is(err, domain.ErrInvalidOwner) {
					t.Fatalf("got %+v, %v; want ErrInvalidOwner", got, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if (tc.wantBP == -1) != (got.OwnershipBasisPoints == nil) || (got.OwnershipBasisPoints != nil && *got.OwnershipBasisPoints != tc.wantBP) {
				t.Fatalf("got %v, want %d", got.OwnershipBasisPoints, tc.wantBP)
			}
		})
	}
	for name, in := range map[string]domain.OwnerInput{
		"no name":         {Role: domain.OwnerDirector},
		"an unknown role": {FullName: "Grace", Role: "shareholder"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := domain.NormalizeOwner(in); !errors.Is(err, domain.ErrInvalidOwner) {
				t.Fatalf("got %v, want ErrInvalidOwner", err)
			}
		})
	}
}

func TestCanSubmitKYB(t *testing.T) {
	cases := []struct {
		name       string
		tier       domain.Tier
		hasProfile bool
		directors  int
		latest     string // latest business check's status, "" for none
		want       error
	}{
		{"a starter account with a profile and a director", starter, true, 1, "", nil},
		{"after a failed check, again", starter, true, 2, domain.KYCFailed, nil},
		{"a business account re-submitting after an expired check", business, true, 1, domain.KYCExpired, nil},
		{"the sandbox", sandbox, true, 1, "", domain.ErrKYBNeedsStarter},
		{"no profile", starter, false, 0, "", domain.ErrNoBusinessProfile},
		{"no director", starter, true, 0, "", domain.ErrNoDirector},
		{"one already in review", starter, true, 1, domain.KYCReview, domain.ErrBusinessProfileLocked},
		{"one already passed", business, true, 1, domain.KYCPassed, domain.ErrBusinessProfileLocked},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := domain.CanSubmitKYB(tc.tier, tc.hasProfile, tc.directors, tc.latest)
			if !errors.Is(err, tc.want) || (tc.want == nil && err != nil) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

func TestBusinessProfileLocked(t *testing.T) {
	for status, want := range map[string]bool{
		"": false, domain.KYCPending: false, domain.KYCFailed: false, domain.KYCExpired: false,
		domain.KYCReview: true, domain.KYCPassed: true,
	} {
		if got := domain.BusinessProfileLocked(status); got != want {
			t.Errorf("BusinessProfileLocked(%q) = %v, want %v", status, got, want)
		}
	}
}
