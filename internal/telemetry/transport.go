package telemetry

import (
	"context"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// HTTPMiddleware emits one bounded-cardinality metric point and server span
// per request. It deliberately excludes URLs, query strings, peer addresses,
// headers, and bodies.
func (m *Metrics) HTTPMiddleware(next http.Handler) http.Handler {
	if !m.MetricsEnabled() && !m.TracesEnabled() {
		return next
	}
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		started := time.Now()
		route := httpRoute(request.URL.Path)
		parent := request.Context()
		if m.TracesEnabled() {
			parent = otel.GetTextMapPropagator().Extract(parent, propagation.HeaderCarrier(request.Header))
			parent = withoutRemoteTraceState(parent)
		}
		spanContext, span := m.tracer.Start(
			parent,
			"HTTP "+route,
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				attribute.String("http.request.method", allowed(request.Method, httpMethods, "OTHER")),
				attribute.String("http.route", route),
			),
		)
		request = request.WithContext(spanContext)
		captured := &responseMetricsWriter{ResponseWriter: writer, status: http.StatusOK}
		completed := false
		defer func() {
			status := captured.status
			if !completed {
				if !captured.wroteHeader {
					status = http.StatusInternalServerError
				}
				span.SetStatus(codes.Error, "panic")
				span.SetAttributes(attribute.String("error.type", "panic"))
			} else if status >= http.StatusInternalServerError {
				span.SetStatus(codes.Error, http.StatusText(status))
			}
			m.HTTPRequest(request.Context(), request.Method, route, status, time.Since(started))
			span.SetAttributes(attribute.Int("http.response.status_code", boundedHTTPStatus(status)))
			span.End()
		}()
		next.ServeHTTP(captured, request)
		completed = true
	})
}

type responseMetricsWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *responseMetricsWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.status = status
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseMetricsWriter) Write(payload []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(payload)
}

func (w *responseMetricsWriter) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if err := http.NewResponseController(w.ResponseWriter).Flush(); err != nil {
		return
	}
}

func (w *responseMetricsWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (m *Metrics) ConnectInterceptor() connect.Interceptor {
	return connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		if !m.MetricsEnabled() && !m.TracesEnabled() {
			return next
		}
		return func(ctx context.Context, request connect.AnyRequest) (response connect.AnyResponse, resultErr error) {
			started := time.Now()
			service, method := rpcTarget(request.Spec().Procedure)
			spanContext, span := m.tracer.Start(
				ctx,
				service+"/"+method,
				trace.WithSpanKind(trace.SpanKindServer),
				trace.WithAttributes(
					attribute.String("rpc.system", "connect_rpc"),
					attribute.String("rpc.service", service),
					attribute.String("rpc.method", method),
				),
			)
			completed := false
			defer func() {
				code := "ok"
				switch {
				case !completed:
					code = "internal"
					span.SetStatus(codes.Error, "panic")
					span.SetAttributes(attribute.String("error.type", "panic"))
				case resultErr != nil:
					code = connect.CodeOf(resultErr).String()
					span.SetStatus(codes.Error, allowed(code, connectCodes, "unknown"))
				}
				span.SetAttributes(attribute.String("rpc.connect.status_code", allowed(code, connectCodes, "unknown")))
				m.RPCRequest(spanContext, service, method, code, time.Since(started))
				span.End()
			}()
			response, resultErr = next(spanContext, request)
			completed = true
			return response, resultErr
		}
	})
}

// StartSnapshotFetch creates a client span without recording the configured
// control-plane URL or request headers. The returned function must be called
// exactly once with a bounded outcome from SnapshotFetch.
func (m *Metrics) StartSnapshotFetch(ctx context.Context) (context.Context, func(string)) {
	if !m.TracesEnabled() {
		return ctx, func(string) {}
	}
	spanContext, span := m.tracer.Start(
		ctx,
		"snapshot.fetch",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attribute.String("http.request.method", http.MethodGet)),
	)
	return spanContext, func(outcome string) {
		outcome = allowed(outcome, snapshotFetchOutcomes, "apply_error")
		span.SetAttributes(attribute.String("simpledns.snapshot.fetch.outcome", outcome))
		if outcome != "success" && outcome != "not_modified" {
			span.SetStatus(codes.Error, outcome)
		}
		span.End()
	}
}

func InjectHTTPTrace(ctx context.Context, header http.Header) {
	// Snapshot requests only need causal trace correlation. Do not forward
	// baggage inherited from a public DNS control request to another service.
	propagation.TraceContext{}.Inject(ctx, propagation.HeaderCarrier(header))
}

func withoutRemoteTraceState(ctx context.Context) context.Context {
	parent := trace.SpanContextFromContext(ctx)
	if !parent.IsRemote() || !parent.IsValid() {
		return ctx
	}
	clean := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    parent.TraceID(),
		SpanID:     parent.SpanID(),
		TraceFlags: parent.TraceFlags(),
		Remote:     true,
	})
	return trace.ContextWithRemoteSpanContext(ctx, clean)
}

func httpRoute(path string) string {
	if _, ok := httpRoutes[path]; ok {
		return path
	}
	return "other"
}

// splitProcedure cuts "/package.Service/Method" into its two halves. It reports
// false for anything that is not that shape.
func splitProcedure(procedure string) (service, method string, ok bool) {
	trimmed := strings.TrimPrefix(procedure, "/")
	if trimmed == procedure {
		return "", "", false
	}
	service, method, ok = strings.Cut(trimmed, "/")
	if !ok || service == "" || method == "" || strings.Contains(method, "/") {
		return "", "", false
	}
	return service, method, true
}

// rpcTarget maps a Connect procedure onto the two bounded attribute values. A
// procedure this binary does not mount collapses to unknown/unknown rather than
// creating a new time series.
func rpcTarget(procedure string) (service, method string) {
	rawService, rawMethod, ok := splitProcedure(procedure)
	if !ok {
		return "unknown", "unknown"
	}
	service = allowed(rawService, rpcServices, "unknown")
	method = allowed(rawMethod, rpcMethods, "unknown")
	if service == "unknown" {
		// A method name from an unmounted service would still be a bounded
		// string, but reporting it beside an unknown service invites a false
		// reading of the metric.
		return service, "unknown"
	}
	return service, method
}
