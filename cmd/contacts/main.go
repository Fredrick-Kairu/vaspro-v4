// Command contacts runs the contacts service standalone.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/contacts"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	svc, err := contacts.Bootstrap(ctx)
	if err != nil {
		slog.Error("contacts: bootstrap", "error", err)
		os.Exit(1)
	}

	if err := svc.Run(ctx); err != nil {
		slog.Error("contacts: exited with error", "error", err)
		os.Exit(1)
	}
}
