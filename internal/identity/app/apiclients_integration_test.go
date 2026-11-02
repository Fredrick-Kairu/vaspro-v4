//go:build integration

package app_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	vascrypto "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/crypto"
	vasdb "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/app"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/email"
)

// signedInUserID signs up, verifies, sets a password, signs in, and
// returns the user's ID.
func signedInUserID(t *testing.T, ctx context.Context, svc *app.Service, sender *email.Sender, emailAddr string) uuid.UUID {
	t.Helper()
	setPassword(t, ctx, svc, sender, emailAddr, "a-strong-password")
	result, err := svc.SignIn(ctx, emailAddr, "a-strong-password", nil, "test-agent")
	if err != nil {
		t.Fatalf("SignIn: %v", err)
	}
	_, userID, err := svc.ResolveSession(ctx, result.Session.Token)
	if err != nil {
		t.Fatalf("ResolveSession: %v", err)
	}
	return userID
}

// tenantOf stands in for the account userID's session selected (task
// 2.2c): a fixed ID per user, distinct from the user's own, so each
// test user works in an account of their own.
func tenantOf(userID uuid.UUID) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceOID, userID[:])
}

// secretHash reads a secret's stored Argon2id hash directly —
// api_client_secrets has no RLS (see the migration's own comment and
// OQ-01), so this plain query needs no tenant transaction.
func secretHash(t *testing.T, ctx context.Context, pool *vasdb.Pool, secretID uuid.UUID) string {
	t.Helper()
	var hash string
	if err := pool.QueryRow(ctx, "SELECT secret_hash FROM api_client_secrets WHERE id = $1", secretID).Scan(&hash); err != nil {
		t.Fatalf("reading secret_hash: %v", err)
	}
	return hash
}

func TestCreateApiClient_HappyPath_ReturnsSecretOnceAndStoresHash(t *testing.T) {
	ctx := context.Background()
	svc, pool, sender, _, _ := newTestService(t)
	userID := signedInUserID(t, ctx, svc, sender, "ada@example.com")

	client, err := svc.CreateApiClient(ctx, tenantOf(userID), userID, "Reporting integration", "sandbox",
		[]string{"reports:read"}, []string{"203.0.113.0/24"}, nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateApiClient: %v", err)
	}
	if client.Secret.Secret == "" {
		t.Fatal("expected a non-empty secret")
	}
	if client.Name != "Reporting integration" {
		t.Fatalf("got name %q", client.Name)
	}
	if len(client.IPAllowlist) != 1 || client.IPAllowlist[0] != "203.0.113.0/24" {
		t.Fatalf("got IP allowlist %v", client.IPAllowlist)
	}

	detail, err := svc.GetApiClient(ctx, tenantOf(userID), client.ID)
	if err != nil {
		t.Fatalf("GetApiClient: %v", err)
	}
	if len(detail.Secrets) != 1 {
		t.Fatalf("got %d secrets, want 1", len(detail.Secrets))
	}
	if detail.Secrets[0].Last4 != client.Secret.Last4 {
		t.Fatalf("got last4 %q, want %q", detail.Secrets[0].Last4, client.Secret.Last4)
	}

	ok, err := vascrypto.Argon2Verify(secretHash(t, ctx, pool, client.Secret.ID), client.Secret.Secret)
	if err != nil {
		t.Fatalf("Argon2Verify: %v", err)
	}
	if !ok {
		t.Fatal("expected the stored hash to verify against the issued secret exactly once")
	}
}

func TestListApiClients_ScopedToTheAccount(t *testing.T) {
	ctx := context.Background()
	svc, _, sender, _, _ := newTestService(t)
	adaID := signedInUserID(t, ctx, svc, sender, "ada@example.com")
	bobID := signedInUserID(t, ctx, svc, sender, "bob@example.com")

	if _, err := svc.CreateApiClient(ctx, tenantOf(adaID), adaID, "Ada's client", "sandbox", []string{"sms:send"}, nil, nil, nil, nil); err != nil {
		t.Fatalf("CreateApiClient(ada): %v", err)
	}

	adaClients, err := svc.ListApiClients(ctx, tenantOf(adaID))
	if err != nil {
		t.Fatalf("ListApiClients(ada): %v", err)
	}
	if len(adaClients) != 1 {
		t.Fatalf("got %d clients for ada, want 1", len(adaClients))
	}

	bobClients, err := svc.ListApiClients(ctx, tenantOf(bobID))
	if err != nil {
		t.Fatalf("ListApiClients(bob): %v", err)
	}
	if len(bobClients) != 0 {
		t.Fatalf("got %d clients for bob, want 0 (ada's client must not be visible)", len(bobClients))
	}
}

func TestRotateApiClientSecret_StartsOverlapOnPrevious(t *testing.T) {
	ctx := context.Background()
	svc, _, sender, _, _ := newTestService(t)
	userID := signedInUserID(t, ctx, svc, sender, "ada@example.com")

	client, err := svc.CreateApiClient(ctx, tenantOf(userID), userID, "Rotated client", "live", []string{"sms:send"}, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateApiClient: %v", err)
	}

	rotated, err := svc.RotateApiClientSecret(ctx, tenantOf(userID), userID, client.ID)
	if err != nil {
		t.Fatalf("RotateApiClientSecret: %v", err)
	}
	if rotated.Secret == client.Secret.Secret {
		t.Fatal("expected a fresh secret on rotation")
	}

	detail, err := svc.GetApiClient(ctx, tenantOf(userID), client.ID)
	if err != nil {
		t.Fatalf("GetApiClient: %v", err)
	}
	if len(detail.Secrets) != 2 {
		t.Fatalf("got %d active secrets after one rotation, want 2", len(detail.Secrets))
	}
	var sawExpiring bool
	for _, s := range detail.Secrets {
		if s.ID == client.Secret.ID {
			if s.ExpiresAt == nil {
				t.Fatal("expected the rotated-out secret to have an expires_at (the overlap end)")
			}
			sawExpiring = true
		}
	}
	if !sawExpiring {
		t.Fatal("expected the original secret to still be listed as active during its overlap")
	}
}

func TestRotateApiClientSecret_WhenAlreadyTwoActive_ReturnsValidationError(t *testing.T) {
	ctx := context.Background()
	svc, _, sender, _, _ := newTestService(t)
	userID := signedInUserID(t, ctx, svc, sender, "ada@example.com")

	client, err := svc.CreateApiClient(ctx, tenantOf(userID), userID, "Client", "live", []string{"sms:send"}, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateApiClient: %v", err)
	}
	if _, err := svc.RotateApiClientSecret(ctx, tenantOf(userID), userID, client.ID); err != nil {
		t.Fatalf("first RotateApiClientSecret: %v", err)
	}

	if _, err := svc.RotateApiClientSecret(ctx, tenantOf(userID), userID, client.ID); !errors.Is(err, app.ErrValidation) {
		t.Fatalf("got error %v, want app.ErrValidation", err)
	}
}

func TestRevokeApiClient_StopsBeingReturnedButSecretsUntouched(t *testing.T) {
	ctx := context.Background()
	svc, _, sender, _, _ := newTestService(t)
	userID := signedInUserID(t, ctx, svc, sender, "ada@example.com")

	client, err := svc.CreateApiClient(ctx, tenantOf(userID), userID, "Client", "sandbox", []string{"sms:send"}, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateApiClient: %v", err)
	}

	if err := svc.RevokeApiClient(ctx, tenantOf(userID), userID, client.ID); err != nil {
		t.Fatalf("RevokeApiClient: %v", err)
	}

	detail, err := svc.GetApiClient(ctx, tenantOf(userID), client.ID)
	if err != nil {
		t.Fatalf("GetApiClient after revoke: %v", err)
	}
	if detail.RevokedAt == nil {
		t.Fatal("expected the client to be marked revoked")
	}
	if len(detail.Secrets) != 1 {
		t.Fatalf("got %d secrets after revoking the client, want 1 (secrets aren't touched by client revocation)", len(detail.Secrets))
	}
}

func TestRevokeApiClientSecret_RevokesOnlyThatOne(t *testing.T) {
	ctx := context.Background()
	svc, _, sender, _, _ := newTestService(t)
	userID := signedInUserID(t, ctx, svc, sender, "ada@example.com")

	client, err := svc.CreateApiClient(ctx, tenantOf(userID), userID, "Client", "sandbox", []string{"sms:send"}, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateApiClient: %v", err)
	}
	rotated, err := svc.RotateApiClientSecret(ctx, tenantOf(userID), userID, client.ID)
	if err != nil {
		t.Fatalf("RotateApiClientSecret: %v", err)
	}

	if err := svc.RevokeApiClientSecret(ctx, tenantOf(userID), client.ID, client.Secret.ID); err != nil {
		t.Fatalf("RevokeApiClientSecret: %v", err)
	}

	detail, err := svc.GetApiClient(ctx, tenantOf(userID), client.ID)
	if err != nil {
		t.Fatalf("GetApiClient: %v", err)
	}
	if len(detail.Secrets) != 1 {
		t.Fatalf("got %d active secrets, want 1", len(detail.Secrets))
	}
	if detail.Secrets[0].ID != rotated.ID {
		t.Fatalf("got remaining secret %v, want the rotated one %v", detail.Secrets[0].ID, rotated.ID)
	}
}

func TestApiClient_NotOwned_OperationsReturnNotFound(t *testing.T) {
	ctx := context.Background()
	svc, _, sender, _, _ := newTestService(t)
	adaID := signedInUserID(t, ctx, svc, sender, "ada@example.com")
	bobID := signedInUserID(t, ctx, svc, sender, "bob@example.com")

	client, err := svc.CreateApiClient(ctx, tenantOf(adaID), adaID, "Ada's client", "sandbox", []string{"sms:send"}, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateApiClient: %v", err)
	}

	if _, err := svc.GetApiClient(ctx, tenantOf(bobID), client.ID); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("GetApiClient: got error %v, want app.ErrNotFound", err)
	}
	if _, err := svc.RotateApiClientSecret(ctx, tenantOf(bobID), bobID, client.ID); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("RotateApiClientSecret: got error %v, want app.ErrNotFound", err)
	}
	if err := svc.RevokeApiClient(ctx, tenantOf(bobID), bobID, client.ID); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("RevokeApiClient: got error %v, want app.ErrNotFound", err)
	}
	if err := svc.RevokeApiClientSecret(ctx, tenantOf(bobID), client.ID, client.Secret.ID); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("RevokeApiClientSecret: got error %v, want app.ErrNotFound", err)
	}
}
