// Package server runs an http.Server with graceful shutdown: every
// service's Run(ctx) ends this way, so a SIGTERM lets in-flight requests
// finish instead of dropping them.
package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

// Serve runs srv over ln until ctx is cancelled, then calls srv.Shutdown
// with a bounded timeout. Takes a caller-supplied net.Listener (rather
// than binding srv.Addr itself) so a caller — and this package's own
// tests — can learn the real address before Serve blocks, which matters
// most when Addr is ":0".
func Serve(ctx context.Context, srv *http.Server, ln net.Listener, shutdownTimeout time.Duration) error {
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.Serve(ln)
	}()

	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("server: %w", err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("server: shutting down: %w", err)
		}
		return nil
	}
}
