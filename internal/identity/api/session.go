package api

import (
	"context"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/app"
)

// SessionLookup resolves a bearer token — a session token, identity's
// own dashboard credential (never the session's id) — to the session
// and user it belongs to. Used only by requireSession. Delegates to
// app.Service.ResolveSession (1.5) so requireSession's bearer-check and
// the real gateway's eventual per-request check (1.11) are the same
// code path, not two.
type SessionLookup struct {
	app *app.Service
}

// NewSessionLookup builds a SessionLookup over a.
func NewSessionLookup(a *app.Service) *SessionLookup {
	return &SessionLookup{app: a}
}

// userIDForSession returns the user a live session belongs to.
func (l *SessionLookup) resolve(ctx context.Context, token string) (sessionID, userID uuid.UUID, err error) {
	return l.app.ResolveSession(ctx, token)
}
