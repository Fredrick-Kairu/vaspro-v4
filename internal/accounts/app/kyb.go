package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/ids"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/accounts/domain"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/accounts/store"
)

// kybProvider is the kyc_checks.provider of a business check staff
// verify by hand (OQ-08: no registry lookup yet).
const kybProvider = "manual"

// registrationLookupRole may count business profiles across accounts
// (migration 20261103170000), only inside WithRoleTx.
const registrationLookupRole = "accounts_registration_lookup"

// BusinessOwner is a director, owner or signatory.
type BusinessOwner struct {
	ID           uuid.UUID
	FullName     string
	Role         string
	OwnershipPct string // "" or e.g. "25.50"
}

// BusinessProfile is an account's business details and its people.
type BusinessProfile struct {
	ID                 uuid.UUID
	Registry           string
	RegistrationNumber string
	RegisteredName     string
	TaxID              string
	RegisteredAddress  string
	VerifiedAt         *time.Time
	Owners             []BusinessOwner
}

func businessProfileFromRow(row store.AccountsBusinessProfile, owners []store.AccountsBeneficialOwner) *BusinessProfile {
	p := &BusinessProfile{
		ID: row.ID, Registry: row.Registry, RegistrationNumber: row.RegistrationNumber, RegisteredName: row.RegisteredName,
		TaxID: row.TaxID.String, RegisteredAddress: row.RegisteredAddress.String,
		Owners: make([]BusinessOwner, len(owners)),
	}
	if row.VerifiedAt.Valid {
		t := row.VerifiedAt.Time
		p.VerifiedAt = &t
	}
	for i, o := range owners {
		p.Owners[i] = ownerFromRow(o)
	}
	return p
}

func ownerFromRow(o store.AccountsBeneficialOwner) BusinessOwner {
	return BusinessOwner{ID: o.ID, FullName: o.FullName, Role: o.Role, OwnershipPct: formatPercent(o.OwnershipPct)}
}

// formatPercent renders numeric(5,2) as "25.50" without a float.
func formatPercent(n pgtype.Numeric) string {
	if !n.Valid || n.Int == nil {
		return ""
	}
	hundredths := new(big.Int).Set(n.Int)
	switch exp := int64(n.Exp) + 2; {
	case exp > 0:
		hundredths.Mul(hundredths, new(big.Int).Exp(big.NewInt(10), big.NewInt(exp), nil))
	case exp < 0:
		hundredths.Quo(hundredths, new(big.Int).Exp(big.NewInt(10), big.NewInt(-exp), nil))
	}
	whole, frac := new(big.Int).QuoRem(hundredths, big.NewInt(100), new(big.Int))
	return fmt.Sprintf("%s.%02d", whole, frac.Int64())
}

func percentFromBasisPoints(bp *int64) pgtype.Numeric {
	if bp == nil {
		return pgtype.Numeric{}
	}
	return pgtype.Numeric{Int: big.NewInt(*bp), Exp: -2, Valid: true}
}

// latestBusinessStatus is the status of tenantID's latest business
// check, "" for none; q runs in tenantID's transaction.
func latestBusinessStatus(ctx context.Context, q *store.Queries, tenantID uuid.UUID) (string, error) {
	c, err := q.LatestKycCheckOfKind(ctx, store.LatestKycCheckOfKindParams{TenantID: tenantID, Kind: domain.KYCKindBusiness})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("app: loading the latest business check: %w", err)
	}
	return c.Status, nil
}

// lockedForKYB locks tenantID's row and refuses when its business
// profile is in review or verified (domain.BusinessProfileLocked).
func lockedForKYB(ctx context.Context, q *store.Queries, tenantID uuid.UUID) (store.AccountsTenant, error) {
	tenant, err := q.GetTenantForUpdate(ctx, tenantID)
	if err != nil {
		return tenant, fmt.Errorf("app: locking tenant: %w", err)
	}
	latest, err := latestBusinessStatus(ctx, q, tenantID)
	if err != nil {
		return tenant, err
	}
	if domain.BusinessProfileLocked(latest) {
		return tenant, domain.ErrBusinessProfileLocked
	}
	return tenant, nil
}

// SetBusinessProfile enters or changes tenantID's one business profile
// (account:manage; task 2.7, OQ-08). The registry comes from the
// country pack. Locked while a business check is in review or after
// one passed.
func (s *Service) SetBusinessProfile(ctx context.Context, userID, tenantID uuid.UUID, in domain.BusinessProfileInput) (*BusinessProfile, error) {
	tenant, _, err := s.authorize(ctx, userID, tenantID, domain.PermAccountManage)
	if err != nil {
		return nil, err
	}
	pack, ok := s.packs[tenant.CountryCode]
	if !ok || pack.KYB == nil {
		return nil, fmt.Errorf("%w: business verification isn't available in %s yet", ErrValidation, tenant.CountryCode)
	}
	in, err = domain.NormalizeBusinessProfile(in)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrValidation, err)
	}

	var profile *BusinessProfile
	err = s.pool.WithTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		if _, err := lockedForKYB(ctx, q, tenantID); err != nil {
			return err
		}
		row, err := q.GetBusinessProfileByTenant(ctx, tenantID)
		switch {
		case err == nil:
			row, err = q.UpdateBusinessProfile(ctx, store.UpdateBusinessProfileParams{
				TenantID: tenantID, RegistrationNumber: in.RegistrationNumber, RegisteredName: in.RegisteredName,
				TaxID: optionalText(in.TaxID), RegisteredAddress: optionalText(in.RegisteredAddress),
			})
		case errors.Is(err, pgx.ErrNoRows):
			var id uuid.UUID
			if id, err = ids.New(); err != nil {
				return fmt.Errorf("app: generating profile id: %w", err)
			}
			row, err = q.CreateBusinessProfile(ctx, store.CreateBusinessProfileParams{
				ID: id, TenantID: tenantID, Registry: pack.KYB.Registry, RegistrationNumber: in.RegistrationNumber,
				RegisteredName: in.RegisteredName, TaxID: optionalText(in.TaxID), RegisteredAddress: optionalText(in.RegisteredAddress),
			})
		}
		if err != nil {
			return fmt.Errorf("app: saving the business profile: %w", err)
		}
		owners, err := q.ListBeneficialOwners(ctx, store.ListBeneficialOwnersParams{TenantID: tenantID, BusinessProfileID: row.ID})
		if err != nil {
			return fmt.Errorf("app: listing owners: %w", err)
		}
		profile = businessProfileFromRow(row, owners)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return profile, nil
}

// GetBusinessProfile returns tenantID's profile and its people
// (account:read); ErrNotFound when there's none yet.
func (s *Service) GetBusinessProfile(ctx context.Context, userID, tenantID uuid.UUID) (*BusinessProfile, error) {
	if _, _, err := s.authorize(ctx, userID, tenantID, domain.PermAccountRead); err != nil {
		return nil, err
	}
	var profile *BusinessProfile
	err := s.pool.WithTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		row, err := q.GetBusinessProfileByTenant(ctx, tenantID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("app: loading the business profile: %w", err)
		}
		owners, err := q.ListBeneficialOwners(ctx, store.ListBeneficialOwnersParams{TenantID: tenantID, BusinessProfileID: row.ID})
		if err != nil {
			return fmt.Errorf("app: listing owners: %w", err)
		}
		profile = businessProfileFromRow(row, owners)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return profile, nil
}

// AddOwner adds a director, owner or signatory to tenantID's profile
// (account:manage); ErrNotFound without a profile, locked like it.
func (s *Service) AddOwner(ctx context.Context, userID, tenantID uuid.UUID, in domain.OwnerInput) (*BusinessOwner, error) {
	if _, _, err := s.authorize(ctx, userID, tenantID, domain.PermAccountManage); err != nil {
		return nil, err
	}
	owner, err := domain.NormalizeOwner(in)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	var added BusinessOwner
	err = s.pool.WithTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		if _, err := lockedForKYB(ctx, q, tenantID); err != nil {
			return err
		}
		profile, err := q.GetBusinessProfileByTenant(ctx, tenantID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("app: loading the business profile: %w", err)
		}
		id, err := ids.New()
		if err != nil {
			return fmt.Errorf("app: generating owner id: %w", err)
		}
		row, err := q.CreateBeneficialOwner(ctx, store.CreateBeneficialOwnerParams{
			ID: id, TenantID: tenantID, BusinessProfileID: profile.ID, FullName: owner.FullName, Role: owner.Role,
			OwnershipPct: percentFromBasisPoints(owner.OwnershipBasisPoints),
		})
		if err != nil {
			return fmt.Errorf("app: adding the owner: %w", err)
		}
		added = ownerFromRow(row)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &added, nil
}

// RemoveOwner removes one of tenantID's owners (account:manage); an
// unknown one or another account's is ErrNotFound. Locked like the
// profile.
func (s *Service) RemoveOwner(ctx context.Context, userID, tenantID, ownerID uuid.UUID) error {
	if _, _, err := s.authorize(ctx, userID, tenantID, domain.PermAccountManage); err != nil {
		return err
	}
	return s.pool.WithTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		if _, err := lockedForKYB(ctx, q, tenantID); err != nil {
			return err
		}
		n, err := q.DeleteBeneficialOwner(ctx, store.DeleteBeneficialOwnerParams{ID: ownerID, TenantID: tenantID})
		if err != nil {
			return fmt.Errorf("app: removing the owner: %w", err)
		}
		if n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// SubmitKYB sends tenantID's business for verification (account:manage;
// task 2.7, OQ-08): from the starter tier, with a profile and a
// director, one at a time (domain.CanSubmitKYB). There's no registry
// lookup yet, so the business check goes straight to staff review
// (2.9), who decide and promote. A registration number other accounts
// also use is flagged in the check's result for the reviewer, not
// refused (data model §4).
func (s *Service) SubmitKYB(ctx context.Context, userID, tenantID uuid.UUID) (*KYCCheck, error) {
	if _, _, err := s.authorize(ctx, userID, tenantID, domain.PermAccountManage); err != nil {
		return nil, err
	}

	// The duplicate count reads across accounts, so it runs as the
	// narrow lookup role in its own transaction; it only flags, so a
	// profile changing in between can't do harm (the profile is locked
	// once the check exists anyway).
	var profile *store.AccountsBusinessProfile
	if err := s.pool.WithTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		row, err := store.New(tx).GetBusinessProfileByTenant(ctx, tenantID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		profile = &row
		return err
	}); err != nil {
		return nil, fmt.Errorf("app: loading the business profile: %w", err)
	}
	var others int64
	if profile != nil {
		if err := s.pool.WithRoleTx(ctx, registrationLookupRole, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			others, err = store.New(tx).CountOtherProfilesWithRegistration(ctx, store.CountOtherProfilesWithRegistrationParams{
				Registry: profile.Registry, RegistrationNumber: profile.RegistrationNumber, TenantID: tenantID,
			})
			return err
		}); err != nil {
			return nil, fmt.Errorf("app: checking for duplicate registrations: %w", err)
		}
	}

	var check KYCCheck
	err := s.pool.WithTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		tenant, err := q.GetTenantForUpdate(ctx, tenantID)
		if err != nil {
			return fmt.Errorf("app: locking tenant: %w", err)
		}
		latest, err := latestBusinessStatus(ctx, q, tenantID)
		if err != nil {
			return err
		}
		directors := 0
		if profile != nil {
			owners, err := q.ListBeneficialOwners(ctx, store.ListBeneficialOwnersParams{TenantID: tenantID, BusinessProfileID: profile.ID})
			if err != nil {
				return fmt.Errorf("app: listing owners: %w", err)
			}
			for _, o := range owners {
				if o.Role == domain.OwnerDirector {
					directors++
				}
			}
		}
		if err := domain.CanSubmitKYB(domain.Tier(tenant.Tier), profile != nil, directors, latest); err != nil {
			return err
		}

		result, err := json.Marshal(map[string]any{"duplicate_registration": others > 0, "other_accounts": others})
		if err != nil {
			return fmt.Errorf("app: encoding the check summary: %w", err)
		}
		id, err := ids.New()
		if err != nil {
			return fmt.Errorf("app: generating check id: %w", err)
		}
		row, err := q.CreateKycCheckWithResult(ctx, store.CreateKycCheckWithResultParams{
			ID: id, TenantID: tenantID, Kind: domain.KYCKindBusiness, SubjectID: profile.ID,
			Provider: kybProvider, Status: domain.KYCReview, Result: result,
		})
		if err != nil {
			return fmt.Errorf("app: recording the business check: %w", err)
		}
		check = kycCheckFromRow(row)
		return s.writeTenantEvent(ctx, tx, tenantID, "kyc.check_started", map[string]any{
			"tenant_id": tenantID, "check_id": id, "kind": domain.KYCKindBusiness, "subject_id": profile.ID,
			"provider": kybProvider, "started_by": userID,
		})
	})
	if err != nil {
		return nil, err
	}
	return &check, nil
}
