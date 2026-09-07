package maild

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// -----------------------------------------------------------------------------
// The composition root
// -----------------------------------------------------------------------------
//
// This file owns everything that is true of every listener and nothing that is
// true of one protocol. It opens the store, loads the TLS certificate, binds the
// ports, bounds the timeouts, counts the sessions, logs exactly what is serving
// and what is not, and takes the whole thing down again.
//
// The protocols themselves live in other files: the JMAP/HTTP surface answers
// POST /jmap and GET /api/account (CONTRACT.md §1 and §2), and SMTP and POP3 are
// line protocols. Those are handed in through Services so that the Server can
// own the store — a handler needs the store to be open before it can be built,
// and the Server needs the handler before it can serve, so Config.Services
// resolves the ordering: the Server opens the store, then asks the caller to
// build the handlers from it.

// Listener names. They appear verbatim in the startup log's "listener" field and
// in Addr, so an operator reading the log and an operator reading the chart are
// looking at the same word.
const (
	ListenerAPI         = "jmap"        // the management HTTP API
	ListenerSMTP        = "smtp"        // inbound mail from the internet
	ListenerSubmission  = "submission"  // authenticated submission, STARTTLS
	ListenerSubmissions = "submissions" // authenticated submission, implicit TLS
	ListenerPOP3        = "pop3"        // mailbox retrieval, cleartext: there is no STLS
	ListenerPOP3S       = "pop3s"       // mailbox retrieval, implicit TLS
)

// WorkerQueue is the outbound queue runner's name in the startup log. Workers
// are named for the same reason listeners are: "is the queue actually running?"
// has to be answerable from the log rather than from the source.
const WorkerQueue = "queue"

// MinAdminTokenBytes is the shortest admin token this server will start with. It
// matches the control plane's own production rule for MAIL_API_TOKEN: this token
// creates and destroys mail domains, so a guessable one is a takeover.
const MinAdminTokenBytes = 32

// storeFileName is the bbolt file inside the data directory.
const storeFileName = "maild.db"

// spoolDirName holds message bodies. QueuedMessage.MessagePath is relative to
// it; the store never holds a body.
const spoolDirName = "spool"

// SessionHandler serves one accepted connection of a line protocol.
//
// The Server owns accepting, the session limit, the TLS wrapping of an
// implicit-TLS port, the first read deadline and the connection's close. The
// handler owns the protocol, and owns every deadline after the first — a handler
// that reads a 25 MB DATA must extend the deadline itself as it makes progress.
//
// ctx is cancelled when the server's shutdown grace period expires, at which
// point the connection is also closed underneath the handler; a handler that
// simply reads until error therefore terminates without watching ctx at all.
// Serve must not close conn: the Server closes it once Serve returns.
type SessionHandler interface {
	ServeSession(ctx context.Context, conn net.Conn)
}

// SessionHandlerFunc adapts a function to SessionHandler.
type SessionHandlerFunc func(ctx context.Context, conn net.Conn)

// ServeSession calls f.
func (f SessionHandlerFunc) ServeSession(ctx context.Context, conn net.Conn) { f(ctx, conn) }

// Deps is what a protocol handler is built from: everything the Server owns that
// a handler needs. It is the only argument to Config.Services.
type Deps struct {
	// Store is open and migrated. The Server closes it during Shutdown, so a
	// handler must not close it.
	Store *Store
	// Hostname is what this server calls itself. x:SystemSettings/get must
	// report it as defaultHostname and it must equal the control plane's
	// MAIL_HOSTNAME, or every mail operation refuses (CONTRACT.md §3.1). SMTP
	// greets with it and DKIM signs for it.
	Hostname string
	// AdminToken is the bearer the management API compares in constant time
	// (CONTRACT.md §1.2). It is a Secret so that logging it is a compile-time
	// nuisance and a runtime redaction rather than a silent leak.
	AdminToken Secret
	// TLS is nil when no certificate was configured, which is the signal that
	// STARTTLS must not be advertised.
	TLS *tls.Config
	// DataDir is the directory the store lives in; SpoolDir is where message
	// bodies go. Both exist by the time this is handed over.
	DataDir  string
	SpoolDir string
	// Limiter is the server's admission control, built once from Limits. Every
	// handler must be given THIS one rather than build its own: the failure
	// budget that locks out a password guesser is per Limiter, so two limiters
	// are two budgets and a peer guessing on SMTP and POP3 at once gets both.
	// It is also where the message size limit lives, so a handler that
	// announces a size takes it from Limiter.Config().MaxMessageBytes rather
	// than from a second field that can disagree.
	Limiter *Limiter
	Logger  *slog.Logger
	Clock   func() time.Time
}

// Worker is a background process the Server runs alongside the listeners. The
// one production installs is the outbound queue runner, and it is a Worker
// rather than a goroutine somebody remembered to start because the ordering
// matters: it must be running before the first message is accepted and stopped
// before the store it reads is closed.
//
// Start returns once the worker is running; the ctx it is given is cancelled
// when the server's shutdown grace expires. Shutdown drains in-flight work and
// returns when there is none left, or when its ctx expires.
type Worker interface {
	Start(ctx context.Context) error
	Shutdown(ctx context.Context) error
}

// NamedWorker is a Worker and the name the startup log gives it.
type NamedWorker struct {
	Name   string
	Worker Worker
}

// Services is the set of protocol handlers. A nil member disables the listeners
// that need it, and the startup log says so by name.
type Services struct {
	// API answers POST /jmap and GET /api/account. It is mounted at the root
	// and owns its own routing, because MAIL_API_URL may carry a path prefix
	// (CONTRACT.md §1.1) and only the handler knows what prefix it was built
	// for.
	API http.Handler
	// SMTP serves port 25: mail arriving from the internet for a hosted domain.
	SMTP SessionHandler
	// Submission serves ports 587 and 465: authenticated mail from a customer's
	// client. It is a separate handler from SMTP because the two differ in what
	// they accept, not in how they are served.
	Submission SessionHandler
	// POP3 serves ports 110 and 995.
	POP3 SessionHandler
	// Workers are started once every port is bound and stopped before the
	// store closes. An empty set is a server that accepts mail and never sends
	// any, which is why the startup log names every worker it started and says
	// so outright when there are none.
	Workers []NamedWorker
}

// Limits bounds every listener. A zero field takes the default; the defaults are
// the ones below, and they are deliberately generous enough for a slow client on
// a bad link and tight enough that an idle socket cannot be held open forever.
type Limits struct {
	// APIReadHeaderTimeout bounds the slowloris window on the management API.
	APIReadHeaderTimeout time.Duration // 10s
	// APIReadTimeout must comfortably exceed the client's own 10s per-request
	// context timeout (CONTRACT.md §1.4) so that we are never the one to give
	// up first on a request we could have answered.
	APIReadTimeout  time.Duration // 30s
	APIWriteTimeout time.Duration // 30s
	APIIdleTimeout  time.Duration // 60s
	// APIMaxHeaderBytes is headers only. The 10 MB body cap of CONTRACT.md §1.4
	// belongs to the handler, which is the only thing that knows a body from a
	// header.
	APIMaxHeaderBytes int // 1 MiB

	// SessionIdleTimeout is the deadline set before the handler's first read.
	// The handler owns every deadline after that.
	SessionIdleTimeout time.Duration // 5m
	// SessionMaxDuration is the hard cap on one connection however busy it
	// looks. It is what stops a peer that dribbles a byte per second from
	// holding a session open indefinitely by keeping the idle deadline alive.
	SessionMaxDuration time.Duration // 30m
	// TLSHandshakeTimeout bounds the handshake on an implicit-TLS port, which
	// happens before any protocol handler sees the connection.
	TLSHandshakeTimeout time.Duration // 30s
	// MaxSessions is the number of concurrent connections across all line
	// protocol listeners. Over the limit a connection is accepted and closed
	// immediately rather than queued, so that the backlog cannot become an
	// invisible latency source.
	MaxSessions int // 256
	// MaxSessionsPerIP is one peer's share of MaxSessions. Without it a single
	// address takes every slot and the server is down for everyone else while
	// looking perfectly healthy.
	MaxSessionsPerIP int // 16
	// MaxMessageBytes is the largest message this server accepts. It is the ONE
	// place that number is set: the Server puts it in the Limiter, and the SMTP
	// handler announces Limiter.Config().MaxMessageBytes rather than carrying a
	// second copy that could be larger than what is actually enforced.
	MaxMessageBytes int64 // 25 MiB
	// MaxMessageMemory caps the sum of the message bodies in memory at once.
	// It is the bound that makes MaxMessageBytes x MaxSessions — 6.4 GiB at the
	// defaults, an OOM a stranger can schedule — not the real number.
	MaxMessageMemory int64 // 256 MiB
	// ShutdownGrace bounds Run's own shutdown. Shutdown(ctx) uses ctx instead.
	ShutdownGrace time.Duration // 20s
}

func (l Limits) withDefaults() Limits {
	if l.APIReadHeaderTimeout <= 0 {
		l.APIReadHeaderTimeout = 10 * time.Second
	}
	if l.APIReadTimeout <= 0 {
		l.APIReadTimeout = 30 * time.Second
	}
	if l.APIWriteTimeout <= 0 {
		l.APIWriteTimeout = 30 * time.Second
	}
	if l.APIIdleTimeout <= 0 {
		l.APIIdleTimeout = 60 * time.Second
	}
	if l.APIMaxHeaderBytes <= 0 {
		l.APIMaxHeaderBytes = 1 << 20
	}
	if l.SessionIdleTimeout <= 0 {
		l.SessionIdleTimeout = 5 * time.Minute
	}
	if l.SessionMaxDuration <= 0 {
		l.SessionMaxDuration = 30 * time.Minute
	}
	if l.TLSHandshakeTimeout <= 0 {
		l.TLSHandshakeTimeout = 30 * time.Second
	}
	if l.MaxSessions <= 0 {
		l.MaxSessions = 256
	}
	if l.ShutdownGrace <= 0 {
		l.ShutdownGrace = 20 * time.Second
	}
	// MaxSessionsPerIP, MaxMessageBytes and MaxMessageMemory are deliberately
	// left at zero here. They belong to the Limiter, whose own defaults and
	// clamps (a per-IP share no larger than the whole, a memory ceiling no
	// smaller than one message) are the ones that decide, and the Server reads
	// the effective values back out of it — so there is one set of numbers
	// rather than two that can drift apart.
	return l
}

// limiterConfig is the policy the Server's one Limiter is built from. Every
// field the operator set travels through here, and everything left at zero
// takes the Limiter's own default.
func (l Limits) limiterConfig(clock func() time.Time) LimiterConfig {
	return LimiterConfig{
		MaxSessions:      l.MaxSessions,
		MaxSessionsPerIP: l.MaxSessionsPerIP,
		MaxMessageBytes:  l.MaxMessageBytes,
		MaxMessageMemory: l.MaxMessageMemory,
		Now:              clock,
	}
}

// Config is everything the Server needs. Every listen address is optional and an
// empty one disables that listener; Hostname, AdminToken and DataDir are not
// optional, and NewServer refuses rather than starting a server that would fail
// later in a way the operator could not read.
type Config struct {
	// Hostname must equal the control plane's MAIL_HOSTNAME. See Deps.Hostname:
	// getting it wrong does not fail here, it fails on every bind with "the
	// mail server calls itself X", so it is worth checking by eye at startup —
	// which is why it is logged.
	Hostname string
	// AdminToken is the MAIL_API_TOKEN the control plane sends as a bearer.
	AdminToken Secret
	// DataDir holds the store and the spool. It is created if absent.
	DataDir string

	// Listen addresses in the "host:port" form net.Listen takes. Empty disables.
	APIAddr         string
	SMTPAddr        string
	SubmissionAddr  string
	SubmissionsAddr string
	POP3Addr        string
	POP3SAddr       string

	// APITLS serves the management API over TLS. It needs a certificate. In a
	// cluster where the gateway terminates TLS this stays false and the API
	// listener is bound on the pod address only.
	APITLS bool

	// TLSCertFile and TLSKeyFile are a PEM pair. Both or neither. Without them
	// the implicit-TLS listeners are disabled and STARTTLS is unavailable,
	// which the startup log states outright rather than leaving to be
	// discovered by a customer whose password crossed the network in the clear.
	TLSCertFile string
	TLSKeyFile  string

	// Services builds the protocol handlers once the store is open. A nil
	// Services, or a nil handler within the returned set, disables the
	// listeners that need it.
	Services func(Deps) (Services, error)

	Logger *slog.Logger
	Clock  func() time.Time
	Limits Limits
}

// Validate reports the configuration mistakes that must stop a start.
//
// The hostname and the token are checked here rather than left to fail on first
// use because both fail invisibly. A wrong hostname is answered by the facade
// with "the mail server calls itself X" on every operation and never at startup;
// a short token is a credential that creates and destroys mail domains. Both
// deserve to be a refusal to start with the operator still watching.
func (c Config) Validate() error {
	var problems []error
	if strings.TrimSpace(c.Hostname) != c.Hostname {
		problems = append(problems, errors.New("the hostname must not have leading or trailing spaces"))
	}
	if strings.TrimSpace(c.Hostname) == "" {
		problems = append(problems, errors.New("the hostname is required and must equal the control plane's MAIL_HOSTNAME: the facade refuses every mail operation unless this server reports that exact name"))
	}
	if len(c.AdminToken) < MinAdminTokenBytes {
		// The length of a token that is already known to be too short is not a
		// secret, and it is the one fact that tells the operator whether the
		// value arrived truncated or empty.
		problems = append(problems, fmt.Errorf("the admin token must be at least %d characters and must equal the control plane's MAIL_API_TOKEN; got %d", MinAdminTokenBytes, len(c.AdminToken)))
	}
	if strings.TrimSpace(c.DataDir) == "" {
		problems = append(problems, errors.New("the data directory is required"))
	}
	if (c.TLSCertFile == "") != (c.TLSKeyFile == "") {
		problems = append(problems, errors.New("the TLS certificate and key must be set together"))
	}
	if c.APITLS && c.TLSCertFile == "" {
		problems = append(problems, errors.New("serving the management API over TLS needs a TLS certificate and key"))
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("maild: %w", errors.Join(problems...))
}

// Server is one mail server: a store, a management API and the line protocol
// listeners, started and stopped together.
type Server struct {
	config   Config
	limits   Limits
	logger   *slog.Logger
	clock    func() time.Time
	store    *Store
	tls      *tls.Config
	services Services
	// limiter is the one admission control every listener shares. The accept
	// loop takes a session slot from it before the TLS handshake, and the
	// protocol handlers take their authentication and message-memory
	// reservations from the same object, so the budgets are the server's rather
	// than each listener's.
	limiter *Limiter

	api     *http.Server
	apiNet  net.Listener
	planned []*listener
	workers []NamedWorker

	// baseCtx is cancelled when the shutdown grace period expires. Its
	// cancellation closes every connection still open.
	baseCtx    context.Context
	cancelBase context.CancelFunc

	wg sync.WaitGroup

	mu       sync.Mutex
	started  bool
	draining bool
	conns    map[net.Conn]struct{}
	closed   chan struct{}
	stopOnce sync.Once
	stopErr  error
}

// listener is one planned port: bound, or disabled with a reason an operator can
// act on.
type listener struct {
	name        string
	addr        string
	implicitTLS bool
	// startTLS records whether this listener's protocol offers an in-band
	// upgrade at all. It is a property of the protocol and not of the port, and
	// it is not derivable from implicitTLS: SMTP and submission implement
	// STARTTLS, POP3 deliberately does not (pop3.go answers STLS with a
	// refusal, because a strippable upgrade on a cleartext port is worse than
	// an honest cleartext port). The startup log prints it, so a value that is
	// merely plausible is a listener the operator believes is protected and is
	// not.
	startTLS bool
	handler  SessionHandler
	// reason is empty when the listener is to be bound, and otherwise says in
	// one clause why it is not.
	reason string
	net    net.Listener
}

// boundListener pairs a planned listener with the socket it took. Start builds
// the whole set before publishing any of it, so that a bind failure leaves the
// Server exactly as it was.
type boundListener struct {
	entry *listener
	net   net.Listener
}

// NewServer opens the store, loads the certificate, builds the handlers and
// plans the listeners. Nothing is bound yet: NewServer can fail without having
// taken a port, and Start can then fail without leaving one taken.
//
// The caller owns the returned Server until Shutdown, which closes the store.
// A NewServer that returns an error has already closed anything it opened.
func NewServer(config Config) (*Server, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	limits := config.Limits.withDefaults()
	logger := config.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	clock := config.Clock
	if clock == nil {
		clock = time.Now
	}

	spoolDir := filepath.Join(config.DataDir, spoolDirName)
	if err := os.MkdirAll(spoolDir, 0o700); err != nil {
		return nil, fmt.Errorf("maild: create the spool directory: %w", err)
	}
	tlsConfig, err := loadTLS(config.TLSCertFile, config.TLSKeyFile)
	if err != nil {
		return nil, err
	}
	store, err := Open(filepath.Join(config.DataDir, storeFileName), WithClock(clock))
	if err != nil {
		return nil, err
	}

	// One Limiter, built before the handlers so that every one of them can be
	// given this exact object. Its own defaults and clamps decide the effective
	// policy, which is then read back into limits: from here on the numbers the
	// accept loop enforces, the numbers the handlers enforce and the numbers the
	// startup log prints are the same numbers.
	limiter := NewLimiter(limits.limiterConfig(clock))
	effective := limiter.Config()
	limits.MaxSessions = effective.MaxSessions
	limits.MaxSessionsPerIP = effective.MaxSessionsPerIP
	limits.MaxMessageBytes = effective.MaxMessageBytes
	limits.MaxMessageMemory = effective.MaxMessageMemory

	server := &Server{
		config:  config,
		limits:  limits,
		logger:  logger,
		clock:   clock,
		store:   store,
		tls:     tlsConfig,
		limiter: limiter,
		conns:   make(map[net.Conn]struct{}),
		closed:  make(chan struct{}),
	}
	server.baseCtx, server.cancelBase = context.WithCancel(context.Background())

	if config.Services != nil {
		services, err := config.Services(Deps{
			Store:      store,
			Hostname:   config.Hostname,
			AdminToken: config.AdminToken,
			TLS:        tlsConfig,
			DataDir:    config.DataDir,
			SpoolDir:   spoolDir,
			Limiter:    limiter,
			Logger:     logger,
			Clock:      clock,
		})
		if err != nil {
			server.cancelBase()
			return nil, errors.Join(fmt.Errorf("maild: build the protocol handlers: %w", err), store.Close())
		}
		server.services = services
	}
	server.plan()
	return server, nil
}

// loadTLS reads the PEM pair. No certificate is a legitimate configuration — a
// development run, or a deployment where the gateway terminates TLS — so an
// empty pair is nil, nil rather than an error.
func loadTLS(certFile, keyFile string) (*tls.Config, error) {
	if certFile == "" && keyFile == "" {
		return nil, nil
	}
	certificate, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		// The error from LoadX509KeyPair names the files and nothing from
		// inside the key.
		return nil, fmt.Errorf("maild: load the TLS certificate: %w", err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{certificate},
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// plan decides, per listener, bind or disabled-with-a-reason. It is separate
// from Start so that the decision can be logged as one block before anything is
// bound, and so that a test can read it.
func (s *Server) plan() {
	add := func(name, addr string, implicitTLS, startTLS bool, handler SessionHandler, missing string) {
		entry := &listener{name: name, addr: addr, implicitTLS: implicitTLS, startTLS: startTLS, handler: handler}
		switch {
		case addr == "":
			entry.reason = "no listen address is configured"
		case handler == nil:
			entry.reason = "no " + missing + " handler is installed in this build"
		case implicitTLS && s.tls == nil:
			entry.reason = "no TLS certificate is configured and this port is TLS from the first byte"
		}
		s.planned = append(s.planned, entry)
	}
	// The startTLS column is what each protocol actually implements, not what
	// its port number suggests: smtp.go serves STARTTLS, pop3.go refuses STLS.
	add(ListenerSMTP, s.config.SMTPAddr, false, true, s.services.SMTP, "SMTP")
	add(ListenerSubmission, s.config.SubmissionAddr, false, true, s.services.Submission, "submission")
	add(ListenerSubmissions, s.config.SubmissionsAddr, true, false, s.services.Submission, "submission")
	add(ListenerPOP3, s.config.POP3Addr, false, false, s.services.POP3, "POP3")
	add(ListenerPOP3S, s.config.POP3SAddr, true, false, s.services.POP3, "POP3")
}

// apiReason is why the management API is not being served, or "" when it is.
func (s *Server) apiReason() string {
	switch {
	case s.config.APIAddr == "":
		return "no listen address is configured"
	case s.services.API == nil:
		return "no management API handler is installed in this build"
	default:
		return ""
	}
}

// Start binds every enabled listener and begins serving. It returns once
// everything is bound, so a caller that gets a nil error knows the ports are
// taken and Addr will answer.
//
// Binding is all-or-nothing: a port already in use fails the start and releases
// the ports taken so far, rather than leaving a half-served process that looks
// healthy to a readiness probe.
func (s *Server) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return errors.New("maild: the server is already started")
	}
	s.started = true
	s.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return err
	}

	apiReason := s.apiReason()
	enabled := 0
	if apiReason == "" {
		enabled++
	}
	for _, entry := range s.planned {
		if entry.reason == "" {
			enabled++
		}
	}
	if enabled == 0 {
		return errors.New("maild: no listener is enabled: set at least one listen address and install the handler that serves it")
	}

	var config net.ListenConfig
	bound := make([]net.Listener, 0, len(s.planned)+1)
	unwind := func() {
		for _, listener := range bound {
			s.ignore(listener.Close())
		}
	}

	var apiListener net.Listener
	if apiReason == "" {
		listener, err := config.Listen(ctx, "tcp", s.config.APIAddr)
		if err != nil {
			unwind()
			return fmt.Errorf("maild: bind %s on %s: %w", ListenerAPI, s.config.APIAddr, err)
		}
		if s.config.APITLS {
			listener = tls.NewListener(listener, s.tls)
		}
		apiListener = listener
		bound = append(bound, listener)
	}
	sessions := make([]boundListener, 0, len(s.planned))
	for _, entry := range s.planned {
		if entry.reason != "" {
			continue
		}
		listener, err := config.Listen(ctx, "tcp", entry.addr)
		if err != nil {
			unwind()
			return fmt.Errorf("maild: bind %s on %s: %w", entry.name, entry.addr, err)
		}
		sessions = append(sessions, boundListener{entry: entry, net: listener})
		bound = append(bound, listener)
	}

	var api *http.Server
	if apiListener != nil {
		api = &http.Server{
			Handler:           s.services.API,
			ReadHeaderTimeout: s.limits.APIReadHeaderTimeout,
			ReadTimeout:       s.limits.APIReadTimeout,
			WriteTimeout:      s.limits.APIWriteTimeout,
			IdleTimeout:       s.limits.APIIdleTimeout,
			MaxHeaderBytes:    s.limits.APIMaxHeaderBytes,
			BaseContext:       func(net.Listener) context.Context { return s.baseCtx },
			ErrorLog:          slog.NewLogLogger(s.logger.Handler(), slog.LevelDebug),
		}
	}

	// The background workers start before anything is published, so a worker
	// that cannot start unwinds the ports exactly like a failed bind: either
	// this server is whole or it took nothing. A queue runner that failed to
	// start on a server that stayed up would be the same silent outage the
	// missing runner was.
	started, err := s.startWorkers()
	if err != nil {
		unwind()
		return err
	}

	// Publish everything in one step under the lock. A caller polling Addr from
	// another goroutine — which is what Run in a goroutine looks like — then
	// sees either nothing bound or the whole set, and never a listener being
	// written underneath it. A failed bind above published nothing at all.
	s.mu.Lock()
	s.apiNet = apiListener
	s.api = api
	s.workers = started
	for _, session := range sessions {
		session.entry.net = session.net
	}
	s.mu.Unlock()

	s.logStartup(apiReason, apiListener, sessions)

	if api != nil {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			if err := api.Serve(apiListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				s.logger.Error("maild listener stopped", "listener", ListenerAPI, "error", err.Error())
			}
		}()
	}
	for _, session := range sessions {
		s.wg.Add(1)
		go s.accept(session.entry, session.net)
	}
	return nil
}

// startWorkers starts every installed worker, in order, and stops the ones it
// already started if a later one fails. A worker with no name or no Worker is
// skipped rather than started, because a half-filled NamedWorker is a wiring
// mistake and starting it would hide the mistake behind a working server.
//
// The workers run under baseCtx, so the cancellation that closes every live
// connection at the end of the grace period also cancels their in-flight work.
func (s *Server) startWorkers() ([]NamedWorker, error) {
	started := make([]NamedWorker, 0, len(s.services.Workers))
	for _, worker := range s.services.Workers {
		if worker.Worker == nil {
			continue
		}
		if err := worker.Worker.Start(s.baseCtx); err != nil {
			stop, cancel := context.WithTimeout(context.Background(), s.limits.ShutdownGrace)
			defer cancel()
			problems := []error{fmt.Errorf("maild: start the %s worker: %w", worker.Name, err)}
			return nil, errors.Join(append(problems, s.stopWorkers(stop, started)...)...)
		}
		started = append(started, worker)
	}
	return started, nil
}

// stopWorkers shuts every worker down and reports what would not stop. Each is
// given the same deadline rather than a share of it: they stop concurrently in
// practice because each Shutdown is waiting on its own in-flight work.
func (s *Server) stopWorkers(ctx context.Context, workers []NamedWorker) []error {
	var problems []error
	for _, worker := range workers {
		if err := worker.Worker.Shutdown(ctx); err != nil {
			problems = append(problems, fmt.Errorf("maild: stop the %s worker: %w", worker.Name, err))
		}
	}
	return problems
}

// logStartup states, once, exactly what is serving and what is not. It is the
// one place an operator can read the answer to "is submission actually up?"
// without a port scan. The admin token is not among the fields, and no field is
// derived from it.
func (s *Server) logStartup(apiReason string, apiListener net.Listener, sessions []boundListener) {
	limits := s.limiter.Config()
	s.logger.Info("maild starting",
		"hostname", s.config.Hostname,
		"data_dir", s.config.DataDir,
		"store", s.store.Path(),
		"tls", s.tls != nil,
	)
	// The effective limits, read out of the Limiter that enforces them rather
	// than out of the Limits that were asked for, so a value that was clamped
	// is printed as what will actually happen. bodies_in_flight is the joint
	// memory bound made legible: it is what stops max_message_bytes x
	// max_sessions being the real memory ceiling.
	s.logger.Info("maild limits",
		"max_sessions", limits.MaxSessions,
		"max_sessions_per_ip", limits.MaxSessionsPerIP,
		"max_message_bytes", limits.MaxMessageBytes,
		"max_message_memory", limits.MaxMessageMemory,
		"bodies_in_flight", s.limiter.MaxConcurrentMessages(),
		"auth_failures", limits.AuthFailures,
		"auth_lockout", limits.AuthLockout.String(),
	)
	if apiReason == "" {
		s.logger.Info("maild listener bound",
			"listener", ListenerAPI,
			"addr", apiListener.Addr().String(),
			"tls", s.config.APITLS,
		)
	} else {
		s.logger.Warn("maild listener disabled", "listener", ListenerAPI, "reason", apiReason)
	}
	for _, entry := range s.planned {
		if entry.reason != "" {
			s.logger.Warn("maild listener disabled", "listener", entry.name, "reason", entry.reason)
		}
	}
	for _, session := range sessions {
		entry := session.entry
		// starttls is true only where the protocol on this listener really
		// offers the upgrade and there is a certificate to offer it with. The
		// earlier form said "not implicit TLS and a certificate exists", which
		// printed starttls=true for POP3 — a port that answers STLS with a
		// refusal. An operator reading that line concluded a password could be
		// protected on port 110, and it cannot.
		startTLS := entry.startTLS && s.tls != nil
		s.logger.Info("maild listener bound",
			"listener", entry.name,
			"addr", session.net.Addr().String(),
			"tls", entry.implicitTLS,
			"starttls", startTLS,
		)
		if !entry.implicitTLS && !startTLS {
			s.logger.Warn("maild listener is cleartext and cannot upgrade: everything sent to it, including any password, crosses the network in the clear",
				"listener", entry.name)
		}
	}
	if s.tls == nil {
		s.logger.Warn("maild has no TLS certificate: STARTTLS cannot be offered and passwords would cross the network in the clear")
	}
	for _, worker := range s.workers {
		s.logger.Info("maild worker started", "worker", worker.Name)
	}
	if len(s.workers) == 0 {
		// The absence this states is the one that is invisible from outside:
		// every listener answers, every message is accepted with 250, and
		// nothing ever leaves the building.
		s.logger.Warn("maild has no outbound queue runner: mail this server accepts for another host is spooled and never sent")
	}
}

// accept is one listener's accept loop.
func (s *Server) accept(entry *listener, socket net.Listener) {
	defer s.wg.Done()
	// backoff is the http.Server treatment of a transient accept failure: sleep
	// a little and try again, doubling to a cap, rather than spinning the CPU
	// on a file descriptor exhaustion that will clear on its own.
	const minBackoff, maxBackoff = 5 * time.Millisecond, time.Second
	backoff := time.Duration(0)
	for {
		conn, err := socket.Accept()
		if err != nil {
			if s.isDraining() || errors.Is(err, net.ErrClosed) {
				return
			}
			if backoff == 0 {
				backoff = minBackoff
			} else {
				backoff = min(2*backoff, maxBackoff)
			}
			s.logger.Warn("maild accept failed", "listener", entry.name, "error", err.Error(), "retry_in", backoff.String())
			select {
			case <-time.After(backoff):
			case <-s.baseCtx.Done():
				return
			}
			continue
		}
		backoff = 0

		// Admission, before the TLS handshake and before a goroutine exists.
		// This is the shared Limiter rather than a per-listener counter, so
		// one peer's share is its share of the whole server: sixteen sockets
		// on POP3 and sixteen more on submission is one peer over its budget,
		// not two peers inside theirs.
		lease, err := s.limiter.AcquireSession(remoteAddress(conn))
		if err != nil {
			// Over the limit. Closing now is a worse experience than a queue
			// for one client and a better one for every other, because a peer
			// that is refused retries and a peer that is queued behind 256
			// sessions times out having held a slot the whole time.
			s.logger.Warn("maild refused a session at the concurrency limit",
				"listener", entry.name,
				"reason", err.Error(),
				"max_sessions", s.limits.MaxSessions,
				"max_sessions_per_ip", s.limits.MaxSessionsPerIP)
			s.ignore(conn.Close())
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			// Release is idempotent, so this is still correct for a handler
			// that took the lease over through SessionLease.
			defer lease.Release()
			s.serve(entry, conn, lease)
		}()
	}
}

// sessionLeaseKey is the context key the accept loop's reservation travels
// under. It is a struct type rather than a string so nothing else can collide
// with it.
type sessionLeaseKey struct{}

// SessionLease is the session slot the Server's accept loop reserved for this
// connection, or nil for a connection the Server did not admit.
//
// It exists for one caller: a handler that takes its own lease from the same
// Limiter — SMTPServer and POP3Server both do, because both can also be driven
// from their own accept loops — must release this one as it takes over, or a
// single connection is charged twice and the session budget the operator
// configured and the startup log printed is quietly halved. Release is
// idempotent and nil-safe, so calling it from the adapter that hands a
// connection to such a handler is always correct.
//
// The handover is not atomic: between the release and the handler's own
// acquisition another peer can take the slot, in which case this connection is
// refused by the handler with the protocol's own "try again later". That is a
// refusal under saturation, which is the honest outcome; the alternative is a
// budget that silently means half what it says.
func SessionLease(ctx context.Context) *Lease {
	lease, ok := ctx.Value(sessionLeaseKey{}).(*Lease)
	if !ok {
		return nil
	}
	return lease
}

// serve runs one connection: TLS if the port is implicit-TLS, the first
// deadline, the hard cap, then the handler.
func (s *Server) serve(entry *listener, conn net.Conn, lease *Lease) {
	if !s.track(conn) {
		// Shutdown's grace period is already over.
		s.ignore(conn.Close())
		return
	}
	defer func() {
		s.untrack(conn)
		s.ignore(conn.Close())
	}()

	ctx, cancel := context.WithTimeout(s.baseCtx, s.limits.SessionMaxDuration)
	defer cancel()
	// The handler may need to take the session slot over from the accept loop;
	// see SessionLease.
	ctx = context.WithValue(ctx, sessionLeaseKey{}, lease)
	// The hard cap and the shutdown both arrive as ctx.Done. Closing the
	// connection is what makes a handler blocked in Read return, whether or not
	// it watches ctx.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			s.ignore(conn.Close())
		case <-done:
		}
	}()

	if entry.implicitTLS {
		secure := tls.Server(conn, s.tls)
		if err := secure.SetDeadline(s.clock().Add(s.limits.TLSHandshakeTimeout)); err != nil {
			s.logger.Debug("maild handshake deadline failed", "listener", entry.name, "error", err.Error())
			return
		}
		if err := secure.HandshakeContext(ctx); err != nil {
			// A failed handshake is routine on a public port — a scanner, a
			// client with no shared cipher — so it is a debug line, not a
			// warning that would drown the log.
			s.logger.Debug("maild TLS handshake failed", "listener", entry.name, "error", err.Error())
			return
		}
		conn = secure
	}
	if err := conn.SetDeadline(s.clock().Add(s.limits.SessionIdleTimeout)); err != nil {
		s.logger.Debug("maild session deadline failed", "listener", entry.name, "error", err.Error())
		return
	}
	entry.handler.ServeSession(ctx, conn)
}

// track registers a live connection so that shutdown can close it. It reports
// false once the server has stopped, which is the race where a connection was
// accepted a moment before the listener closed.
func (s *Server) track(conn net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conns == nil {
		return false
	}
	s.conns[conn] = struct{}{}
	return true
}

func (s *Server) untrack(conn net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.conns, conn)
}

func (s *Server) isDraining() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.draining
}

// Addr is the address a listener actually bound, which is what a caller that
// asked for port 0 needs. It is "" for a listener that is not serving.
func (s *Server) Addr(name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if name == ListenerAPI {
		if s.apiNet == nil {
			return ""
		}
		return s.apiNet.Addr().String()
	}
	for _, entry := range s.planned {
		if entry.name == name && entry.net != nil {
			return entry.net.Addr().String()
		}
	}
	return ""
}

// Addrs is every bound listener by name, for a log line or a test.
func (s *Server) Addrs() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	addrs := make(map[string]string, len(s.planned)+1)
	if s.apiNet != nil {
		addrs[ListenerAPI] = s.apiNet.Addr().String()
	}
	for _, entry := range s.planned {
		if entry.net != nil {
			addrs[entry.name] = entry.net.Addr().String()
		}
	}
	return addrs
}

// Store is the open store. It is here for a caller that needs it after the
// handlers are built — a maintenance command, a test. Closing it is Shutdown's
// job, not the caller's.
func (s *Server) Store() *Store { return s.store }

// Hostname is what this server calls itself.
func (s *Server) Hostname() string { return s.config.Hostname }

// Limiter is the admission control every listener shares. It is exported so a
// caller can read the effective policy — Limiter.Config() is exactly what the
// startup log prints — without inferring it from the Limits it asked for.
func (s *Server) Limiter() *Limiter { return s.limiter }

// Run starts the server, waits for ctx, and shuts down within
// Limits.ShutdownGrace. It is what a process wants: one call that returns when
// the server is fully stopped.
func (s *Server) Run(ctx context.Context) error {
	if err := s.Start(ctx); err != nil {
		// A failed start still leaves the store open, because NewServer opened
		// it. Shutting down is how it gets closed.
		stop, cancel := context.WithTimeout(context.Background(), s.limits.ShutdownGrace)
		defer cancel()
		return errors.Join(err, s.Shutdown(stop))
	}
	<-ctx.Done()
	// The parent context is already cancelled, so shutdown gets a fresh one:
	// deriving from ctx would mean no grace period at all.
	stop, cancel := context.WithTimeout(context.Background(), s.limits.ShutdownGrace)
	defer cancel()
	return s.Shutdown(stop)
}

// Shutdown stops accepting, lets the sessions in flight finish, and closes the
// store. When ctx expires first the remaining connections are closed underneath
// their handlers and Shutdown returns ctx's error — the store is closed either
// way, so a Shutdown that reports a deadline still leaves nothing to clean up.
//
// Shutdown is safe to call more than once and safe to call on a server that was
// never started.
func (s *Server) Shutdown(ctx context.Context) error {
	s.stopOnce.Do(func() { s.stopErr = s.shutdown(ctx) })
	<-s.closed
	return s.stopErr
}

func (s *Server) shutdown(ctx context.Context) error {
	defer close(s.closed)

	s.mu.Lock()
	s.draining = true
	api := s.api
	workers := s.workers
	sockets := make([]boundListener, 0, len(s.planned))
	for _, entry := range s.planned {
		if entry.net != nil {
			sockets = append(sockets, boundListener{entry: entry, net: entry.net})
		}
	}
	s.mu.Unlock()

	var problems []error

	// Stop taking new work first, so that the drain below is finite.
	if api != nil {
		if err := api.Shutdown(ctx); err != nil {
			problems = append(problems, fmt.Errorf("maild: stop %s: %w", ListenerAPI, err))
		}
	}
	for _, socket := range sockets {
		if err := socket.net.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			problems = append(problems, fmt.Errorf("maild: stop %s: %w", socket.entry.name, err))
		}
	}

	drained := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(drained)
	}()
	select {
	case <-drained:
	case <-ctx.Done():
		// Grace is over. Cancelling the base context closes every live
		// connection, which is what unblocks a handler parked in Read.
		s.cancelBase()
		s.closeConns()
		<-drained
		problems = append(problems, fmt.Errorf("maild: shut down with sessions still open: %w", ctx.Err()))
	}

	// The workers stop after the sessions and before the store, which is the
	// only order that is safe in both directions: a worker reads the store, so
	// it cannot outlive it, and a session can queue a message, so a worker that
	// stopped first would leave that message for the next start.
	problems = append(problems, s.stopWorkers(ctx, workers)...)
	s.cancelBase()

	if err := s.store.Close(); err != nil {
		problems = append(problems, fmt.Errorf("maild: close the store: %w", err))
	}
	s.logger.Info("maild stopped")
	return errors.Join(problems...)
}

// ignore consumes an error the Server has decided is not actionable. Closing a
// connection whose peer has already gone, or a listener during an unwind, can
// only fail in ways that change nothing — but the repo's linter requires the
// error to be consumed rather than blanked, and a named consumer makes each
// such decision greppable.
func (*Server) ignore(error) {}

// closeConns closes every tracked connection and stops tracking, which also
// makes any connection accepted from here on close immediately.
func (s *Server) closeConns() {
	s.mu.Lock()
	conns := s.conns
	s.conns = nil
	s.mu.Unlock()
	for conn := range conns {
		s.ignore(conn.Close())
	}
}
