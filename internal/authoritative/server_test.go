package authoritative_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/castlemilk/dns/internal/authoritative"
	"github.com/castlemilk/dns/internal/telemetry"
	"github.com/castlemilk/dns/internal/zone"
	"github.com/miekg/dns"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestServeDNSAuthoritativeSemantics(t *testing.T) {
	t.Parallel()

	server := newFixtureServer(t)
	tests := []struct {
		name      string
		qname     string
		qtype     uint16
		wantRcode int
		wantAA    bool
		check     func(*testing.T, *dns.Msg)
	}{
		{
			name: "exact RRset", qname: "WWW.Example.Test", qtype: dns.TypeA,
			wantRcode: dns.RcodeSuccess, wantAA: true,
			check: func(t *testing.T, response *dns.Msg) {
				assertRRTypes(t, response.Answer, dns.TypeA, dns.TypeA)
				assertAValues(t, response.Answer, "192.0.2.10", "192.0.2.11")
				for _, rr := range response.Answer {
					if rr.Header().Name != "www.example.test." {
						t.Errorf("answer owner = %q, want www.example.test.", rr.Header().Name)
					}
				}
			},
		},
		{
			name: "existing owner NODATA", qname: "txtonly.example.test.", qtype: dns.TypeA,
			wantRcode: dns.RcodeSuccess, wantAA: true,
			check: func(t *testing.T, response *dns.Msg) {
				if len(response.Answer) != 0 {
					t.Errorf("NODATA answer count = %d, want 0", len(response.Answer))
				}
				assertNegativeSOA(t, response)
			},
		},
		{
			name: "NXDOMAIN", qname: "missing.block.example.test.", qtype: dns.TypeA,
			wantRcode: dns.RcodeNameError, wantAA: true,
			check: func(t *testing.T, response *dns.Msg) {
				if len(response.Answer) != 0 {
					t.Errorf("NXDOMAIN answer count = %d, want 0", len(response.Answer))
				}
				assertNegativeSOA(t, response)
			},
		},
		{
			name: "outside served zones is REFUSED", qname: "www.outside.test.", qtype: dns.TypeA,
			wantRcode: dns.RcodeRefused, wantAA: false,
			check: func(t *testing.T, response *dns.Msg) {
				if len(response.Answer) != 0 || len(response.Ns) != 0 {
					t.Errorf("REFUSED sections = answer:%d authority:%d, want both empty", len(response.Answer), len(response.Ns))
				}
			},
		},
		{
			name: "wildcard synthesis", qname: "random.example.test.", qtype: dns.TypeA,
			wantRcode: dns.RcodeSuccess, wantAA: true,
			check: func(t *testing.T, response *dns.Msg) {
				assertRRTypes(t, response.Answer, dns.TypeA)
				if len(response.Answer) != 1 {
					return
				}
				assertAValues(t, response.Answer, "192.0.2.99")
				if response.Answer[0].Header().Name != "random.example.test." {
					t.Errorf("wildcard answer owner = %q, want random.example.test.", response.Answer[0].Header().Name)
				}
			},
		},
		{
			name: "empty non-terminal blocks wildcard", qname: "ent.example.test.", qtype: dns.TypeA,
			wantRcode: dns.RcodeSuccess, wantAA: true,
			check: func(t *testing.T, response *dns.Msg) {
				if len(response.Answer) != 0 {
					t.Errorf("empty non-terminal answer count = %d, want 0", len(response.Answer))
				}
				assertNegativeSOA(t, response)
			},
		},
		{
			name: "local CNAME target is included", qname: "alias.example.test.", qtype: dns.TypeA,
			wantRcode: dns.RcodeSuccess, wantAA: true,
			check: func(t *testing.T, response *dns.Msg) {
				assertRRTypes(t, response.Answer, dns.TypeCNAME, dns.TypeA, dns.TypeA)
				if len(response.Answer) != 3 {
					return
				}
				cname, ok := response.Answer[0].(*dns.CNAME)
				if !ok {
					t.Fatalf("first answer has type %T, want *dns.CNAME", response.Answer[0])
				}
				if cname.Hdr.Name != "alias.example.test." || cname.Target != "www.example.test." {
					t.Errorf("CNAME = %s -> %s, want alias.example.test. -> www.example.test.", cname.Hdr.Name, cname.Target)
				}
				assertAValues(t, response.Answer[1:], "192.0.2.10", "192.0.2.11")
			},
		},
		{
			name: "delegation referral with glue", qname: "host.child.example.test.", qtype: dns.TypeA,
			wantRcode: dns.RcodeSuccess, wantAA: false,
			check: func(t *testing.T, response *dns.Msg) {
				if len(response.Answer) != 0 {
					t.Errorf("referral answer count = %d, want 0", len(response.Answer))
				}
				assertRRTypes(t, response.Ns, dns.TypeNS)
				if len(response.Ns) != 1 {
					return
				}
				ns, ok := response.Ns[0].(*dns.NS)
				if !ok {
					t.Fatalf("authority record has type %T, want *dns.NS", response.Ns[0])
				}
				if ns.Hdr.Name != "child.example.test." || ns.Ns != "ns.child.example.test." {
					t.Errorf("referral NS = %s -> %s", ns.Hdr.Name, ns.Ns)
				}
				assertRRTypes(t, response.Extra, dns.TypeA, dns.TypeAAAA)
				if len(response.Extra) != 2 {
					return
				}
				if response.Extra[0].Header().Name != "ns.child.example.test." || response.Extra[1].Header().Name != "ns.child.example.test." {
					t.Errorf("glue owners = %q, %q", response.Extra[0].Header().Name, response.Extra[1].Header().Name)
				}
			},
		},
		{
			name: "minimal ANY", qname: "www.example.test.", qtype: dns.TypeANY,
			wantRcode: dns.RcodeSuccess, wantAA: true,
			check: func(t *testing.T, response *dns.Msg) {
				assertRRTypes(t, response.Answer, dns.TypeHINFO)
				if len(response.Answer) != 1 {
					return
				}
				hinfo, ok := response.Answer[0].(*dns.HINFO)
				if !ok {
					t.Fatalf("ANY answer has type %T, want *dns.HINFO", response.Answer[0])
				}
				if hinfo.Cpu != "RFC8482" || hinfo.Hdr.Ttl != 60 {
					t.Errorf("minimal ANY HINFO = %#v", hinfo)
				}
			},
		},
		{
			name: "CNAME expansion stops at delegation", qname: "cutalias.example.test.", qtype: dns.TypeA,
			wantRcode: dns.RcodeSuccess, wantAA: true,
			check: func(t *testing.T, response *dns.Msg) {
				assertRRTypes(t, response.Answer, dns.TypeCNAME)
				if len(response.Answer) != 1 {
					t.Errorf("answer count = %d, want only the CNAME", len(response.Answer))
				}
			},
		},
		{
			name: "ANY at nonexistent name is NXDOMAIN", qname: "missing.block.example.test.", qtype: dns.TypeANY,
			wantRcode: dns.RcodeNameError, wantAA: true,
			check: func(t *testing.T, response *dns.Msg) {
				if len(response.Answer) != 0 {
					t.Errorf("NXDOMAIN ANY answer count = %d, want 0", len(response.Answer))
				}
				assertNegativeSOA(t, response)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			request := new(dns.Msg)
			request.SetQuestion(tt.qname, tt.qtype)
			request.RecursionDesired = true
			response := exchangeUDP(t, server, request)

			if response.Rcode != tt.wantRcode {
				t.Errorf("Rcode = %s, want %s", dns.RcodeToString[response.Rcode], dns.RcodeToString[tt.wantRcode])
			}
			if response.Authoritative != tt.wantAA {
				t.Errorf("AA = %t, want %t", response.Authoritative, tt.wantAA)
			}
			if response.RecursionAvailable {
				t.Error("RA = true, want false")
			}
			if !response.RecursionDesired {
				t.Error("RD was not copied from the request")
			}
			tt.check(t, response)
		})
	}
}

func TestServeDNSProtocolErrorsAndEDNS(t *testing.T) {
	t.Parallel()

	server := newFixtureServer(t)
	tests := []struct {
		name      string
		request   *dns.Msg
		wantRcode int
	}{
		{
			name:      "unsupported opcode",
			request:   &dns.Msg{MsgHdr: dns.MsgHdr{Id: 1, Opcode: dns.OpcodeUpdate}},
			wantRcode: dns.RcodeNotImplemented,
		},
		{
			name:      "no question",
			request:   &dns.Msg{MsgHdr: dns.MsgHdr{Id: 2, Opcode: dns.OpcodeQuery}},
			wantRcode: dns.RcodeFormatError,
		},
		{
			name: "unsupported class",
			request: &dns.Msg{
				MsgHdr:   dns.MsgHdr{Id: 3, Opcode: dns.OpcodeQuery},
				Question: []dns.Question{{Name: "www.example.test.", Qtype: dns.TypeA, Qclass: dns.ClassCHAOS}},
			},
			wantRcode: dns.RcodeRefused,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			response := exchangeUDP(t, server, tt.request)
			if response.Rcode != tt.wantRcode {
				t.Errorf("Rcode = %s, want %s", dns.RcodeToString[response.Rcode], dns.RcodeToString[tt.wantRcode])
			}
			if response.Authoritative || response.RecursionAvailable {
				t.Errorf("error flags AA=%t RA=%t, want both false", response.Authoritative, response.RecursionAvailable)
			}
		})
	}

	t.Run("EDNS response advertises server cap", func(t *testing.T) {
		request := new(dns.Msg)
		request.SetQuestion("www.example.test.", dns.TypeA)
		request.SetEdns0(4096, true)
		response := exchangeUDP(t, server, request)
		opt := response.IsEdns0()
		if opt == nil {
			t.Fatal("response has no OPT record")
		}
		if got, want := opt.UDPSize(), uint16(authoritative.DefaultMaxUDPSize); got != want {
			t.Errorf("advertised UDP size = %d, want %d", got, want)
		}
		if !opt.Do() {
			t.Error("response DO = false, want request DO bit echoed")
		}
	})

	t.Run("unsupported EDNS version", func(t *testing.T) {
		request := new(dns.Msg)
		request.SetQuestion("www.example.test.", dns.TypeA)
		request.SetEdns0(1232, false)
		request.IsEdns0().SetVersion(1)
		response := exchangeUDP(t, server, request)
		if response.Rcode != dns.RcodeBadVers {
			t.Errorf("Rcode = %s, want BADVERS", dns.RcodeToString[response.Rcode])
		}
		if opt := response.IsEdns0(); opt == nil || opt.Version() != 0 {
			t.Errorf("response OPT = %#v, want EDNS version 0", opt)
		}
	})
}

func TestServeDNSUDPTruncation(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_801_000_000, 0).UTC()
	store := openAuthoritativeStore(t, &now, "ns1.large.test")
	ctx := context.Background()
	value, err := store.Create(ctx, "large.test")
	if err != nil {
		t.Fatalf("Create zone: %v", err)
	}
	const recordCount = 64
	for index := 1; index <= recordCount; index++ {
		record := normalizeAuthoritativeRecord(t, value.Name, "bulk", zone.TypeA, 60, fmt.Sprintf("198.51.100.%d", index))
		value, err = store.CreateRecord(ctx, value.ID, record)
		if err != nil {
			t.Fatalf("CreateRecord(%d): %v", index, err)
		}
	}

	server := authoritative.New(slog.New(slog.NewTextHandler(io.Discard, nil)), dns.MinMsgSize)
	if err := server.Replace([]zone.Zone{value}); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	request := new(dns.Msg)
	request.SetQuestion("bulk.large.test.", dns.TypeA)
	response := exchangeUDP(t, server, request)
	if !response.Truncated {
		t.Fatal("TC = false, want true for oversized UDP response")
	}
	if len(response.Answer) >= recordCount {
		t.Errorf("truncated answer count = %d, want less than %d", len(response.Answer), recordCount)
	}
	packed, err := response.Pack()
	if err != nil {
		t.Fatalf("Pack response: %v", err)
	}
	if len(packed) > dns.MinMsgSize {
		t.Errorf("packed response size = %d, want <= %d", len(packed), dns.MinMsgSize)
	}
}

func TestCompileThenReplacePublishesOnlyValidatedSnapshot(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_801_100_000, 0).UTC()
	store := openAuthoritativeStore(t, &now, "ns1.dns.test")
	value, err := store.Create(context.Background(), "example.test")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	compiled, err := authoritative.Compile([]zone.Zone{value})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	server := authoritative.New(nil, authoritative.DefaultMaxUDPSize)
	server.ReplaceCompiled(compiled)
	if zones, records := server.Counts(); zones != 1 || records != 2 {
		t.Fatalf("counts = %d/%d, want 1/2", zones, records)
	}

	invalid := value
	invalid.Records = slices.DeleteFunc(slices.Clone(invalid.Records), func(record zone.Record) bool {
		return record.Type == zone.TypeSOA
	})
	if _, err := authoritative.Compile([]zone.Zone{invalid}); err == nil {
		t.Fatal("Compile accepted a zone without SOA")
	}
	if zones, records := server.Counts(); zones != 1 || records != 2 {
		t.Fatalf("invalid compile changed counts to %d/%d", zones, records)
	}
	server.ReplaceCompiled(nil)
	if zones, _ := server.Counts(); zones != 1 {
		t.Fatal("nil compiled snapshot changed the server")
	}
}

func TestDNSMetricsDescribeProtocolNotQuestionName(t *testing.T) {
	t.Parallel()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() {
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown meter provider: %v", err)
		}
	})
	metrics, err := telemetry.NewMetrics(provider.Meter("authority-test"))
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	server := newFixtureServer(t, metrics)
	request := new(dns.Msg)
	request.SetQuestion("missing.block.example.test.", dns.TypeA)
	writer := &captureResponseWriter{network: "udp"}
	server.ServeDNS(writer, request)

	var collected metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &collected); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	metricValue := authorityMetric(collected, "deephost.dns.queries")
	sum, ok := metricValue.Data.(metricdata.Sum[int64])
	if !ok || len(sum.DataPoints) != 1 {
		t.Fatalf("DNS query metric = %#v", metricValue.Data)
	}
	assertAuthorityAttribute(t, sum.DataPoints[0].Attributes, "network.transport", "udp")
	assertAuthorityAttribute(t, sum.DataPoints[0].Attributes, "dns.question.type", "A")
	assertAuthorityAttribute(t, sum.DataPoints[0].Attributes, "dns.response.code", "NXDOMAIN")
	if strings.Contains(fmt.Sprintf("%#v", collected), "missing.block.example.test") {
		t.Fatal("DNS qname leaked into metric data")
	}
}

func TestDNSMetricsLabelUnsupportedEDNSVersionAsBADVERS(t *testing.T) {
	t.Parallel()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() {
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown meter provider: %v", err)
		}
	})
	metrics, err := telemetry.NewMetrics(provider.Meter("authority-test"))
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	server := newFixtureServer(t, metrics)
	request := new(dns.Msg)
	request.SetQuestion("www.example.test.", dns.TypeA)
	request.SetEdns0(1232, false)
	request.IsEdns0().SetVersion(1)
	writer := &captureResponseWriter{network: "udp"}
	server.ServeDNS(writer, request)
	if writer.message == nil || writer.message.Rcode != dns.RcodeBadVers {
		t.Fatalf("DNS response = %#v, want BADVERS", writer.message)
	}

	var collected metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &collected); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	metricValue := authorityMetric(collected, "deephost.dns.queries")
	sum, ok := metricValue.Data.(metricdata.Sum[int64])
	if !ok || len(sum.DataPoints) != 1 {
		t.Fatalf("DNS query metric = %#v", metricValue.Data)
	}
	assertAuthorityAttribute(t, sum.DataPoints[0].Attributes, "dns.response.code", "BADVERS")
	if strings.Contains(fmt.Sprintf("%#v", sum.DataPoints[0].Attributes), "BADSIG") {
		t.Fatal("unsupported EDNS version was mislabeled as BADSIG")
	}
}

func authorityMetric(collected metricdata.ResourceMetrics, name string) *metricdata.Metrics {
	for scopeIndex := range collected.ScopeMetrics {
		for metricIndex := range collected.ScopeMetrics[scopeIndex].Metrics {
			value := &collected.ScopeMetrics[scopeIndex].Metrics[metricIndex]
			if value.Name == name {
				return value
			}
		}
	}
	return &metricdata.Metrics{}
}

func assertAuthorityAttribute(t *testing.T, attributes attribute.Set, key, want string) {
	t.Helper()
	value, ok := attributes.Value(attribute.Key(key))
	if !ok || value.AsString() != want {
		t.Errorf("attribute %s = %q, %t; want %q", key, value.AsString(), ok, want)
	}
}

func newFixtureServer(t *testing.T, metricSets ...*telemetry.Metrics) *authoritative.Server {
	t.Helper()
	now := time.Unix(1_800_900_000, 0).UTC()
	store := openAuthoritativeStore(t, &now, "ns1.example.test")
	ctx := context.Background()
	value, err := store.Create(ctx, "example.test")
	if err != nil {
		t.Fatalf("Create fixture zone: %v", err)
	}
	records := []struct {
		name       string
		recordType zone.RecordType
		ttl        uint32
		value      string
	}{
		{name: "@", recordType: zone.TypeA, ttl: 300, value: "192.0.2.1"},
		{name: "www", recordType: zone.TypeA, ttl: 60, value: "192.0.2.10"},
		{name: "www", recordType: zone.TypeA, ttl: 60, value: "192.0.2.11"},
		{name: "alias", recordType: zone.TypeCNAME, ttl: 60, value: "www"},
		{name: "cutalias", recordType: zone.TypeCNAME, ttl: 60, value: "ns.child.example.test."},
		{name: "txtonly", recordType: zone.TypeTXT, ttl: 300, value: "present"},
		{name: "*", recordType: zone.TypeA, ttl: 60, value: "192.0.2.99"},
		{name: "leaf.ent", recordType: zone.TypeA, ttl: 60, value: "192.0.2.77"},
		{name: "block", recordType: zone.TypeTXT, ttl: 300, value: "closest encloser"},
		{name: "child", recordType: zone.TypeNS, ttl: 300, value: "ns.child.example.test."},
		{name: "ns.child", recordType: zone.TypeA, ttl: 300, value: "198.51.100.53"},
		{name: "ns.child", recordType: zone.TypeAAAA, ttl: 300, value: "2001:db8::53"},
	}
	for _, item := range records {
		record := normalizeAuthoritativeRecord(t, value.Name, item.name, item.recordType, item.ttl, item.value)
		value, err = store.CreateRecord(ctx, value.ID, record)
		if err != nil {
			t.Fatalf("Create fixture record %s %s: %v", item.name, item.recordType, err)
		}
	}

	server := authoritative.New(slog.New(slog.NewTextHandler(io.Discard, nil)), authoritative.DefaultMaxUDPSize, metricSets...)
	if err := server.Replace([]zone.Zone{value}); err != nil {
		t.Fatalf("Replace fixture snapshot: %v", err)
	}
	return server
}

func openAuthoritativeStore(t *testing.T, now *time.Time, nameserver string) *zone.Store {
	t.Helper()
	store, err := zone.Open(
		filepath.Join(t.TempDir(), "zones.db"),
		[]string{nameserver},
		zone.WithClock(func() time.Time { return *now }),
	)
	if err != nil {
		t.Fatalf("zone.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return store
}

func normalizeAuthoritativeRecord(
	t *testing.T,
	zoneName, name string,
	recordType zone.RecordType,
	ttl uint32,
	value string,
) zone.Record {
	t.Helper()
	record, err := zone.NormalizeRecord(zoneName, name, recordType, ttl, value)
	if err != nil {
		t.Fatalf("NormalizeRecord(%q, %q, %q): %v", name, recordType, value, err)
	}
	return record
}

type captureResponseWriter struct {
	message *dns.Msg
	network string
}

func (w *captureResponseWriter) LocalAddr() net.Addr {
	return testAddr{network: w.network, address: "127.0.0.1:53"}
}

func (w *captureResponseWriter) RemoteAddr() net.Addr {
	return testAddr{network: w.network, address: "192.0.2.200:53000"}
}

func (w *captureResponseWriter) WriteMsg(message *dns.Msg) error {
	w.message = message.Copy()
	return nil
}

func (w *captureResponseWriter) Write(raw []byte) (int, error) {
	message := new(dns.Msg)
	if err := message.Unpack(raw); err != nil {
		return 0, err
	}
	w.message = message
	return len(raw), nil
}

func (*captureResponseWriter) Close() error        { return nil }
func (*captureResponseWriter) TsigStatus() error   { return nil }
func (*captureResponseWriter) TsigTimersOnly(bool) {}
func (*captureResponseWriter) Hijack()             {}

type testAddr struct {
	network string
	address string
}

func (a testAddr) Network() string { return a.network }
func (a testAddr) String() string  { return a.address }

func exchangeUDP(t *testing.T, server *authoritative.Server, request *dns.Msg) *dns.Msg {
	t.Helper()
	writer := &captureResponseWriter{network: "udp"}
	server.ServeDNS(writer, request)
	if writer.message == nil {
		t.Fatal("ServeDNS did not write a response")
	}
	return writer.message
}

func assertRRTypes(t *testing.T, records []dns.RR, want ...uint16) {
	t.Helper()
	got := make([]uint16, 0, len(records))
	for _, record := range records {
		got = append(got, record.Header().Rrtype)
	}
	if !slices.Equal(got, want) {
		t.Errorf("RR types = %v, want %v", got, want)
	}
}

func assertAValues(t *testing.T, records []dns.RR, want ...string) {
	t.Helper()
	got := make([]string, 0, len(records))
	for _, record := range records {
		if address, ok := record.(*dns.A); ok {
			got = append(got, address.A.String())
		}
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("A values = %v, want %v", got, want)
	}
}

func assertNegativeSOA(t *testing.T, response *dns.Msg) {
	t.Helper()
	assertRRTypes(t, response.Ns, dns.TypeSOA)
	if len(response.Ns) != 1 {
		return
	}
	soa, ok := response.Ns[0].(*dns.SOA)
	if !ok {
		t.Fatalf("negative authority has type %T, want *dns.SOA", response.Ns[0])
	}
	if soa.Hdr.Name != "example.test." {
		t.Errorf("negative SOA owner = %q, want example.test.", soa.Hdr.Name)
	}
	if soa.Hdr.Ttl != 300 {
		t.Errorf("negative SOA TTL = %d, want minimum TTL 300", soa.Hdr.Ttl)
	}
}
