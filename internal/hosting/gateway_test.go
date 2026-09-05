package hosting

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"
	hostingv1 "github.com/castlemilk/dns/gen/go/hosting/v1"
	"github.com/castlemilk/dns/internal/activity"
	"github.com/castlemilk/dns/internal/config"
	"github.com/castlemilk/dns/internal/platform"
)

// resolvedGateway builds a harness whose gateway comes from a scripted
// resolution rather than a DNS server.
func resolvedGateway(t *testing.T, answers ...[]string) (*harness, func()) {
	t.Helper()
	h := newHarness(t, func(cfg *config.Hosting) {
		cfg.GatewayAddresses = nil
		cfg.GatewayHostname = "gateway.local.test"
	})
	index := 0
	h.service.WithResolver(func(context.Context) ([]netip.Addr, error) {
		if index >= len(answers) {
			return parseAddresses(answers[len(answers)-1]), nil
		}
		answer := answers[index]
		index++
		return parseAddresses(answer), nil
	})
	return h, func() {
		if err := h.service.resolveGateway(context.Background()); err != nil {
			t.Fatalf("resolveGateway: %v", err)
		}
	}
}

func gatewayAddressesOf(t *testing.T, h *harness) []string {
	t.Helper()
	doc, err := h.store.GetGateway(context.Background())
	if err != nil {
		t.Fatalf("get gateway: %v", err)
	}
	return doc.Addresses
}

func TestGatewayFirstResolutionAppliesImmediately(t *testing.T) {
	t.Parallel()

	h, resolve := resolvedGateway(t, []string{"198.51.100.10"})
	resolve()
	if got := gatewayAddressesOf(t, h); !slices.Equal(got, []string{"198.51.100.10"}) {
		t.Fatalf("addresses = %v, want the first answer applied without hysteresis", got)
	}
}

func TestGatewaySingleCycleFlapChangesNothing(t *testing.T) {
	t.Parallel()

	h, resolve := resolvedGateway(t,
		[]string{"198.51.100.10"},
		[]string{"198.51.100.10", "198.51.100.11"},
		[]string{"198.51.100.10"},
	)
	resolve()
	resolve()
	resolve()
	if got := gatewayAddressesOf(t, h); !slices.Equal(got, []string{"198.51.100.10"}) {
		t.Fatalf("addresses = %v, want a single-cycle flap to be ignored", got)
	}
}

func TestGatewayOverlappingChangeAppliesAfterTwoIdenticalResolutions(t *testing.T) {
	t.Parallel()

	h, resolve := resolvedGateway(t,
		[]string{"198.51.100.10"},
		[]string{"198.51.100.10", "198.51.100.11"},
		[]string{"198.51.100.10", "198.51.100.11"},
	)
	resolve()
	resolve()
	if got := gatewayAddressesOf(t, h); !slices.Equal(got, []string{"198.51.100.10"}) {
		t.Fatalf("addresses = %v after one sighting, want the stored set unchanged", got)
	}
	resolve()
	want := []string{"198.51.100.10", "198.51.100.11"}
	if got := gatewayAddressesOf(t, h); !slices.Equal(got, want) {
		t.Fatalf("addresses = %v, want %v after the second sighting", got, want)
	}
	if !h.events.has(activity.KindHostingGatewayChanged) {
		t.Errorf("events = %v, want hosting.gateway.changed", h.events.kinds())
	}
}

func TestGatewayDisjointSetWaitsForAnOperator(t *testing.T) {
	t.Parallel()

	h, resolve := resolvedGateway(t,
		[]string{"198.51.100.10"},
		[]string{"203.0.113.7"},
		[]string{"203.0.113.7"},
		[]string{"203.0.113.7"},
	)
	resolve()
	resolve()
	resolve()

	doc, err := h.store.GetGateway(context.Background())
	if err != nil {
		t.Fatalf("get gateway: %v", err)
	}
	if !slices.Equal(doc.Addresses, []string{"198.51.100.10"}) {
		t.Fatalf("addresses = %v, want a disjoint set never applied on its own", doc.Addresses)
	}
	if !slices.Equal(doc.Pending, []string{"203.0.113.7"}) {
		t.Fatalf("pending = %v, want the disjoint set held", doc.Pending)
	}
	if doc.PendingSince.IsZero() {
		t.Error("pending_since is zero")
	}
	if !h.events.has(activity.KindHostingGatewayPending) {
		t.Errorf("events = %v, want hosting.gateway.pending", h.events.kinds())
	}

	// A confirmation for a different set is refused.
	_, err = h.service.ConfirmGatewayAddresses(context.Background(),
		connect.NewRequest(&hostingv1.ConfirmGatewayAddressesRequest{Addresses: []string{"203.0.113.8"}}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("code = %s, want invalid_argument for the wrong set", connect.CodeOf(err))
	}

	response, err := h.service.ConfirmGatewayAddresses(context.Background(),
		connect.NewRequest(&hostingv1.ConfirmGatewayAddressesRequest{Addresses: []string{"203.0.113.7"}}))
	if err != nil {
		t.Fatalf("ConfirmGatewayAddresses: %v", err)
	}
	if !slices.Equal(response.Msg.GetAddresses(), []string{"203.0.113.7"}) {
		t.Fatalf("confirmed addresses = %v", response.Msg.GetAddresses())
	}
	if got := gatewayAddressesOf(t, h); !slices.Equal(got, []string{"203.0.113.7"}) {
		t.Fatalf("addresses = %v after confirmation", got)
	}
	if !h.events.has(activity.KindHostingGatewayConfirmed) {
		t.Errorf("events = %v, want hosting.gateway.confirmed", h.events.kinds())
	}
}

func TestConfirmGatewayAddressesWithNothingPendingIsRefused(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.seedGateway(t, "198.51.100.10")
	_, err := h.service.ConfirmGatewayAddresses(context.Background(),
		connect.NewRequest(&hostingv1.ConfirmGatewayAddressesRequest{Addresses: []string{"198.51.100.10"}}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("code = %s, want failed_precondition", connect.CodeOf(err))
	}
}

func TestGatewayFilterDropsUnroutableAnswersInProductionOnly(t *testing.T) {
	t.Parallel()

	h, resolve := resolvedGateway(t, []string{"127.0.0.1", "10.0.0.5", "198.51.100.10"})
	h.service.production = true
	resolve()
	if got := gatewayAddressesOf(t, h); !slices.Equal(got, []string{"198.51.100.10"}) {
		t.Fatalf("addresses = %v, want only the global unicast answer in production", got)
	}

	// Outside production a loopback answer is exactly what a local operator
	// meant, so it survives.
	local, resolveLocal := resolvedGateway(t, []string{"127.0.0.1"})
	resolveLocal()
	if got := gatewayAddressesOf(t, local); !slices.Equal(got, []string{"127.0.0.1"}) {
		t.Fatalf("addresses = %v, want the loopback answer kept outside production", got)
	}
}

func TestGatewayRejectsAnAnswerOutsideTheAllowedCIDRs(t *testing.T) {
	t.Parallel()

	h := newHarness(t, func(cfg *config.Hosting) {
		cfg.GatewayAddresses = nil
		cfg.GatewayHostname = "gateway.local.test"
		cfg.GatewayAllowedCIDRs = []netip.Prefix{netip.MustParsePrefix("198.51.100.0/24")}
	})
	h.service.WithResolver(func(context.Context) ([]netip.Addr, error) {
		return parseAddresses([]string{"198.51.100.10", "203.0.113.7"}), nil
	})
	if err := h.service.resolveGateway(context.Background()); err != nil {
		t.Fatalf("resolveGateway: %v", err)
	}
	doc, err := h.store.GetGateway(context.Background())
	if err != nil {
		t.Fatalf("get gateway: %v", err)
	}
	if len(doc.Addresses) != 0 {
		t.Fatalf("addresses = %v, want the whole resolution rejected", doc.Addresses)
	}
	if doc.LastError == "" {
		t.Error("last_error is empty after a rejected resolution")
	}
	if !h.events.has(activity.KindHostingGatewayRejected) {
		t.Errorf("events = %v, want hosting.gateway.rejected", h.events.kinds())
	}
}

func TestGatewayFailureNeverUnpointsTheStoredSet(t *testing.T) {
	t.Parallel()

	h := newHarness(t, func(cfg *config.Hosting) {
		cfg.GatewayAddresses = nil
		cfg.GatewayHostname = "gateway.local.test"
	})
	h.seedGateway(t, "198.51.100.10")
	h.service.WithResolver(func(context.Context) ([]netip.Addr, error) {
		return nil, errors.New("lookup gateway.local.test: no such host")
	})
	for range gatewayFailureEvents {
		if err := h.service.resolveGateway(context.Background()); err != nil {
			t.Fatalf("resolveGateway: %v", err)
		}
	}
	doc, err := h.store.GetGateway(context.Background())
	if err != nil {
		t.Fatalf("get gateway: %v", err)
	}
	if !slices.Equal(doc.Addresses, []string{"198.51.100.10"}) {
		t.Fatalf("addresses = %v, want an outage to leave the published set alone", doc.Addresses)
	}
	if doc.Failures < gatewayFailureEvents {
		t.Errorf("failures = %d", doc.Failures)
	}
	if !h.events.has(activity.KindHostingGatewayResolveFailed) {
		t.Errorf("events = %v, want hosting.gateway.resolve_failed", h.events.kinds())
	}
}

func TestGatewayKeepsTheConfiguredStaticAddresses(t *testing.T) {
	t.Parallel()

	h := newHarness(t, func(cfg *config.Hosting) {
		cfg.GatewayHostname = "gateway.local.test"
		// cfg.GatewayAddresses keeps the harness's 198.51.100.10.
	})
	h.service.WithResolver(func(context.Context) ([]netip.Addr, error) {
		return parseAddresses([]string{"203.0.113.7"}), nil
	})
	for range 3 {
		if err := h.service.resolveGateway(context.Background()); err != nil {
			t.Fatalf("resolveGateway: %v", err)
		}
	}
	want := []string{"198.51.100.10", "203.0.113.7"}
	if got := gatewayAddressesOf(t, h); !slices.Equal(got, want) {
		t.Fatalf("addresses = %v, want %v — a static address is never removed", got, want)
	}
}

func TestGatewayChangeRewritesEveryAttachedApexAndKeepsUserRecords(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	zoneID := h.zoneID(t, "acme.dev")
	h.seedGateway(t, "198.51.100.10")
	attach(t, h, zoneID)

	// A user record at the apex is drift, not something a worker may delete.
	if _, err := h.zones.CreateRecord(context.Background(), zoneID, zoneRecordA("@", "203.0.113.9")); err != nil {
		t.Fatalf("seed user apex record: %v", err)
	}

	if err := h.store.PutGateway(context.Background(), platform.GatewayDoc{
		V: platform.DocVersion, Addresses: []string{"198.51.100.11"},
		Source: platform.GatewaySourceStatic, ResolvedAt: h.clock.Now(),
	}); err != nil {
		t.Fatalf("change gateway: %v", err)
	}
	if err := h.service.reconcileSiteDNS(context.Background(), zoneID); err != nil {
		t.Fatalf("reconcileSiteDNS: %v", err)
	}

	apex := map[string]string{}
	userSurvived := false
	for _, record := range h.records(t, zoneID) {
		if record.Name != "@" || record.Type != "A" {
			continue
		}
		apex[record.Value] = record.Source
		if record.Value == "203.0.113.9" && record.Source == "" {
			userSurvived = true
		}
	}
	if apex["198.51.100.11"] != "hosting" {
		t.Errorf("apex records = %v, want the new gateway address written by the engine", apex)
	}
	if _, stale := apex["198.51.100.10"]; stale {
		t.Errorf("apex records = %v, want the old gateway address removed", apex)
	}
	if !userSurvived {
		t.Error("the worker deleted a user record at the apex")
	}

	site := h.site(t, zoneID)
	if len(site.DNSProblems) == 0 {
		t.Error("dns_problems is empty; the drifted user record should be reported")
	}
	if site.DNSInSync {
		t.Error("dns_in_sync = true while a user record conflicts")
	}
}

func TestDriftDegradesTheSiteAndClearingItRestoresReady(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	zoneID := h.zoneID(t, "acme.dev")
	h.seedGateway(t, "198.51.100.10")
	attach(t, h, zoneID)
	h.service.watchDomains(context.Background())
	if state := h.site(t, zoneID).State; state != SiteStateReady {
		t.Fatalf("state = %q, want READY before the drift", state)
	}

	// A second apex A record someone added by hand is drift: the reconciler
	// reports it and never deletes it.
	drift, err := h.zones.CreateRecord(context.Background(), zoneID, zoneRecordA("@", "203.0.113.9"))
	if err != nil {
		t.Fatalf("seed drift: %v", err)
	}
	if err := h.service.reconcileSiteDNS(context.Background(), zoneID); err != nil {
		t.Fatalf("reconcileSiteDNS: %v", err)
	}
	site := h.site(t, zoneID)
	if site.State != SiteStateDegraded || len(site.DNSProblems) == 0 {
		t.Fatalf("site = %q / %v, want DEGRADED with the conflict reported", site.State, site.DNSProblems)
	}
	if site.DNSInSync {
		t.Error("dns_in_sync = true while a user record conflicts")
	}

	driftID := ""
	for _, record := range drift.Records {
		if record.Value == "203.0.113.9" {
			driftID = record.ID
		}
	}
	if driftID == "" {
		t.Fatal("the seeded drift record has no id")
	}
	if _, err := h.zones.DeleteRecord(context.Background(), zoneID, driftID); err != nil {
		t.Fatalf("remove the drift: %v", err)
	}
	if err := h.service.reconcileSiteDNS(context.Background(), zoneID); err != nil {
		t.Fatalf("reconcileSiteDNS: %v", err)
	}
	site = h.site(t, zoneID)
	if site.State != SiteStateReady || !site.DNSInSync {
		t.Fatalf("site = %q in_sync=%t, want READY once the drift is gone", site.State, site.DNSInSync)
	}
}

// Go fills net.DNSError.Server from the system configuration even when a
// custom Dial sent the query elsewhere. The reported reason must name the
// resolver HOSTING_GATEWAY_RESOLVER selected, or an operator debugs the wrong
// server.
func TestGatewayResolveErrorNamesTheConfiguredResolver(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.service.cfg.GatewayHostname = "gateway.local.test"
	h.service.cfg.GatewayResolver = "127.0.0.1:1053"

	systemErr := &net.DNSError{Err: "no such host", Name: "gateway.local.test", Server: "192.168.0.1:53", IsNotFound: true}
	got := h.service.resolveErrorMessage(systemErr)
	if strings.Contains(got, "192.168.0.1:53") {
		t.Errorf("the reason names a server that was never queried: %q", got)
	}
	if !strings.Contains(got, "127.0.0.1:1053") || !strings.Contains(got, "no such host") {
		t.Errorf("resolveErrorMessage = %q, want the configured resolver and the underlying reason", got)
	}

	// With no resolver configured the system's own text is the honest one.
	h.service.cfg.GatewayResolver = ""
	if got := h.service.resolveErrorMessage(systemErr); got != systemErr.Error() {
		t.Errorf("resolveErrorMessage = %q, want the raw error %q", got, systemErr.Error())
	}
}
