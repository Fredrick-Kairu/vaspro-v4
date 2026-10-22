package api

import (
	"context"
	"net"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/problem"
)

type clientInfo struct {
	IP        net.IP
	UserAgent string
}

type clientInfoKey struct{}
type sessionUserIDKey struct{}
type sessionIDKey struct{}

// withClientInfo stashes the caller's IP and User-Agent into context so
// handlers.go's sign-in/TOTP-challenge methods — which, under
// oapi-codegen's strict mode, only see ctx and a typed request, never
// the raw *http.Request — can still record them against
// identity.sessions and identity.security_events.
func withClientInfo(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		info := clientInfo{IP: net.ParseIP(host), UserAgent: r.UserAgent()}
		ctx := context.WithValue(r.Context(), clientInfoKey{}, info)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func clientInfoFromContext(ctx context.Context) clientInfo {
	info, _ := ctx.Value(clientInfoKey{}).(clientInfo)
	return info
}

func withSessionUserID(ctx context.Context, userID uuid.UUID) context.Context {
	return context.WithValue(ctx, sessionUserIDKey{}, userID)
}

// sessionUserIDFromContext returns the user ID requireSession resolved
// for this request. Only ever called from the two TOTP-enrollment
// handlers, which requireSession gates — so a missing value here would
// be a wiring bug, not a client error.
func sessionUserIDFromContext(ctx context.Context) uuid.UUID {
	userID, _ := ctx.Value(sessionUserIDKey{}).(uuid.UUID)
	return userID
}

func withSessionID(ctx context.Context, sessionID uuid.UUID) context.Context {
	return context.WithValue(ctx, sessionIDKey{}, sessionID)
}

// sessionIDFromContext returns the bearer session ID requireSession
// resolved for this request — used only by listSessions, to flag which
// row is the one the caller is using right now.
func sessionIDFromContext(ctx context.Context) uuid.UUID {
	sessionID, _ := ctx.Value(sessionIDKey{}).(uuid.UUID)
	return sessionID
}

// requireSessionOperations names the only operations requireSession
// checks — every other route passes through untouched. TOTP enrollment
// is a dashboard security setting (system design §3), not part of
// sign-up, so it's the one pair of routes that needs a caller to
// already be signed in, ahead of the real gateway/JWT (tasks 1.8/1.11).
//
// The keys are the generated Go method names StrictMiddlewareFunc is
// actually called with (api_gen.go's strictHandler.<Name>), not the
// operationId strings as written in docs/api/identity.yaml — those
// differ in case ("CreateTotpEnrollment" vs "createTotpEnrollment").
// A case mismatch here fails silently (the map lookup just misses, so
// requireSession never rejects anything) rather than with a compile or
// runtime error — confirmed the hard way when a first draft used the
// lowercase operationId spelling and every "missing bearer" test came
// back 500 instead of 401, because the request sailed through to a
// handler that got called with no real session.
var requireSessionOperations = map[string]bool{
	"CreateTotpEnrollment":                  true,
	"CreateTotpEnrollmentConfirmation":      true,
	"CreatePasskeyRegistration":             true,
	"CreatePasskeyRegistrationConfirmation": true,
	"ListPasskeys":                          true,
	"UpdatePasskey":                         true,
	"DeletePasskey":                         true,
	"ListSessions":                          true,
	"DeleteSession":                         true,
	"DeleteSessions":                        true,
}

// requireSession reads Authorization: Bearer <session_id> and resolves
// it through lookup, rejecting a missing, malformed, unknown, expired
// or revoked token with the same AUTH-4010 sign-in uses — identity has
// no real bearer-auth yet, so this is a narrow, local stand-in, not a
// platform mechanism.
func requireSession(lookup *SessionLookup) StrictMiddlewareFunc {
	return func(f StrictHandlerFunc, operationID string) StrictHandlerFunc {
		if !requireSessionOperations[operationID] {
			return f
		}
		return func(ctx context.Context, w http.ResponseWriter, r *http.Request, request any) (any, error) {
			const prefix = "Bearer "
			header := r.Header.Get("Authorization")
			if !strings.HasPrefix(header, prefix) {
				return nil, problem.New(problem.AUTH4010, "a bearer session token is required")
			}
			sessionID, err := uuid.Parse(strings.TrimPrefix(header, prefix))
			if err != nil {
				return nil, problem.New(problem.AUTH4010, "a bearer session token is required")
			}
			userID, err := lookup.userIDForSession(ctx, sessionID)
			if err != nil {
				return nil, problem.New(problem.AUTH4010, "a bearer session token is required")
			}
			ctx = withSessionUserID(ctx, userID)
			ctx = withSessionID(ctx, sessionID)
			return f(ctx, w, r, request)
		}
	}
}
