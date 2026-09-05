// Command simple-mcp serves the `simple` platform over the Model Context
// Protocol on stdin and stdout, so an assistant can do what the `simple` CLI
// does: read status, manage domains and records, attach and deploy sites,
// manage mail, read billing and read the activity log.
//
// Two things about this program's edges are deliberate.
//
// stdout is the protocol. Nothing else is ever written there — no banner, no
// progress, no log line. Everything for a human goes to stderr.
//
// There is no --token flag. A command line is readable by every process on the
// machine, so the operator credential is taken from the settings file
// "simple auth login" writes, or from SIMPLE_API_TOKEN in this process's
// environment, where an MCP client's own configuration can put it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strings"

	"github.com/castlemilk/dns/internal/simplecli/config"
	"github.com/castlemilk/dns/internal/simplemcp"
)

// Exit codes match the CLI: 0 ok, 1 error, 2 usage.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdin, os.Stdout, os.Stderr))
}

func run(arguments []string, getenv func(string) string, stdin io.Reader, stdout, stderr io.Writer) int {
	if getenv == nil {
		getenv = os.Getenv
	}
	flags := flag.NewFlagSet("simple-mcp", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiURL := flags.String("api-url", "", "control plane base URL (default "+config.EnvAPIURL+", then the settings file, then "+config.DefaultAPIURL+")")
	timeout := flags.Duration("timeout", 0, "how long one request to the control plane may take")
	showVersion := flags.Bool("version", false, "print the version and exit")
	flags.Usage = func() {
		fprintln(stderr, usage)
		flags.PrintDefaults()
	}
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if flags.NArg() != 0 {
		flags.Usage()
		fprintf(stderr, "simple-mcp: positional arguments are not accepted\n")
		return exitUsage
	}
	if *timeout < 0 {
		fprintf(stderr, "simple-mcp: timeout: must not be negative\n")
		return exitUsage
	}
	if *showVersion {
		fprintf(stdout, "simple-mcp %s\n", version())
		return exitOK
	}

	stored, note := load(getenv)
	resolved := config.Resolve(config.Flags{APIURL: *apiURL}, getenv, stored)
	server := simplemcp.New(simplemcp.Options{
		BaseURL:        resolved.APIURL,
		Token:          resolved.Token,
		Version:        version(),
		Timeout:        *timeout,
		CredentialNote: note,
	})
	announce(stderr, server, resolved, note)

	if err := server.Serve(context.Background(), stdin, stdout); err != nil {
		if errors.Is(err, context.Canceled) {
			return exitOK
		}
		fprintf(stderr, "simple-mcp: %v\n", err)
		return exitError
	}
	return exitOK
}

// load reads the settings file. A file that cannot be read is not fatal: the
// server still starts and still serves the read-only tools, and the reason
// travels with the refusal a mutating tool gives, so the operator is told to
// fix the file rather than to log in again.
func load(getenv func(string) string) (config.Config, string) {
	path, err := config.Path(getenv)
	if err != nil {
		return config.Config{}, "The settings file could not be located: " + err.Error()
	}
	stored, err := config.Load(path)
	if err != nil {
		return config.Config{}, "The settings file could not be read: " + err.Error()
	}
	return stored, ""
}

// announce writes the one-line startup report. It names the control plane and
// says whether a credential is in hand; it never renders the credential, and
// it writes to stderr because stdout is the protocol.
func announce(stderr io.Writer, server *simplemcp.Server, resolved config.Resolved, note string) {
	credential := "no operator credential: read-only tools only"
	if resolved.Token != "" {
		credential = fmt.Sprintf("credential from %s", resolved.TokenFrom)
	}
	fprintf(stderr, "simple-mcp %s: serving %s (%s)\n", version(), server.Target(), credential)
	if note != "" {
		fprintf(stderr, "simple-mcp: %s\n", note)
	}
}

func version() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	if v := strings.TrimSpace(info.Main.Version); v != "" && v != "(devel)" {
		return v
	}
	return "dev"
}

// fprintf and fprintln drop write failures deliberately: stderr being closed is
// not a reason to stop serving the protocol on stdout, and there is nowhere
// left to report it anyway.
func fprintf(w io.Writer, format string, values ...any) {
	ignore2(fmt.Fprintf(w, format, values...))
}

func fprintln(w io.Writer, value string) {
	ignore2(fmt.Fprintln(w, value))
}

// ignore2 drops the (n, err) pair a Fprint returns. The repository's errcheck
// rejects the `_, _ =` form, and a named call makes the decision reviewable.
func ignore2(int, error) {}

const usage = `simple-mcp serves the simple platform over the Model Context Protocol.

usage:
  simple-mcp [--api-url URL] [--timeout DURATION]
  simple-mcp --version

It speaks newline-delimited JSON-RPC on stdin and stdout and is meant to be
started by an MCP client, not by hand. Register it with:

  claude mcp add simple -- simple-mcp

The control plane comes from --api-url, then ` + config.EnvAPIURL + `, then the settings
file, then ` + config.DefaultAPIURL + `. The operator credential comes from ` + config.EnvToken + `
or from the settings file "simple auth login" writes; there is no flag for it,
because a command line is readable by every process on the machine.

Read-only tools run without a credential. Tools that change something refuse
without one, and tools that destroy something also require confirm: true.

flags:`
