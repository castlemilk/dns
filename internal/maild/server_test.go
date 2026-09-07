package maild_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/castlemilk/dns/internal/maild"
)

// adminToken is a token long enough to be accepted. It is a literal rather than
// a generated value so that the "never logged" assertions can look for it.
const adminToken = "test-token-0123456789abcdefghijklmnop"

// -----------------------------------------------------------------------------
// Refusals
// -----------------------------------------------------------------------------

func TestNewServerRefuses(t *testing.T) {
	t.Parallel()

	valid := func(dir string) maild.Config {
		return maild.Config{
			Hostname:   "mail.local.test",
			AdminToken: maild.Secret(adminToken),
			DataDir:    dir,
			APIAddr:    "127.0.0.1:0",
			Services:   fixedServices(maild.Services{API: okHandler()}),
			Logger:     discardLogger(),
		}
	}

	tests := []struct {
		name    string
		mutate  func(*maild.Config)
		wantErr string
	}{
		{
			name:    "empty hostname",
			mutate:  func(c *maild.Config) { c.Hostname = "" },
			wantErr: "the hostname is required and must equal the control plane's MAIL_HOSTNAME",
		},
		{
			name:    "blank hostname",
			mutate:  func(c *maild.Config) { c.Hostname = "   " },
			wantErr: "the hostname is required",
		},
		{
			name:    "padded hostname",
			mutate:  func(c *maild.Config) { c.Hostname = " mail.local.test " },
			wantErr: "must not have leading or trailing spaces",
		},
		{
			name:    "empty token",
			mutate:  func(c *maild.Config) { c.AdminToken = nil },
			wantErr: "the admin token must be at least 32 characters",
		},
		{
			name:    "short token",
			mutate:  func(c *maild.Config) { c.AdminToken = maild.Secret(strings.Repeat("x", 31)) },
			wantErr: "got 31",
		},
		{
			name:    "no data directory",
			mutate:  func(c *maild.Config) { c.DataDir = "" },
			wantErr: "the data directory is required",
		},
		{
			name:    "certificate without key",
			mutate:  func(c *maild.Config) { c.TLSCertFile = "cert.pem" },
			wantErr: "must be set together",
		},
		{
			name:    "key without certificate",
			mutate:  func(c *maild.Config) { c.TLSKeyFile = "key.pem" },
			wantErr: "must be set together",
		},
		{
			name:    "API TLS without a certificate",
			mutate:  func(c *maild.Config) { c.APITLS = true },
			wantErr: "needs a TLS certificate",
		},
		{
			name: "the handlers cannot be built",
			mutate: func(c *maild.Config) {
				c.Services = func(maild.Deps) (maild.Services, error) {
					return maild.Services{}, errors.New("the DKIM key is unreadable")
				}
			},
			wantErr: "build the protocol handlers: the DKIM key is unreadable",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			config := valid(t.TempDir())
			test.mutate(&config)
			server, err := maild.NewServer(config)
			if err == nil {
				t.Cleanup(func() { ignore(server.Shutdown(context.Background())) })
				t.Fatalf("NewServer accepted %s", test.name)
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("NewServer error = %q, want it to contain %q", err, test.wantErr)
			}
			if strings.Contains(err.Error(), adminToken) {
				t.Fatal("the refusal quoted the admin token")
			}
		})
	}
}

// A short token is refused before the store is opened, so a misconfigured
// process never touches the data directory.
func TestNewServerRefusesBeforeTouchingTheStore(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	_, err := maild.NewServer(maild.Config{
		Hostname:   "mail.local.test",
		AdminToken: maild.Secret("too-short"),
		DataDir:    dir,
		Logger:     discardLogger(),
	})
	if err == nil {
		t.Fatal("NewServer accepted a short token")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the data directory: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("the refused start left %d entries in the data directory", len(entries))
	}
}

func TestStartRefusesWithNothingToServe(t *testing.T) {
	t.Parallel()

	// Every address is set, but no handler is installed, so there is nothing to
	// serve and saying so is better than idling as a healthy-looking process.
	server := newTestServer(t, func(c *maild.Config) {
		c.Services = fixedServices(maild.Services{})
		c.SMTPAddr = "127.0.0.1:0"
	})
	err := server.Start(context.Background())
	if err == nil {
		t.Fatal("Start accepted a server with no enabled listener")
	}
	if !strings.Contains(err.Error(), "no listener is enabled") {
		t.Fatalf("Start error = %q", err)
	}
}

func TestStartIsNotRepeatable(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, nil)
	mustStart(t, server)
	if err := server.Start(context.Background()); err == nil {
		t.Fatal("Start ran twice")
	}
}

// -----------------------------------------------------------------------------
// The happy path
// -----------------------------------------------------------------------------

func TestServerServesEveryListenerAndShutsDownCleanly(t *testing.T) {
	t.Parallel()

	certFile, keyFile := writeCertificate(t)
	seen := &sessionRecorder{}
	server := newTestServer(t, func(c *maild.Config) {
		c.TLSCertFile, c.TLSKeyFile = certFile, keyFile
		c.SMTPAddr = "127.0.0.1:0"
		c.SubmissionAddr = "127.0.0.1:0"
		c.SubmissionsAddr = "127.0.0.1:0"
		c.POP3Addr = "127.0.0.1:0"
		c.POP3SAddr = "127.0.0.1:0"
		c.Services = func(deps maild.Deps) (maild.Services, error) {
			if deps.Store == nil {
				return maild.Services{}, errors.New("the store was not open")
			}
			if deps.TLS == nil {
				return maild.Services{}, errors.New("the certificate was not loaded")
			}
			if deps.Hostname != "mail.local.test" {
				return maild.Services{}, errors.New("the hostname did not arrive")
			}
			if _, err := os.Stat(deps.SpoolDir); err != nil {
				return maild.Services{}, err
			}
			return maild.Services{
				API:        okHandler(),
				SMTP:       seen.handler("smtp"),
				Submission: seen.handler("submission"),
				POP3:       seen.handler("pop3"),
			}, nil
		}
	})
	mustStart(t, server)

	for _, name := range []string{
		maild.ListenerAPI,
		maild.ListenerSMTP, maild.ListenerSubmission, maild.ListenerSubmissions,
		maild.ListenerPOP3, maild.ListenerPOP3S,
	} {
		if server.Addr(name) == "" {
			t.Fatalf("listener %q did not bind", name)
		}
	}
	if got := len(server.Addrs()); got != 6 {
		t.Fatalf("Addrs reported %d listeners, want 6", got)
	}

	// The management API answers on the port it reported.
	response, err := http.Get("http://" + server.Addr(maild.ListenerAPI) + "/api/account")
	if err != nil {
		t.Fatalf("call the management API: %v", err)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read the management API answer: %v", err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatalf("close the management API answer: %v", err)
	}
	if response.StatusCode != http.StatusOK || string(body) != "served" {
		t.Fatalf("management API answered %d %q", response.StatusCode, body)
	}

	// The plain line protocols greet.
	for _, test := range []struct{ listener, want string }{
		{maild.ListenerSMTP, "smtp"},
		{maild.ListenerSubmission, "submission"},
		{maild.ListenerPOP3, "pop3"},
	} {
		if got := greet(t, server.Addr(test.listener), false); got != test.want {
			t.Fatalf("%s greeted %q, want %q", test.listener, got, test.want)
		}
	}
	// The implicit-TLS ports are TLS from the first byte and reach the same
	// handler as their plaintext twin.
	for _, test := range []struct{ listener, want string }{
		{maild.ListenerSubmissions, "submission"},
		{maild.ListenerPOP3S, "pop3"},
	} {
		if got := greet(t, server.Addr(test.listener), true); got != test.want {
			t.Fatalf("%s greeted %q, want %q", test.listener, got, test.want)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	// The store is closed, so the next process to start can take the file lock.
	if _, err := server.Store().ListDomains(context.Background()); err == nil {
		t.Fatal("the store was still open after Shutdown")
	}
	// Nothing is listening any more.
	for name, addr := range map[string]string{
		maild.ListenerSMTP: server.Addr(maild.ListenerSMTP),
		maild.ListenerAPI:  server.Addr(maild.ListenerAPI),
	} {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			ignore(conn.Close())
			t.Fatalf("listener %q was still accepting after Shutdown", name)
		}
	}
	// Shutting down twice is not an error and does not hang.
	if err := server.Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}
}

func TestRunReturnsWhenTheContextIsCancelled(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()

	waitFor(t, func() bool { return server.Addr(maild.ListenerAPI) != "" })
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after the context was cancelled")
	}
}

func TestRunClosesTheStoreWhenTheStartFails(t *testing.T) {
	t.Parallel()

	// Take the port first, so the bind cannot succeed.
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("take a port: %v", err)
	}
	defer func() { ignore(taken.Close()) }()

	server := newTestServer(t, func(c *maild.Config) { c.APIAddr = taken.Addr().String() })
	if err := server.Run(context.Background()); err == nil {
		t.Fatal("Run started on a port that was already taken")
	}
	if _, err := server.Store().ListDomains(context.Background()); err == nil {
		t.Fatal("the failed start left the store open")
	}
}

// A bind that fails part-way releases the ports it already took, so a retry —
// or a second process — is not blocked by the corpse of the first attempt.
func TestStartReleasesPortsWhenOneBindFails(t *testing.T) {
	t.Parallel()

	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("take a port: %v", err)
	}
	defer func() { ignore(taken.Close()) }()

	// A free port for the API, and the taken one for SMTP: the API binds first
	// and must be released when SMTP fails.
	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find a free port: %v", err)
	}
	apiAddr := free.Addr().String()
	if err := free.Close(); err != nil {
		t.Fatalf("release the probe port: %v", err)
	}

	server := newTestServer(t, func(c *maild.Config) {
		c.APIAddr = apiAddr
		c.SMTPAddr = taken.Addr().String()
		c.Services = fixedServices(maild.Services{
			API:  okHandler(),
			SMTP: maild.SessionHandlerFunc(func(context.Context, net.Conn) {}),
		})
	})
	if err := server.Start(context.Background()); err == nil {
		t.Fatal("Start bound a port that was already taken")
	} else if !strings.Contains(err.Error(), "bind smtp") {
		t.Fatalf("Start error = %q, want it to name the listener", err)
	}
	if len(server.Addrs()) != 0 {
		t.Fatalf("Addrs reported %v after a failed start", server.Addrs())
	}
	retry, err := net.Listen("tcp", apiAddr)
	if err != nil {
		t.Fatalf("the API port was not released: %v", err)
	}
	ignore(retry.Close())
}

// -----------------------------------------------------------------------------
// Startup logging
// -----------------------------------------------------------------------------

func TestStartupLogsSayWhatIsBoundAndWhatIsNot(t *testing.T) {
	t.Parallel()

	logs := &syncBuffer{}
	server := newTestServer(t, func(c *maild.Config) {
		c.Logger = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
		c.SMTPAddr = "127.0.0.1:0"
		c.SubmissionsAddr = "127.0.0.1:0" // set, but there is no certificate
		c.Services = fixedServices(maild.Services{
			API:        okHandler(),
			SMTP:       maild.SessionHandlerFunc(func(context.Context, net.Conn) {}),
			Submission: maild.SessionHandlerFunc(func(context.Context, net.Conn) {}),
		})
	})
	mustStart(t, server)

	text := logs.String()
	for _, want := range []string{
		`"hostname":"mail.local.test"`,
		`"listener":"jmap"`,
		`"listener":"smtp"`,
		// Disabled, each with the reason that tells the operator what to change:
		// a missing address, a missing certificate, a missing handler.
		`"listener":"submission","reason":"no listen address is configured"`,
		`"listener":"submissions","reason":"no TLS certificate is configured`,
		`"listener":"pop3","reason":"no listen address is configured"`,
		`"listener":"pop3s","reason":"no listen address is configured"`,
		"STARTTLS cannot be offered",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("the startup log is missing %q\n%s", want, text)
		}
	}
	// The bound listeners report the address they actually took, which for
	// port 0 is not the address that was asked for.
	if !strings.Contains(text, `"addr":"`+server.Addr(maild.ListenerSMTP)+`"`) {
		t.Fatalf("the startup log did not report the bound SMTP address\n%s", text)
	}
}

// The admin token is a Secret so that it cannot be logged by accident, and
// nothing in the startup path logs it on purpose either.
func TestStartupLogsNeverCarryTheAdminToken(t *testing.T) {
	t.Parallel()

	logs := &syncBuffer{}
	server := newTestServer(t, func(c *maild.Config) {
		c.Logger = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	})
	mustStart(t, server)
	if err := server.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if strings.Contains(logs.String(), adminToken) {
		t.Fatalf("the startup log carried the admin token\n%s", logs.String())
	}
	// Nor is it recoverable from the redacted form.
	if strings.Contains(logs.String(), "token") {
		t.Fatalf("the startup log mentioned a token at all\n%s", logs.String())
	}
}

// -----------------------------------------------------------------------------
// Session bounds
// -----------------------------------------------------------------------------

func TestConcurrencyLimitRefusesRatherThanQueues(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	held := make(chan struct{}, 1)
	server := newTestServer(t, func(c *maild.Config) {
		c.SMTPAddr = "127.0.0.1:0"
		c.Limits.MaxSessions = 1
		c.Services = fixedServices(maild.Services{
			API: okHandler(),
			SMTP: maild.SessionHandlerFunc(func(_ context.Context, conn net.Conn) {
				held <- struct{}{}
				ignoreWrite(conn.Write([]byte("held\n")))
				<-release
			}),
		})
	})
	mustStart(t, server)
	defer func() { close(release) }()

	first, err := net.Dial("tcp", server.Addr(maild.ListenerSMTP))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { ignore(first.Close()) }()
	<-held

	second, err := net.Dial("tcp", server.Addr(maild.ListenerSMTP))
	if err != nil {
		t.Fatalf("dial the second connection: %v", err)
	}
	defer func() { ignore(second.Close()) }()
	if err := second.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("set a deadline: %v", err)
	}
	// Refused means closed immediately, not queued behind the first.
	if _, err := second.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("the second connection read %v, want EOF from an immediate close", err)
	}
}

// A session still running when the grace period expires is closed underneath
// its handler, and Shutdown says so rather than reporting a clean stop.
func TestShutdownClosesSessionsWhenTheGraceExpires(t *testing.T) {
	t.Parallel()

	reading := make(chan struct{})
	finished := make(chan error, 1)
	server := newTestServer(t, func(c *maild.Config) {
		c.SMTPAddr = "127.0.0.1:0"
		c.Services = fixedServices(maild.Services{
			API: okHandler(),
			SMTP: maild.SessionHandlerFunc(func(_ context.Context, conn net.Conn) {
				// No deadline of its own: this is the handler that would hang
				// forever if the Server did not close the connection.
				close(reading)
				_, err := io.Copy(io.Discard, conn)
				finished <- err
			}),
		})
	})
	mustStart(t, server)

	conn, err := net.Dial("tcp", server.Addr(maild.ListenerSMTP))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { ignore(conn.Close()) }()
	<-reading

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err = server.Shutdown(ctx)
	if err == nil || !strings.Contains(err.Error(), "sessions still open") {
		t.Fatalf("Shutdown error = %v, want it to report the open session", err)
	}
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("the handler was not released by the shutdown")
	}
	// The store is closed even though the shutdown was not clean, so there is
	// nothing left for the operator to clean up by hand.
	if _, err := server.Store().ListDomains(context.Background()); err == nil {
		t.Fatal("the store was left open by a forced shutdown")
	}
}

// Shutdown on a server that was never started still closes the store, because
// NewServer opened it.
func TestShutdownWithoutStartClosesTheStore(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, nil)
	if err := server.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if _, err := server.Store().ListDomains(context.Background()); err == nil {
		t.Fatal("the store was left open")
	}
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

// newTestServer builds a server on ephemeral ports with the management API
// enabled, then applies the test's own changes.
func newTestServer(t *testing.T, mutate func(*maild.Config)) *maild.Server {
	t.Helper()
	config := maild.Config{
		Hostname:   "mail.local.test",
		AdminToken: maild.Secret(adminToken),
		DataDir:    t.TempDir(),
		APIAddr:    "127.0.0.1:0",
		Services:   fixedServices(maild.Services{API: okHandler()}),
		Logger:     discardLogger(),
	}
	if mutate != nil {
		mutate(&config)
	}
	server, err := maild.NewServer(config)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		ignore(server.Shutdown(ctx))
	})
	return server
}

func mustStart(t *testing.T, server *maild.Server) {
	t.Helper()
	if err := server.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
}

// ignore and ignoreWrite consume an error a test has decided is not
// actionable — a close on a connection the server has already closed — because
// the repo's linter checks blank assignments as well as unchecked returns.
func ignore(error) {}

func ignoreWrite(int, error) {}

func fixedServices(services maild.Services) func(maild.Deps) (maild.Services, error) {
	return func(maild.Deps) (maild.Services, error) { return services, nil }
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		ignoreWrite(io.WriteString(w, "served"))
	})
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// sessionRecorder hands out a handler per listener that writes the listener's
// name, which is how a test tells which handler a port reached.
type sessionRecorder struct{}

func (*sessionRecorder) handler(name string) maild.SessionHandler {
	return maild.SessionHandlerFunc(func(_ context.Context, conn net.Conn) {
		ignoreWrite(conn.Write([]byte(name + "\n")))
	})
}

// greet dials and reads the one line the fake handler writes.
func greet(t *testing.T, addr string, useTLS bool) string {
	t.Helper()
	var conn net.Conn
	var err error
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	if useTLS {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // a self-signed certificate generated by this test
	} else {
		conn, err = dialer.Dial("tcp", addr)
	}
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	defer func() { ignore(conn.Close()) }()
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("set a deadline: %v", err)
	}
	buffer := make([]byte, 64)
	n, err := conn.Read(buffer)
	if err != nil {
		t.Fatalf("read from %s: %v", addr, err)
	}
	return strings.TrimSpace(string(buffer[:n]))
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the condition was not met in time")
}

// writeCertificate generates a self-signed certificate for 127.0.0.1. Ed25519
// keeps it fast, and nothing leaves the process.
func writeCertificate(t *testing.T) (certFile, keyFile string) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate a key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "mail.local.test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"mail.local.test", "localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		t.Fatalf("create a certificate: %v", err)
	}
	key, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatalf("marshal the key: %v", err)
	}
	dir := t.TempDir()
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")
	write := func(path, blockType string, bytes []byte) {
		if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: bytes}), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	write(certFile, "CERTIFICATE", der)
	write(keyFile, "PRIVATE KEY", key)
	return certFile, keyFile
}

// syncBuffer is a log sink a test can read while the server is still writing.
type syncBuffer struct {
	mu    sync.Mutex
	bytes []byte
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.bytes = append(b.bytes, p...)
	return len(p), nil
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.bytes)
}

// TestStartupLogsTellTheTruthAboutSTARTTLS. The startup log is the one place an
// operator reads "is this port protected?" without a packet capture, so a line
// that overstates a listener is worse than no line at all.
//
// The old form printed starttls = "not implicit TLS and a certificate exists",
// which said starttls=true for pop3 — a port that answers STLS with a refusal
// (pop3.go). An operator reading that concluded a POP3 password could be
// protected on port 110; it cannot be, and the log now says so.
func TestStartupLogsTellTheTruthAboutSTARTTLS(t *testing.T) {
	t.Parallel()

	certFile, keyFile := writeCertificate(t)
	logs := &syncBuffer{}
	server := newTestServer(t, func(c *maild.Config) {
		c.Logger = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
		c.TLSCertFile, c.TLSKeyFile = certFile, keyFile
		c.SMTPAddr = "127.0.0.1:0"
		c.SubmissionAddr = "127.0.0.1:0"
		c.SubmissionsAddr = "127.0.0.1:0"
		c.POP3Addr = "127.0.0.1:0"
		c.POP3SAddr = "127.0.0.1:0"
		c.Services = fixedServices(maild.Services{
			API:        okHandler(),
			SMTP:       maild.SessionHandlerFunc(func(context.Context, net.Conn) {}),
			Submission: maild.SessionHandlerFunc(func(context.Context, net.Conn) {}),
			POP3:       maild.SessionHandlerFunc(func(context.Context, net.Conn) {}),
		})
	})
	mustStart(t, server)

	// Every listener that bound has exactly one "bound" line, and its tls and
	// starttls fields are what the protocol on that port really does.
	for _, want := range []struct {
		listener string
		tls      bool
		startTLS bool
	}{
		{listener: maild.ListenerSMTP, tls: false, startTLS: true},
		{listener: maild.ListenerSubmission, tls: false, startTLS: true},
		{listener: maild.ListenerSubmissions, tls: true, startTLS: false},
		// pop3.go refuses STLS outright, so a certificate does not make this
		// port upgradeable.
		{listener: maild.ListenerPOP3, tls: false, startTLS: false},
		{listener: maild.ListenerPOP3S, tls: true, startTLS: false},
	} {
		line := boundLine(t, logs.String(), want.listener)
		if line["tls"] != want.tls {
			t.Errorf("%s reported tls=%v, want %v", want.listener, line["tls"], want.tls)
		}
		if line["starttls"] != want.startTLS {
			t.Errorf("%s reported starttls=%v, want %v", want.listener, line["starttls"], want.startTLS)
		}
		if line["addr"] != server.Addr(want.listener) {
			t.Errorf("%s reported addr=%v, want the bound %s", want.listener, line["addr"], server.Addr(want.listener))
		}
	}

	// A cleartext port that cannot upgrade is called out by name, because that
	// is the fact an operator has to act on.
	if warned := cleartextWarnings(t, logs.String()); !slices.Equal(warned, []string{maild.ListenerPOP3}) {
		t.Errorf("the cleartext warning named %v, want only %q", warned, maild.ListenerPOP3)
	}
}

// TestStartupLogsClaimNoSTARTTLSWithoutACertificate. Without a certificate not
// even SMTP can upgrade, and the log must not suggest otherwise.
func TestStartupLogsClaimNoSTARTTLSWithoutACertificate(t *testing.T) {
	t.Parallel()

	logs := &syncBuffer{}
	server := newTestServer(t, func(c *maild.Config) {
		c.Logger = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
		c.SMTPAddr = "127.0.0.1:0"
		c.POP3Addr = "127.0.0.1:0"
		c.Services = fixedServices(maild.Services{
			API:  okHandler(),
			SMTP: maild.SessionHandlerFunc(func(context.Context, net.Conn) {}),
			POP3: maild.SessionHandlerFunc(func(context.Context, net.Conn) {}),
		})
	})
	mustStart(t, server)

	for _, name := range []string{maild.ListenerSMTP, maild.ListenerPOP3} {
		if line := boundLine(t, logs.String(), name); line["starttls"] != false {
			t.Errorf("%s reported starttls=%v with no certificate loaded", name, line["starttls"])
		}
	}
	if warned := cleartextWarnings(t, logs.String()); !slices.Equal(warned, []string{maild.ListenerSMTP, maild.ListenerPOP3}) {
		t.Errorf("the cleartext warning named %v, want both cleartext listeners", warned)
	}
}

// boundLine finds the one "maild listener bound" record for a listener. It
// fails rather than returning nothing, because a missing line is exactly the
// failure these tests exist to catch.
func boundLine(t *testing.T, logs, listener string) map[string]any {
	t.Helper()
	var found []map[string]any
	for _, record := range logRecords(t, logs) {
		if record["msg"] == "maild listener bound" && record["listener"] == listener {
			found = append(found, record)
		}
	}
	if len(found) != 1 {
		t.Fatalf("listener %q has %d bound lines, want exactly 1\n%s", listener, len(found), logs)
	}
	return found[0]
}

// cleartextWarnings lists, in log order, the listeners warned about as
// cleartext-and-unupgradeable.
func cleartextWarnings(t *testing.T, logs string) []string {
	t.Helper()
	var names []string
	for _, record := range logRecords(t, logs) {
		message, isString := record["msg"].(string)
		if !isString || !strings.HasPrefix(message, "maild listener is cleartext") {
			continue
		}
		name, ok := record["listener"].(string)
		if !ok {
			t.Fatalf("a cleartext warning did not name a listener: %v", record)
		}
		names = append(names, name)
	}
	return names
}

// logRecords decodes the JSON log lines.
func logRecords(t *testing.T, logs string) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("the startup log line %q is not JSON: %v", line, err)
		}
		records = append(records, record)
	}
	return records
}
