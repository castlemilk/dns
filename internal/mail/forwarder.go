package mail

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"connectrpc.com/connect"
	mailv1 "github.com/castlemilk/dns/gen/go/mail/v1"
	"github.com/castlemilk/dns/internal/activity"
	"github.com/castlemilk/dns/internal/mail/stalwart"
	"github.com/castlemilk/dns/internal/platform"
)

// maxForwarderTargets bounds a forwarder's fan-out. Ten is generous for the
// "team@ goes to these people" case and small enough that a typo cannot turn
// one address into a mailing blast.
const maxForwarderTargets = 10

// Forwarder id prefixes. A forwarder is not an object on the server: it is
// either an alias entry on a mailbox or a mailing list, so the id has to name
// which one it is.
const (
	aliasIDPrefix = "alias:"
	listIDPrefix  = "list:"
)

// ExternalForwardingNote is shown once under the forwarder list when any
// forwarder has an external target. It is the honest version of "forwarding
// just works".
const ExternalForwardingNote = "Forwarded mail keeps the original sender, so some providers may treat it as spam. This mail server adds ARC headers but does not rewrite the sender (no SRS)."

// ListForwarders merges the mailbox aliases and the mailing lists of one zone.
// Both are forwarders to a reader; only the mechanism differs.
func (s *Service) ListForwarders(
	ctx context.Context,
	request *connect.Request[mailv1.ListForwardersRequest],
) (*connect.Response[mailv1.ListForwardersResponse], error) {
	if !s.cfg.Configured() || s.engine == nil {
		return connect.NewResponse(&mailv1.ListForwardersResponse{}), nil
	}
	doc, err := s.mailDomain(ctx, request.Msg.GetZoneId())
	if err != nil {
		return nil, err
	}
	accounts, lists, err := s.addressBook(ctx, doc)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&mailv1.ListForwardersResponse{
		Forwarders: forwarderProtos(doc, accounts, lists),
	}), nil
}

// CreateForwarder adds one forwarder. One target that is a mailbox of this zone
// becomes an alias on that mailbox — the mail is stored, not relayed. Anything
// else becomes a mailing list, which relays and therefore carries the SRS
// caveat above.
func (s *Service) CreateForwarder(
	ctx context.Context,
	request *connect.Request[mailv1.CreateForwarderRequest],
) (*connect.Response[mailv1.CreateForwarderResponse], error) {
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
	targets, err := normalizeTargets(message.GetTargets())
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
	if slices.Contains(targets, address) {
		return nil, invalidArgument("targets", "a forwarder cannot deliver to itself")
	}

	var localTargets []stalwart.Account
	for _, account := range accounts {
		if slices.Contains(targets, account.EmailAddress) {
			localTargets = append(localTargets, account)
		}
	}
	if len(localTargets) > 1 {
		return nil, invalidArgument("targets",
			"an address can forward to one mailbox here; for several people create a group forwarder")
	}

	var forwarder *mailv1.Forwarder
	if len(localTargets) == 1 && len(targets) == 1 {
		account := localTargets[0]
		aliases := append(slices.Clone(account.Aliases), stalwart.Alias{
			Name:     local,
			DomainID: doc.EngineDomainID,
			Enabled:  true,
		})
		if err := observeEngineDo(ctx, s, "update_account", func(ctx context.Context) error {
			return s.engine.UpdateAccount(ctx, account.ID, stalwart.AccountPatch{Aliases: &aliases})
		}); err != nil {
			return nil, engineErrorFor(address, err)
		}
		forwarder = &mailv1.Forwarder{
			Id:        aliasID(account.ID, local),
			ZoneId:    doc.ZoneID,
			Address:   address,
			LocalPart: local,
			Kind:      mailv1.ForwarderKind_FORWARDER_KIND_ALIAS,
			Targets:   []string{account.EmailAddress},
		}
	} else {
		created, err := observeEngineArg(ctx, s, "create_list", stalwart.ListInput{
			LocalPart:   local,
			DomainID:    doc.EngineDomainID,
			Description: stalwart.ListDescription,
			Recipients:  targets,
		}, s.engine.CreateList)
		if err != nil {
			return nil, engineErrorFor(address, err)
		}
		forwarder = &mailv1.Forwarder{
			Id:        listIDPrefix + created.ID,
			ZoneId:    doc.ZoneID,
			Address:   address,
			LocalPart: local,
			Kind:      mailv1.ForwarderKind_FORWARDER_KIND_LIST,
			Targets:   created.Recipients,
			External:  anyExternal(created.Recipients, doc.ZoneName),
		}
	}

	s.deps.Record(ctx, activity.Event{
		ZoneID:   doc.ZoneID,
		ZoneName: doc.ZoneName,
		Actor:    activity.ActorMailEngine,
		Kind:     activity.KindForwarderCreated,
		Severity: activity.SeverityInfo,
		Summary:  fmt.Sprintf("Forwarding %s to %s", address, strings.Join(forwarder.GetTargets(), ", ")),
		Details: map[string]string{
			"forwarder.address": address,
			"forwarder.targets": strings.Join(forwarder.GetTargets(), ","),
		},
	})
	return connect.NewResponse(&mailv1.CreateForwarderResponse{Forwarder: forwarder}), nil
}

// DeleteForwarder removes one forwarder: the alias entry, or the whole list.
func (s *Service) DeleteForwarder(
	ctx context.Context,
	request *connect.Request[mailv1.DeleteForwarderRequest],
) (*connect.Response[mailv1.DeleteForwarderResponse], error) {
	if err := s.requireEngine(); err != nil {
		return nil, err
	}
	doc, err := s.mailDomain(ctx, request.Msg.GetZoneId())
	if err != nil {
		return nil, err
	}
	forwarderID := request.Msg.GetForwarderId()

	switch {
	case strings.HasPrefix(forwarderID, aliasIDPrefix):
		accountID, local, ok := parseAliasID(forwarderID)
		if !ok {
			return nil, invalidArgument("forwarder_id", "is not a forwarder id")
		}
		account, err := s.mailbox(ctx, doc, accountID)
		if err != nil {
			return nil, err
		}
		remaining := make([]stalwart.Alias, 0, len(account.Aliases))
		found := false
		for _, alias := range account.Aliases {
			if alias.Name == local {
				found = true
				continue
			}
			remaining = append(remaining, alias)
		}
		if !found {
			return nil, connect.NewError(connect.CodeNotFound, copyError("this forwarder does not exist"))
		}
		if err := observeEngineDo(ctx, s, "update_account", func(ctx context.Context) error {
			return s.engine.UpdateAccount(ctx, account.ID, stalwart.AccountPatch{Aliases: &remaining})
		}); err != nil {
			return nil, engineError(err)
		}
		s.recordForwarderDeleted(ctx, doc, local)
	case strings.HasPrefix(forwarderID, listIDPrefix):
		listID := strings.TrimPrefix(forwarderID, listIDPrefix)
		lists, err := observeEngineArg(ctx, s, "list_lists", doc.EngineDomainID, s.engine.ListLists)
		if err != nil {
			return nil, engineError(err)
		}
		index := slices.IndexFunc(lists, func(value stalwart.MailingList) bool { return value.ID == listID })
		if index < 0 {
			return nil, connect.NewError(connect.CodeNotFound, copyError("this forwarder does not exist"))
		}
		if err := observeEngineDo(ctx, s, "destroy_list", func(ctx context.Context) error {
			return s.engine.DestroyList(ctx, listID)
		}); err != nil && !stalwart.IsMissing(err) {
			return nil, engineError(err)
		}
		s.recordForwarderDeleted(ctx, doc, lists[index].Name)
	default:
		return nil, invalidArgument("forwarder_id", "is not a forwarder id")
	}
	return connect.NewResponse(&mailv1.DeleteForwarderResponse{}), nil
}

func (s *Service) recordForwarderDeleted(ctx context.Context, doc platform.MailDomainDoc, local string) {
	address := local + "@" + doc.ZoneName
	s.deps.Record(ctx, activity.Event{
		ZoneID:   doc.ZoneID,
		ZoneName: doc.ZoneName,
		Actor:    activity.ActorMailEngine,
		Kind:     activity.KindForwarderDeleted,
		Severity: activity.SeverityWarn,
		Summary:  "Removed the forwarder " + address,
		Details:  map[string]string{"forwarder.address": address},
	})
}

// forwarderProtos renders the merged view. Aliases come first per mailbox, then
// the lists, both sorted by address so the list is stable between reads.
func forwarderProtos(doc platform.MailDomainDoc, accounts []stalwart.Account, lists []stalwart.MailingList) []*mailv1.Forwarder {
	forwarders := make([]*mailv1.Forwarder, 0, len(lists))
	for _, account := range accounts {
		for _, alias := range account.Aliases {
			forwarders = append(forwarders, &mailv1.Forwarder{
				Id:        aliasID(account.ID, alias.Name),
				ZoneId:    doc.ZoneID,
				Address:   alias.Name + "@" + doc.ZoneName,
				LocalPart: alias.Name,
				Kind:      mailv1.ForwarderKind_FORWARDER_KIND_ALIAS,
				Targets:   []string{account.EmailAddress},
			})
		}
	}
	for _, list := range lists {
		address := list.EmailAddress
		if address == "" {
			address = list.Name + "@" + doc.ZoneName
		}
		forwarders = append(forwarders, &mailv1.Forwarder{
			Id:        listIDPrefix + list.ID,
			ZoneId:    doc.ZoneID,
			Address:   address,
			LocalPart: list.Name,
			Kind:      mailv1.ForwarderKind_FORWARDER_KIND_LIST,
			Targets:   list.Recipients,
			External:  anyExternal(list.Recipients, doc.ZoneName),
		})
	}
	sort.Slice(forwarders, func(i, j int) bool { return forwarders[i].GetAddress() < forwarders[j].GetAddress() })
	return forwarders
}

// countForwarders is the number the Email card shows: every alias plus every
// mailing list in the domain.
func countForwarders(accounts []stalwart.Account, lists []stalwart.MailingList) int {
	count := len(lists)
	for _, account := range accounts {
		count += len(account.Aliases)
	}
	return count
}

func aliasID(accountID, local string) string { return aliasIDPrefix + accountID + ":" + local }

func parseAliasID(value string) (string, string, bool) {
	rest := strings.TrimPrefix(value, aliasIDPrefix)
	accountID, local, found := strings.Cut(rest, ":")
	if !found || accountID == "" || local == "" {
		return "", "", false
	}
	return accountID, local, true
}

func anyExternal(targets []string, zoneName string) bool {
	for _, target := range targets {
		_, domain, found := strings.Cut(strings.ToLower(target), "@")
		if !found || domain != strings.ToLower(zoneName) {
			return true
		}
	}
	return false
}

// normalizeTargets validates the fan-out: one to ten syntactically valid
// addresses, lowercased and de-duplicated.
func normalizeTargets(values []string) ([]string, error) {
	if len(values) == 0 {
		return nil, invalidArgument("targets", "enter at least one address")
	}
	if len(values) > maxForwarderTargets {
		return nil, invalidArgument("targets", fmt.Sprintf("enter at most %d addresses", maxForwarderTargets))
	}
	seen := make(map[string]struct{}, len(values))
	targets := make([]string, 0, len(values))
	for _, value := range values {
		address := strings.ToLower(strings.TrimSpace(value))
		local, domain, found := strings.Cut(address, "@")
		if !found || local == "" || !validMailHostname(domain) {
			return nil, invalidArgument("targets", "enter valid email addresses")
		}
		if _, err := normalizeLocalPart(local); err != nil {
			return nil, invalidArgument("targets", "enter valid email addresses")
		}
		if _, duplicate := seen[address]; duplicate {
			continue
		}
		seen[address] = struct{}{}
		targets = append(targets, address)
	}
	sort.Strings(targets)
	return targets, nil
}
