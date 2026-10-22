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
		q := store.New(tx)

		row, err := q.GetUserTOTP(ctx, userID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrTwoFactorInvalid
		}
		if err != nil {
			return fmt.Errorf("app: looking up TOTP enrollment: %w", err)
		}

		secret, err := s.envelope.Open(ctx, s.totpKeyID, row.SecretCiphertext)
		if err != nil {
			return fmt.Errorf("app: decrypting TOTP secret: %w", err)
		}

		if !domain.ValidateTOTPCode(string(secret), code, s.clk.Now()) {
			// No write happened yet in this branch, so returning the
			// sentinel directly (rather than the commit-after-the-fact
			// pattern SignIn/CompleteTOTPChallenge need) is safe here —
			// there's nothing for the rollback to lose.
			return ErrTwoFactorInvalid
		}

		return q.ConfirmUserTOTP(ctx, userID)
	})
}
