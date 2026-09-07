package maild_test

// The whole journey, over real sockets, in one test.
//
// Nothing here is a mock of anything this repository owns. A real maild.Server
// binds ephemeral loopback ports; the REAL frozen internal/mail/stalwart.Client
// drives the management API over TCP; a real SMTP client delivers into a real
// mailbox; a real POP3 client reads it back over real TLS; and the real queue
// runner, holding the real *maild.Relay, opens a real outbound SMTP session to a
// receiving server that this file runs on 127.0.0.1 and that answers with a real
// SMTP conversation.
//
// The two seams that are not real are the two that cannot be: DNS (an injected
// maild.Resolver, because a test must not query the internet) and the dial
// address (redirected to the loopback receiver, with the address the relay ASKED
// for recorded and asserted, so the resolution and the port still have to be
// right). Everything between those two seams is production code.
//
// Synchronisation is by channel or by polling to a deadline. There is no sleep
// standing in for a happens-before.

import (
	"bufio"
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/castlemilk/dns/internal/mail/stalwart"
	"github.com/castlemilk/dns/internal/maild"
)

const (
	// e2eZone is the customer's hosted domain. It differs from testZone so that
	// nothing in this file can accidentally depend on state another test built.
	e2eZone = "journey-customer.test"
	// e2eExternalDomain is the domain the forwarder points at. Only the
	// injected resolver knows it, and only the fake receiver answers for it.
	e2eExternalDomain = "partner.journey-external.test"
	// e2eExternalMX is the name the injected resolver returns for it. The relay
	// must dial exactly this name on exactly the configured port; the dial hook
	// records the address it was asked for so the test can prove it.
	e2eExternalMX = "mx.partner.journey-external.test"
	// e2eSender is the remote sender. It is not hosted here, which is what
	// makes both deliveries unauthenticated inbound mail rather than
	// submission.
	e2eSender = "remote-sender@somewhere-else.test"
	// e2ePassphrase is the credential the facade generates once and shows a
	// customer once.
	e2ePassphrase = "one-shown-once-credential-for-ada"
)

// e2eDeadline bounds every poll and every channel wait in this file. It is
// generous because the assertions it guards are all in the direction where a
// broken server has long since failed; nothing waits it out on the happy path.
const e2eDeadline = 30 * time.Second

// -----------------------------------------------------------------------------
// A real receiving mail server, on loopback
// -----------------------------------------------------------------------------

// e2eDelivered is one message a remote server took responsibility for.
type e2eDelivered struct {
	From string
	To   []string
	Data string
	// EHLO is the name the sending server called itself. A receiver that checks
	// forward-confirmed reverse DNS compares exactly this, so it is worth
	// pinning that the relay sends MAIL_HOSTNAME and not "localhost".
	EHLO string
}

// e2eMX is a receiving SMTP server. It is not a mock of the relay: it is a
// listener, a line protocol and a conversation, and the relay's stdlib net/smtp
// client has to get every step right to make it publish a message.
type e2eMX struct {
	listener net.Listener
	inbox    chan e2eDelivered
	done     chan struct{}
	wg       sync.WaitGroup

	// refused maps an envelope recipient to the reply RCPT must answer with
	// instead of accepting it. It is per address rather than global so the
	// bounce leg can refuse the forwarder's target while still accepting the
	// bounce that refusal produces — a global switch would have to be flipped
	// off at exactly the right moment, which is a race, not a test.
	mu      sync.Mutex
	refused map[string]string
}

func newE2EMX(t *testing.T) *e2eMX {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("bind the receiving mail server: %v", err)
	}
	mx := &e2eMX{
		listener: listener,
		inbox:    make(chan e2eDelivered, 16),
		done:     make(chan struct{}),
		refused:  map[string]string{},
	}
	mx.wg.Add(1)
	go mx.accept()
	t.Cleanup(func() {
		close(mx.done)
		drop(mx.listener.Close())
		mx.wg.Wait()
	})
	return mx
}

// Addr is the loopback address the relay's dial hook is redirected to.
func (m *e2eMX) Addr() string { return m.listener.Addr().String() }

// Port is what RelayConfig.Port is set to, so the address the relay builds and
// the address it reaches are the same port.
func (m *e2eMX) Port() string {
	_, port, err := net.SplitHostPort(m.Addr())
	if err != nil {
		return ""
	}
	return port
}

// refuseRecipient makes the receiver answer RCPT for exactly this address with
// reply, which is the only thing that can honestly turn an outbound message into
// a bounce: a reply the remote actually sent.
func (m *e2eMX) refuseRecipient(address, reply string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refused[strings.ToLower(address)] = reply
}

func (m *e2eMX) refusal(address string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.refused[strings.ToLower(address)]
}

func (m *e2eMX) accept() {
	defer m.wg.Done()
	for {
		conn, err := m.listener.Accept()
		if err != nil {
			return
		}
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			defer func() { drop(conn.Close()) }()
			m.session(conn)
		}()
	}
}

// session speaks enough ESMTP for a real client: a greeting, EHLO with an
// extension list, an envelope, DATA terminated by a lone dot, and QUIT.
func (m *e2eMX) session(conn net.Conn) {
	drop(conn.SetDeadline(time.Now().Add(e2eDeadline)))
	reader := bufio.NewReader(conn)
	write := func(text string) bool {
		_, err := io.WriteString(conn, text)
		return err == nil
	}
	if !write("220 " + e2eExternalMX + " ESMTP\r\n") {
		return
	}

	var (
		from string
		to   []string
		ehlo string
	)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		verb, rest, _ := strings.Cut(strings.TrimRight(line, "\r\n"), " ")
		switch strings.ToUpper(verb) {
		case "EHLO":
			ehlo = strings.TrimSpace(rest)
			if !write("250-" + e2eExternalMX + "\r\n250-8BITMIME\r\n250 SIZE 26214400\r\n") {
				return
			}
		case "HELO":
			ehlo = strings.TrimSpace(rest)
			if !write("250 " + e2eExternalMX + "\r\n") {
				return
			}
		case "MAIL":
			from = e2ePath(rest)
			to = nil
			if !write("250 2.1.0 sender accepted\r\n") {
				return
			}
		case "RCPT":
			address := e2ePath(rest)
			if reply := m.refusal(address); reply != "" {
				if !write(reply + "\r\n") {
					return
				}
				continue
			}
			to = append(to, address)
			if !write("250 2.1.5 recipient accepted\r\n") {
				return
			}
		case "DATA":
			if len(to) == 0 {
				if !write("503 5.5.1 no recipients\r\n") {
					return
				}
				continue
			}
			if !write("354 send it\r\n") {
				return
			}
			body, ok := e2eReadDATA(reader)
			if !ok {
				return
			}
			message := e2eDelivered{From: from, To: append([]string(nil), to...), Data: body, EHLO: ehlo}
			select {
			case m.inbox <- message:
			case <-m.done:
				return
			}
			from, to = "", nil
			if !write("250 2.0.0 accepted\r\n") {
				return
			}
		case "RSET":
			from, to = "", nil
			if !write("250 2.0.0 reset\r\n") {
				return
			}
		case "NOOP":
			if !write("250 2.0.0 ok\r\n") {
				return
			}
		case "QUIT":
			write("221 2.0.0 bye\r\n")
			return
		default:
			if !write("502 5.5.2 not implemented\r\n") {
				return
			}
		}
	}
}

// await takes the next message the receiver accepted, or fails on the deadline.
// It is a channel wait, not a sleep: a working server satisfies it immediately.
func (m *e2eMX) await(t *testing.T, why string) e2eDelivered {
	t.Helper()
	timer := time.NewTimer(e2eDeadline)
	defer timer.Stop()
	select {
	case message := <-m.inbox:
		return message
	case <-timer.C:
		t.Fatalf("nothing reached the receiving mail server within %s: %s", e2eDeadline, why)
		return e2eDelivered{}
	}
}

// e2eReadDATA reads a dot-terminated body and undoes the dot stuffing, which is
// the receiver's half of the transparency rule in RFC 5321 §4.5.2.
func e2eReadDATA(reader *bufio.Reader) (string, bool) {
	var body strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return "", false
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed == "." {
			return body.String(), true
		}
		if strings.HasPrefix(trimmed, "..") {
			trimmed = trimmed[1:]
		}
		body.WriteString(trimmed)
		body.WriteString("\r\n")
	}
}

// e2ePath pulls the address out of "FROM:<a@b>" or "TO:<a@b> ORCPT=...".
func e2ePath(argument string) string {
	_, rest, found := strings.Cut(argument, ":")
	if !found {
		rest = argument
	}
	rest = strings.TrimSpace(rest)
	if open := strings.Index(rest, "<"); open >= 0 {
		if shut := strings.Index(rest[open:], ">"); shut > 0 {
			return rest[open+1 : open+shut]
		}
	}
	field, _, _ := strings.Cut(rest, " ")
	return field
}

// -----------------------------------------------------------------------------
// The server under test, wired the way production wires it, plus the queue
// -----------------------------------------------------------------------------

// e2eHarness is one running server and everything needed to interrogate it.
type e2eHarness struct {
	server  *maild.Server
	client  *stalwart.Client
	mx      *e2eMX
	queue   *maild.Queue
	dataDir string
	// dialed records every address the relay asked its dialer for, so the test
	// can prove the MX name and port reached the socket layer.
	dialed chan string
	logs   *syncBuffer
}

// e2eStart brings up the whole thing.
//
// The protocol handlers come from productionServices(), which integration_test
// .go copies verbatim from cmd/deephost-mail/services.go — so this exercises the
// deployed wiring rather than a convenient one. The queue runner is added on top
// because production does not build one (see
// TestProductionWiringHasNoQueueRunner); without it there is no outbound leg to
// test at all.
func e2eStart(t *testing.T) *e2eHarness {
	t.Helper()
	// The poll interval is short because nothing in the delivery path kicks the
	// runner (see TestAFreshlyQueuedMessageWaitsOutThePollInterval); at the
	// production default this suite would be a minute slower per outbound leg
	// for no extra proof.
	return e2eStartWithInterval(t, 20*time.Millisecond)
}

// e2eStartWithInterval is e2eStart with the queue runner's poll interval named,
// so one test can run the production default and measure what that costs a
// freshly queued message.
func e2eStartWithInterval(t *testing.T, interval time.Duration) *e2eHarness {
	t.Helper()

	mx := newE2EMX(t)
	dataDir := t.TempDir()
	certFile, keyFile := selfSignedPair(t)
	logs := &syncBuffer{}
	harness := &e2eHarness{mx: mx, dataDir: dataDir, dialed: make(chan string, 16), logs: logs}

	server, err := maild.NewServer(maild.Config{
		Hostname:        testHostname,
		AdminToken:      maild.Secret(testToken),
		DataDir:         dataDir,
		APIAddr:         "127.0.0.1:0",
		SMTPAddr:        "127.0.0.1:0",
		SubmissionsAddr: "127.0.0.1:0",
		POP3SAddr:       "127.0.0.1:0",
		TLSCertFile:     certFile,
		TLSKeyFile:      keyFile,
		Logger:          slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Services:        e2eServices(harness, interval),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	harness.server = server

	ctx, cancel := context.WithCancel(context.Background())
	if err := server.Start(ctx); err != nil {
		cancel()
		t.Fatalf("Start: %v", err)
	}
	if harness.queue == nil {
		cancel()
		t.Fatal("the services hook did not build a queue runner")
	}
	if err := harness.queue.Start(ctx); err != nil {
		cancel()
		t.Fatalf("Queue.Start: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		stopCtx, done := context.WithTimeout(context.Background(), e2eDeadline)
		defer done()
		drop(harness.queue.Shutdown(stopCtx))
		drop(server.Shutdown(stopCtx))
		if t.Failed() {
			t.Logf("server log:\n%s", logs.String())
		}
	})

	client, err := stalwart.New("http://"+server.Addr(maild.ListenerAPI), testToken)
	if err != nil {
		t.Fatalf("stalwart.New: %v", err)
	}
	harness.client = client
	return harness
}

// e2eServices is productionServices plus the outbound half.
func e2eServices(harness *e2eHarness, interval time.Duration) func(maild.Deps) (maild.Services, error) {
	return func(deps maild.Deps) (maild.Services, error) {
		services, err := productionServices()(deps)
		if err != nil {
			return maild.Services{}, err
		}

		// The only DNS in this file. It answers for exactly one domain, so a
		// relay that dialled anything else would find nothing to talk to.
		resolver := maild.ResolverFunc(func(_ context.Context, domain string) ([]string, error) {
			if strings.EqualFold(strings.TrimSuffix(domain, "."), e2eExternalDomain) {
				return []string{e2eExternalMX}, nil
			}
			return nil, maild.ErrNoMailHost
		})

		relay, err := maild.NewRelay(maild.RelayConfig{
			Hostname: deps.Hostname,
			Resolver: resolver,
			Port:     harness.mx.Port(),
			DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
				select {
				case harness.dialed <- address:
				default:
				}
				return (&net.Dialer{}).DialContext(ctx, network, harness.mx.Addr())
			},
			Logger: deps.Logger,
		})
		if err != nil {
			return maild.Services{}, err
		}

		queue, err := maild.NewQueue(deps.Store, maild.QueueConfig{
			SpoolDir: deps.SpoolDir,
			Hostname: deps.Hostname,
			Sender:   relay,
			Interval: interval,
			Logger:   deps.Logger,
			Now:      deps.Clock,
		})
		if err != nil {
			return maild.Services{}, err
		}
		harness.queue = queue
		return services, nil
	}
}

// -----------------------------------------------------------------------------
// Small clients
// -----------------------------------------------------------------------------

// e2eSubmit delivers one message over SMTP, as a remote MTA would, and returns
// only once the server has answered the final dot. That answer is the
// happens-before every later assertion stands on: the server has accepted
// responsibility by the time this returns.
func e2eSubmit(t *testing.T, addr, from string, to []string, message string) {
	t.Helper()
	if addr == "" {
		t.Fatal("the SMTP listener reported no address")
	}
	conn, err := net.DialTimeout("tcp", addr, e2eDeadline)
	if err != nil {
		t.Fatalf("dial smtp: %v", err)
	}
	defer func() { drop(conn.Close()) }()
	drop(conn.SetDeadline(time.Now().Add(e2eDeadline)))

	reader := bufio.NewReader(conn)
	expect := func(want byte, step string) string {
		t.Helper()
		line := e2eReadReply(t, reader, step)
		if line[0] != want {
			t.Fatalf("smtp %s answered %q, want a %cxx", step, line, want)
		}
		return line
	}
	send := func(format string, args ...any) {
		t.Helper()
		if _, err := fmt.Fprintf(conn, format+"\r\n", args...); err != nil {
			t.Fatalf("smtp write at %q: %v", format, err)
		}
	}

	expect('2', "banner")
	send("EHLO relay.somewhere-else.test")
	expect('2', "EHLO")
	send("MAIL FROM:<%s>", from)
	expect('2', "MAIL FROM")
	for _, recipient := range to {
		send("RCPT TO:<%s>", recipient)
		expect('2', "RCPT TO <"+recipient+">")
	}
	send("DATA")
	expect('3', "DATA")
	if _, err := io.WriteString(conn, e2eDotStuff(message)+".\r\n"); err != nil {
		t.Fatalf("smtp write body: %v", err)
	}
	expect('2', "end of DATA")
	send("QUIT")
}

// e2eReadReply reads one SMTP reply, multi-line included.
func e2eReadReply(t *testing.T, reader *bufio.Reader, step string) string {
	t.Helper()
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("smtp %s: %v", step, err)
		}
		line = strings.TrimRight(line, "\r\n")
		if len(line) < 4 {
			return line
		}
		if line[3] == '-' {
			continue
		}
		return line
	}
}

// e2eDotStuff applies the sender's half of RFC 5321 §4.5.2 and guarantees the
// body ends on a CRLF, so the terminating dot is on a line of its own.
func e2eDotStuff(message string) string {
	message = strings.ReplaceAll(strings.ReplaceAll(message, "\r\n", "\n"), "\n", "\r\n")
	if !strings.HasSuffix(message, "\r\n") {
		message += "\r\n"
	}
	var out strings.Builder
	for _, line := range strings.SplitAfter(message, "\r\n") {
		if strings.HasPrefix(line, ".") {
			out.WriteString(".")
		}
		out.WriteString(line)
	}
	return out.String()
}

// e2eMessage builds a small, well-formed message with the headers a DKIM
// signature is required to cover.
func e2eMessage(from, to, subject, body string) string {
	return "From: <" + from + ">\r\n" +
		"To: <" + to + ">\r\n" +
		"Subject: " + subject + "\r\n" +
		"Date: Thu, 04 Sep 2026 09:00:00 +0000\r\n" +
		"Message-ID: <" + subject + "@somewhere-else.test>\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/plain; charset=us-ascii\r\n" +
		"\r\n" + body + "\r\n"
}

// e2ePoll runs condition until it holds or the deadline passes. Polling is not a
// sleep standing in for synchronisation: the condition is the synchronisation,
// and a server that already satisfies it never waits.
func e2ePoll(t *testing.T, what string, condition func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(e2eDeadline)
	last := ""
	for {
		ok, detail := condition()
		if ok {
			return
		}
		last = detail
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen within %s; last seen: %s", what, e2eDeadline, last)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// -----------------------------------------------------------------------------
// An independent DKIM verifier
// -----------------------------------------------------------------------------
//
// Written from RFC 6376 §3.4, §3.5 and §3.7 and reading only the message text
// and the TXT record the server published. It shares no code with dkim.go, which
// is the point: a verifier built on the signer's own canonicalisation agrees
// with a wrong signer, and the failure this guards against — a signature a
// receiver rejects, which is worse for deliverability than no signature — is
// exactly the one a self-consistent test cannot see.

var (
	e2eWSP    = regexp.MustCompile(`[ \t]+`)
	e2eBTag   = regexp.MustCompile(`(^|;)([ \t]*b=)[^;]*`)
	errNoDkim = errors.New("the message carries no DKIM-Signature header")
)

type e2eHeader struct {
	name string
	raw  string
}

func e2eSplitMessage(message string) (fields []e2eHeader, body string) {
	block := message
	if index := strings.Index(message, "\r\n\r\n"); index >= 0 {
		block = message[:index]
		body = message[index+4:]
	}
	for _, line := range strings.Split(block, "\r\n") {
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			if len(fields) > 0 {
				fields[len(fields)-1].raw += "\r\n" + line
			}
			continue
		}
		name, _, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		fields = append(fields, e2eHeader{name: strings.ToLower(strings.Trim(name, " \t")), raw: line})
	}
	return fields, body
}

func e2eCanonHeader(raw string) string {
	name, value, found := strings.Cut(raw, ":")
	if !found {
		return strings.ToLower(strings.Trim(raw, " \t")) + ":"
	}
	value = strings.ReplaceAll(value, "\r\n", "")
	value = e2eWSP.ReplaceAllString(value, " ")
	return strings.ToLower(strings.Trim(name, " \t")) + ":" + strings.Trim(value, " ")
}

func e2eCanonBody(body string) string {
	if body == "" {
		return ""
	}
	lines := strings.Split(body, "\r\n")
	for index, line := range lines {
		lines[index] = strings.TrimRight(e2eWSP.ReplaceAllString(line, " "), " ")
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\r\n") + "\r\n"
}

func e2eTags(value string) map[string]string {
	tags := map[string]string{}
	for _, part := range strings.Split(strings.ReplaceAll(value, "\r\n", ""), ";") {
		key, tagValue, found := strings.Cut(part, "=")
		if !found {
			continue
		}
		tags[strings.Trim(key, " \t")] = strings.Trim(tagValue, " \t")
	}
	return tags
}

// e2eVerifyDkim checks the topmost DKIM-Signature of message against one of the
// TXT records the server published, choosing the record by the signature's own
// s= and d= tags. It reports the first thing that does not hold.
func e2eVerifyDkim(message string, published map[string]string, zone string) error {
	fields, body := e2eSplitMessage(message)

	raw := ""
	for _, field := range fields {
		if field.name == "dkim-signature" {
			raw = field.raw
			break
		}
	}
	if raw == "" {
		return errNoDkim
	}
	_, value, _ := strings.Cut(raw, ":")
	tags := e2eTags(value)

	if tags["v"] != "1" {
		return fmt.Errorf("v= is %q, want 1", tags["v"])
	}
	if tags["d"] == "" {
		return errors.New("the signature names no d= domain")
	}
	owner := tags["s"] + "._domainkey." + strings.TrimSuffix(tags["d"], ".")
	record, ok := published[owner]
	if !ok {
		return fmt.Errorf("the signature names %s, which the zone %s does not publish (published: %v)",
			owner, zone, e2eKeys(published))
	}

	bodyHash := sha256.Sum256([]byte(e2eCanonBody(body)))
	if got := base64.StdEncoding.EncodeToString(bodyHash[:]); got != tags["bh"] {
		return fmt.Errorf("bh= is %q, but the canonicalised body hashes to %q", tags["bh"], got)
	}

	var signed strings.Builder
	consumed := map[string]int{}
	for _, name := range strings.Split(tags["h"], ":") {
		name = strings.ToLower(strings.Trim(name, " \t"))
		if name == "" {
			continue
		}
		skip := consumed[name]
		index := -1
		for candidate := len(fields) - 1; candidate >= 0; candidate-- {
			if fields[candidate].name != name {
				continue
			}
			if skip > 0 {
				skip--
				continue
			}
			index = candidate
			break
		}
		if index < 0 {
			continue
		}
		consumed[name]++
		signed.WriteString(e2eCanonHeader(fields[index].raw))
		signed.WriteString("\r\n")
	}
	signed.WriteString(e2eBTag.ReplaceAllString(e2eCanonHeader(raw), "${1}${2}"))
	digest := sha256.Sum256([]byte(signed.String()))

	signature, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(tags["b"]), ""))
	if err != nil {
		return fmt.Errorf("b= is not base64: %w", err)
	}
	key, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(e2eTags(record)["p"]), ""))
	if err != nil {
		return fmt.Errorf("the published p= is not base64: %w", err)
	}

	switch tags["a"] {
	case "rsa-sha256":
		parsed, parseErr := x509.ParsePKIXPublicKey(key)
		if parseErr != nil {
			return fmt.Errorf("the published p= is not a SubjectPublicKeyInfo: %w", parseErr)
		}
		public, isRSA := parsed.(*rsa.PublicKey)
		if !isRSA {
			return fmt.Errorf("the published p= is a %T, not an RSA key", parsed)
		}
		if verifyErr := rsa.VerifyPKCS1v15(public, crypto.SHA256, digest[:], signature); verifyErr != nil {
			return fmt.Errorf("the RSA signature does not verify against the published key: %w", verifyErr)
		}
	case "ed25519-sha256":
		if len(key) != ed25519.PublicKeySize {
			return fmt.Errorf("the published p= is %d bytes, want %d", len(key), ed25519.PublicKeySize)
		}
		if !ed25519.Verify(ed25519.PublicKey(key), digest[:], signature) {
			return errors.New("the Ed25519 signature does not verify against the published key")
		}
	default:
		return fmt.Errorf("unsupported a= %q", tags["a"])
	}
	return nil
}

func e2eKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	return out
}

// -----------------------------------------------------------------------------
// Reading the rendered zone file the way the facade reads it
// -----------------------------------------------------------------------------

// e2eZoneEntry is one publishable record: the facade's parser sees these and
// nothing else.
type e2eZoneEntry struct {
	Owner string
	Type  string
	RData string
}

// e2eParseZoneFile reads the rendered dnsZoneFile the way internal/mail
// /zonefile.go does: everything from an unquoted ';' to the end of the line is
// dropped BEFORE the record is parsed, parenthesised character-strings are
// joined, and what is left is what the facade publishes. A line the server
// commented out therefore contributes nothing here — which is the difference
// between "the text mentions the record" and "the record is published".
func e2eParseZoneFile(text string) []e2eZoneEntry {
	var statements []string
	var current strings.Builder
	depth := 0
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line = e2eStripComment(line)
		if strings.TrimSpace(line) == "" && depth == 0 {
			continue
		}
		current.WriteString(" ")
		current.WriteString(line)
		depth += e2eParenDepth(line)
		if depth <= 0 {
			statements = append(statements, current.String())
			current.Reset()
			depth = 0
		}
	}
	if strings.TrimSpace(current.String()) != "" {
		statements = append(statements, current.String())
	}

	var entries []e2eZoneEntry
	for _, statement := range statements {
		fields := e2eSplitFields(strings.NewReplacer("(", " ", ")", " ").Replace(statement))
		if len(fields) < 3 {
			continue
		}
		owner := fields[0]
		rest := fields[1:]
		if strings.EqualFold(rest[0], "IN") {
			rest = rest[1:]
		}
		if len(rest) < 2 {
			continue
		}
		entries = append(entries, e2eZoneEntry{
			Owner: strings.TrimSuffix(owner, "."),
			Type:  strings.ToUpper(rest[0]),
			RData: strings.Join(rest[1:], " "),
		})
	}
	return entries
}

// e2eStripComment cuts a line at its first ';' outside a quoted string.
func e2eStripComment(line string) string {
	quoted := false
	for index := 0; index < len(line); index++ {
		switch line[index] {
		case '"':
			quoted = !quoted
		case ';':
			if !quoted {
				return line[:index]
			}
		}
	}
	return line
}

func e2eParenDepth(line string) int {
	quoted, depth := false, 0
	for index := 0; index < len(line); index++ {
		switch line[index] {
		case '"':
			quoted = !quoted
		case '(':
			if !quoted {
				depth++
			}
		case ')':
			if !quoted {
				depth--
			}
		}
	}
	return depth
}

// e2eSplitFields splits on whitespace but keeps a quoted character-string whole
// and joins the sequence of them, which is how a TXT rdata is compared on the
// wire.
func e2eSplitFields(statement string) []string {
	var fields []string
	var current strings.Builder
	var quotedText strings.Builder
	quoted, sawQuote := false, false
	flush := func() {
		if sawQuote {
			fields = append(fields, quotedText.String())
			quotedText.Reset()
			sawQuote = false
			return
		}
		if current.Len() > 0 {
			fields = append(fields, current.String())
			current.Reset()
		}
	}
	for index := 0; index < len(statement); index++ {
		char := statement[index]
		switch {
		case char == '"':
			quoted = !quoted
			sawQuote = true
		case quoted:
			quotedText.WriteByte(char)
		case char == ' ' || char == '\t':
			if !sawQuote {
				flush()
				continue
			}
		default:
			current.WriteByte(char)
		}
	}
	// A trailing quoted run and a trailing bare token both have to land.
	if sawQuote {
		fields = append(fields, quotedText.String())
	} else if current.Len() > 0 {
		fields = append(fields, current.String())
	}
	return fields
}

// e2eDkimRecords indexes the published DKIM TXT records by owner.
func e2eDkimRecords(zoneFile string) map[string]string {
	records := map[string]string{}
	for _, entry := range e2eParseZoneFile(zoneFile) {
		if entry.Type == "TXT" && strings.Contains(entry.Owner, "._domainkey.") {
			records[entry.Owner] = entry.RData
		}
	}
	return records
}

// -----------------------------------------------------------------------------
// The journey
// -----------------------------------------------------------------------------

// TestEndToEndTheWholeJourney is the one test the feature has to survive:
// create a domain, create a mailbox, send mail into it from the internet, read
// it back as the customer, add a forwarder, watch the queue runner hand the
// forwarded copy to another mail server over a real socket, then destroy the
// account and find the mail actually gone.
func TestEndToEndTheWholeJourney(t *testing.T) {
	harness := e2eStart(t)
	client := harness.client
	ctx := context.Background()

	// --- 1. create a domain -------------------------------------------------
	domain, err := client.CreateDomain(ctx, stalwart.DomainInput{
		Name:             e2eZone,
		Description:      "end to end",
		ReportAddressURI: "mailto:dmarc@" + e2eZone,
	})
	if err != nil {
		t.Fatalf("CreateDomain: %v", err)
	}
	if domain.ID == "" || !domain.Enabled {
		t.Fatalf("CreateDomain returned %+v", domain)
	}

	// --- 2. create a mailbox ------------------------------------------------
	account, err := client.CreateAccount(ctx, stalwart.AccountInput{
		LocalPart:  "ada",
		DomainID:   domain.ID,
		Secret:     e2ePassphrase,
		QuotaBytes: 1 << 30,
	})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	if account.EmailAddress != "ada@"+e2eZone {
		t.Fatalf("CreateAccount emailAddress = %q", account.EmailAddress)
	}

	// --- 3. send mail into it over SMTP ------------------------------------
	inbound := e2eMessage(e2eSender, account.EmailAddress, "journey-inbound", "the body a customer must be able to read")
	e2eSubmit(t, harness.server.Addr(maild.ListenerSMTP), e2eSender, []string{account.EmailAddress}, inbound)

	// --- 4. read it back over POP3 -----------------------------------------
	pop3 := tlsDial(t, harness.server.Addr(maild.ListenerPOP3S))
	defer func() { drop(pop3.Close()) }()
	reader := bufio.NewReader(pop3)
	pop3Expect(t, reader, "banner")
	pop3Send(t, pop3, "USER %s", account.EmailAddress)
	pop3Expect(t, reader, "USER")
	pop3Send(t, pop3, "PASS %s", e2ePassphrase)
	pop3Expect(t, reader, "PASS")
	pop3Send(t, pop3, "STAT")
	if stat := pop3Expect(t, reader, "STAT"); !strings.HasPrefix(stat, "+OK 1 ") {
		t.Fatalf("STAT = %q, want exactly the one delivered message", stat)
	}
	pop3Send(t, pop3, "RETR 1")
	pop3Expect(t, reader, "RETR")
	retrieved := readUntilDot(t, reader)
	for _, want := range []string{
		"Subject: journey-inbound",
		"the body a customer must be able to read",
		"Delivered-To: " + account.EmailAddress,
		"Return-Path: <" + e2eSender + ">",
	} {
		if !strings.Contains(retrieved, want) {
			t.Errorf("the retrieved message does not carry %q; got:\n%s", want, retrieved)
		}
	}
	pop3Send(t, pop3, "QUIT")
	pop3Expect(t, reader, "QUIT")
	drop(pop3.Close())

	// --- 5. create a forwarder to an external address ----------------------
	external := "archive@" + e2eExternalDomain
	if _, err := client.CreateList(ctx, stalwart.ListInput{
		LocalPart:   "forward",
		DomainID:    domain.ID,
		Description: stalwart.ListDescription,
		Recipients:  []string{external},
	}); err != nil {
		t.Fatalf("CreateList: %v", err)
	}

	// --- 6. the queue runner delivers it to another mail server ------------
	forwarded := e2eMessage(e2eSender, "forward@"+e2eZone, "journey-forwarded", "the body that has to leave the building")
	e2eSubmit(t, harness.server.Addr(maild.ListenerSMTP), e2eSender, []string{"forward@" + e2eZone}, forwarded)

	// The queue must show the message before it is delivered, attributable to
	// the customer's zone; that is the only mail figure the console shows.
	queued, _, err := client.QueryQueue(ctx, e2eZone, 50)
	if err != nil {
		t.Fatalf("QueryQueue: %v", err)
	}
	if len(queued) == 1 && len(queued[0].Recipients) == 1 {
		if got := queued[0].Recipients[0].ORCPT; got != "forward@"+e2eZone {
			t.Errorf("the queued recipient's orcpt = %q, want the customer's own address", got)
		}
	}

	delivered := harness.mx.await(t, "the forwarded copy should have been relayed out")

	if delivered.From != e2eSender {
		t.Errorf("the relayed envelope sender is %q, want the original sender %q", delivered.From, e2eSender)
	}
	if len(delivered.To) != 1 || delivered.To[0] != external {
		t.Errorf("the relayed envelope recipients are %v, want exactly [%s]", delivered.To, external)
	}
	if delivered.EHLO != testHostname {
		t.Errorf("the relay greeted with %q, want MAIL_HOSTNAME %q — a receiver checking forward-confirmed reverse DNS compares exactly this", delivered.EHLO, testHostname)
	}
	if !strings.Contains(delivered.Data, "Subject: journey-forwarded") {
		t.Errorf("the relayed message is not the one that was sent:\n%s", delivered.Data)
	}
	if !strings.Contains(delivered.Data, "Received:") {
		t.Errorf("the relayed message carries no Received header, so a loop cannot be detected downstream:\n%s", delivered.Data)
	}

	// The dial must have been for the resolved MX name on the configured port.
	select {
	case address := <-harness.dialed:
		if want := net.JoinHostPort(e2eExternalMX, harness.mx.Port()); address != want {
			t.Errorf("the relay dialled %q, want %q", address, want)
		}
	default:
		t.Error("the relay delivered a message without dialling anything, which cannot have happened over a socket")
	}

	// A delivered message must leave the queue, or the console reports mail as
	// stuck forever and the runner retries a message the far end already took.
	e2ePoll(t, "the delivered message leaving the queue", func() (bool, string) {
		remaining, _, queryErr := client.QueryQueue(ctx, e2eZone, 50)
		if queryErr != nil {
			return false, queryErr.Error()
		}
		return len(remaining) == 0, fmt.Sprintf("%d message(s) still queued", len(remaining))
	})

	// --- 7. the DKIM signature on outbound mail must verify -----------------
	rendered, err := client.GetDomain(ctx, domain.ID)
	if err != nil {
		t.Fatalf("GetDomain: %v", err)
	}
	published := e2eDkimRecords(rendered.DNSZoneFile)
	if len(published) == 0 {
		t.Fatalf("the rendered zone file publishes no DKIM record at all:\n%s", rendered.DNSZoneFile)
	}
	if err := e2eVerifyDkim(delivered.Data, published, e2eZone); err != nil {
		t.Errorf("the DKIM signature on the message this server relayed does not verify: %v\n"+
			"The facade publishes %v into the customer's zone, so every receiver that checks DKIM\n"+
			"is being told to expect a signature that is not there.\nrelayed message:\n%s",
			err, e2eKeys(published), delivered.Data)
	}

	// --- 8. destroy the account; the maildir must go with it ----------------
	mailbox := maild.NewMaildir(harness.dataDir).Path(account.ID)
	if _, err := os.Stat(mailbox); err != nil {
		t.Fatalf("the mailbox directory %s does not exist before the destroy: %v", mailbox, err)
	}
	if err := client.DestroyAccount(ctx, account.ID); err != nil {
		t.Fatalf("DestroyAccount: %v", err)
	}
	if _, err := os.Stat(mailbox); !os.IsNotExist(err) {
		t.Errorf("the maildir %s survives DestroyAccount (stat error %v); the customer's mail outlives its only owner", mailbox, err)
	}
	remaining, err := client.ListAccounts(ctx, domain.ID)
	if err != nil {
		t.Fatalf("ListAccounts after destroy: %v", err)
	}
	if len(remaining) != 0 {
		t.Errorf("ListAccounts still reports %d account(s) after DestroyAccount", len(remaining))
	}
}

// -----------------------------------------------------------------------------
// The autoconfiguration records the facade actually publishes
// -----------------------------------------------------------------------------

// TestRenderedZoneFilePublishesTheAutoconfigRecords reads the rendered
// dnsZoneFile the way internal/mail/zonefile.go reads it, which is the only
// reading that matters: a line commented out with ';' is cut before parsing and
// publishes nothing.
//
// integration_test.go's TestZoneFileCarriesTheAutoconfigRecordsTheFacadePublishes
// asserts strings.Contains(dnsZoneFile, owner+"."+zone), which a COMMENT
// satisfies. This test therefore distinguishes the two, and says out loud which
// of the eight owners on the facade's allow-list are published and which are
// text.
func TestRenderedZoneFilePublishesTheAutoconfigRecords(t *testing.T) {
	harness := e2eStart(t)
	ctx := context.Background()

	domain, err := harness.client.CreateDomain(ctx, stalwart.DomainInput{Name: e2eZone})
	if err != nil {
		t.Fatalf("CreateDomain: %v", err)
	}
	rendered, err := harness.client.GetDomain(ctx, domain.ID)
	if err != nil {
		t.Fatalf("GetDomain: %v", err)
	}

	live := map[string]e2eZoneEntry{}
	for _, entry := range e2eParseZoneFile(rendered.DNSZoneFile) {
		live[entry.Owner] = entry
	}

	// The listeners this server actually binds are submission-over-implicit-TLS
	// and POP3S, so those two records are the ones it may honestly publish, and
	// it must publish them: publish_client_autoconfig defaults on and a console
	// reporting the feature as enabled with nothing behind it is a lie to the
	// customer.
	for _, offered := range []struct {
		owner string
		want  string
	}{
		{"_submissions._tcp." + e2eZone, fmt.Sprintf("0 1 465 %s.", testHostname)},
		{"_pop3s._tcp." + e2eZone, fmt.Sprintf("0 1 995 %s.", testHostname)},
	} {
		entry, ok := live[offered.owner]
		if !ok {
			t.Errorf("the rendered zone publishes no %s record, so the facade publishes none", offered.owner)
			continue
		}
		if entry.Type != "SRV" {
			t.Errorf("%s is a %s record, want SRV", offered.owner, entry.Type)
		}
		if entry.RData != offered.want {
			t.Errorf("%s rdata = %q, want %q — the facade drops any record whose target is not exactly MAIL_HOSTNAME",
				offered.owner, entry.RData, offered.want)
		}
	}

	// Everything on the facade's allow-list, and what this server does with it.
	var withheld []string
	for _, owner := range []string{
		"_submissions._tcp", "_imaps._tcp", "_pop3s._tcp",
		"_jmap._tcp", "_caldavs._tcp", "_carddavs._tcp",
		"autoconfig", "autodiscover",
	} {
		full := owner + "." + e2eZone
		mentioned := strings.Contains(rendered.DNSZoneFile, full)
		_, isLive := live[full]
		switch {
		case isLive:
			t.Logf("PUBLISHED %-20s %s", owner, live[full].RData)
		case mentioned:
			withheld = append(withheld, owner)
			t.Logf("WITHHELD  %-20s appears in the text as a comment only; the facade publishes nothing", owner)
		default:
			t.Errorf("%s is neither published nor named in the rendered zone file", owner)
		}
	}
	if len(withheld) > 0 {
		t.Logf("FINDING: %d of the 8 owners on the facade's allow-list (%s) are rendered as comments.\n"+
			"That is the honest answer for a server with no IMAP, JMAP, CalDAV, CardDAV or HTTPS autoconfig\n"+
			"listener, but integration_test.go's substring assertion cannot tell a comment from a record,\n"+
			"so it would stay green if the two REAL records were commented out too.",
			len(withheld), strings.Join(withheld, ", "))
	}
}

// -----------------------------------------------------------------------------
// The two wiring defects the journey walks past
// -----------------------------------------------------------------------------

// TestProductionWiringHasNoQueueRunner is the finding that makes the outbound
// leg of the journey above a test of code that is never reached in production.
//
// e2eStart builds the queue itself. Nothing in cmd/deephost-mail does: services
// .go constructs a Deliverer, an SMTPServer, a POP3Server and a Handler, and no
// Queue and no Relay. The Deliverer therefore spools a forwarded message and
// records a queue entry that nothing will ever pick up, so a customer's
// forwarder accepts mail at the SMTP door and silently never delivers it.
func TestProductionWiringHasNoQueueRunner(t *testing.T) {
	source, err := os.ReadFile("../../cmd/deephost-mail/services.go")
	if err != nil {
		t.Fatalf("read the production wiring: %v", err)
	}
	text := string(source)
	for _, want := range []string{"NewQueue", "NewRelay"} {
		if !strings.Contains(text, want) {
			t.Errorf("cmd/deephost-mail/services.go never calls maild.%s, so the deployed binary has no outbound "+
				"delivery at all: a forwarded message is accepted, spooled and queued, and nothing ever sends it.", want)
		}
	}
}

// TestAFreshlyQueuedMessageWaitsOutThePollInterval measures the latency defect
// the queue's own author flagged and could not fix from their file.
//
// Queue.Kick exists so that a message queued a moment ago is attempted now
// rather than at the next tick, and no caller uses it: Deliverer.enqueue writes
// the queue entry and returns. This test runs the runner at the production
// default interval and gives a forwarded message a window far shorter than that
// interval and far longer than a delivery over loopback needs. A server that
// kicked the runner would deliver in milliseconds and pass.
func TestAFreshlyQueuedMessageWaitsOutThePollInterval(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out a window at the production poll interval")
	}
	harness := e2eStartWithInterval(t, maild.DefaultQueueInterval)
	ctx := context.Background()

	domain, err := harness.client.CreateDomain(ctx, stalwart.DomainInput{Name: e2eZone})
	if err != nil {
		t.Fatalf("CreateDomain: %v", err)
	}
	if _, err := harness.client.CreateList(ctx, stalwart.ListInput{
		LocalPart:   "forward",
		DomainID:    domain.ID,
		Description: stalwart.ListDescription,
		Recipients:  []string{"archive@" + e2eExternalDomain},
	}); err != nil {
		t.Fatalf("CreateList: %v", err)
	}

	// The runner's start-up pass has already happened and found nothing, so the
	// only thing that can deliver this promptly is a kick.
	message := e2eMessage(e2eSender, "forward@"+e2eZone, "journey-kick", "this should leave immediately")
	e2eSubmit(t, harness.server.Addr(maild.ListenerSMTP), e2eSender, []string{"forward@" + e2eZone}, message)

	// A window that is generous for a loopback delivery and a small fraction of
	// the poll interval. It is a bounded negative assertion, which is the one
	// direction a wait is honest in: a working server has long since arrived.
	window := 3 * time.Second
	if window >= maild.DefaultQueueInterval {
		t.Fatalf("the window %s is not shorter than the poll interval %s", window, maild.DefaultQueueInterval)
	}
	timer := time.NewTimer(window)
	defer timer.Stop()
	select {
	case <-harness.mx.inbox:
		// Delivered promptly: enqueue must now be kicking the runner.
	case <-timer.C:
		t.Errorf("a forwarded message was still unsent %s after the server accepted it, because "+
			"Deliverer.enqueue never calls Queue.Kick(). At the production poll interval of %s that is how "+
			"long every forwarder makes its mail wait.", window, maild.DefaultQueueInterval)
	}
}

// -----------------------------------------------------------------------------
// The leg the queue exists for when the far end says no
// -----------------------------------------------------------------------------

// TestAPermanentRefusalBouncesBackToTheSender proves the other half of the
// outbound contract over real sockets: when the receiving server refuses a
// recipient permanently, the sender is TOLD. A queue that silently retired a
// refused message would leave a customer believing their forwarder works.
func TestAPermanentRefusalBouncesBackToTheSender(t *testing.T) {
	harness := e2eStart(t)
	ctx := context.Background()

	domain, err := harness.client.CreateDomain(ctx, stalwart.DomainInput{Name: e2eZone})
	if err != nil {
		t.Fatalf("CreateDomain: %v", err)
	}
	if _, err := harness.client.CreateList(ctx, stalwart.ListInput{
		LocalPart:   "forward",
		DomainID:    domain.ID,
		Description: stalwart.ListDescription,
		Recipients:  []string{"nobody@" + e2eExternalDomain},
	}); err != nil {
		t.Fatalf("CreateList: %v", err)
	}

	// The sender is a domain the fake receiver also answers for, so the bounce
	// this server generates has somewhere real to go.
	harness.mx.refuseRecipient("nobody@"+e2eExternalDomain, "550 5.1.1 no such user here")

	sender := "bounce-me@" + e2eExternalDomain
	message := e2eMessage(sender, "forward@"+e2eZone, "journey-bounce", "this one cannot be delivered")
	e2eSubmit(t, harness.server.Addr(maild.ListenerSMTP), sender, []string{"forward@" + e2eZone}, message)

	// The refusal is permanent, so the runner must not retry it for five days:
	// it must compose a bounce. Once the bounce is queued the receiver has to
	// accept it, so the refusal is lifted before waiting for it.
	e2ePoll(t, "the queue reacting to a permanent refusal", func() (bool, string) {
		queued, _, queryErr := harness.client.QueryQueue(ctx, e2eZone, 50)
		if queryErr != nil {
			return false, queryErr.Error()
		}
		if len(queued) == 0 {
			return true, "the refused message has left the queue"
		}
		for _, recipient := range queued[0].Recipients {
			if recipient.Status == "PermanentFailure" {
				return true, "the recipient is marked as a permanent failure"
			}
		}
		return false, fmt.Sprintf("%d message(s) queued, first recipients %+v", len(queued), queued[0].Recipients)
	})
	bounce := harness.mx.await(t, "a bounce should have been generated for a permanently refused message")
	if bounce.From != "" {
		t.Errorf("the bounce was sent with return path %q, want the null sender: a bounce that can itself bounce is a loop", bounce.From)
	}
	if len(bounce.To) != 1 || bounce.To[0] != sender {
		t.Errorf("the bounce went to %v, want the original sender %q", bounce.To, sender)
	}
	for _, want := range []string{
		"multipart/report",
		"report-type=delivery-status",
		"Action: failed",
		"Final-Recipient: rfc822; nobody@" + e2eExternalDomain,
		// ORCPT is what makes a fan-out attributable to the customer's own
		// address rather than to nobody. It is set on the queue entry by
		// expandList and it has to survive all the way into the report.
		"Original-Recipient: rfc822; forward@" + e2eZone,
	} {
		if !strings.Contains(bounce.Data, want) {
			t.Errorf("the bounce does not carry %q; RFC 3464 is what a sender's own software reads. Got:\n%s", want, bounce.Data)
		}
	}
	if strings.Contains(bounce.Data, "this one cannot be delivered") {
		t.Errorf("the bounce quotes the message BODY; two servers doing that to each other is an amplifier. Got:\n%s", bounce.Data)
	}
}
