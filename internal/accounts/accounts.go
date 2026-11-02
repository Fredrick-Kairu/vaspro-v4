// Package accounts is the accounts service: client accounts (tenants),
// members and verification tiers (data model §4). Wiring only; business
// rules live in domain/, use cases in app/, HTTP in api/.
package accounts

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"time"

	gogrpc "google.golang.org/grpc"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/bus"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/clock"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/config"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/httpx"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/idempotency"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/outbox"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/packs"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/server"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/accounts/api"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/accounts/app"
	accountsgrpc "github.com/Fredrick-Kairu/vaspro-v4/internal/accounts/grpc"
)

// Config is accounts' process configuration (platform/config.Load).
//
// accounts trusts the X-Vaspro-User-Id header the gateway sets after
// checking the session (task 2.2a), so HTTPAddr must be reachable only
// from the gateway: a deployment rule (compose network, production
// network policy in Phase 17), not something accounts can check.
//
// DatabaseURL's connection must resolve unqualified table names to the
// accounts schema (search_path=accounts,public; public for citext);
// platform/outbox and platform/idempotency rely on it.
type Config struct {
	DatabaseURL string `env:"ACCOUNTS_DATABASE_URL,required"`
	NATSURL     string `env:"ACCOUNTS_NATS_URL,required"`
	HTTPAddr    string `env:"ACCOUNTS_HTTP_ADDR" envDefault:":8082"`
	// GRPCAddr serves accounts' internal gRPC API (proto/accounts/v1,
	// task 2.2c) to other services, plaintext on the internal network
	// for now (mTLS between services is Phase 17).
	GRPCAddr string `env:"ACCOUNTS_GRPC_ADDR" envDefault:":9091"`
	// PacksDir holds schema.json and one <cc>/pack.yaml per country.
	PacksDir string `env:"ACCOUNTS_PACKS_DIR" envDefault:"packs"`
}

// Service wires accounts' dependencies together and runs them.
type Service struct {
	cfg  Config
	pool *db.Pool
	bus  *bus.Bus
	app  *app.Service
	rtr  http.Handler
}

// New builds a Service from already-connected dependencies.
func New(cfg Config, pool *db.Pool, b *bus.Bus) (*Service, error) {
	loaded, err := loadPacks(cfg.PacksDir)
	if err != nil {
		return nil, err
	}
	a := app.New(app.Deps{Pool: pool, Clock: clock.Real{}, Packs: loaded})
	rtr := api.Routes(api.NewHandlers(a), slog.Default(), []httpx.Check{
		func(ctx context.Context) error { return pool.Ping(ctx) },
	}, idempotency.NewPostgresStore(pool))
	return &Service{cfg: cfg, pool: pool, bus: b, app: a, rtr: rtr}, nil
}

// loadPacks loads every country pack under dir, keyed by country code.
func loadPacks(dir string) (map[string]*packs.Pack, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*", "pack.yaml"))
	if err != nil {
		return nil, fmt.Errorf("accounts: finding packs: %w", err)
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("accounts: no country packs under %s", dir)
	}
	loaded := make(map[string]*packs.Pack, len(paths))
	for _, p := range paths {
		pack, err := packs.Load(filepath.Join(dir, "schema.json"), p)
		if err != nil {
			return nil, fmt.Errorf("accounts: loading %s: %w", p, err)
		}
		loaded[pack.CountryCode] = pack
	}
	return loaded, nil
}

// Bootstrap loads Config from the environment and connects every
// dependency New needs.
func Bootstrap(ctx context.Context) (*Service, error) {
	cfg := config.MustLoad[Config]()
	pool, err := db.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("accounts: connecting to database: %w", err)
	}
	b, err := bus.Connect(ctx, cfg.NATSURL)
	if err != nil {
		return nil, fmt.Errorf("accounts: connecting to nats: %w", err)
	}
	return New(cfg, pool, b)
}

// Router is accounts' HTTP handler, exported for tests.
func (s *Service) Router() http.Handler { return s.rtr }

func relaySubject(eventType string) string { return "accounts." + eventType }

// Run starts the HTTP and gRPC servers and the outbox relay, and blocks
// until ctx is cancelled.
func (s *Service) Run(ctx context.Context) error {
	if err := s.bus.EnsureStream(ctx, "ACCOUNTS_EVENTS", []string{"accounts.>"}); err != nil {
		return fmt.Errorf("accounts: ensuring stream: %w", err)
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

	ln, err := net.Listen("tcp", s.cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("accounts: listening on %s: %w", s.cfg.HTTPAddr, err)
	}
	grpcLn, err := net.Listen("tcp", s.cfg.GRPCAddr)
	if err != nil {
		_ = ln.Close()
		return fmt.Errorf("accounts: listening for gRPC on %s: %w", s.cfg.GRPCAddr, err)
	}
	grpcSrv := gogrpc.NewServer()
	accountsgrpc.Register(grpcSrv, s.app)
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
