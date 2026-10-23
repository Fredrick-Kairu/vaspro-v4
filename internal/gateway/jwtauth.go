// Package gateway is the single entry point every request comes
// through (system design §1/§3): it terminates public traffic, checks
// the API client JWT task 1.8 mints, enforces scopes/environment/IP
// allowlist/rate limits/Idempotency-Key presence/OpenAPI request shape,
// logs an audit line, and proxies whatever's left to identity (the
// only backend that exists yet).
//
// The gateway owns no Postgres schema (data model §2's schema map
// lists 14 services; the gateway isn't one of them) and must reach
// identity only over HTTP — never by importing internal/identity or
// reading its tables (CLAUDE.md's service boundary applies to the
// gateway exactly as it does to every other service).
package gateway

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/lestrrat-go/jwx/v3/jwk"
	"github.com/lestrrat-go/jwx/v3/jwt"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/clock"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/problem"
)

// jwksCacheTTL is how long a fetched JWKS is trusted before the next
// request triggers a fresh fetch — long enough that a steady stream of
// requests doesn't refetch on every one, short enough that a rotated
// signing key (task 1.8: active/retiring/retired) is picked up
// promptly. No background refresh loop: a lazy, request-triggered
// refresh is simpler to reason about and to test than a goroutine with
// its own lifecycle, and the cost (one extra fetch on the first
// request after the TTL lapses) is negligible.
const jwksCacheTTL = 5 * time.Minute

// jwksCache lazily fetches and caches identity's published JWKS.
type jwksCache struct {
	url string

	mu        sync.Mutex
	set       jwk.Set
	fetchedAt time.Time
	clk       clock.Clock
}

func newJWKSCache(url string, clk clock.Clock) *jwksCache {
	return &jwksCache{url: url, clk: clk}
}

func (c *jwksCache) get(ctx context.Context) (jwk.Set, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.set != nil && c.clk.Now().Sub(c.fetchedAt) < jwksCacheTTL {
		return c.set, nil
	}
	set, err := jwk.Fetch(ctx, c.url)
	if err != nil {
		if c.set != nil {
			// A transient fetch failure shouldn't take down every
			// protected route if the previous fetch is still usable —
			// keep serving the stale set rather than failing closed.
			return c.set, nil
		}
		return nil, fmt.Errorf("gateway: fetching jwks from %s: %w", c.url, err)
	}
	c.set = set
	c.fetchedAt = c.clk.Now()
	return c.set, nil
}

// Claims is what the JWT a client exchanged (task 1.8) carries, as the
// gateway's own middlewares need it: ClientID/TenantID/Environment/
// Scopes/IPAllowlist mirror the token's client_id/tid/env/scope/
// ip_allowlist claims exactly.
type Claims struct {
	ClientID    string
	TenantID    string
	Environment string
	Scopes      []string
	IPAllowlist []string
}

func (c Claims) hasScope(scope string) bool {
	return scope == "" || slices.Contains(c.Scopes, scope)
}

type claimsKey struct{}

func withClaims(ctx context.Context, c Claims) context.Context {
	return context.WithValue(ctx, claimsKey{}, c)
}

// claimsFromContext returns the Claims requireJWT resolved for this
// request. Only ever called from middlewares requireJWT runs ahead of
// in the same chain (scope/environment/ip-allowlist checks, rate
// limiting keyed by client_id) — a missing value here would be a
// wiring bug, not a client error.
func claimsFromContext(ctx context.Context) (Claims, bool) {
	c, ok := ctx.Value(claimsKey{}).(Claims)
	return c, ok
}

// requireJWT verifies Authorization: Bearer <jwt> against jwks,
// rejecting a missing header, malformed token, bad signature, unknown
// kid, expired token or wrong issuer alike with AUTH-4010 — same
// "don't distinguish" anti-enumeration principle 1.3 established for
// sign-in, now applied to the machine-credential side of auth. Unlike
// identity's own requireSession (a dashboard session ID), this is the
// OAuth2 client-credentials JWT task 1.8 mints — a different
// mechanism for a different audience, not a replacement for it.
func requireJWT(jwks *jwksCache, clk clock.Clock, issuer string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			const prefix = "Bearer "
			header := r.Header.Get("Authorization")
			if !strings.HasPrefix(header, prefix) {
				rejectUnauthenticated(w, r)
				return
			}
			raw := strings.TrimPrefix(header, prefix)

			set, err := jwks.get(r.Context())
			if err != nil {
				problem.Respond(w, r, problem.New(problem.SYS9000, "An unexpected error occurred."))
				return
			}

			token, err := jwt.Parse([]byte(raw), jwt.WithKeySet(set), jwt.WithClock(clk), jwt.WithIssuer(issuer))
			if err != nil {
				rejectUnauthenticated(w, r)
				return
			}

			claims := Claims{}
			if err := token.Get("client_id", &claims.ClientID); err != nil {
				rejectUnauthenticated(w, r)
				return
			}
			_ = token.Get("tid", &claims.TenantID)
			_ = token.Get("env", &claims.Environment)

			var scope string
			if err := token.Get("scope", &scope); err == nil && scope != "" {
				claims.Scopes = strings.Split(scope, " ")
			}
			var ipAllowlist string
			if err := token.Get("ip_allowlist", &ipAllowlist); err == nil && ipAllowlist != "" {
				claims.IPAllowlist = strings.Split(ipAllowlist, " ")
			}

			next.ServeHTTP(w, r.WithContext(withClaims(r.Context(), claims)))
		})
	}
}

func rejectUnauthenticated(w http.ResponseWriter, r *http.Request) {
	problem.Respond(w, r, problem.New(problem.AUTH4010, "a bearer token is required"))
}
