#!/usr/bin/env bash
set -euo pipefail

[[ -z "${DNS_SNAPSHOT_BEARER_TOKEN:-}" && -z "${DNS_BACKUP_PASSPHRASE:-}" ]] || {
  printf 'backup credentials leaked into curl environment\n' >&2
  exit 1
}

output=""
headers=""
authorization_file=""
url=""

while (($# > 0)); do
  case "$1" in
    --output|--dump-header|--header|--request|--proto|--max-redirs|--connect-timeout|--max-time|--retry|--retry-delay|--max-filesize|--user-agent)
      option="$1"
      value="${2:-}"
      shift 2
      case "$option" in
        --output) output="$value" ;;
        --dump-header) headers="$value" ;;
        --header)
          if [[ "$value" == @* ]]; then
            authorization_file="${value#@}"
          fi
          ;;
      esac
      ;;
    --fail|--silent|--show-error|--retry-connrefused)
      shift
      ;;
    https://*)
      url="$1"
      shift
      ;;
    *)
      printf 'unexpected curl argument: %s\n' "$1" >&2
      exit 1
      ;;
  esac
done

[[ -n "$output" && -n "$headers" && -f "$authorization_file" ]] || exit 1
grep -Fxq 'Authorization: Bearer fixture-snapshot-token-1234567890' "$authorization_file" || exit 1

case "$url" in
  https://snapshot.dns.test/internal/v1/snapshot)
    cp "${FIXTURE_SNAPSHOT:?}" "$output"
    etag="${FAKE_ETAG:-b7a0a81410822ef8cb1a9aa861ea5883956b7e8a3ac526cee3c6eb36c89c094a}"
    printf 'HTTP/2 200\r\nContent-Type: application/json\r\nCache-Control: no-store\r\nETag: "sha256:%s"\r\n\r\n' \
      "$etag" >"$headers"
    ;;
  https://snapshot.dns.test/internal/v1/platform-backup)
    status="${FAKE_PLATFORM_STATUS:-200}"
    if [[ "$status" != 200 ]]; then
      : >"$output"
      printf 'HTTP/2 %s\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: 0\r\n\r\n' \
        "$status" >"$headers"
      exit 0
    fi
    cp "${FIXTURE_PLATFORM:?}" "$output"
    length="$(wc -c <"$output" | tr -d '[:space:]')"
    # A declared length that disagrees with the body simulates a truncated
    # stream, which the caller must refuse.
    length="${FAKE_PLATFORM_LENGTH:-$length}"
    printf 'HTTP/2 200\r\nContent-Type: application/octet-stream\r\nCache-Control: no-store\r\nContent-Length: %s\r\n\r\n' \
      "$length" >"$headers"
    ;;
  *)
    exit 1
    ;;
esac
