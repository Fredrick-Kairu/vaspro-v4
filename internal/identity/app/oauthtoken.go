package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/lestrrat-go/jwx/v3/jwa"
	"github.com/lestrrat-go/jwx/v3/jwk"
	"github.com/lestrrat-go/jwx/v3/jwt"

	vascrypto "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/crypto"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/ids"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/domain"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/store"
)

// identityTokenExchangeRole is the narrow, SELECT-only Postgres role
// (migration 20261022223744) the token endpoint reads api_clients/
// api_client_secrets through — never a connection credential, reached
// only via db.Pool.WithRoleTx for this one lookup transaction.
const identityTokenExchangeRole = "identity_token_exchange"

// tokenTTL matches system design §3: "machines exchange a hashed
// secret for a token that expires within an hour."
const tokenTTL = time.Hour

const oauthIssuer = "vaspro-identity"

// oauthAudience is the aud claim every token carries: the gateway
// (internal/gateway, its own duplicated oauthAudience) rejects a token
// for any other audience.
const oauthAudience = "vaspro-api"

// Token is a successfully issued OAuth2 client-credentials access
// token (RFC 6749 §4.4.3).
type Token struct {
	AccessToken string
	TokenType   string
	ExpiresIn   int
	Scope       string
}

// ExchangeClientCredentials is the token endpoint's whole job: verify
// clientID/clientSecret, check the client isn't revoked/expired,
// narrow the requested scope, and sign a JWT. Read-only — no write,
// no audit event, no lockout counter, consistent with task 1.7's own
// "nothing enforces scopes/IP/spend-cap yet" boundary (no gateway, no
// billing). ErrBadCredentials covers an unknown client, a wrong
// secret, and a revoked/expired client alike, same anti-enumeration
// principle 1.3 already used for human sign-in; api/'s oauth handler
// renders it as RFC 6749's invalid_client rather than AUTH-4010's
// usual problem+json.
func (s *Service) ExchangeClientCredentials(ctx context.Context, clientID, clientSecret, requestedScope string) (*Token, error) {
	parsedClientID, err := uuid.Parse(clientID)
	if err != nil {
		return nil, ErrBadCredentials
	}

	var (
		client  store.IdentityApiClient
		secrets []store.IdentityApiClientSecret
	)
	err = s.pool.WithRoleTx(ctx, identityTokenExchangeRole, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)

		var err error
		client, err = q.GetApiClientByIDAnyTenant(ctx, parsedClientID)
		if err != nil {
			return err
		}

		secrets, err = q.ListActiveApiClientSecretsByClientIDAnyTenant(ctx, store.ListActiveApiClientSecretsByClientIDAnyTenantParams{
			ApiClientID: parsedClientID,
			Now:         s.clk.Now(),
		})
		return err
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrBadCredentials
		}
		return nil, fmt.Errorf("app: looking up api client for token exchange: %w", err)
	}

	if client.RevokedAt.Valid {
		return nil, ErrBadCredentials
	}
	if client.ExpiresAt.Valid && !client.ExpiresAt.Time.After(s.clk.Now()) {
		return nil, ErrBadCredentials
	}

	matched := false
	for _, secret := range secrets {
		ok, err := vascrypto.Argon2Verify(secret.SecretHash, clientSecret)
		if err != nil {
			return nil, fmt.Errorf("app: verifying client secret: %w", err)
		}
		if ok {
			matched = true
			break
		}
	}
	if !matched {
		return nil, ErrBadCredentials
	}

	granted, err := domain.GrantedScopes(client.Scopes, requestedScope)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrValidation, err)
	}

	kid, priv, err := s.ActiveSigningKey(ctx)
	if err != nil {
		return nil, fmt.Errorf("app: loading the active signing key: %w", err)
	}
	signingKey, err := jwk.Import(priv)
	if err != nil {
		return nil, fmt.Errorf("app: importing the active signing key: %w", err)
	}
	if err := signingKey.Set(jwk.KeyIDKey, kid); err != nil {
		return nil, fmt.Errorf("app: setting the signing key's kid: %w", err)
	}

	jti, err := ids.New()
	if err != nil {
		return nil, fmt.Errorf("app: generating a token id: %w", err)
	}

	now := s.clk.Now()
	scope := strings.Join(granted, " ")

	// ip_allowlist is space-separated, same shape as scope — the
	// gateway (task 1.11) checks a caller's IP against this claim
	// directly, since it has no Postgres connection of its own and
	// must never read identity's schema (CLAUDE.md's service
	// boundary). This is a point-in-time snapshot, same staleness
	// already accepted for scope/env/tid: a client's allowlist change
	// takes effect on the next token mint, not instantly.
	ipAllowlist := make([]string, len(client.IpAllowlist))
	for i, p := range client.IpAllowlist {
		ipAllowlist[i] = p.String()
	}

	token, err := jwt.NewBuilder().
		Issuer(oauthIssuer).
		Subject(client.ID.String()).
		Audience([]string{oauthAudience}).
		IssuedAt(now).
		Expiration(now.Add(tokenTTL)).
		JwtID(jti.String()).
		Claim("client_id", client.ID.String()).
		Claim("tid", client.TenantID.String()).
		Claim("env", string(client.Environment)).
		Claim("scope", scope).
		Claim("ip_allowlist", strings.Join(ipAllowlist, " ")).
		Build()
	if err != nil {
		return nil, fmt.Errorf("app: building token claims: %w", err)
	}

	signed, err := jwt.Sign(token, jwt.WithKey(jwa.EdDSA(), signingKey))
	if err != nil {
		return nil, fmt.Errorf("app: signing token: %w", err)
	}

	return &Token{
		AccessToken: string(signed),
		TokenType:   "Bearer",
		ExpiresIn:   int(tokenTTL.Seconds()),
		Scope:       scope,
	}, nil
}
