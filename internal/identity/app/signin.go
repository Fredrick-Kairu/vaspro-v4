package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/jackc/pgx/v5"

	vascrypto "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/crypto"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/domain"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/store"
)

// SignInResult is SignIn's outcome: either Session is set (no TOTP
// enrolled, sign-in is complete), or Challenge is set (TOTP enrolled —
// CompleteTOTPChallenge finishes it).
type SignInResult struct {
	Session   *Session
	Challenge string
}

// SignIn checks rawEmail/password and either completes sign-in
// (returning a Session) or, if the account has a confirmed TOTP
// enrollment, returns a short-lived challenge for CompleteTOTPChallenge.
// An unknown email, a wrong password, and a locked-out account all
// return the same ErrBadCredentials.
func (s *Service) SignIn(ctx context.Context, rawEmail, password string, ip net.IP, userAgent string) (*SignInResult, error) {
	email, err := domain.NormalizeEmail(rawEmail)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrValidation, err)
	}

	var (
		result *SignInResult
		failed bool
	)

	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)

		user, err := q.GetUserByEmail(ctx, email)
		if errors.Is(err, pgx.ErrNoRows) {
			failed = true
			return nil
		}
		if err != nil {
			return fmt.Errorf("app: looking up user: %w", err)
		}

		locked, err := s.tooManyRecentFailures(ctx, q, user.ID)
		if err != nil {
			return err
		}
		if locked {
			failed = true
			return nil
		}

		validPassword := user.PasswordHash.Valid
		if validPassword {
			ok, err := vascrypto.Argon2Verify(user.PasswordHash.String, password)
			if err != nil {
				return fmt.Errorf("app: verifying password: %w", err)
			}
			validPassword = ok
		}
		if !validPassword {
			if err := s.recordSignInFailure(ctx, q, user.ID, ip); err != nil {
				return err
			}
			failed = true
			return nil
		}

		totpRow, err := q.GetUserTOTP(ctx, user.ID)
		totpEnrolled := false
		switch {
		case err == nil:
			totpEnrolled = totpRow.ConfirmedAt.Valid
		case errors.Is(err, pgx.ErrNoRows):
			// No TOTP enrolled at all — fine, sign-in completes below.
		default:
			return fmt.Errorf("app: looking up TOTP enrollment: %w", err)
		}

		if totpEnrolled {
			challenge, err := s.issueChallenge(user.ID)
			if err != nil {
				return err
			}
			result = &SignInResult{Challenge: challenge}
			return nil
		}

		session, err := s.createSession(ctx, q, user.ID, ip, userAgent)
		if err != nil {
			return err
		}
		result = &SignInResult{Session: session}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if failed {
		return nil, ErrBadCredentials
	}
	return result, nil
}

// CompleteTOTPChallenge finishes a sign-in SignIn started by verifying
// challenge and either a live TOTP code or an unused recovery code
// against the challenge's user. A wrong code, a wrong or already-used
// recovery code, an expired/malformed challenge, and a locked-out
// account all return the same ErrTwoFactorInvalid.
func (s *Service) CompleteTOTPChallenge(ctx context.Context, challenge, code string, ip net.IP, userAgent string) (*Session, error) {
	userID, err := s.verifyChallenge(challenge)
	if err != nil {
		return nil, err
	}
	code = strings.TrimSpace(code)

	var (
		session *Session
		failed  bool
	)

	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
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

		valid := domain.ValidateTOTPCode(string(secret), code, s.clk.Now())
		if !valid {
			_, consumeErr := q.ConsumeRecoveryCode(ctx, store.ConsumeRecoveryCodeParams{
				UserID:   userID,
				CodeHash: hashRecoveryCode(code),
			})
			switch {
			case consumeErr == nil:
				valid = true
			case errors.Is(consumeErr, pgx.ErrNoRows):
				// Not a valid/unused recovery code either — falls
				// through to the failure path below.
			default:
				return fmt.Errorf("app: consuming recovery code: %w", consumeErr)
			}
		}

		if !valid {
			if err := s.recordSignInFailure(ctx, q, userID, ip); err != nil {
				return err
			}
			failed = true
			return nil
		}

		session, err = s.createSession(ctx, q, userID, ip, userAgent)
		return err
	})
	if err != nil {
		return nil, err
	}
	if failed {
		return nil, ErrTwoFactorInvalid
	}
	return session, nil
}
