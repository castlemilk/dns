#!/usr/bin/env node

import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { createServer } from "node:http";
import { createConnection } from "node:net";
import { resolve } from "node:path";

const readVarint = (buffer, start) => {
  let next = start;
  let shift = 0n;
  let value = 0n;
  while (next < buffer.length) {
    const byte = BigInt(buffer[next]);
    next += 1;
    value |= (byte & 0x7fn) << shift;
    if ((byte & 0x80n) === 0n) {
      return { next, value };
    }
    shift += 7n;
  }
  throw new Error("truncated protobuf varint");
};

const protobufFields = (buffer) => {
  const fields = [];
  let offset = 0;
  while (offset < buffer.length) {
    const tag = readVarint(buffer, offset);
    offset = tag.next;
    const field = Number(tag.value >> 3n);
    const wireType = Number(tag.value & 7n);
    if (wireType === 0) {
      const scalar = readVarint(buffer, offset);
      fields.push({ field, value: scalar.value, wireType });
      offset = scalar.next;
    } else if (wireType === 1) {
      fields.push({ field, value: buffer.subarray(offset, offset + 8), wireType });
      offset += 8;
    } else if (wireType === 2) {
      const encodedLength = readVarint(buffer, offset);
      const length = Number(encodedLength.value);
      offset = encodedLength.next;
      fields.push({
        field,
        value: buffer.subarray(offset, offset + length),
        wireType,
      });
      offset += length;
    } else if (wireType === 5) {
      fields.push({ field, value: buffer.subarray(offset, offset + 4), wireType });
      offset += 4;
    } else {
      throw new Error(`unsupported protobuf wire type ${wireType}`);
    }
  }
  return fields;
};

const messages = (buffer, field) =>
  protobufFields(buffer)
    .filter((entry) => entry.field === field && entry.wireType === 2)
    .map((entry) => entry.value);

const decodeMetricTypes = (metricBodies) => {
  const decoded = new Map();
  for (const body of metricBodies) {
    for (const resourceMetrics of messages(body, 1)) {
      for (const scopeMetrics of messages(resourceMetrics, 2)) {
        for (const metric of messages(scopeMetrics, 2)) {
          const fields = protobufFields(metric);
          const name = fields.find(
            (entry) => entry.field === 1 && entry.wireType === 2,
          )?.value.toString("utf8");
          const data = fields.find((entry) => [5, 7, 9].includes(entry.field));
          if (!name || !data || decoded.has(name)) {
            continue;
          }
          const type = data.field === 5 ? "gauge" : data.field === 7 ? "sum" : "histogram";
          const monotonic =
            data.field === 7 &&
            protobufFields(data.value).some(
              (entry) => entry.field === 3 && entry.value === 1n,
            );
          decoded.set(name, { monotonic, type });
        }
      }
    }
  }
  return decoded;
};

const payloads = [];
const heldCollectorResponses = [];
let holdCollectorResponses = false;
const acknowledgeExport = (response) => {
  response.writeHead(200, { "Content-Type": "application/x-protobuf" });
  response.end();
};
const collector = createServer((request, response) => {
  const chunks = [];
  request.on("data", (chunk) => chunks.push(chunk));
  request.on("end", () => {
    payloads.push({
      body: Buffer.concat(chunks),
      path: request.url,
    });
    if (holdCollectorResponses) {
      heldCollectorResponses.push(response);
    } else {
      acknowledgeExport(response);
    }
  });
});

const listen = (server) =>
  new Promise((resolveListen, reject) => {
    server.once("error", reject);
    server.listen(0, "127.0.0.1", () => resolveListen(server.address()));
  });

const collectorAddress = await listen(collector);
assert.equal(typeof collectorAddress, "object");

const portProbe = createServer();
const webAddress = await listen(portProbe);
assert.equal(typeof webAddress, "object");
await new Promise((resolveClose, reject) =>
  portProbe.close((error) => (error ? reject(error) : resolveClose())),
);

const secretMarker = "operator-token-must-not-cross-telemetry";
const dnsMarker = "managed-zone-must-not-cross-telemetry.invalid";
const traceStateMarker = "attacker-tracestate-must-not-cross-telemetry";
const child = spawn(process.execPath, [resolve(".next/standalone/server.js")], {
  env: {
    ...process.env,
    HOSTNAME: "127.0.0.1",
    NODE_ENV: "production",
    OTEL_EXPORTER_OTLP_ENDPOINT: `http://127.0.0.1:${collectorAddress.port}`,
    OTEL_EXPORTER_OTLP_METRICS_ENDPOINT: `http://127.0.0.1:${collectorAddress.port}/v1/metrics`,
    OTEL_EXPORTER_OTLP_METRICS_PROTOCOL: "http/protobuf",
    OTEL_EXPORTER_OTLP_PROTOCOL: "http/protobuf",
    OTEL_EXPORTER_OTLP_TRACES_ENDPOINT: `http://127.0.0.1:${collectorAddress.port}/v1/traces`,
    OTEL_EXPORTER_OTLP_TRACES_PROTOCOL: "http/protobuf",
    OTEL_METRICS_EXPORTER: "otlp",
    // Keep periodic export beyond the smoke deadline so final data must be
    // drained by the signal shutdown path.
    OTEL_METRIC_EXPORT_INTERVAL: "60000",
    OTEL_METRIC_EXPORT_TIMEOUT: "1000",
    OTEL_SDK_DISABLED: "false",
    OTEL_SERVICE_NAME: "deephost-web-smoke",
    OTEL_TRACES_EXPORTER: "otlp",
    // A deterministic ratio sampler ignores an untrusted incoming sampled bit.
    // Low/high trace IDs below exercise exported and record-only requests.
    OTEL_TRACES_SAMPLER: "traceidratio",
    OTEL_TRACES_SAMPLER_ARG: "0.5",
    PORT: String(webAddress.port),
  },
  stdio: ["ignore", "pipe", "pipe"],
});

let output = "";
child.stdout.on("data", (chunk) => {
  output += chunk.toString();
});
child.stderr.on("data", (chunk) => {
  output += chunk.toString();
});

const baseUrl = `http://127.0.0.1:${webAddress.port}`;
const deadline = Date.now() + 15_000;
let healthResponse;
while (Date.now() < deadline) {
  try {
    healthResponse = await fetch(
      `${baseUrl}/api/health?token=${secretMarker}&domain=${dnsMarker}`,
      {
        headers: {
          traceparent:
            "00-00000001000000000000000000000000-00f067aa0ba902b7-01",
          tracestate: `vendor=${traceStateMarker}`,
        },
      },
    );
    if (healthResponse.ok) {
      break;
    }
  } catch {
    await new Promise((resolveWait) => setTimeout(resolveWait, 100));
  }
}
assert.equal(healthResponse?.status, 200, `server did not become healthy:\n${output}`);
await healthResponse.arrayBuffer();
const unsampledResponse = await fetch(
  `${baseUrl}/domains?token=${secretMarker}&domain=${dnsMarker}`,
  {
    headers: {
      // This trace ID falls above the configured ratio threshold. The incoming
      // sampled flag must not force trace export, while request metrics must
      // still observe the record-only span.
      traceparent:
        "00-ffffffff000000000000000000000000-00f067aa0ba902b8-01",
    },
  },
);
await unsampledResponse.arrayBuffer();

const requiredMetrics = [
  "deephost.web.health.request.duration",
  "deephost.web.health.requests",
  "deephost.web.process.cpu.time",
  "deephost.web.process.event_loop.utilization",
  "deephost.web.process.memory.usage",
  "deephost.web.process.uptime",
  "deephost.web.server.request.duration",
  "deephost.web.server.requests",
  "deephost.web.server.starts",
];

const finalResponse = await fetch(
  `${baseUrl}/connect?token=${secretMarker}&domain=${dnsMarker}`,
  {
    headers: {
      connection: "close",
      traceparent:
        "00-00000002000000000000000000000000-00f067aa0ba902b9-01",
    },
  },
);
assert.equal(finalResponse.status, 200, "final instrumented request failed");
await finalResponse.arrayBuffer();

let childExited = false;
const childExit = new Promise((resolveExit) => {
  child.once("exit", (code, signal) => {
    childExited = true;
    resolveExit({ code, signal });
  });
});
holdCollectorResponses = true;
assert.equal(child.kill("SIGTERM"), true, "failed to signal standalone server");

const shutdownExportDeadline = Date.now() + 4_000;
let finalShutdownExportsReceived = false;
while (Date.now() < shutdownExportDeadline) {
  const metricText = payloads
    .filter((payload) => payload.path === "/v1/metrics")
    .map((payload) => payload.body.toString("utf8"))
    .join("\n");
  const traceText = payloads
    .filter((payload) => payload.path === "/v1/traces")
    .map((payload) => payload.body.toString("utf8"))
    .join("\n");
  if (
    requiredMetrics.every((name) => metricText.includes(name)) &&
    metricText.includes("/connect") &&
    traceText.includes("http.server.request") &&
    traceText.includes("/connect")
  ) {
    finalShutdownExportsReceived = true;
    break;
  }
  assert.equal(
    childExited,
    false,
    "standalone server exited before final OTLP exports completed",
  );
  await new Promise((resolveWait) => setTimeout(resolveWait, 100));
}

assert.equal(
  finalShutdownExportsReceived,
  true,
  `final telemetry was not exported during shutdown; OTLP paths=${payloads
    .map((payload) => payload.path)
    .join(",")}`,
);
assert.equal(
  childExited,
  false,
  "standalone server did not await collector acknowledgement",
);
const refusedConnection = await new Promise((resolveConnection, reject) => {
  const socket = createConnection({
    host: "127.0.0.1",
    port: webAddress.port,
  });
  socket.setTimeout(1_000);
  socket.once("connect", () => {
    socket.destroy();
    reject(new Error("standalone server accepted a new post-SIGTERM connection"));
  });
  socket.once("error", resolveConnection);
  socket.once("timeout", () => {
    socket.destroy();
    reject(new Error("post-SIGTERM connection attempt did not resolve"));
  });
});
assert.equal(
  refusedConnection.code,
  "ECONNREFUSED",
  `unexpected post-SIGTERM connection result: ${refusedConnection.code}`,
);
assert.ok(
  heldCollectorResponses.length >= 2,
  "shutdown did not attempt both metric and trace exports",
);
holdCollectorResponses = false;
for (const response of heldCollectorResponses) {
  acknowledgeExport(response);
}
const exitResult = await Promise.race([
  childExit,
  new Promise((_, reject) =>
    setTimeout(() => reject(new Error("server did not stop after SIGTERM")), 7_000),
  ),
]);
assert.deepEqual(
  exitResult,
  { code: 143, signal: null },
  "telemetry shutdown changed Next's SIGTERM exit semantics",
);
await new Promise((resolveClose, reject) =>
  collector.close((error) => (error ? reject(error) : resolveClose())),
);

const metricPayload = payloads
  .filter((payload) => payload.path === "/v1/metrics")
  .map((payload) => payload.body.toString("utf8"))
  .join("\n");
const decodedMetrics = decodeMetricTypes(
  payloads
    .filter((payload) => payload.path === "/v1/metrics")
    .map((payload) => payload.body),
);
const tracePayload = payloads
  .filter((payload) => payload.path === "/v1/traces")
  .map((payload) => payload.body.toString("utf8"))
  .join("\n");

for (const name of requiredMetrics) {
  assert.ok(
    decodedMetrics.has(name),
    `missing OTLP metric ${name}; observed ${[
      ...decodedMetrics.keys(),
    ].join(", ")}`,
  );
}
const expectedMetricTypes = new Map([
  ["deephost.web.health.request.duration", { monotonic: false, type: "histogram" }],
  ["deephost.web.health.requests", { monotonic: true, type: "sum" }],
  ["deephost.web.process.cpu.time", { monotonic: true, type: "sum" }],
  ["deephost.web.process.event_loop.utilization", { monotonic: false, type: "gauge" }],
  ["deephost.web.process.memory.usage", { monotonic: false, type: "gauge" }],
  ["deephost.web.process.uptime", { monotonic: false, type: "gauge" }],
  ["deephost.web.server.request.duration", { monotonic: false, type: "histogram" }],
  ["deephost.web.server.requests", { monotonic: true, type: "sum" }],
  ["deephost.web.server.starts", { monotonic: true, type: "sum" }],
]);
for (const [name, expectedType] of expectedMetricTypes) {
  assert.deepEqual(
    decodedMetrics.get(name),
    expectedType,
    `unexpected OTLP data type for ${name}`,
  );
}
assert.ok(
  tracePayload.includes("http.server.request"),
  `missing sanitized root trace; OTLP paths=${payloads
    .map((payload) => payload.path)
    .join(",")} server=${output.trim()}`,
);
assert.ok(
  metricPayload.includes("/domains"),
  "request metrics missed the unsampled /domains trace",
);
assert.ok(
  !tracePayload.includes("/domains"),
  "traceidratio allowed an incoming sampled bit to force trace export",
);
for (const sensitive of [secretMarker, dnsMarker, traceStateMarker]) {
  assert.ok(!metricPayload.includes(sensitive), "metrics leaked request data");
  assert.ok(!tracePayload.includes(sensitive), "traces leaked request data");
  assert.ok(!output.includes(sensitive), "server output leaked request data");
}

console.log(
  `observability smoke passed (${requiredMetrics.length} metrics, ${payloads.length} OTLP exports)`,
);
