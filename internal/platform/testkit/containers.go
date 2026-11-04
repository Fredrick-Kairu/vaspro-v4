//go:build integration

package testkit

import (
	"context"
	"log"
	"testing"

	"github.com/testcontainers/testcontainers-go"
)

// startAttempts is how many containers a Start* helper tries before
// failing the test. Under a full integration run, Docker Desktop
// occasionally never brings up one container's host port (seen
// 2026-11-03: 555 checks over 60s, never reachable); a fresh container
// almost always works, while waiting longer never does.
const startAttempts = 3

// startContainer runs start, which starts one container and proves it's
// usable, through startWithRetries: a container that never becomes
// ready is torn down and a fresh one tried. The working container is
// torn down when the test ends.
func startContainer[T any](t *testing.T, what string, start func(context.Context) (T, testcontainers.Container, error)) T {
	t.Helper()
	// Retries go to the standard log, not t.Logf: Go only prints a
	// passing test's t.Logf output under -v, and a retry that saved a
	// run should still be visible in it.
	value, terminate, err := startWithRetries(context.Background(), log.Printf, what, startAttempts, func(ctx context.Context) (T, func(), error) {
		v, ctr, err := start(ctx)
		return v, func() { _ = testcontainers.TerminateContainer(ctr) }, err
	})
	if err != nil {
		t.Fatalf("testkit: %v", err)
	}
	t.Cleanup(terminate)
	return value
}
