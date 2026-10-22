//go:build integration

package app_test

import (
	"context"
	"testing"

	vascrypto "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/crypto"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/app"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/store"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/email"
)

func signUpAndVerify(t *testing.T, ctx context.Context, svc *app.Service, sender *email.Sender, emailAddr string) {
	t.Helper()
	if err := svc.SignUp(ctx, emailAddr, "Ada Lovelace", nil); err != nil {
		t.Fatalf("SignUp: %v", err)
	}
	sent, _ := sender.LastOfKind(email.KindVerification)
	if err := svc.VerifyEmail(ctx, emailAddr, sent.Code); err != nil {
		t.Fatalf("VerifyEmail: %v", err)
	}
}

func TestRequestPasswordReset_SetsFirstPassword(t *testing.T) {
	ctx := context.Background()
	svc, pool, sender, _ := newTestService(t)
	signUpAndVerify(t, ctx, svc, sender, "ada@example.com")

	if err := svc.RequestPasswordReset(ctx, "ada@example.com"); err != nil {
		t.Fatalf("RequestPasswordReset: %v", err)
	}
	sent, ok := sender.LastOfKind(email.KindPasswordReset)
	if !ok {
		t.Fatal("expected a password reset email to have been sent")
	}

	if err := svc.ConfirmPasswordReset(ctx, "ada@example.com", sent.Code, "a-strong-password"); err != nil {
		t.Fatalf("ConfirmPasswordReset: %v", err)
	}

	q := store.New(pool)
	user, err := q.GetUserByEmail(ctx, "ada@example.com")
	if err != nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}
	if !user.PasswordHash.Valid {
		t.Fatal("expected a password hash to be set")
	}
	ok, err = vascrypto.Argon2Verify(user.PasswordHash.String, "a-strong-password")
	if err != nil {
		t.Fatalf("Argon2Verify: %v", err)
	}
	if !ok {
		t.Fatal("expected the stored hash to verify against the new password")
	}
}

func TestRequestPasswordReset_UnknownEmail_StillSucceedsWithoutSendingMail(t *testing.T) {
	ctx := context.Background()
	svc, _, sender, _ := newTestService(t)

	if err := svc.RequestPasswordReset(ctx, "nobody@example.com"); err != nil {
		t.Fatalf("expected no error for an unknown email (anti-enumeration), got %v", err)
	}
	if _, ok := sender.Last(); ok {
		t.Fatal("expected no email to be sent for an unknown address")
	}
}

func TestConfirmPasswordReset_WrongCode_ReturnsInvalidOrExpired(t *testing.T) {
	ctx := context.Background()
	svc, _, sender, _ := newTestService(t)
	signUpAndVerify(t, ctx, svc, sender, "ada@example.com")

	if err := svc.RequestPasswordReset(ctx, "ada@example.com"); err != nil {
		t.Fatalf("RequestPasswordReset: %v", err)
	}
	sent, _ := sender.LastOfKind(email.KindPasswordReset)
	wrongCode := "000000"
	if wrongCode == sent.Code {
		wrongCode = "111111"
	}

	if err := svc.ConfirmPasswordReset(ctx, "ada@example.com", wrongCode, "a-strong-password"); err == nil {
		t.Fatal("expected a wrong code to be rejected")
	}
}

func TestRequestPasswordReset_ReusedForForgotPassword(t *testing.T) {
	ctx := context.Background()
	svc, _, sender, _ := newTestService(t)
	signUpAndVerify(t, ctx, svc, sender, "ada@example.com")

	if err := svc.RequestPasswordReset(ctx, "ada@example.com"); err != nil {
		t.Fatalf("first RequestPasswordReset: %v", err)
	}
	first, _ := sender.LastOfKind(email.KindPasswordReset)
	if err := svc.ConfirmPasswordReset(ctx, "ada@example.com", first.Code, "first-password-1"); err != nil {
		t.Fatalf("setting the first password: %v", err)
	}

	// "I forgot it" uses the exact same mechanism as the initial set.
	if err := svc.RequestPasswordReset(ctx, "ada@example.com"); err != nil {
		t.Fatalf("second RequestPasswordReset: %v", err)
	}
	second, _ := sender.LastOfKind(email.KindPasswordReset)
	if err := svc.ConfirmPasswordReset(ctx, "ada@example.com", second.Code, "second-password-2"); err != nil {
		t.Fatalf("resetting to the second password: %v", err)
	}

	if _, err := svc.SignIn(ctx, "ada@example.com", "second-password-2", nil, ""); err != nil {
		t.Fatalf("expected sign-in with the reset password to succeed: %v", err)
	}
}
