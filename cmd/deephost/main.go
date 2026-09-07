// Command deephost is the operator CLI for the Deep Hosting platform: every operation
// the console can perform, without a browser.
//
// It talks to the control plane's six Connect services through
// internal/deephostcli/client, keeps its settings in ~/.config/deephost/config.json
// through internal/deephostcli/config, and gets its credential either from
// `deephost auth login` (internal/deephostcli/auth) or from --token /
// DEEPHOST_API_TOKEN.
//
// Exit codes are 0 for success, 1 for a failure and 2 for a usage mistake, so a
// script can tell "you asked for the wrong thing" from "the thing you asked for
// went wrong".
package main

import (
	"context"
	"os"
	"os/signal"
)

func main() {
	// A Ctrl-C cancels the in-flight RPC rather than killing the process
	// mid-write, so a half-finished export or config write cannot be left
	// behind.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	os.Exit(run(ctx, os.Args[1:], &env{
		stdin:  os.Stdin,
		stdout: os.Stdout,
		stderr: os.Stderr,
		getenv: os.Getenv,
	}))
}
