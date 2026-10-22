package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/clock"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/store"
)

// SessionLookup resolves a bearer token — a session ID, identity's own
// temporary stand-in for real gateway/JWT auth (tasks 1.8/1.11) — to
// the user it belongs to. Used only by requireSession, for the two
// TOTP-enrollment routes.
type SessionLookup struct {
	pool *db.Pool
	clk  clock.Clock
}

// NewSessionLookup builds a SessionLookup over pool.
func NewSessionLookup(pool *db.Pool, clk clock.Clock) *SessionLookup {
	return &SessionLookup{pool: pool, clk: clk}
}

// errNoActiveSession is requireSession's sentinel for "the bearer
// token isn't a live, unrevoked, unexpired session" — malformed,
// unknown, expired, and revoked all look the same to the caller.
var errNoActiveSession = errors.New("api: no active session for that token")

// userIDForSession returns the user a live session belongs to.
func (l *SessionLookup) userIDForSession(ctx context.Context, sessionID uuid.UUID) (uuid.UUID, error) {
	session, err := store.New(l.pool).GetActiveSessionByID(ctx, store.GetActiveSessionByIDParams{
		ID:  sessionID,
		Now: l.clk.Now(),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, errNoActiveSession
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("api: looking up session: %w", err)
	}
	return session.UserID, nil
}
