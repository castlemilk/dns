package hosting

import (
	"context"
	"fmt"
	"time"

	"github.com/castlemilk/dns/internal/activity"
	"github.com/castlemilk/dns/internal/platform"
)

// Domain watcher timings.
const (
	// watcherFastInterval is used while any site is still settling.
	watcherFastInterval = 30 * time.Second
	// watcherSlowInterval is the steady-state cadence once every host reports
	// a settled state.
	watcherSlowInterval = 10 * time.Minute
	// watcherFastWindow is how long after an attach the fast cadence applies.
	// A certificate that has not been issued in two hours is not going to be
	// issued by polling harder.
	watcherFastWindow = 2 * time.Hour
	// hostnameFailureLimit is how many consecutive registration failures mark
	// the site degraded while the watcher keeps retrying.
	hostnameFailureLimit = 10
	// detachDrainTimeout bounds how long DetachSite waits inline for the
	// engine to stop listing the site's hosts. It must stay well under
	// newHTTPServer's 30 s WriteTimeout (internal/app/app.go): a longer wait
	// costs the caller its response even though the detach succeeded. When the
	// engine is still collecting at the deadline the RPC answers pending=true
	// and the domain watcher finishes the row.
	detachDrainTimeout = 20 * time.Second
	// detachDrainPoll is the inline polling cadence during that wait.
	detachDrainPoll = 2 * time.Second
	// hostRetakeInterval is how long a host the engine reported as belonging to
	// another app waits before the watcher asks again. The other site can be
	// detached, so the refusal is not permanent — only slow to change.
	hostRetakeInterval = watcherSlowInterval
)

// runDomainWatcher registers hosts, tracks their readiness and finishes
// detaches the engine is still garbage-collecting.
func (s *Service) runDomainWatcher(ctx context.Context) {
	for {
		interval := watcherSlowInterval
		if s.watcherBusy(ctx) {
			interval = watcherFastInterval
		}
		timer := time.NewTimer(jitter(interval))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.watcherWake:
			timer.Stop()
		case <-timer.C:
		}
		passCtx, cancel := context.WithTimeout(ctx, workerBudget)
		s.watchDomains(passCtx)
		cancel()
	}
}

// pokeWatcher asks for a pass now. It never blocks.
func (s *Service) pokeWatcher() {
	select {
	case s.watcherWake <- struct{}{}:
	default:
	}
}

// watcherBusy reports whether any site still needs the fast cadence.
func (s *Service) watcherBusy(ctx context.Context) bool {
	sites, err := s.deps.Store.ListSites(ctx)
	if err != nil {
		return false
	}
	now := s.now()
	for _, site := range sites {
		switch site.State {
		case SiteStateAttaching, SiteStateDetaching:
			return true
		}
		if now.Sub(site.CreatedAt) > watcherFastWindow {
			continue
		}
		for _, host := range site.Hostnames {
			if !hostSettled(host.State) {
				return true
			}
		}
	}
	return false
}

// watchDomains is one pass over every site.
func (s *Service) watchDomains(ctx context.Context) {
	if !s.configured() {
		return
	}
	sites, err := s.deps.Store.ListSites(ctx)
	if err != nil {
		s.log().Warn("list sites for the domain watcher", "error", err)
		return
	}
	for _, site := range sites {
		if ctx.Err() != nil {
			return
		}
		switch site.State {
		case SiteStateDetaching:
			s.finishDetachIfDrained(ctx, site)
		case SiteStateAttaching, SiteStateReady, SiteStateDegraded:
			if site.ZoneMissing && site.State != SiteStateAttaching {
				continue
			}
			if err := s.watchSite(ctx, site); err != nil {
				s.log().Warn("watch site hostnames", "error", err)
			}
		}
	}
}

// watchSite registers the hosts a site still needs and refreshes what the
// engine reports about the ones it has.
func (s *Service) watchSite(ctx context.Context, doc platform.SiteDoc) error {
	desired := s.desiredHosts(doc)
	doc.Hostnames = mergeHostnames(desired, doc.Hostnames)

	for index := range doc.Hostnames {
		host := &doc.Hostnames[index]
		if host.State != HostStateMissing || !s.hostNeedsRegistration(*host) {
			continue
		}
		secret := ""
		if host.Role == HostRoleAuto {
			secret = s.cfg.AppsTLSSecret
		}
		err := s.engine.CreateDomain(ctx, s.cfg.Tenant, doc.App, DomainSpec{
			Host:       host.Host,
			TLSSecret:  secret,
			RedirectTo: redirectTargetFor(doc, host.Role),
		})
		switch {
		case err == nil:
			host.State = HostStateRegistered
			host.Reason = ""
			host.Retrying = false
			host.Failures = 0
			host.ObservedAt = s.now()
			s.deps.Record(ctx, activity.Event{
				ZoneID: doc.ZoneID, ZoneName: doc.ZoneName, Actor: activity.ActorHostingEngine,
				Kind: activity.KindSiteHostnameRegistered, Severity: activity.SeverityInfo,
				Summary: fmt.Sprintf("Registered %s with the hosting engine", host.Host),
			})
		case isHostTaken(err):
			// Another site on the platform already serves this host. Retrying
			// can only ever fail the same way, so the watcher stops.
			host.Reason = fmt.Sprintf(CopyHostTaken, host.Host)
			host.Retrying = false
			host.ObservedAt = s.now()
			doc.State = SiteStateDegraded
			doc.Reason = host.Reason
		default:
			host.Failures++
			host.Retrying = true
			host.Reason = reason(s.mapEngineError(err).Error())
			host.ObservedAt = s.now()
			if host.Failures >= hostnameFailureLimit {
				doc.State = SiteStateDegraded
				doc.Reason = host.Reason
			}
		}
	}

	domains, err := s.engine.ListDomains(ctx, s.cfg.Tenant, doc.App)
	if err != nil && !isNotFound(err) {
		return err
	}
	byHost := make(map[string]DomainView, len(domains))
	readinessReported := false
	redirectReported := false
	for _, domain := range domains {
		byHost[domain.Host] = domain
		if domain.ReadinessReported {
			readinessReported = true
		}
		if domain.RedirectTo != "" {
			redirectReported = true
		}
	}
	if len(domains) > 0 {
		s.updateCapabilities(ctx, func(caps *platform.CapabilitiesDoc) { caps.DomainReadiness = readinessReported })
	}
	if redirectReported {
		// An engine without the field and an engine with nothing to redirect
		// are indistinguishable, so this flag only ever turns true on evidence
		// that the engine reported a redirect it was asked for.
		s.updateCapabilities(ctx, func(caps *platform.CapabilitiesDoc) { caps.DomainRedirect = true })
	}

	registered := 0
	for index := range doc.Hostnames {
		host := &doc.Hostnames[index]
		view, listed := byHost[host.Host]
		if !listed {
			if host.State != HostStateMissing {
				host.State = HostStateMissing
				host.Retrying = true
				host.ObservedAt = s.now()
			}
			continue
		}
		registered++
		s.reconcileRedirect(ctx, doc, *host, view)
		previous := host.State
		host.State = hostnameState(view)
		host.TLSMode = view.TLSMode
		host.RedirectTo = view.RedirectTo
		host.Reason = reason(view.Reason)
		host.Retrying = false
		host.Failures = 0
		host.ObservedAt = s.now()
		if previous != host.State && hostSettled(host.State) {
			s.deps.Record(ctx, activity.Event{
				ZoneID: doc.ZoneID, ZoneName: doc.ZoneName, Actor: activity.ActorHostingEngine,
				Kind: activity.KindSiteHostnameReady, Severity: activity.SeverityInfo,
				Summary: fmt.Sprintf("%s is serving on the hosting engine", host.Host),
			})
		}
	}

	if registered == len(doc.Hostnames) && doc.State == SiteStateAttaching {
		doc.State = SiteStateReady
		doc.Reason = ""
		s.deps.Record(ctx, activity.Event{
			ZoneID: doc.ZoneID, ZoneName: doc.ZoneName, Actor: activity.ActorHostingEngine,
			Kind: activity.KindSiteReady, Severity: activity.SeverityInfo,
			Summary: fmt.Sprintf("%s is attached and every host is registered", doc.ZoneName),
		})
	}
	// A site degraded by a host the engine refused returns to READY as soon as
	// that host registers, rather than carrying the old refusal as its reason
	// until the next record reconcile.
	s.applyDNSProblems(&doc)
	touch(&doc, s.now())
	return s.deps.Store.PutSite(ctx, doc)
}

// reconcileRedirect repairs a host whose redirect mode drifted from what the
// site asked for. DeepHost has no UpdateDomain: CreateDomain on an existing
// host is the update, so one more call is the whole repair.
//
// It deliberately does nothing when the engine reports no redirect and none has
// ever been reported for this engine. A server without the field accepts the
// call and reports nothing back, so acting on that difference would re-register
// every www host on every pass, for ever, and change nothing.
func (s *Service) reconcileRedirect(
	ctx context.Context,
	doc platform.SiteDoc,
	host platform.HostnameDoc,
	view DomainView,
) {
	wanted := redirectTargetFor(doc, host.Role)
	if view.RedirectTo == wanted {
		return
	}
	if view.RedirectTo == "" && !s.capabilities(ctx).DomainRedirect {
		return
	}
	secret := ""
	if host.Role == HostRoleAuto {
		// The whole spec is re-sent, so the platform's shared certificate has
		// to go with it or the repair would quietly move that host onto a
		// per-host certificate.
		secret = s.cfg.AppsTLSSecret
	}
	err := s.engine.CreateDomain(ctx, s.cfg.Tenant, doc.App, DomainSpec{
		Host:       host.Host,
		TLSSecret:  secret,
		RedirectTo: wanted,
	})
	if err != nil {
		s.log().Warn("reapply the redirect mode of a host", "error", err)
	}
}

// hostNeedsRegistration reports whether the watcher should try CreateDomain
// again.
//
// A host the engine said belongs to another app is the one case that stops the
// fast retry: retrying seconds later can only fail the same way. It is still
// re-attempted on the slow cadence, because the site that holds the host can be
// detached and nothing else in the facade would ever clear the refusal — the
// site would otherwise stay DEGRADED naming a host that is free, for ever, with
// only a detach and re-attach to recover and no copy that says so.
func (s *Service) hostNeedsRegistration(host platform.HostnameDoc) bool {
	if host.Reason == "" || host.Retrying {
		return true
	}
	return host.ObservedAt.IsZero() || s.now().Sub(host.ObservedAt) >= hostRetakeInterval
}

// finishDetachIfDrained deletes a DETACHING row once the engine stops listing
// the site's hosts. The DNS records were already released by DetachSite.
func (s *Service) finishDetachIfDrained(ctx context.Context, doc platform.SiteDoc) {
	drained, err := s.hostsDrained(ctx, doc)
	if err != nil {
		s.log().Warn("check detached site hosts", "error", err)
		return
	}
	if !drained && !s.appGone(ctx, doc) {
		return
	}
	s.retireDeploys(ctx, doc)
	if err := s.deps.Store.DeleteSite(ctx, doc.ZoneID); err != nil {
		s.log().Warn("delete detached site", "error", err)
		return
	}
	s.deps.Record(ctx, activity.Event{
		ZoneID: doc.ZoneID, ZoneName: doc.ZoneName, Actor: activity.ActorHostingEngine,
		Kind: activity.KindSiteDetached, Severity: activity.SeverityInfo,
		Summary: fmt.Sprintf("%s is no longer hosted here", doc.ZoneName),
	})
}

// hostsDrained reports whether the engine still lists any of the site's hosts.
// A NotFound app is drained.
func (s *Service) hostsDrained(ctx context.Context, doc platform.SiteDoc) (bool, error) {
	domains, err := s.engine.ListDomains(ctx, s.cfg.Tenant, doc.App)
	if err != nil {
		if isNotFound(err) {
			return true, nil
		}
		return false, err
	}
	wanted := make(map[string]struct{}, len(doc.Hostnames))
	for _, host := range doc.Hostnames {
		wanted[host.Host] = struct{}{}
	}
	for _, domain := range domains {
		if _, ok := wanted[domain.Host]; ok {
			return false, nil
		}
	}
	return true, nil
}

// appGone reports that the engine no longer has the app, which ends the wait
// even though the hosts are still listed. DeepHost does not garbage-collect
// Domain CRs with the App (verified on the kind cluster: GetApp answers
// NotFound while ListDomains still returns every host), so without this a row
// whose hosts could not be removed would sit in DETACHING for ever. It is
// deliberately checked only here, in the background pass, so the RPC itself
// still reports pending honestly while the engine is mid-removal.
func (s *Service) appGone(ctx context.Context, doc platform.SiteDoc) bool {
	_, err := s.engine.GetApp(ctx, s.cfg.Tenant, doc.App)
	return err != nil && isNotFound(err)
}
