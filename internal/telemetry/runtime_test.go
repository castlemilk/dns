package telemetry

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	otelruntime "go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestStartIsDisabledByDefault(t *testing.T) {
	for _, name := range []string{"OTEL_SDK_DISABLED", metricsExporterEnv, tracesExporterEnv} {
		t.Setenv(name, "")
	}
	runtime, err := Start(context.Background(), "authority", nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if runtime.MetricsEnabled() || runtime.TracesEnabled() {
		t.Fatalf("signals enabled by default: metrics=%t traces=%t", runtime.MetricsEnabled(), runtime.TracesEnabled())
	}
	if runtime.Metrics().MetricsEnabled() || runtime.Metrics().TracesEnabled() {
		t.Fatal("disabled runtime returned active instrumentation")
	}
	if err := runtime.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func TestRuntimeShutdownIsIdempotentAndPreservesErrors(t *testing.T) {
	wantErr := errors.New("flush failed")
	var calls atomic.Int64
	runtime := &Runtime{
		metrics: Disabled(),
		shutdowns: []func(context.Context) error{func(context.Context) error {
			calls.Add(1)
			return wantErr
		}},
	}
	for range 2 {
		if err := runtime.Shutdown(context.Background()); !errors.Is(err, wantErr) {
			t.Fatalf("Shutdown error = %v, want %v", err, wantErr)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("shutdown calls = %d, want 1", got)
	}
}

func TestRuntimeShutdownRestoresGlobalErrorHandler(t *testing.T) {
	original := otel.GetErrorHandler()
	previous := &countingErrorHandler{}
	active := &countingErrorHandler{}
	otel.SetErrorHandler(active)
	t.Cleanup(func() { otel.SetErrorHandler(original) })

	runtime := &Runtime{
		metrics:       Disabled(),
		previousError: previous,
	}
	if err := runtime.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	otel.Handle(errors.New("restoration probe"))
	if got := previous.calls.Load(); got != 1 {
		t.Errorf("restored handler calls = %d, want 1", got)
	}
	if got := active.calls.Load(); got != 0 {
		t.Errorf("replaced handler calls = %d, want 0", got)
	}
}

type countingErrorHandler struct {
	calls atomic.Int64
}

func (h *countingErrorHandler) Handle(error) {
	h.calls.Add(1)
}

func TestResourceHonorsStandardEnvironmentPrecedence(t *testing.T) {
	t.Setenv("OTEL_SERVICE_NAME", "dns-authority")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "service.namespace=dns,pod.name=authority-0,deephost.role=spoofed")
	value, err := newResource(context.Background(), "authority")
	if err != nil {
		t.Fatalf("newResource: %v", err)
	}
	assertResourceAttribute(t, value.Set(), "service.name", "dns-authority")
	assertResourceAttribute(t, value.Set(), "service.namespace", "dns")
	assertResourceAttribute(t, value.Set(), "pod.name", "authority-0")
	assertResourceAttribute(t, value.Set(), "deephost.role", "spoofed")
}

func TestResourceAddsOnlyMissingServiceName(t *testing.T) {
	t.Setenv("OTEL_SERVICE_NAME", "")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "service.namespace=dns")
	value, err := newResource(context.Background(), "control")
	if err != nil {
		t.Fatalf("newResource: %v", err)
	}
	assertResourceAttribute(t, value.Set(), "service.name", "deephost")
	assertResourceAttribute(t, value.Set(), "service.namespace", "dns")
}

func TestExporterAndPropagatorConfigurationRejectsUnsupportedValues(t *testing.T) {
	getenv := func(name string) string {
		values := map[string]string{
			metricsExporterEnv: "prometheus",
			"OTEL_PROPAGATORS": "tracecontext,custom",
		}
		return values[name]
	}
	if _, err := configuredExporter(getenv, metricsExporterEnv, false); err == nil {
		t.Fatal("unsupported metric exporter was accepted")
	}
	if _, err := configuredPropagator(getenv); err == nil {
		t.Fatal("unsupported propagator was accepted")
	}
	if got := signalProtocol(func(string) string { return "" }, "METRICS"); got != "grpc" {
		t.Fatalf("default protocol = %q, want grpc", got)
	}
}

func TestOfficialGoRuntimeMetricsAreCollected(t *testing.T) {
	t.Parallel()
	reader := sdkmetric.NewManualReader(sdkmetric.WithProducer(otelruntime.NewProducer()))
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	if err := otelruntime.Start(otelruntime.WithMeterProvider(provider)); err != nil {
		t.Fatalf("start runtime instrumentation: %v", err)
	}
	t.Cleanup(func() {
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown provider: %v", err)
		}
	})
	var collected metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &collected); err != nil {
		t.Fatalf("collect runtime metrics: %v", err)
	}
	for _, name := range []string{"go.goroutine.count", "go.memory.used", "go.memory.gc.goal", "go.schedule.duration"} {
		if findMetric(collected, name) == nil {
			t.Errorf("runtime metric %q was not collected", name)
		}
	}
}

func assertResourceAttribute(t *testing.T, attributes *attribute.Set, key, want string) {
	t.Helper()
	value, ok := attributes.Value(attribute.Key(key))
	if !ok || value.AsString() != want {
		t.Errorf("resource attribute %s = %q, %t; want %q", key, value.AsString(), ok, want)
	}
}
