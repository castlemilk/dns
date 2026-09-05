package telemetry

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestMetricsEmitBoundedCatalogAndAttributes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() {
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown meter provider: %v", err)
		}
	})
	now := time.Date(2026, time.September, 2, 1, 2, 3, 0, time.UTC)
	metrics, err := NewMetrics(provider.Meter("test"), WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}

	metrics.HTTPRequest(ctx, "POST", "/dns.v1.DNSService/CreateZone", 200, 2*time.Millisecond)
	metrics.AuthFailure(ctx, "api", "invalid")
	metrics.RPCRequest(ctx, "dns.v1.DNSService", "CreateZone", "ok", 2*time.Millisecond)
	metrics.ControlMutation(ctx, "zone", "create", "success", "none", 3*time.Millisecond)
	metrics.DNSQuery(ctx, "client=192.0.2.1", "secret.example", "credential", time.Millisecond, 123, true)
	metrics.DNSWriteFailure(ctx, "udp")
	metrics.SnapshotBuild(ctx, "success", "miss", 4*time.Millisecond, 4096)
	metrics.SnapshotFetch(ctx, "success", 5*time.Millisecond, 4096)
	metrics.SnapshotApply(ctx, "fetch", "checksum_error", 6*time.Millisecond)
	metrics.SnapshotAdmission(ctx, "success", 7*time.Millisecond)
	metrics.SnapshotCache(ctx, "write", "success", 8*time.Millisecond, 4096)
	metrics.Readiness(ctx, false, "snapshot is stale")
	metrics.StoreTransaction(ctx, "create_zone", "write", "success", time.Millisecond)
	metrics.AuthoritativeCompile(ctx, "success", 9*time.Millisecond)
	metrics.AuthoritativePublish(ctx)
	metrics.SetInventory(2, 17)
	metrics.SetSnapshotStatus(now.Add(-10*time.Second), 30*time.Second)

	var collected metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &collected); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	wantNames := []string{
		"simpledns.http.server.requests",
		"simpledns.http.server.duration",
		"simpledns.http.auth.failures",
		"simpledns.rpc.server.requests",
		"simpledns.rpc.server.duration",
		"simpledns.control.mutations",
		"simpledns.control.mutation.duration",
		"simpledns.dns.queries",
		"simpledns.dns.query.duration",
		"simpledns.dns.response.size",
		"simpledns.dns.responses.truncated",
		"simpledns.dns.write.failures",
		"simpledns.snapshot.builds",
		"simpledns.snapshot.build.duration",
		"simpledns.snapshot.size",
		"simpledns.snapshot.fetches",
		"simpledns.snapshot.fetch.duration",
		"simpledns.snapshot.applies",
		"simpledns.snapshot.apply.duration",
		"simpledns.snapshot.admissions",
		"simpledns.snapshot.admission.duration",
		"simpledns.snapshot.cache.operations",
		"simpledns.snapshot.cache.operation.duration",
		"simpledns.snapshot.checksum.failures",
		"simpledns.readiness.checks",
		"simpledns.store.transactions",
		"simpledns.store.transaction.duration",
		"simpledns.authoritative.snapshot.compiles",
		"simpledns.authoritative.snapshot.compile.duration",
		"simpledns.authoritative.snapshot.publishes",
		"simpledns.inventory.zones",
		"simpledns.inventory.records",
		"simpledns.snapshot.loaded",
		"simpledns.snapshot.ready",
		"simpledns.snapshot.age",
	}
	for _, name := range wantNames {
		if findMetric(collected, name) == nil {
			t.Errorf("metric %q was not collected", name)
		}
	}

	dnsMetric := findMetric(collected, "simpledns.dns.queries")
	dnsSum, ok := dnsMetric.Data.(metricdata.Sum[int64])
	if !ok || len(dnsSum.DataPoints) != 1 {
		t.Fatalf("DNS queries data = %#v", dnsMetric.Data)
	}
	assertAttribute(t, dnsSum.DataPoints[0].Attributes, "network.transport", "other")
	assertAttribute(t, dnsSum.DataPoints[0].Attributes, "dns.question.type", "OTHER")
	assertAttribute(t, dnsSum.DataPoints[0].Attributes, "dns.response.code", "OTHER")

	assertGaugeValue(t, collected, "simpledns.inventory.zones", 2)
	assertGaugeValue(t, collected, "simpledns.inventory.records", 17)
	assertGaugeValue(t, collected, "simpledns.snapshot.loaded", 1)
	assertGaugeValue(t, collected, "simpledns.snapshot.ready", 1)
	ageMetric := findMetric(collected, "simpledns.snapshot.age")
	ageGauge, ok := ageMetric.Data.(metricdata.Gauge[float64])
	if !ok || len(ageGauge.DataPoints) != 1 || ageGauge.DataPoints[0].Value != 10 {
		t.Fatalf("snapshot age = %#v, want 10 seconds", ageMetric.Data)
	}

	rendered := fmt.Sprintf("%#v", collected)
	for _, forbidden := range []string{"192.0.2.1", "secret.example", "credential"} {
		if strings.Contains(rendered, forbidden) {
			t.Errorf("collected telemetry contains forbidden value %q", forbidden)
		}
	}
}

func findMetric(collected metricdata.ResourceMetrics, name string) *metricdata.Metrics {
	for scopeIndex := range collected.ScopeMetrics {
		for metricIndex := range collected.ScopeMetrics[scopeIndex].Metrics {
			candidate := &collected.ScopeMetrics[scopeIndex].Metrics[metricIndex]
			if candidate.Name == name {
				return candidate
			}
		}
	}
	return nil
}

func assertAttribute(t *testing.T, attributes attribute.Set, key, want string) {
	t.Helper()
	value, ok := attributes.Value(attribute.Key(key))
	if !ok || value.AsString() != want {
		t.Errorf("attribute %s = %q, %t; want %q", key, value.AsString(), ok, want)
	}
}

func assertGaugeValue(t *testing.T, collected metricdata.ResourceMetrics, name string, want int64) {
	t.Helper()
	value := findMetric(collected, name)
	if value == nil {
		t.Fatalf("metric %q is absent", name)
	}
	gauge, ok := value.Data.(metricdata.Gauge[int64])
	if !ok || len(gauge.DataPoints) != 1 || gauge.DataPoints[0].Value != want {
		t.Errorf("%s = %#v, want %d", name, value.Data, want)
	}
}
