#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
scripts_dir="$(cd "${script_dir}/.." && pwd)"
target_script="${scripts_dir}/probe-authorities.sh"
fake_dig="${script_dir}/fake-dig.sh"
passes=0

fail() {
  printf 'FAIL  %s\n' "$1" >&2
  exit 1
}

pass() {
  passes=$((passes + 1))
  printf 'PASS  %s\n' "$1"
}

invoke() {
  env \
    CANARY_ZONE=canary.dns.test \
    NS1_ADDRESS=fixture-ns1 \
    NS2_ADDRESS=fixture-ns2 \
    EXPECTED_NS1=ns1.dns.test \
    EXPECTED_NS2=ns2.dns.test \
    PROBE_NAME=probe.canary.dns.test \
    PROBE_IPV4=192.0.2.53 \
    CHECK_DELEGATION=true \
    PUBLIC_RESOLVERS='1.1.1.1 8.8.8.8' \
    DIG_BIN="$fake_dig" \
    "$@" \
    "$target_script"
}

output="$(invoke)" || fail "successful delegated probe fixture failed"
[[ "$output" == *"Authority probe complete:"* ]] || fail "successful probe did not complete"
[[ "$output" == *"ns1 udp probe A"* && "$output" == *"ns2 tcp probe A"* ]] ||
  fail "successful probe did not cover both authorities and transports"
[[ "$output" == *"resolver 1.1.1.1 udp"* && "$output" == *"resolver 8.8.8.8 tcp"* ]] ||
  fail "successful probe did not cover both public resolvers and transports"
pass "delegated canary covers both authorities, transports, and public resolvers"

if output="$(invoke FAKE_SERIAL_SKEW=true 2>&1)"; then
  fail "serial skew fixture unexpectedly succeeded"
fi
[[ "$output" == *"SOA serial skew"* ]] || fail "serial skew failure was not reported"
pass "SOA serial skew fails closed"

if output="$(invoke FAKE_DS_PRESENT=true 2>&1)"; then
  fail "unexpected DS fixture unexpectedly succeeded"
fi
[[ "$output" == *"returned DS data"* ]] || fail "unexpected DS failure was not reported"
pass "unexpected DS data fails closed"

if output="$(invoke NS2_ADDRESS=fixture-ns1 2>&1)"; then
  fail "shared authority endpoint fixture unexpectedly succeeded"
fi
[[ "$output" == *"must be different endpoints"* ]] || fail "shared endpoint failure was not reported"
pass "shared authority endpoint fails closed"

if output="$(invoke PUBLIC_RESOLVERS='1.1.1.1 1.1.1.1' 2>&1)"; then
  fail "duplicate public resolver fixture unexpectedly succeeded"
fi
[[ "$output" == *"distinct resolver endpoints"* ]] || fail "duplicate resolver failure was not reported"
pass "duplicate public resolver endpoints fail closed"

printf 'Authority probe tests complete: %d checks passed.\n' "$passes"
