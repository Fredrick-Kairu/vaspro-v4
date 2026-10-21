// Command identity runs the identity service standalone.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	svc, err := identity.Bootstrap(ctx)
	if err != nil {
		slog.Error("identity: bootstrap", "error", err)
		os.Exit(1)
	}

	if err := svc.Run(ctx); err != nil {
		slog.Error("identity: exited with error", "error", err)
		os.Exit(1)
	}
}
