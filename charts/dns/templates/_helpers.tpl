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

{{/* Fail closed on topology values that would violate the runtime contract. */}}
{{- define "dns.validate" -}}
{{- range $index, $entry := .Values.control.extraEnv -}}
{{- $name := get $entry "name" | default "" -}}
{{- if hasPrefix "DNS_" $name -}}
{{- fail (printf "control.extraEnv[%d].name %q is reserved; DNS_* runtime settings are owned by the chart" $index $name) -}}
{{- end -}}
{{- end -}}
{{- range $index, $entry := .Values.authority.extraEnv -}}
{{- $name := get $entry "name" | default "" -}}
{{- if hasPrefix "DNS_" $name -}}
{{- fail (printf "authority.extraEnv[%d].name %q is reserved; DNS_* runtime settings are owned by the chart" $index $name) -}}
{{- end -}}
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
