#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
chart="${repo_root}/charts/dns"
production_values="${chart}/ci/production-values.yaml"
tmp_dir="$(mktemp -d)"
trap 'rm -rf -- "${tmp_dir}"' EXIT

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

assert_fails() {
  if "$@" >/dev/null 2>&1; then
    printf 'expected command to fail:' >&2
    printf ' %q' "$@" >&2
    printf '\n' >&2
    exit 1
  fi
}

default_render="${tmp_dir}/default.yaml"
production_render="${tmp_dir}/production.yaml"
monitoring_render="${tmp_dir}/monitoring.yaml"

helm template dns "${chart}" --namespace dns >"${default_render}"
if grep -Fq 'name: dns-otel-collector' "${default_render}"; then
  fail 'default render unexpectedly enabled the collector'
fi
if grep -Fq 'name: OTEL_SERVICE_NAME' "${default_render}"; then
  fail 'default render unexpectedly injected OpenTelemetry SDK settings'
fi

helm template dns "${chart}" --namespace dns \
  -f "${production_values}" >"${production_render}"

# The CRD-independent path is complete and keeps a scrape-annotation fallback.
grep -Fq 'kind: ConfigMap' "${production_render}" || fail 'collector ConfigMap missing'
grep -Fq 'name: dns-otel-collector' "${production_render}" || fail 'collector resources missing'
grep -Fq 'name: dns-otel-metrics' "${production_render}" || fail 'headless scrape Service missing'
grep -Fq 'clusterIP: None' "${production_render}" || fail 'scrape Service is not headless'
grep -Fq 'prometheus.io/scrape: "true"' "${production_render}" || fail 'annotation fallback missing'
grep -Fq 'port: 8889' "${production_render}" || fail 'application metrics port missing'
grep -Fq 'port: 8888' "${production_render}" || fail 'collector telemetry port missing'
grep -Fq 'kind: NetworkPolicy' "${production_render}" || fail 'collector NetworkPolicy missing'
grep -Fq 'kubernetes.io/metadata.name: deephost' "${production_render}" || fail 'monitoring namespace selector missing'
grep -Fq 'app: deephost-prometheus' "${production_render}" || fail 'monitoring pod selector missing'
grep -Fq 'exporters: [span_metrics]' "${production_render}" || fail 'span-metrics trace sink missing'
grep -Fq -- '- span.name' "${production_render}" || fail 'unbounded span.name dimension is not excluded'
grep -Fq 'aggregation_cardinality_limit: 1000' "${production_render}" || fail 'span cardinality bound missing'
grep -Fq 'metric_expiration: 1m' "${production_render}" || fail 'stale collector metric expiration missing'
grep -Fq 'sessionAffinity: ClientIP' "${production_render}" || fail 'cumulative stream affinity missing'
grep -Fq 'max_request_body_size: 8388608' "${production_render}" || fail 'OTLP HTTP request bound missing'
grep -Fq 'max_recv_msg_size_mib: 8' "${production_render}" || fail 'OTLP gRPC request bound missing'
if grep -Fq 'debug:' "${production_render}"; then
  fail 'collector must not log raw telemetry through a debug exporter'
fi

[[ "$(grep -Fc 'name: OTEL_SERVICE_NAME' "${production_render}")" == 3 ]] || fail 'OTel SDK settings were not injected into all three components'
for service_name in deephost-control deephost-authority deephost-web; do
  grep -Fq "value: \"${service_name}\"" "${production_render}" || fail "missing service.name ${service_name}"
done
[[ "$(grep -Fc 'path: /api/health' "${production_render}")" == 3 ]] || fail 'all web probes must drive the dedicated health telemetry route'
grep -Fq 'http://dns-otel-collector.dns.svc.cluster.local:4318' "${production_render}" || fail 'stable in-cluster OTLP endpoint missing'
grep -Fq 'value: "traceidratio"' "${production_render}" || fail 'untrusted remote parents can bypass the trace sampling cap'
grep -Fq 'k8s.pod.name=$(POD_NAME)' "${production_render}" || fail 'replica identity resource attribute missing'
if grep -Fq 'k8s.pod.uid' "${production_render}"; then
  fail 'pod UID cardinality must not be exported'
fi

# Monitoring CRs appear only when explicitly enabled and advertised by API
# discovery; the headless annotated Service remains the no-CRD fallback.
if grep -Fq 'kind: ServiceMonitor' "${production_render}"; then
  fail 'ServiceMonitor rendered without its API being advertised'
fi
helm template dns "${chart}" --namespace dns \
  -f "${production_values}" \
  --api-versions monitoring.coreos.com/v1/ServiceMonitor \
  --api-versions monitoring.coreos.com/v1/PrometheusRule >"${monitoring_render}"
grep -Fq 'kind: ServiceMonitor' "${monitoring_render}" || fail 'ServiceMonitor missing with CRD capability'
[[ "$(sed -n '/kind: ServiceMonitor/,/^---$/p' "${monitoring_render}" | grep -c '^    - port:')" == 2 ]] || fail 'ServiceMonitor must scrape both collector endpoints'
grep -Fq 'kind: PrometheusRule' "${monitoring_render}" || fail 'PrometheusRule missing with CRD capability'
for alert in DeepHostCollectorUnavailable DeepHostCollectorExporterFailures DeepHostControlUnavailable DeepHostWebUnavailable DeepHostAuthorityTelemetryMissing DeepHostAuthoritySnapshotStale DeepHostSnapshotFailures DeepHostServfailRatioHigh; do
  grep -Fq "alert: ${alert}" "${monitoring_render}" || fail "missing alert ${alert}"
done
grep -Fq 'deephost_web_health_requests_total' "${monitoring_render}" || fail 'web health heartbeat alert metric missing'
grep -Fq 'otelcol_receiver_refused_spans' "${monitoring_render}" || fail 'collector refused-spans loss alert missing'
grep -Fq 'otelcol_receiver_failed_spans' "${monitoring_render}" || fail 'collector failed-spans loss alert missing'

# The actual standalone Deephost Prometheus integration is independently
# parseable and uses headless DNS discovery for both per-replica endpoints.
scrape_fragment="${repo_root}/deploy/deephost/prometheus-scrape-configs.yaml"
rules_file="${repo_root}/deploy/deephost/deephost-rules.yaml"
dashboard="${repo_root}/deploy/deephost/dashboards/deephost.json"
jq empty "${dashboard}"
grep -Fq 'groups:' "${rules_file}" || fail 'standalone Prometheus rule groups missing'
grep -Fq 'alert: DeepHostWebUnavailable' "${rules_file}" || fail 'standalone web availability alert missing'
grep -Fq 'otelcol_receiver_refused_spans' "${rules_file}" || fail 'standalone refused-spans alert missing'
grep -Fq 'otelcol_receiver_failed_spans' "${rules_file}" || fail 'standalone failed-spans alert missing'
[[ "$(grep -Fc 'dns-otel-metrics.dns.svc.cluster.local' "${scrape_fragment}")" == 2 ]] || fail 'both scrape jobs must use headless DNS discovery'
grep -Fq 'port: 8889' "${scrape_fragment}" || fail 'Deephost application metrics scrape missing'
grep -Fq 'port: 8888' "${scrape_fragment}" || fail 'Deephost collector telemetry scrape missing'
jq -e '.editable == false' "${dashboard}" >/dev/null || fail 'provisioned Grafana dashboard must be immutable'
jq -e '.timezone == "utc"' "${dashboard}" >/dev/null || fail 'Grafana dashboard timezone must be UTC'
jq -e '.panels | length == 14' "${dashboard}" >/dev/null || fail 'Grafana dashboard must contain 14 operational panels'
jq -e '[.panels[].targets[]] | length == 24' "${dashboard}" >/dev/null || fail 'Grafana dashboard must contain 24 PromQL targets'
jq -e 'all(.panels[]; .datasource.type == "prometheus" and .datasource.uid == "deephost-prometheus")' "${dashboard}" >/dev/null || fail 'every Grafana panel must bind the existing Prometheus datasource'
jq -e 'all(.panels[].targets[].expr | select(contains("service_name=\"")); contains("service_namespace=\"dns\""))' "${dashboard}" >/dev/null || fail 'service-scoped Grafana queries must also constrain the DNS namespace'
jq -e 'any(.panels[]; .title == "Scrape and replica coverage")' "${dashboard}" >/dev/null || fail 'Grafana scrape and replica coverage panel missing'
jq -e 'any(.panels[].targets[].expr; contains("up{job=\"deephost-otel-metrics\"}"))' "${dashboard}" >/dev/null || fail 'Grafana application scrape coverage query missing'
jq -e 'any(.panels[].targets[].expr; contains("up{job=\"deephost-otel-collector\"}"))' "${dashboard}" >/dev/null || fail 'Grafana collector scrape coverage query missing'
jq -e 'any(.panels[].targets[].expr; contains("count by (instance) (otelcol_process_uptime"))' "${dashboard}" >/dev/null || fail 'Grafana collector replica coverage query missing'
jq -e 'any(.panels[].targets[].expr; contains("count by (k8s_pod_name) (deephost_snapshot_ready"))' "${dashboard}" >/dev/null || fail 'Grafana authority replica coverage query missing'
jq -e 'any(.panels[]; .title == "Firing DeepHost alerts" and any(.targets[].expr; contains("ALERTS{alertname=~\"DeepHost.*\",alertstate=\"firing\"}")))' "${dashboard}" >/dev/null || fail 'Grafana firing-alerts panel missing'
jq -e '(.panels[] | select(.id == 1) | reduce .fieldConfig.overrides[] as $override ({}; .[$override.matcher.options] = ($override.properties[] | select(.id == "thresholds") | .value.steps[] | select(.color == "green") | .value))) == {"A":2,"B":2,"C":2,"D":3}' "${dashboard}" >/dev/null || fail 'Grafana scrape and replica thresholds must match the production topology'
jq -e 'any(.panels[]; .id == 11 and .options.colorMode == "none")' "${dashboard}" >/dev/null || fail 'Grafana inventory values must use neutral coloring'
jq -e 'all(.panels[] | select(.id == 6 or .id == 9) | .targets[].expr; endswith("or on() vector(0)"))' "${dashboard}" >/dev/null || fail 'sparse failure panels must render an explicit zero fallback'
grep -Fq 'deephost_web_health_requests_total' "${dashboard}" || fail 'Grafana web heartbeat query missing'
grep -Fq 'otelcol_receiver_refused_spans' "${dashboard}" || fail 'Grafana refused-spans query missing'
if grep -Fq 'otelcol_receiver_failed_spans' "${dashboard}"; then
  fail 'Grafana must omit the inactive receiver_failed_spans series; alert rules retain defensive coverage'
fi

# Schema and template guards both reject unsafe or internally inconsistent
# observability topology.
assert_fails helm template dns "${chart}" --namespace dns \
  --set observability.enabled=true
assert_fails helm template dns "${chart}" --namespace dns \
  --set observability.collector.enabled=true
assert_fails helm template dns "${chart}" --namespace dns \
  --set observability.enabled=true \
  --set observability.collector.enabled=true \
  --set observability.otlp.endpoint=http://external-collector.example:4318
assert_fails helm template dns "${chart}" --namespace dns \
  --set observability.collector.memoryLimiter.limitMiB=48 \
  --set observability.collector.memoryLimiter.spikeLimitMiB=48
assert_fails helm template dns "${chart}" --namespace dns \
  --set observability.collector.batch.sendBatchSize=4097 \
  --set observability.collector.batch.sendBatchMaxSize=4096
assert_fails helm template dns "${chart}" --namespace dns \
  --set observability.enabled=true \
  --set observability.collector.enabled=true \
  --set observability.collector.networkPolicy.enabled=true
assert_fails helm template dns "${chart}" --namespace dns \
  --set 'web.extraEnv[0].name=OTEL_EXPORTER_OTLP_ENDPOINT' \
  --set 'web.extraEnv[0].value=http://shadow.example:4318'
assert_fails helm template dns "${chart}" --namespace dns \
  --skip-schema-validation \
  --set 'web.extraEnv[0].name=OTEL_EXPORTER_OTLP_ENDPOINT' \
  --set 'web.extraEnv[0].value=http://shadow.example:4318'
assert_fails helm template dns "${chart}" --namespace dns \
  -f "${production_values}" \
  --set observability.enabled=false \
  --set observability.collector.enabled=false \
  --set observability.alerts.enabled=false \
  --set observability.collector.networkPolicy.enabled=false

printf 'observability chart fixtures: PASS\n'
