package maild

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/netip"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// Every test in this file and in queue_test.go delivers to a real SMTP server
// on a loopback listener and resolves every domain through an injected
// Resolver, so nothing here reaches the network. The one exception is
// newFakeResolverServer, which runs a real miekg/dns server on 127.0.0.1 in
// order to exercise the DNSResolver itself.

// -----------------------------------------------------------------------------
// A receiving SMTP server
// -----------------------------------------------------------------------------

// fakeDelivery is one message a fakeMX accepted.
type fakeDelivery struct {
	From string
	To   []string
	Body string
	TLS  bool
}

// fakeMX is a receiving SMTP server. It speaks just enough of RFC 5321 for a
// real client: a greeting, EHLO with an optional STARTTLS offer, MAIL, RCPT,
// DATA and QUIT.
//
// Every reply is scriptable per command, and a script is consumed one entry per
// use with the last entry repeating, which is how "fail once then succeed" is
// expressed without a counter in the test.
type fakeMX struct {
	listener  net.Listener
	tlsConfig *tls.Config

	mu         sync.Mutex
	scripts    map[string][]string
	deliveries []fakeDelivery
	sessions   int
	done       sync.WaitGroup
}

// newFakeMX starts a receiving server on a loopback port. A non-nil tlsConfig
// makes it offer STARTTLS.
func newFakeMX(t *testing.T, tlsConfig *tls.Config) *fakeMX {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := &fakeMX{
		listener:  listener,
		tlsConfig: tlsConfig,
		scripts:   make(map[string][]string),
	}
	server.done.Add(1)
	go server.accept()
	t.Cleanup(func() {
		if err := listener.Close(); err != nil {
			t.Errorf("close the fake mail host: %v", err)
		}
		server.done.Wait()
	})
	return server
}

// address is the host:port the server is listening on.
func (f *fakeMX) address() string { return f.listener.Addr().String() }

// script sets the replies for one command. "EHLO", "STARTTLS", "MAIL", "RCPT",
// "DATA" and "DOT" — the reply after the final dot — are recognised.
func (f *fakeMX) script(command string, replies ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scripts[command] = replies
}

func (f *fakeMX) next(command, fallback string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	replies := f.scripts[command]
	switch len(replies) {
	case 0:
		return fallback
	case 1:
		return replies[0]
	default:
		f.scripts[command] = replies[1:]
		return replies[0]
	}
}

func (f *fakeMX) received() []fakeDelivery {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeDelivery(nil), f.deliveries...)
}

func (f *fakeMX) sessionCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sessions
}

func (f *fakeMX) enter() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions++
}

func (f *fakeMX) record(delivery fakeDelivery) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deliveries = append(f.deliveries, delivery)
}

func (f *fakeMX) accept() {
	defer f.done.Done()
	for {
		conn, err := f.listener.Accept()
		if err != nil {
			return
		}
		f.done.Add(1)
		go func() {
			defer f.done.Done()
			f.serve(conn)
		}()
	}
}

func (f *fakeMX) serve(raw net.Conn) {
	defer func() { discardClose(raw) }()
	f.enter()
	if err := raw.SetDeadline(time.Now().Add(20 * time.Second)); err != nil {
		return
	}

	session := raw
	reader := bufio.NewReader(session)
	secure := false
	write := func(line string) bool {
		_, err := session.Write([]byte(line + "\r\n"))
		return err == nil
	}
	if !write("220 mx.test ESMTP fake") {
		return
	}

	var from string
	var to []string
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		command, argument, _ := strings.Cut(strings.TrimRight(line, "\r\n"), " ")
		switch strings.ToUpper(command) {
		case "EHLO":
			if f.tlsConfig != nil && !secure {
				if !write("250-mx.test") || !write(f.next("EHLO", "250 STARTTLS")) {
					return
				}
				continue
			}
			if !write("250 mx.test") {
				return
			}
		case "HELO":
			if !write("250 mx.test") {
				return
			}
		case "STARTTLS":
			reply := f.next("STARTTLS", "220 2.0.0 Ready to start TLS")
			if !write(reply) {
				return
			}
			if !strings.HasPrefix(reply, "220") {
				continue
			}
			encrypted := tls.Server(session, f.tlsConfig)
			if err := encrypted.Handshake(); err != nil {
				return
			}
			session, reader, secure = encrypted, bufio.NewReader(encrypted), true
		case "MAIL":
			reply := f.next("MAIL", "250 2.1.0 Ok")
			if strings.HasPrefix(reply, "250") {
				from, to = pathArgument(argument), nil
			}
			if !write(reply) {
				return
			}
		case "RCPT":
			reply := f.next("RCPT", "250 2.1.5 Ok")
			if strings.HasPrefix(reply, "250") {
				to = append(to, pathArgument(argument))
			}
			if !write(reply) {
				return
			}
		case "DATA":
			reply := f.next("DATA", "354 End data with <CR><LF>.<CR><LF>")
			if !write(reply) {
				return
			}
			if !strings.HasPrefix(reply, "354") {
				continue
			}
			body, err := readDotStuffed(reader)
			if err != nil {
				return
			}
			final := f.next("DOT", "250 2.0.0 Ok: queued")
			if strings.HasPrefix(final, "250") {
				f.record(fakeDelivery{From: from, To: to, Body: body, TLS: secure})
			}
			if !write(final) {
				return
			}
		case "RSET":
			from, to = "", nil
			if !write("250 2.0.0 Ok") {
				return
			}
		case "NOOP":
			if !write("250 2.0.0 Ok") {
				return
			}
		case "QUIT":
			write("221 2.0.0 Bye")
			return
		default:
			if !write("500 5.5.2 Unrecognised command") {
				return
			}
		}
	}
}

// discardClose closes a connection whose close error nobody can act on: the
// peer has usually closed first, and the test's assertions are about the
// conversation, not about the socket.
func discardClose(closer io.Closer) {
	if err := closer.Close(); err != nil {
		return
	}
}

// pathArgument pulls the address out of "FROM:<a@b>" or "TO:<a@b> PARAM=x".
func pathArgument(argument string) string {
	_, rest, found := strings.Cut(argument, ":")
	if !found {
		rest = argument
	}
	rest = strings.TrimSpace(rest)
	if open := strings.Index(rest, "<"); open >= 0 {
		if end := strings.Index(rest[open:], ">"); end >= 0 {
			return rest[open+1 : open+end]
		}
	}
	field, _, _ := strings.Cut(rest, " ")
	return field
}

// readDotStuffed reads a DATA body to its terminating dot and undoes the
// stuffing, so a test compares the bytes the sender meant to send.
func readDotStuffed(reader *bufio.Reader) (string, error) {
	var body strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return "", err
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed == "." {
			return body.String(), nil
		}
		if strings.HasPrefix(trimmed, "..") {
			trimmed = trimmed[1:]
		}
		body.WriteString(trimmed)
		body.WriteString("\r\n")
	}
}

// -----------------------------------------------------------------------------
// Injection helpers
// -----------------------------------------------------------------------------

// routedDialer maps a host name onto a real loopback address. A route with an
// empty target refuses the connection, which is how a dead mail host is
// simulated without waiting for a timeout.
func routedDialer(routes map[string]string) func(ctx context.Context, network, address string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		target, known := routes[host]
		if !known {
			return nil, fmt.Errorf("no route to %q", host)
		}
		if target == "" {
			return nil, errors.New("connection refused")
		}
		var dialer net.Dialer
		return dialer.DialContext(ctx, network, target)
	}
}

// fixedResolver answers every domain with the same hosts.
func fixedResolver(hosts ...string) Resolver {
	return ResolverFunc(func(context.Context, string) ([]string, error) {
		return append([]string(nil), hosts...), nil
	})
}

// relayTLSConfig is a throwaway self-signed certificate. Ed25519 keeps the
// generation instant, so a TLS test costs nothing.
func relayTLSConfig(t *testing.T) *tls.Config {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate a key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(2),
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

// newTestRelay wires a relay onto the given fake hosts.
func newTestRelay(t *testing.T, config RelayConfig) *Relay {
	t.Helper()
	if config.Hostname == "" {
		config.Hostname = "sender.test"
	}
	if config.ConnectTimeout == 0 {
		config.ConnectTimeout = 5 * time.Second
	}
	if config.CommandTimeout == 0 {
		config.CommandTimeout = 5 * time.Second
	}
	if config.DataTimeout == 0 {
		config.DataTimeout = 5 * time.Second
	}
	relay, err := NewRelay(config)
	if err != nil {
		t.Fatalf("build the relay: %v", err)
	}
	return relay
}

// -----------------------------------------------------------------------------
// Delivery
// -----------------------------------------------------------------------------

func TestRelayDeliversAMessageToTheRecipientsMailHost(t *testing.T) {
	t.Parallel()
	host := newFakeMX(t, nil)
	relay := newTestRelay(t, RelayConfig{
		Resolver:    fixedResolver("mx.example.com"),
		DialContext: routedDialer(map[string]string{"mx.example.com": host.address()}),
	})

	body := []byte("Subject: hello\r\n\r\nbody\r\n")
	reports := relay.Send(context.Background(), "example.com", "sender@probe.test",
		[]string{"someone@example.com"}, body)

	if len(reports) != 1 {
		t.Fatalf("want one report, got %d", len(reports))
	}
	if reports[0].Err != nil {
		t.Fatalf("want a delivery, got %v", reports[0].Err)
	}
	delivered := host.received()
	if len(delivered) != 1 {
		t.Fatalf("want one delivered message, got %d", len(delivered))
	}
	if delivered[0].From != "sender@probe.test" {
		t.Errorf("envelope sender = %q", delivered[0].From)
	}
	if len(delivered[0].To) != 1 || delivered[0].To[0] != "someone@example.com" {
		t.Errorf("envelope recipients = %v", delivered[0].To)
	}
	if delivered[0].Body != string(body) {
		t.Errorf("body = %q, want %q", delivered[0].Body, body)
	}
}

// A dot on a line of its own is the one byte sequence that can end a message
// early. The client must stuff it; a receiver that sees it unstuffed has been
// handed a truncated message and a stream that is out of sync.
func TestRelayStuffsALeadingDot(t *testing.T) {
	t.Parallel()
	host := newFakeMX(t, nil)
	relay := newTestRelay(t, RelayConfig{
		Resolver:    fixedResolver("mx.example.com"),
		DialContext: routedDialer(map[string]string{"mx.example.com": host.address()}),
	})

	body := []byte("Subject: dots\r\n\r\n.\r\nafter the dot\r\n")
	reports := relay.Send(context.Background(), "example.com", "sender@probe.test",
		[]string{"someone@example.com"}, body)
	if reports[0].Err != nil {
		t.Fatalf("want a delivery, got %v", reports[0].Err)
	}
	delivered := host.received()
	if len(delivered) != 1 {
		t.Fatalf("want one delivered message, got %d", len(delivered))
	}
	if delivered[0].Body != string(body) {
		t.Errorf("body = %q, want %q", delivered[0].Body, body)
	}
}

func TestRelayTriesTheNextMailHostWhenTheFirstIsDead(t *testing.T) {
	t.Parallel()
	host := newFakeMX(t, nil)
	relay := newTestRelay(t, RelayConfig{
		Resolver: fixedResolver("dead.example.com", "mx.example.com"),
		DialContext: routedDialer(map[string]string{
			"dead.example.com": "",
			"mx.example.com":   host.address(),
		}),
	})

	reports := relay.Send(context.Background(), "example.com", "sender@probe.test",
		[]string{"someone@example.com"}, []byte("Subject: x\r\n\r\n"))
	if reports[0].Err != nil {
		t.Fatalf("want a delivery from the second host, got %v", reports[0].Err)
	}
	if got := len(host.received()); got != 1 {
		t.Fatalf("want one delivered message, got %d", got)
	}
}

// A 5xx before any recipient is named is about the message, not the host, so
// trying the next host would only collect the same refusal.
func TestRelayStopsAtAPermanentSessionRefusal(t *testing.T) {
	t.Parallel()
	first := newFakeMX(t, nil)
	first.script("MAIL", "550 5.7.1 We do not accept mail from you")
	second := newFakeMX(t, nil)
	relay := newTestRelay(t, RelayConfig{
		Resolver: fixedResolver("a.example.com", "b.example.com"),
		DialContext: routedDialer(map[string]string{
			"a.example.com": first.address(),
			"b.example.com": second.address(),
		}),
	})

	reports := relay.Send(context.Background(), "example.com", "sender@probe.test",
		[]string{"someone@example.com"}, []byte("Subject: x\r\n\r\n"))
	var failure *DeliveryError
	if !errors.As(reports[0].Err, &failure) {
		t.Fatalf("want a delivery error, got %v", reports[0].Err)
	}
	if failure.Temporary() {
		t.Errorf("a 550 must be permanent, got %v", failure)
	}
	if failure.Code != 550 {
		t.Errorf("code = %d, want 550", failure.Code)
	}
	if got := second.sessionCount(); got != 0 {
		t.Errorf("the second host was tried %d times, want 0", got)
	}
}

// The mirror of the test above: a 4xx makes the next host worth trying.
func TestRelayTriesTheNextMailHostAfterATemporarySessionRefusal(t *testing.T) {
	t.Parallel()
	first := newFakeMX(t, nil)
	first.script("MAIL", "451 4.3.0 Come back later")
	second := newFakeMX(t, nil)
	relay := newTestRelay(t, RelayConfig{
		Resolver: fixedResolver("a.example.com", "b.example.com"),
		DialContext: routedDialer(map[string]string{
			"a.example.com": first.address(),
			"b.example.com": second.address(),
		}),
	})

	reports := relay.Send(context.Background(), "example.com", "sender@probe.test",
		[]string{"someone@example.com"}, []byte("Subject: x\r\n\r\n"))
	if reports[0].Err != nil {
		t.Fatalf("want a delivery from the second host, got %v", reports[0].Err)
	}
	if got := len(second.received()); got != 1 {
		t.Errorf("the second host received %d messages, want 1", got)
	}
}

func TestRelayReportsEachRecipientSeparately(t *testing.T) {
	t.Parallel()
	host := newFakeMX(t, nil)
	// The first RCPT is refused permanently, the second accepted, and the
	// script's last entry repeats.
	host.script("RCPT", "550 5.1.1 No such user here", "250 2.1.5 Ok")
	relay := newTestRelay(t, RelayConfig{
		Resolver:    fixedResolver("mx.example.com"),
		DialContext: routedDialer(map[string]string{"mx.example.com": host.address()}),
	})

	reports := relay.Send(context.Background(), "example.com", "sender@probe.test",
		[]string{"gone@example.com", "present@example.com"}, []byte("Subject: x\r\n\r\n"))
	if len(reports) != 2 {
		t.Fatalf("want two reports, got %d", len(reports))
	}
	var failure *DeliveryError
	if !errors.As(reports[0].Err, &failure) {
		t.Fatalf("want a refusal for the first recipient, got %v", reports[0].Err)
	}
	if failure.Temporary() || failure.Code != 550 {
		t.Errorf("first recipient = %v, want a permanent 550", failure)
	}
	if failure.Enhanced != "5.1.1" {
		t.Errorf("enhanced status = %q, want the one the remote sent", failure.Enhanced)
	}
	if reports[1].Err != nil {
		t.Errorf("second recipient = %v, want a delivery", reports[1].Err)
	}
	delivered := host.received()
	if len(delivered) != 1 || len(delivered[0].To) != 1 || delivered[0].To[0] != "present@example.com" {
		t.Fatalf("delivered = %+v", delivered)
	}
}

// Every recipient refused means there is nothing to send, and a client that
// issued DATA anyway would be answered 503 and look like a broken sender.
func TestRelaySkipsTheBodyWhenEveryRecipientIsRefused(t *testing.T) {
	t.Parallel()
	host := newFakeMX(t, nil)
	host.script("RCPT", "550 5.1.1 No such user here")
	host.script("DATA", "503 5.5.1 Bad sequence of commands")
	relay := newTestRelay(t, RelayConfig{
		Resolver:    fixedResolver("mx.example.com"),
		DialContext: routedDialer(map[string]string{"mx.example.com": host.address()}),
	})

	reports := relay.Send(context.Background(), "example.com", "sender@probe.test",
		[]string{"gone@example.com"}, []byte("Subject: x\r\n\r\n"))
	var failure *DeliveryError
	if !errors.As(reports[0].Err, &failure) || failure.Code != 550 {
		t.Fatalf("want the RCPT refusal, got %v", reports[0].Err)
	}
	if got := len(host.received()); got != 0 {
		t.Errorf("the host received %d messages, want 0", got)
	}
}

// The reply to the final dot is the one that transfers responsibility. A body
// that was written but never acknowledged has not been delivered.
func TestRelayTreatsARefusalOfTheFinalDotAsAFailure(t *testing.T) {
	t.Parallel()
	host := newFakeMX(t, nil)
	host.script("DOT", "552 5.3.4 Message too big for system")
	relay := newTestRelay(t, RelayConfig{
		Resolver:    fixedResolver("mx.example.com"),
		DialContext: routedDialer(map[string]string{"mx.example.com": host.address()}),
	})

	reports := relay.Send(context.Background(), "example.com", "sender@probe.test",
		[]string{"someone@example.com"}, []byte("Subject: x\r\n\r\n"))
	var failure *DeliveryError
	if !errors.As(reports[0].Err, &failure) {
		t.Fatalf("want a failure, got %v", reports[0].Err)
	}
	if failure.Temporary() || failure.Code != 552 {
		t.Errorf("failure = %v, want a permanent 552", failure)
	}
}

// A connection that was refused is not a refusal of the message.
func TestRelayTreatsAConnectionFailureAsTemporary(t *testing.T) {
	t.Parallel()
	relay := newTestRelay(t, RelayConfig{
		Resolver:    fixedResolver("dead.example.com"),
		DialContext: routedDialer(map[string]string{"dead.example.com": ""}),
	})

	reports := relay.Send(context.Background(), "example.com", "sender@probe.test",
		[]string{"someone@example.com"}, []byte("Subject: x\r\n\r\n"))
	var failure *DeliveryError
	if !errors.As(reports[0].Err, &failure) {
		t.Fatalf("want a failure, got %v", reports[0].Err)
	}
	if !failure.Temporary() {
		t.Errorf("a refused connection must be temporary, got %v", failure)
	}
}

// -----------------------------------------------------------------------------
// TLS
// -----------------------------------------------------------------------------

func TestRelayUsesSTARTTLSWhenItIsOffered(t *testing.T) {
	t.Parallel()
	host := newFakeMX(t, relayTLSConfig(t))
	relay := newTestRelay(t, RelayConfig{
		Resolver:    fixedResolver("mx.example.com"),
		DialContext: routedDialer(map[string]string{"mx.example.com": host.address()}),
	})

	reports := relay.Send(context.Background(), "example.com", "sender@probe.test",
		[]string{"someone@example.com"}, []byte("Subject: x\r\n\r\n"))
	if reports[0].Err != nil {
		t.Fatalf("want a delivery, got %v", reports[0].Err)
	}
	delivered := host.received()
	if len(delivered) != 1 {
		t.Fatalf("want one delivered message, got %d", len(delivered))
	}
	if !delivered[0].TLS {
		t.Error("the message arrived in the clear, want it inside TLS")
	}
}

func TestRelayDoesNotUseSTARTTLSWhenItIsDisabled(t *testing.T) {
	t.Parallel()
	host := newFakeMX(t, relayTLSConfig(t))
	relay := newTestRelay(t, RelayConfig{
		Resolver:    fixedResolver("mx.example.com"),
		DialContext: routedDialer(map[string]string{"mx.example.com": host.address()}),
		DisableTLS:  true,
	})

	reports := relay.Send(context.Background(), "example.com", "sender@probe.test",
		[]string{"someone@example.com"}, []byte("Subject: x\r\n\r\n"))
	if reports[0].Err != nil {
		t.Fatalf("want a delivery, got %v", reports[0].Err)
	}
	if host.received()[0].TLS {
		t.Error("the message went through TLS, want the clear session that was asked for")
	}
}

// Opportunistic means opportunistic: a host that offers STARTTLS and then
// cannot complete it must not hold the message until it expires.
func TestRelayFallsBackToTheClearWhenSTARTTLSFails(t *testing.T) {
	t.Parallel()
	// The host advertises STARTTLS and then refuses the command, which is the
	// cheapest way to make the negotiation fail without a broken handshake.
	host := newFakeMX(t, relayTLSConfig(t))
	host.script("STARTTLS", "454 4.7.0 TLS not available right now")
	relay := newTestRelay(t, RelayConfig{
		Resolver:    fixedResolver("mx.example.com"),
		DialContext: routedDialer(map[string]string{"mx.example.com": host.address()}),
	})

	reports := relay.Send(context.Background(), "example.com", "sender@probe.test",
		[]string{"someone@example.com"}, []byte("Subject: x\r\n\r\n"))
	if reports[0].Err != nil {
		t.Fatalf("want a delivery in the clear, got %v", reports[0].Err)
	}
	delivered := host.received()
	if len(delivered) != 1 {
		t.Fatalf("want one delivered message, got %d", len(delivered))
	}
	if delivered[0].TLS {
		t.Error("the fallback session used TLS, which the host had refused")
	}
	if got := host.sessionCount(); got != 2 {
		t.Errorf("sessions = %d, want 2: one that tried TLS and one that did not", got)
	}
}

// -----------------------------------------------------------------------------
// The retry decision
// -----------------------------------------------------------------------------

func TestResolverFailureIsPermanentOnlyForANonMailDomain(t *testing.T) {
	t.Parallel()
	permanent := resolverFailure(ErrNoMailHost)
	if permanent.Temporary() {
		t.Errorf("a domain with no mail host must be permanent, got %v", permanent)
	}
	if permanent.Code != 550 {
		t.Errorf("code = %d, want 550", permanent.Code)
	}
	temporary := resolverFailure(errors.New("the resolver timed out"))
	if !temporary.Temporary() {
		t.Errorf("a lookup that did not answer must be temporary, got %v", temporary)
	}
}

// A DNS failure must not be reported to the sender with a message that quotes
// the internals of this server.
func TestResolverFailureDoesNotLeakTheCause(t *testing.T) {
	t.Parallel()
	failure := resolverFailure(errors.New("dial udp 10.0.0.53:53: secret-token-in-config"))
	if strings.Contains(failure.Message, "secret-token-in-config") {
		t.Errorf("the wire message quotes the cause: %q", failure.Message)
	}
}

func TestRemoteFailureUsesTheReplyTheServerSent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		err       error
		wantCode  int
		wantTemp  bool
		wantState string
	}{
		{
			name:      "a permanent reply",
			err:       &textproto.Error{Code: 550, Msg: "5.1.1 No such user here"},
			wantCode:  550,
			wantTemp:  false,
			wantState: "5.1.1",
		},
		{
			name:      "a temporary reply",
			err:       &textproto.Error{Code: 451, Msg: "4.3.0 Deferred"},
			wantCode:  451,
			wantTemp:  true,
			wantState: "4.3.0",
		},
		{
			name:      "a reply with no enhanced status",
			err:       &textproto.Error{Code: 550, Msg: "User unknown"},
			wantCode:  550,
			wantTemp:  false,
			wantState: "5.0.0",
		},
		{
			name:      "a dropped connection is not a reply",
			err:       errors.New("connection reset by peer"),
			wantCode:  451,
			wantTemp:  true,
			wantState: "4.3.0",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			failure := remoteFailure(test.err, 451, "4.3.0", "fallback")
			if failure.Code != test.wantCode {
				t.Errorf("code = %d, want %d", failure.Code, test.wantCode)
			}
			if failure.Temporary() != test.wantTemp {
				t.Errorf("temporary = %v, want %v", failure.Temporary(), test.wantTemp)
			}
			if failure.Enhanced != test.wantState {
				t.Errorf("enhanced = %q, want %q", failure.Enhanced, test.wantState)
			}
		})
	}
}

// A reply is written into a bounce header, so a CR in one is a header injection.
func TestRemoteFailureStripsControlCharactersFromTheReply(t *testing.T) {
	t.Parallel()
	failure := remoteFailure(
		&textproto.Error{Code: 550, Msg: "5.1.1 No\r\nX-Injected: yes"}, 451, "4.3.0", "fallback")
	if strings.ContainsAny(failure.Message, "\r\n") {
		t.Errorf("the message carries a line break: %q", failure.Message)
	}
}

func TestLeadingStatusReadsOnlyAWellFormedStatus(t *testing.T) {
	t.Parallel()
	tests := []struct {
		message string
		want    string
		ok      bool
	}{
		{"5.1.1 No such user", "5.1.1", true},
		{"4.4.7 expired", "4.4.7", true},
		{"2.0.0 fine", "2.0.0", true},
		{"550 no", "", false},
		{"5.1 short", "", false},
		{"9.1.1 bad class", "", false},
		{"5.1111.1 too long", "", false},
		{"5.a.1 not a number", "", false},
		{"", "", false},
	}
	for _, test := range tests {
		got, ok := leadingStatus(test.message)
		if ok != test.ok || got != test.want {
			t.Errorf("leadingStatus(%q) = %q, %v; want %q, %v", test.message, got, ok, test.want, test.ok)
		}
	}
}

// -----------------------------------------------------------------------------
// Choosing mail hosts
// -----------------------------------------------------------------------------

func TestMailHostsFromMXOrdersByPreference(t *testing.T) {
	t.Parallel()
	answer := &dns.Msg{Answer: []dns.RR{
		mxRecord(t, "example.com.", 30, "third.example.net."),
		mxRecord(t, "example.com.", 10, "first.example.net."),
		mxRecord(t, "example.com.", 20, "second.example.net."),
	}}
	hosts, nullMX := mailHostsFromMX(answer, 0)
	if nullMX {
		t.Fatal("this is not a null MX")
	}
	want := []string{"first.example.net", "second.example.net", "third.example.net"}
	if strings.Join(hosts, ",") != strings.Join(want, ",") {
		t.Errorf("hosts = %v, want %v", hosts, want)
	}
}

func TestMailHostsFromMXBoundsTheHostsItReturns(t *testing.T) {
	t.Parallel()
	var records []dns.RR
	for i := 0; i < 20; i++ {
		records = append(records, mxRecord(t, "example.com.", uint16(i), fmt.Sprintf("mx%02d.example.net.", i)))
	}
	hosts, _ := mailHostsFromMX(&dns.Msg{Answer: records}, 3)
	if len(hosts) != 3 {
		t.Fatalf("hosts = %v, want three", hosts)
	}
	if hosts[0] != "mx00.example.net" {
		t.Errorf("the bound dropped the best host: %v", hosts)
	}
}

// RFC 7505: one MX with preference 0 and a root target means "this domain sends
// but never receives". Dialling the root would be a delivery attempt against
// every root server.
func TestMailHostsFromMXRefusesANullMX(t *testing.T) {
	t.Parallel()
	answer := &dns.Msg{Answer: []dns.RR{mxRecord(t, "example.com.", 0, ".")}}
	hosts, nullMX := mailHostsFromMX(answer, 0)
	if !nullMX {
		t.Fatalf("want a null MX, got hosts %v", hosts)
	}
}

func TestMailHostsFromMXDropsDuplicatesAndEmptyTargets(t *testing.T) {
	t.Parallel()
	answer := &dns.Msg{Answer: []dns.RR{
		mxRecord(t, "example.com.", 10, "mx.example.net."),
		mxRecord(t, "example.com.", 20, "MX.example.net."),
		mxRecord(t, "example.com.", 30, "."),
		// A non-MX record in the answer section, which a CNAME chain produces.
		&dns.A{Hdr: dns.RR_Header{Name: "example.com.", Rrtype: dns.TypeA, Class: dns.ClassINET}},
	}}
	hosts, nullMX := mailHostsFromMX(answer, 0)
	if nullMX {
		t.Fatal("three records is not a null MX")
	}
	if len(hosts) != 1 || hosts[0] != "mx.example.net" {
		t.Errorf("hosts = %v, want one lowercased host", hosts)
	}
}

// NXDOMAIN is the only response code that must not be retried.
func TestRcodeErrorSplitsPermanentFromTemporary(t *testing.T) {
	t.Parallel()
	if err := rcodeError(dns.RcodeSuccess, "MX"); err != nil {
		t.Errorf("NOERROR is not an error: %v", err)
	}
	if err := rcodeError(dns.RcodeNameError, "MX"); !errors.Is(err, ErrNoMailHost) {
		t.Errorf("NXDOMAIN = %v, want ErrNoMailHost", err)
	}
	err := rcodeError(dns.RcodeServerFailure, "MX")
	if err == nil || errors.Is(err, ErrNoMailHost) {
		t.Errorf("SERVFAIL = %v, want a temporary error", err)
	}
}

func TestResolverAddressPutsTheDefaultPortOnBareAddresses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want string
		ok   bool
	}{
		{"10.0.0.53", "10.0.0.53:53", true},
		{"10.0.0.53:5353", "10.0.0.53:5353", true},
		{"2001:db8::1", "[2001:db8::1]:53", true},
		{"[2001:db8::1]:5353", "[2001:db8::1]:5353", true},
		{"  ", "", false},
	}
	for _, test := range tests {
		got, ok := resolverAddress(test.in)
		if ok != test.ok || got != test.want {
			t.Errorf("resolverAddress(%q) = %q, %v; want %q, %v", test.in, got, ok, test.want, test.ok)
		}
	}
}

// -----------------------------------------------------------------------------
// The DNS resolver, against a real server on loopback
// -----------------------------------------------------------------------------

// newFakeResolverServer runs a miekg/dns server on 127.0.0.1 and returns its
// address. It answers from respond, which sees the question and returns the
// records and response code for it.
func newFakeResolverServer(t *testing.T, respond func(dns.Question) ([]dns.RR, int)) string {
	t.Helper()
	packet, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for DNS: %v", err)
	}
	server := &dns.Server{PacketConn: packet}
	server.Handler = dns.HandlerFunc(func(writer dns.ResponseWriter, request *dns.Msg) {
		reply := new(dns.Msg)
		reply.SetReply(request)
		if len(request.Question) == 1 {
			records, rcode := respond(request.Question[0])
			reply.Rcode = rcode
			reply.Answer = records
		}
		if err := writer.WriteMsg(reply); err != nil {
			t.Errorf("write a DNS reply: %v", err)
		}
	})
	// Shutdown before the read loop has started returns an error and leaves the
	// loop running, so the cleanup would block forever. Waiting for the started
	// callback is what makes a test that asks no question shut down as cleanly
	// as one that does.
	ready := make(chan struct{})
	server.NotifyStartedFunc = func() { close(ready) }
	stopped := make(chan error, 1)
	go func() { stopped <- server.ActivateAndServe() }()
	<-ready
	t.Cleanup(func() {
		if err := server.Shutdown(); err != nil {
			t.Errorf("shut the fake resolver down: %v", err)
		}
		if err := <-stopped; err != nil {
			t.Errorf("the fake resolver failed: %v", err)
		}
	})
	return packet.LocalAddr().String()
}

func newTestDNSResolver(t *testing.T, address string) *DNSResolver {
	t.Helper()
	resolver, err := NewDNSResolver(ResolverConfig{
		Servers: []string{address},
		Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("build the resolver: %v", err)
	}
	return resolver
}

func TestDNSResolverReturnsMXHostsInPreferenceOrder(t *testing.T) {
	t.Parallel()
	address := newFakeResolverServer(t, func(question dns.Question) ([]dns.RR, int) {
		if question.Qtype != dns.TypeMX {
			return nil, dns.RcodeSuccess
		}
		return []dns.RR{
			mxRecord(t, question.Name, 20, "backup.example.net."),
			mxRecord(t, question.Name, 10, "primary.example.net."),
		}, dns.RcodeSuccess
	})

	hosts, err := newTestDNSResolver(t, address).LookupMailHosts(context.Background(), "example.com")
	if err != nil {
		t.Fatalf("look up the mail hosts: %v", err)
	}
	want := []string{"primary.example.net", "backup.example.net"}
	if strings.Join(hosts, ",") != strings.Join(want, ",") {
		t.Errorf("hosts = %v, want %v", hosts, want)
	}
}

// RFC 5321 §5.1: a domain with an address and no MX is its own mail host.
func TestDNSResolverFallsBackToTheAddressWhenThereIsNoMX(t *testing.T) {
	t.Parallel()
	address := newFakeResolverServer(t, func(question dns.Question) ([]dns.RR, int) {
		if question.Qtype == dns.TypeA {
			return []dns.RR{&dns.A{
				Hdr: dns.RR_Header{Name: question.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
				A:   net.IPv4(192, 0, 2, 25),
			}}, dns.RcodeSuccess
		}
		return nil, dns.RcodeSuccess
	})

	hosts, err := newTestDNSResolver(t, address).LookupMailHosts(context.Background(), "example.com")
	if err != nil {
		t.Fatalf("look up the mail hosts: %v", err)
	}
	if len(hosts) != 1 || hosts[0] != "example.com" {
		t.Errorf("hosts = %v, want the domain itself", hosts)
	}
}

func TestDNSResolverFallsBackToAAAAWhenThereIsNoA(t *testing.T) {
	t.Parallel()
	address := newFakeResolverServer(t, func(question dns.Question) ([]dns.RR, int) {
		if question.Qtype == dns.TypeAAAA {
			return []dns.RR{&dns.AAAA{
				Hdr:  dns.RR_Header{Name: question.Name, Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: 60},
				AAAA: net.ParseIP("2001:db8::19"),
			}}, dns.RcodeSuccess
		}
		return nil, dns.RcodeSuccess
	})

	hosts, err := newTestDNSResolver(t, address).LookupMailHosts(context.Background(), "example.com")
	if err != nil {
		t.Fatalf("look up the mail hosts: %v", err)
	}
	if len(hosts) != 1 || hosts[0] != "example.com" {
		t.Errorf("hosts = %v, want the domain itself", hosts)
	}
}

// A domain with neither an MX nor an address accepts no mail, and retrying for
// five days will find the same thing.
func TestDNSResolverRefusesADomainWithNoMailHostPermanently(t *testing.T) {
	t.Parallel()
	address := newFakeResolverServer(t, func(dns.Question) ([]dns.RR, int) {
		return nil, dns.RcodeSuccess
	})

	_, err := newTestDNSResolver(t, address).LookupMailHosts(context.Background(), "example.com")
	if !errors.Is(err, ErrNoMailHost) {
		t.Errorf("err = %v, want ErrNoMailHost", err)
	}
}

func TestDNSResolverTreatsNXDOMAINAsPermanentAndSERVFAILAsTemporary(t *testing.T) {
	t.Parallel()
	missing := newFakeResolverServer(t, func(dns.Question) ([]dns.RR, int) {
		return nil, dns.RcodeNameError
	})
	if _, err := newTestDNSResolver(t, missing).LookupMailHosts(context.Background(), "example.com"); !errors.Is(err, ErrNoMailHost) {
		t.Errorf("NXDOMAIN = %v, want ErrNoMailHost", err)
	}

	broken := newFakeResolverServer(t, func(dns.Question) ([]dns.RR, int) {
		return nil, dns.RcodeServerFailure
	})
	_, err := newTestDNSResolver(t, broken).LookupMailHosts(context.Background(), "example.com")
	if err == nil || errors.Is(err, ErrNoMailHost) {
		t.Errorf("SERVFAIL = %v, want a temporary error", err)
	}
}

// The domain reaches a wire format, so it is validated before it is encoded.
func TestDNSResolverRefusesADomainThatIsNotAName(t *testing.T) {
	t.Parallel()
	address := newFakeResolverServer(t, func(dns.Question) ([]dns.RR, int) {
		t.Error("the resolver was queried for an invalid name")
		return nil, dns.RcodeSuccess
	})
	_, err := newTestDNSResolver(t, address).LookupMailHosts(context.Background(), "not a domain")
	if !errors.Is(err, ErrNoMailHost) {
		t.Errorf("err = %v, want ErrNoMailHost", err)
	}
}

func TestNewRelayRefusesToStartWithoutAResolver(t *testing.T) {
	t.Parallel()
	if _, err := NewRelay(RelayConfig{}); !errors.Is(err, ErrInvalid) {
		t.Errorf("err = %v, want ErrInvalid", err)
	}
}

// mxRecord builds one MX record.
func mxRecord(t *testing.T, owner string, preference uint16, target string) *dns.MX {
	t.Helper()
	return &dns.MX{
		Hdr:        dns.RR_Header{Name: owner, Rrtype: dns.TypeMX, Class: dns.ClassINET, Ttl: 60},
		Preference: preference,
		Mx:         target,
	}
}

// -----------------------------------------------------------------------------
// Refusing a mail host that is not on the public internet
// -----------------------------------------------------------------------------
//
// The MX answer is attacker-controlled input: it is a record in a zone the
// sender's own domain publishes. These tests are about the one place that
// treats it that way — the dialer — and about the two things a refusal has to
// be: permanent, and silent about what it resolved.

// recordingDialer never opens a socket. It records what it was asked for, so a
// test can tell "the guard refused" from "the connection failed".
type recordingDialer struct {
	mu     sync.Mutex
	dialed []string
}

func (d *recordingDialer) dial(_ context.Context, _, address string) (net.Conn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.dialed = append(d.dialed, address)
	return nil, errors.New("this dialer opens no sockets")
}

func (d *recordingDialer) addresses() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.dialed...)
}

// fixedAddrLookup answers the guard's resolution from a table, so a test names
// an address without asking the network what a host name means.
func fixedAddrLookup(t *testing.T, table map[string][]string) func(context.Context, string) ([]netip.Addr, error) {
	t.Helper()
	return func(_ context.Context, host string) ([]netip.Addr, error) {
		texts, known := table[host]
		if !known {
			return nil, fmt.Errorf("no address for %q", host)
		}
		addrs := make([]netip.Addr, 0, len(texts))
		for _, text := range texts {
			addr, err := netip.ParseAddr(text)
			if err != nil {
				t.Fatalf("parse %q: %v", text, err)
			}
			addrs = append(addrs, addr)
		}
		return addrs, nil
	}
}

func TestRoutableMailHostRefusesEveryAddressThatIsNotOnThePublicInternet(t *testing.T) {
	t.Parallel()
	refused := []string{
		"127.0.0.1", "127.1.2.3", // loopback
		"::1",           // loopback, v6
		"0.0.0.0", "::", // unspecified
		"10.1.2.3", "172.16.0.1", "192.168.1.1", // RFC 1918
		"100.64.0.1",                        // carrier-grade NAT
		"169.254.169.254",                   // the cloud metadata service
		"fe80::1",                           // link-local
		"fd00::1",                           // unique local
		"224.0.0.1", "239.1.1.1", "ff02::1", // multicast
		"255.255.255.255", "240.0.0.1", // broadcast and reserved
		"198.18.0.1", "192.0.2.1", "198.51.100.1", "203.0.113.1", // benchmarking and TEST-NET
		"192.0.0.1",   // IETF protocol assignments
		"2001:db8::1", // documentation
		"100::1",      // discard-only
		// The bypasses: the same internal addresses wearing IPv6.
		"::ffff:127.0.0.1", // IPv4-mapped loopback
		"::ffff:10.0.0.1",  // IPv4-mapped RFC 1918
		"::ffff:169.254.169.254",
		"::127.0.0.1",     // deprecated IPv4-compatible form
		"2002:7f00:1::",   // 6to4 wrapping 127.0.0.1
		"64:ff9b::7f00:1", // NAT64 wrapping 127.0.0.1
		"2001:0:7f00:1::", // Teredo
	}
	for _, text := range refused {
		addr, err := netip.ParseAddr(text)
		if err != nil {
			t.Fatalf("parse %q: %v", text, err)
		}
		if routableMailHost(addr) {
			t.Errorf("%s was accepted as a public address", text)
		}
	}

	allowed := []string{
		"8.8.8.8", "1.1.1.1", "93.184.216.34", "203.1.113.1",
		"2606:4700:4700::1111", "2a00:1450:4001:80e::200e",
	}
	for _, text := range allowed {
		addr, err := netip.ParseAddr(text)
		if err != nil {
			t.Fatalf("parse %q: %v", text, err)
		}
		if !routableMailHost(addr) {
			t.Errorf("%s was refused, and it is an ordinary public address", text)
		}
	}

	// The zero value is not an address at all, and "not an address" is not a
	// reason to open a connection.
	if routableMailHost(netip.Addr{}) {
		t.Error("the zero address was accepted")
	}
}

// A name is refused for what it resolves to, not for how it is spelled: this is
// the shape of the attack, an ordinary-looking MX host in a zone the sender
// controls.
func TestMailHostDialerRefusesANameThatResolvesInside(t *testing.T) {
	t.Parallel()
	socket := &recordingDialer{}
	guard := &mailHostDialer{
		dial:   socket.dial,
		logger: discardLogger(),
		lookup: fixedAddrLookup(t, map[string][]string{
			"mx.attacker.example": {"169.254.169.254", "::ffff:127.0.0.1"},
		}),
	}

	_, err := guard.DialContext(context.Background(), "tcp", "mx.attacker.example:25")
	if !errors.Is(err, errUnroutableMailHost) {
		t.Fatalf("err = %v, want errUnroutableMailHost", err)
	}
	if dialed := socket.addresses(); len(dialed) != 0 {
		t.Fatalf("the guard dialled %v", dialed)
	}
	// The refusal names the host, which came from DNS, and never the address,
	// which is this network's own topology.
	if strings.Contains(err.Error(), "169.254") || strings.Contains(err.Error(), "127.0.0.1") {
		t.Errorf("the refusal leaked a resolved address: %v", err)
	}
}

// An address literal is checked whatever the transport underneath is, because
// checking it costs no lookup.
func TestMailHostDialerRefusesAnInternalAddressLiteral(t *testing.T) {
	t.Parallel()
	socket := &recordingDialer{}
	guard := &mailHostDialer{dial: socket.dial, logger: discardLogger()}

	for _, address := range []string{"127.0.0.1:25", "[::1]:25", "10.0.0.7:25", "[::ffff:127.0.0.1]:25"} {
		if _, err := guard.DialContext(context.Background(), "tcp", address); !errors.Is(err, errUnroutableMailHost) {
			t.Errorf("DialContext(%q) = %v, want errUnroutableMailHost", address, err)
		}
	}
	if dialed := socket.addresses(); len(dialed) != 0 {
		t.Fatalf("the guard dialled %v", dialed)
	}
}

// A host with one usable address and one that is not is still deliverable: the
// guard drops the addresses, not the host.
func TestMailHostDialerDialsTheCheckedAddressAndSkipsTheRest(t *testing.T) {
	t.Parallel()
	socket := &recordingDialer{}
	guard := &mailHostDialer{
		dial:   socket.dial,
		logger: discardLogger(),
		lookup: fixedAddrLookup(t, map[string][]string{
			"mx.example.com": {"192.168.1.10", "93.184.216.34"},
		}),
	}

	if _, err := guard.DialContext(context.Background(), "tcp", "mx.example.com:25"); err == nil {
		t.Fatal("want the recording dialer's error")
	} else if errors.Is(err, errUnroutableMailHost) {
		t.Fatalf("the host was refused although one address is public: %v", err)
	}
	// The address is dialled, not the name. A second resolution inside the
	// dialer could answer 192.168.1.10 and undo the check.
	want := []string{"93.184.216.34:25"}
	if got := socket.addresses(); len(got) != 1 || got[0] != want[0] {
		t.Errorf("dialled %v, want %v", got, want)
	}
}

// A lookup that failed is not a refusal. It is the same temporary condition as
// any other DNS failure, and it must not be turned into a bounce.
func TestMailHostDialerTreatsALookupFailureAsTemporary(t *testing.T) {
	t.Parallel()
	guard := &mailHostDialer{
		dial:   (&recordingDialer{}).dial,
		logger: discardLogger(),
		lookup: fixedAddrLookup(t, nil),
	}
	_, err := guard.DialContext(context.Background(), "tcp", "mx.example.com:25")
	if err == nil || errors.Is(err, errUnroutableMailHost) {
		t.Fatalf("err = %v, want the lookup failure", err)
	}
}

// A caller-supplied transport resolves its own names — that is what a test's
// routedDialer is — so a name goes through to it untouched. This is the only
// gap in the guard, and it is only reachable by handing the relay a transport,
// which no production wiring does.
func TestMailHostDialerPassesANameToACallerSuppliedTransport(t *testing.T) {
	t.Parallel()
	socket := &recordingDialer{}
	guard := &mailHostDialer{dial: socket.dial, logger: discardLogger()}

	if _, err := guard.DialContext(context.Background(), "tcp", "mx.example.com:25"); errors.Is(err, errUnroutableMailHost) {
		t.Fatalf("a name was refused without being resolved: %v", err)
	}
	if got := socket.addresses(); len(got) != 1 || got[0] != "mx.example.com:25" {
		t.Errorf("dialled %v, want the name untouched", got)
	}
}

// The relay a caller builds with no DialContext — the production shape — refuses
// an internal mail host, permanently, and tells the sender nothing about what it
// resolved.
func TestRelayRefusesAnInternalMailHostPermanentlyAndSaysNothingAboutIt(t *testing.T) {
	t.Parallel()
	host := newFakeMX(t, nil)
	_, port, err := net.SplitHostPort(host.address())
	if err != nil {
		t.Fatalf("split the fake host address: %v", err)
	}
	relay := newTestRelay(t, RelayConfig{
		Port:       port,
		DisableTLS: true,
		Logger:     discardLogger(),
		Resolver:   fixedResolver("127.0.0.1"),
	})

	reports := relay.Send(context.Background(), "attacker.example", "sender@attacker.example",
		[]string{"victim@attacker.example"}, []byte("Subject: probe\r\n\r\nbody\r\n"))

	if len(reports) != 1 {
		t.Fatalf("want one report, got %d", len(reports))
	}
	if host.sessionCount() != 0 {
		t.Fatalf("the internal listener saw %d session(s)", host.sessionCount())
	}
	var failure *DeliveryError
	if !errors.As(reports[0].Err, &failure) {
		t.Fatalf("err = %v, want a *DeliveryError", reports[0].Err)
	}
	if failure.Temporary() {
		t.Errorf("failure = %d, want a permanent one: a bad MX must not be retried for five days", failure.Code)
	}
	if failure.Code != 550 {
		t.Errorf("code = %d, want 550", failure.Code)
	}
	if !errors.Is(failure, errUnroutableMailHost) {
		t.Errorf("failure does not wrap errUnroutableMailHost: %v", failure)
	}
	// Message is the only field a bounce carries back to the sender, and the
	// sender here is nobody this server trusts.
	if strings.Contains(failure.Message, "127.0.0") || strings.Contains(failure.Message, "resolved") {
		t.Errorf("the bounce text describes what was resolved: %q", failure.Message)
	}
}

// One poisoned MX does not stop delivery to the domain's other hosts, and the
// refusal is still what a fully internal domain gets.
func TestRelayTriesTheNextMailHostWhenTheFirstIsInternal(t *testing.T) {
	t.Parallel()
	host := newFakeMX(t, nil)
	guard := &mailHostDialer{
		dial:   routedDialer(map[string]string{"93.184.216.34": host.address()}),
		logger: discardLogger(),
		lookup: fixedAddrLookup(t, map[string][]string{
			"internal.attacker.example": {"10.0.0.5"},
			"mx.example.com":            {"93.184.216.34"},
		}),
	}
	relay := newTestRelay(t, RelayConfig{
		Logger:      discardLogger(),
		Resolver:    fixedResolver("internal.attacker.example", "mx.example.com"),
		DialContext: guard.DialContext,
	})

	reports := relay.Send(context.Background(), "example.com", "sender@probe.test",
		[]string{"someone@example.com"}, []byte("Subject: hello\r\n\r\nbody\r\n"))

	if len(reports) != 1 || reports[0].Err != nil {
		t.Fatalf("want a delivery, got %+v", reports)
	}
	if delivered := host.received(); len(delivered) != 1 {
		t.Fatalf("the public mail host received %d messages", len(delivered))
	}
}

// The opt-in is the only way past the guard, and it is unexported: this test
// can set it and cmd/deephost-mail cannot name it. The same configuration
// without it is refused, which is what makes this a switch and not a hole.
func TestRelayReachesALoopbackMailHostOnlyWithTheInternalOptIn(t *testing.T) {
	t.Parallel()
	host := newFakeMX(t, nil)
	_, port, err := net.SplitHostPort(host.address())
	if err != nil {
		t.Fatalf("split the fake host address: %v", err)
	}
	config := RelayConfig{
		Port:       port,
		DisableTLS: true,
		Logger:     discardLogger(),
		Resolver:   fixedResolver("127.0.0.1"),
	}

	refused := newTestRelay(t, config).Send(context.Background(), "example.com", "sender@probe.test",
		[]string{"someone@example.com"}, []byte("Subject: hello\r\n\r\nbody\r\n"))
	if len(refused) != 1 || !errors.Is(refused[0].Err, errUnroutableMailHost) {
		t.Fatalf("without the opt-in: %+v, want a refusal", refused)
	}

	config.allowInternalMailHosts = true
	reports := newTestRelay(t, config).Send(context.Background(), "example.com", "sender@probe.test",
		[]string{"someone@example.com"}, []byte("Subject: hello\r\n\r\nbody\r\n"))
	if len(reports) != 1 || reports[0].Err != nil {
		t.Fatalf("with the opt-in: %+v, want a delivery", reports)
	}
	if delivered := host.received(); len(delivered) != 1 {
		t.Fatalf("the loopback mail host received %d messages", len(delivered))
	}
}

// A refusal is about one host. Another host that merely could not be reached
// still means "try later", because bouncing a message that might yet deliver is
// the one failure this file cannot make.
func TestRelayKeepsATemporaryFailureOverALaterInternalMailHost(t *testing.T) {
	t.Parallel()
	guard := &mailHostDialer{
		dial:   routedDialer(map[string]string{"93.184.216.34": ""}),
		logger: discardLogger(),
		lookup: fixedAddrLookup(t, map[string][]string{
			"mx1.example.com":           {"93.184.216.34"},
			"internal.attacker.example": {"10.0.0.5"},
		}),
	}
	relay := newTestRelay(t, RelayConfig{
		Logger:      discardLogger(),
		Resolver:    fixedResolver("mx1.example.com", "internal.attacker.example"),
		DialContext: guard.DialContext,
	})

	reports := relay.Send(context.Background(), "example.com", "sender@probe.test",
		[]string{"someone@example.com"}, []byte("Subject: hello\r\n\r\nbody\r\n"))

	if len(reports) != 1 {
		t.Fatalf("want one report, got %d", len(reports))
	}
	var failure *DeliveryError
	if !errors.As(reports[0].Err, &failure) {
		t.Fatalf("err = %v, want a *DeliveryError", reports[0].Err)
	}
	if !failure.Temporary() {
		t.Fatalf("failure = %v, want the first host's temporary one", failure)
	}
}
