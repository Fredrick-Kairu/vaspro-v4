// Command vaspro-all runs every VASPRO service in one process — the
// default `make run` target, used locally and in CI (ROADMAP §2.1).
// Each `make new-service` run (tools/scaffold) adds one import and one
// registration block between the sentinel comments below; nothing else
// in this file should need to change as services are added.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/sync/errgroup"
	// vaspro-all:imports-begin
	// vaspro-all:imports-end
)

// runner is anything cmd/vaspro-all can run and gracefully stop.
type runner interface {
	Run(ctx context.Context) error
}

func main() {
	logger := slog.Default()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var services []runner
	// vaspro-all:services-begin
	// vaspro-all:services-end

	if len(services) == 0 {
		logger.InfoContext(ctx, "vaspro-all: no services registered yet; idling until shutdown")
	}

	g, gctx := errgroup.WithContext(ctx)
	for _, svc := range services { //nolint:gosec // services is a plain slice; ranging over it empty or nil is always safe, this isn't an index access
		g.Go(func() error { return svc.Run(gctx) })
	}
	g.Go(func() error {
		<-gctx.Done()
		return nil
	})

	if err := g.Wait(); err != nil {
		logger.Error("vaspro-all: exited with error", "error", err)
		os.Exit(1)
	}
}
