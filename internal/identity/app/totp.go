package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/domain"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/store"
)

// totpIssuer names VASPRO in the otpauth:// URL an authenticator app
// shows to the user.
const totpIssuer = "VASPRO"

// TOTPEnrollment is EnrollTOTP's result — shown exactly once.
type TOTPEnrollment struct {
	URL           string
	RecoveryCodes []string
}

// EnrollTOTP generates a fresh TOTP secret and a batch of recovery
// codes for userID, encrypts the secret (platform/crypto.Envelope) and
// stores both — replacing any unconfirmed prior enrollment
// (UpsertUserTOTP). The secret isn't usable to sign in until
// ConfirmTOTPEnrollment proves the caller actually scanned it.
func (s *Service) EnrollTOTP(ctx context.Context, userID uuid.UUID) (*TOTPEnrollment, error) {
	user, err := store.New(s.pool).GetUserByID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("app: looking up user: %w", err)
	}

	secret, url, err := domain.GenerateTOTPSecret(totpIssuer, user.Email)
	if err != nil {
		return nil, fmt.Errorf("app: generating TOTP secret: %w", err)
	}
	ciphertext, err := s.envelope.Seal(ctx, s.totpKeyID, []byte(secret))
	if err != nil {
		return nil, fmt.Errorf("app: encrypting TOTP secret: %w", err)
	}

	codes, err := domain.GenerateRecoveryCodes()
	if err != nil {
		return nil, fmt.Errorf("app: generating recovery codes: %w", err)
	}
	hashes := make([]string, len(codes))
	for i, c := range codes {
		hashes[i] = hashRecoveryCode(c)
	}

	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		_, err := q.UpsertUserTOTP(ctx, store.UpsertUserTOTPParams{
			UserID:             userID,
			SecretCiphertext:   ciphertext,
			RecoveryCodeHashes: hashes,
		})
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("app: storing TOTP enrollment: %w", err)
	}

	return &TOTPEnrollment{URL: url, RecoveryCodes: codes}, nil
}

// ConfirmTOTPEnrollment proves userID's pending TOTP enrollment is
// actually usable before sign-in starts requiring it: code must
// validate against the secret EnrollTOTP just stored. A wrong code (or
// no pending enrollment at all) returns ErrTwoFactorInvalid.
func (s *Service) ConfirmTOTPEnrollment(ctx context.Context, userID uuid.UUID, code string) error {
	code = strings.TrimSpace(code)

	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		confirmed, err := s.confirmTOTPEnrollment(ctx, store.New(tx), userID, code)
		if err != nil {
			return err
		}
		if !confirmed {
			// No write happened on this path, so returning the sentinel
			// directly (rather than the commit-after-the-fact pattern
			// SignIn/CompleteTOTPChallenge need) is safe here.
			return ErrTwoFactorInvalid
		}
		return nil
	})
}

// confirmTOTPEnrollment checks code against userID's pending TOTP
// secret and, if it's valid, marks the enrollment confirmed. It reports
// false, writing nothing, for a wrong code or no pending enrollment.
// Shared by ConfirmTOTPEnrollment and Google sign-up's TOTP enrolment
// (task 1.16).
func (s *Service) confirmTOTPEnrollment(ctx context.Context, q *store.Queries, userID uuid.UUID, code string) (bool, error) {
	row, err := q.GetUserTOTP(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("app: looking up TOTP enrollment: %w", err)
	}

	secret, err := s.envelope.Open(ctx, s.totpKeyID, row.SecretCiphertext)
	if err != nil {
		return false, fmt.Errorf("app: decrypting TOTP secret: %w", err)
	}

	accepted, err := s.acceptTOTPCode(ctx, q, userID, string(secret), code)
	if err != nil || !accepted {
		return false, err
	}
	if err := q.ConfirmUserTOTP(ctx, userID); err != nil {
		return false, fmt.Errorf("app: confirming TOTP enrollment: %w", err)
	}
	return true, nil
}

// acceptTOTPCode reports whether code is valid for secret now and, if
// so, spends it: the code's time step is claimed for userID, so neither
// it nor any code from an earlier step is accepted again (RFC 6238
// §5.2). Enrolment confirmation, the sign-in challenge and step-up all
// go through here, so a code is spent wherever it's first used.
func (s *Service) acceptTOTPCode(ctx context.Context, q *store.Queries, userID uuid.UUID, secret, code string) (bool, error) {
	step, ok := domain.MatchTOTPStep(secret, code, s.clk.Now())
	if !ok {
		return false, nil
	}
	claimed, err := q.ClaimTOTPStep(ctx, store.ClaimTOTPStepParams{UserID: userID, Step: step})
	if err != nil {
		return false, fmt.Errorf("app: claiming TOTP time step: %w", err)
	}
	return claimed == 1, nil
}
