// Package api serves payments' HTTP API (docs/api/payments.yaml, task
// 4.3), reached only through the gateway.
package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
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

// HeaderSessionID is the session the gateway resolved, forwarded with
// the user; payments asks identity whether its step-up is fresh before
// staff match a payment (task 4.5).
const HeaderSessionID = "X-Vaspro-Session-Id"

// HeaderClientIP is the caller's address as the gateway saw it on the
// connection, set on every request it forwards (client-sent copies are
// stripped). A rail's callback is checked against it (OQ-22).
const HeaderClientIP = "X-Vaspro-Client-Ip"

const idempotencyKeyTTL = 24 * time.Hour

type callerKey struct{}

type sessionKey struct{}

// sessionFromContext is the forwarded session, or uuid.Nil (which is
// never stepped up).
func sessionFromContext(ctx context.Context) uuid.UUID {
	id, _ := ctx.Value(sessionKey{}).(uuid.UUID)
	return id
}

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
		if sessionID, err := uuid.Parse(r.Header.Get(HeaderSessionID)); err == nil {
			ctx = context.WithValue(ctx, sessionKey{}, sessionID)
		}
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
	r.Use(httpx.BodyLimitFunc(bodyLimit))

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
			exceptRailCallbacks(idempotency.Middleware(idemStore, idempotencyKeyTTL)),
			exceptRailCallbacks(requireCaller),
			keepCallbackBody,
		},
	})
	return r
}

// bodyLimit is 1 MB for every request but a statement import, which
// may carry a busy day's statement (task 4.6): 10 MB plus the JSON
// around it, as the gateway allows.
func bodyLimit(r *http.Request) int64 {
	if r.Method == http.MethodPost && r.URL.Path == "/v1/payments/staff/statement-imports" {
		return 11 << 20
	}
	return 1 << 20
}

// railCallbackPrefix is where payment rails call back (task 4.4). A rail
// sends neither a session nor an Idempotency-Key: its source address
// and path token authenticate it, and a repeated callback is recorded
// once.
const railCallbackPrefix = "/v1/payments/callbacks/"

func isRailCallback(r *http.Request) bool { return strings.HasPrefix(r.URL.Path, railCallbackPrefix) }

// exceptRailCallbacks runs mw for every request but a rail callback.
func exceptRailCallbacks(mw func(http.Handler) http.Handler) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		wrapped := mw(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isRailCallback(r) {
				next.ServeHTTP(w, r)
				return
			}
			wrapped.ServeHTTP(w, r)
		})
	}
}

type callbackKey struct{}

type callbackRequest struct {
	clientIP netip.Addr
	body     []byte
}

// keepCallbackBody keeps a rail callback's exact bytes, stored as the
// raw record, and the address the gateway saw: the strict server
// decodes the JSON before the handler runs (1.9's lesson).
func keepCallbackBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isRailCallback(r) {
			next.ServeHTTP(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			problem.Respond(w, r, problem.New(problem.VAL4000, "the request body could not be read"))
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		ip, _ := netip.ParseAddr(r.Header.Get(HeaderClientIP)) // none or malformed: refused by the source check
		ctx := context.WithValue(r.Context(), callbackKey{}, callbackRequest{clientIP: ip, body: body})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func callbackFromContext(ctx context.Context) (callbackRequest, bool) {
	cr, ok := ctx.Value(callbackKey{}).(callbackRequest)
	return cr, ok
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
