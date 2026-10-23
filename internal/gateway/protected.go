package gateway

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/clock"
)

// ProtectedRoute declares one method+pattern that requires a valid API
// client JWT (task 1.8), not identity's own dashboard session auth.
// Empty in production today (gateway.New's own caller) — no business
// endpoint that takes a scope-gated JWT exists yet (messaging etc.
// are later phases); this mechanism is still fully exercised by this
// package's own tests against a real identity route, same scoping
// principle task 1.9 used for HMAC signing: build and fully test the
// generic mechanism, wire it to nothing that doesn't exist yet.
type ProtectedRoute struct {
	Method  string
	Pattern string
	// Scope is the scope this route requires; empty means any valid
	// JWT is enough.
	Scope string
	// LiveOnly rejects a sandbox credential's token with AUTH-4016.
	LiveOnly bool
}

// protectedRouteRateLimit is the per-client_id rate limit the
// protected-route pipeline applies, in addition to the IP-keyed limit
// every proxied request already gets — same global-default-only
// reasoning ratelimit.go's own doc comment gives (no per-client
// column exists yet).
const (
	protectedRouteRateLimit  = 600
	protectedRouteRateWindow = time.Minute
)

// mountProtectedRoutes registers each of routes on r, wrapping
// handler (the same reverse-proxy handler every other request also
// reaches) with the full JWT → rate-limit(client_id) → scope →
// environment → ip-allowlist chain. Order matters: requireJWT must run
// first (everything after it reads Claims from context), and scope/
// environment/ip-allowlist all reuse the one JWT verification rather
// than re-parsing the token.
func mountProtectedRoutes(r chi.Router, routes []ProtectedRoute, jwks *jwksCache, clk clock.Clock, issuer string, limiter RateLimiter, handler http.Handler) {
	for _, route := range routes {
		mw := []func(http.Handler) http.Handler{
			requireJWT(jwks, clk, issuer),
			rateLimit(limiter, rateLimitByClientID, protectedRouteRateLimit, protectedRouteRateWindow),
			requireScope(route.Scope),
		}
		if route.LiveOnly {
			mw = append(mw, requireLiveEnvironment)
		}
		mw = append(mw, requireIPAllowlist)

		wrapped := handler
		for i := len(mw) - 1; i >= 0; i-- {
			wrapped = mw[i](wrapped)
		}
		r.Method(route.Method, route.Pattern, wrapped)
	}
}
