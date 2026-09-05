#!/usr/bin/env bash
# Bootstrap a freshly started Stalwart container and mint the control plane's
# API key. scripts/dev/stalwart-up.sh calls this; run it directly to re-mint a
# key against a container that is already bootstrapped.
#
# Steps (all headless, no web wizard — see mail-facts.md §1 and §2):
#   1. x:Bootstrap/set as the temporary recovery admin  → permanent admin secret
#   2. docker restart                                   → the temp credential dies
#   3. x:Account/set create simple-cp@<domain> with the exact sys* permission
#      set the facade uses (permissions "Replace", so the key can do nothing
#      else)
#   4. x:ApiKey/set create as simple-cp, accountId omitted (passing another
#      account's id answers notCreated notFound)
#
# Secrets are written only to files under the data directory, mode 0600. The
# terminal gets the non-secret env lines and the path of the env file.
set -euo pipefail

package="${STALWART_PACKAGE:-mail}"
container="${STALWART_CONTAINER:-simple-stalwart-${package}}"
default_dir="${SCRATCH:-${TMPDIR:-/tmp}}/stalwart-${package}"
dir="${STALWART_DIR:-$default_dir}"
port_http="${STALWART_PORT_HTTP:-20080}"
port_smtp="${STALWART_PORT_SMTP:-20025}"
hostname="${STALWART_HOSTNAME:-mail.local.test}"
default_domain="${STALWART_DEFAULT_DOMAIN:-local.test}"
control_user="${STALWART_CONTROL_USER:-simple-cp}"

base="http://127.0.0.1:${port_http}"
env_file="$dir/mail.env"
admin_file="$dir/admin.secret"
key_file="$dir/api-key.secret"

die() { printf 'stalwart-bootstrap: %s\n' "$*" >&2; exit 1; }
note() { printf 'stalwart-bootstrap: %s\n' "$*" >&2; }

# random_secret LENGTH — LENGTH alphanumeric characters from /dev/urandom.
# The fixed-size read comes first so no stage of the pipeline is killed by
# SIGPIPE, which `set -o pipefail` would otherwise turn into a failed run.
random_secret() {
  head -c 512 /dev/urandom | LC_ALL=C tr -dc 'A-Za-z0-9' | cut -c1-"$1"
}

command -v curl >/dev/null 2>&1 || die "curl is not on PATH"
command -v jq >/dev/null 2>&1 || die "jq is not on PATH"
[ -d "$dir" ] || die "data directory $dir does not exist; run scripts/dev/stalwart-up.sh first"

umask 077

# jmap USER PASSWORD BODY — one JMAP request, basic auth, fails on HTTP != 200.
jmap() {
  local user="$1" password="$2" body="$3" status response
  response="$(curl -sS -u "${user}:${password}" -w '\n%{http_code}' \
    -X POST "${base}/jmap" -H 'Content-Type: application/json' -d "$body")"
  status="${response##*$'\n'}"
  response="${response%$'\n'*}"
  [ "$status" = "200" ] || die "JMAP request failed with HTTP $status"
  printf '%s' "$response"
}

# The management permissions the facade needs (spec2 §5.1). A "set" of
# permissions is encoded as an object of name -> true, and the Merge variant
# of x:Permissions carries it under enabledPermissions (verified against
# GET /api/schema on v0.16.20).
#
# Merge, not Replace: Stalwart refuses to create an account whose role grants
# permissions the caller does not itself hold ("You are not authorized to
# grant permissions: jmapPrincipalCreate, …"), so a Replace-scoped principal
# cannot create mailboxes at all. The account therefore keeps the ordinary
# User role and gains the sys* management set on top.
permissions_json="$(jq -n '[
  "authenticate",
  "sysDomainGet","sysDomainCreate","sysDomainUpdate","sysDomainDestroy","sysDomainQuery",
  "sysAccountGet","sysAccountCreate","sysAccountUpdate","sysAccountDestroy","sysAccountQuery",
  "sysAccountPasswordUpdate",
  "sysMailingListGet","sysMailingListCreate","sysMailingListUpdate","sysMailingListDestroy","sysMailingListQuery",
  "sysDkimSignatureGet","sysDkimSignatureQuery","sysDkimSignatureDestroy",
  "sysQueuedMessageGet","sysQueuedMessageQuery",
  "sysSystemSettingsGet",
  "sysApiKeyGet","sysApiKeyCreate","sysApiKeyQuery","sysApiKeyDestroy"
] | map({(.): true}) | add')"

if [ ! -s "$admin_file" ]; then
  [ -s "$dir/recovery-admin.secret" ] || die "no recovery admin secret at $dir/recovery-admin.secret"
  recovery_password="$(cat "$dir/recovery-admin.secret")"

  note "bootstrapping (serverHostname=${hostname}, defaultDomain=${default_domain})"
  bootstrap_body="$(jq -n --arg host "$hostname" --arg domain "$default_domain" '{
    using: ["urn:ietf:params:jmap:core","urn:stalwart:jmap"],
    methodCalls: [["x:Bootstrap/set", {update: {singleton: {
      serverHostname: $host,
      defaultDomain: $domain,
      requestTlsCertificate: false,
      generateDkimKeys: true,
      dataStore: {"@type":"RocksDb", path:"/var/lib/stalwart/", blobSize:16834, bufferSize:134217728, poolWorkers:null, cacheSize:134217728},
      blobStore: {"@type":"Default"},
      searchStore: {"@type":"Default"},
      inMemoryStore: {"@type":"Default"},
      directory: {"@type":"Internal"},
      tracer: {"@type":"Stdout", ansi:false, lossy:false, multiline:false, enable:true, level:"info", events:{}, eventsPolicy:"exclude"},
      dnsServer: {"@type":"Manual"}
    }}}, "0"]]
  }')"
  response="$(jmap admin "$recovery_password" "$bootstrap_body")"

  admin_user="$(printf '%s' "$response" | jq -r '.methodResponses[0][1].updated.singleton.username // empty')"
  admin_secret="$(printf '%s' "$response" | jq -r '.methodResponses[0][1].updated.singleton.secret // empty')"
  if [ -z "$admin_user" ] || [ -z "$admin_secret" ]; then
    die "bootstrap did not return an administrator credential (see 'docker logs $container')"
  fi
  printf '%s\n%s\n' "$admin_user" "$admin_secret" >"$admin_file"

  note "restarting $container so the bootstrap configuration takes effect"
  docker restart "$container" >/dev/null
else
  note "already bootstrapped; reusing the administrator credential"
fi

admin_user="$(sed -n '1p' "$admin_file")"
admin_secret="$(sed -n '2p' "$admin_file")"

note "waiting for $base to report ready"
ready=""
for _ in $(seq 1 90); do
  if curl -fsS -o /dev/null "${base}/healthz/ready" 2>/dev/null; then ready=yes; break; fi
  sleep 1
done
[ -n "$ready" ] || die "$base never reported ready (see 'docker logs $container')"

edition="$(curl -fsS -u "${admin_user}:${admin_secret}" "${base}/api/account" | jq -r '.edition // "unknown"')"
note "authenticated as ${admin_user} (edition: ${edition})"

# The control-plane user. A second run reuses the existing one.
control_address="${control_user}@${default_domain}"
domain_id="$(jmap "$admin_user" "$admin_secret" "$(jq -n --arg name "$default_domain" '{
  using: ["urn:ietf:params:jmap:core","urn:stalwart:jmap"],
  methodCalls: [["x:Domain/query", {filter: {name: $name}}, "0"]]
}')" | jq -r '.methodResponses[0][1].ids[0] // empty')"
[ -n "$domain_id" ] || die "the default domain ${default_domain} does not exist on the server"

control_id="$(jmap "$admin_user" "$admin_secret" "$(jq -n --arg name "$control_user" --arg domain "$domain_id" '{
  using: ["urn:ietf:params:jmap:core","urn:stalwart:jmap"],
  methodCalls: [["x:Account/query", {filter: {"@type":"User", name: $name, domainId: $domain}}, "0"]]
}')" | jq -r '.methodResponses[0][1].ids[0] // empty')"

control_password="$(random_secret 40)"
if [ -z "$control_id" ]; then
  note "creating the control-plane account ${control_address}"
  create_body="$(jq -n --arg name "$control_user" --arg domain "$domain_id" \
    --arg secret "$control_password" --argjson permissions "$permissions_json" '{
    using: ["urn:ietf:params:jmap:core","urn:stalwart:jmap"],
    methodCalls: [["x:Account/set", {create: {u1: {
      "@type": "User",
      name: $name,
      domainId: $domain,
      description: "simple control plane",
      credentials: {"0": {"@type":"Password", secret: $secret}},
      roles: {"@type":"User"},
      permissions: {"@type":"Merge", enabledPermissions: $permissions},
      quotas: {},
      aliases: {},
      locale: "en-US",
      memberGroupIds: {},
      encryptionAtRest: {"@type":"Disabled"}
    }}}, "0"]]
  }')"
  response="$(jmap "$admin_user" "$admin_secret" "$create_body")"
  control_id="$(printf '%s' "$response" | jq -r '.methodResponses[0][1].created.u1.id // empty')"
  if [ -z "$control_id" ]; then
    printf '%s' "$response" | jq -r '.methodResponses[0][1].notCreated // .methodResponses[0][1] | tostring' >&2
    die "could not create ${control_address}"
  fi
else
  note "resetting the control-plane account's password"
  update_body="$(jq -n --arg id "$control_id" --arg secret "$control_password" --argjson permissions "$permissions_json" '{
    using: ["urn:ietf:params:jmap:core","urn:stalwart:jmap"],
    methodCalls: [["x:Account/set", {update: {($id): {
      credentials: {"0": {"@type":"Password", secret: $secret}},
      permissions: {"@type":"Merge", enabledPermissions: $permissions}
    }}}, "0"]]
  }')"
  jmap "$admin_user" "$admin_secret" "$update_body" >/dev/null
fi

# The API key is minted BY the owning account with accountId omitted; passing
# another account's id answers notCreated {"type":"notFound"} (mail-facts §2).
note "minting the control-plane API key"
key_body='{
  "using": ["urn:ietf:params:jmap:core","urn:stalwart:jmap"],
  "methodCalls": [["x:ApiKey/set", {"create": {"k1": {
    "description": "simple control plane",
    "permissions": {"@type":"Inherit"},
    "allowedIps": {},
    "expiresAt": null
  }}}, "0"]]
}'
response="$(jmap "$control_address" "$control_password" "$key_body")"
api_key="$(printf '%s' "$response" | jq -r '.methodResponses[0][1].created.k1.secret // empty')"
if [ -z "$api_key" ]; then
  printf '%s' "$response" | jq -r '.methodResponses[0][1].notCreated // .methodResponses[0][1] | tostring' >&2
  die "could not create the API key"
fi
printf '%s\n' "$api_key" >"$key_file"

cat >"$env_file" <<EOF
# Generated by scripts/dev/stalwart-bootstrap.sh. Contains a credential:
# keep it out of git and out of shared terminals.
MAIL_API_URL=${base}
MAIL_API_TOKEN=${api_key}
MAIL_HOSTNAME=${hostname}
SIMPLE_TEST_STALWART_URL=${base}
SIMPLE_TEST_STALWART_TOKEN=${api_key}
SIMPLE_TEST_STALWART_SMTP=127.0.0.1:${port_smtp}
EOF
chmod 600 "$env_file"

cat >&2 <<EOF
stalwart-bootstrap: ready.

  MAIL_API_URL=${base}
  MAIL_HOSTNAME=${hostname}
  MAIL_API_TOKEN     written to ${key_file} (not printed)

  source the whole set with:   set -a; . "${env_file}"; set +a
  run the integration tests:   set -a; . "${env_file}"; set +a; make test-integration-stalwart
  stop and remove the server:  docker rm -f ${container}
EOF
