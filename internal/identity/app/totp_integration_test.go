//go:build integration

package app_test

import (
	"context"
	"errors"
	"testing"

	"github.com/pquerna/otp/totp"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/app"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/store"
)

func TestEnrollTOTP_ReturnsScannableURLAndEightRecoveryCodes(t *testing.T) {
	ctx := context.Background()
	svc, pool, sender, _, _ := newTestService(t)
	signUpAndVerify(t, ctx, svc, sender, "ada@example.com")

	user, err := store.New(pool).GetUserByEmail(ctx, "ada@example.com")
	if err != nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}

	enrollment, err := svc.EnrollTOTP(ctx, user.ID)
	if err != nil {
		t.Fatalf("EnrollTOTP: %v", err)
	}
	if enrollment.URL == "" {
		t.Fatal("expected a non-empty enrollment URL")
	}
	if len(enrollment.RecoveryCodes) != 8 {
		t.Fatalf("got %d recovery codes, want 8", len(enrollment.RecoveryCodes))
	}
}

func TestConfirmTOTPEnrollment_WrongCode_ReturnsTwoFactorInvalid(t *testing.T) {
	ctx := context.Background()
	svc, pool, sender, _, _ := newTestService(t)
	signUpAndVerify(t, ctx, svc, sender, "ada@example.com")

	user, err := store.New(pool).GetUserByEmail(ctx, "ada@example.com")
	if err != nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}
	if _, err := svc.EnrollTOTP(ctx, user.ID); err != nil {
		t.Fatalf("EnrollTOTP: %v", err)
	}

	if err := svc.ConfirmTOTPEnrollment(ctx, user.ID, "000000"); !errors.Is(err, app.ErrTwoFactorInvalid) {
		t.Fatalf("got error %v, want app.ErrTwoFactorInvalid", err)
	}
}

func TestEnrollTOTP_ReplacesAnUnconfirmedPriorEnrollment(t *testing.T) {
	ctx := context.Background()
	svc, pool, sender, clk, _ := newTestService(t)
	signUpAndVerify(t, ctx, svc, sender, "ada@example.com")

	user, err := store.New(pool).GetUserByEmail(ctx, "ada@example.com")
	if err != nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}

	first, err := svc.EnrollTOTP(ctx, user.ID)
	if err != nil {
		t.Fatalf("first EnrollTOTP: %v", err)
	}
	second, err := svc.EnrollTOTP(ctx, user.ID)
	if err != nil {
		t.Fatalf("second EnrollTOTP: %v", err)
	}

	firstSecret := totpSecretFromURL(t, first.URL)
	secondSecret := totpSecretFromURL(t, second.URL)
	if firstSecret == secondSecret {
		t.Fatal("expected a fresh secret on re-enrollment")
	}

	// Confirming against the superseded first secret must fail — only
	// the second (most recent) enrollment is live.
	firstCode, err := totp.GenerateCode(firstSecret, clk.Now())
	if err != nil {
		t.Fatalf("generating a code for the first secret: %v", err)
	}
	if err := svc.ConfirmTOTPEnrollment(ctx, user.ID, firstCode); !errors.Is(err, app.ErrTwoFactorInvalid) {
		t.Fatalf("expected the superseded enrollment's code to be rejected, got %v", err)
	}

	secondCode, err := totp.GenerateCode(secondSecret, clk.Now())
	if err != nil {
		t.Fatalf("generating a code for the second secret: %v", err)
	}
	if err := svc.ConfirmTOTPEnrollment(ctx, user.ID, secondCode); err != nil {
		t.Fatalf("expected the current enrollment's code to confirm, got %v", err)
	}
}
