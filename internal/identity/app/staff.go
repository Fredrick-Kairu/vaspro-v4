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

// ErrNotStaff means the caller isn't VASPRO staff (task 2.9).
var ErrNotStaff = errors.New("app: staff only")

// GrantStaff makes the user with email VASPRO staff (task 2.9, OQ-10).
// Only the ops CLI calls it; grantedBy names the operator, for the
// audit trail. Granting someone who is already staff changes nothing.
// An unknown email is ErrNotFound.
func (s *Service) GrantStaff(ctx context.Context, email, grantedBy string) error {
	grantedBy = strings.TrimSpace(grantedBy)
	if grantedBy == "" {
		return fmt.Errorf("%w: the operator granting staff must be named", ErrValidation)
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		user, err := userByEmail(ctx, q, email)
		if err != nil {
			return err
		}
		already, err := q.IsStaff(ctx, user.ID)
		if err != nil {
			return fmt.Errorf("app: checking staff: %w", err)
		}
		if already {
			return nil
		}
		if err := q.GrantStaff(ctx, store.GrantStaffParams{UserID: user.ID, GrantedBy: grantedBy}); err != nil {
			return fmt.Errorf("app: granting staff: %w", err)
		}
		return recordSecurityEvent(ctx, q, user.ID, "staff_granted", nil, map[string]any{"by": grantedBy})
	})
}

// RevokeStaff stops the user with email being staff. Staff checks ask
// on every call, so it takes effect at once. An unknown email, or a
// user who isn't staff, is ErrNotFound.
func (s *Service) RevokeStaff(ctx context.Context, email, revokedBy string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		user, err := userByEmail(ctx, q, email)
		if err != nil {
			return err
		}
		n, err := q.RevokeStaff(ctx, user.ID)
		if err != nil {
			return fmt.Errorf("app: revoking staff: %w", err)
		}
		if n == 0 {
			return ErrNotFound
		}
		return recordSecurityEvent(ctx, q, user.ID, "staff_revoked", nil, map[string]any{"by": strings.TrimSpace(revokedBy)})
	})
}

// IsStaff reports whether userID is VASPRO staff. An unknown user isn't.
func (s *Service) IsStaff(ctx context.Context, userID uuid.UUID) (bool, error) {
	staff, err := store.New(s.pool).IsStaff(ctx, userID)
	if err != nil {
		return false, fmt.Errorf("app: checking staff: %w", err)
	}
	return staff, nil
}

// UnlockByStaff lifts the sign-in lockout of the user with email (task
// 2.9; Phase 1 deferred it). It records an unlocked_by_staff security
// event naming the staff member; tooManyRecentFailures counts only
// failures after the latest one, so new failures lock again. Unlocking
// a user who isn't locked is harmless.
func (s *Service) UnlockByStaff(ctx context.Context, staffID uuid.UUID, email string) error {
	staff, err := s.IsStaff(ctx, staffID)
	if err != nil {
		return err
	}
	if !staff {
		return ErrNotStaff
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		user, err := userByEmail(ctx, q, email)
		if err != nil {
			return err
		}
		return recordSecurityEvent(ctx, q, user.ID, "unlocked_by_staff", nil, map[string]any{"staff_user_id": staffID})
	})
}

// userByEmail finds a user by a normalized email; malformed and unknown
// addresses are both ErrNotFound, since only staff and operators ask.
func userByEmail(ctx context.Context, q *store.Queries, email string) (store.IdentityUser, error) {
	normalized, err := domain.NormalizeEmail(email)
	if err != nil {
		return store.IdentityUser{}, ErrNotFound
	}
	user, err := q.GetUserByEmail(ctx, normalized)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.IdentityUser{}, ErrNotFound
	}
	if err != nil {
		return store.IdentityUser{}, fmt.Errorf("app: looking up user: %w", err)
	}
	return user, nil
}
