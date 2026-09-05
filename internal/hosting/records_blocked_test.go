package hosting

import (
	"context"
	"strings"
	"testing"

	"github.com/castlemilk/dns/internal/activity"
	"github.com/castlemilk/dns/internal/zone"
)

// TestARestoreThatBlocksOneRecordReportsOnlyWhatItWrote is the honesty
// regression for the planner's `blocked` operations. plan.Create describes the
// whole desired set, but WorkerOps leaves out every operation a standing
// conflict makes unappliable, so counting plan.Create claimed records that
// never reached the zone.
//
// The shape: a restore brings back the zone without the engine's records and
// with a user CNAME at www. The apex A can be written; the www CNAME cannot,
// because a CNAME may not share a name with any other record. Exactly one
// record is created, and the activity line has to say one.
func TestARestoreThatBlocksOneRecordReportsOnlyWhatItWrote(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	zoneID := h.zoneID(t, "acme.dev")
	h.seedGateway(t, "198.51.100.10")
	attach(t, h, zoneID)
	if got := len(h.engineRecords(t, zoneID)); got != 2 {
		t.Fatalf("engine records = %d, want the apex A and the www CNAME", got)
	}

	// The restore: an old provider's zone file, imported over the top. Every
	// engine record is gone, every restored record comes back as the
	// operator's, and one of them is a CNAME at www.
	restored := []zone.Record{{
		Name: "www", Type: zone.TypeCNAME, TTL: zone.DefaultRecordTTL,
		Value: "oldhost.provider.example.",
	}}
	for _, record := range h.records(t, zoneID) {
		if record.Managed || record.Source != "" {
			continue
		}
		restored = append(restored, zone.Record{
			Name: record.Name, Type: record.Type, TTL: record.TTL, Value: record.Value,
		})
	}
	if _, err := h.zones.ImportZone(
		context.Background(), "acme.dev", restored, zone.ImportReplace, false,
	); err != nil {
		t.Fatalf("import the restored zone: %v", err)
	}
	if got := len(h.engineRecords(t, zoneID)); got != 0 {
		t.Fatalf("engine records = %d after a REPLACE import, want none", got)
	}

	h.events.events = nil
	if err := h.service.reconcileSiteDNS(context.Background(), zoneID); err != nil {
		t.Fatalf("reconcileSiteDNS: %v", err)
	}

	// One record reached the zone: the apex the site needs. The blocked www
	// CNAME must not have rolled the apex back with it.
	owned := h.engineRecords(t, zoneID)
	if len(owned) != 1 || owned[0].Name != "@" || owned[0].Type != zone.TypeA {
		t.Fatalf("engine records = %+v, want only the apex A", owned)
	}

	var summary string
	for _, event := range h.events.all() {
		if event.Kind == activity.KindDNSEngineRestored {
			summary = event.Summary
		}
	}
	if summary == "" {
		t.Fatal("no dns.engine.restored event for the record that was written")
	}
	if !strings.Contains(summary, "Re-created 1 website record(s)") {
		t.Errorf("summary = %q, want it to count only the record that was written", summary)
	}

	// And the site says why it is not whole.
	site := h.site(t, zoneID)
	if len(site.DNSProblems) == 0 || site.DNSInSync {
		t.Errorf("site = %+v, want the standing conflict reported", site)
	}
}
