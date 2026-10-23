package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	vascrypto "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/crypto"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/ids"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/domain"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/store"
)

const (
	smppAccountStatusActive    = "active"
	smppAccountStatusSuspended = "suspended"
)

// SMPPAccountSummary is an SMPP account as shown to its owner — never
// password_hash.
type SMPPAccountSummary struct {
	ID          uuid.UUID
	SystemID    string
	IPAllowlist []string
	MaxBinds    int16
	MaxTPS      int32
	Environment string
	Status      string
}

// SMPPAccountWithPassword is CreateSMPPAccount's/RotateSMPPAccountPassword's
// result — Password is the raw value, shown exactly once.
type SMPPAccountWithPassword struct {
	SMPPAccountSummary
	Password string
}

func smppAccountSummary(row store.IdentitySmppAccount) SMPPAccountSummary {
	summary := SMPPAccountSummary{
		ID:          row.ID,
		SystemID:    row.SystemID,
		MaxBinds:    row.MaxBinds,
		MaxTPS:      row.MaxTps,
		Environment: string(row.Environment),
		Status:      row.Status,
	}
	summary.IPAllowlist = make([]string, len(row.IpAllowlist))
	for i, p := range row.IpAllowlist {
		summary.IPAllowlist[i] = p.String()
	}
	return summary
}

// CreateSMPPAccount creates a new SMPP account under tenantID (this
// phase's placeholder: the creating user's own ID — same reasoning as
// CreateApiClient, apiclients.go's own doc comment) plus its first
// password. The password is returned exactly once; only its Argon2id
// hash is stored. ipAllowlist must be non-empty: data model §3's own
// comment says SMPP accounts always require one, since the protocol's
// 8-character password ceiling leaves little else to lean on.
func (s *Service) CreateSMPPAccount(
	ctx context.Context,
	userID uuid.UUID,
	systemID, environment string,
	ipAllowlist []string,
	maxBinds int16,
	maxTPS int32,
) (*SMPPAccountWithPassword, error) {
	systemID = strings.TrimSpace(systemID)
	if systemID == "" {
		return nil, fmt.Errorf("%w: system_id is required", ErrValidation)
	}
	env := store.IdentityEnvironment(environment)
	if env != store.IdentityEnvironmentSandbox && env != store.IdentityEnvironmentLive {
		return nil, fmt.Errorf("%w: environment must be \"sandbox\" or \"live\"", ErrValidation)
	}
	if len(ipAllowlist) == 0 {
		return nil, fmt.Errorf("%w: ip_allowlist is required for an SMPP account", ErrValidation)
	}
	prefixes, err := domain.ParseIPAllowlist(ipAllowlist)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	if maxBinds <= 0 {
		return nil, fmt.Errorf("%w: max_binds must be positive", ErrValidation)
	}
	if maxTPS <= 0 {
		return nil, fmt.Errorf("%w: max_tps must be positive", ErrValidation)
	}

	password, err := domain.GenerateSMPPAccountPassword()
	if err != nil {
		return nil, fmt.Errorf("app: generating smpp account password: %w", err)
	}
	passwordHash, err := vascrypto.Argon2Hash(password)
	if err != nil {
		return nil, fmt.Errorf("app: hashing smpp account password: %w", err)
	}

	tenantID := userID // this phase's placeholder — see CreateApiClient's own doc comment

	var result *SMPPAccountWithPassword

	err = s.pool.WithTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		accountID, err := ids.New()
		if err != nil {
			return fmt.Errorf("app: generating smpp account id: %w", err)
		}
		account, err := q.CreateSMPPAccount(ctx, store.CreateSMPPAccountParams{
			ID:           accountID,
			TenantID:     tenantID,
			SystemID:     systemID,
			PasswordHash: passwordHash,
			IpAllowlist:  prefixes,
			MaxBinds:     maxBinds,
			MaxTps:       maxTPS,
			Environment:  env,
			Status:       smppAccountStatusActive,
		})
		if err != nil {
			return fmt.Errorf("app: creating smpp account: %w", err)
		}
		result = &SMPPAccountWithPassword{SMPPAccountSummary: smppAccountSummary(account), Password: password}
		return nil
	})
	if err != nil {
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("%w: system_id is already in use", ErrValidation)
		}
		return nil, err
	}
	return result, nil
}

// ListSMPPAccounts returns tenantID's SMPP accounts, newest first (by
// id — see ListSMPPAccountsByTenant's own comment on why).
func (s *Service) ListSMPPAccounts(ctx context.Context, tenantID uuid.UUID) ([]SMPPAccountSummary, error) {
	var summaries []SMPPAccountSummary

	err := s.pool.WithTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := store.New(tx).ListSMPPAccountsByTenant(ctx, tenantID)
		if err != nil {
			return fmt.Errorf("app: listing smpp accounts: %w", err)
		}
		summaries = make([]SMPPAccountSummary, len(rows))
		for i, row := range rows {
			summaries[i] = smppAccountSummary(row)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return summaries, nil
}

// GetSMPPAccount returns one of tenantID's SMPP accounts.
func (s *Service) GetSMPPAccount(ctx context.Context, tenantID, accountID uuid.UUID) (*SMPPAccountSummary, error) {
	var (
		summary  *SMPPAccountSummary
		notFound bool
	)

	err := s.pool.WithTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		row, err := store.New(tx).GetSMPPAccountByID(ctx, store.GetSMPPAccountByIDParams{ID: accountID, TenantID: tenantID})
		if errors.Is(err, pgx.ErrNoRows) {
			notFound = true
			return nil
		}
		if err != nil {
			return fmt.Errorf("app: looking up smpp account: %w", err)
		}
		s := smppAccountSummary(row)
		summary = &s
		return nil
	})
	if err != nil {
		return nil, err
	}
	if notFound {
		return nil, ErrNotFound
	}
	return summary, nil
}

// RotateSMPPAccountPassword issues a fresh password for accountID,
// overwriting the previous one outright — unlike API client secrets
// (1.7), smpp_accounts has a single password_hash column, not a
// rotation-overlap table, so there's no "two active at once" to manage.
func (s *Service) RotateSMPPAccountPassword(ctx context.Context, tenantID, accountID uuid.UUID) (*SMPPAccountWithPassword, error) {
	var (
		result   *SMPPAccountWithPassword
		notFound bool
	)

	err := s.pool.WithTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)

		account, err := q.GetSMPPAccountByID(ctx, store.GetSMPPAccountByIDParams{ID: accountID, TenantID: tenantID})
		if errors.Is(err, pgx.ErrNoRows) {
			notFound = true
			return nil
		}
		if err != nil {
			return fmt.Errorf("app: looking up smpp account: %w", err)
		}

		password, err := domain.GenerateSMPPAccountPassword()
		if err != nil {
			return fmt.Errorf("app: generating smpp account password: %w", err)
		}
		passwordHash, err := vascrypto.Argon2Hash(password)
		if err != nil {
			return fmt.Errorf("app: hashing smpp account password: %w", err)
		}

		n, err := q.UpdateSMPPAccountPassword(ctx, store.UpdateSMPPAccountPasswordParams{
			ID:           accountID,
			TenantID:     tenantID,
			PasswordHash: passwordHash,
		})
		if err != nil {
			return fmt.Errorf("app: rotating smpp account password: %w", err)
		}
		if n == 0 {
			notFound = true
			return nil
		}

		account.PasswordHash = passwordHash
		result = &SMPPAccountWithPassword{SMPPAccountSummary: smppAccountSummary(account), Password: password}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if notFound {
		return nil, ErrNotFound
	}
	return result, nil
}

// setSMPPAccountStatus is SuspendSMPPAccount's/ReactivateSMPPAccount's
// shared implementation.
func (s *Service) setSMPPAccountStatus(ctx context.Context, tenantID, accountID uuid.UUID, status string) error {
	var n int64
	err := s.pool.WithTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		n, err = store.New(tx).UpdateSMPPAccountStatus(ctx, store.UpdateSMPPAccountStatusParams{
			ID:       accountID,
			TenantID: tenantID,
			Status:   status,
		})
		return err
	})
	if err != nil {
		return fmt.Errorf("app: updating smpp account status: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SuspendSMPPAccount stops accountID's binds without losing its
// configuration — the schema's only states are "active" and
// "suspended" (data model §3's CHECK constraint), so there's no
// permanent revoke to model here.
func (s *Service) SuspendSMPPAccount(ctx context.Context, tenantID, accountID uuid.UUID) error {
	return s.setSMPPAccountStatus(ctx, tenantID, accountID, smppAccountStatusSuspended)
}

// ReactivateSMPPAccount reverses SuspendSMPPAccount.
func (s *Service) ReactivateSMPPAccount(ctx context.Context, tenantID, accountID uuid.UUID) error {
	return s.setSMPPAccountStatus(ctx, tenantID, accountID, smppAccountStatusActive)
}

// isUniqueViolation reports whether err is a Postgres unique-constraint
// violation (23505) — smpp_accounts.system_id is globally UNIQUE (data
// model §3), so two tenants racing to claim the same system_id is a
// real possibility, not just a same-tenant mistake.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
