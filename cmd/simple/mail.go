package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

	"connectrpc.com/connect"

	mailv1 "github.com/castlemilk/dns/gen/go/mail/v1"
	"github.com/castlemilk/dns/internal/simplecli/client"
)

func cmdMail(ctx context.Context, e *env, o *options, args []string) error {
	if len(args) == 0 {
		return &usageError{message: "mail: choose a command", usage: mailUsage}
	}
	switch args[0] {
	case "status":
		return cmdMailStatus(ctx, e, o, args[1:])
	case "list", "ls":
		return cmdMailList(ctx, e, o, args[1:])
	case "show", "get":
		return cmdMailShow(ctx, e, o, args[1:])
	case "bind":
		return cmdMailBind(ctx, e, o, args[1:])
	case "update":
		return cmdMailUpdate(ctx, e, o, args[1:])
	case "unbind":
		return cmdMailUnbind(ctx, e, o, args[1:])
	case "reapply-dns":
		return cmdMailReapplyDNS(ctx, e, o, args[1:])
	case "mailbox":
		return cmdMailbox(ctx, e, o, args[1:])
	case "forwarder":
		return cmdForwarder(ctx, e, o, args[1:])
	case "queue":
		return cmdMailQueue(ctx, e, o, args[1:])
	case "help", "-h", "--help":
		return &helpError{usage: mailUsage}
	default:
		return &usageError{message: fmt.Sprintf("unknown mail command %q", args[0]), usage: mailUsage}
	}
}

func cmdMailStatus(ctx context.Context, e *env, o *options, args []string) error {
	fs, err := o.parse("simple mail status", mailUsage, args, nil)
	if err != nil {
		return err
	}
	if err := noExtraArgs(fs, 0, mailUsage); err != nil {
		return err
	}
	clients, err := o.connect(e)
	if err != nil {
		return err
	}
	callCtx, cancel := o.call(ctx)
	defer cancel()
	response, err := clients.Mail.GetMailStatus(callCtx, connect.NewRequest(&mailv1.GetMailStatusRequest{}))
	if err != nil {
		return fail(clients, err)
	}
	status := response.Msg
	return newPrinter(e, o).emit(status, func(w io.Writer) error {
		// counts_available is the difference between "no mailboxes" and "the
		// engine could not be asked". Both are zeroes on the wire, so the
		// number is only shown when the server says it is real.
		mailboxes, used := "unknown (the mail server could not be asked)", "unknown"
		if status.GetCountsAvailable() {
			mailboxes = formatUint(status.GetMailboxes())
			used = formatBytes(status.GetUsedBytes())
		}
		if err := fields(w,
			pair("engine", engineDetail(status.GetEngine())),
			pair("hostname", orDash(status.GetMailHostname())),
			pair("edition", orDash(status.GetEdition())),
			pair("domains", formatUint(status.GetDomains())),
			pair("mailboxes", mailboxes),
			pair("used", used),
			pair("mailboxes per domain", formatUint(status.GetMailboxesPerDomain())),
			pair("delivery stats", deliveryNote(status)),
		); err != nil {
			return err
		}
		return writeList(w, "missing permissions", status.GetMissingPermissions())
	})
}

func deliveryNote(status *mailv1.GetMailStatusResponse) string {
	if !status.GetDeliveryStatsAvailable() {
		return orDash(status.GetDeliveryStatsNote())
	}
	return fmt.Sprintf("over %d hours, last event %s",
		status.GetDeliveryWindowHours(), formatTime(status.GetReceiverLastEventAt()))
}

func cmdMailList(ctx context.Context, e *env, o *options, args []string) error {
	fs, err := o.parse("simple mail list", mailUsage, args, nil)
	if err != nil {
		return err
	}
	if err := noExtraArgs(fs, 0, mailUsage); err != nil {
		return err
	}
	clients, err := o.connect(e)
	if err != nil {
		return err
	}
	callCtx, cancel := o.call(ctx)
	defer cancel()
	response, err := clients.Mail.ListMailDomains(callCtx, connect.NewRequest(&mailv1.ListMailDomainsRequest{}))
	if err != nil {
		return fail(clients, err)
	}
	domains := response.Msg
	return newPrinter(e, o).emit(domains, func(w io.Writer) error {
		if len(domains.GetDomains()) == 0 {
			return writeLine(w, "no domains are bound to mail; bind one with `simple mail bind DOMAIN`")
		}
		table := newTable(w, "DOMAIN", "STATE", "DNS", "MAILBOXES", "FORWARDERS", "DMARC", "REASON")
		for _, domain := range domains.GetDomains() {
			table.row(
				domain.GetZoneName(),
				label(domain.GetState().String(), "MAIL_DOMAIN_STATE_"),
				mailDNSSummary(domain),
				mailboxCount(domain),
				countOrUnknown(domain.GetCountsAvailable(), domain.GetForwarderCount()),
				orDash(domain.GetDmarcPolicy()),
				orDash(domain.GetReason()),
			)
		}
		return table.flush()
	})
}

func mailDNSSummary(domain *mailv1.MailDomain) string {
	if domain.GetZoneMissing() {
		return "zone missing"
	}
	if domain.GetRecordsInSync() {
		return "in sync"
	}
	return "out of sync"
}

// mailboxCount refuses to print a zero the control plane did not vouch for.
func mailboxCount(domain *mailv1.MailDomain) string {
	if !domain.GetCountsAvailable() {
		return "unknown"
	}
	if limit := domain.GetMailboxLimit(); limit > 0 {
		return fmt.Sprintf("%d/%d", domain.GetMailboxCount(), limit)
	}
	return formatUint(domain.GetMailboxCount())
}

func countOrUnknown(available bool, count uint32) string {
	if !available {
		return "unknown"
	}
	return formatUint(count)
}

func cmdMailShow(ctx context.Context, e *env, o *options, args []string) error {
	fs, err := o.parse("simple mail show", mailUsage, args, nil)
	if err != nil {
		return err
	}
	reference, err := argument(fs, 0, "domain", mailUsage)
	if err != nil {
		return err
	}
	if err := noExtraArgs(fs, 1, mailUsage); err != nil {
		return err
	}
	clients, err := o.connect(e)
	if err != nil {
		return err
	}
	zone, err := lookupZoneWithTimeout(ctx, o, clients, reference)
	if err != nil {
		return err
	}
	callCtx, cancel := o.call(ctx)
	defer cancel()
	response, err := clients.Mail.GetMailDomain(callCtx,
		connect.NewRequest(&mailv1.GetMailDomainRequest{ZoneId: zone.GetId()}))
	if err != nil {
		return fail(clients, err)
	}
	result := response.Msg
	return newPrinter(e, o).emit(result, func(w io.Writer) error {
		domain := result.GetDomain()
		if err := fields(w,
			pair("domain", orDash(domain.GetZoneName())),
			pair("state", label(domain.GetState().String(), "MAIL_DOMAIN_STATE_")),
			pair("reason", orDash(domain.GetReason())),
			pair("mail hostname", orDash(domain.GetMailHostname())),
			pair("dmarc policy", orDash(domain.GetDmarcPolicy())),
			pair("report address", orDash(domain.GetReportAddress())),
			pair("mailboxes", mailboxCount(domain)),
			pair("forwarders", countOrUnknown(domain.GetCountsAvailable(), domain.GetForwarderCount())),
			pair("postmaster", postmasterSummary(domain)),
			pair("dns", mailDNSSummary(domain)),
			pair("client autoconfig", yesNo(domain.GetPublishClientAutoconfig())),
			pair("queue", queueSummaryLine(result.GetQueue())),
		); err != nil {
			return err
		}
		if keys := domain.GetDkim(); len(keys) > 0 {
			if _, err := fmt.Fprintln(w, "\ndkim:"); err != nil {
				return err
			}
			table := newTable(w, "SELECTOR", "ALGORITHM", "STAGE", "PUBLISHED")
			for _, key := range keys {
				table.row(key.GetSelector(), key.GetAlgorithm(), key.GetStage(), yesNo(key.GetPublished()))
			}
			if err := table.flush(); err != nil {
				return err
			}
		}
		if records := domain.GetRecords(); len(records) > 0 {
			if _, err := fmt.Fprintln(w, "\nrecords:"); err != nil {
				return err
			}
			table := newTable(w, "NAME", "TYPE", "TTL", "PRESENT", "MATCHES", "VALUE")
			for _, record := range records {
				table.row(record.GetName(), record.GetType(), formatUint(record.GetTtl()),
					yesNo(record.GetPresent()), yesNo(record.GetMatches()), record.GetValue())
			}
			return table.flush()
		}
		return nil
	})
}

func postmasterSummary(domain *mailv1.MailDomain) string {
	if !domain.GetCountsAvailable() {
		return "unknown"
	}
	return yesNo(domain.GetHasPostmaster())
}

func queueSummaryLine(queue *mailv1.QueueSummary) string {
	if queue == nil || !queue.GetAvailable() {
		return "unknown"
	}
	return fmt.Sprintf("%d scheduled, %d failing, %d failed",
		queue.GetScheduled(), queue.GetFailing(), queue.GetFailed())
}

func cmdMailBind(ctx context.Context, e *env, o *options, args []string) error {
	var dmarcPolicy string
	var clientAutoconfig, replaceConflicting, dryRun bool
	fs, err := o.parse("simple mail bind", mailUsage, args, func(fs *flag.FlagSet) {
		fs.StringVar(&dmarcPolicy, "dmarc-policy", "", "none, quarantine or reject (default the platform's)")
		fs.BoolVar(&clientAutoconfig, "client-autoconfig", true, "publish the client autoconfiguration records")
		fs.BoolVar(&replaceConflicting, "replace-conflicting", false, "replace records that block the mail records")
		fs.BoolVar(&dryRun, "dry-run", false, "show the DNS plan and write nothing")
	})
	if err != nil {
		return err
	}
	reference, err := argument(fs, 0, "domain", mailUsage)
	if err != nil {
		return err
	}
	if err := noExtraArgs(fs, 1, mailUsage); err != nil {
		return err
	}
	clients, err := o.connect(e)
	if err != nil {
		return err
	}
	zone, err := lookupZoneWithTimeout(ctx, o, clients, reference)
	if err != nil {
		return err
	}
	request := &mailv1.BindMailDomainRequest{
		ZoneId:                    zone.GetId(),
		DmarcPolicy:               dmarcPolicy,
		ReplaceConflictingRecords: replaceConflicting,
		DryRun:                    dryRun,
	}
	// Absent means the platform's default, which is on; only an explicit flag
	// is sent, so this CLI never opts a binding out by accident.
	if provided(fs, "client-autoconfig") {
		request.PublishClientAutoconfig = &clientAutoconfig
	}
	callCtx, cancel := o.call(ctx)
	defer cancel()
	response, err := clients.Mail.BindMailDomain(callCtx, connect.NewRequest(request))
	if err != nil {
		return fail(clients, err)
	}
	result := response.Msg
	return newPrinter(e, o).emit(result, func(w io.Writer) error {
		if err := fields(w,
			pair("domain", zone.GetName()),
			pair("state", label(result.GetDomain().GetState().String(), "MAIL_DOMAIN_STATE_")),
			pair("dry run", yesNo(result.GetDryRun())),
		); err != nil {
			return err
		}
		if err := writeChanges(w, "dns plan", result.GetDnsPlan()); err != nil {
			return err
		}
		return writeChanges(w, "conflicts", result.GetConflicts())
	})
}

func cmdMailUpdate(ctx context.Context, e *env, o *options, args []string) error {
	var dmarcPolicy string
	var clientAutoconfig bool
	fs, err := o.parse("simple mail update", mailUsage, args, func(fs *flag.FlagSet) {
		fs.StringVar(&dmarcPolicy, "dmarc-policy", "", "none, quarantine or reject")
		fs.BoolVar(&clientAutoconfig, "client-autoconfig", true, "publish the client autoconfiguration records")
	})
	if err != nil {
		return err
	}
	reference, err := argument(fs, 0, "domain", mailUsage)
	if err != nil {
		return err
	}
	if err := noExtraArgs(fs, 1, mailUsage); err != nil {
		return err
	}
	if !provided(fs, "dmarc-policy") && !provided(fs, "client-autoconfig") {
		return usagef("mail update: give --dmarc-policy or --client-autoconfig")
	}
	clients, err := o.connect(e)
	if err != nil {
		return err
	}
	zone, err := lookupZoneWithTimeout(ctx, o, clients, reference)
	if err != nil {
		return err
	}
	request := &mailv1.UpdateMailDomainRequest{ZoneId: zone.GetId()}
	if provided(fs, "dmarc-policy") {
		request.DmarcPolicy = &dmarcPolicy
	}
	if provided(fs, "client-autoconfig") {
		request.PublishClientAutoconfig = &clientAutoconfig
	}
	callCtx, cancel := o.call(ctx)
	defer cancel()
	response, err := clients.Mail.UpdateMailDomain(callCtx, connect.NewRequest(request))
	if err != nil {
		return fail(clients, err)
	}
	result := response.Msg
	return newPrinter(e, o).emit(result, func(w io.Writer) error {
		if err := fields(w,
			pair("domain", zone.GetName()),
			pair("dmarc policy", orDash(result.GetDomain().GetDmarcPolicy())),
			pair("client autoconfig", yesNo(result.GetDomain().GetPublishClientAutoconfig())),
		); err != nil {
			return err
		}
		return writeChanges(w, "dns plan", result.GetDnsPlan())
	})
}

func cmdMailUnbind(ctx context.Context, e *env, o *options, args []string) error {
	var deleteMailboxes, keepDNS bool
	fs, err := o.parse("simple mail unbind", mailUsage, args, func(fs *flag.FlagSet) {
		fs.BoolVar(&deleteMailboxes, "delete-mailboxes", false, "delete the mailboxes and forwarders too")
		fs.BoolVar(&keepDNS, "keep-dns", false, "keep the mail records as ordinary user records")
	})
	if err != nil {
		return err
	}
	reference, err := argument(fs, 0, "domain", mailUsage)
	if err != nil {
		return err
	}
	if err := noExtraArgs(fs, 1, mailUsage); err != nil {
		return err
	}
	clients, err := o.connect(e)
	if err != nil {
		return err
	}
	zone, err := lookupZoneWithTimeout(ctx, o, clients, reference)
	if err != nil {
		return err
	}
	question := fmt.Sprintf("Unbind mail from %s? It stops receiving mail.", zone.GetName())
	if deleteMailboxes {
		question = fmt.Sprintf(
			"Delete every mailbox and forwarder on %s and unbind it? Stored mail is destroyed.", zone.GetName())
	}
	if err := confirm(e, o, question); err != nil {
		return err
	}
	request := &mailv1.UnbindMailDomainRequest{
		ZoneId:          zone.GetId(),
		DeleteMailboxes: deleteMailboxes,
		KeepDnsRecords:  keepDNS,
	}
	if deleteMailboxes {
		// The control plane asks for the zone name back as its own check that
		// the caller meant this domain and not the one above it in the list.
		request.ConfirmZoneName = zone.GetName()
	}
	callCtx, cancel := o.call(ctx)
	defer cancel()
	response, err := clients.Mail.UnbindMailDomain(callCtx, connect.NewRequest(request))
	if err != nil {
		return fail(clients, err)
	}
	result := response.Msg
	return newPrinter(e, o).emit(result, func(w io.Writer) error {
		return fields(w,
			pair("unbound", zone.GetName()),
			pair("mailboxes deleted", formatUint(result.GetMailboxesDeleted())),
			pair("forwarders deleted", formatUint(result.GetForwardersDeleted())),
		)
	})
}

func cmdMailReapplyDNS(ctx context.Context, e *env, o *options, args []string) error {
	var replaceConflicting, dryRun bool
	fs, err := o.parse("simple mail reapply-dns", mailUsage, args, func(fs *flag.FlagSet) {
		fs.BoolVar(&replaceConflicting, "replace-conflicting", false, "replace records that block the mail records")
		fs.BoolVar(&dryRun, "dry-run", false, "show the plan and write nothing")
	})
	if err != nil {
		return err
	}
	reference, err := argument(fs, 0, "domain", mailUsage)
	if err != nil {
		return err
	}
	if err := noExtraArgs(fs, 1, mailUsage); err != nil {
		return err
	}
	clients, err := o.connect(e)
	if err != nil {
		return err
	}
	zone, err := lookupZoneWithTimeout(ctx, o, clients, reference)
	if err != nil {
		return err
	}
	callCtx, cancel := o.call(ctx)
	defer cancel()
	response, err := clients.Mail.ReapplyMailDns(callCtx, connect.NewRequest(&mailv1.ReapplyMailDnsRequest{
		ZoneId: zone.GetId(), ReplaceConflictingRecords: replaceConflicting, DryRun: dryRun,
	}))
	if err != nil {
		return fail(clients, err)
	}
	result := response.Msg
	return newPrinter(e, o).emit(result, func(w io.Writer) error {
		if err := fields(w,
			pair("domain", zone.GetName()),
			pair("dry run", yesNo(result.GetDryRun())),
			pair("dns", mailDNSSummary(result.GetDomain())),
		); err != nil {
			return err
		}
		if err := writeChanges(w, "dns plan", result.GetDnsPlan()); err != nil {
			return err
		}
		return writeChanges(w, "conflicts", result.GetConflicts())
	})
}

func cmdMailQueue(ctx context.Context, e *env, o *options, args []string) error {
	fs, err := o.parse("simple mail queue", mailUsage, args, nil)
	if err != nil {
		return err
	}
	reference, err := argument(fs, 0, "domain", mailUsage)
	if err != nil {
		return err
	}
	if err := noExtraArgs(fs, 1, mailUsage); err != nil {
		return err
	}
	clients, err := o.connect(e)
	if err != nil {
		return err
	}
	zone, err := lookupZoneWithTimeout(ctx, o, clients, reference)
	if err != nil {
		return err
	}
	callCtx, cancel := o.call(ctx)
	defer cancel()
	response, err := clients.Mail.GetMailQueue(callCtx,
		connect.NewRequest(&mailv1.GetMailQueueRequest{ZoneId: zone.GetId()}))
	if err != nil {
		return fail(clients, err)
	}
	result := response.Msg
	return newPrinter(e, o).emit(result, func(w io.Writer) error {
		if err := fields(w,
			pair("domain", zone.GetName()),
			pair("queue", queueSummaryLine(result.GetSummary())),
			pair("observed", formatTime(result.GetSummary().GetObservedAt())),
		); err != nil {
			return err
		}
		if len(result.GetMessages()) == 0 {
			return writeLine(w, "\nno messages listed")
		}
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
		table := newTable(w, "ID", "FROM", "TO", "STATUS", "QUEUE", "DUE", "ATTEMPTS")
		for _, message := range result.GetMessages() {
			table.row(
				message.GetId(),
				orDash(message.GetFrom()),
				strings.Join(message.GetTo(), " "),
				message.GetStatus(),
				orDash(message.GetQueue()),
				formatTime(message.GetDueAt()),
				formatUint(message.GetAttempts()),
			)
		}
		return table.flush()
	})
}

func cmdMailbox(ctx context.Context, e *env, o *options, args []string) error {
	if len(args) == 0 {
		return &usageError{message: "mail mailbox: choose list, add, edit, reset or delete", usage: mailUsage}
	}
	switch args[0] {
	case "list", "ls":
		return cmdMailboxList(ctx, e, o, args[1:])
	case "add", "create":
		return cmdMailboxAdd(ctx, e, o, args[1:])
	case "edit", "update":
		return cmdMailboxEdit(ctx, e, o, args[1:])
	case "reset":
		return cmdMailboxReset(ctx, e, o, args[1:])
	case "delete", "rm":
		return cmdMailboxDelete(ctx, e, o, args[1:])
	default:
		return &usageError{message: fmt.Sprintf("unknown mailbox command %q", args[0]), usage: mailUsage}
	}
}

func cmdMailboxList(ctx context.Context, e *env, o *options, args []string) error {
	fs, err := o.parse("simple mail mailbox list", mailUsage, args, nil)
	if err != nil {
		return err
	}
	reference, err := argument(fs, 0, "domain", mailUsage)
	if err != nil {
		return err
	}
	if err := noExtraArgs(fs, 1, mailUsage); err != nil {
		return err
	}
	clients, err := o.connect(e)
	if err != nil {
		return err
	}
	zone, err := lookupZoneWithTimeout(ctx, o, clients, reference)
	if err != nil {
		return err
	}
	callCtx, cancel := o.call(ctx)
	defer cancel()
	response, err := clients.Mail.ListMailboxes(callCtx,
		connect.NewRequest(&mailv1.ListMailboxesRequest{ZoneId: zone.GetId()}))
	if err != nil {
		return fail(clients, err)
	}
	result := response.Msg
	return newPrinter(e, o).emit(result, func(w io.Writer) error {
		if len(result.GetMailboxes()) == 0 {
			return writeLine(w, "no mailboxes")
		}
		table := newTable(w, "ADDRESS", "MAILBOX ID", "DISPLAY NAME", "USED", "QUOTA", "ALIASES")
		for _, mailbox := range result.GetMailboxes() {
			quota := "unlimited"
			if mailbox.GetQuotaBytes() > 0 {
				quota = formatBytes(mailbox.GetQuotaBytes())
			}
			table.row(
				mailbox.GetAddress(),
				mailbox.GetId(),
				orDash(mailbox.GetDisplayName()),
				formatBytes(mailbox.GetUsedBytes()),
				quota,
				orDash(strings.Join(mailbox.GetAliases(), " ")),
			)
		}
		return table.flush()
	})
}

func cmdMailboxAdd(ctx context.Context, e *env, o *options, args []string) error {
	var displayName string
	var quotaBytes uint64
	fs, err := o.parse("simple mail mailbox add", mailUsage, args, func(fs *flag.FlagSet) {
		fs.StringVar(&displayName, "display-name", "", "the human's name, up to 80 characters")
		fs.Uint64Var(&quotaBytes, "quota-bytes", 0, "mailbox quota in bytes (0 = the platform default)")
	})
	if err != nil {
		return err
	}
	reference, err := argument(fs, 0, "domain", mailUsage)
	if err != nil {
		return err
	}
	localPart, err := argument(fs, 1, "local_part", mailUsage)
	if err != nil {
		return err
	}
	if err := noExtraArgs(fs, 2, mailUsage); err != nil {
		return err
	}
	clients, err := o.connect(e)
	if err != nil {
		return err
	}
	zone, err := lookupZoneWithTimeout(ctx, o, clients, reference)
	if err != nil {
		return err
	}
	callCtx, cancel := o.call(ctx)
	defer cancel()
	response, err := clients.Mail.CreateMailbox(callCtx, connect.NewRequest(&mailv1.CreateMailboxRequest{
		ZoneId: zone.GetId(), LocalPart: localPart, DisplayName: displayName, QuotaBytes: quotaBytes,
	}))
	if err != nil {
		return fail(clients, err)
	}
	return emitNewPassword(e, o, response.Msg)
}

// emitNewPassword prints a generated password exactly once, because the
// platform does not keep it: there is nowhere to fetch it from later. The
// warning goes to stderr so that a piped JSON document stays a document.
func emitNewPassword(e *env, o *options, result *mailv1.CreateMailboxResponse) error {
	printer := newPrinter(e, o)
	if err := printer.emit(result, func(w io.Writer) error {
		return fields(w,
			pair("mailbox", result.GetMailbox().GetAddress()),
			pair("mailbox id", result.GetMailbox().GetId()),
			pair("password", result.GetPassword()),
			pair("imap", fmt.Sprintf("%s:%d", result.GetImapHost(), result.GetImapPort())),
			pair("smtp", fmt.Sprintf("%s:%d", result.GetSmtpHost(), result.GetSmtpPort())),
		)
	}); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(e.stderr,
		"warning: this password is shown once and is not stored anywhere; put it in a password manager now."); err != nil {
		return err
	}
	return nil
}

func cmdMailboxEdit(ctx context.Context, e *env, o *options, args []string) error {
	var displayName string
	var quotaBytes uint64
	fs, err := o.parse("simple mail mailbox edit", mailUsage, args, func(fs *flag.FlagSet) {
		fs.StringVar(&displayName, "display-name", "", "the human's name")
		fs.Uint64Var(&quotaBytes, "quota-bytes", 0, "mailbox quota in bytes")
	})
	if err != nil {
		return err
	}
	reference, err := argument(fs, 0, "domain", mailUsage)
	if err != nil {
		return err
	}
	mailboxRef, err := argument(fs, 1, "mailbox", mailUsage)
	if err != nil {
		return err
	}
	if err := noExtraArgs(fs, 2, mailUsage); err != nil {
		return err
	}
	if !provided(fs, "display-name") && !provided(fs, "quota-bytes") {
		return usagef("mailbox edit: give --display-name or --quota-bytes")
	}
	clients, err := o.connect(e)
	if err != nil {
		return err
	}
	zoneID, mailbox, err := lookupMailbox(ctx, o, clients, reference, mailboxRef)
	if err != nil {
		return err
	}
	request := &mailv1.UpdateMailboxRequest{ZoneId: zoneID, MailboxId: mailbox.GetId()}
	if provided(fs, "display-name") {
		request.DisplayName = &displayName
	}
	if provided(fs, "quota-bytes") {
		request.QuotaBytes = &quotaBytes
	}
	callCtx, cancel := o.call(ctx)
	defer cancel()
	response, err := clients.Mail.UpdateMailbox(callCtx, connect.NewRequest(request))
	if err != nil {
		return fail(clients, err)
	}
	return newPrinter(e, o).emit(response.Msg, func(w io.Writer) error {
		return fields(w,
			pair("mailbox", response.Msg.GetMailbox().GetAddress()),
			pair("display name", orDash(response.Msg.GetMailbox().GetDisplayName())),
			pair("quota", formatBytes(response.Msg.GetMailbox().GetQuotaBytes())),
		)
	})
}

func cmdMailboxReset(ctx context.Context, e *env, o *options, args []string) error {
	fs, err := o.parse("simple mail mailbox reset", mailUsage, args, nil)
	if err != nil {
		return err
	}
	reference, err := argument(fs, 0, "domain", mailUsage)
	if err != nil {
		return err
	}
	mailboxRef, err := argument(fs, 1, "mailbox", mailUsage)
	if err != nil {
		return err
	}
	if err := noExtraArgs(fs, 2, mailUsage); err != nil {
		return err
	}
	clients, err := o.connect(e)
	if err != nil {
		return err
	}
	zoneID, mailbox, err := lookupMailbox(ctx, o, clients, reference, mailboxRef)
	if err != nil {
		return err
	}
	if err := confirm(e, o, fmt.Sprintf(
		"Reset the password for %s? Every client signed in with the old one stops working.",
		mailbox.GetAddress())); err != nil {
		return err
	}
	callCtx, cancel := o.call(ctx)
	defer cancel()
	response, err := clients.Mail.ResetMailboxPassword(callCtx,
		connect.NewRequest(&mailv1.ResetMailboxPasswordRequest{ZoneId: zoneID, MailboxId: mailbox.GetId()}))
	if err != nil {
		return fail(clients, err)
	}
	document := struct {
		Address  string `json:"address"`
		Password string `json:"password"`
	}{Address: mailbox.GetAddress(), Password: response.Msg.GetPassword()}
	printer := newPrinter(e, o)
	if err := printer.emit(document, func(w io.Writer) error {
		return fields(w, pair("mailbox", document.Address), pair("password", document.Password))
	}); err != nil {
		return err
	}
	_, err = fmt.Fprintln(e.stderr,
		"warning: this password is shown once and is not stored anywhere; put it in a password manager now.")
	return err
}

func cmdMailboxDelete(ctx context.Context, e *env, o *options, args []string) error {
	fs, err := o.parse("simple mail mailbox delete", mailUsage, args, nil)
	if err != nil {
		return err
	}
	reference, err := argument(fs, 0, "domain", mailUsage)
	if err != nil {
		return err
	}
	mailboxRef, err := argument(fs, 1, "mailbox", mailUsage)
	if err != nil {
		return err
	}
	if err := noExtraArgs(fs, 2, mailUsage); err != nil {
		return err
	}
	clients, err := o.connect(e)
	if err != nil {
		return err
	}
	zoneID, mailbox, err := lookupMailbox(ctx, o, clients, reference, mailboxRef)
	if err != nil {
		return err
	}
	if err := confirm(e, o, fmt.Sprintf(
		"Delete %s and everything in it? Stored mail cannot be recovered.", mailbox.GetAddress())); err != nil {
		return err
	}
	callCtx, cancel := o.call(ctx)
	defer cancel()
	if _, err := clients.Mail.DeleteMailbox(callCtx, connect.NewRequest(&mailv1.DeleteMailboxRequest{
		// The control plane wants the address back as its own proof that the
		// caller meant this mailbox.
		ZoneId: zoneID, MailboxId: mailbox.GetId(), ConfirmAddress: mailbox.GetAddress(),
	})); err != nil {
		return fail(clients, err)
	}
	document := struct {
		Deleted   bool   `json:"deleted"`
		Address   string `json:"address"`
		MailboxID string `json:"mailbox_id"`
	}{Deleted: true, Address: mailbox.GetAddress(), MailboxID: mailbox.GetId()}
	return newPrinter(e, o).emit(document, func(w io.Writer) error {
		return writeLine(w, "deleted "+document.Address)
	})
}

func cmdForwarder(ctx context.Context, e *env, o *options, args []string) error {
	if len(args) == 0 {
		return &usageError{message: "mail forwarder: choose list, add or delete", usage: mailUsage}
	}
	switch args[0] {
	case "list", "ls":
		return cmdForwarderList(ctx, e, o, args[1:])
	case "add", "create":
		return cmdForwarderAdd(ctx, e, o, args[1:])
	case "delete", "rm":
		return cmdForwarderDelete(ctx, e, o, args[1:])
	default:
		return &usageError{message: fmt.Sprintf("unknown forwarder command %q", args[0]), usage: mailUsage}
	}
}

func cmdForwarderList(ctx context.Context, e *env, o *options, args []string) error {
	fs, err := o.parse("simple mail forwarder list", mailUsage, args, nil)
	if err != nil {
		return err
	}
	reference, err := argument(fs, 0, "domain", mailUsage)
	if err != nil {
		return err
	}
	if err := noExtraArgs(fs, 1, mailUsage); err != nil {
		return err
	}
	clients, err := o.connect(e)
	if err != nil {
		return err
	}
	zone, err := lookupZoneWithTimeout(ctx, o, clients, reference)
	if err != nil {
		return err
	}
	callCtx, cancel := o.call(ctx)
	defer cancel()
	response, err := clients.Mail.ListForwarders(callCtx,
		connect.NewRequest(&mailv1.ListForwardersRequest{ZoneId: zone.GetId()}))
	if err != nil {
		return fail(clients, err)
	}
	result := response.Msg
	return newPrinter(e, o).emit(result, func(w io.Writer) error {
		if len(result.GetForwarders()) == 0 {
			return writeLine(w, "no forwarders")
		}
		table := newTable(w, "ADDRESS", "FORWARDER ID", "KIND", "EXTERNAL", "TARGETS")
		for _, forwarder := range result.GetForwarders() {
			table.row(
				forwarder.GetAddress(),
				forwarder.GetId(),
				label(forwarder.GetKind().String(), "FORWARDER_KIND_"),
				yesNo(forwarder.GetExternal()),
				strings.Join(forwarder.GetTargets(), " "),
			)
		}
		return table.flush()
	})
}

func cmdForwarderAdd(ctx context.Context, e *env, o *options, args []string) error {
	var targets stringList
	fs, err := o.parse("simple mail forwarder add", mailUsage, args, func(fs *flag.FlagSet) {
		fs.Var(&targets, "target", "an address to deliver to; repeat for more than one")
	})
	if err != nil {
		return err
	}
	reference, err := argument(fs, 0, "domain", mailUsage)
	if err != nil {
		return err
	}
	localPart, err := argument(fs, 1, "local_part", mailUsage)
	if err != nil {
		return err
	}
	if err := noExtraArgs(fs, 2, mailUsage); err != nil {
		return err
	}
	if len(targets) == 0 {
		return usagef("--target: at least one is required")
	}
	clients, err := o.connect(e)
	if err != nil {
		return err
	}
	zone, err := lookupZoneWithTimeout(ctx, o, clients, reference)
	if err != nil {
		return err
	}
	callCtx, cancel := o.call(ctx)
	defer cancel()
	response, err := clients.Mail.CreateForwarder(callCtx, connect.NewRequest(&mailv1.CreateForwarderRequest{
		ZoneId: zone.GetId(), LocalPart: localPart, Targets: targets,
	}))
	if err != nil {
		return fail(clients, err)
	}
	forwarder := response.Msg.GetForwarder()
	return newPrinter(e, o).emit(response.Msg, func(w io.Writer) error {
		return fields(w,
			pair("forwarder", forwarder.GetAddress()),
			pair("forwarder id", forwarder.GetId()),
			pair("targets", strings.Join(forwarder.GetTargets(), " ")),
			pair("external", yesNo(forwarder.GetExternal())),
		)
	})
}

func cmdForwarderDelete(ctx context.Context, e *env, o *options, args []string) error {
	fs, err := o.parse("simple mail forwarder delete", mailUsage, args, nil)
	if err != nil {
		return err
	}
	reference, err := argument(fs, 0, "domain", mailUsage)
	if err != nil {
		return err
	}
	forwarderID, err := argument(fs, 1, "forwarder_id", mailUsage)
	if err != nil {
		return err
	}
	if err := noExtraArgs(fs, 2, mailUsage); err != nil {
		return err
	}
	clients, err := o.connect(e)
	if err != nil {
		return err
	}
	zone, err := lookupZoneWithTimeout(ctx, o, clients, reference)
	if err != nil {
		return err
	}
	if err := confirm(e, o, fmt.Sprintf("Delete forwarder %s from %s?",
		forwarderID, zone.GetName())); err != nil {
		return err
	}
	callCtx, cancel := o.call(ctx)
	defer cancel()
	if _, err := clients.Mail.DeleteForwarder(callCtx, connect.NewRequest(&mailv1.DeleteForwarderRequest{
		ZoneId: zone.GetId(), ForwarderId: forwarderID,
	})); err != nil {
		return fail(clients, err)
	}
	document := struct {
		Deleted     bool   `json:"deleted"`
		ZoneID      string `json:"zone_id"`
		ForwarderID string `json:"forwarder_id"`
	}{Deleted: true, ZoneID: zone.GetId(), ForwarderID: forwarderID}
	return newPrinter(e, o).emit(document, func(w io.Writer) error {
		return writeLine(w, "deleted forwarder "+forwarderID)
	})
}

// lookupMailbox accepts an address, a local part or a mailbox id, so an
// operator never has to copy an opaque identifier out of a list first.
func lookupMailbox(
	ctx context.Context, o *options, clients *client.Clients, zoneRef, mailboxRef string,
) (string, *mailv1.Mailbox, error) {
	zone, err := lookupZoneWithTimeout(ctx, o, clients, zoneRef)
	if err != nil {
		return "", nil, err
	}
	callCtx, cancel := o.call(ctx)
	defer cancel()
	response, err := clients.Mail.ListMailboxes(callCtx,
		connect.NewRequest(&mailv1.ListMailboxesRequest{ZoneId: zone.GetId()}))
	if err != nil {
		return "", nil, fail(clients, err)
	}
	wanted := strings.ToLower(strings.TrimSpace(mailboxRef))
	for _, mailbox := range response.Msg.GetMailboxes() {
		switch {
		case mailbox.GetId() == mailboxRef,
			strings.EqualFold(mailbox.GetAddress(), wanted),
			strings.EqualFold(mailbox.GetLocalPart(), wanted):
			return zone.GetId(), mailbox, nil
		}
	}
	return "", nil, fmt.Errorf("mailbox: %q is not a mailbox on %s", mailboxRef, zone.GetName())
}
