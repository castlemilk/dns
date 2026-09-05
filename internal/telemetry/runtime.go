package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	otelruntime "go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	metricgrpc "go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	metrichttp "go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	tracegrpc "go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	tracehttp "go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

const (
	metricsExporterEnv = "OTEL_METRICS_EXPORTER"
	tracesExporterEnv  = "OTEL_TRACES_EXPORTER"
)

// Runtime is the process-wide OpenTelemetry lifecycle. With no exporter
// variables configured it contains no-op providers and performs no network
// activity. Shutdown is safe to call more than once and flushes enabled SDK
// providers within the caller's context deadline.
type Runtime struct {
	metrics        *Metrics
	metricsEnabled bool
	tracesEnabled  bool
	shutdowns      []func(context.Context) error
	shutdownOnce   sync.Once
	shutdownErr    error
	previousMeter  metric.MeterProvider
	previousTracer trace.TracerProvider
	previousProp   propagation.TextMapPropagator
	previousError  otel.ErrorHandler
}

func Start(ctx context.Context, role string, logger *slog.Logger) (*Runtime, error) {
	if logger == nil {
		logger = slog.Default()
	}
	disabled, err := sdkDisabled(os.Getenv)
	if err != nil {
		return nil, err
	}
	metricExporterName, err := configuredExporter(os.Getenv, metricsExporterEnv, disabled)
	if err != nil {
		return nil, err
	}
	traceExporterName, err := configuredExporter(os.Getenv, tracesExporterEnv, disabled)
	if err != nil {
		return nil, err
	}

	var metricProvider metric.MeterProvider = metricnoop.NewMeterProvider()
	var traceProvider trace.TracerProvider = tracenoop.NewTracerProvider()
	runtime := &Runtime{
		metricsEnabled: metricExporterName == "otlp",
		tracesEnabled:  traceExporterName == "otlp",
	}
	if !runtime.metricsEnabled && !runtime.tracesEnabled {
		runtime.metrics = Disabled()
		return runtime, nil
	}

	serviceResource, err := newResource(ctx, role)
	if err != nil {
		return nil, err
	}

	if runtime.metricsEnabled {
		exporter, exporterErr := newMetricExporter(ctx, signalProtocol(os.Getenv, "METRICS"))
		if exporterErr != nil {
			return nil, exporterErr
		}
		reader := sdkmetric.NewPeriodicReader(exporter, sdkmetric.WithProducer(otelruntime.NewProducer()))
		provider := sdkmetric.NewMeterProvider(
			sdkmetric.WithResource(serviceResource),
			sdkmetric.WithReader(reader),
		)
		if runtimeErr := otelruntime.Start(otelruntime.WithMeterProvider(provider)); runtimeErr != nil {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if shutdownErr := provider.Shutdown(cleanupCtx); shutdownErr != nil {
				logger.Error("clean up OpenTelemetry metrics after startup failure", "error", shutdownErr)
			}
			return nil, fmt.Errorf("start Go runtime metrics: %w", runtimeErr)
		}
		metricProvider = provider
		runtime.shutdowns = append(runtime.shutdowns, provider.Shutdown)
		runtime.previousMeter = otel.GetMeterProvider()
		otel.SetMeterProvider(provider)
	}
	if runtime.tracesEnabled {
		propagator, propagatorErr := configuredPropagator(os.Getenv)
		if propagatorErr != nil {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if shutdownErr := runtime.Shutdown(cleanupCtx); shutdownErr != nil {
				logger.Error("clean up OpenTelemetry after propagator configuration failure", "error", shutdownErr)
			}
			return nil, propagatorErr
		}
		runtime.previousProp = otel.GetTextMapPropagator()
		otel.SetTextMapPropagator(propagator)
		exporter, exporterErr := newTraceExporter(ctx, signalProtocol(os.Getenv, "TRACES"))
		if exporterErr != nil {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if shutdownErr := runtime.Shutdown(cleanupCtx); shutdownErr != nil {
				logger.Error("clean up OpenTelemetry after trace exporter failure", "error", shutdownErr)
			}
			return nil, exporterErr
		}
		provider := sdktrace.NewTracerProvider(
			sdktrace.WithResource(serviceResource),
			sdktrace.WithBatcher(exporter),
		)
		traceProvider = provider
		runtime.shutdowns = append(runtime.shutdowns, provider.Shutdown)
		runtime.previousTracer = otel.GetTracerProvider()
		otel.SetTracerProvider(provider)
	}
	runtime.previousError = otel.GetErrorHandler()
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		logger.Error("OpenTelemetry export", "error", err)
	}))
	runtime.metrics, err = NewMetrics(
		metricProvider.Meter(instrumentationScope),
		WithTracer(traceProvider.Tracer(instrumentationScope)),
		withSignals(runtime.metricsEnabled, runtime.tracesEnabled),
	)
	if err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if shutdownErr := runtime.Shutdown(cleanupCtx); shutdownErr != nil {
			logger.Error("clean up OpenTelemetry after instrumentation failure", "error", shutdownErr)
		}
		return nil, err
	}
	logger.Info("OpenTelemetry initialized", "metrics", runtime.metricsEnabled, "traces", runtime.tracesEnabled)
	return runtime, nil
}

func (r *Runtime) Metrics() *Metrics {
	if r == nil || r.metrics == nil {
		return Disabled()
	}
	return r.metrics
}

func (r *Runtime) MetricsEnabled() bool {
	return r != nil && r.metricsEnabled
}

func (r *Runtime) TracesEnabled() bool {
	return r != nil && r.tracesEnabled
}

func (r *Runtime) Shutdown(ctx context.Context) error {
	if r == nil {
		return nil
	}
	r.shutdownOnce.Do(func() {
		var shutdownErrs []error
		for index := len(r.shutdowns) - 1; index >= 0; index-- {
			if err := r.shutdowns[index](ctx); err != nil {
				shutdownErrs = append(shutdownErrs, err)
			}
		}
		r.shutdownErr = errors.Join(shutdownErrs...)
		if r.previousMeter != nil {
			otel.SetMeterProvider(r.previousMeter)
		}
		if r.previousTracer != nil {
			otel.SetTracerProvider(r.previousTracer)
		}
		if r.previousProp != nil {
			otel.SetTextMapPropagator(r.previousProp)
		}
		if r.previousError != nil {
			otel.SetErrorHandler(r.previousError)
		}
	})
	return r.shutdownErr
}

func configuredExporter(getenv func(string) string, name string, disabled bool) (string, error) {
	if disabled {
		return "none", nil
	}
	value := strings.ToLower(strings.TrimSpace(getenv(name)))
	if value == "" || value == "none" {
		return "none", nil
	}
	if value != "otlp" {
		return "", fmt.Errorf("%s must be otlp or none", name)
	}
	return value, nil
}

func newResource(ctx context.Context, role string) (*resource.Resource, error) {
	serviceResource, err := resource.New(
		ctx,
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
	)
	if err != nil {
		return nil, fmt.Errorf("create OpenTelemetry resource: %w", err)
	}
	ownedAttributes := make([]attribute.KeyValue, 0, 2)
	if _, configured := serviceResource.Set().Value(attribute.Key("service.name")); !configured {
		ownedAttributes = append(ownedAttributes, attribute.String("service.name", "simpledns"))
	}
	if _, configured := serviceResource.Set().Value(attribute.Key("simpledns.role")); !configured {
		ownedAttributes = append(ownedAttributes, attribute.String("simpledns.role", allowed(role, serviceRoles, "unknown")))
	}
	serviceResource, err = resource.Merge(serviceResource, resource.NewSchemaless(ownedAttributes...))
	if err != nil {
		return nil, fmt.Errorf("merge OpenTelemetry resource: %w", err)
	}
	return serviceResource, nil
}

func sdkDisabled(getenv func(string) string) (bool, error) {
	raw := strings.TrimSpace(getenv("OTEL_SDK_DISABLED"))
	if raw == "" {
		return false, nil
	}
	disabled, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("OTEL_SDK_DISABLED must be true or false")
	}
	return disabled, nil
}

func signalProtocol(getenv func(string) string, signal string) string {
	if value := strings.ToLower(strings.TrimSpace(getenv("OTEL_EXPORTER_OTLP_" + signal + "_PROTOCOL"))); value != "" {
		return value
	}
	if value := strings.ToLower(strings.TrimSpace(getenv("OTEL_EXPORTER_OTLP_PROTOCOL"))); value != "" {
		return value
	}
	return "grpc"
}

func newMetricExporter(ctx context.Context, protocol string) (sdkmetric.Exporter, error) {
	switch protocol {
	case "grpc":
		exporter, err := metricgrpc.New(ctx)
		if err != nil {
			return nil, fmt.Errorf("create OTLP/gRPC metric exporter: %w", err)
		}
		return exporter, nil
	case "http/protobuf":
		exporter, err := metrichttp.New(ctx)
		if err != nil {
			return nil, fmt.Errorf("create OTLP/HTTP metric exporter: %w", err)
		}
		return exporter, nil
	default:
		return nil, fmt.Errorf("OTLP metrics protocol %q is unsupported; use grpc or http/protobuf", protocol)
	}
}

func newTraceExporter(ctx context.Context, protocol string) (sdktrace.SpanExporter, error) {
	switch protocol {
	case "grpc":
		exporter, err := tracegrpc.New(ctx)
		if err != nil {
			return nil, fmt.Errorf("create OTLP/gRPC trace exporter: %w", err)
		}
		return exporter, nil
	case "http/protobuf":
		exporter, err := tracehttp.New(ctx)
		if err != nil {
			return nil, fmt.Errorf("create OTLP/HTTP trace exporter: %w", err)
		}
		return exporter, nil
	default:
		return nil, fmt.Errorf("OTLP traces protocol %q is unsupported; use grpc or http/protobuf", protocol)
	}
}

func configuredPropagator(getenv func(string) string) (propagation.TextMapPropagator, error) {
	raw := strings.ToLower(strings.TrimSpace(getenv("OTEL_PROPAGATORS")))
	if raw == "" {
		raw = "tracecontext,baggage"
	}
	parts := strings.Split(raw, ",")
	if len(parts) == 1 && strings.TrimSpace(parts[0]) == "none" {
		return propagation.NewCompositeTextMapPropagator(), nil
	}
	propagators := make([]propagation.TextMapPropagator, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		name := strings.TrimSpace(part)
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		seen[name] = struct{}{}
		switch name {
		case "tracecontext":
			propagators = append(propagators, propagation.TraceContext{})
		case "baggage":
			propagators = append(propagators, propagation.Baggage{})
		case "none":
			return nil, fmt.Errorf("OTEL_PROPAGATORS none cannot be combined with other propagators")
		default:
			return nil, fmt.Errorf("OTEL_PROPAGATORS contains unsupported propagator %q", name)
		}
	}
	return propagation.NewCompositeTextMapPropagator(propagators...), nil
}

var serviceRoles = choices("all", "control", "authority", "unknown")
