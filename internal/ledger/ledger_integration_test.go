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

	cfg := ledger.Config{HTTPAddr: freeAddr(t), GRPCAddr: freeAddr(t)}
	svc := ledger.New(cfg, pool, b, staffcheck.New())
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
