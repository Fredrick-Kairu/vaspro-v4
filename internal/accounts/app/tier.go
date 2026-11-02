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

// ErrCheckNotUsable means the KYC check named as evidence can't back
// this change: it belongs to another tenant, doesn't exist, or its
// status doesn't fit (a promotion needs a passed check, a demotion a
// failed or expired one).
var ErrCheckNotUsable = errors.New("app: KYC check can't back this tier change")

// ChangeTierRequest asks for a tenant's tier to change.
type ChangeTierRequest struct {
	TenantID   uuid.UUID
	To         domain.Tier
	Reason     string
	KYCCheckID *uuid.UUID // the check behind the change, if any
	ChangedBy  *uuid.UUID // the staff member, if any; nil when automatic
}

// ChangeTier is the only way a tenant's tier changes. In one tenant
// transaction it locks the tenant, checks the evidence and the rules
// (domain.ValidateTierChange), then writes the new tier, its
// tier_changes row and a tenant.tier_changed event together; the
// tenants_tier_change_recorded trigger refuses the tier update without
// that row. The account's selected services follow the new tier in the
// same transaction (task 2.4). A refused change writes nothing. ChangedBy is trusted as a
// staff member here; the API that exposes staff changes checks the
// staff role (task 2.9).
func (s *Service) ChangeTier(ctx context.Context, req ChangeTierRequest) error {
	return s.pool.WithTenantTx(ctx, req.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		tenant, err := q.GetTenantForUpdate(ctx, req.TenantID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("app: locking tenant: %w", err)
		}
		from := domain.Tier(tenant.Tier)

		evidence := domain.Evidence{ByStaff: req.ChangedBy != nil}
		if req.KYCCheckID != nil {
			check, err := q.GetKycCheckByID(ctx, store.GetKycCheckByIDParams{ID: *req.KYCCheckID, TenantID: req.TenantID})
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrCheckNotUsable
			}
			if err != nil {
				return fmt.Errorf("app: looking up KYC check: %w", err)
			}
			if !checkStatusFits(from, req.To, check.Status) {
				return ErrCheckNotUsable
			}
			evidence.KYCCheckKind = check.Kind
		}

		if err := domain.ValidateTierChange(domain.TierChange{
			From: from, To: req.To, TenantStatus: tenant.Status, Reason: req.Reason, Evidence: evidence,
		}); err != nil {
			return err
		}

		if err := q.UpdateTenantTier(ctx, store.UpdateTenantTierParams{ID: req.TenantID, Tier: store.AccountsTier(req.To)}); err != nil {
			return fmt.Errorf("app: updating tier: %w", err)
		}
		changeID, err := ids.New()
		if err != nil {
			return fmt.Errorf("app: generating tier change id: %w", err)
		}
		if _, err := q.CreateTierChange(ctx, store.CreateTierChangeParams{
			ID:         changeID,
			TenantID:   req.TenantID,
			FromTier:   store.AccountsTier(from),
			ToTier:     store.AccountsTier(req.To),
			Reason:     req.Reason,
			KycCheckID: optionalUUID(req.KYCCheckID),
			ChangedBy:  optionalUUID(req.ChangedBy),
		}); err != nil {
			return fmt.Errorf("app: recording tier change: %w", err)
		}
		if err := s.reevaluateServices(ctx, q, tx, req.TenantID, tenant.CountryCode, req.To); err != nil {
			return err
		}

		data := map[string]any{
			"tenant_id": req.TenantID,
			"from_tier": from,
			"to_tier":   req.To,
			"reason":    req.Reason,
		}
		if req.KYCCheckID != nil {
			data["kyc_check_id"] = *req.KYCCheckID
		}
		if req.ChangedBy != nil {
			data["changed_by"] = *req.ChangedBy
		}
		eventID, err := ids.New()
		if err != nil {
			return fmt.Errorf("app: generating event id: %w", err)
		}
		env, err := platformevents.New(eventID, "tenant.tier_changed", 1, req.TenantID, s.clk.Now(), "", data)
		if err != nil {
			return fmt.Errorf("app: building tenant.tier_changed event: %w", err)
		}
		return outbox.Write(ctx, tx, "tenant", req.TenantID, env)
	})
}

// checkStatusFits reports whether a check in status can back a change
// from → to: a promotion needs a passed check, a demotion one that
// failed or expired (re-verification). An unknown tier fits nothing
// here; ValidateTierChange reports it.
func checkStatusFits(from, to domain.Tier, status string) bool {
	fromRank, okFrom := from.Rank()
	toRank, okTo := to.Rank()
	if !okFrom || !okTo {
		return true
	}
	if toRank > fromRank {
		return status == "passed"
	}
	return status == "failed" || status == "expired"
}

func optionalUUID(id *uuid.UUID) pgtype.UUID {
	if id == nil {
		return pgtype.UUID{}
	}
	return pgtype.UUID{Bytes: *id, Valid: true}
}
