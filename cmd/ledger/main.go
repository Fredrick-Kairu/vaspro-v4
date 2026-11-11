// Command ledger runs the ledger service standalone.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/ledger"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	svc, err := ledger.Bootstrap(ctx)
	if err != nil {
		slog.Error("ledger: bootstrap", "error", err)
		os.Exit(1)
	}

	if err := svc.Run(ctx); err != nil {
		slog.Error("ledger: exited with error", "error", err)
		os.Exit(1)
	}
}
