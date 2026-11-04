package domain

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
)

// Roles of a business's people (accounts.beneficial_owners.role).
const (
	OwnerDirector  = "director"
	OwnerOwner     = "owner"
	OwnerSignatory = "signatory"
)

var (
	ErrInvalidBusinessProfile = errors.New("domain: invalid business profile")
	ErrInvalidOwner           = errors.New("domain: invalid owner")
	ErrKYBNeedsStarter        = errors.New("domain: KYB needs the starter tier first")
	ErrNoBusinessProfile      = errors.New("domain: no business profile")
	ErrNoDirector             = errors.New("domain: a business needs at least one director")
	ErrBusinessProfileLocked  = errors.New("domain: the business profile is in review or verified")
	ErrMissingDocuments       = errors.New("domain: required documents are missing")
)

// BusinessProfileInput is what a client enters (task 2.7). The registry
// comes from the country pack, not from here.
type BusinessProfileInput struct {
	RegistrationNumber string
	RegisteredName     string
	TaxID              string
	RegisteredAddress  string
}

// NormalizeBusinessProfile trims the profile, upper-cases the
// registration number and tax ID, and requires the number and name.
// Formats aren't checked: staff verify the details (OQ-08), and
// per-country formats belong in the pack once a registry is integrated.
func NormalizeBusinessProfile(in BusinessProfileInput) (BusinessProfileInput, error) {
	out := BusinessProfileInput{
		RegistrationNumber: strings.ToUpper(strings.TrimSpace(in.RegistrationNumber)),
		RegisteredName:     strings.TrimSpace(in.RegisteredName),
		TaxID:              strings.ToUpper(strings.TrimSpace(in.TaxID)),
		RegisteredAddress:  strings.TrimSpace(in.RegisteredAddress),
	}
	switch {
	case out.RegistrationNumber == "":
		return out, errors.Join(ErrInvalidBusinessProfile, errors.New("registration number is required"))
	case out.RegisteredName == "":
		return out, errors.Join(ErrInvalidBusinessProfile, errors.New("registered name is required"))
	case len(out.RegisteredName) > maxNameLength || len(out.RegisteredAddress) > 500 ||
		len(out.RegistrationNumber) > 64 || len(out.TaxID) > 64:
		return out, errors.Join(ErrInvalidBusinessProfile, errors.New("a field is too long"))
	}
	return out, nil
}

// OwnerInput is a director, owner or signatory as entered.
type OwnerInput struct {
	FullName     string
	Role         string
	OwnershipPct string // "" or a decimal string, e.g. "25.50"
}

// Owner is a checked OwnerInput. OwnershipBasisPoints is the percentage
// in hundredths (2550 for 25.50%), so no float is ever involved.
type Owner struct {
	FullName             string
	Role                 string
	OwnershipBasisPoints *int64
}

var percentage = regexp.MustCompile(`^([0-9]{1,3})(?:\.([0-9]{1,2}))?$`)

// NormalizeOwner checks a name, a known role and an optional percentage
// from 0 to 100 with up to two decimal places.
func NormalizeOwner(in OwnerInput) (Owner, error) {
	out := Owner{FullName: strings.TrimSpace(in.FullName), Role: in.Role}
	if out.FullName == "" || len(out.FullName) > maxNameLength {
		return out, errors.Join(ErrInvalidOwner, errors.New("a name up to 200 characters is required"))
	}
	if out.Role != OwnerDirector && out.Role != OwnerOwner && out.Role != OwnerSignatory {
		return out, errors.Join(ErrInvalidOwner, errors.New("role must be director, owner or signatory"))
	}
	pct := strings.TrimSpace(in.OwnershipPct)
	if pct == "" {
		return out, nil
	}
	m := percentage.FindStringSubmatch(pct)
	if m == nil {
		return out, errors.Join(ErrInvalidOwner, errors.New("ownership_pct must be a number from 0 to 100 with up to two decimal places"))
	}
	whole, _ := strconv.ParseInt(m[1], 10, 64)
	frac := m[2]
	for len(frac) < 2 {
		frac += "0"
	}
	hundredths, _ := strconv.ParseInt(frac, 10, 64)
	bp := whole*100 + hundredths
	if bp > 10000 {
		return out, errors.Join(ErrInvalidOwner, errors.New("ownership_pct can't be over 100"))
	}
	out.OwnershipBasisPoints = &bp
	return out, nil
}

// CanSubmitKYB reports whether an account may submit its business for
// verification (agreed 3 Nov, OQ-08): from the starter tier, with a
// profile and at least one director, and not while a business check is
// in review or after one passed. latest is the latest business check's
// status, "" for none.
func CanSubmitKYB(tier Tier, hasProfile bool, directors int, latest string) error {
	switch {
	case !Unlocked(tier, TierStarter):
		return ErrKYBNeedsStarter
	case !hasProfile:
		return ErrNoBusinessProfile
	case directors == 0:
		return ErrNoDirector
	case BusinessProfileLocked(latest):
		return ErrBusinessProfileLocked
	}
	return nil
}

// BusinessProfileLocked reports whether the profile and its owners are
// frozen: while staff review a business check, and after one passed
// (until re-verification exists).
func BusinessProfileLocked(latest string) bool {
	return latest == KYCReview || latest == KYCPassed
}
