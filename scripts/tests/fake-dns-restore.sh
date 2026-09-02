#!/usr/bin/env bash
set -euo pipefail

[[ -z "${DNS_SNAPSHOT_BEARER_TOKEN:-}" && -z "${DNS_BACKUP_PASSPHRASE:-}" ]] || {
  printf 'backup credentials leaked into verifier environment\n' >&2
  exit 1
}

[[ "${1:-}" == --snapshot && -f "${2:-}" && "${3:-}" == --verify-only && $# == 3 ]] || exit 1
if [[ "${FAKE_RESTORE_FAILURE:-false}" == true ]]; then
  printf 'fixture verification failure\n' >&2
  exit 1
fi
printf '%s\n' '{"status":"valid","zones":0,"records":0,"generated_at":"2026-09-02T00:00:00Z","checksum":"b7a0a81410822ef8cb1a9aa861ea5883956b7e8a3ac526cee3c6eb36c89c094a"}'
