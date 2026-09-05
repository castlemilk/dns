#!/usr/bin/env bash
set -euo pipefail
umask 077

fail() {
  printf 'ERROR: %s\n' "$1" >&2
  exit 2
}

sha256_file() {
  local path="$1"
  if command -v sha256sum >/dev/null; then
    sha256sum "$path" | awk '{print $1}'
  else
    shasum -a 256 "$path" | awk '{print $1}'
  fi
}

header_value() {
  local header_name="$1" path="$2"
  awk -v wanted="${header_name}:" '
    tolower($1) == tolower(wanted) {
      $1 = ""
      sub(/^ /, "")
      gsub(/\r/, "")
      value = $0
    }
    END { print value }
  ' "$path"
}

snapshot_url="${SNAPSHOT_URL:-}"
platform_backup_url="${PLATFORM_BACKUP_URL:-}"
snapshot_token="${DNS_SNAPSHOT_BEARER_TOKEN:-}"
backup_passphrase="${DNS_BACKUP_PASSPHRASE:-}"
source_revision="${SOURCE_REVISION:-}"
output_directory="${BACKUP_OUTPUT_DIR:-}"
max_snapshot_bytes="${MAX_SNAPSHOT_BYTES:-1073741824}"
curl_bin="${CURL_BIN:-curl}"
restore_bin="${DNS_RESTORE_BIN:-dns-restore}"

# Keep child processes from inheriting either credential. The bearer token is
# passed to curl through a mode-0600 header file and the recovery key reaches
# gpg only through a dedicated file descriptor.
unset DNS_SNAPSHOT_BEARER_TOKEN DNS_BACKUP_PASSPHRASE

if [[ ! "$snapshot_url" =~ ^https://[A-Za-z0-9.-]+(:[0-9]+)?/internal/v1/snapshot$ ]]; then
  fail "SNAPSHOT_URL must be an HTTPS URL with exact path /internal/v1/snapshot and no query, fragment, or userinfo"
fi
# The platform store is served by the same control process, behind the same
# snapshot bearer token, on a sibling path. A control plane without a platform
# store answers 404 and the backup still succeeds.
if [[ -z "$platform_backup_url" ]]; then
  platform_backup_url="${snapshot_url%/internal/v1/snapshot}/internal/v1/platform-backup"
fi
if [[ ! "$platform_backup_url" =~ ^https://[A-Za-z0-9.-]+(:[0-9]+)?/internal/v1/platform-backup$ ]]; then
  fail "PLATFORM_BACKUP_URL must be an HTTPS URL with exact path /internal/v1/platform-backup and no query, fragment, or userinfo"
fi
if [[ ! "$snapshot_token" =~ ^[!-~]{32,}$ ]]; then
  fail "DNS_SNAPSHOT_BEARER_TOKEN must contain at least 32 visible non-space ASCII characters"
fi
if [[ ${#backup_passphrase} -lt 32 || "$backup_passphrase" == *$'\n'* || "$backup_passphrase" == *$'\r'* ]]; then
  fail "DNS_BACKUP_PASSPHRASE must contain at least 32 characters and no line breaks"
fi
if [[ ! "$source_revision" =~ ^[0-9a-fA-F]{40}$ ]]; then
  fail "SOURCE_REVISION must be a full 40-character Git commit SHA"
fi
if [[ -z "$output_directory" || "$output_directory" == / ]]; then
  fail "BACKUP_OUTPUT_DIR must be a specific output directory"
fi
if [[ ! "$max_snapshot_bytes" =~ ^[0-9]+$ ]] ||
   ((max_snapshot_bytes < 1 || max_snapshot_bytes > 1073741824)); then
  fail "MAX_SNAPSHOT_BYTES must be between 1 and 1073741824"
fi

command -v "$curl_bin" >/dev/null || fail "curl executable not found: ${curl_bin}"
command -v "$restore_bin" >/dev/null || fail "dns-restore executable not found: ${restore_bin}"
command -v jq >/dev/null || fail "jq is required"
command -v gpg >/dev/null || fail "gpg is required"
command -v tar >/dev/null || fail "tar is required"
command -v cmp >/dev/null || fail "cmp is required"

if [[ -L "$output_directory" ]]; then
  fail "BACKUP_OUTPUT_DIR must not be a symbolic link"
fi
mkdir -p "$output_directory"
if [[ ! -d "$output_directory" ]]; then
  fail "BACKUP_OUTPUT_DIR is not a directory"
fi
if find "$output_directory" -mindepth 1 -maxdepth 1 -print -quit | grep -q .; then
  fail "BACKUP_OUTPUT_DIR must be empty"
fi

temporary_directory="$(mktemp -d "${TMPDIR:-/tmp}/dns-snapshot-backup.XXXXXX")"
cleanup() {
  if [[ -n "${temporary_directory:-}" && -d "$temporary_directory" ]]; then
    rm -rf -- "$temporary_directory"
  fi
}
trap cleanup EXIT

max_platform_bytes=268435456
raw_snapshot="${temporary_directory}/snapshot.json"
raw_platform="${temporary_directory}/platform.db"
platform_headers="${temporary_directory}/platform.headers"
response_headers="${temporary_directory}/response.headers"
authorization_header="${temporary_directory}/authorization.header"
payload_directory="${temporary_directory}/payload"
gpg_home="${temporary_directory}/gnupg"
mkdir -m 0700 "$payload_directory" "$gpg_home"
printf 'Authorization: Bearer %s\n' "$snapshot_token" >"$authorization_header"
unset snapshot_token

"$curl_bin" \
  --request GET \
  --proto '=https' \
  --max-redirs 0 \
  --fail \
  --silent \
  --show-error \
  --connect-timeout 10 \
  --max-time 90 \
  --retry 3 \
  --retry-delay 2 \
  --retry-connrefused \
  --max-filesize "$max_snapshot_bytes" \
  --header "@${authorization_header}" \
  --header 'Accept: application/json' \
  --user-agent 'castlemilk-dns-snapshot-backup/1' \
  --dump-header "$response_headers" \
  --output "$raw_snapshot" \
  "$snapshot_url"

status_code="$(awk '/^HTTP\// { value=$2; gsub(/\r/, "", value) } END { print value }' "$response_headers")"
if [[ "$status_code" != 200 ]]; then
  fail "snapshot endpoint did not return HTTP 200"
fi
content_type="$(header_value Content-Type "$response_headers")"
content_type_lower="$(LC_ALL=C tr '[:upper:]' '[:lower:]' <<<"$content_type")"
if [[ "$content_type_lower" != application/json* ]]; then
  fail "snapshot endpoint did not return application/json"
fi
cache_control="$(header_value Cache-Control "$response_headers")"
cache_control_lower="$(LC_ALL=C tr '[:upper:]' '[:lower:]' <<<"$cache_control")"
if [[ "$cache_control_lower" != *no-store* ]]; then
  fail "snapshot endpoint did not return Cache-Control: no-store"
fi

snapshot_size="$(wc -c <"$raw_snapshot" | tr -d '[:space:]')"
if [[ ! "$snapshot_size" =~ ^[0-9]+$ ]] ||
   ((snapshot_size < 1 || snapshot_size > max_snapshot_bytes)); then
  fail "downloaded snapshot size is outside the configured bound"
fi

verification_json="$("$restore_bin" --snapshot "$raw_snapshot" --verify-only)" ||
  fail "dns-restore rejected the downloaded snapshot"
if ! jq -e '
  type == "object"
    and .status == "valid"
    and (.zones | type == "number" and floor == . and . >= 0)
    and (.records | type == "number" and floor == . and . >= 0)
    and (.generated_at | type == "string" and length > 0)
    and (.checksum | type == "string" and test("^[0-9a-f]{64}$"))
' <<<"$verification_json" >/dev/null; then
  fail "dns-restore returned malformed verification metadata"
fi

snapshot_checksum="$(jq -r '.checksum' <<<"$verification_json")"
etag="$(header_value ETag "$response_headers")"
if [[ "$etag" != "\"sha256:${snapshot_checksum}\"" ]]; then
  fail "snapshot ETag does not match the verified document checksum"
fi

# Platform store. The control process owns bbolt's exclusive lock, so the only
# consistent copy is the one it streams. A 404 means this control plane has no
# platform store (an older release, or role=all without a snapshot token); that
# is recorded, not a failure. Any other status is.
"$curl_bin" \
  --request GET \
  --proto '=https' \
  --max-redirs 0 \
  --silent \
  --show-error \
  --connect-timeout 10 \
  --max-time 600 \
  --retry 3 \
  --retry-delay 2 \
  --retry-connrefused \
  --max-filesize "$max_platform_bytes" \
  --header "@${authorization_header}" \
  --header 'Accept: application/octet-stream' \
  --user-agent 'castlemilk-dns-snapshot-backup/1' \
  --dump-header "$platform_headers" \
  --output "$raw_platform" \
  "$platform_backup_url"

platform_status="$(awk '/^HTTP\// { value=$2; gsub(/\r/, "", value) } END { print value }' "$platform_headers")"
platform_state=absent
platform_bytes=0
platform_sha256=""
case "$platform_status" in
  200)
    platform_state=present
    ;;
  404)
    rm -f -- "$raw_platform"
    ;;
  *)
    fail "platform backup endpoint returned HTTP ${platform_status:-<none>}"
    ;;
esac

if [[ "$platform_state" == present ]]; then
  platform_content_type="$(header_value Content-Type "$platform_headers")"
  platform_content_type_lower="$(LC_ALL=C tr '[:upper:]' '[:lower:]' <<<"$platform_content_type")"
  if [[ "$platform_content_type_lower" != application/octet-stream* ]]; then
    fail "platform backup endpoint did not return application/octet-stream"
  fi
  platform_bytes="$(wc -c <"$raw_platform" | tr -d '[:space:]')"
  platform_length="$(header_value Content-Length "$platform_headers")"
  platform_length="${platform_length//[[:space:]]/}"
  if [[ ! "$platform_length" =~ ^[0-9]+$ ]]; then
    fail "platform backup response did not declare a Content-Length"
  fi
  if [[ "$platform_bytes" != "$platform_length" ]]; then
    fail "platform backup is truncated: ${platform_bytes} bytes received, ${platform_length} declared"
  fi
  if ((platform_bytes < 32 || platform_bytes > max_platform_bytes)); then
    fail "platform backup size is outside the configured bound"
  fi
  # bbolt writes its magic 0xED0CDAED little-endian at byte offset 16 of the
  # meta page. Checking it here means a truncated body or an HTML error page
  # can never be archived as a database.
  platform_magic="$(od -An -v -tx1 -j 16 -N 4 "$raw_platform" | tr -d '[:space:]')"
  if [[ "$platform_magic" != "edda0ced" ]]; then
    fail "platform backup does not carry the bbolt database magic"
  fi
  platform_sha256="$(sha256_file "$raw_platform")"
fi

raw_sha256="$(sha256_file "$raw_snapshot")"
retrieved_at="$(date -u +'%Y-%m-%dT%H:%M:%SZ')"
source_revision_lower="$(LC_ALL=C tr '[:upper:]' '[:lower:]' <<<"$source_revision")"
cp "$raw_snapshot" "${payload_directory}/snapshot.json"
printf '%s\n' "$verification_json" >"${payload_directory}/verification.json"
archive_members=(snapshot.json verification.json manifest.json)
if [[ "$platform_state" == present ]]; then
  cp "$raw_platform" "${payload_directory}/platform.db"
  archive_members+=(platform.db)
fi
jq -n \
  --arg retrievedAt "$retrieved_at" \
  --arg sourceURL "$snapshot_url" \
  --arg sourceRevision "$source_revision_lower" \
  --arg rawSHA256 "$raw_sha256" \
  --arg platformState "$platform_state" \
  --arg platformURL "$platform_backup_url" \
  --arg platformSHA256 "$platform_sha256" \
  --argjson platformBytes "$platform_bytes" \
  --argjson bytes "$snapshot_size" \
  --argjson verification "$verification_json" '{
    format: "castlemilk-dns-snapshot-backup-v1",
    retrieved_at: $retrievedAt,
    source_url: $sourceURL,
    verifier_revision: $sourceRevision,
    snapshot_bytes: $bytes,
    snapshot_file_sha256: $rawSHA256,
    verification: $verification,
    platform_store: (
      if $platformState == "present" then {
        status: "present",
        source_url: $platformURL,
        bytes: $platformBytes,
        file_sha256: $platformSHA256
      } else {
        status: "absent",
        source_url: $platformURL
      } end
    )
  }' >"${payload_directory}/manifest.json"

timestamp="$(date -u +'%Y%m%dT%H%M%SZ')"
# Keep the checksum inside the encrypted payload. Workflow logs and artifact
# file names are often more broadly visible than artifact contents.
archive_name="dns-snapshot-${timestamp}.tar.gz"
plaintext_archive="${temporary_directory}/${archive_name}"
encrypted_name="${archive_name}.gpg"
encrypted_archive="${temporary_directory}/${encrypted_name}"
verification_archive="${temporary_directory}/verification.tar.gz"

tar -czf "$plaintext_archive" -C "$payload_directory" "${archive_members[@]}"

exec 3<<<"$backup_passphrase"
gpg --homedir "$gpg_home" --batch --yes --quiet --no-symkey-cache --pinentry-mode loopback \
  --passphrase-fd 3 --symmetric --cipher-algo AES256 --s2k-mode 3 \
  --s2k-digest-algo SHA512 --s2k-count 65011712 --compress-algo none \
  --output "$encrypted_archive" "$plaintext_archive"
exec 3<&-

exec 3<<<"$backup_passphrase"
gpg --homedir "$gpg_home" --batch --yes --quiet --no-symkey-cache --pinentry-mode loopback \
  --passphrase-fd 3 --decrypt --output "$verification_archive" "$encrypted_archive"
exec 3<&-
unset backup_passphrase

if ! cmp -s "$plaintext_archive" "$verification_archive"; then
  fail "encrypted backup did not decrypt byte-for-byte"
fi
expected_members="$(printf '%s\n' "${archive_members[@]}" | LC_ALL=C sort)"
observed_members="$(tar -tzf "$verification_archive" | LC_ALL=C sort)"
if [[ "$observed_members" != "$expected_members" ]]; then
  fail "encrypted archive contains an unexpected file set"
fi

encrypted_sha256="$(sha256_file "$encrypted_archive")"
install -m 0600 "$encrypted_archive" "${output_directory}/${encrypted_name}"
printf '%s  %s\n' "$encrypted_sha256" "$encrypted_name" >"${output_directory}/${encrypted_name}.sha256"
chmod 0600 "${output_directory}/${encrypted_name}.sha256"

printf 'Created verified encrypted snapshot backup: %s\n' "${output_directory}/${encrypted_name}"
printf 'Ciphertext SHA-256: %s\n' "$encrypted_sha256"
printf 'Platform store: %s\n' "$platform_state"
