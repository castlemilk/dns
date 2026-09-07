package maild

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// -----------------------------------------------------------------------------
// Adversarial re-check of the six blocking findings, plus a hunt for holes the
// queue runner introduced. Every test in here is written to FAIL if the hole it
// names is still open, and to be readable as the proof of that.
// -----------------------------------------------------------------------------

// loopFixture wires the whole outbound path back into the inbound one, which is
// what a real deployment does the moment a hosted domain's MX points at this
// server: Queue -> Relay -> (socket) -> SMTPServer -> Deliverer -> Queue.
//
// Nothing here is a mock. The relay speaks real SMTP over a real net.Pipe to the
// real SMTPServer, which hands the message to the real Deliverer.
type loopFixture struct {
	store    *Store
	root     string
	deliver  *Deliverer
	smtp     *SMTPServer
	queue    *Queue
	domain   Domain
	sessions atomic.Int64
}

func newLoopFixture(t *testing.T, lists map[string][]string, maxMessageBytes int64) *loopFixture {
	t.Helper()
	ctx := context.Background()
	store := openTestStore(t)
	root := t.TempDir()

	domain, err := store.CreateDomain(ctx, NewDomain("acme.test", referenceTime))
	if err != nil {
		t.Fatalf("CreateDomain: %v", err)
	}
	for localPart, recipients := range lists {
		list := NewList(domain.ID, localPart, referenceTime)
		list.Recipients = recipients
		if _, err := store.CreateList(ctx, list); err != nil {
			t.Fatalf("CreateList(%s): %v", localPart, err)
		}
	}

	deliver, err := NewDeliverer(store, DeliveryConfig{
		Root:     root,
		Hostname: "mail.acme.test",
		Logger:   discardLogger(),
		Now:      time.Now,
	})
	if err != nil {
		t.Fatalf("NewDeliverer: %v", err)
	}
	smtpServer, err := NewSMTPServer(deliver, SMTPConfig{
		Hostname:        "mail.acme.test",
		MaxMessageBytes: maxMessageBytes,
		Logger:          discardLogger(),
		Now:             time.Now,
	})
	if err != nil {
		t.Fatalf("NewSMTPServer: %v", err)
	}
	t.Cleanup(func() {
		if err := smtpServer.Close(); err != nil {
			t.Errorf("close the SMTP server: %v", err)
		}
	})

	fixture := &loopFixture{store: store, root: root, deliver: deliver, smtp: smtpServer, domain: domain}

	// The MX for every domain is this server, and the dialer lands on it. That
	// is the ordinary case for a hosted domain, not a contrived one.
	relay, err := NewRelay(RelayConfig{
		Hostname:   "mail.acme.test",
		DisableTLS: true,
		Logger:     discardLogger(),
		Resolver: ResolverFunc(func(context.Context, string) ([]string, error) {
			return []string{"mail.acme.test"}, nil
		}),
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			fixture.sessions.Add(1)
			client, peer := net.Pipe()
			go smtpServer.ServeConn(peer)
			return client, nil
		},
	})
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	queue, err := NewQueue(store, QueueConfig{
		SpoolDir: filepath.Join(root, SpoolDir),
		Hostname: "mail.acme.test",
		Sender:   relay,
		Logger:   discardLogger(),
		Now:      time.Now,
	})
	if err != nil {
		t.Fatalf("NewQueue: %v", err)
	}
	fixture.queue = queue
	return fixture
}

// inject hands one message to the delivery path exactly as a finished DATA
// command does.
func (f *loopFixture) inject(t *testing.T, returnPath, to string) {
	t.Helper()
	ctx := context.Background()
	recipient, err := f.deliver.Resolve(ctx, to, false)
	if err != nil {
		t.Fatalf("Resolve(%q): %v", to, err)
	}
	envelope := &Envelope{
		ID:         "seed",
		ReturnPath: returnPath,
		Recipients: []Recipient{recipient},
		Received:   "Received: from probe.test (198.51.100.7) by mail.acme.test; seed\r\n",
		Data:       []byte("Subject: probe\r\n\r\nbody\r\n"),
		ReceivedAt: time.Now(),
	}
	if err := f.deliver.Deliver(ctx, envelope); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
}

func (f *loopFixture) queued(t *testing.T) []QueuedMessage {
	t.Helper()
	messages, err := f.store.ListQueuedMessages(context.Background(), MaxObjectsPerCall)
	if err != nil {
		t.Fatalf("ListQueuedMessages: %v", err)
	}
	return messages
}

// TestSecurityAListThatNamesAHostedAddressIsAMailLoop is the hole the queue
// runner opened.
//
// delivery.go's expandList queues every list target that is not a local mailbox
// for REMOTE delivery, and its comment says that is "what stops a list that
// points at a list from looping". That was true only while nothing ever drained
// the queue. Now that a runner exists, a queued target on a hosted domain goes
// out over SMTP, arrives back at this server's own MX (the facade publishes
// MAIL_HOSTNAME as the MX for every hosted domain), is expanded again, and is
// queued again. Nothing counts hops: there is no Received: ceiling, no X-Loop
// header and no "is this MX me" check anywhere in the package.
//
// The message grows by one Received: header per lap, so the loop is an
// amplifier as well as a spin, and the only thing that ever ends it is the
// message finally exceeding MaxMessageBytes.
func TestSecurityAListThatNamesAHostedAddressIsAMailLoop(t *testing.T) {
	tests := []struct {
		name  string
		lists map[string][]string
		to    string
	}{
		{
			name:  "a forwarder that names itself",
			lists: map[string][]string{"team": {"team@acme.test"}},
			to:    "team@acme.test",
		},
		{
			// The realistic shape: two forwarders on the same hosted domain
			// that name each other, which a customer can create with two
			// ordinary x:MailingList/set calls.
			name: "two forwarders that name each other",
			lists: map[string][]string{
				"team":     {"everyone@acme.test"},
				"everyone": {"team@acme.test"},
			},
			to: "team@acme.test",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newLoopFixture(t, test.lists, 0)
			fixture.inject(t, "sender@outside.test", test.to)

			// This began life as a bug demonstration that ended in an
			// unconditional t.Fatalf, so it reported the hole whether or not the
			// hole was open — and it drove only 8 laps against a 25-hop limit, so
			// a correct fix could not have shown up in it either. It now asserts
			// the property it was documenting: a loop must END. The laps ceiling
			// is generous headroom over maxReceivedHops so this is a test about
			// termination, not about the exact lap it lands on.
			const laps = maxReceivedHops * 3
			drained := -1
			for lap := range laps {
				if len(fixture.queued(t)) == 0 {
					drained = lap
					break
				}
				if err := fixture.queue.RunOnce(context.Background()); err != nil {
					t.Fatalf("RunOnce on lap %d: %v", lap, err)
				}
			}

			if drained < 0 {
				t.Fatalf("MAIL LOOP: after %d laps (hop limit %d) the queue still holds %d message(s) "+
					"and %d outbound sessions have been opened. Nothing counts hops.",
					laps, maxReceivedHops, len(fixture.queued(t)), fixture.sessions.Load())
			}
			if sessions := fixture.sessions.Load(); int(sessions) > maxReceivedHops+2 {
				t.Errorf("the loop opened %d outbound sessions before terminating, want no more than the hop limit %d (+2 slack)",
					sessions, maxReceivedHops)
			}
		})
	}
}

// TestSecurityMeasureWhereTheMailLoopFinallyStops pins the only thing that ends
// the loop, so the size of the defect is a number rather than an adjective. With
// a deliberately small message limit the loop runs until the growing message is
// refused with 552, which is the same arithmetic the 25 MiB production limit
// gives at a far larger scale.
func TestSecurityMeasureWhereTheMailLoopFinallyStops(t *testing.T) {
	const limit = 4000
	fixture := newLoopFixture(t, map[string][]string{"team": {"team@acme.test"}}, limit)
	fixture.inject(t, "sender@outside.test", "team@acme.test")

	laps, bytesMoved, growth := 0, int64(0), int64(0)
	var first int64
	for laps < 5000 {
		queued := fixture.queued(t)
		if len(queued) == 0 {
			break
		}
		if first == 0 {
			first = queued[0].SizeBytes
		}
		bytesMoved += queued[0].SizeBytes
		growth = queued[0].SizeBytes
		if err := fixture.queue.RunOnce(context.Background()); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
		laps++
	}
	perLap := int64(1)
	if laps > 1 {
		perLap = (growth - first) / int64(laps-1)
	}
	t.Logf("with a %d-byte message limit the loop ran %d laps, moved %d bytes and grew %d bytes per lap; "+
		"at the %d-byte production default that is roughly %d laps and %.2f TB of traffic per seed message",
		limit, laps, bytesMoved, perLap, DefaultSMTPMaxMessageBytes,
		DefaultSMTPMaxMessageBytes/perLap,
		float64(DefaultSMTPMaxMessageBytes/perLap)*float64(DefaultSMTPMaxMessageBytes)/2/1e12)
}

// -----------------------------------------------------------------------------
// NEW HOLE 2: the relay dials whatever a stranger's DNS names, with no filter.
// -----------------------------------------------------------------------------

// internalMX is a listener standing in for something on the internal network
// that happens to speak SMTP on the relay's port: another mail server, a
// mail-submission sidecar, or this very process.
type internalMX struct {
	listener net.Listener
	mu       sync.Mutex
	accepted int
	closeErr error
	body     strings.Builder
}

func newInternalMX(t *testing.T) *internalMX {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen on loopback: %v", err)
	}
	peer := &internalMX{listener: listener}
	t.Cleanup(func() {
		if err := listener.Close(); err != nil {
			t.Logf("close the internal listener: %v", err)
		}
	})
	go peer.serve()
	return peer
}

func (m *internalMX) port() string {
	_, port, err := net.SplitHostPort(m.listener.Addr().String())
	if err != nil {
		panic(err)
	}
	return port
}

func (m *internalMX) serve() {
	for {
		conn, err := m.listener.Accept()
		if err != nil {
			return
		}
		m.mu.Lock()
		m.accepted++
		m.mu.Unlock()
		go m.session(conn)
	}
}

// session is a minimal SMTP server that takes everything it is offered, which
// is what makes it an exfiltration sink rather than merely a port that answered.
func (m *internalMX) session(conn net.Conn) {
	defer func() {
		if err := conn.Close(); err != nil {
			m.mu.Lock()
			m.closeErr = err
			m.mu.Unlock()
		}
	}()
	reader := bufio.NewReader(conn)
	write := func(text string) {
		if _, err := io.WriteString(conn, text+"\r\n"); err != nil {
			return
		}
	}
	write("220 internal.invalid ESMTP")
	inData := false
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if inData {
			if trimmed == "." {
				inData = false
				write("250 2.0.0 Ok, queued")
				continue
			}
			m.mu.Lock()
			m.body.WriteString(trimmed + "\n")
			m.mu.Unlock()
			continue
		}
		switch verb := strings.ToUpper(strings.Fields(trimmed + " ")[0]); verb {
		case "EHLO", "HELO":
			write("250 internal.invalid")
		case "MAIL", "RCPT":
			write("250 2.1.0 Ok")
		case "DATA":
			inData = true
			write("354 go ahead")
		case "QUIT":
			write("221 bye")
			return
		default:
			write("250 2.0.0 Ok")
		}
	}
}

func (m *internalMX) report() (int, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.accepted, m.body.String()
}

// TestSecurityRelayDialsAnyAddressAnMXPointsAt proves there is no SSRF guard on
// the outbound path.
//
// The host the relay dials comes from a DNS answer an attacker controls: it is
// the MX target for a domain they own, or the A record that MX target resolves
// to. relay.go passes that name straight to a plain net.Dialer with no check
// that the address is routable on the public internet — no loopback, private,
// link-local or unique-local refusal anywhere in the file. So a queued message
// addressed to attacker.example is a request this server will make to
// 127.0.0.1, 10.0.0.0/8 or 169.254.169.254 on the relay port, carrying a body
// the attacker also chose.
//
// This test uses no DialContext override, because the production wiring uses
// none: the default dialer is what would run.
func TestSecurityRelayDialsAnyAddressAnMXPointsAt(t *testing.T) {
	peer := newInternalMX(t)

	relay, err := NewRelay(RelayConfig{
		Hostname:   "mail.acme.test",
		Port:       peer.port(),
		DisableTLS: true,
		Logger:     discardLogger(),
		// Exactly what LookupMailHosts returns for a domain whose MX record
		// names a host with an internal address. No filtering happens between
		// this answer and the dial.
		Resolver: ResolverFunc(func(context.Context, string) ([]string, error) {
			return []string{"127.0.0.1"}, nil
		}),
	})
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}

	reports := relay.Send(context.Background(), "attacker.example", "sender@attacker.example",
		[]string{"victim@attacker.example"}, []byte("Subject: probe\r\n\r\nSSRF-CANARY-9f31\r\n"))

	accepted, body := peer.report()
	if accepted == 0 {
		return // a guard exists: nothing was dialled.
	}
	if len(reports) != 1 || reports[0].Err != nil {
		t.Logf("delivery report: %+v", reports[0])
	}
	t.Fatalf("SSRF: the relay opened %d connection(s) to a loopback address named only by DNS "+
		"and delivered the message to it. The internal peer received:\n%s", accepted, body)
}

// -----------------------------------------------------------------------------
// FINDING 1: POP3 unlimited online guessing and CPU exhaustion.
// -----------------------------------------------------------------------------

// pop3Attempt runs one USER/PASS pair against a live server as one peer and
// returns the reply to PASS. secure is true because passCommand refuses a
// cleartext password before anything else.
func pop3Attempt(t *testing.T, server *POP3Server, remote, address, secret string) string {
	t.Helper()
	input := "USER " + address + "\r\nPASS " + secret + "\r\nQUIT\r\n"
	var output strings.Builder
	stream := struct {
		io.Reader
		io.Writer
	}{strings.NewReader(input), &output}
	if err := server.session(context.Background(), stream, true, remote); err != nil && err != io.EOF {
		t.Fatalf("session: %v", err)
	}
	lines := splitTranscript(output.String())
	if len(lines) < 3 {
		t.Fatalf("short transcript: %q", lines)
	}
	return lines[2] // greeting, USER, PASS
}

// TestSecurityPOP3GuessingIsLockedOutAndCostsNoCPU is the adversarial version of
// finding 1. It asserts two separate things, because only one of them is about
// brute force and the other is about CPU:
//
//	(a) after the failure budget, a guess is refused — and the refusal is proved
//	    to happen BEFORE the 600,000-iteration verification by presenting the
//	    CORRECT password and requiring it to be refused too. No black-box result
//	    other than that can distinguish "checked the lockout first" from
//	    "hashed, then checked";
//	(b) the refused attempts cost no measurable CPU, measured against the cost
//	    of one attempt that is allowed through.
func TestSecurityPOP3GuessingIsLockedOutAndCostsNoCPU(t *testing.T) {
	fixture := newPOP3Fixture(t)
	// Warm the decoy so the first unknown-mailbox attempt is not paying for the
	// one-time derivation as well as the verification.
	decoyHash()

	const attacker = "198.51.100.55:40000"
	locked := ""
	allowed := 0
	for attempt := 1; attempt <= 40; attempt++ {
		reply := pop3Attempt(t, fixture.server, attacker,
			"nobody"+fmt.Sprint(attempt)+"@probe.test", "guess")
		if strings.Contains(reply, "too many failed authentication attempts") {
			locked = reply
			break
		}
		allowed++
	}
	if locked == "" {
		t.Fatalf("BRUTE FORCE: 40 consecutive guesses from one address were all answered "+
			"%q with no lockout", pop3Attempt(t, fixture.server, attacker, "x@probe.test", "guess"))
	}
	if allowed > 10 {
		t.Errorf("the lockout allowed %d guesses before engaging; the configured budget is 5", allowed)
	}

	// (a) The correct credential is refused while the lock stands. That is only
	// possible if the lockout is consulted before the password is verified.
	correct := pop3Attempt(t, fixture.server, attacker, testMailboxAddress, testMailboxPassword)
	if !strings.Contains(correct, "too many failed authentication attempts") {
		t.Fatalf("CPU EXHAUSTION: while locked out the CORRECT password was answered %q, "+
			"so the PBKDF2 verification is being paid before the lockout is consulted", correct)
	}

	// (b) Cost. One allowed attempt against an unknown mailbox pays the real
	// decoy verification; the refused ones must pay nothing like it.
	start := time.Now()
	pop3Attempt(t, fixture.server, "203.0.113.99:40000", "someone@probe.test", "guess")
	oneVerification := time.Since(start)

	start = time.Now()
	const refused = 50
	for range refused {
		pop3Attempt(t, fixture.server, attacker, testMailboxAddress, "guess")
	}
	refusedTotal := time.Since(start)

	if refusedTotal > 5*oneVerification {
		t.Fatalf("CPU EXHAUSTION: %d refused attempts took %v, while a single allowed "+
			"verification takes %v — the refusal is not free", refused, refusedTotal, oneVerification)
	}
	t.Logf("one verification %v; %d refused attempts %v", oneVerification, refused, refusedTotal)
}

// -----------------------------------------------------------------------------
// FINDING 5: one peer exhausting every session slot.
// -----------------------------------------------------------------------------

// TestSecurityOnePeerCannotTakeEverySessionSlot uses the DEFAULT configuration,
// because that is what cmd/deephost-mail builds: neither NewSMTPServer nor
// NewPOP3Server is given a Limiter there, so whatever the defaults do is what
// production does.
func TestSecurityOnePeerCannotTakeEverySessionSlot(t *testing.T) {
	t.Run("smtp", func(t *testing.T) {
		server := newSMTPTestServer(t, &fakeMailer{}, SMTPConfig{Hostname: "mail.probe.test"})
		limit := server.config.Limiter.Config()
		if limit.MaxSessionsPerIP >= limit.MaxSessions {
			t.Fatalf("one peer may hold %d of %d slots", limit.MaxSessionsPerIP, limit.MaxSessions)
		}
		var refusedAt int
		for opened := 1; opened <= limit.MaxSessionsPerIP+1; opened++ {
			client := dialSMTP(t, server)
			code, text := client.readReply()
			if code == 421 {
				refusedAt = opened
				break
			}
			if code != 220 {
				t.Fatalf("session %d greeting = %q", opened, text)
			}
		}
		if refusedAt == 0 {
			t.Fatalf("SLOT EXHAUSTION: one peer opened %d simultaneous SMTP sessions "+
				"(the per-address share is %d of %d)", limit.MaxSessionsPerIP+1,
				limit.MaxSessionsPerIP, limit.MaxSessions)
		}
		t.Logf("one peer was refused at session %d of a %d-slot server", refusedAt, limit.MaxSessions)
	})

	t.Run("pop3", func(t *testing.T) {
		fixture := newPOP3Fixture(t)
		limit := fixture.server.limiter.Config()
		if limit.MaxSessionsPerIP >= limit.MaxSessions {
			t.Fatalf("one peer may hold %d of %d slots", limit.MaxSessionsPerIP, limit.MaxSessions)
		}
		refusedAt := 0
		open := make([]*pop3Conn, 0, limit.MaxSessionsPerIP+1)
		for opened := 1; opened <= limit.MaxSessionsPerIP+1; opened++ {
			conn := dialPOP3(t, fixture.server)
			open = append(open, conn)
			greeting := conn.line(t)
			if strings.Contains(greeting, "too many connections") {
				refusedAt = opened
				break
			}
		}
		for _, conn := range open {
			if err := conn.conn.Close(); err != nil {
				t.Logf("close a POP3 client: %v", err)
			}
		}
		if refusedAt == 0 {
			t.Fatalf("SLOT EXHAUSTION: one peer opened %d simultaneous POP3 sessions "+
				"(the per-address share is %d of %d)", limit.MaxSessionsPerIP+1,
				limit.MaxSessionsPerIP, limit.MaxSessions)
		}
		t.Logf("one peer was refused at session %d of a %d-slot server", refusedAt, limit.MaxSessions)
	})
}

// -----------------------------------------------------------------------------
// FINDING 6: message size times concurrent sessions.
// -----------------------------------------------------------------------------

// TestSecurityMessageMemoryIsBoundedJointly drives real sessions to the 354 and
// leaves them there, which is the cheapest way for a peer to make this process
// hold a buffer. The bound has to be on the PRODUCT, so the test asserts a
// session is refused before MaxMessageBytes x MaxSessions can be reached.
func TestSecurityMessageMemoryIsBoundedJointly(t *testing.T) {
	server := newSMTPTestServer(t, &fakeMailer{}, SMTPConfig{Hostname: "mail.probe.test"})
	limit := server.config.Limiter.Config()
	concurrent := server.config.Limiter.MaxConcurrentMessages()

	unbounded := limit.MaxMessageBytes * int64(limit.MaxSessions)
	bounded := limit.MaxMessageBytes * int64(concurrent)
	if bounded >= unbounded {
		t.Fatalf("MEMORY: the product is unbounded — %d bodies x %d bytes = %d",
			limit.MaxSessions, limit.MaxMessageBytes, unbounded)
	}

	refusedAt := 0
	for opened := 1; opened <= concurrent+1; opened++ {
		client := dialSMTP(t, server)
		client.expect(220)
		client.do("EHLO probe.test", 250)
		client.do("MAIL FROM:<sender@probe.test>", 250)
		client.do("RCPT TO:<mara@probe.test>", 250)
		client.send("DATA")
		code, text := client.readReply()
		switch code {
		case 354:
		case 452:
			refusedAt = opened
		default:
			t.Fatalf("session %d DATA = %q", opened, text)
		}
		if refusedAt != 0 {
			break
		}
	}
	if refusedAt == 0 {
		t.Fatalf("MEMORY: %d sessions all reached DATA at once, holding %d bytes",
			concurrent+1, int64(concurrent+1)*limit.MaxMessageBytes)
	}
	t.Logf("bodies in flight capped at %d (%d bytes), refused at session %d; "+
		"without the cap the product would be %d bytes", concurrent, bounded, refusedAt, unbounded)
}

// -----------------------------------------------------------------------------
// FINDING 2: CR/LF in a mailing-list recipient.
// -----------------------------------------------------------------------------

// recordingSender captures every address that would have reached the wire.
type recordingSender struct {
	mu    sync.Mutex
	calls []struct {
		domain string
		from   string
		to     []string
	}
	err error
}

func (s *recordingSender) Send(_ context.Context, domain, from string, to []string, _ []byte) []RelayReport {
	s.mu.Lock()
	s.calls = append(s.calls, struct {
		domain string
		from   string
		to     []string
	}{domain, from, append([]string(nil), to...)})
	s.mu.Unlock()
	reports := make([]RelayReport, len(to))
	for i, address := range to {
		reports[i] = RelayReport{Address: address, Err: s.err}
	}
	return reports
}

func (s *recordingSender) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var all []string
	for _, call := range s.calls {
		all = append(all, call.to...)
	}
	return all
}

// TestSecurityACRLFRecipientReachesNeitherTheWireNorABounceHeader is finding 2
// re-checked at the two places it can still bite.
//
// The JMAP boundary is one gate, but a queue row is not only written by JMAP —
// a row written before the gate existed, or by any future path, is still read
// and acted on. So the queue is required to refuse the address itself, and the
// bounce it then writes is required to contain no header the address smuggled.
func TestSecurityACRLFRecipientReachesNeitherTheWireNorABounceHeader(t *testing.T) {
	// The gate itself, on the way in.
	for _, hostile := range []string{
		"ada@example.test\r\nBcc: everyone@example.test",
		"ada@example.test\nBcc: everyone@example.test",
		"ada@example.test\r",
		"ada@example.test\n",
	} {
		if _, err := NormalizeAddress(hostile); err == nil {
			t.Errorf("NormalizeAddress(%q) was accepted", hostile)
		}
		if _, failure := enabledRecipients(map[string]bool{hostile: true}); failure == nil {
			t.Errorf("enabledRecipients accepted %q", hostile)
		}
	}

	// And the gate on the way out, over a stored row the API never saw.
	ctx := context.Background()
	store := openTestStore(t)
	spool := t.TempDir()
	body := "Subject: probe\r\n\r\nbody\r\n"
	if err := os.WriteFile(filepath.Join(spool, "m1.eml"), []byte(body), 0o600); err != nil {
		t.Fatalf("write the spool body: %v", err)
	}
	sender := &recordingSender{}
	queue, err := NewQueue(store, QueueConfig{
		SpoolDir: spool,
		Hostname: "mail.acme.test",
		Sender:   sender,
		Logger:   discardLogger(),
		Now:      time.Now,
	})
	if err != nil {
		t.Fatalf("NewQueue: %v", err)
	}
	const smuggled = "victim@example.test\r\nBcc: everyone@example.test"
	if _, err := store.PutQueuedMessage(ctx, QueuedMessage{
		V:  DocVersion,
		ID: "q-injected",
		// The return path is a real address so a bounce is actually produced,
		// which is the second place the hostile string could land.
		ReturnPath: "reporter@probe.test",
		Recipients: []QueuedRecipient{{
			Address:   smuggled,
			ORCPT:     "team@acme.test\r\nX-Injected: yes",
			Status:    QueueStatusScheduled,
			QueueName: "remote",
			RetryDue:  time.Now().Add(-time.Minute),
		}},
		MessagePath: "m1.eml",
		SizeBytes:   int64(len(body)),
		CreatedAt:   time.Now(),
		NextRetry:   time.Now().Add(-time.Minute),
	}); err != nil {
		t.Fatalf("PutQueuedMessage: %v", err)
	}
	if err := queue.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	for _, address := range sender.seen() {
		if strings.ContainsAny(address, "\r\n") {
			t.Fatalf("HEADER INJECTION: the queue put %q on the wire", address)
		}
	}
	if len(sender.seen()) != 0 {
		t.Fatalf("the queue attempted delivery to %q instead of refusing it", sender.seen())
	}

	// The bounce it wrote instead must carry no smuggled header.
	entries, err := os.ReadDir(spool)
	if err != nil {
		t.Fatalf("read the spool: %v", err)
	}
	bounced := false
	for _, entry := range entries {
		if entry.IsDir() || entry.Name() == "m1.eml" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(spool, entry.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		bounced = true
		text := string(raw)
		// Injection means a NEW header line, not the bytes appearing folded
		// into the value of a line that was going to be written anyway. So the
		// assertion is structural: no line of the message may begin with a
		// field name the recipient string smuggled, and no line may contain a
		// bare CR or LF of its own.
		for _, line := range strings.Split(text, "\r\n") {
			if strings.ContainsAny(line, "\n\r") {
				t.Fatalf("HEADER INJECTION: a bounce line carries a bare CR/LF: %q", line)
			}
			for _, field := range []string{"Bcc:", "X-Injected:"} {
				if strings.HasPrefix(line, field) {
					t.Fatalf("HEADER INJECTION: the bounce grew a %s line: %q\n%s", field, line, text)
				}
			}
		}
	}
	if !bounced {
		t.Fatal("the unusable recipient produced no bounce at all: the sender is never told")
	}
}

// -----------------------------------------------------------------------------
// FINDING 4: outbound amplification from an unauthenticated inbound message.
// -----------------------------------------------------------------------------

// TestSecurityUnauthenticatedFanOutCeiling measures what one unauthenticated
// message can actually make this server send, end to end through the real SMTP
// session and the real delivery path. It does not assert a policy; it asserts
// the number, so the number cannot drift silently.
func TestSecurityUnauthenticatedFanOutCeiling(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	root := t.TempDir()
	domain, err := store.CreateDomain(ctx, NewDomain("acme.test", referenceTime))
	if err != nil {
		t.Fatalf("CreateDomain: %v", err)
	}
	// Every list is full: maxFanOut external targets each, which is what a
	// customer building an amplifier would create.
	const lists = 12
	for l := range lists {
		list := NewList(domain.ID, fmt.Sprintf("team%d", l), referenceTime)
		for r := range maxFanOut {
			list.Recipients = append(list.Recipients, fmt.Sprintf("victim%d@target%d.test", r, l))
		}
		if _, err := store.CreateList(ctx, list); err != nil {
			t.Fatalf("CreateList: %v", err)
		}
	}
	deliver, err := NewDeliverer(store, DeliveryConfig{
		Root: root, Hostname: "mail.acme.test", Logger: discardLogger(), Now: time.Now,
	})
	if err != nil {
		t.Fatalf("NewDeliverer: %v", err)
	}
	client := startSMTPSession(t, deliver, SMTPConfig{Hostname: "mail.acme.test", Logger: discardLogger()})
	client.do("EHLO probe.test", 250)
	client.do("MAIL FROM:<stranger@outside.test>", 250)

	accepted := 0
	for l := range lists {
		client.send(fmt.Sprintf("RCPT TO:<team%d@acme.test>", l))
		code, text := client.readReply()
		switch code {
		case 250:
			accepted++
		case 452:
			l = lists // refused; stop
			_ = l
		default:
			t.Fatalf("RCPT %d = %q", l, text)
		}
		if code != 250 {
			break
		}
	}
	client.sendMessage("Subject: probe\r\n\r\nbody\r\n")
	client.expect(250)

	messages, err := store.ListQueuedMessages(ctx, MaxObjectsPerCall)
	if err != nil {
		t.Fatalf("ListQueuedMessages: %v", err)
	}
	outbound := 0
	for _, message := range messages {
		outbound += len(message.Recipients)
	}
	if accepted >= lists {
		t.Fatalf("AMPLIFICATION: no fan-out bound at all — %d list recipients accepted, "+
			"producing %d outbound deliveries", accepted, outbound)
	}
	t.Logf("one unauthenticated message: %d list recipients accepted (bound %d), "+
		"%d outbound deliveries queued, amplification factor %dx",
		accepted, smtpMaxUnauthenticatedFanOut, outbound, outbound)
	if outbound > smtpMaxUnauthenticatedFanOut*maxFanOut {
		t.Fatalf("outbound %d exceeds the stated ceiling %d", outbound, smtpMaxUnauthenticatedFanOut*maxFanOut)
	}
}

// -----------------------------------------------------------------------------
// FINDING 3: mail accepted with 250 and then never delivered.
// -----------------------------------------------------------------------------

// TestSecurityTheQueueRunnerIsActuallyWired is a source-level assertion because
// the defect is an absence, and an absence in another package cannot be
// observed from a unit test here.
//
// internal/maild now CONTAINS a queue runner, and every one of its own tests
// passes. None of that delivers a message: cmd/deephost-mail/services.go is the
// only place that builds this package's handlers, and it constructs no Queue,
// no Relay and no Resolver. The linker agrees — `go tool nm` on the built
// deephost-mail binary contains no maild.NewQueue, maild.NewRelay or
// maild.NewDNSResolver symbol at all, because nothing references them.
//
// Until this passes, every forwarded and every relayed message is still
// answered 250 and then spooled forever.
func TestSecurityTheQueueRunnerIsActuallyWired(t *testing.T) {
	const wiring = "../../cmd/deephost-mail/services.go"
	raw, err := os.ReadFile(wiring)
	if err != nil {
		t.Fatalf("read %s: %v", wiring, err)
	}
	source := string(raw)
	var missing []string
	for _, symbol := range []string{"maild.NewQueue", "maild.NewRelay"} {
		if !strings.Contains(source, symbol) {
			missing = append(missing, symbol)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("NO QUEUE RUNNER IN THE PRODUCT: %s constructs none of %v, so nothing "+
			"drains the outbound queue. Mail is accepted with 250 and never leaves.",
			wiring, missing)
	}
}
