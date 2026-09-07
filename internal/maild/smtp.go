package maild

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// This file is the SMTP server: the line protocol, the session state machine and
// the limits that keep a hostile client from costing more than it should. Every
// message it accepts leaves through the Mailer in delivery.go; nothing is stored
// from here.
//
// Two rules are absolute.
//
// AUTH is never offered and never accepted on a cleartext session. It is not
// advertised in the EHLO response, and the check happens before the mechanism
// name is even parsed, so no credential is read off an unencrypted connection
// even by a client that ignores the advertisement.
//
// An unauthenticated session may only address a recipient this server hosts.
// That single check in Mailer.Resolve is the whole difference between a mail
// server and an open relay.
//
// Everything a stranger can make this process spend is bounded by the Limiter:
// session slots globally and per source address, the memory held by concurrent
// DATA bodies, and the rate at which a peer may present a credential. Those
// refusals are all made before the work they are protecting.

// Defaults chosen so an unconfigured server is still safe to expose.
const (
	// DefaultSMTPMaxMessageBytes is the largest message accepted. 25 MiB is what
	// the large consumer providers accept, so it is the size customers expect to
	// be able to send.
	DefaultSMTPMaxMessageBytes int64 = 25 << 20
	// DefaultSMTPMaxRecipients bounds one transaction. RFC 5321 §4.5.3.1.8 sets
	// 100 as the minimum a server must accept.
	DefaultSMTPMaxRecipients = 100
	// DefaultSMTPMaxErrors drops a client that is guessing rather than sending.
	DefaultSMTPMaxErrors = 10
	// DefaultSMTPMaxConnections bounds concurrent sessions, because each one
	// holds a read buffer and, during DATA, a message.
	DefaultSMTPMaxConnections = 256
	// DefaultSMTPCommandTimeout and DefaultSMTPDataTimeout are the RFC 5321
	// §4.5.3.2 server timeouts.
	DefaultSMTPCommandTimeout = 5 * time.Minute
	DefaultSMTPDataTimeout    = 10 * time.Minute
)

const (
	// smtpMaxCommandLine is well over RFC 5321's 512-octet command line, because
	// RFC 4954 §4 allows an AUTH command to reach 12288 octets and a client may
	// send its initial response inline.
	smtpMaxCommandLine = 12288
	// smtpMaxDataLine is generous on purpose: real mail contains single lines
	// far longer than the 1000 octets the specification allows, and the total
	// message size is bounded anyway.
	smtpMaxDataLine = 1 << 20
	// smtpDrainAllowance is how much more than the message limit is read before
	// the connection is dropped. Reading to the end of an oversized message
	// keeps the session in sync so the next transaction still works; refusing to
	// read forever is what stops a client that never sends the final dot.
	smtpDrainAllowance int64 = 1 << 20
	smtpReadBuffer           = 8192
	smtpWriteBuffer          = 4096
	// smtpMaxReplyText bounds a reply line, which can quote client-supplied
	// text.
	smtpMaxReplyText = 400
	// smtpMaxUnauthenticatedFanOut bounds how many fan-out recipients one
	// unauthenticated transaction may name.
	//
	// A mailing-list recipient turns one inbound message into up to maxFanOut
	// outbound copies. RFC 5321 §4.5.3.1.8 makes this server accept 100
	// recipients, so without a separate bound a stranger's single message can
	// become 100 x maxFanOut deliveries that this server pays to make — the
	// amplification is in the product of the two limits, and neither limit on
	// its own sees it.
	//
	// A correspondent writing to a customer addresses one or two of their
	// forwarders; a message naming more than a handful is building an
	// amplifier. Authenticated submission is not bounded here, because it is
	// already bounded by having to own a mailbox on this server.
	smtpMaxUnauthenticatedFanOut = 4
)

// ErrSMTPServerClosed is returned by Serve after Close.
var ErrSMTPServerClosed = errors.New("maild: the SMTP server is closed")

var (
	errLineTooLong     = errors.New("maild: the line is too long")
	errMessageTooLarge = errors.New("maild: the message is too large")
	errRunawayMessage  = errors.New("maild: the message never ended")
)

// SMTPConfig configures an SMTPServer. Every zero value has a safe default.
type SMTPConfig struct {
	// Hostname is what this server calls itself in the banner, the EHLO
	// response and the Received header. It should be the name the MX record
	// points at.
	Hostname string
	// TLSConfig enables STARTTLS, and with it AUTH. Without it the server is
	// inbound-only: submission is impossible because AUTH is refused on every
	// cleartext session.
	TLSConfig *tls.Config

	MaxMessageBytes int64
	MaxRecipients   int
	MaxErrors       int
	MaxConnections  int
	CommandTimeout  time.Duration
	DataTimeout     time.Duration

	// Limiter is the admission control: session slots, the joint memory bound
	// on concurrent DATA bodies, and the authentication-failure lockout. It is
	// a pointer so one Limiter can be shared with the other listeners, which is
	// what makes the limits the server's rather than each listener's.
	//
	// A nil Limiter is built from MaxConnections and MaxMessageBytes, so a
	// server configured without one is bounded anyway: the failure mode of a
	// forgotten field must never be "no limit".
	Limiter *Limiter

	Logger *slog.Logger
	Now    func() time.Time
}

func (c SMTPConfig) withDefaults() SMTPConfig {
	if strings.TrimSpace(c.Hostname) == "" {
		c.Hostname = "localhost"
	}
	c.Hostname = sanitizeHeaderValue(c.Hostname)
	if c.MaxMessageBytes <= 0 {
		c.MaxMessageBytes = DefaultSMTPMaxMessageBytes
	}
	if c.MaxRecipients <= 0 {
		c.MaxRecipients = DefaultSMTPMaxRecipients
	}
	if c.MaxErrors <= 0 {
		c.MaxErrors = DefaultSMTPMaxErrors
	}
	if c.MaxConnections <= 0 {
		c.MaxConnections = DefaultSMTPMaxConnections
	}
	if c.CommandTimeout <= 0 {
		c.CommandTimeout = DefaultSMTPCommandTimeout
	}
	if c.DataTimeout <= 0 {
		c.DataTimeout = DefaultSMTPDataTimeout
	}
	if c.Limiter == nil {
		c.Limiter = NewLimiter(LimiterConfig{
			MaxSessions:     c.MaxConnections,
			MaxMessageBytes: c.MaxMessageBytes,
		})
	}
	// Two components each carry a message size limit, so they can disagree. The
	// smaller one is what actually happens and the larger one is a lie the
	// server would tell in its EHLO SIZE and in its 552 text, so adopt the
	// smaller as the announced limit and keep every reply honest.
	if limit := c.Limiter.Config().MaxMessageBytes; limit < c.MaxMessageBytes {
		c.MaxMessageBytes = limit
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return c
}

// SMTPServer speaks SMTP on behalf of a Mailer.
type SMTPServer struct {
	mailer Mailer
	config SMTPConfig
	logger *slog.Logger

	baseCtx context.Context
	cancel  context.CancelFunc

	mu        sync.Mutex
	closed    bool
	listeners map[net.Listener]struct{}
	conns     map[net.Conn]struct{}
	wg        sync.WaitGroup
}

// NewSMTPServer prepares a server. It does not listen; Serve does.
func NewSMTPServer(mailer Mailer, config SMTPConfig) (*SMTPServer, error) {
	if mailer == nil {
		return nil, fmt.Errorf("%w: the SMTP server needs a mailer", ErrInvalid)
	}
	resolved := config.withDefaults()
	ctx, cancel := context.WithCancel(context.Background())
	return &SMTPServer{
		mailer:    mailer,
		config:    resolved,
		logger:    resolved.Logger,
		baseCtx:   ctx,
		cancel:    cancel,
		listeners: make(map[net.Listener]struct{}),
		conns:     make(map[net.Conn]struct{}),
	}, nil
}

// Serve accepts cleartext connections. STARTTLS upgrades them when TLSConfig is
// set. It returns nil after Close.
func (s *SMTPServer) Serve(listener net.Listener) error {
	return s.serve(listener, false)
}

// ServeTLS accepts connections that are already encrypted — the implicit-TLS
// submission port. AUTH is available from the first command.
func (s *SMTPServer) ServeTLS(listener net.Listener) error {
	if s.config.TLSConfig == nil {
		return fmt.Errorf("%w: implicit TLS needs a TLS configuration", ErrInvalid)
	}
	return s.serve(listener, true)
}

func (s *SMTPServer) serve(listener net.Listener, implicitTLS bool) error {
	if !s.addListener(listener) {
		return ErrSMTPServerClosed
	}
	defer s.removeListener(listener)
	for {
		conn, err := listener.Accept()
		if err != nil {
			if s.isClosed() {
				return nil
			}
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				continue
			}
			return fmt.Errorf("maild: accept an SMTP connection: %w", err)
		}
		if implicitTLS {
			conn = tls.Server(conn, s.config.TLSConfig)
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.ServeConn(conn)
		}()
	}
}

// ServeConn runs one session to completion and closes the connection. It is
// exported so a caller with its own accept loop — or a test over net.Pipe — can
// drive a session directly.
func (s *SMTPServer) ServeConn(conn net.Conn) {
	defer func() {
		if err := conn.Close(); err != nil {
			s.logger.Debug("maild: closing an SMTP connection failed", "error", err)
		}
	}()
	// Admission first, and cheaply: a peer over its share must cost this server
	// a socket and one map lookup, not a session.
	remote := remoteAddress(conn)
	lease, err := s.config.Limiter.AcquireSession(remote)
	// Release before the error check on purpose — Lease.Release is nil-safe, so
	// this is correct on the refusal path and cannot leak a slot on any other.
	defer lease.Release()
	if err != nil {
		s.logger.Warn("maild: refused an SMTP session",
			"remote", sanitizeHeaderValue(remote), "reason", err.Error())
		s.refuse(conn, refusalText(err))
		return
	}
	if !s.addConn(conn) {
		s.refuse(conn, "Service not available, closing transmission channel")
		return
	}
	defer s.removeConn(conn)
	defer func() {
		// One malformed session must not take the daemon with it.
		if recovered := recover(); recovered != nil {
			s.logger.Error("maild: an SMTP session panicked", "panic", recovered)
		}
	}()
	newSMTPSession(s, conn).run()
}

// refuse tells a connection arriving over a limit to come back, rather than
// dropping it silently. 421 is the reply RFC 5321 §3.8 reserves for exactly
// this: the service is unavailable now and the peer should retry.
func (s *SMTPServer) refuse(conn net.Conn, text string) {
	if err := conn.SetWriteDeadline(s.config.Now().Add(10 * time.Second)); err != nil {
		return
	}
	if _, err := conn.Write([]byte("421 4.7.0 " + sanitizeReplyText(text) + "\r\n")); err != nil {
		s.logger.Debug("maild: refusing an SMTP connection failed", "error", err)
	}
}

// refusalText is what a peer is told about a limiter refusal. The per-address
// share is named separately because it is the one the peer can do something
// about; both are transient and neither quotes anything the peer sent.
func refusalText(err error) string {
	if errors.Is(err, ErrTooManySessionsFromIP) {
		return "Too many simultaneous connections from your address, please try again later"
	}
	return "Too many connections, please try again later"
}

// remoteAddress is the peer's address, or a stable placeholder for a connection
// that has none. It is the limiter's key, so it must never be empty.
func remoteAddress(conn net.Conn) string {
	if addr := conn.RemoteAddr(); addr != nil {
		if text := addr.String(); text != "" {
			return text
		}
	}
	return "unknown"
}

// Close stops accepting, drops every open session and waits for them to finish.
// It is idempotent.
func (s *SMTPServer) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	listeners := make([]net.Listener, 0, len(s.listeners))
	for listener := range s.listeners {
		listeners = append(listeners, listener)
	}
	conns := make([]net.Conn, 0, len(s.conns))
	for conn := range s.conns {
		conns = append(conns, conn)
	}
	s.mu.Unlock()

	s.cancel()
	var closeErr error
	for _, listener := range listeners {
		if err := listener.Close(); err != nil {
			closeErr = errors.Join(closeErr, err)
		}
	}
	for _, conn := range conns {
		if err := conn.Close(); err != nil {
			s.logger.Debug("maild: closing an SMTP session failed", "error", err)
		}
	}
	s.wg.Wait()
	return closeErr
}

func (s *SMTPServer) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *SMTPServer) addListener(listener net.Listener) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.listeners[listener] = struct{}{}
	return true
}

func (s *SMTPServer) removeListener(listener net.Listener) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.listeners, listener)
}

// addConn tracks a session so Close can drop it. The concurrency limit is not
// checked here: the Limiter owns admission, so there is one place a session
// count is enforced rather than two that can disagree.
func (s *SMTPServer) addConn(conn net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.conns[conn] = struct{}{}
	return true
}

func (s *SMTPServer) removeConn(conn net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.conns, conn)
}

// -----------------------------------------------------------------------------
// Session
// -----------------------------------------------------------------------------

type smtpSession struct {
	server *SMTPServer
	logger *slog.Logger
	conn   net.Conn
	reader *bufio.Reader
	writer *bufio.Writer

	remote   string
	helo     string
	extended bool
	secure   bool
	identity *Identity

	fromSet    bool
	from       string
	recipients []Recipient
	// fanOut counts the recipients in this transaction that expand to more than
	// one delivery. It is what bounds the amplification an unauthenticated
	// message can buy.
	fanOut int

	errorCount int
	// quit ends the session after the current command; writeErr ends it because
	// the connection is already gone. The loop checks both, which is what keeps
	// every handler free of error plumbing.
	quit     bool
	writeErr error
}

func newSMTPSession(server *SMTPServer, conn net.Conn) *smtpSession {
	return &smtpSession{
		server: server,
		logger: server.logger,
		conn:   conn,
		reader: bufio.NewReaderSize(conn, smtpReadBuffer),
		writer: bufio.NewWriterSize(conn, smtpWriteBuffer),
		remote: sanitizeHeaderValue(remoteAddress(conn)),
	}
}

func (t *smtpSession) run() {
	// An implicit-TLS listener hands over a tls.Conn whose handshake has not
	// happened yet. Doing it here means `secure` is true before the banner, so
	// AUTH is available on the submission port from the first command.
	if tlsConn, ok := t.conn.(*tls.Conn); ok {
		if err := t.setDeadline(t.server.config.CommandTimeout); err != nil {
			return
		}
		if err := tlsConn.Handshake(); err != nil {
			t.logger.Debug("maild: the TLS handshake failed", "remote", t.remote, "error", err)
			return
		}
		t.secure = true
	}

	t.reply(220, "", t.server.config.Hostname+" ESMTP ready")
	for t.writeErr == nil && !t.quit {
		if err := t.setDeadline(t.server.config.CommandTimeout); err != nil {
			return
		}
		line, err := readLimitedLine(t.reader, smtpMaxCommandLine)
		if err != nil {
			t.readFailure(err)
			return
		}
		t.command(string(trimEOL(line)))
	}
	if t.writeErr != nil {
		t.logger.Debug("maild: an SMTP session ended on a write error",
			"remote", t.remote, "error", t.writeErr)
	}
}

// readFailure answers what can still be answered on a connection that failed
// mid-command.
func (t *smtpSession) readFailure(err error) {
	switch {
	case errors.Is(err, errLineTooLong):
		t.reply(500, "5.5.2", "That line is too long")
	case isTimeout(err):
		t.reply(421, "4.4.2", t.server.config.Hostname+" closing the connection: timed out")
	default:
		// io.EOF and a reset connection are ordinary; there is nobody to answer.
		t.logger.Debug("maild: an SMTP session ended", "remote", t.remote, "error", err)
	}
}

func (t *smtpSession) command(line string) {
	verb, rest, _ := strings.Cut(line, " ")
	verb = strings.ToUpper(strings.TrimSpace(verb))
	rest = strings.TrimSpace(rest)

	switch verb {
	case "":
		t.fail(500, "5.5.2", "Command not recognised")
	case "QUIT":
		t.reply(221, "2.0.0", t.server.config.Hostname+" closing the connection")
		t.quit = true
	case "NOOP":
		t.reply(250, "2.0.0", "OK")
	case "RSET":
		t.resetTransaction()
		t.reply(250, "2.0.0", "OK")
	case "HELO":
		t.hello(rest, false)
	case "EHLO":
		t.hello(rest, true)
	case "STARTTLS":
		t.starttls(rest)
	case "AUTH":
		t.auth(rest)
	case "MAIL":
		t.mail(rest)
	case "RCPT":
		t.rcpt(rest)
	case "DATA":
		t.data()
	case "VRFY":
		// 252 is the honest answer: this server will not confirm or deny that an
		// address exists to an unauthenticated stranger.
		t.reply(252, "2.5.2", "Cannot verify a user, but will accept the message and attempt delivery")
	case "EXPN":
		t.fail(502, "5.5.1", "EXPN is not available")
	case "HELP":
		t.reply(214, "2.0.0", "Commands: EHLO HELO MAIL RCPT DATA RSET NOOP QUIT STARTTLS AUTH")
	default:
		t.fail(500, "5.5.1", "Command not recognised")
	}
}

func (t *smtpSession) hello(name string, extended bool) {
	if name == "" {
		t.fail(501, "5.5.4", "Expected a domain name after "+map[bool]string{true: "EHLO", false: "HELO"}[extended])
		return
	}
	// A greeting resets any transaction in progress (RFC 5321 §4.1.4).
	t.resetTransaction()
	t.helo = sanitizeHeaderValue(name)
	t.extended = extended
	if !extended {
		t.reply(250, "", t.server.config.Hostname+" greets "+t.helo)
		return
	}
	lines := []string{
		t.server.config.Hostname + " greets " + t.helo,
		"PIPELINING",
		"SIZE " + strconv.FormatInt(t.server.config.MaxMessageBytes, 10),
		"8BITMIME",
		"ENHANCEDSTATUSCODES",
	}
	if t.server.config.TLSConfig != nil && !t.secure {
		lines = append(lines, "STARTTLS")
	}
	if t.secure {
		// Advertised only on an encrypted session, and refused on a cleartext
		// one even if a client asks anyway.
		lines = append(lines, "AUTH PLAIN LOGIN")
	}
	lines = append(lines, "HELP")
	t.replyMulti(250, "", lines)
}

func (t *smtpSession) starttls(rest string) {
	switch {
	case t.server.config.TLSConfig == nil:
		t.fail(454, "4.7.0", "TLS is not available on this server")
		return
	case t.secure:
		t.fail(503, "5.5.1", "TLS is already active")
		return
	case rest != "":
		t.fail(501, "5.5.4", "STARTTLS takes no parameter")
		return
	case t.reader.Buffered() > 0:
		// Anything already buffered was sent before the handshake and would be
		// read as though it had arrived inside the encrypted session. That is
		// the STARTTLS command-injection bug; the only safe answer is to refuse.
		t.reply(501, "5.5.0", "Pipelining across STARTTLS is not allowed")
		t.quit = true
		return
	}
	t.reply(220, "2.0.0", "Ready to start TLS")
	if t.writeErr != nil {
		return
	}
	tlsConn := tls.Server(t.conn, t.server.config.TLSConfig)
	if err := t.setDeadline(t.server.config.CommandTimeout); err != nil {
		t.quit = true
		return
	}
	if err := tlsConn.Handshake(); err != nil {
		t.logger.Debug("maild: the STARTTLS handshake failed", "remote", t.remote, "error", err)
		t.quit = true
		return
	}
	// RFC 3207 §4.2: everything negotiated before the handshake is discarded.
	t.conn = tlsConn
	t.reader = bufio.NewReaderSize(tlsConn, smtpReadBuffer)
	t.writer = bufio.NewWriterSize(tlsConn, smtpWriteBuffer)
	t.secure = true
	t.helo = ""
	t.extended = false
	t.identity = nil
	t.resetTransaction()
}

// -----------------------------------------------------------------------------
// AUTH
// -----------------------------------------------------------------------------

func (t *smtpSession) auth(rest string) {
	// This check comes first, before the mechanism name is even looked at, so a
	// credential is never parsed off an unencrypted connection.
	if !t.secure {
		t.fail(538, "5.7.11", "Encryption required for that authentication mechanism")
		return
	}
	switch {
	case t.helo == "":
		t.fail(503, "5.5.1", "Send EHLO first")
		return
	case t.identity != nil:
		t.fail(503, "5.5.1", "Already authenticated")
		return
	case t.fromSet:
		t.fail(503, "5.5.1", "AUTH is not allowed during a mail transaction")
		return
	case rest == "":
		t.fail(501, "5.5.4", "Expected an authentication mechanism")
		return
	}

	mechanism, initial, _ := strings.Cut(rest, " ")
	var username, password string
	var ok bool
	switch strings.ToUpper(strings.TrimSpace(mechanism)) {
	case "PLAIN":
		username, password, ok = t.authPlain(strings.TrimSpace(initial))
	case "LOGIN":
		username, password, ok = t.authLogin(strings.TrimSpace(initial))
	default:
		t.fail(504, "5.5.4", "That authentication mechanism is not supported")
		return
	}
	if !ok {
		return
	}

	limiter := t.server.config.Limiter
	// Before the verification, never after. Mailer.Authenticate is PBKDF2 at the
	// production work factor and deliberately pays the same cost for a mailbox
	// that does not exist, so a peer guessing in a loop is a CPU-exhaustion
	// attack as much as a brute-force one. This is a map lookup; it has to come
	// first or the lockout is itself the thing being attacked.
	if err := limiter.AllowAuth(t.remote, username); err != nil {
		t.logger.Info("maild: refused an SMTP authentication attempt at the failure limit",
			"remote", t.remote)
		t.fail(454, "4.7.0", "Too many failed authentication attempts, please try again later")
		return
	}

	ctx, cancel := context.WithTimeout(t.server.baseCtx, 30*time.Second)
	identity, err := t.server.mailer.Authenticate(ctx, username, password)
	cancel()
	if err != nil {
		// Recorded for every failure, including a username that does not exist:
		// an attacker enumerating names is exactly the case this is for.
		limiter.RecordAuthFailure(t.remote, username)
		code, enhanced, message := replyFor(err)
		// Only the outcome is logged. The username is the customer's address and
		// the password must never reach a log line, an error string or a reply.
		t.logger.Info("maild: an SMTP authentication attempt failed",
			"remote", t.remote, "code", code)
		t.fail(code, enhanced, message)
		return
	}
	limiter.RecordAuthSuccess(t.remote, username)
	t.identity = &identity
	t.reply(235, "2.7.0", "Authentication successful")
}

// authPlain reads a SASL PLAIN credential, either inline or after a challenge.
func (t *smtpSession) authPlain(initial string) (string, string, bool) {
	encoded := initial
	if encoded == "" {
		t.reply(334, "", "")
		if t.writeErr != nil {
			return "", "", false
		}
		line, ok := t.readAuthLine()
		if !ok {
			return "", "", false
		}
		encoded = line
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.fail(501, "5.5.2", "That authentication response is not valid base64")
		return "", "", false
	}
	// authzid NUL authcid NUL password
	parts := bytes.SplitN(decoded, []byte{0}, 3)
	if len(parts) != 3 {
		t.fail(501, "5.5.2", "That authentication response is malformed")
		return "", "", false
	}
	username := string(parts[1])
	if username == "" {
		username = string(parts[0])
	}
	return username, string(parts[2]), true
}

// authLogin reads a SASL LOGIN credential. The mechanism is obsolete but is
// still the only one some desktop clients offer.
func (t *smtpSession) authLogin(initial string) (string, string, bool) {
	username, ok := t.authLoginField(initial, "Username:")
	if !ok {
		return "", "", false
	}
	password, ok := t.authLoginField("", "Password:")
	if !ok {
		return "", "", false
	}
	return username, password, true
}

func (t *smtpSession) authLoginField(initial, prompt string) (string, bool) {
	encoded := initial
	if encoded == "" {
		t.reply(334, "", base64.StdEncoding.EncodeToString([]byte(prompt)))
		if t.writeErr != nil {
			return "", false
		}
		line, ok := t.readAuthLine()
		if !ok {
			return "", false
		}
		encoded = line
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.fail(501, "5.5.2", "That authentication response is not valid base64")
		return "", false
	}
	return string(decoded), true
}

// readAuthLine reads one SASL continuation. "*" is the client cancelling, which
// is an answer rather than an error.
func (t *smtpSession) readAuthLine() (string, bool) {
	if err := t.setDeadline(t.server.config.CommandTimeout); err != nil {
		t.quit = true
		return "", false
	}
	line, err := readLimitedLine(t.reader, smtpMaxCommandLine)
	if err != nil {
		t.readFailure(err)
		t.quit = true
		return "", false
	}
	value := strings.TrimSpace(string(line))
	if value == "*" {
		t.fail(501, "5.7.0", "Authentication cancelled")
		return "", false
	}
	return value, true
}

// -----------------------------------------------------------------------------
// The mail transaction
// -----------------------------------------------------------------------------

func (t *smtpSession) mail(rest string) {
	switch {
	case t.helo == "":
		t.fail(503, "5.5.1", "Send EHLO or HELO first")
		return
	case t.fromSet:
		t.fail(503, "5.5.1", "A mail transaction is already open")
		return
	}
	address, params, ok := parsePathCommand(rest, "FROM")
	if !ok {
		t.fail(501, "5.5.4", "Expected MAIL FROM:<address>")
		return
	}
	for _, parameter := range params {
		key, value, _ := strings.Cut(parameter, "=")
		switch strings.ToUpper(key) {
		case "SIZE":
			declared, err := strconv.ParseInt(value, 10, 64)
			if err != nil || declared < 0 {
				t.fail(501, "5.5.4", "That SIZE parameter is not a number")
				return
			}
			if declared > t.server.config.MaxMessageBytes {
				t.fail(552, "5.3.4", "Message size exceeds the fixed limit of "+
					strconv.FormatInt(t.server.config.MaxMessageBytes, 10)+" bytes")
				return
			}
		case "BODY":
			switch strings.ToUpper(value) {
			case "7BIT", "8BITMIME":
			default:
				t.fail(501, "5.5.4", "That BODY parameter is not supported")
				return
			}
		case "AUTH", "RET", "ENVID":
			// Accepted and ignored: they carry no obligation this server fails
			// to meet, and refusing them would break ordinary clients.
		default:
			t.fail(555, "5.5.4", "That MAIL parameter is not supported")
			return
		}
	}
	// The empty path is the null sender RFC 5321 §4.5.5 requires for a bounce.
	// Everything else is a single addr-spec by address.go's rules, which is the
	// one parser every entry point into this package shares.
	if address != "" && !ValidAddress(address) {
		t.fail(501, "5.1.7", "That sender address is not valid")
		return
	}
	// An authenticated sender may only use an address it owns. Without this one
	// customer could send as another, from inside the same server.
	if t.identity != nil && address != "" && !t.identity.MayUse(address) {
		t.fail(550, "5.7.1", "You are not allowed to send from that address")
		return
	}
	t.fromSet = true
	t.from = address
	t.reply(250, "2.1.0", "Sender OK")
}

func (t *smtpSession) rcpt(rest string) {
	if !t.fromSet {
		t.fail(503, "5.5.1", "Send MAIL FROM first")
		return
	}
	if len(t.recipients) >= t.server.config.MaxRecipients {
		t.fail(452, "4.5.3", "Too many recipients")
		return
	}
	address, params, ok := parsePathCommand(rest, "TO")
	if !ok || address == "" {
		t.fail(501, "5.5.4", "Expected RCPT TO:<address>")
		return
	}
	for _, parameter := range params {
		key, _, _ := strings.Cut(parameter, "=")
		switch strings.ToUpper(key) {
		case "ORCPT", "NOTIFY":
		default:
			t.fail(555, "5.5.4", "That RCPT parameter is not supported")
			return
		}
	}
	if !ValidAddress(address) {
		t.fail(501, "5.1.3", "That recipient address is not valid")
		return
	}

	ctx, cancel := context.WithTimeout(t.server.baseCtx, 30*time.Second)
	recipient, err := t.server.mailer.Resolve(ctx, address, t.identity != nil)
	cancel()
	if err != nil {
		code, enhanced, message := replyFor(err)
		t.fail(code, enhanced, message)
		return
	}
	for _, existing := range t.recipients {
		if strings.EqualFold(existing.Address, recipient.Address) {
			// A duplicate is accepted and dropped: the sender asked for one
			// copy per address, not one per mention.
			t.reply(250, "2.1.5", "Recipient OK")
			return
		}
	}
	// Counted after the duplicate check, so naming one forwarder twice is one
	// fan-out. The refusal is temporary: a sender with a real reason to reach
	// several forwarders splits the message and every copy is delivered, while
	// a sender building an amplifier gets nothing it can multiply.
	if recipient.Kind == RecipientList && t.identity == nil {
		if t.fanOut >= smtpMaxUnauthenticatedFanOut {
			t.fail(452, "4.5.3", "Too many forwarding recipients for one message")
			return
		}
		t.fanOut++
	}
	t.recipients = append(t.recipients, recipient)
	t.reply(250, "2.1.5", "Recipient OK")
}

func (t *smtpSession) data() {
	switch {
	case !t.fromSet:
		t.fail(503, "5.5.1", "Send MAIL FROM first")
		return
	case len(t.recipients) == 0:
		t.fail(503, "5.5.1", "Send RCPT TO first")
		return
	}

	// readMessage accumulates the whole body in memory, so the memory this
	// process can be made to hold is the message limit times the number of
	// sessions in DATA at once. Reserve before the 354, and hold the
	// reservation until the body has been handed to delivery and is no longer
	// referenced. 452 is RFC 5321's "insufficient system storage": transient,
	// and true.
	lease, err := t.server.config.Limiter.AcquireMessage()
	defer lease.Release()
	if err != nil {
		t.logger.Warn("maild: refused a message at the memory limit", "remote", t.remote)
		t.fail(452, "4.3.1", "Insufficient system storage, please try again later")
		return
	}

	t.reply(354, "", "Start mail input; end with <CRLF>.<CRLF>")
	if t.writeErr != nil {
		return
	}

	body, err := t.readMessage()
	switch {
	case errors.Is(err, errMessageTooLarge):
		// The whole message was read, so the session is still in sync and the
		// client can start another transaction.
		t.resetTransaction()
		t.fail(552, "5.3.4", "Message size exceeds the fixed limit of "+
			strconv.FormatInt(t.server.config.MaxMessageBytes, 10)+" bytes")
		return
	case errors.Is(err, errRunawayMessage):
		t.reply(552, "5.3.4", "Message size exceeds the fixed limit and never ended")
		t.quit = true
		return
	case err != nil:
		t.readFailure(err)
		t.quit = true
		return
	}

	id, err := randomID("m")
	if err != nil {
		t.logger.Error("maild: could not mint a message identifier", "error", err)
		t.resetTransaction()
		t.fail(451, "4.3.0", "Temporary local problem, please try again later")
		return
	}
	now := t.server.config.Now().UTC()
	envelope := &Envelope{
		ID:         id,
		ReturnPath: t.from,
		Recipients: t.recipients,
		Received:   t.receivedHeader(id, now),
		Data:       body,
		ReceivedAt: now,
		Identity:   t.identity,
		RemoteAddr: t.remote,
		HELO:       t.helo,
		TLS:        t.secure,
	}

	ctx, cancel := context.WithTimeout(t.server.baseCtx, 2*time.Minute)
	deliverErr := t.server.mailer.Deliver(ctx, envelope)
	cancel()
	t.resetTransaction()
	if deliverErr != nil {
		code, enhanced, message := replyFor(deliverErr)
		t.logger.Warn("maild: a message was not accepted",
			"remote", t.remote, "message", id, "code", code, "error", deliverErr)
		t.fail(code, enhanced, message)
		return
	}
	t.logger.Info("maild: accepted a message",
		"remote", t.remote, "message", id, "recipients", len(envelope.Recipients), "bytes", len(body))
	t.reply(250, "2.0.0", "Message accepted for delivery as "+id)
}

// readMessage reads DATA to the final dot.
//
// An oversized message is still read to its end so the session stays in sync,
// but only up to a bounded overrun: a client that never sends the dot is a
// client trying to make this server read forever.
func (t *smtpSession) readMessage() ([]byte, error) {
	limit := t.server.config.MaxMessageBytes
	var body bytes.Buffer
	var total int64
	oversize := false
	for {
		if err := t.setDeadline(t.server.config.DataTimeout); err != nil {
			return nil, err
		}
		line, err := readLimitedLine(t.reader, smtpMaxDataLine)
		if err != nil {
			return nil, err
		}
		content := trimEOL(line)
		if len(content) == 1 && content[0] == '.' {
			break
		}
		// RFC 5321 §4.5.2: a leading dot was added by the sender.
		if len(content) > 0 && content[0] == '.' {
			content = content[1:]
		}
		total += int64(len(content)) + 2
		if total > limit {
			oversize = true
			if total > limit+smtpDrainAllowance {
				return nil, errRunawayMessage
			}
			continue
		}
		body.Write(content)
		body.WriteString("\r\n")
	}
	if oversize {
		return nil, errMessageTooLarge
	}
	return body.Bytes(), nil
}

// receivedHeader is the trace header RFC 5321 §4.4 requires. The "for" clause
// only appears when there is exactly one recipient, because with several it
// would tell each of them who the others were.
func (t *smtpSession) receivedHeader(id string, now time.Time) string {
	protocol := "SMTP"
	if t.extended {
		protocol = "ESMTP"
	}
	if t.secure {
		protocol += "S"
	}
	if t.identity != nil {
		protocol += "A"
	}
	helo := t.helo
	if helo == "" {
		helo = "unknown"
	}
	lines := []string{
		"Received: from " + helo + " (" + t.remote + ")",
		"\tby " + t.server.config.Hostname + " with " + protocol + " id " + id,
	}
	if len(t.recipients) == 1 {
		lines = append(lines, "\tfor <"+sanitizeHeaderValue(t.recipients[0].Address)+">")
	}
	return strings.Join(lines, "\r\n") + ";\r\n\t" + now.Format(time.RFC1123Z) + "\r\n"
}

func (t *smtpSession) resetTransaction() {
	t.fromSet = false
	t.from = ""
	t.recipients = nil
	t.fanOut = 0
}

// -----------------------------------------------------------------------------
// Replies
// -----------------------------------------------------------------------------

func (t *smtpSession) reply(code int, enhanced string, text string) {
	t.replyMulti(code, enhanced, []string{text})
}

func (t *smtpSession) replyMulti(code int, enhanced string, lines []string) {
	if t.writeErr != nil {
		return
	}
	if len(lines) == 0 {
		lines = []string{""}
	}
	var out strings.Builder
	for index, line := range lines {
		separator := "-"
		if index == len(lines)-1 {
			separator = " "
		}
		out.WriteString(strconv.Itoa(code))
		out.WriteString(separator)
		if enhanced != "" {
			out.WriteString(enhanced)
			out.WriteString(" ")
		}
		out.WriteString(sanitizeReplyText(line))
		out.WriteString("\r\n")
	}
	if err := t.conn.SetWriteDeadline(t.server.config.Now().Add(t.server.config.CommandTimeout)); err != nil {
		t.writeErr = err
		return
	}
	if _, err := t.writer.WriteString(out.String()); err != nil {
		t.writeErr = err
		return
	}
	if err := t.writer.Flush(); err != nil {
		t.writeErr = err
	}
}

// fail replies and counts against the session's error budget, so a client that
// only ever guesses is eventually dropped.
func (t *smtpSession) fail(code int, enhanced, text string) {
	t.errorCount++
	t.reply(code, enhanced, text)
	if t.errorCount >= t.server.config.MaxErrors {
		t.reply(421, "4.7.0", "Too many errors on this connection")
		t.quit = true
	}
}

func (t *smtpSession) setDeadline(timeout time.Duration) error {
	if err := t.conn.SetDeadline(t.server.config.Now().Add(timeout)); err != nil {
		t.logger.Debug("maild: could not set a connection deadline", "error", err)
		return err
	}
	return nil
}

// -----------------------------------------------------------------------------
// Line reading and parsing
// -----------------------------------------------------------------------------

// readLimitedLine reads one line and refuses to buffer more than max bytes,
// which is what stops a client from making the server allocate without ever
// sending a newline.
func readLimitedLine(reader *bufio.Reader, max int) ([]byte, error) {
	var line []byte
	for {
		chunk, err := reader.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			line = append(line, chunk...)
			if len(line) > max {
				return nil, errLineTooLong
			}
			continue
		}
		if err != nil {
			return nil, err
		}
		line = append(line, chunk...)
		if len(line) > max {
			return nil, errLineTooLong
		}
		return line, nil
	}
}

func trimEOL(line []byte) []byte {
	line = bytes.TrimSuffix(line, []byte("\n"))
	return bytes.TrimSuffix(line, []byte("\r"))
}

// parsePathCommand parses "FROM:<address> PARAM=value" and "TO:<address> …".
func parsePathCommand(rest, keyword string) (address string, params []string, ok bool) {
	if len(rest) < len(keyword) || !strings.EqualFold(rest[:len(keyword)], keyword) {
		return "", nil, false
	}
	rest = strings.TrimLeft(rest[len(keyword):], " \t")
	if !strings.HasPrefix(rest, ":") {
		return "", nil, false
	}
	rest = strings.TrimLeft(rest[1:], " \t")
	if !strings.HasPrefix(rest, "<") {
		return "", nil, false
	}
	end := strings.Index(rest, ">")
	if end < 0 {
		return "", nil, false
	}
	path := rest[1:end]
	if remainder := strings.TrimSpace(rest[end+1:]); remainder != "" {
		params = strings.Fields(remainder)
	}
	// RFC 5321 §4.1.1.2: a source route is accepted and ignored.
	if strings.HasPrefix(path, "@") {
		if colon := strings.Index(path, ":"); colon >= 0 {
			path = path[colon+1:]
		}
	}
	// Spaces only, deliberately: strings.TrimSpace would also eat a trailing CR
	// or tab, which is the header injection ParseAddress exists to catch. Left
	// in place, those characters reach the address parser and are refused.
	return strings.Trim(path, " "), params, true
}

// plausibleAddress is the loose syntax gate this server used before address.go
// existed: it rejects the shapes that would break something downstream — a
// missing or repeated "@", a separator, a control character, an over-long part
// — and accepts the rest.
//
// The session no longer uses it. ValidAddress is strictly stricter (address.go
// pins that relationship in a test), and the difference is the point: an
// address literal and a 320-octet address are now refused at MAIL FROM and
// RCPT TO. This is kept only as the reference the comparison test measures
// against, so the two parsers cannot drift apart unnoticed.
func plausibleAddress(address string) bool {
	if address == "" || len(address) > 320 {
		return false
	}
	local, domain, found := strings.Cut(address, "@")
	if !found || local == "" || domain == "" {
		return false
	}
	if len(local) > 64 || len(domain) > 255 || strings.Contains(domain, "@") {
		return false
	}
	for _, r := range address {
		if r < 0x20 || r == 0x7f || r == ' ' || r == '<' || r == '>' || r == ',' || r == ';' {
			return false
		}
	}
	return !strings.HasPrefix(domain, ".") && !strings.HasSuffix(domain, ".") &&
		!strings.Contains(domain, "..")
}

// sanitizeReplyText keeps client-supplied text from breaking the reply grammar.
// A CR or LF here would let a remote client inject a reply line of its own.
func sanitizeReplyText(text string) string {
	if len(text) > smtpMaxReplyText {
		text = text[:smtpMaxReplyText]
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, text)
}

// sanitizeHeaderValue keeps client-supplied text from breaking the Received
// header it is quoted into.
func sanitizeHeaderValue(value string) string {
	value = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, strings.TrimSpace(value))
	if len(value) > 255 {
		value = value[:255]
	}
	return value
}

func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}
