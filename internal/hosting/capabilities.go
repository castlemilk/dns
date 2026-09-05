package hosting

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"time"

	"connectrpc.com/connect"
	"github.com/castlemilk/dns/internal/platform"
)

// capabilityInterval is how often the facade re-asks what this server supports.
// A DeepHost upgrade is picked up within ten minutes without a restart.
const capabilityInterval = 10 * time.Minute

// capabilityProbeApp is the app name the ListBuilds capability probe uses. The
// probe has no side effects: ListBuilds on an app that does not exist answers
// with an empty list on a new server and Unimplemented on an old one, which is
// exactly the distinction being made.
const capabilityProbeApp = "capability-probe"

// capabilities reads the persisted capability set.
func (s *Service) capabilities(ctx context.Context) platform.CapabilitiesDoc {
	doc, err := s.deps.Store.GetCapabilities(ctx)
	if err != nil {
		return platform.CapabilitiesDoc{}
	}
	return doc
}

// updateCapabilities applies mutate to the stored set and persists it when
// something actually changed.
func (s *Service) updateCapabilities(ctx context.Context, mutate func(*platform.CapabilitiesDoc)) {
	doc, err := s.deps.Store.GetCapabilities(ctx)
	if err != nil {
		s.log().Warn("read hosting capabilities", "error", err)
		return
	}
	before := doc
	mutate(&doc)
	if doc == before {
		return
	}
	doc.V = platform.DocVersion
	doc.ProbedAt = s.now()
	if err := s.deps.Store.PutCapabilities(ctx, doc); err != nil {
		s.log().Warn("store hosting capabilities", "error", err)
	}
}

// runCapabilityProber probes at start and every ten minutes.
func (s *Service) runCapabilityProber(ctx context.Context) {
	s.probeCapabilities(ctx)
	for {
		timer := time.NewTimer(jitter(capabilityInterval))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		s.probeCapabilities(ctx)
	}
}

// probeCapabilities answers four questions without changing anything: does
// this server have ListBuilds, does it stamp build timestamps, does it report
// domain readiness, and does it have the environment-variable RPCs.
// DeleteDomain is answered lazily by the first detach, and domain_redirect and
// resolved_commit only ever turn true on evidence the engine reported one.
func (s *Service) probeCapabilities(ctx context.Context) {
	if !s.configured() {
		return
	}
	passCtx, cancel := context.WithTimeout(ctx, workerBudget)
	defer cancel()

	listBuilds := true
	if _, err := s.engine.ListBuilds(passCtx, s.cfg.Tenant, capabilityProbeApp, 1); err != nil {
		if isUnsupported(err) {
			listBuilds = false
		} else if isTransport(err) {
			// An unreachable engine says nothing about its version.
			return
		}
	}
	s.updateCapabilities(passCtx, func(doc *platform.CapabilitiesDoc) { doc.ListBuilds = listBuilds })

	s.probeBuildTimestamps(passCtx)
	s.probeDomainReadiness(passCtx)
	s.probeEnvVars(passCtx)
}

// probeEnvVars asks one attached site's app whether this server has the
// environment-variable RPCs. It reads; it never writes, so the probe cannot
// change a variable or create a Secret.
//
// It deliberately answers nothing when there is no site to ask against: with no
// app, Unimplemented and NotFound are not distinguishable in a way worth acting
// on, and the environment sheet only ever opens on a site anyway.
func (s *Service) probeEnvVars(ctx context.Context) {
	sites, err := s.deps.Store.ListSites(ctx)
	if err != nil {
		return
	}
	for _, site := range sites {
		if site.App == "" || site.State == SiteStateDetaching {
			continue
		}
		_, err := s.engine.ListAppEnvVars(ctx, s.cfg.Tenant, site.App, "")
		switch {
		case err == nil:
			s.updateCapabilities(ctx, func(doc *platform.CapabilitiesDoc) { doc.EnvVars = true })
		case isUnsupported(err):
			s.updateCapabilities(ctx, func(doc *platform.CapabilitiesDoc) { doc.EnvVars = false })
		default:
			// A transport failure or a missing app says nothing about the
			// server's version, so the flag is left as it was.
			continue
		}
		return
	}
}

// probeBuildTimestamps asks the newest build this facade knows about whether
// the server reports its creation time. Only a build created after an upgrade
// carries the operator's start/finish stamps, so a false here can turn true
// after the next deploy — which is why it is re-probed.
func (s *Service) probeBuildTimestamps(ctx context.Context) {
	deploys, _, err := s.deps.Store.ListDeploys(ctx, "", 20, "")
	if err != nil {
		return
	}
	for _, deploy := range deploys {
		if deploy.BuildID == "" {
			continue
		}
		site, err := s.deps.Store.GetSite(ctx, deploy.ZoneID)
		if err != nil {
			continue
		}
		build, err := s.engine.GetBuild(ctx, s.cfg.Tenant, site.App, deploy.BuildID)
		if err != nil {
			continue
		}
		reported := !build.CreatedAt.IsZero()
		s.updateCapabilities(ctx, func(doc *platform.CapabilitiesDoc) { doc.BuildTimestamps = reported })
		return
	}
}

// probeDomainReadiness asks one attached site's hosts whether the server
// reports readiness at all.
func (s *Service) probeDomainReadiness(ctx context.Context) {
	sites, err := s.deps.Store.ListSites(ctx)
	if err != nil {
		return
	}
	for _, site := range sites {
		if site.App == "" || site.State == SiteStateDetaching {
			continue
		}
		domains, err := s.engine.ListDomains(ctx, s.cfg.Tenant, site.App)
		if err != nil {
			continue
		}
		reported := false
		for _, domain := range domains {
			if domain.ReadinessReported {
				reported = true
				break
			}
		}
		if len(domains) == 0 {
			return
		}
		s.updateCapabilities(ctx, func(doc *platform.CapabilitiesDoc) { doc.DomainReadiness = reported })
		return
	}
}

// probeTenantScope reports whether the engine token is scoped to this tenant.
// A token that answers for a random tenant name is an admin token: the facade
// says so on Settings rather than pretending the blast radius is one tenant.
func (s *Service) probeTenantScope(ctx context.Context) {
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		return
	}
	probeTenant := "simple-scope-probe-" + hex.EncodeToString(suffix)
	_, err := s.engine.ListApps(ctx, probeTenant)
	scoped := err != nil && connect.CodeOf(err) == connect.CodePermissionDenied
	s.updateCapabilities(ctx, func(doc *platform.CapabilitiesDoc) { doc.TenantScoped = scoped })
}

// isTransport reports whether an error means "the engine did not answer", as
// opposed to "the engine answered no".
func isTransport(err error) bool {
	switch connect.CodeOf(err) {
	case connect.CodeUnavailable, connect.CodeDeadlineExceeded, connect.CodeCanceled, connect.CodeUnauthenticated:
		return true
	default:
		return false
	}
}
