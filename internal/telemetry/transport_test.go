package telemetry

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/codes"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestHTTPAndSnapshotTracePropagationExcludesSensitiveRequestData(t *testing.T) {
	previousPropagator := otel.GetTextMapPropagator()
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	t.Cleanup(func() { otel.SetTextMapPropagator(previousPropagator) })

	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(recorder),
	)
	t.Cleanup(func() {
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown tracer provider: %v", err)
		}
	})
	metrics, err := NewMetrics(
		metricnoop.NewMeterProvider().Meter("test"),
		WithTracer(provider.Tracer("test")),
		withSignals(false, true),
	)
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}

	handled := false
	var handledContext context.Context
	handler := metrics.HTTPMiddleware(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		handled = true
		handledContext = request.Context()
		if got := baggage.FromContext(request.Context()).Member("tenant").Value(); got != "private-tenant" {
			t.Errorf("propagated baggage = %q", got)
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest(http.MethodGet, "https://secret.example/healthz?token=supersecret", nil)
	request.RemoteAddr = "192.0.2.10:53000"
	request.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	request.Header.Set("tracestate", "vendor=attacker-marker")
	request.Header.Set("baggage", "tenant=private-tenant")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if !handled || response.Code != http.StatusNoContent {
		t.Fatalf("HTTP handler result: handled=%t status=%d", handled, response.Code)
	}

	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(spans))
	}
	span := spans[0]
	if span.Name() != "HTTP /healthz" {
		t.Errorf("span name = %q", span.Name())
	}
	if got := span.Parent().TraceID().String(); got != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("parent trace ID = %q", got)
	}
	if got := span.Parent().TraceState().String(); got != "" {
		t.Errorf("remote parent tracestate was retained: %q", got)
	}
	assertSpanAttribute(t, span.Attributes(), "http.request.method", "GET")
	assertSpanAttribute(t, span.Attributes(), "http.route", "/healthz")

	traceContext, finish := metrics.StartSnapshotFetch(handledContext)
	header := make(http.Header)
	InjectHTTPTrace(traceContext, header)
	if traceparent := header.Get("traceparent"); traceparent == "" {
		t.Fatal("snapshot trace context was not injected")
	}
	if got := header.Get("baggage"); got != "" {
		t.Fatalf("snapshot request forwarded public baggage %q", got)
	}
	finish("success")

	rendered := fmt.Sprintf("%#v", recorder.Ended())
	for _, forbidden := range []string{"secret.example", "supersecret", "192.0.2.10", "private-tenant", "attacker-marker"} {
		if strings.Contains(rendered, forbidden) {
			t.Errorf("trace export contains forbidden request data %q", forbidden)
		}
	}
}

func TestRootRatioSamplerCannotBeForcedBySampledTraceparent(t *testing.T) {
	t.Setenv("OTEL_TRACES_SAMPLER", "traceidratio")
	t.Setenv("OTEL_TRACES_SAMPLER_ARG", "0")
	previousPropagator := otel.GetTextMapPropagator()
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() { otel.SetTextMapPropagator(previousPropagator) })

	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() {
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown tracer provider: %v", err)
		}
	})
	metrics, err := NewMetrics(
		metricnoop.NewMeterProvider().Meter("sampler-test"),
		WithTracer(provider.Tracer("sampler-test")),
		withSignals(false, true),
	)
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}

	handler := metrics.HTTPMiddleware(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest(http.MethodGet, "https://dns.example/healthz", nil)
	request.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	handler.ServeHTTP(httptest.NewRecorder(), request)
	if spans := recorder.Ended(); len(spans) != 0 {
		t.Fatalf("sampled public parent forced %d recorded spans with traceidratio=0", len(spans))
	}
}

func TestHTTPAndConnectPanicFinalizationIsBoundedAndRepanics(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	meterProvider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() {
		if err := meterProvider.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown meter provider: %v", err)
		}
	})
	spanRecorder := tracetest.NewSpanRecorder()
	traceProvider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(spanRecorder),
	)
	t.Cleanup(func() {
		if err := traceProvider.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown tracer provider: %v", err)
		}
	})
	metrics, err := NewMetrics(
		meterProvider.Meter("panic-test"),
		WithTracer(traceProvider.Tracer("panic-test")),
	)
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}

	panicValue := &panicMarker{value: "private-panic-value"}
	handler := metrics.HTTPMiddleware(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusAccepted)
		panic(panicValue)
	}))
	httpPanic := capturePanic(func() {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz", nil))
	})
	if httpPanic != panicValue {
		t.Fatalf("HTTP panic = %#v, want original panic value", httpPanic)
	}

	next := func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
		panic(panicValue)
	}
	intercepted := metrics.ConnectInterceptor().WrapUnary(next)
	connectPanic := capturePanic(func() {
		if _, err := intercepted(context.Background(), connect.NewRequest(&struct{}{})); err != nil {
			t.Errorf("Connect interceptor returned instead of panicking: %v", err)
		}
	})
	if connectPanic != panicValue {
		t.Fatalf("Connect panic = %#v, want original panic value", connectPanic)
	}

	spans := spanRecorder.Ended()
	if len(spans) != 2 {
		t.Fatalf("ended spans = %d, want 2", len(spans))
	}
	if got := spans[0].Status(); got.Code != codes.Error || got.Description != "panic" {
		t.Errorf("HTTP span status = %#v, want Error/panic", got)
	}
	assertSpanIntAttribute(t, spans[0].Attributes(), "http.response.status_code", http.StatusAccepted)
	assertSpanAttribute(t, spans[0].Attributes(), "error.type", "panic")
	if got := spans[1].Status(); got.Code != codes.Error || got.Description != "panic" {
		t.Errorf("Connect span status = %#v, want Error/panic", got)
	}
	assertSpanAttribute(t, spans[1].Attributes(), "rpc.connect.status_code", "internal")
	assertSpanAttribute(t, spans[1].Attributes(), "error.type", "panic")

	var collected metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &collected); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	httpMetric := findMetric(collected, "simpledns.http.server.requests")
	httpSum, ok := httpMetric.Data.(metricdata.Sum[int64])
	if !ok || len(httpSum.DataPoints) != 1 || httpSum.DataPoints[0].Value != 1 {
		t.Fatalf("HTTP request metric = %#v, want one request", httpMetric.Data)
	}
	assertIntAttribute(t, httpSum.DataPoints[0].Attributes, "http.response.status_code", http.StatusAccepted)
	rpcMetric := findMetric(collected, "simpledns.rpc.server.requests")
	rpcSum, ok := rpcMetric.Data.(metricdata.Sum[int64])
	if !ok || len(rpcSum.DataPoints) != 1 || rpcSum.DataPoints[0].Value != 1 {
		t.Fatalf("RPC request metric = %#v, want one request", rpcMetric.Data)
	}
	assertAttribute(t, rpcSum.DataPoints[0].Attributes, "rpc.connect.status_code", "internal")

	rendered := fmt.Sprintf("%#v %#v", spans, collected)
	if strings.Contains(rendered, panicValue.value) {
		t.Fatal("panic value leaked into telemetry")
	}
}

type panicMarker struct {
	value string
}

func capturePanic(run func()) (value any) {
	defer func() {
		value = recover()
	}()
	run()
	return nil
}

func assertSpanAttribute(t *testing.T, attributes []attribute.KeyValue, key, want string) {
	t.Helper()
	for _, candidate := range attributes {
		if string(candidate.Key) == key {
			if got := candidate.Value.AsString(); got != want {
				t.Errorf("span attribute %s = %q, want %q", key, got, want)
			}
			return
		}
	}
	t.Errorf("span attribute %s is absent", key)
}

func assertSpanIntAttribute(t *testing.T, attributes []attribute.KeyValue, key string, want int) {
	t.Helper()
	for _, candidate := range attributes {
		if string(candidate.Key) == key {
			if got := candidate.Value.AsInt64(); got != int64(want) {
				t.Errorf("span attribute %s = %d, want %d", key, got, want)
			}
			return
		}
	}
	t.Errorf("span attribute %s is absent", key)
}

func assertIntAttribute(t *testing.T, attributes attribute.Set, key string, want int) {
	t.Helper()
	value, ok := attributes.Value(attribute.Key(key))
	if !ok || value.AsInt64() != int64(want) {
		t.Errorf("attribute %s = %d, %t; want %d", key, value.AsInt64(), ok, want)
	}
}
