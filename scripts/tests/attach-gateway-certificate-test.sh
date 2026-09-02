#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
scripts_dir="$(cd "${script_dir}/.." && pwd)"
fixture_source="${scripts_dir}/testdata/attach-gateway-certificate"
target_script="${scripts_dir}/attach-gateway-certificate.sh"
fake_kubectl="${script_dir}/fake-kubectl.sh"
temporary_root="$(mktemp -d)"
trap 'rm -rf "$temporary_root"' EXIT

passes=0

fail() {
  printf 'FAIL  %s\n' "$1" >&2
  exit 1
}

pass() {
  passes=$((passes + 1))
  printf 'PASS  %s\n' "$1"
}

new_fixture() {
  local name="$1"
  local directory="${temporary_root}/${name}"
  mkdir -p "$directory"
  cp "${fixture_source}"/*.json "$directory/"
  printf '%s\n' "$directory"
}

invoke() {
  local fixture="$1"
  shift
  env \
    EXPECTED_CONTEXT=fixture-context \
    FAKE_CONTEXT=fixture-context \
    FIXTURE_DIR="$fixture" \
    PATCH_LOG="${fixture}/patch.log" \
    KUBECTL_BIN="$fake_kubectl" \
    "$@" \
    "$target_script"
}

expect_failure() {
  local description="$1" fixture="$2" expected_message="$3"
  shift 3
  local output
  if output="$(invoke "$fixture" "$@" 2>&1)"; then
    fail "${description}: command unexpectedly succeeded"
  fi
  if [[ "$output" != *"$expected_message"* ]]; then
    printf '%s\n' "$output" >&2
    fail "${description}: expected message '${expected_message}'"
  fi
  if [[ -s "${fixture}/patch.log" ]]; then
    fail "${description}: patch was attempted"
  fi
  pass "$description"
}

fixture="$(new_fixture plan)"
output="$(invoke "$fixture")" || fail "default plan failed"
[[ "$output" == *"PLAN_ONLY=true"* ]] || fail "default plan did not report plan-only mode"
[[ "$output" == *'"path":"/spec/listeners/1/tls/certificateRefs/-"'* ]] ||
  fail "default plan did not resolve the HTTPS listener index"
[[ ! -s "${fixture}/patch.log" ]] || fail "default plan attempted a patch"
pass "default mode plans without mutation"

fixture="$(new_fixture apply)"
output="$(invoke "$fixture" APPLY=true)" || fail "explicit apply fixture failed"
[[ "$output" == *"Attached Secret dns/dns-tls"* ]] || fail "apply did not report attachment"
[[ "$(wc -l <"${fixture}/patch.log" | tr -d ' ')" == 1 ]] || fail "apply did not issue exactly one patch"
jq -e '
  ([.[] | select(.op == "add")] | length) == 1
    and ([.[] | select(.op == "add")][0] == {
      "op": "add",
      "path": "/spec/listeners/1/tls/certificateRefs/-",
      "value": {"group":"", "kind":"Secret", "name":"dns-tls", "namespace":"dns"}
    })
    and ([.[] | select(.op == "test" and .path == "/metadata/resourceVersion")] | length) == 1
' "${fixture}/patch.log" >/dev/null || fail "apply patch was not the expected guarded append"
pass "APPLY=true performs one guarded append"

fixture="$(new_fixture idempotent)"
jq '.spec.listeners[1].tls.certificateRefs += [{"group":"","kind":"Secret","name":"dns-tls","namespace":"dns"}]' \
  "${fixture}/gateway.json" >"${fixture}/gateway.next.json"
mv "${fixture}/gateway.next.json" "${fixture}/gateway.json"
output="$(invoke "$fixture" APPLY=true)" || fail "idempotent fixture failed"
[[ "$output" == *"already attached"* ]] || fail "idempotent run did not report no-op"
[[ ! -s "${fixture}/patch.log" ]] || fail "idempotent run attempted a patch"
pass "existing exact reference is a no-op"

fixture="$(new_fixture context-mismatch)"
expect_failure "context mismatch fails closed" "$fixture" "does not exactly match" FAKE_CONTEXT=other-context

fixture="$(new_fixture context-missing)"
expect_failure "missing expected context fails closed" "$fixture" "EXPECTED_CONTEXT is required" EXPECTED_CONTEXT=

fixture="$(new_fixture certificate-not-ready)"
jq '(.status.conditions[] | select(.type == "Ready")).status = "False"' \
  "${fixture}/certificate.json" >"${fixture}/certificate.next.json"
mv "${fixture}/certificate.next.json" "${fixture}/certificate.json"
expect_failure "unready Certificate fails closed" "$fixture" "is not Ready"

fixture="$(new_fixture wrong-secret-type)"
jq '.type = "Opaque"' "${fixture}/secret.json" >"${fixture}/secret.next.json"
mv "${fixture}/secret.next.json" "${fixture}/secret.json"
expect_failure "non-TLS Secret fails closed" "$fixture" "is not a populated kubernetes.io/tls Secret"

fixture="$(new_fixture multiple-https)"
jq '.spec.listeners += [{
  "name":"alternate-https",
  "port":8443,
  "protocol":"HTTPS",
  "tls":{"mode":"Terminate","certificateRefs":[]}
}]' "${fixture}/gateway.json" >"${fixture}/gateway.next.json"
mv "${fixture}/gateway.next.json" "${fixture}/gateway.json"
expect_failure "multiple HTTPS listeners fail closed" "$fixture" "exactly one HTTPS listener"

fixture="$(new_fixture missing-https)"
jq 'del(.spec.listeners[1])' "${fixture}/gateway.json" >"${fixture}/gateway.next.json"
mv "${fixture}/gateway.next.json" "${fixture}/gateway.json"
expect_failure "missing HTTPS listener fails closed" "$fixture" "exactly one HTTPS listener"

fixture="$(new_fixture malformed-ref)"
jq 'del(.spec.listeners[1].tls.certificateRefs[0].namespace)' \
  "${fixture}/gateway.json" >"${fixture}/gateway.next.json"
mv "${fixture}/gateway.next.json" "${fixture}/gateway.json"
expect_failure "malformed existing reference fails closed" "$fixture" "malformed or duplicate certificateRefs"

fixture="$(new_fixture conflicting-modes)"
expect_failure "conflicting apply and plan modes fail closed" "$fixture" "conflicts with explicit PLAN_ONLY=true" APPLY=true PLAN_ONLY=true

printf 'Gateway certificate attachment tests complete: %d checks passed.\n' "$passes"
