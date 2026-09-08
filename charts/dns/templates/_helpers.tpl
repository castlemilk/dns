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
--- PUBLIC HOSTNAMES, DERIVED FROM ONE ZONE -----------------------------------

Six values used to name a host in the same zone independently of each other.
The helpers below are the single answer to each of those questions, and every
template asks them rather than reading the value directly, so a zone move is
one edit to domain.base instead of six that must agree.

Two rules hold for all of them, and nothing else is worth remembering:

  1. THE EXPLICIT VALUE WINS. If httpRoute.hostnames, snapshotRoute.hostname,
     tls.dnsNames, control.nameservers, platform.billing.publicUrl or
     platform.mail.hostname is set, that is the answer, domain.base or not.
     Deriving fills an empty value; it never overrides a set one.
  2. NOTHING DERIVES WITHOUT domain.base. With base empty every helper returns
     the explicit value verbatim — including an empty one, which the guards in
     dns.validate then reject with the same message they always did. That is
     what makes this block a no-op for a values file that spells out all six,
     and it is why the helpers never cross-derive one explicit value from
     another: an empty publicUrl with an explicit httpRoute.hostnames still
     fails loudly rather than quietly picking a host.

Helpers returning a list emit a YAML sequence; read one back with
`include "dns.x" . | fromYamlArray`, as httproute.yaml already does for
dns.apiPathPrefixes. Every helper is safe with base empty: it short-circuits
rather than printing a stem-less name like ".example.com" or "dns.".
*/}}

{{/*
The zone every derived name hangs off, or "" when this deployment is
configured with explicit hostnames. Internal: it exists so the "is anything
derived at all" test is written once. A trailing dot is trimmed even though
dns.validate rejects one, because this value is concatenated, not compared.
*/}}
{{- define "dns.domainBase" -}}
{{- trimSuffix "." (.Values.domain.base | default "") -}}
{{- end }}

{{/*
<service>.<base> — the stem this platform's names share — or "" when
domain.base is empty. Internal. domain.service is schema-required and
non-empty, and dns.validate rejects a base with a scheme, a path or a leading
or trailing dot, so this printf cannot produce a malformed stem.
*/}}
{{- define "dns.platformDomain" -}}
{{- $base := include "dns.domainBase" . -}}
{{- if $base -}}
{{- printf "%s.%s" .Values.domain.service $base -}}
{{- end -}}
{{- end }}

{{/*
The public host the console and the Connect API answer on: the first
httpRoute.hostnames entry, else <service>.<base>, else "".

The first entry rather than the whole list because a certificate and a Stripe
redirect each need exactly one origin, and the route's first hostname is the
one an operator who listed several meant as canonical.
*/}}
{{- define "dns.platformHost" -}}
{{- if .Values.httpRoute.hostnames -}}
{{- first .Values.httpRoute.hostnames -}}
{{- else -}}
{{- include "dns.platformDomain" . -}}
{{- end -}}
{{- end }}

{{/*
Every hostname the console HTTPRoute claims, as a YAML list: httpRoute.hostnames
when set, else the single derived console host, else empty. Empty is a valid
answer here — the route is off by default — and dns.validate is what refuses it
when httpRoute.enabled is true.
*/}}
{{- define "dns.httpRouteHostnames" -}}
{{- if .Values.httpRoute.hostnames -}}
{{- toYaml .Values.httpRoute.hostnames -}}
{{- else -}}
{{- toYaml (compact (list (include "dns.platformDomain" .))) -}}
{{- end -}}
{{- end }}

{{/*
The host carrying the authenticated snapshot feed for remote authority fleets:
snapshotRoute.hostname when set, else snapshot.<service>.<base>, else "".

It is a separate name from the console host on purpose — the snapshot route
exposes one exact path and no Connect procedure, and keeping it off the console
hostname keeps that separation visible in the certificate.
*/}}
{{- define "dns.snapshotHost" -}}
{{- if .Values.snapshotRoute.hostname -}}
{{- .Values.snapshotRoute.hostname -}}
{{- else -}}
{{- $stem := include "dns.platformDomain" . -}}
{{- if $stem -}}
{{- printf "snapshot.%s" $stem -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
The public name of the mail host — what the MX points at and what its PTR must
say: platform.mail.hostname when set, else mail.<service>.<base>, else "".
*/}}
{{- define "dns.mailHost" -}}
{{- if .Values.platform.mail.hostname -}}
{{- .Values.platform.mail.hostname -}}
{{- else -}}
{{- $stem := include "dns.platformDomain" . -}}
{{- if $stem -}}
{{- printf "mail.%s" $stem -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
The delegated nameserver names, as a YAML list: control.nameservers when it has
entries, else ns1..ns<domain.nameserverCount>.<service>.<base>, else empty.

The shipped control.nameservers placeholders are entries, so they win — which
is a trap for anyone who sets domain.base and forgets to clear them, and
dns.validate refuses that exact pairing by name rather than letting example.net
reach an NS record.
*/}}
{{- define "dns.nameservers" -}}
{{- if .Values.control.nameservers -}}
{{- toYaml .Values.control.nameservers -}}
{{- else -}}
{{- $stem := include "dns.platformDomain" . -}}
{{- $names := list -}}
{{- if $stem -}}
{{- range $index := until (int .Values.domain.nameserverCount) -}}
{{- $names = append $names (printf "ns%d.%s" (add1 $index) $stem) -}}
{{- end -}}
{{- end -}}
{{- toYaml $names -}}
{{- end -}}
{{- end }}

{{/*
The absolute origin the console is reached at, used for Stripe's return URL and
as the origin its webhooks are aimed at: platform.billing.publicUrl when set,
else https://<console host> once domain.base is set, else "".

https, never http, and never a port: the derived form is only ever the public
edge, which terminates TLS at the Gateway. An operator who needs anything else
sets publicUrl.
*/}}
{{- define "dns.consoleURL" -}}
{{- if .Values.platform.billing.publicUrl -}}
{{- .Values.platform.billing.publicUrl -}}
{{- else -}}
{{- $host := include "dns.platformHost" . -}}
{{- if and $host (include "dns.domainBase" .) -}}
{{- printf "https://%s" $host -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
The names the certificate covers, as a YAML list: tls.dnsNames when set, else
the console hostnames plus the snapshot host, deduplicated, else empty.

The mail host is deliberately absent. cert-manager fails an entire Order when
any one name in it cannot be validated, so folding mail.<...> in before its A
record and MX exist would take the console certificate down with it — a much
worse failure than a mail server without TLS. values.yaml says to add that name
by hand once it resolves.
*/}}
{{- define "dns.tlsDNSNames" -}}
{{- if .Values.tls.dnsNames -}}
{{- toYaml .Values.tls.dnsNames -}}
{{- else if (include "dns.domainBase" .) -}}
{{- $names := concat (include "dns.httpRouteHostnames" . | fromYamlArray) (compact (list (include "dns.snapshotHost" .))) -}}
{{- toYaml (uniq $names) -}}
{{- else -}}
{{- toYaml (list) -}}
{{- end -}}
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
  value: {{ include "dns.mailHost" $ | quote }}
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
  value: {{ include "dns.consoleURL" $ | quote }}
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
  value: {{ printf "deephost-%s" .component | quote }}
- name: OTEL_RESOURCE_ATTRIBUTES
  value: {{ printf "service.namespace=$(POD_NAMESPACE),service.version=%s,deployment.environment.name=%s,deephost.component=%s,k8s.namespace.name=$(POD_NAMESPACE),k8s.pod.name=$(POD_NAME)" .root.Chart.AppVersion .root.Values.observability.environment .component | quote }}
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

{{/*
"true" when a hostname is a shipped placeholder or a name reserved by RFC 2606
and RFC 6761, "" otherwise. Extracted so the production nameserver scan and the
domain.base pairing guard below apply one list rather than two that drift.
Expects host.
*/}}
{{- define "dns.reservedHostname" -}}
{{- $name := trimSuffix "." (lower .host) -}}
{{- if or (contains "replace" $name) (eq $name "example.com") (hasSuffix ".example.com" $name) (eq $name "example.net") (hasSuffix ".example.net" $name) (eq $name "example.org") (hasSuffix ".example.org" $name) (eq $name "invalid") (hasSuffix ".invalid" $name) (eq $name "localhost") (hasSuffix ".localhost" $name) (eq $name "test") (hasSuffix ".test" $name) -}}
true
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
{{/*
domain.base is the root six public hostnames now hang off, so a malformed one
is not one render error, it is six wrong names — and a wrong name fails at
delegation or certificate issuance days later, never here. Reject the shapes
that would concatenate into something that still looks plausible: a scheme or
path (https://example.com renders "dns.https://example.com"), a leading or
trailing dot ("dns..example.com"), a single label with no dot (a delegated zone
always has one), and anything outside the letter-digit-hyphen alphabet.

The schema carries the same shape as a pattern and rejects it first. This
restates it because a pattern mismatch says only that a regex did not match,
and because this is the layer that still runs under --skip-schema-validation.
The bounds on nameserverCount are the other way round — guard only — because
they are meaningful only when something derives from them: with base empty the
value is unused, and failing a render over an unused number is noise.
*/}}
{{- $domain := .Values.domain -}}
{{- if $domain.base -}}
{{- if not (regexMatch `^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)+$` $domain.base) -}}
{{- fail (printf "domain.base %q must be a bare domain name such as example.com: no scheme, no path, no leading or trailing dot, at least one dot, and only letters, digits and hyphens" $domain.base) -}}
{{- end -}}
{{- if not (regexMatch `^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?$` $domain.service) -}}
{{- fail (printf "domain.service %q must be a single DNS label such as dns: it is prefixed to domain.base, so it carries no dots and no empty labels" $domain.service) -}}
{{- end -}}
{{- if lt (int $domain.nameserverCount) 2 -}}
{{- fail (printf "domain.nameserverCount is %d; a zone delegated to fewer than two nameservers has no redundancy, and this chart already refuses fewer than two in production" (int $domain.nameserverCount)) -}}
{{- end -}}
{{- if gt (int $domain.nameserverCount) 13 -}}
{{- fail (printf "domain.nameserverCount is %d; more than 13 NS records overflow the 512-byte referral every resolver asks for first and force each of them to retry over TCP" (int $domain.nameserverCount)) -}}
{{- end -}}
{{/*
A derived ns<N> is a promise that the name resolves. When this chart also
publishes the node addresses those names point at, it can check the promise:
more derived names than published addresses means at least one nameserver in
the delegation answers from nowhere, which costs every resolver a timeout on
the way to an answer it could have had immediately.
*/}}
{{- if and (not .Values.control.nameservers) .Values.externalIPService.enabled -}}
{{- if gt (int $domain.nameserverCount) (len .Values.externalIPService.addresses) -}}
{{- fail (printf "domain.nameserverCount is %d but externalIPService.addresses publishes %d node address(es); each derived ns<N>.%s.%s needs one to resolve to. Lower the count, or publish an address for every name" (int $domain.nameserverCount) (len .Values.externalIPService.addresses) $domain.service $domain.base) -}}
{{- end -}}
{{- end -}}
{{/*
control.nameservers wins over the derived names, and the two placeholders this
chart ships are entries like any other — so setting domain.base and forgetting
to clear them would publish example.net in an NS record. Refuse that pairing
here, where the reason can be stated, rather than at the registrar.
*/}}
{{- range .Values.control.nameservers -}}
{{- if include "dns.reservedHostname" (dict "host" .) -}}
{{- fail (printf "control.nameservers still lists the placeholder %q while domain.base is set; empty control.nameservers to derive ns1..ns%d.%s.%s, or replace the list with the real delegated names" . (int $domain.nameserverCount) $domain.service $domain.base) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{/*
control.nameservers may now be empty, because empty is how an operator asks for
the derived names. The schema can no longer require two entries, so the floor
moves here: the server refuses to start on an empty DNS_NAMESERVERS, and every
zone this chart serves publishes NS records built from this list.
*/}}
{{- if eq (len (include "dns.nameservers" . | fromYamlArray)) 0 -}}
{{- fail "control.nameservers is empty and domain.base is not set, so no nameserver names exist: list the delegated names in control.nameservers, or set domain.base to derive ns1..ns<domain.nameserverCount>.<domain.service>.<domain.base>" -}}
{{- end -}}
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
{{/*
The mail WORKLOAD is a separate switch from the facade binding above: mail.enabled
deploys the server, platform.mail.enabled tells the control plane to use it. Turning
on only the workload used to render a projected Secret volume with an empty name,
which the API server rejects — and helm reports nothing, because an empty string is
valid YAML. Require the token Secret from whichever of the two places supplies it.
*/}}
{{- if .Values.mail.enabled -}}
{{- $mailSecret := default .Values.platform.mail.existingSecret .Values.mail.auth.existingSecret -}}
{{- if not $mailSecret -}}
{{- fail "mail.enabled requires mail.auth.existingSecret (or platform.mail.existingSecret); the chart never renders engine tokens" -}}
{{- end -}}
{{- $mailKey := default .Values.platform.mail.apiTokenKey .Values.mail.auth.adminTokenKey -}}
{{- if not $mailKey -}}
{{- fail "mail.enabled requires mail.auth.adminTokenKey (or platform.mail.apiTokenKey)" -}}
{{- end -}}
{{- end -}}
{{- if $platform.mail.enabled -}}
{{- if not $platform.mail.apiUrl -}}
{{- fail "platform.mail.enabled requires platform.mail.apiUrl" -}}
{{- end -}}
{{- if not (include "dns.mailHost" .) -}}
{{- fail "platform.mail.enabled requires platform.mail.hostname, or domain.base to derive mail.<domain.service>.<domain.base>" -}}
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
{{- if not (hasPrefix "https://" (include "dns.consoleURL" .)) -}}
{{- fail "platform.billing.provider=stripe requires an https platform.billing.publicUrl, or domain.base to derive https://<domain.service>.<domain.base>" -}}
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
{{- if eq (len (include "dns.httpRouteHostnames" . | fromYamlArray)) 0 -}}
{{- fail "httpRoute.enabled requires at least one hostname: list them in httpRoute.hostnames, or set domain.base to derive <domain.service>.<domain.base>" -}}
{{- end -}}
{{- end -}}
{{- if .Values.snapshotRoute.enabled -}}
{{- if not .Values.production -}}
{{- fail "snapshotRoute.enabled requires production=true so the snapshot endpoint enforces bearer authentication" -}}
{{- end -}}
{{- if not .Values.snapshotRoute.acknowledgePublicSnapshotFeed -}}
{{- fail "snapshotRoute.enabled requires snapshotRoute.acknowledgePublicSnapshotFeed=true" -}}
{{- end -}}
{{- if not (include "dns.snapshotHost" .) -}}
{{- fail "snapshotRoute.enabled requires snapshotRoute.hostname, or domain.base to derive snapshot.<domain.service>.<domain.base>" -}}
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
{{/*
The certificate must cover the names actually routed, whether those were
listed or derived. Comparing effective names rather than raw values is what
lets a derived tls.dnsNames satisfy a listed httpRoute.hostnames, and it keeps
these three checks meaning what they always meant when nothing is derived.
*/}}
{{- $tlsNames := include "dns.tlsDNSNames" . | fromYamlArray -}}
{{- if eq (len $tlsNames) 0 -}}
{{- fail "tls.enabled requires at least one hostname: list them in tls.dnsNames, or set domain.base to derive the console and snapshot hosts" -}}
{{- end -}}
{{- if and .Values.snapshotRoute.enabled (not (has (include "dns.snapshotHost" .) $tlsNames)) -}}
{{- fail "tls.dnsNames must include the snapshot hostname when both features are enabled" -}}
{{- end -}}
{{- if .Values.httpRoute.enabled -}}
{{- range (include "dns.httpRouteHostnames" . | fromYamlArray) -}}
{{- if not (has . $tlsNames) -}}
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
{{- if .Values.authority.cache.persistence.enabled -}}
{{- if not .Values.authority.cache.persistence.storageClass -}}
{{- fail "production=true requires an explicit retaining authority cache storageClass" -}}
{{- end -}}
{{- else -}}
{{/*
An ephemeral cache is permitted in production, but only as a decision somebody
made on purpose. What it costs: the snapshot cache is a per-pod recovery copy of
the last valid zone snapshot, so without it a restarted authority cannot serve
until it has reached the control plane. It is NOT on the query path — a query
reads an in-memory pointer — so steady-state serving is unaffected; what is lost
is independence from the control plane across a restart.

Accepting that is reasonable when replicas do not restart together (three
replicas behind a minAvailable=2 PDB), and it is the only way to run this chart
on a provider account that has run out of block-storage subscriptions.
*/}}
{{- if not .Values.authority.cache.acknowledgeEphemeralCache -}}
{{- fail "production=true requires authority.cache.persistence.enabled=true, or authority.cache.acknowledgeEphemeralCache=true to accept that a restarted authority cannot serve until it has reached the control plane" -}}
{{- end -}}
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
{{- $nameservers := include "dns.nameservers" . | fromYamlArray -}}
{{- if lt (len $nameservers) 2 -}}
{{- fail "production=true requires at least two nameservers: list them in control.nameservers, or set domain.base with domain.nameserverCount >= 2" -}}
{{- end -}}
{{- range $nameservers -}}
{{- if include "dns.reservedHostname" (dict "host" .) -}}
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
