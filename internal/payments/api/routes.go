// Package api serves payments' HTTP API (docs/api/payments.yaml, task
// 4.3), reached only through the gateway.
package api

import (
	"context"
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
)

// HeaderUserID is set by the gateway once it has checked the caller's
// dashboard session (gateway/sessionauth.go). Payments trusts it
// because only the gateway can reach its HTTP API: a deployment rule
// (Config's doc), not something payments can check.
const HeaderUserID = "X-Vaspro-User-Id"

const idempotencyKeyTTL = 24 * time.Hour

type callerKey struct{}

func callerFromContext(ctx context.Context) uuid.UUID {
	id, _ := ctx.Value(callerKey{}).(uuid.UUID)
	return id
}

// requireCaller reads the user the gateway forwarded; with none, the
// request didn't come through the gateway's session check (AUTH-4010).
// The user also scopes Idempotency-Keys, as in accounts (2.2a) and the
// ledger.
func requireCaller(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID, err := uuid.Parse(r.Header.Get(HeaderUserID))
		if err != nil || userID == uuid.Nil {
			problem.Respond(w, r, problem.New(problem.AUTH4010, "a signed-in session is required"))
			return
		}
		ctx := context.WithValue(r.Context(), callerKey{}, userID)
		ctx = vaslog.WithTenantID(ctx, userID.String())
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// Routes builds payments' HTTP handler: health checks and the API.
func Routes(h *Handlers, logger *slog.Logger, ready []httpx.Check, idemStore idempotency.Store) http.Handler {
	r := chi.NewRouter()
	r.Use(httpx.RequestID)
	r.Use(httpx.Recover(logger))
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
			idempotency.Middleware(idemStore, idempotencyKeyTTL),
			requireCaller,
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
	slog.ErrorContext(r.Context(), "payments: unexpected error", "error", err)
	problem.Respond(w, r, problem.New(problem.SYS9000, "An unexpected error occurred."))
}
