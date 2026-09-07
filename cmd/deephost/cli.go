package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/castlemilk/dns/internal/deephostcli/client"
	"github.com/castlemilk/dns/internal/deephostcli/config"
)

// defaultTimeout bounds one RPC. It matches the client package's own default,
// so the two never disagree about how long an operator waits.
const defaultTimeout = client.DefaultTimeout

// env is every boundary the CLI crosses: the three streams, the environment and
// the two seams a test replaces (the browser opener and the randomness the
// login flow draws from). A zero value is not usable; main builds the real one.
type env struct {
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
	getenv func(string) string

	// openBrowser opens the console approval screen. Nil means the real
	// desktop opener; a test supplies a fake console instead.
	openBrowser func(string) error

	// random draws the PKCE verifier and the login state. Nil means
	// crypto/rand.
	random io.Reader

	// now is the clock `deephost activity --since 2h` reads. Nil means
	// time.Now.
	now func() time.Time

	// version is what `deephost version` reports. Empty means the build info
	// embedded by the toolchain.
	version string
}

func (e *env) clock() time.Time {
	if e.now == nil {
		return time.Now()
	}
	return e.now()
}

// options are the flags every command accepts. They are registered on the
// top-level flag set and again on each command's, so `deephost --json domain
// list` and `deephost domain list --json` mean the same thing.
type options struct {
	apiURL     string
	consoleURL string
	token      string
	json       bool
	yes        bool
	timeout    time.Duration
}

func (o *options) register(fs *flag.FlagSet) {
	fs.StringVar(&o.apiURL, "api-url", o.apiURL, "control plane base URL (DEEPHOST_API_URL)")
	fs.StringVar(&o.token, "token", o.token, "operator credential (DEEPHOST_API_TOKEN)")
	fs.StringVar(&o.consoleURL, "console-url", o.consoleURL, "console base URL for `deephost auth login` (DEEPHOST_CONSOLE_URL)")
	fs.BoolVar(&o.json, "json", o.json, "print machine-readable JSON")
	fs.BoolVar(&o.yes, "yes", o.yes, "do not ask before a destructive change")
	fs.DurationVar(&o.timeout, "timeout", o.timeout, "per-request timeout")
}

// parse builds a command's flag set with the global flags already on it.
//
// The flag package's own output is discarded: its message is folded into a
// usageError instead, so that one place decides whether an operator sees a
// sentence or a JSON object.
func (o *options) parse(name, usage string, args []string, extra func(*flag.FlagSet)) (*flag.FlagSet, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	o.register(fs)
	if extra != nil {
		extra(fs)
	}
	if err := fs.Parse(permute(fs, args)); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return fs, &helpError{usage: usage}
		}
		return fs, &usageError{message: err.Error(), usage: usage}
	}
	if o.timeout <= 0 {
		return fs, &usageError{message: "--timeout: must be positive", usage: usage}
	}
	return fs, nil
}

// permute moves the flags in front of the positional arguments.
//
// The standard flag package stops at the first non-flag word, which would make
// `deephost record add example.com --type A` silently treat --type as a
// positional. Every other CLI an operator uses accepts flags after the subject,
// so the arguments are reordered here rather than the operator being made to
// remember. A `--` still ends the flags, and an unknown flag is passed through
// untouched so that flag.Parse reports it.
func permute(fs *flag.FlagSet, args []string) []string {
	flags := make([]string, 0, len(args))
	positional := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		argument := args[index]
		switch {
		case argument == "--":
			positional = append(positional, args[index+1:]...)
			index = len(args)
		case len(argument) > 1 && strings.HasPrefix(argument, "-"):
			name := strings.TrimLeft(argument, "-")
			if strings.Contains(name, "=") {
				flags = append(flags, argument)
				continue
			}
			// A non-boolean flag takes the next word with it; a boolean and an
			// unknown flag do not.
			if found := fs.Lookup(name); found != nil && !isBoolFlag(found) && index+1 < len(args) {
				flags = append(flags, argument, args[index+1])
				index++
				continue
			}
			flags = append(flags, argument)
		default:
			positional = append(positional, argument)
		}
	}
	if len(positional) == 0 {
		return flags
	}
	// The terminator keeps a positional that begins with a dash — a TXT record
	// value, say — from being read as a flag.
	return append(append(flags, "--"), positional...)
}

func isBoolFlag(found *flag.Flag) bool {
	boolean, ok := found.Value.(interface{ IsBoolFlag() bool })
	return ok && boolean.IsBoolFlag()
}

// run is the whole program: parse, dispatch, and turn whatever comes back into
// an exit code and one rendering of the failure.
func run(ctx context.Context, args []string, e *env) int {
	opts := &options{timeout: defaultTimeout}
	// Global flags may lead the command word. flag.Parse stops at the first
	// non-flag argument, so what is left is the command and its own flags.
	head := flag.NewFlagSet("deephost", flag.ContinueOnError)
	head.SetOutput(io.Discard)
	head.Usage = func() {}
	opts.register(head)
	if err := head.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return report(e, opts, &helpError{usage: rootUsage})
		}
		return report(e, opts, &usageError{message: err.Error(), usage: rootUsage})
	}
	rest := head.Args()

	if len(rest) == 0 {
		// `deephost` with no arguments is the branded greeting, not a mistake.
		return report(e, opts, greet(e, opts))
	}
	return report(e, opts, dispatch(ctx, e, opts, rest[0], rest[1:]))
}

func dispatch(ctx context.Context, e *env, o *options, command string, args []string) error {
	switch command {
	case "help", "-h", "--help":
		return &helpError{usage: rootUsage}
	case "auth":
		return cmdAuth(ctx, e, o, args)
	case "status":
		return cmdStatus(ctx, e, o, args)
	case "domain", "zone":
		return cmdDomain(ctx, e, o, args)
	case "record":
		return cmdRecord(ctx, e, o, args)
	case "site":
		return cmdSite(ctx, e, o, args)
	case "mail":
		return cmdMail(ctx, e, o, args)
	case "billing":
		return cmdBilling(ctx, e, o, args)
	case "activity":
		return cmdActivity(ctx, e, o, args)
	case "platform":
		return cmdPlatform(ctx, e, o, args)
	case "version":
		return cmdVersion(e, o, args)
	default:
		return &usageError{message: fmt.Sprintf("unknown command %q", command), usage: rootUsage}
	}
}

// greet prints the banner and the command list. It is what `deephost` alone does.
func greet(e *env, o *options) error {
	if o.json {
		return newPrinter(e, o).emit(map[string]any{"usage": rootUsage}, nil)
	}
	if err := writeBanner(e); err != nil {
		return err
	}
	_, err := fmt.Fprintf(e.stdout, "\n%s\n", rootUsage)
	return err
}

// report renders err and returns the process exit code.
func report(e *env, o *options, err error) int {
	var help *helpError
	if errors.As(err, &help) {
		// --help is an answer, not a failure: it goes to stdout and exits 0,
		// as a document when the caller asked for documents.
		if o.json {
			if writeErr := writeJSONTo(e.stdout, map[string]any{"usage": help.usage}); writeErr != nil {
				return codeError
			}
			return codeOK
		}
		if _, writeErr := fmt.Fprintln(e.stdout, help.usage); writeErr != nil {
			return codeError
		}
		return codeOK
	}
	if err == nil {
		return codeOK
	}

	var usage *usageError
	code := codeError
	if errors.As(err, &usage) {
		code = codeUsage
	}
	if o.json {
		// A machine reads the failure as JSON on stderr, so stdout carries
		// only the document a successful command would have written.
		if writeErr := writeJSONTo(e.stderr, map[string]any{"error": err.Error()}); writeErr != nil {
			return codeError
		}
		return code
	}
	if _, writeErr := fmt.Fprintf(e.stderr, "deephost: %v\n", err); writeErr != nil {
		return codeError
	}
	if usage != nil && usage.usage != "" {
		if _, writeErr := fmt.Fprintln(e.stderr, "\n"+usage.usage); writeErr != nil {
			return codeError
		}
	}
	return code
}

// The three exit codes the CLI promises: nothing else is ever returned.
const (
	codeOK    = 0
	codeError = 1
	codeUsage = 2
)

// usageError is a mistake in what was asked for, and is the only error that
// exits 2.
type usageError struct {
	message string
	usage   string
}

func (e *usageError) Error() string { return e.message }

// usagef reports a misuse without a usage block, for the cases where the
// command was found but its arguments were wrong.
func usagef(format string, args ...any) error {
	return &usageError{message: fmt.Sprintf(format, args...)}
}

// helpError is --help: not a failure, so it prints to stdout and exits 0.
type helpError struct{ usage string }

func (e *helpError) Error() string { return "help requested" }

// rpcError is a control-plane failure rendered as the sentence
// client.ExplainFor produced, with the original kept underneath so callers can
// still ask client.IsUnauthenticated about it.
type rpcError struct {
	message string
	cause   error
}

func (e *rpcError) Error() string { return e.message }
func (e *rpcError) Unwrap() error { return e.cause }

// fail turns a Connect failure into the sentence an operator acts on. The
// control plane's own "field: message" validation text passes through
// unchanged, because it already names the field.
func fail(clients *client.Clients, err error) error {
	target := ""
	if clients != nil {
		target = clients.BaseURL
	}
	return &rpcError{message: client.ExplainFor(target, err), cause: err}
}

// settings resolves flag > environment > file. A config path that cannot even
// be located (no HOME) is not fatal here: a caller that passed --token and
// --api-url needs no file at all, and the paths that do need one ask for it
// explicitly.
func (o *options) settings(e *env) (config.Resolved, string, error) {
	path, pathErr := config.Path(e.getenv)
	var stored config.Config
	if pathErr == nil {
		loaded, err := config.Load(path)
		if err != nil {
			return config.Resolved{}, path, err
		}
		stored = loaded
	}
	flags := config.Flags{APIURL: o.apiURL, Token: o.token}
	return config.Resolve(flags, e.getenv, stored), path, nil
}

// configPath is the settings file's location, for the commands that must write
// or delete it and so cannot carry on without one.
func (o *options) configPath(e *env) (string, error) {
	return config.Path(e.getenv)
}

// connect builds the six clients for this invocation.
func (o *options) connect(e *env) (*client.Clients, error) {
	resolved, _, err := o.settings(e)
	if err != nil {
		return nil, err
	}
	base, err := resolved.Validate()
	if err != nil {
		return nil, err
	}
	return client.New(client.Options{BaseURL: base, Token: resolved.Token, Timeout: o.timeout})
}

// call bounds one RPC by --timeout.
func (o *options) call(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, o.timeout)
}

// confirm asks before a destructive change. --yes answers it, and --json
// refuses to ask at all: a machine-readable run must never block on a prompt.
func confirm(e *env, o *options, question string) error {
	if o.yes {
		return nil
	}
	if o.json {
		return usagef("this command destroys data: pass --yes to confirm it in --json mode")
	}
	if _, err := fmt.Fprintf(e.stderr, "%s [y/N]: ", question); err != nil {
		return err
	}
	line, err := bufio.NewReader(e.stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return nil
	default:
		return errors.New("cancelled; nothing was changed")
	}
}

// argument reads the nth positional argument, reporting a missing one as a
// usage failure that names the field.
func argument(fs *flag.FlagSet, index int, field, usage string) (string, error) {
	if fs.NArg() <= index {
		return "", &usageError{message: field + ": is required", usage: usage}
	}
	value := strings.TrimSpace(fs.Arg(index))
	if value == "" {
		return "", &usageError{message: field + ": is required", usage: usage}
	}
	return value, nil
}

// noExtraArgs refuses trailing arguments rather than ignoring them, so a
// mistyped flag is never silently dropped.
func noExtraArgs(fs *flag.FlagSet, allowed int, usage string) error {
	if fs.NArg() > allowed {
		return &usageError{
			message: fmt.Sprintf("unexpected argument %q", fs.Arg(allowed)),
			usage:   usage,
		}
	}
	return nil
}
