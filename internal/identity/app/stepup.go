package app

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/ids"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/domain"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/store"
)

// stepUpWindow is how long a session's step_up_at counts as "fresh" —
// system design §3's step-up list (create or reveal credentials,
// among others not yet built) needs MFA proved recently, not just at
// some point in the session's 24-hour life. Matches this codebase's
// existing precedent for "freshly proved" windows (challengeTTL).
const stepUpWindow = 5 * time.Minute

// StepUpFresh reports whether sessionID has stepped up within
// stepUpWindow. Always reads Postgres directly, never the 1.5 Redis
// session cache — that cache only stores a session's user ID with a
// long TTL, and a security-sensitive recency check must never read a
// stale cached value. requireStepUp (api/middleware.go) is the only
// caller, and it's wired onto two low-traffic endpoints, so the extra
// round trip costs nothing worth avoiding.
func (s *Service) StepUpFresh(ctx context.Context, sessionID uuid.UUID) (bool, error) {
	session, err := store.New(s.pool).GetActiveSessionByID(ctx, store.GetActiveSessionByIDParams{
		ID:  sessionID,
		Now: s.clk.Now(),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("app: looking up session: %w", err)
	}
	if !session.StepUpAt.Valid {
		return false, nil
	}
	return s.clk.Now().Sub(session.StepUpAt.Time) <= stepUpWindow, nil
}

// VerifyStepUpTOTP marks sessionID's step-up fresh after checking code
// against userID's existing TOTP enrollment — the same check
// CompleteTOTPChallenge (signin.go) uses at sign-in, including its
// rolling lockout. An account with no confirmed TOTP enrollment, a
// wrong code, and a locked-out account all return the same
// ErrTwoFactorInvalid.
func (s *Service) VerifyStepUpTOTP(ctx context.Context, userID, sessionID uuid.UUID, code string) error {
	code = strings.TrimSpace(code)

	var failed bool

	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)

		locked, err := s.tooManyRecentFailures(ctx, q, userID)
		if err != nil {
			return err
		}
		if locked {
			failed = true
			return nil
		}

		totpRow, err := q.GetUserTOTP(ctx, userID)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && !totpRow.ConfirmedAt.Valid) {
			failed = true
			return nil
		}
		if err != nil {
			return fmt.Errorf("app: looking up TOTP enrollment: %w", err)
		}

		secret, err := s.envelope.Open(ctx, s.totpKeyID, totpRow.SecretCiphertext)
		if err != nil {
			return fmt.Errorf("app: decrypting TOTP secret: %w", err)
		}

		accepted, err := s.acceptTOTPCode(ctx, q, userID, string(secret), code)
		if err != nil {
			return err
		}
		if !accepted {
			if err := s.recordSignInFailure(ctx, q, userID, nil); err != nil {
				return err
			}
			failed = true
			return nil
		}

		return q.SetSessionStepUp(ctx, sessionID)
	})
	if err != nil {
		return err
	}
	if failed {
		return ErrTwoFactorInvalid
	}
	return nil
}

// RequestStepUpEmail emails userID's own account a one-time step-up
// code — step-up's only method for an account with no TOTP enrolled.
// This is an authenticated action (the caller already has a valid
// session), so unlike RequestPasswordReset there's no enumeration
// concern to preserve: it always sends.
func (s *Service) RequestStepUpEmail(ctx context.Context, userID uuid.UUID) error {
	code, err := domain.GenerateVerificationCode()
	if err != nil {
		return fmt.Errorf("app: generating step-up code: %w", err)
	}

	var sendTo, sendFullName string

	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)

		user, err := q.GetUserByID(ctx, userID)
		if err != nil {
			return fmt.Errorf("app: looking up user: %w", err)
		}

		if err := q.DeletePendingVerificationTokens(ctx, store.DeletePendingVerificationTokensParams{
			Target:  user.Email,
			Purpose: store.IdentityTokenPurposeStepUp,
		}); err != nil {
			return fmt.Errorf("app: clearing previous step-up tokens: %w", err)
		}

		tokenID, err := ids.New()
		if err != nil {
			return fmt.Errorf("app: generating token id: %w", err)
		}
		if _, err := q.CreateVerificationToken(ctx, store.CreateVerificationTokenParams{
			ID:        tokenID,
			UserID:    userID,
			Purpose:   store.IdentityTokenPurposeStepUp,
			Target:    user.Email,
			TokenHash: hashVerificationCode(user.Email, store.IdentityTokenPurposeStepUp, code),
			ExpiresAt: s.clk.Now().Add(verificationTTL),
		}); err != nil {
			return fmt.Errorf("app: creating step-up token: %w", err)
		}

		sendTo, sendFullName = user.Email, user.FullName
		return nil
	})
	if err != nil {
		return err
	}

	if err := s.email.SendStepUpCode(ctx, sendTo, sendFullName, code); err != nil {
		return fmt.Errorf("app: sending step-up email: %w", err)
	}
	return nil
}

// ConfirmStepUpEmail checks code against the most recently issued
// step-up token for userID and, if valid, marks sessionID's step-up
// fresh. A wrong code, an expired or already-used token, and attempts
// exhausted all return the same ErrInvalidOrExpiredCode.
func (s *Service) ConfirmStepUpEmail(ctx context.Context, userID, sessionID uuid.UUID, code string) error {
	code = strings.TrimSpace(code)

	var invalid bool

	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)

		user, err := q.GetUserByID(ctx, userID)
		if err != nil {
			return fmt.Errorf("app: looking up user: %w", err)
		}

		token, err := q.GetPendingVerificationTokenByTarget(ctx, store.GetPendingVerificationTokenByTargetParams{
			Target:  user.Email,
			Purpose: store.IdentityTokenPurposeStepUp,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			invalid = true
			return nil
		}
		if err != nil {
			return fmt.Errorf("app: looking up step-up token: %w", err)
		}

		if token.Attempts >= maxVerificationAttempts {
			invalid = true
			return nil
		}

		wantHash := hashVerificationCode(user.Email, store.IdentityTokenPurposeStepUp, code)
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
		return q.SetSessionStepUp(ctx, sessionID)
	})
	if err != nil {
		return err
	}
	if invalid {
		return ErrInvalidOrExpiredCode
	}
	return nil
}

// IsStepUpFresh is StepUpFresh for another service asking about a
// session it was handed (task 2.3b): sessionID must be userID's own
// active session, or it's never fresh.
func (s *Service) IsStepUpFresh(ctx context.Context, userID, sessionID uuid.UUID) (bool, error) {
	session, err := store.New(s.pool).GetActiveSessionByID(ctx, store.GetActiveSessionByIDParams{ID: sessionID, Now: s.clk.Now()})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("app: looking up session: %w", err)
	}
	if session.UserID != userID || !session.StepUpAt.Valid {
		return false, nil
	}
	return s.clk.Now().Sub(session.StepUpAt.Time) <= stepUpWindow, nil
}
