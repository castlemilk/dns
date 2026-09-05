package authoritative_test

import (
	"fmt"
	"net"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/castlemilk/dns/internal/authoritative"
	"github.com/castlemilk/dns/internal/telemetry"
	"github.com/castlemilk/dns/internal/zone"
	"github.com/miekg/dns"
	"go.opentelemetry.io/otel/metric/noop"
)

const benchmarkZoneName = "bench.invalid"

func BenchmarkCompileReplacement(b *testing.B) {
	benchmarks := []struct {
		name   string
		values []zone.Zone
	}{
		{name: "Realistic10K", values: []zone.Zone{realisticBenchmarkZone(10_000)}},
		{name: "LargeTXT2000x3360B", values: []zone.Zone{largeTXTBenchmarkZone(2_000, 3_360)}},
	}

	for _, benchmark := range benchmarks {
		benchmark := benchmark
		b.Run(benchmark.name, func(b *testing.B) {
			server := authoritative.New(nil, authoritative.DefaultMaxUDPSize)
			steadyHeap, replacementHeap := observeSnapshotHeap(b, server, benchmark.values)

			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				compiled, err := authoritative.Compile(benchmark.values)
				if err != nil {
					b.Fatalf("Compile: %v", err)
				}
				server.ReplaceCompiled(compiled)
			}
			b.StopTimer()
			b.ReportMetric(steadyHeap, "steady-heap-inuse-B")
			b.ReportMetric(replacementHeap, "replacement-heap-inuse-B")
			runtime.KeepAlive(server)
			runtime.KeepAlive(benchmark.values)
		})
	}
}

func BenchmarkServeDNSExactA(b *testing.B) {
	benchmarks := []struct {
		name    string
		metrics func(*testing.B) *telemetry.Metrics
	}{
		{name: "Disabled", metrics: func(*testing.B) *telemetry.Metrics { return telemetry.Disabled() }},
		{name: "EnabledNoop", metrics: func(b *testing.B) *telemetry.Metrics {
			b.Helper()
			value, err := telemetry.NewMetrics(noop.NewMeterProvider().Meter("benchmark"))
			if err != nil {
				b.Fatalf("NewMetrics: %v", err)
			}
			return value
		}},
	}
	for _, benchmark := range benchmarks {
		b.Run(benchmark.name, func(b *testing.B) {
			server := authoritative.New(nil, authoritative.DefaultMaxUDPSize, benchmark.metrics(b))
			compiled, err := authoritative.Compile([]zone.Zone{realisticBenchmarkZone(10_000)})
			if err != nil {
				b.Fatalf("Compile: %v", err)
			}
			server.ReplaceCompiled(compiled)

			request := new(dns.Msg)
			request.SetQuestion("host-00000."+benchmarkZoneName+".", dns.TypeA)
			writer := &packingBenchmarkResponseWriter{}

			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				server.ServeDNS(writer, request)
			}
			b.StopTimer()
			if writer.err != nil {
				b.Fatalf("pack response: %v", writer.err)
			}
			runtime.KeepAlive(server)
			runtime.KeepAlive(request)
			runtime.KeepAlive(writer.last)
		})
	}
}

func observeSnapshotHeap(b *testing.B, server *authoritative.Server, values []zone.Zone) (float64, float64) {
	b.Helper()
	runtime.GC()
	var baseline runtime.MemStats
	runtime.ReadMemStats(&baseline)

	initial, err := authoritative.Compile(values)
	if err != nil {
		b.Fatalf("compile initial snapshot: %v", err)
	}
	server.ReplaceCompiled(initial)
	runtime.GC()
	var steady runtime.MemStats
	runtime.ReadMemStats(&steady)

	replacement, err := authoritative.Compile(values)
	if err != nil {
		b.Fatalf("compile replacement snapshot: %v", err)
	}
	var replacing runtime.MemStats
	runtime.ReadMemStats(&replacing)
	server.ReplaceCompiled(replacement)

	runtime.KeepAlive(initial)
	runtime.KeepAlive(replacement)
	return heapIncrease(steady.HeapInuse, baseline.HeapInuse), heapIncrease(replacing.HeapInuse, baseline.HeapInuse)
}

func heapIncrease(after, before uint64) float64 {
	if after <= before {
		return 0
	}
	return float64(after - before)
}

func realisticBenchmarkZone(recordCount int) zone.Zone {
	records := benchmarkManagedRecords(recordCount)
	for index := 0; len(records) < recordCount; index++ {
		owner := fmt.Sprintf("host-%05d", index/2)
		record := zone.Record{
			ID:   fmt.Sprintf("record-%05d", index),
			Name: owner,
			TTL:  300,
		}
		switch bucket := index % 100; {
		case bucket < 56:
			record.Type = zone.TypeA
			record.Value = fmt.Sprintf("192.0.%d.%d", (index/254)%256, index%254+1)
		case bucket < 81:
			record.Type = zone.TypeAAAA
			record.Value = fmt.Sprintf("2001:db8::%x", index+1)
		default:
			record.Type = zone.TypeTXT
			record.Value = strconv.Quote(strings.Repeat("x", 64+index%161))
		}
		records = append(records, record)
	}
	return zone.Zone{Name: benchmarkZoneName, Records: records}
}

func largeTXTBenchmarkZone(recordCount, payloadBytes int) zone.Zone {
	records := benchmarkManagedRecords(recordCount + 2)
	value := segmentedTXTValue(payloadBytes)
	for index := 0; index < recordCount; index++ {
		records = append(records, zone.Record{
			ID:    fmt.Sprintf("txt-%05d", index),
			Name:  fmt.Sprintf("txt-%05d", index),
			Type:  zone.TypeTXT,
			TTL:   300,
			Value: value,
		})
	}
	return zone.Zone{Name: benchmarkZoneName, Records: records}
}

func benchmarkManagedRecords(capacity int) []zone.Record {
	records := make([]zone.Record, 0, capacity)
	return append(records,
		zone.Record{
			ID:      "soa",
			Name:    "@",
			Type:    zone.TypeSOA,
			TTL:     zone.DefaultSOATTL,
			Value:   "ns1.bench.invalid. hostmaster.bench.invalid. 1 3600 600 1209600 300",
			Managed: true,
		},
		zone.Record{
			ID:      "ns",
			Name:    "@",
			Type:    zone.TypeNS,
			TTL:     zone.DefaultSOATTL,
			Value:   "ns1.bench.invalid.",
			Managed: true,
		},
	)
}

func segmentedTXTValue(payloadBytes int) string {
	const segmentBytes = 240
	var value strings.Builder
	for remaining := payloadBytes; remaining > 0; {
		length := min(remaining, segmentBytes)
		if value.Len() > 0 {
			value.WriteByte(' ')
		}
		value.WriteString(strconv.Quote(strings.Repeat("x", length)))
		remaining -= length
	}
	return value.String()
}

type packingBenchmarkResponseWriter struct {
	last []byte
	err  error
}

func (*packingBenchmarkResponseWriter) LocalAddr() net.Addr {
	return benchmarkAddr{network: "udp", address: "127.0.0.1:53"}
}

func (*packingBenchmarkResponseWriter) RemoteAddr() net.Addr {
	return benchmarkAddr{network: "udp", address: "192.0.2.1:53000"}
}

func (w *packingBenchmarkResponseWriter) WriteMsg(message *dns.Msg) error {
	w.last, w.err = message.Pack()
	return w.err
}

func (w *packingBenchmarkResponseWriter) Write(raw []byte) (int, error) {
	w.last = append(w.last[:0], raw...)
	return len(raw), nil
}

func (*packingBenchmarkResponseWriter) Close() error        { return nil }
func (*packingBenchmarkResponseWriter) TsigStatus() error   { return nil }
func (*packingBenchmarkResponseWriter) TsigTimersOnly(bool) {}
func (*packingBenchmarkResponseWriter) Hijack()             {}

type benchmarkAddr struct {
	network string
	address string
}

func (a benchmarkAddr) Network() string { return a.network }
func (a benchmarkAddr) String() string  { return a.address }
