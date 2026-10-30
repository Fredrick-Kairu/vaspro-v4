//go:build integration

package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/descope/virtualwebauthn"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pquerna/otp/totp"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/adapters"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/app"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/store"
	vasdb "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/testkit"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/email"
	fakeoidc "github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/oidc"
)

const testGoogleClientID = "vaspro-test-client"

type googleHarness struct {
	svc    *app.Service
	pool   *vasdb.Pool
	sender *email.Sender
	clk    *testkit.Clock
	idp    *fakeoidc.Provider
}

func newGoogleHarness(t *testing.T) googleHarness {
	t.Helper()
	idp, err := fakeoidc.New(testGoogleClientID)
	if err != nil {
		t.Fatalf("starting fake OIDC provider: %v", err)
	}
	t.Cleanup(idp.Close)
	provider := adapters.NewGoogleOIDC(idp.Issuer(), testGoogleClientID, "test-secret", "https://localhost/auth/google/callback")
	svc, pool, sender, clk, _ := newTestServiceWithOIDC(t, provider)
	return googleHarness{svc: svc, pool: pool, sender: sender, clk: clk, idp: idp}
}

func googleUser(sub, emailAddr string) fakeoidc.User {
	return fakeoidc.User{Subject: sub, Email: emailAddr, EmailVerified: true, Name: "Grace Hopper"}
}

// googleSignIn runs a whole browser round trip: start, "sign in" at the
// fake provider, callback.
func (h googleHarness) googleSignIn(t *testing.T, user fakeoidc.User, tamper fakeoidc.Tamper) (*app.GoogleSignInResult, error) {
	t.Helper()
	ctx := context.Background()
	auth, err := h.svc.StartGoogleSignIn(ctx)
	if err != nil {
		t.Fatalf("StartGoogleSignIn: %v", err)
	}
	code, state, err := h.idp.Authorize(auth.URL, user, tamper)
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if state != auth.State {
		t.Fatalf("state in URL %q, want %q", state, auth.State)
	}
	return h.svc.CompleteGoogleSignIn(ctx, code, state, nil)
}

func (h googleHarness) sessionRow(t *testing.T, token string) store.IdentitySession {
	t.Helper()
	ctx := context.Background()
	sessionID, _, err := h.svc.ResolveSession(ctx, token)
	if err != nil {
		t.Fatalf("ResolveSession: %v", err)
	}
	row, err := store.New(h.pool).GetSessionByID(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetSessionByID: %v", err)
	}
	return row
}

// signUpWithGoogleAndTOTP signs up through Google and enrols TOTP,
// returning the user, the TOTP secret and the first session's token.
func (h googleHarness) signUpWithGoogleAndTOTP(t *testing.T, user fakeoidc.User) (uuid.UUID, string, string) {
	t.Helper()
	ctx := context.Background()
	result, err := h.googleSignIn(t, user, fakeoidc.Tamper{})
	if err != nil {
		t.Fatalf("Google sign-up: %v", err)
	}
	if result.EnrollmentToken == "" || result.Challenge != "" {
		t.Fatalf("got %+v, want only an enrolment token", result)
	}
	enrollment, err := h.svc.EnrollGoogleTOTP(ctx, result.EnrollmentToken)
	if err != nil {
		t.Fatalf("EnrollGoogleTOTP: %v", err)
	}
	secret := totpSecretFromURL(t, enrollment.URL)
	code, err := totp.GenerateCode(secret, h.clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	session, err := h.svc.ConfirmGoogleTOTPEnrollment(ctx, result.EnrollmentToken, code, nil, "test-agent")
	if err != nil {
		t.Fatalf("ConfirmGoogleTOTPEnrollment: %v", err)
	}
	userRow, err := store.New(h.pool).GetUserByEmail(ctx, user.Email)
	if err != nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}
	return userRow.ID, secret, session.Token
}

func TestGoogleSignIn_RejectsBadTokensAndFlows(t *testing.T) {
	h := newGoogleHarness(t)
	ctx := context.Background()
	user := googleUser("sub-reject", "reject@example.com")

	cases := []struct {
		name   string
		tamper fakeoidc.Tamper
	}{
		{"tampered signature", fakeoidc.Tamper{BadSignature: true}},
		{"signed by a key not in the JWKS", fakeoidc.Tamper{WrongSigningKey: true}},
		{"expired ID token", fakeoidc.Tamper{ExpiresIn: -time.Minute}},
		{"wrong audience", fakeoidc.Tamper{Audience: "someone-elses-client"}},
		{"wrong issuer", fakeoidc.Tamper{Issuer: "https://evil.example"}},
		{"nonce from another flow", fakeoidc.Tamper{Nonce: "a-nonce-this-flow-never-sent"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := h.googleSignIn(t, user, tc.tamper); !errors.Is(err, app.ErrExternalSignInFailed) {
				t.Fatalf("got %v, want ErrExternalSignInFailed", err)
			}
		})
	}

	t.Run("unknown state", func(t *testing.T) {
		if _, err := h.svc.CompleteGoogleSignIn(ctx, "any-code", "never-issued", nil); !errors.Is(err, app.ErrExternalSignInFailed) {
			t.Fatalf("got %v, want ErrExternalSignInFailed", err)
		}
	})
	t.Run("a state is usable once", func(t *testing.T) {
		auth, err := h.svc.StartGoogleSignIn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		code, state, err := h.idp.Authorize(auth.URL, googleUser("sub-replay", "replay@example.com"), fakeoidc.Tamper{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.svc.CompleteGoogleSignIn(ctx, code, state, nil); err != nil {
			t.Fatalf("first callback: %v", err)
		}
		if _, err := h.svc.CompleteGoogleSignIn(ctx, code, state, nil); !errors.Is(err, app.ErrExternalSignInFailed) {
			t.Fatalf("replayed callback: got %v, want ErrExternalSignInFailed", err)
		}
	})
	t.Run("a link flow's state cannot complete a sign-in", func(t *testing.T) {
		auth, err := h.svc.StartGoogleLink(ctx, uuid.New())
		if err != nil {
			t.Fatal(err)
		}
		code, state, err := h.idp.Authorize(auth.URL, user, fakeoidc.Tamper{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.svc.CompleteGoogleSignIn(ctx, code, state, nil); !errors.Is(err, app.ErrExternalSignInFailed) {
			t.Fatalf("got %v, want ErrExternalSignInFailed", err)
		}
	})
	t.Run("an unverified Google email cannot sign up", func(t *testing.T) {
		u := googleUser("sub-unverified", "unverified@example.com")
		u.EmailVerified = false
		if _, err := h.googleSignIn(t, u, fakeoidc.Tamper{}); !errors.Is(err, app.ErrExternalSignInFailed) {
			t.Fatalf("got %v, want ErrExternalSignInFailed", err)
		}
		if _, err := store.New(h.pool).GetUserByEmail(ctx, "unverified@example.com"); err == nil {
			t.Fatal("a user was created for an unverified Google email")
		}
	})
}

func TestGoogleSignIn_NotConfigured_IsNotFound(t *testing.T) {
	svc, _, _, _, _ := newTestService(t)
	if _, err := svc.StartGoogleSignIn(context.Background()); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

func TestGoogleSignUp_EnrolTOTP_ThenSignInNeedsTOTPAndNeverStepsUp(t *testing.T) {
	h := newGoogleHarness(t)
	ctx := context.Background()
	user := googleUser("sub-grace", "grace@example.com")

	userID, secret, firstToken := h.signUpWithGoogleAndTOTP(t, user)

	t.Run("sign-up creates a verified, password-less user and publishes the sign-up events", func(t *testing.T) {
		row, err := store.New(h.pool).GetUserByID(ctx, userID)
		if err != nil {
			t.Fatal(err)
		}
		if !row.EmailVerifiedAt.Valid || row.PasswordHash.Valid {
			t.Fatalf("got verified=%v password=%v, want verified and no password", row.EmailVerifiedAt.Valid, row.PasswordHash.Valid)
		}
		types := outboxEventTypes(t, ctx, h.pool)
		if len(types) != 2 || types[0] != "user.registered" || types[1] != "user.email_verified" {
			t.Fatalf("got outbox %v, want [user.registered user.email_verified]", types)
		}
	})
	t.Run("the first session records auth_method google and no step-up", func(t *testing.T) {
		row := h.sessionRow(t, firstToken)
		if row.AuthMethod != "google" || row.StepUpAt.Valid {
			t.Fatalf("got auth_method=%q step_up=%v, want google and no step-up", row.AuthMethod, row.StepUpAt.Valid)
		}
	})

	t.Run("signing in again returns a TOTP challenge, never a session", func(t *testing.T) {
		result, err := h.googleSignIn(t, user, fakeoidc.Tamper{})
		if err != nil {
			t.Fatalf("Google sign-in: %v", err)
		}
		if result.Challenge == "" || result.EnrollmentToken != "" {
			t.Fatalf("got %+v, want a challenge only", result)
		}
		if len(result.SecondFactors) != 1 || result.SecondFactors[0] != "totp" {
			t.Fatalf("got second factors %v, want [totp]", result.SecondFactors)
		}

		h.clk.Advance(30 * time.Second)
		code, err := totp.GenerateCode(secret, h.clk.Now())
		if err != nil {
			t.Fatal(err)
		}
		session, err := h.svc.CompleteTOTPChallenge(ctx, result.Challenge, code, nil, "test-agent")
		if err != nil {
			t.Fatalf("CompleteTOTPChallenge: %v", err)
		}
		row := h.sessionRow(t, session.Token)
		if row.AuthMethod != "google" {
			t.Fatalf("got auth_method %q, want google", row.AuthMethod)
		}
		fresh, err := h.svc.StepUpFresh(ctx, row.ID)
		if err != nil {
			t.Fatal(err)
		}
		if fresh {
			t.Fatal("a Google sign-in counted as step-up")
		}
	})

	t.Run("a changed Google email still finds the same user by sub", func(t *testing.T) {
		result, err := h.googleSignIn(t, googleUser("sub-grace", "grace.hopper@other.example"), fakeoidc.Tamper{})
		if err != nil {
			t.Fatalf("Google sign-in: %v", err)
		}
		if result.Challenge == "" {
			t.Fatalf("got %+v, want a challenge for the existing user", result)
		}
	})
}

func TestGoogleSignUp_EnrolmentToken(t *testing.T) {
	h := newGoogleHarness(t)
	ctx := context.Background()

	t.Run("an enrolment token is refused once the user has a second factor", func(t *testing.T) {
		result, err := h.googleSignIn(t, googleUser("sub-once", "once@example.com"), fakeoidc.Tamper{})
		if err != nil {
			t.Fatal(err)
		}
		enrollment, err := h.svc.EnrollGoogleTOTP(ctx, result.EnrollmentToken)
		if err != nil {
			t.Fatal(err)
		}
		code, err := totp.GenerateCode(totpSecretFromURL(t, enrollment.URL), h.clk.Now())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.svc.ConfirmGoogleTOTPEnrollment(ctx, result.EnrollmentToken, code, nil, "test-agent"); err != nil {
			t.Fatalf("ConfirmGoogleTOTPEnrollment: %v", err)
		}
		if _, err := h.svc.EnrollGoogleTOTP(ctx, result.EnrollmentToken); !errors.Is(err, app.ErrTwoFactorInvalid) {
			t.Fatalf("re-enrolling with the same token: got %v, want ErrTwoFactorInvalid", err)
		}
	})
	t.Run("a sign-in challenge is not an enrolment token", func(t *testing.T) {
		_, _, _ = h.signUpWithGoogleAndTOTP(t, googleUser("sub-chal", "chal@example.com"))
		result, err := h.googleSignIn(t, googleUser("sub-chal", "chal@example.com"), fakeoidc.Tamper{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.svc.EnrollGoogleTOTP(ctx, result.Challenge); !errors.Is(err, app.ErrTwoFactorInvalid) {
			t.Fatalf("got %v, want ErrTwoFactorInvalid", err)
		}
	})
	t.Run("a wrong enrolment code creates no session", func(t *testing.T) {
		result, err := h.googleSignIn(t, googleUser("sub-wrong", "wrong@example.com"), fakeoidc.Tamper{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.svc.EnrollGoogleTOTP(ctx, result.EnrollmentToken); err != nil {
			t.Fatal(err)
		}
		if _, err := h.svc.ConfirmGoogleTOTPEnrollment(ctx, result.EnrollmentToken, "000000", nil, "test-agent"); !errors.Is(err, app.ErrTwoFactorInvalid) {
			t.Fatalf("got %v, want ErrTwoFactorInvalid", err)
		}
	})
}

func TestGoogleSignUp_EnrolPasskey_ThenPasskeyAsSecondFactor(t *testing.T) {
	h := newGoogleHarness(t)
	ctx := context.Background()
	user := googleUser("sub-pk", "pk@example.com")

	result, err := h.googleSignIn(t, user, fakeoidc.Tamper{})
	if err != nil {
		t.Fatal(err)
	}
	rp := testRelyingParty()
	authenticator := virtualwebauthn.NewAuthenticator()
	cred := virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)
	authenticator.AddCredential(cred)

	reg, err := h.svc.BeginGooglePasskeyEnrollment(ctx, result.EnrollmentToken)
	if err != nil {
		t.Fatalf("BeginGooglePasskeyEnrollment: %v", err)
	}
	attOptions, err := virtualwebauthn.ParseAttestationOptions(marshalOptions(t, reg.Options))
	if err != nil {
		t.Fatal(err)
	}
	attResponse := virtualwebauthn.CreateAttestationResponse(rp, authenticator, cred, *attOptions)
	session, err := h.svc.FinishGooglePasskeyEnrollment(ctx, result.EnrollmentToken, reg.SessionToken, "Laptop", []byte(attResponse), nil, "test-agent")
	if err != nil {
		t.Fatalf("FinishGooglePasskeyEnrollment: %v", err)
	}
	if row := h.sessionRow(t, session.Token); row.AuthMethod != "google" {
		t.Fatalf("got auth_method %q, want google", row.AuthMethod)
	}

	again, err := h.googleSignIn(t, user, fakeoidc.Tamper{})
	if err != nil {
		t.Fatal(err)
	}
	if len(again.SecondFactors) != 1 || again.SecondFactors[0] != "passkey" {
		t.Fatalf("got second factors %v, want [passkey]", again.SecondFactors)
	}
	assertion, err := h.svc.BeginGooglePasskeySecondFactor(ctx, again.Challenge)
	if err != nil {
		t.Fatalf("BeginGooglePasskeySecondFactor: %v", err)
	}
	assertOptions, err := virtualwebauthn.ParseAssertionOptions(marshalOptions(t, assertion.Options))
	if err != nil {
		t.Fatal(err)
	}
	assertResponse := virtualwebauthn.CreateAssertionResponse(rp, authenticator, cred, *assertOptions)
	second, err := h.svc.CompleteGooglePasskeySecondFactor(ctx, again.Challenge, assertion.SessionToken, []byte(assertResponse), nil, "test-agent")
	if err != nil {
		t.Fatalf("CompleteGooglePasskeySecondFactor: %v", err)
	}
	row := h.sessionRow(t, second.Token)
	if row.AuthMethod != "google" || row.StepUpAt.Valid {
		t.Fatalf("got auth_method=%q step_up=%v, want google and no step-up", row.AuthMethod, row.StepUpAt.Valid)
	}

	t.Run("a password challenge cannot drive the Google passkey step", func(t *testing.T) {
		setPassword(t, ctx, h.svc, h.sender, "pw@example.com", "a-strong-password")
		pwUser, err := store.New(h.pool).GetUserByEmail(ctx, "pw@example.com")
		if err != nil {
			t.Fatal(err)
		}
		enrollConfirmedTOTP(t, ctx, h.svc, h.clk, pwUser.ID)
		signIn, err := h.svc.SignIn(ctx, "pw@example.com", "a-strong-password", nil, "test-agent")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.svc.BeginGooglePasskeySecondFactor(ctx, signIn.Challenge); !errors.Is(err, app.ErrTwoFactorInvalid) {
			t.Fatalf("got %v, want ErrTwoFactorInvalid", err)
		}
	})
}

func TestGoogleSignIn_ExistingEmailIsNeverLinkedSilently(t *testing.T) {
	h := newGoogleHarness(t)
	ctx := context.Background()
	setPassword(t, ctx, h.svc, h.sender, "ada@example.com", "a-strong-password")
	google := googleUser("sub-ada", "ada@example.com")

	if _, err := h.googleSignIn(t, google, fakeoidc.Tamper{}); !errors.Is(err, app.ErrAccountExistsLinkRequired) {
		t.Fatalf("got %v, want ErrAccountExistsLinkRequired", err)
	}
	ada, err := store.New(h.pool).GetUserByEmail(ctx, "ada@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.GetGoogleLink(ctx, ada.ID); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("a Google identity was linked: %v", err)
	}

	// Ada signs in the usual way, then links Google herself.
	auth, err := h.svc.StartGoogleLink(ctx, ada.ID)
	if err != nil {
		t.Fatal(err)
	}
	code, state, err := h.idp.Authorize(auth.URL, google, fakeoidc.Tamper{})
	if err != nil {
		t.Fatal(err)
	}
	linked, err := h.svc.CompleteGoogleLink(ctx, ada.ID, code, state, nil)
	if err != nil {
		t.Fatalf("CompleteGoogleLink: %v", err)
	}
	if linked.Email != "ada@example.com" || linked.Provider != "google" {
		t.Fatalf("got %+v", linked)
	}

	// With no second factor yet, Google sign-in asks her to enrol one.
	result, err := h.googleSignIn(t, google, fakeoidc.Tamper{})
	if err != nil {
		t.Fatalf("Google sign-in after linking: %v", err)
	}
	if result.EnrollmentToken == "" {
		t.Fatalf("got %+v, want an enrolment token", result)
	}
}

func TestGoogleLink(t *testing.T) {
	h := newGoogleHarness(t)
	ctx := context.Background()
	setPassword(t, ctx, h.svc, h.sender, "alice@example.com", "a-strong-password")
	setPassword(t, ctx, h.svc, h.sender, "bob@example.com", "a-strong-password")
	q := store.New(h.pool)
	alice, _ := q.GetUserByEmail(ctx, "alice@example.com")
	bob, _ := q.GetUserByEmail(ctx, "bob@example.com")

	link := func(starter, completer uuid.UUID, user fakeoidc.User) (*app.LinkedIdentity, error) {
		auth, err := h.svc.StartGoogleLink(ctx, starter)
		if err != nil {
			t.Fatal(err)
		}
		code, state, err := h.idp.Authorize(auth.URL, user, fakeoidc.Tamper{})
		if err != nil {
			t.Fatal(err)
		}
		return h.svc.CompleteGoogleLink(ctx, completer, code, state, nil)
	}

	t.Run("a link started by one user cannot be completed by another", func(t *testing.T) {
		if _, err := link(alice.ID, bob.ID, googleUser("sub-csrf", "csrf@example.com")); !errors.Is(err, app.ErrExternalSignInFailed) {
			t.Fatalf("got %v, want ErrExternalSignInFailed", err)
		}
		if _, err := h.svc.GetGoogleLink(ctx, bob.ID); !errors.Is(err, app.ErrNotFound) {
			t.Fatal("bob got a Google link from alice's flow")
		}
	})
	t.Run("a Google account linked to someone else is refused", func(t *testing.T) {
		if _, err := link(alice.ID, alice.ID, googleUser("sub-shared", "shared@example.com")); err != nil {
			t.Fatalf("alice linking: %v", err)
		}
		if _, err := link(bob.ID, bob.ID, googleUser("sub-shared", "shared@example.com")); !errors.Is(err, app.ErrIdentityLinkedElsewhere) {
			t.Fatalf("got %v, want ErrIdentityLinkedElsewhere", err)
		}
	})
	t.Run("a second Google account for the same user is refused", func(t *testing.T) {
		if _, err := link(alice.ID, alice.ID, googleUser("sub-second", "second@example.com")); !errors.Is(err, app.ErrValidation) {
			t.Fatalf("got %v, want ErrValidation", err)
		}
	})
	t.Run("linking and unlinking are security events", func(t *testing.T) {
		if err := h.svc.UnlinkGoogle(ctx, alice.ID, nil); err != nil {
			t.Fatalf("UnlinkGoogle: %v", err)
		}
		events, err := h.svc.ListSecurityEvents(ctx, alice.ID)
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		for _, e := range events {
			seen[e.Type] = true
		}
		if !seen["identity_linked"] || !seen["identity_unlinked"] {
			t.Fatalf("got event types %v, want identity_linked and identity_unlinked", seen)
		}
	})
}

func TestUnlinkGoogle_RefusedWhenItIsTheOnlyWayIn(t *testing.T) {
	h := newGoogleHarness(t)
	ctx := context.Background()
	userID, _, _ := h.signUpWithGoogleAndTOTP(t, googleUser("sub-only", "only@example.com"))

	if err := h.svc.UnlinkGoogle(ctx, userID, nil); !errors.Is(err, app.ErrOnlySignInMethod) {
		t.Fatalf("got %v, want ErrOnlySignInMethod", err)
	}

	// Setting a password (the reset flow) gives another way in.
	if err := h.svc.RequestPasswordReset(ctx, "only@example.com"); err != nil {
		t.Fatal(err)
	}
	sent, _ := h.sender.LastOfKind(email.KindPasswordReset)
	if err := h.svc.ConfirmPasswordReset(ctx, "only@example.com", sent.Code, "a-strong-password"); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.UnlinkGoogle(ctx, userID, nil); err != nil {
		t.Fatalf("UnlinkGoogle with a password set: %v", err)
	}
}

func TestGoogleSignIn_SSODomainIsRefused(t *testing.T) {
	h := newGoogleHarness(t)
	ctx := context.Background()
	tenant := uuid.New()
	err := h.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		if _, err := q.CreateSSOConnection(ctx, store.CreateSSOConnectionParams{ID: uuid.New(), TenantID: tenant, Protocol: "oidc", Domain: "acme.example", Config: []byte(`{}`)}); err != nil {
			return err
		}
		if _, err := q.CreateSSOConnection(ctx, store.CreateSSOConnectionParams{ID: uuid.New(), TenantID: tenant, Protocol: "oidc", Domain: "off.example", Config: []byte(`{}`)}); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE sso_connections SET enabled = false WHERE domain = 'off.example'`)
		return err
	})
	if err != nil {
		t.Fatalf("seeding SSO connections: %v", err)
	}

	if _, err := h.googleSignIn(t, googleUser("sub-acme", "jo@acme.example"), fakeoidc.Tamper{}); !errors.Is(err, app.ErrSSORequired) {
		t.Fatalf("enabled SSO domain: got %v, want ErrSSORequired", err)
	}
	if _, err := store.New(h.pool).GetUserByEmail(ctx, "jo@acme.example"); err == nil {
		t.Fatal("a user was created on an SSO domain")
	}
	result, err := h.googleSignIn(t, googleUser("sub-off", "jo@off.example"), fakeoidc.Tamper{})
	if err != nil || result.EnrollmentToken == "" {
		t.Fatalf("disabled SSO domain: got %+v, %v, want a normal sign-up", result, err)
	}
}

func TestExistingSignInPaths_RecordTheirAuthMethod(t *testing.T) {
	h := newGoogleHarness(t)
	ctx := context.Background()
	setPassword(t, ctx, h.svc, h.sender, "pw@example.com", "a-strong-password")
	result, err := h.svc.SignIn(ctx, "pw@example.com", "a-strong-password", nil, "test-agent")
	if err != nil {
		t.Fatal(err)
	}
	if row := h.sessionRow(t, result.Session.Token); row.AuthMethod != "password" {
		t.Fatalf("password sign-in: got auth_method %q", row.AuthMethod)
	}
}
