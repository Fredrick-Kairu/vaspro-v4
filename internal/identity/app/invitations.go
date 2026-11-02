package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	platformevents "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/events"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/ids"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/outbox"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/domain"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/store"
)

// UserSummary is what other services may show about a user.
type UserSummary struct {
	ID       uuid.UUID
	Email    string
	FullName string
}

// EnsureInvitee returns the user with rawEmail, creating an unverified
// one with no name if there's none (as sign-up does; task 2.3b, OQ-05).
// Every case returns a user ID, so accounts, and whoever invited, can't
// tell whether the address was registered. The invited person signs in,
// or signs up with this address (which sets their name and verifies the
// email), then accepts in accounts.
func (s *Service) EnsureInvitee(ctx context.Context, rawEmail string) (uuid.UUID, error) {
	addr, err := domain.NormalizeEmail(rawEmail)
	if err != nil {
		return uuid.Nil, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	var inviteeID uuid.UUID
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		existing, err := q.GetUserByEmail(ctx, addr)
		switch {
		case err == nil:
			inviteeID = existing.ID
			return nil
		case errors.Is(err, pgx.ErrNoRows):
			inviteeID, err = s.createInvitedUser(ctx, q, tx, addr)
			return err
		default:
			return fmt.Errorf("app: looking up user: %w", err)
		}
	})
	if err != nil {
		return uuid.Nil, err
	}
	return inviteeID, nil
}

// SendInvitation emails inviteeID that inviterID invited them to
// accountName, once accounts has recorded the invitation, and logs it
// in the inviter's security events. The same email goes to a user with
// a login and one without. An unknown invitee or inviter is ErrNotFound.
func (s *Service) SendInvitation(ctx context.Context, inviteeID, inviterID uuid.UUID, accountName string) error {
	var to, inviterName string
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		invitee, err := q.GetUserByID(ctx, inviteeID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("app: loading the invitee: %w", err)
		}
		inviter, err := q.GetUserByID(ctx, inviterID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("app: loading the inviter: %w", err)
		}
		to, inviterName = invitee.Email, inviter.FullName
		return recordSecurityEvent(ctx, q, inviterID, "user_invited", nil, map[string]any{
			"invitee_user_id": inviteeID.String(), "account_name": accountName,
		})
	})
	if err != nil {
		return err
	}
	if err := s.email.SendInvitation(ctx, to, accountName, inviterName); err != nil {
		return fmt.Errorf("app: sending invitation: %w", err)
	}
	return nil
}

// createInvitedUser adds an unverified user with no name for addr and
// publishes user.registered, as sign-up does for a new address.
func (s *Service) createInvitedUser(ctx context.Context, q *store.Queries, tx pgx.Tx, addr string) (uuid.UUID, error) {
	id, err := ids.New()
	if err != nil {
		return uuid.Nil, fmt.Errorf("app: generating user id: %w", err)
	}
	if _, err := q.CreateUser(ctx, store.CreateUserParams{ID: id, Email: addr, FullName: ""}); err != nil {
		return uuid.Nil, fmt.Errorf("app: creating invited user: %w", err)
	}
	eventID, err := ids.New()
	if err != nil {
		return uuid.Nil, fmt.Errorf("app: generating event id: %w", err)
	}
	env, err := platformevents.New(eventID, "user.registered", 1, uuid.Nil, s.clk.Now(), "", map[string]any{
		"user_id": id,
		"email":   addr,
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("app: building user.registered event: %w", err)
	}
	if err := outbox.Write(ctx, tx, "user", id, env); err != nil {
		return uuid.Nil, fmt.Errorf("app: writing user.registered event: %w", err)
	}
	return id, nil
}

// GetUsers returns the email and name of each of userIDs that exists,
// for other services to show (accounts' member lists, task 2.3b).
func (s *Service) GetUsers(ctx context.Context, userIDs []uuid.UUID) ([]UserSummary, error) {
	rows, err := store.New(s.pool).ListUsersByIDs(ctx, userIDs)
	if err != nil {
		return nil, fmt.Errorf("app: listing users: %w", err)
	}
	out := make([]UserSummary, len(rows))
	for i, r := range rows {
		out[i] = UserSummary{ID: r.ID, Email: r.Email, FullName: r.FullName}
	}
	return out, nil
}
