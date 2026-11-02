package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/adapters"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/store"
)

// ErrNoTenantSelected covers a session with no account selected and one
// whose user is no longer allowed into it, alike. api/ maps it to
// AUTH-4023.
var ErrNoTenantSelected = errors.New("app: no account selected")

// SelectSessionTenant sets the account sessionID works in (task 2.2c):
// API clients and SMPP accounts belong to it from then on. Accounts
// decides who may work in an account; one the user can't reach, and a
// session that isn't the user's own active one, are ErrNotFound alike.
func (s *Service) SelectSessionTenant(ctx context.Context, userID, sessionID, tenantID uuid.UUID) error {
	if err := s.checkTenantAccess(ctx, userID, tenantID); err != nil {
		if errors.Is(err, adapters.ErrNoTenantAccess) {
			return ErrNotFound
		}
		return err
	}
	n, err := store.New(s.pool).SetSessionTenant(ctx, store.SetSessionTenantParams{
		TenantID: pgtype.UUID{Bytes: tenantID, Valid: true},
		ID:       sessionID,
		UserID:   userID,
		Now:      s.clk.Now(),
	})
	if err != nil {
		return fmt.Errorf("app: selecting account: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SessionTenant returns the account sessionID has selected, asking
// accounts again on every call so a removed member loses access at
// once (decision Q2). No selection and lost access are both
// ErrNoTenantSelected; any other failure, accounts being unreachable
// included, is returned as-is so the caller fails closed. It reads
// Postgres rather than the session cache: the selection can change
// while a cached session lives on.
func (s *Service) SessionTenant(ctx context.Context, userID, sessionID uuid.UUID) (uuid.UUID, error) {
	row, err := store.New(s.pool).GetActiveSessionByID(ctx, store.GetActiveSessionByIDParams{ID: sessionID, Now: s.clk.Now()})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && row.UserID != userID) {
		return uuid.Nil, ErrNoTenantSelected
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("app: loading session: %w", err)
	}
	if !row.TenantID.Valid {
		return uuid.Nil, ErrNoTenantSelected
	}
	tenantID := uuid.UUID(row.TenantID.Bytes)
	if err := s.checkTenantAccess(ctx, userID, tenantID); err != nil {
		if errors.Is(err, adapters.ErrNoTenantAccess) {
			return uuid.Nil, ErrNoTenantSelected
		}
		return uuid.Nil, err
	}
	return tenantID, nil
}

func (s *Service) checkTenantAccess(ctx context.Context, userID, tenantID uuid.UUID) error {
	if s.access == nil {
		return errors.New("app: account access check isn't configured")
	}
	return s.access.CheckTenantAccess(ctx, userID, tenantID)
}
