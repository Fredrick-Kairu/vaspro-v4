package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/getkin/kin-openapi/routers"
	"github.com/go-chi/chi/v5"
	"github.com/redis/go-redis/v9"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/cache"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/clock"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/config"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/httpx"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/server"
)

// oauthIssuer must match identity/app/oauthtoken.go's own oauthIssuer
// constant exactly — the gateway verifies against it, identity signs
// with it. Duplicated, not imported: the gateway must never import
// internal/identity (CLAUDE.md's service boundary).
const oauthIssuer = "vaspro-identity"

// oauthAudience must match identity/app/oauthtoken.go's own
// oauthAudience the same way, for the same reason. A token minted for
// any other audience is rejected even if its signature verifies.
const oauthAudience = "vaspro-api"

// globalRateLimit/globalRateWindow gate every proxied request by
// caller IP, ahead of (and independent from) the protected-route
// pipeline's own per-client_id limit (protected.go) — basic abuse
// protection for traffic that never carries a JWT at all (every
// production route today; see package doc).
const (
	globalRateLimit  = 1200
	globalRateWindow = time.Minute
)

// Config is the gateway's process configuration (platform/config.Load).
type Config struct {
	HTTPAddr string `env:"GATEWAY_HTTP_ADDR" envDefault:":8081"`

	// IdentityBaseURL receives every request no other backend claims.
	IdentityBaseURL string `env:"GATEWAY_IDENTITY_BASE_URL,required"`
	// IdentityOpenAPIPath is the spec requireValidRequestShape loads
	// at startup to validate every request proxied to identity.
	IdentityOpenAPIPath string `env:"GATEWAY_IDENTITY_OPENAPI_PATH" envDefault:"docs/api/identity.yaml"`
	// IdentityGRPCAddr is identity's internal gRPC API, used to check
	// dashboard sessions for every backend but identity (task 2.2a).
	IdentityGRPCAddr string `env:"GATEWAY_IDENTITY_GRPC_ADDR,required"`

	// AccountsBaseURL receives everything under /v1/accounts/, after
	// the gateway has checked the session (task 2.2a).
	AccountsBaseURL     string `env:"GATEWAY_ACCOUNTS_BASE_URL,required"`
	AccountsOpenAPIPath string `env:"GATEWAY_ACCOUNTS_OPENAPI_PATH" envDefault:"docs/api/accounts.yaml"`

	// LedgerBaseURL receives everything under /v1/ledger/ (staff token
	// prices, task 3.4b), after the gateway has checked the session.
	LedgerBaseURL     string `env:"GATEWAY_LEDGER_BASE_URL,required"`
	LedgerOpenAPIPath string `env:"GATEWAY_LEDGER_OPENAPI_PATH" envDefault:"docs/api/ledger.yaml"`

	// PaymentsBaseURL receives everything under /v1/payments/ (top-ups,
	// task 4.3), after the gateway has checked the session.
	PaymentsBaseURL     string `env:"GATEWAY_PAYMENTS_BASE_URL,required"`
	PaymentsOpenAPIPath string `env:"GATEWAY_PAYMENTS_OPENAPI_PATH" envDefault:"docs/api/payments.yaml"`

	// RedisURL backs rate-limit counters (RedisRateLimiter) — the
	// gateway's own connection, not identity's; it owns no schema to
	// share a service's Redis namespace with.
	RedisURL string `env:"GATEWAY_REDIS_URL,required"`

	// CORSAllowedOrigins lets browsers on these exact origins call the
	// API (the dashboard during development). Comma-separated, no
	// wildcard; empty means no CORS headers and preflights go to the
	// backends as before.
	CORSAllowedOrigins []string `env:"GATEWAY_CORS_ALLOWED_ORIGINS"`

	// TrustedProxies are the reverse proxies (IPs or CIDRs,
	// comma-separated) whose X-Forwarded-For names the real caller for
	// rate limits, IP allowlists and the audit log (resolveClientIP).
	// Empty: the connection's own address, as before.
	TrustedProxies []string `env:"GATEWAY_TRUSTED_PROXIES"`
}

// Service wires the gateway's dependencies together.
type Service struct {
	cfg Config
	rtr http.Handler
}

// New builds a Service from already-connected dependencies. protected
// is the production route table — empty today (see ProtectedRoute's
// own doc); tests pass their own to exercise the pipeline against a
// real identity route. sessions checks dashboard sessions for every
// backend but identity.
func New(cfg Config, c *cache.Cache, clk clock.Clock, logger *slog.Logger, protected []ProtectedRoute, sessions SessionResolver) (*Service, error) {
	identityTarget, err := url.Parse(cfg.IdentityBaseURL)
	if err != nil {
		return nil, fmt.Errorf("gateway: parsing GATEWAY_IDENTITY_BASE_URL: %w", err)
	}
	accountsTarget, err := url.Parse(cfg.AccountsBaseURL)
	if err != nil {
		return nil, fmt.Errorf("gateway: parsing GATEWAY_ACCOUNTS_BASE_URL: %w", err)
	}
	ledgerTarget, err := url.Parse(cfg.LedgerBaseURL)
	if err != nil {
		return nil, fmt.Errorf("gateway: parsing GATEWAY_LEDGER_BASE_URL: %w", err)
	}
	paymentsTarget, err := url.Parse(cfg.PaymentsBaseURL)
	if err != nil {
		return nil, fmt.Errorf("gateway: parsing GATEWAY_PAYMENTS_BASE_URL: %w", err)
	}
	trustedProxies, err := parseIPAllowlist(cfg.TrustedProxies)
	if err != nil {
		return nil, fmt.Errorf("gateway: parsing GATEWAY_TRUSTED_PROXIES: %w", err)
	}
	identitySpec, identityRoutes, err := loadSpecAndRoutes(cfg.IdentityOpenAPIPath)
	if err != nil {
		return nil, err
	}
	accountsSpec, accountsRoutes, err := loadSpecAndRoutes(cfg.AccountsOpenAPIPath)
	if err != nil {
		return nil, err
	}
	ledgerSpec, ledgerRoutes, err := loadSpecAndRoutes(cfg.LedgerOpenAPIPath)
	if err != nil {
		return nil, err
	}
	paymentsSpec, paymentsRoutes, err := loadSpecAndRoutes(cfg.PaymentsOpenAPIPath)
	if err != nil {
		return nil, err
	}

	jwks := newJWKSCache(cfg.IdentityBaseURL+"/.well-known/jwks.json", clk)
	limiter := NewRedisRateLimiter(c)
	identityProxy := requireValidRequestShape(identitySpec)(newReverseProxy(identityTarget))
	accountsProxy := newReverseProxy(accountsTarget)
	ledgerProxy := newReverseProxy(ledgerTarget)
	paymentsProxy := newReverseProxy(paymentsTarget)

	r := chi.NewRouter()
	if len(cfg.CORSAllowedOrigins) > 0 {
		r.Use(httpx.CORS(httpx.CORSConfig{
			AllowedOrigins: cfg.CORSAllowedOrigins,
			AllowedMethods: []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions},
			AllowedHeaders: []string{"Authorization", "Content-Type", "Idempotency-Key", "X-Request-Id"},
			MaxAge:         10 * time.Minute,
		}))
	}
	r.Use(httpx.RequestID)
	r.Use(resolveClientIP(trustedProxies))
	r.Use(httpx.Recover(logger))
	r.Use(httpx.Timeout(30 * time.Second))
	r.Use(refuseOversizedUploads)
	r.Use(httpx.BodyLimitFunc(bodyLimit))

	r.Get("/healthz", httpx.Healthz)
	r.Get("/readyz", httpx.Readyz())

	proxied := chi.NewRouter()
	proxied.Use(stripForwardedIdentity)
	proxied.Use(audit(logger, []routers.Router{identityRoutes, accountsRoutes, ledgerRoutes, paymentsRoutes}))
	proxied.Use(rateLimit(limiter, rateLimitByIP, globalRateLimit, globalRateWindow))
	proxied.Use(requireIdempotencyKeyHeader)

	// accounts: the session is checked here, then the request is
	// validated against accounts' own spec and forwarded with the user
	// (sessionauth.go).
	proxied.Route("/v1/accounts", func(r chi.Router) {
		r.Use(requireSessionUnlessPublic(sessions, accountsRoutes))
		r.Use(requireValidRequestShape(accountsSpec))
		r.Handle("/*", accountsProxy)
	})

	// ledger: the same, against the ledger's spec (task 3.4b).
	proxied.Route("/v1/ledger", func(r chi.Router) {
		r.Use(requireSessionUnlessPublic(sessions, ledgerRoutes))
		r.Use(requireValidRequestShape(ledgerSpec))
		r.Handle("/*", ledgerProxy)
	})

	// payments' hosted checkout page (task 4.8): HTML for anyone with a
	// link, no session, and not API, so no spec validation. The link's
	// token is in the path; the audit log records unmatched paths as
	// "unmatched", so it's never logged.
	proxied.Handle("/pay/*", paymentsProxy)

	// payments: the same, against payments' spec (task 4.3).
	proxied.Route("/v1/payments", func(r chi.Router) {
		r.Use(requireSessionUnlessPublic(sessions, paymentsRoutes))
		r.Use(requireValidRequestShape(paymentsSpec))
		r.Handle("/*", paymentsProxy)
	})

	// identity: everything else. Identity checks its own sessions.
	mountProtectedRoutes(proxied, protected, jwks, clk, oauthIssuer, oauthAudience, limiter, identityProxy)
	proxied.NotFound(identityProxy.ServeHTTP)
	proxied.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) { identityProxy.ServeHTTP(w, r) })

	r.Mount("/", proxied)

	return &Service{cfg: cfg, rtr: r}, nil
}

// Bootstrap loads Config from the environment and connects every
// dependency New needs.
func Bootstrap(_ context.Context) (*Service, error) {
	cfg := config.MustLoad[Config]()

	redisOpts, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return nil, fmt.Errorf("gateway: parsing GATEWAY_REDIS_URL: %w", err)
	}
	c := cache.New(redisOpts)

	sessions, err := DialGRPCSessions(cfg.IdentityGRPCAddr)
	if err != nil {
		return nil, err
	}
	return New(cfg, c, clock.Real{}, slog.Default(), nil, sessions)
}

// Router is the gateway's HTTP handler, exported so tests can drive it
// with httptest without a real listener.
func (s *Service) Router() http.Handler { return s.rtr }

// Run starts the gateway's HTTP server and blocks until ctx is
// cancelled, then shuts down gracefully (platform/server.Serve). The
// gateway owns no outbox/relay — it owns no schema at all (package
// doc) — so there's nothing else to run alongside the server.
func (s *Service) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("gateway: listening on %s: %w", s.cfg.HTTPAddr, err)
	}
	srv := &http.Server{Handler: s.rtr, ReadHeaderTimeout: 5 * time.Second}
	return server.Serve(ctx, srv, ln, 10*time.Second)
}
