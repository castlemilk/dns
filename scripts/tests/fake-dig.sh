#!/usr/bin/env bash
set -euo pipefail

server=""
name=""
record_type=""
transport=udp

while (($# > 0)); do
  case "$1" in
    @*) server="${1#@}"; shift ;;
    -p) shift 2 ;;
    +tcp) transport=tcp; shift ;;
    +*) shift ;;
    *)
      if [[ -z "$name" ]]; then
        name="$1"
      elif [[ -z "$record_type" ]]; then
        record_type="$1"
      else
        printf 'unexpected dig argument %s\n' "$1" >&2
        exit 1
      fi
      shift
      ;;
  esac
done

zone="${CANARY_ZONE%.}."
probe="${PROBE_NAME%.}."
# These values are supplied by probe-authorities-test.sh.
# shellcheck disable=SC2153
expected_ns1="${EXPECTED_NS1%.}."
# shellcheck disable=SC2153
expected_ns2="${EXPECTED_NS2%.}."
status=NOERROR
flags='qr aa'
answers=()

if [[ "$server" == fixture-ns1 || "$server" == fixture-ns2 ]]; then
  serial=2026090201
  if [[ "${FAKE_SERIAL_SKEW:-false}" == true && "$server" == fixture-ns2 && "$transport" == tcp ]]; then
    serial=2026090202
  fi
  case "${name}|${record_type}" in
    "${zone}|SOA")
      answers+=("${zone} 60 IN SOA ${expected_ns1} hostmaster.${zone} ${serial} 3600 600 86400 60")
      ;;
    "${zone}|NS")
      answers+=("${zone} 60 IN NS ${expected_ns1}" "${zone} 60 IN NS ${expected_ns2}")
      ;;
    "${probe}|A")
      answers+=("${probe} 60 IN A ${PROBE_IPV4}")
      ;;
    "example.invalid.|A")
      status=REFUSED
      ;;
    missing-*"${zone}|A")
      status=NXDOMAIN
      ;;
    *)
      printf 'unexpected direct query %s %s %s\n' "$server" "$name" "$record_type" >&2
      exit 1
      ;;
  esac
elif [[ "$server" == 1.1.1.1 || "$server" == 8.8.8.8 ]]; then
  flags='qr rd ra'
  case "${name}|${record_type}" in
    "${probe}|A")
      answers+=("${probe} 60 IN A ${PROBE_IPV4}")
      ;;
    "${zone}|NS")
      answers+=("${zone} 60 IN NS ${expected_ns1}" "${zone} 60 IN NS ${expected_ns2}")
      ;;
    "${zone}|DS")
      if [[ "${FAKE_DS_PRESENT:-false}" == true ]]; then
        answers+=("${zone} 60 IN DS 12345 13 2 DEADBEEF")
      fi
      ;;
    *)
      printf 'unexpected recursive query %s %s %s\n' "$server" "$name" "$record_type" >&2
      exit 1
      ;;
  esac
else
  printf 'unexpected resolver %s\n' "$server" >&2
  exit 1
fi

printf ';; ->>HEADER<<- opcode: QUERY, status: %s, id: 1\n' "$status"
printf ';; flags: %s; QUERY: 1, ANSWER: %d, AUTHORITY: 0, ADDITIONAL: 0\n' "$flags" "${#answers[@]}"
for answer in "${answers[@]}"; do
  printf '%s\n' "$answer"
done
