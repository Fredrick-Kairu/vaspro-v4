package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/accounts/domain"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/accounts/store"
)

// ErrNotStaff means the caller isn't VASPRO staff (task 2.9, OQ-10).
// api/ maps it to AUTH-4025.
var ErrNotStaff = errors.New("app: staff only")

// ErrAlreadyDecided means the check isn't in review any more: staff or
// the vendor decided it, or it never went to review. api/ maps it to
// VAL-4010.
var ErrAlreadyDecided = errors.New("app: the check isn't waiting for review")

// staffReviewRole may read every check, and the accounts that have one,
// across accounts (migration 20261105100000), only inside WithRoleTx.
const staffReviewRole = "accounts_staff_review"

const (
	defaultReviewPage = 50
	maxReviewPage     = 100
)

// KYCReviewSummary is one check in the staff queue.
type KYCReviewSummary struct {
	CheckID, TenantID              uuid.UUID
	AccountName, CountryCode, Tier string
	TenantStatus, Kind             string
	CreatedAt                      time.Time
	DuplicateRegistration          bool // KYB only (task 2.7)
	OtherAccounts                  int  // KYB only
}

// KYCReviewPage is a page of the staff queue; NextCursor is empty on
// the last page.
type KYCReviewPage struct {
	Reviews    []KYCReviewSummary
	NextCursor string
}

// KYCReview is one check in review with what the reviewer needs: for a
// business check, the profile, its owners and the documents it was
// submitted with; for an individual check, the subject's email and
// name (left out if identity can't be reached).
type KYCReview struct {
	Summary                   KYCReviewSummary
	Check                     KYCCheck
	SubjectEmail, SubjectName string
	BusinessProfile           *BusinessProfile
	Documents                 []Document
}

// checkResult is the part of kyc_checks.result staff review reads.
type checkResult struct {
	DuplicateRegistration bool        `json:"duplicate_registration"`
	OtherAccounts         int         `json:"other_accounts"`
	DocumentIDs           []uuid.UUID `json:"document_ids"`
}

func parseCheckResult(raw []byte) checkResult {
	var r checkResult
	_ = json.Unmarshal(raw, &r) // a vendor's summary may hold none of these
	return r
}

// requireStaff asks identity on every call, so a revocation takes
// effect at once; identity unreachable fails closed.
func (s *Service) requireStaff(ctx context.Context, userID uuid.UUID) error {
	staff, err := s.identity.IsStaff(ctx, userID)
	if err != nil {
		return fmt.Errorf("app: checking staff: %w", err)
	}
	if !staff {
		return ErrNotStaff
	}
	return nil
}

// ListKYCReviews is the staff queue (task 2.9): checks in review across
// accounts, oldest first, limit per page (default 50, at most 100),
// after cursor.
func (s *Service) ListKYCReviews(ctx context.Context, staffID uuid.UUID, cursor string, limit int) (*KYCReviewPage, error) {
	if err := s.requireStaff(ctx, staffID); err != nil {
		return nil, err
	}
	if limit == 0 {
		limit = defaultReviewPage
	}
	if limit < 1 || limit > maxReviewPage {
		return nil, fmt.Errorf("%w: limit must be 1 to %d", ErrValidation, maxReviewPage)
	}
	params := store.ListKycReviewsParams{MaxRows: int32(limit + 1)} //nolint:gosec // limit is at most maxReviewPage
	if cursor != "" {
		at, id, err := decodeReviewCursor(cursor)
		if err != nil {
			return nil, err
		}
		params.AfterCreatedAt = pgtype.Timestamptz{Time: at, Valid: true}
		params.AfterID = pgtype.UUID{Bytes: id, Valid: true}
	}
	var rows []store.ListKycReviewsRow
	err := s.pool.WithRoleTx(ctx, staffReviewRole, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		rows, err = store.New(tx).ListKycReviews(ctx, params)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("app: listing reviews: %w", err)
	}
	page := &KYCReviewPage{Reviews: []KYCReviewSummary{}}
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[limit-1]
		page.NextCursor = encodeReviewCursor(last.CreatedAt, last.ID)
	}
	for _, r := range rows {
		result := parseCheckResult(r.Result)
		page.Reviews = append(page.Reviews, KYCReviewSummary{
			CheckID: r.ID, TenantID: r.TenantID, AccountName: r.DisplayName, CountryCode: r.CountryCode,
			Tier: string(r.Tier), TenantStatus: r.TenantStatus, Kind: r.Kind, CreatedAt: r.CreatedAt,
			DuplicateRegistration: result.DuplicateRegistration, OtherAccounts: result.OtherAccounts,
		})
	}
	return page, nil
}

func encodeReviewCursor(at time.Time, id uuid.UUID) string {
	return base64.RawURLEncoding.EncodeToString([]byte(at.UTC().Format(time.RFC3339Nano) + "|" + id.String()))
}

func decodeReviewCursor(cursor string) (time.Time, uuid.UUID, error) {
	bad := fmt.Errorf("%w: malformed cursor", ErrValidation)
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, uuid.Nil, bad
	}
	at, id, ok := strings.Cut(string(raw), "|")
	if !ok {
		return time.Time{}, uuid.Nil, bad
	}
	t, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return time.Time{}, uuid.Nil, bad
	}
	u, err := uuid.Parse(id)
	if err != nil {
		return time.Time{}, uuid.Nil, bad
	}
	return t, u, nil
}

// checkTenant finds which account checkID belongs to, across accounts.
func (s *Service) checkTenant(ctx context.Context, checkID uuid.UUID) (uuid.UUID, error) {
	var tenantID uuid.UUID
	err := s.pool.WithRoleTx(ctx, staffReviewRole, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		tenantID, err = store.New(tx).FindKycCheckTenant(ctx, checkID)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrNotFound
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("app: finding the check: %w", err)
	}
	return tenantID, nil
}

// GetKYCReview returns a check in review with what the reviewer needs.
func (s *Service) GetKYCReview(ctx context.Context, staffID, checkID uuid.UUID) (*KYCReview, error) {
	if err := s.requireStaff(ctx, staffID); err != nil {
		return nil, err
	}
	tenantID, err := s.checkTenant(ctx, checkID)
	if err != nil {
		return nil, err
	}
	var (
		review  KYCReview
		subject uuid.UUID
	)
	err = s.pool.WithTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		check, err := q.GetKycCheckByID(ctx, store.GetKycCheckByIDParams{ID: checkID, TenantID: tenantID})
		if err != nil {
			return fmt.Errorf("app: loading the check: %w", err)
		}
		if check.Status != domain.KYCReview {
			return ErrAlreadyDecided
		}
		tenant, err := q.GetTenantByID(ctx, tenantID)
		if err != nil {
			return fmt.Errorf("app: loading the account: %w", err)
		}
		result := parseCheckResult(check.Result)
		review.Summary = KYCReviewSummary{
			CheckID: check.ID, TenantID: tenantID, AccountName: tenant.DisplayName, CountryCode: tenant.CountryCode,
			Tier: string(tenant.Tier), TenantStatus: tenant.Status, Kind: check.Kind, CreatedAt: check.CreatedAt,
			DuplicateRegistration: result.DuplicateRegistration, OtherAccounts: result.OtherAccounts,
		}
		review.Check = kycCheckFromRow(check)
		subject = check.SubjectID
		if check.Kind != domain.KYCKindBusiness {
			return nil
		}
		return loadBusinessEvidence(ctx, q, tenantID, result.DocumentIDs, &review)
	})
	if err != nil {
		return nil, err
	}
	if review.Check.Kind == domain.KYCKindIndividual {
		if users, err := s.identity.GetUsers(ctx, []uuid.UUID{subject}); err == nil && len(users) == 1 {
			review.SubjectEmail, review.SubjectName = users[0].Email, users[0].FullName
		}
	}
	return &review, nil
}

// loadBusinessEvidence adds the profile, its owners and the check's
// documents to review.
func loadBusinessEvidence(ctx context.Context, q *store.Queries, tenantID uuid.UUID, documentIDs []uuid.UUID, review *KYCReview) error {
	profile, err := q.GetBusinessProfileByTenant(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("app: loading the business profile: %w", err)
	}
	owners, err := q.ListBeneficialOwners(ctx, store.ListBeneficialOwnersParams{TenantID: tenantID, BusinessProfileID: profile.ID})
	if err != nil {
		return fmt.Errorf("app: listing owners: %w", err)
	}
	review.BusinessProfile = businessProfileFromRow(profile, owners)
	docs, err := q.ListDocuments(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("app: listing documents: %w", err)
	}
	review.Documents = []Document{}
	for _, d := range docs {
		if slices.Contains(documentIDs, d.ID) {
			review.Documents = append(review.Documents, documentFromRow(d))
		}
	}
	return nil
}

// DecideKYCReview records a staff decision on a check in review (task
// 2.9), in the check's account transaction with the check locked:
//   - passed, business: the profile is verified, its documents accepted,
//     and an active starter account moves to business;
//   - passed, individual: an active sandbox account moves to starter;
//   - failed: no tier change; the documents named are rejected, so the
//     client uploads them again before resubmitting.
//
// The check records the reviewer and the reason (shown to the client),
// and kyc.check_decided names the reviewer. A check no longer in
// review is ErrAlreadyDecided and changes nothing.
func (s *Service) DecideKYCReview(ctx context.Context, staffID, checkID uuid.UUID, d domain.StaffDecision) (*KYCCheck, error) {
	if err := s.requireStaff(ctx, staffID); err != nil {
		return nil, err
	}
	tenantID, err := s.checkTenant(ctx, checkID)
	if err != nil {
		return nil, err
	}
	var decided KYCCheck
	err = s.pool.WithTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		check, err := q.GetKycCheckForUpdate(ctx, store.GetKycCheckForUpdateParams{ID: checkID, TenantID: tenantID})
		if err != nil {
			return fmt.Errorf("app: locking the check: %w", err)
		}
		if check.Status != domain.KYCReview {
			return ErrAlreadyDecided
		}
		documentIDs := parseCheckResult(check.Result).DocumentIDs
		if err := domain.ValidateStaffDecision(d, documentIDs); err != nil {
			return err
		}
		extra, err := json.Marshal(map[string]any{"decision_reason": strings.TrimSpace(d.Reason)})
		if err != nil {
			return fmt.Errorf("app: encoding the decision: %w", err)
		}
		if err := q.DecideKycCheckByStaff(ctx, store.DecideKycCheckByStaffParams{
			Status: d.Decision, Extra: extra, ReviewedBy: pgtype.UUID{Bytes: staffID, Valid: true}, ID: checkID, TenantID: tenantID,
		}); err != nil {
			return fmt.Errorf("app: recording the decision: %w", err)
		}
		promoted, err := s.applyStaffDecision(ctx, tx, check, d, documentIDs, staffID)
		if err != nil {
			return err
		}
		if err := s.writeTenantEvent(ctx, tx, tenantID, "kyc.check_decided", map[string]any{
			"tenant_id": tenantID, "check_id": checkID, "kind": check.Kind, "status": d.Decision,
			"promoted": promoted, "reviewed_by": staffID,
		}); err != nil {
			return err
		}
		row, err := q.GetKycCheckByID(ctx, store.GetKycCheckByIDParams{ID: checkID, TenantID: tenantID})
		if err != nil {
			return fmt.Errorf("app: reloading the check: %w", err)
		}
		decided = kycCheckFromRow(row)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &decided, nil
}

// applyStaffDecision does what a decision means for the account, its
// profile and its documents, and reports whether the tier moved.
func (s *Service) applyStaffDecision(ctx context.Context, tx pgx.Tx, check store.AccountsKycCheck, d domain.StaffDecision, documentIDs []uuid.UUID, staffID uuid.UUID) (bool, error) {
	q := store.New(tx)
	if d.Decision == domain.KYCFailed {
		if len(d.RejectedDocumentIDs) == 0 {
			return false, nil
		}
		err := q.SetDocumentsStatus(ctx, store.SetDocumentsStatusParams{Status: domain.DocumentRejected, TenantID: check.TenantID, Ids: d.RejectedDocumentIDs})
		if err != nil {
			return false, fmt.Errorf("app: rejecting documents: %w", err)
		}
		return false, nil
	}

	from, to := domain.TierSandbox, domain.TierStarter
	if check.Kind == domain.KYCKindBusiness {
		from, to = domain.TierStarter, domain.TierBusiness
		if err := q.SetBusinessProfileVerified(ctx, store.SetBusinessProfileVerifiedParams{ID: check.SubjectID, TenantID: check.TenantID}); err != nil {
			return false, fmt.Errorf("app: verifying the business profile: %w", err)
		}
		if len(documentIDs) > 0 {
			err := q.SetDocumentsStatus(ctx, store.SetDocumentsStatusParams{Status: domain.DocumentAccepted, TenantID: check.TenantID, Ids: documentIDs})
			if err != nil {
				return false, fmt.Errorf("app: accepting documents: %w", err)
			}
		}
	}
	tenant, err := q.GetTenantForUpdate(ctx, check.TenantID)
	if err != nil {
		return false, fmt.Errorf("app: locking tenant: %w", err)
	}
	if domain.Tier(tenant.Tier) != from || tenant.Status != domain.TenantActive {
		return false, nil // recorded without a promotion, as the webhook does (2.6)
	}
	if err := s.changeTierTx(ctx, tx, ChangeTierRequest{
		TenantID: check.TenantID, To: to, Reason: strings.TrimSpace(d.Reason), KYCCheckID: &check.ID, ChangedBy: &staffID,
	}); err != nil {
		return false, err
	}
	return true, nil
}

// StaffChangeTier changes an account's tier as staff (ChangeTier with
// the staff member as ChangedBy), and returns the account.
func (s *Service) StaffChangeTier(ctx context.Context, staffID, tenantID uuid.UUID, to domain.Tier, reason string, checkID *uuid.UUID) (*Tenant, error) {
	if err := s.requireStaff(ctx, staffID); err != nil {
		return nil, err
	}
	if err := s.ChangeTier(ctx, ChangeTierRequest{TenantID: tenantID, To: to, Reason: reason, KYCCheckID: checkID, ChangedBy: &staffID}); err != nil {
		return nil, err
	}
	return s.tenantForStaff(ctx, tenantID)
}

// StaffMakeReseller makes an account a reseller as staff, and returns it.
func (s *Service) StaffMakeReseller(ctx context.Context, staffID, tenantID uuid.UUID, reason string) (*Tenant, error) {
	if err := s.requireStaff(ctx, staffID); err != nil {
		return nil, err
	}
	if err := s.MakeReseller(ctx, MakeResellerRequest{TenantID: tenantID, ChangedBy: staffID, Reason: reason}); err != nil {
		return nil, err
	}
	return s.tenantForStaff(ctx, tenantID)
}

// StaffDownloadDocument decrypts an account's document for a reviewer.
func (s *Service) StaffDownloadDocument(ctx context.Context, staffID, tenantID, documentID uuid.UUID) ([]byte, string, error) {
	if err := s.requireStaff(ctx, staffID); err != nil {
		return nil, "", err
	}
	return s.readDocument(ctx, tenantID, documentID)
}

func (s *Service) tenantForStaff(ctx context.Context, tenantID uuid.UUID) (*Tenant, error) {
	var tenant *Tenant
	err := s.pool.WithTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		row, err := store.New(tx).GetTenantByID(ctx, tenantID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("app: loading the account: %w", err)
		}
		tenant = tenantFromRow(row)
		return nil
	})
	return tenant, err
}
