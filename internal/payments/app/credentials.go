package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	vascrypto "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/crypto"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments/adapters"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments/store"
)

// CredentialSource decrypts a VASPRO paybill's Daraja credentials at
// call time (adapters.CredentialSource; data model §7: decrypted only
// for the rail adapter's call, never logged or returned by an API). It
// reads as payments_system, so it reaches only VASPRO's own accounts.
type CredentialSource struct {
	pool *db.Pool
	kms  vascrypto.KMS
}

// NewCredentialSource reads from pool and opens with kms.
func NewCredentialSource(pool *db.Pool, kms vascrypto.KMS) *CredentialSource {
	return &CredentialSource{pool: pool, kms: kms}
}

// DarajaCredentials implements adapters.CredentialSource.
func (c *CredentialSource) DarajaCredentials(ctx context.Context, railAccountID uuid.UUID) (adapters.DarajaCredentials, error) {
	var (
		acct  store.PaymentsRailAccount
		creds []store.PaymentsRailCredential
	)
	err := c.pool.WithRoleTx(ctx, systemRole, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		var err error
		acct, err = q.GetRailAccount(ctx, railAccountID)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: no rail account %s", ErrNotFound, railAccountID)
		}
		if err != nil {
			return fmt.Errorf("app: loading the rail account: %w", err)
		}
		creds, err = q.ListActiveRailCredentials(ctx, railAccountID)
		return err
	})
	if err != nil {
		return adapters.DarajaCredentials{}, err
	}
	env := vascrypto.NewEnvelope(c.kms)
	out := adapters.DarajaCredentials{ShortCode: acct.Number}
	for _, cr := range creds {
		plain, err := env.Open(ctx, cr.KmsKeyID, cr.Ciphertext)
		if err != nil {
			return adapters.DarajaCredentials{}, fmt.Errorf("app: opening the %s credential: %w", cr.Kind, err)
		}
		switch cr.Kind {
		case "app_key":
			out.ConsumerKey = string(plain)
		case "app_secret":
			out.ConsumerSecret = string(plain)
		case "passkey":
			out.Passkey = string(plain)
		}
	}
	if out.ConsumerKey == "" || out.ConsumerSecret == "" || out.Passkey == "" {
		return adapters.DarajaCredentials{}, fmt.Errorf("app: rail account %s is missing credentials", railAccountID)
	}
	return out, nil
}
