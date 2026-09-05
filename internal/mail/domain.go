package mail

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"connectrpc.com/connect"
	mailv1 "github.com/castlemilk/dns/gen/go/mail/v1"
	platformv1 "github.com/castlemilk/dns/gen/go/platform/v1"
	"github.com/castlemilk/dns/internal/activity"
	"github.com/castlemilk/dns/internal/enginedns"
	"github.com/castlemilk/dns/internal/mail/stalwart"
	"github.com/castlemilk/dns/internal/platform"
	"github.com/castlemilk/dns/internal/zone"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// dkimReadyBudget bounds the in-handler wait for the mail server's DKIM keys.
//
// spec2 §5.3 asks for 30 s, but the HTTP server's WriteTimeout is 30 s, so a
// 30 s wait inside the handler would race the response out of existence. 20 s
// keeps the whole call inside that budget. In practice both keys are generated
// synchronously with the domain (probe-verified on v0.16.20), so the wait is
// normally a single poll; when it does expire the row is persisted DEGRADED
// with dkim_pending and the reconciler finishes the publish 60 s later.
const (
	dkimReadyBudget   = 20 * time.Second
	dkimPollInterval  = time.Second
	dkimFollowUpDelay = 60 * time.Second
)

// descriptionPrefix marks the mail domains this control plane owns. An existing
// Stalwart domain is adopted only when its description matches exactly, so a
// domain somebody else created is never quietly taken over.
const descriptionPrefix = "simple zone "

func domainDescription(zoneID string) string { return descriptionPrefix + zoneID }

// ListMailDomains returns every bound zone with its live counts.
func (s *Service) ListMailDomains(
	ctx context.Context,
	_ *connect.Request[mailv1.ListMailDomainsRequest],
) (*connect.Response[mailv1.ListMailDomainsResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, connect.NewError(connect.CodeCanceled, err)
	}
	if !s.cfg.Configured() {
		return connect.NewResponse(&mailv1.ListMailDomainsResponse{}), nil
	}
	docs, err := s.deps.Store.ListMailDomains(ctx)
	if err != nil {
		return nil, internalError(err)
	}
	response := &mailv1.ListMailDomainsResponse{Domains: make([]*mailv1.MailDomain, 0, len(docs))}
	for _, doc := range docs {
		response.Domains = append(response.Domains, s.domainProto(ctx, doc))
	}
	return connect.NewResponse(response), nil
}

// GetMailDomain returns one bound zone with its queue summary.
func (s *Service) GetMailDomain(
	ctx context.Context,
	request *connect.Request[mailv1.GetMailDomainRequest],
) (*connect.Response[mailv1.GetMailDomainResponse], error) {
	if err := s.requireEngine(); err != nil {
		return nil, err
	}
	doc, err := s.mailDomain(ctx, request.Msg.GetZoneId())
	if err != nil {
		return nil, err
	}
	response := &mailv1.GetMailDomainResponse{
		Domain: s.domainProto(ctx, doc),
		Queue:  s.queueSummary(ctx, doc.ZoneName),
	}
	return connect.NewResponse(response), nil
}

// BindMailDomain binds a zone to the mail engine and publishes exactly the §3.3
// record set.
//
// Order matters and is deliberate: validate, refuse conflicts the caller did
// not agree to replace, THEN persist the row before the first engine or DNS
// write. A crash after that leaves a BINDING row the reconciler finishes; a
// crash before it leaves nothing at all.
func (s *Service) BindMailDomain(
	ctx context.Context,
	request *connect.Request[mailv1.BindMailDomainRequest],
) (*connect.Response[mailv1.BindMailDomainResponse], error) {
	if err := s.requireEngine(); err != nil {
		return nil, err
	}
	message := request.Msg
	zoneValue, err := s.zoneFor(ctx, message.GetZoneId())
	if err != nil {
		return nil, err
	}
	policy, err := s.dmarcPolicy(message.GetDmarcPolicy())
	if err != nil {
		return nil, err
	}

	existing, err := s.deps.Store.GetMailDomain(ctx, zoneValue.ID)
	switch {
	case err == nil && existing.State == StateUnbinding:
		return nil, failedPrecondition("the previous binding is still being removed")
	case err == nil:
		return nil, failedPrecondition("this domain is already bound to the mail engine")
	case !isStoreNotFound(err):
		return nil, internalError(err)
	}

	if err := s.requireHostname(ctx); err != nil {
		return nil, err
	}

	engineDomain, adopted, err := s.findOrRefuse(ctx, zoneValue)
	if err != nil {
		return nil, err
	}

	report := s.reportAddress(zoneValue.Name)

	// A new binding publishes the client-autoconfiguration records unless the
	// caller says otherwise. Absent means on: an older console that does not
	// know the field gets this deployment's current default rather than an
	// accidental opt-out, and no existing binding is touched, because this path
	// only ever runs for a zone that has none.
	autoconfig := true
	if message.PublishClientAutoconfig != nil {
		autoconfig = message.GetPublishClientAutoconfig()
	}

	// The conflict check runs on the records that are known without the engine:
	// the configuration-derived MX, SPF and DMARC, and — when the binding asks
	// for them — a probe standing in for the client-autoconfiguration records,
	// whose owners and types come from this facade's own allow-list even though
	// their values come from the server. An existing MX, or a CNAME at
	// autoconfig, is what a bind actually collides with, and it must be found
	// before anything is written: a refused bind writes nothing at all — no row,
	// no Stalwart domain, no records.
	basePlan, err := enginedns.Plan(zoneValue, zone.SourceMail,
		s.desiredRecords(zoneValue.Name, policy, report, nil, nil))
	if err != nil {
		return nil, internalError(err)
	}
	conflictPlan := basePlan
	if autoconfig {
		conflictPlan, err = enginedns.Plan(zoneValue, zone.SourceMail,
			s.desiredRecords(zoneValue.Name, policy, report, nil, s.autoconfigProbe()))
		if err != nil {
			return nil, internalError(err)
		}
	}
	if len(conflictPlan.Conflicts) > 0 && !message.GetReplaceConflictingRecords() {
		return nil, conflictError(conflictPlan, len(conflictPlan.Conflicts))
	}

	if message.GetDryRun() {
		changes := basePlan.ChangesProto()
		if adopted {
			// An adopted domain already has selectors and a rendered zone file,
			// so the plan can name the real records instead of a placeholder.
			set, setErr := s.engineRecords(ctx, engineDomain.ID, zoneValue.Name, autoconfig)
			if setErr == nil {
				full, planErr := enginedns.Plan(zoneValue, zone.SourceMail,
					s.desiredRecords(zoneValue.Name, policy, report, set.DKIM, set.Autoconfig))
				if planErr == nil {
					changes = full.ChangesProto()
				}
			}
		} else {
			changes = append(changes, dkimPlaceholders()...)
			if autoconfig {
				changes = append(changes, s.autoconfigPlaceholders()...)
			}
		}
		return connect.NewResponse(&mailv1.BindMailDomainResponse{
			DnsPlan:   changes,
			Conflicts: conflictChanges(conflictPlan),
			DryRun:    true,
		}), nil
	}

	now := s.deps.Now()
	doc := platform.MailDomainDoc{
		V:                       1,
		ZoneID:                  zoneValue.ID,
		ZoneName:                zoneValue.Name,
		EngineDomainID:          engineDomain.ID,
		State:                   StateBinding,
		DmarcPolicy:             policy,
		ReportAddress:           report,
		PublishClientAutoconfig: autoconfig,
		CreatedAt:               now,
		UpdatedAt:               now,
		BindStartedAt:           &now,
	}
	if err := s.deps.Store.PutMailDomain(ctx, doc); err != nil {
		return nil, internalError(err)
	}

	if !adopted {
		created, err := observeEngineArg(ctx, s, "create_mail_domain", stalwart.DomainInput{
			Name:             zoneValue.Name,
			Description:      domainDescription(zoneValue.ID),
			ReportAddressURI: "mailto:" + report,
		}, s.engine.CreateDomain)
		if err != nil {
			s.degrade(ctx, doc, err)
			return nil, engineError(err)
		}
		doc.EngineDomainID = created.ID
	}

	doc, err = s.publish(ctx, doc, zoneValue, message.GetReplaceConflictingRecords())
	if err != nil {
		return nil, err
	}

	s.deps.Record(ctx, activity.Event{
		ZoneID:   zoneValue.ID,
		ZoneName: zoneValue.Name,
		Actor:    activity.ActorMailEngine,
		Kind:     activity.KindMailDomainBound,
		Severity: activity.SeverityInfo,
		Summary:  fmt.Sprintf("Email is hosted for %s", zoneValue.Name),
		Details: map[string]string{
			"mail.hostname":       s.cfg.Hostname,
			"dmarc.policy":        policy,
			"mail.autoconfig_dns": fmt.Sprint(autoconfig),
		},
	})
	if doc.DkimPending {
		s.wakeAfter(dkimFollowUpDelay)
	}

	return connect.NewResponse(&mailv1.BindMailDomainResponse{
		Domain:  s.domainProto(ctx, doc),
		DnsPlan: s.planChanges(ctx, doc),
	}), nil
}

// UpdateMailDomain changes the DMARC policy and republishes the _dmarc record.
func (s *Service) UpdateMailDomain(
	ctx context.Context,
	request *connect.Request[mailv1.UpdateMailDomainRequest],
) (*connect.Response[mailv1.UpdateMailDomainResponse], error) {
	if err := s.requireEngine(); err != nil {
		return nil, err
	}
	doc, err := s.mailDomain(ctx, request.Msg.GetZoneId())
	if err != nil {
		return nil, err
	}
	if request.Msg.DmarcPolicy == nil && request.Msg.PublishClientAutoconfig == nil {
		return connect.NewResponse(&mailv1.UpdateMailDomainResponse{Domain: s.domainProto(ctx, doc)}), nil
	}
	policy := doc.DmarcPolicy
	if request.Msg.DmarcPolicy != nil {
		policy, err = s.dmarcPolicy(request.Msg.GetDmarcPolicy())
		if err != nil {
			return nil, err
		}
	}
	autoconfig := doc.PublishClientAutoconfig
	if request.Msg.PublishClientAutoconfig != nil {
		autoconfig = request.Msg.GetPublishClientAutoconfig()
	}
	zoneValue, err := s.zoneFor(ctx, doc.ZoneID)
	if err != nil {
		return nil, err
	}
	// Turning the autoconfiguration records off leaves them in the desired set
	// of nothing, so the very next plan removes exactly the records this engine
	// created — the same path a rotated DKIM selector takes.
	summary := fmt.Sprintf("DMARC policy for %s is now %s", doc.ZoneName, policy)
	if request.Msg.DmarcPolicy == nil {
		summary = autoconfigSummary(doc.ZoneName, autoconfig)
	}
	doc.DmarcPolicy = policy
	doc.PublishClientAutoconfig = autoconfig
	if err := s.deps.Store.PutMailDomain(ctx, doc); err != nil {
		return nil, internalError(err)
	}
	doc, err = s.publish(ctx, doc, zoneValue, false)
	if err != nil {
		return nil, err
	}
	s.deps.Record(ctx, activity.Event{
		ZoneID:   doc.ZoneID,
		ZoneName: doc.ZoneName,
		Actor:    activity.ActorMailEngine,
		Kind:     activity.KindMailDomainUpdated,
		Severity: activity.SeverityInfo,
		Summary:  summary,
		Details: map[string]string{
			"dmarc.policy":        policy,
			"mail.autoconfig_dns": fmt.Sprint(autoconfig),
		},
	})
	return connect.NewResponse(&mailv1.UpdateMailDomainResponse{
		Domain:  s.domainProto(ctx, doc),
		DnsPlan: s.planChanges(ctx, doc),
	}), nil
}

// ReapplyMailDns re-plans and re-applies the published set, optionally
// replacing user records that stand in its way.
func (s *Service) ReapplyMailDns(
	ctx context.Context,
	request *connect.Request[mailv1.ReapplyMailDnsRequest],
) (*connect.Response[mailv1.ReapplyMailDnsResponse], error) {
	if err := s.requireEngine(); err != nil {
		return nil, err
	}
	doc, err := s.mailDomain(ctx, request.Msg.GetZoneId())
	if err != nil {
		return nil, err
	}
	zoneValue, err := s.zoneFor(ctx, doc.ZoneID)
	if err != nil {
		return nil, err
	}
	set, err := s.engineRecords(ctx, doc.EngineDomainID, zoneValue.Name, doc.PublishClientAutoconfig)
	if err != nil {
		return nil, engineError(err)
	}
	desired := s.desiredRecords(zoneValue.Name, doc.DmarcPolicy, doc.ReportAddress, set.DKIM, set.Autoconfig)
	plan, err := enginedns.Plan(zoneValue, zone.SourceMail, desired)
	if err != nil {
		return nil, internalError(err)
	}
	if request.Msg.GetDryRun() {
		return connect.NewResponse(&mailv1.ReapplyMailDnsResponse{
			Domain:    s.domainProto(ctx, doc),
			DnsPlan:   plan.ChangesProto(),
			Conflicts: conflictChanges(plan),
			DryRun:    true,
		}), nil
	}
	if len(plan.Conflicts) > 0 && !request.Msg.GetReplaceConflictingRecords() {
		return nil, conflictError(plan, len(plan.Conflicts))
	}
	doc, err = s.publish(ctx, doc, zoneValue, request.Msg.GetReplaceConflictingRecords())
	if err != nil {
		return nil, err
	}
	s.deps.Record(ctx, activity.Event{
		ZoneID:   doc.ZoneID,
		ZoneName: doc.ZoneName,
		Actor:    activity.ActorMailEngine,
		Kind:     activity.KindMailDNSReapplied,
		Severity: activity.SeverityInfo,
		Summary:  fmt.Sprintf("Re-applied the email records for %s", doc.ZoneName),
	})
	return connect.NewResponse(&mailv1.ReapplyMailDnsResponse{
		Domain:  s.domainProto(ctx, doc),
		DnsPlan: s.planChanges(ctx, doc),
	}), nil
}

// UnbindMailDomain removes the binding: mailboxes, forwarders, DKIM keys and
// the domain on the engine, then the records in the zone.
//
// Every step is idempotent and a missing object is success, so a failed unbind
// can simply be retried from wherever it stopped.
func (s *Service) UnbindMailDomain(
	ctx context.Context,
	request *connect.Request[mailv1.UnbindMailDomainRequest],
) (*connect.Response[mailv1.UnbindMailDomainResponse], error) {
	if err := s.requireEngine(); err != nil {
		return nil, err
	}
	message := request.Msg
	doc, err := s.mailDomain(ctx, message.GetZoneId())
	if err != nil {
		return nil, err
	}

	accounts, err := observeEngineArg(ctx, s, "list_accounts", doc.EngineDomainID, s.engine.ListAccounts)
	if err != nil {
		return nil, engineError(err)
	}
	lists, err := observeEngineArg(ctx, s, "list_lists", doc.EngineDomainID, s.engine.ListLists)
	if err != nil {
		return nil, engineError(err)
	}
	forwarders := countForwarders(accounts, lists)
	if len(accounts) > 0 || forwarders > 0 {
		if !message.GetDeleteMailboxes() {
			return nil, failedPrecondition(fmt.Sprintf(
				"%d mailboxes and %d forwarders still exist — confirm their deletion", len(accounts), forwarders))
		}
		if message.GetConfirmZoneName() != doc.ZoneName {
			return nil, invalidArgument("confirm_zone_name", "type the domain name to confirm")
		}
	}

	doc.State = StateUnbinding
	doc.Reason = ""
	doc.UpdatedAt = s.deps.Now()
	if err := s.deps.Store.PutMailDomain(ctx, doc); err != nil {
		return nil, internalError(err)
	}

	for _, account := range accounts {
		if err := observeEngineDo(ctx, s, "destroy_account", func(ctx context.Context) error {
			return s.engine.DestroyAccount(ctx, account.ID)
		}); err != nil && !stalwart.IsMissing(err) {
			s.degrade(ctx, doc, err)
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
	}
	for _, list := range lists {
		if err := observeEngineDo(ctx, s, "destroy_list", func(ctx context.Context) error {
			return s.engine.DestroyList(ctx, list.ID)
		}); err != nil && !stalwart.IsMissing(err) {
			s.degrade(ctx, doc, err)
			return nil, engineError(err)
		}
	}
	if err := s.destroyDomain(ctx, doc); err != nil {
		s.degrade(ctx, doc, err)
		return nil, engineError(err)
	}

	if err := s.releaseRecords(ctx, doc, message.GetKeepDnsRecords()); err != nil {
		return nil, err
	}
	if err := s.deps.Store.DeleteMailDomain(ctx, doc.ZoneID); err != nil {
		return nil, internalError(err)
	}
	s.deps.Record(ctx, activity.Event{
		ZoneID:   doc.ZoneID,
		ZoneName: doc.ZoneName,
		Actor:    activity.ActorMailEngine,
		Kind:     activity.KindMailDomainUnbound,
		Severity: activity.SeverityWarn,
		Summary:  fmt.Sprintf("Email hosting removed for %s", doc.ZoneName),
		Details: map[string]string{
			"mailboxes":  fmt.Sprint(len(accounts)),
			"forwarders": fmt.Sprint(forwarders),
		},
	})
	return connect.NewResponse(&mailv1.UnbindMailDomainResponse{
		MailboxesDeleted:  uint32(len(accounts)),
		ForwardersDeleted: uint32(forwarders),
	}), nil
}

// destroyDomain removes the engine domain, clearing the DKIM signatures first.
// objectIsLinked is retried once after re-reading the children, because the
// account purge behind a destroy is asynchronous: the first attempt can see a
// child the second no longer does.
func (s *Service) destroyDomain(ctx context.Context, doc platform.MailDomainDoc) error {
	if doc.EngineDomainID == "" {
		return nil
	}
	for attempt := range 2 {
		signatures, err := observeEngineArg(ctx, s, "list_dkim", doc.EngineDomainID, s.engine.ListDkim)
		if err != nil {
			return err
		}
		for _, signature := range signatures {
			if err := observeEngineDo(ctx, s, "destroy_dkim", func(ctx context.Context) error {
				return s.engine.DestroyDkim(ctx, signature.ID)
			}); err != nil && !stalwart.IsMissing(err) {
				return err
			}
		}
		err = observeEngineDo(ctx, s, "destroy_mail_domain", func(ctx context.Context) error {
			return s.engine.DestroyDomain(ctx, doc.EngineDomainID)
		})
		switch {
		case err == nil, stalwart.IsMissing(err):
			return nil
		case stalwart.IsLinked(err) && attempt == 0:
			continue
		default:
			return err
		}
	}
	return nil
}

// releaseRecords either deletes this engine's records or re-sources them as
// user records, so "keep the records" leaves editable rows behind.
func (s *Service) releaseRecords(ctx context.Context, doc platform.MailDomainDoc, keep bool) error {
	if doc.ZoneMissing {
		return nil
	}
	zoneValue, err := s.zoneFor(ctx, doc.ZoneID)
	if err != nil {
		if connect.CodeOf(err) == connect.CodeNotFound {
			return nil
		}
		return err
	}
	var ops []enginedns.Op
	for _, record := range zoneValue.Records {
		if record.Source != zone.SourceMail {
			continue
		}
		if keep {
			released := record
			released.Source = zone.SourceUser
			ops = append(ops, enginedns.Op{Kind: enginedns.OpUpdate, RecordID: record.ID, Record: released})
			continue
		}
		ops = append(ops, enginedns.Op{Kind: enginedns.OpDelete, RecordID: record.ID, Record: record})
	}
	if len(ops) == 0 {
		return nil
	}
	return s.deps.Serial.Run(ctx, doc.ZoneID, func(ctx context.Context) error {
		_, err := s.deps.Zones.ApplyRecordSet(ctx, doc.ZoneID, ops, activity.ActorMailEngine, "")
		return err
	})
}

// publish computes the current desired set and applies it inside the zone's
// queue, then persists the resulting state.
func (s *Service) publish(
	ctx context.Context,
	doc platform.MailDomainDoc,
	zoneValue zone.Zone,
	replaceConflicts bool,
) (platform.MailDomainDoc, error) {
	set, pending, err := s.waitForDkim(ctx, doc.EngineDomainID, zoneValue.Name, doc.PublishClientAutoconfig)
	if err != nil {
		s.degrade(ctx, doc, err)
		return doc, engineError(err)
	}
	desired := s.desiredRecords(zoneValue.Name, doc.DmarcPolicy, doc.ReportAddress, set.DKIM, set.Autoconfig)

	var applied zone.Zone
	err = s.deps.Serial.Run(ctx, doc.ZoneID, func(ctx context.Context) error {
		current, err := s.deps.Zones.GetZone(ctx, doc.ZoneID)
		if err != nil {
			return err
		}
		plan, err := enginedns.Plan(current, zone.SourceMail, desired)
		if err != nil {
			return err
		}
		ops := plan.ApplyOps(replaceConflicts)
		if len(ops) == 0 {
			applied = current
			return nil
		}
		result, err := s.deps.Zones.ApplyRecordSet(ctx, doc.ZoneID, ops, activity.ActorMailEngine, "")
		if err != nil {
			return err
		}
		applied = result.Zone
		return nil
	})
	if err != nil {
		s.degrade(ctx, doc, err)
		return doc, err
	}

	doc.Records = recordDocs(desired)
	doc.RecordIDs = recordIDs(applied)
	doc.DKIM = dkimDocs(ctx, s, doc.EngineDomainID, applied, set.DKIM)
	doc.DkimPending = pending
	doc.ZoneMissing = false
	doc.UpdatedAt = s.deps.Now()
	doc.ReconciledAt = doc.UpdatedAt
	doc.BindStartedAt = nil
	doc.State = StateBound
	doc.Reason = ""
	if pending {
		doc.State = StateDegraded
		doc.Reason = reasonDkimPending
	} else if problems := planProblems(applied, desired); len(problems) > 0 {
		doc.State = StateDegraded
		doc.Reason = problems[0]
	}
	if err := s.deps.Store.PutMailDomain(ctx, doc); err != nil {
		return doc, internalError(err)
	}
	return doc, nil
}

// planProblems re-plans against the applied zone and reports what still stands
// in the way, so a DEGRADED row can say why.
func planProblems(applied zone.Zone, desired []zone.Record) []string {
	plan, err := enginedns.Plan(applied, zone.SourceMail, desired)
	if err != nil {
		return nil
	}
	return plan.Problems()
}

// waitForDkim polls until one active signature exists per requested algorithm,
// bounded by dkimReadyBudget. RSA can appear a couple of seconds after Ed25519,
// and Gmail and Microsoft verify RSA only — so publishing the Ed25519 key alone
// would leave outbound mail unsigned for exactly the receivers that matter.
func (s *Service) waitForDkim(ctx context.Context, domainID, zoneName string, autoconfig bool) (engineRecordSet, bool, error) {
	deadline := time.Now().Add(s.dkimBudget)
	for {
		set, err := s.engineRecords(ctx, domainID, zoneName, autoconfig)
		if err != nil {
			return engineRecordSet{}, false, err
		}
		if hasAllAlgorithms(set.Algorithms) {
			return set, false, nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return set, true, nil
		}
		select {
		case <-ctx.Done():
			return set, true, nil
		case <-time.After(dkimPollInterval):
		}
	}
}

func hasAllAlgorithms(present map[string]struct{}) bool {
	for _, algorithm := range stalwart.RequestedDkimAlgorithms {
		if _, ok := present[algorithm]; !ok {
			return false
		}
	}
	return true
}

// engineRecordSet is everything this facade takes from the engine's rendered
// zone file for one domain, plus the DKIM algorithms it found active.
type engineRecordSet struct {
	DKIM       []dkimRecord
	Autoconfig []autoconfigRecord
	// Algorithms names the DKIM algorithms with at least one active, valid
	// signature. waitForDkim compares it against the requested set.
	Algorithms map[string]struct{}
}

// engineRecords reads the engine's active signatures and the records its zone
// file renders: the DKIM TXTs always, and the client-autoconfiguration set when
// autoconfig is true.
//
// The DKIM values come from the zone file — never from the public key alone —
// so the record this facade publishes is byte-for-byte the one the server signs
// with.
func (s *Service) engineRecords(ctx context.Context, domainID, zoneName string, autoconfig bool) (engineRecordSet, error) {
	if domainID == "" {
		return engineRecordSet{}, nil
	}
	signatures, err := observeEngineArg(ctx, s, "list_dkim", domainID, s.engine.ListDkim)
	if err != nil {
		return engineRecordSet{}, err
	}
	active := map[string]struct{}{}
	algorithms := map[string]struct{}{}
	for _, signature := range signatures {
		if !signature.Active() {
			continue
		}
		active[signature.Selector] = struct{}{}
		algorithms[signature.Algorithm] = struct{}{}
	}
	set := engineRecordSet{Algorithms: algorithms}
	if len(active) == 0 && !autoconfig {
		return set, nil
	}
	domain, err := observeEngineArg(ctx, s, "get_mail_domain", domainID, s.engine.GetDomain)
	if err != nil {
		return set, err
	}
	if autoconfig {
		set.Autoconfig = parseAutoconfigRecords(domain.DNSZoneFile, zoneName, s.cfg.Hostname)
	}
	if len(active) == 0 {
		return set, nil
	}
	records, invalid := parseDkimRecords(domain.DNSZoneFile, zoneName, active)
	if len(invalid) > 0 {
		sort.Strings(invalid)
		s.deps.Record(ctx, activity.Event{
			ZoneName: zoneName,
			Actor:    activity.ActorMailEngine,
			Kind:     activity.KindMailRecordsInvalid,
			Severity: activity.SeverityWarn,
			Summary:  fmt.Sprintf("The mail engine offered %d DKIM records that are not valid keys; they were not published", len(invalid)),
			Details:  map[string]string{"dkim.selectors": strings.Join(invalid, ",")},
		})
		// A selector whose record did not parse is not an active key we can
		// honour, so it must not count towards readiness either.
		for _, selector := range invalid {
			for _, signature := range signatures {
				if signature.Selector == selector {
					delete(algorithms, signature.Algorithm)
				}
			}
		}
	}
	set.DKIM = records
	return set, nil
}

// dkimDocs renders the persisted DKIM view, with published computed from the
// zone rather than assumed.
func dkimDocs(
	ctx context.Context,
	s *Service,
	domainID string,
	zoneValue zone.Zone,
	want []dkimRecord,
) []platform.DkimDoc {
	signatures, err := observeEngineArg(ctx, s, "list_dkim", domainID, s.engine.ListDkim)
	if err != nil {
		return nil
	}
	published := publishedSelectors(zoneValue, want)
	docs := make([]platform.DkimDoc, 0, len(signatures))
	for _, signature := range signatures {
		_, ok := published[signature.Selector]
		docs = append(docs, platform.DkimDoc{
			Selector:  signature.Selector,
			Algorithm: signature.Algorithm,
			Stage:     signature.Stage,
			Published: ok,
			CreatedAt: signature.CreatedAt,
		})
	}
	return docs
}

// findOrRefuse adopts an existing engine domain only when it is one this
// control plane created for this exact zone.
func (s *Service) findOrRefuse(ctx context.Context, zoneValue zone.Zone) (stalwart.Domain, bool, error) {
	found, ok, err := observeEngineFind(ctx, s, "find_domain", zoneValue.Name, s.engine.FindDomain)
	if err != nil {
		return stalwart.Domain{}, false, engineError(err)
	}
	if !ok {
		return stalwart.Domain{}, false, nil
	}
	if found.Description != domainDescription(zoneValue.ID) {
		return stalwart.Domain{}, false, failedPrecondition("a mail domain with this name already exists on the mail server")
	}
	return found, true, nil
}

// requireHostname refuses to act while the server's own hostname differs from
// MAIL_HOSTNAME: the MX record this facade publishes names MAIL_HOSTNAME, so
// binding against a server that calls itself something else would publish mail
// routing to a host that does not serve these mailboxes.
func (s *Service) requireHostname(ctx context.Context) error {
	settings, err := observeEngine(ctx, s, "system_settings", s.engine.SystemSettings)
	if err != nil {
		return engineError(err)
	}
	if !strings.EqualFold(settings.DefaultHostname, s.cfg.Hostname) {
		return failedPrecondition(hostnameMismatch(s.cfg.Hostname, settings.DefaultHostname))
	}
	return nil
}

// zoneFor reads a zone and enforces the hostname rule: phase-1 zone names may
// contain characters that are legal in the store but not in a mail domain.
func (s *Service) zoneFor(ctx context.Context, zoneID string) (zone.Zone, error) {
	if strings.TrimSpace(zoneID) == "" {
		return zone.Zone{}, invalidArgument("zone_id", "is required")
	}
	zoneValue, err := s.deps.Zones.GetZone(ctx, zoneID)
	if err != nil {
		return zone.Zone{}, err
	}
	if !validMailHostname(zoneValue.Name) {
		return zone.Zone{}, invalidArgument("zone_id", "this zone name is not a valid hostname")
	}
	return zoneValue, nil
}

// validMailHostname is the rule /api/nameservers uses: at least two labels, each
// a valid DNS label, no more than 253 characters.
func validMailHostname(name string) bool {
	name = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
	if name == "" || len(name) > 253 {
		return false
	}
	labels := strings.Split(name, ".")
	if len(labels) < 2 {
		return false
	}
	for _, label := range labels {
		if label == "" || len(label) > 63 {
			return false
		}
		for index, char := range label {
			switch {
			case char >= 'a' && char <= 'z', char >= '0' && char <= '9':
			case char == '-' && index > 0 && index < len(label)-1:
			default:
				return false
			}
		}
	}
	return true
}

// mailDomain reads one bound row.
func (s *Service) mailDomain(ctx context.Context, zoneID string) (platform.MailDomainDoc, error) {
	if strings.TrimSpace(zoneID) == "" {
		return platform.MailDomainDoc{}, invalidArgument("zone_id", "is required")
	}
	doc, err := s.deps.Store.GetMailDomain(ctx, zoneID)
	if isStoreNotFound(err) {
		return platform.MailDomainDoc{}, connect.NewError(connect.CodeNotFound, copyError("this domain is not bound to the mail engine"))
	}
	if err != nil {
		return platform.MailDomainDoc{}, internalError(err)
	}
	return doc, nil
}

func isStoreNotFound(err error) bool {
	return err != nil && errors.Is(err, platform.ErrNotFound)
}

// degrade records a failed step on the row without losing the binding, so the
// reconciler can pick it up and an operator can see why.
func (s *Service) degrade(ctx context.Context, doc platform.MailDomainDoc, cause error) {
	doc.State = StateDegraded
	doc.Reason = reasonOf(cause)
	doc.UpdatedAt = s.deps.Now()
	if err := s.deps.Store.PutMailDomain(ctx, doc); err != nil {
		s.deps.Log().Warn("persist a degraded mail domain", "error", err)
	}
}

// currentPlan re-plans against the live zone, for the response's dns_plan.
func (s *Service) currentPlan(ctx context.Context, doc platform.MailDomainDoc) (enginedns.ZonePlan, error) {
	zoneValue, err := s.deps.Zones.GetZone(ctx, doc.ZoneID)
	if err != nil {
		return enginedns.ZonePlan{}, err
	}
	desired := make([]zone.Record, 0, len(doc.Records))
	for _, record := range doc.Records {
		desired = append(desired, zone.Record{
			Name:  record.Name,
			Type:  zone.RecordType(record.Type),
			TTL:   record.TTL,
			Value: record.Value,
		})
	}
	return enginedns.Plan(zoneValue, zone.SourceMail, desired)
}

// planChanges renders the plan a response carries. A plan that cannot be
// computed (the zone vanished between the write and the response) yields no
// changes rather than an error: the write already succeeded, and the row the
// response also carries reports the missing zone.
func (s *Service) planChanges(ctx context.Context, doc platform.MailDomainDoc) []*platformv1.RecordChange {
	plan, err := s.currentPlan(ctx, doc)
	if err != nil {
		s.deps.Log().Warn("re-plan the mail records for the response", "zone", doc.ZoneName, "error", err)
		return nil
	}
	return plan.ChangesProto()
}

// conflictError is the FailedPrecondition an explicit RPC returns when the zone
// holds records the caller has not agreed to replace. The conflicts travel as a
// Connect error detail so the UI can render the checklist.
func conflictError(plan enginedns.ZonePlan, count int) error {
	err := connect.NewError(connect.CodeFailedPrecondition,
		copyError(fmt.Sprintf("%d existing records conflict with the email records", count)))
	detail, detailErr := connect.NewErrorDetail(&mailv1.ReapplyMailDnsResponse{Conflicts: conflictChanges(plan)})
	if detailErr == nil {
		err.AddDetail(detail)
	}
	return err
}

func conflictChanges(plan enginedns.ZonePlan) []*platformv1.RecordChange {
	changes := make([]*platformv1.RecordChange, 0, len(plan.Conflicts))
	for _, change := range plan.Changes {
		if change.Op != enginedns.ChangeReplace {
			continue
		}
		changes = append(changes, change.Proto())
	}
	return changes
}

// dkimPlaceholders describes the DKIM records a bind will publish for a domain
// the engine has not created yet: the selectors do not exist until the server
// generates the keys, so the plan names the shape and says where it comes from.
func dkimPlaceholders() []*platformv1.RecordChange {
	changes := make([]*platformv1.RecordChange, 0, len(stalwart.RequestedDkimAlgorithms))
	for _, algorithm := range stalwart.RequestedDkimAlgorithms {
		changes = append(changes, &platformv1.RecordChange{
			Op:   enginedns.ChangeCreate,
			Name: "<selector>._domainkey",
			Type: string(zone.TypeTXT),
			Ttl:  recordTTL,
			Why:  fmt.Sprintf("a %s DKIM key generated by the mail server on bind", algorithmLabel(algorithm)),
		})
	}
	return changes
}

// autoconfigProbe stands in for the client-autoconfiguration records during the
// pre-flight conflict check, before the engine has rendered a zone file to read
// them from.
//
// The owners and types are exact — they are this facade's allow-list — and that
// is all a conflict depends on: a CNAME collides with anything else at its name,
// and an SRV shares its RRset with whatever is already there. The values are
// placeholders and are never published or shown; the real ones come from the
// server, and s.autoconfigPlaceholders is what a dry run renders instead.
func (s *Service) autoconfigProbe() []autoconfigRecord {
	records := make([]autoconfigRecord, 0, len(autoconfigSRVOwners)+len(autoconfigCNAMEOwners))
	for _, owner := range autoconfigSRVOwners {
		records = append(records, autoconfigRecord{Name: owner, Type: "SRV", Value: "0 1 443 " + s.cfg.Hostname + "."})
	}
	for _, owner := range autoconfigCNAMEOwners {
		records = append(records, autoconfigRecord{Name: owner, Type: "CNAME", Value: s.cfg.Hostname + "."})
	}
	return records
}

// autoconfigPlaceholders describes the client-autoconfiguration records a bind
// will publish for a domain the engine has not rendered a zone file for yet.
// The owners and types are this facade's own allow-list, so they are known; the
// priorities, weights and ports come from the server and are not guessed at.
func (s *Service) autoconfigPlaceholders() []*platformv1.RecordChange {
	changes := make([]*platformv1.RecordChange, 0, len(autoconfigSRVOwners)+len(autoconfigCNAMEOwners))
	for _, owner := range autoconfigSRVOwners {
		changes = append(changes, &platformv1.RecordChange{
			Op:   enginedns.ChangeCreate,
			Name: owner,
			Type: string(zone.TypeSRV),
			Ttl:  recordTTL,
			Why:  "a mail client autoconfiguration record offered by the mail server",
		})
	}
	for _, owner := range autoconfigCNAMEOwners {
		changes = append(changes, &platformv1.RecordChange{
			Op:    enginedns.ChangeCreate,
			Name:  owner,
			Type:  string(zone.TypeCNAME),
			Ttl:   recordTTL,
			Value: s.cfg.Hostname + ".",
			Why:   "a mail client autoconfiguration alias offered by the mail server",
		})
	}
	return changes
}

// autoconfigSummary is the activity line for an update that only moved the
// autoconfiguration switch.
func autoconfigSummary(zoneName string, on bool) string {
	if on {
		return fmt.Sprintf("Publishing the mail client autoconfiguration records for %s", zoneName)
	}
	return fmt.Sprintf("Removed the mail client autoconfiguration records for %s", zoneName)
}

func algorithmLabel(algorithm string) string {
	switch algorithm {
	case stalwart.DkimAlgorithmEd25519:
		return "Ed25519"
	case stalwart.DkimAlgorithmRSA:
		return "RSA"
	default:
		return algorithm
	}
}

// domainProto renders one row for the wire, enriching it with the live counts
// and the DNS state of every record it owns.
func (s *Service) domainProto(ctx context.Context, doc platform.MailDomainDoc) *mailv1.MailDomain {
	message := &mailv1.MailDomain{
		ZoneId:         doc.ZoneID,
		ZoneName:       doc.ZoneName,
		EngineDomainId: doc.EngineDomainID,
		State:          domainState(doc.State),
		Reason:         doc.Reason,
		MailHostname:   s.cfg.Hostname,
		DmarcPolicy:    doc.DmarcPolicy,
		ReportAddress:  doc.ReportAddress,
		MailboxLimit:   uint32(max(s.cfg.MailboxesPerDomain, 0)),
		DkimPending:    doc.DkimPending,
		ZoneMissing:    doc.ZoneMissing,

		PublishClientAutoconfig: doc.PublishClientAutoconfig,
		Delivery:                s.deliveryStats(doc),
	}
	if !doc.CreatedAt.IsZero() {
		message.CreatedAt = timestamppb.New(doc.CreatedAt.UTC())
	}
	if !doc.ReconciledAt.IsZero() {
		message.ReconciledAt = timestamppb.New(doc.ReconciledAt.UTC())
	}
	for _, key := range doc.DKIM {
		entry := &mailv1.DkimKey{
			Selector:  key.Selector,
			Algorithm: key.Algorithm,
			Stage:     key.Stage,
			Published: key.Published,
		}
		if !key.CreatedAt.IsZero() {
			entry.CreatedAt = timestamppb.New(key.CreatedAt.UTC())
		}
		message.Dkim = append(message.Dkim, entry)
	}

	zoneValue, zoneErr := s.deps.Zones.GetZone(ctx, doc.ZoneID)
	if zoneErr == nil {
		desired := make([]zone.Record, 0, len(doc.Records))
		for _, record := range doc.Records {
			desired = append(desired, zone.Record{
				Name:  record.Name,
				Type:  zone.RecordType(record.Type),
				TTL:   record.TTL,
				Value: record.Value,
			})
		}
		message.Records, message.RecordsInSync = recordState(zoneValue, desired)
		// published comes from the record rows this response is already
		// carrying, so a key cannot be reported as published next to a row that
		// says its record is missing or drifted.
		published := publishedFromRecords(message.Records)
		for index, key := range doc.DKIM {
			_, ok := published[key.Selector]
			message.Dkim[index].Published = ok
		}
	} else if connect.CodeOf(zoneErr) == connect.CodeNotFound {
		message.ZoneMissing = true
		message.State = mailv1.MailDomainState_MAIL_DOMAIN_STATE_DEGRADED
		message.Reason = reasonZoneMissing
	}

	if s.engine == nil || doc.EngineDomainID == "" {
		return message
	}
	accounts, err := observeEngineArg(ctx, s, "list_accounts", doc.EngineDomainID, s.engine.ListAccounts)
	if err != nil {
		message.State = mailv1.MailDomainState_MAIL_DOMAIN_STATE_DEGRADED
		message.Reason = countsUnknownReason(reasonOf(engineError(err)))
		return message
	}
	lists, err := observeEngineArg(ctx, s, "list_lists", doc.EngineDomainID, s.engine.ListLists)
	if err != nil {
		message.State = mailv1.MailDomainState_MAIL_DOMAIN_STATE_DEGRADED
		message.Reason = countsUnknownReason(reasonOf(engineError(err)))
		return message
	}
	message.MailboxCount = uint32(len(accounts))
	for _, account := range accounts {
		message.UsedBytes += account.UsedBytes
	}
	message.ForwarderCount = uint32(countForwarders(accounts, lists))
	message.HasPostmaster = hasPostmaster(accounts, lists)
	// Both reads answered, so the four figures above are the mail server's own.
	// Every other return from this function leaves counts_available false.
	message.CountsAvailable = true
	return message
}

// countsUnknownReason says out loud what the zeros on this message mean.
//
// mailbox_count, used_bytes, forwarder_count and has_postmaster are plain proto3
// scalars: when the engine read that would have filled them fails, they go on
// the wire as 0 and false, and nothing in those fields distinguishes that from a
// domain with no mailboxes and no postmaster. MailDomain.counts_available is the
// flag that says which it is; this reason is the sentence a console shows in its
// place, so the operator is told what failed and not only that something did.
func countsUnknownReason(reason string) string {
	return strings.TrimSuffix(strings.TrimSpace(reason), ".") +
		". The mailbox, forwarder and storage figures for this domain are unknown while that read fails."
}

func domainState(state string) mailv1.MailDomainState {
	switch state {
	case StateBinding:
		return mailv1.MailDomainState_MAIL_DOMAIN_STATE_BINDING
	case StateBound:
		return mailv1.MailDomainState_MAIL_DOMAIN_STATE_BOUND
	case StateDegraded:
		return mailv1.MailDomainState_MAIL_DOMAIN_STATE_DEGRADED
	case StateUnbinding:
		return mailv1.MailDomainState_MAIL_DOMAIN_STATE_UNBINDING
	default:
		return mailv1.MailDomainState_MAIL_DOMAIN_STATE_UNSPECIFIED
	}
}

// hasPostmaster reports whether postmaster@<zone> resolves to something: a
// mailbox, an alias or a forwarder. DMARC reports are sent there by default, so
// an absent postmaster means nobody reads them.
func hasPostmaster(accounts []stalwart.Account, lists []stalwart.MailingList) bool {
	const local = "postmaster"
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
