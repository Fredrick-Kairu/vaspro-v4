//go:build integration

package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lestrrat-go/jwx/v3/jwa"
	"github.com/lestrrat-go/jwx/v3/jwk"
	"github.com/lestrrat-go/jwx/v3/jwt"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/app"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/email"
)

// createTestApiClient signs up+verifies+sets a password+signs in a
// fresh account, then creates one API client under it with the given
// scopes — the real path task 1.7 built, reused here so this task's
// tests exercise the actual client/secret rows the token endpoint
// reads, not a hand-inserted shortcut.
func createTestApiClient(t *testing.T, ctx context.Context, svc *app.Service, sender *email.Sender, emailAddr string, scopes []string) (*app.ApiClientWithSecret, uuid.UUID) {
	t.Helper()
	userID := signedInUserID(t, ctx, svc, sender, emailAddr)
	client, err := svc.CreateApiClient(ctx, userID, "test client", "sandbox", scopes, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateApiClient: %v", err)
	}
	return client, userID
}

func TestExchangeClientCredentials_HappyPath(t *testing.T) {
	ctx := context.Background()
	svc, _, sender, _, _ := newTestService(t)
	if err := svc.EnsureActiveSigningKey(ctx); err != nil {
		t.Fatalf("EnsureActiveSigningKey: %v", err)
	}

	client, tenantID := createTestApiClient(t, ctx, svc, sender, "oauth-happy@example.com", []string{"sms:send", "reports:read"})

	token, err := svc.ExchangeClientCredentials(ctx, client.ID.String(), client.Secret.Secret, "")
	if err != nil {
		t.Fatalf("ExchangeClientCredentials: %v", err)
	}
	if token.TokenType != "Bearer" {
		t.Fatalf("got token_type %q, want Bearer", token.TokenType)
	}
	if token.ExpiresIn != 3600 {
		t.Fatalf("got expires_in %d, want 3600", token.ExpiresIn)
	}
	if token.Scope != "sms:send reports:read" {
		t.Fatalf("got scope %q, want every client scope", token.Scope)
	}

	// Verify the token against the service's own published JWKS — the
	// real proof the signing and publishing halves agree with each
	// other, not just that something was returned.
	keys, err := svc.PublishableSigningKeys(ctx)
	if err != nil {
		t.Fatalf("PublishableSigningKeys: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("got %d publishable keys, want 1", len(keys))
	}
	pubKey, err := jwk.Import(keys[0].PublicKey)
	if err != nil {
		t.Fatalf("importing the published public key: %v", err)
	}

	parsed, err := jwt.Parse([]byte(token.AccessToken), jwt.WithKey(jwa.EdDSA(), pubKey))
	if err != nil {
		t.Fatalf("verifying the issued token against the published JWKS: %v", err)
	}
	if sub, _ := parsed.Subject(); sub != client.ID.String() {
		t.Fatalf("got sub %q, want %q", sub, client.ID.String())
	}
	var tid string
	if err := parsed.Get("tid", &tid); err != nil {
		t.Fatalf("getting tid claim: %v", err)
	}
	if tid != tenantID.String() {
		t.Fatalf("got tid %q, want %q", tid, tenantID.String())
	}
	var env string
	if err := parsed.Get("env", &env); err != nil {
		t.Fatalf("getting env claim: %v", err)
	}
	if env != "sandbox" {
		t.Fatalf("got env %q, want sandbox", env)
	}
	var scope string
	if err := parsed.Get("scope", &scope); err != nil {
		t.Fatalf("getting scope claim: %v", err)
	}
	if scope != "sms:send reports:read" {
		t.Fatalf("got scope claim %q, want every client scope", scope)
	}
	var kycTier string
	if err := parsed.Get("kyc_tier", &kycTier); err == nil {
		t.Fatal("expected no kyc_tier claim (Phase 2 concept, not faked in Phase 1)")
	}
}

func TestExchangeClientCredentials_NarrowedScope(t *testing.T) {
	ctx := context.Background()
	svc, _, sender, _, _ := newTestService(t)
	if err := svc.EnsureActiveSigningKey(ctx); err != nil {
		t.Fatalf("EnsureActiveSigningKey: %v", err)
	}
	client, _ := createTestApiClient(t, ctx, svc, sender, "oauth-scope@example.com", []string{"sms:send", "reports:read"})

	token, err := svc.ExchangeClientCredentials(ctx, client.ID.String(), client.Secret.Secret, "reports:read")
	if err != nil {
		t.Fatalf("ExchangeClientCredentials: %v", err)
	}
	if token.Scope != "reports:read" {
		t.Fatalf("got scope %q, want reports:read", token.Scope)
	}
}

func TestExchangeClientCredentials_ScopeNotGranted(t *testing.T) {
	ctx := context.Background()
	svc, _, sender, _, _ := newTestService(t)
	if err := svc.EnsureActiveSigningKey(ctx); err != nil {
		t.Fatalf("EnsureActiveSigningKey: %v", err)
	}
	client, _ := createTestApiClient(t, ctx, svc, sender, "oauth-badscope@example.com", []string{"sms:send"})

	_, err := svc.ExchangeClientCredentials(ctx, client.ID.String(), client.Secret.Secret, "reports:read")
	if !errors.Is(err, app.ErrValidation) {
		t.Fatalf("got err %v, want ErrValidation", err)
	}
}

func TestExchangeClientCredentials_WrongSecret(t *testing.T) {
	ctx := context.Background()
	svc, _, sender, _, _ := newTestService(t)
	if err := svc.EnsureActiveSigningKey(ctx); err != nil {
		t.Fatalf("EnsureActiveSigningKey: %v", err)
	}
	client, _ := createTestApiClient(t, ctx, svc, sender, "oauth-wrongsecret@example.com", []string{"sms:send"})

	_, err := svc.ExchangeClientCredentials(ctx, client.ID.String(), "not-the-real-secret", "")
	if !errors.Is(err, app.ErrBadCredentials) {
		t.Fatalf("got err %v, want ErrBadCredentials", err)
	}
}

func TestExchangeClientCredentials_UnknownClient(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _, _ := newTestService(t)
	if err := svc.EnsureActiveSigningKey(ctx); err != nil {
		t.Fatalf("EnsureActiveSigningKey: %v", err)
	}

	_, err := svc.ExchangeClientCredentials(ctx, "00000000-0000-0000-0000-000000000000", "whatever", "")
	if !errors.Is(err, app.ErrBadCredentials) {
		t.Fatalf("got err %v, want ErrBadCredentials", err)
	}
}

func TestExchangeClientCredentials_RevokedClient(t *testing.T) {
	ctx := context.Background()
	svc, _, sender, _, _ := newTestService(t)
	if err := svc.EnsureActiveSigningKey(ctx); err != nil {
		t.Fatalf("EnsureActiveSigningKey: %v", err)
	}
	userID := signedInUserID(t, ctx, svc, sender, "oauth-revoked@example.com")
	client, err := svc.CreateApiClient(ctx, userID, "test client", "sandbox", []string{"sms:send"}, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateApiClient: %v", err)
	}
	if err := svc.RevokeApiClient(ctx, userID, client.ID); err != nil {
		t.Fatalf("RevokeApiClient: %v", err)
	}

	_, err = svc.ExchangeClientCredentials(ctx, client.ID.String(), client.Secret.Secret, "")
	if !errors.Is(err, app.ErrBadCredentials) {
		t.Fatalf("got err %v, want ErrBadCredentials", err)
	}
}

func TestExchangeClientCredentials_ExpiredClient(t *testing.T) {
	ctx := context.Background()
	svc, _, sender, clk, _ := newTestService(t)
	if err := svc.EnsureActiveSigningKey(ctx); err != nil {
		t.Fatalf("EnsureActiveSigningKey: %v", err)
	}
	userID := signedInUserID(t, ctx, svc, sender, "oauth-expired@example.com")
	expiresAt := clk.Now().Add(time.Hour)
	client, err := svc.CreateApiClient(ctx, userID, "test client", "sandbox", []string{"sms:send"}, nil, &expiresAt, nil, nil)
	if err != nil {
		t.Fatalf("CreateApiClient: %v", err)
	}

	clk.Advance(2 * time.Hour)

	_, err = svc.ExchangeClientCredentials(ctx, client.ID.String(), client.Secret.Secret, "")
	if !errors.Is(err, app.ErrBadCredentials) {
		t.Fatalf("got err %v, want ErrBadCredentials", err)
	}
}
