package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
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
// dashboard session (gateway/sessionauth.go). accounts trusts it
// because only the gateway can reach accounts; that is a deployment
// rule (Config's doc), not something accounts can check.
const HeaderUserID = "X-Vaspro-User-Id"

// HeaderSessionID is the session the gateway resolved, forwarded with
// the user; accounts asks identity whether its step-up is fresh before
// adding users (task 2.3b).
const HeaderSessionID = "X-Vaspro-Session-Id"

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
// The user also scopes Idempotency-Keys: an account doesn't exist yet
// when one is being created, so the key can't be scoped by tenant, and
// one shared anonymous scope lets two users' keys collide (1.12).
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

// Routes builds accounts' HTTP handler.
func Routes(h *Handlers, logger *slog.Logger, ready []httpx.Check, idemStore idempotency.Store) http.Handler {
	r := chi.NewRouter()
	r.Use(httpx.RequestID)
	r.Use(httpx.Recover(logger))
	r.Use(httpx.Timeout(30 * time.Second))
	r.Use(httpx.BodyLimit(1 << 20))

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
			exceptVendorWebhooks(idempotency.Middleware(idemStore, idempotencyKeyTTL)),
			exceptVendorWebhooks(requireCaller),
			keepWebhookBody,
		},
	})
	return r
}

// vendorWebhookPrefix is where KYC vendors post results (task 2.6). A
// vendor sends neither a session nor an Idempotency-Key; its signature,
// checked by the KYC adapter, authenticates the request, and repeated
// results are harmless by design (domain.NextKYCStatus).
const vendorWebhookPrefix = "/v1/accounts/kyc/webhooks/"

func isVendorWebhook(r *http.Request) bool { return strings.HasPrefix(r.URL.Path, vendorWebhookPrefix) }

// exceptVendorWebhooks runs mw for every request but a vendor webhook,
// as identity's skipIdempotencyFor does for its token endpoint.
func exceptVendorWebhooks(mw func(http.Handler) http.Handler) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		wrapped := mw(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isVendorWebhook(r) {
				next.ServeHTTP(w, r)
				return
			}
			wrapped.ServeHTTP(w, r)
		})
	}
}

type webhookKey struct{}

type webhookRequest struct {
	header http.Header
	body   []byte
}

// keepWebhookBody keeps a vendor webhook's exact bytes and headers for
// the handler: the strict server decodes the JSON before the handler
// runs, and the signature covers the bytes as sent (1.9's lesson).
func keepWebhookBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isVendorWebhook(r) {
			next.ServeHTTP(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			problem.Respond(w, r, problem.New(problem.VAL4000, "the request body could not be read"))
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		ctx := context.WithValue(r.Context(), webhookKey{}, webhookRequest{header: r.Header.Clone(), body: body})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func webhookFromContext(ctx context.Context) (webhookRequest, bool) {
	wr, ok := ctx.Value(webhookKey{}).(webhookRequest)
	return wr, ok
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
	slog.ErrorContext(r.Context(), "accounts: unexpected error", "error", err)
	problem.Respond(w, r, problem.New(problem.SYS9000, "An unexpected error occurred."))
}
