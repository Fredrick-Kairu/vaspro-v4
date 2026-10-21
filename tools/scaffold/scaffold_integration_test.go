//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/bus"
	vasdb "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/testkit"
)

// Generate can't be exercised by importing its output: internal/demo
// doesn't exist in the checked-out tree, only transiently while this
// test runs, so nothing here can statically import it. Every check
// below instead builds and runs the generated cmd/demo as a real
// subprocess and talks to it over HTTP and Postgres, the same way a
// human verifying the M0 exit gate by hand would.

// repoRoot finds the module root from this test's working directory
// (tools/scaffold), since Generate must write into the real module for
// its output to compile against it.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("failed to resolve repo root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("expected %s to contain go.mod: %v", root, err)
	}
	return root
}

// generateDemo generates the "demo" service into the real repo, and
// registers cleanup that removes every generated file and restores
// cmd/vaspro-all/main.go to what it was before — so the M0 gate scenario
// gets checked on every run without leaving a 15th service behind.
func generateDemo(t *testing.T, root string) {
	t.Helper()

	vasproAllMain := filepath.Join(root, "cmd", "vaspro-all", "main.go")
	before, err := os.ReadFile(vasproAllMain) //nolint:gosec // fixed path under the repo root
	if err != nil {
		t.Fatalf("failed to snapshot %s: %v", vasproAllMain, err)
	}

	t.Cleanup(func() {
		for _, dir := range []string{
			filepath.Join(root, "internal", "demo"),
			filepath.Join(root, "cmd", "demo"),
			filepath.Join(root, "migrations", "demo"),
		} {
			if err := os.RemoveAll(dir); err != nil {
				t.Errorf("cleanup: removing %s: %v", dir, err)
			}
		}
		if err := os.WriteFile(vasproAllMain, before, 0o600); err != nil { //nolint:gosec // vasproAllMain is a fixed path under the repo root, not external input
			t.Errorf("cleanup: restoring %s: %v", vasproAllMain, err)
		}
	})

	if err := Generate(root, "demo"); err != nil {
		t.Fatalf("Generate: %v", err)
	}
}

func TestGenerate_DemoService_CompilesAgainstTheRealModule(t *testing.T) {
	root := repoRoot(t)
	generateDemo(t, root)

	buildBothBinaries(t, root)
}

// buildBothBinaries compiles the generated cmd/demo and (proving the
// scaffold's registration edit also compiles) cmd/vaspro-all, returning
// cmd/demo's built binary path.
func buildBothBinaries(t *testing.T, root string) string {
	t.Helper()
	binPath := filepath.Join(t.TempDir(), "demo")

	cmd := exec.Command("go", "build", "-o", binPath, "./cmd/demo") //nolint:gosec // fixed "go build" invocation; binPath is our own t.TempDir() path, not external input
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/demo failed: %v\n%s", err, out)
	}

	vasproAllOut := filepath.Join(t.TempDir(), "vaspro-all")
	cmd = exec.Command("go", "build", "-o", vasproAllOut, "./cmd/vaspro-all") //nolint:gosec // fixed "go build" invocation; vasproAllOut is our own t.TempDir() path, not external input
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/vaspro-all failed (demo registration broke it): %v\n%s", err, out)
	}

	return binPath
}

func freeTCPAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to find a free port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !cond() {
		t.Fatalf("condition not met within %s", timeout)
	}
}

func TestGenerate_DemoService_RunsHealthzOutboxAndIdempotentInbox(t *testing.T) {
	root := repoRoot(t)
	generateDemo(t, root)
	binPath := buildBothBinaries(t, root)

	ctx := context.Background()

	pg := testkit.StartPostgres(t, testkit.WithMigrations(filepath.Join(root, "migrations", "demo")))
	natsURL := testkit.StartNATSURL(t)

	// A second NATS client, independent of the subprocess's own
	// connection, used below to simulate a redelivery.
	testBus, err := bus.Connect(ctx, natsURL)
	if err != nil {
		t.Fatalf("failed to connect to nats: %v", err)
	}
	defer testBus.Close()

	// A direct Postgres connection, scoped to the demo schema exactly
	// the way the subprocess's own connection is, to inspect
	// outbox_events/processed_events without going through the service.
	pool, err := vasdb.NewPool(ctx, pg.DSN()+"&search_path=demo")
	if err != nil {
		t.Fatalf("failed to connect with search_path=demo: %v", err)
	}
	defer pool.Close()

	httpAddr := freeTCPAddr(t)
	cmd := exec.CommandContext(ctx, binPath) //nolint:gosec // binPath is the binary we just built into our own t.TempDir(), not external input
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"DEMO_DATABASE_URL="+pg.DSN()+"&search_path=demo",
		"DEMO_NATS_URL="+natsURL,
		"DEMO_HTTP_ADDR="+httpAddr,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start demo binary: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	baseURL := "http://" + httpAddr
	waitFor(t, 10*time.Second, func() bool {
		resp, err := http.Get(baseURL + "/healthz") //nolint:gosec // fixed localhost test address
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})

	// --- /readyz over the real, running process ---
	resp, err := http.Get(baseURL + "/readyz") //nolint:gosec // fixed localhost test address
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /readyz: got status %d", resp.StatusCode)
	}

	// --- POST /v1/widgets writes an outbox row ---
	tenantID := "11111111-1111-7111-8111-111111111111"
	body, _ := json.Marshal(map[string]string{"tenant_id": tenantID, "name": "widget-a"})
	resp, err = http.Post(baseURL+"/v1/widgets", "application/json", bytes.NewReader(body)) //nolint:gosec // fixed localhost test address
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /v1/widgets: got status %d", resp.StatusCode)
	}

	var outboxCount int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM outbox_events").Scan(&outboxCount); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if outboxCount != 1 {
		t.Fatalf("expected exactly 1 outbox row after the write, got %d", outboxCount)
	}

	// --- the running service's own relay ticker and consumer process it ---
	var eventID string
	var payload []byte
	waitFor(t, 10*time.Second, func() bool {
		err := pool.QueryRow(ctx, "SELECT id, payload FROM outbox_events ORDER BY occurred_at DESC LIMIT 1").Scan(&eventID, &payload)
		if err != nil {
			return false
		}
		var n int
		_ = pool.QueryRow(ctx, "SELECT count(*) FROM processed_events WHERE event_id = $1", eventID).Scan(&n)
		return n == 1
	})

	// --- idempotent consumption: redeliver the identical envelope bytes
	// over the real bus (a genuine second delivery, not a simulated
	// one) and confirm processed_events still has exactly one row for
	// this event ID ---
	if err := testBus.Publish(ctx, "demo.widget_created", payload); err != nil {
		t.Fatalf("unexpected error republishing: %v", err)
	}

	// Give the consumer a moment to (not) double-apply it, then assert
	// the count never moves — there's no "it happened" signal to poll
	// for on a no-op, so this is a bounded wait, not a sleep standing in
	// for a missing precondition.
	time.Sleep(500 * time.Millisecond)
	var processedCount int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM processed_events WHERE event_id = $1", eventID).Scan(&processedCount); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if processedCount != 1 {
		t.Fatalf("expected exactly 1 processed_events row for %s after redelivery, got %d (redelivery must be a no-op)", eventID, processedCount)
	}
}
