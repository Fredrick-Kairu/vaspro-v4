package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	platformevents "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/events"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/ids"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/outbox"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/accounts/domain"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/accounts/store"
)

// MakeResellerRequest asks for a direct account to become a reseller.
type MakeResellerRequest struct {
	TenantID  uuid.UUID
	ChangedBy uuid.UUID // the staff member
	Reason    string
}

// SubAccountInput is what a reseller's member gives for a new
// sub-account; its country comes from the reseller.
type SubAccountInput struct {
	DisplayName  string
	LegalName    string
	BillingEmail string
}

// MakeReseller makes an active, financial-tier direct account a
// reseller (domain.CanBecomeReseller). In one tenant transaction it
// locks the tenant, checks the rules, then writes the new type and
// tenant.type_changed together; a refused change writes nothing.
// ChangedBy is trusted as a staff member here, as in ChangeTier; the
// API that exposes it checks the staff role (task 2.9).
func (s *Service) MakeReseller(ctx context.Context, req MakeResellerRequest) error {
	return s.pool.WithTenantTx(ctx, req.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		tenant, err := q.GetTenantForUpdate(ctx, req.TenantID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("app: locking tenant: %w", err)
		}
		if err := domain.CanBecomeReseller(factsOf(tenant), req.Reason); err != nil {
			return err
		}
		if err := q.UpdateTenantType(ctx, store.UpdateTenantTypeParams{ID: req.TenantID, Type: store.AccountsTenantTypeReseller}); err != nil {
			return fmt.Errorf("app: updating tenant type: %w", err)
		}

		eventID, err := ids.New()
		if err != nil {
			return fmt.Errorf("app: generating event id: %w", err)
		}
		env, err := platformevents.New(eventID, "tenant.type_changed", 1, req.TenantID, s.clk.Now(), "", map[string]any{
			"tenant_id":  req.TenantID,
			"from_type":  tenant.Type,
			"to_type":    domain.TenantReseller,
			"reason":     req.Reason,
			"changed_by": req.ChangedBy,
		})
		if err != nil {
			return fmt.Errorf("app: building tenant.type_changed event: %w", err)
		}
		return outbox.Write(ctx, tx, "tenant", req.TenantID, env)
	})
}

// CreateSubAccount creates a sub-account under parentID for one of the
// reseller's clients (agreed 2 Nov, OQ-04). userID must be an active
// member of the reseller whose role allows account:manage, and the
// reseller must still be allowed to
// create one (domain.CanCreateSubAccount), checked under a lock on its
// row so a demotion can't interleave. The sub-account starts in the
// sandbox and takes the reseller's country, currency, time zone and
// region. It gets no membership: the reseller's members reach it
// through the parent (GetTenant). The row and tenant.created are
// written in the reseller's transaction, which the tenants policy
// allows through parent_tenant_id.
func (s *Service) CreateSubAccount(ctx context.Context, userID, parentID uuid.UUID, in SubAccountInput) (*Tenant, error) {
	subID, err := ids.New()
	if err != nil {
		return nil, fmt.Errorf("app: generating tenant id: %w", err)
	}

	var created *Tenant
	err = s.pool.WithTenantTx(ctx, parentID, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		if err := requireMemberWith(ctx, q, parentID, userID, domain.PermAccountManage); err != nil {
			return err
		}
		parent, err := q.GetTenantForUpdate(ctx, parentID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("app: locking reseller: %w", err)
		}
		if err := domain.CanCreateSubAccount(factsOf(parent)); err != nil {
			return err
		}
		norm, err := domain.NormalizeNewTenant(domain.NewTenantInput{
			DisplayName: in.DisplayName, CountryCode: parent.CountryCode, LegalName: in.LegalName, BillingEmail: in.BillingEmail,
		})
		if err != nil {
			return fmt.Errorf("%w: %v", ErrValidation, err)
		}

		row, err := q.CreateTenant(ctx, store.CreateTenantParams{
			ID:              subID,
			Type:            store.AccountsTenantTypeSubAccount,
			ParentTenantID:  pgtype.UUID{Bytes: parentID, Valid: true},
			LegalName:       optionalText(norm.LegalName),
			DisplayName:     norm.DisplayName,
			CountryCode:     parent.CountryCode,
			Timezone:        parent.Timezone,
			DefaultCurrency: parent.DefaultCurrency,
			Region:          parent.Region,
			BillingEmail:    optionalText(norm.BillingEmail),
		})
		if err != nil {
			return fmt.Errorf("app: creating sub-account: %w", err)
		}

		eventID, err := ids.New()
		if err != nil {
			return fmt.Errorf("app: generating event id: %w", err)
		}
		env, err := platformevents.New(eventID, "tenant.created", 1, subID, s.clk.Now(), "", map[string]any{
			"tenant_id":        subID,
			"type":             domain.TenantSubAccount,
			"parent_tenant_id": parentID,
			"display_name":     row.DisplayName,
			"country_code":     row.CountryCode,
			"tier":             row.Tier,
			"created_by":       userID,
		})
		if err != nil {
			return fmt.Errorf("app: building tenant.created event: %w", err)
		}
		if err := outbox.Write(ctx, tx, "tenant", subID, env); err != nil {
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

// ListSubAccounts returns parentID's sub-accounts, oldest first, if
// userID's role in it allows account:read.
func (s *Service) ListSubAccounts(ctx context.Context, userID, parentID uuid.UUID) ([]*Tenant, error) {
	var subs []*Tenant
	err := s.pool.WithTenantTx(ctx, parentID, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		if err := requireMemberWith(ctx, q, parentID, userID, domain.PermAccountRead); err != nil {
			return err
		}
		rows, err := q.ListSubAccounts(ctx, pgtype.UUID{Bytes: parentID, Valid: true})
		if err != nil {
			return fmt.Errorf("app: listing sub-accounts: %w", err)
		}
		subs = make([]*Tenant, 0, len(rows))
		for _, row := range rows {
			subs = append(subs, tenantFromRow(row))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return subs, nil
}

func factsOf(t store.AccountsTenant) domain.TenantFacts {
	return domain.TenantFacts{Type: string(t.Type), Tier: domain.Tier(t.Tier), Status: t.Status}
}
