// Package payments is the payments service: VASPRO's own paybill and
// wallet top-ups in Phase 4, the rails clients collect and pay out on
// later (data model §7, system design §4.1 and §13). Wiring only; rules
// live in domain/, use cases in app/, the HTTP API in api/.
package payments

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"time"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/bus"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/clock"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/config"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/httpx"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/idempotency"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/outbox"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/packs"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/server"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments/adapters"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments/api"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments/app"
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
	// PacksDir holds schema.json and one <cc>/pack.yaml per country: the
	// rails, and which one takes top-ups.
	PacksDir string `env:"PAYMENTS_PACKS_DIR" envDefault:"packs"`
}

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
func New(cfg Config, pool *db.Pool, b *bus.Bus, accounts adapters.Accounts, loaded map[string]*packs.Pack) *Service {
	a := app.New(app.Deps{Pool: pool, Clock: clock.Real{}, Logger: slog.Default(), Accounts: accounts, Packs: loaded})
	rtr := api.Routes(api.NewHandlers(a), slog.Default(), []httpx.Check{
		func(ctx context.Context) error { return pool.Ping(ctx) },
	}, idempotency.NewPostgresStore(pool))
	return &Service{cfg: cfg, pool: pool, bus: b, app: a, rtr: rtr}
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
	loaded, err := LoadPacks(cfg.PacksDir)
	if err != nil {
		return nil, err
	}
	return New(cfg, pool, b, accounts, loaded), nil
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

	ln, err := net.Listen("tcp", s.cfg.HTTPAddr)
	if err != nil {
		<-relayDone
		return fmt.Errorf("payments: listening on %s: %w", s.cfg.HTTPAddr, err)
	}
	srv := &http.Server{Handler: s.rtr, ReadHeaderTimeout: 5 * time.Second}
	err = server.Serve(ctx, srv, ln, 10*time.Second)
	<-relayDone
	return err
}
