# maild wire contract

This is the **complete** contract `internal/maild` must satisfy. It is derived by
reading `internal/mail/stalwart/{client.go,calls.go,objects.go,errors.go}` and
`internal/mail/{engine.go,records.go,domain.go,mailbox.go,forwarder.go,zonefile.go,webhook.go}`
line by line, plus the committed transcripts in
`internal/mail/stalwart/testdata/`.

**Read this file rather than re-deriving it.** A wrong method name, a wrong JSON
tag or a missing result reference breaks the whole feature silently: the facade
degrades a binding instead of crashing, so a mistake here shows up as "email is
not configured" rather than as a test failure.

## 0. The decision this rests on

`internal/mail` is a finished facade whose `Engine` interface is satisfied by
`*stalwart.Client`. We do **not** write a second `Engine`. `maild` speaks the
same JMAP subset `stalwart.Client` already calls, so pointing `MAIL_API_URL` at
`maild` selects it and nothing in `internal/mail/**` changes.

Consequence: **the client is frozen.** Everything below is a hard requirement on
the server, never a negotiation. If something here looks wrong, it is still what
the client does.

---

## 1. Transport

### 1.1 Endpoints

| Method | Path           | Purpose |
| ------ | -------------- | ------- |
| `POST` | `/jmap`        | every management call |
| `GET`  | `/api/account` | edition + permissions self-check |

`MAIL_API_URL` may carry a path prefix; the client appends `/jmap` and
`/api/account` to `scheme://host` + `TrimSuffix(path, "/")`. Serve both under
whatever prefix the deployment configures.

### 1.2 Authentication

Both endpoints receive `Authorization: Bearer <MAIL_API_TOKEN>`.
The header is **omitted entirely** when the configured token is empty; production
config forbids that, so treat a missing header as unauthenticated.

Compare the token in constant time (`crypto/subtle`). Never log it, never echo it
in an error body.

### 1.3 Status codes — this is load-bearing

`statusError` in `client.go`:

* `200` — success. **Every method-level failure must still be a 200** with the
  failure inside `methodResponses`. A 4xx for "domain already exists" surfaces to
  the operator as `ErrTransport` ("the mail engine did not answer"), which is a
  lie and a support ticket.
* `401` — and only 401 — maps to `ErrUnauthorized` ("the mail engine rejected the
  API key"). Use it for a bad or missing bearer, nothing else.
* anything else — `ErrTransport: the server answered HTTP <n>`.

Request-level violations that have no JMAP representation (body over 10 MB, more
than 16 method calls, unparseable JSON) should answer a non-200; the client will
report a transport error, which is the honest description.

### 1.4 Request headers and body

The client sends `Content-Type: application/json` and `Accept: application/json`,
and a 10 s context timeout per request (`DefaultTimeout`). Answer within that.

Request body:

```json
{
  "using": ["urn:ietf:params:jmap:core", "urn:stalwart:jmap"],
  "methodCalls": [["<method>", {<args>}, "<callId>"], ...]
}
```

* `using` is always exactly those two strings, in that order. Accept them.
  Do not require any other capability.
* `createdIds` is declared `*struct{}` with `omitempty` and is always nil, so the
  key is **never present**. Do not require it.
* Call ids are the decimal strings `"0"`, `"1"` — assigned per request, not
  globally unique.
* Limits the client enforces on itself and you may enforce too:
  `MaxCallsPerRequest = 16`, `MaxObjectsPerCall = 500`, body ≤ 10 MB.

Response body:

```json
{
  "methodResponses": [["<name>", {<args>}, "<callId>"], ...],
  "sessionState": "<opaque string>"
}
```

* `sessionState` must be present and a string; the client decodes but ignores it.
* Every entry must be a **3-element array**; anything else is
  `ErrTransport: a method response was not a [name, args, id] triple`.
* **Emit exactly one response per call, in call order, echoing the call id.**
  `Call` reads results positionally (`decodeAt(responses, 1, …)`), and when a
  response is `"error"` it names the failing method as `calls[index].Name`. An
  error at the wrong index blames the wrong method.
* Extra properties in an args object are ignored by the client. Extra
  `methodResponses` entries beyond the call count are ignored too, but do not
  emit them.
* The response is read through `io.LimitReader(_, 10 MB + 1)`; anything larger is
  a transport error.

### 1.5 Result references (`#ids`) — mandatory

`ListDomains`, `FindDomain`, `ListDkim`, `ListAccounts`, `ListLists` and
`QueryQueue` all chain `query -> get` **in one request**. The `get` call carries
no `ids`; it carries:

```json
"#ids": { "resultOf": "0", "name": "x:Domain/query", "path": "/ids" }
```

Required server behaviour:

1. Find the **already-produced response** whose call id equals `resultOf` **and**
   whose name equals `name`. (Responses are produced in order, so this is always
   an earlier call in the same request.)
2. Evaluate the JSON pointer `path` against that response's args object.
3. The result must be an array of strings; bind it to the `ids` argument of the
   call being executed, then execute normally.
4. On any failure — no such call id, name mismatch, pointer misses, wrong type —
   emit `["error", {"type": "invalidResultReference", "description": "..."}, "<callId>"]`
   at that call's index.

The only pointer the client ever sends is `/ids`. Supporting exactly `/ids` is
sufficient; reject anything else with `invalidResultReference` rather than
guessing.

A `#ids` reference and a literal `ids` never appear together in the same call.

### 1.6 Error shape

`errors.go` parses two shapes.

**Method error** — the whole call failed. The response *name* is the literal
string `"error"`:

```json
["error", {"type": "<type>", "description": "<human sentence>"}, "0"]
```

`description` is optional; an unreadable args object still yields a
`*MethodError` with an empty type. `Call` returns at the first such entry and
does not look at later responses.

**Set error** — a per-object failure inside a `/set` that otherwise succeeded.
See §3.3.

Types the facade reasons about (`errors.go`), and what each one buys:

| type | recognised by | facade behaviour |
| ---- | ------------- | ---------------- |
| `forbidden` | `IsForbidden` | Connect `Unimplemented`, "Not available on this mail server edition." |
| `unsupportedFilter` | — | plain method error |
| `invalidPatch`, `invalidArguments`, `invalidProperties` | `IsInvalid` | blames the request |
| `objectIsLinked` | `IsLinked` | destroy retried once after re-reading children |
| `notFound`, `accountNotFound` | `IsMissing` | a destroy of a missing object is **success** |
| `primaryKeyViolation`, `alreadyExists` | `IsDuplicate` | "that address is already taken" |
| `unknownMethod`, `stateMismatch`, `overQuota`, `tooLarge`, `rateLimit`, `cannotCalculateChanges` | — | plain method error |

Use `unknownMethod` for a method name you do not implement.

---

## 2. `GET /api/account`

Response (`api_account.json`), decoded by `Client.Account`:

```json
{
  "permissions": ["authenticate", "sysDomainGet", ...],
  "edition": "community",
  "locale": "en-US"
}
```

Only `permissions` and `edition` are read. `edition` is display-only — nothing
compares it — and is shown on the Settings page.

`Service.Probe` computes `missingPermissions(info.Permissions)` against
`mail.RequiredPermissions`. Any name absent is reported to the operator as a
missing permission. **`maild` must therefore report all 22 names**, whether or
not it has a permission model of its own:

```
sysDomainGet sysDomainCreate sysDomainUpdate sysDomainDestroy sysDomainQuery
sysAccountGet sysAccountCreate sysAccountUpdate sysAccountDestroy sysAccountQuery
sysAccountPasswordUpdate
sysMailingListGet sysMailingListCreate sysMailingListUpdate sysMailingListDestroy sysMailingListQuery
sysDkimSignatureGet sysDkimSignatureQuery sysDkimSignatureDestroy
sysQueuedMessageGet sysQueuedMessageQuery
sysSystemSettingsGet
```

`maild.GrantedPermissions` (model.go) holds exactly this list; a test in
`store_test.go` asserts it equals `mail.RequiredPermissions`, so drift fails the
build rather than the Settings page.

`authenticate` may be included as well; the facade ignores unknown names.

---

## 3. JMAP method reference

Every method below is issued by `internal/mail/stalwart/calls.go`. **No other
method name is ever sent.** Objects are the `x:` extension objects of capability
`urn:stalwart:jmap`.

Complete list of names the server must implement:

```
x:SystemSettings/get
x:Domain/query   x:Domain/get   x:Domain/set
x:DkimSignature/query   x:DkimSignature/get   x:DkimSignature/set
x:Account/query  x:Account/get  x:Account/set
x:MailingList/query  x:MailingList/get  x:MailingList/set
x:QueuedMessage/query  x:QueuedMessage/get
```

`x:DkimSignature/set` and `x:QueuedMessage/*` are read/destroy only: the client
never creates or updates a DKIM signature or a queued message.

### 3.0 Shared `/get`, `/query` conventions

**`/get` args**

| arg | type | notes |
| --- | ---- | ----- |
| `ids` | `[]string` | literal, or bound from `#ids` (§1.5) |
| `properties` | `[]string` | which properties to return |

**`/get` response args**

```json
{"list": [ {…object…}, … ], "notFound": []}
```

Only `list` is read. `notFound` should be present (the transcripts have it) but
is ignored. `accountId` and `state` may be included; they are ignored.

`properties` handling: always include `id` whatever `properties` says, and
include every property that is requested. Extra properties are harmless — with
one exception worth honouring: `dnsZoneFile` is the expensive one, and
`domainListProperties` deliberately omits it, so skip rendering it when it was
not asked for.

An id that does not exist is simply absent from `list`. `GetDomain` and
`GetAccount` turn an empty `list` into `ErrNotFound`.

**`/query` args**

| arg | type | notes |
| --- | ---- | ----- |
| `filter` | object | per-object, see below |
| `limit` | number | max ids to return |
| `position` | number | offset for paging |

**`/query` response args**

```json
{"ids": ["…"], "position": 0, "queryState": "n", "canCalculateChanges": true}
```

Only `ids` is read (and only where the caller pages). `position`, `queryState`
and `canCalculateChanges` appear in the transcripts; include them for fidelity.

**Paging is a correctness requirement, not a nicety.** `ListDomains` and
`ListLists` loop `position += 500` and stop when a page returns fewer than 500
ids. A server that ignores `position` and always returns the same full page
**loops forever** once a deployment has 500 domains. Honour both `limit` and
`position`.

### 3.1 `x:SystemSettings/get`

The **only** call that matters before any other: `Service.Probe` and
`requireHostname` refuse to bind, publish or reconcile unless
`defaultHostname` equals `MAIL_HOSTNAME` (case-insensitive). Get it wrong and
every mail operation fails with "the mail server calls itself X".

Request:

```json
["x:SystemSettings/get", {"ids": ["singleton"]}, "0"]
```

Note there is no `properties`. A bare `/get` with no `ids` must return an empty
list (the doc comment says so); with `ids: ["singleton"]` return exactly one
object (`system_settings_get.json`):

```json
{"list":[{
  "id": "singleton",
  "defaultHostname": "mail.local.test",
  "defaultDomainId": "b",
  "mailExchangers": {"0": {"hostname": null, "priority": 10}}
}], "notFound": []}
```

* `defaultHostname` — **must equal `MAIL_HOSTNAME`.**
* `mailExchangers` — a map keyed by decimal index strings. `hostname: null`
  means "the default hostname". Read for display only: the MX record the facade
  publishes comes from `MAIL_HOSTNAME` + `MAIL_MX_PRIORITY`, never from here.
* `defaultDomainId` is not decoded.

Empty `list` → `ErrNotFound: the system settings singleton`.

### 3.2 Object property sets the client requests

```go
domainProperties     = ["name","isEnabled","description","dnsZoneFile","createdAt"]
domainListProperties = ["name","isEnabled","description","createdAt"]
accountProperties    = ["emailAddress","description","domainId","quotas","usedDiskQuota","aliases","createdAt"]
dkimProperties       = ["selector","@type","stage","createdAt","publicKey"]
listProperties       = ["name","domainId","description","recipients","emailAddress"]
queueProperties      = ["returnPath","recipients","createdAt","nextRetry"]
```

### 3.3 Shared `/set`

Args (any subset; the client never sends more than one of the three at a time):

```json
{"create":  {"<creationId>": {…}},
 "update":  {"<objectId>":   {…patch…}},
 "destroy": ["<objectId>"]}
```

Response args (`setResponse` in calls.go):

```json
{"created":      {"<creationId>": {"id": "<newId>"}},
 "updated":      {"<objectId>": null},
 "destroyed":    ["<objectId>"],
 "notCreated":   {"<creationId>": <setError>},
 "notUpdated":   {"<objectId>":   <setError>},
 "notDestroyed": {"<objectId>":   <setError>}}
```

* `created[<creationId>].id` is **required** on success; the client errors with
  "reported neither a created object nor an error" otherwise.
* `updated` values are read as raw JSON and ignored — `null` is fine.
* `destroyed` is a plain array of ids and is not read.
* `checkSet` reports the **first** entry, scanning `notCreated`, then
  `notUpdated`, then `notDestroyed`, each in sorted-key order.

`<setError>`:

```json
{"type": "primaryKeyViolation",
 "description": "…",
 "properties": ["email"],
 "linkedObjects": [{"object": "Account", "id": "d"}]}
```

`description`, `properties` and `linkedObjects` are all optional. The
transcripts also carry `objectId`, which the client does not decode.

### 3.4 Domains

#### `x:Domain/query`

`FindDomain` (exact-name lookup):

```json
["x:Domain/query", {"filter": {"name": "acme.dev"}}, "0"]
```

No `limit`, no `position`. **The `name` filter must be supported server-side.**
The client re-checks with `strings.EqualFold`, so a superset is tolerated, but
return the exact match.

`ListDomains` (paging, used by `RebuildPlatformStore`):

```json
["x:Domain/query", {"limit": 500, "position": 0}, "0"]
```

#### `x:Domain/get`

Chained after both queries via `#ids`. Object (`domain_query_get.json`):

```json
{"id": "c",
 "name": "probe.test",
 "isEnabled": true,
 "description": "simple zone z-probe",
 "dnsZoneFile": "…BIND text…",
 "createdAt": "2026-09-03T08:52:28Z"}
```

Go tags: `id name description isEnabled dnsZoneFile createdAt`. Note
**`isEnabled`**, not `enabled`. `createdAt` is parsed with
`time.RFC3339Nano` then `time.RFC3339`; anything else silently becomes the zero
time.

`description` is how the facade decides ownership. `findOrRefuse` and the
reconciler adopt a domain **only** when `description == "simple zone " + zoneID`
exactly. It must round-trip byte-for-byte. `null` decodes to `""`, which never
matches, so a domain the facade created must always report its description back.

#### `x:Domain/set` create

```json
["x:Domain/set", {"create": {"d1": {
  "name": "acme.dev",
  "isEnabled": true,
  "description": "simple zone z-abc123",
  "dkimManagement": {
    "@type": "Automatic",
    "algorithms": {"Dkim1Ed25519Sha256": true, "Dkim1RsaSha256": true},
    "selectorTemplate": "v{version}-{algorithm}-{date-%Y%m%d}",
    "rotateAfter": 7776000000,
    "retireAfter": 604800000,
    "deleteAfter": 2592000000
  },
  "dnsManagement":         {"@type": "Manual"},
  "certificateManagement": {"@type": "Manual"},
  "subAddressing":         {"@type": "Enabled"},
  "reportAddressUri":      "mailto:postmaster@acme.dev",
  "allowRelaying":         false,
  "aliases":               {},
  "catchAllAddress":       null
}}}, "0"]
```

The creation id is always the literal `"d1"`. `description` is **omitted** when
empty (`setDescription`), but the facade always sends one for a domain.

Durations are **milliseconds**: 90 d / 7 d / 30 d.

Required side effects — this is the part that makes binding work:

1. Persist the domain and answer `{"created": {"d1": {"id": "<id>"}}}`.
2. **Generate both DKIM keypairs synchronously**, Ed25519 and RSA-2048, stage
   `active`, selectors expanded from `selectorTemplate` (§4.2).
3. Render `dnsZoneFile` so it contains their TXT records (§4).

`BindMailDomain` polls `ListDkim` for at most **20 s** (`dkimReadyBudget`, 1 s
interval) waiting for one `active` signature per algorithm. Keys that appear
later still work — the reconciler finishes the publish 60 s on — but the operator
sees a DEGRADED binding in the meantime. Generate them in the create call.

Duplicate name → `notCreated: {"d1": {"type": "primaryKeyViolation", "properties": ["name"]}}`.

Immediately after a successful create the client calls `GetDomain(id)`, which
must return the object including `dnsZoneFile`.

#### `x:Domain/set` update

```json
["x:Domain/set", {"update": {"<id>": {
  "description": "…", "reportAddressUri": "mailto:…", "isEnabled": true
}}}, "0"]
```

Only the keys present are changed; a patch with no keys is never sent (the client
short-circuits). `description` here is the raw string, never null.

#### `x:Domain/set` destroy

```json
["x:Domain/set", {"destroy": ["<id>"]}, "0"]
```

**Must refuse with `objectIsLinked` while any account, mailing list or DKIM
signature still references the domain** (`domain_destroy_object_is_linked.json`):

```json
{"notDestroyed": {"c": {"type": "objectIsLinked",
  "linkedObjects": [{"object":"Account","id":"d"},
                    {"object":"DkimSignature","id":"j1"},
                    {"object":"MailingList","id":"b"}]}}}
```

`destroyDomain` deletes the signatures, then the domain, and retries the pair
once on `objectIsLinked`. A destroy of an id that does not exist must be
success — either list it in `destroyed`, or answer
`notDestroyed: {"<id>": {"type": "notFound"}}`; `IsMissing` treats both as done.

### 3.5 DKIM signatures

#### `x:DkimSignature/query`

```json
["x:DkimSignature/query", {"filter": {"domainId": "<id>"}}, "0"]
```

No limit, no position. **The `domainId` filter must be supported server-side** —
a `unsupportedFilter` here breaks every bind.

#### `x:DkimSignature/get`

`properties: ["selector","@type","stage","createdAt","publicKey"]`, chained via
`#ids`. Object (`dkim_signature_get.json`):

```json
{"id": "jc7331anktaa",
 "selector": "v1-rsa-20260903",
 "@type": "Dkim1RsaSha256",
 "stage": "active",
 "publicKey": "MIIBIjANBgkq…",
 "createdAt": "2026-09-03T08:52:28Z"}
```

* `@type` — the **algorithm**, exactly `"Dkim1Ed25519Sha256"` or
  `"Dkim1RsaSha256"`. `waitForDkim` requires one active signature of each before
  a binding leaves DEGRADED.
* `stage` — one of `pending` / `active` / `retiring` / `retired`. Only `active`
  is published (`DkimSignature.Active()`).
* `publicKey` — base64. RSA: the DER **SubjectPublicKeyInfo**
  (`x509.MarshalPKIXPublicKey`, standard base64, no wrapping) — the fixture's
  `MIIBIjANBgkqhkiG9w0BAQEFAAOC…` prefix is exactly that. Ed25519: the raw
  32-byte public key, base64 (44 chars). This value is read for display only;
  **the record actually published comes from `dnsZoneFile`** (§4), so the two
  must agree.

The client sorts the result by selector.

#### `x:DkimSignature/set` destroy

```json
["x:DkimSignature/set", {"destroy": ["<id>"]}, "0"]
```

Only ever a destroy. A missing id is success.

### 3.6 Accounts (mailboxes)

#### `x:Account/query`

```json
["x:Account/query", {"filter": {"@type": "User", "domainId": "<id>"}, "limit": 500}, "0"]
```

No paging loop — one page of up to 500. **Both filter keys must be supported.**
`@type` is `"User"`; `maild` may treat it as the only account type.

#### `x:Account/get`

Object (`account_get.json`):

```json
{"id": "d",
 "emailAddress": "mara@probe.test",
 "description": "Mara",
 "domainId": "c",
 "quotas": {"maxDiskQuota": 5368709120},
 "usedDiskQuota": 1894,
 "aliases": {"0": {"name": "hello", "domainId": "c", "description": null, "enabled": true}},
 "createdAt": "2026-09-03T08:53:21Z"}
```

* `emailAddress` is the full address; the client derives the local part by
  cutting at `@`.
* `quotas.maxDiskQuota` and `usedDiskQuota` are bytes. `usedDiskQuota` is shown
  as live usage — report the real figure, `0` if nothing is stored.
* `aliases` is a **map keyed by decimal index strings**, not an array. Each entry
  has `name` (local part, same domain), `domainId`, `description` (string or
  null), `enabled` (note: **`enabled`**, not `isEnabled`, unlike a domain).
* **The credential must never appear here.** No `credentials`, no `secret`, no
  hash — `properties` never asks for it and a list response must not carry it.

#### `x:Account/set` create

```json
["x:Account/set", {"create": {"u1": {
  "@type": "User",
  "name": "mara",
  "domainId": "<id>",
  "description": "Mara",
  "credentials": {"0": {"@type": "Password", "secret": "<generated passphrase>"}},
  "roles":            {"@type": "User"},
  "permissions":      {"@type": "Inherit"},
  "quotas":           {"maxDiskQuota": 5368709120},
  "aliases":          {},
  "locale":           "en-US",
  "memberGroupIds":   {},
  "encryptionAtRest": {"@type": "Disabled"}
}}}, "0"]
```

Creation id is always `"u1"`. `name` is the **local part**; the server composes
`emailAddress` as `name + "@" + domain.name`. `description` is omitted when the
display name is empty.

`secret` is a generated five-word passphrase (~61.6 bits). Hash it on arrival
(`maild.HashPassword`, PBKDF2-HMAC-SHA256) and drop the plaintext. It must never
be logged, stored in clear, or returned.

Duplicate address → `notCreated: {"u1": {"type": "primaryKeyViolation", "properties": ["email"]}}`
(`account_create_primary_key_violation.json`). The address space is shared with
mailing lists and with other accounts' aliases — refuse a collision with any of
them.

The client immediately calls `GetAccount(id)` afterwards.

#### `x:Account/set` update

Sent by `UpdateMailbox`, the password reset, and both forwarder paths:

```json
["x:Account/set", {"update": {"<id>": {
  "description": "Mara" | null,
  "quotas": {"maxDiskQuota": 5368709120},
  "aliases": {"0": {"name": "hello", "domainId": "c", "description": null, "enabled": true}},
  "credentials": {"0": {"@type": "Password", "secret": "<new passphrase>"}}
}}}, "0"]
```

* Only the keys present are changed.
* `description: null` **clears** the display name (`descriptionValue`).
* **`aliases` replaces the whole map**, it does not merge. The client does a
  read-modify-write over what `ListAccounts` just returned, so the map it sends
  is the complete desired set. Delete any alias not in it.
* `credentials` replaces the password. Re-hash; never store or log the plaintext.

Rejections use `notUpdated` with `invalidPatch` / `invalidProperties`
(`account_create_invalid_patch.json` shows the shape).

#### `x:Account/set` destroy

Missing id is success. The facade treats the purge behind it as asynchronous —
it is allowed to be, but the account must disappear from `x:Account/query`
promptly, because `destroyDomain` retries `objectIsLinked` exactly twice.

### 3.7 Mailing lists (forwarders with external or multiple targets)

#### `x:MailingList/query`

```json
["x:MailingList/query", {"limit": 500, "position": 0}, "0"]
```

**Always unfiltered and paged.** The comment in `calls.go` records that
Stalwart answers `unsupportedFilter` for a `domainId` filter here, so the client
never sends one and matches the domain itself. `maild` must therefore return
**every** list, paged by `limit`/`position`; `ListLists("")` (used by
`RebuildPlatformStore`) depends on that.

#### `x:MailingList/get`

`properties: ["name","domainId","description","recipients","emailAddress"]`.
Object (`mailinglist_query_unfiltered.json`):

```json
{"id": "b",
 "name": "team",
 "domainId": "c",
 "description": "forwarder",
 "emailAddress": "team@probe.test",
 "recipients": {"someone@example.com": true, "mara@probe.test": true}}
```

`recipients` is a **map address → bool**; only `true` entries are kept by the
client. `description` is the literal marker `"forwarder"`
(`stalwart.ListDescription`) for lists this product created.

#### `x:MailingList/set` create

```json
["x:MailingList/set", {"create": {"l1": {
  "name": "team",
  "domainId": "<id>",
  "description": "forwarder",
  "recipients": {"someone@example.com": true, "mara@probe.test": true},
  "aliases": {}
}}}, "0"]
```

Creation id is always `"l1"`. **The client does not re-read the list after
creating it** — it builds the returned `MailingList` locally from the input plus
the new id. So `created.l1.id` is the only thing that must come back, but the
stored object must match what was sent or the next `ListLists` will disagree with
the console.

`emailAddress` is composed by the server as `name + "@" + domain.name`.

#### `x:MailingList/set` destroy

Missing id is success.

### 3.8 Queue

#### `x:QueuedMessage/query`

```json
["x:QueuedMessage/query", {"limit": 500}, "0"]
```

Unfiltered, no position. The client filters by zone itself (`BelongsToZone`)
because the server's `returnPath`/`to` filters are substring matches and a
forwarder fan-out hides the customer address in `orcpt`. If the page comes back
**full** (500 ids) the facade reports "not available" instead of a count, so
returning fewer than 500 when fewer exist is what makes the number showable.

#### `x:QueuedMessage/get`

`properties: ["returnPath","recipients","createdAt","nextRetry"]`. Object
(`queued_message_get_forwarder.json`):

```json
{"id": "j1",
 "returnPath": "sender@example.org",
 "createdAt": "2026-09-03T09:10:00Z",
 "nextRetry": "2026-09-03T09:15:00Z",
 "recipients": {
   "someone@example.com": {
     "orcpt": "rfc822;team@probe.test",
     "queueName": "remote",
     "retryCount": 2,
     "retryDue": "2026-09-03T09:15:00Z",
     "status": {"@type": "TemporaryFailure", "code": 451, "message": "deferred"}
   }}}
```

* `recipients` is a **map keyed by the recipient address**.
* `orcpt` is `null` or `"rfc822;<address>"`; the client strips the prefix. For a
  forwarder fan-out it carries the **customer's** address while the map key is
  the external target — set it, or the message is attributed to nobody.
* `status.@type` ∈ `Scheduled` / `Completed` / `TemporaryFailure` /
  `PermanentFailure`. `code` and `message` are not decoded.
* `nextRetry: null` is fine (decodes to the zero time).

---

## 4. `dnsZoneFile` — the records the facade actually publishes

**This is the single most important output of the whole server.** The DKIM TXT
records published into the customer's zone are parsed out of this text
(`internal/mail/zonefile.go`); `DkimSignature.publicKey` is *not* used to build
them. `internal/mail/stalwart/testdata/zonefile_probe.txt` is a captured, exact
example — match its layout.

### 4.1 Format the parser accepts

`parseZoneFile` handles: absolute owner names, **no `$ORIGIN`, no `$TTL`**,
optional per-record TTL and/or class `IN` in either order, `;` comments outside
quotes, and parenthesised multi-line TXT. Anything it cannot parse is skipped
silently.

Owners **must be absolute** and end with `.<domain>.` — a record whose owner does
not end with the zone's suffix is dropped (that is how the mail host's own SPF
line is ignored).

### 4.2 DKIM — required

One TXT record per **active** signature:

```
<selector>._domainkey.<domain>. IN TXT "v=DKIM1; k=ed25519; h=sha256; p=<base64>"
```

`validDkimValue` will refuse the record unless **all** of these hold:

* the **first** tag is `v` and `v=DKIM1`;
* `k` is present and is exactly `rsa` or `ed25519` (a missing `k` is rejected
  even though RFC 6376 defaults it to rsa);
* `p` is non-empty and decodes as standard base64 after whitespace is stripped.

A selector whose record fails this is **not published** and is reported as
`mail.records.invalid`; its algorithm is then removed from the readiness set, so
the binding never completes. Get the value right.

Long RSA values must be split into character-strings of at most 255 bytes and
wrapped in parentheses, exactly as the probe capture does:

```
v1-rsa-20260903._domainkey.probe.test. IN TXT (
    "v=DKIM1; k=rsa; h=sha256; p=MIIBIjANBgkqhkiG9w0…sP"
    "yIJd60baX2lnbonM4…IDAQAB"
)
```

`joinCharacterStrings` concatenates them; the facade compares TXT by wire text
(`enginedns.TxtEqual`), so the split form and the single-string form are equal.

**Selector template.** `selectorTemplate` is
`v{version}-{algorithm}-{date-%Y%m%d}`; the probe shows it expanding to
`v1-ed25519-20260903` and `v1-rsa-20260903`. So `{version}` → the integer key
version starting at `1`, `{algorithm}` → `ed25519` / `rsa` (**not** the
`Dkim1…Sha256` name), `{date-%Y%m%d}` → the UTC creation date.

The selector is also the join key: `parseDkimRecords` keeps only owners whose
first label is the selector of an `active` signature, and
`publishedSelectors` reports a key as published only when the zone holds that
selector's record **with that exact value**.

### 4.3 Client autoconfiguration — optional but allow-listed

Published only when the binding sets `publish_client_autoconfig`, and only these
owners, and only when the target is exactly `MAIL_HOSTNAME`:

| owner | type | rdata in the probe |
| ----- | ---- | ------------------ |
| `_submissions._tcp` | SRV | `0 1 465 <hostname>.` |
| `_imaps._tcp`       | SRV | `0 1 993 <hostname>.` |
| `_pop3s._tcp`       | SRV | `0 1 995 <hostname>.` |
| `_jmap._tcp`        | SRV | `0 1 443 <hostname>.` |
| `_caldavs._tcp`     | SRV | `0 1 443 <hostname>.` |
| `_carddavs._tcp`    | SRV | `0 1 443 <hostname>.` |
| `autoconfig`        | CNAME | `<hostname>.` |
| `autodiscover`      | CNAME | `<hostname>.` |

An SRV must have exactly four fields with numeric priority/weight/port; a CNAME
exactly one target. Anything else is dropped.

### 4.4 Everything else in the file is deliberately ignored

MX, SPF and DMARC are **derived from configuration** by the facade
(`records.go`), never copied — that is what stops a changed mail server from
redirecting a customer's mail. `mta-sts`, `_mta-sts`, `_smtp._tls`,
`ua-auto-config`, `_ua-auto-config` and TLSA are on a deny-list. `maild` may
emit them for fidelity or omit them; nothing reads them.

The records the facade publishes itself, for reference:

```
@       MX  3600  <MAIL_MX_PRIORITY> <MAIL_HOSTNAME>.
@       TXT 3600  "v=spf1 mx [include:<MAIL_SPF_INCLUDE>] -all"
_dmarc  TXT 3600  "v=DMARC1; p=<policy>; rua=mailto:<report address>"
```

So `maild` must accept mail for the customer domain **on `MAIL_HOSTNAME`**, and
SPF authorises whatever the MX names.

---

## 5. Delivery-event webhook (optional, `internal/mail/webhook.go`)

Only mounted when `MAIL_WEBHOOK_SECRET` is set. If `maild` implements it:

`POST <control plane>/mail/v1/delivery-events`

* `Authorization: Bearer <MAIL_WEBHOOK_SECRET>` — required, compared in constant
  time.
* `X-Signature: base64(HMAC-SHA256(signatureKey, body))` — required when
  `MAIL_WEBHOOK_SIGNATURE_KEY` is set, otherwise optional but verified against
  the bearer when present.
* **No `Origin` header** — a request carrying one is rejected 403.
* Body ≤ 1 MiB, at most 5000 events:

```json
{"events": [{"id": "…", "createdAt": "2026-09-03T09:10:00Z",
             "type": "delivery.dsn-success", "data": {"from": "ada@acme.dev"}}]}
```

* Counted types are exactly `delivery.dsn-success` and `delivery.dsn-perm-fail`;
  anything else is ignored.
* `data.from` is the **envelope sender**; only outbound mail is counted, and
  `"<>"` is dropped.
* `id` must be stable across retries — it is the dedupe key.
* A 200 with no body means accepted; retry on anything else.

---

## 6. Non-negotiables for the implementation

* **Zero new Go module dependencies.** Standard library plus what `go.mod`
  already has (`go.etcd.io/bbolt`, `github.com/miekg/dns`). SMTP is a line
  protocol; DKIM signing is `crypto/rsa` + `crypto/ed25519` + `crypto/sha256`;
  the password KDF is PBKDF2 built on `crypto/hmac` + `crypto/sha256`, written
  out in `store.go`. If a dependency looks unavoidable, **stop and report**.
* **Secrets never leave.** Mailbox passwords, DKIM private keys and the admin
  token must never be logged, never appear in an error string, and never be
  returned by a list call. `maild.Secret` and `maild.PasswordHash` both redact
  themselves under `%v`/`%s`/`%#v`; keep new secret-bearing fields inside those
  types.
* Do not modify `internal/mail/**`.
* gofmt clean, table-driven tests, fakes over mocks, no network in unit tests.

## 7. File ownership

| file | owner |
| ---- | ----- |
| `CONTRACT.md`, `doc.go`, `model.go`, `store.go`, `store_test.go` | CONTRACT (this phase) |
| the JMAP/HTTP server, `#ids` resolution, `/api/account` | JMAP phase |
| DKIM keygen, selector expansion, `dnsZoneFile` rendering, signing | DKIM phase |
| SMTP listeners, queue, delivery | SMTP phase |
| IMAP/mailbox storage | store/IMAP phase |

The `queue` bbolt bucket is created by `store.go` and left empty; it is reserved
for the delivery phase so adding it later needs no migration.
