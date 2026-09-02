package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr, runDependencies{}); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "vultr-dns-lb:", err)
		os.Exit(1)
	}
}
