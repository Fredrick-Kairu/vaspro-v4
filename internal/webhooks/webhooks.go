// Package webhooks is the webhooks service: clients' endpoints, signed
// deliveries of the events sent to them, and the copy of every event in
// ClickHouse for reports (data model §8, OQ-23). Wiring only; rules live
// in domain/, use cases in app/.
package webhooks

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/bus"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/clock"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/config"
	vascrypto "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/crypto"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/httpx"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/idempotency"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/outbox"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/packs"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/server"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/webhooks/adapters"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/webhooks/api"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/webhooks/app"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/webhooks/domain"
)

// Config is webhooks' process configuration (platform/config.Load).
//
// HTTPAddr serves the HTTP API (docs/api/webhooks.yaml) and the health
// checks. It trusts the X-Vaspro-User-Id header the gateway sets after
// checking the session, so it must be reachable only from the gateway.
//
// DatabaseURL's role must be granted webhooks_service and must not own
// the webhooks tables (migration 20261112090000); its search_path must
// resolve unqualified names to the webhooks schema
// (search_path=webhooks,public), which platform/outbox relies on.
type Config struct {
	DatabaseURL string `env:"WEBHOOKS_DATABASE_URL,required"`
	NATSURL     string `env:"WEBHOOKS_NATS_URL,required"`
	HTTPAddr    string `env:"WEBHOOKS_HTTP_ADDR" envDefault:":8085"`
	// AccountsGRPCAddr is asked on every call whether the user may work
	// with the account's endpoints (credentials:read or :manage).
	// Dialled lazily; calls fail closed while accounts is down.
	AccountsGRPCAddr string `env:"WEBHOOKS_ACCOUNTS_GRPC_ADDR,required"`
	// IdentityGRPCAddr is asked whether a step-up is fresh before an
	// endpoint is created, its URL changed or its secret rotated.
	IdentityGRPCAddr string `env:"WEBHOOKS_IDENTITY_GRPC_ADDR,required"`
	// SecretsKMSKey seals endpoint secrets (base64 of 32 bytes; LocalKMS
	// until a cloud KMS is chosen).
	SecretsKMSKey string `env:"WEBHOOKS_SECRETS_KMS_KEY,required"`
	// SecretFingerprintKey keys endpoint secrets' fingerprints (base64
	// of 32 bytes).
	SecretFingerprintKey string `env:"WEBHOOKS_SECRET_FINGERPRINT_KEY,required"`
	// AllowInsecureURLs allows plain http and private addresses as
	// endpoints: development only, never staging or production.
	AllowInsecureURLs bool `env:"WEBHOOKS_ALLOW_INSECURE_URLS" envDefault:"false"`
	// MaxEndpoints caps a client's undeleted endpoints (data model §8).
	MaxEndpoints int `env:"WEBHOOKS_MAX_ENDPOINTS" envDefault:"16"`

	// Delivery (task 5.4, OQ-23). RetrySchedule is the wait before each
	// retry, comma-separated: one attempt at once, then one after each.
	// RetryJitter varies each wait by up to that fraction of itself.
	RetrySchedule string  `env:"WEBHOOKS_RETRY_SCHEDULE" envDefault:"30s,2m,10m,30m,1h,3h,6h,12h,24h,24h"`
	RetryJitter   float64 `env:"WEBHOOKS_RETRY_JITTER" envDefault:"0.1"`
	// DisableAfter is how long an endpoint fails, with no success,
	// before it is disabled.
	DisableAfter time.Duration `env:"WEBHOOKS_DISABLE_AFTER" envDefault:"72h"`
	// Workers bounds requests in flight per replica, PerEndpoint those to
	// any one endpoint, BatchSize the deliveries claimed at once.
	Workers     int `env:"WEBHOOKS_WORKERS" envDefault:"16"`
	PerEndpoint int `env:"WEBHOOKS_PER_ENDPOINT" envDefault:"4"`
	BatchSize   int `env:"WEBHOOKS_BATCH_SIZE" envDefault:"100"`
	// Lease is how long a claimed batch is held before another replica
	// may take what's left of it; it must cover a batch's sending.
	Lease time.Duration `env:"WEBHOOKS_LEASE" envDefault:"5m"`
	// PollInterval is how often each replica looks for due deliveries.
	PollInterval time.Duration `env:"WEBHOOKS_POLL_INTERVAL" envDefault:"1s"`
	// RequestTimeout limits each request to an endpoint.
	RequestTimeout time.Duration `env:"WEBHOOKS_REQUEST_TIMEOUT" envDefault:"10s"`
	// StreamRetry is how often a consumer whose stream doesn't exist yet
	// (its service hasn't started) tries again.
	StreamRetry time.Duration `env:"WEBHOOKS_STREAM_RETRY" envDefault:"5s"`

	// Reports (task 5.6). ClickHouseURL is the events table's ClickHouse
	// (clickhouse://user:pass@host:9000/db), migrated from
	// migrations/clickhouse. PacksDir holds schema.json and one
	// <cc>/pack.yaml per country, whose time zone a client's report days
	// are in. The sink inserts up to SinkBatch events at a time, or what
	// arrived within SinkWait.
	ClickHouseURL string        `env:"WEBHOOKS_CLICKHOUSE_URL,required"`
	PacksDir      string        `env:"WEBHOOKS_PACKS_DIR" envDefault:"packs"`
	SinkBatch     int           `env:"WEBHOOKS_SINK_BATCH" envDefault:"1000"`
	SinkWait      time.Duration `env:"WEBHOOKS_SINK_WAIT" envDefault:"1s"`
}

// sinkStreams are every stream on the bus, all copied into ClickHouse.
// Webhooks' own is filtered to its events, leaving out dead letters
// (copies of other streams' events).
var sinkStreams = []struct{ stream, filter string }{
	{"IDENTITY_EVENTS", ""},
	{"ACCOUNTS_EVENTS", ""},
	{"LEDGER_EVENTS", ""},
	{"PAYMENTS_EVENTS", ""},
	{"WEBHOOKS_EVENTS", "webhooks.webhook_endpoint.>"},
}

// fanoutStreams are the streams carrying events sent to clients: the
// services publishing them, and webhooks' own webhook_endpoint.disabled.
// Identity publishes none.
var fanoutStreams = []struct{ stream, filter string }{
	{"ACCOUNTS_EVENTS", ""},
	{"LEDGER_EVENTS", ""},
	{"PAYMENTS_EVENTS", ""},
	{"WEBHOOKS_EVENTS", "webhooks.webhook_endpoint.>"},
}

// fanoutDeadLetter receives an event the fan-out failed on every time.
const fanoutDeadLetter = "webhooks.dead_letter.fanout"

// Clients are the outside services webhooks calls.
type Clients struct {
	Accounts adapters.Accounts
	Identity adapters.Identity
	Events   app.EventStore
}

// Service wires webhooks' dependencies together and runs them.
type Service struct {
	cfg  Config
	pool *db.Pool
	bus  *bus.Bus
	app  *app.Service
	rtr  http.Handler
}

// New builds a Service from already-connected dependencies.
func New(cfg Config, pool *db.Pool, b *bus.Bus, c Clients) (*Service, error) {
	kmsKey, err := base64.StdEncoding.DecodeString(cfg.SecretsKMSKey)
	if err != nil {
		return nil, fmt.Errorf("webhooks: WEBHOOKS_SECRETS_KMS_KEY: %w", err)
	}
	kms, err := vascrypto.NewLocalKMS(map[string][]byte{app.SecretsKMSKeyID: kmsKey})
	if err != nil {
		return nil, fmt.Errorf("webhooks: WEBHOOKS_SECRETS_KMS_KEY: %w", err)
	}
	fpKey, err := base64.StdEncoding.DecodeString(cfg.SecretFingerprintKey)
	if err != nil || len(fpKey) != 32 {
		return nil, errors.New("webhooks: WEBHOOKS_SECRET_FINGERPRINT_KEY must be base64 of 32 bytes")
	}
	if cfg.MaxEndpoints < 1 {
		return nil, errors.New("webhooks: WEBHOOKS_MAX_ENDPOINTS must be at least 1")
	}
	schedule, err := domain.ParseSchedule(cfg.RetrySchedule)
	if err != nil {
		return nil, fmt.Errorf("webhooks: WEBHOOKS_RETRY_SCHEDULE: %w", err)
	}
	if err := checkDelivery(cfg); err != nil {
		return nil, err
	}
	if cfg.SinkBatch < 1 || cfg.SinkWait <= 0 {
		return nil, errors.New("webhooks: WEBHOOKS_SINK_BATCH must be at least 1 and WEBHOOKS_SINK_WAIT positive")
	}
	loaded, err := LoadPacks(cfg.PacksDir)
	if err != nil {
		return nil, err
	}
	if len(loaded) == 0 {
		return nil, fmt.Errorf("webhooks: no country packs in WEBHOOKS_PACKS_DIR %q; reports need their time zones", cfg.PacksDir)
	}
	if cfg.AllowInsecureURLs {
		slog.Warn("webhooks: WEBHOOKS_ALLOW_INSECURE_URLS is on: endpoints may be plain http and private addresses; development only")
	}
	a := app.New(app.Deps{
		Pool: pool, Clock: clock.Real{}, Logger: slog.Default(), Accounts: c.Accounts, Identity: c.Identity,
		KMS: kms, FingerprintKey: fpKey, AllowInsecureURLs: cfg.AllowInsecureURLs, MaxEndpoints: cfg.MaxEndpoints,
		Sender:   adapters.NewSender(adapters.SenderConfig{AllowInsecure: cfg.AllowInsecureURLs, Timeout: cfg.RequestTimeout}),
		Schedule: schedule, Jitter: cfg.RetryJitter, DisableAfter: cfg.DisableAfter,
		Workers: cfg.Workers, PerEndpoint: cfg.PerEndpoint, BatchSize: cfg.BatchSize, Lease: cfg.Lease,
		Events: c.Events, Packs: loaded,
	})
	rtr := api.Routes(api.NewHandlers(a), slog.Default(), []httpx.Check{
		func(ctx context.Context) error { return pool.Ping(ctx) },
	}, idempotency.NewPostgresStore(pool))
	return &Service{cfg: cfg, pool: pool, bus: b, app: a, rtr: rtr}, nil
}

// checkDelivery refuses delivery settings that can't work.
func checkDelivery(cfg Config) error {
	switch {
	case cfg.RetryJitter < 0 || cfg.RetryJitter >= 1:
		return errors.New("webhooks: WEBHOOKS_RETRY_JITTER must be at least 0 and below 1")
	case cfg.DisableAfter <= 0:
		return errors.New("webhooks: WEBHOOKS_DISABLE_AFTER must be positive")
	case cfg.Workers < 1 || cfg.PerEndpoint < 1 || cfg.BatchSize < 1:
		return errors.New("webhooks: WEBHOOKS_WORKERS, _PER_ENDPOINT and _BATCH_SIZE must be at least 1")
	case cfg.PerEndpoint > cfg.Workers:
		return errors.New("webhooks: WEBHOOKS_PER_ENDPOINT can't exceed WEBHOOKS_WORKERS")
	case cfg.RequestTimeout <= 0 || cfg.PollInterval <= 0 || cfg.StreamRetry <= 0:
		return errors.New("webhooks: WEBHOOKS_REQUEST_TIMEOUT, _POLL_INTERVAL and _STREAM_RETRY must be positive")
	case cfg.Lease <= cfg.RequestTimeout*time.Duration((cfg.BatchSize+cfg.PerEndpoint-1)/cfg.PerEndpoint):
		// A batch all for one endpoint runs PerEndpoint at a time; the
		// lease must outlast it, or another replica sends what's left.
		return errors.New("webhooks: WEBHOOKS_LEASE must outlast a batch for one endpoint (BATCH_SIZE / PER_ENDPOINT × REQUEST_TIMEOUT)")
	}
	return nil
}

// Bootstrap loads Config from the environment and connects every
// dependency New needs.
func Bootstrap(ctx context.Context) (*Service, error) {
	cfg := config.MustLoad[Config]()
	pool, err := db.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("webhooks: connecting to database: %w", err)
	}
	b, err := bus.Connect(ctx, cfg.NATSURL)
	if err != nil {
		return nil, fmt.Errorf("webhooks: connecting to nats: %w", err)
	}
	accounts, err := adapters.DialAccounts(cfg.AccountsGRPCAddr)
	if err != nil {
		return nil, fmt.Errorf("webhooks: %w", err)
	}
	identity, err := adapters.DialIdentity(cfg.IdentityGRPCAddr)
	if err != nil {
		return nil, fmt.Errorf("webhooks: %w", err)
	}
	ch, err := adapters.OpenClickHouse(cfg.ClickHouseURL)
	if err != nil {
		return nil, fmt.Errorf("webhooks: %w", err)
	}
	return New(cfg, pool, b, Clients{Accounts: accounts, Identity: identity, Events: ch})
}

// LoadPacks loads every <dir>/<cc>/pack.yaml against <dir>/schema.json.
func LoadPacks(dir string) (map[string]*packs.Pack, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*", "pack.yaml"))
	if err != nil {
		return nil, fmt.Errorf("webhooks: finding packs: %w", err)
	}
	out := make(map[string]*packs.Pack, len(paths))
	for _, path := range paths {
		p, err := packs.Load(filepath.Join(dir, "schema.json"), path)
		if err != nil {
			return nil, fmt.Errorf("webhooks: %w", err)
		}
		out[p.CountryCode] = p
	}
	return out, nil
}

// App is the service's use cases, for tests and the API (task 5.5).
func (s *Service) App() *app.Service { return s.app }

func relaySubject(eventType string) string { return "webhooks." + eventType }

// consume runs the fan-out on one stream until ctx ends. A stream that
// doesn't exist yet belongs to a service that hasn't started; webhooks
// waits for it rather than creating it.
func (s *Service) consume(ctx context.Context, stream, filter string) {
	for {
		cc, err := s.bus.Consume(ctx, bus.ConsumeConfig{
			Stream: stream, Durable: "webhooks-fanout", FilterSubject: filter, AckWait: 30 * time.Second,
			MaxDeliver: 10, DeadLetterSubject: fanoutDeadLetter,
		}, s.app.HandleEvent)
		if err == nil {
			<-ctx.Done()
			cc.Stop()
			return
		}
		slog.WarnContext(ctx, "webhooks: can't consume a stream yet; retrying", "stream", stream, "error", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.cfg.StreamRetry):
		}
	}
}

// sink copies one stream's events into ClickHouse until ctx ends, a
// batch at a time, acknowledging a batch only once it's inserted. Like
// consume, it waits for a stream that doesn't exist yet.
func (s *Service) sink(ctx context.Context, stream, filter string) {
	for ctx.Err() == nil {
		err := s.bus.ConsumeBatch(ctx, bus.ConsumeConfig{
			Stream: stream, Durable: "webhooks-clickhouse", FilterSubject: filter, AckWait: time.Minute,
			MaxDeliver: 10, DeadLetterSubject: sinkDeadLetter,
		}, bus.BatchConfig{Max: s.cfg.SinkBatch, Wait: s.cfg.SinkWait}, s.app.SinkEvents)
		if err == nil {
			return
		}
		slog.WarnContext(ctx, "webhooks: can't sink a stream yet; retrying", "stream", stream, "error", err)
		select {
		case <-ctx.Done():
		case <-time.After(s.cfg.StreamRetry):
		}
	}
}

// sinkDeadLetter receives an event the sink failed to insert every time.
const sinkDeadLetter = "webhooks.dead_letter.clickhouse"

// deliverEvery sends due deliveries on every poll, back to back while
// there is work.
func (s *Service) deliverEvery(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		for ctx.Err() == nil {
			sum, err := s.app.DeliverDue(ctx)
			if err != nil {
				slog.ErrorContext(ctx, "webhooks: delivering failed", "error", err)
				break
			}
			if sum.Claimed == 0 {
				break
			}
			slog.DebugContext(ctx, "webhooks: delivered", "claimed", sum.Claimed, "succeeded", sum.Succeeded,
				"failed", sum.Failed, "dead", sum.Dead, "held", sum.Held)
		}
	}
}

// Run starts the HTTP server, the outbox relay, the fan-out consumers
// and the delivery worker, and blocks until ctx is cancelled.
func (s *Service) Run(ctx context.Context) error {
	// The background loops stop, and are waited for, however Run ends.
	var background sync.WaitGroup
	defer background.Wait()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if err := s.bus.EnsureStream(ctx, "WEBHOOKS_EVENTS", []string{"webhooks.>"}); err != nil {
		return fmt.Errorf("webhooks: ensuring stream: %w", err)
	}
	for _, fs := range fanoutStreams {
		background.Add(1)
		go func() {
			defer background.Done()
			s.consume(ctx, fs.stream, fs.filter)
		}()
	}
	for _, ss := range sinkStreams {
		background.Add(1)
		go func() {
			defer background.Done()
			s.sink(ctx, ss.stream, ss.filter)
		}()
	}
	background.Add(1)
	go func() {
		defer background.Done()
		s.deliverEvery(ctx)
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
		<-relayDone
		return fmt.Errorf("webhooks: listening on %s: %w", s.cfg.HTTPAddr, err)
	}
	srv := &http.Server{Handler: s.rtr, ReadHeaderTimeout: 5 * time.Second}
	err = server.Serve(ctx, srv, ln, 10*time.Second)
	<-relayDone
	return err
}
