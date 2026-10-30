// Package identity is the identity service: wiring only. Business rules
// live in domain/, use cases in app/, HTTP routes in api/.
package identity

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/redis/go-redis/v9"
	gogrpc "google.golang.org/grpc"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/bus"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/cache"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/clock"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/config"
	vascrypto "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/crypto"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/httpx"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/idempotency"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/outbox"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/server"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/adapters"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/api"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/app"
	identitygrpc "github.com/Fredrick-Kairu/vaspro-v4/internal/identity/grpc"
)

// totpKMSKeyID names the LocalKMS master key TOTP secrets are
// encrypted under (platform/crypto.Envelope) — one key for now, no
// rotation story yet.
const totpKMSKeyID = "identity-totp"

// signingKeysKMSKeyID names the LocalKMS master key JWT signing keys'
// private material is sealed under (task 1.8) — a separate key from
// totpKMSKeyID's, wrapped by the same Envelope instance (0.11:
// Envelope/LocalKMS take a keyID per call, one KMS can hold several
// master keys).
const signingKeysKMSKeyID = "identity-signing-keys"

// Config is identity's process configuration (platform/config.Load).
// DatabaseURL's connection must resolve unqualified table names to the
// identity schema — either the connecting role's default search_path
// is set at provisioning time, or the DSN includes search_path=identity
// explicitly; platform/outbox queries unqualified table names and
// relies on it.
type Config struct {
	DatabaseURL string `env:"IDENTITY_DATABASE_URL,required"`
	NATSURL     string `env:"IDENTITY_NATS_URL,required"`
	HTTPAddr    string `env:"IDENTITY_HTTP_ADDR" envDefault:":8080"`
	// GRPCAddr serves identity's internal gRPC API (proto/identity/v1,
	// task 2.2a) to other services. Plaintext on the internal network
	// for now; mTLS between services is Phase 17 hardening.
	GRPCAddr  string `env:"IDENTITY_GRPC_ADDR" envDefault:":9090"`
	SMTPAddr  string `env:"IDENTITY_SMTP_ADDR" envDefault:"localhost:1025"`
	EmailFrom string `env:"IDENTITY_EMAIL_FROM" envDefault:"no-reply@vaspro.io"`

	// TOTPKMSKey is LocalKMS's master key, base64-encoded, exactly 32
	// bytes once decoded (AES-256) — encrypts user_totp.secret_ciphertext.
	// A real Cloud KMS/Vault implementation is still undecided (0.11's
	// own note); LocalKMS is what the tech stack table names for
	// development.
	TOTPKMSKey string `env:"IDENTITY_TOTP_KMS_KEY,required"`
	// SigningKeysKMSKey is LocalKMS's master key, base64-encoded,
	// exactly 32 bytes once decoded (AES-256) — encrypts
	// signing_keys.private_ref (task 1.8), the JWT signing keys'
	// private material. A separate key from TOTPKMSKey's, same
	// reasoning: different purpose, shouldn't share material.
	SigningKeysKMSKey string `env:"IDENTITY_SIGNING_KEYS_KMS_KEY,required"`
	// ChallengeSigningKey (base64) signs every short-lived, stateless
	// challenge token this service issues — the TOTP sign-in
	// challenge and the WebAuthn ceremony session token
	// (app.Service.issueChallenge/issueWebAuthnSession) —
	// platform/crypto.HMACSign, not the same key as TOTPKMSKey:
	// different purpose, shouldn't share material.
	ChallengeSigningKey string `env:"IDENTITY_CHALLENGE_SIGNING_KEY,required"`

	// WebAuthnRPID is the WebAuthn relying party ID — the domain a
	// passkey is scoped to (e.g. "dashboard.vaspro.io", or "localhost"
	// in development). WebAuthnOrigins must each be an origin under
	// this domain.
	WebAuthnRPID string `env:"IDENTITY_WEBAUTHN_RP_ID,required"`
	// WebAuthnRPDisplayName is shown to the user by the authenticator
	// UI during registration.
	WebAuthnRPDisplayName string `env:"IDENTITY_WEBAUTHN_RP_DISPLAY_NAME" envDefault:"VASPRO"`
	// WebAuthnOrigins are the exact origins (scheme+host+port) a
	// ceremony response is allowed to have been collected at.
	// Comma-separated.
	WebAuthnOrigins []string `env:"IDENTITY_WEBAUTHN_ORIGINS,required"`

	// RedisURL caches identity.sessions (1.5) — ResolveSession's
	// per-request check tries it before Postgres.
	RedisURL string `env:"IDENTITY_REDIS_URL,required"`

	// Sign in with Google (task 1.16). Optional: with no client ID,
	// Google sign-in is off and its routes return VAL-4003.
	// GoogleRedirectURL is the dashboard page Google sends the browser
	// back to; it must be registered with Google exactly. GoogleIssuer
	// is only overridden in tests (tools/fakes/oidc).
	GoogleClientID     string `env:"IDENTITY_GOOGLE_CLIENT_ID"`
	GoogleClientSecret string `env:"IDENTITY_GOOGLE_CLIENT_SECRET"`
	GoogleRedirectURL  string `env:"IDENTITY_GOOGLE_REDIRECT_URL"`
	GoogleIssuer       string `env:"IDENTITY_GOOGLE_ISSUER" envDefault:"https://accounts.google.com"`
}

// Service wires identity's dependencies together and runs them.
type Service struct {
	cfg  Config
	pool *db.Pool
	bus  *bus.Bus
	app  *app.Service
	rtr  http.Handler
}

// New builds a Service from already-connected dependencies. Bootstrap is
// the usual entry point; New exists separately so tests can build a
// Service against testkit's containers (and a fake email sender)
// without going through it.
func New(cfg Config, pool *db.Pool, b *bus.Bus, email adapters.EmailSender) (*Service, error) {
	totpKMSKey, err := base64.StdEncoding.DecodeString(cfg.TOTPKMSKey)
	if err != nil {
		return nil, fmt.Errorf("identity: decoding IDENTITY_TOTP_KMS_KEY: %w", err)
	}
	signingKeysKMSKey, err := base64.StdEncoding.DecodeString(cfg.SigningKeysKMSKey)
	if err != nil {
		return nil, fmt.Errorf("identity: decoding IDENTITY_SIGNING_KEYS_KMS_KEY: %w", err)
	}
	kms, err := vascrypto.NewLocalKMS(map[string][]byte{
		totpKMSKeyID:        totpKMSKey,
		signingKeysKMSKeyID: signingKeysKMSKey,
	})
	if err != nil {
		return nil, fmt.Errorf("identity: building KMS: %w", err)
	}
	challengeKey, err := base64.StdEncoding.DecodeString(cfg.ChallengeSigningKey)
	if err != nil {
		return nil, fmt.Errorf("identity: decoding IDENTITY_CHALLENGE_SIGNING_KEY: %w", err)
	}

	wa, err := webauthn.New(&webauthn.Config{
		RPID:          cfg.WebAuthnRPID,
		RPDisplayName: cfg.WebAuthnRPDisplayName,
		RPOrigins:     cfg.WebAuthnOrigins,
	})
	if err != nil {
		return nil, fmt.Errorf("identity: building webauthn config: %w", err)
	}

	redisOpts, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return nil, fmt.Errorf("identity: parsing IDENTITY_REDIS_URL: %w", err)
	}
	c := cache.New(redisOpts)

	var oidcProvider adapters.OIDCProvider
	if cfg.GoogleClientID != "" {
		if cfg.GoogleRedirectURL == "" {
			return nil, fmt.Errorf("identity: IDENTITY_GOOGLE_REDIRECT_URL is required when IDENTITY_GOOGLE_CLIENT_ID is set")
		}
		issuer := cfg.GoogleIssuer
		if issuer == "" {
			issuer = "https://accounts.google.com"
		}
		oidcProvider = adapters.NewGoogleOIDC(issuer, cfg.GoogleClientID, cfg.GoogleClientSecret, cfg.GoogleRedirectURL)
	}

	a := app.New(app.Deps{
		Pool:             pool,
		Email:            email,
		Clock:            clock.Real{},
		Envelope:         vascrypto.NewEnvelope(kms),
		TOTPKeyID:        totpKMSKeyID,
		SigningKeysKeyID: signingKeysKMSKeyID,
		ChallengeKey:     challengeKey,
		WebAuthn:         wa,
		Cache:            c,
		OIDC:             oidcProvider,
	})
	h := api.NewHandlers(a)
	store := idempotency.NewPostgresStore(pool)
	rtr := api.Routes(h, slog.Default(), []httpx.Check{
		func(ctx context.Context) error { return pool.Ping(ctx) },
	}, store, api.NewSessionLookup(a), a)
	return &Service{cfg: cfg, pool: pool, bus: b, app: a, rtr: rtr}, nil
}

// Bootstrap loads Config from the environment and connects every
// dependency New needs — the only thing cmd/identity and cmd/vaspro-all
// have to call.
func Bootstrap(ctx context.Context) (*Service, error) {
	cfg := config.MustLoad[Config]()

	pool, err := db.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("identity: connecting to database: %w", err)
	}
	b, err := bus.Connect(ctx, cfg.NATSURL)
	if err != nil {
		return nil, fmt.Errorf("identity: connecting to nats: %w", err)
	}
	email := adapters.NewSMTPSender(cfg.SMTPAddr, cfg.EmailFrom)
	return New(cfg, pool, b, email)
}

// Router is identity's HTTP handler, exported so tests can drive it
// with httptest without a real listener.
func (s *Service) Router() http.Handler { return s.rtr }

// EnsureActiveSigningKey delegates to app.Service.EnsureActiveSigningKey
// — exported so Run() and a test building a Service via New() directly
// (never calling Run()) can both bootstrap the first signing key.
func (s *Service) EnsureActiveSigningKey(ctx context.Context) error {
	return s.app.EnsureActiveSigningKey(ctx)
}

func (s *Service) relaySubject(eventType string) string { return "identity." + eventType }

// Run starts identity's HTTP server and outbox relay loop, and blocks
// until ctx is cancelled, then shuts down gracefully
// (platform/server.Serve). identity doesn't consume any events yet
// (1.1's own finding — nothing in Phase 1 needs it to), only publishes,
// but JetStream still needs the stream declared before Publish works.
func (s *Service) Run(ctx context.Context) error {
	if err := s.bus.EnsureStream(ctx, "IDENTITY_EVENTS", []string{"identity.>"}); err != nil {
		return fmt.Errorf("identity: ensuring stream: %w", err)
	}

	// Bootstraps the first signing key on a brand new database — a
	// test building a Service via New() directly (not Run()) must call
	// this itself; see EnsureActiveSigningKey's own doc.
	if err := s.EnsureActiveSigningKey(ctx); err != nil {
		return fmt.Errorf("identity: ensuring an active signing key: %w", err)
	}

	relay := outbox.NewRelay(s.pool, s.bus, s.relaySubject)
	relayDone := make(chan struct{})
	go func() {
		defer close(relayDone)
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_, _ = relay.RelayOnce(ctx)
			}
		}
	}()

	ln, err := net.Listen("tcp", s.cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("identity: listening on %s: %w", s.cfg.HTTPAddr, err)
	}

	grpcLn, err := net.Listen("tcp", s.cfg.GRPCAddr)
	if err != nil {
		_ = ln.Close()
		return fmt.Errorf("identity: listening for gRPC on %s: %w", s.cfg.GRPCAddr, err)
	}
	grpcSrv := gogrpc.NewServer()
	identitygrpc.Register(grpcSrv, s.app)
	grpcDone := make(chan struct{})
	go func() {
		defer close(grpcDone)
		_ = grpcSrv.Serve(grpcLn)
	}()

	srv := &http.Server{Handler: s.rtr, ReadHeaderTimeout: 5 * time.Second}
	err = server.Serve(ctx, srv, ln, 10*time.Second)
	grpcSrv.GracefulStop()
	<-grpcDone
	<-relayDone
	return err
}
