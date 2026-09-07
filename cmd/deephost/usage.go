package main

// The usage blocks. They are the CLI's documentation of record: every command
// in §1 of the spec appears here exactly as it is dispatched, so `deephost help`
// and the code cannot drift apart.

const rootUsage = `usage: deephost <group> <command> [flags]

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
  --api-url URL      control plane base URL (DEEPHOST_API_URL, default http://127.0.0.1:8080)
  --token TOKEN      operator credential (DEEPHOST_API_TOKEN); prefer ` + "`deephost auth login`" + `
  --console-url URL  console base URL for ` + "`deephost auth login`" + ` (DEEPHOST_CONSOLE_URL)
  --json             print one JSON document on stdout instead of a table
  --yes              answer the confirmation of a destructive command
  --timeout D        per-request timeout (default 30s)

exit codes: 0 ok, 1 error, 2 usage`

const authUsage = `usage: deephost auth <command> [flags]

  deephost auth login [--console-url URL] [--no-browser] [--timeout D]
      Hand the operator credential from an unlocked console session to this
      machine. This is not OIDC and there is no identity provider: the console
      has one static operator token, and this is an authorization-code-shaped
      handoff of it over a loopback callback.
  deephost auth logout          forget the stored credential
  deephost auth status          where the credential and URL in force came from
  deephost auth token [--show]  print the credential (only with --show)`

const statusUsage = `usage: deephost status [--probe] [--json]

Reports each engine (DNS, hosting, mail, billing) as configured, reachable or
neither, with the environment variable names an unconfigured engine is waiting
for. --probe re-probes now instead of reading the last result.`

const domainUsage = `usage: deephost domain <command> [flags]

  deephost domain list                 every zone, with its record count
  deephost domain add NAME             create a zone
  deephost domain show NAME|ZONE_ID    one zone, its nameservers and its records
  deephost domain delete NAME|ZONE_ID  delete a zone and everything in it (--yes)`

const recordUsage = `usage: deephost record <command> DOMAIN [flags]

  deephost record list DOMAIN [--type TYPE] [--name NAME]
  deephost record add DOMAIN --name NAME --type TYPE --value VALUE [--ttl SECONDS]
  deephost record edit DOMAIN RECORD_ID [--name NAME] [--type TYPE] [--value VALUE] [--ttl SECONDS]
  deephost record delete DOMAIN RECORD_ID [--yes]
  deephost record import DOMAIN --file PATH|- [--mode create|replace] [--dry-run]
  deephost record export DOMAIN [--output PATH|-]

DOMAIN is a zone name or a zone id. TYPE is one of A, AAAA, CNAME, MX, TXT, NS,
SRV, CAA, SOA. Records written by the hosting or mail engines are shown with
their source and are not yours to edit.`

const siteUsage = `usage: deephost site <command> DOMAIN [flags]

  deephost site list
  deephost site show DOMAIN
  deephost site attach DOMAIN --framework static|node|nextjs [--repository URL]
      [--branch BRANCH] [--path DIR] [--skip-www] [--www-mode serve|redirect]
      [--replace-conflicting] [--dry-run]
  deephost site detach DOMAIN [--keep-dns] [--keep-app] [--yes]
  deephost site deploy DOMAIN [--repository URL] [--revision REV] [--path DIR]
      [--framework F] [--upload-id ID] [--git-token-stdin]
  deephost site deploys DOMAIN [--limit N] [--cursor ID] [--deploy-id ID]
  deephost site update DOMAIN [--repository URL] [--branch BRANCH] [--path DIR]
      [--www-mode serve|redirect]
  deephost site rollback DOMAIN --deploy-id ID
  deephost site logs DOMAIN [--deploy-id ID] [--tail LINES]
  deephost site env get DOMAIN [--environment ENV]
  deephost site env set DOMAIN NAME [--environment ENV] [--value-file PATH]
  deephost site env unset DOMAIN NAME [--environment ENV] [--yes]
  deephost site www DOMAIN --mode serve|redirect
  deephost site reapply-dns DOMAIN [--replace-conflicting] [--dry-run]
  deephost site gateway [--confirm ADDRESS,ADDRESS]

An environment variable's value is read from stdin, or from --value-file, so it
never appears in the process list. Values are write-only: nothing can read one
back, here or in the console.`

const mailUsage = `usage: deephost mail <command> [DOMAIN] [flags]

  deephost mail status
  deephost mail list
  deephost mail show DOMAIN
  deephost mail bind DOMAIN [--dmarc-policy none|quarantine|reject]
      [--client-autoconfig[=false]] [--replace-conflicting] [--dry-run]
  deephost mail update DOMAIN [--dmarc-policy P] [--client-autoconfig[=false]]
  deephost mail unbind DOMAIN [--delete-mailboxes] [--keep-dns] [--yes]
  deephost mail reapply-dns DOMAIN [--replace-conflicting] [--dry-run]
  deephost mail mailbox list DOMAIN
  deephost mail mailbox add DOMAIN LOCAL_PART [--display-name NAME] [--quota-bytes N]
  deephost mail mailbox edit DOMAIN ADDRESS [--display-name NAME] [--quota-bytes N]
  deephost mail mailbox reset DOMAIN ADDRESS [--yes]
  deephost mail mailbox delete DOMAIN ADDRESS [--yes]
  deephost mail forwarder list DOMAIN
  deephost mail forwarder add DOMAIN LOCAL_PART --target ADDRESS [--target ADDRESS]
  deephost mail forwarder delete DOMAIN FORWARDER_ID [--yes]
  deephost mail queue DOMAIN

A new or reset mailbox password is shown once and is never stored by the
platform: put it in a password manager before the terminal scrolls.`

const billingUsage = `usage: deephost billing <command> [flags]

  deephost billing status                       provider, price and webhook health
  deephost billing summary                      one row per zone
  deephost billing subscribe DOMAIN [--return-path PATH] [--open]
  deephost billing confirm SESSION_ID           finish a checkout started elsewhere
  deephost billing portal [--flow payment_method_update|subscription_cancel]
      [--domain DOMAIN] [--return-path PATH] [--open]
  deephost billing invoices [--limit N] [--cursor ID]
  deephost billing retry-webhooks               re-queue dead-lettered events

Billing is informational in this release: nothing here changes what the
platform serves.`

const activityUsage = `usage: deephost activity [flags]

  deephost activity [--domain DOMAIN] [--kind KIND] [--since WHEN] [--limit N] [--cursor ID]
  deephost activity show EVENT_ID

--kind may be repeated, and a kind ending in "." matches a prefix ("deploy.").
--since takes an RFC 3339 timestamp or a duration back from now ("2h", "7d").`

const platformUsage = `usage: deephost platform <command> [flags]

  deephost platform rebuild [--dry-run] [--yes]
      Reconstruct sites, mail domains and subscriptions from the engines after
      the platform store was lost. The control plane refuses unless those
      buckets are all empty. Run it with --dry-run first.`

const versionUsage = `usage: deephost version [--json]`
