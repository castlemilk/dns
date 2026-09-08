# Architecture and production boundaries

## Scope

Deep Hosting has three surfaces in one repository:

- The **data plane** answers authoritative DNS on UDP and TCP.
- The **control plane** stores zones and exposes CRUD plus atomic BIND import/export over Connect, and mounts the platform engine facades — hosting (DeepHost), billing (Stripe), mail, and activity — on the same handler.
- The **web app** serves the public landing page at `/` and the operator console beneath it, both on the single console host.

The server is authoritative-only. It never walks the DNS hierarchy, forwards a question, or fills a recursive cache. Queries below a hosted zone are answered with `AA=1` where appropriate; recursion is never advertised (`RA=0`). Names outside every hosted zone receive `REFUSED`. This boundary keeps v0 small and prevents it from accidentally becoming an open recursive resolver.

```text
                                  control plane
operator browser ── / ───────────────> Next.js web
       └────────── Connect + API bearer ──> control ──transaction──> bbolt/PVC
                                                │
                                                └── authenticated, checksummed snapshot
                                                                      │
                                  data plane                          ▼
recursive resolvers ──node IPs, UDP/TCP 53──> read-only authority replicas
                                                   │
                                                   └── atomic pointer + emptyDir cache
```

The production chart runs three distinct workloads: one `DNS_ROLE=control` Deployment, a three-pod `DNS_ROLE=authority` StatefulSet, and the web Deployment. Control owns the only bbolt writer and does not open DNS listeners. Authorities do not open bbolt or expose the Connect API; they poll the authenticated snapshot feed and serve immutable compiled snapshots. `DNS_ROLE=all` keeps the original combined process for local development, where a mutation is published directly to its co-located DNS handler.

## Write and replication path

Zone and record mutations follow one serialized control path:

1. A generated Connect handler validates the protobuf request.
2. The zone package normalizes owner names and values, enforces RRset constraints, and compiles records through `miekg/dns` as a final syntax check.
3. A bbolt write transaction applies the tentative complete-zone update. Zone-name uniqueness is maintained by a separate index bucket; the SOA serial advances and managed SOA/NS records remain protected.
4. Before commit, the transaction reads the tentative aggregate dataset, compiles it, and encodes a worst-case-timestamp snapshot. If it exceeds `DNS_SNAPSHOT_MAX_BODY_BYTES` (8 MiB by default), the whole transaction rolls back. The same admission check runs against an existing database during `control` or `all` startup and during import dry runs.
5. After commit, all zones are read back and compiled even if the client canceled. In local `all` mode, that compiled snapshot is also published directly.

In split production mode, an authority observes the committed change on its next poll:

1. Control reads all zones and returns a versioned JSON document from exact path `/internal/v1/snapshot`. The document includes a generation time and SHA-256 checksum and is protected by its own snapshot bearer token. Control reuses that exact document and ETag until zone content changes.
2. Authorities poll with `If-None-Match`. A `304 Not Modified` refreshes readiness freshness without recompiling or rewriting the cache.
3. For changed content, an authority rejects oversized, malformed, checksum-invalid, unsupported, future-dated, older, or production-reserved data, then compiles the complete zone set before publication.
4. A valid changed response is written to the pod's cache path with a secure atomic replacement before the authoritative server swaps its in-memory pointer. Failed polls never replace the last valid version; readiness fails once its configured freshness window expires.

Snapshots are checksummed, not cryptographically signed, and publication is pull-based and eventually consistent. There is no mutation journal, quorum, per-authority acknowledgement, or consensus protocol. Each mutation performs full-dataset admission, each feed request reads and hashes the dataset, and each changed snapshot is fully compiled by every authority. Unchanged polls avoid compilation and cache fsync, but control and replication costs still grow with the hosted dataset.

bbolt serializes writes and owns an exclusive database file lock. The Helm chart therefore fixes control at one replica, uses `ReadWriteOnce` storage, and selects a `Recreate` rollout strategy. Authority replicas never share or open that database.

## Read path

Each authoritative process holds an `atomic.Pointer` to an immutable compiled snapshot. Each snapshot contains:

- a map of canonical zone names;
- per-zone owner and RRset maps;
- precomputed empty-nonterminal existence;
- delegation cuts;
- the zone SOA for negative answers; and
- aggregate zone and record counts.

A query loads the pointer once and reads only that version. There is no store or cache-file access and no shared mutex on the query path. Replacing a snapshot does not mutate the previous version; in-flight queries can finish against it while Go reclaims it after it is no longer referenced.

The handler currently supports:

- exact RRset answers;
- CNAME replies with an additional local target RRset when available;
- wildcard synthesis;
- delegation referrals with available in-zone A/AAAA glue;
- NODATA and NXDOMAIN responses carrying the zone SOA;
- a small RFC 8482-style HINFO response for `ANY`;
- EDNS UDP-size negotiation capped by `DNS_MAX_UDP_SIZE`; and
- UDP truncation so a client can retry over TCP.

Malformed multi-question requests receive `FORMERR`, unsupported opcodes receive `NOTIMP`, and unsupported classes or out-of-scope names receive `REFUSED`.

## Influence of Cloudflare's cache work

Cloudflare's [DNS cache memory optimization article](https://blog.cloudflare.com/dns-cache-memory-optimization-1111/) describes optimizations to a large **recursive resolver cache**. Deep Hosting is not implementing that cache or claiming equivalent performance. The relevant design lesson is narrower: make read-side data purpose-built and immutable, remove general-purpose object overhead from the hot path where measurements justify it, and replace complete versions atomically.

v0 applies the immutability and read/write separation ideas, but still stores ordinary Go maps and `dns.RR` values. Packed owner names, exact-sized RRset storage, pre-encoded wire answers, and more compact indexing are possible later optimizations. They should be introduced only with allocation profiles, representative zone corpora, and latency/throughput benchmarks; copying a recursive-cache layout into an authoritative server without evidence would add complexity without proving value.

## Current benchmark evidence

The checked-in benchmarks measure complete immutable-snapshot replacement and an exact A lookup that includes DNS message packing. On 2026-09-02 with Go 1.27, darwin/arm64, an Apple M5 Max, and five benchmark runs:

| Benchmark corpus | Median time | Go allocation result | Observed steady/replacement heap |
| --- | ---: | ---: | ---: |
| Compile/replace representative 10,000-record zone | 11.30 ms | 12.46 MB, 286,412 allocs/op | 4.06 MiB / 11.06 MiB |
| Compile/replace 2,000 × 3,360-byte TXT stress zone | 36.77 ms | 24.60 MB, 122,135 allocs/op | 8.47 MiB / 17.41 MiB median |
| Exact A query from the 10,000-record snapshot | 385.8 ns | 536 B, 10 allocs/op | not measured |

Run the same corpus with:

```sh
go test ./internal/authoritative -run '^$' -bench 'Benchmark(CompileReplacement|ServeDNSExactA)$' -benchmem -count=5
```

The heap metric is runtime-observed and showed run-to-run noise, especially while holding both large-TXT snapshots during replacement. These numbers provide replacement-cost context for the current 8 MiB admission bound and show why unchanged ETag polls avoid needless work. They do not measure sockets, Kubernetes, the public load balancer, mixed traffic, tail latency, or sustained QPS, and therefore are not a capacity or SLO claim.

## API, authentication, and frontend

[`proto/dns/v1/dns.proto`](../proto/dns/v1/dns.proto) defines the DNS API; `proto/platform/v1`, `proto/hosting/v1`, `proto/billing/v1`, `proto/mail/v1`, and `proto/activity/v1` define the platform services mounted alongside it. Buf generates:

- protobuf Go messages and Connect-Go handler/client interfaces under `gen/go`; and
- protobuf-es message and service descriptors under `apps/web/gen` for Connect-ES.

The DNS service exposes `ListZones`, `CreateZone`, `DeleteZone`, `CreateRecord`, `UpdateRecord`, `DeleteRecord`, `ImportZone`, and `ExportZone`; the platform, hosting, billing, mail, and activity services are mounted on the same control handler behind the same API bearer token. Connect supports its JSON-over-HTTP protocol as well as gRPC and gRPC-Web on the same handlers. The frontend and `dnsctl` call the generated service instead of maintaining hand-written request types.

`ImportZone` parses a bounded BIND file for the unsigned record types supported by the server. Source apex SOA and NS records are skipped with warnings because the store creates managed apex records from `DNS_NAMESERVERS`; any unsupported or invalid record rejects the complete create/replace transaction. Dry-run uses the same mode preconditions, validation, and aggregate snapshot admission, then deliberately rolls back. `ExportZone` emits deterministic canonical BIND text including the managed records. The operator interfaces are:

```sh
dnsctl import --zone NAME --file PATH|- [--mode create|replace] [--dry-run] [--api-url URL] [--timeout DURATION]
dnsctl export --zone-id ID [--output PATH|-] [--api-url URL] [--timeout DURATION]
```

`dnsctl` reads only `DNS_API_BEARER_TOKEN`; `DNS_API_URL` optionally selects the Connect base URL. It permits plaintext HTTP only for localhost or a loopback IP, requiring HTTPS for remote control endpoints.

Production control requires an API bearer token for every Connect procedure and a different snapshot bearer token for the snapshot feed. Every configured production token must contain at least 32 visible ASCII characters. The chart reads them from independent `api-bearer-token` and `snapshot-bearer-token` keys in an existing Secret. Control receives both values; authority pods receive only the snapshot token. The snapshot hostname and ordinary UI hostname are separate routes so remote authorities cannot reach Connect through the narrow snapshot route.

The operator enters the API token into the browser's locked screen. The frontend retains it only in that tab's `sessionStorage`, never in `localStorage`, a cookie, or a build-time environment variable. A Connect-ES interceptor adds `Authorization: Bearer <token>` to each RPC. Explicit Lock and any unauthenticated RPC clear the token and return the UI to the locked state. This is shared-secret authentication only: v0 still has no identities, roles, tenancy, or authorization policy.

Health endpoints are deliberately separate from the protobuf API and remain unauthenticated:

- `GET /healthz` reports that the process is alive.
- Control `GET /readyz` reports that its HTTP process is accepting work.
- Authority `GET /readyz` requires a valid snapshot no older than `DNS_SNAPSHOT_MAX_STALENESS`.

The authority freshness probe does not verify public DNS reachability or erase a stale-but-valid serving snapshot. None of the probes verifies PVC durability, delegation correctness, or a second authoritative site.

`DNS_CORS_ORIGINS` only controls browser origins. CORS and Gateway routing are not authentication; the Go API still enforces its bearer token after traffic reaches control.

## Kubernetes and Paprika

The production Helm chart deploys:

- one `Recreate` control Deployment with a single-writer bbolt `ReadWriteOnce` PVC;
- three read-only authority StatefulSet pods with an ephemeral `emptyDir` snapshot cache per pod;
- one independently deployable Next.js web Deployment;
- an authority-only mixed-protocol `externalIPs` Service that publishes UDP/TCP 53 on the delegated node addresses;
- experimental shared-Vultr public DNS LoadBalancer Services; and
- separate optional Gateway API routes for the operator surface and remote snapshot consumers.

The control claim carries `helm.sh/resource-policy: keep` and `paprika.io/prune: "false"`; the production overlay selects a retaining Vultr storage class. Authority snapshot caches are ephemeral `emptyDir` volumes in production: the Vultr account is at its block-storage subscription cap, and `authority.cache.acknowledgeEphemeralCache` is the explicit opt-in. Retention limits accidental deletion of the control volume, but a retained volume is not an off-provider backup.

The authority StatefulSet uses required hostname anti-affinity and a `DoNotSchedule` topology-spread constraint. A PodDisruptionBudget sets `minAvailable: 2`. Those controls require enough schedulable workers and improve pod/node availability, but the replicas still share the same VKE cluster and Sydney region.

The operator `HTTPRoute` sends the DNS and platform Connect prefixes and the exact Stripe webhook path `/billing/v1/stripe/webhook` to control, and `/` to the web Service. It deliberately does not expose the snapshot endpoint. A second, optional route exposes only exact path `/internal/v1/snapshot` to control for remote authorities. The chart can render a Certificate and cross-namespace ReferenceGrant, but the platform owner must still add the resulting TLS Secret as a `certificateRef` on the shared Gateway. DNS itself does not traverse either HTTP route.

No NetworkPolicy is rendered yet. Live Paprika probes, Gateway traffic, and the provider load balancer originate from different sources that the chart cannot safely infer; deny-by-default policy should be introduced only after those selectors and CIDRs are known and canaried.

### Vultr UDP/TCP load balancer

The public path in production is the CCM-unmanaged `externalIPs` Service. It selects only authority pods and publishes UDP/53 and TCP/53 straight onto the delegated node addresses, because a Vultr load balancer refuses a second forwarding rule on a frontend port already in use and so cannot carry both protocols on one address. The NodePort Service and the reconciler below are the alternative path and are disabled today (`nodePortService.enabled: false`); enabling them fixes both protocols at node port `30053`, with `externalTrafficPolicy: Cluster` so an attached worker can forward to a ready authority. The declarative reconciler in [`cmd/vultr-dns-lb`](../cmd/vultr-dns-lb/README.md) discovers one exact-label Sydney load balancer, fails closed on duplicates, and manages:

- UDP/53 → UDP/30053;
- TCP/53 → TCP/30053;
- a TCP/30053 health check; and
- public IPv4 and IPv6 port-53 firewall rules.

Its default `--nodes=1` is the lightweight billable baseline; odd higher values are an explicit availability and cost choice. Plan is read-only by default and prints a deterministic redacted diff. Pass each current core VKE instance ID, review that plan, and use the same arguments for apply:

```sh
go run ./cmd/vultr-dns-lb --plan --instance-id='<core-vke-id-1>' --instance-id='<core-vke-id-2>' --instance-id='<core-vke-id-3>' --instance-id='<core-vke-id-4>'
go run ./cmd/vultr-dns-lb --apply --instance-id='<core-vke-id-1>' --instance-id='<core-vke-id-2>' --instance-id='<core-vke-id-3>' --instance-id='<core-vke-id-4>'
```

The API key is read only from `VULTR_API_KEY` and is never included in output. Re-run the reconciler when the core worker set changes, and separately ensure worker firewalls admit the load balancer's TCP and UDP traffic to `30053`. After each change, verify ordinary `dig` and `dig +tcp` from outside Vultr. See the [load-balancer runbook](../cmd/vultr-dns-lb/README.md) for the complete CLI contract.

The chart's experimental Vultr CCM path renders separate UDP and TCP Services that target the same authority pods and load-balancer label. [Vultr CCM issue #346](https://github.com/vultr/vultr-cloud-controller-manager/issues/346) documents the earlier forwarding-rule conflict; even with merged rules, a shared load-balancer-level health check remains hazardous. This path is disabled by default, requires an explicit risk acknowledgement, must never be enabled with the NodePort path, and needs an external canary for both protocols.

[`deploy/paprika/application.yaml`](../deploy/paprika/application.yaml) creates the restricted `dns` namespace and points Paprika at `charts/dns`. It fails closed until the source revision is a full 40-character Git SHA and both server and web images use reviewed `sha256` digests. Paprika polls that immutable revision, prunes ordinary removed resources, self-heals drift, checks both control and authority readiness, and rolls back on health failure. The production values set authority snapshot freshness to `24h`, allowing a shorter control outage without withdrawing the last checksum-valid serving snapshot.

### Runtime security

The chart defaults to:

- non-root UIDs and the namespace's restricted Pod Security profile;
- `RuntimeDefault` seccomp;
- no privilege escalation and all Linux capabilities dropped;
- read-only root filesystems with explicit writable data, cache, and `/tmp` mounts;
- disabled service-account token mounting and Kubernetes service links;
- CPU/memory requests and limits;
- startup, liveness, and readiness probes; and
- graceful termination periods plus the authority scheduling constraints described above.

The authority server listens on unprivileged container port 1053; Kubernetes Services map public port 53 to it, so the process does not need `CAP_NET_BIND_SERVICE`.

These are baseline containment settings, not a complete security program. Public authoritative service also needs load-balancer/DDoS controls, dependency and image scanning, off-provider control-data backups, monitoring, operational access controls, and an incident/runbook process. It also needs an independently operated authority; follow the [production launch and cutover runbook](production-launch.md) for the Sydney/Melbourne topology and delegation gates.

### Telemetry plane

Control, authority, and web export server-side OTLP to two in-cluster
OpenTelemetry Collectors. The collectors expose application metrics on 8889
and their own health metrics on 8888 through a headless Service. Prometheus
therefore discovers every collector replica instead of scraping a randomly
selected ClusterIP backend. Client-IP affinity normally keeps each cumulative
SDK stream on one collector, and short exporter series expiry bounds duplicate
last values after failover.

The collector converts sampled spans into bounded request metrics and has no
raw trace or debug exporter. Application instrumentation excludes DNS names,
record content, IDs, URLs, headers, addresses, and errors; the Next.js browser
bundle contains no telemetry SDK. A NetworkPolicy admits OTLP only from the
three application components and scrape traffic only from the configured
monitoring workload. Collector loss is intentionally fail-open for serving and
mutations. See the [observability runbook](observability.md) for the metric
catalog, VKE Prometheus integration, and verification gates.

## Authoritative topology requirements

A registrable production zone should delegate to at least two stable nameserver names backed by independently operated endpoints. They should not share a single node, load balancer, cluster, region, storage volume, or control-plane failure mode.

For each production rollout:

1. Set `DNS_NAMESERVERS` to the real NS hostnames before creating zones; new zones receive managed SOA and NS records from this value.
2. Give every NS hostname a stable address. If it is inside a delegated zone, publish the required glue at the registrar/parent zone.
3. Run authoritative nodes in at least two independent failure domains and verify UDP/53 and TCP/53 from outside each one.
4. Delegate a test zone first and inspect it through multiple public recursive resolvers before moving important zones.
5. Monitor authoritative answers, SOA serials, delegation consistency, SERVFAIL rates, latency, snapshot freshness, and certificate/authentication state for the control surfaces.

The three charted authority pods are not three independent sites: they share one VKE cluster, region, and set of node addresses. The production launch design adds a separately operated Melbourne authority that consumes the narrow authenticated snapshot feed. See [`docs/production-launch.md`](production-launch.md) for the rollout and cutover gates.

## Failure behavior and operations

- A bbolt transaction is atomic, but there is still one control writer and no replicated mutation journal or consensus.
- After commit, control validates the complete stored dataset even if the request context was canceled. A post-commit validation failure is reported as an internal error and requires operator investigation.
- Snapshot polling is eventually consistent and has no acknowledgement path. A checksum, compile, cache-write, authentication, network, or freshness failure leaves each authority on its previous valid snapshot.
- A stale authority continues answering from that immutable snapshot, but Kubernetes readiness fails and removes it from ready backends after the configured window.
- `Recreate` avoids concurrent bbolt ownership. A short control rollout does not directly interrupt authority DNS; it only prevents refresh until control returns.
- The retained control PVC protects against an ordinary Pod replacement. Authority snapshot caches are ephemeral, so a restarted authority holds no local recovery copy and serves nothing until it reaches control. The control volume remains provider- and region-correlated, so it does not replace a separately managed verified snapshot archive or bbolt-consistent copy outside the cluster's failure domain.
- In-process query counters reset on restart. OpenTelemetry counters are also process-lifetime cumulative values, but Prometheus preserves their samples and handles resets across pod restarts.

### Snapshot verification and restore

`dns-restore` is offline and never contacts control. Verification performs the bounded strict-JSON/version/checksum checks, validates persistent zone IDs, timestamps, canonical values and managed records, and compiles the complete authoritative snapshot:

```sh
dns-restore --snapshot PATH|- --verify-only
```

Exact restore preserves the archived SOA serials and writes all zones in one bbolt transaction. The target must be new or a demonstrably empty bbolt database; symlinks, corrupt files, unknown/nonempty buckets, and existing zone data are refused:

```sh
dns-restore --snapshot PATH|- --database PATH
dns-restore --snapshot PATH|- --database PATH --bump-serials
```

For disaster recovery where resolvers must observe a newer SOA, add `--bump-serials`. Each restored serial uses the normal clock-aware rule: current Unix time when numerically ahead, otherwise one modulo-`uint32` step, with the managed SOA rewritten consistently. This only establishes a serial newer than the archived snapshot. A stale backup can still trail post-backup live mutations, so use the latest verified archive and compare with the last observed live serial before publishing recovered authorities.

## Limitations and roadmap

### Explicit v0 limitations

- Shared bearer secrets only; no identities, roles, tenant isolation, audit log, or API rate limiting
- Unsigned zones only: no DNSSEC signing, key management, or DS automation
- No AXFR, IXFR, NOTIFY, TSIG, secondary-server mode, or hidden-primary workflow
- No recursive resolution or recursive cache by design
- One bbolt control writer; snapshot pull is not a replicated journal, consensus system, or multi-writer control plane
- Full-dataset admission and changed-snapshot generation/compilation rather than incremental publication
- Authority snapshot caches are ephemeral `emptyDir` volumes; there is no durable off-provider archival backup
- ASCII DNS names only; callers must provide IDNs as punycode
- A focused record set rather than the full DNS RR type registry
- Prometheus metrics and span-derived request metrics, but no retained trace backend, Alertmanager notification delivery, formal SLO/error-budget policy, or end-to-end public DNS health proof
- No DNS response-rate limiting (RRL) or built-in DDoS mitigation

### Roadmap

1. **Strengthen control operations:** add identities, roles/tenancy, audit events, request limits, RRL, secret rotation, and automated off-provider snapshot archival.
2. **Strengthen snapshot distribution:** add version/acknowledgement observability, safe rollback, and independently operated authority fleet management.
3. **Meet DNS availability requirements:** complete the independent Sydney/Melbourne topology, external UDP/TCP probes, delegation monitoring, alert delivery, and formal SLOs.
4. **Add protocol operations:** AXFR/IXFR/NOTIFY with TSIG and a supported primary/secondary model.
5. **Add DNSSEC:** offline/online key roles, automated rotation, signing, DS publication workflow, and validation monitoring.
6. **Optimize from evidence:** extend the current microbenchmarks with representative multi-zone corpora and profiles, then consider incremental compilation, compact indexes, and pre-encoded wire RRsets inspired by the immutable-cache principles above.

Any public launch should complete the independent-authority and external-monitoring gates in the [production launch runbook](production-launch.md). Mature secondary DNS providers remain the safest fallback for any missing failure domain.
