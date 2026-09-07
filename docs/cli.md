# `deephost` — the command line client

`deephost` is one binary that does everything the console does: zones and
records, hosted sites and their deploys, mail bindings, mailboxes and
forwarders, billing, the activity log, and the operator recovery actions. It
talks to the same six Connect services the console talks to, with the same
operator bearer token, so the product is fully configurable without a browser.

The MCP server that exposes the same operations to an agent is documented in
[docs/mcp.md](mcp.md).

## Install

```sh
go build -o ~/bin/deephost ./cmd/deephost
```

There is nothing else to install. `deephost` links only the standard library and
the `connectrpc` packages this module already depends on.

`cmd/dnsctl` is unchanged and still supported; it remains the smaller tool for
BIND import/export and platform backup/rebuild in a script that already knows
its bearer token.

## Settings

| Value | Flag | Environment | File |
| --- | --- | --- | --- |
| Control plane URL | `--api-url` | `DEEPHOST_API_URL` | `api_url` |
| Operator credential | `--token` | `DEEPHOST_API_TOKEN` | `token` |
| Console URL (login only) | `--console-url` | `DEEPHOST_CONSOLE_URL` | — |

Precedence is flag, then environment, then file, then a built-in default of
`http://127.0.0.1:8080` for the URL. There is no default credential.

The file is `~/.config/deephost/config.json` (`XDG_CONFIG_HOME` is honoured),
written `0600` inside a `0700` directory. A file any other user can read is a
startup error rather than a warning: it holds an operator credential, and
continuing would mean spending a secret the program knows is exposed.

The credential is never printed. It is redacted in `deephost auth status`, in
every error sentence, and in the URL `deephost auth login` opens. `deephost auth
token --show` is the single exception, and it warns on stderr first.

## Authentication

```sh
deephost auth login                 # hand the credential over from an unlocked console
deephost auth login --no-browser    # print the approval URL instead of opening one
deephost auth logout                # forget the stored credential
deephost auth status [--check]      # where the URL and credential in force came from
deephost auth token --show          # print the credential (warns first)
```

**This is not OIDC and there is no identity provider.** The control plane has
one static operator credential, `DNS_API_BEARER_TOKEN`, and no user accounts.
`deephost auth login` is an authorization-code-shaped *handoff* of that one
credential from a console tab that is already unlocked, so that the secret is
bound to a single invocation and never travels in a URL, a shell history entry
or a log line. The code and this document say so rather than implying a login.

What happens, in order:

1. `deephost` generates a PKCE verifier and its S256 challenge and a random
   state, binds an HTTP listener to `127.0.0.1` on an ephemeral port, and opens
   the browser at
   `<console>/cli/authorize?callback=http://127.0.0.1:<port>/callback&challenge=<S256>&state=<state>`.
   The verifier stays in the CLI's memory; only the challenge is sent.
2. `/cli/authorize` renders behind the console's existing operator gate, so a
   human must already be unlocked in that tab. The screen names the control API
   the token is for, the loopback URL it will be posted to, and everything the
   CLI will then be able to do.
3. Approve POSTs the state, the challenge and the token to the loopback
   callback. `deephost` checks the state and the challenge against what it
   generated, writes `0600`, and prints the banner. Deny POSTs
   `access_denied`, and `deephost` exits without writing anything.
4. Failures are explicit. A state mismatch, a challenge mismatch, a missing
   proof or an unusable token all abort with nothing stored. After five minutes
   the login times out with a message pointing at `--token`. The listener binds
   `127.0.0.1` only and shuts down the moment it has an answer or times out.

Two honest limits the approval screen states, and this page repeats:

- **The token is the console's own.** It authenticates against the API the
  console is configured for. If `--api-url` points somewhere else, the stored
  credential will not work there.
- **There is no per-CLI revocation.** `deephost auth logout` deletes this
  machine's copy; revoking the credential everywhere means rotating
  `DNS_API_BEARER_TOKEN` on the control plane, which also logs out every
  console tab.

The console and the control plane share an origin in the default deployment,
so `--console-url` is only needed for a split deployment. It cannot be
persisted: the settings file holds `api_url` and `token` and nothing else.

If a browser is not available at all — a remote shell, a CI runner — skip the
flow and set `DEEPHOST_API_TOKEN`, or pass `--token`.

## Global flags

```text
--api-url URL      control plane base URL
--token TOKEN      operator credential
--console-url URL  console base URL, for `deephost auth login`
--json             print one JSON document on stdout instead of a table
--yes              answer the confirmation of a destructive command
--timeout D        per-request timeout (default 30s)
```

They are accepted before or after the command word, so
`deephost record add example.com --type A` and
`deephost --json record add example.com --type A` both work.

## Exit codes

| Code | Meaning |
| --- | --- |
| 0 | the command did what it said |
| 1 | the control plane refused, or could not be reached |
| 2 | the command was used wrongly — an unknown flag, a missing argument, a destructive command without `--yes` in `--json` mode |

## The JSON contract

`--json` writes **exactly one document to stdout and nothing else**. Human
prose, warnings and progress go to stderr, so `deephost … --json | jq` is always
safe.

- A success is the RPC's response rendered with proto field names and
  unpopulated fields kept, so a `false` that matters — `counts_available`,
  `configured`, `in_sync` — is visible rather than absent.
- A failure is `{"error": "…"}` on **stderr**, with the same exit code as text
  mode.
- `--help` is `{"usage": "…"}` on stdout at exit 0, so a caller that always
  parses stdout as JSON never has to special-case it.
- `--json` never prompts. A destructive command without `--yes` exits 2 with
  the error envelope rather than blocking a script on a prompt it cannot see.

Two commands write data rather than a report to stdout, and are not wrapped:
`deephost record export` writes a zone file, and `deephost site logs` writes log
lines with their provenance on stderr.

## Errors

Validation failures keep the control plane's `field: message` convention
verbatim, because rewording them would lose the field:

```text
deephost: domain: is required
deephost: record_id: "abc" is not a record in example.test
```

Everything else is one sentence naming what happened and the next thing to try:

| Connect code | Sentence |
| --- | --- |
| `unauthenticated` | *&lt;url&gt; refused the credential; run `deephost auth login`, or pass --token* |
| `unavailable` | *&lt;url&gt; is not reachable; check --api-url and that it is running* |
| `deadline_exceeded` | *the control plane did not answer in time; try again, or raise --timeout* |
| `failed_precondition` | the control plane's own message, unchanged |
| `unimplemented` | *this control plane does not implement that operation; it may be an older build* |

## Confirmation

These commands destroy data and ask before acting unless `--yes` is given:
`domain delete`, `record delete`, `record import --mode replace`,
`site detach`, `site env unset`, `mail unbind`, `mail mailbox delete`,
`mail mailbox reset`, `mail forwarder delete`, `site gateway --confirm`, and
`platform rebuild` without `--dry-run`.

```text
$ deephost record delete example.test 7942a199…
Delete www A "203.0.113.10" from example.test? [y/N]: n
deephost: cancelled; nothing was changed
```

`site rollback` and the various `--replace-conflicting` flags change what is
served but destroy nothing, so they do not prompt.

## Branding

`deephost`, `deephost version` and a successful `deephost auth login` print the
wordmark; every other command prints a one-line mark. Colour is 24-bit where
the terminal supports it and a 256-colour approximation otherwise. Output that
is redirected, `NO_COLOR`, and `TERM=dumb` all produce plain ASCII with no
escape bytes at all, so nothing leaks into a pipe or a CI log.

## Command groups

### `deephost status`

The Settings view: each engine as configured, reachable or neither. An engine
that is not configured reports the environment variable **names** it is waiting
for — never a value.

```text
$ deephost status
ENGINE   CONFIGURED  REACHABLE  PROVIDER   ENDPOINT  DETAIL
dns      yes         yes        simpledns  -         v0.4.1
hosting  no          no         -          -         not configured; set HOSTING_API_URL, HOSTING_API_TOKEN, HOSTING_GATEWAY_HOSTNAME
mail     no          no         -          -         not configured; set MAIL_API_URL, MAIL_API_TOKEN, MAIL_HOSTNAME
billing  no          no         -          -         not configured; set BILLING_PROVIDER, STRIPE_SECRET_KEY, STRIPE_WEBHOOK_SECRET, STRIPE_PRICE_ID, BILLING_CUSTOMER_EMAIL
```

`--probe` re-probes the engines now instead of reading the last result. The
same variables are documented in [`.env.example`](../.env.example) and the
[platform runbook](platform-runbook.md).

### `deephost domain`

```sh
deephost domain list
deephost domain add example.com
deephost domain show example.com          # name, id, trailing dot — any of them
deephost domain delete example.com --yes
```

```text
$ deephost domain add example.com
created:      example.com
zone id:      a20e2cadaf6fd4050566d75c08797994
nameservers:  ns1.example.net. ns2.example.net.
```

Creating a zone does not register the domain. Delegate it to the nameservers
`deephost status` reports.

### `deephost record`

```sh
deephost record list example.com [--type A] [--name www]
deephost record add example.com --name www --type A --value 203.0.113.10 --ttl 300
deephost record edit example.com RECORD_ID --value 203.0.113.11
deephost record delete example.com RECORD_ID --yes
deephost record import example.com --file zone.txt --mode replace --dry-run
deephost record export example.com --output zone.txt
```

`edit` fills every field you do not mention from the record as it stands, so
changing a TTL cannot silently blank a value. Records the hosting or mail
engine wrote are listed with their source and are refused by `edit` and
`delete` — change them through `deephost site` or `deephost mail` instead.

`import --mode replace` discards the zone's existing records; run it with
`--dry-run` first, which reports the warnings and the resulting zone without
writing anything.

### `deephost site`

```sh
deephost site attach example.com --framework nextjs --repository https://github.com/acme/site --dry-run
deephost site attach example.com --framework nextjs --repository https://github.com/acme/site
deephost site deploy example.com --revision main
deephost site deploys example.com --limit 5
deephost site logs example.com --tail 200
deephost site rollback example.com --deploy-id dpl_123
deephost site env get example.com
printf %s "$DATABASE_URL" | deephost site env set example.com DATABASE_URL
deephost site env unset example.com DATABASE_URL --yes
deephost site www example.com --mode redirect
deephost site detach example.com --yes
```

An environment variable's value is read from stdin or `--value-file`, never
from a flag, because `argv` is readable by every process on the machine. For
the same reason `deephost site deploy` takes a private-repository token from
`DEEPHOST_GIT_TOKEN` or `--git-token-stdin`; `--git-token` is refused as a usage
error.

Values are write-only. `env get` lists names, environments and last-set times,
because nothing — not this CLI, not the console, not the platform — can read a
value back.

### `deephost mail`

```sh
deephost mail bind example.com --dmarc-policy quarantine --dry-run
deephost mail bind example.com --dmarc-policy quarantine
deephost mail show example.com
deephost mail mailbox add example.com ben --display-name "Ben"
deephost mail mailbox reset example.com ben@example.com --yes
deephost mail forwarder add example.com hello --target ben@example.com
deephost mail queue example.com
deephost mail unbind example.com --delete-mailboxes --yes
```

A generated mailbox password is printed **once**, with a warning on stderr,
because the RPC returning it is the only delivery channel there is. Put it in a
password manager before the terminal scrolls; neither the platform nor this
CLI stores it, and nothing can show it again.

Where the mail engine could not vouch for a count, `deephost` prints `unknown`
rather than a zero it did not receive.

### `deephost billing`

```sh
deephost billing status
deephost billing summary
deephost billing subscribe example.com --open
deephost billing portal --flow payment_method_update
deephost billing invoices --limit 20
```

Billing is informational in this release: nothing here changes what the
platform serves. A provider that cannot be reached is reported as unreachable,
never as "no invoices".

### `deephost activity`

```sh
deephost activity --domain example.com --kind deploy. --since 24h
deephost activity show 000001a07068bfd2-0000
```

`--kind` may be repeated, and a kind ending in `.` matches a prefix. `--since`
takes an RFC 3339 timestamp or a duration back from now (`2h`, `7d`).

### `deephost platform`

```sh
deephost platform rebuild --dry-run
deephost platform rebuild --yes
```

Reconstructs sites, mail domains and subscriptions from the engines after the
platform store was lost. The control plane refuses unless those buckets are all
empty. See the [platform runbook](platform-runbook.md) for when this is the
right recovery.

### `deephost version`

Prints the banner, the version, the Go version and the platform. It makes no
network call, so it works before there is a control plane to talk to.
