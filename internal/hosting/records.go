package hosting

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"time"

	"github.com/castlemilk/dns/internal/activity"
	"github.com/castlemilk/dns/internal/enginedns"
	"github.com/castlemilk/dns/internal/platform"
	"github.com/castlemilk/dns/internal/zone"
)

// recordReconcileInterval is the periodic pass; a poke runs one immediately.
const recordReconcileInterval = 5 * time.Minute

// desiredRecords is exactly the set spec2 §3.3 lists for a site: A and AAAA at
// the apex for the gateway addresses, and a www CNAME back to the apex. No CAA
// (users keep their own) and no _acme-challenge (HTTP-01 needs none).
func desiredRecords(doc platform.SiteDoc, gateway []netip.Addr) []zone.Record {
	records := make([]zone.Record, 0, len(gateway)+1)
	for _, address := range gateway {
		address = address.Unmap()
		if !address.IsValid() {
			continue
		}
		recordType := zone.TypeA
		if address.Is6() {
			recordType = zone.TypeAAAA
		}
		records = append(records, zone.Record{
			Name:  "@",
			Type:  recordType,
			TTL:   zone.DefaultRecordTTL,
			Value: address.String(),
		})
	}
	if doc.WWW {
		records = append(records, zone.Record{
			Name:  "www",
			Type:  zone.TypeCNAME,
			TTL:   zone.DefaultRecordTTL,
			Value: doc.ZoneName + ".",
		})
	}
	return records
}

// planSite computes the plan for one site against the current gateway set. It
// reads the zone through the mutator, so it always plans against what the DNS
// API would return.
func (s *Service) planSite(ctx context.Context, doc platform.SiteDoc, gateway []netip.Addr) (enginedns.ZonePlan, error) {
	value, err := s.deps.Zones.GetZone(ctx, doc.ZoneID)
	if err != nil {
		return enginedns.ZonePlan{}, err
	}
	plan, err := enginedns.Plan(value, enginedns.SourceHosting, desiredRecords(doc, gateway))
	if err != nil {
		return enginedns.ZonePlan{}, err
	}
	return plan, nil
}

// applyPlan writes a plan and folds the result back into the site document.
// Every caller is already inside the zone's serializer.
//
// The zone is re-read even when the write failed. What the document says about
// DNS has to be an observation of the zone rather than the last write that
// happened to succeed: leaving the pre-failure values in place would let the
// console keep reporting records that are no longer published.
func (s *Service) applyPlan(
	ctx context.Context,
	doc *platform.SiteDoc,
	plan enginedns.ZonePlan,
	ops []enginedns.Op,
	correlationID string,
) error {
	var applyErr error
	if len(ops) > 0 {
		if _, err := s.deps.Zones.ApplyRecordSet(ctx, doc.ZoneID, ops, activity.ActorHostingEngine, correlationID); err != nil {
			applyErr = err
		}
	}
	if err := s.refreshDNSState(ctx, doc); err != nil {
		if applyErr != nil {
			return applyErr
		}
		return err
	}
	return applyErr
}

// persistAfterApply stores the site row after an apply, whether or not the
// write reached the zone. A failed write leaves the row DEGRADED naming that
// failure, so the state on screen is never a claim about records the facade
// could not publish.
func (s *Service) persistAfterApply(ctx context.Context, doc *platform.SiteDoc, applyErr error) error {
	if applyErr == nil {
		s.applyDNSProblems(doc)
		return s.deps.Store.PutSite(ctx, *doc)
	}
	if s.markZoneMissing(ctx, doc, applyErr) {
		return nil
	}
	doc.State = SiteStateDegraded
	doc.Reason = reason(s.mapEngineError(applyErr).Error())
	touch(doc, s.now())
	return s.deps.Store.PutSite(ctx, *doc)
}

// refreshDNSState re-reads the zone and records which engine records exist,
// what the apex now answers and what still stands in the way.
func (s *Service) refreshDNSState(ctx context.Context, doc *platform.SiteDoc) error {
	value, err := s.deps.Zones.GetZone(ctx, doc.ZoneID)
	if err != nil {
		return err
	}
	gateway, err := s.gatewayAddresses(ctx)
	if err != nil {
		return err
	}
	plan, err := enginedns.Plan(value, enginedns.SourceHosting, desiredRecords(*doc, gateway))
	if err != nil {
		return err
	}

	ids := make([]string, 0, 4)
	apex := make([]string, 0, 2)
	for _, record := range value.Records {
		if record.Source != enginedns.SourceHosting {
			continue
		}
		ids = append(ids, record.ID)
		if record.Name == "@" && (record.Type == zone.TypeA || record.Type == zone.TypeAAAA) {
			apex = append(apex, record.Value)
		}
	}
	slices.Sort(ids)
	slices.Sort(apex)

	doc.RecordIDs = ids
	doc.ApexAddresses = apex
	doc.DNSInSync = plan.InSync()
	doc.DNSProblems = plan.Problems()
	doc.ReconciledAt = s.now()
	touch(doc, s.now())
	return nil
}

// reconcileSiteDNS is the single writer for hosting records. It only ever
// adopts, aligns, creates and removes its own records — a conflicting user
// record is reported as a problem, never deleted, because only an explicit RPC
// with replace_conflicting_records may delete what a person wrote.
func (s *Service) reconcileSiteDNS(ctx context.Context, zoneID string) error {
	return s.deps.Serial.Run(ctx, zoneID, func(ctx context.Context) error {
		doc, err := s.deps.Store.GetSite(ctx, zoneID)
		if err != nil {
			return err
		}
		switch doc.State {
		case SiteStateAttaching, SiteStateReady, SiteStateDegraded:
		default:
			return nil
		}

		gateway, err := s.gatewayAddresses(ctx)
		if err != nil {
			return err
		}
		if len(gateway) == 0 {
			return nil
		}

		// An ATTACHING row is an attach that did not finish — the process died
		// between steps. Re-running the app step here is what makes the attach
		// resumable; it is idempotent because CreateApp is only ever issued
		// after GetApp reported NotFound.
		if doc.State == SiteStateAttaching {
			if err := s.ensureApp(ctx, &doc); err != nil {
				doc.Reason = reason(s.mapEngineError(err).Error())
				touch(&doc, s.now())
				return s.deps.Store.PutSite(ctx, doc)
			}
		}

		plan, err := s.planSite(ctx, doc, gateway)
		if err != nil {
			if s.markZoneMissing(ctx, &doc, err) {
				return nil
			}
			return err
		}

		// plan.Create describes the whole desired set, but WorkerOps leaves out
		// every operation a standing conflict blocks. Count the creates that
		// actually reach the zone, so the activity line cannot claim records
		// the reconcile could not write.
		ops := plan.WorkerOps()
		created := 0
		for _, op := range ops {
			if op.Kind == enginedns.OpCreate {
				created++
			}
		}
		applyErr := s.applyPlan(ctx, &doc, plan, ops, "reconcile-site-dns")
		if applyErr == nil && created > 0 {
			s.deps.Record(ctx, activity.Event{
				ZoneID: doc.ZoneID, ZoneName: doc.ZoneName, Actor: activity.ActorHostingEngine,
				Kind: activity.KindDNSEngineRestored, Severity: activity.SeverityInfo,
				Summary: fmt.Sprintf("Re-created %d website record(s) for %s", created, doc.ZoneName),
			})
		}
		if err := s.persistAfterApply(ctx, &doc, applyErr); err != nil {
			return err
		}
		return applyErr
	})
}

// applyDNSProblems folds the DNS state into the site's own state: a drifted
// zone is DEGRADED with the first problem as the reason, and a site returns to
// READY only when both halves are healthy — the records are in sync AND every
// host the engine should serve is registered. Clearing on the DNS half alone
// would hide a host the engine refused.
func (s *Service) applyDNSProblems(doc *platform.SiteDoc) {
	switch {
	case len(doc.DNSProblems) > 0 && doc.State == SiteStateReady:
		doc.State = SiteStateDegraded
		doc.Reason = doc.DNSProblems[0]
	case len(doc.DNSProblems) == 0 && doc.State == SiteStateDegraded && !doc.ZoneMissing && hostsHealthy(*doc):
		doc.State = SiteStateReady
		doc.Reason = ""
	}
}

// hostsHealthy reports whether every host the site expects is registered with
// the engine.
func hostsHealthy(doc platform.SiteDoc) bool {
	for _, host := range doc.Hostnames {
		if host.State == HostStateMissing || host.State == "" {
			return false
		}
	}
	return true
}

// markZoneMissing records, once, that a site's zone no longer exists. Such a
// row offers only Detach, and no worker touches DNS for it again.
func (s *Service) markZoneMissing(ctx context.Context, doc *platform.SiteDoc, err error) bool {
	if !isNotFound(err) && !isZoneGone(err) {
		return false
	}
	if doc.ZoneMissing {
		return true
	}
	doc.ZoneMissing = true
	doc.State = SiteStateDegraded
	doc.Reason = "zone no longer exists"
	touch(doc, s.now())
	if putErr := s.deps.Store.PutSite(ctx, *doc); putErr != nil {
		s.log().Warn("record missing zone for site", "error", putErr)
	}
	s.deps.Record(ctx, activity.Event{
		ZoneID: doc.ZoneID, ZoneName: doc.ZoneName, Actor: activity.ActorHostingEngine,
		Kind: activity.KindSiteZoneMissing, Severity: activity.SeverityWarn,
		Summary: fmt.Sprintf("The zone for %s no longer exists; only detaching is offered", doc.ZoneName),
	})
	return true
}

func isZoneGone(err error) bool {
	return err != nil && (isNotFound(err) || err.Error() == zone.ErrNotFound.Error())
}

// sweepOrphans releases every hosting record whose site row is gone. It runs
// each reconcile pass, so a crash between deleting a row and clearing its
// records self-heals rather than leaving a record nobody can edit.
func (s *Service) sweepOrphans(ctx context.Context) error {
	sites, err := s.deps.Store.ListSites(ctx)
	if err != nil {
		return err
	}
	bound := make(map[string]struct{}, len(sites))
	for _, site := range sites {
		bound[site.ZoneID] = struct{}{}
	}

	zones, err := s.deps.Zones.ListZones(ctx)
	if err != nil {
		return err
	}
	for _, value := range zones {
		if _, ok := bound[value.ID]; ok {
			continue
		}
		if len(releaseOps(value)) == 0 {
			continue
		}
		zoneID, zoneName := value.ID, value.Name
		count := 0
		// The reads above are outside the zone's gate, and AttachSite holds
		// that gate across both its PutSite and its ApplyRecordSet. A site
		// attached between them is invisible here, so everything the release
		// depends on — the site row and the zone — is read again inside the
		// gate, where an attach cannot be half done.
		err := s.deps.Serial.Run(ctx, zoneID, func(ctx context.Context) error {
			switch _, err := s.deps.Store.GetSite(ctx, zoneID); {
			case err == nil:
				return nil
			case errors.Is(err, platform.ErrNotFound):
			default:
				return err
			}
			current, err := s.deps.Zones.GetZone(ctx, zoneID)
			if err != nil {
				if isZoneGone(err) {
					return nil
				}
				return err
			}
			ops := releaseOps(current)
			if len(ops) == 0 {
				return nil
			}
			if _, err := s.deps.Zones.ApplyRecordSet(ctx, zoneID, ops, activity.ActorHostingEngine, "orphan-sweep"); err != nil {
				return err
			}
			count = len(ops)
			return nil
		})
		if err != nil {
			s.log().Warn("release orphaned website records", "error", err)
			continue
		}
		if count == 0 {
			continue
		}
		s.deps.Record(ctx, activity.Event{
			ZoneID: zoneID, ZoneName: zoneName, Actor: activity.ActorHostingEngine,
			Kind: activity.KindDNSEngineOrphaned, Severity: activity.SeverityWarn,
			Summary: fmt.Sprintf("Released %d website record(s) on %s with no attached site", count, zoneName),
		})
	}
	return nil
}

// releaseOps re-sources every hosting record in a zone back to the operator.
func releaseOps(value zone.Zone) []enginedns.Op {
	ops := make([]enginedns.Op, 0, 4)
	for _, record := range value.Records {
		if record.Source != enginedns.SourceHosting {
			continue
		}
		released := record
		released.Source = enginedns.SourceUser
		ops = append(ops, enginedns.Op{Kind: enginedns.OpUpdate, RecordID: record.ID, Record: released})
	}
	return ops
}

// releaseOrKeepRecords is the DNS half of a detach: keep=true re-sources the
// records as user records, keep=false deletes them.
func (s *Service) releaseOrKeepRecords(ctx context.Context, doc platform.SiteDoc, keep bool) error {
	if doc.ZoneMissing {
		return nil
	}
	return s.deps.Serial.Run(ctx, doc.ZoneID, func(ctx context.Context) error {
		value, err := s.deps.Zones.GetZone(ctx, doc.ZoneID)
		if err != nil {
			if isZoneGone(err) {
				return nil
			}
			return err
		}
		wanted := make(map[string]struct{}, len(doc.RecordIDs))
		for _, id := range doc.RecordIDs {
			wanted[id] = struct{}{}
		}
		ops := make([]enginedns.Op, 0, len(doc.RecordIDs))
		for _, record := range value.Records {
			if record.Source != enginedns.SourceHosting {
				continue
			}
			if _, ok := wanted[record.ID]; !ok {
				continue
			}
			if keep {
				released := record
				released.Source = enginedns.SourceUser
				ops = append(ops, enginedns.Op{Kind: enginedns.OpUpdate, RecordID: record.ID, Record: released})
				continue
			}
			ops = append(ops, enginedns.Op{Kind: enginedns.OpDelete, RecordID: record.ID, Record: record})
		}
		if len(ops) == 0 {
			return nil
		}
		_, err = s.deps.Zones.ApplyRecordSet(ctx, doc.ZoneID, ops, activity.ActorHostingEngine, "detach-site")
		return err
	})
}
