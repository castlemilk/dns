package mail

import (
	"context"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/castlemilk/dns/internal/activity"
	"github.com/castlemilk/dns/internal/enginedns"
	"github.com/castlemilk/dns/internal/platform"
	"github.com/castlemilk/dns/internal/zone"
)

// reconcileInterval is the periodic pass. Mail records change only when the
// server rotates a DKIM key or an operator edits the zone, and both wake the
// reconciler directly, so the timer is a safety net rather than the mechanism.
const reconcileInterval = 10 * time.Minute

// domainBudget bounds one row's reconcile, so one unreachable engine cannot
// stall the pass for every other domain.
const domainBudget = 20 * time.Second

// ReconcileAll runs one pass over every bound row and then sweeps orphaned
// records. It is called at start-up, on the timer, and whenever an operator
// mutation wakes the service.
func (s *Service) ReconcileAll(ctx context.Context) error {
	if !s.cfg.Configured() || s.engine == nil {
		return nil
	}
	docs, err := s.deps.Store.ListMailDomains(ctx)
	if err != nil {
		return fmt.Errorf("list mail domains: %w", err)
	}
	bound := make(map[string]struct{}, len(docs))
	for _, doc := range docs {
		bound[doc.ZoneID] = struct{}{}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		domainCtx, cancel := context.WithTimeout(ctx, domainBudget)
		if err := s.reconcileDomain(domainCtx, doc); err != nil && domainCtx.Err() == nil {
			s.deps.Log().Warn("reconcile mail domain", "zone", doc.ZoneName, "error", err)
		}
		cancel()
	}
	return s.sweepOrphans(ctx, bound)
}

// reconcileDomain brings one row back in line with the engine and the zone.
//
// It only ever adopts, aligns, creates and removes its own records: a user
// record that stands in the way is reported as a problem and the row goes
// DEGRADED. Deleting a user record is something only an explicit RPC with
// replace_conflicting_records may do.
func (s *Service) reconcileDomain(ctx context.Context, doc platform.MailDomainDoc) error {
	zoneValue, err := s.deps.Zones.GetZone(ctx, doc.ZoneID)
	if err != nil {
		if connect.CodeOf(err) == connect.CodeNotFound {
			return s.markZoneMissing(ctx, doc)
		}
		return err
	}
	if doc.State == StateUnbinding {
		// An interrupted unbind is finished by the operator retrying it: the
		// confirmation it required is not something a worker may assume.
		return nil
	}

	// The hostname is re-checked every pass. A server that renamed itself must
	// not have its new name published as MX by a background worker, so the row
	// degrades and nothing is written.
	settings, err := observeEngine(ctx, s, "system_settings", s.engine.SystemSettings)
	if err != nil {
		return err
	}
	if !strings.EqualFold(settings.DefaultHostname, s.cfg.Hostname) {
		reason := hostnameMismatch(s.cfg.Hostname, settings.DefaultHostname)
		if doc.State != StateDegraded || doc.Reason != reason {
			doc.State = StateDegraded
			doc.Reason = reason
			doc.UpdatedAt = s.deps.Now()
			doc.ReconciledAt = doc.UpdatedAt
			return s.deps.Store.PutMailDomain(ctx, doc)
		}
		return nil
	}

	// A BINDING row is an interrupted bind: the engine domain may or may not
	// exist. Both steps are idempotent, so re-running them finishes the job.
	if doc.EngineDomainID == "" || doc.State == StateBinding {
		found, ok, err := observeEngineFind(ctx, s, "find_domain", doc.ZoneName, s.engine.FindDomain)
		if err != nil {
			return err
		}
		switch {
		case ok && found.Description == domainDescription(doc.ZoneID):
			doc.EngineDomainID = found.ID
		case ok:
			doc.State = StateDegraded
			doc.Reason = "a mail domain with this name already exists on the mail server"
			doc.UpdatedAt = s.deps.Now()
			return s.deps.Store.PutMailDomain(ctx, doc)
		default:
			created, err := observeEngineArg(ctx, s, "create_mail_domain", DomainInput{
				Name:             doc.ZoneName,
				Description:      domainDescription(doc.ZoneID),
				ReportAddressURI: "mailto:" + doc.ReportAddress,
			}, s.engine.CreateDomain)
			if err != nil {
				return err
			}
			doc.EngineDomainID = created.ID
		}
	}

	before := slices.Clone(doc.DKIM)
	updated, err := s.publish(ctx, doc, zoneValue, false)
	if err != nil {
		return err
	}
	if dkimChanged(before, updated.DKIM) {
		s.deps.Record(ctx, activity.Event{
			ZoneID:   updated.ZoneID,
			ZoneName: updated.ZoneName,
			Actor:    activity.ActorReconciler,
			Kind:     activity.KindMailRecordsChanged,
			Severity: activity.SeverityInfo,
			Summary:  fmt.Sprintf("Published the current DKIM keys for %s", updated.ZoneName),
			Details:  map[string]string{"dkim.selectors": selectorList(updated.DKIM)},
		})
	}
	return nil
}

// markZoneMissing records, once, that the zone behind a binding is gone. The
// row is kept so the operator can still unbind and free the mail domain.
func (s *Service) markZoneMissing(ctx context.Context, doc platform.MailDomainDoc) error {
	if doc.ZoneMissing {
		return nil
	}
	doc.ZoneMissing = true
	doc.State = StateDegraded
	doc.Reason = reasonZoneMissing
	doc.UpdatedAt = s.deps.Now()
	if err := s.deps.Store.PutMailDomain(ctx, doc); err != nil {
		return err
	}
	s.deps.Record(ctx, activity.Event{
		ZoneID:   doc.ZoneID,
		ZoneName: doc.ZoneName,
		Actor:    activity.ActorReconciler,
		Kind:     activity.KindMailZoneMissing,
		Severity: activity.SeverityWarn,
		Summary:  fmt.Sprintf("The zone behind the email binding for %s no longer exists", doc.ZoneName),
	})
	return nil
}

// sweepOrphans releases records this engine owns in a zone that is no longer
// bound — the residue of a crash between an unbind's DNS step and its row
// delete, or of a restore that brought a zone back without its binding.
//
// They are re-sourced as user records, never deleted: the operator's zone is
// not this facade's to prune once the binding is gone.
func (s *Service) sweepOrphans(ctx context.Context, bound map[string]struct{}) error {
	zones, err := s.deps.Zones.ListZones(ctx)
	if err != nil {
		return fmt.Errorf("list zones: %w", err)
	}
	for _, zoneValue := range zones {
		if _, ok := bound[zoneValue.ID]; ok {
			continue
		}
		var ops []enginedns.Op
		for _, record := range zoneValue.Records {
			if record.Source != zone.SourceMail {
				continue
			}
			released := record
			released.Source = zone.SourceUser
			ops = append(ops, enginedns.Op{Kind: enginedns.OpUpdate, RecordID: record.ID, Record: released})
		}
		if len(ops) == 0 {
			continue
		}
		err := s.deps.Serial.Run(ctx, zoneValue.ID, func(ctx context.Context) error {
			_, err := s.deps.Zones.ApplyRecordSet(ctx, zoneValue.ID, ops, activity.ActorReconciler, "")
			return err
		})
		if err != nil {
			s.deps.Log().Warn("release orphaned mail records", "zone", zoneValue.Name, "error", err)
			continue
		}
		s.deps.Record(ctx, activity.Event{
			ZoneID:   zoneValue.ID,
			ZoneName: zoneValue.Name,
			Actor:    activity.ActorReconciler,
			Kind:     activity.KindDNSEngineOrphaned,
			Severity: activity.SeverityWarn,
			Summary: fmt.Sprintf("%d email records in %s had no binding and are now editable",
				len(ops), zoneValue.Name),
			Details: map[string]string{"records": fmt.Sprint(len(ops))},
		})
	}
	return nil
}

// wakeAfter schedules one wake. It is used after a bind that could not wait for
// the RSA key: the reconciler publishes it as soon as the server has it.
func (s *Service) wakeAfter(delay time.Duration) {
	time.AfterFunc(delay, s.wake)
}

func dkimChanged(before, after []platform.DkimDoc) bool {
	if len(before) != len(after) {
		return true
	}
	for index := range before {
		if before[index].Selector != after[index].Selector || before[index].Stage != after[index].Stage {
			return true
		}
	}
	return false
}

func selectorList(keys []platform.DkimDoc) string {
	selectors := make([]string, 0, len(keys))
	for _, key := range keys {
		selectors = append(selectors, key.Selector)
	}
	return strings.Join(selectors, ",")
}

// jitter spreads the periodic passes by ±10 %, so several control planes
// restarted together do not all wake at the same instant.
func jitter(interval time.Duration) time.Duration {
	spread := int64(interval / 5)
	if spread <= 0 {
		return interval
	}
	return interval - time.Duration(spread/2) + time.Duration(rand.Int64N(spread))
}
