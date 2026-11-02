//go:build integration

package app_test

import (
	"context"
	"errors"
	"testing"

	vascrypto "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/crypto"
	vasdb "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"
	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/app"
)

// smppPasswordHash reads an account's stored Argon2id hash directly.
func smppPasswordHash(t *testing.T, ctx context.Context, pool *vasdb.Pool, accountID uuid.UUID) string {
	t.Helper()
	var hash string
	if err := pool.QueryRow(ctx, "SELECT password_hash FROM identity.smpp_accounts WHERE id = $1", accountID).Scan(&hash); err != nil {
		t.Fatalf("reading password_hash: %v", err)
	}
	return hash
}

func TestCreateSMPPAccount_HappyPath_ReturnsPasswordOnceAndStoresHash(t *testing.T) {
	ctx := context.Background()
	svc, pool, sender, _, _ := newTestService(t)
	userID := signedInUserID(t, ctx, svc, sender, "ada@example.com")

	account, err := svc.CreateSMPPAccount(ctx, tenantOf(userID), userID, "ACME01", "sandbox", []string{"203.0.113.0/24"}, 2, 50)
	if err != nil {
		t.Fatalf("CreateSMPPAccount: %v", err)
	}
	if account.Password == "" {
		t.Fatal("expected a non-empty password")
	}
	if account.SystemID != "ACME01" {
		t.Fatalf("got system_id %q", account.SystemID)
	}
	if len(account.IPAllowlist) != 1 || account.IPAllowlist[0] != "203.0.113.0/24" {
		t.Fatalf("got IP allowlist %v", account.IPAllowlist)
	}
	if account.Status != "active" {
		t.Fatalf("got status %q, want active", account.Status)
	}

	ok, err := vascrypto.Argon2Verify(smppPasswordHash(t, ctx, pool, account.ID), account.Password)
	if err != nil {
		t.Fatalf("Argon2Verify: %v", err)
	}
	if !ok {
		t.Fatal("expected the stored hash to verify against the issued password exactly once")
	}
}

func TestCreateSMPPAccount_EmptyIPAllowlist_ReturnsValidationError(t *testing.T) {
	ctx := context.Background()
	svc, _, sender, _, _ := newTestService(t)
	userID := signedInUserID(t, ctx, svc, sender, "ada@example.com")

	if _, err := svc.CreateSMPPAccount(ctx, tenantOf(userID), userID, "ACME01", "sandbox", nil, 2, 50); !errors.Is(err, app.ErrValidation) {
		t.Fatalf("got error %v, want app.ErrValidation", err)
	}
}

func TestCreateSMPPAccount_DuplicateSystemID_ReturnsValidationError(t *testing.T) {
	ctx := context.Background()
	svc, _, sender, _, _ := newTestService(t)
	adaID := signedInUserID(t, ctx, svc, sender, "ada@example.com")
	bobID := signedInUserID(t, ctx, svc, sender, "bob@example.com")

	if _, err := svc.CreateSMPPAccount(ctx, tenantOf(adaID), adaID, "SHARED01", "sandbox", []string{"203.0.113.0/24"}, 2, 50); err != nil {
		t.Fatalf("CreateSMPPAccount(ada): %v", err)
	}
	if _, err := svc.CreateSMPPAccount(ctx, tenantOf(bobID), bobID, "SHARED01", "sandbox", []string{"203.0.113.0/24"}, 2, 50); !errors.Is(err, app.ErrValidation) {
		t.Fatalf("got error %v, want app.ErrValidation (system_id is globally unique)", err)
	}
}

func TestListSMPPAccounts_ScopedToCreatingUser(t *testing.T) {
	ctx := context.Background()
	svc, _, sender, _, _ := newTestService(t)
	adaID := signedInUserID(t, ctx, svc, sender, "ada@example.com")
	bobID := signedInUserID(t, ctx, svc, sender, "bob@example.com")

	if _, err := svc.CreateSMPPAccount(ctx, tenantOf(adaID), adaID, "ADA01", "sandbox", []string{"203.0.113.0/24"}, 2, 50); err != nil {
		t.Fatalf("CreateSMPPAccount(ada): %v", err)
	}

	adaAccounts, err := svc.ListSMPPAccounts(ctx, tenantOf(adaID))
	if err != nil {
		t.Fatalf("ListSMPPAccounts(ada): %v", err)
	}
	if len(adaAccounts) != 1 {
		t.Fatalf("got %d accounts for ada, want 1", len(adaAccounts))
	}

	bobAccounts, err := svc.ListSMPPAccounts(ctx, tenantOf(bobID))
	if err != nil {
		t.Fatalf("ListSMPPAccounts(bob): %v", err)
	}
	if len(bobAccounts) != 0 {
		t.Fatalf("got %d accounts for bob, want 0 (ada's account must not be visible)", len(bobAccounts))
	}
}

func TestRotateSMPPAccountPassword_OverwritesInPlace(t *testing.T) {
	ctx := context.Background()
	svc, _, sender, _, _ := newTestService(t)
	userID := signedInUserID(t, ctx, svc, sender, "ada@example.com")

	account, err := svc.CreateSMPPAccount(ctx, tenantOf(userID), userID, "ACME01", "live", []string{"203.0.113.0/24"}, 2, 50)
	if err != nil {
		t.Fatalf("CreateSMPPAccount: %v", err)
	}

	rotated, err := svc.RotateSMPPAccountPassword(ctx, tenantOf(userID), userID, account.ID)
	if err != nil {
		t.Fatalf("RotateSMPPAccountPassword: %v", err)
	}
	if rotated.Password == account.Password {
		t.Fatal("expected a fresh password on rotation")
	}
}

func TestSuspendAndReactivateSMPPAccount(t *testing.T) {
	ctx := context.Background()
	svc, _, sender, _, _ := newTestService(t)
	userID := signedInUserID(t, ctx, svc, sender, "ada@example.com")

	account, err := svc.CreateSMPPAccount(ctx, tenantOf(userID), userID, "ACME01", "sandbox", []string{"203.0.113.0/24"}, 2, 50)
	if err != nil {
		t.Fatalf("CreateSMPPAccount: %v", err)
	}

	if err := svc.SuspendSMPPAccount(ctx, tenantOf(userID), userID, account.ID); err != nil {
		t.Fatalf("SuspendSMPPAccount: %v", err)
	}
	got, err := svc.GetSMPPAccount(ctx, tenantOf(userID), account.ID)
	if err != nil {
		t.Fatalf("GetSMPPAccount: %v", err)
	}
	if got.Status != "suspended" {
		t.Fatalf("got status %q, want suspended", got.Status)
	}

	if err := svc.ReactivateSMPPAccount(ctx, tenantOf(userID), userID, account.ID); err != nil {
		t.Fatalf("ReactivateSMPPAccount: %v", err)
	}
	got, err = svc.GetSMPPAccount(ctx, tenantOf(userID), account.ID)
	if err != nil {
		t.Fatalf("GetSMPPAccount: %v", err)
	}
	if got.Status != "active" {
		t.Fatalf("got status %q, want active", got.Status)
	}
}

func TestSMPPAccount_NotOwned_OperationsReturnNotFound(t *testing.T) {
	ctx := context.Background()
	svc, _, sender, _, _ := newTestService(t)
	adaID := signedInUserID(t, ctx, svc, sender, "ada@example.com")
	bobID := signedInUserID(t, ctx, svc, sender, "bob@example.com")

	account, err := svc.CreateSMPPAccount(ctx, tenantOf(adaID), adaID, "ADA01", "sandbox", []string{"203.0.113.0/24"}, 2, 50)
	if err != nil {
		t.Fatalf("CreateSMPPAccount: %v", err)
	}

	if _, err := svc.GetSMPPAccount(ctx, tenantOf(bobID), account.ID); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("GetSMPPAccount: got error %v, want app.ErrNotFound", err)
	}
	if _, err := svc.RotateSMPPAccountPassword(ctx, tenantOf(bobID), bobID, account.ID); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("RotateSMPPAccountPassword: got error %v, want app.ErrNotFound", err)
	}
	if err := svc.SuspendSMPPAccount(ctx, tenantOf(bobID), bobID, account.ID); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("SuspendSMPPAccount: got error %v, want app.ErrNotFound", err)
	}
}
