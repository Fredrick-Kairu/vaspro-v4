//go:build integration

package webhooks_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/bus"
	vasdb "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/events"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/testkit"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/staffcheck"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/tenantperms"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/webhooks"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/webhooks/adapters"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/webhooks/api"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/webhooks/app"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/webhooks/domain"
)

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func key(b byte) string {
	return base64.StdEncoding.EncodeToString([]byte(strings.Repeat(string(b), 32)))
}

// config is the environment's defaults, with test keys.
func config() webhooks.Config {
	return webhooks.Config{
		SecretsKMSKey: key(1), SecretFingerprintKey: key(2), MaxEndpoints: 16,
		RetrySchedule: "30s,2m,10m,30m,1h,3h,6h,12h,24h,24h", RetryJitter: 0.1, DisableAfter: 72 * time.Hour,
		Workers: 16, PerEndpoint: 4, BatchSize: 100, Lease: 5 * time.Minute,
		PollInterval: time.Second, RequestTimeout: 10 * time.Second, StreamRetry: 5 * time.Second,
		PacksDir: "../../packs", SinkBatch: 1000, SinkWait: time.Second,
	}
}

// TestService_DeliversABusEventEndToEnd runs the service as production
// wires it (as a role granted webhooks_service): /readyz; an endpoint
// registered and verified through its own wiring (KMS, fingerprint key,
// Sender); then a wallet.credited published on the ledger's stream,
// which is created only after the service started, as when the ledger
// starts later, reaches the endpoint signed, through the consumer and
// the worker loop. It shuts down cleanly.
func TestService_DeliversABusEventEndToEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pg := testkit.StartPostgres(t, testkit.WithMigrations("../../migrations/webhooks"))
	if _, err := pg.Pool.Exec(ctx, `CREATE ROLE webhooks_app LOGIN PASSWORD 'webhooks_app'; GRANT webhooks_service TO webhooks_app;`); err != nil {
		t.Fatal(err)
	}
	pool, err := vasdb.NewPool(ctx, pg.RoleDSN("webhooks_app", "webhooks_app")+"&search_path=webhooks,public")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	b, err := bus.Connect(ctx, testkit.StartNATSURL(t))
	if err != nil {
		t.Fatal(err)
	}

	accounts, identity := tenantperms.New(), staffcheck.New()
	cfg := config()
	cfg.HTTPAddr, cfg.AllowInsecureURLs = freeAddr(t), true
	cfg.PollInterval, cfg.StreamRetry, cfg.SinkWait = 100*time.Millisecond, 100*time.Millisecond, 100*time.Millisecond
	chDSN := testkit.StartClickHouse(t, testkit.WithClickHouseMigrations("../../migrations/clickhouse"))
	ch, err := adapters.OpenClickHouse(chDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ch.Close() })
	svc, err := webhooks.New(cfg, pool, b, webhooks.Clients{Accounts: accounts, Identity: identity, Events: ch})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- svc.Run(ctx) }()

	deadline := time.Now().Add(15 * time.Second)
	for {
		resp, err := http.Get("http://" + cfg.HTTPAddr + "/readyz") //nolint:gosec,noctx // the test's own server
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("/readyz never became ready: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}

	var (
		mu       sync.Mutex
		received []*http.Request
		bodies   [][]byte
	)
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var env struct {
			Data struct {
				Challenge string `json:"challenge"`
			} `json:"data"`
		}
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		received, bodies = append(received, r), append(bodies, body)
		mu.Unlock()
		_ = json.Unmarshal(body, &env)
		_ = json.NewEncoder(w).Encode(map[string]string{"challenge": env.Data.Challenge})
	}))
	defer endpoint.Close()
	user, session, tenant := uuid.New(), uuid.New(), uuid.New()
	accounts.Grant(user, tenant, app.PermCredentialsManage)
	identity.StepUp(user, session)
	// Over the HTTP API, as the gateway forwards it.
	call := func(method, path string, body any) map[string]any {
		t.Helper()
		raw, _ := json.Marshal(body)
		req, _ := http.NewRequestWithContext(ctx, method, "http://"+cfg.HTTPAddr+"/v1/webhooks/tenants/"+tenant.String()+path, bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", uuid.NewString())
		req.Header.Set(api.HeaderUserID, user.String())
		req.Header.Set(api.HeaderSessionID, session.String())
		resp, err := http.DefaultClient.Do(req) //nolint:gosec // the test's own server
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		if resp.StatusCode >= 300 {
			t.Fatalf("%s %s: %d %v", method, path, resp.StatusCode, out)
		}
		return out
	}
	created := call(http.MethodPost, "/endpoints", map[string]any{"url": endpoint.URL, "events": []string{"*"}})
	secret, _ := created["secret"].(string)
	if got := call(http.MethodPost, "/endpoints/"+created["id"].(string)+"/verification", nil); got["status"] != "active" {
		t.Fatalf("verify: %v", got)
	}

	if err := b.EnsureStream(ctx, "LEDGER_EVENTS", []string{"ledger.>"}); err != nil {
		t.Fatal(err)
	}
	env, err := events.New(uuid.New(), "wallet.credited", 1, tenant, time.Now().UTC(), "trace", map[string]any{"tenant_id": tenant, "amount_minor": 500})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(env)
	if err := b.Publish(ctx, "ledger.wallet.credited", raw); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(20 * time.Second)
	var got *http.Request
	var gotBody []byte
	for got == nil {
		mu.Lock()
		for i, r := range received {
			if r.Header.Get(domain.HeaderID) == env.ID.String() {
				got, gotBody = r, bodies[i]
			}
		}
		mu.Unlock()
		if got == nil && time.Now().After(deadline) {
			t.Fatal("the event never reached the endpoint")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := domain.Verify(secret, got.Header.Get(domain.HeaderTimestamp), got.Header.Get(domain.HeaderSignature), gotBody, time.Now()); err != nil {
		t.Fatalf("the delivery doesn't verify: %v", err)
	}
	if !strings.Contains(string(gotBody), `"amount_minor":500`) || strings.Contains(string(gotBody), "trace") {
		t.Fatalf("body %s", gotBody)
	}

	// The same event, published again, is in ClickHouse once (task 5.6).
	if err := b.Publish(ctx, "ledger.wallet.credited", raw); err != nil {
		t.Fatal(err)
	}
	raw2, err := sql.Open("clickhouse", chDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw2.Close() }()
	stored := func() (copies, distinct int64) {
		_ = raw2.QueryRowContext(ctx, "SELECT count() FROM events WHERE id = ?", env.ID).Scan(&copies)
		_ = raw2.QueryRowContext(ctx, "SELECT count() FROM events FINAL WHERE id = ?", env.ID).Scan(&distinct)
		return copies, distinct
	}
	deadline = time.Now().Add(20 * time.Second)
	for {
		copies, distinct := stored()
		if copies >= 2 {
			if distinct != 1 {
				t.Fatalf("%d copies read as %d events with FINAL, want 1", copies, distinct)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the sink stored %d copies of the event published twice", copies)
		}
		time.Sleep(50 * time.Millisecond)
	}
	var source, tenantID string
	if err := raw2.QueryRowContext(ctx, "SELECT source, toString(tenant_id) FROM events FINAL WHERE id = ?", env.ID).Scan(&source, &tenantID); err != nil ||
		source != "ledger" || tenantID != tenant.String() {
		t.Fatalf("stored source %q, tenant %q (%v)", source, tenantID, err)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run didn't stop")
	}
}

func TestService_RefusesBadConfig(t *testing.T) {
	good := config()
	for name, mutate := range map[string]func(*webhooks.Config){
		"a KMS key that isn't base64":    func(c *webhooks.Config) { c.SecretsKMSKey = "not base64!" },
		"a KMS key of the wrong length":  func(c *webhooks.Config) { c.SecretsKMSKey = base64.StdEncoding.EncodeToString([]byte("short")) },
		"a short fingerprint key":        func(c *webhooks.Config) { c.SecretFingerprintKey = base64.StdEncoding.EncodeToString([]byte("short")) },
		"no endpoints allowed":           func(c *webhooks.Config) { c.MaxEndpoints = 0 },
		"a malformed retry schedule":     func(c *webhooks.Config) { c.RetrySchedule = "30s,soon" },
		"jitter of 1":                    func(c *webhooks.Config) { c.RetryJitter = 1 },
		"negative jitter":                func(c *webhooks.Config) { c.RetryJitter = -0.1 },
		"no workers":                     func(c *webhooks.Config) { c.Workers = 0 },
		"more per endpoint than workers": func(c *webhooks.Config) { c.PerEndpoint = 32 },
		"a lease shorter than a batch":   func(c *webhooks.Config) { c.Lease = time.Minute },
		"no request timeout":             func(c *webhooks.Config) { c.RequestTimeout = 0 },
		"no disabling time":              func(c *webhooks.Config) { c.DisableAfter = 0 },
		"no sink batch":                  func(c *webhooks.Config) { c.SinkBatch = 0 },
		"no packs":                       func(c *webhooks.Config) { c.PacksDir = "../../packs-missing/schema" },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := good
			mutate(&cfg)
			if _, err := webhooks.New(cfg, nil, nil, webhooks.Clients{}); err == nil {
				t.Fatal("New accepted it")
			}
		})
	}
	if _, err := webhooks.New(good, nil, nil, webhooks.Clients{}); err != nil {
		t.Fatalf("the good config: %v", err)
	}
}
