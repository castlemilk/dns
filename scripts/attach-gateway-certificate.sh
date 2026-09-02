#!/usr/bin/env bash
set -euo pipefail

readonly certificate_namespace="dns"
readonly certificate_name="dns-tls"
readonly secret_namespace="dns"
readonly secret_name="dns-tls"
readonly gateway_namespace="envoy-gateway-system"
readonly gateway_name="paprika"
readonly listener_name="https"

fail() {
  printf 'ERROR: %s\n' "$1" >&2
  exit 2
}

require_boolean() {
  local name="$1" value="$2"
  if [[ "$value" != true && "$value" != false ]]; then
    fail "${name} must be exactly true or false"
  fi
}

expected_context="${EXPECTED_CONTEXT:-}"
if [[ -z "$expected_context" ]]; then
  fail "EXPECTED_CONTEXT is required"
fi
if [[ "$expected_context" == *$'\n'* ]]; then
  fail "EXPECTED_CONTEXT must contain exactly one context name"
fi

apply="${APPLY:-false}"
require_boolean APPLY "$apply"

plan_only_was_set=false
if [[ -v PLAN_ONLY ]]; then
  plan_only_was_set=true
  plan_only="$PLAN_ONLY"
else
  plan_only=true
fi
require_boolean PLAN_ONLY "$plan_only"

if [[ "$apply" == true ]]; then
  if [[ "$plan_only_was_set" == true && "$plan_only" == true ]]; then
    fail "APPLY=true conflicts with explicit PLAN_ONLY=true"
  fi
  plan_only=false
elif [[ "$plan_only" != true ]]; then
  fail "PLAN_ONLY=false requires APPLY=true"
fi

kubectl_bin="${KUBECTL_BIN:-kubectl}"
command -v "$kubectl_bin" >/dev/null || fail "kubectl executable not found: ${kubectl_bin}"
command -v jq >/dev/null || fail "jq is required"

current_context="$("$kubectl_bin" config current-context)" || fail "could not read the current kubectl context"
if [[ "$current_context" != "$expected_context" ]]; then
  fail "current kubectl context '${current_context}' does not exactly match EXPECTED_CONTEXT '${expected_context}'"
fi

kubectl_get() {
  "$kubectl_bin" --context "$expected_context" get "$@" -o json
}

certificate_json="$(kubectl_get certificate.cert-manager.io "$certificate_name" -n "$certificate_namespace")" ||
  fail "could not read Certificate ${certificate_namespace}/${certificate_name}"

if ! jq -e \
  --arg namespace "$certificate_namespace" \
  --arg name "$certificate_name" \
  --arg secret "$secret_name" '
    . as $certificate
    | ((.status.conditions // []) | map(select(.type == "Ready"))) as $ready
    | .metadata.namespace == $namespace
        and .metadata.name == $name
        and .spec.secretName == $secret
        and ($ready | length) == 1
        and $ready[0].status == "True"
        and (($ready[0].observedGeneration // $certificate.metadata.generation) == $certificate.metadata.generation)
  ' <<<"$certificate_json" >/dev/null; then
  fail "Certificate ${certificate_namespace}/${certificate_name} is not Ready for its current generation or targets the wrong Secret"
fi

secret_json="$(kubectl_get secret "$secret_name" -n "$secret_namespace")" ||
  fail "could not read Secret ${secret_namespace}/${secret_name}"

if ! jq -e \
  --arg namespace "$secret_namespace" \
  --arg name "$secret_name" '
    .metadata.namespace == $namespace
      and .metadata.name == $name
      and .type == "kubernetes.io/tls"
      and (.data["tls.crt"] | type == "string" and length > 0)
      and (.data["tls.key"] | type == "string" and length > 0)
  ' <<<"$secret_json" >/dev/null; then
  fail "Secret ${secret_namespace}/${secret_name} is not a populated kubernetes.io/tls Secret"
fi

gateway_json="$(kubectl_get gateway.gateway.networking.k8s.io "$gateway_name" -n "$gateway_namespace")" ||
  fail "could not read Gateway ${gateway_namespace}/${gateway_name}"

if ! jq -e \
  --arg namespace "$gateway_namespace" \
  --arg name "$gateway_name" '
    .metadata.namespace == $namespace
      and .metadata.name == $name
      and (.metadata.resourceVersion | type == "string" and length > 0)
      and (.spec.listeners | type == "array")
  ' <<<"$gateway_json" >/dev/null; then
  fail "Gateway identity, resourceVersion, or listeners are malformed"
fi

https_listeners="$(jq -c '
  [.spec.listeners | to_entries[] | select(.value.protocol == "HTTPS")]
' <<<"$gateway_json")"

if ! jq -e --arg name "$listener_name" '
  length == 1 and .[0].value.name == $name
' <<<"$https_listeners" >/dev/null; then
  fail "Gateway must contain exactly one HTTPS listener and it must be named '${listener_name}'"
fi

listener_index="$(jq -r '.[0].key' <<<"$https_listeners")"
if [[ ! "$listener_index" =~ ^[0-9]+$ ]]; then
  fail "resolved HTTPS listener index is malformed"
fi

if ! jq -e --argjson index "$listener_index" '
  .spec.listeners[$index].name == "https"
    and .spec.listeners[$index].protocol == "HTTPS"
    and .spec.listeners[$index].tls.mode == "Terminate"
    and (.spec.listeners[$index].tls.certificateRefs | type == "array")
' <<<"$gateway_json" >/dev/null; then
  fail "HTTPS listener must terminate TLS and contain a certificateRefs array"
fi

certificate_refs="$(jq -c --argjson index "$listener_index" \
  '.spec.listeners[$index].tls.certificateRefs' <<<"$gateway_json")"

if ! jq -e '
  def valid_ref:
    type == "object"
      and ((keys_unsorted - ["group", "kind", "name", "namespace"]) | length == 0)
      and (.group | type == "string")
      and (.kind | type == "string" and length > 0)
      and (.name | type == "string" and length > 0)
      and (.namespace | type == "string" and length > 0);
  all(.[]; valid_ref)
    and (([.[] | [.group, .kind, .namespace, .name] | @json] | unique | length) == length)
' <<<"$certificate_refs" >/dev/null; then
  fail "HTTPS listener contains malformed or duplicate certificateRefs"
fi

target_ref='{"group":"","kind":"Secret","name":"dns-tls","namespace":"dns"}'
target_count="$(jq -r --argjson target "$target_ref" \
  '[.[] | select(. == $target)] | length' <<<"$certificate_refs")"

if [[ "$target_count" == 1 ]]; then
  printf 'Certificate reference already attached to Gateway %s/%s listener %s; no change.\n' \
    "$gateway_namespace" "$gateway_name" "$listener_name"
  exit 0
fi
if [[ "$target_count" != 0 ]]; then
  fail "target certificate reference appears more than once"
fi

resource_version="$(jq -r '.metadata.resourceVersion' <<<"$gateway_json")"
refs_path="/spec/listeners/${listener_index}/tls/certificateRefs"
patch_json="$(jq -cn \
  --arg resourceVersion "$resource_version" \
  --arg listenerPath "/spec/listeners/${listener_index}" \
  --arg refsPath "$refs_path" \
  --argjson refs "$certificate_refs" \
  --argjson target "$target_ref" '[
    {"op":"test", "path":"/metadata/resourceVersion", "value":$resourceVersion},
    {"op":"test", "path":($listenerPath + "/name"), "value":"https"},
    {"op":"test", "path":($listenerPath + "/protocol"), "value":"HTTPS"},
    {"op":"test", "path":$refsPath, "value":$refs},
    {"op":"add", "path":($refsPath + "/-"), "value":$target}
  ]')"

printf 'Validated context: %s\n' "$expected_context"
printf 'Validated Certificate and TLS Secret: %s/%s\n' "$certificate_namespace" "$certificate_name"
printf 'Resolved Gateway listener: %s/%s[%s] name=%s existingRefs=%s\n' \
  "$gateway_namespace" "$gateway_name" "$listener_index" "$listener_name" \
  "$(jq 'length' <<<"$certificate_refs")"

if [[ "$plan_only" == true ]]; then
  printf 'PLAN_ONLY=true; no change will be made. Proposed JSON Patch:\n%s\n' "$patch_json"
  exit 0
fi

patched_gateway="$("$kubectl_bin" --context "$expected_context" patch \
  gateway.gateway.networking.k8s.io "$gateway_name" \
  -n "$gateway_namespace" --type=json --patch "$patch_json" -o json)" ||
  fail "Gateway patch failed; no retry was attempted"

if ! jq -e \
  --argjson index "$listener_index" \
  --argjson before "$certificate_refs" \
  --argjson target "$target_ref" '
    (.spec.listeners[$index].tls.certificateRefs) as $after
    | .spec.listeners[$index].name == "https"
        and .spec.listeners[$index].protocol == "HTTPS"
        and ($after | length) == (($before | length) + 1)
        and $after[0:($before | length)] == $before
        and $after[-1] == $target
  ' <<<"$patched_gateway" >/dev/null; then
  fail "Gateway patch response did not preserve every existing reference and append the target exactly once"
fi

printf 'Attached Secret %s/%s to Gateway %s/%s listener %s.\n' \
  "$secret_namespace" "$secret_name" "$gateway_namespace" "$gateway_name" "$listener_name"
