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

// StaffRole is what a staff check returns (task 3.7): whether the user
// is staff, and whether they are a finance lead, who approves reversals
// at or above the pack's threshold (OQ-18).
type StaffRole struct {
	Staff       bool
	FinanceLead bool
}

// StaffRole reports userID's staff role. An unknown user has none.
func (s *Service) StaffRole(ctx context.Context, userID uuid.UUID) (StaffRole, error) {
	r, err := store.New(s.pool).GetStaffRole(ctx, userID)
	if err != nil {
		return StaffRole{}, fmt.Errorf("app: checking staff: %w", err)
	}
	return StaffRole{Staff: r.Staff, FinanceLead: r.FinanceLead}, nil
}

// GrantFinanceLead makes the staff member with email a finance lead
// (task 3.7, OQ-18). Only the ops CLI calls it. Someone who isn't staff
// is ErrNotFound; granting a finance lead again changes nothing.
func (s *Service) GrantFinanceLead(ctx context.Context, email, grantedBy string) error {
	return s.setFinanceLead(ctx, email, grantedBy, true)
}

// RevokeFinanceLead stops the staff member with email being a finance
// lead; they stay staff. Someone who isn't one is ErrNotFound.
func (s *Service) RevokeFinanceLead(ctx context.Context, email, revokedBy string) error {
	return s.setFinanceLead(ctx, email, revokedBy, false)
}

func (s *Service) setFinanceLead(ctx context.Context, email, by string, lead bool) error {
	by = strings.TrimSpace(by)
	if by == "" {
		return fmt.Errorf("%w: the operator making the change must be named", ErrValidation)
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		user, err := userByEmail(ctx, q, email)
		if err != nil {
			return err
		}
		role, err := q.GetStaffRole(ctx, user.ID)
		if err != nil {
			return fmt.Errorf("app: checking staff: %w", err)
		}
		switch {
		case !role.Staff, !lead && !role.FinanceLead:
			return ErrNotFound
		case lead && role.FinanceLead:
			return nil
		}
		if _, err := q.SetFinanceLead(ctx, store.SetFinanceLeadParams{UserID: user.ID, FinanceLead: lead}); err != nil {
			return fmt.Errorf("app: setting finance lead: %w", err)
		}
		event := "finance_lead_revoked"
		if lead {
			event = "finance_lead_granted"
		}
		return recordSecurityEvent(ctx, q, user.ID, event, nil, map[string]any{"by": by})
	})
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
