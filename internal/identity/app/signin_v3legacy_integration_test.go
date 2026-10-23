//go:build integration

package app_test

import (
	"context"
	"errors"
	"testing"

	vascrypto "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/crypto"
	vasdb "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/app"
)

// passwordHashFor reads a user's stored password_hash directly —
// identity.users has no RLS (it's not a tenant-owned table, 1.1's own
// finding), so this plain query needs no tenant transaction.
func passwordHashFor(t *testing.T, ctx context.Context, pool *vasdb.Pool, email string) string {
	t.Helper()
	var hash string
	if err := pool.QueryRow(ctx, "SELECT password_hash FROM identity.users WHERE email = $1", email).Scan(&hash); err != nil {
		t.Fatalf("reading password_hash: %v", err)
	}
	return hash
}

func TestSignIn_V3LegacyHash_VerifiesOnceThenUpgradesToArgon2id(t *testing.T) {
	ctx := context.Background()
	svc, pool, _, _, _ := newTestService(t)

	report, err := svc.ImportV3Users(ctx, []app.V3UserRecord{
		{V3ID: "1", EmailAddress: "ada@example.com", FullNames: "Ada Lovelace", Password: v3Hash(t, "the-v3-password")},
	})
	if err != nil {
		t.Fatalf("ImportV3Users: %v", err)
	}
	if report.Imported != 1 {
		t.Fatalf("got report %+v, want 1 imported", report)
	}

	hashBefore := passwordHashFor(t, ctx, pool, "ada@example.com")
	if !vascrypto.IsBcryptHash(hashBefore) {
		t.Fatalf("got hash %q before sign-in, want a bcrypt (v3) hash", hashBefore)
	}

	result, err := svc.SignIn(ctx, "ada@example.com", "the-v3-password", nil, "test-agent")
	if err != nil {
		t.Fatalf("SignIn with the v3 password: %v", err)
	}
	if result.Session == nil {
		t.Fatal("expected a session (no TOTP enrolled)")
	}

	hashAfter := passwordHashFor(t, ctx, pool, "ada@example.com")
	if vascrypto.IsBcryptHash(hashAfter) {
		t.Fatalf("got hash %q after sign-in, want it upgraded away from bcrypt", hashAfter)
	}
	ok, err := vascrypto.Argon2Verify(hashAfter, "the-v3-password")
	if err != nil {
		t.Fatalf("Argon2Verify: %v", err)
	}
	if !ok {
		t.Fatal("expected the upgraded Argon2id hash to verify against the same password")
	}

	// And the upgrade actually took effect for the *next* sign-in too,
	// not just this one: a second sign-in with the same password still
	// succeeds, now purely through the Argon2Verify path.
	if _, err := svc.SignIn(ctx, "ada@example.com", "the-v3-password", nil, "test-agent"); err != nil {
		t.Fatalf("second SignIn after upgrade: %v", err)
	}
}

func TestSignIn_V3LegacyHash_WrongPassword_FailsAndStaysLegacy(t *testing.T) {
	ctx := context.Background()
	svc, pool, _, _, _ := newTestService(t)

	if _, err := svc.ImportV3Users(ctx, []app.V3UserRecord{
		{V3ID: "1", EmailAddress: "ada@example.com", FullNames: "Ada", Password: v3Hash(t, "the-real-password")},
	}); err != nil {
		t.Fatalf("ImportV3Users: %v", err)
	}

	_, err := svc.SignIn(ctx, "ada@example.com", "the-wrong-password", nil, "test-agent")
	if !errors.Is(err, app.ErrBadCredentials) {
		t.Fatalf("got error %v, want ErrBadCredentials", err)
	}

	hash := passwordHashFor(t, ctx, pool, "ada@example.com")
	if !vascrypto.IsBcryptHash(hash) {
		t.Fatalf("got hash %q, want it to stay a bcrypt (v3) hash after a failed attempt", hash)
	}
}
