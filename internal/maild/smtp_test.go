package maild

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// -----------------------------------------------------------------------------
// A client that speaks SMTP back
//
// The sessions below run over net.Pipe, so there is no network, no port and no
// timing: a write blocks until the server reads it, which makes every exchange
// in these tests strictly ordered.
// -----------------------------------------------------------------------------

const testIOTimeout = 10 * time.Second

type smtpTestClient struct {
	t      *testing.T
	conn   net.Conn
	reader *bufio.Reader
}

func startSMTPSession(t *testing.T, mailer Mailer, config SMTPConfig) *smtpTestClient {
	t.Helper()
	client, _ := startSMTPSessionWithServer(t, mailer, config)
	return client
}

func startSMTPSessionWithServer(t *testing.T, mailer Mailer, config SMTPConfig) (*smtpTestClient, *SMTPServer) {
	t.Helper()
	if config.Logger == nil {
		config.Logger = discardLogger()
	}
	server, err := NewSMTPServer(mailer, config)
	if err != nil {
		t.Fatalf("build the SMTP server: %v", err)
	}
	clientConn, serverConn := net.Pipe()
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		server.ServeConn(serverConn)
	}()
	t.Cleanup(func() {
		if err := clientConn.Close(); err != nil && !errors.Is(err, io.ErrClosedPipe) {
			t.Errorf("close the client: %v", err)
		}
		select {
		case <-finished:
		case <-time.After(testIOTimeout):
			t.Error("the session did not finish after the client went away")
		}
		if err := server.Close(); err != nil {
			t.Errorf("close the server: %v", err)
		}
	})
	client := &smtpTestClient{t: t, conn: clientConn, reader: bufio.NewReader(clientConn)}
	client.expect(220)
	return client, server
}

func (c *smtpTestClient) deadline() {
	c.t.Helper()
	if err := c.conn.SetDeadline(time.Now().Add(testIOTimeout)); err != nil {
		c.t.Fatalf("set a deadline: %v", err)
	}
}

func (c *smtpTestClient) send(line string) {
	c.t.Helper()
	c.deadline()
	if _, err := c.conn.Write([]byte(line + "\r\n")); err != nil {
		c.t.Fatalf("write %q: %v", line, err)
	}
}

// readReply reads one complete reply, following the "250-" continuation form.
func (c *smtpTestClient) readReply() (int, string) {
	c.t.Helper()
	c.deadline()
	var whole strings.Builder
	for {
		line, err := c.reader.ReadString('\n')
		if err != nil {
			c.t.Fatalf("read a reply: %v (so far %q)", err, whole.String())
		}
		whole.WriteString(line)
		trimmed := strings.TrimRight(line, "\r\n")
		if len(trimmed) < 3 {
			c.t.Fatalf("malformed reply line %q", line)
		}
		code, err := strconv.Atoi(trimmed[:3])
		if err != nil {
			c.t.Fatalf("malformed reply code in %q", line)
		}
		if len(trimmed) == 3 || trimmed[3] != '-' {
			return code, whole.String()
		}
	}
}

func (c *smtpTestClient) expect(want int) string {
	c.t.Helper()
	code, text := c.readReply()
	if code != want {
		c.t.Fatalf("got reply %d, want %d:\n%s", code, want, text)
	}
	return text
}

func (c *smtpTestClient) do(line string, want int) string {
	c.t.Helper()
	c.send(line)
	return c.expect(want)
}

// sendMessage runs DATA through to the final dot.
func (c *smtpTestClient) sendMessage(body string) {
	c.t.Helper()
	c.do("DATA", 354)
	c.deadline()
	if _, err := c.conn.Write([]byte(body)); err != nil {
		c.t.Fatalf("write the message: %v", err)
	}
	c.send(".")
}

func (c *smtpTestClient) startTLS() {
	c.t.Helper()
	c.do("STARTTLS", 220)
	tlsConn := tls.Client(c.conn, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12})
	c.deadline()
	if err := tlsConn.Handshake(); err != nil {
		c.t.Fatalf("the client handshake failed: %v", err)
	}
	c.conn = tlsConn
	c.reader = bufio.NewReader(tlsConn)
}

// -----------------------------------------------------------------------------
// Fakes and fixtures
// -----------------------------------------------------------------------------

// fakeMailer stands in for the delivery path where a test needs a failure that
// is awkward to arrange for real — a store that is down, for instance. Every
// test that can use the real *Deliverer does.
type fakeMailer struct {
	authenticate func(ctx context.Context, username, password string) (Identity, error)
	resolve      func(ctx context.Context, address string, authenticated bool) (Recipient, error)
	deliver      func(ctx context.Context, envelope *Envelope) error

	mu            sync.Mutex
	authAttempts  int
	deliverCalls  int
	lastEnvelope  *Envelope
	seenPasswords []string
}

func (f *fakeMailer) Authenticate(ctx context.Context, username, password string) (Identity, error) {
	f.mu.Lock()
	f.authAttempts++
	f.seenPasswords = append(f.seenPasswords, password)
	f.mu.Unlock()
	if f.authenticate != nil {
		return f.authenticate(ctx, username, password)
	}
	return Identity{}, errAuthFailed
}

func (f *fakeMailer) Resolve(ctx context.Context, address string, authenticated bool) (Recipient, error) {
	if f.resolve != nil {
		return f.resolve(ctx, address, authenticated)
	}
	return Recipient{Address: address, Kind: RecipientMailbox, AccountID: "a1"}, nil
}

func (f *fakeMailer) Deliver(ctx context.Context, envelope *Envelope) error {
	f.mu.Lock()
	f.deliverCalls++
	f.lastEnvelope = envelope
	f.mu.Unlock()
	if f.deliver != nil {
		return f.deliver(ctx, envelope)
	}
	return nil
}

func (f *fakeMailer) counts() (auth, deliver int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.authAttempts, f.deliverCalls
}

func (f *fakeMailer) envelope() *Envelope {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastEnvelope
}

// testTLSConfig builds a throwaway self-signed certificate. Ed25519 keeps the
// generation instant, so a TLS test costs nothing.
func testTLSConfig(t *testing.T) *tls.Config {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate a key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "mx.test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"mx.test"},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		t.Fatalf("create a certificate: %v", err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: private}},
		MinVersion:   tls.VersionTLS12,
	}
}

// newHostedFixture wires a real store and a real delivery path with one domain
// and one mailbox, which is what most of these tests actually want to exercise.
func newHostedFixture(t *testing.T) (*Deliverer, *Store, Account) {
	t.Helper()
	store := newStore(t)
	deliverer, err := NewDeliverer(store, DeliveryConfig{
		Root:     filepath.Join(t.TempDir(), "mail"),
		Hostname: "mx.test",
		Logger:   discardLogger(),
		Now:      func() time.Time { return testClock },
	})
	if err != nil {
		t.Fatalf("build the deliverer: %v", err)
	}
	domain := seedDomain(t, store, "example.test")
	ada := setTestPassword(t, store, seedAccount(t, store, domain, "ada"), "a good passphrase")
	return deliverer, store, ada
}

func plainCredential(username, password string) string {
	return base64.StdEncoding.EncodeToString([]byte("\x00" + username + "\x00" + password))
}

// -----------------------------------------------------------------------------
// The four behaviours this server exists to get right
// -----------------------------------------------------------------------------

func TestSMTPDeliversAMessage(t *testing.T) {
	deliverer, _, ada := newHostedFixture(t)
	client := startSMTPSession(t, deliverer, SMTPConfig{Hostname: "mx.test"})

	greeting := client.do("EHLO client.test", 250)
	for _, want := range []string{"PIPELINING", "SIZE ", "8BITMIME", "ENHANCEDSTATUSCODES"} {
		if !strings.Contains(greeting, want) {
			t.Errorf("the greeting does not advertise %q:\n%s", want, greeting)
		}
	}
	client.do("MAIL FROM:<sender@elsewhere.test>", 250)
	client.do("RCPT TO:<ada@example.test>", 250)
	client.sendMessage("Subject: hello\r\n\r\nThis is the body.\r\n")
	client.expect(250)
	client.do("QUIT", 221)

	messages := mailboxMessages(t, deliverer, ada.ID)
	if len(messages) != 1 {
		t.Fatalf("delivered %d messages, want 1", len(messages))
	}
	for _, want := range []string{
		"Return-Path: <sender@elsewhere.test>\r\n",
		"Delivered-To: ada@example.test\r\n",
		"Received: from client.test (pipe)",
		"by mx.test with ESMTP id m",
		"for <ada@example.test>;",
		"Subject: hello\r\n\r\nThis is the body.\r\n",
	} {
		if !strings.Contains(messages[0], want) {
			t.Errorf("the stored message is missing %q:\n%s", want, messages[0])
		}
	}
}

func TestSMTPRejectsAnUnknownRecipient(t *testing.T) {
	deliverer, _, _ := newHostedFixture(t)
	client := startSMTPSession(t, deliverer, SMTPConfig{Hostname: "mx.test"})

	client.do("EHLO client.test", 250)
	client.do("MAIL FROM:<sender@elsewhere.test>", 250)

	reply := client.do("RCPT TO:<nobody@example.test>", 550)
	if !strings.Contains(reply, "5.1.1") {
		t.Errorf("an unknown user must be 5.1.1, got %q", reply)
	}

	// A refused recipient is not a refused session: the transaction is still
	// open and a good address still works.
	client.do("RCPT TO:<ada@example.test>", 250)
	client.sendMessage("Subject: hello\r\n\r\nbody\r\n")
	client.expect(250)
}

func TestSMTPRefusesAuthOnACleartextSession(t *testing.T) {
	mailer := &fakeMailer{
		authenticate: func(context.Context, string, string) (Identity, error) {
			return Identity{AccountID: "a1", Addresses: []string{"ada@example.test"}}, nil
		},
	}
	// TLS is configured, so the server could offer AUTH after STARTTLS. It must
	// still not offer or accept it before the handshake.
	client := startSMTPSession(t, mailer, SMTPConfig{Hostname: "mx.test", TLSConfig: testTLSConfig(t)})

	greeting := client.do("EHLO client.test", 250)
	if strings.Contains(greeting, "AUTH") {
		t.Errorf("a cleartext session advertised AUTH:\n%s", greeting)
	}
	if !strings.Contains(greeting, "STARTTLS") {
		t.Errorf("the greeting should advertise STARTTLS:\n%s", greeting)
	}

	for _, command := range []string{
		"AUTH PLAIN " + plainCredential("ada@example.test", "a good passphrase"),
		"AUTH PLAIN",
		"AUTH LOGIN",
		"AUTH SOMETHING",
	} {
		reply := client.do(command, 538)
		if !strings.Contains(reply, "5.7.11") {
			t.Errorf("%q: want 538 5.7.11, got %q", command, reply)
		}
	}

	// The decisive assertion: no credential was even handed to the mailer.
	if attempts, _ := mailer.counts(); attempts != 0 {
		t.Fatalf("the server verified %d credentials over cleartext, want 0", attempts)
	}
}

func TestSMTPRefusesAnOversizedMessage(t *testing.T) {
	deliverer, _, ada := newHostedFixture(t)
	client := startSMTPSession(t, deliverer, SMTPConfig{Hostname: "mx.test", MaxMessageBytes: 1024})

	greeting := client.do("EHLO client.test", 250)
	if !strings.Contains(greeting, "SIZE 1024") {
		t.Errorf("the advertised size is not the configured one:\n%s", greeting)
	}

	// A client that declares the size up front is refused before it sends.
	reply := client.do("MAIL FROM:<sender@elsewhere.test> SIZE=99999", 552)
	if !strings.Contains(reply, "5.3.4") {
		t.Errorf("want 5.3.4, got %q", reply)
	}

	// A client that does not declare it is refused after the final dot, and the
	// session survives so it can send something smaller.
	client.do("MAIL FROM:<sender@elsewhere.test>", 250)
	client.do("RCPT TO:<ada@example.test>", 250)
	client.sendMessage("Subject: big\r\n\r\n" + strings.Repeat("padding padding\r\n", 200))
	reply = client.expect(552)
	if !strings.Contains(reply, "5.3.4") {
		t.Errorf("want 5.3.4, got %q", reply)
	}
	if messages := mailboxMessages(t, deliverer, ada.ID); len(messages) != 0 {
		t.Fatalf("an oversized message was stored anyway: %d files", len(messages))
	}

	client.do("MAIL FROM:<sender@elsewhere.test>", 250)
	client.do("RCPT TO:<ada@example.test>", 250)
	client.sendMessage("Subject: small\r\n\r\nbody\r\n")
	client.expect(250)
	if messages := mailboxMessages(t, deliverer, ada.ID); len(messages) != 1 {
		t.Fatalf("the session did not recover: %d messages", len(messages))
	}
}

// TestSMTPReportsATemporaryFailure covers both places a 4xx can appear: at RCPT
// and after the final dot.
func TestSMTPReportsATemporaryFailure(t *testing.T) {
	tests := []struct {
		name     string
		mailer   *fakeMailer
		wantCode int
		atRCPT   bool
	}{
		{
			name: "a recipient that cannot be checked",
			mailer: &fakeMailer{
				resolve: func(context.Context, string, bool) (Recipient, error) {
					return Recipient{}, temporaryf(451, "4.3.0", errors.New("the store is down"),
						"Cannot check that recipient right now")
				},
			},
			wantCode: 451,
			atRCPT:   true,
		},
		{
			name: "a mailbox that is full",
			mailer: &fakeMailer{
				deliver: func(context.Context, *Envelope) error { return errMailboxFull },
			},
			wantCode: 452,
		},
		{
			name: "a failure that does not say what it means is temporary",
			mailer: &fakeMailer{
				deliver: func(context.Context, *Envelope) error { return errors.New("a bug") },
			},
			wantCode: 451,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := startSMTPSession(t, test.mailer, SMTPConfig{Hostname: "mx.test"})
			client.do("EHLO client.test", 250)
			client.do("MAIL FROM:<sender@elsewhere.test>", 250)
			if test.atRCPT {
				reply := client.do("RCPT TO:<ada@example.test>", test.wantCode)
				if !strings.Contains(reply, "4.") {
					t.Errorf("a temporary failure needs a 4.x.x enhanced code: %q", reply)
				}
				return
			}
			client.do("RCPT TO:<ada@example.test>", 250)
			client.sendMessage("Subject: hello\r\n\r\nbody\r\n")
			client.expect(test.wantCode)
		})
	}
}

// -----------------------------------------------------------------------------
// Submission
// -----------------------------------------------------------------------------

func TestSMTPStartTLSThenAuthenticatedSubmission(t *testing.T) {
	deliverer, store, _ := newHostedFixture(t)
	client := startSMTPSession(t, deliverer, SMTPConfig{Hostname: "mx.test", TLSConfig: testTLSConfig(t)})

	client.do("EHLO client.test", 250)
	client.startTLS()

	// RFC 3207: everything before the handshake is forgotten, so the session
	// starts again with EHLO — and only now is AUTH on offer.
	greeting := client.do("EHLO client.test", 250)
	if !strings.Contains(greeting, "AUTH PLAIN LOGIN") {
		t.Fatalf("an encrypted session must offer AUTH:\n%s", greeting)
	}
	if strings.Contains(greeting, "STARTTLS") {
		t.Errorf("STARTTLS was offered inside TLS:\n%s", greeting)
	}
	client.do("AUTH PLAIN "+plainCredential("ada@example.test", "a good passphrase"), 235)

	// An authenticated session may relay; an unauthenticated one may not.
	client.do("MAIL FROM:<ada@example.test>", 250)
	client.do("RCPT TO:<friend@elsewhere.test>", 250)
	client.sendMessage("Subject: out\r\n\r\nbody\r\n")
	client.expect(250)
	client.do("QUIT", 221)

	queued, err := store.ListQueuedMessages(context.Background(), 0)
	if err != nil {
		t.Fatalf("list the queue: %v", err)
	}
	if len(queued) != 1 || len(queued[0].Recipients) != 1 {
		t.Fatalf("queued %+v, want one message for one recipient", queued)
	}
	if queued[0].Recipients[0].Address != "friend@elsewhere.test" {
		t.Errorf("queued %q", queued[0].Recipients[0].Address)
	}
	body, err := readSpooled(deliverer, queued[0].MessagePath)
	if err != nil {
		t.Fatalf("read the spooled body: %v", err)
	}
	if !strings.Contains(body, "with ESMTPSA id ") {
		t.Errorf("the trace header does not record TLS and authentication:\n%s", body)
	}
}

func TestSMTPAuthOverTLSFailsClosed(t *testing.T) {
	deliverer, _, _ := newHostedFixture(t)
	client := startSMTPSession(t, deliverer, SMTPConfig{Hostname: "mx.test", TLSConfig: testTLSConfig(t)})
	client.do("EHLO client.test", 250)
	client.startTLS()
	client.do("EHLO client.test", 250)

	reply := client.do("AUTH PLAIN "+plainCredential("ada@example.test", "the wrong passphrase"), 535)
	if strings.Contains(reply, "passphrase") {
		t.Fatalf("the reply quotes the credential: %q", reply)
	}
	// Still unauthenticated, so still not a relay.
	client.do("MAIL FROM:<ada@example.test>", 250)
	client.do("RCPT TO:<friend@elsewhere.test>", 550)
}

func TestSMTPRefusesToRelayWithoutAuthentication(t *testing.T) {
	deliverer, _, _ := newHostedFixture(t)
	client := startSMTPSession(t, deliverer, SMTPConfig{Hostname: "mx.test"})
	client.do("EHLO client.test", 250)
	client.do("MAIL FROM:<sender@elsewhere.test>", 250)
	reply := client.do("RCPT TO:<friend@elsewhere.test>", 550)
	if !strings.Contains(reply, "5.7.1") {
		t.Fatalf("relay denial must be 5.7.1, got %q", reply)
	}
}

func TestSMTPAuthenticatedSenderMustOwnTheAddress(t *testing.T) {
	deliverer, _, _ := newHostedFixture(t)
	client := startSMTPSession(t, deliverer, SMTPConfig{Hostname: "mx.test", TLSConfig: testTLSConfig(t)})
	client.do("EHLO client.test", 250)
	client.startTLS()
	client.do("EHLO client.test", 250)
	client.do("AUTH PLAIN "+plainCredential("ada@example.test", "a good passphrase"), 235)

	reply := client.do("MAIL FROM:<someone.else@example.test>", 550)
	if !strings.Contains(reply, "5.7.1") {
		t.Errorf("want 5.7.1, got %q", reply)
	}
	// Its own address, and a sub-address of it, are both fine.
	client.do("MAIL FROM:<ada+news@example.test>", 250)
}

func TestSMTPAuthLoginMechanism(t *testing.T) {
	deliverer, _, _ := newHostedFixture(t)
	client := startSMTPSession(t, deliverer, SMTPConfig{Hostname: "mx.test", TLSConfig: testTLSConfig(t)})
	client.do("EHLO client.test", 250)
	client.startTLS()
	client.do("EHLO client.test", 250)

	client.send("AUTH LOGIN")
	prompt := client.expect(334)
	if !strings.Contains(prompt, base64.StdEncoding.EncodeToString([]byte("Username:"))) {
		t.Errorf("unexpected username prompt %q", prompt)
	}
	client.send(base64.StdEncoding.EncodeToString([]byte("ada@example.test")))
	prompt = client.expect(334)
	if !strings.Contains(prompt, base64.StdEncoding.EncodeToString([]byte("Password:"))) {
		t.Errorf("unexpected password prompt %q", prompt)
	}
	client.send(base64.StdEncoding.EncodeToString([]byte("a good passphrase")))
	client.expect(235)
}

func TestSMTPAuthCanBeCancelled(t *testing.T) {
	mailer := &fakeMailer{}
	client := startSMTPSession(t, mailer, SMTPConfig{Hostname: "mx.test", TLSConfig: testTLSConfig(t)})
	client.do("EHLO client.test", 250)
	client.startTLS()
	client.do("EHLO client.test", 250)
	client.send("AUTH PLAIN")
	client.expect(334)
	client.send("*")
	client.expect(501)
	if attempts, _ := mailer.counts(); attempts != 0 {
		t.Errorf("a cancelled exchange still called the mailer %d times", attempts)
	}
}

func TestSMTPStartTLSRefusesPipelining(t *testing.T) {
	mailer := &fakeMailer{}
	client := startSMTPSession(t, mailer, SMTPConfig{Hostname: "mx.test", TLSConfig: testTLSConfig(t)})
	client.do("EHLO client.test", 250)
	// Both lines arrive in one write, so the second is buffered before the
	// handshake. Accepting it would be the STARTTLS command-injection bug.
	client.deadline()
	if _, err := client.conn.Write([]byte("STARTTLS\r\nMAIL FROM:<attacker@elsewhere.test>\r\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	client.expect(501)
}

func TestSMTPStartTLSWithoutACertificate(t *testing.T) {
	mailer := &fakeMailer{}
	client := startSMTPSession(t, mailer, SMTPConfig{Hostname: "mx.test"})
	greeting := client.do("EHLO client.test", 250)
	if strings.Contains(greeting, "STARTTLS") {
		t.Errorf("STARTTLS was advertised without a certificate:\n%s", greeting)
	}
	client.do("STARTTLS", 454)
}

// -----------------------------------------------------------------------------
// Protocol behaviour and limits
// -----------------------------------------------------------------------------

func TestSMTPCommandSequence(t *testing.T) {
	tests := []struct {
		name     string
		commands []string
		want     int
	}{
		{name: "MAIL before a greeting", commands: []string{"MAIL FROM:<a@b.test>"}, want: 503},
		{name: "RCPT before MAIL", commands: []string{"EHLO c.test", "RCPT TO:<ada@example.test>"}, want: 503},
		{name: "DATA before MAIL", commands: []string{"EHLO c.test", "DATA"}, want: 503},
		{
			name:     "DATA before RCPT",
			commands: []string{"EHLO c.test", "MAIL FROM:<a@b.test>", "DATA"},
			want:     503,
		},
		{
			name:     "a second MAIL inside a transaction",
			commands: []string{"EHLO c.test", "MAIL FROM:<a@b.test>", "MAIL FROM:<c@d.test>"},
			want:     503,
		},
		{name: "EHLO without a name", commands: []string{"EHLO"}, want: 501},
		{name: "a malformed MAIL", commands: []string{"EHLO c.test", "MAIL a@b.test"}, want: 501},
		{name: "a malformed RCPT", commands: []string{"EHLO c.test", "MAIL FROM:<a@b.test>", "RCPT"}, want: 501},
		{
			name:     "an address with no domain",
			commands: []string{"EHLO c.test", "MAIL FROM:<nobody>"},
			want:     501,
		},
		{
			name:     "an unsupported MAIL parameter",
			commands: []string{"EHLO c.test", "MAIL FROM:<a@b.test> SOMETHING=1"},
			want:     555,
		},
		{
			name:     "a SIZE that is not a number",
			commands: []string{"EHLO c.test", "MAIL FROM:<a@b.test> SIZE=lots"},
			want:     501,
		},
		{name: "an unknown verb", commands: []string{"WIDGET"}, want: 500},
		{name: "EXPN is refused", commands: []string{"EHLO c.test", "EXPN staff"}, want: 502},
		{name: "VRFY is neither confirmed nor denied", commands: []string{"EHLO c.test", "VRFY ada"}, want: 252},
		{name: "HELO is accepted", commands: []string{"HELO c.test"}, want: 250},
		{name: "NOOP", commands: []string{"NOOP"}, want: 250},
		{name: "RSET", commands: []string{"EHLO c.test", "MAIL FROM:<a@b.test>", "RSET"}, want: 250},
		{name: "HELP", commands: []string{"HELP"}, want: 214},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := startSMTPSession(t, &fakeMailer{}, SMTPConfig{Hostname: "mx.test"})
			for index, command := range test.commands {
				want := 250
				if index == len(test.commands)-1 {
					want = test.want
				}
				client.do(command, want)
			}
		})
	}
}

func TestSMTPRSETClearsTheTransaction(t *testing.T) {
	mailer := &fakeMailer{}
	client := startSMTPSession(t, mailer, SMTPConfig{Hostname: "mx.test"})
	client.do("EHLO client.test", 250)
	client.do("MAIL FROM:<sender@elsewhere.test>", 250)
	client.do("RCPT TO:<ada@example.test>", 250)
	client.do("RSET", 250)
	client.do("DATA", 503)
	if _, delivered := mailer.counts(); delivered != 0 {
		t.Errorf("a reset transaction was delivered %d times", delivered)
	}
}

func TestSMTPBoundsRecipients(t *testing.T) {
	client := startSMTPSession(t, &fakeMailer{}, SMTPConfig{Hostname: "mx.test", MaxRecipients: 2})
	client.do("EHLO client.test", 250)
	client.do("MAIL FROM:<sender@elsewhere.test>", 250)
	client.do("RCPT TO:<one@example.test>", 250)
	client.do("RCPT TO:<two@example.test>", 250)
	reply := client.do("RCPT TO:<three@example.test>", 452)
	if !strings.Contains(reply, "4.5.3") {
		t.Errorf("want 4.5.3, got %q", reply)
	}
}

func TestSMTPDropsAClientThatOnlyGuesses(t *testing.T) {
	client := startSMTPSession(t, &fakeMailer{}, SMTPConfig{Hostname: "mx.test", MaxErrors: 3})
	client.do("WIDGET", 500)
	client.do("WIDGET", 500)
	client.send("WIDGET")
	client.expect(500)
	// The budget is spent, so the third failure is followed by a farewell.
	client.expect(421)
	if _, err := client.reader.ReadString('\n'); err == nil {
		t.Fatal("the connection stayed open after 421")
	}
}

func TestSMTPDeduplicatesRecipients(t *testing.T) {
	mailer := &fakeMailer{
		resolve: func(_ context.Context, address string, _ bool) (Recipient, error) {
			local, domain, _ := SplitAddress(address)
			return Recipient{Address: local + "@" + domain, Kind: RecipientMailbox, AccountID: "a1"}, nil
		},
	}
	client := startSMTPSession(t, mailer, SMTPConfig{Hostname: "mx.test"})
	client.do("EHLO client.test", 250)
	client.do("MAIL FROM:<sender@elsewhere.test>", 250)
	client.do("RCPT TO:<ada@example.test>", 250)
	client.do("RCPT TO:<ADA@Example.test>", 250)
	client.sendMessage("Subject: hello\r\n\r\nbody\r\n")
	client.expect(250)

	envelope := mailer.envelope()
	if envelope == nil || len(envelope.Recipients) != 1 {
		t.Fatalf("the envelope carries %v, want one recipient", envelope)
	}
	// With more than one recipient the trace header must not name any of them.
	if !strings.Contains(envelope.Received, "for <ada@example.test>") {
		t.Errorf("a single recipient should appear in the trace header:\n%s", envelope.Received)
	}
}

func TestSMTPTraceHeaderOmitsRecipientsWhenThereAreSeveral(t *testing.T) {
	mailer := &fakeMailer{
		resolve: func(_ context.Context, address string, _ bool) (Recipient, error) {
			return Recipient{Address: address, Kind: RecipientMailbox, AccountID: "a1"}, nil
		},
	}
	client := startSMTPSession(t, mailer, SMTPConfig{Hostname: "mx.test"})
	client.do("EHLO client.test", 250)
	client.do("MAIL FROM:<sender@elsewhere.test>", 250)
	client.do("RCPT TO:<one@example.test>", 250)
	client.do("RCPT TO:<two@example.test>", 250)
	client.sendMessage("Subject: hello\r\n\r\nbody\r\n")
	client.expect(250)

	envelope := mailer.envelope()
	if envelope == nil {
		t.Fatal("nothing was delivered")
	}
	if strings.Contains(envelope.Received, "for <") {
		t.Errorf("the trace header tells each recipient about the others:\n%s", envelope.Received)
	}
}

func TestSMTPDataTransparency(t *testing.T) {
	mailer := &fakeMailer{}
	client := startSMTPSession(t, mailer, SMTPConfig{Hostname: "mx.test"})
	client.do("EHLO client.test", 250)
	client.do("MAIL FROM:<sender@elsewhere.test>", 250)
	client.do("RCPT TO:<ada@example.test>", 250)
	// A body line that begins with a dot arrives doubled and must be stored
	// singly; a bare LF is normalised to CRLF.
	client.sendMessage("Subject: dots\r\n\r\n..hidden\r\n...more\r\nbare\nend\r\n")
	client.expect(250)

	envelope := mailer.envelope()
	if envelope == nil {
		t.Fatal("nothing was delivered")
	}
	want := "Subject: dots\r\n\r\n.hidden\r\n..more\r\nbare\r\nend\r\n"
	if string(envelope.Data) != want {
		t.Fatalf("data =\n%q\nwant\n%q", envelope.Data, want)
	}
}

func TestSMTPHeaderInjectionIsNeutralised(t *testing.T) {
	mailer := &fakeMailer{}
	client := startSMTPSession(t, mailer, SMTPConfig{Hostname: "mx.test"})
	// A HELO name carrying control characters must not be able to forge a
	// header line inside the trace header it is quoted into. A bare CR or LF
	// cannot reach here — it would end the command line — so the vector is the
	// control characters that can.
	client.send("EHLO evil\x00.test\x0bX-Injected:\x0cyes")
	client.expect(250)
	client.do("MAIL FROM:<sender@elsewhere.test>", 250)
	client.do("RCPT TO:<ada@example.test>", 250)
	client.sendMessage("Subject: hello\r\n\r\nbody\r\n")
	client.expect(250)

	envelope := mailer.envelope()
	if envelope == nil {
		t.Fatal("nothing was delivered")
	}
	for _, forbidden := range []string{"\x00", "\x0b", "\x0c"} {
		if strings.Contains(envelope.Received, forbidden) {
			t.Errorf("a control character survived into the trace header: %q", envelope.Received)
		}
	}
	if strings.Count(envelope.Received, "\r\n") != strings.Count(envelope.Received, "\n") {
		t.Errorf("the trace header contains a bare newline: %q", envelope.Received)
	}
}

// TestSMTPRunawayMessageIsDropped covers the client that never sends the final
// dot: the server reads a bounded amount past the limit and then gives up,
// rather than reading until it runs out of memory.
func TestSMTPRunawayMessageIsDropped(t *testing.T) {
	mailer := &fakeMailer{}
	client := startSMTPSession(t, mailer, SMTPConfig{Hostname: "mx.test", MaxMessageBytes: 1024})
	client.do("EHLO client.test", 250)
	client.do("MAIL FROM:<sender@elsewhere.test>", 250)
	client.do("RCPT TO:<ada@example.test>", 250)
	client.do("DATA", 354)

	// Written from another goroutine because the server stops reading as soon as
	// it has had enough, which is the behaviour under test.
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		chunk := []byte(strings.Repeat("x", 1023) + "\r\n")
		for range 4096 {
			if err := client.conn.SetWriteDeadline(time.Now().Add(testIOTimeout)); err != nil {
				return
			}
			if _, err := client.conn.Write(chunk); err != nil {
				return
			}
		}
	}()
	client.expect(552)
	<-stopped
	if _, delivered := mailer.counts(); delivered != 0 {
		t.Errorf("a runaway message was delivered %d times", delivered)
	}
}

// TestSMTPServeOnALoopbackListener exercises the accept loop and Close, which
// net.Pipe cannot reach.
func TestSMTPServeOnALoopbackListener(t *testing.T) {
	deliverer, _, ada := newHostedFixture(t)
	server, err := NewSMTPServer(deliverer, SMTPConfig{Hostname: "mx.test", Logger: discardLogger()})
	if err != nil {
		t.Fatalf("build the server: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()

	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	client := &smtpTestClient{t: t, conn: conn, reader: bufio.NewReader(conn)}
	client.expect(220)
	client.do("EHLO client.test", 250)
	client.do("MAIL FROM:<sender@elsewhere.test>", 250)
	client.do("RCPT TO:<ada@example.test>", 250)
	client.sendMessage("Subject: hello\r\n\r\nbody\r\n")
	client.expect(250)
	client.do("QUIT", 221)
	if err := conn.Close(); err != nil {
		t.Errorf("close the connection: %v", err)
	}

	if err := server.Close(); err != nil {
		t.Fatalf("close the server: %v", err)
	}
	if err := <-served; err != nil {
		t.Fatalf("serve: %v", err)
	}
	if messages := mailboxMessages(t, deliverer, ada.ID); len(messages) != 1 {
		t.Fatalf("delivered %d messages, want 1", len(messages))
	}
	// Serving again after Close is refused rather than silently accepted.
	if err := server.Serve(listener); !errors.Is(err, ErrSMTPServerClosed) {
		t.Errorf("serve after close: %v, want ErrSMTPServerClosed", err)
	}
	// Close is idempotent.
	if err := server.Close(); err != nil {
		t.Errorf("close twice: %v", err)
	}
}

func TestNewSMTPServerRefusesNoMailer(t *testing.T) {
	if _, err := NewSMTPServer(nil, SMTPConfig{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("got %v, want ErrInvalid", err)
	}
	server, err := NewSMTPServer(&fakeMailer{}, SMTPConfig{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if err := server.ServeTLS(nil); !errors.Is(err, ErrInvalid) {
		t.Errorf("implicit TLS without a certificate: %v, want ErrInvalid", err)
	}
	if err := server.Close(); err != nil {
		t.Errorf("close: %v", err)
	}
}

// -----------------------------------------------------------------------------
// Parsing
// -----------------------------------------------------------------------------

func TestParsePathCommand(t *testing.T) {
	tests := []struct {
		name       string
		rest       string
		keyword    string
		wantOK     bool
		wantPath   string
		wantParams []string
	}{
		{name: "a plain sender", rest: "FROM:<ada@example.test>", keyword: "FROM", wantOK: true, wantPath: "ada@example.test"},
		{name: "case does not matter", rest: "from:<ada@example.test>", keyword: "FROM", wantOK: true, wantPath: "ada@example.test"},
		{name: "a space after the colon", rest: "TO: <ada@example.test>", keyword: "TO", wantOK: true, wantPath: "ada@example.test"},
		{
			name: "parameters", rest: "FROM:<ada@example.test> SIZE=100 BODY=8BITMIME", keyword: "FROM",
			wantOK: true, wantPath: "ada@example.test", wantParams: []string{"SIZE=100", "BODY=8BITMIME"},
		},
		{name: "the null sender", rest: "FROM:<>", keyword: "FROM", wantOK: true},
		{
			name: "a source route is ignored", rest: "TO:<@relay.test:ada@example.test>", keyword: "TO",
			wantOK: true, wantPath: "ada@example.test",
		},
		{name: "the wrong keyword", rest: "TO:<a@b.test>", keyword: "FROM"},
		{name: "no colon", rest: "FROM <a@b.test>", keyword: "FROM"},
		{name: "no brackets", rest: "FROM:a@b.test", keyword: "FROM"},
		{name: "an unterminated path", rest: "FROM:<a@b.test", keyword: "FROM"},
		{name: "nothing at all", rest: "", keyword: "FROM"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path, params, ok := parsePathCommand(test.rest, test.keyword)
			if ok != test.wantOK {
				t.Fatalf("ok = %v, want %v", ok, test.wantOK)
			}
			if !ok {
				return
			}
			if path != test.wantPath {
				t.Errorf("path = %q, want %q", path, test.wantPath)
			}
			if strings.Join(params, ",") != strings.Join(test.wantParams, ",") {
				t.Errorf("params = %v, want %v", params, test.wantParams)
			}
		})
	}
}

func TestPlausibleAddress(t *testing.T) {
	tests := []struct {
		address string
		want    bool
	}{
		{address: "ada@example.test", want: true},
		{address: "ada+news@example.test", want: true},
		{address: "a@b", want: true},
		{address: "", want: false},
		{address: "ada", want: false},
		{address: "@example.test", want: false},
		{address: "ada@", want: false},
		{address: "ada@a@b.test", want: false},
		{address: "ada example@test", want: false},
		{address: "ada@example.test>", want: false},
		{address: "ada@.example.test", want: false},
		{address: "ada@example..test", want: false},
		{address: "ada@example.test.", want: false},
		{address: "ada\r\nbcc@example.test", want: false},
		{address: strings.Repeat("a", 65) + "@example.test", want: false},
	}
	for _, test := range tests {
		t.Run(test.address, func(t *testing.T) {
			if got := plausibleAddress(test.address); got != test.want {
				t.Fatalf("plausibleAddress(%q) = %v, want %v", test.address, got, test.want)
			}
		})
	}
}

func TestSanitizeReplyAndHeaderText(t *testing.T) {
	tests := []struct {
		name       string
		in         string
		wantReply  string
		wantHeader string
	}{
		{name: "plain text", in: "OK", wantReply: "OK", wantHeader: "OK"},
		{
			name: "an injected reply line", in: "OK\r\n250 owned",
			wantReply: "OK  250 owned", wantHeader: "OK250 owned",
		},
		{name: "a NUL", in: "a\x00b", wantReply: "a b", wantHeader: "ab"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := sanitizeReplyText(test.in); got != test.wantReply {
				t.Errorf("sanitizeReplyText(%q) = %q, want %q", test.in, got, test.wantReply)
			}
			if got := sanitizeHeaderValue(test.in); got != test.wantHeader {
				t.Errorf("sanitizeHeaderValue(%q) = %q, want %q", test.in, got, test.wantHeader)
			}
		})
	}
	if got := sanitizeReplyText(strings.Repeat("x", smtpMaxReplyText+50)); len(got) != smtpMaxReplyText {
		t.Errorf("a long reply was not truncated: %d bytes", len(got))
	}
}

func TestReadLimitedLine(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		max     int
		want    string
		wantErr error
	}{
		{name: "a short line", input: "EHLO x\r\nrest", max: 32, want: "EHLO x\r\n"},
		{name: "a bare LF", input: "EHLO x\n", max: 32, want: "EHLO x\n"},
		{name: "exactly the limit", input: "abcd\n", max: 5, want: "abcd\n"},
		{name: "over the limit", input: strings.Repeat("a", 64) + "\n", max: 16, wantErr: errLineTooLong},
		{
			name:  "over the limit across several buffer fills",
			input: strings.Repeat("a", 40000) + "\n", max: 4096, wantErr: errLineTooLong,
		},
		{name: "no newline at all", input: "EHLO x", max: 32, wantErr: io.EOF},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reader := bufio.NewReaderSize(strings.NewReader(test.input), 64)
			line, err := readLimitedLine(reader, test.max)
			if test.wantErr != nil {
				if !errors.Is(err, test.wantErr) {
					t.Fatalf("err = %v, want %v", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if string(line) != test.want {
				t.Fatalf("line = %q, want %q", line, test.want)
			}
		})
	}
}

// readSpooled reads the body a queued message points at.
func readSpooled(deliverer *Deliverer, relative string) (string, error) {
	raw, err := os.ReadFile(deliverer.SpoolPath(relative))
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// -----------------------------------------------------------------------------
// Admission control
//
// Everything below is about what an unauthenticated stranger can make this
// process spend: session slots, memory, CPU, and outbound deliveries. Each test
// drives the real limiter through the real session, because the limiter having
// the right behaviour is worth nothing if the session forgets to ask it.
// -----------------------------------------------------------------------------

// newSMTPTestServer builds a server the caller can open several sessions on,
// which is what a concurrency limit needs and startSMTPSession cannot give.
func newSMTPTestServer(t *testing.T, mailer Mailer, config SMTPConfig) *SMTPServer {
	t.Helper()
	if config.Logger == nil {
		config.Logger = discardLogger()
	}
	server, err := NewSMTPServer(mailer, config)
	if err != nil {
		t.Fatalf("build the SMTP server: %v", err)
	}
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Errorf("close the server: %v", err)
		}
	})
	return server
}

// dialSMTP opens one more session on an existing server. It does not read the
// greeting, because a refused connection never sends one.
func dialSMTP(t *testing.T, server *SMTPServer) *smtpTestClient {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		server.ServeConn(serverConn)
	}()
	t.Cleanup(func() {
		if err := clientConn.Close(); err != nil && !errors.Is(err, io.ErrClosedPipe) {
			t.Errorf("close the client: %v", err)
		}
		select {
		case <-finished:
		case <-time.After(testIOTimeout):
			t.Error("the session did not finish after the client went away")
		}
	})
	return &smtpTestClient{t: t, conn: clientConn, reader: bufio.NewReader(clientConn)}
}

// expectClosed waits for the server to hang up.
func (c *smtpTestClient) expectClosed() {
	c.t.Helper()
	// Deliberately NOT c.deadline(): the server may already have closed the
	// pipe, in which case SetDeadline fails with "io: read/write on closed
	// pipe". That is the very condition this assertion exists to observe, so
	// treating it as a fatal setup error made the test fail intermittently
	// whenever the server won the race — which is most of the time.
	if err := c.conn.SetDeadline(time.Now().Add(testIOTimeout)); err != nil {
		return
	}
	if _, err := c.reader.ReadByte(); err == nil {
		c.t.Fatal("the server did not close the connection")
	}
}

// TestSMTPRefusesSessionsOverTheConnectionLimits covers both halves of the
// session bound. The per-address share is the one that matters: without it a
// single peer takes every slot for the price of one socket each, and the
// server is down for everybody while looking perfectly healthy.
func TestSMTPRefusesSessionsOverTheConnectionLimits(t *testing.T) {
	tests := []struct {
		name    string
		config  LimiterConfig
		wantHas string
	}{
		{
			name:    "the global limit",
			config:  LimiterConfig{MaxSessions: 1, MaxSessionsPerIP: 1},
			wantHas: "Too many connections",
		},
		{
			name: "one peer's share of it",
			// Room in the server, none for this peer: every pipe reports the
			// same remote address, so both sessions are one peer.
			config:  LimiterConfig{MaxSessions: 8, MaxSessionsPerIP: 1},
			wantHas: "from your address",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := newSMTPTestServer(t, &fakeMailer{}, SMTPConfig{
				Hostname: "mx.test",
				Limiter:  NewLimiter(test.config),
			})

			first := dialSMTP(t, server)
			first.expect(220)

			second := dialSMTP(t, server)
			code, text := second.readReply()
			if code != 421 {
				t.Fatalf("the session over the limit got %d, want 421:\n%s", code, text)
			}
			if !strings.Contains(text, test.wantHas) {
				t.Errorf("the refusal does not say why: %q", text)
			}

			// The session already in progress is untouched. A limiter that
			// refused the wrong connection would be worse than none.
			first.do("NOOP", 250)
		})
	}
}

// TestSMTPReleasesASessionSlotWhenTheSessionEnds is the other half of the
// bound: a slot that is not returned is a slot lost for the life of the
// process, and the server would refuse everything after MaxSessions
// connections had merely happened.
func TestSMTPReleasesASessionSlotWhenTheSessionEnds(t *testing.T) {
	limiter := NewLimiter(LimiterConfig{MaxSessions: 1, MaxSessionsPerIP: 1})
	server := newSMTPTestServer(t, &fakeMailer{}, SMTPConfig{Hostname: "mx.test", Limiter: limiter})

	for attempt := range 3 {
		client := dialSMTP(t, server)
		client.expect(220)
		client.do("QUIT", 221)
		// The server closes the connection at the end of the session, and
		// ServeConn's deferred Release runs before that Close, so a client that
		// has seen the pipe go is looking at a slot that is already back.
		client.expectClosed()
		if held := limiter.Stats().Sessions; held != 0 {
			t.Fatalf("attempt %d left %d sessions held", attempt, held)
		}
	}
}

// TestSMTPLocksOutRepeatedAuthenticationFailures pins the order the whole
// defence rests on: the lockout is consulted BEFORE the credential is
// verified. Mailer.Authenticate is PBKDF2 at the production work factor and
// pays the same cost for a mailbox that does not exist, so if the refusal came
// after it, a peer guessing in a loop would still buy tens of milliseconds of
// CPU per guess and the lockout would be the attack rather than the defence.
func TestSMTPLocksOutRepeatedAuthenticationFailures(t *testing.T) {
	clock := newTestLimiterClock()
	limiter := NewLimiter(LimiterConfig{
		AuthFailures:   2,
		AuthLockout:    time.Minute,
		AuthLockoutMax: time.Minute,
		Now:            clock.Now,
	})
	mailer := &fakeMailer{}
	client := startSMTPSession(t, mailer, SMTPConfig{
		Hostname:  "mx.test",
		TLSConfig: testTLSConfig(t),
		Limiter:   limiter,
	})
	client.do("EHLO client.test", 250)
	client.startTLS()
	client.do("EHLO client.test", 250)

	credential := func(password string) string {
		return "AUTH PLAIN " + plainCredential("ada@example.test", password)
	}
	client.do(credential("wrong one"), 535)
	client.do(credential("wrong two"), 535)

	reply := client.do(credential("wrong three"), 454)
	if strings.Contains(reply, "wrong three") {
		t.Fatalf("the refusal quotes the credential: %q", reply)
	}
	// It must not say which key is locked. "This account is locked" tells an
	// attacker the account exists; "your address is locked" tells it which of
	// its hosts is burnt.
	if strings.Contains(strings.ToLower(reply), "ada@example.test") {
		t.Errorf("the refusal names the account: %q", reply)
	}
	if attempts, _ := mailer.counts(); attempts != 2 {
		t.Fatalf("the server verified %d credentials, want 2: the third was not "+
			"refused before the hash", attempts)
	}

	// Nothing is locked forever: an account lockout is also a way to deny a
	// customer their own mail.
	clock.Advance(time.Minute)
	client.do(credential("wrong four"), 535)
	if attempts, _ := mailer.counts(); attempts != 3 {
		t.Fatalf("the server verified %d credentials after the lockout expired, want 3", attempts)
	}
}

// TestSMTPAuthenticationSuccessClearsTheFailureCount: a customer whose client
// retried a stale password before being corrected must not be one failure away
// from a lockout for the next quarter of an hour.
func TestSMTPAuthenticationSuccessClearsTheFailureCount(t *testing.T) {
	limiter := NewLimiter(LimiterConfig{AuthFailures: 2, AuthLockout: time.Minute})
	accepted := false
	mailer := &fakeMailer{
		authenticate: func(_ context.Context, _, password string) (Identity, error) {
			if password == "the right passphrase" {
				accepted = true
				return Identity{AccountID: "a1", Addresses: []string{"ada@example.test"}}, nil
			}
			return Identity{}, errAuthFailed
		},
	}
	client := startSMTPSession(t, mailer, SMTPConfig{
		Hostname:  "mx.test",
		TLSConfig: testTLSConfig(t),
		Limiter:   limiter,
	})
	client.do("EHLO client.test", 250)
	client.startTLS()
	client.do("EHLO client.test", 250)

	client.do("AUTH PLAIN "+plainCredential("ada@example.test", "stale"), 535)
	client.do("AUTH PLAIN "+plainCredential("ada@example.test", "the right passphrase"), 235)
	if !accepted {
		t.Fatal("the good credential was never verified")
	}
	if keys := limiter.Stats().TrackedAuthKeys; keys != 0 {
		t.Fatalf("a successful login left %d failure counters standing", keys)
	}
}

// TestSMTPBoundsConcurrentMessageMemory: DATA is accumulated in memory, so
// without a joint bound the memory this process can be made to hold is the
// message limit times the number of sessions in DATA at once — an OOM an
// attacker schedules. The reservation is taken before the 354 and returned
// after the body is handed on, which is what this drives end to end.
func TestSMTPBoundsConcurrentMessageMemory(t *testing.T) {
	limiter := NewLimiter(LimiterConfig{
		MaxMessageBytes:  1 << 20,
		MaxMessageMemory: 1 << 20, // room for exactly one body
	})
	if got := limiter.MaxConcurrentMessages(); got != 1 {
		t.Fatalf("MaxConcurrentMessages = %d, want 1", got)
	}
	server := newSMTPTestServer(t, &fakeMailer{}, SMTPConfig{Hostname: "mx.test", Limiter: limiter})

	open := func() *smtpTestClient {
		client := dialSMTP(t, server)
		client.expect(220)
		client.do("EHLO client.test", 250)
		client.do("MAIL FROM:<sender@elsewhere.test>", 250)
		client.do("RCPT TO:<ada@example.test>", 250)
		return client
	}
	first, second := open(), open()

	// The first session holds the only body's worth of memory.
	first.do("DATA", 354)
	reply := second.do("DATA", 452)
	if !strings.Contains(reply, "4.3.1") {
		t.Errorf("a memory refusal must be 4.3.1 insufficient system storage, got %q", reply)
	}
	if held := limiter.Stats().MessageBytesHeld; held != 1<<20 {
		t.Fatalf("held %d bytes, want the one reservation", held)
	}

	// Finishing the first message returns the reservation, and the second
	// session — still in the same transaction — can now send.
	first.deadline()
	if _, err := first.conn.Write([]byte("Subject: one\r\n\r\nbody\r\n")); err != nil {
		t.Fatalf("write the message: %v", err)
	}
	first.send(".")
	first.expect(250)
	// A round trip after the reply, so the reservation's release — which
	// happens as the DATA handler returns — has certainly run before it is
	// measured.
	first.do("NOOP", 250)
	if held := limiter.Stats().MessageBytesHeld; held != 0 {
		t.Fatalf("a finished message still holds %d bytes", held)
	}
	second.do("DATA", 354)
}

// TestSMTPMessageSizeTakesTheSmallerOfTheTwoLimits: SMTPConfig and
// LimiterConfig each carry a message size, so they can disagree. The smaller is
// what actually happens, so the larger must not be what the server announces —
// an EHLO advertising a size the server will not accept sends every client that
// believes it through a full upload and a 552.
func TestSMTPMessageSizeTakesTheSmallerOfTheTwoLimits(t *testing.T) {
	client := startSMTPSession(t, &fakeMailer{}, SMTPConfig{
		Hostname:        "mx.test",
		MaxMessageBytes: 1 << 20,
		Limiter:         NewLimiter(LimiterConfig{MaxMessageBytes: 1024}),
	})
	greeting := client.do("EHLO client.test", 250)
	if !strings.Contains(greeting, "SIZE 1024") {
		t.Fatalf("the advertised size is not the real one:\n%s", greeting)
	}
	client.do("MAIL FROM:<sender@elsewhere.test> SIZE=2000", 552)
}

// TestSMTPBoundsTheFanOutAnUnauthenticatedMessageCanCause closes the
// amplification hole this server is exposed to as a public MX.
//
// A mailing-list recipient expands to up to maxFanOut deliveries, and RFC 5321
// makes this server accept 100 recipients. Neither limit sees the other, so
// their product — one inbound message becoming thousands of outbound ones this
// server pays to send — is what a stranger can buy. Bounding the fan-out
// recipients of an unauthenticated transaction is what removes the multiplier
// while leaving a correspondent writing to a customer's forwarder alone.
func TestSMTPBoundsTheFanOutAnUnauthenticatedMessageCanCause(t *testing.T) {
	mailer := &fakeMailer{
		authenticate: func(context.Context, string, string) (Identity, error) {
			return Identity{AccountID: "a1", Addresses: []string{"ada@example.test"}}, nil
		},
		resolve: func(_ context.Context, address string, _ bool) (Recipient, error) {
			return Recipient{Address: address, Kind: RecipientList, ListID: "l1"}, nil
		},
	}
	client := startSMTPSession(t, mailer, SMTPConfig{Hostname: "mx.test", TLSConfig: testTLSConfig(t)})
	client.do("EHLO client.test", 250)
	client.do("MAIL FROM:<sender@elsewhere.test>", 250)

	for index := range smtpMaxUnauthenticatedFanOut {
		client.do("RCPT TO:<list"+strconv.Itoa(index)+"@example.test>", 250)
	}
	// Naming one of them again is not a second fan-out.
	client.do("RCPT TO:<list0@example.test>", 250)

	reply := client.do("RCPT TO:<list99@example.test>", 452)
	if !strings.Contains(reply, "4.5.3") {
		t.Errorf("the refusal must be temporary so a real sender can split the message, got %q", reply)
	}

	// A submission client is bounded by owning a mailbox, not by this.
	client.do("RSET", 250)
	client.startTLS()
	client.do("EHLO client.test", 250)
	client.do("AUTH PLAIN "+plainCredential("ada@example.test", "a good passphrase"), 235)
	client.do("MAIL FROM:<ada@example.test>", 250)
	for index := range smtpMaxUnauthenticatedFanOut + 2 {
		client.do("RCPT TO:<list"+strconv.Itoa(index)+"@example.test>", 250)
	}
}

// TestSMTPTellsTheDeliveryPathTheTruthAboutAuthentication.
//
// Everything that stops this server relaying for a stranger hangs off one
// boolean. The session must pass the real state of the connection and must put
// no Identity on an envelope it did not authenticate, because a delivery path
// told "authenticated" about an inbound message would relay it, and a mail
// server that relays for strangers is an open relay whatever else it does.
func TestSMTPTellsTheDeliveryPathTheTruthAboutAuthentication(t *testing.T) {
	var mu sync.Mutex
	var asked []bool
	mailer := &fakeMailer{
		resolve: func(_ context.Context, address string, authenticated bool) (Recipient, error) {
			mu.Lock()
			asked = append(asked, authenticated)
			mu.Unlock()
			if !authenticated && !strings.HasSuffix(address, "@example.test") {
				return Recipient{}, errRelayDenied
			}
			return Recipient{Address: address, Kind: RecipientMailbox, AccountID: "a1"}, nil
		},
	}
	client := startSMTPSession(t, mailer, SMTPConfig{Hostname: "mx.test"})
	client.do("EHLO client.test", 250)
	client.do("MAIL FROM:<sender@elsewhere.test>", 250)
	client.do("RCPT TO:<friend@elsewhere.test>", 550)
	client.do("RCPT TO:<ada@example.test>", 250)
	client.sendMessage("Subject: in\r\n\r\nbody\r\n")
	client.expect(250)

	mu.Lock()
	defer mu.Unlock()
	if len(asked) != 2 {
		t.Fatalf("the delivery path was asked about %d recipients, want 2", len(asked))
	}
	for index, authenticated := range asked {
		if authenticated {
			t.Fatalf("recipient %d was resolved as though the session had authenticated", index)
		}
	}
	if envelope := mailer.envelope(); envelope == nil || envelope.Identity != nil {
		t.Fatal("an unauthenticated message carried a submission identity")
	}
}

// TestSMTPRefusesAddressesTheStrictParserRejects: address.go is the one parser
// every entry point into this package shares, and it is stricter than the gate
// the session used to carry. The cases below are the difference, and each is a
// shape that reaches a header, a log line or a queue key if it is let through.
func TestSMTPRefusesAddressesTheStrictParserRejects(t *testing.T) {
	long := strings.Repeat("a", 250) + "@example.test"
	tests := []struct {
		name     string
		command  string
		isSender bool
	}{
		{name: "an address literal", command: "RCPT TO:<ada@[192.0.2.1]>"},
		{name: "a trailing tab inside the path", command: "RCPT TO:<ada@example.test\t>"},
		{name: "a trailing carriage return inside the path", command: "RCPT TO:<ada@example.test\r>"},
		{name: "an address over 254 octets", command: "RCPT TO:<" + long + ">"},
		{name: "a quoted local part", command: `RCPT TO:<"ada bell"@example.test>`},
		{name: "a sender that is not an address", command: "MAIL FROM:<nobody>", isSender: true},
		{name: "a sender with a control character", command: "MAIL FROM:<ada\x01@example.test>", isSender: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := startSMTPSession(t, &fakeMailer{}, SMTPConfig{Hostname: "mx.test"})
			client.do("EHLO client.test", 250)
			if !test.isSender {
				client.do("MAIL FROM:<sender@elsewhere.test>", 250)
			}
			reply := client.do(test.command, 501)
			// The reply must not quote what was sent: it is the untrusted
			// string, and a reply line is somewhere it wants to reach.
			if strings.Contains(reply, "192.0.2.1") || strings.Contains(reply, "nobody") {
				t.Errorf("the refusal quotes the address: %q", reply)
			}
		})
	}
}

// TestParsePathCommandKeepsWhatTheAddressParserMustSee: trimming with
// strings.TrimSpace here would eat a trailing CR or tab, and the address parser
// would then accept the very injection it exists to refuse.
func TestParsePathCommandKeepsWhatTheAddressParserMustSee(t *testing.T) {
	tests := []struct {
		name string
		rest string
		want string
	}{
		{name: "spaces are stripped", rest: "FROM:< ada@example.test >", want: "ada@example.test"},
		{name: "a trailing carriage return survives", rest: "FROM:<ada@example.test\r>", want: "ada@example.test\r"},
		{name: "a trailing tab survives", rest: "FROM:<ada@example.test\t>", want: "ada@example.test\t"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path, _, ok := parsePathCommand(test.rest, "FROM")
			if !ok {
				t.Fatal("the command did not parse")
			}
			if path != test.want {
				t.Fatalf("path = %q, want %q", path, test.want)
			}
			if ValidAddress(path) != (path == "ada@example.test") {
				t.Fatalf("ValidAddress(%q) did not refuse the injected character", path)
			}
		})
	}
}

// TestSMTPCleartextAuthDoesNotSpendTheFailureBudget: the cleartext refusal is
// made before the mechanism name is parsed, and therefore before the limiter is
// touched. That ordering matters twice over — no credential is read off an
// unencrypted connection, and a peer that cannot do TLS cannot lock a customer
// out of their own mailbox by shouting AUTH at port 25.
func TestSMTPCleartextAuthDoesNotSpendTheFailureBudget(t *testing.T) {
	limiter := NewLimiter(LimiterConfig{AuthFailures: 2, AuthLockout: time.Minute})
	mailer := &fakeMailer{}
	client := startSMTPSession(t, mailer, SMTPConfig{
		Hostname:  "mx.test",
		TLSConfig: testTLSConfig(t),
		Limiter:   limiter,
	})
	client.do("EHLO client.test", 250)
	for range 3 {
		client.do("AUTH PLAIN "+plainCredential("ada@example.test", "a guess"), 538)
	}
	if attempts, _ := mailer.counts(); attempts != 0 {
		t.Fatalf("the server verified %d credentials over cleartext, want 0", attempts)
	}
	if keys := limiter.Stats().TrackedAuthKeys; keys != 0 {
		t.Fatalf("a cleartext peer spent %d of the account's failure budget", keys)
	}
}
