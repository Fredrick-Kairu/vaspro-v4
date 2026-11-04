package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/ids"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/accounts/adapters"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/accounts/domain"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/accounts/store"
)

// ErrBadWebhook means a vendor webhook's signature didn't verify or its
// timestamp is outside the window. api/ maps it to AUTH-4013.
var ErrBadWebhook = errors.New("app: KYC webhook signature invalid or expired")

// kycWebhookRole may find a check by its vendor reference across
// accounts (migration 20261103150000), only inside WithRoleTx.
const kycWebhookRole = "accounts_kyc_webhook"

// KYCCheck is a KYC check as an account sees it; no personal data.
type KYCCheck struct {
	ID        uuid.UUID
	Kind      string
	Status    string
	URL       string // the vendor's hosted flow, only when just started
	DecidedAt *time.Time
	Reason    string // the reviewer's reason, once staff have decided (task 2.9)
	CreatedAt time.Time
}

func kycCheckFromRow(row store.AccountsKycCheck) KYCCheck {
	c := KYCCheck{ID: row.ID, Kind: row.Kind, Status: row.Status, CreatedAt: row.CreatedAt}
	var result struct {
		DecisionReason string `json:"decision_reason"`
	}
	if json.Unmarshal(row.Result, &result) == nil {
		c.Reason = result.DecisionReason
	}
	if row.DecidedAt.Valid {
		t := row.DecidedAt.Time
		c.DecidedAt = &t
	}
	return c
}

// StartKYC starts a KYC check with the vendor (task 2.6, OQ-07): an
// Admin (account:manage) verifies themselves, so the subject is userID.
// Only individual checks until KYB (2.7). The vendor is called first,
// then the pending check and kyc.check_started are written together;
// the returned URL is the vendor's hosted flow.
func (s *Service) StartKYC(ctx context.Context, userID, tenantID uuid.UUID, kind string) (*KYCCheck, error) {
	if _, _, err := s.authorize(ctx, userID, tenantID, domain.PermAccountManage); err != nil {
		return nil, err
	}
	if kind != domain.KYCKindIndividual {
		return nil, fmt.Errorf("%w: only individual checks can be started until KYB (2.7)", ErrValidation)
	}
	if s.kyc == nil {
		return nil, fmt.Errorf("%w: KYC isn't configured", ErrValidation)
	}
	checkID, err := ids.New()
	if err != nil {
		return nil, fmt.Errorf("app: generating check id: %w", err)
	}
	started, err := s.kyc.StartCheck(ctx, adapters.KYCStart{CheckID: checkID, Kind: kind, SubjectID: userID})
	if err != nil {
		return nil, err
	}

	var check KYCCheck
	err = s.pool.WithTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		row, err := store.New(tx).CreateKycCheck(ctx, store.CreateKycCheckParams{
			ID: checkID, TenantID: tenantID, Kind: kind, SubjectID: userID,
			Provider: s.kyc.Name(), ProviderRef: pgtype.Text{String: started.ProviderRef, Valid: true}, Status: domain.KYCPending,
		})
		if err != nil {
			return fmt.Errorf("app: recording the check: %w", err)
		}
		check = kycCheckFromRow(row)
		return s.writeTenantEvent(ctx, tx, tenantID, "kyc.check_started", map[string]any{
			"tenant_id": tenantID, "check_id": checkID, "kind": kind, "subject_id": userID,
			"provider": s.kyc.Name(), "started_by": userID,
		})
	})
	if err != nil {
		return nil, err
	}
	check.URL = started.URL
	return &check, nil
}

// ListKYCChecks returns tenantID's checks, newest first (account:read).
func (s *Service) ListKYCChecks(ctx context.Context, userID, tenantID uuid.UUID) ([]KYCCheck, error) {
	if _, _, err := s.authorize(ctx, userID, tenantID, domain.PermAccountRead); err != nil {
		return nil, err
	}
	var checks []KYCCheck
	err := s.pool.WithTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := store.New(tx).ListKycChecks(ctx, tenantID)
		if err != nil {
			return fmt.Errorf("app: listing checks: %w", err)
		}
		checks = make([]KYCCheck, len(rows))
		for i, r := range rows {
			checks[i] = kycCheckFromRow(r)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return checks, nil
}

// HandleKYCWebhook applies a vendor's result (task 2.6, OQ-07). The
// adapter verifies the signature before anything is read
// (ErrBadWebhook otherwise). The check is found by its vendor
// reference; an unknown one is acknowledged and ignored. In the check's
// account transaction, domain.NextKYCStatus decides: a decided check is
// final, so a repeated or out-of-order result changes nothing. A passed
// individual check promotes a sandbox account to starter in the same
// transaction; nothing here ever demotes. Nothing from the body is
// logged.
func (s *Service) HandleKYCWebhook(ctx context.Context, provider string, header http.Header, body []byte) error {
	if s.kyc == nil || provider != s.kyc.Name() {
		return ErrNotFound
	}
	result, err := s.kyc.ParseWebhook(header, body, s.clk.Now())
	if errors.Is(err, adapters.ErrBadWebhookSignature) {
		return ErrBadWebhook
	}
	if err != nil {
		return fmt.Errorf("%w: %v", ErrValidation, err)
	}

	var found store.FindKycCheckByProviderRefRow
	err = s.pool.WithRoleTx(ctx, kycWebhookRole, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		found, err = store.New(tx).FindKycCheckByProviderRef(ctx, store.FindKycCheckByProviderRefParams{
			Provider: provider, ProviderRef: pgtype.Text{String: result.ProviderRef, Valid: true},
		})
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("app: finding the check: %w", err)
	}

	return s.pool.WithTenantTx(ctx, found.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		check, err := q.GetKycCheckForUpdate(ctx, store.GetKycCheckForUpdateParams(found))
		if err != nil {
			return fmt.Errorf("app: locking the check: %w", err)
		}
		next, changed := domain.NextKYCStatus(check.Status, result.Status)
		if !changed {
			return nil
		}
		summary, err := json.Marshal(result.Summary)
		if err != nil {
			return fmt.Errorf("app: encoding the result summary: %w", err)
		}
		if err := q.DecideKycCheck(ctx, store.DecideKycCheckParams{Status: next, Result: summary, ID: check.ID, TenantID: check.TenantID}); err != nil {
			return fmt.Errorf("app: recording the result: %w", err)
		}

		promoted := false
		if next == domain.KYCPassed && check.Kind == domain.KYCKindIndividual {
			tenant, err := q.GetTenantForUpdate(ctx, check.TenantID)
			if err != nil {
				return fmt.Errorf("app: locking tenant: %w", err)
			}
			if domain.Tier(tenant.Tier) == domain.TierSandbox && tenant.Status == domain.TenantActive {
				if err := s.changeTierTx(ctx, tx, ChangeTierRequest{
					TenantID: check.TenantID, To: domain.TierStarter, Reason: "individual KYC passed", KYCCheckID: &check.ID,
				}); err != nil {
					return err
				}
				promoted = true
			}
		}
		return s.writeTenantEvent(ctx, tx, check.TenantID, "kyc.check_decided", map[string]any{
			"tenant_id": check.TenantID, "check_id": check.ID, "kind": check.Kind, "status": next, "promoted": promoted,
		})
	})
}
