//go:build integration

package app_test

import (
	"bytes"
	"context"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/santhosh-tekuri/jsonschema/v6"

	vascrypto "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/crypto"
	vasdb "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/events"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/packs"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/testkit"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/staffcheck"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/tenantperms"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/webhooks/adapters"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/webhooks/app"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/webhooks/domain"
)

var fingerprintKey = bytes.Repeat([]byte{7}, 32)

type harness struct {
	svc      *app.Service
	super    *vasdb.Pool
	app      *vasdb.Pool
	accounts *tenantperms.Fake
	identity *staffcheck.Fake
	clk      *testkit.Clock
	logs     *syncBuffer
	recv     *receiver
}

type option func(*app.Deps, *adapters.SenderConfig)

// secure turns the development setting off: https and public addresses
// only, as in production.
func secure() option {
	return func(d *app.Deps, c *adapters.SenderConfig) { d.AllowInsecureURLs, c.AllowInsecure = false, false }
}

func maxEndpoints(n int) option {
	return func(d *app.Deps, _ *adapters.SenderConfig) { d.MaxEndpoints = n }
}

func schedule(waits ...time.Duration) option {
	return func(d *app.Deps, _ *adapters.SenderConfig) { d.Schedule = waits }
}

func pool(workers, perEndpoint int) option {
	return func(d *app.Deps, _ *adapters.SenderConfig) { d.Workers, d.PerEndpoint = workers, perEndpoint }
}

// withClickHouse gives the service a migrated ClickHouse and the KE
// pack (task 5.6's reports); raw is set to a connection of the test's
// own, for what the service never does (OPTIMIZE, counting rows).
func withClickHouse(t *testing.T, raw **sql.DB) option {
	t.Helper()
	dsn := testkit.StartClickHouse(t, testkit.WithClickHouseMigrations("../../../migrations/clickhouse"))
	ch, err := adapters.OpenClickHouse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ch.Close() })
	db, err := sql.Open("clickhouse", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	*raw = db
	ke, err := packs.Load("../../../packs/schema.json", "../../../packs/ke/pack.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return func(d *app.Deps, _ *adapters.SenderConfig) {
		d.Events, d.Packs = ch, map[string]*packs.Pack{"KE": ke}
	}
}

func requestTimeout(d time.Duration) option {
	return func(_ *app.Deps, c *adapters.SenderConfig) { c.Timeout = d }
}

func newHarness(t *testing.T, opts ...option) harness {
	t.Helper()
	ctx := context.Background()
	pg := testkit.StartPostgres(t, testkit.WithMigrations("../../../migrations/webhooks"))
	if _, err := pg.Pool.Exec(ctx, `CREATE ROLE webhooks_app LOGIN PASSWORD 'webhooks_app'; GRANT webhooks_service TO webhooks_app;`); err != nil {
		t.Fatal(err)
	}
	super, err := vasdb.NewPool(ctx, pg.DSN()+"&search_path=webhooks,public")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(super.Close)
	pool, err := vasdb.NewPool(ctx, pg.RoleDSN("webhooks_app", "webhooks_app")+"&search_path=webhooks,public")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	kms, err := vascrypto.NewLocalKMS(map[string][]byte{app.SecretsKMSKeyID: bytes.Repeat([]byte{9}, 32)})
	if err != nil {
		t.Fatal(err)
	}

	recv := newReceiver(t)
	clk := testkit.NewClock(time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC))
	logs := &syncBuffer{}
	h := harness{super: super, app: pool, accounts: tenantperms.New(), identity: staffcheck.New(), clk: clk, logs: logs, recv: recv}
	deps := app.Deps{
		Pool: pool, Clock: clk, Logger: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Accounts: h.accounts, Identity: h.identity, KMS: kms, FingerprintKey: fingerprintKey, AllowInsecureURLs: true,
	}
	cfg := adapters.SenderConfig{AllowInsecure: true, RootCAs: recv.roots, Timeout: 500 * time.Millisecond}
	for _, o := range opts {
		o(&deps, &cfg)
	}
	deps.Sender = adapters.NewSender(cfg)
	h.svc = app.New(deps)
	return h
}

// member is a dashboard user in a tenant, with a session.
type member struct {
	user, session, tenant uuid.UUID
}

// admin can manage credentials and has a fresh step-up.
func admin(h harness, tenant uuid.UUID) member {
	m := member{user: uuid.New(), session: uuid.New(), tenant: tenant}
	h.accounts.Grant(m.user, tenant, app.PermCredentialsRead, app.PermCredentialsManage)
	h.identity.StepUp(m.user, m.session)
	return m
}

func viewer(h harness, tenant uuid.UUID) member {
	m := member{user: uuid.New(), session: uuid.New(), tenant: tenant}
	h.accounts.Grant(m.user, tenant, app.PermCredentialsRead)
	h.identity.StepUp(m.user, m.session)
	return m
}

func create(t *testing.T, h harness, m member, url string, events ...string) app.CreatedEndpoint {
	t.Helper()
	if len(events) == 0 {
		events = []string{"wallet.credited"}
	}
	ep, err := h.svc.CreateEndpoint(context.Background(), m.user, m.session, m.tenant, app.NewEndpoint{URL: url, Description: "orders", Events: events})
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	return ep
}

func count(t *testing.T, h harness, query string, args ...any) int {
	t.Helper()
	var n int
	if err := h.super.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func wantErr(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("got %v, want %v", err, want)
	}
}

// warmPool opens every pooled connection first, so concurrent callers
// really overlap (4.5's lesson).
func warmPool(t *testing.T, pool *vasdb.Pool) {
	t.Helper()
	conns := make([]*pgxpool.Conn, 0, pool.Stat().MaxConns())
	for range pool.Stat().MaxConns() {
		c, err := pool.Acquire(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
	}
	for _, c := range conns {
		c.Release()
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// received is one request an endpoint got.
type received struct {
	header http.Header
	body   []byte
	at     time.Time // real time, for ordering across endpoints
}

// receiver is a client's endpoint: an https server answering in the
// mode a test sets, recording every request.
type receiver struct {
	srv    *httptest.Server
	roots  *x509.CertPool
	mu     sync.Mutex
	mode   string
	script []string // modes for the next requests, before mode
	before func()
	got    []received
}

// The receiver's modes.
const (
	echo     = "echo"     // 200 with the challenge echoed
	wrong    = "wrong"    // 200 with another value
	fail500  = "500"      // 500 with the challenge echoed
	slow     = "slow"     // no answer until the client gives up
	redirect = "redirect" // 302 to an echoing path
	notJSON  = "not-json" // 200 with the bare challenge
)

// then answers the next requests in these modes, one each, then
// returns to the receiver's mode.
func (r *receiver) then(modes ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.script = append(r.script, modes...)
}

// reset forgets the requests so far (the verification challenge, say).
func (r *receiver) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = nil
}

// newReceiver is another endpoint. httptest's TLS servers share one
// certificate, so the harness's Sender trusts every receiver.
func newReceiver(t *testing.T) *receiver {
	r := &receiver{mode: echo}
	r.srv = httptest.NewTLSServer(http.HandlerFunc(r.serve))
	t.Cleanup(r.srv.Close)
	r.roots = x509.NewCertPool()
	r.roots.AddCert(r.srv.Certificate())
	return r
}

func (r *receiver) URL() string { return r.srv.URL + "/hooks/vaspro" }

func (r *receiver) set(mode string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.mode = mode
}

// beforeAnswer runs fn once, inside the next request, before answering.
func (r *receiver) beforeAnswer(fn func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.before = fn
}

func (r *receiver) requests() []received {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]received(nil), r.got...)
}

func (r *receiver) last(t *testing.T) received {
	t.Helper()
	got := r.requests()
	if len(got) == 0 {
		t.Fatal("the endpoint received nothing")
	}
	return got[len(got)-1]
}

func (r *receiver) serve(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	r.got = append(r.got, received{header: req.Header.Clone(), body: body, at: time.Now()})
	mode, before := r.mode, r.before
	if len(r.script) > 0 {
		mode, r.script = r.script[0], r.script[1:]
	}
	r.before = nil
	r.mu.Unlock()
	if before != nil {
		before()
	}
	var env struct {
		Data struct {
			Challenge string `json:"challenge"`
		} `json:"data"`
	}
	_ = json.Unmarshal(body, &env)
	answer, _ := json.Marshal(map[string]string{"challenge": env.Data.Challenge})
	switch mode {
	case echo:
		_, _ = w.Write(answer)
	case wrong:
		_, _ = w.Write([]byte(`{"challenge":"something else"}`))
	case fail500:
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write(answer)
	case slow:
		select {
		case <-req.Context().Done():
		case <-time.After(5 * time.Second):
		}
	case redirect:
		http.Redirect(w, req, "/elsewhere", http.StatusFound)
	case notJSON:
		_, _ = w.Write([]byte(env.Data.Challenge))
	}
}

// signatures splits a request's signature header into its v1 values.
func signatures(rec received) []string {
	return splitFields(rec.header.Get(domain.HeaderSignature))
}

func splitFields(s string) []string {
	var out []string
	for _, f := range bytes.Fields([]byte(s)) {
		out = append(out, string(f))
	}
	return out
}

// activeEndpoint registers an endpoint at r's URL and passes its
// challenge, then forgets the challenge request.
func activeEndpoint(t *testing.T, h harness, m member, r *receiver, events ...string) app.CreatedEndpoint {
	t.Helper()
	ep := create(t, h, m, r.URL(), events...)
	if _, err := h.svc.VerifyEndpoint(context.Background(), m.user, m.tenant, ep.ID); err != nil {
		t.Fatalf("VerifyEndpoint: %v", err)
	}
	r.reset()
	return ep
}

// publish hands the service an event as the bus would, and returns it.
func publish(t *testing.T, h harness, eventType string, tenant uuid.UUID) events.Envelope {
	t.Helper()
	env, err := events.New(uuid.New(), eventType, 1, tenant, h.clk.Now(), "trace-1", map[string]any{"tenant_id": tenant, "n": 1})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.svc.HandleEvent(context.Background(), raw); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	return env
}

func deliverDue(t *testing.T, h harness) app.Summary {
	t.Helper()
	sum, err := h.svc.DeliverDue(context.Background())
	if err != nil {
		t.Fatalf("DeliverDue: %v", err)
	}
	return sum
}

// deliveryRow is a delivery as stored.
type deliveryRow struct {
	id         uuid.UUID
	endpoint   uuid.UUID
	status     string
	deadReason *string
	attempts   int
	next       *time.Time
	body       []byte
}

func deliveriesFor(t *testing.T, h harness, eventID uuid.UUID) []deliveryRow {
	t.Helper()
	rows, err := h.super.Query(context.Background(), `
		SELECT id, endpoint_id, status, dead_reason, attempts, next_attempt_at, body FROM deliveries
		WHERE event_id = $1 ORDER BY created_at, id`, eventID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []deliveryRow
	for rows.Next() {
		var d deliveryRow
		if err := rows.Scan(&d.id, &d.endpoint, &d.status, &d.deadReason, &d.attempts, &d.next, &d.body); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

func onlyDelivery(t *testing.T, h harness, eventID uuid.UUID) deliveryRow {
	t.Helper()
	ds := deliveriesFor(t, h, eventID)
	if len(ds) != 1 {
		t.Fatalf("%d deliveries for the event, want 1", len(ds))
	}
	return ds[0]
}

// attemptRow is a recorded attempt.
type attemptRow struct {
	attempt   int
	outcome   string
	status    *int
	body      []byte
	headers   map[string]string
	errorText *string
}

func attemptsOf(t *testing.T, h harness, delivery uuid.UUID) []attemptRow {
	t.Helper()
	rows, err := h.super.Query(context.Background(), `
		SELECT attempt, outcome, response_status, response_body, request_headers, error FROM delivery_attempts
		WHERE delivery_id = $1 ORDER BY attempt`, delivery)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []attemptRow
	for rows.Next() {
		var a attemptRow
		if err := rows.Scan(&a.attempt, &a.outcome, &a.status, &a.body, &a.headers, &a.errorText); err != nil {
			t.Fatal(err)
		}
		out = append(out, a)
	}
	return out
}

// assertEventValid checks the newest eventType in the outbox against
// docs/events and returns the envelope's raw bytes and its data.
func assertEventValid(t *testing.T, h harness, eventType string) ([]byte, map[string]any) {
	t.Helper()
	var payload []byte
	if err := h.super.QueryRow(context.Background(),
		`SELECT payload FROM outbox_events WHERE event_type = $1 ORDER BY occurred_at DESC LIMIT 1`, eventType).Scan(&payload); err != nil {
		t.Fatalf("no %s in the outbox: %v", eventType, err)
	}
	var env events.Envelope
	if err := json.Unmarshal(payload, &env); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../../../docs/events/" + eventType + ".schema.json") //nolint:gosec // a fixed name from the test
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	comp := jsonschema.NewCompiler()
	if err := comp.AddResource(eventType+".schema.json", doc); err != nil {
		t.Fatal(err)
	}
	schema, err := comp.Compile(eventType + ".schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var data any
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(data); err != nil {
		t.Fatalf("%s doesn't match its schema: %v", eventType, err)
	}
	return payload, data.(map[string]any)
}
