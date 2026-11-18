//go:build integration

package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/clamav"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/clock"
	vascrypto "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/crypto"
	vasdb "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/events"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/objectstore"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/packs"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/testkit"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/uploads"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/clamd"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/contactaccess"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/contacts/app"
)

type harness struct {
	svc      *app.Service
	super    *vasdb.Pool
	app      *vasdb.Pool
	store    *objectstore.Store
	clamd    *clamd.Server
	accounts *contactaccess.Fake
	clk      *clock.Fake
	logs     *syncBuffer
	deps     app.Deps
}

type option func(*app.Deps)

func batchSize(n int) option { return func(d *app.Deps) { d.BatchSize = n } }

func maxRows(n int) option { return func(d *app.Deps) { d.MaxRows = n } }

func afterBatch(f func(importID uuid.UUID, batch int) error) option {
	return func(d *app.Deps) { d.AfterBatch = f }
}

// newHarness starts Postgres (as contacts_app, with only
// contacts_service's privileges), SeaweedFS and the clamd fake, with
// the KE pack and a fake clock at the real time (storage checks signed
// forms against real time).
func newHarness(t *testing.T, opts ...option) *harness {
	t.Helper()
	ctx := context.Background()
	pg := testkit.StartPostgres(t, testkit.WithMigrations("../../../migrations/contacts"))
	if _, err := pg.Pool.Exec(ctx, `CREATE ROLE contacts_app LOGIN PASSWORD 'contacts_app'; GRANT contacts_service TO contacts_app;`); err != nil {
		t.Fatal(err)
	}
	super, err := vasdb.NewPool(ctx, pg.DSN()+"&search_path=contacts,public")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(super.Close)
	appPool, err := vasdb.NewPool(ctx, pg.RoleDSN("contacts_app", "contacts_app")+"&search_path=contacts,public")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(appPool.Close)

	cfg := testkit.StartObjectStorage(t)
	cfg.Bucket = "contacts"
	cfg.PublicEndpoint = cfg.Endpoint
	store, err := objectstore.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureBucket(ctx); err != nil {
		t.Fatal(err)
	}
	fake, err := clamd.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fake.Close)

	clk := clock.NewFake(time.Now().UTC().Truncate(time.Second))
	kms, err := vascrypto.NewLocalKMS(map[string][]byte{"contacts-files": bytes.Repeat([]byte{5}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	upCfg := uploads.DefaultConfig()
	upCfg.KMSKeyID = "contacts-files"
	upCfg.DownloadKey = bytes.Repeat([]byte{6}, 32)
	up, err := uploads.New(store, clamav.New(fake.Addr()), vascrypto.NewEnvelope(kms), clk, upCfg)
	if err != nil {
		t.Fatal(err)
	}
	ke, err := packs.Load("../../../packs/schema.json", "../../../packs/ke/pack.yaml")
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	accounts := contactaccess.New()
	deps := app.Deps{
		Pool: appPool, Clock: clk, Logger: slog.New(slog.NewJSONHandler(logs, nil)), Accounts: accounts,
		Packs: map[string]*packs.Pack{"KE": ke}, Uploads: up, DownloadBaseURL: "https://api.vaspro.test",
	}
	for _, o := range opts {
		o(&deps)
	}
	return &harness{svc: app.New(deps), super: super, app: appPool, store: store, clamd: fake, accounts: accounts, clk: clk, logs: logs, deps: deps}
}

// member is a user granted perms in a fresh account.
type member struct {
	user, tenant uuid.UUID
}

func (h *harness) member(perms ...string) member {
	m := member{user: uuid.New(), tenant: uuid.New()}
	h.accounts.Grant(m.user, m.tenant, perms...)
	return m
}

func (h *harness) manager() member { return h.member(app.PermRead, app.PermManage) }

// upload posts body through the import's signed form, as a browser does.
func upload(t *testing.T, form objectstore.UploadForm, body []byte) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range form.Fields {
		_ = mw.WriteField(k, v)
	}
	fw, err := mw.CreateFormFile("file", "contacts.csv")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fw.Write(body)
	_ = mw.Close()
	req, err := http.NewRequest(http.MethodPost, form.URL, &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		t.Fatalf("upload answered %d", resp.StatusCode)
	}
}

// runImport creates an import, uploads body, confirms it and runs the
// worker until nothing is left to process.
func (h *harness) runImport(t *testing.T, m member, in app.ImportInput, body []byte) app.Import {
	t.Helper()
	ctx := context.Background()
	imp, form, err := h.svc.CreateImport(ctx, m.user, m.tenant, in)
	if err != nil {
		t.Fatalf("CreateImport: %v", err)
	}
	upload(t, form.Form, body)
	if _, err := h.svc.ConfirmImportUpload(ctx, m.user, m.tenant, imp.ID); err != nil {
		t.Fatalf("ConfirmImportUpload: %v", err)
	}
	h.drain(t)
	got, err := h.svc.GetImport(ctx, m.user, m.tenant, imp.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// drain runs the worker until a pass finds nothing to do.
func (h *harness) drain(t *testing.T) {
	t.Helper()
	for range 20 {
		n, err := h.svc.ProcessImports(context.Background())
		if err != nil {
			t.Fatalf("ProcessImports: %v", err)
		}
		if n == 0 {
			return
		}
	}
	t.Fatal("imports still being processed after 20 passes")
}

func csvImport() app.ImportInput {
	return app.ImportInput{FileName: "contacts.csv", ContentType: "text/csv"}
}

// count runs a count as the owner.
func (h *harness) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := h.super.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// events returns the outbox's events of a type, oldest first, each
// checked against its schema in docs/events/.
func (h *harness) events(t *testing.T, eventType string) []map[string]any {
	t.Helper()
	rows, err := h.super.Query(context.Background(), `SELECT payload FROM outbox_events WHERE event_type = $1 ORDER BY occurred_at, id`, eventType)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	raw, err := os.ReadFile("../../../docs/events/" + eventType + ".schema.json") //nolint:gosec // a fixed name from the test
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	comp := jsonschema.NewCompiler()
	if err := comp.AddResource("s.json", doc); err != nil {
		t.Fatal(err)
	}
	schema, err := comp.Compile("s.json")
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			t.Fatal(err)
		}
		var env events.Envelope
		if err := json.Unmarshal(payload, &env); err != nil {
			t.Fatal(err)
		}
		var data any
		if err := json.Unmarshal(env.Data, &data); err != nil {
			t.Fatal(err)
		}
		if err := schema.Validate(data); err != nil {
			t.Fatalf("%s doesn't match its schema: %v", eventType, err)
		}
		if strings.Contains(string(env.Data), "+254") || strings.Contains(string(env.Data), "+999") {
			t.Fatalf("%s carries a phone number: %s", eventType, env.Data)
		}
		out = append(out, data.(map[string]any))
	}
	return out
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
