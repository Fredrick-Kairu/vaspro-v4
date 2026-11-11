// Package payments is the payments service: VASPRO's own paybill and
// wallet top-ups in Phase 4, the rails clients collect and pay out on
// later (data model §7, system design §4.1 and §13). Wiring only; rules
// live in domain/, use cases in app/, the HTTP API in api/.
package payments

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/bus"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/cache"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/clock"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/config"
	vascrypto "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/crypto"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/httpx"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/idempotency"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/outbox"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/packs"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/server"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments/adapters"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments/api"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments/app"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments/domain"
)

// Config is payments' process configuration (platform/config.Load).
//
// HTTPAddr serves the HTTP API (docs/api/payments.yaml) and the health
// checks. Like the ledger's, it trusts the X-Vaspro-User-Id header the
// gateway sets after checking the session, so it must be reachable only
// from the gateway.
//
// DatabaseURL's role must be granted payments_service and must not own
// the payments tables (migration 20261111090000); its search_path must
// resolve unqualified names to the payments schema
// (search_path=payments,public), which platform/outbox relies on.
type Config struct {
	DatabaseURL string `env:"PAYMENTS_DATABASE_URL,required"`
	NATSURL     string `env:"PAYMENTS_NATS_URL,required"`
	HTTPAddr    string `env:"PAYMENTS_HTTP_ADDR" envDefault:":8084"`
	// AccountsGRPCAddr is accounts' internal gRPC API, asked on every
	// dashboard call whether the user may work in the account, and the
	// account's country (task 4.3). Dialled lazily; dashboard calls fail
	// closed while accounts is down.
	AccountsGRPCAddr string `env:"PAYMENTS_ACCOUNTS_GRPC_ADDR,required"`
	// IdentityGRPCAddr is identity's internal gRPC API, asked on every
	// staff call whether the user is staff and, before a match, whether
	// their step-up is fresh (task 4.5). Dialled lazily; staff calls fail
	// closed while identity is down.
	IdentityGRPCAddr string `env:"PAYMENTS_IDENTITY_GRPC_ADDR,required"`
	// PacksDir holds schema.json and one <cc>/pack.yaml per country: the
	// rails, and which one takes top-ups.
	PacksDir string `env:"PAYMENTS_PACKS_DIR" envDefault:"packs"`

	// RedisURL caches rails' access tokens until they expire (data model
	// §7), payments' own connection.
	RedisURL string `env:"PAYMENTS_REDIS_URL,required"`
	// LedgerGRPCAddr is the ledger's posting API: confirmed top-ups are
	// posted there (task 4.4). Dialled lazily; postings wait for the
	// sweep while the ledger is down.
	LedgerGRPCAddr string `env:"PAYMENTS_LEDGER_GRPC_ADDR,required"`
	// DarajaBaseURL is Safaricom's Daraja API (the fake in development,
	// https://sandbox.safaricom.co.ke on staging).
	DarajaBaseURL string `env:"DARAJA_BASE_URL,required"`
	// CredentialsKMSKey opens VASPRO's rail credentials (base64 of 32
	// bytes; LocalKMS until a cloud KMS is chosen).
	CredentialsKMSKey string `env:"PAYMENTS_CREDENTIALS_KMS_KEY,required"`
	// CallbackBaseURL is the public base URL rails call back on: the
	// gateway's, e.g. https://api.vaspro.co.ke.
	CallbackBaseURL string `env:"PAYMENTS_CALLBACK_BASE_URL,required"`
	// CallbackSecret keys callback URLs' path tokens (base64 of 32
	// bytes). Changing it invalidates the URLs of pushes in flight.
	CallbackSecret string `env:"PAYMENTS_CALLBACK_SECRET,required"`
	// DarajaCallbackCIDRs are Safaricom's published callback source
	// ranges, comma-separated (OQ-22). Empty refuses every callback.
	DarajaCallbackCIDRs string `env:"PAYMENTS_DARAJA_CALLBACK_CIDRS,required"`
	// STKExpiry is how long an STK top-up waits for its callback before
	// the sweep asks the rail; STKGiveUp how much longer it keeps asking.
	STKExpiry time.Duration `env:"PAYMENTS_STK_EXPIRY" envDefault:"3m"`
	STKGiveUp time.Duration `env:"PAYMENTS_STK_GIVE_UP" envDefault:"10m"`
	// SweepInterval is how often the sweep posts, applies and expires.
	SweepInterval time.Duration `env:"PAYMENTS_SWEEP_INTERVAL" envDefault:"30s"`
	// StatementDue is how long after local midnight yesterday's
	// statement must be imported before recon.statement_missing (task
	// 4.6), e.g. 12h for noon.
	StatementDue time.Duration `env:"PAYMENTS_STATEMENT_DUE" envDefault:"12h"`
}

// Clients are the outside services payments calls.
type Clients struct {
	Accounts adapters.Accounts
	Identity adapters.Identity
	Ledger   adapters.Ledger
	Cache    *cache.Cache
}

// darajaRails are the rails Safaricom's Daraja API serves.
var darajaRails = map[string]bool{"mpesa_ke": true}

// Service wires payments' dependencies together and runs them.
type Service struct {
	cfg  Config
	pool *db.Pool
	bus  *bus.Bus
	app  *app.Service
	rtr  http.Handler
}

// New builds a Service from already-connected dependencies and the
// loaded country packs, by country code.
func New(cfg Config, pool *db.Pool, b *bus.Bus, c Clients, loaded map[string]*packs.Pack) (*Service, error) {
	kmsKey, err := base64.StdEncoding.DecodeString(cfg.CredentialsKMSKey)
	if err != nil {
		return nil, fmt.Errorf("payments: PAYMENTS_CREDENTIALS_KMS_KEY: %w", err)
	}
	kms, err := vascrypto.NewLocalKMS(map[string][]byte{app.CredentialsKMSKeyID: kmsKey})
	if err != nil {
		return nil, fmt.Errorf("payments: PAYMENTS_CREDENTIALS_KMS_KEY: %w", err)
	}
	secret, err := base64.StdEncoding.DecodeString(cfg.CallbackSecret)
	if err != nil || len(secret) != 32 {
		return nil, errors.New("payments: PAYMENTS_CALLBACK_SECRET must be base64 of 32 bytes")
	}
	cidrs, err := domain.ParseCIDRs(cfg.DarajaCallbackCIDRs)
	if err != nil {
		return nil, fmt.Errorf("payments: PAYMENTS_DARAJA_CALLBACK_CIDRS: %w", err)
	}
	creds := app.NewCredentialSource(pool, kms)
	rails := map[string]app.RailClient{}
	for _, p := range loaded {
		for _, r := range p.Rails {
			if !darajaRails[r.Code] {
				continue
			}
			loc, err := time.LoadLocation(p.Timezone)
			if err != nil {
				return nil, fmt.Errorf("payments: %s's time zone: %w", p.CountryCode, err)
			}
			rails[r.Code] = adapters.NewDaraja(adapters.DarajaConfig{
				BaseURL: cfg.DarajaBaseURL, Cache: c.Cache, Credentials: creds, Location: loc, Clock: clock.Real{},
			})
		}
	}
	a := app.New(app.Deps{
		Pool: pool, Clock: clock.Real{}, Logger: slog.Default(), Accounts: c.Accounts, Identity: c.Identity, Packs: loaded,
		KMS: kms, Rails: rails, Ledger: c.Ledger,
		CallbackBaseURL: cfg.CallbackBaseURL, CallbackSecret: secret, CallbackCIDRs: cidrs,
		STKExpiry: cfg.STKExpiry, STKGiveUp: cfg.STKGiveUp, StatementDue: cfg.StatementDue,
	})
	rtr := api.Routes(api.NewHandlers(a), slog.Default(), []httpx.Check{
		func(ctx context.Context) error { return pool.Ping(ctx) },
	}, idempotency.NewPostgresStore(pool))
	return &Service{cfg: cfg, pool: pool, bus: b, app: a, rtr: rtr}, nil
}

// Bootstrap loads Config from the environment and connects every
// dependency New needs.
func Bootstrap(ctx context.Context) (*Service, error) {
	cfg := config.MustLoad[Config]()
	pool, err := db.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("payments: connecting to database: %w", err)
	}
	b, err := bus.Connect(ctx, cfg.NATSURL)
	if err != nil {
		return nil, fmt.Errorf("payments: connecting to nats: %w", err)
	}
	accounts, err := adapters.DialAccounts(cfg.AccountsGRPCAddr)
	if err != nil {
		return nil, fmt.Errorf("payments: %w", err)
	}
	identity, err := adapters.DialIdentity(cfg.IdentityGRPCAddr)
	if err != nil {
		return nil, fmt.Errorf("payments: %w", err)
	}
	ledger, err := adapters.DialLedger(cfg.LedgerGRPCAddr)
	if err != nil {
		return nil, fmt.Errorf("payments: %w", err)
	}
	redisOpts, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return nil, fmt.Errorf("payments: parsing PAYMENTS_REDIS_URL: %w", err)
	}
	loaded, err := LoadPacks(cfg.PacksDir)
	if err != nil {
		return nil, err
	}
	return New(cfg, pool, b, Clients{Accounts: accounts, Identity: identity, Ledger: ledger, Cache: cache.New(redisOpts)}, loaded)
}

// LoadPacks loads every <dir>/<cc>/pack.yaml against <dir>/schema.json.
func LoadPacks(dir string) (map[string]*packs.Pack, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*", "pack.yaml"))
	if err != nil {
		return nil, fmt.Errorf("payments: finding packs: %w", err)
	}
	out := make(map[string]*packs.Pack, len(paths))
	for _, path := range paths {
		p, err := packs.Load(filepath.Join(dir, "schema.json"), path)
		if err != nil {
			return nil, fmt.Errorf("payments: %w", err)
		}
		out[p.CountryCode] = p
	}
	return out, nil
}

func relaySubject(eventType string) string { return "payments." + eventType }

// Run syncs the rails from the packs, then starts the HTTP server and
// the outbox relay, and blocks until ctx is cancelled.
func (s *Service) Run(ctx context.Context) error {
	if err := s.app.SyncRails(ctx); err != nil {
		return fmt.Errorf("payments: syncing rails from the packs: %w", err)
	}
	if err := s.bus.EnsureStream(ctx, "PAYMENTS_EVENTS", []string{"payments.>"}); err != nil {
		return fmt.Errorf("payments: ensuring stream: %w", err)
	}
	relay := outbox.NewRelay(s.pool, s.bus, relaySubject)
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

	sweepDone := make(chan struct{})
	go func() {
		defer close(sweepDone)
		s.sweepEvery(ctx)
	}()

	ln, err := net.Listen("tcp", s.cfg.HTTPAddr)
	if err != nil {
		<-relayDone
		<-sweepDone
		return fmt.Errorf("payments: listening on %s: %w", s.cfg.HTTPAddr, err)
	}
	srv := &http.Server{Handler: s.rtr, ReadHeaderTimeout: 5 * time.Second}
	err = server.Serve(ctx, srv, ln, 10*time.Second)
	<-relayDone
	<-sweepDone
	return err
}

// sweepEvery posts, applies and expires what callbacks left (task 4.4)
// on every interval. A failed sweep is logged; the next picks its work
// up.
func (s *Service) sweepEvery(ctx context.Context) {
	interval := s.cfg.SweepInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		sum, err := s.app.Sweep(ctx)
		if err != nil {
			slog.ErrorContext(ctx, "payments: sweep failed", "error", err)
		} else if sum != (app.SweepSummary{}) {
			slog.InfoContext(ctx, "payments: swept", "posted", sum.Posted, "applied", sum.Applied, "queried", sum.Queried, "expired", sum.Expired)
		}
		if _, err := s.app.CheckStatements(ctx); err != nil {
			slog.ErrorContext(ctx, "payments: checking for missing statements failed", "error", err)
		}
	}
}
