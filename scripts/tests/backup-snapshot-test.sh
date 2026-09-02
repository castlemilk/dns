#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
scripts_dir="$(cd "${script_dir}/.." && pwd)"
target_script="${scripts_dir}/backup-snapshot.sh"
fixture_snapshot="${scripts_dir}/testdata/backup-snapshot/snapshot.json"
fake_curl="${script_dir}/fake-curl.sh"
fake_restore="${script_dir}/fake-dns-restore.sh"
temporary_root="$(mktemp -d)"
trap 'rm -rf "$temporary_root"' EXIT

passes=0
token='fixture-snapshot-token-1234567890'
passphrase='fixture-backup-passphrase-1234567890'
revision='aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'

fail() {
  printf 'FAIL  %s\n' "$1" >&2
  exit 1
}

pass() {
  passes=$((passes + 1))
  printf 'PASS  %s\n' "$1"
}

invoke() {
  local output_directory="$1"
  shift
  env \
    SNAPSHOT_URL=https://snapshot.dns.test/internal/v1/snapshot \
    DNS_SNAPSHOT_BEARER_TOKEN="$token" \
    DNS_BACKUP_PASSPHRASE="$passphrase" \
    SOURCE_REVISION="$revision" \
    BACKUP_OUTPUT_DIR="$output_directory" \
    CURL_BIN="$fake_curl" \
    DNS_RESTORE_BIN="$fake_restore" \
    FIXTURE_SNAPSHOT="$fixture_snapshot" \
    "$@" \
    "$target_script"
}

expect_failure() {
  local description="$1" output_directory="$2" expected="$3"
  shift 3
  local output
  if output="$(invoke "$output_directory" "$@" 2>&1)"; then
    fail "${description}: unexpectedly succeeded"
  fi
  [[ "$output" == *"$expected"* ]] || {
    printf '%s\n' "$output" >&2
    fail "${description}: expected '${expected}'"
  }
  if [[ -d "$output_directory" ]] &&
     find "$output_directory" -mindepth 1 -maxdepth 1 -print -quit | grep -q .; then
    fail "${description}: left a retained artifact after failure"
  fi
  pass "$description"
}

output_directory="${temporary_root}/success"
output="$(invoke "$output_directory")" || fail "successful backup fixture failed"
[[ "$output" != *"$token"* && "$output" != *"$passphrase"* ]] || fail "success output exposed a credential"
encrypted_archive="$(find "$output_directory" -maxdepth 1 -type f -name '*.tar.gz.gpg' -print -quit)"
[[ -n "$encrypted_archive" ]] || fail "encrypted backup was not created"
checksum_file="${encrypted_archive}.sha256"
[[ -f "$checksum_file" ]] || fail "ciphertext checksum file was not created"
(
  cd "$output_directory"
  if command -v sha256sum >/dev/null; then
    sha256sum -c "$(basename "$checksum_file")" >/dev/null
  else
    expected="$(awk '{print $1}' "$checksum_file")"
    actual="$(shasum -a 256 "$(basename "$encrypted_archive")" | awk '{print $1}')"
    [[ "$actual" == "$expected" ]]
  fi
) || fail "ciphertext checksum did not verify"
decrypted_archive="${temporary_root}/decrypted.tar.gz"
gpg_home="${temporary_root}/test-gnupg"
mkdir -m 0700 "$gpg_home"
exec 3<<<"$passphrase"
gpg --homedir "$gpg_home" --batch --yes --pinentry-mode loopback --passphrase-fd 3 \
  --decrypt --output "$decrypted_archive" "$encrypted_archive" >/dev/null 2>&1
exec 3<&-
[[ "$(tar -tzf "$decrypted_archive" | LC_ALL=C sort)" == $'manifest.json\nsnapshot.json\nverification.json' ]] ||
  fail "decrypted backup contained an unexpected file set"
tar -xOf "$decrypted_archive" manifest.json | jq -e \
  '.format == "castlemilk-dns-snapshot-backup-v1"
   and .verification.status == "valid"
   and .verification.checksum == "b7a0a81410822ef8cb1a9aa861ea5883956b7e8a3ac526cee3c6eb36c89c094a"' \
  >/dev/null || fail "backup manifest was malformed"
pass "verified snapshot is retained only as an encrypted artifact"

output_directory="${temporary_root}/verify-failure"
expect_failure "verifier failure retains nothing" "$output_directory" "dns-restore rejected" FAKE_RESTORE_FAILURE=true

output_directory="${temporary_root}/etag-failure"
expect_failure "ETag mismatch retains nothing" "$output_directory" "ETag does not match" \
  FAKE_ETAG=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa

output_directory="${temporary_root}/bad-url"
expect_failure "unsafe snapshot URL fails before fetch" "$output_directory" "SNAPSHOT_URL must be" \
  SNAPSHOT_URL='https://attacker.test/internal/v1/snapshot?redirect=true'

output_directory="${temporary_root}/short-passphrase"
expect_failure "short backup passphrase fails before fetch" "$output_directory" "at least 32 characters" \
  DNS_BACKUP_PASSPHRASE=short

printf 'Snapshot backup tests complete: %d checks passed.\n' "$passes"
