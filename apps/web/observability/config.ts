type Environment = Readonly<Record<string, string | undefined>>;
type Signal = "metrics" | "traces";

export type TelemetryConfig = {
  metricExportIntervalMillis: number;
  metricExportTimeoutMillis: number;
  metrics: boolean;
  traces: boolean;
};

const DEFAULT_EXPORT_INTERVAL_MILLIS = 30_000;
const DEFAULT_EXPORT_TIMEOUT_MILLIS = 10_000;

const exporterEnabled = (signal: Signal, environment: Environment) => {
  if (environment.OTEL_SDK_DISABLED?.trim().toLowerCase() === "true") {
    return false;
  }

  const signalName = signal.toUpperCase();
  const exporters = environment[`OTEL_${signalName}_EXPORTER`]
    ?.split(",")
    .map((value) => value.trim().toLowerCase())
    .filter(Boolean);
  if (!exporters?.includes("otlp") || exporters.includes("none")) {
    return false;
  }

  const endpoint =
    environment[`OTEL_EXPORTER_OTLP_${signalName}_ENDPOINT`] ??
    environment.OTEL_EXPORTER_OTLP_ENDPOINT;
  if (!endpoint) {
    return false;
  }

  try {
    const parsed = new URL(endpoint);
    if (
      (parsed.protocol !== "http:" && parsed.protocol !== "https:") ||
      parsed.username !== "" ||
      parsed.password !== "" ||
      parsed.search !== "" ||
      parsed.hash !== ""
    ) {
      return false;
    }
  } catch {
    return false;
  }

  const protocol = (
    environment[`OTEL_EXPORTER_OTLP_${signalName}_PROTOCOL`] ??
    environment.OTEL_EXPORTER_OTLP_PROTOCOL
  )
    ?.trim()
    .toLowerCase();
  return protocol === "http/protobuf";
};

const positiveMilliseconds = (
  raw: string | undefined,
  fallback: number,
  minimum: number,
  maximum: number,
) => {
  if (!raw || !/^\d+$/.test(raw.trim())) {
    return fallback;
  }
  const parsed = Number(raw);
  return parsed >= minimum && parsed <= maximum ? parsed : fallback;
};

export function getTelemetryConfig(
  environment: Environment = process.env,
): TelemetryConfig {
  const metricExportIntervalMillis = positiveMilliseconds(
    environment.OTEL_METRIC_EXPORT_INTERVAL,
    DEFAULT_EXPORT_INTERVAL_MILLIS,
    1_000,
    300_000,
  );
  const metricExportTimeoutMillis = Math.min(
    positiveMilliseconds(
      environment.OTEL_METRIC_EXPORT_TIMEOUT,
      DEFAULT_EXPORT_TIMEOUT_MILLIS,
      100,
      60_000,
    ),
    metricExportIntervalMillis,
  );

  return {
    metricExportIntervalMillis,
    metricExportTimeoutMillis,
    metrics: exporterEnabled("metrics", environment),
    traces: exporterEnabled("traces", environment),
  };
}
