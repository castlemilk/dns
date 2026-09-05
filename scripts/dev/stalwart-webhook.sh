#!/usr/bin/env bash
# Point a local Stalwart container's delivery events at this control plane's
# receiver, and write the matching MAIL_WEBHOOK_SECRET into the run's env file.
#
#   scripts/dev/stalwart-up.sh                 # start the server first
#   scripts/dev/stalwart-webhook.sh            # then this
#
# Community Stalwart CAN push delivery events to a webhook — that was verified
# empirically against v0.16.20, and docs/mail-runbook.md records the experiment
# under "Delivery statistics". This script does the mail-server half of the wiring:
#
#   1. creates (or updates) the single x:WebHook object,
#   2. subscribes it to the two per-recipient outcome events the facade counts,
#   3. sets its bearer token AND its HMAC signature key to one shared secret,
#   4. restarts the container, because a webhook change does NOT take effect
#      until the server reloads its configuration — an update alone is silently
#      ignored, which is the single easiest thing to get wrong here,
#   5. appends MAIL_WEBHOOK_SECRET and SIMPLE_TEST_STALWART_WEBHOOK_* to the
#      run's mail.env.
#
# The secret is generated here and written only to files under the data
# directory, mode 0600. Nothing secret is printed.
#
# The URL must be reachable FROM THE CONTAINER. On Docker Desktop that is
# host.docker.internal; the default below assumes the control plane (or the
# integration test's listener) is on the host.
#
# Remove the webhook again with:
#   scripts/dev/stalwart-webhook.sh --remove
set -euo pipefail

package="${STALWART_PACKAGE:-mail}"
container="${STALWART_CONTAINER:-simple-stalwart-${package}}"
default_dir="${SCRATCH:-${TMPDIR:-/tmp}}/stalwart-${package}"
dir="${STALWART_DIR:-$default_dir}"
port_http="${STALWART_PORT_HTTP:-20080}"

# Where the control plane listens, as seen from inside the container.
webhook_host="${STALWART_WEBHOOK_HOST:-host.docker.internal}"
webhook_port="${STALWART_WEBHOOK_PORT:-8080}"
webhook_path="${STALWART_WEBHOOK_PATH:-/mail/v1/delivery-events}"
webhook_url="${STALWART_WEBHOOK_URL:-http://${webhook_host}:${webhook_port}${webhook_path}}"

base="http://127.0.0.1:${port_http}"
env_file="$dir/mail.env"
admin_file="$dir/admin.secret"
secret_file="$dir/webhook.secret"

die() { printf 'stalwart-webhook: %s\n' "$*" >&2; exit 1; }
note() { printf 'stalwart-webhook: %s\n' "$*" >&2; }

command -v curl >/dev/null 2>&1 || die "curl is not on PATH"
command -v jq >/dev/null 2>&1 || die "jq is not on PATH"
command -v docker >/dev/null 2>&1 || die "docker is not on PATH"
[ -s "$admin_file" ] || die "no administrator credential at $admin_file; run scripts/dev/stalwart-up.sh first"

admin_user="$(sed -n '1p' "$admin_file")"
admin_secret="$(sed -n '2p' "$admin_file")"

# jmap BODY — one JMAP request as the administrator, fails on HTTP != 200.
jmap() {
  local status response
  response="$(curl -sS -u "${admin_user}:${admin_secret}" -w '\n%{http_code}' \
    -X POST "${base}/jmap" -H 'Content-Type: application/json' \
    -d "$(jq -n --argjson mc "$1" '{using:["urn:ietf:params:jmap:core","urn:stalwart:jmap"],methodCalls:$mc}')")"
  status="${response##*$'\n'}"
  response="${response%$'\n'*}"
  [ "$status" = "200" ] || die "JMAP request failed with HTTP $status"
  printf '%s' "$response"
}

restart() {
  note "restarting $container — a webhook change is not live until the server reloads"
  docker restart "$container" >/dev/null
  for _ in $(seq 1 90); do
    if curl -fsS -o /dev/null "${base}/healthz/ready" 2>/dev/null; then return 0; fi
    sleep 1
  done
  die "$container never reported ready again (see 'docker logs $container')"
}

existing="$(jmap '[["x:WebHook/query",{},"0"]]' | jq -r '.methodResponses[0][1].ids[0] // empty')"

if [ "${1:-}" = "--remove" ]; then
  [ -n "$existing" ] || { note "no webhook is configured"; exit 0; }
  jmap "$(jq -n --arg id "$existing" '[["x:WebHook/set",{destroy:[$id]},"0"]]')" >/dev/null
  rm -f "$secret_file"
  restart
  note "removed. Unset MAIL_WEBHOOK_SECRET to take the receiver off the control plane too."
  exit 0
fi

umask 077
if [ -s "$secret_file" ]; then
  secret="$(cat "$secret_file")"
  note "reusing the existing shared secret"
else
  secret="$(head -c 512 /dev/urandom | LC_ALL=C tr -dc 'A-Za-z0-9' | cut -c1-48)"
  printf '%s\n' "$secret" >"$secret_file"
fi

# The two per-recipient outcome events the facade counts, and nothing else.
# delivery.delivered is deliberately absent: it fires alongside dsn-success for
# remote deliveries only, so subscribing to both would double every outbound
# message and count no local one. See internal/mail/delivery.go.
events_json='{"delivery.dsn-success": true, "delivery.dsn-perm-fail": true}'

# bearerToken and signatureKey are two independent settings on the mail server,
# and this control plane checks the bearer against MAIL_WEBHOOK_SECRET and
# verifies X-Signature with the same value — so both are set to one generated
# secret here, which is what a production deployment must do too. Give them
# different values and every delivery is refused with bad_signature; the console
# then says the deliveries are being rejected rather than showing a zero.
#
# throttle is the minimum gap between deliveries and must be a valid duration;
# 0 is rejected. 1 s batches a burst into one POST, which is what the receiver
# is built for.
payload="$(jq -n --arg url "$webhook_url" --arg secret "$secret" --argjson events "$events_json" '{
  url: $url,
  enable: true,
  level: "info",
  lossy: false,
  eventsPolicy: "include",
  events: $events,
  throttle: 1000,
  timeout: 10000,
  discardAfter: 300000,
  allowInvalidCerts: false,
  httpAuth: {"@type":"Bearer", bearerToken: {"@type":"Value", secret: $secret}},
  httpHeaders: {},
  signatureKey: {"@type":"Value", secret: $secret}
}')"

if [ -n "$existing" ]; then
  note "updating the existing webhook"
  response="$(jmap "$(jq -n --arg id "$existing" --argjson hook "$payload" \
    '[["x:WebHook/set",{update:{($id): $hook}},"0"]]')")"
  ok="$(printf '%s' "$response" | jq -r '.methodResponses[0][1].updated | keys[0] // empty')"
else
  note "creating the webhook"
  response="$(jmap "$(jq -n --argjson hook "$payload" \
    '[["x:WebHook/set",{create:{w1: $hook}},"0"]]')")"
  ok="$(printf '%s' "$response" | jq -r '.methodResponses[0][1].created.w1.id // empty')"
fi
if [ -z "$ok" ]; then
  printf '%s' "$response" | jq -r '.methodResponses[0][1] | (.notCreated // .notUpdated // .) | tostring' >&2
  die "the mail server refused the webhook"
fi

restart

# Refresh the env file's webhook lines without duplicating them.
if [ -f "$env_file" ]; then
  grep -v -e '^MAIL_WEBHOOK_SECRET=' -e '^SIMPLE_TEST_STALWART_WEBHOOK_' "$env_file" >"$env_file.tmp"
  mv "$env_file.tmp" "$env_file"
else
  : >"$env_file"
fi
cat >>"$env_file" <<EOF
MAIL_WEBHOOK_SECRET=${secret}
SIMPLE_TEST_STALWART_WEBHOOK_SECRET=${secret}
SIMPLE_TEST_STALWART_WEBHOOK_PORT=${webhook_port}
SIMPLE_TEST_STALWART_WEBHOOK_PATH=${webhook_path}
EOF
chmod 600 "$env_file"

cat >&2 <<EOF
stalwart-webhook: ready.

  events              delivery.dsn-success, delivery.dsn-perm-fail
  posted to           ${webhook_url}
  bearer + HMAC key   written to ${secret_file} (not printed)

  The receiver must listen on port ${webhook_port} of the host, reachable from the
  container as ${webhook_host}. Run the control plane with the secret set:

    set -a; . "${env_file}"; set +a

  run the delivery-event integration test:
    set -a; . "${env_file}"; set +a; go test ./internal/mail/ -run IntegrationMailDeliveryEvents -v

  remove it again:  scripts/dev/stalwart-webhook.sh --remove
EOF
