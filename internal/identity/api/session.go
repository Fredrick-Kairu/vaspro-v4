package api

import (
	"context"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/app"
)

// SessionLookup resolves a bearer token — a session ID, identity's own
// temporary stand-in for real gateway/JWT auth (tasks 1.8/1.11) — to
// the user it belongs to. Used only by requireSession. Delegates to
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
func (l *SessionLookup) userIDForSession(ctx context.Context, sessionID uuid.UUID) (uuid.UUID, error) {
	return l.app.ResolveSession(ctx, sessionID)
}
