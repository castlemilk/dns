// POP3, and why this server speaks it instead of IMAP.
//
// The outcome a customer wants is "read your mail in a mail client". IMAP buys
// that plus server-side folders, flags, search, partial fetches and idle
// notification — and it costs, in round numbers, an order of magnitude more
// protocol: a parser for the fetch grammar, literals, sequence-set arithmetic,
// per-mailbox flag and UID state that has to stay consistent across concurrent
// sessions, and CONDSTORE/QRESYNC before a real client stops resynchronising
// everything. POP3 is nine commands over a line protocol with one piece of
// per-session state (the deleted set) and no server-side state at all.
//
// For a lightweight server whose promise is "real email", POP3 delivers the
// whole promise for a fraction of the surface, and every surface we do not
// write is a place a bug cannot be. The trade is real and worth naming: POP3
// gives no multi-device sync and no server-side folders. Webmail and IMAP, if
// they are ever wanted, are additive — the maildir this reads is the standard
// layout an IMAP server would read too, so nothing here has to be undone.
//
// (A file comment, not the package doc comment: doc.go owns that. It is here
// because it is the decision the rest of this file rests on.)

package maild

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// -----------------------------------------------------------------------------
// The spool
// -----------------------------------------------------------------------------

// Maildir reads the message spool the delivery path writes. The layout is
// delivery.go's, unchanged:
//
//	<DeliveryConfig.Root>/mailboxes/<account id>/{tmp,new,cur}
//
// Maildir is used rather than mbox because delivery needs no lock: the delivery
// path writes into tmp/ and renames into new/, and rename within a filesystem
// is atomic, so a reader can never see half a message and two deliveries can
// never interleave. That is what lets this reader run with no coordination with
// the SMTP side at all — the two share a directory and nothing else.
//
// Mailboxes are keyed by account id, not by address, for the reason delivery.go
// gives: an id is base32 over a fixed alphabet, so it cannot contain a
// separator and a crafted address cannot escape the root.
//
// Two properties of what delivery.go writes this reader depends on:
//
//   - a file is one complete RFC 5322 message with CRLF line endings. POP3
//     frames a message by a line containing a single dot, so a bare LF would
//     let a body line end a transfer early. This reader normalises defensively,
//     but STAT and LIST report file sizes, so a spool that did not hold CRLF
//     would misreport them;
//   - the name up to the first ':' is unique and never changes. It is the
//     UIDL, so a client uses it to decide what it has already downloaded, and
//     it must survive a message moving from new/ to cur/. delivery.go's
//     "<unix>.M<usec>P<pid>Q<seq>.<host>,S=<size>" satisfies that: the flag
//     suffix a mover appends starts at the ':'.
type Maildir struct {
	root string
}

// NewMaildir names a spool root: the same DeliveryConfig.Root the Deliverer was
// given, not the mailboxes directory inside it.
func NewMaildir(root string) Maildir {
	return Maildir{root: filepath.Clean(root)}
}

// Root is the delivery root.
func (m Maildir) Root() string { return m.root }

// Path is one mailbox's directory, the same path Deliverer.MailboxDir returns.
func (m Maildir) Path(accountID string) string {
	return filepath.Join(m.root, MailboxesDir, accountID)
}

// Provision creates a mailbox's tmp, new and cur directories. Delivery creates
// them too, so this only matters for a mailbox nothing has been delivered to
// yet; it is safe to call repeatedly.
func (m Maildir) Provision(accountID string) error {
	base := m.Path(accountID)
	for _, name := range []string{maildirTmp, maildirNew, maildirCur} {
		if err := os.MkdirAll(filepath.Join(base, name), mailDirPerm); err != nil {
			return fmt.Errorf("maild: create the maildir for a mailbox: %w", err)
		}
	}
	return nil
}

// MaildirMessage is one stored message.
type MaildirMessage struct {
	// UID is the POP3 UIDL: printable ASCII, stable for the life of the
	// message. It is a digest of the maildir unique name rather than the name
	// itself, because a maildir name can exceed the 70 octets UIDL allows and
	// can contain the ':' and ',' a flag suffix adds.
	UID string
	// Path is the file.
	Path string
	// Size is the file size in octets, which is what STAT and LIST report.
	Size int64
}

// Messages lists a mailbox in delivery order: new/ and cur/ together, ordered
// by the maildir unique name, which begins with the delivery timestamp.
//
// A mailbox that has never been delivered to has no directory; that is an empty
// maildrop, not an error, because a mailbox created a moment ago is a real
// mailbox a customer may well log into first.
func (m Maildir) Messages(accountID string) ([]MaildirMessage, error) {
	base := m.Path(accountID)
	var messages []MaildirMessage
	for _, folder := range []string{maildirNew, maildirCur} {
		entries, err := os.ReadDir(filepath.Join(base, folder))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("maild: read a maildir: %w", err)
		}
		for _, entry := range entries {
			name := entry.Name()
			// Maildir reserves leading dots for folders and for the state
			// files other tools drop in.
			if entry.IsDir() || strings.HasPrefix(name, ".") {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					// Delivered and removed between the listing and the stat.
					continue
				}
				return nil, fmt.Errorf("maild: stat a maildir message: %w", err)
			}
			messages = append(messages, MaildirMessage{
				UID:  maildirUID(name),
				Path: filepath.Join(base, folder, name),
				Size: info.Size(),
			})
		}
	}
	sort.Slice(messages, func(i, j int) bool {
		left, right := filepath.Base(messages[i].Path), filepath.Base(messages[j].Path)
		if leftKey, rightKey := maildirUnique(left), maildirUnique(right); leftKey != rightKey {
			return leftKey < rightKey
		}
		return left < right
	})
	return messages, nil
}

// maildirUnique is the part of a maildir file name before the flag suffix. It
// does not change when a message moves from new/ to cur/, which is what makes
// the UIDL stable across sessions.
func maildirUnique(name string) string {
	if index := strings.IndexByte(name, ':'); index >= 0 {
		return name[:index]
	}
	return name
}

// maildirUID is the UIDL for a maildir file: 32 hex characters, which is inside
// the 70-octet, 0x21-0x7E alphabet RFC 1939 allows, whatever the file is called.
func maildirUID(name string) string {
	digest := sha256.Sum256([]byte(maildirUnique(name)))
	return hex.EncodeToString(digest[:16])
}

// -----------------------------------------------------------------------------
// The server
// -----------------------------------------------------------------------------

// POP3 limits and defaults.
const (
	// DefaultPOP3Port is the implicit-TLS port, the one the facade publishes as
	// _pop3s._tcp when a binding asks for client autoconfiguration.
	DefaultPOP3Port = 995
	// pop3MaxCommand is the command line limit. RFC 1939 caps a command at 255
	// octets; the extra room absorbs a long USER without changing behaviour.
	pop3MaxCommand = 512
	// defaultPOP3IdleTimeout is the autologout timer. RFC 1939 requires at
	// least 10 minutes.
	defaultPOP3IdleTimeout = 10 * time.Minute
)

// POP3Server serves the maildrop. One server handles every domain in the store;
// a session names its mailbox by full address.
type POP3Server struct {
	store    *Store
	spool    Maildir
	hostname string
	logger   *slog.Logger
	tls      *tls.Config
	idle     time.Duration
	// limiter is the admission control: connection slots per source address,
	// and the failed-attempt lockout that has to be consulted before the KDF
	// runs. It is shared with the SMTP listener when the server is wired from
	// one place, so a peer guessing on both ports is one peer.
	limiter *Limiter

	// mu guards locked, the set of mailboxes with a session open. RFC 1939
	// requires exclusive access for the length of a session: two sessions
	// sharing a maildrop would each number the same messages and each delete
	// by number.
	mu     sync.Mutex
	locked map[string]struct{}
}

// POP3Option customises a server.
type POP3Option func(*POP3Server)

// POP3WithLogger sets the logger. Nothing it is given is ever a credential.
func POP3WithLogger(logger *slog.Logger) POP3Option {
	return func(s *POP3Server) {
		if logger != nil {
			s.logger = logger
		}
	}
}

// POP3WithHostname sets the name in the greeting. It should be MAIL_HOSTNAME:
// that is the name the published _pop3s._tcp SRV record points at, and the name
// the TLS certificate has to match.
func POP3WithHostname(hostname string) POP3Option {
	return func(s *POP3Server) {
		if trimmed := strings.TrimSpace(hostname); trimmed != "" {
			s.hostname = trimmed
		}
	}
}

// POP3WithTLS supplies the TLS configuration. Serve wraps its listener with it,
// which is implicit TLS on port 995 — there is deliberately no STLS, because a
// STARTTLS-style upgrade on a cleartext port is strippable by anything on the
// path and the credential here is a mailbox password.
func POP3WithTLS(config *tls.Config) POP3Option {
	return func(s *POP3Server) { s.tls = config }
}

// POP3WithLimiter shares one Limiter with this server. Without it the server
// builds its own at the default policy, because a listener with no admission
// control is not a safe default for a port on the public internet.
func POP3WithLimiter(limiter *Limiter) POP3Option {
	return func(s *POP3Server) {
		if limiter != nil {
			s.limiter = limiter
		}
	}
}

// POP3WithIdleTimeout replaces the autologout timer.
func POP3WithIdleTimeout(timeout time.Duration) POP3Option {
	return func(s *POP3Server) {
		if timeout > 0 {
			s.idle = timeout
		}
	}
}

// NewPOP3Server builds a server over a store and a spool.
func NewPOP3Server(store *Store, spool Maildir, options ...POP3Option) *POP3Server {
	server := &POP3Server{
		store:    store,
		spool:    spool,
		hostname: "localhost",
		logger:   slog.Default(),
		idle:     defaultPOP3IdleTimeout,
		locked:   make(map[string]struct{}),
		limiter:  NewLimiter(LimiterConfig{}),
	}
	for _, option := range options {
		option(server)
	}
	return server
}

// Serve accepts connections until ctx is cancelled or the listener fails. When
// the server has a TLS configuration the listener is wrapped, so every session
// is encrypted from the first byte.
func (s *POP3Server) Serve(ctx context.Context, listener net.Listener) error {
	if s.tls != nil {
		listener = tls.NewListener(listener, s.tls)
	}

	var wait sync.WaitGroup
	defer wait.Wait()

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			if err := listener.Close(); err != nil {
				s.logger.Debug("maild pop3: close the listener", "error", err)
			}
		case <-done:
		}
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("maild: accept a POP3 connection: %w", err)
		}
		wait.Add(1)
		go func() {
			defer wait.Done()
			s.ServeConn(ctx, conn)
		}()
	}
}

// ServeConn runs one session and closes the connection.
func (s *POP3Server) ServeConn(ctx context.Context, conn net.Conn) {
	defer func() {
		if err := conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			s.logger.Debug("maild pop3: close a connection", "error", err)
		}
	}()

	remote := remoteAddress(conn)

	// Admission before anything else. A peer that opens sockets and says
	// nothing would otherwise hold a maildrop's worth of session state, and one
	// peer holding every slot is a complete outage for everyone else.
	lease, err := s.limiter.AcquireSession(remote)
	// Nil-safe, so this is correct on the refusal path too.
	defer lease.Release()
	if err != nil {
		s.logger.Warn("maild pop3: refused a session", "remote", remote, "reason", err.Error())
		s.refuse(conn)
		return
	}

	// A TLS connection is one this server can accept a password on. Anything
	// else gets the protocol but not the credential.
	_, secure := conn.(interface{ ConnectionState() tls.ConnectionState })

	if err := s.session(ctx, &pop3Deadline{conn: conn, idle: s.idle}, secure, remote); err != nil {
		s.logger.Debug("maild pop3: session ended", "remote", remote, "error", err)
	}
}

// refuse answers a connection arriving over a limit before dropping it, so a
// client sees a reason it can retry on rather than a reset. RFC 3206's SYS/TEMP
// is "a temporary system problem, try again"; it names no mailbox, which is
// what keeps the refusal from being an oracle.
func (s *POP3Server) refuse(conn net.Conn) {
	if s.idle > 0 {
		if err := conn.SetWriteDeadline(time.Now().Add(s.idle)); err != nil {
			return
		}
	}
	if _, err := conn.Write([]byte("-ERR [SYS/TEMP] too many connections, try again later\r\n")); err != nil {
		s.logger.Debug("maild pop3: refuse a connection", "error", err)
	}
}

// pop3Deadline refreshes the connection's deadlines around every read and
// write, which is the autologout timer RFC 1939 asks for and the thing that
// stops an abandoned session holding a maildrop lock forever.
type pop3Deadline struct {
	conn net.Conn
	idle time.Duration
}

func (d *pop3Deadline) Read(buffer []byte) (int, error) {
	if d.idle > 0 {
		if err := d.conn.SetReadDeadline(time.Now().Add(d.idle)); err != nil {
			return 0, err
		}
	}
	return d.conn.Read(buffer)
}

func (d *pop3Deadline) Write(buffer []byte) (int, error) {
	if d.idle > 0 {
		if err := d.conn.SetWriteDeadline(time.Now().Add(d.idle)); err != nil {
			return 0, err
		}
	}
	return d.conn.Write(buffer)
}

// -----------------------------------------------------------------------------
// The session
// -----------------------------------------------------------------------------

// pop3Entry is one message in the session's view of the maildrop. The view is
// taken once, at login: RFC 1939 numbers messages for the length of a session,
// so a delivery that arrives mid-session must not renumber anything.
type pop3Entry struct {
	MaildirMessage
	deleted bool
}

type pop3Session struct {
	server *POP3Server
	reader *bufio.Reader
	writer *bufio.Writer
	secure bool
	remote string

	// user is the address named by USER, remembered only until PASS.
	user string
	// account, domain and messages are set once the session authenticates.
	account  Account
	domain   Domain
	messages []pop3Entry
	// lock is the maildrop lock key, empty until it is held.
	lock string
}

var errPOP3Quit = errors.New("maild: the client sent QUIT")

// session runs the protocol over rw. It takes secure as an argument rather than
// sniffing rw so the state machine can be exercised without a socket.
func (s *POP3Server) session(ctx context.Context, rw io.ReadWriter, secure bool, remote string) error {
	session := &pop3Session{
		server: s,
		reader: bufio.NewReaderSize(rw, pop3MaxCommand*2),
		writer: bufio.NewWriter(rw),
		secure: secure,
		remote: remote,
	}
	defer session.release()

	session.ok(s.hostname + " POP3 ready")
	if err := session.writer.Flush(); err != nil {
		return err
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		line, err := session.readCommand()
		if err != nil {
			if errors.Is(err, errPOP3LineTooLong) {
				session.err("the command line is too long")
				return session.writer.Flush()
			}
			return err
		}

		verb, argument := splitPOP3Command(line)
		handleErr := session.dispatch(ctx, verb, argument)
		if flushErr := session.writer.Flush(); flushErr != nil {
			return flushErr
		}
		if handleErr != nil {
			if errors.Is(handleErr, errPOP3Quit) {
				return nil
			}
			return handleErr
		}
	}
}

var errPOP3LineTooLong = errors.New("maild: the POP3 command line is too long")

// readCommand reads one CRLF-terminated command, bounded so a client cannot
// make the server buffer without limit.
func (s *pop3Session) readCommand() (string, error) {
	line, err := s.reader.ReadSlice('\n')
	if err != nil {
		if errors.Is(err, bufio.ErrBufferFull) {
			return "", errPOP3LineTooLong
		}
		return "", err
	}
	if len(line) > pop3MaxCommand {
		return "", errPOP3LineTooLong
	}
	return strings.TrimRight(string(line), "\r\n"), nil
}

// splitPOP3Command cuts a command into its uppercased verb and its argument.
func splitPOP3Command(line string) (verb, argument string) {
	verb, argument, _ = strings.Cut(strings.TrimLeft(line, " "), " ")
	return strings.ToUpper(verb), strings.TrimSpace(argument)
}

// dispatch runs one command. The state machine is explicit: a command legal in
// TRANSACTION is refused in AUTHORIZATION and the other way round, so no path
// can reach a maildrop the session has not authenticated to.
func (s *pop3Session) dispatch(ctx context.Context, verb, argument string) error {
	switch verb {
	case "QUIT":
		return s.quit()
	case "CAPA":
		s.capa()
		return nil
	case "NOOP":
		if !s.authenticated() {
			s.err("authenticate first")
			return nil
		}
		s.ok("")
		return nil
	}

	if !s.authenticated() {
		switch verb {
		case "USER":
			s.userCommand(argument)
		case "PASS":
			s.passCommand(ctx, argument)
		case "APOP":
			// APOP needs the password recoverable to compute the digest, and
			// this store holds only a PBKDF2 digest. Refusing is the honest
			// answer; offering it would mean storing passwords reversibly.
			s.err("APOP is not supported; use USER and PASS over TLS")
		case "STLS":
			s.err("this port is implicit TLS; there is no STLS")
		default:
			s.err("authenticate first")
		}
		return nil
	}

	switch verb {
	case "STAT":
		s.stat()
	case "LIST":
		s.list(argument)
	case "UIDL":
		s.uidl(argument)
	case "RETR":
		s.retr(argument)
	case "TOP":
		s.top(argument)
	case "DELE":
		s.dele(argument)
	case "RSET":
		s.rset()
	default:
		s.err("unknown command")
	}
	return nil
}

func (s *pop3Session) authenticated() bool { return s.account.ID != "" }

// -----------------------------------------------------------------------------
// AUTHORIZATION
// -----------------------------------------------------------------------------

func (s *pop3Session) userCommand(argument string) {
	if !s.secure {
		s.err("a mailbox password may only be sent over TLS")
		return
	}
	if argument == "" {
		s.err("USER needs a mailbox address")
		return
	}
	s.user = argument
	s.ok("send PASS")
}

// passCommand authenticates and takes the maildrop.
//
// Every failure answers with the same sentence. A message that distinguished
// "no such mailbox" from "wrong password" would let anyone enumerate a
// customer's addresses one login at a time.
func (s *pop3Session) passCommand(ctx context.Context, secret string) {
	if !s.secure {
		s.err("a mailbox password may only be sent over TLS")
		return
	}
	if s.user == "" {
		s.err("send USER first")
		return
	}
	address := s.user
	s.user = ""

	limiter := s.server.limiter
	// This is the cheap check and it comes first. authenticate below always
	// pays a PBKDF2 verification — the real one, or the decoy that makes an
	// unknown mailbox cost the same — which is tens of milliseconds of a core
	// per attempt. A peer guessing in a loop must not be able to buy that, so
	// the refusal is made with a map lookup before any hashing happens.
	//
	// The reply names a temporary system condition rather than the credential.
	// It does tell the peer it is being throttled, which it must: a client that
	// was told "authentication failed" would keep retrying and stay locked out.
	// It reveals nothing about the mailbox, because the lock is set by the
	// peer's own failures whether or not the address exists.
	if err := limiter.AllowAuth(s.remote, address); err != nil {
		s.server.logger.Info("maild pop3: refused a login at the failure limit", "remote", s.remote)
		s.err("[SYS/TEMP] too many failed authentication attempts, try again later")
		return
	}

	account, domain, err := s.server.authenticate(ctx, address, secret)
	if err != nil {
		// Recorded for every failure, an unknown mailbox included: the peer
		// choosing which names to try is the case this is defending against.
		limiter.RecordAuthFailure(s.remote, address)
		// The address is logged; the secret is not, and is not in err either.
		s.server.logger.Info("maild pop3: authentication failed", "address", address, "remote", s.remote)
		s.err("authentication failed")
		return
	}
	limiter.RecordAuthSuccess(s.remote, address)

	if !s.server.acquire(account.ID) {
		s.err("[IN-USE] the maildrop is locked by another session")
		return
	}
	s.lock = account.ID

	messages, err := s.server.spool.Messages(account.ID)
	if err != nil {
		s.server.logger.Error("maild pop3: read a maildrop", "address", address, "error", err)
		s.server.release(s.lock)
		s.lock = ""
		s.err("the maildrop could not be opened")
		return
	}

	s.account = account
	s.domain = domain
	s.messages = make([]pop3Entry, 0, len(messages))
	for _, message := range messages {
		s.messages = append(s.messages, pop3Entry{MaildirMessage: message})
	}

	count, octets := s.totals()
	s.ok(fmt.Sprintf("maildrop has %d messages (%d octets)", count, octets))
}

// authenticate resolves an address to a mailbox and checks the credential. The
// error it returns never says which half failed and never carries the secret.
func (s *POP3Server) authenticate(ctx context.Context, address, secret string) (Account, Domain, error) {
	failure := errors.New("maild: the mailbox or the credential is wrong")

	localPart, domainName, ok := SplitAddress(address)
	if !ok || secret == "" {
		// Still pay the KDF: a malformed address must not be the fast path
		// that tells an attacker the address space's shape.
		decoyHash().Verify(secret)
		return Account{}, Domain{}, failure
	}

	domain, err := s.store.FindDomain(ctx, domainName)
	if err != nil {
		decoyHash().Verify(secret)
		return Account{}, Domain{}, failure
	}
	account, err := s.store.FindAccount(ctx, domain.ID, localPart)
	if err != nil {
		decoyHash().Verify(secret)
		return Account{}, Domain{}, failure
	}
	if !account.Password.Verify(secret) {
		return Account{}, Domain{}, failure
	}
	// Checked after the credential on purpose: answering "disabled" to an
	// unauthenticated caller would confirm the address exists.
	if !domain.Enabled {
		return Account{}, Domain{}, failure
	}
	return account, domain, nil
}

// acquire takes the maildrop lock, reporting false when another session holds
// it. The lock is process-local, which is what this deployment needs: one maild
// serves a mailbox, and a second process on the same spool would be a
// misconfiguration a lock file could not fix either.
func (s *POP3Server) acquire(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, held := s.locked[id]; held {
		return false
	}
	s.locked[id] = struct{}{}
	return true
}

func (s *POP3Server) release(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.locked, id)
}

// release drops the maildrop lock at the end of a session, however it ended.
func (s *pop3Session) release() {
	if s.lock != "" {
		s.server.release(s.lock)
		s.lock = ""
	}
}

// -----------------------------------------------------------------------------
// TRANSACTION
// -----------------------------------------------------------------------------

// totals is the count and size of the messages not marked deleted, which is
// what STAT reports and what a client uses to size its download.
func (s *pop3Session) totals() (count int, octets int64) {
	for _, entry := range s.messages {
		if entry.deleted {
			continue
		}
		count++
		octets += entry.Size
	}
	return count, octets
}

func (s *pop3Session) stat() {
	count, octets := s.totals()
	s.ok(fmt.Sprintf("%d %d", count, octets))
}

func (s *pop3Session) list(argument string) {
	if argument != "" {
		index, entry, err := s.message(argument)
		if err != nil {
			s.err(err.Error())
			return
		}
		s.ok(fmt.Sprintf("%d %d", index+1, entry.Size))
		return
	}
	count, octets := s.totals()
	s.ok(fmt.Sprintf("%d messages (%d octets)", count, octets))
	for index, entry := range s.messages {
		if entry.deleted {
			continue
		}
		s.line(fmt.Sprintf("%d %d", index+1, entry.Size))
	}
	s.endMultiline()
}

func (s *pop3Session) uidl(argument string) {
	if argument != "" {
		index, entry, err := s.message(argument)
		if err != nil {
			s.err(err.Error())
			return
		}
		s.ok(fmt.Sprintf("%d %s", index+1, entry.UID))
		return
	}
	s.ok("")
	for index, entry := range s.messages {
		if entry.deleted {
			continue
		}
		s.line(fmt.Sprintf("%d %s", index+1, entry.UID))
	}
	s.endMultiline()
}

func (s *pop3Session) retr(argument string) {
	index, entry, err := s.message(argument)
	if err != nil {
		s.err(err.Error())
		return
	}
	body, err := os.ReadFile(entry.Path)
	if err != nil {
		// The message vanished, or the spool is unreadable. Say so rather than
		// sending a truncated message a client would file as complete.
		s.server.logger.Error("maild pop3: read a message", "message", index+1, "error", err)
		s.err("the message could not be read")
		return
	}
	s.ok(fmt.Sprintf("%d octets", entry.Size))
	s.writeMessage(body, -1)
}

func (s *pop3Session) top(argument string) {
	number, count, found := strings.Cut(argument, " ")
	if !found {
		s.err("TOP needs a message number and a line count")
		return
	}
	lines, err := strconv.Atoi(strings.TrimSpace(count))
	if err != nil || lines < 0 {
		s.err("the line count is not a number")
		return
	}
	index, entry, err := s.message(strings.TrimSpace(number))
	if err != nil {
		s.err(err.Error())
		return
	}
	body, err := os.ReadFile(entry.Path)
	if err != nil {
		s.server.logger.Error("maild pop3: read a message", "message", index+1, "error", err)
		s.err("the message could not be read")
		return
	}
	s.ok("")
	s.writeMessage(body, lines)
}

func (s *pop3Session) dele(argument string) {
	index, _, err := s.message(argument)
	if err != nil {
		s.err(err.Error())
		return
	}
	// Marked, not unlinked: RFC 1939 removes messages in the UPDATE state, so a
	// session that drops without QUIT leaves the maildrop untouched. That is
	// what makes a flaky connection lose no mail.
	s.messages[index].deleted = true
	s.ok(fmt.Sprintf("message %d deleted", index+1))
}

func (s *pop3Session) rset() {
	for index := range s.messages {
		s.messages[index].deleted = false
	}
	count, octets := s.totals()
	s.ok(fmt.Sprintf("maildrop has %d messages (%d octets)", count, octets))
}

// quit ends the session. From TRANSACTION it enters UPDATE and unlinks the
// messages marked by DELE; from AUTHORIZATION it is just a goodbye.
func (s *pop3Session) quit() error {
	if !s.authenticated() {
		s.ok("bye")
		return errPOP3Quit
	}

	removed, failed := 0, 0
	for _, entry := range s.messages {
		if !entry.deleted {
			continue
		}
		if err := os.Remove(entry.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			s.server.logger.Error("maild pop3: remove a deleted message", "error", err)
			failed++
			continue
		}
		removed++
	}
	s.release()

	if failed > 0 {
		s.err(fmt.Sprintf("some deleted messages were not removed (%d of %d)", failed, failed+removed))
		return errPOP3Quit
	}
	s.ok("bye")
	return errPOP3Quit
}

// message resolves a message number against the session's view.
func (s *pop3Session) message(argument string) (int, pop3Entry, error) {
	number, err := strconv.Atoi(strings.TrimSpace(argument))
	if err != nil {
		return 0, pop3Entry{}, errors.New("the message number is not a number")
	}
	if number < 1 || number > len(s.messages) {
		return 0, pop3Entry{}, errors.New("no such message")
	}
	entry := s.messages[number-1]
	if entry.deleted {
		return 0, pop3Entry{}, errors.New("that message is deleted")
	}
	return number - 1, entry, nil
}

// -----------------------------------------------------------------------------
// Wire output
// -----------------------------------------------------------------------------

func (s *pop3Session) ok(text string) {
	if text == "" {
		s.write("+OK\r\n")
		return
	}
	s.write("+OK " + text + "\r\n")
}

func (s *pop3Session) err(text string) {
	s.write("-ERR " + text + "\r\n")
}

// line writes one line of a multi-line response, byte-stuffed.
func (s *pop3Session) line(text string) {
	if strings.HasPrefix(text, ".") {
		text = "." + text
	}
	s.write(text + "\r\n")
}

func (s *pop3Session) endMultiline() { s.write(".\r\n") }

// write ignores the error deliberately: bufio.Writer latches its first failure
// and the session's Flush reports it, so checking here would only duplicate the
// same error at every call site.
func (s *pop3Session) write(text string) {
	if _, err := s.writer.WriteString(text); err != nil {
		s.server.logger.Debug("maild pop3: write", "error", err)
	}
}

// writeMessage sends a message body as a multi-line response: byte-stuffed,
// CRLF-terminated, ended by a lone dot. A negative headerLines sends the whole
// message; zero or more sends the headers plus that many body lines, which is
// TOP.
//
// The CRLF normalisation is defensive. The spool is supposed to hold CRLF
// already; if it does not, a bare LF before a lone "." would end the transfer
// early and the client would file a truncated message as complete.
func (s *pop3Session) writeMessage(body []byte, headerLines int) {
	text := string(normalizeCRLF(body))
	text = strings.TrimSuffix(text, "\r\n")
	if text == "" {
		// An empty file is not a message. Send the terminator alone rather than
		// a blank line the client would file as a message with no headers.
		s.endMultiline()
		return
	}

	inHeaders := headerLines >= 0
	sent := 0
	for _, line := range strings.Split(text, "\r\n") {
		if inHeaders && line == "" {
			inHeaders = false
			s.line("")
			continue
		}
		if !inHeaders && headerLines >= 0 {
			if sent >= headerLines {
				break
			}
			sent++
		}
		s.line(line)
	}
	s.endMultiline()
}

// capa answers the capability list. It is the same in both states, which is
// what lets a client decide whether it can use UIDL before it logs in.
func (s *pop3Session) capa() {
	s.ok("capability list follows")
	for _, capability := range []string{"TOP", "UIDL", "USER", "RESP-CODES", "IMPLEMENTATION maild"} {
		s.line(capability)
	}
	s.endMultiline()
}
