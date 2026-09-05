package simplemcp

import (
	"context"
	"sort"
	"strings"

	dnsv1 "github.com/castlemilk/dns/gen/go/dns/v1"
)

// handler runs one tool. It is written as a method expression on *Server, so
// the table below reads as a list of operations rather than a list of
// closures.
type handler func(*Server, context.Context, args) (any, error)

// confirmRule decides, from the arguments alone, whether a call that is not
// always destructive is destructive this time.
type confirmRule func(args) (bool, error)

// tool is one exposed operation.
type tool struct {
	Name       string
	Summary    string
	Properties map[string]any
	Required   []string
	Handler    handler
	// Mutates marks a tool that writes. Without a credential it refuses
	// instead of spending a call the control plane would reject.
	Mutates bool
	// Destructive marks a tool that destroys something. It always requires
	// confirm: true.
	Destructive bool
	// ConfirmIf marks a tool that destroys something only sometimes, such as
	// an import that replaces a zone's records. ConfirmWhen describes the
	// condition in the tool's own description.
	ConfirmIf   confirmRule
	ConfirmWhen string
	// Local marks a tool answered from this process, with no control plane and
	// no credential.
	Local bool
}

// needsConfirmation reports whether this tool ever asks for confirm: true, and
// therefore whether its schema and description must mention it.
func (t tool) needsConfirmation() bool { return t.Destructive || t.ConfirmIf != nil }

// Description is the summary plus the sentences the contract requires, added
// in one place so no tool can be destructive without saying so.
func (t tool) Description() string {
	description := t.Summary
	switch {
	case t.Destructive:
		description += " Destructive: it permanently removes data and requires confirm: true in its arguments. Called without it, it refuses and changes nothing. Ask the person you are acting for before you confirm."
	case t.ConfirmIf != nil:
		description += " It requires confirm: true when " + t.ConfirmWhen + ", and refuses without it. Ask the person you are acting for before you confirm."
	}
	return description
}

// inputSchema renders the JSON Schema an MCP client validates against. The
// confirm property is added here rather than by hand, so it cannot go missing
// from a tool that needs it.
func (t tool) inputSchema() map[string]any {
	properties := map[string]any{}
	for name, property := range t.Properties {
		properties[name] = property
	}
	if t.needsConfirmation() {
		properties["confirm"] = property("boolean", "Must be true. Set it only after a human has agreed to this exact change; it is the caller's statement that the loss is intended.")
	}
	schema := map[string]any{"type": "object", "properties": properties}
	if len(t.Required) > 0 {
		required := append([]string(nil), t.Required...)
		sort.Strings(required)
		schema["required"] = required
	}
	return schema
}

func property(kind, description string) map[string]any {
	return map[string]any{"type": kind, "description": description}
}

func str(description string) map[string]any     { return property("string", description) }
func boolean(description string) map[string]any { return property("boolean", description) }
func integer(description string) map[string]any { return property("integer", description) }

func enum(description string, values ...string) map[string]any {
	schema := property("string", description)
	schema["enum"] = values
	return schema
}

func stringArray(description string) map[string]any {
	return map[string]any{"type": "array", "description": description, "items": map[string]any{"type": "string"}}
}

// fields merges property sets, so the domain selector every zone-scoped tool
// shares is written once.
func fields(sets ...map[string]any) map[string]any {
	merged := map[string]any{}
	for _, set := range sets {
		for name, value := range set {
			merged[name] = value
		}
	}
	return merged
}

// domainSelector is on every zone-scoped tool. Either field identifies the
// domain; `domain` is the one a person will have.
var domainSelector = map[string]any{
	"domain":  str("The domain name, for example acme.dev. Either this or zone_id is required."),
	"zone_id": str("The zone id from simple_domain_list. Either this or domain is required."),
}

// optionalDomainSelector is on the tools whose zone filter is optional, so a
// caller is not told a field is required when leaving it out is meaningful.
var optionalDomainSelector = map[string]any{
	"domain":  str("Optional: limit to this domain, for example acme.dev. Omit to cover every domain."),
	"zone_id": str("Optional: limit to this zone id. Omit to cover every domain."),
}

// replaceWhenReplacing is the confirm rule for zone import: creating a zone
// destroys nothing, replacing one discards every record it had.
func replaceWhenReplacing(a args) (bool, error) {
	mode, err := importModeArg(a, "mode")
	if err != nil {
		return false, err
	}
	dryRun, err := boolArg(a, "dry_run", false)
	if err != nil {
		return false, err
	}
	return mode == dnsv1.ZoneImportMode_ZONE_IMPORT_MODE_REPLACE && !dryRun, nil
}

// registry is every tool this server exposes, in the order tools/list reports
// them: the CLI's command groups, in the CLI's order.
var registry = []tool{
	{
		Name:    "simple_version",
		Summary: "Report this MCP server's version, the control plane it addresses, and whether an operator credential is configured. Answers from this process alone, so it works even when the control plane does not.",
		Local:   true,
		Handler: (*Server).serverVersion,
	},
	{
		Name:       "simple_status",
		Summary:    "Read platform status: each engine (dns, hosting, mail, billing) with whether it is configured and reachable, the nameservers to delegate to, and the build. An engine that is not configured is reported as such, with the environment variable names an operator must set — never their values.",
		Properties: map[string]any{"probe": boolean("Re-probe the engines now instead of reading the last probe. Rate limited to once per 10 seconds per engine.")},
		Handler:    (*Server).status,
	},

	// Domains.
	{
		Name:    "simple_domain_list",
		Summary: "List the domains hosted here with their zone ids, serials, nameservers and records.",
		Handler: (*Server).domainList,
	},
	{
		Name:       "simple_domain_show",
		Summary:    "Show one domain: its zone id, serial, nameservers and every record in it.",
		Properties: domainSelector,
		Handler:    (*Server).domainShow,
	},
	{
		Name:       "simple_domain_add",
		Summary:    "Create a zone for a domain. This does not register the domain: the owner must still delegate it to the nameservers simple_status reports.",
		Properties: map[string]any{"name": str("The domain name to host, for example acme.dev.")},
		Required:   []string{"name"},
		Mutates:    true,
		Handler:    (*Server).domainAdd,
	},
	{
		Name:        "simple_domain_delete",
		Summary:     "Delete a domain's zone and every record in it. Any site or mail binding on the domain must be removed first.",
		Properties:  domainSelector,
		Mutates:     true,
		Destructive: true,
		Handler:     (*Server).domainDelete,
	},

	// Records.
	{
		Name:    "simple_record_list",
		Summary: "List a domain's DNS records, optionally filtered by type or by owner name. Records written by the hosting or mail engines are marked managed and are refused by the edit and delete tools.",
		Properties: fields(domainSelector, map[string]any{
			"type": str("Optional filter: one of " + recordTypeList + "."),
			"name": str("Optional filter on the owner name, for example www or @."),
		}),
		Handler: (*Server).recordList,
	},
	{
		Name:    "simple_record_add",
		Summary: "Add one DNS record to a domain.",
		Properties: fields(domainSelector, map[string]any{
			"name":  str("The owner name relative to the domain: @ for the domain itself, www for www.<domain>."),
			"type":  enum("The record type.", "A", "AAAA", "CNAME", "MX", "TXT", "NS", "SRV", "CAA"),
			"value": str("The record value in zone-file form: an address for A/AAAA, a target for CNAME/NS, \"<priority> <host>\" for MX, the text for TXT, \"<priority> <weight> <port> <target>\" for SRV, \"<flags> <tag> <value>\" for CAA."),
			"ttl":   integer("Time to live in seconds. Omit to let the control plane choose its default."),
		}),
		Required: []string{"name", "type", "value"},
		Mutates:  true,
		Handler:  (*Server).recordAdd,
	},
	{
		Name:    "simple_record_edit",
		Summary: "Change one DNS record. Fields left out keep the value the record has now, so a single field can be changed without restating the rest.",
		Properties: fields(domainSelector, map[string]any{
			"record_id": str("The record id from simple_record_list."),
			"name":      str("New owner name. Omit to leave it as it is."),
			"type":      enum("New record type. Omit to leave it as it is.", "A", "AAAA", "CNAME", "MX", "TXT", "NS", "SRV", "CAA"),
			"value":     str("New value. Omit to leave it as it is."),
			"ttl":       integer("New time to live in seconds. Omit to leave it as it is."),
		}),
		Required: []string{"record_id"},
		Mutates:  true,
		Handler:  (*Server).recordEdit,
	},
	{
		Name:        "simple_record_delete",
		Summary:     "Delete one DNS record from a domain.",
		Properties:  fields(domainSelector, map[string]any{"record_id": str("The record id from simple_record_list.")}),
		Required:    []string{"record_id"},
		Mutates:     true,
		Destructive: true,
		Handler:     (*Server).recordDelete,
	},
	{
		Name:    "simple_record_import",
		Summary: "Import a zone file. In create mode it fills a zone that has none of these records; in replace mode it discards the zone's existing records and installs the file's. Use dry_run first: it reports the warnings and the resulting zone without writing anything.",
		Properties: map[string]any{
			"name":      str("The domain the zone file belongs to, for example acme.dev."),
			"zone_file": str("The zone file text, in RFC 1035 master-file form."),
			"mode":      enum("create (default) adds to a zone; replace discards the zone's current records first.", "create", "replace"),
			"dry_run":   boolean("Parse and report the outcome without writing anything."),
		},
		Required:    []string{"name", "zone_file"},
		Mutates:     true,
		ConfirmIf:   replaceWhenReplacing,
		ConfirmWhen: "mode is replace and dry_run is not set, because that discards every record the zone has now",
		Handler:     (*Server).recordImport,
	},
	{
		Name:       "simple_record_export",
		Summary:    "Export a domain's records as a zone file.",
		Properties: domainSelector,
		Handler:    (*Server).recordExport,
	},

	// Sites.
	{
		Name:    "simple_site_list",
		Summary: "List the hosted sites: which domain each serves, its state, its hostnames and its live and latest deploys.",
		Handler: (*Server).siteList,
	},
	{
		Name:       "simple_site_show",
		Summary:    "Show one domain's site: state, hostnames and their certificates, the DNS the hosting engine owns, and the live and latest deploys.",
		Properties: domainSelector,
		Handler:    (*Server).siteShow,
	},
	{
		Name:    "simple_site_attach",
		Summary: "Attach a site to a domain: create the app, register the hostnames and write the DNS records that point the domain at the platform. Use dry_run to see the DNS plan and any conflicting records first.",
		Properties: fields(domainSelector, map[string]any{
			"framework":                   enum("How the site is built and served.", "static", "node", "nextjs"),
			"repository":                  str("Optional GitHub repository, https://github.com/owner/name, remembered as the default for deploys."),
			"branch":                      str("Default revision to deploy. Defaults to main."),
			"path":                        str("Optional path within the repository."),
			"skip_www":                    boolean("Do not register or point www.<domain> at the site."),
			"www_mode":                    enum("What www.<domain> does: serve the same site (default), or redirect to the domain.", "serve", "redirect"),
			"replace_conflicting_records": boolean("Replace existing records that stand in the way. Without it, a conflict is reported and nothing is written."),
			"dry_run":                     boolean("Report the DNS plan and conflicts without creating or writing anything."),
		}),
		Required: []string{"framework"},
		Mutates:  true,
		Handler:  (*Server).siteAttach,
	},
	{
		Name:    "simple_site_detach",
		Summary: "Detach a domain's site: remove the app and its releases, unregister the hostnames and remove the DNS records the hosting engine wrote. The domain and its other records stay.",
		Properties: fields(domainSelector, map[string]any{
			"keep_dns_records": boolean("Keep the engine-written records as ordinary records instead of removing them."),
			"keep_app":         boolean("Leave the hosting app and its releases in place; only stop serving this domain."),
		}),
		Mutates:     true,
		Destructive: true,
		Handler:     (*Server).siteDetach,
	},
	{
		Name:    "simple_site_deploy",
		Summary: "Deploy a domain's site from its git repository. Returns the queued deploy; poll simple_site_show or simple_site_deploy_list for the phase. Private repositories need a per-deploy token, which this server does not accept — use the `simple site deploy` command for those.",
		Properties: fields(domainSelector, map[string]any{
			"repository": str("Repository to build, https://github.com/owner/name. Defaults to the site's."),
			"revision":   str("Branch, tag or commit. Defaults to the site's branch."),
			"path":       str("Path within the repository. Defaults to the site's."),
			"framework":  enum("Override the site's framework for this deploy.", "static", "node", "nextjs"),
			"upload_id":  str("Deploy a previously uploaded bundle by its upload id instead of building from git."),
		}),
		Mutates: true,
		Handler: (*Server).siteDeploy,
	},
	{
		Name:    "simple_site_deploy_list",
		Summary: "List deploys, newest first, for one domain or for every site.",
		Properties: fields(optionalDomainSelector, map[string]any{
			"limit":  integer("How many to return. Default 20, maximum 100."),
			"cursor": str("The next_cursor from a previous call."),
		}),
		Handler: (*Server).siteDeployList,
	},
	{
		Name:       "simple_site_rollback",
		Summary:    "Put a previous deploy back in front of traffic. The deploy must be one this site released before.",
		Properties: fields(domainSelector, map[string]any{"deploy_id": str("The deploy id to return to, from simple_site_deploy_list.")}),
		Required:   []string{"deploy_id"},
		Mutates:    true,
		Handler:    (*Server).siteRollback,
	},
	{
		Name:    "simple_site_logs",
		Summary: "Read the tail of a deploy's build log. Defaults to the site's latest deploy. The answer says whether the log is live, a stored snapshot, or unavailable.",
		Properties: fields(domainSelector, map[string]any{
			"deploy_id":  str("The deploy to read. Defaults to the site's latest deploy."),
			"tail_lines": integer("How many lines from the end. Default 500, maximum 1000."),
		}),
		Handler: (*Server).siteLogs,
	},
	{
		Name:    "simple_site_env_get",
		Summary: "List a site's runtime environment variable names, environments and last-set times. Values are write-only and are never returned by the platform, so none appear here.",
		Properties: fields(domainSelector, map[string]any{
			"environment": enum("Limit to one environment. Omit to list every environment.", "production", "preview", "development"),
		}),
		Handler: (*Server).siteEnvGet,
	},
	{
		Name:    "simple_site_env_set",
		Summary: "Set one runtime environment variable on a site. The value is write-only: it is passed to the hosting engine and is never stored, logged or returned here, and it does not appear in the answer. The running site keeps its current values until the next deploy. Never put a secret in a chat transcript to get it here — set it with the `simple site env set` command instead.",
		Properties: fields(domainSelector, map[string]any{
			"name":        str("The variable name."),
			"value":       str("The value. Write-only: it is never echoed back."),
			"environment": enum("Which environment to set it in. Defaults to production.", "production", "preview", "development"),
		}),
		Required: []string{"name", "value"},
		Mutates:  true,
		Handler:  (*Server).siteEnvSet,
	},
	{
		Name:    "simple_site_env_unset",
		Summary: "Remove one runtime environment variable from a site. The value cannot be recovered afterwards.",
		Properties: fields(domainSelector, map[string]any{
			"name":        str("The variable name."),
			"environment": enum("Which environment to remove it from. Defaults to production.", "production", "preview", "development"),
		}),
		Required:    []string{"name"},
		Mutates:     true,
		Destructive: true,
		Handler:     (*Server).siteEnvUnset,
	},
	{
		Name:       "simple_site_www",
		Summary:    "Choose what www.<domain> does: serve the same site, or redirect to the domain.",
		Properties: fields(domainSelector, map[string]any{"mode": enum("serve or redirect.", "serve", "redirect")}),
		Required:   []string{"mode"},
		Mutates:    true,
		Handler:    (*Server).siteWww,
	},

	// Mail.
	{
		Name:    "simple_mail_list",
		Summary: "List the domains bound to mail, with their state, DKIM keys, the mail records in the zone and whether they are in sync.",
		Handler: (*Server).mailList,
	},
	{
		Name:       "simple_mail_show",
		Summary:    "Show one domain's mail binding: state, DMARC policy, DKIM keys, every mail record with whether it is present and matching, the mailbox and forwarder counts, and the outbound queue summary. Where a count could not be read it says so rather than showing a zero.",
		Properties: domainSelector,
		Handler:    (*Server).mailShow,
	},
	{
		Name:    "simple_mail_bind",
		Summary: "Bind a domain to mail: create it on the mail server, generate DKIM keys and write the MX, SPF, DMARC and DKIM records. Use dry_run to see the DNS plan and any conflicts first.",
		Properties: fields(domainSelector, map[string]any{
			"dmarc_policy":                enum("The DMARC policy to publish. Defaults to the platform's setting.", "none", "quarantine", "reject"),
			"publish_client_autoconfig":   boolean("Also publish the mail client autoconfiguration records. Defaults to on."),
			"replace_conflicting_records": boolean("Replace existing records that stand in the way. Without it, a conflict is reported and nothing is written."),
			"dry_run":                     boolean("Report the DNS plan and conflicts without binding anything."),
		}),
		Mutates: true,
		Handler: (*Server).mailBind,
	},
	{
		Name:    "simple_mail_unbind",
		Summary: "Unbind a domain from mail and remove the mail records from its zone. With delete_mailboxes it also deletes every mailbox on the domain and the messages in them, which cannot be undone.",
		Properties: fields(domainSelector, map[string]any{
			"delete_mailboxes": boolean("Delete the domain's mailboxes and forwarders. Required when any exist. The domain must be named in the domain argument as well, as the control plane's own confirmation."),
			"keep_dns_records": boolean("Keep the mail records as ordinary records instead of removing them."),
		}),
		Mutates:     true,
		Destructive: true,
		Handler:     (*Server).mailUnbind,
	},
	{
		Name:       "simple_mail_mailbox_list",
		Summary:    "List a domain's mailboxes with their addresses, aliases, quotas and usage. No password is stored, so none can be listed.",
		Properties: domainSelector,
		Handler:    (*Server).mailboxList,
	},
	{
		Name:    "simple_mail_mailbox_add",
		Summary: "Create a mailbox and generate its first password. SECRET HANDLING: the password is in the answer, it is shown exactly once, and neither this server nor the platform can ever show it again. Give it to the person who owns the mailbox by a channel they already trust, tell them to change it, and do not write it to a file, a note, a ticket or a summary. If you cannot hand it over now, use simple_mail_mailbox_reset later instead of keeping a copy.",
		Properties: fields(domainSelector, map[string]any{
			"local_part":   str("The part before the @, for example mara."),
			"display_name": str("Optional display name, up to 80 characters."),
			"quota_bytes":  integer("Optional mailbox quota in bytes. Omit for the platform default."),
		}),
		Required: []string{"local_part"},
		Mutates:  true,
		Handler:  (*Server).mailboxAdd,
	},
	{
		Name:    "simple_mail_mailbox_reset",
		Summary: "Generate a new password for a mailbox. The old one stops working at once. SECRET HANDLING: the new password is in the answer, it is shown exactly once, and it cannot be shown again. Give it to the mailbox owner by a channel they already trust and keep no copy of it anywhere.",
		Properties: fields(domainSelector, map[string]any{
			"address":    str("The mailbox address, for example mara@acme.dev. Either this or mailbox_id is required."),
			"mailbox_id": str("The mailbox id from simple_mail_mailbox_list. Either this or address is required."),
		}),
		Mutates: true,
		Handler: (*Server).mailboxReset,
	},
	{
		Name:        "simple_mail_mailbox_delete",
		Summary:     "Delete a mailbox and every message in it. The address must be given in full as the control plane's own confirmation.",
		Properties:  fields(domainSelector, map[string]any{"address": str("The full mailbox address, for example mara@acme.dev. The control plane requires it as confirmation, so it cannot be looked up for you.")}),
		Required:    []string{"address"},
		Mutates:     true,
		Destructive: true,
		Handler:     (*Server).mailboxDelete,
	},
	{
		Name:       "simple_mail_forwarder_list",
		Summary:    "List a domain's forwarders and aliases with their targets.",
		Properties: domainSelector,
		Handler:    (*Server).forwarderList,
	},
	{
		Name:    "simple_mail_forwarder_add",
		Summary: "Create a forwarder: an address on the domain that delivers to one or more other addresses.",
		Properties: fields(domainSelector, map[string]any{
			"local_part": str("The part before the @ of the new address."),
			"targets":    stringArray("Where mail to it should go: one to ten addresses."),
		}),
		Required: []string{"local_part", "targets"},
		Mutates:  true,
		Handler:  (*Server).forwarderAdd,
	},
	{
		Name:        "simple_mail_forwarder_delete",
		Summary:     "Delete a forwarder. Mail to its address stops being delivered.",
		Properties:  fields(domainSelector, map[string]any{"forwarder_id": str("The forwarder id from simple_mail_forwarder_list.")}),
		Required:    []string{"forwarder_id"},
		Mutates:     true,
		Destructive: true,
		Handler:     (*Server).forwarderDelete,
	},
	{
		Name:       "simple_mail_queue",
		Summary:    "Show a domain's outbound mail queue: how many messages are scheduled, failing or failed, and the messages themselves. When the figures could not be read completely it says so rather than reporting a total.",
		Properties: domainSelector,
		Handler:    (*Server).mailQueue,
	},

	// Billing.
	{
		Name:    "simple_billing_status",
		Summary: "Read billing status: whether a provider is configured, the price, the webhook health, and every domain's subscription state. Billing is informational in this release: nothing is switched off when a subscription lapses.",
		Handler: (*Server).billingStatus,
	},
	{
		Name:    "simple_billing_subscribe",
		Summary: "Start a checkout for one domain and return the URL that completes it. Only a human can finish it: give them the URL, do not try to follow it.",
		Properties: fields(domainSelector, map[string]any{
			"return_path": str("Where the console should land afterwards, for example /billing or /domains/<domain>. Must start with /."),
		}),
		Mutates: true,
		Handler: (*Server).billingSubscribe,
	},
	{
		Name:    "simple_billing_portal",
		Summary: "Create a billing portal URL for the customer to manage payment methods or cancel a subscription. Only a human can use it: give them the URL.",
		Properties: fields(map[string]any{
			"domain":  str("The domain whose subscription the portal should open on. Required when flow is subscription_cancel."),
			"zone_id": str("The zone id, as an alternative to domain."),
		}, map[string]any{
			"flow":        enum("Open the portal on a particular task.", "payment_method_update", "subscription_cancel"),
			"return_path": str("Where the console should land afterwards. Must start with /."),
		}),
		Mutates: true,
		Handler: (*Server).billingPortal,
	},
	{
		Name:    "simple_billing_invoices",
		Summary: "List invoices, newest first, with their status and the domain each covers.",
		Properties: map[string]any{
			"limit":  integer("How many to return. Default 25, maximum 100."),
			"cursor": str("The next_cursor from a previous call."),
		},
		Handler: (*Server).billingInvoices,
	},

	// Activity.
	{
		Name:    "simple_activity_list",
		Summary: "Read the activity log, newest first: what changed, who changed it and when. Filter by domain, by event kind, or by time.",
		Properties: fields(optionalDomainSelector, map[string]any{
			"kinds":  stringArray("Event kinds to include. A kind ending in a dot is a prefix, for example deploy. matches every deploy event."),
			"since":  str("Only events at or after this RFC 3339 time, for example 2026-09-05T09:00:00Z."),
			"limit":  integer("How many to return. Default 50, maximum 200."),
			"cursor": str("The next_cursor from a previous call."),
		}),
		Handler: (*Server).activityList,
	},
}

// byName indexes the registry once. The table is a package-level literal, so
// this map is built at start-up and never changes.
var byName = func() map[string]tool {
	index := make(map[string]tool, len(registry))
	for _, definition := range registry {
		index[definition.Name] = definition
	}
	return index
}()

func lookupTool(name string) (tool, bool) {
	definition, found := byName[strings.TrimSpace(name)]
	return definition, found
}

// describeTools renders tools/list. The annotations are hints an MCP client
// uses to decide what to show a human before it runs something; they repeat
// what the description says rather than replacing it.
func describeTools() []map[string]any {
	described := make([]map[string]any, 0, len(registry))
	for _, definition := range registry {
		described = append(described, map[string]any{
			"name":        definition.Name,
			"description": definition.Description(),
			"inputSchema": definition.inputSchema(),
			"annotations": map[string]any{
				"title":           definition.Name,
				"readOnlyHint":    !definition.Mutates,
				"destructiveHint": definition.Destructive,
			},
		})
	}
	return described
}
