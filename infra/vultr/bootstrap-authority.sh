#!/usr/bin/env bash
set -euo pipefail
set +x

if [[ -z "${SECONDARY_AUTHORITY_IP:-}" ]]; then
  printf 'SECONDARY_AUTHORITY_IP is required\n' >&2
  exit 1
fi
if [[ ! "$SECONDARY_AUTHORITY_IP" =~ ^[0-9A-Fa-f:.]+$ ]]; then
  printf 'SECONDARY_AUTHORITY_IP must be a literal IPv4 or IPv6 address\n' >&2
  exit 1
fi

snapshot_token="${DNS_SNAPSHOT_BEARER_TOKEN:-}"
# Retain the credential only as a non-exported shell variable so helper
# processes used for host-key verification cannot inherit it.
unset DNS_SNAPSHOT_BEARER_TOKEN
if [[ -z "$snapshot_token" ]]; then
  read -r -s -p 'Snapshot bearer token: ' snapshot_token
  printf '\n' >&2
fi
if [[ ! "$snapshot_token" =~ ^[!-~]{32,}$ ]]; then
  printf 'Snapshot bearer token must contain at least 32 visible non-space ASCII characters\n' >&2
  exit 1
fi
if [[ "$snapshot_token" == *$'\n'* || "$snapshot_token" == *$'\r'* ]]; then
  printf 'Snapshot bearer token cannot contain a newline\n' >&2
  exit 1
fi

remote_user="${SECONDARY_AUTHORITY_USER:-root}"
if [[ ! "$remote_user" =~ ^[a-z_][a-z0-9_-]*$ ]]; then
  printf 'SECONDARY_AUTHORITY_USER is not a safe account name\n' >&2
  exit 1
fi
ssh_target="${remote_user}@${SECONDARY_AUTHORITY_IP}"
expected_host_key="${SECONDARY_AUTHORITY_HOST_KEY_SHA256:-}"
if [[ -z "$expected_host_key" ]]; then
  read -r -p 'Secondary ED25519 SSH host-key fingerprint (SHA256:...): ' expected_host_key
fi
if [[ ! "$expected_host_key" =~ ^SHA256:[A-Za-z0-9+/]{43}$ ]]; then
  printf 'SECONDARY_AUTHORITY_HOST_KEY_SHA256 must be an SHA256 SSH fingerprint\n' >&2
  exit 1
fi

known_hosts_file="$(mktemp "${TMPDIR:-/tmp}/simpledns-known-hosts.XXXXXX")"
candidate_host_keys="$(mktemp "${TMPDIR:-/tmp}/simpledns-host-keys.XXXXXX")"
# Invoked by the EXIT trap below. SC2329 is the function itself looking
# uncalled; SC2317 is its body looking unreachable, which shellcheck 0.9 (the
# ubuntu-24.04 runner's version) reports and 0.10+ no longer does.
# shellcheck disable=SC2329,SC2317
cleanup() {
  rm -f -- "$known_hosts_file" "$candidate_host_keys"
}
trap cleanup EXIT

ssh_options=(
  -o BatchMode=yes
  -o ConnectTimeout=5
  -o StrictHostKeyChecking=yes
  -o "UserKnownHostsFile=${known_hosts_file}"
  -o GlobalKnownHostsFile=/dev/null
)

printf 'Waiting for SSH on %s...\n' "$SECONDARY_AUTHORITY_IP"
for _ in $(seq 1 60); do
  : >"$candidate_host_keys"
  if ssh-keyscan -T 5 -t ed25519 "$SECONDARY_AUTHORITY_IP" >"$candidate_host_keys" 2>/dev/null; then
    observed_host_key="$(ssh-keygen -lf "$candidate_host_keys" -E sha256 | awk 'NR == 1 { print $2 }')"
    if [[ "$observed_host_key" != "$expected_host_key" ]]; then
      printf 'SSH host-key fingerprint mismatch: expected %s, observed %s\n' \
        "$expected_host_key" "${observed_host_key:-unavailable}" >&2
      exit 1
    fi
    cp "$candidate_host_keys" "$known_hosts_file"
  fi
  if ssh "${ssh_options[@]}" "$ssh_target" true 2>/dev/null; then
    break
  fi
  sleep 5
done

if ! ssh "${ssh_options[@]}" "$ssh_target" true; then
  printf 'SSH did not become ready\n' >&2
  exit 1
fi

printf 'Installing the authority snapshot credential without writing it locally...\n'
printf 'DNS_SNAPSHOT_BEARER_TOKEN=%s\n' "$snapshot_token" |
  ssh "${ssh_options[@]}" "$ssh_target" 'set -eu; umask 077; install -d -m 0700 /etc/simpledns; cat > /etc/simpledns/authority.env; systemctl daemon-reload; systemctl restart simpledns-authority.service'
unset snapshot_token

printf 'Waiting for authority readiness...\n'
for _ in $(seq 1 60); do
  if ssh "${ssh_options[@]}" "$ssh_target" '/usr/local/sbin/simpledns-healthcheck' 2>/dev/null; then
    printf 'Secondary authority is ready.\n'
    exit 0
  fi
  sleep 5
done

ssh "${ssh_options[@]}" "$ssh_target" 'systemctl --no-pager --full status simpledns-authority.service' || true
printf 'Secondary authority did not become ready\n' >&2
exit 1
