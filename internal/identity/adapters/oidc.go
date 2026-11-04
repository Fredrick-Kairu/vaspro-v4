package adapters

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"sync"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// OIDCClaims is what identity needs from a verified ID token (task
// 1.16). Subject is the provider's stable user ID; accounts are matched
// on it, never on Email (D-22).
type OIDCClaims struct {
	Subject       string
	Email         string
	EmailVerified bool
	Name          string
	HostedDomain  string // Google Workspace's hd claim; empty for consumer accounts
}

// ErrOIDCVerification is returned for any failed code exchange or ID
// token check: bad signature, wrong issuer or audience, expired, or a
// nonce that isn't the one this flow sent.
var ErrOIDCVerification = errors.New("adapters: OIDC verification failed")

// OIDCProvider runs the authorization code flow with PKCE against one
// OpenID Connect provider (Google, D-22).
type OIDCProvider interface {
	// AuthCodeURL is where to send the browser. state and nonce are
	// echoed back; verifier is the PKCE secret whose S256 challenge
	// goes in the URL.
	AuthCodeURL(ctx context.Context, state, nonce, verifier string) (string, error)
	// Exchange trades code for tokens using verifier, then verifies the
	// ID token (signature against the provider's keys, issuer, audience
	// = our client ID, expiry) and that its nonce equals nonce.
	Exchange(ctx context.Context, code, verifier, nonce string) (*OIDCClaims, error)
}

// GoogleOIDC is OIDCProvider over coreos/go-oidc and x/oauth2. The
// discovery document is fetched lazily and retried until it succeeds,
// so identity still starts when the provider is briefly unreachable.
type GoogleOIDC struct {
	issuer       string
	clientID     string
	clientSecret string
	redirectURL  string

	mu       sync.Mutex
	oauth    *oauth2.Config
	verifier *oidc.IDTokenVerifier
}

// NewGoogleOIDC builds a GoogleOIDC. issuer is
// https://accounts.google.com in production; tests point it at
// tools/fakes/oidc.
func NewGoogleOIDC(issuer, clientID, clientSecret, redirectURL string) *GoogleOIDC {
	return &GoogleOIDC{issuer: issuer, clientID: clientID, clientSecret: clientSecret, redirectURL: redirectURL}
}

func (g *GoogleOIDC) init(ctx context.Context) (*oauth2.Config, *oidc.IDTokenVerifier, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.oauth != nil {
		return g.oauth, g.verifier, nil
	}
	provider, err := oidc.NewProvider(ctx, g.issuer)
	if err != nil {
		return nil, nil, fmt.Errorf("adapters: OIDC discovery for %s: %w", g.issuer, err)
	}
	g.oauth = &oauth2.Config{
		ClientID:     g.clientID,
		ClientSecret: g.clientSecret,
		RedirectURL:  g.redirectURL,
		Endpoint:     provider.Endpoint(),
		Scopes:       []string{oidc.ScopeOpenID, "email", "profile"},
	}
	g.verifier = provider.Verifier(&oidc.Config{ClientID: g.clientID})
	return g.oauth, g.verifier, nil
}

// AuthCodeURL implements OIDCProvider.
func (g *GoogleOIDC) AuthCodeURL(ctx context.Context, state, nonce, verifier string) (string, error) {
	cfg, _, err := g.init(ctx)
	if err != nil {
		return "", err
	}
	return cfg.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier)), nil
}

// Exchange implements OIDCProvider.
func (g *GoogleOIDC) Exchange(ctx context.Context, code, verifier, nonce string) (*OIDCClaims, error) {
	cfg, idVerifier, err := g.init(ctx)
	if err != nil {
		return nil, err
	}
	token, err := cfg.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return nil, fmt.Errorf("%w: code exchange: %v", ErrOIDCVerification, err)
	}
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		return nil, fmt.Errorf("%w: no id_token in token response", ErrOIDCVerification)
	}
	idToken, err := idVerifier.Verify(ctx, rawIDToken)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrOIDCVerification, err)
	}
	// go-oidc leaves the nonce to the caller.
	if subtle.ConstantTimeCompare([]byte(idToken.Nonce), []byte(nonce)) != 1 {
		return nil, fmt.Errorf("%w: nonce mismatch", ErrOIDCVerification)
	}
	var claims struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		Name          string `json:"name"`
		HostedDomain  string `json:"hd"`
	}
	if err := idToken.Claims(&claims); err != nil {
		return nil, fmt.Errorf("%w: decoding claims: %v", ErrOIDCVerification, err)
	}
	return &OIDCClaims{
		Subject:       idToken.Subject,
		Email:         claims.Email,
		EmailVerified: claims.EmailVerified,
		Name:          claims.Name,
		HostedDomain:  claims.HostedDomain,
	}, nil
}
