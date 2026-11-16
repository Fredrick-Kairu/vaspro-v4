// Package api serves webhooks' HTTP API (docs/api/webhooks.yaml, task
// 5.5), reached only through the gateway.
package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/httpx"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/idempotency"
	vaslog "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/log"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/problem"
)

// HeaderUserID is set by the gateway once it has checked the caller's
// dashboard session (gateway/sessionauth.go). Webhooks trusts it because
// only the gateway can reach its HTTP API: a deployment rule (Config's
// doc), not something webhooks can check.
const HeaderUserID = "X-Vaspro-User-Id"

// HeaderSessionID is the session the gateway resolved, forwarded with
// the user; webhooks asks identity whether its step-up is fresh before
// an endpoint is created, its URL changed or its secret rotated.
const HeaderSessionID = "X-Vaspro-Session-Id"

const idempotencyKeyTTL = 24 * time.Hour

type callerKey struct{}

type sessionKey struct{}

func callerFromContext(ctx context.Context) uuid.UUID {
	id, _ := ctx.Value(callerKey{}).(uuid.UUID)
	return id
}

// sessionFromContext is the forwarded session, or uuid.Nil (which is
// never stepped up).
func sessionFromContext(ctx context.Context) uuid.UUID {
	id, _ := ctx.Value(sessionKey{}).(uuid.UUID)
	return id
}

// requireCaller reads the user the gateway forwarded; with none, the
// request didn't come through the gateway's session check (AUTH-4010).
// The user also scopes Idempotency-Keys, as in accounts and payments.
func requireCaller(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID, err := uuid.Parse(r.Header.Get(HeaderUserID))
		if err != nil || userID == uuid.Nil {
			problem.Respond(w, r, problem.New(problem.AUTH4010, "a signed-in session is required"))
			return
		}
		ctx := context.WithValue(r.Context(), callerKey{}, userID)
		if sessionID, err := uuid.Parse(r.Header.Get(HeaderSessionID)); err == nil {
			ctx = context.WithValue(ctx, sessionKey{}, sessionID)
		}
		ctx = vaslog.WithTenantID(ctx, userID.String())
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// secretRoutes return an endpoint secret shown once: creating an
// endpoint and rotating its secret. Their stored idempotency records
// keep the secret as null, so a retry never shows it again (task 5.5).
var secretRoutes = regexp.MustCompile(`^/v1/webhooks/tenants/[^/]+/endpoints(/[^/]+/secret-rotations)?$`)

func returnsSecret(r *http.Request) bool {
	return r.Method == http.MethodPost && secretRoutes.MatchString(r.URL.Path)
}

// Routes builds webhooks' HTTP handler: health checks and the API.
func Routes(h *Handlers, logger *slog.Logger, ready []httpx.Check, idemStore idempotency.Store) http.Handler {
	r := chi.NewRouter()
	r.Use(httpx.RequestID)
	r.Use(httpx.Recover(logger))
	// Verifying waits up to the challenge's 10 seconds.
	r.Use(httpx.Timeout(30 * time.Second))
	r.Use(httpx.BodyLimitFunc(func(*http.Request) int64 { return 1 << 20 }))

	r.Get("/healthz", httpx.Healthz)
	r.Get("/readyz", httpx.Readyz(ready...))

	strict := NewStrictHandlerWithOptions(h, nil, StrictHTTPServerOptions{
		ResponseErrorHandlerFunc: respondProblem,
	})
	HandlerWithOptions(strict, ChiServerOptions{
		BaseRouter: r,
		// The last entry runs first (1.2's lesson): requireCaller sets
		// the scope idempotency.Middleware needs.
		Middlewares: []MiddlewareFunc{
			idempotency.Middleware(idemStore, idempotencyKeyTTL, idempotency.WithRedactedFields(returnsSecret, "secret")),
			requireCaller,
		},
		ErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			problem.Respond(w, r, problem.New(problem.VAL4000, err.Error()))
		},
	})
	return r
}

// respondProblem renders a *problem.Problem as-is and anything else as
// a generic SYS-9000, logging it (without request data) so an
// unexpected error is never silent.
func respondProblem(w http.ResponseWriter, r *http.Request, err error) {
	var p *problem.Problem
	if errors.As(err, &p) {
		problem.Respond(w, r, p)
		return
	}
	slog.ErrorContext(r.Context(), "webhooks: unexpected error", "error", err)
	problem.Respond(w, r, problem.New(problem.SYS9000, "An unexpected error occurred."))
}
