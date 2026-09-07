package snapshot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/castlemilk/dns/internal/authoritative"
	"github.com/castlemilk/dns/internal/telemetry"
	"github.com/castlemilk/dns/internal/zone"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestConsumerFetchCachesPublishesAndBecomesStale(t *testing.T) {
	t.Parallel()
	generatedAt := time.Date(2026, time.September, 2, 3, 4, 5, 0, time.UTC)
	now := generatedAt.Add(time.Second)
	raw, _, err := Build([]zone.Zone{validZone("example.test", "ns1.dns.test.", generatedAt)}, generatedAt, false)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	var requests atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests.Add(1)
		if got := request.Header.Get("Authorization"); got != "Bearer snapshot-secret" {
			return testResponse(http.StatusUnauthorized, []byte("unauthorized")), nil
		}
		return testResponse(http.StatusOK, raw), nil
	})}

	cachePath := filepath.Join(t.TempDir(), "nested", "snapshot.json")
	server := authoritative.New(nil, authoritative.DefaultMaxUDPSize)
	status := NewStatus(30 * time.Second)
	status.now = func() time.Time { return now }
	consumer := NewConsumer(server, status, client, "http://control"+Path, "snapshot-secret", cachePath, 1<<20, false, discardLogger())
	consumer.now = func() time.Time { return now }

	if ready, _ := status.Ready(); ready {
		t.Fatal("status ready before a snapshot")
	}
	if err := consumer.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if requests.Load() != 1 {
		t.Fatalf("requests = %d", requests.Load())
	}
	if zones, records := server.Counts(); zones != 1 || records != 3 {
		t.Fatalf("counts = %d/%d, want 1/3", zones, records)
	}
	if ready, reason := status.Ready(); !ready || reason != "ready" {
		t.Fatalf("ready = %t, reason = %q", ready, reason)
	}
	cached, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("read cache: %v", err)
	}
	if !bytes.Equal(cached, raw) {
		t.Fatal("cache does not contain the accepted snapshot")
	}
	info, err := os.Stat(cachePath)
	if err != nil {
		t.Fatalf("stat cache: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("cache permissions = %04o, want 0600", got)
	}
	directoryInfo, err := os.Stat(filepath.Dir(cachePath))
	if err != nil {
		t.Fatalf("stat cache directory: %v", err)
	}
	if got := directoryInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("cache directory permissions = %04o, want 0700", got)
	}

	now = generatedAt.Add(32 * time.Second)
	if ready, reason := status.Ready(); ready || reason != "snapshot is stale" {
		t.Fatalf("stale ready = %t, reason = %q", ready, reason)
	}
	if zones, _ := server.Counts(); zones != 1 {
		t.Fatal("stale snapshot was removed from the live server")
	}

	restartedServer := authoritative.New(nil, authoritative.DefaultMaxUDPSize)
	restartedStatus := NewStatus(30 * time.Second)
	restartedStatus.now = func() time.Time { return now }
	restarted := NewConsumer(restartedServer, restartedStatus, client, "http://control"+Path, "snapshot-secret", cachePath, 1<<20, false, discardLogger())
	restarted.now = func() time.Time { return now }
	if err := restarted.LoadCache(); err != nil {
		t.Fatalf("LoadCache: %v", err)
	}
	if zones, _ := restartedServer.Counts(); zones != 1 {
		t.Fatalf("restarted zone count = %d", zones)
	}
	if ready, reason := restartedStatus.Ready(); ready || reason != "snapshot is stale" {
		t.Fatalf("restarted ready = %t, reason = %q", ready, reason)
	}
}

func TestConsumerUnchangedPollAvoidsCompileAndCacheRewrite(t *testing.T) {
	t.Parallel()
	generatedAt := time.Date(2026, time.September, 2, 3, 4, 5, 0, time.UTC)
	now := generatedAt.Add(time.Second)
	raw, _, err := Build([]zone.Zone{validZone("example.test", "ns1.dns.test.", generatedAt)}, generatedAt, false)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	var conditional atomic.Bool
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("If-None-Match") != "" {
			conditional.Store(true)
		}
		return testResponse(http.StatusOK, raw), nil
	})}
	consumer := NewConsumer(
		authoritative.New(nil, authoritative.DefaultMaxUDPSize), NewStatus(time.Minute), client,
		"http://control"+Path, "secret", filepath.Join(t.TempDir(), "snapshot.json"), 1<<20, false, discardLogger(),
	)
	consumer.now = func() time.Time { return now }
	var compiles, writes atomic.Int32
	consumer.compile = func(values []zone.Zone) (*authoritative.CompiledSnapshot, error) {
		compiles.Add(1)
		return authoritative.Compile(values)
	}
	consumer.writeCache = func(path string, value []byte) error {
		writes.Add(1)
		return writeAtomic(path, value)
	}
	if err := consumer.Fetch(context.Background()); err != nil {
		t.Fatalf("first Fetch: %v", err)
	}
	now = now.Add(10 * time.Second)
	if err := consumer.Fetch(context.Background()); err != nil {
		t.Fatalf("second Fetch: %v", err)
	}
	if !conditional.Load() || compiles.Load() != 1 || writes.Load() != 1 {
		t.Fatalf("conditional/compiles/writes = %t/%d/%d, want true/1/1", conditional.Load(), compiles.Load(), writes.Load())
	}
}

func TestConsumerNotModifiedRefreshesReadiness(t *testing.T) {
	t.Parallel()
	generatedAt := time.Date(2026, time.September, 2, 3, 4, 5, 0, time.UTC)
	now := generatedAt.Add(time.Second)
	raw, document, err := Build([]zone.Zone{validZone("example.test", "ns1.dns.test.", generatedAt)}, generatedAt, false)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	var requests atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if requests.Add(1) == 1 {
			return testResponse(http.StatusOK, raw), nil
		}
		if got, want := request.Header.Get("If-None-Match"), checksumETag(document.Checksum); got != want {
			t.Fatalf("If-None-Match = %q, want %q", got, want)
		}
		return testResponse(http.StatusNotModified, nil), nil
	})}
	status := NewStatus(30 * time.Second)
	status.now = func() time.Time { return now }
	consumer := NewConsumer(
		authoritative.New(nil, authoritative.DefaultMaxUDPSize), status, client,
		"http://control"+Path, "secret", filepath.Join(t.TempDir(), "snapshot.json"), 1<<20, false, discardLogger(),
	)
	consumer.now = func() time.Time { return now }
	if err := consumer.Fetch(context.Background()); err != nil {
		t.Fatalf("first Fetch: %v", err)
	}
	now = now.Add(time.Minute)
	if ready, _ := status.Ready(); ready {
		t.Fatal("status remained ready before refresh")
	}
	if err := consumer.Fetch(context.Background()); err != nil {
		t.Fatalf("304 Fetch: %v", err)
	}
	if ready, reason := status.Ready(); !ready || reason != "ready" {
		t.Fatalf("ready after 304 = %t/%q", ready, reason)
	}
}

func TestConsumerRejectsInvalidSnapshotWithoutReplacingLastGood(t *testing.T) {
	t.Parallel()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() {
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown meter provider: %v", err)
		}
	})
	metrics, err := telemetry.NewMetrics(provider.Meter("snapshot-test"))
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	now := time.Date(2026, time.September, 2, 3, 4, 5, 0, time.UTC)
	validRaw, _, err := Build([]zone.Zone{validZone("one.test", "ns1.dns.test.", now)}, now, false)
	if err != nil {
		t.Fatalf("Build valid: %v", err)
	}
	invalidRaw, _, err := Build([]zone.Zone{
		validZone("one.test", "ns1.dns.test.", now),
		validZone("two.test", "ns1.dns.test.", now),
	}, now, false)
	if err != nil {
		t.Fatalf("Build invalid base: %v", err)
	}
	invalidRaw = bytes.Replace(invalidRaw, []byte("two.test"), []byte("bad.test"), 1)

	var body atomic.Value
	body.Store(validRaw)
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		raw, ok := body.Load().([]byte)
		if !ok {
			return nil, errors.New("snapshot response body has unexpected type")
		}
		return testResponse(http.StatusOK, raw), nil
	})}

	cachePath := filepath.Join(t.TempDir(), "snapshot.json")
	server := authoritative.New(nil, authoritative.DefaultMaxUDPSize, metrics)
	status := NewStatus(time.Minute, metrics)
	status.now = func() time.Time { return now }
	consumer := NewConsumer(server, status, client, "http://control"+Path, "secret", cachePath, 1<<20, false, discardLogger(), metrics)
	consumer.now = func() time.Time { return now }
	if err := consumer.Fetch(context.Background()); err != nil {
		t.Fatalf("initial Fetch: %v", err)
	}

	body.Store(invalidRaw)
	if err := consumer.Fetch(context.Background()); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("invalid Fetch error = %v", err)
	}
	var collected metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &collected); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	checksumMetric := snapshotMetric(collected, "deephost.snapshot.checksum.failures")
	checksumSum, ok := checksumMetric.Data.(metricdata.Sum[int64])
	if !ok || len(checksumSum.DataPoints) != 1 || checksumSum.DataPoints[0].Value != 1 {
		t.Fatalf("checksum failures = %#v, want 1", checksumMetric.Data)
	}
	compileMetric := snapshotMetric(collected, "deephost.authoritative.snapshot.compiles")
	compileSum, ok := compileMetric.Data.(metricdata.Sum[int64])
	if !ok || len(compileSum.DataPoints) != 1 || compileSum.DataPoints[0].Value != 1 {
		t.Fatalf("authoritative compiles = %#v, want exactly one successful consumer compile", compileMetric.Data)
	}
	compileOutcome, ok := compileSum.DataPoints[0].Attributes.Value(attribute.Key("outcome"))
	if !ok || compileOutcome.AsString() != "success" {
		t.Errorf("authoritative compile outcome = %q, %t; want success", compileOutcome.AsString(), ok)
	}
	compileDurationMetric := snapshotMetric(collected, "deephost.authoritative.snapshot.compile.duration")
	compileDuration, ok := compileDurationMetric.Data.(metricdata.Histogram[float64])
	if !ok || len(compileDuration.DataPoints) != 1 || compileDuration.DataPoints[0].Count != 1 {
		t.Fatalf("authoritative compile duration = %#v, want exactly one consumer compile observation", compileDurationMetric.Data)
	}
	if zones, _ := server.Counts(); zones != 1 {
		t.Fatalf("zone count after rejection = %d, want 1", zones)
	}
	cached, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("read cache: %v", err)
	}
	if !bytes.Equal(cached, validRaw) {
		t.Fatal("invalid snapshot replaced the cache")
	}

	olderRaw, _, err := Build([]zone.Zone{validZone("older.test", "ns1.dns.test.", now.Add(-time.Second))}, now.Add(-time.Second), false)
	if err != nil {
		t.Fatalf("Build older: %v", err)
	}
	body.Store(olderRaw)
	if err := consumer.Fetch(context.Background()); err == nil || !strings.Contains(err.Error(), "predates") {
		t.Fatalf("older Fetch error = %v", err)
	}
	if zones, _ := server.Counts(); zones != 1 {
		t.Fatalf("older snapshot changed zone count to %d", zones)
	}
}

func snapshotMetric(collected metricdata.ResourceMetrics, name string) *metricdata.Metrics {
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

func TestConsumerBoundsAndValidatesCache(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.September, 2, 3, 4, 5, 0, time.UTC)
	raw, _, err := Build([]zone.Zone{validZone("example.test", "ns1.dns.test.", now)}, now, false)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	t.Run("oversized feed", func(t *testing.T) {
		t.Parallel()
		client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return testResponse(http.StatusOK, raw), nil
		})}
		cachePath := filepath.Join(t.TempDir(), "snapshot.json")
		server := authoritative.New(nil, authoritative.DefaultMaxUDPSize)
		status := NewStatus(time.Minute)
		consumer := NewConsumer(server, status, client, "http://control"+Path, "secret", cachePath, int64(len(raw)-1), false, discardLogger())
		if err := consumer.Fetch(context.Background()); err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("Fetch error = %v", err)
		}
		if _, err := os.Stat(cachePath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("cache stat error = %v, want not exist", err)
		}
		if zones, _ := server.Counts(); zones != 0 {
			t.Fatalf("zone count = %d", zones)
		}
	})

	t.Run("open permissions", func(t *testing.T) {
		t.Parallel()
		cachePath := filepath.Join(t.TempDir(), "snapshot.json")
		if err := os.WriteFile(cachePath, raw, 0o644); err != nil {
			t.Fatalf("write cache: %v", err)
		}
		consumer := NewConsumer(
			authoritative.New(nil, authoritative.DefaultMaxUDPSize),
			NewStatus(time.Minute),
			&http.Client{}, "http://unused", "secret", cachePath, 1<<20, false, discardLogger(),
		)
		if err := consumer.LoadCache(); err == nil || !strings.Contains(err.Error(), "permissions") {
			t.Fatalf("LoadCache error = %v", err)
		}
	})

	t.Run("symlink", func(t *testing.T) {
		t.Parallel()
		directory := t.TempDir()
		target := filepath.Join(directory, "target.json")
		link := filepath.Join(directory, "snapshot.json")
		if err := os.WriteFile(target, raw, 0o600); err != nil {
			t.Fatalf("write target: %v", err)
		}
		if err := os.Symlink(target, link); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		consumer := NewConsumer(
			authoritative.New(nil, authoritative.DefaultMaxUDPSize),
			NewStatus(time.Minute),
			&http.Client{}, "http://unused", "secret", link, 1<<20, false, discardLogger(),
		)
		if err := consumer.LoadCache(); err == nil || !strings.Contains(err.Error(), "regular file") {
			t.Fatalf("LoadCache error = %v", err)
		}
	})
}

func TestConsumerRejectsChecksummedButInvalidZoneData(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.September, 2, 3, 4, 5, 0, time.UTC)
	invalid := validZone("example.test", "ns1.dns.test.", now)
	invalid.Records = invalid.Records[1:]
	document := Document{Version: Version, GeneratedAt: now, Zones: []zone.Zone{invalid}}
	checksum, err := calculateChecksum(document)
	if err != nil {
		t.Fatalf("calculateChecksum: %v", err)
	}
	document.Checksum = checksum
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return testResponse(http.StatusOK, raw), nil
	})}
	server := authoritative.New(nil, authoritative.DefaultMaxUDPSize)
	status := NewStatus(time.Minute)
	status.now = func() time.Time { return now }
	cachePath := filepath.Join(t.TempDir(), "snapshot.json")
	consumer := NewConsumer(server, status, client, "http://control"+Path, "secret", cachePath, 1<<20, false, discardLogger())
	consumer.now = func() time.Time { return now }
	if err := consumer.Fetch(context.Background()); err == nil || !strings.Contains(err.Error(), "SOA") {
		t.Fatalf("Fetch error = %v", err)
	}
	if zones, _ := server.Counts(); zones != 0 {
		t.Fatalf("invalid data published %d zones", zones)
	}
	if ready, _ := status.Ready(); ready {
		t.Fatal("invalid data made authority ready")
	}
	if _, err := os.Stat(cachePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cache stat error = %v, want not exist", err)
	}
}

func TestConsumerRejectsFutureAndProductionPlaceholderSnapshots(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.September, 2, 3, 4, 5, 0, time.UTC)

	tests := []struct {
		name       string
		generated  time.Time
		nameserver string
		production bool
		want       string
	}{
		{name: "future", generated: now.Add(maxFutureClockSkew + time.Second), nameserver: "ns1.dns.test.", want: "future"},
		{name: "production placeholder", generated: now, nameserver: "ns1.example.net.", production: true, want: "reserved"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			raw, _, err := Build([]zone.Zone{validZone("example.test", tt.nameserver, tt.generated)}, tt.generated, false)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return testResponse(http.StatusOK, raw), nil
			})}
			consumer := NewConsumer(
				authoritative.New(nil, authoritative.DefaultMaxUDPSize),
				NewStatus(time.Minute),
				client, "http://control"+Path, "secret", filepath.Join(t.TempDir(), "snapshot.json"), 1<<20, tt.production, discardLogger(),
			)
			consumer.now = func() time.Time { return now }
			if err := consumer.Fetch(context.Background()); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Fetch error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestConsumerRunPollsUntilCanceled(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	raw, _, err := Build([]zone.Zone{validZone("example.test", "ns1.dns.test.", now)}, now, false)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	var requests atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return testResponse(http.StatusOK, raw), nil
	})}
	consumer := NewConsumer(
		authoritative.New(nil, authoritative.DefaultMaxUDPSize),
		NewStatus(time.Minute),
		client, "http://control"+Path, "secret", filepath.Join(t.TempDir(), "snapshot.json"), 1<<20, false, discardLogger(),
	)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		consumer.Run(ctx, 5*time.Millisecond)
		close(done)
	}()
	deadline := time.After(time.Second)
	for requests.Load() < 2 {
		select {
		case <-deadline:
			t.Fatal("consumer did not poll twice")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("consumer did not stop after cancellation")
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func testResponse(status int, body []byte) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
}
