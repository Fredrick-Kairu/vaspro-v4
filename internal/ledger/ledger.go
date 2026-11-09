// Package ledger is the ledger service: balanced, immutable journal
// postings, wallet balances and holds (data model §5, system design
// §4). Wiring only; rules live in domain/, use cases in app/, the
// internal gRPC API in grpc/.
package ledger

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

	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/adapters"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/api"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/app"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/domain"
	ledgergrpc "github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/grpc"
)

// Config is the ledger's process configuration (platform/config.Load).
//
// GRPCAddr serves the posting API (proto/ledger/v1, task 3.3), which
// trusts the tenant each request names: it must be reachable only from
// VASPRO's own services, a deployment rule (compose network, Phase
// 17's network policy and mTLS), not something the ledger can check.
// HTTPAddr serves the HTTP API (docs/api/ledger.yaml: staff token
// prices, task 3.4b) and the health checks. Like accounts', it trusts
// the X-Vaspro-User-Id header the gateway sets after checking the
// session, so it too must be reachable only from the gateway.
//
// DatabaseURL's role must be granted ledger_service and must not own
// the ledger tables (migration 20261106090000); its search_path must
// resolve unqualified names to the ledger schema
// (search_path=ledger,public), which platform/outbox relies on.
type Config struct {
	DatabaseURL string `env:"LEDGER_DATABASE_URL,required"`
	NATSURL     string `env:"LEDGER_NATS_URL,required"`
	HTTPAddr    string `env:"LEDGER_HTTP_ADDR" envDefault:":8083"`
	GRPCAddr    string `env:"LEDGER_GRPC_ADDR" envDefault:":9092"`
	// IdentityGRPCAddr is identity's internal gRPC API, asked whether a
	// caller is staff and stepped up (task 3.4b). Dialled lazily, so the
	// ledger starts while identity is down; staff actions then fail
	// closed.
	IdentityGRPCAddr string `env:"LEDGER_IDENTITY_GRPC_ADDR,required"`
	// SettlementWindow is how often usage settles (task 3.5, OD-07's
	// default); every replica settles the same grid of windows.
	SettlementWindow time.Duration `env:"LEDGER_SETTLEMENT_WINDOW" envDefault:"5m"`
	// PacksDir holds schema.json and one <cc>/pack.yaml per country: the
	// VAT rates settlement uses.
	PacksDir string `env:"LEDGER_PACKS_DIR" envDefault:"packs"`
	// RebuildAt is when the nightly rebuild runs, HH:MM UTC (task 3.8,
	// OQ-19); empty is the default, 21:30 (00:30 in Nairobi). One
	// replica runs each date.
	RebuildAt string `env:"LEDGER_REBUILD_AT" envDefault:"21:30"`
}

const defaultRebuildAt = "21:30"

// Service wires the ledger's dependencies together and runs them.
type Service struct {
	cfg  Config
	pool *db.Pool
	bus  *bus.Bus
	app  *app.Service
	rtr  http.Handler
}

// New builds a Service from already-connected dependencies and the
// loaded country packs, by country code.
func New(cfg Config, pool *db.Pool, b *bus.Bus, identity adapters.Identity, loaded map[string]*packs.Pack) *Service {
	a := app.New(app.Deps{
		Pool: pool, Clock: clock.Real{}, Logger: slog.Default(), Identity: identity,
		Packs: loaded, SettlementWindow: cfg.SettlementWindow,
	})
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
		return nil, fmt.Errorf("ledger: connecting to database: %w", err)
	}
	b, err := bus.Connect(ctx, cfg.NATSURL)
	if err != nil {
		return nil, fmt.Errorf("ledger: connecting to nats: %w", err)
	}
	identity, err := adapters.DialIdentity(cfg.IdentityGRPCAddr)
	if err != nil {
		return nil, fmt.Errorf("ledger: %w", err)
	}
	loaded, err := loadPacks(cfg.PacksDir)
	if err != nil {
		return nil, err
	}
	return New(cfg, pool, b, identity, loaded), nil
}

// loadPacks loads every <dir>/<cc>/pack.yaml against <dir>/schema.json.
func loadPacks(dir string) (map[string]*packs.Pack, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*", "pack.yaml"))
	if err != nil {
		return nil, fmt.Errorf("ledger: finding packs: %w", err)
	}
	out := make(map[string]*packs.Pack, len(paths))
	for _, path := range paths {
		p, err := packs.Load(filepath.Join(dir, "schema.json"), path)
		if err != nil {
			return nil, fmt.Errorf("ledger: %w", err)
		}
		out[p.CountryCode] = p
	}
	return out, nil
}

func relaySubject(eventType string) string { return "ledger." + eventType }

// Run starts the gRPC and HTTP servers and the outbox relay, and
// blocks until ctx is cancelled.
func (s *Service) Run(ctx context.Context) error {
	rebuildAt := s.cfg.RebuildAt
	if rebuildAt == "" {
		rebuildAt = defaultRebuildAt
	}
	at, err := domain.ParseTimeOfDay(rebuildAt)
	if err != nil {
		return fmt.Errorf("ledger: LEDGER_REBUILD_AT: %w", err)
	}
	// The packs are the source of VAT rates (task 3.5); a rate changed
	// without a new date, or two packs sharing a currency, stops startup.
	if err := s.app.SyncTaxRates(ctx); err != nil {
		return fmt.Errorf("ledger: syncing tax rates from the packs: %w", err)
	}
	if err := s.bus.EnsureStream(ctx, "LEDGER_EVENTS", []string{"ledger.>"}); err != nil {
		return fmt.Errorf("ledger: ensuring stream: %w", err)
	}
	settleDone := make(chan struct{})
	go func() {
		defer close(settleDone)
		s.settleEachWindow(ctx)
	}()

	rebuildDone := make(chan struct{})
	go func() {
		defer close(rebuildDone)
		s.rebuildNightly(ctx, at)
	}()

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
		return fmt.Errorf("ledger: listening on %s: %w", s.cfg.HTTPAddr, err)
	}
	grpcLn, err := net.Listen("tcp", s.cfg.GRPCAddr)
	if err != nil {
		_ = ln.Close()
		return fmt.Errorf("ledger: listening for gRPC on %s: %w", s.cfg.GRPCAddr, err)
	}
	grpcSrv := gogrpc.NewServer()
	ledgergrpc.Register(grpcSrv, s.app, slog.Default())
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
	<-settleDone
	<-rebuildDone
	return err
}

// rebuildNightly runs the nightly rebuild (task 3.8) at the configured
// time each day. Every replica wakes; the one that claims the date runs
// it. A failed run is logged, and the date's row is taken over after an
// hour by any replica that wakes again; the next night runs regardless.
func (s *Service) rebuildNightly(ctx context.Context, at domain.TimeOfDay) {
	clk := clock.Real{}
	for {
		next, runDate := domain.NextRebuild(clk.Now(), at)
		select {
		case <-ctx.Done():
			return
		case <-time.After(next.Sub(clk.Now())):
		}
		sum, err := s.app.RebuildBalances(ctx, runDate)
		switch {
		case err != nil:
			slog.ErrorContext(ctx, "ledger: P1 the nightly rebuild couldn't run", "run_date", runDate.Format(time.DateOnly), "error", err)
		case sum.Ran:
			slog.InfoContext(ctx, "ledger: nightly rebuild", "run_date", runDate.Format(time.DateOnly), "passed", sum.Passed,
				"differences", len(sum.Differences), "accounts", sum.AccountsChecked, "transactions", sum.TransactionsChecked)
		}
		// Past this run's time, so the next wake is tomorrow's.
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

// settleEachWindow wakes just after each window boundary, settles the
// window that ended (task 3.5) and expires holds (task 3.6). A run that
// fails is logged; the next window's run picks its work up.
func (s *Service) settleEachWindow(ctx context.Context) {
	window := s.cfg.SettlementWindow
	if window <= 0 {
		window = app.DefaultSettlementWindow
	}
	clk := clock.Real{}
	for {
		next := clk.Now().UTC().Truncate(window).Add(window)
		select {
		case <-ctx.Done():
			return
		case <-time.After(next.Sub(clk.Now()) + 100*time.Millisecond):
		}
		sum, err := s.app.SettleUsage(ctx, clk.Now())
		if err != nil {
			slog.ErrorContext(ctx, "ledger: settlement run failed", "error", err)
		} else if len(sum.Batches) > 0 || sum.Failed > 0 {
			slog.InfoContext(ctx, "ledger: settled usage", "window_end", sum.WindowEnd,
				"batches", len(sum.Batches), "skipped", sum.Skipped, "failed", sum.Failed)
		}
		// Then expire holds past their expiry plus a window (task 3.6).
		expired, err := s.app.ExpireHolds(ctx, clk.Now())
		if err != nil {
			slog.ErrorContext(ctx, "ledger: expiry run failed", "error", err)
		} else if expired.Expired > 0 || expired.Failed > 0 {
			slog.InfoContext(ctx, "ledger: expired holds", "expired", expired.Expired, "failed", expired.Failed)
		}
	}
}
