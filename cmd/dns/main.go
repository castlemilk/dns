package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/castlemilk/dns/internal/app"
	"github.com/castlemilk/dns/internal/config"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load(os.Getenv)
	if err != nil {
		logger.Error("load configuration", "error", err)
		os.Exit(1)
	}
	if err := app.Run(ctx, cfg, logger); err != nil {
		logger.Error("run service", "error", err)
		os.Exit(1)
	}
}
