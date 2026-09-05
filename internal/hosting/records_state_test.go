package hosting

import (
	"context"
	"errors"
	"testing"

	"github.com/castlemilk/dns/internal/activity"
	"github.com/castlemilk/dns/internal/platform"
	"github.com/castlemilk/dns/internal/zone"
)

// TestAFailedRecordWriteDoesNotLeaveTheSiteClaimingItsRecords is the honesty
// rule applied to the site row: what platform.db says about DNS has to be an
// observation of the zone, never the last write that happened to succeed. A
// reconcile whose ApplyRecordSet fails used to return before the zone was
// re-read, so the row kept dns_in_sync=true and the apex it published before
// the failure while the zone answered something else.
func TestAFailedRecordWriteDoesNotLeaveTheSiteClaimingItsRecords(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := newHarness(t)
	zoneID := h.zoneID(t, "acme.dev")
	h.seedGateway(t, "198.51.100.10")
	attach(t, h, zoneID)
	h.service.watchDomains(ctx)

	site := h.site(t, zoneID)
	if site.State != SiteStateReady || !site.DNSInSync {
		t.Fatalf("site = %q / in-sync %v, want a READY site in sync", site.State, site.DNSInSync)
	}

	// The gateway moved, so the reconcile has real work; the zone write fails.
	h.seedGateway(t, "203.0.113.10")
	h.faults.failApply(errors.New("the zone store is unavailable"))

	if err := h.service.reconcileSiteDNS(ctx, zoneID); err == nil {
		t.Fatal("reconcileSiteDNS reported success though the zone write failed")
	}

	site = h.site(t, zoneID)
	if site.DNSInSync {
		t.Error("dns_in_sync stayed true after a write that never reached the zone")
	}
	if site.State != SiteStateDegraded {
		t.Errorf("state = %q, want DEGRADED", site.State)
	}
	if site.Reason == "" {
		t.Error("reason is empty, want the row to name why the records are not in place")
	}
	if !contains(site.ApexAddresses, "198.51.100.10") {
		t.Errorf("apex = %v, want what the zone still answers", site.ApexAddresses)
	}
	if contains(site.ApexAddresses, "203.0.113.10") {
		t.Errorf("apex = %v, want no address the write never published", site.ApexAddresses)
	}
}

// TestOrphanSweepDoesNotReleaseTheRecordsOfASiteAttachedWhileItRan covers the
// window between the sweep's two reads. AttachSite holds the zone's gate across
// both its PutSite and its ApplyRecordSet, so a site attached after the sweep
// listed sites is invisible to it; the sweep has to re-read inside the gate
// before it releases anything.
func TestOrphanSweepDoesNotReleaseTheRecordsOfASiteAttachedWhileItRan(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := newHarness(t)
	zoneID := h.zoneID(t, "acme.dev")
	h.seedGateway(t, "198.51.100.10")

	// The zone already carries this engine's records with no site row, which is
	// exactly what the sweep exists to release.
	attach(t, h, zoneID)
	if err := h.store.DeleteSite(ctx, zoneID); err != nil {
		t.Fatalf("delete site row: %v", err)
	}
	before := len(h.engineRecords(t, zoneID))
	if before == 0 {
		t.Fatal("the attach left no engine records to sweep")
	}

	// The attach lands between the sweep's site read and its zone read.
	h.faults.duringListZones(func() {
		doc := platform.SiteDoc{
			V: platform.DocVersion, ZoneID: zoneID, ZoneName: "acme.dev",
			App: AppName("simple-test", "acme.dev"), State: SiteStateReady,
			CreatedAt: h.clock.Now(), UpdatedAt: h.clock.Now(),
		}
		if err := h.store.PutSite(ctx, doc); err != nil {
			t.Errorf("re-attach the site: %v", err)
		}
	})

	if err := h.service.sweepOrphans(ctx); err != nil {
		t.Fatalf("sweepOrphans: %v", err)
	}

	if after := len(h.engineRecords(t, zoneID)); after != before {
		t.Errorf("engine records = %d, want the %d of an attached site left alone", after, before)
	}
	if h.events.has(activity.KindDNSEngineOrphaned) {
		t.Errorf("events = %v, want no orphan warning about an attached site", h.events.kinds())
	}
}

// TestDeletingAUserRecordWakesTheRecordReconciler guards the observer's filter.
// Handler.DeleteRecord refuses every record an engine owns before it notifies,
// so the record a notification carries always has an empty source; a filter on
// SourceHosting made the whole body unreachable and left a site DEGRADED for up
// to a full reconcile interval after the operator removed the very record that
// was standing in the way of the plan.
func TestDeletingAUserRecordWakesTheRecordReconciler(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	zoneID := h.zoneID(t, "acme.dev")

	h.service.Observer().RecordDeleted(context.Background(), zoneID, zone.Record{
		Name: "@", Type: zone.TypeA, TTL: zone.DefaultRecordTTL, Value: "203.0.113.9",
	})

	if queued := h.service.takePendingZones(); len(queued) != 1 || queued[0] != zoneID {
		t.Fatalf("pending zones = %v, want the zone whose record was deleted", queued)
	}
}
