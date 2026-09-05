package hosting

import (
	"context"
	"fmt"
	"slices"

	platformv1 "github.com/castlemilk/dns/gen/go/platform/v1"
	"github.com/castlemilk/dns/internal/platform"
)

// Kind identifies this engine in the platform status and the rebuild report.
func (s *Service) Kind() platformv1.EngineKind { return platformv1.EngineKind_ENGINE_KIND_HOSTING }

// Rebuild reconstructs the `sites` rows from the engine after platform.db was
// lost. It works because app names are a pure function of tenant and zone name
// (§4.1): the facade can ask "does an app for this zone exist?" without any
// stored state. Deploy history and activity are not recoverable and the report
// says so.
//
// It runs inside the control plane, so no engine credential leaves the pod.
func (s *Service) Rebuild(ctx context.Context, dryRun bool) (platform.RebuildReport, error) {
	report := platform.RebuildReport{}
	if !s.configured() {
		return report, nil
	}

	apps, err := s.engine.ListApps(ctx, s.cfg.Tenant)
	if err != nil {
		return report, s.mapEngineError(err)
	}
	byName := make(map[string]AppView, len(apps))
	for _, app := range apps {
		byName[app.Name] = app
	}

	zones, err := s.deps.Zones.ListZones(ctx)
	if err != nil {
		return report, err
	}

	for _, value := range zones {
		app := AppName(s.cfg.Tenant, value.Name)
		view, ok := byName[AppRef(s.cfg.Tenant, app)]
		if !ok {
			continue
		}
		// The registered hosts are read before the row is built: a redirecting
		// www host is deliberately absent from App.spec.domains (it is not in
		// the router's map), so AppView.Domains alone would rebuild the site as
		// if it had no www host at all and the reconciler would then delete the
		// www record of a site that is serving.
		wwwHost := "www." + value.Name
		listed := map[string]DomainView{}
		domains, listErr := s.engine.ListDomains(ctx, s.cfg.Tenant, app)
		if listErr == nil {
			for _, domain := range domains {
				listed[domain.Host] = domain
			}
		} else {
			report.Warnings = append(report.Warnings,
				fmt.Sprintf("the hosting engine did not list the hosts of %s", value.Name))
		}
		_, wwwListed := listed[wwwHost]

		doc := platform.SiteDoc{
			V:         platform.DocVersion,
			ZoneID:    value.ID,
			ZoneName:  value.Name,
			App:       app,
			AppRef:    view.Name,
			Framework: string(view.Framework),
			// Repository and branch are engine metadata, so they survive the
			// loss of platform.db.
			Repository: view.Repository,
			Branch:     view.Branch,
			State:      SiteStateReady,
			WWW:        wwwListed || slices.Contains(view.Domains, wwwHost),
			WWWMode:    WWWModeServe,
			CreatedAt:  s.now(),
			UpdatedAt:  s.now(),
		}
		// The mode is recovered from what the engine reports rather than
		// guessed: a host it says redirects, redirects.
		if listed[wwwHost].RedirectTo != "" {
			doc.WWWMode = WWWModeRedirect
		}
		if s.cfg.AppsSuffix != "" {
			doc.AutoHostname = view.Name + "." + s.cfg.AppsSuffix
		}
		doc.Hostnames = s.desiredHosts(doc)

		for index := range doc.Hostnames {
			domain, ok := listed[doc.Hostnames[index].Host]
			if !ok {
				continue
			}
			doc.Hostnames[index].State = hostnameState(domain)
			doc.Hostnames[index].TLSMode = domain.TLSMode
			doc.Hostnames[index].RedirectTo = domain.RedirectTo
			doc.Hostnames[index].ObservedAt = s.now()
		}

		report.Count++
		if dryRun {
			continue
		}
		if err := s.deps.Store.PutSite(ctx, doc); err != nil {
			return report, err
		}
		if err := s.reconcileSiteDNS(ctx, doc.ZoneID); err != nil {
			report.Warnings = append(report.Warnings,
				fmt.Sprintf("the website records of %s were not re-adopted yet", value.Name))
		}
	}

	if report.Count > 0 {
		report.Warnings = append(report.Warnings,
			"deploy history and build logs are not recoverable; the next deploy repopulates them")
	}
	return report, nil
}
