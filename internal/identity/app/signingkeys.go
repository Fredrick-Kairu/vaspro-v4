package app

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/domain"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/store"
)

// signingKeyRetiringOverlap is how long a demoted key stays published
// in the JWKS (status = 'retiring') after a rotation — comfortably
// longer than tokenTTL so a token signed moments before rotation never
// outlives its own key's visibility.
const signingKeyRetiringOverlap = 24 * time.Hour

// PublicSigningKey is one row of a published JWKS — never any private
// material.
type PublicSigningKey struct {
	Kid       string
	Algorithm string
	PublicKey ed25519.PublicKey
}

// identity.signing_keys has no tenant_id and no RLS at all (1.1's own
// finding — it's global, published as the JWKS) — every transaction
// here is a plain pgx.BeginFunc, same as every other identity table
// with no RLS.

// EnsureActiveSigningKey generates and stores a fresh signing key only
// if none is currently active — the bootstrap case, so the service
// can always sign a token even on a brand new database. A no-op
// otherwise.
func (s *Service) EnsureActiveSigningKey(ctx context.Context) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		if _, err := q.GetActiveSigningKey(ctx); err == nil {
			return nil
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("app: checking for an active signing key: %w", err)
		}
		return s.createActiveSigningKey(ctx, q)
	})
}

// RotateSigningKey generates a fresh signing key, demotes whichever
// key was active to retiring, and in the same transaction retires any
// key that has been retiring for longer than
// signingKeyRetiringOverlap — a "sweep on rotate" rather than a
// separate scheduled job, since rotation is already the one place
// this needs to happen.
func (s *Service) RotateSigningKey(ctx context.Context) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)

		now := s.clk.Now()
		if previous, err := q.GetActiveSigningKey(ctx); err == nil {
			if err := q.DemoteSigningKeyToRetiring(ctx, store.DemoteSigningKeyToRetiringParams{
				Kid: previous.Kid,
				Now: now,
			}); err != nil {
				return fmt.Errorf("app: demoting the previously active signing key: %w", err)
			}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("app: looking up the active signing key to demote: %w", err)
		}

		if err := s.createActiveSigningKey(ctx, q); err != nil {
			return err
		}

		if _, err := q.RetireStaleSigningKeys(ctx, now.Add(-signingKeyRetiringOverlap)); err != nil {
			return fmt.Errorf("app: retiring stale signing keys: %w", err)
		}
		return nil
	})
}

func (s *Service) createActiveSigningKey(ctx context.Context, q *store.Queries) error {
	pair, err := domain.GenerateSigningKeyPair()
	if err != nil {
		return fmt.Errorf("app: generating a signing key pair: %w", err)
	}
	sealed, err := s.envelope.Seal(ctx, s.signingKeysKeyID, pair.PrivateKey)
	if err != nil {
		return fmt.Errorf("app: sealing a signing key's private key: %w", err)
	}
	if _, err := q.CreateSigningKey(ctx, store.CreateSigningKeyParams{
		Kid:        pair.Kid,
		Algorithm:  domain.SigningKeyAlgorithm,
		PublicKey:  pair.PublicKey,
		PrivateRef: base64.StdEncoding.EncodeToString(sealed),
		Status:     "active",
	}); err != nil {
		return fmt.Errorf("app: storing a new signing key: %w", err)
	}
	return nil
}

// ActiveSigningKey returns the current active key's kid and decrypted
// Ed25519 private key, for signing a fresh JWT. Run() calls
// EnsureActiveSigningKey at startup, so in practice this never finds
// none — but it's still a real error, not a panic, if it ever does.
func (s *Service) ActiveSigningKey(ctx context.Context) (kid string, priv ed25519.PrivateKey, err error) {
	var row store.IdentitySigningKey
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		row, err = store.New(tx).GetActiveSigningKey(ctx)
		return err
	})
	if err != nil {
		return "", nil, fmt.Errorf("app: no active signing key: %w", err)
	}

	sealed, err := base64.StdEncoding.DecodeString(row.PrivateRef)
	if err != nil {
		return "", nil, fmt.Errorf("app: decoding the active signing key's sealed private key: %w", err)
	}
	opened, err := s.envelope.Open(ctx, s.signingKeysKeyID, sealed)
	if err != nil {
		return "", nil, fmt.Errorf("app: opening the active signing key's private key: %w", err)
	}
	return row.Kid, ed25519.PrivateKey(opened), nil
}

// PublishableSigningKeys returns every key the JWKS endpoint should
// publish — active and retiring, never retired, never any private
// material.
func (s *Service) PublishableSigningKeys(ctx context.Context) ([]PublicSigningKey, error) {
	var rows []store.IdentitySigningKey
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		rows, err = store.New(tx).ListPublishableSigningKeys(ctx)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("app: listing publishable signing keys: %w", err)
	}

	keys := make([]PublicSigningKey, 0, len(rows))
	for _, r := range rows {
		keys = append(keys, PublicSigningKey{
			Kid:       r.Kid,
			Algorithm: r.Algorithm,
			PublicKey: ed25519.PublicKey(r.PublicKey),
		})
	}
	return keys, nil
}
