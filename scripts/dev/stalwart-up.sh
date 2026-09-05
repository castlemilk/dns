#!/usr/bin/env bash
# Start a local Stalwart mail server for development and the mail integration
# tests, then bootstrap it and mint the control plane's API key.
#
#   scripts/dev/stalwart-up.sh                 # start + bootstrap
#   STALWART_PACKAGE=hosting scripts/dev/stalwart-up.sh
#
# Everything the run needs lives under one directory (default
# $SCRATCH/stalwart-<package>, else $TMPDIR/stalwart-<package>): the container's
# /etc, /var/lib and /var/log mounts, the permanent admin secret and the
# generated API key. Nothing secret is ever printed to the terminal — the
# script prints the path of the env file instead, so a shared terminal, a CI
# log or a screen recording never captures a credential.
#
# Stop and remove ONLY this container when you are done:
#   docker rm -f simple-stalwart-<package>
#
# See docs/mail-runbook.md for the production equivalent.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

package="${STALWART_PACKAGE:-mail}"
container="${STALWART_CONTAINER:-simple-stalwart-${package}}"
image="${STALWART_IMAGE:-stalwartlabs/stalwart:v0.16}"

default_dir="${SCRATCH:-${TMPDIR:-/tmp}}/stalwart-${package}"
dir="${STALWART_DIR:-$default_dir}"

# Host ports. Override any of them when 20xxx is taken.
port_smtp="${STALWART_PORT_SMTP:-20025}"
port_submissions="${STALWART_PORT_SUBMISSIONS:-20465}"
port_imaps="${STALWART_PORT_IMAPS:-20993}"
port_https="${STALWART_PORT_HTTPS:-20443}"
port_http="${STALWART_PORT_HTTP:-20080}"

hostname="${STALWART_HOSTNAME:-mail.local.test}"
default_domain="${STALWART_DEFAULT_DOMAIN:-local.test}"

die() { printf 'stalwart-up: %s\n' "$*" >&2; exit 1; }
note() { printf 'stalwart-up: %s\n' "$*" >&2; }

# random_secret LENGTH — LENGTH alphanumeric characters from /dev/urandom.
# The fixed-size read comes first so no stage of the pipeline is killed by
# SIGPIPE, which `set -o pipefail` would otherwise turn into a failed run.
random_secret() {
  head -c 512 /dev/urandom | LC_ALL=C tr -dc 'A-Za-z0-9' | cut -c1-"$1"
}

command -v docker >/dev/null 2>&1 || die "docker is not on PATH"

if docker ps -a --format '{{.Names}}' | grep -qx "$container"; then
  die "container $container already exists; remove it with 'docker rm -f $container' or set STALWART_CONTAINER"
fi

for port in "$port_smtp" "$port_submissions" "$port_imaps" "$port_https" "$port_http"; do
  if lsof -nP -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1; then
    die "port $port is already in use; override STALWART_PORT_* and retry"
  fi
done

mkdir -p "$dir/etc" "$dir/lib" "$dir/log"
chmod 700 "$dir"
# The image runs as UID 2000. Linux bind mounts need the ownership; on macOS
# the Docker VM maps it already, so a failure here is not fatal.
chown -R 2000:2000 "$dir/etc" "$dir/lib" "$dir/log" 2>/dev/null || true

# The temporary bootstrap admin. It is replaced by the permanent admin the
# bootstrap call returns and is never used again.
recovery_admin_password="$(random_secret 32)"
umask 077
printf '%s\n' "$recovery_admin_password" >"$dir/recovery-admin.secret"

note "starting $container from $image"
docker run -d --name "$container" \
  -p "127.0.0.1:${port_smtp}:25" \
  -p "127.0.0.1:${port_submissions}:465" \
  -p "127.0.0.1:${port_imaps}:993" \
  -p "127.0.0.1:${port_https}:443" \
  -p "127.0.0.1:${port_http}:8080" \
  -e "STALWART_RECOVERY_ADMIN=admin:${recovery_admin_password}" \
  -v "$dir/etc:/etc/stalwart" \
  -v "$dir/lib:/var/lib/stalwart" \
  -v "$dir/log:/var/log/stalwart" \
  "$image" >/dev/null

base="http://127.0.0.1:${port_http}"
note "waiting for $base to answer"
for _ in $(seq 1 60); do
  if curl -fsS -o /dev/null "${base}/healthz/live" 2>/dev/null; then
    break
  fi
  # In bootstrap mode /healthz is not mounted yet; a 4xx from /jmap still
  # proves the listener is up.
  if curl -sS -o /dev/null "${base}/jmap" 2>/dev/null; then
    break
  fi
  sleep 1
done

STALWART_PACKAGE="$package" \
STALWART_CONTAINER="$container" \
STALWART_DIR="$dir" \
STALWART_PORT_HTTP="$port_http" \
STALWART_PORT_SMTP="$port_smtp" \
STALWART_HOSTNAME="$hostname" \
STALWART_DEFAULT_DOMAIN="$default_domain" \
  "$here/stalwart-bootstrap.sh"
