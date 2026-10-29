package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	vascrypto "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/crypto"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/ids"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/domain"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/store"
)

// secretRotationOverlap is how long a rotated-out secret keeps
// working after a new one is issued (data model: "the rotation
// overlap ends at expires_at") — long enough for an integrator to
// update a deployed config.
const secretRotationOverlap = 7 * 24 * time.Hour

// maxActiveApiClientSecrets is the cap data model §3's own comment
// names ("Up to two active secrets per API client: the service
// enforces this").
const maxActiveApiClientSecrets = 2

// ApiClientSecretSummary is one of a client's active secrets, as
// shown to its owner — never secret_hash.
type ApiClientSecretSummary struct {
	ID        uuid.UUID
	Last4     string
	CreatedAt time.Time
	ExpiresAt *time.Time
}

// ApiClientSummary is an API client as shown to its owner.
type ApiClientSummary struct {
	ID               uuid.UUID
	Name             string
	Environment      string
	Scopes           []string
	IPAllowlist      []string
	ExpiresAt        *time.Time
	SpendCapMinor    *int64
	SpendCapCurrency *string
	CreatedAt        time.Time
	RevokedAt        *time.Time
}

// ApiClientDetail is a summary plus its active secrets' metadata.
type ApiClientDetail struct {
	ApiClientSummary
	Secrets []ApiClientSecretSummary
}

// ApiClientSecretResult is CreateApiClient's/RotateApiClientSecret's
// result — Secret is the raw value, shown exactly once.
type ApiClientSecretResult struct {
	ID        uuid.UUID
	Secret    string
	Last4     string
	ExpiresAt *time.Time
}

// ApiClientWithSecret is CreateApiClient's result: the new client
// plus its first secret.
type ApiClientWithSecret struct {
	ApiClientSummary
	Secret ApiClientSecretResult
}

func apiClientSummary(row store.IdentityApiClient) ApiClientSummary {
	summary := ApiClientSummary{
		ID:          row.ID,
		Name:        row.Name,
		Environment: string(row.Environment),
		Scopes:      row.Scopes,
		CreatedAt:   row.CreatedAt,
	}
	summary.IPAllowlist = make([]string, len(row.IpAllowlist))
	for i, p := range row.IpAllowlist {
		summary.IPAllowlist[i] = p.String()
	}
	if row.ExpiresAt.Valid {
		t := row.ExpiresAt.Time
		summary.ExpiresAt = &t
	}
	if row.RevokedAt.Valid {
		t := row.RevokedAt.Time
		summary.RevokedAt = &t
	}
	if row.SpendCapMinor.Valid {
		v := row.SpendCapMinor.Int64
		summary.SpendCapMinor = &v
	}
	if row.SpendCapCurrency.Valid {
		v := row.SpendCapCurrency.String
		summary.SpendCapCurrency = &v
	}
	return summary
}

func apiClientSecretSummary(row store.IdentityApiClientSecret) ApiClientSecretSummary {
	summary := ApiClientSecretSummary{ID: row.ID, Last4: row.Last4, CreatedAt: row.CreatedAt}
	if row.ExpiresAt.Valid {
		t := row.ExpiresAt.Time
		summary.ExpiresAt = &t
	}
	return summary
}

func toTimestamptz(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: *t, Valid: true}
}

// CreateApiClient creates a new API client under tenantID (this
// phase's placeholder: the creating user's own ID — accounts/real
// tenants don't exist until Phase 2 task 2.2) plus its first secret.
// The secret is returned exactly once; only its Argon2id hash is
// stored.
func (s *Service) CreateApiClient(
	ctx context.Context,
	userID uuid.UUID,
	name, environment string,
	scopes, ipAllowlist []string,
	expiresAt *time.Time,
	spendCapMinor *int64,
	spendCapCurrency *string,
) (*ApiClientWithSecret, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("%w: name is required", ErrValidation)
	}
	env := store.IdentityEnvironment(environment)
	if env != store.IdentityEnvironmentSandbox && env != store.IdentityEnvironmentLive {
		return nil, fmt.Errorf("%w: environment must be \"sandbox\" or \"live\"", ErrValidation)
	}
	prefixes, err := domain.ParseIPAllowlist(ipAllowlist)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrValidation, err)
	}

	secret, last4, err := domain.GenerateApiClientSecret(string(env))
	if err != nil {
		return nil, fmt.Errorf("app: generating api client secret: %w", err)
	}
	secretHash, err := vascrypto.Argon2Hash(secret)
	if err != nil {
		return nil, fmt.Errorf("app: hashing api client secret: %w", err)
	}

	tenantID := userID // this phase's placeholder — see apiclients.go's own doc comment

	var result *ApiClientWithSecret

	err = s.pool.WithTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)

		clientID, err := ids.New()
		if err != nil {
			return fmt.Errorf("app: generating api client id: %w", err)
		}
		client, err := q.CreateApiClient(ctx, store.CreateApiClientParams{
			ID:               clientID,
			TenantID:         tenantID,
			Name:             name,
			Environment:      env,
			Scopes:           scopes,
			IpAllowlist:      prefixes,
			ExpiresAt:        toTimestamptz(expiresAt),
			CreatedBy:        userID,
			SpendCapMinor:    pgtype.Int8{Int64: derefInt64(spendCapMinor), Valid: spendCapMinor != nil},
			SpendCapCurrency: pgtype.Text{String: derefString(spendCapCurrency), Valid: spendCapCurrency != nil},
		})
		if err != nil {
			return fmt.Errorf("app: creating api client: %w", err)
		}

		secretID, err := ids.New()
		if err != nil {
			return fmt.Errorf("app: generating api client secret id: %w", err)
		}
		secretRow, err := q.CreateApiClientSecret(ctx, store.CreateApiClientSecretParams{
			ID:          secretID,
			ApiClientID: clientID,
			SecretHash:  secretHash,
			Last4:       last4,
		})
		if err != nil {
			return fmt.Errorf("app: storing api client secret: %w", err)
		}

		if err := recordSecurityEvent(ctx, q, userID, "api_client_created", nil, map[string]any{
			"api_client_id": clientID.String(), "name": name, "environment": string(env),
		}); err != nil {
			return err
		}

		result = &ApiClientWithSecret{
			ApiClientSummary: apiClientSummary(client),
			Secret:           ApiClientSecretResult{ID: secretRow.ID, Secret: secret, Last4: last4},
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// ListApiClients returns tenantID's API clients, newest first. Reads
// against api_clients need app.tenant_id set just as much as writes
// do — it's FORCE ROW LEVEL SECURITY, so a plain query with no tenant
// transaction silently matches zero rows regardless of any WHERE
// clause, rather than erroring. WithTenantTx for a read-only closure
// just commits a no-op; that's fine.
func (s *Service) ListApiClients(ctx context.Context, tenantID uuid.UUID) ([]ApiClientSummary, error) {
	var summaries []ApiClientSummary

	err := s.pool.WithTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := store.New(tx).ListApiClientsByTenant(ctx, tenantID)
		if err != nil {
			return fmt.Errorf("app: listing api clients: %w", err)
		}
		summaries = make([]ApiClientSummary, len(rows))
		for i, row := range rows {
			summaries[i] = apiClientSummary(row)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return summaries, nil
}

// GetApiClient returns one of tenantID's clients, including its
// active secrets' metadata (never secret_hash).
func (s *Service) GetApiClient(ctx context.Context, tenantID, clientID uuid.UUID) (*ApiClientDetail, error) {
	var (
		detail   *ApiClientDetail
		notFound bool
	)

	err := s.pool.WithTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)

		client, err := q.GetApiClientByID(ctx, store.GetApiClientByIDParams{ID: clientID, TenantID: tenantID})
		if errors.Is(err, pgx.ErrNoRows) {
			notFound = true
			return nil
		}
		if err != nil {
			return fmt.Errorf("app: looking up api client: %w", err)
		}

		secretRows, err := q.ListActiveApiClientSecretsByClientID(ctx, store.ListActiveApiClientSecretsByClientIDParams{
			ApiClientID: clientID,
			TenantID:    tenantID,
			Now:         s.clk.Now(),
		})
		if err != nil {
			return fmt.Errorf("app: listing api client secrets: %w", err)
		}
		secrets := make([]ApiClientSecretSummary, len(secretRows))
		for i, row := range secretRows {
			secrets[i] = apiClientSecretSummary(row)
		}

		detail = &ApiClientDetail{ApiClientSummary: apiClientSummary(client), Secrets: secrets}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if notFound {
		return nil, ErrNotFound
	}
	return detail, nil
}

// RotateApiClientSecret issues a new secret for clientID and starts
// the rotation overlap on the previous one (its expires_at moves to
// now+7 days, rather than revoking it immediately). Rejects with
// ErrValidation if the client already has 2 secrets that are neither
// revoked nor past their own expiry — data model's "the service
// enforces this" reads as a hard cap, not a silent evict-the-oldest;
// the caller must revoke one first. The precondition check and the
// mutation share one transaction, closing the race a separate
// check-then-act pair would leave open.
func (s *Service) RotateApiClientSecret(ctx context.Context, tenantID, clientID uuid.UUID) (*ApiClientSecretResult, error) {
	var (
		result   *ApiClientSecretResult
		notFound bool
		tooMany  bool
	)

	err := s.pool.WithTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)

		client, err := q.GetApiClientByID(ctx, store.GetApiClientByIDParams{ID: clientID, TenantID: tenantID})
		if errors.Is(err, pgx.ErrNoRows) {
			notFound = true
			return nil
		}
		if err != nil {
			return fmt.Errorf("app: looking up api client: %w", err)
		}

		active, err := q.ListActiveApiClientSecretsByClientID(ctx, store.ListActiveApiClientSecretsByClientIDParams{
			ApiClientID: clientID,
			TenantID:    tenantID,
			Now:         s.clk.Now(),
		})
		if err != nil {
			return fmt.Errorf("app: listing api client secrets: %w", err)
		}
		if len(active) >= maxActiveApiClientSecrets {
			tooMany = true
			return nil
		}

		secret, last4, err := domain.GenerateApiClientSecret(string(client.Environment))
		if err != nil {
			return fmt.Errorf("app: generating api client secret: %w", err)
		}
		secretHash, err := vascrypto.Argon2Hash(secret)
		if err != nil {
			return fmt.Errorf("app: hashing api client secret: %w", err)
		}

		overlapEnd := s.clk.Now().Add(secretRotationOverlap)
		for _, old := range active {
			if err := q.ExpireApiClientSecretForRotation(ctx, store.ExpireApiClientSecretForRotationParams{
				ID:        old.ID,
				TenantID:  tenantID,
				ExpiresAt: overlapEnd,
			}); err != nil {
				return fmt.Errorf("app: starting rotation overlap: %w", err)
			}
		}

		secretID, err := ids.New()
		if err != nil {
			return fmt.Errorf("app: generating api client secret id: %w", err)
		}
		secretRow, err := q.CreateApiClientSecret(ctx, store.CreateApiClientSecretParams{
			ID:          secretID,
			ApiClientID: clientID,
			SecretHash:  secretHash,
			Last4:       last4,
		})
		if err != nil {
			return fmt.Errorf("app: storing rotated api client secret: %w", err)
		}

		if err := recordSecurityEvent(ctx, q, tenantID, "api_client_secret_rotated", nil, map[string]any{
			"api_client_id": clientID.String(), "secret_id": secretRow.ID.String(),
		}); err != nil {
			return err
		}

		result = &ApiClientSecretResult{ID: secretRow.ID, Secret: secret, Last4: last4}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if notFound {
		return nil, ErrNotFound
	}
	if tooMany {
		return nil, fmt.Errorf("%w: this client already has %d active secrets; revoke one before rotating", ErrValidation, maxActiveApiClientSecrets)
	}
	return result, nil
}

// RevokeApiClient revokes clientID as a whole — every secret it has
// stops working regardless of each secret's own state (task 1.8's
// job to check both when it builds the real token exchange).
func (s *Service) RevokeApiClient(ctx context.Context, tenantID, clientID uuid.UUID) error {
	var n int64
	err := s.pool.WithTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		var err error
		n, err = q.RevokeApiClient(ctx, store.RevokeApiClientParams{ID: clientID, TenantID: tenantID})
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
		return recordSecurityEvent(ctx, q, tenantID, "api_client_revoked", nil, map[string]any{
			"api_client_id": clientID.String(),
		})
	})
	if err != nil {
		return fmt.Errorf("app: revoking api client: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// RevokeApiClientSecret revokes one secret of clientID without
// touching the client or its other secrets.
func (s *Service) RevokeApiClientSecret(ctx context.Context, tenantID, clientID, secretID uuid.UUID) error {
	var n int64
	err := s.pool.WithTenantTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		n, err = store.New(tx).RevokeApiClientSecret(ctx, store.RevokeApiClientSecretParams{
			ID:          secretID,
			ApiClientID: clientID,
			TenantID:    tenantID,
		})
		return err
	})
	if err != nil {
		return fmt.Errorf("app: revoking api client secret: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func derefInt64(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

func derefString(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}
