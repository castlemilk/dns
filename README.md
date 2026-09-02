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
- Distroless/non-root container images, a Helm chart, and a Paprika application manifest for VKE
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

Open [http://localhost:3000](http://localhost:3000), enter `local-operator-token`, and unlock the console. The token stays only in that tab's `sessionStorage`, is attached by the Connect-ES interceptor, and is cleared by Lock or an unauthenticated response. `GET /healthz` and `GET /readyz` remain unauthenticated; an authority's readiness additionally enforces snapshot freshness. The variables and defaults are listed in [`.env.example`](.env.example); the Go binary does not load that file automatically, so export or source it when using it locally.

To seed a zone without the UI, either set `DNS_BOOTSTRAP_ZONE=example.test` before starting the server or use the API below. A bootstrap occurs only when the database contains no zones.

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
pnpm --dir apps/web build
helm lint charts/dns
helm template dns charts/dns --namespace dns
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
- one independently deployable web Deployment.

Before applying the manifest:

1. Replace the fail-closed full Git SHA placeholder and both image digest placeholders with one reviewed revision and its published server/web digests. Tags are not used for a production rollout.
2. Make both GHCR packages public and verify anonymous pulls, or configure `imagePullSecrets`; newly published GHCR packages are private by default.
3. Create the referenced `dns-auth` Secret out of band with distinct `api-bearer-token` and `snapshot-bearer-token` keys. The chart never renders token values; control receives both tokens and authority pods receive only the snapshot token.
4. Confirm `vultr-block-storage-retain` exists, replace `control.nameservers` with the real authoritative names, and arrange their stable addresses and registrar/parent-zone glue.
5. Reconcile the separately managed Vultr DNS load balancer against the current core VKE instance IDs, then keep `nodePortService.enabled=true` and the experimental `dnsService` path disabled.
6. For the operator console, enable the guarded `httpRoute`; its Connect prefix targets control and `/` targets web. For remote authorities, the optional snapshot route exposes only exact path `/internal/v1/snapshot`. If the chart issues a Certificate and ReferenceGrant, the platform owner must still add that Secret as a `certificateRef` on the shared Gateway.

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
cmd/dnsctl/              authenticated BIND import/export client
cmd/dns-restore/         offline snapshot verification and transactional restore
cmd/vultr-dns-lb/        plan/apply reconciler for the Sydney Vultr DNS LB
internal/authoritative/  immutable query snapshot and DNS handler
internal/control/        Connect API implementation
internal/snapshot/       checksummed feed, consumer, freshness, and cache logic
internal/zone/           validation and bbolt persistence
internal/zonefile/       bounded BIND parser and canonical exporter
proto/                   protobuf API source
gen/go/                  generated Go bindings
apps/web/                Next.js control panel and generated TS bindings
charts/dns/              Kubernetes Helm chart
deploy/paprika/          Paprika bootstrap manifest
docs/architecture.md     design, operations, limitations, and roadmap
docs/production-launch.md production launch and cutover gates
```

## Status and roadmap

v0 now includes bearer-authenticated operator access and checksum-validated snapshot replication to read-only authorities. It still serves unsigned zones only: there is no DNSSEC, AXFR/IXFR/NOTIFY/TSIG, recursive resolution, multi-writer control plane, tenancy/roles/audit log, durable metrics, API rate limiting, or DNS response-rate limiting. The bbolt control store remains single-writer, and the VKE replicas alone are not independent failure domains. The roadmap is detailed in [docs/architecture.md](docs/architecture.md#limitations-and-roadmap).

## License

Apache-2.0. See [LICENSE](LICENSE).
