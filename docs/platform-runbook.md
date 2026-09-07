# Platform store and engine runbook

This runbook covers the second bbolt file the control plane keeps — the
platform store — and the three optional engines behind it: hosting (DeepHost),
mail (Stalwart) and billing (Stripe). Zone data is not covered here; for the
zone store, snapshots and delegation see
[the production launch runbook](production-launch.md).

## What lives where

| Store | Path | Holds | Recoverable from |
| --- | --- | --- | --- |
| Zone store | `DNS_DATA_PATH` (`/data/dns.db`) | zones and records | a verified snapshot archive, via `dns-restore` |
| Platform store | `DNS_PLATFORM_PATH` (`/data/platform.db`) | site and mail bindings, deploys, subscriptions, checkouts, the webhook queue, the activity log, gateway and capability state | its own backup, or partly from the engines |
| Engines | DeepHost, Stalwart, Stripe | apps, releases, domains, mail accounts, subscriptions | themselves — the control plane is a facade, not the system of record |

The split matters when something is lost. The engines are the truth for the
objects they own; the platform store is the truth for **which zone** each of
those objects belongs to, plus the history the engines do not keep (deploy
rows, log snapshots, activity events).

Both files sit on the same control PVC. The control process holds bbolt's
exclusive lock on each while it runs, and the image is distroless with no shell
and no second binary, so neither file can be copied from inside the pod. The
copies are served by the process that owns the locks.

## Backup

Two routes, both behind `DNS_SNAPSHOT_BEARER_TOKEN`, both `no-store`:

- `GET /internal/v1/snapshot` — the zone snapshot document (JSON).
- `GET /internal/v1/platform-backup` — a consistent `platform.db` copy
  (`application/octet-stream`, truthful `Content-Length`), refused with 503
  above 256 MiB.

The platform route is mounted only on a writer role **and** only when
`DNS_SNAPSHOT_BEARER_TOKEN` is set. A `DNS_ROLE=all` deployment that wants
backups therefore has to set that token even though it serves no snapshot feed;
`GetPlatformStatus` reports whether the route is mounted
(`platform_backup_route`).

### Scheduled backup

`scripts/backup-snapshot.sh` fetches both in one run and encrypts them into one
archive. It derives the platform URL from `SNAPSHOT_URL` unless
`PLATFORM_BACKUP_URL` is set, verifies that the body's size equals the declared
`Content-Length` and that byte offset 16 carries bbolt's magic `ed da 0c ed`,
and records the outcome in the archive manifest:

```json
"platform_store": { "status": "present", "bytes": 262144, "file_sha256": "…" }
```

An HTTP 404 is recorded as `"status": "absent"` and is **not** a failure — that
is the honest answer from a control plane with no platform store. Any other
status, a truncated body, or a missing magic fails the run and leaves nothing
behind.

### Ad-hoc backup

```sh
DNS_SNAPSHOT_BEARER_TOKEN=… DNS_CONTROL_URL=https://dns.example \
  go run ./cmd/dnsctl platform-backup --out ./platform.copy.db
```

`dnsctl platform-backup` performs the same two checks and deletes the file if
either fails, so a rejected copy can never be restored later by mistake. It
prints the path, byte count and SHA-256 as JSON. It never opens bbolt — the
running control plane still owns the lock.

Verify a copy off-host with any bbolt reader, or simply reopen it read-only;
the file is a complete database, not a delta.

## Restore

Order matters, because the reconcilers read both stores at start.

1. **Stop the control plane.** `kubectl scale deploy/<release>-control --replicas=0`.
   One writer is a chart invariant; two processes cannot open either file.
2. **Restore the zone store** from a verified snapshot:
   `go run ./cmd/dns-restore --snapshot ./snapshot.json --database ./dns.db`
   (add `--bump-serials` when resolvers must converge). Copy it to `/data/dns.db`.
3. **Restore the platform store** by copying the backup to `/data/platform.db`,
   mode `0600`. Or leave it absent and rebuild it (below).
4. **Start the control plane.** Confirm in the log and in `/activity`:
   `platform.started`, then the `ReconcileAll` pass. Engine records whose
   provenance was lost in the zone restore are re-adopted by that pass; until
   it runs they show as "Custom" and are editable, which is honest.
5. **Check `/settings`.** Every engine should read `configured · reachable`.
   An unreachable engine shows the probe's reason and the prober records
   `engine.unreachable`; it recovers on its own with `engine.recovered`.

### Rebuilding a lost platform store

`platform.v1.PlatformService.RebuildPlatformStore` reconstructs what the
engines still know:

- **sites** from `ListApps{tenant}` intersected with the zones that exist (app
  names are deterministic per zone), hostnames from `ListDomains`, the live
  deploy from `ListReleases`;
- **mail domains** from the mail engine's domain list;
- **subscriptions** from the Stripe customer found by `BILLING_CUSTOMER_EMAIL`
  and the `zone_id` metadata on each subscription.

It runs **inside the control plane**, so no engine credential leaves the pod,
and it refuses unless the `sites`, `mail_domains` and `subscriptions` buckets
are all empty (`FailedPrecondition: the platform store is not empty`). Rows
whose zone no longer exists are dropped with a warning.

```sh
export DNS_API_BEARER_TOKEN=…
go run ./cmd/dnsctl platform-rebuild --dry-run   # report only, writes nothing
go run ./cmd/dnsctl platform-rebuild
```

Compare the printed counts against what the console showed before the loss. One
`platform.rebuilt` event is recorded.

A rebuilt mail domain comes back in `BINDING` with no DKIM keys and no record
state: the rebuild reads only the engine's domain list. The mail reconciler
fills both in on its next pass (within 10 minutes, or immediately on a poke),
after which the row reports `records_in_sync` honestly. See
[docs/mail-runbook.md](mail-runbook.md) for what that pass republishes and how
to read a row that stays `DEGRADED`. The hosting watcher does the same for
sites: a rebuilt site is `ATTACHING` until the hosts are registered.

**Not recoverable by a rebuild:** the activity log, deploy history and stored
build logs, checkout rows, and the webhook queue. Stripe re-delivers webhook
events for up to a few days, and the billing reconciler converges every row
with a subscription id every six hours, so billing state repairs itself; deploy
history does not come back.

### A restore to an older zone snapshot

Rows whose zone is missing after a restore are marked `ZoneMissing` with reason
`zone no longer exists`, emit one warn event (`site.zone_missing` /
`mail.zone_missing`), and offer only Detach or Unbind. Recreating the zone with
the same name does not re-link them: detach or unbind the row, then attach or
bind again.

## Secret rotation

Every engine credential is a `secretKeyRef` in the chart; the chart never
renders a value. All three rotations end with a control-plane restart, which is
a `Recreate` rollout of a single pod.

### Stripe webhook signing secret (no downtime)

`STRIPE_WEBHOOK_SECRET` accepts **one to three** comma-separated `whsec_`
secrets, so the rotation overlap needs no coordination with Stripe:

1. Create the new endpoint secret in Stripe, keeping the old one.
2. Set the Secret key `stripe-webhook-secret` to `<new>,<old>`.
3. Restart control. Both secrets now verify; the metric
   `deephost.billing.webhooks` carries a `secret` index so you can watch
   traffic move onto the new one.
4. After 24 hours, drop the old secret from the list and restart again.

Register the endpoint with API version **`2026-08-26.dahlia`** — the version the
pinned stripe-go SDK sends. An event signed for a different release train is
rejected with `400 api_version_mismatch`, by design: silently accepting a
payload shaped for another version is how billing state goes quietly wrong.

### Stripe secret key

Create the new restricted key, update `stripe-secret-key`, restart, confirm the
Billing page still lists invoices (the price probe runs on the prober's next
cycle), then revoke the old key in Stripe.

### Stalwart API key

Mint a new API key, update `mail-api-token`, restart, confirm `/settings` shows
mail `reachable` (the prober checks `GET /api/account` and that
`defaultHostname` still equals `MAIL_HOSTNAME`), then delete the old key in
Stalwart. Only an API key is accepted at runtime; admin user/password is a
bootstrap-only credential and never belongs in this Secret.

### DeepHost bearer token

Update `hosting-api-token`, restart, confirm `/settings` shows hosting
`reachable`. The prober also reports whether the token is tenant-scoped; an
unscoped token is a warning chip on that row, not a failure.

## Operational checks

| Symptom | Where to look | Action |
| --- | --- | --- |
| Engine row `unreachable` | `/settings` reason, `engine.unreachable` in `/activity` | Fix the engine or the network; recovery is automatic and records `engine.recovered` |
| Deploys stuck `BUILDING` past the deadline | `/deploys`, `HOSTING_BUILD_DEADLINE` | The poller marks them `ABANDONED` at the deadline; the build itself lives in DeepHost |
| Website card shows `Drifted` | The domain's DNS card | Someone edited an engine record, or an import replaced it — use `Re-apply` |
| Gateway addresses pending | `/settings` gateway block, warn item on the dashboard | A resolved set sharing no address with the current one is never applied silently; confirm it explicitly |
| `N webhook events could not be applied` | `/billing` attention item | Retry them; they are kept with their payloads for seven days |
| Activity page thinning out | `ACTIVITY_MAX_EVENTS` | The janitor trims oldest-first every 15 minutes; raise the bound (1000–1000000) and restart |

## Chart notes

- `platform.storePath` must live under `/data/` and must differ from
  `/data/dns.db`; the chart refuses both mistakes, with and without values
  schema validation.
- An enabled engine requires its `existingSecret`. The chart creates no Secret
  and renders no credential value.
- `platform.hosting.uploads.enabled` mounts a dedicated `emptyDir` at
  `/uploads`, and adds an HTTPRoute rule with a 10-minute request timeout,
  because Envoy's default route timeout is 15 seconds and the root filesystem
  is read-only while `/data` is the DNS volume an upload must never fill.
- The uploads volume `sizeLimit` is derived, never configured directly:
  `2 * (uploads.maxTotalBytes + uploads.maxBytes)`. The server admits a folder
  while staged plus in-flight bytes stay within `maxTotalBytes`, so staged
  directories peak at `maxTotalBytes + maxBytes`, and the deployer packs each
  upload into a `.tar.zst` beside it in the same directory. Admission counts the
  finished archives too, but the one being written grows between two reads, so
  the figure is doubled to cover it. Raising
  `maxTotalBytes` therefore grows the quota with it; kubelet evicts the control
  pod — the single replica holding `dns.db`, `platform.db` and the snapshot
  feed — the moment the volume exceeds its `sizeLimit`, and the server's
  free-space check cannot see that quota because it `statfs`es the node
  filesystem underneath. If you also set
  `control.resources.limits.ephemeral-storage`, it must be at least that
  derived figure; the chart refuses a smaller one it can parse.
- `platform.billing.webhookRoute.enabled` adds exactly one unauthenticated
  path, `Exact /billing/v1/stripe/webhook`. It is signature-verified, capped at
  256 KiB, and rejects any request carrying an `Origin` header. Two more
  answers an operator will see there: `429` once the anonymous admission bucket
  (1200/min) is empty, which Stripe retries and the reconciler converges either
  way, and `400 livemode_mismatch` when a delivery's mode does not match the
  configured key — that one means a test-mode secret was pasted into a live
  rotation, and it is recorded as a `billing.webhook.rejected` activity line.
- `GET /public/v1/plan` and `GET /internal/v1/platform-backup` are deliberately
  not routed publicly. The web pod reads the plan through the cluster Service
  (`DNS_API_URL`); take backups from inside the cluster or through a
  port-forward.
- In production the chart refuses a fake provider, a non-HTTPS non-cluster-local
  engine URL, and a loopback or private gateway address.
