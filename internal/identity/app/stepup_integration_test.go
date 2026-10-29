//go:build integration

package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pquerna/otp/totp"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/app"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/store"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/testkit"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/email"
)

// enrollConfirmedTOTP enrolls and confirms TOTP for userID, returning
// the base32 secret so the test can keep generating valid codes.
func enrollConfirmedTOTP(t *testing.T, ctx context.Context, svc *app.Service, clk *testkit.Clock, userID uuid.UUID) string {
	t.Helper()
	enrollment, err := svc.EnrollTOTP(ctx, userID)
	if err != nil {
		t.Fatalf("EnrollTOTP: %v", err)
	}
	secret := totpSecretFromURL(t, enrollment.URL)
	code, err := totp.GenerateCode(secret, clk.Now())
	if err != nil {
		t.Fatalf("generating a TOTP code: %v", err)
	}
	if err := svc.ConfirmTOTPEnrollment(ctx, userID, code); err != nil {
		t.Fatalf("ConfirmTOTPEnrollment: %v", err)
	}
	return secret
}

func TestVerifyStepUpTOTP_HappyPath_MarksSessionFresh(t *testing.T) {
	ctx := context.Background()
	svc, pool, sender, clk, _ := newTestService(t)
	setPassword(t, ctx, svc, sender, "ada@example.com", "a-strong-password")
	user, err := store.New(pool).GetUserByEmail(ctx, "ada@example.com")
	if err != nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}
	secret := enrollConfirmedTOTP(t, ctx, svc, clk, user.ID)

	// Enrolling and confirming TOTP goes through SignIn's own two-step
	// challenge, not a plain session — use SignIn+CompleteTOTPChallenge
	// to get a real session bound to this account.
	result, err := svc.SignIn(ctx, "ada@example.com", "a-strong-password", nil, "test-agent")
	if err != nil {
		t.Fatalf("SignIn: %v", err)
	}
	code, err := totp.GenerateCode(secret, clk.Now())
	if err != nil {
		t.Fatalf("generating a TOTP code: %v", err)
	}
	session, err := svc.CompleteTOTPChallenge(ctx, result.Challenge, code, nil, "test-agent")
	if err != nil {
		t.Fatalf("CompleteTOTPChallenge: %v", err)
	}

	fresh, err := svc.StepUpFresh(ctx, session.ID)
	if err != nil {
		t.Fatalf("StepUpFresh (before step-up): %v", err)
	}
	if fresh {
		t.Fatal("expected a brand-new session to not already be stepped up")
	}

	stepUpCode, err := totp.GenerateCode(secret, clk.Now())
	if err != nil {
		t.Fatalf("generating a step-up TOTP code: %v", err)
	}
	if err := svc.VerifyStepUpTOTP(ctx, user.ID, session.ID, stepUpCode); err != nil {
		t.Fatalf("VerifyStepUpTOTP: %v", err)
	}

	fresh, err = svc.StepUpFresh(ctx, session.ID)
	if err != nil {
		t.Fatalf("StepUpFresh (after step-up): %v", err)
	}
	if !fresh {
		t.Fatal("expected the session to be stepped up after a correct TOTP code")
	}
}

func TestVerifyStepUpTOTP_RepeatedWrongCode_EventuallyLocksOut(t *testing.T) {
	ctx := context.Background()
	svc, pool, sender, clk, _ := newTestService(t)
	setPassword(t, ctx, svc, sender, "ada@example.com", "a-strong-password")
	user, err := store.New(pool).GetUserByEmail(ctx, "ada@example.com")
	if err != nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}
	secret := enrollConfirmedTOTP(t, ctx, svc, clk, user.ID)

	result, err := svc.SignIn(ctx, "ada@example.com", "a-strong-password", nil, "test-agent")
	if err != nil {
		t.Fatalf("SignIn: %v", err)
	}
	code, err := totp.GenerateCode(secret, clk.Now())
	if err != nil {
		t.Fatalf("generating a TOTP code: %v", err)
	}
	session, err := svc.CompleteTOTPChallenge(ctx, result.Challenge, code, nil, "test-agent")
	if err != nil {
		t.Fatalf("CompleteTOTPChallenge: %v", err)
	}

	for i := 0; i < 5; i++ {
		if err := svc.VerifyStepUpTOTP(ctx, user.ID, session.ID, "000000"); !errors.Is(err, app.ErrTwoFactorInvalid) {
			t.Fatalf("attempt %d: got error %v, want app.ErrTwoFactorInvalid", i, err)
		}
	}

	rightCode, err := totp.GenerateCode(secret, clk.Now())
	if err != nil {
		t.Fatalf("generating the correct TOTP code: %v", err)
	}
	if err := svc.VerifyStepUpTOTP(ctx, user.ID, session.ID, rightCode); !errors.Is(err, app.ErrTwoFactorInvalid) {
		t.Fatalf("expected the correct code to also be locked out, got %v", err)
	}
}

func TestStepUpEmail_HappyPath_MarksSessionFresh(t *testing.T) {
	ctx := context.Background()
	svc, _, sender, _, _ := newTestService(t)
	setPassword(t, ctx, svc, sender, "ada@example.com", "a-strong-password")

	result, err := svc.SignIn(ctx, "ada@example.com", "a-strong-password", nil, "test-agent")
	if err != nil {
		t.Fatalf("SignIn: %v", err)
	}
	session := result.Session
	_, userID, err := svc.ResolveSession(ctx, session.Token)
	if err != nil {
		t.Fatalf("ResolveSession: %v", err)
	}

	if err := svc.RequestStepUpEmail(ctx, userID); err != nil {
		t.Fatalf("RequestStepUpEmail: %v", err)
	}
	sent, ok := sender.LastOfKind(email.KindStepUp)
	if !ok {
		t.Fatal("expected a step-up email to have been sent")
	}

	if err := svc.ConfirmStepUpEmail(ctx, userID, session.ID, sent.Code); err != nil {
		t.Fatalf("ConfirmStepUpEmail: %v", err)
	}

	fresh, err := svc.StepUpFresh(ctx, session.ID)
	if err != nil {
		t.Fatalf("StepUpFresh: %v", err)
	}
	if !fresh {
		t.Fatal("expected the session to be stepped up after confirming the emailed code")
	}
}

func TestConfirmStepUpEmail_WrongCode_IncrementsAttemptsWithoutSteppingUp(t *testing.T) {
	ctx := context.Background()
	svc, _, sender, _, _ := newTestService(t)
	setPassword(t, ctx, svc, sender, "ada@example.com", "a-strong-password")

	result, err := svc.SignIn(ctx, "ada@example.com", "a-strong-password", nil, "test-agent")
	if err != nil {
		t.Fatalf("SignIn: %v", err)
	}
	session := result.Session
	_, userID, err := svc.ResolveSession(ctx, session.Token)
	if err != nil {
		t.Fatalf("ResolveSession: %v", err)
	}

	if err := svc.RequestStepUpEmail(ctx, userID); err != nil {
		t.Fatalf("RequestStepUpEmail: %v", err)
	}
	sent, _ := sender.LastOfKind(email.KindStepUp)
	wrongCode := "000000"
	if wrongCode == sent.Code {
		wrongCode = "111111"
	}

	if err := svc.ConfirmStepUpEmail(ctx, userID, session.ID, wrongCode); !errors.Is(err, app.ErrInvalidOrExpiredCode) {
		t.Fatalf("got error %v, want app.ErrInvalidOrExpiredCode", err)
	}

	fresh, err := svc.StepUpFresh(ctx, session.ID)
	if err != nil {
		t.Fatalf("StepUpFresh: %v", err)
	}
	if fresh {
		t.Fatal("expected a wrong code to not step up the session")
	}
}

func TestStepUpFresh_ExpiresAfterWindow(t *testing.T) {
	ctx := context.Background()
	svc, _, sender, clk, _ := newTestService(t)
	setPassword(t, ctx, svc, sender, "ada@example.com", "a-strong-password")

	result, err := svc.SignIn(ctx, "ada@example.com", "a-strong-password", nil, "test-agent")
	if err != nil {
		t.Fatalf("SignIn: %v", err)
	}
	session := result.Session
	_, userID, err := svc.ResolveSession(ctx, session.Token)
	if err != nil {
		t.Fatalf("ResolveSession: %v", err)
	}

	if err := svc.RequestStepUpEmail(ctx, userID); err != nil {
		t.Fatalf("RequestStepUpEmail: %v", err)
	}
	sent, _ := sender.LastOfKind(email.KindStepUp)
	if err := svc.ConfirmStepUpEmail(ctx, userID, session.ID, sent.Code); err != nil {
		t.Fatalf("ConfirmStepUpEmail: %v", err)
	}

	fresh, err := svc.StepUpFresh(ctx, session.ID)
	if err != nil {
		t.Fatalf("StepUpFresh (immediately after): %v", err)
	}
	if !fresh {
		t.Fatal("expected the session to be stepped up immediately after confirming")
	}

	clk.Advance(6 * time.Minute)

	fresh, err = svc.StepUpFresh(ctx, session.ID)
	if err != nil {
		t.Fatalf("StepUpFresh (after the window): %v", err)
	}
	if fresh {
		t.Fatal("expected step-up to no longer be fresh after the 5-minute window")
	}
}
