//go:build integration

package payments_test

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/bus"
	vasdb "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/testkit"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/tenantinfo"
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

// TestService_RunsAndSyncsRails runs the service as production wires it
// (as a role granted payments_service): the rails are synced from the
// real packs at startup, /readyz answers, and it shuts down cleanly.
func TestService_RunsAndSyncsRails(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pg := testkit.StartPostgres(t, testkit.WithMigrations("../../migrations/payments"))
	if _, err := pg.Pool.Exec(ctx, `CREATE ROLE payments_app LOGIN PASSWORD 'payments_app'; GRANT payments_service TO payments_app;`); err != nil {
		t.Fatal(err)
	}
	pool, err := vasdb.NewPool(ctx, pg.RoleDSN("payments_app", "payments_app")+"&search_path=payments,public")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	b, err := bus.Connect(ctx, testkit.StartNATSURL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	loaded, err := payments.LoadPacks("../../packs")
	if err != nil {
		t.Fatal(err)
	}

	cfg := payments.Config{HTTPAddr: freeAddr(t)}
	svc := payments.New(cfg, pool, b, tenantinfo.New(), loaded)
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
			t.Fatalf("/readyz never answered 200: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	var live int
	if err := pg.Pool.QueryRow(ctx, `SELECT count(*) FROM payments.rails WHERE code = 'mpesa_ke' AND status = 'live'`).Scan(&live); err != nil || live != 1 {
		t.Fatalf("mpesa_ke synced at startup: %d %v", live, err)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run didn't return after cancel")
	}
}
