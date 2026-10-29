package idempotency

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"
)

type postgresStore struct {
	pool *db.Pool
}

// NewPostgresStore returns a Store backed by idempotency_keys.
func NewPostgresStore(pool *db.Pool) Store {
	return &postgresStore{pool: pool}
}

func (s *postgresStore) Claim(ctx context.Context, tenantID uuid.UUID, key string, requestHash []byte, expiresAt time.Time) (*Record, error) {
	// A fresh key inserts normally. An existing-but-expired key is
	// reclaimed in the same statement (the WHERE guards the UPDATE so a
	// still-live key is left untouched). Either way RETURNING gives back
	// a row only when this call now owns the key.
	var discard []byte
	err := s.pool.QueryRow(ctx, `
		INSERT INTO idempotency_keys (tenant_id, key, request_hash, expires_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (tenant_id, key) DO UPDATE
			SET request_hash = EXCLUDED.request_hash,
			    expires_at   = EXCLUDED.expires_at,
			    status_code  = NULL,
			    response     = NULL,
			    created_at   = now()
			WHERE idempotency_keys.expires_at < now()
		RETURNING request_hash`,
		tenantID, key, requestHash, expiresAt,
	).Scan(&discard)
	if err == nil {
		return nil, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("idempotency: claiming %s: %w", key, err)
	}

	// Conflict existed and wasn't expired: fetch the live record.
	var rec Record
	var statusCode *int
	err = s.pool.QueryRow(ctx,
		"SELECT request_hash, status_code, response FROM idempotency_keys WHERE tenant_id = $1 AND key = $2",
		tenantID, key,
	).Scan(&rec.RequestHash, &statusCode, &rec.Response)
	if err != nil {
		return nil, fmt.Errorf("idempotency: fetching existing claim for %s: %w", key, err)
	}
	if statusCode != nil {
		rec.StatusCode = *statusCode
		rec.Complete = true
	}
	return &rec, nil
}

func (s *postgresStore) Complete(ctx context.Context, tenantID uuid.UUID, key string, statusCode int, response []byte) error {
	_, err := s.pool.Exec(ctx,
		"UPDATE idempotency_keys SET status_code = $3, response = $4 WHERE tenant_id = $1 AND key = $2",
		tenantID, key, statusCode, string(response),
	)
	if err != nil {
		return fmt.Errorf("idempotency: completing %s: %w", key, err)
	}
	return nil
}
