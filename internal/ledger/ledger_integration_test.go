//go:build integration

package ledger_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	gogrpc "google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/bus"
	vasdb "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"
	platformevents "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/events"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/packs"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/testkit"
	ledgerv1 "github.com/Fredrick-Kairu/vaspro-v4/proto/ledger/v1"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/staffcheck"
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

// TestService_RunsAndRelaysWalletCredited runs the whole service as
// production wires it (as a role granted ledger_service): /readyz
// answers, a top-up over gRPC posts, and its wallet.credited event
// reaches NATS on ledger.wallet.credited through the outbox relay.
func TestService_RunsAndRelaysWalletCredited(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pg := testkit.StartPostgres(t, testkit.WithMigrations("../../migrations/ledger"))
	if _, err := pg.Pool.Exec(ctx, `CREATE ROLE ledger_app LOGIN PASSWORD 'ledger_app'; GRANT ledger_service TO ledger_app;`); err != nil {
		t.Fatal(err)
	}
	pool, err := vasdb.NewPool(ctx, pg.RoleDSN("ledger_app", "ledger_app")+"&search_path=ledger,public")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	natsURL := testkit.StartNATSURL(t)
	b, err := bus.Connect(ctx, natsURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)

	ke, err := packs.Load("../../packs/schema.json", "../../packs/ke/pack.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, `
		INSERT INTO ledger.token_prices (id, service_code, currency, minor_per_token, status, effective_from, created_by, approved_by)
		VALUES ($1, 'sms', 'KES', 80, 'active', '2026-01-01', $2, $3)`, uuid.New(), uuid.New(), uuid.New()); err != nil {
		t.Fatal(err)
	}
	cfg := ledger.Config{HTTPAddr: freeAddr(t), GRPCAddr: freeAddr(t), SettlementWindow: time.Second}
	svc := ledger.New(cfg, pool, b, staffcheck.New(), map[string]*packs.Pack{"KE": ke})
	done := make(chan error, 1)
	go func() { done <- svc.Run(ctx) }()

	deadline := time.Now().Add(15 * time.Second)
	for {
		resp, err := http.Get(fmt.Sprintf("http://%s/readyz", cfg.HTTPAddr)) //nolint:gosec,noctx // a local test server
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("/readyz never answered OK: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}

	nc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	events, err := js.OrderedConsumer(ctx, "LEDGER_EVENTS", jetstream.OrderedConsumerConfig{FilterSubjects: []string{"ledger.wallet.credited"}})
	if err != nil {
		t.Fatal(err)
	}

	conn, err := gogrpc.NewClient(cfg.GRPCAddr, gogrpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	tenant := uuid.New()
	res, err := ledgerv1.NewLedgerServiceClient(conn).PostTransaction(ctx, &ledgerv1.PostTransactionRequest{
		TenantId: tenant.String(),
		Posting: &ledgerv1.Posting{IdempotencyKey: "mpesa:SMOKE1", Type: "topup", Description: "M-Pesa top-up", Lines: []*ledgerv1.Line{
			{Account: &ledgerv1.AccountRef{Type: "provider_clearing", Rail: "mpesa_ke", Currency: "KES"}, Direction: "D", AmountMinor: 1300000},
			{Account: &ledgerv1.AccountRef{Type: "main_wallet", Currency: "KES"}, Direction: "C", AmountMinor: 1300000},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	msg, err := events.Next(jetstream.FetchMaxWait(10 * time.Second))
	if err != nil {
		t.Fatalf("no wallet.credited on the bus: %v", err)
	}
	var env platformevents.Envelope
	if err := json.Unmarshal(msg.Data(), &env); err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatal(err)
	}
	if env.TenantID != tenant || data["transaction_id"] != res.GetTransactionId() || data["amount_minor"].(float64) != 1300000 {
		t.Fatalf("wallet.credited: tenant %s, data %v", env.TenantID, data)
	}

	// Usage settles on its own each window (task 3.5): hold 400 SMS
	// units, capture 380, and usage.posted follows within a window.
	usage, err := js.OrderedConsumer(ctx, "LEDGER_EVENTS", jetstream.OrderedConsumerConfig{FilterSubjects: []string{"ledger.usage.posted"}})
	if err != nil {
		t.Fatal(err)
	}
	client := ledgerv1.NewLedgerServiceClient(conn)
	if _, err := client.PurchaseTokens(ctx, &ledgerv1.PurchaseTokensRequest{TenantId: tenant.String(), IdempotencyKey: "buy-1",
		ServiceCode: "sms", Currency: "KES", Tokens: "10000"}); err != nil {
		t.Fatal(err)
	}
	ref := uuid.NewString()
	sms := &ledgerv1.AccountRef{Type: "service_wallet", ServiceCode: "sms", Currency: "KES"}
	if _, err := client.PlaceHold(ctx, &ledgerv1.PlaceHoldRequest{TenantId: tenant.String(), Account: sms, ReferenceType: "campaign",
		ReferenceId: ref, Tokens: "400", ExpiresAtUnix: time.Now().Add(time.Hour).Unix()}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CaptureHold(ctx, &ledgerv1.CaptureHoldRequest{TenantId: tenant.String(), ReferenceType: "campaign",
		ReferenceId: ref, CapturedTokens: "380", Events: 380}); err != nil {
		t.Fatal(err)
	}
	msg, err = usage.Next(jetstream.FetchMaxWait(10 * time.Second))
	if err != nil {
		t.Fatalf("no usage.posted on the bus: %v", err)
	}
	if err := json.Unmarshal(msg.Data(), &env); err != nil {
		t.Fatal(err)
	}
	var posted map[string]any
	if err := json.Unmarshal(env.Data, &posted); err != nil {
		t.Fatal(err)
	}
	if posted["value_minor"].(float64) != 30400 || posted["revenue_minor"].(float64) != 26207 || posted["vat_minor"].(float64) != 4193 {
		t.Fatalf("usage.posted %v, want 304.00 = 262.07 + 41.93", posted)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v on shutdown", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run didn't return after cancel")
	}
}
