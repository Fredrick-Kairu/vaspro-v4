package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	vascrypto "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/crypto"
	platformevents "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/events"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/ids"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/outbox"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments/adapters"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments/store"
)

// darajaCredentialKinds are the credentials a Daraja paybill needs:
// the consumer key and secret, and the STK passkey.
var darajaCredentialKinds = []string{"app_key", "app_secret", "passkey"}

// RailAccount is a rail account as the ops CLI sees it.
type RailAccount struct {
	ID          uuid.UUID
	RailCode    string
	AccountType string
	Number      string
	DisplayName string
	Status      string
}

// AddRailAccountRequest adds one of VASPRO's own top-up paybills.
type AddRailAccountRequest struct {
	StaffEmail  string
	RailCode    string
	Number      string
	DisplayName string
}

// SetRailCredentialRequest stores one credential of a rail account.
type SetRailCredentialRequest struct {
	StaffEmail    string
	RailAccountID uuid.UUID
	Kind          string
	Secret        []byte
}

// requireStaff finds the staff member with email (task 4.3, OQ-22):
// anyone else is ErrNotStaff; identity being down fails closed.
func (s *Service) requireStaff(ctx context.Context, email string) (uuid.UUID, error) {
	if s.identity == nil {
		return uuid.Nil, errors.New("app: no identity adapter for staff checks")
	}
	id, err := s.identity.StaffByEmail(ctx, email)
	if errors.Is(err, adapters.ErrNotStaff) {
		return uuid.Nil, ErrNotStaff
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("app: checking staff: %w", err)
	}
	return id, nil
}

// AddRailAccount adds a VASPRO top-up paybill (ops CLI, task 4.3),
// pending until its credentials are set and verified.
func (s *Service) AddRailAccount(ctx context.Context, req AddRailAccountRequest) (RailAccount, error) {
	staff, err := s.requireStaff(ctx, req.StaffEmail)
	if err != nil {
		return RailAccount{}, err
	}
	number, name := strings.TrimSpace(req.Number), strings.TrimSpace(req.DisplayName)
	if number == "" || name == "" {
		return RailAccount{}, fmt.Errorf("%w: a number and a display name are required", ErrValidation)
	}
	id, err := ids.New()
	if err != nil {
		return RailAccount{}, err
	}
	var out RailAccount
	err = s.pool.WithRoleTx(ctx, systemRole, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		rail, err := q.GetRail(ctx, req.RailCode)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && rail.Status == "retired") {
			return fmt.Errorf("%w: unknown rail %q", ErrValidation, req.RailCode)
		}
		if err != nil {
			return fmt.Errorf("app: loading rail: %w", err)
		}
		row, err := q.CreateRailAccount(ctx, store.CreateRailAccountParams{
			ID: id, RailCode: rail.Code, AccountType: "paybill", Number: number, DisplayName: name,
			Uses: []string{"vaspro_topups"}, Status: "pending_verification",
			CreatedBy: pgtype.UUID{Bytes: staff, Valid: true},
		})
		if err != nil {
			return fmt.Errorf("app: adding the paybill: %w", err)
		}
		out = railAccountOf(row)
		return nil
	})
	return out, err
}

func railAccountOf(r store.PaymentsRailAccount) RailAccount {
	return RailAccount{ID: r.ID, RailCode: r.RailCode, AccountType: r.AccountType, Number: r.Number, DisplayName: r.DisplayName, Status: r.Status}
}

// SetRailCredential stores one of a VASPRO paybill's credentials
// (ops CLI, task 4.3): sealed under CredentialsKMSKeyID, with a keyed
// fingerprint, retiring the one it replaces. The account goes back to
// pending until it's verified with the new credentials.
func (s *Service) SetRailCredential(ctx context.Context, req SetRailCredentialRequest) error {
	staff, err := s.requireStaff(ctx, req.StaffEmail)
	if err != nil {
		return err
	}
	if !slices.Contains(darajaCredentialKinds, req.Kind) {
		return fmt.Errorf("%w: kind must be one of %v", ErrValidation, darajaCredentialKinds)
	}
	if len(req.Secret) == 0 {
		return fmt.Errorf("%w: the credential is empty", ErrValidation)
	}
	if s.kms == nil || len(s.fpKey) == 0 {
		return errors.New("app: no KMS or fingerprint key for credentials")
	}
	sealed, err := vascrypto.NewEnvelope(s.kms).Seal(ctx, CredentialsKMSKeyID, req.Secret)
	if err != nil {
		return fmt.Errorf("app: sealing the credential: %w", err)
	}
	fingerprint := vascrypto.HMACSign(s.fpKey, req.Secret)
	id, err := ids.New()
	if err != nil {
		return err
	}
	return s.pool.WithRoleTx(ctx, systemRole, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		acct, err := q.GetRailAccountForUpdate(ctx, req.RailAccountID)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: no rail account %s", ErrNotFound, req.RailAccountID)
		}
		if err != nil {
			return fmt.Errorf("app: loading the rail account: %w", err)
		}
		if _, err := q.RetireActiveRailCredential(ctx, store.RetireActiveRailCredentialParams{
			RailAccountID: acct.ID, Kind: req.Kind, RetiredAt: pgtype.Timestamptz{Time: s.clk.Now(), Valid: true},
		}); err != nil {
			return fmt.Errorf("app: retiring the old credential: %w", err)
		}
		if _, err := q.CreateRailCredential(ctx, store.CreateRailCredentialParams{
			ID: id, RailAccountID: acct.ID, Kind: req.Kind, Ciphertext: sealed,
			KmsKeyID: CredentialsKMSKeyID, Fingerprint: fingerprint, CreatedBy: staff,
		}); err != nil {
			return fmt.Errorf("app: storing the credential: %w", err)
		}
		if _, err := q.SetRailAccountPending(ctx, acct.ID); err != nil {
			return fmt.Errorf("app: returning the account to pending: %w", err)
		}
		return nil
	})
}

// VerifyRailAccount makes a VASPRO paybill active once a test call with
// its stored credentials works (ops CLI, task 4.3; data model §7). A
// refused or failed call leaves it pending (ErrVerificationFailed). If
// the credentials change while the call is in flight, nothing changes.
func (s *Service) VerifyRailAccount(ctx context.Context, staffEmail string, railAccountID uuid.UUID) error {
	staff, err := s.requireStaff(ctx, staffEmail)
	if err != nil {
		return err
	}
	credIDs, err := s.activeCredentialIDs(ctx, railAccountID)
	if err != nil {
		return err
	}
	if s.rail == nil {
		return errors.New("app: no rail adapter to verify with")
	}
	if err := s.rail.CheckCredentials(ctx, railAccountID); err != nil {
		return fmt.Errorf("%w: %w", ErrVerificationFailed, err)
	}
	return s.pool.WithRoleTx(ctx, systemRole, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		if _, err := q.GetRailAccountForUpdate(ctx, railAccountID); err != nil {
			return fmt.Errorf("app: locking the rail account: %w", err)
		}
		now, err := activeIDs(ctx, q, railAccountID)
		if err != nil {
			return err
		}
		if !slices.Equal(now, credIDs) {
			return fmt.Errorf("%w: the credentials changed during verification", ErrVerificationFailed)
		}
		at := s.clk.Now()
		row, err := q.SetRailAccountVerified(ctx, store.SetRailAccountVerifiedParams{ID: railAccountID, VerifiedAt: pgtype.Timestamptz{Time: at, Valid: true}})
		if err != nil {
			return fmt.Errorf("app: activating the rail account: %w", err)
		}
		return writeRailAccountVerified(ctx, tx, row, staff, at)
	})
}

// activeCredentialIDs checks the account exists and has every Daraja
// credential, and returns their IDs.
func (s *Service) activeCredentialIDs(ctx context.Context, railAccountID uuid.UUID) ([]uuid.UUID, error) {
	var out []uuid.UUID
	err := s.pool.WithRoleTx(ctx, systemRole, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		if _, err := q.GetRailAccount(ctx, railAccountID); errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: no rail account %s", ErrNotFound, railAccountID)
		} else if err != nil {
			return fmt.Errorf("app: loading the rail account: %w", err)
		}
		var err error
		out, err = activeIDs(ctx, q, railAccountID)
		return err
	})
	return out, err
}

// activeIDs returns the account's active credential IDs, in kind order,
// and refuses an account missing any Daraja credential.
func activeIDs(ctx context.Context, q *store.Queries, railAccountID uuid.UUID) ([]uuid.UUID, error) {
	creds, err := q.ListActiveRailCredentials(ctx, railAccountID)
	if err != nil {
		return nil, fmt.Errorf("app: loading credentials: %w", err)
	}
	have := map[string]bool{}
	out := make([]uuid.UUID, len(creds))
	for i, c := range creds {
		have[c.Kind] = true
		out[i] = c.ID
	}
	for _, k := range darajaCredentialKinds {
		if !have[k] {
			return nil, fmt.Errorf("%w: the account has no %s yet", ErrValidation, k)
		}
	}
	return out, nil
}

func writeRailAccountVerified(ctx context.Context, tx pgx.Tx, row store.PaymentsRailAccount, staff uuid.UUID, at time.Time) error {
	eventID, err := ids.New()
	if err != nil {
		return err
	}
	data := map[string]any{
		"rail_account_id": row.ID, "rail_code": row.RailCode, "account_type": row.AccountType,
		"number": row.Number, "uses": row.Uses, "verified_by": staff, "verified_at": at,
	}
	tenant := uuid.Nil
	if row.TenantID.Valid {
		tenant = uuid.UUID(row.TenantID.Bytes)
		data["tenant_id"] = tenant
	}
	env, err := platformevents.New(eventID, "rail_account.verified", 1, tenant, at, "", data)
	if err != nil {
		return fmt.Errorf("app: building rail_account.verified: %w", err)
	}
	return outbox.Write(ctx, tx, "rail_account", row.ID, env)
}
