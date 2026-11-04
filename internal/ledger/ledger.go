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
	"time"

	gogrpc "google.golang.org/grpc"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/bus"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/clock"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/config"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/httpx"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/idempotency"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/outbox"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/server"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/adapters"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/api"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger/app"
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
}

// Service wires the ledger's dependencies together and runs them.
type Service struct {
	cfg  Config
	pool *db.Pool
	bus  *bus.Bus
	app  *app.Service
	rtr  http.Handler
}

// New builds a Service from already-connected dependencies.
func New(cfg Config, pool *db.Pool, b *bus.Bus, identity adapters.Identity) *Service {
	a := app.New(app.Deps{Pool: pool, Clock: clock.Real{}, Logger: slog.Default(), Identity: identity})
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
	return New(cfg, pool, b, identity), nil
}

func relaySubject(eventType string) string { return "ledger." + eventType }

// Run starts the gRPC and HTTP servers and the outbox relay, and
// blocks until ctx is cancelled.
func (s *Service) Run(ctx context.Context) error {
	if err := s.bus.EnsureStream(ctx, "LEDGER_EVENTS", []string{"ledger.>"}); err != nil {
		return fmt.Errorf("ledger: ensuring stream: %w", err)
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
	return err
}
