// Package app holds identity's use cases: validate through domain/,
// then write through store/ and platform/outbox inside one transaction.
package app

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	platformevents "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/events"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/ids"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/outbox"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/domain"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/store"
)

// SignUp starts (or restarts) sign-up for rawEmail. It always succeeds
// from the caller's point of view unless the input itself is invalid —
// whether the address is new, already pending verification, or already
// verified is never revealed (api/ returns the same response for all
// three, to avoid email enumeration).
func (s *Service) SignUp(ctx context.Context, rawEmail, fullName string, rawPhone *string) error {
	email, err := domain.NormalizeEmail(rawEmail)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrValidation, err)
	}

	fullName = strings.TrimSpace(fullName)
	if fullName == "" {
		return fmt.Errorf("%w: full name must not be empty", ErrValidation)
	}

	var phone pgtype.Text
	if rawPhone != nil && strings.TrimSpace(*rawPhone) != "" {
		validated, err := domain.ValidatePhoneE164(*rawPhone)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrValidation, err)
		}
		phone = pgtype.Text{String: validated, Valid: true}
	}

	code, err := domain.GenerateVerificationCode()
	if err != nil {
		return fmt.Errorf("app: generating verification code: %w", err)
	}

	var (
		sendTo       string
		sendFullName string
		shouldSend   bool
	)

	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)

		var userID uuid.UUID
		existing, err := q.GetUserByEmail(ctx, email)
		switch {
		case err == nil:
			if existing.EmailVerifiedAt.Valid {
				// Already verified: silently do nothing. The caller
				// gets the same response as every other case.
				return nil
			}
			userID = existing.ID
			sendFullName = existing.FullName
			if existing.FullName == "" {
				// Invited before signing up (task 2.3b): the invitation
				// created the user without a name.
				if err := q.SetUserFullNameIfEmpty(ctx, store.SetUserFullNameIfEmptyParams{ID: existing.ID, FullName: fullName}); err != nil {
					return fmt.Errorf("app: setting the invited user's name: %w", err)
				}
				sendFullName = fullName
			}
		case errors.Is(err, pgx.ErrNoRows):
			id, err := ids.New()
			if err != nil {
				return fmt.Errorf("app: generating user id: %w", err)
			}
			created, err := q.CreateUser(ctx, store.CreateUserParams{
				ID:        id,
				Email:     email,
				FullName:  fullName,
				PhoneE164: phone,
			})
			if err != nil {
				return fmt.Errorf("app: creating user: %w", err)
			}
			userID = created.ID
			sendFullName = created.FullName

			eventID, err := ids.New()
			if err != nil {
				return fmt.Errorf("app: generating event id: %w", err)
			}
			env, err := platformevents.New(eventID, "user.registered", 1, uuid.Nil, s.clk.Now(), "", map[string]any{
				"user_id": userID,
				"email":   email,
			})
			if err != nil {
				return fmt.Errorf("app: building user.registered event: %w", err)
			}
			if err := outbox.Write(ctx, tx, "user", userID, env); err != nil {
				return fmt.Errorf("app: writing user.registered event: %w", err)
			}
		default:
			return fmt.Errorf("app: looking up user: %w", err)
		}

		if err := q.DeletePendingVerificationTokens(ctx, store.DeletePendingVerificationTokensParams{
			Target:  email,
			Purpose: store.IdentityTokenPurposeVerifyEmail,
		}); err != nil {
			return fmt.Errorf("app: clearing previous verification tokens: %w", err)
		}

		tokenID, err := ids.New()
		if err != nil {
			return fmt.Errorf("app: generating token id: %w", err)
		}
		if _, err := q.CreateVerificationToken(ctx, store.CreateVerificationTokenParams{
			ID:        tokenID,
			UserID:    userID,
			Purpose:   store.IdentityTokenPurposeVerifyEmail,
			Target:    email,
			TokenHash: hashVerificationCode(email, store.IdentityTokenPurposeVerifyEmail, code),
			ExpiresAt: s.clk.Now().Add(verificationTTL),
		}); err != nil {
			return fmt.Errorf("app: creating verification token: %w", err)
		}

		sendTo = email
		shouldSend = true
		return nil
	})
	if err != nil {
		return err
	}

	if !shouldSend {
		return nil
	}

	// Sent only after a successful commit — an email for a sign-up that
	// got rolled back would be worse than a sign-up that silently needs
	// a retry.
	if err := s.email.SendVerificationCode(ctx, sendTo, sendFullName, code); err != nil {
		return fmt.Errorf("app: sending verification email: %w", err)
	}
	return nil
}

// VerifyEmail checks code against the most recently issued pending
// token for rawEmail. A wrong code, an expired or already-used token,
// and too many prior attempts all return the same ErrInvalidOrExpiredCode.
func (s *Service) VerifyEmail(ctx context.Context, rawEmail, code string) error {
	email, err := domain.NormalizeEmail(rawEmail)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrValidation, err)
	}
	code = strings.TrimSpace(code)

	// invalid is decided inside the transaction but only acted on after
	// it commits: pgx.BeginFunc rolls back on ANY non-nil return, and a
	// "wrong code" outcome still has to commit its own side effect (the
	// attempts increment) — returning ErrInvalidOrExpiredCode directly
	// from the closure would silently roll that increment back along
	// with it (found by testing: a fifth wrong attempt never actually
	// stuck, so attempts never reached the lockout threshold).
	var invalid bool

	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)

		token, err := q.GetPendingVerificationTokenByTarget(ctx, store.GetPendingVerificationTokenByTargetParams{
			Target:  email,
			Purpose: store.IdentityTokenPurposeVerifyEmail,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			invalid = true
			return nil
		}
		if err != nil {
			return fmt.Errorf("app: looking up verification token: %w", err)
		}

		if token.Attempts >= maxVerificationAttempts {
			invalid = true
			return nil
		}

		wantHash := hashVerificationCode(email, store.IdentityTokenPurposeVerifyEmail, code)
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
		if err := q.MarkEmailVerified(ctx, token.UserID); err != nil {
			return fmt.Errorf("app: marking email verified: %w", err)
		}

		eventID, err := ids.New()
		if err != nil {
			return fmt.Errorf("app: generating event id: %w", err)
		}
		env, err := platformevents.New(eventID, "user.email_verified", 1, uuid.Nil, s.clk.Now(), "", map[string]any{
			"user_id": token.UserID,
			"email":   email,
		})
		if err != nil {
			return fmt.Errorf("app: building user.email_verified event: %w", err)
		}
		return outbox.Write(ctx, tx, "user", token.UserID, env)
	})
	if err != nil {
		return err
	}
	if invalid {
		return ErrInvalidOrExpiredCode
	}
	return nil
}
