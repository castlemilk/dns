{{/* Expand the chart name. */}}
{{- define "dns.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* Create a release-qualified name, avoiding duplicate chart names. */}}
{{- define "dns.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/* Chart name and version, safe for use as a label. */}}
{{- define "dns.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* Stable labels used by workload selectors. Expects root and component. */}}
{{- define "dns.selectorLabels" -}}
app.kubernetes.io/name: {{ include "dns.name" .root }}
app.kubernetes.io/instance: {{ .root.Release.Name }}
app.kubernetes.io/component: {{ .component }}
{{- end }}

{{/* Standard labels. Expects root and component. */}}
{{- define "dns.labels" -}}
{{- $standard := dict
  "helm.sh/chart" (include "dns.chart" .root)
  "app.kubernetes.io/name" (include "dns.name" .root)
  "app.kubernetes.io/instance" .root.Release.Name
  "app.kubernetes.io/component" .component
  "app.kubernetes.io/version" .root.Chart.AppVersion
  "app.kubernetes.io/managed-by" .root.Release.Service
  "app.kubernetes.io/part-of" (include "dns.name" .root)
}}
{{- toYaml (mergeOverwrite (dict) .root.Values.commonLabels $standard) }}
{{- end }}

{{/* Resource name with a component suffix. Expects root and suffix. */}}
{{- define "dns.resourceName" -}}
{{- printf "%s-%s" (include "dns.fullname" .root) .suffix | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* Control PVC name, supporting an existing claim. */}}
{{- define "dns.controlClaimName" -}}
{{- if .Values.control.persistence.existingClaim }}
{{- .Values.control.persistence.existingClaim }}
{{- else }}
{{- include "dns.resourceName" (dict "root" . "suffix" "control-data") }}
{{- end }}
{{- end }}

{{/*
Bytes the uploads emptyDir must be able to hold, derived from the admission
cap the control plane actually enforces. The server admits a folder while
staged + in-flight + maxBytes stays within maxTotalBytes, so the staged
directories peak at maxTotalBytes + maxBytes; the deployer then packs each
upload into a .tar.zst beside it in the same directory. The staged accounting
now counts finished archives, but the one being written grows between two
admission reads, so the whole figure is doubled to cover it. Deriving this
from maxBytes alone silently assumed maxTotalBytes == 2 * maxBytes and let a
schema-valid pair render a quota far below what the server would stage,
evicting the single control pod that holds dns.db, platform.db and the
snapshot feed.
*/}}
{{- define "dns.uploadsSizeLimit" -}}
{{- $uploads := .Values.platform.hosting.uploads -}}
{{- printf "%d" (mul (add (int64 $uploads.maxTotalBytes) (int64 $uploads.maxBytes)) 2) -}}
{{- end }}

{{/* Render an image, preferring an immutable digest. Expects image. */}}
{{- define "dns.image" -}}
{{- if .digest -}}
{{- printf "%s@%s" .repository .digest -}}
{{- else -}}
{{- printf "%s:%s" .repository (required "image tag or digest is required" .tag) -}}
{{- end -}}
{{- end }}

{{/* In-cluster control snapshot endpoint used by authority pods. */}}
{{- define "dns.snapshotURL" -}}
{{- if .Values.authority.snapshot.url -}}
{{- .Values.authority.snapshot.url -}}
{{- else -}}
{{- printf "http://%s.%s.svc.cluster.local:%v/internal/v1/snapshot" (include "dns.resourceName" (dict "root" . "suffix" "control")) .Release.Namespace .Values.control.service.port -}}
{{- end -}}
{{- end }}

{{/*
Every Connect service prefix routed to control. The DNS prefix stays a
schema-pinned value so existing values files keep validating; the platform
services are chart-owned and derived here so a new prefix is added in one
place.
*/}}
{{- define "dns.apiPathPrefixes" -}}
{{- $prefixes := list
  .Values.httpRoute.apiPathPrefix
  "/platform.v1.PlatformService"
  "/hosting.v1.HostingService"
  "/mail.v1.MailService"
  "/billing.v1.BillingService"
  "/activity.v1.ActivityService"
}}
{{- toYaml $prefixes -}}
{{- end }}

{{/* In-cluster control API base URL used by the web pod's landing-page fetch. */}}
{{- define "dns.controlURL" -}}
{{- printf "http://%s.%s.svc.cluster.local:%v" (include "dns.resourceName" (dict "root" . "suffix" "control")) .Release.Namespace .Values.control.service.port -}}
{{- end }}

{{/*
Platform state and engine environment for the control container. Engine
credentials are always secretKeyRef; every other value is a plain setting an
operator may read. A disabled engine renders nothing at all, because the
server treats a lone variable from an engine block as a partial block and
refuses to start.
*/}}
{{- define "dns.platformEnv" -}}
{{- $platform := .Values.platform }}
- name: DNS_PLATFORM_PATH
  value: {{ $platform.storePath | quote }}
- name: ACTIVITY_MAX_EVENTS
  value: {{ printf "%d" (int64 $platform.activity.maxEvents) | quote }}
{{- if $platform.hosting.enabled }}
- name: HOSTING_API_URL
  value: {{ $platform.hosting.apiUrl | quote }}
- name: HOSTING_API_TOKEN
  valueFrom:
    secretKeyRef:
      name: {{ $platform.hosting.existingSecret }}
      key: {{ $platform.hosting.apiTokenKey }}
- name: HOSTING_TENANT
  value: {{ $platform.hosting.tenant | quote }}
{{- with $platform.hosting.gatewayHostname }}
- name: HOSTING_GATEWAY_HOSTNAME
  value: {{ . | quote }}
{{- end }}
{{- with $platform.hosting.gatewayAddresses }}
- name: HOSTING_GATEWAY_ADDRESSES
  value: {{ join "," . | quote }}
{{- end }}
{{- with $platform.hosting.gatewayAllowedCidrs }}
- name: HOSTING_GATEWAY_ALLOWED_CIDRS
  value: {{ join "," . | quote }}
{{- end }}
{{- with $platform.hosting.gatewayResolver }}
- name: HOSTING_GATEWAY_RESOLVER
  value: {{ . | quote }}
{{- end }}
{{- with $platform.hosting.appsSuffix }}
- name: HOSTING_APPS_SUFFIX
  value: {{ . | quote }}
- name: HOSTING_APPS_TLS_SECRET
  value: {{ $platform.hosting.appsTlsSecret | quote }}
{{- end }}
- name: HOSTING_BUILD_DEADLINE
  value: {{ $platform.hosting.buildDeadline | quote }}
- name: HOSTING_UPLOADS_ENABLED
  value: {{ ternary "true" "false" $platform.hosting.uploads.enabled | quote }}
{{- if $platform.hosting.uploads.enabled }}
- name: HOSTING_UPLOAD_MAX_BYTES
  value: {{ printf "%d" (int64 $platform.hosting.uploads.maxBytes) | quote }}
- name: HOSTING_UPLOAD_MAX_TOTAL_BYTES
  value: {{ printf "%d" (int64 $platform.hosting.uploads.maxTotalBytes) | quote }}
- name: HOSTING_UPLOAD_DIR
  value: "/uploads"
{{- end }}
{{- end }}
{{- if $platform.mail.enabled }}
- name: MAIL_API_URL
  value: {{ $platform.mail.apiUrl | quote }}
- name: MAIL_API_TOKEN
  valueFrom:
    secretKeyRef:
      name: {{ $platform.mail.existingSecret }}
      key: {{ $platform.mail.apiTokenKey }}
- name: MAIL_HOSTNAME
  value: {{ $platform.mail.hostname | quote }}
- name: MAIL_MX_PRIORITY
  value: {{ printf "%d" (int64 $platform.mail.mxPriority) | quote }}
{{- with $platform.mail.spfInclude }}
- name: MAIL_SPF_INCLUDE
  value: {{ . | quote }}
{{- end }}
- name: MAIL_DMARC_POLICY
  value: {{ $platform.mail.dmarcPolicy | quote }}
{{- with $platform.mail.reportAddress }}
- name: MAIL_REPORT_ADDRESS
  value: {{ . | quote }}
{{- end }}
- name: MAIL_MAILBOXES_PER_DOMAIN
  value: {{ printf "%d" (int64 $platform.mail.mailboxesPerDomain) | quote }}
- name: MAIL_DEFAULT_QUOTA_BYTES
  value: {{ printf "%d" (int64 $platform.mail.defaultQuotaBytes) | quote }}
{{- with $platform.mail.webhookSecretKey }}
- name: MAIL_WEBHOOK_SECRET
  valueFrom:
    secretKeyRef:
      name: {{ $platform.mail.existingSecret }}
      key: {{ . }}
{{- end }}
{{- with $platform.mail.webhookSignatureKeyKey }}
- name: MAIL_WEBHOOK_SIGNATURE_KEY
  valueFrom:
    secretKeyRef:
      name: {{ $platform.mail.existingSecret }}
      key: {{ . }}
{{- end }}
{{- end }}
{{- if $platform.billing.provider }}
- name: BILLING_PROVIDER
  value: {{ $platform.billing.provider | quote }}
- name: BILLING_PUBLIC_URL
  value: {{ $platform.billing.publicUrl | quote }}
- name: BILLING_CUSTOMER_EMAIL
  value: {{ $platform.billing.customerEmail | quote }}
- name: STRIPE_SECRET_KEY
  valueFrom:
    secretKeyRef:
      name: {{ $platform.billing.existingSecret }}
      key: {{ $platform.billing.secretKeyKey }}
- name: STRIPE_WEBHOOK_SECRET
  valueFrom:
    secretKeyRef:
      name: {{ $platform.billing.existingSecret }}
      key: {{ $platform.billing.webhookSecretKey }}
- name: STRIPE_PRICE_ID
  valueFrom:
    secretKeyRef:
      name: {{ $platform.billing.existingSecret }}
      key: {{ $platform.billing.priceIdKey }}
{{- end }}
{{- end }}

{{/* Stable in-cluster OpenTelemetry Collector Service endpoint. */}}
{{- define "dns.otelEndpoint" -}}
{{- if .Values.observability.otlp.endpoint -}}
{{- .Values.observability.otlp.endpoint -}}
{{- else -}}
{{- printf "http://%s.%s.svc.cluster.local:4318" (include "dns.resourceName" (dict "root" . "suffix" "otel-collector")) .Release.Namespace -}}
{{- end -}}
{{- end }}

{{/*
Standard OpenTelemetry SDK environment shared by each component. Expects root
and component. Pod name is useful for replica-level authority freshness; pod
UID is deliberately omitted to avoid needless cardinality.
*/}}
{{- define "dns.otelEnv" -}}
- name: POD_NAMESPACE
  valueFrom:
    fieldRef:
      fieldPath: metadata.namespace
- name: POD_NAME
  valueFrom:
    fieldRef:
      fieldPath: metadata.name
- name: OTEL_SERVICE_NAME
  value: {{ printf "simpledns-%s" .component | quote }}
- name: OTEL_RESOURCE_ATTRIBUTES
  value: {{ printf "service.namespace=$(POD_NAMESPACE),service.version=%s,deployment.environment.name=%s,simpledns.component=%s,k8s.namespace.name=$(POD_NAMESPACE),k8s.pod.name=$(POD_NAME)" .root.Chart.AppVersion .root.Values.observability.environment .component | quote }}
- name: OTEL_EXPORTER_OTLP_ENDPOINT
  value: {{ include "dns.otelEndpoint" .root | quote }}
- name: OTEL_EXPORTER_OTLP_PROTOCOL
  value: {{ .root.Values.observability.otlp.protocol | quote }}
- name: OTEL_EXPORTER_OTLP_COMPRESSION
  value: {{ .root.Values.observability.otlp.compression | quote }}
- name: OTEL_METRICS_EXPORTER
  value: "otlp"
- name: OTEL_TRACES_EXPORTER
  value: "otlp"
- name: OTEL_LOGS_EXPORTER
  value: "none"
- name: OTEL_METRIC_EXPORT_INTERVAL
  value: {{ printf "%d" (int64 .root.Values.observability.otlp.metricExportIntervalMilliseconds) | quote }}
- name: OTEL_METRIC_EXPORT_TIMEOUT
  value: {{ printf "%d" (int64 .root.Values.observability.otlp.metricExportTimeoutMilliseconds) | quote }}
- name: OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE
  value: "cumulative"
- name: OTEL_PROPAGATORS
  value: "tracecontext,baggage"
- name: OTEL_TRACES_SAMPLER
  # Do not trust a public caller's sampled traceparent to bypass our cap.
  # Trace-ID ratio sampling remains deterministic across local child spans.
  value: "traceidratio"
- name: OTEL_TRACES_SAMPLER_ARG
  value: {{ printf "%v" .root.Values.observability.otlp.traceSampleRatio | quote }}
{{- end }}

{{/*
Reject an extraEnv entry that would shadow a chart-owned variable. Expects
component and entries. The prefix list is the same for every component: the
control plane owns the engine variables, and an authority or web pod that
carries one is a misconfiguration whichever component it lands on.
*/}}
{{- define "dns.validateExtraEnv" -}}
{{- $component := .component -}}
{{- range $index, $entry := .entries -}}
{{- $name := get $entry "name" | default "" -}}
{{- if or (hasPrefix "DNS_" $name) (hasPrefix "OTEL_" $name) (hasPrefix "HOSTING_" $name) (hasPrefix "MAIL_" $name) (hasPrefix "STRIPE_" $name) (hasPrefix "BILLING_" $name) (hasPrefix "ACTIVITY_" $name) (eq $name "POD_NAMESPACE") (eq $name "POD_NAME") -}}
{{- fail (printf "%s.extraEnv[%d].name %q is reserved; DNS_*, OTEL_*, HOSTING_*, MAIL_*, STRIPE_*, BILLING_*, ACTIVITY_*, and downward API variables are owned by the chart" $component $index $name) -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
Refuse an extraEnvFrom list. The chart cannot read the keys inside a Secret or
a ConfigMap it only holds a reference to, so envFrom is a hole in the name
filter dns.validateExtraEnv enforces: a configMapRef would put the DeepHost
bearer, the Stalwart API key and STRIPE_SECRET_KEY in plaintext in a namespace
object, and either reference could set HOSTING_API_URL and friends and turn on
an engine the chart's own guards refused to configure. Nothing envFrom can do
is beyond an extraEnv entry with a secretKeyRef, which is name-filtered, so the
key is gone from values.yaml and a caller that still sets it is told why.
Expects component and entries.
*/}}
{{- define "dns.rejectExtraEnvFrom" -}}
{{- if .entries -}}
{{- fail (printf "%s.extraEnvFrom is not supported; the chart cannot filter the names inside a referenced Secret or ConfigMap, so engine and chart-owned variables could be smuggled into the pod. Use %s.extraEnv with a valueFrom.secretKeyRef instead" .component .component) -}}
{{- end -}}
{{- end }}

{{/*
Bytes in a Kubernetes quantity, or "" when it is not a plain integer with an
optional binary or decimal suffix. Returning "" keeps callers from failing a
render on a shape this helper cannot read (1.5Gi, 2e9) rather than guessing.
Expects quantity.
*/}}
{{- define "dns.quantityBytes" -}}
{{- $q := printf "%v" .quantity -}}
{{- $units := dict "Ki" 1024 "Mi" 1048576 "Gi" 1073741824 "Ti" 1099511627776 "k" 1000 "M" 1000000 "G" 1000000000 "T" 1000000000000 -}}
{{- if regexMatch `^[0-9]+$` $q -}}
{{- $q -}}
{{- else -}}
{{- $suffix := regexFind `[A-Za-z]+$` $q -}}
{{- $digits := trimSuffix $suffix $q -}}
{{- if and (regexMatch `^[0-9]+$` $digits) (hasKey $units $suffix) -}}
{{- printf "%d" (mul (int64 $digits) (int64 (get $units $suffix))) -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
Refuse an engine endpoint that is neither HTTPS nor an in-cluster Service
address. The schema cannot see .Values.production, so this guard is the one
that runs in a production render. Expects field and url.
*/}}
{{- define "dns.requireSecureEngineURL" -}}
{{- if not (regexMatch `^(https://|http://[^/]+\.svc(\.cluster\.local)?(:|/|$))` .url) -}}
{{- fail (printf "production=true requires %s to use https:// or an in-cluster .svc address" .field) -}}
{{- end -}}
{{- end }}

{{/* Fail closed on topology values that would violate the runtime contract. */}}
{{- define "dns.validate" -}}
{{- include "dns.validateExtraEnv" (dict "component" "control" "entries" .Values.control.extraEnv) -}}
{{- include "dns.validateExtraEnv" (dict "component" "authority" "entries" .Values.authority.extraEnv) -}}
{{- include "dns.validateExtraEnv" (dict "component" "web" "entries" .Values.web.extraEnv) -}}
{{- include "dns.rejectExtraEnvFrom" (dict "component" "control" "entries" .Values.control.extraEnvFrom) -}}
{{- include "dns.rejectExtraEnvFrom" (dict "component" "authority" "entries" .Values.authority.extraEnvFrom) -}}
{{- include "dns.rejectExtraEnvFrom" (dict "component" "web" "entries" .Values.web.extraEnvFrom) -}}
{{- $platform := .Values.platform -}}
{{- if not (hasPrefix "/data/" $platform.storePath) -}}
{{- fail "platform.storePath must live on the control data volume under /data/" -}}
{{- end -}}
{{- if eq $platform.storePath "/data/dns.db" -}}
{{- fail "platform.storePath must differ from the zone store at /data/dns.db" -}}
{{- end -}}
{{- if $platform.hosting.enabled -}}
{{- if not $platform.hosting.apiUrl -}}
{{- fail "platform.hosting.enabled requires platform.hosting.apiUrl" -}}
{{- end -}}
{{- if not $platform.hosting.existingSecret -}}
{{- fail "platform.hosting.enabled requires platform.hosting.existingSecret; the chart never renders engine tokens" -}}
{{- end -}}
{{- if not $platform.hosting.apiTokenKey -}}
{{- fail "platform.hosting.apiTokenKey is required" -}}
{{- end -}}
{{- if and (not $platform.hosting.gatewayHostname) (eq (len $platform.hosting.gatewayAddresses) 0) -}}
{{- fail "platform.hosting.enabled requires platform.hosting.gatewayHostname or a non-empty platform.hosting.gatewayAddresses" -}}
{{- end -}}
{{- if lt (int $platform.hosting.uploads.maxTotalBytes) (int $platform.hosting.uploads.maxBytes) -}}
{{- fail "platform.hosting.uploads.maxTotalBytes must be at least uploads.maxBytes" -}}
{{- end -}}
{{- if $platform.hosting.uploads.enabled -}}
{{- $needed := int64 (include "dns.uploadsSizeLimit" .) -}}
{{- $ephemeral := dig "limits" "ephemeral-storage" "" (.Values.control.resources | default dict) -}}
{{- if $ephemeral -}}
{{- $limit := include "dns.quantityBytes" (dict "quantity" $ephemeral) -}}
{{- if and $limit (lt (int64 $limit) $needed) -}}
{{- fail (printf "control.resources.limits.ephemeral-storage (%s) is below the %d bytes the uploads volume may hold; kubelet would evict the control pod before uploads reach platform.hosting.uploads.maxTotalBytes" $ephemeral $needed) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- if .Values.production -}}
{{- include "dns.requireSecureEngineURL" (dict "field" "platform.hosting.apiUrl" "url" $platform.hosting.apiUrl) -}}
{{- range $platform.hosting.gatewayAddresses -}}
{{- if regexMatch `^(0\.|10\.|127\.|169\.254\.|192\.168\.|172\.(1[6-9]|2[0-9]|3[01])\.|::1$|[fF][cCdD])` . -}}
{{- fail "production=true requires globally routable platform.hosting.gatewayAddresses; loopback, link-local and private addresses are refused" -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- if $platform.hosting.appsSuffix -}}
{{- if not $platform.hosting.appsTlsSecret -}}
{{- fail "platform.hosting.appsSuffix requires platform.hosting.appsTlsSecret; an automatic host is never registered in per-host certificate mode" -}}
{{- end -}}
{{- end -}}
{{- if $platform.mail.enabled -}}
{{- if not $platform.mail.apiUrl -}}
{{- fail "platform.mail.enabled requires platform.mail.apiUrl" -}}
{{- end -}}
{{- if not $platform.mail.hostname -}}
{{- fail "platform.mail.enabled requires platform.mail.hostname" -}}
{{- end -}}
{{- if not $platform.mail.existingSecret -}}
{{- fail "platform.mail.enabled requires platform.mail.existingSecret; the chart never renders engine tokens" -}}
{{- end -}}
{{- if not $platform.mail.apiTokenKey -}}
{{- fail "platform.mail.apiTokenKey is required" -}}
{{- end -}}
{{- if and $platform.mail.webhookSignatureKeyKey (not $platform.mail.webhookSecretKey) -}}
{{- fail "platform.mail.webhookSignatureKeyKey requires platform.mail.webhookSecretKey: without the bearer the delivery-event receiver is not mounted at all" -}}
{{- end -}}
{{- if .Values.production -}}
{{- include "dns.requireSecureEngineURL" (dict "field" "platform.mail.apiUrl" "url" $platform.mail.apiUrl) -}}
{{- end -}}
{{- end -}}
{{- if $platform.billing.provider -}}
{{- if ne $platform.billing.provider "stripe" -}}
{{- fail "platform.billing.provider must be empty or stripe; the fake provider is a local-development mode only" -}}
{{- end -}}
{{- if not (hasPrefix "https://" $platform.billing.publicUrl) -}}
{{- fail "platform.billing.provider=stripe requires an https platform.billing.publicUrl" -}}
{{- end -}}
{{- if not $platform.billing.customerEmail -}}
{{- fail "platform.billing.provider=stripe requires platform.billing.customerEmail" -}}
{{- end -}}
{{- if not $platform.billing.existingSecret -}}
{{- fail "platform.billing.provider=stripe requires platform.billing.existingSecret; the chart never renders Stripe keys" -}}
{{- end -}}
{{- if or (not $platform.billing.secretKeyKey) (not $platform.billing.webhookSecretKey) (not $platform.billing.priceIdKey) -}}
{{- fail "platform.billing.secretKeyKey, webhookSecretKey, and priceIdKey are required" -}}
{{- end -}}
{{- end -}}
{{- if $platform.billing.webhookRoute.enabled -}}
{{- if not .Values.httpRoute.enabled -}}
{{- fail "platform.billing.webhookRoute.enabled requires httpRoute.enabled=true" -}}
{{- end -}}
{{- if ne $platform.billing.provider "stripe" -}}
{{- fail "platform.billing.webhookRoute.enabled requires platform.billing.provider=stripe" -}}
{{- end -}}
{{- end -}}
{{- if and .Values.observability.collector.enabled (not .Values.observability.enabled) -}}
{{- fail "observability.collector.enabled requires observability.enabled=true" -}}
{{- end -}}
{{- if and .Values.observability.enabled (not .Values.observability.collector.enabled) (not .Values.observability.otlp.endpoint) -}}
{{- fail "observability.enabled requires the in-cluster collector or an explicit observability.otlp.endpoint" -}}
{{- end -}}
{{- if and .Values.observability.collector.enabled .Values.observability.otlp.endpoint -}}
{{- fail "observability.otlp.endpoint must be empty when the in-cluster collector is enabled" -}}
{{- end -}}
{{- if gt (int64 .Values.observability.otlp.metricExportTimeoutMilliseconds) (int64 .Values.observability.otlp.metricExportIntervalMilliseconds) -}}
{{- fail "observability OTLP metric export timeout must not exceed its export interval" -}}
{{- end -}}
{{- if and .Values.observability.collector.podDisruptionBudget.enabled (ge (int .Values.observability.collector.podDisruptionBudget.minAvailable) (int .Values.observability.collector.replicaCount)) -}}
{{- fail "collector podDisruptionBudget.minAvailable must be lower than collector.replicaCount" -}}
{{- end -}}
{{- if ge (int .Values.observability.collector.memoryLimiter.spikeLimitMiB) (int .Values.observability.collector.memoryLimiter.limitMiB) -}}
{{- fail "collector memoryLimiter.spikeLimitMiB must be lower than limitMiB" -}}
{{- end -}}
{{- if gt (int .Values.observability.collector.batch.sendBatchSize) (int .Values.observability.collector.batch.sendBatchMaxSize) -}}
{{- fail "collector batch.sendBatchSize must not exceed sendBatchMaxSize" -}}
{{- end -}}
{{- if and .Values.observability.collector.serviceMonitor.enabled (not .Values.observability.collector.enabled) -}}
{{- fail "collector.serviceMonitor.enabled requires the in-cluster collector" -}}
{{- end -}}
{{- if and .Values.observability.collector.networkPolicy.enabled (not .Values.observability.collector.enabled) -}}
{{- fail "collector.networkPolicy.enabled requires the in-cluster collector" -}}
{{- end -}}
{{- if and .Values.observability.collector.networkPolicy.enabled (eq (len .Values.observability.collector.networkPolicy.monitoringNamespaceSelector) 0) -}}
{{- fail "collector NetworkPolicy requires a non-empty monitoringNamespaceSelector" -}}
{{- end -}}
{{- if and .Values.observability.collector.networkPolicy.enabled (eq (len .Values.observability.collector.networkPolicy.monitoringPodSelector) 0) -}}
{{- fail "collector NetworkPolicy requires a non-empty monitoringPodSelector" -}}
{{- end -}}
{{- if and .Values.observability.alerts.enabled (not .Values.observability.enabled) -}}
{{- fail "observability.alerts.enabled requires observability.enabled=true" -}}
{{- end -}}
{{- if and .Values.observability.alerts.enabled (not .Values.observability.collector.enabled) -}}
{{- fail "observability.alerts.enabled requires the in-cluster collector" -}}
{{- end -}}
{{- if ne (int .Values.control.replicaCount) 1 -}}
{{- fail "control.replicaCount must be exactly 1 because bbolt has one writer" -}}
{{- end -}}
{{- if not .Values.auth.existingSecret -}}
{{- fail "auth.existingSecret is required; the chart never renders bearer tokens" -}}
{{- end -}}
{{- if not .Values.auth.apiBearerTokenKey -}}
{{- fail "auth.apiBearerTokenKey is required" -}}
{{- end -}}
{{- if not .Values.auth.snapshotBearerTokenKey -}}
{{- fail "auth.snapshotBearerTokenKey is required" -}}
{{- end -}}
{{- if eq .Values.auth.apiBearerTokenKey .Values.auth.snapshotBearerTokenKey -}}
{{- fail "API and snapshot bearer tokens must use different Secret keys" -}}
{{- end -}}
{{- if and .Values.authority.podDisruptionBudget.enabled (ge (int .Values.authority.podDisruptionBudget.minAvailable) (int .Values.authority.replicaCount)) -}}
{{- fail "authority.podDisruptionBudget.minAvailable must be lower than authority.replicaCount" -}}
{{- end -}}
{{- if and .Values.nodePortService.enabled .Values.dnsService.enabled -}}
{{- fail "nodePortService.enabled and dnsService.enabled are mutually exclusive" -}}
{{- end -}}
{{- if and .Values.externalIPService.enabled .Values.dnsService.enabled -}}
{{- fail "externalIPService.enabled and dnsService.enabled are mutually exclusive" -}}
{{- end -}}
{{- if and .Values.externalIPService.enabled .Values.nodePortService.enabled -}}
{{- fail "externalIPService.enabled and nodePortService.enabled are mutually exclusive" -}}
{{- end -}}
{{- if and .Values.externalIPService.enabled (eq (len .Values.externalIPService.addresses) 0) -}}
{{- fail "externalIPService.enabled requires at least one address in externalIPService.addresses" -}}
{{- end -}}
{{- if and .Values.externalIPService.enabled (gt (len .Values.externalIPService.addresses) (int .Values.authority.replicaCount)) -}}
{{- fail "externalIPService.addresses must not exceed authority.replicaCount so every published address has an authority to serve it" -}}
{{- end -}}
{{- if and .Values.dnsService.enabled (not .Values.dnsService.acknowledgeVultrSharedHealthCheckRisk) -}}
{{- fail "dnsService.enabled requires dnsService.acknowledgeVultrSharedHealthCheckRisk=true" -}}
{{- end -}}
{{- if .Values.httpRoute.enabled -}}
{{- if not .Values.production -}}
{{- fail "httpRoute.enabled requires production=true so the control API enforces bearer authentication" -}}
{{- end -}}
{{- if not .Values.httpRoute.acknowledgePublicControlPlane -}}
{{- fail "httpRoute.enabled requires httpRoute.acknowledgePublicControlPlane=true" -}}
{{- end -}}
{{- if eq (len .Values.httpRoute.hostnames) 0 -}}
{{- fail "httpRoute.hostnames must contain at least one hostname" -}}
{{- end -}}
{{- end -}}
{{- if .Values.snapshotRoute.enabled -}}
{{- if not .Values.production -}}
{{- fail "snapshotRoute.enabled requires production=true so the snapshot endpoint enforces bearer authentication" -}}
{{- end -}}
{{- if not .Values.snapshotRoute.acknowledgePublicSnapshotFeed -}}
{{- fail "snapshotRoute.enabled requires snapshotRoute.acknowledgePublicSnapshotFeed=true" -}}
{{- end -}}
{{- if not .Values.snapshotRoute.hostname -}}
{{- fail "snapshotRoute.hostname is required when snapshotRoute.enabled=true" -}}
{{- end -}}
{{- end -}}
{{- if .Values.tls.enabled -}}
{{- if not .Values.production -}}
{{- fail "tls.enabled requires production=true" -}}
{{- end -}}
{{- if not .Values.tls.clusterIssuer -}}
{{- fail "tls.clusterIssuer is required when tls.enabled=true" -}}
{{- end -}}
{{- if not .Values.tls.secretName -}}
{{- fail "tls.secretName is required when tls.enabled=true" -}}
{{- end -}}
{{- if eq (len .Values.tls.dnsNames) 0 -}}
{{- fail "tls.dnsNames must contain at least one hostname when tls.enabled=true" -}}
{{- end -}}
{{- if and .Values.snapshotRoute.enabled (not (has .Values.snapshotRoute.hostname .Values.tls.dnsNames)) -}}
{{- fail "tls.dnsNames must include snapshotRoute.hostname when both features are enabled" -}}
{{- end -}}
{{- if .Values.httpRoute.enabled -}}
{{- range .Values.httpRoute.hostnames -}}
{{- if not (has . $.Values.tls.dnsNames) -}}
{{- fail "tls.dnsNames must include every httpRoute hostname when both features are enabled" -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- if .Values.production -}}
{{- if not .Values.observability.enabled -}}
{{- fail "production=true requires observability.enabled=true" -}}
{{- end -}}
{{- if not .Values.observability.collector.enabled -}}
{{- fail "production=true requires the in-cluster OpenTelemetry Collector" -}}
{{- end -}}
{{- if lt (int .Values.observability.collector.replicaCount) 2 -}}
{{- fail "production=true requires at least two OpenTelemetry Collector replicas" -}}
{{- end -}}
{{- if not .Values.observability.collector.image.digest -}}
{{- fail "production=true requires an immutable collector image digest" -}}
{{- end -}}
{{- if not .Values.observability.collector.podDisruptionBudget.enabled -}}
{{- fail "production=true requires the collector PodDisruptionBudget" -}}
{{- end -}}
{{- if not .Values.observability.collector.topologySpread.enabled -}}
{{- fail "production=true requires collector topology spread constraints" -}}
{{- end -}}
{{- if not .Values.observability.collector.prometheusAnnotations.enabled -}}
{{- fail "production=true requires Prometheus scrape annotations as a compatibility fallback" -}}
{{- end -}}
{{- if not .Values.observability.collector.networkPolicy.enabled -}}
{{- fail "production=true requires a collector NetworkPolicy" -}}
{{- end -}}
{{- if not .Values.observability.alerts.enabled -}}
{{- fail "production=true requires observability alerts" -}}
{{- end -}}
{{- if not .Values.control.persistence.enabled -}}
{{- fail "production=true requires control.persistence.enabled=true" -}}
{{- end -}}
{{- if and (not .Values.control.persistence.existingClaim) (not .Values.control.persistence.storageClass) -}}
{{- fail "production=true requires an explicit retaining control.persistence.storageClass (or existingClaim)" -}}
{{- end -}}
{{- if not .Values.authority.cache.persistence.enabled -}}
{{- fail "production=true requires authority.cache.persistence.enabled=true" -}}
{{- end -}}
{{- if not .Values.authority.cache.persistence.storageClass -}}
{{- fail "production=true requires an explicit retaining authority cache storageClass" -}}
{{- end -}}
{{- if lt (int .Values.authority.replicaCount) 3 -}}
{{- fail "production=true requires authority.replicaCount >= 3" -}}
{{- end -}}
{{- if not .Values.authority.podDisruptionBudget.enabled -}}
{{- fail "production=true requires authority.podDisruptionBudget.enabled=true" -}}
{{- end -}}
{{- if lt (int .Values.authority.podDisruptionBudget.minAvailable) 2 -}}
{{- fail "production=true requires authority.podDisruptionBudget.minAvailable >= 2" -}}
{{- end -}}
{{- if not .Values.authority.podAntiAffinity.required -}}
{{- fail "production=true requires required authority pod anti-affinity" -}}
{{- end -}}
{{- if not .Values.authority.topologySpread.enabled -}}
{{- fail "production=true requires authority topology spread constraints" -}}
{{- end -}}
{{- if lt (len .Values.control.nameservers) 2 -}}
{{- fail "production=true requires at least two control.nameservers" -}}
{{- end -}}
{{- range .Values.control.nameservers -}}
{{- $name := trimSuffix "." (lower .) -}}
{{- if or (contains "replace" $name) (eq $name "example.com") (hasSuffix ".example.com" $name) (eq $name "example.net") (hasSuffix ".example.net" $name) (eq $name "example.org") (hasSuffix ".example.org" $name) (eq $name "invalid") (hasSuffix ".invalid" $name) (eq $name "localhost") (hasSuffix ".localhost" $name) (eq $name "test") (hasSuffix ".test" $name) -}}
{{- fail "production nameservers must be real delegated names, not placeholders or reserved test names" -}}
{{- end -}}
{{- end -}}
{{- if not .Values.images.server.digest -}}
{{- fail "production=true requires images.server.digest" -}}
{{- end -}}
{{- if not .Values.images.web.digest -}}
{{- fail "production=true requires images.web.digest" -}}
{{- end -}}
{{- if contains "REPLACE" .Values.auth.existingSecret -}}
{{- fail "production auth.existingSecret placeholder must be replaced" -}}
{{- end -}}
{{- end -}}
{{- end }}
