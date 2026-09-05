package mail

import (
	"context"
	"fmt"
	"strings"

	"github.com/castlemilk/dns/internal/activity"
	"github.com/castlemilk/dns/internal/platform"
	"github.com/castlemilk/dns/internal/zone"
)

// Rebuild reconstructs the mail_domains rows from the engine after platform.db
// was lost.
//
// The engine is the truth for which domains exist; this control plane's own
// domains are recognisable by their description ("simple zone <zone_id>"), so a
// domain an operator created by hand in the Stalwart UI is never adopted. A
// domain whose zone no longer exists is reported as a warning and skipped: a
// row pointing at a missing zone would offer an unbind that cannot write DNS.
//
// It runs inside the control plane, so no engine credential leaves the pod.
func (s *Service) Rebuild(ctx context.Context, dryRun bool) (platform.RebuildReport, error) {
	if !s.cfg.Configured() || s.engine == nil {
		return platform.RebuildReport{}, nil
	}
	domains, err := observeEngine(ctx, s, "list_mail_domains", s.engine.ListDomains)
	if err != nil {
		return platform.RebuildReport{}, err
	}
	zones, err := s.deps.Zones.ListZones(ctx)
	if err != nil {
		return platform.RebuildReport{}, fmt.Errorf("list zones: %w", err)
	}
	byID := make(map[string]zone.Zone, len(zones))
	for _, value := range zones {
		byID[value.ID] = value
	}

	report := platform.RebuildReport{}
	now := s.deps.Now()
	for _, domain := range domains {
		zoneID, ok := strings.CutPrefix(domain.Description, descriptionPrefix)
		if !ok || zoneID == "" {
			continue
		}
		zoneValue, ok := byID[zoneID]
		if !ok {
			report.Warnings = append(report.Warnings,
				fmt.Sprintf("mail domain %s has no matching zone", domain.Name))
			continue
		}
		if !strings.EqualFold(zoneValue.Name, domain.Name) {
			report.Warnings = append(report.Warnings,
				fmt.Sprintf("mail domain %s does not match the zone it names", domain.Name))
			continue
		}
		report.Count++
		if dryRun {
			continue
		}
		// The row is written in the BINDING state on purpose: the reconciler's
		// next pass reads the DKIM keys and the zone file and fills in the rest,
		// so the rebuild never has to guess what is published.
		doc := platform.MailDomainDoc{
			V:              1,
			ZoneID:         zoneValue.ID,
			ZoneName:       zoneValue.Name,
			EngineDomainID: domain.ID,
			State:          StateBinding,
			DmarcPolicy:    s.cfg.DMARCPolicy,
			ReportAddress:  s.reportAddress(zoneValue.Name),
			// The autoconfiguration switch is not stored on the engine, so it
			// is read back off the zone: a binding that has those records
			// published had it on. Guessing either way would have the
			// reconciler add records nobody asked for, or delete records
			// somebody did.
			PublishClientAutoconfig: autoconfigPublished(zoneValue),
			CreatedAt:               firstNonZero(domain.CreatedAt, now),
			UpdatedAt:               now,
			BindStartedAt:           &now,
		}
		if err := s.deps.Store.PutMailDomain(ctx, doc); err != nil {
			return report, err
		}
	}
	if !dryRun && report.Count > 0 {
		s.wake()
		s.deps.Record(ctx, activity.Event{
			Actor:    activity.ActorSystem,
			Kind:     activity.KindMailDomainBound,
			Severity: activity.SeverityWarn,
			Summary:  fmt.Sprintf("Recovered %d email bindings from the mail engine", report.Count),
			Details:  map[string]string{"mail_domains": fmt.Sprint(report.Count)},
		})
	}
	return report, nil
}

func firstNonZero[T interface{ IsZero() bool }](values ...T) T {
	var zeroValue T
	for _, value := range values {
		if !value.IsZero() {
			return value
		}
	}
	return zeroValue
}
