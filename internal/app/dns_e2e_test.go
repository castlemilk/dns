package app_test

// The DNS journey, over real sockets, in one test.
//
// Nothing here fakes anything this repository owns. A real bbolt-backed
// zone.Store takes the record through the same normalisation the Connect API
// calls; a real snapshot.FeedHandler serves the document over real HTTP with the
// real bearer check; a real snapshot.Consumer fetches and compiles it; a real
// authoritative.Server answers on a real ephemeral UDP and TCP port; and a real
// dns.Client asks it a question. The only thing constructed by hand is the
// loopback address, because a test must not bind :53.
//
// It exists because the layers agreed with each other while all being wrong: a
// TXT value quoted with Go escapes was stored, returned by the API and served
// consistently, and differed only from what the customer typed. A test that
// stops at any one layer cannot see that. This one compares the bytes a resolver
// receives against the bytes that went in.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/castlemilk/dns/internal/authoritative"
	"github.com/castlemilk/dns/internal/snapshot"
	"github.com/castlemilk/dns/internal/telemetry"
	"github.com/castlemilk/dns/internal/zone"
)

const e2eNameserver = "ns1.deephost.test."

// TestDNSJourneyServesExactlyWhatWasStored walks a record from the store to a
// resolver's answer and asserts the bytes never change.
func TestDNSJourneyServesExactlyWhatWasStored(t *testing.T) {
	t.Parallel()

	// Values a customer plausibly pastes. The first is the one that used to
	// break: a DKIM key that wrapped across lines somewhere else and carries the
	// wrap with it.
	values := map[string]string{
		"dkim-with-a-tab":   "v=DKIM1; k=rsa; p=MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQC1\tvJ8m2Q==",
		"spf":               "v=spf1 include:_spf.example.com ~all",
		"literal-backslash": `C:\path\to\thing`,
		"embedded-quote":    `say "hello" twice`,
		"non-ascii":         "café",
	}

	stack := startDNSStack(t, func(t *testing.T, store *zone.Store) string {
		t.Helper()
		created, err := store.Create(context.Background(), "journey.test")
		if err != nil {
			t.Fatalf("create zone: %v", err)
		}
		for name, value := range values {
			record, err := zone.NormalizeRecord(created.Name, name, zone.TypeTXT, 300, value)
			if err != nil {
				t.Fatalf("normalize %s: %v", name, err)
			}
			if _, err := store.CreateRecord(context.Background(), created.ID, record); err != nil {
				t.Fatalf("create record %s: %v", name, err)
			}
		}
		return created.Name
	})

	for name, want := range values {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for _, network := range []string{"udp", "tcp"} {
				t.Run(network, func(t *testing.T) {
					answer := stack.ask(t, name+".journey.test.", dns.TypeTXT, network)
					if len(answer) != 1 {
						t.Fatalf("got %d answers, want 1", len(answer))
					}
					if _, ok := answer[0].(*dns.TXT); !ok {
						t.Fatalf("answer is %T, want *dns.TXT", answer[0])
					}
					got := txtWireBytes(t, answer[0])
					if !bytes.Equal(got, []byte(want)) {
						t.Errorf("value changed between the store and the wire:\n  stored: %q\n  served: %q", want, string(got))
					}
				})
			}
		})
	}
}

// TestDNSJourneyAnswersFromTheZoneItWasGiven covers the ordinary path alongside
// the fidelity one, so a break in the plumbing is not mistaken for a quoting bug.
func TestDNSJourneyAnswersFromTheZoneItWasGiven(t *testing.T) {
	t.Parallel()

	stack := startDNSStack(t, func(t *testing.T, store *zone.Store) string {
		t.Helper()
		created, err := store.Create(context.Background(), "plumbing.test")
		if err != nil {
			t.Fatalf("create zone: %v", err)
		}
		record, err := zone.NormalizeRecord(created.Name, "www", zone.TypeA, 300, "192.0.2.10")
		if err != nil {
			t.Fatalf("normalize: %v", err)
		}
		if _, err := store.CreateRecord(context.Background(), created.ID, record); err != nil {
			t.Fatalf("create record: %v", err)
		}
		return created.Name
	})

	answer := stack.ask(t, "www.plumbing.test.", dns.TypeA, "udp")
	if len(answer) != 1 {
		t.Fatalf("got %d answers, want 1", len(answer))
	}
	a, ok := answer[0].(*dns.A)
	if !ok {
		t.Fatalf("answer is %T, want *dns.A", answer[0])
	}
	if a.A.String() != "192.0.2.10" {
		t.Errorf("A = %s, want 192.0.2.10", a.A)
	}

	// The delegation the zone publishes must be the configured nameserver, which
	// is what a parent zone is told to point at.
	ns := stack.ask(t, "plumbing.test.", dns.TypeNS, "udp")
	if len(ns) == 0 {
		t.Fatal("zone served no NS records")
	}
	nsRecord, ok := ns[0].(*dns.NS)
	if !ok {
		t.Fatalf("NS answer is %T, want *dns.NS", ns[0])
	}
	if nsRecord.Ns != e2eNameserver {
		t.Errorf("NS = %s, want %s", nsRecord.Ns, e2eNameserver)
	}
}

// TestDNSJourneySurvivesAnEmptyControlPlane is the availability case: a control
// plane whose store came back blank publishes a valid, current, correctly
// checksummed document containing no zones. Applying it would answer REFUSED for
// every delegated domain on every replica within one poll, with the apply
// recorded as a success and every alert quiet.
//
// The assertion is deliberately made against a real query rather than the
// consumer's return value: what matters is that resolvers keep getting answers.
func TestDNSJourneySurvivesAnEmptyControlPlane(t *testing.T) {
	t.Parallel()
	metrics := telemetry.Disabled()
	now := time.Now().UTC()

	served, _, err := snapshot.Build([]zone.Zone{e2eZone(t, "survives.test", now)}, now, false)
	if err != nil {
		t.Fatalf("build populated snapshot: %v", err)
	}
	// What a control plane with an empty store serves: newer, valid, and empty.
	empty, _, err := snapshot.Build(nil, now.Add(time.Second), false)
	if err != nil {
		t.Fatalf("build empty snapshot: %v", err)
	}

	body := served
	feedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write(body); err != nil {
			t.Errorf("write snapshot body: %v", err)
		}
	}))
	t.Cleanup(feedServer.Close)

	server := authoritative.New(e2eLogger(), authoritative.DefaultMaxUDPSize, metrics)
	status := snapshot.NewStatus(time.Minute, metrics)
	consumer := snapshot.NewConsumer(
		server, status, feedServer.Client(), feedServer.URL, "",
		filepath.Join(t.TempDir(), "cache.json"), 1<<20, false, e2eLogger(), metrics,
	)
	if err := consumer.Fetch(context.Background()); err != nil {
		t.Fatalf("initial fetch: %v", err)
	}
	stack := serveAuthority(t, server)

	if got := stack.ask(t, "survives.test.", dns.TypeSOA, "udp"); len(got) == 0 {
		t.Fatal("zone was not being served before the empty snapshot")
	}

	body = empty
	if err := consumer.Fetch(context.Background()); err == nil {
		t.Fatal("consumer accepted a snapshot that would empty a serving authority")
	}

	// The whole point: the zone still answers.
	if got := stack.ask(t, "survives.test.", dns.TypeSOA, "udp"); len(got) == 0 {
		t.Error("authority stopped answering after refusing the empty snapshot")
	}
}

// e2eZone builds a minimal valid zone without going through a store, for tests
// that need to control the snapshot document directly.
func e2eZone(t *testing.T, name string, now time.Time) zone.Zone {
	t.Helper()
	store, err := zone.Open(filepath.Join(t.TempDir(), "seed.db"), []string{e2eNameserver})
	if err != nil {
		t.Fatalf("open seed store: %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Errorf("close seed store: %v", err)
		}
	}()
	if _, err := store.Create(context.Background(), name); err != nil {
		t.Fatalf("create seed zone: %v", err)
	}
	values, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("list seed zones: %v", err)
	}
	return values[0]
}

// dnsStack is the running system under test.
type dnsStack struct {
	addr string
}

// ask sends a real query over a real socket and returns the answer section.
func (s dnsStack) ask(t *testing.T, name string, qtype uint16, network string) []dns.RR {
	t.Helper()
	question := new(dns.Msg)
	question.SetQuestion(name, qtype)
	client := &dns.Client{Net: network, Timeout: 5 * time.Second}
	response, _, err := client.Exchange(question, s.addr)
	if err != nil {
		t.Fatalf("query %s %s over %s: %v", name, dns.TypeToString[qtype], network, err)
	}
	if response.Rcode != dns.RcodeSuccess {
		t.Fatalf("query %s returned %s", name, dns.RcodeToString[response.Rcode])
	}
	if !response.Authoritative {
		t.Errorf("answer for %s is not authoritative", name)
	}
	return response.Answer
}

// startDNSStack wires the real components together and returns once the
// authority is actually answering, so nothing downstream has to sleep.
func startDNSStack(t *testing.T, seed func(*testing.T, *zone.Store) string) dnsStack {
	t.Helper()
	metrics := telemetry.Disabled()

	store, err := zone.Open(filepath.Join(t.TempDir(), "zones.db"), []string{e2eNameserver})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	zoneName := seed(t, store)

	// The real feed, behind the real bearer check the control plane applies.
	const token = "e2e-snapshot-token-that-is-long-enough"
	feed := snapshot.NewFeedHandler(store, e2eLogger(), 1<<20, false, metrics)
	feedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		feed.ServeHTTP(w, r)
	}))
	t.Cleanup(feedServer.Close)

	server := authoritative.New(e2eLogger(), authoritative.DefaultMaxUDPSize, metrics)
	status := snapshot.NewStatus(time.Minute, metrics)
	consumer := snapshot.NewConsumer(
		server, status, feedServer.Client(), feedServer.URL+snapshot.Path, token,
		filepath.Join(t.TempDir(), "cache.json"), 1<<20, false, e2eLogger(), metrics,
	)
	if err := consumer.Fetch(context.Background()); err != nil {
		t.Fatalf("fetch snapshot: %v", err)
	}

	stack := serveAuthority(t, server)
	t.Logf("serving %s on %s", zoneName, stack.addr)
	return stack
}

// serveAuthority binds the same UDP and TCP listeners app.go builds, on ports
// the kernel picks so tests run concurrently, and returns once both are actually
// accepting — started, not slept for.
func serveAuthority(t *testing.T, server *authoritative.Server) dnsStack {
	t.Helper()
	udpConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	addr := udpConn.LocalAddr().String()
	tcpListener, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("listen tcp on %s: %v", addr, err)
	}

	udpReady, tcpReady := make(chan struct{}), make(chan struct{})
	udpServer := &dns.Server{PacketConn: udpConn, Handler: server, NotifyStartedFunc: func() { close(udpReady) }}
	tcpServer := &dns.Server{Listener: tcpListener, Handler: server, NotifyStartedFunc: func() { close(tcpReady) }}
	for _, each := range []*dns.Server{udpServer, tcpServer} {
		go func(each *dns.Server) {
			if err := each.ActivateAndServe(); err != nil && !errors.Is(err, net.ErrClosed) {
				t.Errorf("serve: %v", err)
			}
		}(each)
	}
	<-udpReady
	<-tcpReady
	t.Cleanup(func() {
		if err := udpServer.Shutdown(); err != nil {
			t.Errorf("shutdown udp: %v", err)
		}
		if err := tcpServer.Shutdown(); err != nil {
			t.Errorf("shutdown tcp: %v", err)
		}
	})
	return dnsStack{addr: addr}
}

// e2eLogger keeps the components' own logging out of the test output while
// still passing them a real logger rather than nil.
func e2eLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// txtWireBytes returns the bytes a resolver actually receives for a TXT record.
//
// It cannot read dns.TXT.Txt directly: miekg re-escapes non-printable bytes into
// presentation form when it unpacks, so that field shows a real tab and the four
// characters of "\\009" identically — which is precisely the distinction this
// test exists to make. Packing the answer back to wire and reading the
// character-strings out of the RDATA gives the bytes without that ambiguity.
func txtWireBytes(t *testing.T, rr dns.RR) []byte {
	t.Helper()
	buf := make([]byte, 4096)
	n, err := dns.PackRR(rr, buf, 0, nil, false)
	if err != nil {
		t.Fatalf("PackRR: %v", err)
	}
	wire := buf[:n]

	offset := 0
	for offset < len(wire) && wire[offset] != 0 {
		offset += int(wire[offset]) + 1
	}
	offset++
	offset += 2 + 2 + 4
	if offset+2 > len(wire) {
		t.Fatalf("packed answer is too short to hold RDLENGTH")
	}
	rdLength := int(wire[offset])<<8 | int(wire[offset+1])
	offset += 2
	rdata := wire[offset : offset+rdLength]

	var value []byte
	for cursor := 0; cursor < len(rdata); {
		size := int(rdata[cursor])
		cursor++
		value = append(value, rdata[cursor:cursor+size]...)
		cursor += size
	}
	return value
}
