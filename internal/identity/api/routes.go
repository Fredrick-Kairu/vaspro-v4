package api

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/httpx"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/idempotency"
	vaslog "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/log"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/problem"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/app"
)

// idempotencyKeyTTL is how long a claimed Idempotency-Key is remembered
// — long enough that a client retrying after a transient failure still
// replays the original result, not a second attempt.
const idempotencyKeyTTL = 24 * time.Hour

// Routes builds identity's HTTP handler: platform/httpx's standard
// middlewares, health/readiness, and the operations generated from
// docs/api/identity.yaml. lookup resolves the bearer session token
// requireSession checks; a is requireStepUp's dependency
// (app.Service.StepUpFresh, task 1.6).
func Routes(h *Handlers, logger *slog.Logger, ready []httpx.Check, idemStore idempotency.Store, lookup *SessionLookup, a *app.Service) http.Handler {
	r := chi.NewRouter()
	r.Use(httpx.RequestID)
	r.Use(httpx.Recover(logger))
	r.Use(httpx.Timeout(30 * time.Second))
	r.Use(httpx.BodyLimit(1 << 20))
	r.Use(withClientInfo)

	r.Get("/healthz", httpx.Healthz)
	r.Get("/readyz", httpx.Readyz(ready...))

	// requireStepUp must run after requireSession (it reads the session
	// ID requireSession resolves), and this slice wraps iteratively with
	// the *last* entry outermost/running first (same mechanic as the
	// ChiServerOptions.Middlewares lesson below) — requireSession is
	// last here so it executes before requireStepUp.
	strict := NewStrictHandlerWithOptions(h, []StrictMiddlewareFunc{requireStepUp(a), requireSession(lookup)}, StrictHTTPServerOptions{
		ResponseErrorHandlerFunc: respondProblem,
	})
	HandlerWithOptions(strict, ChiServerOptions{
		BaseRouter: r,
		// Both generated operations are anonymous sign-up endpoints
		// (no gateway/auth context yet, task 1.11) and both are writes
		// (.claude/rules/api.md: every write needs an Idempotency-Key).
		// ServerInterfaceWrapper wraps iteratively (handler =
		// middleware(handler) for each entry in order), so the LAST
		// entry ends up outermost and runs FIRST — withAnonymousTenant
		// has to be last here so it sets the tenant context before
		// idempotency.Middleware looks for one (found by testing: the
		// reverse order produced "no tenant in request context" on
		// every request).
		Middlewares: []MiddlewareFunc{
			idempotency.Middleware(idemStore, idempotencyKeyTTL),
			withAnonymousTenant,
		},
	})

	return r
}

// withAnonymousTenant sets a sentinel "no tenant" value
// (uuid.Nil — identity's core tables have no tenant_id at all, 1.1's
// own finding) so idempotency.Middleware has the tenant context it
// normally only gets from a gateway's auth middleware.
func withAnonymousTenant(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := vaslog.WithTenantID(r.Context(), uuid.Nil.String())
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// respondProblem bridges oapi-codegen's strict mode to problem+json: a
// *problem.Problem returned as the strict handler's error is rendered
// as-is; anything else becomes a generic SYS-9000, never leaking an
// internal error string to the client.
func respondProblem(w http.ResponseWriter, r *http.Request, err error) {
	var p *problem.Problem
	if errors.As(err, &p) {
		problem.Respond(w, r, p)
		return
	}
	problem.Respond(w, r, problem.New(problem.SYS9000, "An unexpected error occurred."))
}
