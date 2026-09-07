package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/castlemilk/dns/internal/maild"
)

// Exit codes. They are what a supervisor and a script read, so they are worth
// keeping distinct: 2 means "the invocation is wrong and retrying it will fail
// the same way", which is the one a restart loop should not retry.
const (
	codeOK    = 0
	codeError = 1
	codeUsage = 2
)

const usage = `usage: deephost-mail [flags]

The Deep Hosting mail server. It serves the management API the control plane
drives, the SMTP listeners, and POP3.

identity (both are required, and both must match the control plane):
  --hostname NAME          what this server calls itself; must equal the
                           control plane's MAIL_HOSTNAME (MAILD_HOSTNAME)
  --admin-token TOKEN      the bearer the management API requires; must equal
                           the control plane's MAIL_API_TOKEN, at least 32
                           characters (MAILD_ADMIN_TOKEN)
  --admin-token-file PATH  read the token from a file instead, which keeps it
                           out of the process table (MAILD_ADMIN_TOKEN_FILE)

storage:
  --data-dir DIR           the store and the mail spool (MAILD_DATA_DIR,
                           default /var/lib/deephost-mail)

listeners (an empty address disables that listener):
  --api-addr ADDR          management API (MAILD_API_ADDR, default 127.0.0.1:20080)
  --api-tls                serve the management API over TLS (MAILD_API_TLS)
  --path-prefix PREFIX     mount the management API under a prefix, when
                           MAIL_API_URL carries one (MAILD_PATH_PREFIX)
  --smtp-addr ADDR         inbound mail (MAILD_SMTP_ADDR, default :25)
  --submission-addr ADDR   submission, STARTTLS (MAILD_SUBMISSION_ADDR, default :587)
  --submissions-addr ADDR  submission, implicit TLS (MAILD_SUBMISSIONS_ADDR, default :465)
  --pop3-addr ADDR         POP3, STLS (MAILD_POP3_ADDR, default :110)
  --pop3s-addr ADDR        POP3, implicit TLS (MAILD_POP3S_ADDR, default :995)

TLS (both or neither; without them the implicit-TLS ports do not start and
STARTTLS cannot be offered):
  --tls-cert PATH          certificate chain, PEM (MAILD_TLS_CERT)
  --tls-key PATH           private key, PEM (MAILD_TLS_KEY)

bounds (one set, shared by every listener: the same budget locks out a password
guesser whichever port they use):
  --max-sessions N         concurrent line protocol connections (MAILD_MAX_SESSIONS, default 256)
  --max-sessions-per-ip N  one peer's share of that (MAILD_MAX_SESSIONS_PER_IP, default 16)
  --max-message-bytes N    largest message accepted, announced in EHLO SIZE
                           (MAILD_MAX_MESSAGE_BYTES, default 26214400)
  --max-message-memory N   ceiling on the message bodies held at once
                           (MAILD_MAX_MESSAGE_MEMORY, default 268435456)
  --session-timeout D      idle timeout before a handler's first read (default 5m)
  --shutdown-grace D       how long a stop waits for sessions in flight (default 20s)

outbound delivery (the queue runner and the relay that drains it; without a
resolver this server cannot send at all and refuses to start):
  --dns-server LIST        comma-separated recursive resolvers, host or host:port
                           (MAILD_DNS_SERVERS, default /etc/resolv.conf)
  --dns-timeout D          bound on one MX lookup (MAILD_DNS_TIMEOUT, default 5s)
  --relay-port PORT        port every MX is dialled on (MAILD_RELAY_PORT, default 25)
  --relay-connect-timeout D  bound on one outbound TCP connect (default 30s)
  --relay-command-timeout D  bound on one outbound SMTP command (default 5m)
  --relay-data-timeout D     bound on sending one message body (default 10m)
  --relay-no-tls           never offer STARTTLS outbound; for a peer whose TLS
                           is broken (MAILD_RELAY_NO_TLS)
  --queue-interval D       how often the runner looks for due mail
                           (MAILD_QUEUE_INTERVAL, default 1m; a freshly queued
                           message does not wait, it is sent at once)
  --queue-base-backoff D   wait after the first temporary failure (default 5m)
  --queue-max-backoff D    cap on the doubling (default 2h)
  --queue-lifetime D       how long a message may keep failing before it bounces
                           (MAILD_QUEUE_LIFETIME, default 120h)
  --queue-concurrency N    concurrent outbound sessions (MAILD_QUEUE_CONCURRENCY, default 8)

other:
  --log-format text|json   log format (MAILD_LOG_FORMAT, default text)
  --log-level LEVEL        debug, info, warn or error (MAILD_LOG_LEVEL, default info)
  --validate               check the configuration and exit without binding
  --version                print the version and exit

exit codes: 0 clean stop, 1 error, 2 usage`

// options is the parsed invocation, before it becomes a maild.Config.
type options struct {
	hostname  string
	token     string
	tokenFile string
	dataDir   string

	apiAddr     string
	apiTLS      bool
	pathPrefix  string
	smtpAddr    string
	submission  string
	submissions string
	pop3        string
	pop3s       string

	tlsCert string
	tlsKey  string

	maxSessions      int
	maxSessionsPerIP int
	maxMessageBytes  int64
	maxMessageMemory int64
	sessionTimeout   time.Duration
	shutdownGrace    time.Duration

	dnsServers string
	dnsTimeout time.Duration

	relayPort       string
	relayConnect    time.Duration
	relayCommand    time.Duration
	relayData       time.Duration
	relayDisableTLS bool

	queueInterval    time.Duration
	queueBaseBackoff time.Duration
	queueMaxBackoff  time.Duration
	queueLifetime    time.Duration
	queueConcurrency int

	logFormat string
	logLevel  string

	validate    bool
	wantVersion bool
}

// run is main with its environment injected, so that every path through it is
// reachable from a test.
func run(ctx context.Context, args []string, lookupEnv func(string) (string, bool), stdout, stderr io.Writer) int {
	options, err := parse(args, lookupEnv, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			say(stdout, "%s\n", usage)
			return codeOK
		}
		say(stderr, "deephost-mail: %v\n\n%s\n", err, usage)
		return codeUsage
	}
	if options.wantVersion {
		say(stdout, "deephost-mail %s %s %s/%s\n",
			version(), runtime.Version(), runtime.GOOS, runtime.GOARCH)
		return codeOK
	}

	token, err := options.resolveToken()
	if err != nil {
		say(stderr, "deephost-mail: %v\n", err)
		return codeUsage
	}
	config, err := options.config(token)
	if err != nil {
		say(stderr, "deephost-mail: %v\n", err)
		return codeUsage
	}

	// Validate before anything is opened or bound. The two refusals that matter
	// — an empty hostname and a short admin token — are configuration mistakes
	// whose consequences are otherwise invisible, so they stop the process here
	// with the operator still watching rather than degrading a customer's mail
	// binding hours later.
	if err := config.Validate(); err != nil {
		say(stderr, "deephost-mail: %v\n", err)
		return codeUsage
	}
	if options.validate {
		say(stdout, "deephost-mail: the configuration is valid for %s\n", config.Hostname)
		return codeOK
	}

	config.Logger, err = newLogger(options, stderr)
	if err != nil {
		say(stderr, "deephost-mail: %v\n", err)
		return codeUsage
	}
	config.Services = services(options.pathPrefix, options.outbound())

	server, err := maild.NewServer(config)
	if err != nil {
		say(stderr, "deephost-mail: %v\n", err)
		return codeError
	}
	if err := server.Run(ctx); err != nil {
		say(stderr, "deephost-mail: %v\n", err)
		return codeError
	}
	return codeOK
}

// parse reads the flags, each defaulting to its environment variable and then to
// a built-in default, so that a chart can set everything by environment and an
// operator can override any of it on the command line.
func parse(args []string, lookupEnv func(string) (string, bool), stderr io.Writer) (*options, error) {
	options := &options{}
	flags := flag.NewFlagSet("deephost-mail", flag.ContinueOnError)
	flags.SetOutput(stderr)
	// The usage block is printed by run, in one place, so that a flag error and
	// --help produce the same document.
	flags.Usage = func() {}

	text := func(target *string, name, env, fallback string) {
		flags.StringVar(target, name, envOr(lookupEnv, env, fallback), "")
	}
	text(&options.hostname, "hostname", "MAILD_HOSTNAME", "")
	text(&options.token, "admin-token", "MAILD_ADMIN_TOKEN", "")
	text(&options.tokenFile, "admin-token-file", "MAILD_ADMIN_TOKEN_FILE", "")
	text(&options.dataDir, "data-dir", "MAILD_DATA_DIR", "/var/lib/deephost-mail")
	text(&options.apiAddr, "api-addr", "MAILD_API_ADDR", "127.0.0.1:20080")
	text(&options.pathPrefix, "path-prefix", "MAILD_PATH_PREFIX", "")
	text(&options.smtpAddr, "smtp-addr", "MAILD_SMTP_ADDR", ":25")
	text(&options.submission, "submission-addr", "MAILD_SUBMISSION_ADDR", ":587")
	text(&options.submissions, "submissions-addr", "MAILD_SUBMISSIONS_ADDR", ":465")
	text(&options.pop3, "pop3-addr", "MAILD_POP3_ADDR", ":110")
	text(&options.pop3s, "pop3s-addr", "MAILD_POP3S_ADDR", ":995")
	text(&options.tlsCert, "tls-cert", "MAILD_TLS_CERT", "")
	text(&options.tlsKey, "tls-key", "MAILD_TLS_KEY", "")
	text(&options.dnsServers, "dns-server", "MAILD_DNS_SERVERS", "")
	text(&options.relayPort, "relay-port", "MAILD_RELAY_PORT", maild.DefaultRelayPort)
	text(&options.logFormat, "log-format", "MAILD_LOG_FORMAT", "text")
	text(&options.logLevel, "log-level", "MAILD_LOG_LEVEL", "info")

	apiTLS, err := envBool(lookupEnv, "MAILD_API_TLS")
	if err != nil {
		return nil, err
	}
	flags.BoolVar(&options.apiTLS, "api-tls", apiTLS, "")

	relayNoTLS, err := envBool(lookupEnv, "MAILD_RELAY_NO_TLS")
	if err != nil {
		return nil, err
	}
	flags.BoolVar(&options.relayDisableTLS, "relay-no-tls", relayNoTLS, "")

	// number and span read the environment first and then let a flag override
	// it, which is the pattern text() already established for strings. Every
	// one of them is checked for a positive value below rather than here, so
	// that one refusal names the flag whichever way the value arrived.
	number := func(target *int, name, env string, fallback int) error {
		value, err := envInt(lookupEnv, env, fallback)
		if err != nil {
			return err
		}
		flags.IntVar(target, name, value, "")
		return nil
	}
	number64 := func(target *int64, name, env string, fallback int64) error {
		value, err := envInt64(lookupEnv, env, fallback)
		if err != nil {
			return err
		}
		flags.Int64Var(target, name, value, "")
		return nil
	}
	span := func(target *time.Duration, name, env string, fallback time.Duration) error {
		value, err := envDuration(lookupEnv, env, fallback)
		if err != nil {
			return err
		}
		flags.DurationVar(target, name, value, "")
		return nil
	}
	for _, bind := range []func() error{
		func() error { return number(&options.maxSessions, "max-sessions", "MAILD_MAX_SESSIONS", 256) },
		func() error {
			return number(&options.maxSessionsPerIP, "max-sessions-per-ip", "MAILD_MAX_SESSIONS_PER_IP", 16)
		},
		func() error {
			return number64(&options.maxMessageBytes, "max-message-bytes", "MAILD_MAX_MESSAGE_BYTES",
				maild.DefaultSMTPMaxMessageBytes)
		},
		func() error {
			return number64(&options.maxMessageMemory, "max-message-memory", "MAILD_MAX_MESSAGE_MEMORY", 256<<20)
		},
		func() error {
			return number(&options.queueConcurrency, "queue-concurrency", "MAILD_QUEUE_CONCURRENCY",
				maild.DefaultQueueConcurrency)
		},
		func() error {
			return span(&options.sessionTimeout, "session-timeout", "MAILD_SESSION_TIMEOUT", 5*time.Minute)
		},
		func() error {
			return span(&options.shutdownGrace, "shutdown-grace", "MAILD_SHUTDOWN_GRACE", 20*time.Second)
		},
		func() error {
			return span(&options.dnsTimeout, "dns-timeout", "MAILD_DNS_TIMEOUT", maild.DefaultResolverTimeout)
		},
		func() error {
			return span(&options.relayConnect, "relay-connect-timeout", "MAILD_RELAY_CONNECT_TIMEOUT",
				maild.DefaultRelayConnectTimeout)
		},
		func() error {
			return span(&options.relayCommand, "relay-command-timeout", "MAILD_RELAY_COMMAND_TIMEOUT",
				maild.DefaultRelayCommandTimeout)
		},
		func() error {
			return span(&options.relayData, "relay-data-timeout", "MAILD_RELAY_DATA_TIMEOUT",
				maild.DefaultRelayDataTimeout)
		},
		func() error {
			return span(&options.queueInterval, "queue-interval", "MAILD_QUEUE_INTERVAL", maild.DefaultQueueInterval)
		},
		func() error {
			return span(&options.queueBaseBackoff, "queue-base-backoff", "MAILD_QUEUE_BASE_BACKOFF",
				maild.DefaultQueueBaseBackoff)
		},
		func() error {
			return span(&options.queueMaxBackoff, "queue-max-backoff", "MAILD_QUEUE_MAX_BACKOFF",
				maild.DefaultQueueMaxBackoff)
		},
		func() error {
			return span(&options.queueLifetime, "queue-lifetime", "MAILD_QUEUE_LIFETIME", maild.DefaultQueueLifetime)
		},
	} {
		if err := bind(); err != nil {
			return nil, err
		}
	}

	flags.BoolVar(&options.validate, "validate", false, "")
	flags.BoolVar(&options.wantVersion, "version", false, "")

	if err := flags.Parse(args); err != nil {
		return nil, err
	}
	if flags.NArg() > 0 {
		return nil, fmt.Errorf("unexpected argument %q: this command takes flags only", flags.Arg(0))
	}
	return options, options.check()
}

// check refuses the values that would otherwise be applied as a silent default.
// A zero or negative bound is always a mistake — every one of these has a
// working default, so nobody asks for zero on purpose — and applying the
// default instead would leave an operator believing they had turned something
// off or turned it up.
func (o *options) check() error {
	for _, bound := range []struct {
		flag  string
		value int64
	}{
		{"--max-sessions", int64(o.maxSessions)},
		{"--max-sessions-per-ip", int64(o.maxSessionsPerIP)},
		{"--max-message-bytes", o.maxMessageBytes},
		{"--max-message-memory", o.maxMessageMemory},
		{"--queue-concurrency", int64(o.queueConcurrency)},
		{"--session-timeout", int64(o.sessionTimeout)},
		{"--shutdown-grace", int64(o.shutdownGrace)},
		{"--dns-timeout", int64(o.dnsTimeout)},
		{"--relay-connect-timeout", int64(o.relayConnect)},
		{"--relay-command-timeout", int64(o.relayCommand)},
		{"--relay-data-timeout", int64(o.relayData)},
		{"--queue-interval", int64(o.queueInterval)},
		{"--queue-base-backoff", int64(o.queueBaseBackoff)},
		{"--queue-max-backoff", int64(o.queueMaxBackoff)},
		{"--queue-lifetime", int64(o.queueLifetime)},
	} {
		if bound.value <= 0 {
			return errors.New(bound.flag + ": must be positive")
		}
	}
	return nil
}

// outbound is the sending half of the invocation, in the shape services.go
// takes it.
func (o *options) outbound() outbound {
	return outbound{
		dnsServers:       splitList(o.dnsServers),
		dnsTimeout:       o.dnsTimeout,
		relayPort:        strings.TrimSpace(o.relayPort),
		relayConnect:     o.relayConnect,
		relayCommand:     o.relayCommand,
		relayData:        o.relayData,
		relayDisableTLS:  o.relayDisableTLS,
		queueInterval:    o.queueInterval,
		queueBaseBackoff: o.queueBaseBackoff,
		queueMaxBackoff:  o.queueMaxBackoff,
		queueLifetime:    o.queueLifetime,
		queueConcurrency: o.queueConcurrency,
	}
}

// splitList reads a comma-separated flag, dropping the empty fields a trailing
// comma or a rendered-but-unset chart value leaves behind.
func splitList(value string) []string {
	var fields []string
	for _, field := range strings.Split(value, ",") {
		if trimmed := strings.TrimSpace(field); trimmed != "" {
			fields = append(fields, trimmed)
		}
	}
	return fields
}

// resolveToken reads the admin token. A file is offered because a token on the
// command line is visible in the process table to every other process on the
// host, and a token in the environment is visible in /proc and in a crash dump.
func (o *options) resolveToken() (maild.Secret, error) {
	if o.token != "" && o.tokenFile != "" {
		return nil, errors.New("--admin-token and --admin-token-file: choose one")
	}
	if o.tokenFile == "" {
		return maild.Secret(o.token), nil
	}
	raw, err := os.ReadFile(o.tokenFile)
	if err != nil {
		// The error names the file, never the contents.
		return nil, fmt.Errorf("--admin-token-file: %w", err)
	}
	// A trailing newline is what every way of writing a secret to a file
	// produces, and a token with one would silently never match.
	return maild.Secret(strings.TrimRight(string(raw), "\r\n")), nil
}

// config turns the invocation into the server's configuration.
func (o *options) config(token maild.Secret) (maild.Config, error) {
	if o.apiTLS && o.tlsCert == "" {
		// Validate catches this too; saying it here names the flag.
		return maild.Config{}, errors.New("--api-tls: needs --tls-cert and --tls-key")
	}
	return maild.Config{
		Hostname:        strings.TrimSpace(o.hostname),
		AdminToken:      token,
		DataDir:         o.dataDir,
		APIAddr:         o.apiAddr,
		APITLS:          o.apiTLS,
		SMTPAddr:        o.smtpAddr,
		SubmissionAddr:  o.submission,
		SubmissionsAddr: o.submissions,
		POP3Addr:        o.pop3,
		POP3SAddr:       o.pop3s,
		TLSCertFile:     o.tlsCert,
		TLSKeyFile:      o.tlsKey,
		Limits: maild.Limits{
			MaxSessions:        o.maxSessions,
			MaxSessionsPerIP:   o.maxSessionsPerIP,
			MaxMessageBytes:    o.maxMessageBytes,
			MaxMessageMemory:   o.maxMessageMemory,
			SessionIdleTimeout: o.sessionTimeout,
			ShutdownGrace:      o.shutdownGrace,
		},
	}, nil
}

// newLogger builds the process logger. It writes to stderr so that stdout stays
// free for the one-line answers --version and --validate give.
func newLogger(o *options, stderr io.Writer) (*slog.Logger, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(strings.ToLower(strings.TrimSpace(o.logLevel)))); err != nil {
		return nil, errors.New("--log-level: must be debug, info, warn or error")
	}
	handlerOptions := &slog.HandlerOptions{Level: level}
	switch strings.ToLower(strings.TrimSpace(o.logFormat)) {
	case "json":
		return slog.New(slog.NewJSONHandler(stderr, handlerOptions)), nil
	case "text", "":
		return slog.New(slog.NewTextHandler(stderr, handlerOptions)), nil
	default:
		return nil, errors.New("--log-format: must be text or json")
	}
}

// say writes to stdout or stderr. The error is dropped at this one point
// because a process that cannot write to its own stderr has nowhere left to
// report that it could not write to its own stderr.
func say(w io.Writer, format string, args ...any) {
	_, err := fmt.Fprintf(w, format, args...)
	ignore(err)
}

// ignore consumes an error the caller has decided is not actionable.
func ignore(error) {}

// version is the build's version, from the module build info when the binary was
// built from a tagged module and "(devel)" otherwise.
func version() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "(unknown)"
	}
	if value := strings.TrimSpace(info.Main.Version); value != "" {
		return value
	}
	return "(devel)"
}

// envOr is the environment's value when the variable is set, and the built-in
// default when it is not. A variable that is set but empty keeps its empty
// value, because "" is the documented way to disable a listener and a chart
// that renders an empty address must be obeyed rather than second-guessed.
func envOr(lookupEnv func(string) (string, bool), name, fallback string) string {
	if value, ok := lookup(lookupEnv, name); ok {
		return value
	}
	return fallback
}

func envBool(lookupEnv func(string) (string, bool), name string) (bool, error) {
	value, ok := lookup(lookupEnv, name)
	if !ok || value == "" {
		return false, nil
	}
	switch strings.ToLower(value) {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	default:
		return false, fmt.Errorf("%s: %q is not a boolean", name, value)
	}
}

func envInt(lookupEnv func(string) (string, bool), name string, fallback int) (int, error) {
	value, ok := lookup(lookupEnv, name)
	if !ok || value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not a number", name, value)
	}
	return parsed, nil
}

// envInt64 is envInt for the one bound that is a byte count rather than a
// count of things: 25 MiB does not fit an int on a 32-bit build.
func envInt64(lookupEnv func(string) (string, bool), name string, fallback int64) (int64, error) {
	value, ok := lookup(lookupEnv, name)
	if !ok || value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not a number", name, value)
	}
	return parsed, nil
}

func envDuration(lookupEnv func(string) (string, bool), name string, fallback time.Duration) (time.Duration, error) {
	value, ok := lookup(lookupEnv, name)
	if !ok || value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not a duration", name, value)
	}
	return parsed, nil
}

func lookup(lookupEnv func(string) (string, bool), name string) (string, bool) {
	if lookupEnv == nil {
		return "", false
	}
	return lookupEnv(name)
}
