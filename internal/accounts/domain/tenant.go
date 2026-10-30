package domain

import (
	"errors"
	"net/mail"
	"strings"

	"github.com/google/uuid"
)

// BuiltinAdminRoleID is the built-in Admin role (migration
// 20261030160000) every new account's creator gets.
var BuiltinAdminRoleID = uuid.MustParse("019a0000-0000-7000-8000-000000000001")

// Tenant types (accounts.tenant_type).
const (
	TenantDirect     = "direct"
	TenantReseller   = "reseller"
	TenantSubAccount = "sub_account"
)

// ErrInvalidTenant wraps every rejected account field.
var ErrInvalidTenant = errors.New("domain: invalid account details")

const maxNameLength = 200

// NewTenantInput is what a user gives at onboarding.
type NewTenantInput struct {
	DisplayName  string
	CountryCode  string
	LegalName    string
	BillingEmail string
}

// NormalizeNewTenant trims and checks a new account's fields: a display
// name, a two-letter country code (upper-cased), an optional legal name
// and an optional billing email. Whether the country has a pack is the
// caller's check.
func NormalizeNewTenant(in NewTenantInput) (NewTenantInput, error) {
	out := NewTenantInput{
		DisplayName:  strings.TrimSpace(in.DisplayName),
		CountryCode:  strings.ToUpper(strings.TrimSpace(in.CountryCode)),
		LegalName:    strings.TrimSpace(in.LegalName),
		BillingEmail: strings.TrimSpace(in.BillingEmail),
	}
	switch {
	case out.DisplayName == "":
		return out, errors.Join(ErrInvalidTenant, errors.New("display name is required"))
	case len(out.DisplayName) > maxNameLength || len(out.LegalName) > maxNameLength:
		return out, errors.Join(ErrInvalidTenant, errors.New("names are limited to 200 characters"))
	case len(out.CountryCode) != 2:
		return out, errors.Join(ErrInvalidTenant, errors.New("country code must be two letters"))
	}
	if out.BillingEmail != "" {
		addr, err := mail.ParseAddress(out.BillingEmail)
		if err != nil || addr.Address != out.BillingEmail {
			return out, errors.Join(ErrInvalidTenant, errors.New("billing email is not a valid address"))
		}
	}
	return out, nil
}
