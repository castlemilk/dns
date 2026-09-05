package simplemcp

import (
	"context"
	"strings"

	"connectrpc.com/connect"

	mailv1 "github.com/castlemilk/dns/gen/go/mail/v1"
)

// passwordNote travels with every generated password. It is the instruction the
// caller must follow, in the answer itself and not only in the tool
// description, because that is what a model actually reads back.
const passwordNote = "Shown once. Give this password to the person who owns the mailbox through a channel they already trust, tell them to change it, and keep no copy: do not write it to a file, a note, a ticket, a commit or a summary of this conversation. Neither this server nor the platform can show it again; if it is lost, generate a new one with simple_mail_mailbox_reset."

func (s *Server) mailList(ctx context.Context, _ args) (any, error) {
	response, err := s.clients.Mail.ListMailDomains(ctx, connect.NewRequest(&mailv1.ListMailDomainsRequest{}))
	if err != nil {
		return nil, err
	}
	return view(response.Msg)
}

func (s *Server) mailShow(ctx context.Context, a args) (any, error) {
	zone, err := s.resolveZone(ctx, a)
	if err != nil {
		return nil, err
	}
	response, err := s.clients.Mail.GetMailDomain(ctx, connect.NewRequest(&mailv1.GetMailDomainRequest{ZoneId: zone.ID}))
	if err != nil {
		return nil, err
	}
	return view(response.Msg)
}

func (s *Server) mailBind(ctx context.Context, a args) (any, error) {
	zone, err := s.resolveZone(ctx, a)
	if err != nil {
		return nil, err
	}
	policy, err := stringArg(a, "dmarc_policy", false)
	if err != nil {
		return nil, err
	}
	autoconfig, err := optionalBoolArg(a, "publish_client_autoconfig")
	if err != nil {
		return nil, err
	}
	replace, err := boolArg(a, "replace_conflicting_records", false)
	if err != nil {
		return nil, err
	}
	dryRun, err := boolArg(a, "dry_run", false)
	if err != nil {
		return nil, err
	}
	response, err := s.clients.Mail.BindMailDomain(ctx, connect.NewRequest(&mailv1.BindMailDomainRequest{
		ZoneId:                    zone.ID,
		DmarcPolicy:               policy,
		PublishClientAutoconfig:   autoconfig,
		ReplaceConflictingRecords: replace,
		DryRun:                    dryRun,
	}))
	if err != nil {
		return nil, err
	}
	return view(response.Msg)
}

// mailUnbind forwards the domain name the caller typed as the control plane's
// confirmation. It is never filled in from the lookup: the guard exists so that
// deleting mailboxes requires someone to have written the name, and deriving it
// here would quietly remove that.
func (s *Server) mailUnbind(ctx context.Context, a args) (any, error) {
	zone, err := s.resolveZone(ctx, a)
	if err != nil {
		return nil, err
	}
	deleteMailboxes, err := boolArg(a, "delete_mailboxes", false)
	if err != nil {
		return nil, err
	}
	keepDNS, err := boolArg(a, "keep_dns_records", false)
	if err != nil {
		return nil, err
	}
	typed, err := stringArg(a, "domain", false)
	if err != nil {
		return nil, err
	}
	confirmName := ""
	if deleteMailboxes {
		if typed == "" {
			return nil, invalid("domain", "required when delete_mailboxes is true: the control plane takes the domain name itself as confirmation, so it cannot be filled in from zone_id")
		}
		confirmName = zone.Name
		if confirmName == "" {
			confirmName = typed
		}
	}
	response, err := s.clients.Mail.UnbindMailDomain(ctx, connect.NewRequest(&mailv1.UnbindMailDomainRequest{
		ZoneId: zone.ID, DeleteMailboxes: deleteMailboxes, KeepDnsRecords: keepDNS, ConfirmZoneName: confirmName,
	}))
	if err != nil {
		return nil, err
	}
	return viewWith(response.Msg, map[string]any{"unbound": true, "zone_id": zone.ID, "domain": zone.Name})
}

func (s *Server) mailboxList(ctx context.Context, a args) (any, error) {
	zone, err := s.resolveZone(ctx, a)
	if err != nil {
		return nil, err
	}
	response, err := s.clients.Mail.ListMailboxes(ctx, connect.NewRequest(&mailv1.ListMailboxesRequest{ZoneId: zone.ID}))
	if err != nil {
		return nil, err
	}
	return view(response.Msg)
}

func (s *Server) mailboxAdd(ctx context.Context, a args) (any, error) {
	zone, err := s.resolveZone(ctx, a)
	if err != nil {
		return nil, err
	}
	localPart, err := stringArg(a, "local_part", true)
	if err != nil {
		return nil, err
	}
	displayName, err := stringArg(a, "display_name", false)
	if err != nil {
		return nil, err
	}
	quota, err := uint64Arg(a, "quota_bytes")
	if err != nil {
		return nil, err
	}
	response, err := s.clients.Mail.CreateMailbox(ctx, connect.NewRequest(&mailv1.CreateMailboxRequest{
		ZoneId: zone.ID, LocalPart: localPart, DisplayName: displayName, QuotaBytes: quota,
	}))
	if err != nil {
		return nil, err
	}
	return viewWith(response.Msg, map[string]any{"password_note": passwordNote})
}

func (s *Server) mailboxReset(ctx context.Context, a args) (any, error) {
	zone, err := s.resolveZone(ctx, a)
	if err != nil {
		return nil, err
	}
	mailbox, err := s.findMailbox(ctx, zone, a, false)
	if err != nil {
		return nil, err
	}
	response, err := s.clients.Mail.ResetMailboxPassword(ctx, connect.NewRequest(&mailv1.ResetMailboxPasswordRequest{
		ZoneId: zone.ID, MailboxId: mailbox.GetId(),
	}))
	if err != nil {
		return nil, err
	}
	return viewWith(response.Msg, map[string]any{
		"mailbox_id":    mailbox.GetId(),
		"address":       mailbox.GetAddress(),
		"password_note": passwordNote,
	})
}

func (s *Server) mailboxDelete(ctx context.Context, a args) (any, error) {
	zone, err := s.resolveZone(ctx, a)
	if err != nil {
		return nil, err
	}
	mailbox, err := s.findMailbox(ctx, zone, a, true)
	if err != nil {
		return nil, err
	}
	if _, err := s.clients.Mail.DeleteMailbox(ctx, connect.NewRequest(&mailv1.DeleteMailboxRequest{
		ZoneId: zone.ID, MailboxId: mailbox.GetId(), ConfirmAddress: mailbox.GetAddress(),
	})); err != nil {
		return nil, err
	}
	return map[string]any{"deleted": true, "mailbox_id": mailbox.GetId(), "address": mailbox.GetAddress()}, nil
}

// findMailbox identifies one mailbox from `address` or `mailbox_id`. When
// addressRequired is set the caller must have written the address in full,
// because that address is what the control plane demands as confirmation
// before it deletes anything.
func (s *Server) findMailbox(ctx context.Context, zone zoneRef, a args, addressRequired bool) (*mailv1.Mailbox, error) {
	address, err := stringArg(a, "address", addressRequired)
	if err != nil {
		return nil, err
	}
	id, err := stringArg(a, "mailbox_id", false)
	if err != nil {
		return nil, err
	}
	if address == "" && id == "" {
		return nil, invalid("address", "required (or mailbox_id)")
	}
	response, err := s.clients.Mail.ListMailboxes(ctx, connect.NewRequest(&mailv1.ListMailboxesRequest{ZoneId: zone.ID}))
	if err != nil {
		return nil, err
	}
	for _, mailbox := range response.Msg.GetMailboxes() {
		if id != "" && mailbox.GetId() == id {
			return mailbox, nil
		}
		if id == "" && strings.EqualFold(mailbox.GetAddress(), address) {
			return mailbox, nil
		}
	}
	if id != "" {
		return nil, notFound("mailbox_id: this domain has no mailbox with that id; call simple_mail_mailbox_list to see the ones it has")
	}
	return nil, notFound("address: " + address + " is not a mailbox on this domain; call simple_mail_mailbox_list to see the ones it has")
}

func (s *Server) forwarderList(ctx context.Context, a args) (any, error) {
	zone, err := s.resolveZone(ctx, a)
	if err != nil {
		return nil, err
	}
	response, err := s.clients.Mail.ListForwarders(ctx, connect.NewRequest(&mailv1.ListForwardersRequest{ZoneId: zone.ID}))
	if err != nil {
		return nil, err
	}
	return view(response.Msg)
}

func (s *Server) forwarderAdd(ctx context.Context, a args) (any, error) {
	zone, err := s.resolveZone(ctx, a)
	if err != nil {
		return nil, err
	}
	localPart, err := stringArg(a, "local_part", true)
	if err != nil {
		return nil, err
	}
	targets, err := stringsArg(a, "targets", true)
	if err != nil {
		return nil, err
	}
	response, err := s.clients.Mail.CreateForwarder(ctx, connect.NewRequest(&mailv1.CreateForwarderRequest{
		ZoneId: zone.ID, LocalPart: localPart, Targets: targets,
	}))
	if err != nil {
		return nil, err
	}
	return view(response.Msg)
}

func (s *Server) forwarderDelete(ctx context.Context, a args) (any, error) {
	zone, err := s.resolveZone(ctx, a)
	if err != nil {
		return nil, err
	}
	forwarderID, err := stringArg(a, "forwarder_id", true)
	if err != nil {
		return nil, err
	}
	if _, err := s.clients.Mail.DeleteForwarder(ctx, connect.NewRequest(&mailv1.DeleteForwarderRequest{
		ZoneId: zone.ID, ForwarderId: forwarderID,
	})); err != nil {
		return nil, err
	}
	return map[string]any{"deleted": true, "forwarder_id": forwarderID, "zone_id": zone.ID, "domain": zone.Name}, nil
}

func (s *Server) mailQueue(ctx context.Context, a args) (any, error) {
	zone, err := s.resolveZone(ctx, a)
	if err != nil {
		return nil, err
	}
	response, err := s.clients.Mail.GetMailQueue(ctx, connect.NewRequest(&mailv1.GetMailQueueRequest{ZoneId: zone.ID}))
	if err != nil {
		return nil, err
	}
	return view(response.Msg)
}
