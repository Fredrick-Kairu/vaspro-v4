//go:build integration

package app_test

import (
	"context"
	"errors"
	"net/url"
	"testing"

	"github.com/pquerna/otp/totp"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/app"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/store"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/email"
)

// setPassword signs up, verifies, and sets a password for emailAddr in
// one call — every sign-in test needs an account that can actually
// authenticate with a password.
func setPassword(t *testing.T, ctx context.Context, svc *app.Service, sender *email.Sender, emailAddr, password string) {
	t.Helper()
	signUpAndVerify(t, ctx, svc, sender, emailAddr)
	if err := svc.RequestPasswordReset(ctx, emailAddr); err != nil {
		t.Fatalf("RequestPasswordReset: %v", err)
	}
	sent, _ := sender.LastOfKind(email.KindPasswordReset)
	if err := svc.ConfirmPasswordReset(ctx, emailAddr, sent.Code, password); err != nil {
		t.Fatalf("ConfirmPasswordReset: %v", err)
	}
}

// totpSecretFromURL pulls the base32 secret back out of an
// EnrollTOTP-issued otpauth:// URL — the only way a test (outside
// app/) can get at it, since TOTPEnrollment deliberately doesn't
// return the raw secret on its own.
func totpSecretFromURL(t *testing.T, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parsing enrollment URL: %v", err)
	}
	secret := u.Query().Get("secret")
	if secret == "" {
		t.Fatalf("enrollment URL %q has no secret query param", rawURL)
	}
	return secret
}

func TestSignIn_NoTOTP_ReturnsSession(t *testing.T) {
	ctx := context.Background()
	svc, _, sender, _ := newTestService(t)
	setPassword(t, ctx, svc, sender, "ada@example.com", "a-strong-password")

	result, err := svc.SignIn(ctx, "ada@example.com", "a-strong-password", nil, "test-agent")
	if err != nil {
		t.Fatalf("SignIn: %v", err)
	}
	if result.Session == nil {
		t.Fatal("expected a session for an account with no TOTP enrolled")
	}
	if result.Challenge != "" {
		t.Fatal("expected no challenge for an account with no TOTP enrolled")
	}
}

func TestSignIn_WrongPassword_RejectsWithGenericError(t *testing.T) {
	ctx := context.Background()
	svc, _, sender, _ := newTestService(t)
	setPassword(t, ctx, svc, sender, "ada@example.com", "a-strong-password")

	_, err := svc.SignIn(ctx, "ada@example.com", "wrong-password", nil, "test-agent")
	if !errors.Is(err, app.ErrBadCredentials) {
		t.Fatalf("got error %v, want app.ErrBadCredentials", err)
	}
}

func TestSignIn_UnknownEmail_RejectsWithSameGenericError(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _ := newTestService(t)

	_, err := svc.SignIn(ctx, "nobody@example.com", "whatever-password", nil, "test-agent")
	if !errors.Is(err, app.ErrBadCredentials) {
		t.Fatalf("got error %v, want app.ErrBadCredentials (anti-enumeration)", err)
	}
}

func TestSignIn_RepeatedWrongPassword_EventuallyLocksOutEvenTheRightPassword(t *testing.T) {
	ctx := context.Background()
	svc, _, sender, _ := newTestService(t)
	setPassword(t, ctx, svc, sender, "ada@example.com", "a-strong-password")

	for i := 0; i < 5; i++ {
		if _, err := svc.SignIn(ctx, "ada@example.com", "wrong-password", nil, "test-agent"); !errors.Is(err, app.ErrBadCredentials) {
			t.Fatalf("attempt %d: got error %v, want app.ErrBadCredentials", i, err)
		}
	}

	if _, err := svc.SignIn(ctx, "ada@example.com", "a-strong-password", nil, "test-agent"); !errors.Is(err, app.ErrBadCredentials) {
		t.Fatalf("expected the correct password to also be locked out, got %v", err)
	}
}

func TestSignIn_WithTOTP_ChallengeThenComplete_ReturnsSession(t *testing.T) {
	ctx := context.Background()
	svc, pool, sender, clk := newTestService(t)
	setPassword(t, ctx, svc, sender, "ada@example.com", "a-strong-password")

	q := store.New(pool)
	user, err := q.GetUserByEmail(ctx, "ada@example.com")
	if err != nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}

	enrollment, err := svc.EnrollTOTP(ctx, user.ID)
	if err != nil {
		t.Fatalf("EnrollTOTP: %v", err)
	}
	secret := totpSecretFromURL(t, enrollment.URL)
	code, err := totp.GenerateCode(secret, clk.Now())
	if err != nil {
		t.Fatalf("generating a TOTP code: %v", err)
	}
	if err := svc.ConfirmTOTPEnrollment(ctx, user.ID, code); err != nil {
		t.Fatalf("ConfirmTOTPEnrollment: %v", err)
	}

	result, err := svc.SignIn(ctx, "ada@example.com", "a-strong-password", nil, "test-agent")
	if err != nil {
		t.Fatalf("SignIn: %v", err)
	}
	if result.Challenge == "" {
		t.Fatal("expected a challenge for an account with TOTP enrolled")
	}
	if result.Session != nil {
		t.Fatal("expected no session before the TOTP step completes")
	}

	loginCode, err := totp.GenerateCode(secret, clk.Now())
	if err != nil {
		t.Fatalf("generating a second TOTP code: %v", err)
	}
	session, err := svc.CompleteTOTPChallenge(ctx, result.Challenge, loginCode, nil, "test-agent")
	if err != nil {
		t.Fatalf("CompleteTOTPChallenge: %v", err)
	}
	if session == nil {
		t.Fatal("expected a session after completing the TOTP challenge")
	}
}

func TestCompleteTOTPChallenge_RecoveryCode_ConsumesExactlyOnce(t *testing.T) {
	ctx := context.Background()
	svc, pool, sender, clk := newTestService(t)
	setPassword(t, ctx, svc, sender, "ada@example.com", "a-strong-password")

	q := store.New(pool)
	user, err := q.GetUserByEmail(ctx, "ada@example.com")
	if err != nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}

	enrollment, err := svc.EnrollTOTP(ctx, user.ID)
	if err != nil {
		t.Fatalf("EnrollTOTP: %v", err)
	}
	secret := totpSecretFromURL(t, enrollment.URL)
	code, err := totp.GenerateCode(secret, clk.Now())
	if err != nil {
		t.Fatalf("generating a TOTP code: %v", err)
	}
	if err := svc.ConfirmTOTPEnrollment(ctx, user.ID, code); err != nil {
		t.Fatalf("ConfirmTOTPEnrollment: %v", err)
	}
	recoveryCode := enrollment.RecoveryCodes[0]

	result, err := svc.SignIn(ctx, "ada@example.com", "a-strong-password", nil, "test-agent")
	if err != nil {
		t.Fatalf("first SignIn: %v", err)
	}
	if _, err := svc.CompleteTOTPChallenge(ctx, result.Challenge, recoveryCode, nil, "test-agent"); err != nil {
		t.Fatalf("expected the recovery code to complete the challenge, got %v", err)
	}

	result2, err := svc.SignIn(ctx, "ada@example.com", "a-strong-password", nil, "test-agent")
	if err != nil {
		t.Fatalf("second SignIn: %v", err)
	}
	if _, err := svc.CompleteTOTPChallenge(ctx, result2.Challenge, recoveryCode, nil, "test-agent"); !errors.Is(err, app.ErrTwoFactorInvalid) {
		t.Fatalf("expected the already-used recovery code to be rejected, got %v", err)
	}
}
