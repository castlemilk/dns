# `deephost-mcp` — the MCP server

`deephost-mcp` is a stdio Model Context Protocol server that exposes the same
operations as the [`deephost` CLI](cli.md) — DNS, hosting, mail, billing and the
activity log — as 41 tools. Every tool answers with structured JSON, never
prose, so a model reads a document rather than parsing English.

It speaks newline-delimited JSON-RPC on stdin and stdout. **stdout carries
protocol only**: there is no banner and no log line on it. The startup report
and every human-facing message go to stderr.

## Register it

```sh
go build -o ~/bin/deephost-mcp ./cmd/deephost-mcp
claude mcp add deephost -- deephost-mcp
```

Settings resolve exactly as the CLI's do — flag, then environment, then
`~/.config/deephost/config.json`, then a default URL — with one deliberate
difference: **there is no `--token` flag.** A command line is readable by every
process on the machine, so the credential comes only from the settings file
`deephost auth login` writes, or from `DEEPHOST_API_TOKEN` in the MCP client's own
env block.

```json
{
  "mcpServers": {
    "deephost": {
      "command": "deephost-mcp",
      "env": { "DEEPHOST_API_URL": "https://api.example.com" }
    }
  }
}
```

The recommended setup is to run `deephost auth login` once and let the server
read the file; then no credential appears in any configuration at all.

Flags: `--api-url`, `--timeout`, `--version`. Exit codes are 0 ok, 1 error,
2 usage.

The server starts even when it has no credential and even when the control
plane cannot be addressed — an MCP client cannot read the error of a server
that exited before it connected. In that state the read-only tools still work
where they can, `deephost_version` always answers from the process itself, and
every other tool returns a refusal that names the reason.

## Identifying a domain

Every zone-scoped tool accepts either `domain` (the name a person has) or
`zone_id`. Names are resolved through `ListZones`, case-insensitively, with or
without the trailing dot. Where the zone filter is optional the schema says so
rather than claiming one of the two is required.

## The tools

R read-only, W mutating, D destructive (requires `confirm: true`).

| | Tool | What it does |
| --- | --- | --- |
| R | `deephost_version` | version, target and whether a credential is configured; answered locally |
| R | `deephost_status` | each engine configured/reachable, plus the variable names an unconfigured one waits for |
| R | `deephost_domain_list` | every domain with its zone id, serial, nameservers and records |
| R | `deephost_domain_show` | one domain |
| W | `deephost_domain_add` | create a zone (does not register the domain) |
| D | `deephost_domain_delete` | delete a zone and every record in it |
| R | `deephost_record_list` | records, filterable by type or name; engine-written ones are marked managed |
| W | `deephost_record_add` | add one record |
| W | `deephost_record_edit` | change one record; omitted fields keep their current value |
| D | `deephost_record_delete` | delete one record |
| W | `deephost_record_import` | import a zone file; **destructive only when `mode` is `replace` and `dry_run` is not set** |
| R | `deephost_record_export` | export the zone as a zone file |
| R | `deephost_site_list` | every hosted site |
| R | `deephost_site_show` | one site: state, hostnames, certificates, DNS and deploys |
| W | `deephost_site_attach` | create the app, register hostnames, write the DNS |
| D | `deephost_site_detach` | remove the app, its releases, its hostnames and its DNS |
| W | `deephost_site_deploy` | deploy from the site's repository |
| R | `deephost_site_deploy_list` | deploys, newest first |
| W | `deephost_site_rollback` | put a previous deploy back in front of traffic |
| R | `deephost_site_logs` | tail of a deploy's build log, with whether it is live or stored |
| R | `deephost_site_env_get` | variable *names*, environments and last-set times |
| W | `deephost_site_env_set` | set one variable (write-only value) |
| D | `deephost_site_env_unset` | remove one variable; the value cannot be recovered |
| W | `deephost_site_www` | serve or redirect `www.<domain>` |
| R | `deephost_mail_list` | every mail-bound domain |
| R | `deephost_mail_show` | one binding: records, DKIM, counts, queue |
| W | `deephost_mail_bind` | bind a domain to mail and write its records |
| D | `deephost_mail_unbind` | unbind, and with `delete_mailboxes` destroy the mailboxes too |
| R | `deephost_mail_mailbox_list` | mailboxes with quotas and usage |
| W | `deephost_mail_mailbox_add` | create a mailbox — **returns a password once** |
| W | `deephost_mail_mailbox_reset` | new password — **returned once** |
| D | `deephost_mail_mailbox_delete` | delete a mailbox and its messages |
| R | `deephost_mail_forwarder_list` | forwarders and their targets |
| W | `deephost_mail_forwarder_add` | create a forwarder |
| D | `deephost_mail_forwarder_delete` | delete a forwarder |
| R | `deephost_mail_queue` | outbound queue, with unknown counts reported as unknown |
| R | `deephost_billing_status` | provider, price, webhook health and per-domain subscriptions |
| W | `deephost_billing_subscribe` | checkout URL for a human to finish |
| W | `deephost_billing_portal` | customer portal URL for a human to use |
| R | `deephost_billing_invoices` | invoices, newest first |
| R | `deephost_activity_list` | the activity log, filterable by domain, kind or time |

Every mutating and destructive tool mirrors a CLI command exactly. Three
read-only tools — `deephost_site_list`, `deephost_site_deploy_list` and
`deephost_mail_list` — have no CLI command of their own; they are views the
console offers and they cost nothing.

## The confirmation contract

A destructive tool refuses unless its arguments carry `confirm: true`, and says
so in its own description, so a model reading `tools/list` learns the rule
before it calls anything. The `confirm` property and the sentence are generated
from the same flag, so a destructive tool cannot fail to declare itself.

```json
{"name": "deephost_record_delete",
 "arguments": {"domain": "example.com", "record_id": "7942a199…"}}
```

```json
{"error": {"kind": "confirmation_required",
           "message": "deephost_record_delete destroys data and was called without confirm: true. Ask the person you are acting for, then call it again with confirm: true."}}
```

`confirm: true` is a **question for a human**, not a retry. The intended
behaviour on receiving that refusal is to tell the person what will be
destroyed and call again only once they have agreed.

`deephost_record_import` is conditionally destructive: creating records or a dry
run destroys nothing and needs no confirmation, while `mode: "replace"` without
`dry_run` does. Requiring confirmation for the harmless cases would only train a
caller to confirm by reflex.

Two tools forward a **typed confirmation** the control plane itself demands:
`deephost_mail_unbind` takes `confirm_zone_name` and `deephost_mail_mailbox_delete`
takes `confirm_address`. These are never derived from a lookup — that would
answer a question meant for a human — so those tools refuse when the target was
identified by `zone_id` or `mailbox_id` instead of by name.

## Refusals

Every refusal is a tool result with `isError: true` whose `structuredContent`
is `{"error": {"kind", "message", "code"}}`. Branch on `kind`, never on the
English.

| Kind | Meaning | What to do |
| --- | --- | --- |
| `invalid_argument` | this server rejected an argument, as `field: message` | fix the argument |
| `confirmation_required` | a destructive tool was called without `confirm: true` | ask a human, then call again |
| `credential_required` | no usable operator credential | run `deephost auth login`, or set `DEEPHOST_API_TOKEN`, and restart the server |
| `not_configured` | this server has no usable control plane | fix `--api-url` / `DEEPHOST_API_URL` |
| `not_found` | no such domain, record, site or mailbox | check the name |
| `unknown_tool` | no tool by that name | read `tools/list` |
| `control_plane` | the platform refused; `code` is the Connect code | read the message |

`control_plane` messages for `invalid_argument`, `not_found`, `already_exists`
and `failed_precondition` pass through verbatim, so the control plane's own
`field: message` survives.

Read-only tools remain available without a credential, because the control
plane answers some of them and its refusal is more informative than a
pre-emptive one. Mutating tools refuse immediately and name the fix. If the
settings file could not be read, the reason travels into that refusal, so the
operator is told to fix the file rather than to log in again.

## Secret handling

**The only secret this server ever returns is a generated mailbox password**,
from `deephost_mail_mailbox_add` and `deephost_mail_mailbox_reset`. It comes back
exactly once. Neither this server nor the platform can produce it again.

The rule travels with it, in the tool's description and again as
`password_note` in the answer: hand it to the mailbox owner through a channel
they already trust, tell them to change it, and keep no copy anywhere — not in
a file, a note, a ticket or a summary. If it cannot be handed over now, call
`deephost_mail_mailbox_reset` later rather than storing it.

Everything else is kept out of the answer by construction:

- **Site environment variable values are write-only.** `deephost_site_env_set`
  forwards the value to the hosting engine and it appears in no answer;
  `deephost_site_env_get` returns names only. Do not put a real secret in a chat
  transcript to get it here — use `deephost site env set`, which reads it from
  stdin.
- **`deephost_site_deploy` accepts no git token**, although the RPC has the
  field. A per-deploy credential handed to a model is a secret in a transcript;
  private repositories go through `deephost site deploy`.
- **The operator credential is rendered nowhere.** It is attached by the
  client's interceptor and never reaches a tool answer, a URL, a log line or a
  metric attribute. A test asserts that none of the three secrets appears in
  any byte the server writes.
- **An unconfigured engine reports variable names, never values**, exactly as
  `deephost status` does.

## Notes for implementers

Answers are rendered with proto field names and unpopulated fields kept, so
`counts_available: false`, `configured: false`, `in_sync: false` and
`live: false` stay on the wire. Omitting them would let a caller read absence
as "no problem", which is exactly what those fields exist to prevent. 64-bit
counts are strings, as protobuf JSON requires.

One JSON-RPC line is capped to match the client's own read bound, so a zone
file that imports through the CLI also imports through a tool call. A longer
line ends the session with a message naming the limit, because the reader
cannot resynchronise mid-stream.
