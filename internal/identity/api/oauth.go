package api

import (
	"context"
	"encoding/base64"
	"errors"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/app"
)

// CreateOAuthToken implements RFC 6749 §4.4's client-credentials grant.
// Unlike every other identity operation, its failure responses are
// RFC 6749 §5.2's {"error": "..."} shape, not problem+json — the
// whole point of calling this "OAuth2" is that a generic client
// library can parse the error without knowing anything about VASPRO's
// own conventions (api.md). No Idempotency-Key, no session bearer:
// routes.go exempts this one path from idempotency.Middleware, and
// this grant authenticates via the body itself.
func (h *Handlers) CreateOAuthToken(ctx context.Context, request CreateOAuthTokenRequestObject) (CreateOAuthTokenResponseObject, error) {
	body := request.Body
	if body == nil || body.GrantType != "client_credentials" {
		desc := `grant_type must be "client_credentials"`
		return CreateOAuthToken400JSONResponse{Error: "unsupported_grant_type", ErrorDescription: &desc}, nil
	}
	if body.ClientId == "" || body.ClientSecret == "" {
		desc := "client_id and client_secret are required"
		return CreateOAuthToken400JSONResponse{Error: "invalid_request", ErrorDescription: &desc}, nil
	}

	var scope string
	if body.Scope != nil {
		scope = *body.Scope
	}

	token, err := h.app.ExchangeClientCredentials(ctx, body.ClientId, body.ClientSecret, scope)
	if err != nil {
		switch {
		case errors.Is(err, app.ErrBadCredentials):
			return CreateOAuthToken401JSONResponse{Error: "invalid_client"}, nil
		case errors.Is(err, app.ErrValidation):
			desc := "the requested scope is not granted to this client"
			return CreateOAuthToken400JSONResponse{Error: "invalid_scope", ErrorDescription: &desc}, nil
		default:
			return nil, err
		}
	}

	return CreateOAuthToken200JSONResponse{
		AccessToken: token.AccessToken,
		TokenType:   token.TokenType,
		ExpiresIn:   token.ExpiresIn,
		Scope:       token.Scope,
	}, nil
}

// GetJwks publishes identity's current signing keys (active and
// retiring — never retired, never any private material) at the one
// unversioned, root-level path this API has, per RFC 8414/OIDC
// discovery convention.
func (h *Handlers) GetJwks(ctx context.Context, request GetJwksRequestObject) (GetJwksResponseObject, error) {
	keys, err := h.app.PublishableSigningKeys(ctx)
	if err != nil {
		return nil, err
	}

	jwkKeys := make([]JwkKey, 0, len(keys))
	for _, k := range keys {
		jwkKeys = append(jwkKeys, JwkKey{
			Kty: "OKP",
			Crv: "Ed25519",
			X:   base64.RawURLEncoding.EncodeToString(k.PublicKey),
			Kid: k.Kid,
			Alg: k.Algorithm,
			Use: "sig",
		})
	}
	return GetJwks200JSONResponse{Keys: jwkKeys}, nil
}
