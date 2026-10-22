package app

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	vascrypto "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/crypto"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/ids"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/domain"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/store"
)

// RequestPasswordReset emails a reset code for rawEmail — the same
// mechanism for "set your first password" and "I forgot it": proving
// you control the email is the same bar either way. Always succeeds
// from the caller's point of view unless the input itself is invalid;
// whether the address exists is never revealed (api/ returns the same
// response regardless), the same anti-enumeration principle 1.2
// applied to sign-up.
func (s *Service) RequestPasswordReset(ctx context.Context, rawEmail string) error {
	email, err := domain.NormalizeEmail(rawEmail)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrValidation, err)
	}

	code, err := domain.GenerateVerificationCode()
	if err != nil {
		return fmt.Errorf("app: generating reset code: %w", err)
	}

	var (
		sendTo, sendFullName string
		shouldSend           bool
	)

	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)

		user, err := q.GetUserByEmail(ctx, email)
		if errors.Is(err, pgx.ErrNoRows) {
			// Nothing to do, but still the same response as success —
			// api/ never learns which case this was.
			return nil
		}
		if err != nil {
			return fmt.Errorf("app: looking up user: %w", err)
		}

		if err := q.DeletePendingVerificationTokens(ctx, store.DeletePendingVerificationTokensParams{
			Target:  email,
			Purpose: store.IdentityTokenPurposeResetPassword,
		}); err != nil {
			return fmt.Errorf("app: clearing previous reset tokens: %w", err)
		}

		tokenID, err := ids.New()
		if err != nil {
			return fmt.Errorf("app: generating token id: %w", err)
		}
		if _, err := q.CreateVerificationToken(ctx, store.CreateVerificationTokenParams{
			ID:        tokenID,
			UserID:    user.ID,
			Purpose:   store.IdentityTokenPurposeResetPassword,
			Target:    email,
			TokenHash: hashVerificationCode(email, store.IdentityTokenPurposeResetPassword, code),
			ExpiresAt: s.clk.Now().Add(verificationTTL),
		}); err != nil {
			return fmt.Errorf("app: creating reset token: %w", err)
		}

		sendTo, sendFullName = email, user.FullName
		shouldSend = true
		return nil
	})
	if err != nil {
		return err
	}

	if !shouldSend {
		return nil
	}
	// Sent only after a successful commit, same reasoning as sign-up's
	// verification email.
	if err := s.email.SendPasswordResetCode(ctx, sendTo, sendFullName, code); err != nil {
		return fmt.Errorf("app: sending password reset email: %w", err)
	}
	return nil
}

// ConfirmPasswordReset checks code against the most recently issued
// reset token for rawEmail and, if valid, sets newPassword as the
// account's password (Argon2id). A wrong code, an expired or
// already-used token, and attempts exhausted all return the same
// ErrInvalidOrExpiredCode.
func (s *Service) ConfirmPasswordReset(ctx context.Context, rawEmail, code, newPassword string) error {
	email, err := domain.NormalizeEmail(rawEmail)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrValidation, err)
	}
	if len(newPassword) < minPasswordLength {
		return fmt.Errorf("%w: password must be at least %d characters", ErrValidation, minPasswordLength)
	}
	code = strings.TrimSpace(code)

	hash, err := vascrypto.Argon2Hash(newPassword)
	if err != nil {
		return fmt.Errorf("app: hashing password: %w", err)
	}

	var invalid bool

	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)

		token, err := q.GetPendingVerificationTokenByTarget(ctx, store.GetPendingVerificationTokenByTargetParams{
			Target:  email,
			Purpose: store.IdentityTokenPurposeResetPassword,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			invalid = true
			return nil
		}
		if err != nil {
			return fmt.Errorf("app: looking up reset token: %w", err)
		}

		if token.Attempts >= maxVerificationAttempts {
			invalid = true
			return nil
		}

		wantHash := hashVerificationCode(email, store.IdentityTokenPurposeResetPassword, code)
		if subtle.ConstantTimeCompare(wantHash, token.TokenHash) != 1 {
			if err := q.IncrementVerificationTokenAttempts(ctx, token.ID); err != nil {
				return fmt.Errorf("app: recording a failed attempt: %w", err)
			}
			invalid = true
			return nil
		}

		if err := q.MarkVerificationTokenUsed(ctx, token.ID); err != nil {
			return fmt.Errorf("app: marking token used: %w", err)
		}
		if err := q.SetPasswordHash(ctx, store.SetPasswordHashParams{
			ID:           token.UserID,
			PasswordHash: pgtype.Text{String: hash, Valid: true},
		}); err != nil {
			return fmt.Errorf("app: setting password: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if invalid {
		return ErrInvalidOrExpiredCode
	}
	return nil
}
