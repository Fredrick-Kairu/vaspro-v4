package domain_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/accounts/domain"
)

func TestNormalizeNewTenant(t *testing.T) {
	cases := []struct {
		name string
		in   domain.NewTenantInput
		want error
	}{
		{"a display name and country are enough", domain.NewTenantInput{DisplayName: "Acme", CountryCode: "KE"}, nil},
		{"the country code is upper-cased", domain.NewTenantInput{DisplayName: "Acme", CountryCode: " ke "}, nil},
		{"a blank display name is rejected", domain.NewTenantInput{DisplayName: "   ", CountryCode: "KE"}, domain.ErrInvalidTenant},
		{"a three-letter country is rejected", domain.NewTenantInput{DisplayName: "Acme", CountryCode: "KEN"}, domain.ErrInvalidTenant},
		{"an overlong name is rejected", domain.NewTenantInput{DisplayName: strings.Repeat("a", 201), CountryCode: "KE"}, domain.ErrInvalidTenant},
		{"a bad billing email is rejected", domain.NewTenantInput{DisplayName: "Acme", CountryCode: "KE", BillingEmail: "not-an-email"}, domain.ErrInvalidTenant},
		{"a display-name email is rejected", domain.NewTenantInput{DisplayName: "Acme", CountryCode: "KE", BillingEmail: "Billing <b@acme.example>"}, domain.ErrInvalidTenant},
		{"a plain billing email is fine", domain.NewTenantInput{DisplayName: "Acme", CountryCode: "KE", BillingEmail: "billing@acme.example"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := domain.NormalizeNewTenant(tc.in)
			if !errors.Is(err, tc.want) || (tc.want == nil && err != nil) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			if err == nil && out.CountryCode != "KE" {
				t.Fatalf("country %q, want KE", out.CountryCode)
			}
		})
	}
}
