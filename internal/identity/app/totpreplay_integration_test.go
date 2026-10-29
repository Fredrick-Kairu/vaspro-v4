//go:build integration

package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/app"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/store"
)

// RFC 6238 §5.2: a verifier must not accept the second attempt of an
// OTP after the first succeeded. Every TOTP check — enrolment
// confirmation, the sign-in challenge and step-up — shares one record
// of the last time step used, so a code is spent wherever it's used.

func TestCompleteTOTPChallenge_CodeUsedToConfirmEnrollment_IsRejected(t *testing.T) {
	ctx := context.Background()
	svc, pool, sender, clk, _ := newTestService(t)
	setPassword(t, ctx, svc, sender, "ada@example.com", "a-strong-password")
	user, err := store.New(pool).GetUserByEmail(ctx, "ada@example.com")
	if err != nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}
	secret := enrollConfirmedTOTP(t, ctx, svc, clk, user.ID)
	spent, err := totp.GenerateCode(secret, clk.Now())
	if err != nil {
		t.Fatalf("generating a TOTP code: %v", err)
	}

	result, err := svc.SignIn(ctx, "ada@example.com", "a-strong-password", nil, "test-agent")
	if err != nil {
		t.Fatalf("SignIn: %v", err)
	}
	if _, err := svc.CompleteTOTPChallenge(ctx, result.Challenge, spent, nil, "test-agent"); !errors.Is(err, app.ErrTwoFactorInvalid) {
		t.Fatalf("replaying the enrolment code: got %v, want app.ErrTwoFactorInvalid", err)
	}

	clk.Advance(30 * time.Second)
	fresh, err := totp.GenerateCode(secret, clk.Now())
	if err != nil {
		t.Fatalf("generating a TOTP code: %v", err)
	}
	if _, err := svc.CompleteTOTPChallenge(ctx, result.Challenge, fresh, nil, "test-agent"); err != nil {
		t.Fatalf("a code from the next time step: %v", err)
	}
}

func TestVerifyStepUpTOTP_ReplayedOrOlderCode_IsRejected(t *testing.T) {
	ctx := context.Background()
	svc, pool, sender, clk, _ := newTestService(t)
	setPassword(t, ctx, svc, sender, "ada@example.com", "a-strong-password")
	user, err := store.New(pool).GetUserByEmail(ctx, "ada@example.com")
	if err != nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}
	secret := enrollConfirmedTOTP(t, ctx, svc, clk, user.ID)

	clk.Advance(30 * time.Second)
	older, err := totp.GenerateCode(secret, clk.Now())
	if err != nil {
		t.Fatalf("generating a TOTP code: %v", err)
	}
	clk.Advance(30 * time.Second)
	signInCode, err := totp.GenerateCode(secret, clk.Now())
	if err != nil {
		t.Fatalf("generating a TOTP code: %v", err)
	}
	result, err := svc.SignIn(ctx, "ada@example.com", "a-strong-password", nil, "test-agent")
	if err != nil {
		t.Fatalf("SignIn: %v", err)
	}
	session, err := svc.CompleteTOTPChallenge(ctx, result.Challenge, signInCode, nil, "test-agent")
	if err != nil {
		t.Fatalf("CompleteTOTPChallenge: %v", err)
	}

	// Both are still inside the ±1 drift window, so only the replay
	// record stops them.
	for name, code := range map[string]string{"the sign-in code again": signInCode, "an older, unused code": older} {
		if err := svc.VerifyStepUpTOTP(ctx, user.ID, session.ID, code); !errors.Is(err, app.ErrTwoFactorInvalid) {
			t.Fatalf("%s: got %v, want app.ErrTwoFactorInvalid", name, err)
		}
	}
	fresh, err := svc.StepUpFresh(ctx, session.ID)
	if err != nil {
		t.Fatalf("StepUpFresh: %v", err)
	}
	if fresh {
		t.Fatal("expected a replayed code not to step the session up")
	}
}
