package hosting

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/castlemilk/dns/internal/activity"
	"github.com/castlemilk/dns/internal/platform"
)

// Gateway reconciler timings.
const (
	gatewayInterval       = 60 * time.Second
	gatewayResolveTimeout = 3 * time.Second
	// gatewayEventInterval bounds how often a repeated failure or rejection is
	// recorded, so a resolver that is down for a day writes 24 events, not
	// 1 440.
	gatewayEventInterval = time.Hour
	// gatewayFailureEvents is how many consecutive failures pass before the
	// first warn event: a single flap is not worth an operator's attention.
	gatewayFailureEvents = 10
	// gatewayHysteresis is how many consecutive identical resolutions a new
	// set needs before it replaces the stored one.
	gatewayHysteresis = 2
)

// gatewayAddresses returns the address set every attached apex points at.
func (s *Service) gatewayAddresses(ctx context.Context) ([]netip.Addr, error) {
	doc, err := s.deps.Store.GetGateway(ctx)
	if err != nil {
		return nil, err
	}
	return parseAddresses(doc.Addresses), nil
}

func parseAddresses(values []string) []netip.Addr {
	addresses := make([]netip.Addr, 0, len(values))
	for _, value := range values {
		address, err := netip.ParseAddr(value)
		if err != nil {
			continue
		}
		addresses = append(addresses, address.Unmap())
	}
	return addresses
}

func formatAddresses(addresses []netip.Addr) []string {
	values := make([]string, 0, len(addresses))
	for _, address := range addresses {
		values = append(values, address.Unmap().String())
	}
	slices.Sort(values)
	return slices.Compact(values)
}

// staticAddresses is the configured set, which is never removed from the stored
// set by any resolution.
func (s *Service) staticAddresses() []string {
	return formatAddresses(s.cfg.GatewayAddresses)
}

// ResolveGatewayOnce runs one resolution synchronously. internal/app calls it
// before HTTP starts serving so the first AttachSite after a restart never
// fails "gateway addresses unknown".
func (s *Service) ResolveGatewayOnce(ctx context.Context) error {
	if !s.configured() {
		return nil
	}
	return s.resolveGateway(ctx)
}

// runGatewayReconciler is the 60 s loop. It writes only the gateway document
// and pokes the site DNS reconciler; it never writes DNS itself, so a zone has
// exactly one writer.
func (s *Service) runGatewayReconciler(ctx context.Context) {
	for {
		timer := time.NewTimer(jitter(gatewayInterval))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.gatewayWake:
			timer.Stop()
		case <-timer.C:
		}
		passCtx, cancel := context.WithTimeout(ctx, workerBudget)
		if err := s.resolveGateway(passCtx); err != nil {
			s.log().Warn("resolve gateway addresses", "error", err)
		}
		cancel()
	}
}

// pokeGateway asks for a resolution now (AttachSite does this while the stored
// set is empty). It never blocks.
func (s *Service) pokeGateway() {
	select {
	case s.gatewayWake <- struct{}{}:
	default:
	}
}

// resolveGateway is one pass of spec2 §3.5.
func (s *Service) resolveGateway(ctx context.Context) error {
	doc, err := s.deps.Store.GetGateway(ctx)
	if err != nil {
		return err
	}

	static := s.staticAddresses()
	if s.cfg.GatewayHostname == "" {
		if slices.Equal(doc.Addresses, static) && doc.Source == platform.GatewaySourceStatic {
			s.deps.Meter().GatewayResolution(ctx, "unchanged")
			return nil
		}
		return s.applyGateway(ctx, doc, static, platform.GatewaySourceStatic, "")
	}

	resolved, err := s.resolve(ctx)
	if err != nil {
		return s.recordResolveFailure(ctx, doc, s.resolveErrorMessage(err), "error")
	}
	if len(resolved) == 0 {
		return s.recordResolveFailure(ctx, doc, "the gateway hostname resolved to no addresses", "empty")
	}

	filtered, dropped := s.filterAddresses(resolved)
	if dropped > 0 {
		s.deps.Meter().GatewayResolution(ctx, "filtered")
	}
	if len(filtered) == 0 {
		return s.recordResolveFailure(ctx, doc, "every resolved gateway address was filtered out", "empty")
	}
	if !s.withinAllowedCIDRs(filtered) {
		s.deps.Meter().GatewayResolution(ctx, "rejected")
		s.recordGatewayEvent(ctx, &s.gatewayRejectedAt, activity.KindHostingGatewayRejected, activity.SeverityWarn,
			"A resolved gateway address fell outside HOSTING_GATEWAY_ALLOWED_CIDRS and was not applied", nil)
		doc.LastError = "a resolved address fell outside HOSTING_GATEWAY_ALLOWED_CIDRS"
		doc.Failures++
		doc.V = platform.DocVersion
		return s.deps.Store.PutGateway(ctx, doc)
	}

	candidate := unionAddresses(formatAddresses(filtered), static)
	doc.LastError = ""
	doc.Failures = 0

	// Nothing to overlap with: the first good answer after a fresh start is
	// applied straight away, or a site could never be attached.
	if len(doc.Addresses) == 0 {
		return s.applyGateway(ctx, doc, candidate, platform.GatewaySourceResolved, "")
	}
	if slices.Equal(candidate, doc.Addresses) {
		doc.Candidate = nil
		doc.CandidateSeen = 0
		doc.ResolvedAt = s.now()
		doc.V = platform.DocVersion
		s.deps.Meter().GatewayResolution(ctx, "unchanged")
		return s.deps.Store.PutGateway(ctx, doc)
	}

	if slices.Equal(candidate, doc.Candidate) {
		doc.CandidateSeen++
	} else {
		doc.Candidate = candidate
		doc.CandidateSeen = 1
	}
	if doc.CandidateSeen < gatewayHysteresis {
		doc.V = platform.DocVersion
		s.deps.Meter().GatewayResolution(ctx, "success")
		return s.deps.Store.PutGateway(ctx, doc)
	}

	// A set that shares no address with the current one is never applied on
	// its own: one hijacked answer would otherwise repoint every customer
	// apex. Configured static addresses are the exception — an operator wrote
	// them down.
	if overlaps(candidate, doc.Addresses) || subsetOf(candidate, static) {
		return s.applyGateway(ctx, doc, candidate, platform.GatewaySourceResolved, "")
	}

	if !slices.Equal(doc.Pending, candidate) {
		doc.Pending = candidate
		doc.PendingSince = s.now()
		s.deps.Meter().GatewayResolution(ctx, "pending")
		s.recordGatewayEvent(ctx, nil, activity.KindHostingGatewayPending, activity.SeverityWarn,
			"The gateway hostname resolved to a completely different address set; confirm it on Settings before it is applied",
			map[string]string{"pending": strings.Join(candidate, ", ")})
	}
	doc.V = platform.DocVersion
	return s.deps.Store.PutGateway(ctx, doc)
}

// resolveErrorMessage names the resolver that was actually queried. Go fills
// net.DNSError.Server from /etc/resolv.conf even when a custom Dial sent the
// query elsewhere, so the raw text would send an operator debugging a server
// this control plane never spoke to.
func (s *Service) resolveErrorMessage(err error) string {
	if s.cfg.GatewayResolver == "" {
		return err.Error()
	}
	var dnsErr *net.DNSError
	if !errors.As(err, &dnsErr) || dnsErr.Server == "" || dnsErr.Server == s.cfg.GatewayResolver {
		return err.Error()
	}
	return fmt.Sprintf("lookup %s on %s: %s", s.cfg.GatewayHostname, s.cfg.GatewayResolver, dnsErr.Err)
}

// recordResolveFailure never changes the stored set: an outage must not
// un-point every customer's apex.
func (s *Service) recordResolveFailure(ctx context.Context, doc platform.GatewayDoc, message, outcome string) error {
	doc.LastError = reason(message)
	doc.Failures++
	doc.V = platform.DocVersion
	s.deps.Meter().GatewayResolution(ctx, outcome)
	if doc.Failures >= gatewayFailureEvents {
		s.recordGatewayEvent(ctx, &s.gatewayFailedAt, activity.KindHostingGatewayResolveFailed, activity.SeverityWarn,
			"The gateway hostname has not resolved for several minutes; the published addresses are unchanged", nil)
	}
	return s.deps.Store.PutGateway(ctx, doc)
}

// applyGateway stores a new set, records the change once and pokes the site
// DNS reconciler for every site that publishes an apex.
func (s *Service) applyGateway(
	ctx context.Context,
	doc platform.GatewayDoc,
	addresses []string,
	source, confirmedBy string,
) error {
	previous := append([]string(nil), doc.Addresses...)
	doc.V = platform.DocVersion
	doc.Addresses = addresses
	doc.Source = source
	doc.ResolvedAt = s.now()
	doc.Candidate = nil
	doc.CandidateSeen = 0
	doc.Pending = nil
	doc.PendingSince = time.Time{}
	doc.LastError = ""
	doc.Failures = 0
	if err := s.deps.Store.PutGateway(ctx, doc); err != nil {
		return err
	}

	queued := s.pokeAttachedSites(ctx)
	if slices.Equal(previous, addresses) {
		s.deps.Meter().GatewayResolution(ctx, "unchanged")
		return nil
	}
	s.deps.Meter().GatewayResolution(ctx, "changed")

	kind := activity.KindHostingGatewayChanged
	summary := "The website gateway addresses changed; every attached apex is being rewritten"
	if confirmedBy != "" {
		kind = activity.KindHostingGatewayConfirmed
		summary = "An operator confirmed the new website gateway addresses"
	}
	s.deps.Record(ctx, activity.Event{
		Actor: activity.ActorHostingEngine, Kind: kind, Severity: activity.SeverityInfo,
		Summary: summary,
		Details: map[string]string{
			"old":   strings.Join(previous, ", "),
			"new":   strings.Join(addresses, ", "),
			"sites": fmt.Sprintf("%d", queued),
		},
	})
	return nil
}

// pokeAttachedSites queues a DNS reconcile for every site that publishes an
// apex, and reports how many were queued.
func (s *Service) pokeAttachedSites(ctx context.Context) int {
	sites, err := s.deps.Store.ListSites(ctx)
	if err != nil {
		s.log().Warn("list sites after gateway change", "error", err)
		return 0
	}
	queued := 0
	for _, site := range sites {
		switch site.State {
		case SiteStateAttaching, SiteStateReady, SiteStateDegraded:
		default:
			continue
		}
		if site.ZoneMissing {
			continue
		}
		queued++
		s.pokeRecords(site.ZoneID)
	}
	return queued
}

// lookupGateway resolves the gateway hostname. Go's resolver cannot validate
// DNSSEC, so HOSTING_GATEWAY_RESOLVER only selects which resolver answers; the
// filters and the overlap rule are the actual defence.
func (s *Service) lookupGateway(ctx context.Context) ([]netip.Addr, error) {
	lookupCtx, cancel := context.WithTimeout(ctx, gatewayResolveTimeout)
	defer cancel()

	resolver := &net.Resolver{PreferGo: true}
	if s.cfg.GatewayResolver != "" {
		server := s.cfg.GatewayResolver
		resolver.Dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
			dialer := net.Dialer{Timeout: gatewayResolveTimeout}
			return dialer.DialContext(ctx, network, server)
		}
	}
	addresses, err := resolver.LookupNetIP(lookupCtx, "ip", s.cfg.GatewayHostname)
	if err != nil {
		return nil, err
	}
	return addresses, nil
}

// filterAddresses drops what must never be published. In production only
// global unicast addresses survive; elsewhere (kind, a laptop) a loopback
// answer is exactly what an operator meant, so only the unspecified address is
// dropped.
func (s *Service) filterAddresses(addresses []netip.Addr) ([]netip.Addr, int) {
	kept := make([]netip.Addr, 0, len(addresses))
	dropped := 0
	for _, address := range addresses {
		address = address.Unmap()
		switch {
		case !address.IsValid(), address.IsUnspecified():
			dropped++
		case s.production && !routable(address):
			dropped++
		default:
			kept = append(kept, address)
		}
	}
	return kept, dropped
}

func routable(address netip.Addr) bool {
	return address.IsGlobalUnicast() && !address.IsPrivate() && !address.IsLoopback() &&
		!address.IsUnspecified() && !address.IsLinkLocalUnicast()
}

func (s *Service) withinAllowedCIDRs(addresses []netip.Addr) bool {
	if len(s.cfg.GatewayAllowedCIDRs) == 0 {
		return true
	}
	for _, address := range addresses {
		inside := false
		for _, prefix := range s.cfg.GatewayAllowedCIDRs {
			if prefix.Contains(address) {
				inside = true
				break
			}
		}
		if !inside {
			return false
		}
	}
	return true
}

func unionAddresses(left, right []string) []string {
	joined := append(append([]string(nil), left...), right...)
	slices.Sort(joined)
	return slices.Compact(joined)
}

func overlaps(left, right []string) bool {
	for _, value := range left {
		if slices.Contains(right, value) {
			return true
		}
	}
	return false
}

func subsetOf(values, set []string) bool {
	if len(set) == 0 {
		return false
	}
	for _, value := range values {
		if !slices.Contains(set, value) {
			return false
		}
	}
	return true
}

// recordGatewayEvent rate-limits a repeated warning to at most one per hour.
// last is nil for events that are already deduplicated by their content.
func (s *Service) recordGatewayEvent(
	ctx context.Context,
	last *time.Time,
	kind activity.Kind,
	severity activity.Severity,
	summary string,
	details map[string]string,
) {
	now := s.now()
	if last != nil {
		s.mu.Lock()
		if !last.IsZero() && now.Sub(*last) < gatewayEventInterval {
			s.mu.Unlock()
			return
		}
		*last = now
		s.mu.Unlock()
	}
	s.deps.Record(ctx, activity.Event{
		Actor: activity.ActorHostingEngine, Kind: kind, Severity: severity,
		Summary: summary, Details: details,
	})
}
