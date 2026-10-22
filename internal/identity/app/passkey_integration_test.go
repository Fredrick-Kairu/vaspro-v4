//go:build integration

package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/descope/virtualwebauthn"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/app"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/store"
)

func testRelyingParty() virtualwebauthn.RelyingParty {
	return virtualwebauthn.RelyingParty{ID: testRPID, Name: "VASPRO Test", Origin: testRPOrigin}
}

func marshalOptions(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshaling ceremony options: %v", err)
	}
	return string(b)
}

func TestPasskey_RegisterThenSignIn_CreatesSessionWithoutPasswordOrTOTP(t *testing.T) {
	ctx := context.Background()
	svc, pool, sender, _ := newTestService(t)
	signUpAndVerify(t, ctx, svc, sender, "ada@example.com")

	user, err := store.New(pool).GetUserByEmail(ctx, "ada@example.com")
	if err != nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}

	rp := testRelyingParty()
	authenticator := virtualwebauthn.NewAuthenticator()
	cred := virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)
	authenticator.AddCredential(cred)

	reg, err := svc.BeginPasskeyRegistration(ctx, user.ID)
	if err != nil {
		t.Fatalf("BeginPasskeyRegistration: %v", err)
	}
	attOptions, err := virtualwebauthn.ParseAttestationOptions(marshalOptions(t, reg.Options))
	if err != nil {
		t.Fatalf("ParseAttestationOptions: %v", err)
	}
	attResponse := virtualwebauthn.CreateAttestationResponse(rp, authenticator, cred, *attOptions)

	summary, err := svc.FinishPasskeyRegistration(ctx, user.ID, reg.SessionToken, "Work laptop", []byte(attResponse))
	if err != nil {
		t.Fatalf("FinishPasskeyRegistration: %v", err)
	}
	if summary.Name != "Work laptop" {
		t.Fatalf("got name %q, want %q", summary.Name, "Work laptop")
	}

	challenge, err := svc.BeginPasskeyLogin(ctx, "ada@example.com")
	if err != nil {
		t.Fatalf("BeginPasskeyLogin: %v", err)
	}
	assertOptions, err := virtualwebauthn.ParseAssertionOptions(marshalOptions(t, challenge.Options))
	if err != nil {
		t.Fatalf("ParseAssertionOptions: %v", err)
	}
	assertResponse := virtualwebauthn.CreateAssertionResponse(rp, authenticator, cred, *assertOptions)

	session, err := svc.CompletePasskeyLogin(ctx, challenge.SessionToken, []byte(assertResponse), nil, "test-agent")
	if err != nil {
		t.Fatalf("CompletePasskeyLogin: %v", err)
	}
	if session == nil {
		t.Fatal("expected a session after a successful passkey sign-in")
	}
}

// TestPasskey_BackupEligibleAuthenticator_SignsInTwice proves the
// schema-widening fix (migration 20261022074003) actually works, not
// just compiles: a synced/backup-eligible passkey (the common case for
// real platform authenticators — iCloud Keychain, Google Password
// Manager) sets BackupEligible/BackupState on every response.
// go-webauthn's own ValidateLogin unconditionally rejects a login
// whose BackupEligible flag doesn't match what was stored at
// registration — without persisting flags at all, this would fail on
// the very first post-registration login.
func TestPasskey_BackupEligibleAuthenticator_SignsInTwice(t *testing.T) {
	ctx := context.Background()
	svc, pool, sender, _ := newTestService(t)
	signUpAndVerify(t, ctx, svc, sender, "ada@example.com")
	user, err := store.New(pool).GetUserByEmail(ctx, "ada@example.com")
	if err != nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}

	rp := testRelyingParty()
	authenticator := virtualwebauthn.NewAuthenticatorWithOptions(virtualwebauthn.AuthenticatorOptions{
		BackupEligible: true,
		BackupState:    true,
	})
	cred := virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)
	authenticator.AddCredential(cred)

	reg, err := svc.BeginPasskeyRegistration(ctx, user.ID)
	if err != nil {
		t.Fatalf("BeginPasskeyRegistration: %v", err)
	}
	attOptions, err := virtualwebauthn.ParseAttestationOptions(marshalOptions(t, reg.Options))
	if err != nil {
		t.Fatalf("ParseAttestationOptions: %v", err)
	}
	attResponse := virtualwebauthn.CreateAttestationResponse(rp, authenticator, cred, *attOptions)
	if _, err := svc.FinishPasskeyRegistration(ctx, user.ID, reg.SessionToken, "Synced passkey", []byte(attResponse)); err != nil {
		t.Fatalf("FinishPasskeyRegistration: %v", err)
	}

	for i := 0; i < 2; i++ {
		challenge, err := svc.BeginPasskeyLogin(ctx, "ada@example.com")
		if err != nil {
			t.Fatalf("attempt %d: BeginPasskeyLogin: %v", i, err)
		}
		assertOptions, err := virtualwebauthn.ParseAssertionOptions(marshalOptions(t, challenge.Options))
		if err != nil {
			t.Fatalf("attempt %d: ParseAssertionOptions: %v", i, err)
		}
		assertResponse := virtualwebauthn.CreateAssertionResponse(rp, authenticator, cred, *assertOptions)
		if _, err := svc.CompletePasskeyLogin(ctx, challenge.SessionToken, []byte(assertResponse), nil, "test-agent"); err != nil {
			t.Fatalf("attempt %d: CompletePasskeyLogin: %v", i, err)
		}
	}
}

func TestPasskey_BeginLogin_UnknownEmail_StillReturnsAChallenge(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _ := newTestService(t)

	challenge, err := svc.BeginPasskeyLogin(ctx, "nobody@example.com")
	if err != nil {
		t.Fatalf("expected no error for an unknown email (anti-enumeration), got %v", err)
	}
	if challenge.SessionToken == "" || challenge.Options == nil {
		t.Fatal("expected a structurally valid challenge for an unknown email")
	}
}

func TestPasskey_BeginLogin_AccountWithNoPasskeys_StillReturnsAChallenge(t *testing.T) {
	ctx := context.Background()
	svc, _, sender, _ := newTestService(t)
	signUpAndVerify(t, ctx, svc, sender, "ada@example.com")

	challenge, err := svc.BeginPasskeyLogin(ctx, "ada@example.com")
	if err != nil {
		t.Fatalf("expected no error for an account with no passkeys (anti-enumeration), got %v", err)
	}
	if challenge.SessionToken == "" || challenge.Options == nil {
		t.Fatal("expected a structurally valid challenge for an account with no passkeys")
	}
}

func TestPasskey_ListRenameDelete(t *testing.T) {
	ctx := context.Background()
	svc, pool, sender, _ := newTestService(t)
	signUpAndVerify(t, ctx, svc, sender, "ada@example.com")
	user, err := store.New(pool).GetUserByEmail(ctx, "ada@example.com")
	if err != nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}

	rp := testRelyingParty()

	enrollOne := func(name string) *app.PasskeySummary {
		authenticator := virtualwebauthn.NewAuthenticator()
		cred := virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)
		authenticator.AddCredential(cred)

		reg, err := svc.BeginPasskeyRegistration(ctx, user.ID)
		if err != nil {
			t.Fatalf("BeginPasskeyRegistration: %v", err)
		}
		attOptions, err := virtualwebauthn.ParseAttestationOptions(marshalOptions(t, reg.Options))
		if err != nil {
			t.Fatalf("ParseAttestationOptions: %v", err)
		}
		attResponse := virtualwebauthn.CreateAttestationResponse(rp, authenticator, cred, *attOptions)
		summary, err := svc.FinishPasskeyRegistration(ctx, user.ID, reg.SessionToken, name, []byte(attResponse))
		if err != nil {
			t.Fatalf("FinishPasskeyRegistration: %v", err)
		}
		return summary
	}

	first := enrollOne("Work laptop")
	second := enrollOne("Phone")

	list, err := svc.ListPasskeys(ctx, user.ID)
	if err != nil {
		t.Fatalf("ListPasskeys: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("got %d passkeys, want 2", len(list))
	}

	renamed, err := svc.RenamePasskey(ctx, user.ID, first.ID, "Renamed laptop")
	if err != nil {
		t.Fatalf("RenamePasskey: %v", err)
	}
	if renamed.Name != "Renamed laptop" {
		t.Fatalf("got name %q, want %q", renamed.Name, "Renamed laptop")
	}

	if err := svc.DeletePasskey(ctx, user.ID, second.ID); err != nil {
		t.Fatalf("DeletePasskey: %v", err)
	}

	list, err = svc.ListPasskeys(ctx, user.ID)
	if err != nil {
		t.Fatalf("ListPasskeys after delete: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("got %d passkeys after delete, want 1", len(list))
	}
	if list[0].ID != first.ID {
		t.Fatalf("the remaining passkey is %v, want %v", list[0].ID, first.ID)
	}
}

func TestRenamePasskey_NotOwned_ReturnsNotFound(t *testing.T) {
	ctx := context.Background()
	svc, pool, sender, _ := newTestService(t)
	signUpAndVerify(t, ctx, svc, sender, "ada@example.com")
	signUpAndVerify(t, ctx, svc, sender, "bob@example.com")
	ada, err := store.New(pool).GetUserByEmail(ctx, "ada@example.com")
	if err != nil {
		t.Fatalf("GetUserByEmail(ada): %v", err)
	}
	bob, err := store.New(pool).GetUserByEmail(ctx, "bob@example.com")
	if err != nil {
		t.Fatalf("GetUserByEmail(bob): %v", err)
	}

	rp := testRelyingParty()
	authenticator := virtualwebauthn.NewAuthenticator()
	cred := virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)
	authenticator.AddCredential(cred)
	reg, err := svc.BeginPasskeyRegistration(ctx, ada.ID)
	if err != nil {
		t.Fatalf("BeginPasskeyRegistration: %v", err)
	}
	attOptions, err := virtualwebauthn.ParseAttestationOptions(marshalOptions(t, reg.Options))
	if err != nil {
		t.Fatalf("ParseAttestationOptions: %v", err)
	}
	attResponse := virtualwebauthn.CreateAttestationResponse(rp, authenticator, cred, *attOptions)
	adasPasskey, err := svc.FinishPasskeyRegistration(ctx, ada.ID, reg.SessionToken, "Ada's laptop", []byte(attResponse))
	if err != nil {
		t.Fatalf("FinishPasskeyRegistration: %v", err)
	}

	if _, err := svc.RenamePasskey(ctx, bob.ID, adasPasskey.ID, "Mine now"); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("got error %v, want app.ErrNotFound", err)
	}
	if err := svc.DeletePasskey(ctx, bob.ID, adasPasskey.ID); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("got error %v, want app.ErrNotFound", err)
	}
}

func TestCompletePasskeyLogin_SignCounterGoesBackward_Rejected(t *testing.T) {
	ctx := context.Background()
	svc, pool, sender, _ := newTestService(t)
	signUpAndVerify(t, ctx, svc, sender, "ada@example.com")
	user, err := store.New(pool).GetUserByEmail(ctx, "ada@example.com")
	if err != nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}

	rp := testRelyingParty()
	authenticator := virtualwebauthn.NewAuthenticator()
	cred := virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)
	authenticator.AddCredential(cred)

	reg, err := svc.BeginPasskeyRegistration(ctx, user.ID)
	if err != nil {
		t.Fatalf("BeginPasskeyRegistration: %v", err)
	}
	attOptions, err := virtualwebauthn.ParseAttestationOptions(marshalOptions(t, reg.Options))
	if err != nil {
		t.Fatalf("ParseAttestationOptions: %v", err)
	}
	attResponse := virtualwebauthn.CreateAttestationResponse(rp, authenticator, cred, *attOptions)
	if _, err := svc.FinishPasskeyRegistration(ctx, user.ID, reg.SessionToken, "Work laptop", []byte(attResponse)); err != nil {
		t.Fatalf("FinishPasskeyRegistration: %v", err)
	}

	// First sign-in: a real, ascending sign counter.
	cred.Counter = 5
	challenge, err := svc.BeginPasskeyLogin(ctx, "ada@example.com")
	if err != nil {
		t.Fatalf("first BeginPasskeyLogin: %v", err)
	}
	assertOptions, err := virtualwebauthn.ParseAssertionOptions(marshalOptions(t, challenge.Options))
	if err != nil {
		t.Fatalf("ParseAssertionOptions: %v", err)
	}
	assertResponse := virtualwebauthn.CreateAssertionResponse(rp, authenticator, cred, *assertOptions)
	if _, err := svc.CompletePasskeyLogin(ctx, challenge.SessionToken, []byte(assertResponse), nil, "test-agent"); err != nil {
		t.Fatalf("first CompletePasskeyLogin: %v", err)
	}

	// Second sign-in: a lower counter than the one just stored — a
	// cloned authenticator signal. ROADMAP's own test for this task:
	// "a sign counter that goes backwards is rejected."
	cred.Counter = 2
	challenge2, err := svc.BeginPasskeyLogin(ctx, "ada@example.com")
	if err != nil {
		t.Fatalf("second BeginPasskeyLogin: %v", err)
	}
	assertOptions2, err := virtualwebauthn.ParseAssertionOptions(marshalOptions(t, challenge2.Options))
	if err != nil {
		t.Fatalf("ParseAssertionOptions: %v", err)
	}
	assertResponse2 := virtualwebauthn.CreateAssertionResponse(rp, authenticator, cred, *assertOptions2)

	_, err = svc.CompletePasskeyLogin(ctx, challenge2.SessionToken, []byte(assertResponse2), nil, "test-agent")
	if !errors.Is(err, app.ErrBadCredentials) {
		t.Fatalf("got error %v, want app.ErrBadCredentials", err)
	}

	var failures int64
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM security_events WHERE user_id = $1 AND type = 'sign_in_failed'",
		user.ID,
	).Scan(&failures); err != nil {
		t.Fatalf("counting sign_in_failed events: %v", err)
	}
	if failures < 1 {
		t.Fatalf("expected at least one sign_in_failed security event, got %d", failures)
	}
}
