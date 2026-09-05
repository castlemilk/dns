# Simple DNS

Simple DNS is a small authoritative DNS hosting service: a Go server for DNS over UDP and TCP, a protobuf-first Connect API, and an authenticated Next.js operator console. One control writer persists zones in bbolt; read-only authority processes consume checksum-validated snapshots and publish them atomically to the DNS query path.

> [!IMPORTANT]
> This is an **authoritative-only** v0. It is not a recursive resolver, does not contact upstream resolvers, advertises `RA=0`, and returns `REFUSED` for names outside the zones it hosts. Do not configure it as a workstation or cluster recursive DNS server.

## What is included

- Authoritative UDP and TCP DNS with A, AAAA, CNAME, MX, TXT, NS, SRV, CAA, and managed SOA records
- Wildcard answers, local CNAME expansion, delegation referrals and glue, negative SOA responses, EDNS size negotiation, and deliberately minimal ANY replies
- A bearer-authenticated Connect-Go control API generated from Buf-managed protobuf definitions
- Atomic BIND zone-file import/export with managed provider SOA/NS records and an authenticated operator CLI
- Connect-ES browser bindings and a Next.js 16, Tailwind CSS, and shadcn/ui operator console that keeps its token in per-tab session storage
- Transactional, single-writer bbolt persistence with an authenticated snapshot feed and atomically replaced read snapshots
- Offline checksum verification and transactional restore tooling for snapshot archives
- Optional engine facades for website hosting, mailboxes and billing, each independently configurable and each honest about being unconfigured
- Distroless/non-root container images, a Helm chart, and a Paprika application manifest for VKE
- Privacy-bounded OpenTelemetry metrics and span-derived metrics for Go, Next.js, and the snapshot/DNS data paths, exported through an HA in-cluster collector for Prometheus
- CI for protobuf drift, Go race tests/vet/build, frontend lint/build, and Helm lint/render

The production chart separates one persistent control writer from three read-only authority pods. `DNS_ROLE=all` remains a convenient single-process local mode. See [Architecture and production boundaries](docs/architecture.md) and the [production launch runbook](docs/production-launch.md) before putting it on the public DNS.

## Run locally

Prerequisites:

- Go 1.27
- Node.js 24 and pnpm 11.23
- Buf CLI (for protobuf work)
- `dig` for DNS checks; `jq` is convenient for the API examples

Install the web dependencies once:

```sh
pnpm --dir apps/web install --frozen-lockfile
```

Start the Go service in the first terminal. Local `DNS_ROLE=all` serves the control API on `:8080` and authoritative DNS on UDP and TCP `:1053`. Give the API a local operator token so the UI exercises the same bearer flow as production:

```sh
export DNS_API_BEARER_TOKEN=local-operator-token

DNS_DATA_PATH=./data/dev.db \
DNS_NAMESERVERS=ns1.example.net,ns2.example.net \
go run ./cmd/dns
```

Start the web app in another terminal:

```sh
NEXT_PUBLIC_DNS_API_URL=http://localhost:8080 pnpm --dir apps/web dev
```

Open [http://localhost:3000](http://localhost:3000) for the public landing page — it needs no token, and it is served with `X-Robots-Tag: noindex, nofollow` like every other path, so it is deliberately not indexable. The console lives behind the operator-token gate:

| Path | What it is |
| --- | --- |
| `/` | Landing page (public, static; the product screenshot in it renders a hard-coded sample zone) |
| `/domains` | Dashboard: every zone, with website / email / DNS status derived from the records |
| `/domains/<zone>` | Domain view for one zone by name (e.g. `/domains/example.test`): website, email and DNS cards plus the raw zone table |
| `/connect` | Guided setup: create the zone, point the nameservers, then the website and email records |
| `/deploys`, `/mailboxes`, `/billing` | One page per engine. Each renders its own not-configured state, naming the variables, when its engine is off |
| `/activity` | The append-only event log, filterable by domain and kind |
| `/settings` | Operator session, nameservers, and one row per engine with its configured/reachable state |

Open [http://localhost:3000/domains](http://localhost:3000/domains), enter `local-operator-token`, and unlock the console. The token stays only in that tab's `sessionStorage`, is attached by the Connect-ES interceptor, and is cleared by Lock or an unauthenticated response. `GET /healthz` and `GET /readyz` remain unauthenticated; an authority's readiness additionally enforces snapshot freshness. The variables and defaults are listed in [`.env.example`](.env.example); the Go binary does not load that file automatically, so export or source it when using it locally.

The connect flow polls `GET /api/nameservers?domain=…`, a route handler in the web app that resolves NS records with `node:dns`. It therefore needs outbound DNS from the **web** process (not the browser); without it the check reports `timeout`/`error` and the flow still lets you skip the step.

To seed a zone without the UI, either set `DNS_BOOTSTRAP_ZONE=example.test` before starting the server or use the API below. A bootstrap occurs only when the database contains no zones.

## Engines and platform state

The control plane is the only thing the browser talks to. Behind it sit three
optional engines, each reached through a facade that holds the engine's
credential server-side and never returns it:

| Engine | Backed by | Turns on with | Adds |
| --- | --- | --- | --- |
| Hosting | a DeepHost control plane | `HOSTING_API_URL` (+ `HOSTING_API_TOKEN` off loopback) and a gateway target | `hosting.v1.HostingService`: attach a site, deploy from a repository or an uploaded folder, read the build log, roll back |
| Mail | a Stalwart server | `MAIL_API_URL`, `MAIL_API_TOKEN`, `MAIL_HOSTNAME` | `mail.v1.MailService`: bind a domain, publish MX/SPF/DKIM/DMARC, create mailboxes and forwarders |
| Billing | Stripe | `BILLING_PROVIDER=stripe` and the `STRIPE_*` keys | `billing.v1.BillingService`: Checkout, the billing portal, invoices, and a signature-verified webhook |

Every engine is optional and independent. With one unconfigured its service is
still mounted, `Get*Status` reports `configured=false` with the names of the
variables an operator has to set, and every mutating call returns
`FailedPrecondition`. Nothing is faked: a screen with no engine behind it says
so and shows the variable names instead of a number.

Alongside them the writer roles keep a second bbolt file, `DNS_PLATFORM_PATH`
(default `platform.db` beside `DNS_DATA_PATH`), holding site and mail bindings,
subscriptions, the webhook queue and the activity log. The zone store, the
snapshot feed and `dns-restore` are untouched by it. Backing it up and
rebuilding it after a loss are described in the
[platform runbook](docs/platform-runbook.md).

Two more variables belong to the deployment rather than an engine:
`ACTIVITY_MAX_EVENTS` bounds the activity ring, and the **web** process reads
`DNS_API_URL` server-side so the landing page can fetch the unauthenticated
`GET /public/v1/plan`. With `DNS_API_URL` unset the landing page falls back to
its DNS-only copy.

### Running with engines locally

[`.env.example`](.env.example) documents every variable with local values: the
kind DeepHost control plane on `127.0.0.1:30081`, a Docker Stalwart on
`127.0.0.1:20080`, and `BILLING_PROVIDER=fake`, which runs an in-process Stripe
API on a loopback port (rejected outright when `DNS_PRODUCTION=true`). Copy it
to `.env.local` — gitignored, and the file the browser acceptance run expects —
edit it, and source it, because the Go binary does not read it by itself:

```sh
cp .env.example .env.local
set -a; . ./.env.local; set +a
go run ./cmd/dns
```

Two rules that catch most local misconfigurations early: a variable from an
engine block without that engine's URL is a startup error rather than a
silently ignored setting, and a real engine credential makes
`DNS_API_BEARER_TOKEN` mandatory, because the new RPCs spend that credential,
return a mailbox password once, and can delete apps and mailboxes.

## Connect JSON API

The protobuf schema is the source of truth. Connect exposes protobuf-compatible JSON over ordinary HTTP in addition to its binary protocols; these are Connect procedure paths, not hand-written resource-style REST routes.

In the client shell, write the bearer header to a private temporary file so the
token is not expanded into `curl`'s process arguments:

```sh
umask 077
DNS_API_AUTH_HEADER="$(mktemp "${TMPDIR:-/tmp}/simpledns-api-auth.XXXXXX")"
trap 'rm -f -- "${DNS_API_AUTH_HEADER}"' EXIT
printf 'Authorization: Bearer %s\n' "${DNS_API_BEARER_TOKEN}" >"${DNS_API_AUTH_HEADER}"
chmod 600 "${DNS_API_AUTH_HEADER}"
unset DNS_API_BEARER_TOKEN
```

Create a zone and retain its generated ID:

```sh
ZONE_ID="$(
  curl --fail-with-body --silent --show-error \
    --header "@${DNS_API_AUTH_HEADER}" \
    -H 'Content-Type: application/json' \
    -H 'Connect-Protocol-Version: 1' \
    --data '{"name":"example.test"}' \
    http://localhost:8080/dns.v1.DNSService/CreateZone \
  | jq -r '.zone.id'
)"
```

Add an A record:

```sh
curl --fail-with-body \
  --header "@${DNS_API_AUTH_HEADER}" \
  -H 'Content-Type: application/json' \
  -H 'Connect-Protocol-Version: 1' \
  --data "{\"zoneId\":\"${ZONE_ID}\",\"name\":\"www\",\"type\":\"RECORD_TYPE_A\",\"ttl\":300,\"value\":\"192.0.2.10\"}" \
  http://localhost:8080/dns.v1.DNSService/CreateRecord
```

List zones and server counters:

```sh
curl --fail-with-body \
  --header "@${DNS_API_AUTH_HEADER}" \
  -H 'Content-Type: application/json' \
  -H 'Connect-Protocol-Version: 1' \
  --data '{}' \
  http://localhost:8080/dns.v1.DNSService/ListZones
```

The remaining procedures are `DeleteZone`, `UpdateRecord`, `DeleteRecord`, `ImportZone`, and `ExportZone`; their request shapes are defined in [`proto/dns/v1/dns.proto`](proto/dns/v1/dns.proto).

## Zone-file migration and recovery

`dnsctl` uses the generated Connect client and reads its credential only from `DNS_API_BEARER_TOKEN`. `DNS_API_URL` defaults to `http://127.0.0.1:8080`; non-loopback endpoints must use HTTPS.

Validate a BIND file against create-mode preconditions without changing bbolt, then perform the import:

```sh
go run ./cmd/dnsctl import --zone example.test --file ./example.test.zone --mode create --dry-run
go run ./cmd/dnsctl import --zone example.test --file ./example.test.zone --mode create
```

Use `--mode replace` for an existing zone. Import accepts the unsigned record types supported by the server, skips source apex SOA/NS records with warnings, installs the configured provider SOA/NS records, and rejects the whole operation if any record or aggregate invariant fails. Export is canonical BIND text:

```sh
go run ./cmd/dnsctl export --zone-id "${ZONE_ID}" --output ./example.test.zone
```

Every tentative control mutation, including an import dry run, must fit the complete encoded snapshot before its bbolt transaction can commit. `DNS_SNAPSHOT_MAX_BODY_BYTES` defaults to 8 MiB, and the same check runs against an existing database at startup, preventing control from accepting state that its authorities cannot fetch.

Verify an archived snapshot without opening a database, or restore it into a new or demonstrably empty bbolt file:

```sh
go run ./cmd/dns-restore --snapshot ./snapshot.json --verify-only
go run ./cmd/dns-restore --snapshot ./snapshot.json --database ./restored.db
go run ./cmd/dns-restore --snapshot ./snapshot.json --database ./restored.db --bump-serials
```

The platform store is not part of a snapshot: the running control process holds
bbolt's exclusive lock on it and the image has no shell, so the copy is served
by the process that owns the lock. `dnsctl platform-backup` fetches that route
with `DNS_SNAPSHOT_BEARER_TOKEN`, checks the bbolt magic and the declared
length, and deletes the file rather than keep one that failed either check. It
never opens the database itself.

```sh
DNS_CONTROL_URL=http://127.0.0.1:8080 go run ./cmd/dnsctl platform-backup --out ./platform.copy.db
```

If the store is lost, the facades can reconstruct the parts the engines still
know about — sites, mail domains and subscriptions, but not the activity log or
deploy history. It runs inside the control plane, so no engine credential
leaves the pod, and it refuses to run unless those buckets are all empty:

```sh
go run ./cmd/dnsctl platform-rebuild --dry-run
go run ./cmd/dnsctl platform-rebuild
```

Both commands, and the order to restore in, are covered in the
[platform runbook](docs/platform-runbook.md).

Verification checks the bounded JSON document, version, SHA-256 checksum, persistent zone invariants, and complete authoritative compilation. Restore refuses symlinks, corrupt targets, and nonempty databases, and writes every zone in one transaction. Exact restore preserves SOA serials. For a recovery that must trigger resolver convergence, add `--bump-serials`; it selects each recovery serial with the live clock-aware serial rule. Always use the latest verified backup and compare against the last observed live serial when available—a serial newer than the backup is not necessarily newer than mutations made after that backup.

## Exercise UDP and TCP

After creating the example record, both transports should return an authoritative answer (`aa`) with recursion unavailable:

```sh
dig @127.0.0.1 -p 1053 www.example.test A +norecurse
dig @127.0.0.1 -p 1053 www.example.test A +norecurse +tcp
```

An unrelated name should be refused instead of being resolved recursively:

```sh
dig @127.0.0.1 -p 1053 example.org A +norecurse
```

## Protobuf workflow

Edit [`proto/dns/v1/dns.proto`](proto/dns/v1/dns.proto), then regenerate both Go and TypeScript bindings:

```sh
buf lint
buf generate
git diff -- gen/go apps/web/gen
```

Remote plugins are version-pinned in [`buf.gen.yaml`](buf.gen.yaml). Generated Go files live under `gen/go`; generated protobuf-es service and message descriptors live under `apps/web/gen` and are consumed by Connect-ES.

## Verify a change

```sh
go test -race ./...
go vet ./...
go build ./...
pnpm --dir apps/web lint
pnpm --dir apps/web test:observability
pnpm --dir apps/web build
helm lint --strict charts/dns
helm lint --strict charts/dns -f charts/dns/ci/production-values.yaml
helm lint --strict charts/dns -f charts/dns/ci/platform-values.yaml
helm template dns charts/dns --namespace dns
scripts/tests/backup-snapshot-test.sh
scripts/tests/observability-chart-test.sh
```

The engine integration tests carry no build tag: they compile and vet with
everything else and skip themselves unless their engine URL is set. Point them
at a local engine with the Makefile targets, which is the only way they
actually run:

```sh
make test-integration-deephost   # SIMPLE_TEST_DEEPHOST_URL, default the local kind cluster
make test-integration-stalwart   # SIMPLE_TEST_STALWART_URL and _TOKEN from scripts/dev/stalwart-up.sh
```

The same checks run in GitHub Actions. A successful CI run on `main` publishes immutable images named `ghcr.io/castlemilk/dns:sha-<commit>` and `ghcr.io/castlemilk/dns-web:sha-<commit>`.

### Runtime license inventories

Both published images expose their compliance material at `/licenses` and carry
the OCI `org.opencontainers.image.licenses=Apache-2.0` label. Each inventory is
generated from the runtime artifact during its image build and fails the build
when a dependency has no license evidence.

- The server image contains the project's exact `LICENSE`, a deterministic
  `THIRD_PARTY.json`, and each linked Go module's exact root legal files and
  `go.mod` under `third_party/go`.
- The web image contains the project's exact `LICENSE`, every package manifest
  actually present in Next's standalone output, and deduplicated exact
  license/notice files with SHA-256 digests. When a publisher omits a separate
  legal file, the inventory retains that package's exact metadata and SPDX
  declaration as its evidence.

The UI does not use the Next image optimizer, so `images.unoptimized` and the
standalone tracing exclusions keep `sharp`, `@img/*`, and native libvips files
out of the production image. The web collector independently rejects the build
if any of those paths reappear.

## Benchmark evidence

The authoritative microbenchmarks can be reproduced with:

```sh
go test ./internal/authoritative -run '^$' -bench 'Benchmark(CompileReplacement|ServeDNSExactA)$' -benchmem -count=5
```

On Go 1.27/darwin-arm64 on an Apple M5 Max, the five-run medians were 11.30 ms to compile and replace a representative 10,000-record zone, 36.77 ms for the large-TXT stress corpus, and 385.8 ns for an exact A query including response packing (536 B and 10 allocations per operation). These are local microbenchmarks, not public-QPS or availability claims; the corpora, heap evidence, and interpretation are recorded in [the architecture document](docs/architecture.md#current-benchmark-evidence).

## Deploy with Paprika on VKE

The chart is in [`charts/dns`](charts/dns), and [`deploy/paprika/application.yaml`](deploy/paprika/application.yaml) bootstraps the restricted `dns` namespace and Paprika `Application`. Its production layout is:

- one `Recreate` control Deployment with the single-writer bbolt `ReadWriteOnce` PVC and authenticated Connect/snapshot endpoints;
- three read-only authority StatefulSet pods, each with a retained `ReadWriteOnce` snapshot-cache PVC, required hostname anti-affinity, `DoNotSchedule` topology spread, and a PodDisruptionBudget with `minAvailable: 2`; and
- one independently deployable web Deployment; and
- two OpenTelemetry Collector replicas receiving server-side OTLP and exposing application and self-metrics through a headless Service so Prometheus scrapes every replica.

Before applying the manifest:

1. Replace the fail-closed full Git SHA placeholder and both image digest placeholders with one reviewed revision and its published server/web digests. Tags are not used for a production rollout.
2. Make both GHCR packages public and verify anonymous pulls, or configure `imagePullSecrets`; newly published GHCR packages are private by default.
3. Create the referenced `dns-auth` Secret out of band with distinct `api-bearer-token` and `snapshot-bearer-token` keys. The chart never renders token values; control receives both tokens and authority pods receive only the snapshot token.
4. Confirm `vultr-block-storage-retain` exists, replace `control.nameservers` with the real authoritative names, and arrange their stable addresses and registrar/parent-zone glue.
5. Reconcile the separately managed Vultr DNS load balancer against the current core VKE instance IDs, then keep `nodePortService.enabled=true` and the experimental `dnsService` path disabled.
6. Apply the source-controlled Deephost Prometheus and existing-Grafana integration described in [the observability runbook](docs/observability.md). The VKE Prometheus is not driven by the installed `ServiceMonitor`/`PrometheusRule` CRDs, so a live ConfigMap patch would be incomplete and would be reverted by Paprika.
7. For the operator console, enable the guarded `httpRoute`; its Connect prefix targets control and `/` targets web. For remote authorities, the optional snapshot route exposes only exact path `/internal/v1/snapshot`. If the chart issues a Certificate and ReferenceGrant, the platform owner must still add that Secret as a `certificateRef` on the shared Gateway.

Plan the load balancer first; only run apply after reviewing the deterministic diff. The default is one billable LB node, with odd higher counts available through `--nodes`:

```sh
go run ./cmd/vultr-dns-lb --plan --instance-id='<core-vke-id-1>' --instance-id='<core-vke-id-2>' --instance-id='<core-vke-id-3>' --instance-id='<core-vke-id-4>'
go run ./cmd/vultr-dns-lb --apply --instance-id='<core-vke-id-1>' --instance-id='<core-vke-id-2>' --instance-id='<core-vke-id-3>' --instance-id='<core-vke-id-4>'
```

The reconciler preserves unrelated load-balancer fields, manages UDP/53 → UDP/30053 and TCP/53 → TCP/30053, configures the TCP/30053 health check and public v4/v6 port-53 firewall rules, and fails closed on duplicate exact-label matches. Its complete contract is in [`cmd/vultr-dns-lb/README.md`](cmd/vultr-dns-lb/README.md). Re-run it whenever core workers are scaled or replaced, and separately ensure worker firewalls admit the load balancer's TCP and UDP traffic to `30053`.

Then bootstrap the pinned application:

```sh
kubectl apply -f deploy/paprika/application.yaml
```

Paprika polls the pinned full Git revision, self-heals drift, and health-checks both the control Service and the authority health Service before rollback decisions. The production overlay sets snapshot staleness to `24h`; authority pods retain and continue serving their last checksum-valid snapshot through a shorter control outage, while readiness fails after the configured freshness window.

The UI's bearer token is still required through the HTTPRoute; the route is transport, not authentication. The snapshot feed uses a distinct bearer token. Health endpoints remain unauthenticated. No NetworkPolicy is rendered until the exact Paprika, Gateway, and Vultr load-balancer sources can be safely selected.

The three VKE authority pods improve pod and worker availability but still share one cluster, region, and public load balancer. Follow the [production launch and cutover runbook](docs/production-launch.md) to add and validate an independently operated authority before delegating a production zone.

The retained control volume and per-authority snapshot caches are useful logical recovery copies. They are not an automated, off-provider archival backup: use a separately managed, verified snapshot archive or bbolt-consistent copy outside the cluster's failure domain.

## Repository map

```text
cmd/dns/                 process entry point
cmd/dnsctl/              authenticated BIND import/export and platform backup/rebuild client
cmd/dns-restore/         offline snapshot verification and transactional restore
cmd/vultr-dns-lb/        plan/apply reconciler for the Sydney Vultr DNS LB
internal/activity/       append-only event log and its Connect service
internal/authoritative/  immutable query snapshot and DNS handler
internal/billing/        Stripe facade, webhook queue, and the in-process fake
internal/control/        Connect API implementation
internal/enginedns/      engine record planning, provenance, and per-zone serialisation
internal/hosting/        DeepHost facade, deploy state machine, and workers
internal/mail/           Stalwart facade, mail records, mailboxes, and forwarders
internal/platform/       platform store, engine prober, backup and rebuild
internal/secretguard/    log and value redaction for engine credentials
internal/snapshot/       checksummed feed, consumer, freshness, and cache logic
internal/telemetry/      bounded OpenTelemetry metrics, traces, and SDK lifecycle
internal/zone/           validation and bbolt persistence
internal/zonefile/       bounded BIND parser and canonical exporter
proto/                   protobuf API source
gen/go/                  generated Go bindings
apps/web/                Next.js control panel and generated TS bindings
charts/dns/              Kubernetes Helm chart
deploy/paprika/          Paprika bootstrap manifest
docs/architecture.md     design, operations, limitations, and roadmap
docs/observability.md    metric contract, VKE Prometheus path, queries, and rollout
docs/platform-runbook.md platform store backup, restore, rebuild, and secret rotation
docs/production-launch.md production launch and cutover gates
```

## Status and roadmap

v0 now includes bearer-authenticated operator access, checksum-validated snapshot replication to read-only authorities, and durable Prometheus ingestion through OpenTelemetry. It still serves unsigned zones only: there is no DNSSEC, AXFR/IXFR/NOTIFY/TSIG, recursive resolution, multi-writer control plane, tenancy or per-user roles — the activity log records every mutation, but the operator bearer token is the only principal, so it attributes actions to "operator", not to a person — retained trace backend, alert notification delivery, API rate limiting, or DNS response-rate limiting. The bbolt control store remains single-writer, and the VKE replicas alone are not independent failure domains. The roadmap is detailed in [docs/architecture.md](docs/architecture.md#limitations-and-roadmap).

## License

Apache-2.0. See [LICENSE](LICENSE).
