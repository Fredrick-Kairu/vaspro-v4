// Command gateway runs the API gateway standalone.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/gateway"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	svc, err := gateway.Bootstrap(ctx)
	if err != nil {
		slog.Error("gateway: bootstrap", "error", err)
		os.Exit(1)
	}

	if err := svc.Run(ctx); err != nil {
		slog.Error("gateway: exited with error", "error", err)
		os.Exit(1)
	}
}
