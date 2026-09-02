#!/usr/bin/env bash
set -euo pipefail

required=(CANARY_ZONE NS1_ADDRESS NS2_ADDRESS)
for name in "${required[@]}"; do
  if [[ -z "${!name:-}" ]]; then
    printf '%s is required\n' "$name" >&2
    exit 2
  fi
done

dig_bin="${DIG_BIN:-dig}"
command -v "$dig_bin" >/dev/null || {
  printf 'dig is required\n' >&2
  exit 2
}

check_delegation="${CHECK_DELEGATION:-false}"
if [[ "$check_delegation" != true && "$check_delegation" != false ]]; then
  printf 'CHECK_DELEGATION must be exactly true or false\n' >&2
  exit 2
fi

zone="${CANARY_ZONE%.}."
expected_ns1="${EXPECTED_NS1:-}"
expected_ns2="${EXPECTED_NS2:-}"
probe_name="${PROBE_NAME:-probe.${zone}}"
probe_name="${probe_name%.}."
probe_ipv4="${PROBE_IPV4:-192.0.2.53}"
dns_port="${DNS_PORT:-53}"
public_resolvers="${PUBLIC_RESOLVERS:-1.1.1.1 8.8.8.8}"

ascii_lower() {
  LC_ALL=C tr '[:upper:]' '[:lower:]' <<<"$1"
}

if [[ ! "$zone" =~ ^[A-Za-z0-9._-]+\.$ || "$zone" == *..* ]]; then
  printf 'CANARY_ZONE is not a safe DNS name\n' >&2
  exit 2
fi
if [[ ! "$probe_name" =~ ^[A-Za-z0-9._-]+\.$ || "$probe_name" == *..* ]]; then
  printf 'PROBE_NAME is not a safe DNS name\n' >&2
  exit 2
fi
if [[ ! "$probe_ipv4" =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}$ ]]; then
  printf 'PROBE_IPV4 must be an IPv4 address\n' >&2
  exit 2
fi
if [[ ! "$dns_port" =~ ^[0-9]+$ ]] || ((dns_port < 1 || dns_port > 65535)); then
  printf 'DNS_PORT must be between 1 and 65535\n' >&2
  exit 2
fi
if [[ "$NS1_ADDRESS" == "$NS2_ADDRESS" ]]; then
  printf 'NS1_ADDRESS and NS2_ADDRESS must be different endpoints\n' >&2
  exit 2
fi
for address in "$NS1_ADDRESS" "$NS2_ADDRESS"; do
  if [[ ! "$address" =~ ^[A-Za-z0-9:._%-]+$ ]]; then
    printf 'authority endpoint %q contains unsafe characters\n' "$address" >&2
    exit 2
  fi
done

if [[ -n "$expected_ns1" || -n "$expected_ns2" ]]; then
  if [[ -z "$expected_ns1" || -z "$expected_ns2" ]]; then
    printf 'EXPECTED_NS1 and EXPECTED_NS2 must be supplied together\n' >&2
    exit 2
  fi
  expected_ns1="${expected_ns1%.}."
  expected_ns2="${expected_ns2%.}."
  for expected_ns in "$expected_ns1" "$expected_ns2"; do
    if [[ ! "$expected_ns" =~ ^[A-Za-z0-9._-]+\.$ || "$expected_ns" == *..* ]]; then
      printf 'expected nameserver %q is not a safe DNS name\n' "$expected_ns" >&2
      exit 2
    fi
  done
  if [[ "$(ascii_lower "$expected_ns1")" == "$(ascii_lower "$expected_ns2")" ]]; then
    printf 'EXPECTED_NS1 and EXPECTED_NS2 must be different names\n' >&2
    exit 2
  fi
fi
if [[ "$check_delegation" == true && (-z "$expected_ns1" || -z "$expected_ns2") ]]; then
  printf 'delegation checks require EXPECTED_NS1 and EXPECTED_NS2\n' >&2
  exit 2
fi

read -r -a resolver_list <<<"$public_resolvers"
if [[ ${#resolver_list[@]} -lt 2 ]]; then
  printf 'PUBLIC_RESOLVERS must contain at least two resolvers\n' >&2
  exit 2
fi
for resolver in "${resolver_list[@]}"; do
  if [[ ! "$resolver" =~ ^[A-Za-z0-9:._%-]+$ ]]; then
    printf 'resolver endpoint %q contains unsafe characters\n' "$resolver" >&2
    exit 2
  fi
done
for ((i = 0; i < ${#resolver_list[@]}; i++)); do
  for ((j = 0; j < i; j++)); do
    if [[ "$(ascii_lower "${resolver_list[$i]}")" == "$(ascii_lower "${resolver_list[$j]}")" ]]; then
      printf 'PUBLIC_RESOLVERS must contain distinct resolver endpoints\n' >&2
      exit 2
    fi
  done
done

pass_count=0

pass() {
  pass_count=$((pass_count + 1))
  printf 'PASS  %s\n' "$1"
}

fail() {
  printf 'FAIL  %s\n' "$1" >&2
  return 1
}

direct_query() {
  local server="$1" name="$2" type="$3" transport="$4"
  local args=("@${server}" -p "$dns_port" "$name" "$type" +norecurse +time=4 +tries=2)
  if [[ "$transport" == tcp ]]; then
    args+=(+tcp)
  else
    # Prevent dig from silently retrying a truncated UDP reply over TCP. The
    # canary must exercise the configured UDP path independently.
    args+=(+ignore)
  fi
  "$dig_bin" "${args[@]}"
}

recursive_query() {
  local server="$1" name="$2" type="$3" transport="$4"
  local args=("@${server}" -p 53 "$name" "$type" +time=4 +tries=2)
  if [[ "$transport" == tcp ]]; then
    args+=(+tcp)
  else
    args+=(+ignore)
  fi
  "$dig_bin" "${args[@]}"
}

assert_status() {
  local output="$1" expected="$2" description="$3"
  if grep -Eq "status: ${expected}," <<<"$output"; then
    pass "$description status=${expected}"
  else
    printf '%s\n' "$output" >&2
    fail "$description expected status=${expected}"
  fi
}

response_flags() {
  sed -n 's/^;; flags: \([^;]*\);.*/\1/p' | head -n 1
}

assert_authoritative() {
  local output="$1" description="$2" flags
  flags="$(response_flags <<<"$output")"
  if [[ " $flags " == *" aa "* && " $flags " != *" ra "* ]]; then
    pass "$description is authoritative with recursion unavailable"
  else
    printf '%s\n' "$output" >&2
    fail "$description flags were '$flags'"
  fi
}

assert_recursive() {
  local output="$1" description="$2" flags
  flags="$(response_flags <<<"$output")"
  if [[ " $flags " == *" ra "* ]]; then
    pass "$description recursion is available"
  else
    printf '%s\n' "$output" >&2
    fail "$description flags were '$flags'"
  fi
}

assert_probe_ipv4() {
  local output="$1" description="$2"
  if awk -v expected="$probe_ipv4" '$4 == "A" && $5 == expected { found=1 } END { exit !found }' <<<"$output"; then
    pass "$description returned probe IPv4 $probe_ipv4"
  else
    printf '%s\n' "$output" >&2
    fail "$description did not return probe IPv4 $probe_ipv4"
  fi
}

assert_expected_ns() {
  local output="$1" description="$2" expected
  for expected in "$expected_ns1" "$expected_ns2"; do
    if awk -v expected="$expected" '$4 == "NS" && tolower($5) == tolower(expected) { found=1 } END { exit !found }' <<<"$output"; then
      pass "$description returned NS $expected"
    else
      printf '%s\n' "$output" >&2
      fail "$description did not return NS $expected"
    fi
  done
}

soa_serial() {
  awk '$4 == "SOA" { print $7; exit }'
}

check_endpoint() {
  local label="$1" address="$2" transport="$3"
  local output serial target_variable

  output="$(direct_query "$address" "$zone" SOA "$transport")"
  assert_status "$output" NOERROR "$label $transport SOA"
  assert_authoritative "$output" "$label $transport SOA"
  serial="$(soa_serial <<<"$output")"
  if [[ ! "$serial" =~ ^[0-9]+$ ]]; then
    printf '%s\n' "$output" >&2
    fail "$label $transport did not return an SOA serial"
  fi
  target_variable="${label}_${transport}_serial"
  printf -v "$target_variable" '%s' "$serial"

  output="$(direct_query "$address" "$probe_name" A "$transport")"
  assert_status "$output" NOERROR "$label $transport probe A"
  assert_authoritative "$output" "$label $transport probe A"
  assert_probe_ipv4 "$output" "$label $transport probe A"

  if [[ -n "$expected_ns1" && -n "$expected_ns2" ]]; then
    output="$(direct_query "$address" "$zone" NS "$transport")"
    assert_status "$output" NOERROR "$label $transport NS"
    assert_authoritative "$output" "$label $transport NS"
    assert_expected_ns "$output" "$label $transport NS"
  fi
}

ns1_udp_serial=""
ns1_tcp_serial=""
ns2_udp_serial=""
ns2_tcp_serial=""

check_endpoint ns1 "$NS1_ADDRESS" udp
check_endpoint ns1 "$NS1_ADDRESS" tcp
check_endpoint ns2 "$NS2_ADDRESS" udp
check_endpoint ns2 "$NS2_ADDRESS" tcp

if [[ "$ns1_udp_serial" == "$ns1_tcp_serial" &&
      "$ns1_udp_serial" == "$ns2_udp_serial" &&
      "$ns1_udp_serial" == "$ns2_tcp_serial" ]]; then
  pass "all authority transports agree on SOA serial $ns1_udp_serial"
else
  fail "SOA serial skew: ns1/udp=$ns1_udp_serial ns1/tcp=$ns1_tcp_serial ns2/udp=$ns2_udp_serial ns2/tcp=$ns2_tcp_serial"
fi

for entry in "ns1:$NS1_ADDRESS" "ns2:$NS2_ADDRESS"; do
  label="${entry%%:*}"
  address="${entry#*:}"

  output="$(direct_query "$address" "missing-${GITHUB_RUN_ID:-local}-${RANDOM}.${zone}" A udp)"
  assert_status "$output" NXDOMAIN "$label negative answer"
  assert_authoritative "$output" "$label negative answer"

  output="$(direct_query "$address" example.invalid. A udp)"
  assert_status "$output" REFUSED "$label out-of-scope answer"
done

if [[ "$check_delegation" == true ]]; then
  for resolver in "${resolver_list[@]}"; do
    for transport in udp tcp; do
      description="resolver $resolver $transport"

      output="$(recursive_query "$resolver" "$probe_name" A "$transport")"
      assert_status "$output" NOERROR "$description probe A"
      assert_recursive "$output" "$description probe A"
      assert_probe_ipv4 "$output" "$description probe A"

      output="$(recursive_query "$resolver" "$zone" NS "$transport")"
      assert_status "$output" NOERROR "$description delegated NS"
      assert_recursive "$output" "$description delegated NS"
      assert_expected_ns "$output" "$description delegated NS"

      output="$(recursive_query "$resolver" "$zone" DS "$transport")"
      assert_status "$output" NOERROR "$description unsigned DS"
      assert_recursive "$output" "$description unsigned DS"
      if awk '$4 == "DS" { found=1 } END { exit !found }' <<<"$output"; then
        printf '%s\n' "$output" >&2
        fail "$description returned DS data for unsigned canary $zone"
      fi
      pass "$description returned no DS data for unsigned canary $zone"
    done
  done
fi

printf 'Authority probe complete: %d checks passed.\n' "$pass_count"
