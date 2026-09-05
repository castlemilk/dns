import { metrics, trace } from "@opentelemetry/api";
import { OTLPMetricExporter } from "@opentelemetry/exporter-metrics-otlp-proto";
import { OTLPTraceExporter } from "@opentelemetry/exporter-trace-otlp-proto";
import {
  defaultResource,
  detectResources,
  envDetector,
  resourceFromAttributes,
} from "@opentelemetry/resources";
import {
  AggregationType,
  MeterProvider,
  PeriodicExportingMetricReader,
} from "@opentelemetry/sdk-metrics";
import {
  BatchSpanProcessor,
  NodeTracerProvider,
} from "@opentelemetry/sdk-trace-node";
import {
  ATTR_SERVICE_NAME,
  ATTR_SERVICE_NAMESPACE,
  ATTR_SERVICE_VERSION,
} from "@opentelemetry/semantic-conventions";

import { getTelemetryConfig } from "./observability/config";
import { METRIC_NAMES, registerRuntimeMetrics } from "./observability/metrics";
import { PrivacyPreservingSpanExporter } from "./observability/privacy";
import {
  MetricsPreservingSampler,
  RequestMetricSpanProcessor,
  samplerFromEnvironment,
} from "./observability/tracing";

type TelemetryState = {
  meterProvider?: MeterProvider;
  shutdownPromise?: Promise<void>;
  tracerProvider?: NodeTracerProvider;
};

const SHUTDOWN_TIMEOUT_MILLIS = 5_000;
const terminationSignals = ["SIGINT", "SIGTERM"] as const;

const boundedTelemetryShutdown = (state: TelemetryState) => {
  if (state.shutdownPromise) {
    return state.shutdownPromise;
  }

  const shutdown = Promise.allSettled([
    state.meterProvider?.shutdown(),
    state.tracerProvider?.shutdown(),
  ]).then(() => undefined);
  state.shutdownPromise = new Promise<void>((resolve) => {
    const timeout = setTimeout(resolve, SHUTDOWN_TIMEOUT_MILLIS);
    void shutdown.then(() => {
      clearTimeout(timeout);
      resolve();
    });
  });
  return state.shutdownPromise;
};

const installSignalShutdown = (state: TelemetryState) => {
  let terminationStarted = false;
  let exitRequested = false;
  const originalExit = process.exit;
  const managedSignals = terminationSignals.filter(
    (signal) => process.listenerCount(signal) > 0,
  );

  const restoreExit = () => {
    process.exit = originalExit;
  };
  const finishExit = (code: Parameters<typeof process.exit>[0]) => {
    restoreExit();
    Reflect.apply(originalExit, process, [code]);
  };
  const onSignal = (signal: NodeJS.Signals) => {
    if (terminationStarted) {
      // Next intentionally ignores duplicate termination signals while its
      // cleanup is running. Treat a second signal as the operator's force-exit
      // request instead of trapping the process behind telemetry.
      finishExit(signal === "SIGINT" ? 130 : 143);
      return;
    }
    terminationStarted = true;

    // Next's listener was registered first and has already called
    // server.close() by the time this later listener runs. Its async cleanup
    // drains active requests before calling process.exit(130/143). Defer only
    // that final exit, leaving Next's ingress and request lifecycle untouched.
    process.exit = ((code) => {
      if (!exitRequested) {
        exitRequested = true;
        void boundedTelemetryShutdown(state).then(() => finishExit(code));
      }
      return undefined as never;
    }) as typeof process.exit;
  };

  // Do not install a signal listener where no framework/application handler
  // exists: retaining a signal would otherwise disable Node's default exit.
  for (const signal of managedSignals) {
    process.on(signal, onSignal);
  }
};

const stateKey = Symbol.for("simpledns.web.opentelemetry.state");
const globalState = globalThis as typeof globalThis & {
  [stateKey]?: TelemetryState;
};

const config = getTelemetryConfig();

if (!globalState[stateKey] && (config.metrics || config.traces)) {
  const applicationResource = resourceFromAttributes({
    [ATTR_SERVICE_NAME]: "simpledns-web",
    [ATTR_SERVICE_NAMESPACE]: "simpledns",
    [ATTR_SERVICE_VERSION]: "0.1.0",
  });
  // Environment resource attributes intentionally override the safe defaults
  // so Kubernetes can attach deployment metadata using standard OTEL_* vars.
  const resource = defaultResource()
    .merge(applicationResource)
    .merge(detectResources({ detectors: [envDetector] }));

  const state: TelemetryState = {};
  const spanProcessors = [];

  if (config.metrics) {
    const metricReader = new PeriodicExportingMetricReader({
      cardinalityLimits: { default: 64 },
      exporter: new OTLPMetricExporter(),
      exportIntervalMillis: config.metricExportIntervalMillis,
      exportTimeoutMillis: config.metricExportTimeoutMillis,
    });
    const meterProvider = new MeterProvider({
      readers: [metricReader],
      resource,
      views: [
        {
          aggregation: {
            type: AggregationType.EXPLICIT_BUCKET_HISTOGRAM,
            options: {
              boundaries: [0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10],
            },
          },
          aggregationCardinalityLimit: 32,
          instrumentName: METRIC_NAMES.serverRequestDuration,
        },
        {
          aggregation: {
            type: AggregationType.EXPLICIT_BUCKET_HISTOGRAM,
            options: {
              boundaries: [0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1],
            },
          },
          aggregationCardinalityLimit: 16,
          instrumentName: METRIC_NAMES.healthRequestDuration,
        },
      ],
    });
    metrics.setGlobalMeterProvider(meterProvider);
    state.meterProvider = meterProvider;
    registerRuntimeMetrics();
    spanProcessors.push(new RequestMetricSpanProcessor());
  }

  if (config.traces) {
    spanProcessors.push(
      new BatchSpanProcessor(
        new PrivacyPreservingSpanExporter(new OTLPTraceExporter()),
      ),
    );
  }

  const configuredSampler = samplerFromEnvironment();
  const tracerProvider = new NodeTracerProvider({
    resource,
    sampler: config.metrics
      ? new MetricsPreservingSampler(configuredSampler)
      : configuredSampler,
    spanProcessors,
  });
  tracerProvider.register();
  state.tracerProvider = tracerProvider;
  globalState[stateKey] = state;

  if (config.traces) {
    const startupSpan = trace
      .getTracer("simpledns.web", "0.1.0")
      .startSpan("simpledns.web.server.start");
    startupSpan.setAttribute("next.span_type", "SimpleDNS.serverStart");
    startupSpan.end();
  }

  installSignalShutdown(state);
}
