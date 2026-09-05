package mail

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"connectrpc.com/connect"
	mailv1 "github.com/castlemilk/dns/gen/go/mail/v1"
	"github.com/castlemilk/dns/internal/activity"
	"github.com/castlemilk/dns/internal/mail/stalwart"
	"github.com/castlemilk/dns/internal/platform"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Client connection facts the console shows once, next to the generated
// credential. Stalwart's default listeners are implicit-TLS only (mail-facts
// §1): 993 IMAPS and 465 submissions, with no 143 or 587.
const (
	imapPort = 993
	smtpPort = 465
)

// Quota bounds. The lower bound keeps a mailbox usable; the upper one keeps a
// typo from handing out a terabyte.
const (
	minQuotaBytes = uint64(64 << 20)
	maxQuotaBytes = uint64(1 << 40)
)

// localPartPattern is the address rule: lowercase, starts and ends
// alphanumeric, and allows the separators every mail client accepts.
var localPartPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._+-]{0,62}[a-z0-9])?$`)

// maxDisplayNameRunes bounds the display name.
const maxDisplayNameRunes = 80

// ListMailboxes returns the zone's mailboxes with their live usage.
func (s *Service) ListMailboxes(
	ctx context.Context,
	request *connect.Request[mailv1.ListMailboxesRequest],
) (*connect.Response[mailv1.ListMailboxesResponse], error) {
	if !s.cfg.Configured() || s.engine == nil {
		return connect.NewResponse(&mailv1.ListMailboxesResponse{}), nil
	}
	doc, err := s.mailDomain(ctx, request.Msg.GetZoneId())
	if err != nil {
		return nil, err
	}
	accounts, err := observeEngineArg(ctx, s, "list_accounts", doc.EngineDomainID, s.engine.ListAccounts)
	if err != nil {
		return nil, engineError(err)
	}
	response := &mailv1.ListMailboxesResponse{
		Mailboxes: make([]*mailv1.Mailbox, 0, len(accounts)),
		Limit:     uint32(max(s.cfg.MailboxesPerDomain, 0)),
	}
	for _, account := range accounts {
		response.Mailboxes = append(response.Mailboxes, mailboxProto(doc, account))
	}
	return connect.NewResponse(response), nil
}

// CreateMailbox creates one mailbox and returns its generated credential once.
//
// The credential is built here, handed to the engine, put into this response
// and dropped. It is never persisted, never logged and never recorded as an
// activity detail — the event below carries the address and nothing else.
func (s *Service) CreateMailbox(
	ctx context.Context,
	request *connect.Request[mailv1.CreateMailboxRequest],
) (*connect.Response[mailv1.CreateMailboxResponse], error) {
	if err := s.requireEngine(); err != nil {
		return nil, err
	}
	message := request.Msg
	doc, err := s.mailDomain(ctx, message.GetZoneId())
	if err != nil {
		return nil, err
	}
	local, err := normalizeLocalPart(message.GetLocalPart())
	if err != nil {
		return nil, err
	}
	displayName, err := normalizeDisplayName(message.GetDisplayName())
	if err != nil {
		return nil, err
	}
	quota, err := s.normalizeQuota(message.GetQuotaBytes())
	if err != nil {
		return nil, err
	}

	accounts, lists, err := s.addressBook(ctx, doc)
	if err != nil {
		return nil, err
	}
	address := local + "@" + doc.ZoneName
	if addressTaken(accounts, lists, local) {
		return nil, duplicateAddressError(address)
	}
	if len(accounts) >= s.cfg.MailboxesPerDomain {
		return nil, connect.NewError(connect.CodeResourceExhausted,
			copyError(fmt.Sprintf("this plan includes %d mailboxes per domain", s.cfg.MailboxesPerDomain)))
	}

	generated, err := newPassphrase()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, copyError(msgInternal))
	}
	account, err := observeEngineArg(ctx, s, "create_account", stalwart.AccountInput{
		LocalPart:   local,
		DomainID:    doc.EngineDomainID,
		Description: displayName,
		Secret:      generated,
		QuotaBytes:  quota,
	}, s.engine.CreateAccount)
	if err != nil {
		return nil, engineErrorFor(address, err)
	}

	s.deps.Record(ctx, activity.Event{
		ZoneID:   doc.ZoneID,
		ZoneName: doc.ZoneName,
		Actor:    activity.ActorMailEngine,
		Kind:     activity.KindMailboxCreated,
		Severity: activity.SeverityInfo,
		Summary:  "Created mailbox " + account.EmailAddress,
		Details:  map[string]string{"mailbox.address": account.EmailAddress},
	})

	// The response is assembled after the engine call, so the generated
	// credential reaches exactly one place: this message.
	return connect.NewResponse(&mailv1.CreateMailboxResponse{
		Mailbox:  mailboxProto(doc, account),
		Password: generated,
		ImapHost: s.cfg.Hostname,
		ImapPort: imapPort,
		SmtpHost: s.cfg.Hostname,
		SmtpPort: smtpPort,
	}), nil
}

// UpdateMailbox changes the display name or the quota.
func (s *Service) UpdateMailbox(
	ctx context.Context,
	request *connect.Request[mailv1.UpdateMailboxRequest],
) (*connect.Response[mailv1.UpdateMailboxResponse], error) {
	if err := s.requireEngine(); err != nil {
		return nil, err
	}
	message := request.Msg
	doc, err := s.mailDomain(ctx, message.GetZoneId())
	if err != nil {
		return nil, err
	}
	account, err := s.mailbox(ctx, doc, message.GetMailboxId())
	if err != nil {
		return nil, err
	}

	patch := stalwart.AccountPatch{}
	if message.DisplayName != nil {
		displayName, err := normalizeDisplayName(message.GetDisplayName())
		if err != nil {
			return nil, err
		}
		patch.Description = &displayName
	}
	if message.QuotaBytes != nil {
		quota, err := s.normalizeQuota(message.GetQuotaBytes())
		if err != nil {
			return nil, err
		}
		patch.QuotaBytes = &quota
	}
	if patch.Description == nil && patch.QuotaBytes == nil {
		return connect.NewResponse(&mailv1.UpdateMailboxResponse{Mailbox: mailboxProto(doc, account)}), nil
	}
	if err := observeEngineDo(ctx, s, "update_account", func(ctx context.Context) error {
		return s.engine.UpdateAccount(ctx, account.ID, patch)
	}); err != nil {
		return nil, engineError(err)
	}
	updated, err := s.mailbox(ctx, doc, account.ID)
	if err != nil {
		return nil, err
	}
	s.deps.Record(ctx, activity.Event{
		ZoneID:   doc.ZoneID,
		ZoneName: doc.ZoneName,
		Actor:    activity.ActorMailEngine,
		Kind:     activity.KindMailboxUpdated,
		Severity: activity.SeverityInfo,
		Summary:  "Updated mailbox " + updated.EmailAddress,
		Details:  map[string]string{"mailbox.address": updated.EmailAddress},
	})
	return connect.NewResponse(&mailv1.UpdateMailboxResponse{Mailbox: mailboxProto(doc, updated)}), nil
}

// ResetMailboxPassword issues a new credential and returns it once. Existing
// sessions and app credentials are signed out by the server on the change.
func (s *Service) ResetMailboxPassword(
	ctx context.Context,
	request *connect.Request[mailv1.ResetMailboxPasswordRequest],
) (*connect.Response[mailv1.ResetMailboxPasswordResponse], error) {
	if err := s.requireEngine(); err != nil {
		return nil, err
	}
	doc, err := s.mailDomain(ctx, request.Msg.GetZoneId())
	if err != nil {
		return nil, err
	}
	account, err := s.mailbox(ctx, doc, request.Msg.GetMailboxId())
	if err != nil {
		return nil, err
	}
	generated, err := newPassphrase()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, copyError(msgInternal))
	}
	if err := observeEngineDo(ctx, s, "update_account", func(ctx context.Context) error {
		return s.engine.UpdateAccount(ctx, account.ID, stalwart.AccountPatch{Secret: &generated})
	}); err != nil {
		return nil, engineError(err)
	}
	s.deps.Record(ctx, activity.Event{
		ZoneID:   doc.ZoneID,
		ZoneName: doc.ZoneName,
		Actor:    activity.ActorMailEngine,
		Kind:     activity.KindMailboxPasswordReset,
		Severity: activity.SeverityWarn,
		Summary:  "Reset the credential for " + account.EmailAddress,
		Details:  map[string]string{"mailbox.address": account.EmailAddress},
	})
	return connect.NewResponse(&mailv1.ResetMailboxPasswordResponse{Password: generated}), nil
}

// DeleteMailbox removes one mailbox. The typed confirmation is required because
// the server purges the stored mail behind it.
func (s *Service) DeleteMailbox(
	ctx context.Context,
	request *connect.Request[mailv1.DeleteMailboxRequest],
) (*connect.Response[mailv1.DeleteMailboxResponse], error) {
	if err := s.requireEngine(); err != nil {
		return nil, err
	}
	doc, err := s.mailDomain(ctx, request.Msg.GetZoneId())
	if err != nil {
		return nil, err
	}
	account, err := s.mailbox(ctx, doc, request.Msg.GetMailboxId())
	if err != nil {
		return nil, err
	}
	if request.Msg.GetConfirmAddress() != account.EmailAddress {
		return nil, invalidArgument("confirm_address", "type the mailbox address to confirm")
	}
	if err := observeEngineDo(ctx, s, "destroy_account", func(ctx context.Context) error {
		return s.engine.DestroyAccount(ctx, account.ID)
	}); err != nil && !stalwart.IsMissing(err) {
		return nil, engineError(err)
	}
	s.deps.Record(ctx, activity.Event{
		ZoneID:   doc.ZoneID,
		ZoneName: doc.ZoneName,
		Actor:    activity.ActorMailEngine,
		Kind:     activity.KindMailboxDeleted,
		Severity: activity.SeverityWarn,
		Summary:  "Deleted mailbox " + account.EmailAddress,
		Details:  map[string]string{"mailbox.address": account.EmailAddress},
	})
	return connect.NewResponse(&mailv1.DeleteMailboxResponse{}), nil
}

// mailbox reads one account of this zone by id.
func (s *Service) mailbox(ctx context.Context, doc platform.MailDomainDoc, mailboxID string) (stalwart.Account, error) {
	if strings.TrimSpace(mailboxID) == "" {
		return stalwart.Account{}, invalidArgument("mailbox_id", "is required")
	}
	accounts, err := observeEngineArg(ctx, s, "list_accounts", doc.EngineDomainID, s.engine.ListAccounts)
	if err != nil {
		return stalwart.Account{}, engineError(err)
	}
	for _, account := range accounts {
		if account.ID == mailboxID {
			return account, nil
		}
	}
	return stalwart.Account{}, connect.NewError(connect.CodeNotFound, copyError("this mailbox does not exist"))
}

// addressBook reads everything that occupies a local part in the zone.
func (s *Service) addressBook(ctx context.Context, doc platform.MailDomainDoc) ([]stalwart.Account, []stalwart.MailingList, error) {
	accounts, err := observeEngineArg(ctx, s, "list_accounts", doc.EngineDomainID, s.engine.ListAccounts)
	if err != nil {
		return nil, nil, engineError(err)
	}
	lists, err := observeEngineArg(ctx, s, "list_lists", doc.EngineDomainID, s.engine.ListLists)
	if err != nil {
		return nil, nil, engineError(err)
	}
	return accounts, lists, nil
}

// addressTaken mirrors the server's uniqueness rule so the facade can answer
// AlreadyExists with the address the caller asked for, instead of relaying a
// primaryKeyViolation the operator has to decode.
func addressTaken(accounts []stalwart.Account, lists []stalwart.MailingList, local string) bool {
	for _, account := range accounts {
		if account.LocalPart == local {
			return true
		}
		for _, alias := range account.Aliases {
			if alias.Name == local {
				return true
			}
		}
	}
	for _, list := range lists {
		if list.Name == local {
			return true
		}
	}
	return false
}

func normalizeLocalPart(value string) (string, error) {
	local := strings.ToLower(strings.TrimSpace(value))
	if local == "" {
		return "", invalidArgument("local_part", "enter the part before the @")
	}
	if strings.Contains(local, "..") || !localPartPattern.MatchString(local) {
		return "", invalidArgument("local_part", "use letters, digits, and . _ + - between letters or digits")
	}
	return local, nil
}

func normalizeDisplayName(value string) (string, error) {
	name := strings.TrimSpace(value)
	if len([]rune(name)) > maxDisplayNameRunes {
		return "", invalidArgument("display_name", fmt.Sprintf("must be at most %d characters", maxDisplayNameRunes))
	}
	return name, nil
}

func (s *Service) normalizeQuota(value uint64) (uint64, error) {
	if value == 0 {
		return uint64(s.cfg.DefaultQuotaBytes), nil
	}
	if value < minQuotaBytes || value > maxQuotaBytes {
		return 0, invalidArgument("quota_bytes", "must be between 64 MiB and 1 TiB")
	}
	return value, nil
}

func mailboxProto(doc platform.MailDomainDoc, account stalwart.Account) *mailv1.Mailbox {
	message := &mailv1.Mailbox{
		Id:          account.ID,
		ZoneId:      doc.ZoneID,
		Address:     account.EmailAddress,
		LocalPart:   account.LocalPart,
		DisplayName: account.Description,
		QuotaBytes:  account.QuotaBytes,
		UsedBytes:   account.UsedBytes,
	}
	for _, alias := range account.Aliases {
		message.Aliases = append(message.Aliases, alias.Name)
	}
	if !account.CreatedAt.IsZero() {
		message.CreatedAt = timestamppb.New(account.CreatedAt.UTC())
	}
	return message
}
