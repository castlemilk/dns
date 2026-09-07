import type { Attributes, SpanContext } from "@opentelemetry/api";
import type {
  ReadableSpan,
  SpanExporter,
} from "@opentelemetry/sdk-trace-node";

import {
  normalizeMethod,
  normalizeRoute,
  normalizeStatusCode,
} from "./attributes";

const knownSpanTypes = new Map<string, string>([
  ["AppRender.fetch", "http.client.request"],
  ["AppRender.getBodyResult", "next.render.route"],
  ["AppRouteRouteHandlers.runHandler", "next.route.handler"],
  ["BaseServer.handleRequest", "http.server.request"],
  ["NextNodeServer.findPageComponents", "next.resolve.page_components"],
  ["NextNodeServer.getLayoutOrPageModule", "next.resolve.segment_modules"],
  ["NextNodeServer.startResponse", "next.start_response"],
  ["Render.getServerSideProps", "next.get_server_side_props"],
  ["Render.getStaticProps", "next.get_static_props"],
  ["Render.renderDocument", "next.render.document"],
  ["ResolveMetadata.generateMetadata", "next.generate_metadata"],
  ["DeepHost.serverStart", "deephost.web.server.start"],
]);

const sanitizedAttributes = (attributes: Attributes): Attributes => {
  const sanitized: Attributes = {};
  const spanType = attributes["next.span_type"];
  if (typeof spanType === "string" && knownSpanTypes.has(spanType)) {
    sanitized["next.span_type"] = spanType;
  }

  const route = normalizeRoute(attributes["next.route"] ?? attributes["http.route"]);
  sanitized["next.route"] = route;
  sanitized["http.route"] = route;

  const method = normalizeMethod(
    attributes["http.request.method"] ?? attributes["http.method"],
  );
  sanitized["http.request.method"] = method;

  const statusCode = normalizeStatusCode(
    attributes["http.response.status_code"] ?? attributes["http.status_code"],
  );
  if (statusCode !== 0) {
    sanitized["http.response.status_code"] = statusCode;
  }

  if (typeof attributes["next.rsc"] === "boolean") {
    sanitized["next.rsc"] = attributes["next.rsc"];
  }
  return sanitized;
};

const withoutTraceState = (
  context: SpanContext | undefined,
): SpanContext | undefined =>
  context
    ? {
        isRemote: context.isRemote,
        spanId: context.spanId,
        traceFlags: context.traceFlags,
        traceId: context.traceId,
      }
    : undefined;

export const sanitizeSpan = (span: ReadableSpan): ReadableSpan => {
  const spanType = span.attributes["next.span_type"];
  return {
    attributes: sanitizedAttributes(span.attributes),
    droppedAttributesCount: span.droppedAttributesCount,
    droppedEventsCount: span.droppedEventsCount,
    droppedLinksCount: span.droppedLinksCount,
    duration: span.duration,
    ended: span.ended,
    endTime: span.endTime,
    // Error events carry exception messages/stacks and fetch spans carry raw
    // URLs. Dropping all events and links is a deliberate privacy boundary.
    events: [],
    instrumentationScope: span.instrumentationScope,
    kind: span.kind,
    links: [],
    name:
      typeof spanType === "string"
        ? (knownSpanTypes.get(spanType) ?? "next.server.operation")
        : "next.server.operation",
    // W3C tracestate is copied from an incoming request and is therefore an
    // attacker-controlled header. Preserve only the correlation identifiers
    // and sampled flag in exported child/parent contexts.
    parentSpanContext: withoutTraceState(span.parentSpanContext),
    resource: span.resource,
    spanContext: () => withoutTraceState(span.spanContext())!,
    startTime: span.startTime,
    status: { code: span.status.code },
  };
};

export class PrivacyPreservingSpanExporter implements SpanExporter {
  constructor(private readonly delegate: SpanExporter) {}

  export(
    spans: ReadableSpan[],
    resultCallback: Parameters<SpanExporter["export"]>[1],
  ) {
    this.delegate.export(spans.map(sanitizeSpan), resultCallback);
  }

  forceFlush() {
    return this.delegate.forceFlush?.() ?? Promise.resolve();
  }

  shutdown() {
    return this.delegate.shutdown();
  }
}
