package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/getkin/kin-openapi/routers/gorillamux"
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

	// IdentityBaseURL is the one backend this phase has — identity.
	// A future service adds its own *BaseURL + prefix, not a change to
	// how this one works.
	IdentityBaseURL string `env:"GATEWAY_IDENTITY_BASE_URL,required"`
	// IdentityOpenAPIPath is the spec requireValidRequestShape loads
	// at startup to validate every request proxied to identity.
	IdentityOpenAPIPath string `env:"GATEWAY_IDENTITY_OPENAPI_PATH" envDefault:"docs/api/identity.yaml"`

	// RedisURL backs rate-limit counters (RedisRateLimiter) — the
	// gateway's own connection, not identity's; it owns no schema to
	// share a service's Redis namespace with.
	RedisURL string `env:"GATEWAY_REDIS_URL,required"`
}

// Service wires the gateway's dependencies together.
type Service struct {
	cfg Config
	rtr http.Handler
}

// New builds a Service from already-connected dependencies. protected
// is the production route table — empty today (see ProtectedRoute's
// own doc); tests pass their own to exercise the pipeline against a
// real identity route.
func New(cfg Config, c *cache.Cache, clk clock.Clock, logger *slog.Logger, protected []ProtectedRoute) (*Service, error) {
	target, err := url.Parse(cfg.IdentityBaseURL)
	if err != nil {
		return nil, fmt.Errorf("gateway: parsing GATEWAY_IDENTITY_BASE_URL: %w", err)
	}
	spec, err := loadOpenAPISpec(cfg.IdentityOpenAPIPath)
	if err != nil {
		return nil, err
	}
	specRoutes, err := gorillamux.NewRouter(spec)
	if err != nil {
		return nil, fmt.Errorf("gateway: building openapi route table: %w", err)
	}

	jwks := newJWKSCache(cfg.IdentityBaseURL+"/.well-known/jwks.json", clk)
	limiter := NewRedisRateLimiter(c)
	proxyHandler := newReverseProxy(target)

	r := chi.NewRouter()
	r.Use(httpx.RequestID)
	r.Use(httpx.Recover(logger))
	r.Use(httpx.Timeout(30 * time.Second))
	r.Use(httpx.BodyLimit(1 << 20))

	r.Get("/healthz", httpx.Healthz)
	r.Get("/readyz", httpx.Readyz())

	proxied := chi.NewRouter()
	proxied.Use(audit(logger, specRoutes))
	proxied.Use(rateLimit(limiter, rateLimitByIP, globalRateLimit, globalRateWindow))
	proxied.Use(requireIdempotencyKeyHeader)
	proxied.Use(requireValidRequestShape(spec))

	mountProtectedRoutes(proxied, protected, jwks, clk, oauthIssuer, oauthAudience, limiter, proxyHandler)
	proxied.NotFound(proxyHandler.ServeHTTP)
	proxied.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) { proxyHandler.ServeHTTP(w, r) })

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

	return New(cfg, c, clock.Real{}, slog.Default(), nil)
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
