import { metrics } from "@opentelemetry/api";
import { performance } from "node:perf_hooks";

import {
  normalizeMethod,
  normalizeRoute,
  normalizeRouteType,
  normalizeStatusCode,
  requestAttributes,
} from "./attributes";

export const METRIC_NAMES = {
  healthRequestDuration: "deephost.web.health.request.duration",
  healthRequests: "deephost.web.health.requests",
  processCpuTime: "deephost.web.process.cpu.time",
  processEventLoopUtilization: "deephost.web.process.event_loop.utilization",
  processMemoryUsage: "deephost.web.process.memory.usage",
  processUptime: "deephost.web.process.uptime",
  serverErrors: "deephost.web.server.errors",
  serverRequestDuration: "deephost.web.server.request.duration",
  serverRequests: "deephost.web.server.requests",
  serverStarts: "deephost.web.server.starts",
} as const;

let instruments: ReturnType<typeof createInstruments> | undefined;

function createInstruments() {
  // MetricsAPI returns a permanent no-op meter before provider registration
  // (unlike the trace API's proxy provider), so instruments must be created
  // lazily after instrumentation.node has installed the real provider.
  const meter = metrics.getMeter("deephost.web", "0.1.0");
  return {
    healthRequestDuration: meter.createHistogram(
      METRIC_NAMES.healthRequestDuration,
      {
        description: "Health endpoint request duration",
        unit: "s",
      },
    ),
    healthRequests: meter.createCounter(METRIC_NAMES.healthRequests, {
      description: "Health endpoint requests handled by the Next.js server",
      unit: "{request}",
    }),
    meter,
    serverErrors: meter.createCounter(METRIC_NAMES.serverErrors, {
      description: "Server errors captured by the Next.js onRequestError hook",
      unit: "{error}",
    }),
    serverRequestDuration: meter.createHistogram(
      METRIC_NAMES.serverRequestDuration,
      {
        description:
          "Server request duration observed from Next.js root request spans",
        unit: "s",
      },
    ),
    serverRequests: meter.createCounter(METRIC_NAMES.serverRequests, {
      description: "Server requests observed from Next.js root request spans",
      unit: "{request}",
    }),
    serverStarts: meter.createCounter(METRIC_NAMES.serverStarts, {
      description: "Next.js server process starts",
      unit: "{start}",
    }),
  };
}

const getInstruments = () => (instruments ??= createInstruments());

let runtimeMetricsRegistered = false;

export function registerRuntimeMetrics() {
  if (runtimeMetricsRegistered) {
    return;
  }
  runtimeMetricsRegistered = true;
  const { meter, serverStarts } = getInstruments();
  serverStarts.add(1);

  meter
    .createObservableGauge(METRIC_NAMES.processUptime, {
      description: "Next.js process uptime",
      unit: "s",
    })
    .addCallback((result) => result.observe(process.uptime()));

  meter
    .createObservableGauge(METRIC_NAMES.processMemoryUsage, {
      description: "Next.js process memory usage by fixed memory kind",
      unit: "By",
    })
    .addCallback((result) => {
      const memory = process.memoryUsage();
      result.observe(memory.rss, { kind: "rss" });
      result.observe(memory.heapTotal, { kind: "heap_total" });
      result.observe(memory.heapUsed, { kind: "heap_used" });
      result.observe(memory.external, { kind: "external" });
      result.observe(memory.arrayBuffers, { kind: "array_buffers" });
    });

  meter
    .createObservableCounter(METRIC_NAMES.processCpuTime, {
      description: "Cumulative Next.js process CPU time by fixed CPU mode",
      unit: "s",
    })
    .addCallback((result) => {
      const cpu = process.cpuUsage();
      result.observe(cpu.user / 1_000_000, { mode: "user" });
      result.observe(cpu.system / 1_000_000, { mode: "system" });
    });

  meter
    .createObservableGauge(METRIC_NAMES.processEventLoopUtilization, {
      description: "Cumulative Next.js event-loop utilization",
      unit: "1",
    })
    .addCallback((result) => {
      result.observe(performance.eventLoopUtilization().utilization);
    });
}

export function recordHealthRequest(durationSeconds: number, statusCode: number) {
  const { healthRequestDuration, healthRequests } = getInstruments();
  const attributes = requestAttributes({
    method: "GET",
    route: "/api/health",
    statusCode,
  });
  healthRequests.add(1, attributes);
  healthRequestDuration.record(Math.max(0, durationSeconds), attributes);
}

export function recordServerRequest({
  durationSeconds,
  method,
  route,
  statusCode,
}: {
  durationSeconds: number;
  method: unknown;
  route: unknown;
  statusCode: unknown;
}) {
  const { serverRequestDuration, serverRequests } = getInstruments();
  const attributes = requestAttributes({ method, route, statusCode });
  serverRequests.add(1, attributes);
  serverRequestDuration.record(Math.max(0, durationSeconds), attributes);
}

export function recordServerError({
  method,
  route,
  routeType,
}: {
  method: unknown;
  route: unknown;
  routeType: unknown;
}) {
  const { serverErrors } = getInstruments();
  serverErrors.add(1, {
    "http.request.method": normalizeMethod(method),
    "http.route": normalizeRoute(route),
    "next.route.type": normalizeRouteType(routeType),
  });
}

export const numericStatusCode = normalizeStatusCode;
