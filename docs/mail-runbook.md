# Mail runbook — Stalwart behind the control plane

The control plane never holds a Stalwart administrator password. It authenticates
with a **long-lived API key** (`MAIL_API_TOKEN`) that belongs to one dedicated
account. Minting and rotating that key are operator actions run from these
scripts or by hand; no runtime code creates credentials.

Everything below is verified against **Stalwart v0.16.20** (community edition).
v0.16 removed the REST management API and the TOML configuration: every
management action is a JMAP method `x:<Object>/get|set|query` posted to
`POST /jmap` under the capability `urn:stalwart:jmap`. Old tutorials describing
`/api/principal`, `/api/dns/records` or `stalwart-cli` inside the image do not
apply — those paths answer 404 and the CLI ships separately.

---

## 1. Local development

```sh
scripts/dev/stalwart-up.sh
```

It starts `simple-stalwart-mail` from `stalwartlabs/stalwart:v0.16` with the
data under `$SCRATCH/stalwart-mail/` (override with `STALWART_DIR`), bootstraps
it headlessly, creates the control-plane account and mints the API key. Host
ports default to 20025 (SMTP), 20465, 20993, 20443 and 20080 (management), all
bound to 127.0.0.1; override any of them with `STALWART_PORT_*` when they are
taken.

Nothing secret is printed. The credentials land in files under the data
directory, mode 0600:

| File | Contents |
|---|---|
| `recovery-admin.secret` | the temporary bootstrap admin password |
| `admin.secret` | the permanent `admin@local.test` credential (two lines: user, secret) |
| `api-key.secret` | the control plane's `API_…` key |
| `webhook.secret` | the delivery-event shared secret, once `stalwart-webhook.sh` has run |
| `mail.env` | `MAIL_*` and `DEEPHOST_TEST_STALWART_*` for the shell |

Use it:

```sh
set -a; . "$SCRATCH/stalwart-mail/mail.env"; set +a
make test-integration-stalwart
```

Stop and remove **only** that container when you are done, and delete the data
directory — it holds DKIM private keys and hashed credentials:

```sh
docker rm -f simple-stalwart-mail
rm -rf "$SCRATCH/stalwart-mail"
```

`scripts/dev/stalwart-bootstrap.sh` can be re-run against a container that is
already bootstrapped: it reuses the stored administrator credential, resets the
control-plane account's password and mints a fresh API key.

`scripts/dev/stalwart-webhook.sh` is a separate, optional step that points the
server's delivery events at this control plane's receiver. It is what turns the
Email card's last-24h line on; see "Delivery statistics" below.

### What the bootstrap actually does

1. `x:Bootstrap/set` as the temporary recovery admin, with
   `serverHostname=mail.local.test`, `defaultDomain=local.test`,
   `requestTlsCertificate=false`, `generateDkimKeys=true`, a `Stdout` tracer and
   `dnsServer: Manual`. The response returns the permanent administrator
   credential **exactly once**.
2. `docker restart` — required; the temporary credential stops working.
3. Creates `simple-cp@local.test` with the `sys*` permission set the facade
   uses, then mints the API key **as that account with `accountId` omitted**
   (passing another account's id answers `notCreated {"type":"notFound"}`).

Two details are easy to get wrong and are worth knowing before you edit the
script:

- The permission set is applied as `{"@type":"Merge", "enabledPermissions": …}`,
  not `Replace`. Stalwart refuses to create an account whose role grants
  permissions the caller does not itself hold ("You are not authorized to grant
  permissions: jmapPrincipalCreate, …"), so a `Replace`-scoped principal cannot
  create mailboxes at all. `Merge` keeps the ordinary `User` role and adds the
  management permissions on top.
- `authenticate` must be in the set. Without it every authenticated request from
  that account answers 403, including the one that mints the key.

---

## 2. Production

### The API key

Create one dedicated account per control plane and mint the key under it:

```jsonc
// as the control-plane account, accountId omitted
[["x:ApiKey/set", {"create": {"k1": {
  "description": "simple control plane",
  "permissions": {"@type": "Inherit"},
  "allowedIps": {"198.51.100.10": true},   // the control plane's egress addresses
  "expiresAt": null
}}}, "0"]]
```

Set `allowedIps` to the control plane's egress addresses. It is the difference
between a leaked key being usable from anywhere and being usable from nowhere
that matters.

Store the secret as a Kubernetes Secret key `mail-api-token` and reference it
from the chart's `platform.mail.apiTokenSecret`. It is returned once; there is
no way to read it back.

### Permissions the facade needs

`internal/mail`'s `RequiredPermissions` is the authoritative list, and the
Settings page reports any that are missing rather than assuming them:

```
sysDomainGet   sysDomainCreate   sysDomainUpdate   sysDomainDestroy   sysDomainQuery
sysAccountGet  sysAccountCreate  sysAccountUpdate  sysAccountDestroy  sysAccountQuery
sysAccountPasswordUpdate
sysMailingListGet sysMailingListCreate sysMailingListUpdate sysMailingListDestroy sysMailingListQuery
sysDkimSignatureGet sysDkimSignatureQuery sysDkimSignatureDestroy
sysQueuedMessageGet sysQueuedMessageQuery
sysSystemSettingsGet
```

The permission for the system settings singleton is `sysSystemSettingsGet` (and
`sysSystemSettingsUpdate`). There is no `sysSettingsGet`; the docs page listing
hyphenated pre-0.16 names is stale — read `GET /api/account` or `/api/schema`
for the real enum.

### Networking

- The control plane reaches Stalwart over **HTTPS**, or over plain HTTP only
  when the host is cluster-local; configuration refuses anything else in
  production, because the key travels as a bearer header on every call.
- Keep `:8080` cluster-internal. It is a plain-HTTP management listener.
- Stalwart needs outbound 25 (delivery), 53 (resolution) and 443 (ACME).
- Inbound: 25 (MX), 465, 993, 995, 443. Add 587/143 listeners with
  `x:NetworkListener/set` only if STARTTLS clients matter — v0.16 creates
  implicit-TLS listeners only.
- **PTR** for every outbound address must resolve to `MAIL_HOSTNAME`, and the
  address needs a static egress. Without it Gmail and Outlook defer or
  spam-folder everything.

### Rotation (no downtime)

1. Mint a second key for the same account.
2. Roll the Kubernetes Secret and restart the control plane.
3. Confirm Settings shows `reachable` and no missing permissions.
4. `x:ApiKey/set {"destroy": ["<old id>"]}`.

`GET /api/account` with the new key is the check that it works before you
destroy the old one.

---

## 3. What the facade writes into a customer's zone

Five policy records, and — when the binding asks for them — the client-autoconfiguration
records the server offers:

| Name | Type | Value | Source |
|---|---|---|---|
| `@` | MX | `<MAIL_MX_PRIORITY> <MAIL_HOSTNAME>.` | configuration |
| `@` | TXT | `v=spf1 mx -all`, plus `include:<MAIL_SPF_INCLUDE>` when set | configuration |
| `_dmarc` | TXT | `v=DMARC1; p=<policy>; rua=mailto:<report address>` | configuration |
| `<selector>._domainkey` | TXT | the DKIM public key, one per active key | the server's `dnsZoneFile` |

**MX, SPF and DMARC come from configuration, never from the server.** Stalwart's
own `dnsZoneFile` carries an MX naming whatever it currently calls itself and a
hard-coded `p=reject` DMARC. Copying either would let the mail server, rather
than this deployment, decide where a customer's mail goes — so the facade
refuses to bind, and the reconciler stops writing, whenever
`x:SystemSettings.defaultHostname` differs from `MAIL_HOSTNAME`.

The DKIM values are the only thing taken from the server's zone file, and each
one must parse as `v=DKIM1; k=(rsa|ed25519); …; p=<base64>` before it is
published. A value that does not parse is dropped with a `mail.records.invalid`
warning: receivers treat a broken DKIM record as a failed signature, which is
worse than an unsigned message.

### The client-autoconfiguration records (`publish_client_autoconfig`)

Stalwart renders a second group of records for every domain it hosts, and this
facade republishes exactly this allow-list of them:

| Name | Type | Value | What reads it |
|---|---|---|---|
| `_submissions._tcp` | SRV | `0 1 465 <MAIL_HOSTNAME>.` | RFC 6186 submission discovery |
| `_imaps._tcp` | SRV | `0 1 993 <MAIL_HOSTNAME>.` | RFC 6186 IMAP discovery |
| `_pop3s._tcp` | SRV | `0 1 995 <MAIL_HOSTNAME>.` | RFC 6186 POP3 discovery |
| `_jmap._tcp` | SRV | `0 1 443 <MAIL_HOSTNAME>.` | RFC 8620 JMAP session discovery |
| `_caldavs._tcp` | SRV | `0 1 443 <MAIL_HOSTNAME>.` | RFC 6764 CalDAV |
| `_carddavs._tcp` | SRV | `0 1 443 <MAIL_HOSTNAME>.` | RFC 6764 CardDAV |
| `autoconfig` | CNAME | `<MAIL_HOSTNAME>.` | Thunderbird |
| `autodiscover` | CNAME | `<MAIL_HOSTNAME>.` | Outlook |

The priorities, weights and ports come from the server's own `dnsZoneFile`, not
from this list — the list is only the owners and types. Any record whose target
is not `MAIL_HOSTNAME` is dropped, so a renamed or replaced mail server cannot
point a customer's clients somewhere this deployment does not vouch for.

Note two things that differ from what the older documentation implies, both
verified against v0.16.20 by reading a real `dnsZoneFile`:

- the submission record is `_submissions._tcp` (plural, implicit TLS on 465),
  not `_submission._tcp`;
- there is **no** `_autodiscover._tcp` SRV record. Stalwart offers Outlook an
  `autodiscover` CNAME instead, which is what this facade publishes.

`publish_client_autoconfig` is **on by default for a new binding and off for
every binding made before these records existed**. A row written by an older
build decodes the field as `false`, so the reconciler never adds anything to a
zone underneath a running deployment; the switch is moved explicitly, through
`UpdateMailDomain`. Turning it off removes exactly these records — they are
`MAIL`-sourced like the rest, so they are adopted, aligned, reconciled, released
on unbind with `keep_dns_records`, and swept as orphans, all on the same path.

`RebuildPlatformStore` has no stored switch to read, so it reads the zone: a
binding that already has one of these records published is rebuilt with the
switch on.

The two CNAMEs only do their job if the mail host's certificate covers
`autoconfig.<domain>` and `autodiscover.<domain>`. When it does not, Thunderbird
and Outlook fall through to the SRV records above and to MX guessing, which is
exactly where they would have been without the aliases — so the records cost
nothing, and are worth having wherever the certificate does cover them.

**Still not published**, and shown to the operator as such:

| Record | Why not |
|---|---|
| `mta-sts` CNAME, `_mta-sts` TXT | MTA-STS needs a policy served over HTTPS at `mta-sts.<domain>`. This product serves no such endpoint, and a policy senders cannot fetch is worse than none. |
| `_smtp._tls` TXT | TLS-RPT asks receivers to report on the MTA-STS and DANE policies we just declined to publish. |
| `ua-auto-config` CNAME, `_ua-auto-config` TXT | Stalwart's own draft scheme. The TXT carries a digest of a document served over HTTPS under the customer's name, so it has MTA-STS's certificate problem with none of MTA-STS's client support. |
| TLSA | Needs DANE keys this deployment does not hold; the server emits none. |
| CAA | Not the mail engine's to decide. |

### DKIM

Domains are created with `dkimManagement: Automatic` for both
`Dkim1Ed25519Sha256` and `Dkim1RsaSha256`. A bind waits (up to 20 s) for one
active key per algorithm before publishing: RSA can appear a second or two after
Ed25519, and Gmail and Microsoft verify RSA only, so publishing Ed25519 alone
would leave outbound mail unsigned for exactly the receivers that matter. If the
wait expires the row is marked `DEGRADED` with `dkim_pending` and the reconciler
publishes the key as soon as the server has it.

Rotation does **not** happen automatically: `rotateAfter` requires automatic DNS
management, and this deployment uses `dnsManagement: Manual`. Keys stay active.
To rotate by hand: create a new `x:DkimSignature`, wait for the reconciler to
publish its TXT, then destroy the old signature after a grace period longer than
the record's TTL (3600 s).

---

## 4. Operations

### The domain lifecycle

Binding creates an `x:Domain` whose `description` is `simple zone <zone_id>`.
That description is the ownership marker: an existing domain of the same name is
adopted only when it matches exactly, and `RebuildPlatformStore` uses it to
recover the bindings after `platform.db` is lost. A domain an operator created
by hand in the Stalwart UI is never adopted or destroyed.

Unbinding destroys, in this order: accounts → mailing lists → DKIM signatures →
the domain. The order is not cosmetic — `x:Domain/set destroy` answers
`notDestroyed {"type":"objectIsLinked"}` while any child exists. The account
purge behind a destroy is asynchronous, so the facade retries the domain destroy
once after re-reading the children.

### Diagnosing a degraded domain

| Reason on the row | What happened | What to do |
|---|---|---|
| `MAIL_HOSTNAME (x) differs from the mail server's hostname (y)` | The server was renamed, or `MAIL_HOSTNAME` is wrong | Fix one of them. Nothing is written while they disagree. |
| `DKIM keys still generating` | The RSA key was not ready within the bind's budget | Wait for the next reconcile pass (10 min) or re-apply from the Email card. |
| `zone no longer exists` | The zone was deleted, or a restore went back past its creation | Only Unbind is offered; it frees the mail domain. |
| `a mail domain with this name already exists on the mail server` | A domain of that name exists without this control plane's description | Remove it in Stalwart, or bind a different zone. |
| an MX/SPF/DMARC conflict | An operator added a record the engine's set collides with | Re-apply with "replace these records", or delete the conflicting record. |

### Delivery statistics

Community Stalwart exposes no per-domain delivery *history*: `x:Metric/query`
answers `{"type":"forbidden","description":"This feature is only available in
the Enterprise edition…"}`, and the Prometheus counters at
`/metrics/prometheus` are global with no domain label. An integration test
asserts that `forbidden` refusal, so nobody can start coding against a number
that edition cannot produce.

It **can**, however, push its own delivery events to a webhook, and that is
where the console's delivered/bounced line comes from when it has one.

#### The experiment (v0.16.20, community edition, 2026-09-04)

Run against a throwaway container with a Python listener on the host:

1. `GET /api/schema` (send `Accept-Encoding` — the body is compressed) has an
   `x:WebHook` object with `permissionPrefix: sysWebHook`, and `x:WebHook/query`
   answers normally on the community edition rather than `forbidden`. Its
   `EventType` enum has 637 entries, including the whole `delivery.*` family.
2. `x:WebHook/set` created a hook with `eventsPolicy: "include"`, an
   `httpAuth: {"@type":"Bearer"}` token and a `signatureKey`. Two gotchas:
   `throttle: 0` is refused with `invalidPatch … "Invalid duration value"` (use
   `1000`), and `host.docker.internal` resolved to an **IPv6** address only, so
   an IPv4-only listener never sees the delivery.
3. Nothing arrived. **A webhook change does not take effect until the server
   reloads its configuration.** After `docker restart`, events arrived
   immediately. This was confirmed in the other direction too: changing the
   hook's `url` and sending mail without a restart delivered to the *old* URL.
4. Each POST carries `Authorization: Bearer <bearerToken>` and
   `X-Signature: base64(HMAC-SHA256(signatureKey, rawBody))` — verified byte for
   byte — with a body of `{"events":[{"id","createdAt","type","data":{…}}]}`.

What each outcome emits, observed by sending real mail through the server
(inbound on 25, authenticated submission on 465, and outbound to a relay route
pointed at a local SMTP sink so a *remote* success could be seen at all):

| Outcome | Events | `data.to` |
|---|---|---|
| delivered locally | `delivery.dsn-success`, `delivery.completed` | the single recipient |
| delivered remotely | `delivery.delivered`, `delivery.dsn-success`, `delivery.completed` | the single recipient |
| permanent failure | `delivery.dsn-perm-fail`, `queue.dsn-queued`, `delivery.completed` | the single recipient |
| unknown local mailbox | `smtp.mailbox-does-not-exist` — rejected at RCPT TO, never queued | the address |

So the facade counts exactly two event types:

- **delivered** ← `delivery.dsn-success`
- **bounced** ← `delivery.dsn-perm-fail`

They are the per-recipient records the server writes for the DSN it may have to
send, so there is exactly one per recipient per outcome, for local and remote
delivery alike. `delivery.delivered` is deliberately **not** counted: it fires
only for remote deliveries and only alongside `dsn-success`, so counting both
would double every outbound message and count no local one.
`delivery.completed` is per *message*, not per recipient, and would undercount
a message with several recipients.

#### What this deployment does with them

`MAIL_WEBHOOK_SECRET` is the switch, and it is empty by default. With it empty
there is no route, no counters and the unchanged copy:

> Delivery counts aren't available: this mail server edition doesn't expose
> per-domain history. The queue shows what's in flight right now.

Set it and `POST /mail/v1/delivery-events` is mounted. The receiver:

- checks the bearer in constant time **before** reading the body;
- verifies `X-Signature` when present, and refuses a wrong one;
- refuses a request carrying an `Origin` header (only a browser sends one);
- caps the body at 1 MiB and the batch at 5 000 events;
- deduplicates on the event id, because the server retries a delivery it could
  not complete;
- resolves each event's return path — the address the message was sent from —
  to a **bound** zone, and drops anything that resolves to none. This control
  plane keeps no statistics about domains it does not host, and `<>` (a bounce's
  null return path) is attributed to nothing.

Counts accumulate in memory and are flushed to `platform.db` every 30 s by the
reconciler's goroutine, into at most 25 hourly buckets on the domain's own row.
The window is 24 hours: the current hour and the 23 before it.

Three things keep the number honest:

- `available` is false while no receiver is configured at all, so the console
  says the counts are unavailable instead of showing a zero.
- `available` stays false until the mail server has actually posted a delivery
  here. A secret set on this side is only half of the wiring: a hook that was
  never created, one created without the restart that loads it, a URL the server
  cannot reach and a signature key that does not match are indistinguishable
  from a quiet day, and every one of them used to render as a confident
  "0 delivered · 0 bounced" over a full 24 hours. Until a delivery is accepted
  the status note names what to check; once one has been, a zero is a real zero
  and is reported as one. Deliveries that arrive and are refused are counted per
  reason, and the note names the reason — so a bearer or signature key that does
  not match this control plane's says so on the page instead of in one log line
  a minute.
- `window_hours` on the response is the width **actually observed**, not the
  width asked for. The counters live in this process, so a restart shortens the
  window and the response says so — three hours of counting is reported as three
  hours, never as a day. Buckets written before the restart stay on the row and
  age out unread: the new process cannot tell an hour in which nothing was
  delivered from an hour in which it was not running to be posted to (the server
  discards an undeliverable webhook after `discardAfter`, five minutes in the
  shipped script), and summing those hours would present a hole as a quiet hour.

An event is counted in the hour its `createdAt` falls in, except when that hour
is outside the window above — including the ordinary case of the buffered
deliveries the server retries in the first minutes after this control plane
starts. Those are counted at the moment they arrived instead. The delivery was
accepted, so it is counted somewhere the response will sum: dating it in an hour
nothing reports would store a real delivery in `platform.db` and show the
operator a zero.

The counts are one-sided on purpose. Mail delivered *to* a hosted mailbox emits
`delivery.dsn-success` too, but the commonest inbound failure — a message to a
local address that does not exist — is refused at RCPT TO as
`smtp.mailbox-does-not-exist` and never queued, so it emits nothing at all.
Counting the recipient side as well would add every inbound success and no
inbound failure to a pair of numbers a reader takes for a sending record: a
domain that receives 500 newsletters a day and sends three messages, one of
which hard-bounces, would read as "500 delivered · 1 bounced" — a 0.2% bounce
rate where the real one is 33%. So delivered/bounced is the mail a domain sent.
`DeliveryStats` has no field that could say which direction it describes, so
mail received is not counted at all rather than folded into a figure that cannot
carry it.

Unbinding a domain takes its row and its history with it, and a late event for a
zone that is no longer bound is dropped rather than recreating the row.

#### Wiring it up

Locally, `scripts/dev/stalwart-webhook.sh` does the mail-server half — creates
the hook, subscribes it to the two events, sets the bearer and the HMAC key to
one generated secret, restarts the container, and appends `MAIL_WEBHOOK_SECRET`
to the run's `mail.env`. `--remove` undoes it. Then:

```sh
set -a; . "$SCRATCH/stalwart-mail/mail.env"; set +a
go test ./internal/mail/ -run IntegrationMailDeliveryEvents -v
```

That test sends a real message, asserts that nothing is reported before the
server has posted anything, that the delivery reaches the receiver, and that
a message the domain *received* is not counted as one it sent; it skips when
the webhook env is absent.

In production:

- store the secret in the same Kubernetes Secret as the API key and point
  `platform.mail.webhookSecretKey` at its key; leaving that value `""` (the
  default) leaves the route unmounted;
- the URL Stalwart posts to must be reachable **from the mail server**, and
  should stay cluster-internal — the route is outside the operator bearer
  because the mail server cannot present one;
- the permissions to configure the hook are `sysWebHookGet`, `sysWebHookCreate`,
  `sysWebHookUpdate`, `sysWebHookQuery` and `sysWebHookDestroy`. They are **not**
  in the facade's `RequiredPermissions`: no runtime code creates or edits the
  hook, an operator does, exactly as with the API key;
- the mail server has **two** webhook settings — the `httpAuth` bearer token and
  the `signatureKey` — and this control plane has a variable for each:
  `MAIL_WEBHOOK_SECRET` is compared against the bearer, and
  `MAIL_WEBHOOK_SIGNATURE_KEY` is the HMAC key `X-Signature` is verified with.
  Leave the second unset and one value stands in for both, exactly as before;
  set it (`platform.mail.webhookSignatureKeyKey` in the chart, a second key in
  the same Secret) and it must be the server's `signatureKey`. Whichever way,
  a server signing with a key this side does not hold has every delivery refused
  with `bad_signature`, and the Email card and the Settings page say the
  deliveries are being rejected and why rather than showing a zero;
- remember the restart. A hook created or edited without one is silently
  inactive, which looks identical to a receiver that is not being reached.

What the signature is worth depends on which of the two you configured. With
`MAIL_WEBHOOK_SIGNATURE_KEY` set, it is a genuine second factor: the HMAC key is
not the bearer, a delivery arriving **without** an `X-Signature` is refused
(`missing_signature`), and a caller has to hold both values. With it unset, the
`X-Signature` check is keyed with the bearer this side just compared, so anything
able to present the bearer can compute the signature too, and an unsigned
delivery is accepted on the bearer alone — the check is then a wiring check that
catches a mail server signing with a different key, not a second factor. In that
configuration treat the secret as the whole of the receiver's security: keep the
route cluster-internal, keep the Secret readable only by the control plane and
the mail server, and rotate it as one value on both sides. Anything that holds it
can post delivery counts for any hosted domain.


### Reading the queue

`x:QueuedMessage/query` is queried **unfiltered** and attributed in the client.
The server's `returnPath` and `to` filters are substring matches, and a
forwarder fan-out carries the *external* target as the recipient with the
customer's address only in `orcpt` — so a server-side filter would both over-
and under-match. A message belongs to a zone when the exact domain of
`returnPath`, of any recipient address, or of any recipient's `orcpt` equals the
zone name. When the 500-message page comes back full the summary reports
`available: false` rather than a count that is silently a lower bound.

`x:MailingList/query` has the same shape of problem for a different reason: its
only filters are `text` and `memberTenantId`, and a `domainId` filter answers
`unsupportedFilter`. Forwarder lists are therefore queried unfiltered and paged,
and matched by domain in the client.

### Forwarders

One target that is a mailbox of the same zone becomes an **alias** on that
mailbox: the mail is stored, not relayed. Anything else — an external address,
or several targets — becomes an `x:MailingList`, which relays.

Relayed mail keeps the original envelope sender. Stalwart has no SRS and will
not add one, so SPF at the receiver fails and only DKIM plus ARC sealing carry
the message. The console says so under any forwarder with an external target.

`x:Account/set` replaces an account's whole `aliases` map rather than merging
into it, and adding or removing an alias-backed forwarder is a read-modify-write
over that map. Every alias is therefore written back exactly as it was read,
including its `enabled` flag and its description: an address you disabled in
Stalwart's own UI stays disabled when someone adds an unrelated forwarder in the
console. Note the flip side, which this deployment does not yet surface: a
disabled alias is still listed and counted as a forwarder here, because the wire
has no way to say "this forwarder exists but does not deliver".

### Backup

Back up `/var/lib/stalwart` (RocksDB: settings, DKIM private keys, mail) and
`/etc/stalwart/config.json` together. Losing the RocksDB store loses the DKIM
private keys, and the published TXT records become signatures nobody can make.

Losing this repo's `platform.db` is recoverable: `RebuildPlatformStore` (or
`dnsctl platform-rebuild`) reconstructs the bindings from the engine's domain
descriptions, and the reconciler fills in the DKIM keys and the record state on
its next pass.
