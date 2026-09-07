# Observability

Deep Hosting exports server-side metrics and sampled, privacy-sanitized traces
through OpenTelemetry Protocol (OTLP). The production Helm chart runs two
OpenTelemetry Collector replicas and exposes their Prometheus views for the
cluster monitoring plane.

```text
control ───┐
authority ─┼─ OTLP/HTTP :4318 ─> dns-otel-collector (2 replicas)
web ───────┘                         │
                                     ├─ application + span metrics :8889
                                     └─ collector self-metrics     :8888
                                                   │
                              dns-otel-metrics (headless Service)
                                                   │ DNS A discovery
                                                   v
                                  deephost Prometheus in VKE
```

The normal collector Service uses client-IP affinity so a process's cumulative
metric stream normally stays on one replica. The separate headless Service is
important: Prometheus discovers and scrapes every collector pod instead of
hitting one random backend through a ClusterIP.

## Signal and privacy contract

- The Go control and authority processes emit OTLP metrics for HTTP, Connect
  RPC, authentication, control mutations, bbolt transactions, snapshot build,
  transfer, validation, cache and publication, DNS queries, and Go runtime
  health.
- The Next.js process emits request, health, server-error, start, CPU, memory,
  uptime, and event-loop metrics. Browser telemetry is deliberately disabled:
  the operator token and managed DNS data never enter a browser telemetry SDK.
- Sampled Go and Next.js spans are accepted only to derive bounded span metrics.
  Raw traces are neither logged nor retained by the in-chart collector.
- DNS names, record values, zone or record IDs, request bodies, URL query
  strings, headers, bearer tokens, client addresses, exception messages, and
  stack traces are excluded. Every application-controlled attribute is reduced
  to an explicit allowlist before export.
- Resource labels are limited to deployment metadata such as service,
  environment, namespace, component, version, and pod name. Pod UID is omitted
  to avoid needless churn.
- Application logs continue to use stdout/stderr. `OTEL_LOGS_EXPORTER=none` is
  intentional; log collection is a separate cluster concern.

Changing that contract requires a cardinality and privacy review plus a smoke
test containing secret and DNS-name markers. In particular, never add qname,
zone name, record value, raw route, URL, peer IP, or arbitrary error text as a
metric label or span attribute.

## Enable locally

With no exporter configuration the SDKs are no-ops and perform no network
activity. A local collector using OTLP/HTTP can be selected with standard
OpenTelemetry variables:

```sh
export OTEL_SERVICE_NAME=deephost-local
export OTEL_RESOURCE_ATTRIBUTES='deployment.environment.name=development'
export OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4318
export OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf
export OTEL_EXPORTER_OTLP_COMPRESSION=gzip
export OTEL_METRICS_EXPORTER=otlp
export OTEL_TRACES_EXPORTER=otlp
export OTEL_METRIC_EXPORT_INTERVAL=15000
export OTEL_METRIC_EXPORT_TIMEOUT=10000
export OTEL_TRACES_SAMPLER=traceidratio
export OTEL_TRACES_SAMPLER_ARG=0.1
```

Set `OTEL_METRICS_EXPORTER=none`, `OTEL_TRACES_EXPORTER=none`, or
`OTEL_SDK_DISABLED=true` to disable signals. The supported application export
protocol is `http/protobuf`; the collector also accepts OTLP/gRPC from other
trusted in-cluster producers.

## Helm and VKE

Production values must enable the telemetry plane:

```yaml
observability:
  enabled: true
  environment: production
  collector:
    enabled: true
    replicaCount: 2
    networkPolicy:
      enabled: true
      monitoringNamespaceSelector:
        kubernetes.io/metadata.name: deephost
      monitoringPodSelector:
        app: deephost-prometheus
  alerts:
    enabled: true
```

The chart injects the SDK configuration into control, authority, and web pods.
It deploys a memory limiter and bounded batch processor, expires abandoned
Prometheus series after a short failover window, caps span-metric cardinality,
and accepts OTLP only from those three application components. Ports 8888 and
8889 are admitted only from the selected monitoring pod.

The chart can render `ServiceMonitor` and `PrometheusRule` objects when those
CRDs are installed. The current VKE Prometheus is a standalone, Paprika-managed
Prometheus rather than Prometheus Operator, so those objects are portability
artifacts and are not its source of truth. The companion Deephost integration
under `deploy/deephost` adds two `dns_sd_configs` jobs for the headless Service,
loads the same alert rules, and provisions the dashboard into the existing
Deephost Grafana and its existing `deephost-prometheus` datasource. It does not
deploy a second Grafana. Apply that change to the Deephost source repository;
do not patch the live Prometheus or Grafana ConfigMaps because Paprika
self-healing will revert them.

The observed VKE monitoring installation has no Alertmanager. Rules can be
evaluated and viewed in Prometheus/Grafana, but no notification is delivered
until the platform provisions and configures an alert receiver.

## Metric catalog

Application instruments use stable OpenTelemetry names. The collector's
Prometheus exporter converts dots to underscores, appends unit suffixes where
appropriate, and appends `_total` to counters.

| Area | OpenTelemetry instruments | Bounded dimensions |
| --- | --- | --- |
| DNS | `deephost.dns.queries`, `deephost.dns.query.duration`, `deephost.dns.response.size`, `deephost.dns.responses.truncated`, `deephost.dns.write.failures` | transport, RR type, response code |
| HTTP/auth | `deephost.http.server.requests`, `deephost.http.server.duration`, `deephost.http.auth.failures` | method, route template, status, audience, reason |
| Connect | `deephost.rpc.server.requests`, `deephost.rpc.server.duration` | service, method, Connect status |
| Control | `deephost.control.mutations`, `deephost.control.mutation.duration` | entity, operation, outcome, error class |
| Store | `deephost.store.transactions`, `deephost.store.transaction.duration` | operation, read/write, outcome |
| Snapshot | `deephost.snapshot.*` build, admission, size, fetch, apply, cache, checksum, loaded, ready, and age instruments | operation, source, cache result, bounded outcome |
| Authority | `deephost.authoritative.snapshot.compiles`, `deephost.authoritative.snapshot.compile.duration`, `deephost.authoritative.snapshot.publishes` | outcome |
| Inventory | `deephost.inventory.zones`, `deephost.inventory.records` | resource labels only |
| Readiness | `deephost.readiness.checks` | ready/not-ready and bounded reason |
| Web | `deephost.web.*` request, health, error, process, and event-loop instruments | route template, method, status class, fixed process kind |
| Runtime | standard OpenTelemetry Go runtime metrics plus the bounded web process metrics | fixed runtime kind/type only |
| Derived spans | `deephost.trace.*` calls and duration | service, span kind/status, bounded HTTP method/status and RPC method; span name excluded |

Prometheus label names are normalized in the same way, for example
`dns.response.code` becomes `dns_response_code`, `service.name` becomes
`service_name`, and `k8s.pod.name` becomes `k8s_pod_name`.

## First operational queries

Use these as smoke queries after the first scrape; dashboard and alert rules
contain the production versions with explicit windows and namespace matchers.

```promql
# Per-second authoritative traffic by response code.
sum by (dns_response_code) (rate(deephost_dns_queries_total[5m]))

# Authoritative p95 request latency.
histogram_quantile(
  0.95,
  sum by (le) (rate(deephost_dns_query_duration_seconds_bucket[5m]))
)

# Snapshot age and readiness by authority pod.
max by (k8s_pod_name) (deephost_snapshot_age_seconds)
min by (k8s_pod_name) (deephost_snapshot_ready)

# Control mutation failures by operation and stable error class.
sum by (operation, error_type) (
  rate(deephost_control_mutations_total{outcome="error"}[15m])
)

# Collector ingestion failures or refused metric points.
sum(rate(otelcol_receiver_refused_metric_points[5m]))
```

Always aggregate application counters across collector scrape targets. The
`instance` label identifies the collector holding a stream, not the Deep Hosting
application instance; use `k8s_pod_name` for the latter.

## Verification and rollout

1. Render production values and run
   `scripts/tests/observability-chart-test.sh`. Confirm the normal OTLP Service,
   headless scrape Service, two collector replicas, memory/cardinality guards,
   network policy, and all three SDK environment blocks.
2. Validate the rendered collector configuration with the exact pinned
   `otel/opentelemetry-collector-contrib` image before updating Paprika.
3. Validate the Prometheus rule file with `promtool check rules` and render the
   Deephost chart integration from a clean source checkout.
4. Start the collector locally, send real Go DNS/API and Next.js health/page
   traffic through it, then scrape 8889. Assert expected series exist and that
   secret, zone-name, query-string, and record-value markers do not.
5. Deploy the Deephost Prometheus/Grafana integration and Deep Hosting
   application from pinned source revisions. In Prometheus, verify both
   collector jobs under `/targets` and confirm the expected number of healthy
   collector endpoints.
   In the existing Grafana, open **DeepHost / DeepHost Operations** and confirm
   its scrape, collector, authority, control, web, and alert-state panels.
6. Run the smoke queries above, deliberately exercise UDP/TCP DNS and one
   authenticated mutation, and confirm counters advance on the expected pod.
7. Test one collector rollout and one authority rollout. Metrics should move
   between collector targets without a long-lived duplicate series, and DNS
   serving must not depend on collector availability.

Telemetry is fail-open for the DNS and control data paths: a collector outage
causes bounded export loss and SDK error logging, not DNS unavailability.
Collector readiness, scrape health, and missing-series alerts make that loss
visible. Component telemetry still cannot prove that a registrar delegation,
Vultr load balancer, Internet path, or remote resolver works; retain the
external UDP/TCP canary and delegation checks from the production launch
runbook.
