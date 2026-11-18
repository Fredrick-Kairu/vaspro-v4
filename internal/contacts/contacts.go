// Package contacts is the contacts service: clients' contacts, groups
// and imports (data model §8 and §10, OQ-26), and, from 6.3 to 6.5,
// segments and consent. Wiring only; rules live in domain/, use cases
// in app/.
package contacts

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

	gogrpc "google.golang.org/grpc"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/bus"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/clamav"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/clock"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/config"
	vascrypto "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/crypto"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/httpx"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/idempotency"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/objectstore"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/outbox"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/packs"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/server"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/uploads"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/contacts/adapters"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/contacts/api"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/contacts/app"
	contactsgrpc "github.com/Fredrick-Kairu/vaspro-v4/internal/contacts/grpc"
)

// FilesKMSKeyID is the KMS key imported files are sealed under.
const FilesKMSKeyID = "contacts-files"

// Config is contacts' process configuration (platform/config.Load).
//
// HTTPAddr serves the HTTP API (docs/api/contacts.yaml) and the health
// checks. It trusts the X-Vaspro-User-Id header the gateway sets after
// checking the session, so it must be reachable only from the gateway.
//
// DatabaseURL's role must be granted contacts_service and must not own
// the contacts tables (migration 20261117090000); its search_path must
// resolve unqualified names to the contacts schema
// (search_path=contacts,public), which platform/outbox relies on.
type Config struct {
	DatabaseURL string `env:"CONTACTS_DATABASE_URL,required"`
	NATSURL     string `env:"CONTACTS_NATS_URL,required"`
	HTTPAddr    string `env:"CONTACTS_HTTP_ADDR" envDefault:":8086"`
	// GRPCAddr serves the internal API (proto/contacts/v1, task 6.5):
	// consent checks and audiences for messaging. Plaintext until Phase
	// 17's mTLS; it trusts the tenant a request names, so it must be
	// reachable only from VASPRO services.
	GRPCAddr string `env:"CONTACTS_GRPC_ADDR" envDefault:":9093"`
	// AccountsGRPCAddr is asked on every call whether the user may work
	// with the account's contacts (contacts:read or :manage) and for the
	// account's country. Dialled lazily; calls fail closed while
	// accounts is down.
	AccountsGRPCAddr string `env:"CONTACTS_ACCOUNTS_GRPC_ADDR,required"`
	// PacksDir holds schema.json and one <cc>/pack.yaml per country,
	// whose numbering reads an account's numbers.
	PacksDir string `env:"CONTACTS_PACKS_DIR" envDefault:"packs"`

	// Uploads (D-23, data model §8). Object storage holds imported files
	// (sealed) and quarantine. PublicEndpoint is the host browsers post
	// signed forms to, which needs CORS for the dashboard's origin.
	ObjectStorageEndpoint       string `env:"CONTACTS_OBJECT_STORAGE_ENDPOINT,required"`
	ObjectStoragePublicEndpoint string `env:"CONTACTS_OBJECT_STORAGE_PUBLIC_ENDPOINT,required"`
	ObjectStorageAccessKey      string `env:"CONTACTS_OBJECT_STORAGE_ACCESS_KEY,required"`
	ObjectStorageSecretKey      string `env:"CONTACTS_OBJECT_STORAGE_SECRET_KEY,required"`
	ObjectStorageBucket         string `env:"CONTACTS_OBJECT_STORAGE_BUCKET" envDefault:"contacts"`
	ObjectStorageUseTLS         bool   `env:"CONTACTS_OBJECT_STORAGE_USE_TLS"`
	ObjectStoragePublicUseTLS   bool   `env:"CONTACTS_OBJECT_STORAGE_PUBLIC_USE_TLS"`
	ObjectStorageRegion         string `env:"CONTACTS_OBJECT_STORAGE_REGION" envDefault:"us-east-1"`
	// ClamAVAddr is clamd (host:port); its StreamMaxLength must be at
	// least MaxUploadBytes (clamd's default 25 MB covers 20 MB).
	ClamAVAddr string `env:"CONTACTS_CLAMAV_ADDR,required"`
	// FilesKMSKey seals stored files (base64 of 32 bytes; LocalKMS until
	// a cloud KMS is chosen). DownloadKey signs download links (base64
	// of 32 bytes).
	FilesKMSKey string `env:"CONTACTS_FILES_KMS_KEY,required"`
	DownloadKey string `env:"CONTACTS_DOWNLOAD_KEY,required"`
	// DownloadBaseURL is the gateway's public URL, which download links
	// start with.
	DownloadBaseURL string `env:"CONTACTS_DOWNLOAD_BASE_URL,required"`
	MaxUploadBytes  int64  `env:"CONTACTS_MAX_UPLOAD_BYTES" envDefault:"20971520"`
	// FileRetention is how long an import's files are kept after it
	// finishes (data model §8's transient class).
	FileRetention time.Duration `env:"CONTACTS_FILE_RETENTION" envDefault:"48h"`
	// WorkerPoll is how often each replica looks for imports to work on;
	// SweepInterval how often it expires unused forms and deletes files.
	WorkerPoll    time.Duration `env:"CONTACTS_WORKER_POLL" envDefault:"2s"`
	SweepInterval time.Duration `env:"CONTACTS_SWEEP_INTERVAL" envDefault:"1m"`
}

// Clients are the services contacts calls.
type Clients struct {
	Accounts adapters.Accounts
}

// Service is the contacts service.
type Service struct {
	cfg  Config
	pool *db.Pool
	bus  *bus.Bus
	app  *app.Service
	rtr  http.Handler
	// ensureBucket creates the files' bucket if it's missing.
	ensureBucket func(context.Context) error
}

// New checks cfg and wires the service.
func New(cfg Config, pool *db.Pool, b *bus.Bus, c Clients) (*Service, error) {
	key := func(name, v string) ([]byte, error) {
		k, err := base64.StdEncoding.DecodeString(v)
		if err != nil || len(k) != 32 {
			return nil, fmt.Errorf("contacts: %s must be base64 of 32 bytes", name)
		}
		return k, nil
	}
	filesKey, err := key("CONTACTS_FILES_KMS_KEY", cfg.FilesKMSKey)
	if err != nil {
		return nil, err
	}
	downloadKey, err := key("CONTACTS_DOWNLOAD_KEY", cfg.DownloadKey)
	if err != nil {
		return nil, err
	}
	if cfg.WorkerPoll <= 0 || cfg.SweepInterval <= 0 {
		return nil, errors.New("contacts: CONTACTS_WORKER_POLL and CONTACTS_SWEEP_INTERVAL must be positive")
	}
	loaded, err := LoadPacks(cfg.PacksDir)
	if err != nil {
		return nil, err
	}
	if len(loaded) == 0 {
		return nil, fmt.Errorf("contacts: no country packs in CONTACTS_PACKS_DIR %q; numbers are read with them", cfg.PacksDir)
	}
	store, err := objectstore.New(objectstore.Config{
		Endpoint: cfg.ObjectStorageEndpoint, AccessKey: cfg.ObjectStorageAccessKey, SecretKey: cfg.ObjectStorageSecretKey,
		Bucket: cfg.ObjectStorageBucket, UseTLS: cfg.ObjectStorageUseTLS, PublicEndpoint: cfg.ObjectStoragePublicEndpoint,
		PublicUseTLS: cfg.ObjectStoragePublicUseTLS, Region: cfg.ObjectStorageRegion,
	})
	if err != nil {
		return nil, fmt.Errorf("contacts: %w", err)
	}
	kms, err := vascrypto.NewLocalKMS(map[string][]byte{FilesKMSKeyID: filesKey})
	if err != nil {
		return nil, fmt.Errorf("contacts: %w", err)
	}
	upCfg := uploads.DefaultConfig()
	upCfg.KMSKeyID, upCfg.DownloadKey = FilesKMSKeyID, downloadKey
	upCfg.MaxBytes, upCfg.TransientRetention = cfg.MaxUploadBytes, cfg.FileRetention
	up, err := uploads.New(store, clamav.New(cfg.ClamAVAddr), vascrypto.NewEnvelope(kms), clock.Real{}, upCfg)
	if err != nil {
		return nil, fmt.Errorf("contacts: %w", err)
	}
	a := app.New(app.Deps{
		Pool: pool, Clock: clock.Real{}, Logger: slog.Default(), Accounts: c.Accounts, Packs: loaded,
		Uploads: up, DownloadBaseURL: cfg.DownloadBaseURL,
	})
	rtr := api.Routes(api.NewHandlers(a), slog.Default(), []httpx.Check{
		func(ctx context.Context) error { return pool.Ping(ctx) },
	}, idempotency.NewPostgresStore(pool))
	return &Service{cfg: cfg, pool: pool, bus: b, app: a, rtr: rtr, ensureBucket: store.EnsureBucket}, nil
}

// Bootstrap loads Config and connects contacts' dependencies.
func Bootstrap(ctx context.Context) (*Service, error) {
	cfg := config.MustLoad[Config]()
	pool, err := db.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("contacts: connecting to database: %w", err)
	}
	b, err := bus.Connect(ctx, cfg.NATSURL)
	if err != nil {
		return nil, fmt.Errorf("contacts: connecting to nats: %w", err)
	}
	accounts, err := adapters.DialAccounts(cfg.AccountsGRPCAddr)
	if err != nil {
		return nil, fmt.Errorf("contacts: %w", err)
	}
	return New(cfg, pool, b, Clients{Accounts: accounts})
}

// LoadPacks loads every <cc>/pack.yaml in dir, by country code.
func LoadPacks(dir string) (map[string]*packs.Pack, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*", "pack.yaml"))
	if err != nil {
		return nil, fmt.Errorf("contacts: finding packs: %w", err)
	}
	out := make(map[string]*packs.Pack, len(paths))
	for _, path := range paths {
		p, err := packs.Load(filepath.Join(dir, "schema.json"), path)
		if err != nil {
			return nil, fmt.Errorf("contacts: %w", err)
		}
		out[p.CountryCode] = p
	}
	return out, nil
}

// App is the service's use cases, for tests.
func (s *Service) App() *app.Service { return s.app }

func relaySubject(eventType string) string { return "contacts." + eventType }

// every runs fn now and then every interval until ctx ends.
func every(ctx context.Context, interval time.Duration, fn func(context.Context)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		fn(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Run ensures the bucket and the stream, then serves HTTP while it
// relays the outbox, works on imports, and expires and sweeps files.
func (s *Service) Run(ctx context.Context) error {
	var background sync.WaitGroup
	defer background.Wait()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if err := s.ensureBucket(ctx); err != nil {
		return fmt.Errorf("contacts: ensuring bucket: %w", err)
	}
	if err := s.bus.EnsureStream(ctx, "CONTACTS_EVENTS", []string{"contacts.>"}); err != nil {
		return fmt.Errorf("contacts: ensuring stream: %w", err)
	}
	relay := outbox.NewRelay(s.pool, s.bus, relaySubject)
	loops := []struct {
		interval time.Duration
		fn       func(context.Context)
	}{
		{500 * time.Millisecond, func(ctx context.Context) { _, _ = relay.RelayOnce(ctx) }},
		{s.cfg.WorkerPoll, func(ctx context.Context) {
			for ctx.Err() == nil {
				n, err := s.app.ProcessImports(ctx)
				if err != nil {
					slog.ErrorContext(ctx, "contacts: processing imports", "error", err)
				}
				if n == 0 || err != nil {
					return
				}
			}
		}},
		{s.cfg.SweepInterval, func(ctx context.Context) {
			if _, err := s.app.ExpireImports(ctx); err != nil {
				slog.ErrorContext(ctx, "contacts: expiring imports", "error", err)
			}
			if res, err := s.app.SweepImportFiles(ctx); err != nil {
				slog.ErrorContext(ctx, "contacts: deleting imports' files", "error", err)
			} else if res.Failed > 0 {
				slog.WarnContext(ctx, "contacts: some imports' files couldn't be deleted yet", "failed", res.Failed)
			}
		}},
	}
	for _, l := range loops {
		background.Add(1)
		go func() {
			defer background.Done()
			every(ctx, l.interval, l.fn)
		}()
	}

	grpcLn, err := net.Listen("tcp", s.cfg.GRPCAddr)
	if err != nil {
		return fmt.Errorf("contacts: listening for gRPC on %s: %w", s.cfg.GRPCAddr, err)
	}
	// A CheckConsent of 10,000 numbers is about 200 KB.
	grpcSrv := gogrpc.NewServer(gogrpc.MaxRecvMsgSize(4 << 20))
	contactsgrpc.Register(grpcSrv, s.app, slog.Default())
	background.Add(1)
	go func() {
		defer background.Done()
		_ = grpcSrv.Serve(grpcLn)
	}()
	defer grpcSrv.GracefulStop()

	ln, err := net.Listen("tcp", s.cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("contacts: listening on %s: %w", s.cfg.HTTPAddr, err)
	}
	srv := &http.Server{Handler: s.rtr, ReadHeaderTimeout: 5 * time.Second}
	return server.Serve(ctx, srv, ln, 10*time.Second)
}
