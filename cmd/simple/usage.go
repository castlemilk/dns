package main

// The usage blocks. They are the CLI's documentation of record: every command
// in §1 of the spec appears here exactly as it is dispatched, so `simple help`
// and the code cannot drift apart.

const rootUsage = `usage: simple <group> <command> [flags]

groups:
  auth      log in, log out and inspect the stored credential
  status    engine status for DNS, hosting, mail and billing
  domain    the zones this platform serves
  record    records inside one zone
  site      static and application hosting for a zone
  mail      the mail binding, its mailboxes and its forwarders
  billing   subscriptions, invoices and the customer portal
  activity  the platform event log
  platform  operator recovery actions
  version   print the version and the banner

global flags:
  --api-url URL      control plane base URL (SIMPLE_API_URL, default http://127.0.0.1:8080)
  --token TOKEN      operator credential (SIMPLE_API_TOKEN); prefer ` + "`simple auth login`" + `
  --console-url URL  console base URL for ` + "`simple auth login`" + ` (SIMPLE_CONSOLE_URL)
  --json             print one JSON document on stdout instead of a table
  --yes              answer the confirmation of a destructive command
  --timeout D        per-request timeout (default 30s)

exit codes: 0 ok, 1 error, 2 usage`

const authUsage = `usage: simple auth <command> [flags]

  simple auth login [--console-url URL] [--no-browser] [--timeout D]
      Hand the operator credential from an unlocked console session to this
      machine. This is not OIDC and there is no identity provider: the console
      has one static operator token, and this is an authorization-code-shaped
      handoff of it over a loopback callback.
  simple auth logout          forget the stored credential
  simple auth status          where the credential and URL in force came from
  simple auth token [--show]  print the credential (only with --show)`

const statusUsage = `usage: simple status [--probe] [--json]

Reports each engine (DNS, hosting, mail, billing) as configured, reachable or
neither, with the environment variable names an unconfigured engine is waiting
for. --probe re-probes now instead of reading the last result.`

const domainUsage = `usage: simple domain <command> [flags]

  simple domain list                 every zone, with its record count
  simple domain add NAME             create a zone
  simple domain show NAME|ZONE_ID    one zone, its nameservers and its records
  simple domain delete NAME|ZONE_ID  delete a zone and everything in it (--yes)`

const recordUsage = `usage: simple record <command> DOMAIN [flags]

  simple record list DOMAIN [--type TYPE] [--name NAME]
  simple record add DOMAIN --name NAME --type TYPE --value VALUE [--ttl SECONDS]
  simple record edit DOMAIN RECORD_ID [--name NAME] [--type TYPE] [--value VALUE] [--ttl SECONDS]
  simple record delete DOMAIN RECORD_ID [--yes]
  simple record import DOMAIN --file PATH|- [--mode create|replace] [--dry-run]
  simple record export DOMAIN [--output PATH|-]

DOMAIN is a zone name or a zone id. TYPE is one of A, AAAA, CNAME, MX, TXT, NS,
SRV, CAA, SOA. Records written by the hosting or mail engines are shown with
their source and are not yours to edit.`

const siteUsage = `usage: simple site <command> DOMAIN [flags]

  simple site list
  simple site show DOMAIN
  simple site attach DOMAIN --framework static|node|nextjs [--repository URL]
      [--branch BRANCH] [--path DIR] [--skip-www] [--www-mode serve|redirect]
      [--replace-conflicting] [--dry-run]
  simple site detach DOMAIN [--keep-dns] [--keep-app] [--yes]
  simple site deploy DOMAIN [--repository URL] [--revision REV] [--path DIR]
      [--framework F] [--upload-id ID] [--git-token-stdin]
  simple site deploys DOMAIN [--limit N] [--cursor ID] [--deploy-id ID]
  simple site update DOMAIN [--repository URL] [--branch BRANCH] [--path DIR]
      [--www-mode serve|redirect]
  simple site rollback DOMAIN --deploy-id ID
  simple site logs DOMAIN [--deploy-id ID] [--tail LINES]
  simple site env get DOMAIN [--environment ENV]
  simple site env set DOMAIN NAME [--environment ENV] [--value-file PATH]
  simple site env unset DOMAIN NAME [--environment ENV] [--yes]
  simple site www DOMAIN --mode serve|redirect
  simple site reapply-dns DOMAIN [--replace-conflicting] [--dry-run]
  simple site gateway [--confirm ADDRESS,ADDRESS]

An environment variable's value is read from stdin, or from --value-file, so it
never appears in the process list. Values are write-only: nothing can read one
back, here or in the console.`

const mailUsage = `usage: simple mail <command> [DOMAIN] [flags]

  simple mail status
  simple mail list
  simple mail show DOMAIN
  simple mail bind DOMAIN [--dmarc-policy none|quarantine|reject]
      [--client-autoconfig[=false]] [--replace-conflicting] [--dry-run]
  simple mail update DOMAIN [--dmarc-policy P] [--client-autoconfig[=false]]
  simple mail unbind DOMAIN [--delete-mailboxes] [--keep-dns] [--yes]
  simple mail reapply-dns DOMAIN [--replace-conflicting] [--dry-run]
  simple mail mailbox list DOMAIN
  simple mail mailbox add DOMAIN LOCAL_PART [--display-name NAME] [--quota-bytes N]
  simple mail mailbox edit DOMAIN ADDRESS [--display-name NAME] [--quota-bytes N]
  simple mail mailbox reset DOMAIN ADDRESS [--yes]
  simple mail mailbox delete DOMAIN ADDRESS [--yes]
  simple mail forwarder list DOMAIN
  simple mail forwarder add DOMAIN LOCAL_PART --target ADDRESS [--target ADDRESS]
  simple mail forwarder delete DOMAIN FORWARDER_ID [--yes]
  simple mail queue DOMAIN

A new or reset mailbox password is shown once and is never stored by the
platform: put it in a password manager before the terminal scrolls.`

const billingUsage = `usage: simple billing <command> [flags]

  simple billing status                       provider, price and webhook health
  simple billing summary                      one row per zone
  simple billing subscribe DOMAIN [--return-path PATH] [--open]
  simple billing confirm SESSION_ID           finish a checkout started elsewhere
  simple billing portal [--flow payment_method_update|subscription_cancel]
      [--domain DOMAIN] [--return-path PATH] [--open]
  simple billing invoices [--limit N] [--cursor ID]
  simple billing retry-webhooks               re-queue dead-lettered events

Billing is informational in this release: nothing here changes what the
platform serves.`

const activityUsage = `usage: simple activity [flags]

  simple activity [--domain DOMAIN] [--kind KIND] [--since WHEN] [--limit N] [--cursor ID]
  simple activity show EVENT_ID

--kind may be repeated, and a kind ending in "." matches a prefix ("deploy.").
--since takes an RFC 3339 timestamp or a duration back from now ("2h", "7d").`

const platformUsage = `usage: simple platform <command> [flags]

  simple platform rebuild [--dry-run] [--yes]
      Reconstruct sites, mail domains and subscriptions from the engines after
      the platform store was lost. The control plane refuses unless those
      buckets are all empty. Run it with --dry-run first.`

const versionUsage = `usage: simple version [--json]`
