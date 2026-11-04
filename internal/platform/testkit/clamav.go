//go:build integration

package testkit

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// clamavImage is ClamAV with its signature database built in. The
// "-debian" variant is the multi-architecture one (the plain tag has no
// arm64 build).
const clamavImage = "clamav/clamav:1.5.4-debian"

// StartClamAV starts real ClamAV and returns clamd's host:port once it
// answers PING. It needs about 1 GB of memory and a minute or more to
// load its signatures, so use it sparingly: other tests use
// tools/fakes/clamd, which speaks the same protocol.
func StartClamAV(t *testing.T) string {
	t.Helper()
	return startContainer(t, "clamav", func(ctx context.Context) (string, testcontainers.Container, error) {
		ctr, err := testcontainers.Run(ctx, clamavImage,
			testcontainers.WithEnv(map[string]string{"CLAMAV_NO_FRESHCLAMD": "true", "CLAMAV_NO_MILTERD": "true"}),
			testcontainers.WithExposedPorts("3310/tcp"),
			testcontainers.WithWaitStrategy(wait.ForListeningPort("3310/tcp").WithStartupTimeout(5*time.Minute)),
		)
		if err != nil {
			return "", ctr, fmt.Errorf("starting clamav: %w", err)
		}
		addr, err := ctr.PortEndpoint(ctx, "3310/tcp", "")
		if err != nil {
			return "", ctr, fmt.Errorf("clamav endpoint: %w", err)
		}
		return addr, ctr, waitUntilReady(ctx, "clamav", 5*time.Minute, time.Second, func(ctx context.Context) error {
			return clamdPing(ctx, addr)
		})
	})
}

func clamdPing(ctx context.Context, addr string) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	if _, err := conn.Write([]byte("zPING\x00")); err != nil {
		return err
	}
	reply, err := bufio.NewReader(conn).ReadString(0)
	if err != nil {
		return err
	}
	if reply != "PONG\x00" {
		return fmt.Errorf("clamd answered %q", reply)
	}
	return nil
}
