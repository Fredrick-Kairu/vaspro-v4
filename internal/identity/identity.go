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

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/bus"
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
)

// totpKMSKeyID names the LocalKMS master key TOTP secrets are
// encrypted under (platform/crypto.Envelope) — one key for now, no
// rotation story yet.
const totpKMSKeyID = "identity-totp"

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
	SMTPAddr    string `env:"IDENTITY_SMTP_ADDR" envDefault:"localhost:1025"`
	EmailFrom   string `env:"IDENTITY_EMAIL_FROM" envDefault:"no-reply@vaspro.io"`

	// TOTPKMSKey is LocalKMS's master key, base64-encoded, exactly 32
	// bytes once decoded (AES-256) — encrypts user_totp.secret_ciphertext.
	// A real Cloud KMS/Vault implementation is still undecided (0.11's
	// own note); LocalKMS is what the tech stack table names for
	// development.
	TOTPKMSKey string `env:"IDENTITY_TOTP_KMS_KEY,required"`
	// ChallengeSigningKey (base64) signs the short-lived TOTP sign-in
	// challenge token (app.Service.issueChallenge) — platform/crypto.HMACSign,
	// not the same key as TOTPKMSKey: different purpose, shouldn't
	// share material.
	ChallengeSigningKey string `env:"IDENTITY_CHALLENGE_SIGNING_KEY,required"`
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
	kmsKey, err := base64.StdEncoding.DecodeString(cfg.TOTPKMSKey)
	if err != nil {
		return nil, fmt.Errorf("identity: decoding IDENTITY_TOTP_KMS_KEY: %w", err)
	}
	kms, err := vascrypto.NewLocalKMS(map[string][]byte{totpKMSKeyID: kmsKey})
	if err != nil {
		return nil, fmt.Errorf("identity: building TOTP KMS: %w", err)
	}
	challengeKey, err := base64.StdEncoding.DecodeString(cfg.ChallengeSigningKey)
	if err != nil {
		return nil, fmt.Errorf("identity: decoding IDENTITY_CHALLENGE_SIGNING_KEY: %w", err)
	}

	a := app.New(app.Deps{
		Pool:         pool,
		Email:        email,
		Clock:        clock.Real{},
		Envelope:     vascrypto.NewEnvelope(kms),
		TOTPKeyID:    totpKMSKeyID,
		ChallengeKey: challengeKey,
	})
	h := api.NewHandlers(a)
	store := idempotency.NewPostgresStore(pool)
	rtr := api.Routes(h, slog.Default(), []httpx.Check{
		func(ctx context.Context) error { return pool.Ping(ctx) },
	}, store, api.NewSessionLookup(pool, clock.Real{}))
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

	srv := &http.Server{Handler: s.rtr, ReadHeaderTimeout: 5 * time.Second}
	err = server.Serve(ctx, srv, ln, 10*time.Second)
	<-relayDone
	return err
}
