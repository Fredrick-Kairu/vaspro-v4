//go:build integration

package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments/adapters"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments/app"
	vascrypto "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/crypto"
	vasdb "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"
	platformevents "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/events"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/packs"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/testkit"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/daraja"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/staffdirectory"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/tenantinfo"
)

// Test-only Daraja app for the fake; nothing real.
var darajaApp = daraja.App{ConsumerKey: "fake-consumer-key-4321", ConsumerSecret: "fake-consumer-secret-8765", ShortCode: "600100", Passkey: "fake-passkey-value-2468"}

var fingerprintKey = bytes.Repeat([]byte{7}, 32)

type harness struct {
	svc      *app.Service
	super    *vasdb.Pool
	app      *vasdb.Pool
	accounts *tenantinfo.Fake
	staff    *staffdirectory.Fake
	daraja   *daraja.Fake
	creds    *app.CredentialSource
	clk      *testkit.Clock
	logs     *bytes.Buffer
	ke       *packs.Pack
}

type option func(*app.Deps)

func newHarness(t *testing.T, opts ...option) harness {
	t.Helper()
	ctx := context.Background()
	pg := testkit.StartPostgres(t, testkit.WithMigrations("../../../migrations/payments"))
	if _, err := pg.Pool.Exec(ctx, `CREATE ROLE payments_app LOGIN PASSWORD 'payments_app'; GRANT payments_service TO payments_app;`); err != nil {
		t.Fatal(err)
	}
	super, err := vasdb.NewPool(ctx, pg.DSN()+"&search_path=payments,public")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(super.Close)
	pool, err := vasdb.NewPool(ctx, pg.RoleDSN("payments_app", "payments_app")+"&search_path=payments,public")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	ke, err := packs.Load("../../../packs/schema.json", "../../../packs/ke/pack.yaml")
	if err != nil {
		t.Fatal(err)
	}
	kms, err := vascrypto.NewLocalKMS(map[string][]byte{app.CredentialsKMSKeyID: bytes.Repeat([]byte{9}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	fake := daraja.New(darajaApp)
	t.Cleanup(fake.Close)
	creds := app.NewCredentialSource(pool, kms)
	logs := &bytes.Buffer{}
	clk := testkit.NewClock(time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC))
	h := harness{
		super: super, app: pool, accounts: tenantinfo.New(), staff: staffdirectory.New(), daraja: fake,
		creds: creds, clk: clk, logs: logs, ke: ke,
	}
	deps := app.Deps{
		Pool: pool, Clock: clk, Logger: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Accounts: h.accounts, Identity: h.staff, Packs: map[string]*packs.Pack{"KE": ke},
		KMS: kms, FingerprintKey: fingerprintKey,
		Rails: map[string]app.RailClient{"mpesa_ke": adapters.NewDaraja(adapters.DarajaConfig{
			BaseURL: fake.URL(), Cache: testkit.StartRedis(t), Credentials: creds, Location: daraja.EAT, Clock: clk,
		})},
	}
	for _, o := range opts {
		o(&deps)
	}
	h.svc = app.New(deps)
	return h
}

func count(t *testing.T, h harness, query string, args ...any) int {
	t.Helper()
	var n int
	if err := h.super.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// seedPaybill syncs the rails and inserts VASPRO's paybill with status,
// as the table owner.
func seedPaybill(t *testing.T, h harness, status string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	if err := h.svc.SyncRails(ctx); err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	var verified any
	if status == "active" {
		verified = h.clk.Now()
	}
	if _, err := h.super.Exec(ctx, `
		INSERT INTO rail_accounts (id, rail_code, account_type, number, display_name, uses, status, verified_at)
		VALUES ($1, 'mpesa_ke', 'paybill', $2, 'VASPRO', '{vaspro_topups}', $3, $4)`,
		id, darajaApp.ShortCode, status, verified); err != nil {
		t.Fatal(err)
	}
	return id
}

func wantErr(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("got %v, want %v", err, want)
	}
}

// assertEventValid checks the newest eventType for aggregate in the
// outbox against docs/events and returns its data.
func assertEventValid(t *testing.T, h harness, eventType string, aggregate uuid.UUID) map[string]any {
	t.Helper()
	var payload []byte
	err := h.super.QueryRow(context.Background(),
		`SELECT payload FROM outbox_events WHERE event_type = $1 AND aggregate_id = $2 ORDER BY occurred_at DESC LIMIT 1`,
		eventType, aggregate).Scan(&payload)
	if errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("no %s event for %s in the outbox", eventType, aggregate)
	}
	if err != nil {
		t.Fatal(err)
	}
	var env platformevents.Envelope
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
	return data.(map[string]any)
}
