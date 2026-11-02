package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	platformevents "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/events"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/ids"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/outbox"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/accounts/domain"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/accounts/store"
)

// Tenant is an account as its members see it.
type Tenant struct {
	ID              uuid.UUID
	Type            string
	ParentTenantID  *uuid.UUID
	LegalName       string
	DisplayName     string
	CountryCode     string
	Timezone        string
	DefaultCurrency string
	Region          string
	Tier            domain.Tier
	Status          string
	BillingEmail    string
	CreatedAt       time.Time
}

func tenantFromRow(row store.AccountsTenant) *Tenant {
	t := &Tenant{
		ID:              row.ID,
		Type:            string(row.Type),
		LegalName:       row.LegalName.String,
		DisplayName:     row.DisplayName,
		CountryCode:     row.CountryCode,
		Timezone:        row.Timezone,
		DefaultCurrency: row.DefaultCurrency,
		Region:          row.Region,
		Tier:            domain.Tier(row.Tier),
		Status:          row.Status,
		BillingEmail:    row.BillingEmail.String,
		CreatedAt:       row.CreatedAt,
	}
	if row.ParentTenantID.Valid {
		id := uuid.UUID(row.ParentTenantID.Bytes)
		t.ParentTenantID = &id
	}
	return t
}

func optionalText(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }

// CreateTenant creates a direct account in the sandbox with userID as
// its Admin (onboarding's "create your account", decision Q1). Currency,
// time zone and data region come from the country's pack. The account,
// the Admin membership and tenant.created are written together.
func (s *Service) CreateTenant(ctx context.Context, userID uuid.UUID, in domain.NewTenantInput) (*Tenant, error) {
	in, err := domain.NormalizeNewTenant(in)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	pack, ok := s.packs[in.CountryCode]
	if !ok {
		return nil, fmt.Errorf("%w: VASPRO doesn't serve country %q yet", ErrValidation, in.CountryCode)
	}
	tenantID, err := ids.New()
	if err != nil {
		return nil, fmt.Errorf("app: generating tenant id: %w", err)
	}

	var created *Tenant
	err = s.pool.WithTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		row, err := q.CreateTenant(ctx, store.CreateTenantParams{
			ID:              tenantID,
			Type:            store.AccountsTenantTypeDirect,
			LegalName:       optionalText(in.LegalName),
			DisplayName:     in.DisplayName,
			CountryCode:     in.CountryCode,
			Timezone:        pack.Timezone,
			DefaultCurrency: pack.Currency.Code,
			Region:          pack.DataRegion,
			BillingEmail:    optionalText(in.BillingEmail),
		})
		if err != nil {
			return fmt.Errorf("app: creating tenant: %w", err)
		}
		if _, err := q.CreateMembership(ctx, store.CreateMembershipParams{
			TenantID: tenantID,
			UserID:   userID,
			RoleID:   domain.BuiltinAdminRoleID,
			Status:   "active",
		}); err != nil {
			return fmt.Errorf("app: adding the creator as Admin: %w", err)
		}

		eventID, err := ids.New()
		if err != nil {
			return fmt.Errorf("app: generating event id: %w", err)
		}
		env, err := platformevents.New(eventID, "tenant.created", 1, tenantID, s.clk.Now(), "", map[string]any{
			"tenant_id":    tenantID,
			"type":         domain.TenantDirect,
			"display_name": row.DisplayName,
			"country_code": row.CountryCode,
			"tier":         row.Tier,
			"created_by":   userID,
		})
		if err != nil {
			return fmt.Errorf("app: building tenant.created event: %w", err)
		}
		if err := outbox.Write(ctx, tx, "tenant", tenantID, env); err != nil {
			return err
		}
		created = tenantFromRow(row)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return created, nil
}

// GetTenant returns tenantID if userID is an active member of it, or,
// for a sub-account, an active member of its reseller (agreed 2 Nov,
// OQ-04; any role for now, task 2.3 narrows it). A tenant that doesn't
// exist and one the user can't see are both ErrNotFound. A sub-account's
// own members don't reach its reseller or siblings: only the parent
// link grants access, and only downwards.
func (s *Service) GetTenant(ctx context.Context, userID, tenantID uuid.UUID) (*Tenant, error) {
	var (
		row    store.AccountsTenant
		member bool
	)
	err := s.pool.WithTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		var err error
		row, err = q.GetTenantByID(ctx, tenantID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("app: loading tenant: %w", err)
		}
		member, err = isMember(ctx, q, tenantID, userID)
		return err
	})
	if err != nil {
		return nil, err
	}
	if !member {
		if !row.ParentTenantID.Valid {
			return nil, ErrNotFound
		}
		// The parent's memberships are only visible in its own tenant
		// transaction.
		parentID := uuid.UUID(row.ParentTenantID.Bytes)
		if err := s.pool.WithTenantTx(ctx, parentID, func(ctx context.Context, tx pgx.Tx) error {
			return requireMember(ctx, store.New(tx), parentID, userID)
		}); err != nil {
			return nil, err
		}
	}
	return tenantFromRow(row), nil
}

// isMember reports whether userID is an active member of tenantID; q
// must run in tenantID's transaction.
func isMember(ctx context.Context, q *store.Queries, tenantID, userID uuid.UUID) (bool, error) {
	_, err := q.GetActiveMembership(ctx, store.GetActiveMembershipParams{TenantID: tenantID, UserID: userID})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("app: checking membership: %w", err)
	}
	return true, nil
}

// requireMember is isMember with ErrNotFound for a non-member.
func requireMember(ctx context.Context, q *store.Queries, tenantID, userID uuid.UUID) error {
	ok, err := isMember(ctx, q, tenantID, userID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	return nil
}
