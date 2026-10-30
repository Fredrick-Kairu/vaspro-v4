// Command accounts runs the accounts service standalone.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/accounts"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	svc, err := accounts.Bootstrap(ctx)
	if err != nil {
		slog.Error("accounts: bootstrap", "error", err)
		os.Exit(1)
	}

	if err := svc.Run(ctx); err != nil {
		slog.Error("accounts: exited with error", "error", err)
		os.Exit(1)
	}
}
