package simplemcp

import (
	"context"

	"connectrpc.com/connect"

	hostingv1 "github.com/castlemilk/dns/gen/go/hosting/v1"
)

// optionalZone resolves the domain only when the caller named one, for the
// tools whose zone filter is optional.
func (s *Server) optionalZone(ctx context.Context, a args) (zoneRef, error) {
	if !a.present("domain") && !a.present("zone_id") {
		return zoneRef{}, nil
	}
	return s.resolveZone(ctx, a)
}

func (s *Server) siteList(ctx context.Context, _ args) (any, error) {
	response, err := s.clients.Hosting.ListSites(ctx, connect.NewRequest(&hostingv1.ListSitesRequest{}))
	if err != nil {
		return nil, err
	}
	return view(response.Msg)
}

func (s *Server) siteShow(ctx context.Context, a args) (any, error) {
	zone, err := s.resolveZone(ctx, a)
	if err != nil {
		return nil, err
	}
	response, err := s.clients.Hosting.GetSite(ctx, connect.NewRequest(&hostingv1.GetSiteRequest{ZoneId: zone.ID}))
	if err != nil {
		return nil, err
	}
	return view(response.Msg)
}

func (s *Server) siteAttach(ctx context.Context, a args) (any, error) {
	zone, err := s.resolveZone(ctx, a)
	if err != nil {
		return nil, err
	}
	framework, err := frameworkArg(a, "framework", true)
	if err != nil {
		return nil, err
	}
	repository, err := stringArg(a, "repository", false)
	if err != nil {
		return nil, err
	}
	branch, err := stringArg(a, "branch", false)
	if err != nil {
		return nil, err
	}
	path, err := stringArg(a, "path", false)
	if err != nil {
		return nil, err
	}
	skipWww, err := boolArg(a, "skip_www", false)
	if err != nil {
		return nil, err
	}
	wwwMode, err := wwwModeArg(a, "www_mode", false)
	if err != nil {
		return nil, err
	}
	replace, err := boolArg(a, "replace_conflicting_records", false)
	if err != nil {
		return nil, err
	}
	dryRun, err := boolArg(a, "dry_run", false)
	if err != nil {
		return nil, err
	}
	response, err := s.clients.Hosting.AttachSite(ctx, connect.NewRequest(&hostingv1.AttachSiteRequest{
		ZoneId:                    zone.ID,
		Framework:                 framework,
		Repository:                repository,
		Branch:                    branch,
		Path:                      path,
		SkipWww:                   skipWww,
		WwwMode:                   wwwMode,
		ReplaceConflictingRecords: replace,
		DryRun:                    dryRun,
	}))
	if err != nil {
		return nil, err
	}
	return view(response.Msg)
}

func (s *Server) siteDetach(ctx context.Context, a args) (any, error) {
	zone, err := s.resolveZone(ctx, a)
	if err != nil {
		return nil, err
	}
	keepDNS, err := boolArg(a, "keep_dns_records", false)
	if err != nil {
		return nil, err
	}
	keepApp, err := boolArg(a, "keep_app", false)
	if err != nil {
		return nil, err
	}
	response, err := s.clients.Hosting.DetachSite(ctx, connect.NewRequest(&hostingv1.DetachSiteRequest{
		ZoneId: zone.ID, KeepDnsRecords: keepDNS, KeepApp: keepApp,
	}))
	if err != nil {
		return nil, err
	}
	return viewWith(response.Msg, map[string]any{"detached": true, "zone_id": zone.ID, "domain": zone.Name})
}

// siteDeploy takes no git token. A per-deploy credential handed to a model
// would be a secret in a transcript; private repositories deploy from the CLI,
// where the token can be given straight to the control plane.
func (s *Server) siteDeploy(ctx context.Context, a args) (any, error) {
	zone, err := s.resolveZone(ctx, a)
	if err != nil {
		return nil, err
	}
	repository, err := stringArg(a, "repository", false)
	if err != nil {
		return nil, err
	}
	revision, err := stringArg(a, "revision", false)
	if err != nil {
		return nil, err
	}
	path, err := stringArg(a, "path", false)
	if err != nil {
		return nil, err
	}
	framework, err := frameworkArg(a, "framework", false)
	if err != nil {
		return nil, err
	}
	uploadID, err := stringArg(a, "upload_id", false)
	if err != nil {
		return nil, err
	}
	response, err := s.clients.Hosting.CreateDeploy(ctx, connect.NewRequest(&hostingv1.CreateDeployRequest{
		ZoneId: zone.ID, Repository: repository, Revision: revision, Path: path,
		Framework: framework, UploadId: uploadID,
	}))
	if err != nil {
		return nil, err
	}
	return view(response.Msg)
}

func (s *Server) siteDeployList(ctx context.Context, a args) (any, error) {
	zone, err := s.optionalZone(ctx, a)
	if err != nil {
		return nil, err
	}
	limit, err := uint32Arg(a, "limit")
	if err != nil {
		return nil, err
	}
	cursor, err := stringArg(a, "cursor", false)
	if err != nil {
		return nil, err
	}
	response, err := s.clients.Hosting.ListDeploys(ctx, connect.NewRequest(&hostingv1.ListDeploysRequest{
		ZoneId: zone.ID, Limit: limit, Cursor: cursor,
	}))
	if err != nil {
		return nil, err
	}
	return view(response.Msg)
}

func (s *Server) siteRollback(ctx context.Context, a args) (any, error) {
	zone, err := s.resolveZone(ctx, a)
	if err != nil {
		return nil, err
	}
	deployID, err := stringArg(a, "deploy_id", true)
	if err != nil {
		return nil, err
	}
	response, err := s.clients.Hosting.RollbackSite(ctx, connect.NewRequest(&hostingv1.RollbackSiteRequest{
		ZoneId: zone.ID, DeployId: deployID,
	}))
	if err != nil {
		return nil, err
	}
	return view(response.Msg)
}

func (s *Server) siteLogs(ctx context.Context, a args) (any, error) {
	zone, err := s.resolveZone(ctx, a)
	if err != nil {
		return nil, err
	}
	deployID, err := stringArg(a, "deploy_id", false)
	if err != nil {
		return nil, err
	}
	tailLines, err := uint32Arg(a, "tail_lines")
	if err != nil {
		return nil, err
	}
	if deployID == "" {
		site, err := s.clients.Hosting.GetSite(ctx, connect.NewRequest(&hostingv1.GetSiteRequest{ZoneId: zone.ID}))
		if err != nil {
			return nil, err
		}
		deployID = site.Msg.GetSite().GetLatestDeployId()
		if deployID == "" {
			deployID = site.Msg.GetSite().GetLiveDeployId()
		}
		if deployID == "" {
			return nil, notFound("deploy_id: this site has no deploys yet, so there is no log to read")
		}
	}
	response, err := s.clients.Hosting.GetDeployLog(ctx, connect.NewRequest(&hostingv1.GetDeployLogRequest{
		ZoneId: zone.ID, DeployId: deployID, TailLines: tailLines,
	}))
	if err != nil {
		return nil, err
	}
	return viewWith(response.Msg, map[string]any{"deploy_id": deployID})
}

func (s *Server) siteEnvGet(ctx context.Context, a args) (any, error) {
	zone, err := s.resolveZone(ctx, a)
	if err != nil {
		return nil, err
	}
	environment, err := stringArg(a, "environment", false)
	if err != nil {
		return nil, err
	}
	response, err := s.clients.Hosting.ListSiteEnvVars(ctx, connect.NewRequest(&hostingv1.ListSiteEnvVarsRequest{
		ZoneId: zone.ID, Environment: environment,
	}))
	if err != nil {
		return nil, err
	}
	// `supported: false` means the engine could not be asked, not that the site
	// has none. It is on the wire either way; leave it there and say so.
	return viewWith(response.Msg, map[string]any{
		"values_available": false,
		"values_note":      "Values are write-only on this platform. Names, environments and last-set times are all there is to read.",
	})
}

func (s *Server) siteEnvSet(ctx context.Context, a args) (any, error) {
	zone, err := s.resolveZone(ctx, a)
	if err != nil {
		return nil, err
	}
	name, err := stringArg(a, "name", true)
	if err != nil {
		return nil, err
	}
	value, err := rawStringArg(a, "value", true)
	if err != nil {
		return nil, err
	}
	environment, err := stringArg(a, "environment", false)
	if err != nil {
		return nil, err
	}
	response, err := s.clients.Hosting.SetSiteEnvVar(ctx, connect.NewRequest(&hostingv1.SetSiteEnvVarRequest{
		ZoneId: zone.ID, Name: name, Value: value, Environment: environment,
	}))
	if err != nil {
		return nil, err
	}
	// The response carries the variable's name and stamp and no value, which is
	// the whole of what may be reported: the value went to the engine and is
	// not readable from here or anywhere else.
	return view(response.Msg)
}

func (s *Server) siteEnvUnset(ctx context.Context, a args) (any, error) {
	zone, err := s.resolveZone(ctx, a)
	if err != nil {
		return nil, err
	}
	name, err := stringArg(a, "name", true)
	if err != nil {
		return nil, err
	}
	environment, err := stringArg(a, "environment", false)
	if err != nil {
		return nil, err
	}
	response, err := s.clients.Hosting.DeleteSiteEnvVar(ctx, connect.NewRequest(&hostingv1.DeleteSiteEnvVarRequest{
		ZoneId: zone.ID, Name: name, Environment: environment,
	}))
	if err != nil {
		return nil, err
	}
	return viewWith(response.Msg, map[string]any{"deleted": true, "name": name})
}

func (s *Server) siteWww(ctx context.Context, a args) (any, error) {
	zone, err := s.resolveZone(ctx, a)
	if err != nil {
		return nil, err
	}
	mode, err := wwwModeArg(a, "mode", true)
	if err != nil {
		return nil, err
	}
	response, err := s.clients.Hosting.UpdateSite(ctx, connect.NewRequest(&hostingv1.UpdateSiteRequest{
		ZoneId: zone.ID, WwwMode: mode,
	}))
	if err != nil {
		return nil, err
	}
	return view(response.Msg)
}
