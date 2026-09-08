# Production launch and cutover runbook

This runbook defines the launch gate for Deep Hosting. A successful Kubernetes
rollout is not, by itself, permission to delegate a zone. Delegation happens
only after two independently reachable authorities pass the direct and
recursive checks below.

## Production topology

```text
                                        control plane (Sydney VKE)
operator -> bearer-auth Connect API -> one control pod -> bbolt retained PVC
                                                |
                                    authenticated snapshots
                                      /                   \
                          cluster-local                    HTTPS
                              /                               \
             three read-only authority pods       Melbourne authority VM
                         |                                  |
                  Vultr DNS LB                         public IPv4/IPv6
                         |                                  |
             ns1.<nameserver-domain>             ns2.<nameserver-domain>
```

The authorities compile and atomically publish the same checksummed snapshot.
Each keeps a last-known-good cache and continues answering from it if the
control plane is temporarily unavailable. The VKE endpoint and Melbourne VM
must not share an IP, load balancer, cluster, disk, or region.

The public operator console is optional. The API requires a high-entropy bearer
token in production; the snapshot feed uses a different token. Health endpoints
remain unauthenticated. Never reuse either token for another service.

The first Paprika launch uses these currently unclaimed names:

- `deephost.benebsworth.com` — proxied operator UI and Connect API;
- `snapshot.deephost.benebsworth.com` — unproxied, exact-path snapshot feed;
- `ns1.deephost.benebsworth.com` — Sydney VKE load-balancer authority;
- `ns2.deephost.benebsworth.com` — Melbourne VM authority; and
- `canary.benebsworth.com` — opt-in child delegation used only after direct
  authority tests pass.

## Scope of the first cutover

The first release accepts only unsigned zones whose complete record inventory
is supported by the importer. The parent must return no `DS` record. Do not
remove a production `DS` merely to force a migration through this gate: plan a
DNSSEC-aware migration or keep that zone at its current provider.

For every candidate zone, retain the old provider until the parent delegation
TTL and the zone's previous TTLs have expired. Workload rollback and data
rollback are separate: Paprika can roll back a container, but it cannot undo a
published DNS mutation.

## Required operator inputs

- two nameserver FQDNs hosted in a stable parent zone;
- one unimportant child canary zone and confirmation that it has no `DS`;
- a scoped Cloudflare API token with DNS read/write for the parent zone;
- a Vultr API key supplied through an approved secret manager and source-IP
  allowlist;
- an existing Vultr SSH key ID and the operator's `/32` (and optional IPv6)
  management CIDR;
- a remote S3-compatible state backend;
- distinct 32-byte-or-longer API and snapshot bearer tokens; and
- the published server and web OCI digests from the same Git revision.

Provider credentials are environment variables (`CLOUDFLARE_API_TOKEN` and
`VULTR_API_KEY`) and never Terraform variables, command-line arguments, GitHub
variables, or repository files.

## Scheduled backup and canary controls

Both scheduled workflows are inert until an operator sets an exact repository
enable variable. Keep them disabled while the hostnames, credentials, and
canary delegation are placeholders:

- set `DNS_SNAPSHOT_BACKUP_ENABLED=true` only after the authenticated snapshot
  route is externally reachable and a manual backup recovery test succeeds;
- store `DNS_SNAPSHOT_BEARER_TOKEN` and `DNS_BACKUP_PASSPHRASE` as GitHub
  Actions secrets, never repository variables;
- set `DNS_CANARY_ENABLED=true` only after the child zone is delegated and set
  `DNS_CANARY_ZONE`, `DNS_CANARY_PROBE_NAME`, `DNS_CANARY_PROBE_IPV4`,
  `DNS_EXPECTED_NS1`, `DNS_EXPECTED_NS2`, `DNS_NS1_ADDRESS`, and
  `DNS_NS2_ADDRESS` as non-secret repository variables; and
- use literal public IP addresses for both authority addresses so the direct
  probes cannot be redirected by nameserver address resolution.

The backup workflow accepts at most 8 MiB from the snapshot endpoint, validates
the document checksum and complete zone corpus with `dns-restore --verify-only`,
checks the response ETag, encrypts the backup, verifies the ciphertext by
decrypting it, and retains only the ciphertext and its SHA-256 sidecar as a
90-day GitHub Actions artifact. The canary workflow performs only read-only DNS
queries against both authorities and two public recursive resolvers. Neither
workflow has provider credentials or a provider mutation path.

Treat `DNS_BACKUP_PASSPHRASE` as a recovery key. Generate it with an approved
cryptographic password generator, escrow it in the recovery vault outside
GitHub and outside the DNS cluster, and restrict access under the same
dual-control policy as other disaster-recovery credentials. The GitHub secret
is only an execution copy. Losing the escrowed value makes every matching
artifact unrecoverable; when rotating it, retain each retired value for at
least the lifetime of artifacts encrypted with that value and record the key
version without recording the key itself.

Before enabling the schedule, run it manually from the default branch, download
the artifact, verify its `.sha256` sidecar, decrypt it in an approved recovery
environment, and require the archive to contain only `snapshot.json`,
`verification.json`, and `manifest.json`. Run the checked-out release's
`dns-restore --snapshot snapshot.json --verify-only`, then restore into a new,
empty test database with `dns-restore --snapshot snapshot.json --database
restore-test.db`. Start an isolated control process against that database, feed
an isolated authority from it, and compare representative answers. Never use
the restore test against the production database or publish its answers.

## 1. Release immutable images

Run the complete local suite, commit to `main`, and wait for both GitHub Actions
workflows. Capture the server and web digests—not just `sha-*` tags—and make the
packages anonymously pullable or configure a scoped pull Secret. Verify the
image platform is `linux/amd64`, the source revision label matches, and the
provenance/SBOM attestations are attached.

## 2. Bootstrap the Paprika application

Before every mutation, confirm:

```sh
kubectl config current-context
```

It must be the intended VKE admin context, never the local kind context.

Create namespace `dns` and an out-of-band Secret named by the chart. Its keys
are `api-bearer-token` and `snapshot-bearer-token`. Render the production values
locally, inspect every object, then apply `deploy/paprika/application.yaml`.
Wait for Paprika health, the control pod, all authority pods, PVCs, Services,
PodDisruptionBudgets, and both OpenTelemetry Collector pods. Confirm each
authority reports ready and the snapshot checksum/version agrees across pods.

The production values must set:

- `DNS_ROLE=control` on the one persistent control pod;
- `DNS_ROLE=authority` on read-only authority pods;
- `DNS_PRODUCTION=true` everywhere;
- the two real `DNS_NAMESERVERS` on the control pod;
- `DNS_SNAPSHOT_MAX_STALENESS=24h` (or an explicitly reviewed value); and
- digest-pinned application and collector image references plus an immutable
  Git revision.

Before exposing DNS, merge and deploy the Deephost monitoring integration from
the [observability runbook](observability.md). Require both per-replica
collector scrape jobs to be healthy, then exercise one API mutation plus UDP
and TCP queries and confirm the control, snapshot, and DNS series advance.
ServiceMonitor and PrometheusRule CRDs alone do not configure the current
standalone VKE Prometheus. Do not patch its live ConfigMap; Paprika owns and
self-heals that resource. The current platform has no Alertmanager, so visible
firing rules are not yet a paging path.

## 3. Reconcile the Sydney DNS load balancer

Use `cmd/vultr-dns-lb` in plan mode first. Its desired endpoint is one stable
Vultr Load Balancer with these rules:

- UDP/53 to UDP/30053 on every untainted core VKE worker;
- TCP/53 to TCP/30053 on the same workers; and
- TCP/30053 health checks.

The worker instance IDs come from the live nodes' `spec.providerID`; do not copy
an old Terraform node count. Review the exact plan, run apply, then re-run plan
and require a no-op. Record the stable IPv4 address and verify all attached
backends are healthy. A node-pool scale or replacement event must run the
reconciler again.

## 4. Publish the authenticated snapshot route

Issue a DNS-01 certificate for the snapshot hostname in namespace `dns`, create
the cross-namespace `ReferenceGrant`, and attach the resulting Secret as an
additional `certificateRef` on `Gateway/envoy-gateway-system/paprika`. Do not
replace or reorder existing certificate references. Expose only the exact
`/internal/v1/snapshot` path to the control Service.

Verify from outside the cluster:

```sh
umask 077
SNAPSHOT_AUTH_HEADER="$(mktemp "${TMPDIR:-/tmp}/simpledns-snapshot-auth.XXXXXX")"
trap 'rm -f -- "${SNAPSHOT_AUTH_HEADER}"' EXIT
printf 'Authorization: Bearer %s\n' "${DNS_SNAPSHOT_BEARER_TOKEN}" >"${SNAPSHOT_AUTH_HEADER}"
chmod 600 "${SNAPSHOT_AUTH_HEADER}"
unset DNS_SNAPSHOT_BEARER_TOKEN

curl --fail --silent --show-error \
  --header "@${SNAPSHOT_AUTH_HEADER}" \
  "https://${SNAPSHOT_HOSTNAME}/internal/v1/snapshot" >/dev/null
```

An omitted, malformed, API, or expired token must fail. `/dns.v1.DNSService/*`
must not be reachable through the snapshot hostname.

## 5. Provision the independent authority

`infra/vultr` creates a protected 1 GiB-RAM Melbourne instance, a narrowly scoped
Vultr firewall, unproxied nameserver/snapshot address records, the proxied
operator record, and (only when explicitly enabled) the child delegation.
Initialize it with an approved remote backend, inspect `tofu plan`, and apply
with canary delegation and both optional IPv6 publications disabled.

The instance is deliberately inert until the snapshot credential is injected:

```sh
SECONDARY_AUTHORITY_IP=<terraform-output> \
  infra/vultr/bootstrap-authority.sh
```

The script prompts without echo if `DNS_SNAPSHOT_BEARER_TOKEN` is not already
in the environment and transfers it over SSH stdin. Verify the service, cache
permissions, image digest, and readiness. Reboot the VM and prove that it loads
its cached last-known-good snapshot before proceeding.

Probe the load balancer and VM IPv6 addresses directly over both transports
before setting `primary_authority_ipv6` or `publish_secondary_ipv6`. An
unreachable AAAA record is worse than an intentionally IPv4-only nameserver.

## 6. Create and exercise the canary zone

Create the child zone through one authenticated atomic import/publish operation.
Include at least A, AAAA, CNAME, MX, TXT, CAA, wildcard, NXDOMAIN, and NODATA
cases within the supported contract. Record the SOA serial.

Query each address directly before creating any parent NS records:

```sh
dig @${NS1_IPV4} ${CANARY_ZONE} SOA +norecurse
dig @${NS1_IPV4} ${CANARY_ZONE} SOA +norecurse +tcp
dig @${NS2_IPV4} ${CANARY_ZONE} SOA +norecurse
dig @${NS2_IPV4} ${CANARY_ZONE} SOA +norecurse +tcp
```

Require `status: NOERROR`, `aa`, identical current SOA serials, correct NS data,
and expected answers. Repeat A/AAAA tests over UDP and TCP. Verify an unrelated
name is `REFUSED`, recursion is unavailable, oversized UDP answers truncate and
succeed over TCP, and both endpoints survive a control-plane outage using their
last-known-good cache.

Run the external probe from at least two networks outside Vultr. Load-test a
representative zone corpus before using an important domain.

## 7. Delegate the child canary

Confirm immediately before delegation:

```sh
dig +short DS "${CANARY_ZONE}"
```

The result must be empty. Enable `enable_canary_delegation` in the reviewed
Terraform plan, apply the two NS records, then verify the trace and public
recursive view:

```sh
dig +trace "${CANARY_ZONE}" SOA
dig @1.1.1.1 "${CANARY_ZONE}" SOA
dig @8.8.8.8 "${CANARY_ZONE}" SOA
```

Keep the canary delegated for at least one full parent TTL while monitoring
both transports, rcodes, latency, snapshot age, SOA skew, pod/VM restarts, and
backend health. Component telemetry does not replace the external probe: it
cannot observe registrar delegation, the public load balancer, or resolver
reachability from outside VKE.

## Production-zone cutover gate

For an actual zone:

1. Export an exact, timestamped copy from the old provider and an independent
   `dig` inventory. Reject unsupported types and detect existing DNSSEC/DS.
2. Lower relevant TTLs at least one old TTL before the change.
3. Import the complete zone atomically; never build it record by record while
   it is public.
4. Diff every RRset and query both new authorities directly over UDP and TCP.
5. Freeze or dual-write old-provider changes during the migration window.
6. Change only the parent NS set (and registrar glue when names are in-bailiwick).
7. Monitor from multiple resolvers through the full parent/old-data TTL window.
8. Roll back the parent NS set immediately on stale serial, SERVFAIL, authority
   loss, or material record mismatch; keep both new snapshots and the old
   provider intact for investigation.

Do not accept a production zone until automated encrypted off-region backups
and a tested restore are in place. Restore must publish an RFC-valid newer SOA
serial so caches converge on the recovered version.
