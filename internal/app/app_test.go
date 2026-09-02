package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/castlemilk/dns/gen/go/dns/v1/dnsv1connect"
	"github.com/castlemilk/dns/internal/authoritative"
	"github.com/castlemilk/dns/internal/config"
	"github.com/castlemilk/dns/internal/control"
	"github.com/castlemilk/dns/internal/snapshot"
	"github.com/castlemilk/dns/internal/zone"
	"github.com/miekg/dns"
)

func TestServeListenersImmediateCancellation(t *testing.T) {
	httpListener := newBlockingListener("tcp")
	udpConn := newBlockingPacketConn("udp")
	tcpListener := newBlockingListener("tcp")

	httpServer := &http.Server{Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})}
	dnsHandler := dns.HandlerFunc(func(dns.ResponseWriter, *dns.Msg) {})
	udpServer := &dns.Server{PacketConn: udpConn, Handler: dnsHandler}
	tcpServer := &dns.Server{Listener: tcpListener, Handler: dnsHandler}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)
	go func() {
		done <- serveListeners(ctx, time.Second, httpServer, httpListener, udpServer, tcpServer, nil)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serveListeners after immediate cancellation: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("serveListeners hung after immediate cancellation")
	}
}

func TestServeHTTPImmediateCancellation(t *testing.T) {
	listener := newBlockingListener("tcp")
	server := &http.Server{Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)
	go func() {
		done <- serveHTTP(ctx, time.Second, server, listener)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serveHTTP after immediate cancellation: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("serveHTTP hung after immediate cancellation")
	}
}

type blockingListener struct {
	address testAddress
	closed  chan struct{}
	once    sync.Once
}

func newBlockingListener(network string) *blockingListener {
	return &blockingListener{address: testAddress(network), closed: make(chan struct{})}
}

func (l *blockingListener) Accept() (net.Conn, error) {
	<-l.closed
	return nil, net.ErrClosed
}

func (l *blockingListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *blockingListener) Addr() net.Addr { return l.address }

type blockingPacketConn struct {
	address testAddress
	closed  chan struct{}
	once    sync.Once
}

func newBlockingPacketConn(network string) *blockingPacketConn {
	return &blockingPacketConn{address: testAddress(network), closed: make(chan struct{})}
}

func (c *blockingPacketConn) ReadFrom([]byte) (int, net.Addr, error) {
	<-c.closed
	return 0, nil, net.ErrClosed
}

func (c *blockingPacketConn) WriteTo(payload []byte, _ net.Addr) (int, error) {
	return len(payload), nil
}

func (c *blockingPacketConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

func (c *blockingPacketConn) LocalAddr() net.Addr              { return c.address }
func (c *blockingPacketConn) SetDeadline(time.Time) error      { return nil }
func (c *blockingPacketConn) SetWriteDeadline(time.Time) error { return nil }

func (c *blockingPacketConn) SetReadDeadline(time.Time) error {
	return c.Close()
}

type testAddress string

func (a testAddress) Network() string { return string(a) }
func (a testAddress) String() string  { return string(a) }

func TestCORS(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		origin      string
		host        string
		configured  []string
		wantStatus  int
		wantAllowed string
		wantHandled bool
	}{
		{
			name: "request without origin", host: "dns.example.com",
			wantStatus: http.StatusNoContent, wantHandled: true,
		},
		{
			name: "configured development origin", origin: "http://localhost:3000", host: "127.0.0.1:8080",
			configured: []string{"http://localhost:3000"}, wantStatus: http.StatusNoContent,
			wantAllowed: "http://localhost:3000", wantHandled: true,
		},
		{
			name: "same origin behind gateway", origin: "https://dns.example.com", host: "dns.example.com",
			wantStatus: http.StatusNoContent, wantAllowed: "https://dns.example.com", wantHandled: true,
		},
		{
			name: "untrusted origin", origin: "https://attacker.example", host: "dns.example.com",
			wantStatus: http.StatusForbidden, wantHandled: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			handled := false
			handler := cors(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				handled = true
				writer.WriteHeader(http.StatusNoContent)
			}), tt.configured)
			request := httptest.NewRequest(http.MethodPost, "https://"+tt.host+"/dns.v1.DNSService/ListZones", nil)
			request.Host = tt.host
			if tt.origin != "" {
				request.Header.Set("Origin", tt.origin)
			}
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, request)

			if response.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", response.Code, tt.wantStatus)
			}
			if got := response.Header().Get("Access-Control-Allow-Origin"); got != tt.wantAllowed {
				t.Errorf("Access-Control-Allow-Origin = %q, want %q", got, tt.wantAllowed)
			}
			if got := response.Header().Values("Vary"); len(got) != 1 || got[0] != "Origin" {
				t.Errorf("Vary = %v, want [Origin]", got)
			}
			if handled != tt.wantHandled {
				t.Errorf("next handler called = %t, want %t", handled, tt.wantHandled)
			}
		})
	}
}

func TestBearerAuth(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		headers []string
		want    int
	}{
		{name: "missing", want: http.StatusUnauthorized},
		{name: "wrong", headers: []string{"Bearer wrong"}, want: http.StatusUnauthorized},
		{name: "wrong scheme", headers: []string{"Basic secret"}, want: http.StatusUnauthorized},
		{name: "multiple", headers: []string{"Bearer secret", "Bearer secret"}, want: http.StatusUnauthorized},
		{name: "correct", headers: []string{"Bearer secret"}, want: http.StatusNoContent},
		{name: "case insensitive scheme", headers: []string{"bearer secret"}, want: http.StatusNoContent},
	}
	handler := bearerAuth("secret", http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	}))
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			request := httptest.NewRequest(http.MethodGet, "http://service.test/protected", nil)
			for _, value := range tt.headers {
				request.Header.Add("Authorization", value)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != tt.want {
				t.Fatalf("status = %d, want %d", response.Code, tt.want)
			}
			if tt.want == http.StatusUnauthorized && response.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Fatalf("WWW-Authenticate = %q", response.Header().Get("WWW-Authenticate"))
			}
		})
	}

	t.Run("empty token leaves local handler open", func(t *testing.T) {
		t.Parallel()
		handler := bearerAuth("", http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(http.StatusNoContent)
		}))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://service.test/local", nil))
		if response.Code != http.StatusNoContent {
			t.Fatalf("status = %d", response.Code)
		}
	})

	t.Run("constant length digest comparison", func(t *testing.T) {
		t.Parallel()
		expected := sha256.Sum256([]byte("secret"))
		request := httptest.NewRequest(http.MethodGet, "http://service.test/protected", nil)
		request.Header.Set("Authorization", "Bearer secret")
		if !validBearer(request, expected) {
			t.Fatal("valid bearer rejected")
		}
	})
}

func TestHTTPRoutesKeepHealthPublicAndTokensSeparate(t *testing.T) {
	t.Parallel()
	feed := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	})
	handler := newHTTPHandler(
		nil,
		feed,
		nil,
		nil,
		"api-secret",
		"snapshot-secret",
		1024,
		func() (bool, string) { return false, "no valid snapshot" },
	)

	tests := []struct {
		name  string
		path  string
		token string
		want  int
		body  string
	}{
		{name: "health", path: "/healthz", want: http.StatusOK},
		{name: "readiness", path: "/readyz", want: http.StatusServiceUnavailable, body: "no valid snapshot"},
		{name: "snapshot missing token", path: "/internal/v1/snapshot", want: http.StatusUnauthorized},
		{name: "snapshot API token rejected", path: "/internal/v1/snapshot", token: "api-secret", want: http.StatusUnauthorized},
		{name: "snapshot token accepted", path: "/internal/v1/snapshot", token: "snapshot-secret", want: http.StatusNoContent},
		{name: "control RPC absent", path: "/dns.v1.DNSService/ListZones", want: http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			request := httptest.NewRequest(http.MethodGet, "http://authority.test"+tt.path, nil)
			if tt.token != "" {
				request.Header.Set("Authorization", "Bearer "+tt.token)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != tt.want {
				t.Fatalf("status = %d, want %d; body = %q", response.Code, tt.want, response.Body.String())
			}
			if tt.body != "" && !strings.Contains(response.Body.String(), tt.body) {
				t.Fatalf("body = %q, want %q", response.Body.String(), tt.body)
			}
		})
	}
}

func TestControlAPIUsesOnlyAPIToken(t *testing.T) {
	t.Parallel()
	store, err := zone.Open(filepath.Join(t.TempDir(), "zones.db"), []string{"ns1.dns.test"})
	if err != nil {
		t.Fatalf("zone.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	dnsServer := authoritative.New(nil, authoritative.DefaultMaxUDPSize)
	controlHandler := control.NewHandler(store, dnsServer, nil, time.Now())
	if err := controlHandler.Reload(context.Background()); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	handler := newHTTPHandler(controlHandler, nil, nil, nil, "api-secret", "snapshot-secret", 1024, nil)

	tests := []struct {
		name  string
		token string
		want  int
	}{
		{name: "missing", want: http.StatusUnauthorized},
		{name: "snapshot token rejected", token: "snapshot-secret", want: http.StatusUnauthorized},
		{name: "API token accepted", token: "api-secret", want: http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			request := httptest.NewRequest(
				http.MethodPost,
				"http://control.test"+dnsv1connect.DNSServiceListZonesProcedure,
				strings.NewReader(`{}`),
			)
			request.Header.Set("Content-Type", "application/json")
			if tt.token != "" {
				request.Header.Set("Authorization", "Bearer "+tt.token)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != tt.want {
				t.Fatalf("status = %d, want %d; body = %q", response.Code, tt.want, response.Body.String())
			}
		})
	}
}

func TestControlSnapshotToAuthorityIntegration(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	store, err := zone.Open(filepath.Join(t.TempDir(), "zones.db"), []string{"ns1.dns.test"}, zone.WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("zone.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	if _, err := store.Create(context.Background(), "replicated.test"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	feed := snapshot.NewFeedHandler(store, nil, 1<<20, false)
	controlHTTP := newHTTPHandler(nil, feed, nil, nil, "api-secret", "snapshot-secret", 1<<20, nil)
	client := &http.Client{Transport: handlerRoundTripper{handler: controlHTTP}}

	authority := authoritative.New(nil, authoritative.DefaultMaxUDPSize)
	status := snapshot.NewStatus(time.Minute)
	consumer := snapshot.NewConsumer(
		authority,
		status,
		client,
		"http://control.test"+snapshot.Path,
		"snapshot-secret",
		filepath.Join(t.TempDir(), "snapshot.json"),
		1<<20,
		false,
		nil,
	)
	if err := consumer.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if zones, records := authority.Counts(); zones != 1 || records != 2 {
		t.Fatalf("authority counts = %d/%d, want 1/2", zones, records)
	}
	if ready, reason := status.Ready(); !ready {
		t.Fatalf("authority not ready after replication: %s", reason)
	}
}

func TestRequestBodyLimit(t *testing.T) {
	t.Parallel()
	handler := limitRequestBody(4, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, err := io.ReadAll(request.Body)
		if err != nil {
			http.Error(writer, err.Error(), http.StatusRequestEntityTooLarge)
			return
		}
		writer.WriteHeader(http.StatusNoContent)
	}))

	tests := []struct {
		name          string
		body          string
		contentLength int64
		want          int
	}{
		{name: "at limit", body: "1234", contentLength: 4, want: http.StatusNoContent},
		{name: "declared oversized", body: "12345", contentLength: 5, want: http.StatusRequestEntityTooLarge},
		{name: "streamed oversized", body: "12345", contentLength: -1, want: http.StatusRequestEntityTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			request := httptest.NewRequest(http.MethodPost, "http://service.test/api", strings.NewReader(tt.body))
			request.ContentLength = tt.contentLength
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != tt.want {
				t.Fatalf("status = %d, want %d", response.Code, tt.want)
			}
		})
	}
}

func TestSnapshotHTTPClientIsBoundedAndDoesNotFollowRedirects(t *testing.T) {
	t.Parallel()
	client := newSnapshotHTTPClient(3 * time.Second)
	if client.Timeout != 3*time.Second {
		t.Fatalf("Timeout = %s", client.Timeout)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T", client.Transport)
	}
	if transport.ResponseHeaderTimeout != 3*time.Second || transport.MaxResponseHeaderBytes != 32<<10 {
		t.Fatalf("transport bounds = timeout:%s headers:%d", transport.ResponseHeaderTimeout, transport.MaxResponseHeaderBytes)
	}
	if err := client.CheckRedirect(nil, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("CheckRedirect error = %v", err)
	}
}

func TestRunWriterRejectsSnapshotLimitBeforeListening(t *testing.T) {
	t.Parallel()
	err := Run(context.Background(), config.Config{
		Role: config.RoleAll, DataPath: filepath.Join(t.TempDir(), "zones.db"),
		Nameservers: []string{"ns1.provider.example"}, SnapshotMaxBodyBytes: 1,
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "aggregate snapshot") {
		t.Fatalf("Run error = %v", err)
	}
}

type handlerRoundTripper struct {
	handler http.Handler
}

func (transport handlerRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	response := httptest.NewRecorder()
	transport.handler.ServeHTTP(response, request)
	result := response.Result()
	result.Request = request
	return result, nil
}
