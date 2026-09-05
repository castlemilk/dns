package hosting

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestAHostFreedByAnotherSiteIsRegisteredOnTheSlowCadence covers the one
// refusal the watcher stops retrying. Stopping the fast retry is right — the
// same call seconds later can only fail the same way — but the other site can
// be detached, and nothing in the facade cleared the refusal, so the site
// stayed DEGRADED naming a host that was free with only a detach and re-attach
// to recover and no copy that said so.
func TestAHostFreedByAnotherSiteIsRegisteredOnTheSlowCadence(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := newHarness(t)
	zoneID := h.zoneID(t, "acme.dev")
	h.seedGateway(t, "198.51.100.10")
	h.engine.SeedForeignHost("www.acme.dev", "other-tenant-site")

	attach(t, h, zoneID)
	h.service.watchDomains(ctx)
	site := h.site(t, zoneID)
	if site.State != SiteStateDegraded || !strings.Contains(site.Reason, "already served by another site") {
		t.Fatalf("site = %q / %q, want DEGRADED naming the taken host", site.State, site.Reason)
	}

	// The other site is detached and the host is free again.
	h.engine.ReleaseForeignHost("www.acme.dev")

	// The fast cadence still does not hammer the engine with a call that only
	// just failed.
	before := countCalls(h.engine.Calls(), "CreateDomain")
	h.clock.Advance(time.Minute)
	h.service.watchDomains(ctx)
	if after := countCalls(h.engine.Calls(), "CreateDomain"); after != before {
		t.Errorf("CreateDomain was retried %d times a minute after the refusal", after-before)
	}

	// The slow cadence asks again, and the site recovers on its own.
	h.clock.Advance(hostRetakeInterval)
	h.service.watchDomains(ctx)

	site = h.site(t, zoneID)
	for _, host := range site.Hostnames {
		if host.Role != HostRoleWWW {
			continue
		}
		if host.State == HostStateMissing {
			t.Errorf("www host = %+v, want it registered once the host was free", host)
		}
		if strings.Contains(host.Reason, "already served by another site") {
			t.Errorf("www reason = %q, want the stale refusal cleared", host.Reason)
		}
	}
	if site.State != SiteStateReady || site.Reason != "" {
		t.Errorf("site = %q / %q, want READY with no reason", site.State, site.Reason)
	}
}
