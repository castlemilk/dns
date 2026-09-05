import {
  type Attributes,
  type Context,
  type Link,
  type SpanKind,
  SpanStatusCode,
} from "@opentelemetry/api";
import {
  AlwaysOffSampler,
  AlwaysOnSampler,
  ParentBasedSampler,
  SamplingDecision,
  type ReadableSpan,
  type Sampler,
  type SamplingResult,
  type SpanProcessor,
  TraceIdRatioBasedSampler,
} from "@opentelemetry/sdk-trace-node";

import { numericStatusCode, recordServerRequest } from "./metrics";

const ROOT_REQUEST_SPAN = "BaseServer.handleRequest";

const ratioFromEnvironment = () => {
  const raw = process.env.OTEL_TRACES_SAMPLER_ARG?.trim();
  if (!raw) {
    return 1;
  }
  const ratio = Number(raw);
  return Number.isFinite(ratio) && ratio >= 0 && ratio <= 1 ? ratio : 1;
};

export function samplerFromEnvironment(): Sampler {
  const name = process.env.OTEL_TRACES_SAMPLER?.trim().toLowerCase();
  switch (name) {
    case "always_off":
      return new AlwaysOffSampler();
    case "traceidratio":
      return new TraceIdRatioBasedSampler(ratioFromEnvironment());
    case "parentbased_always_off":
      return new ParentBasedSampler({ root: new AlwaysOffSampler() });
    case "parentbased_traceidratio":
      return new ParentBasedSampler({
        root: new TraceIdRatioBasedSampler(ratioFromEnvironment()),
      });
    case "always_on":
      return new AlwaysOnSampler();
    case "parentbased_always_on":
    case undefined:
    case "":
    default:
      return new ParentBasedSampler({ root: new AlwaysOnSampler() });
  }
}

/**
 * Keeps all spans recordable for exact request metrics while preserving the
 * configured sampled bit. BatchSpanProcessor exports only sampled spans, so
 * OTEL_TRACES_SAMPLER continues to control trace volume without undercounting
 * Prometheus request metrics.
 */
export class MetricsPreservingSampler implements Sampler {
  constructor(private readonly delegate: Sampler) {}

  shouldSample(
    context: Context,
    traceId: string,
    spanName: string,
    spanKind: SpanKind,
    attributes: Attributes,
    links: Link[],
  ): SamplingResult {
    const result = this.delegate.shouldSample(
      context,
      traceId,
      spanName,
      spanKind,
      attributes,
      links,
    );
    return result.decision === SamplingDecision.NOT_RECORD
      ? { ...result, decision: SamplingDecision.RECORD }
      : result;
  }

  toString() {
    return `MetricsPreservingSampler{${this.delegate.toString()}}`;
  }
}

const hrTimeSeconds = ([seconds, nanoseconds]: readonly [number, number]) =>
  Math.max(0, seconds + nanoseconds / 1_000_000_000);

export class RequestMetricSpanProcessor implements SpanProcessor {
  onStart() {}

  onEnd(span: ReadableSpan) {
    if (span.attributes["next.span_type"] !== ROOT_REQUEST_SPAN) {
      return;
    }

    const statusCode = numericStatusCode(
      span.attributes["http.response.status_code"] ??
        span.attributes["http.status_code"],
    );
    recordServerRequest({
      durationSeconds: hrTimeSeconds(span.duration),
      method:
        span.attributes["http.request.method"] ?? span.attributes["http.method"],
      route: span.attributes["next.route"] ?? span.attributes["http.route"],
      statusCode:
        statusCode === 0 && span.status.code === SpanStatusCode.ERROR
          ? 500
          : statusCode,
    });
  }

  forceFlush() {
    return Promise.resolve();
  }

  shutdown() {
    return Promise.resolve();
  }
}
