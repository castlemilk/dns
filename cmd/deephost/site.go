package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

	"connectrpc.com/connect"

	hostingv1 "github.com/castlemilk/dns/gen/go/hosting/v1"
	platformv1 "github.com/castlemilk/dns/gen/go/platform/v1"
	"github.com/castlemilk/dns/internal/deephostcli/client"
)

// envGitToken carries a private repository's token into `deephost site deploy`
// without putting it in the process list.
const envGitToken = "DEEPHOST_GIT_TOKEN" //nolint:gosec // the name of a variable, not a credential

func cmdSite(ctx context.Context, e *env, o *options, args []string) error {
	if len(args) == 0 {
		return &usageError{message: "site: choose a command", usage: siteUsage}
	}
	switch args[0] {
	case "list", "ls":
		return cmdSiteList(ctx, e, o, args[1:])
	case "show", "get":
		return cmdSiteShow(ctx, e, o, args[1:])
	case "attach":
		return cmdSiteAttach(ctx, e, o, args[1:])
	case "detach":
		return cmdSiteDetach(ctx, e, o, args[1:])
	case "deploy":
		return cmdSiteDeploy(ctx, e, o, args[1:])
	case "deploys":
		return cmdSiteDeploys(ctx, e, o, args[1:])
	case "rollback":
		return cmdSiteRollback(ctx, e, o, args[1:])
	case "logs":
		return cmdSiteLogs(ctx, e, o, args[1:])
	case "env":
		return cmdSiteEnv(ctx, e, o, args[1:])
	case "update":
		return cmdSiteUpdate(ctx, e, o, args[1:])
	case "www":
		return cmdSiteWww(ctx, e, o, args[1:])
	case "reapply-dns":
		return cmdSiteReapplyDNS(ctx, e, o, args[1:])
	case "gateway":
		return cmdSiteGateway(ctx, e, o, args[1:])
	case "help", "-h", "--help":
		return &helpError{usage: siteUsage}
	default:
		return &usageError{message: fmt.Sprintf("unknown site command %q", args[0]), usage: siteUsage}
	}
}

func cmdSiteList(ctx context.Context, e *env, o *options, args []string) error {
	fs, err := o.parse("deephost site list", siteUsage, args, nil)
	if err != nil {
		return err
	}
	if err := noExtraArgs(fs, 0, siteUsage); err != nil {
		return err
	}
	clients, err := o.connect(e)
	if err != nil {
		return err
	}
	callCtx, cancel := o.call(ctx)
	defer cancel()
	response, err := clients.Hosting.ListSites(callCtx, connect.NewRequest(&hostingv1.ListSitesRequest{}))
	if err != nil {
		return fail(clients, err)
	}
	sites := response.Msg
	return newPrinter(e, o).emit(sites, func(w io.Writer) error {
		if len(sites.GetSites()) == 0 {
			return writeLine(w, "no sites yet; attach one with `deephost site attach DOMAIN --framework static`")
		}
		table := newTable(w, "DOMAIN", "STATE", "FRAMEWORK", "WWW", "DNS", "LIVE DEPLOY", "REASON")
		for _, site := range sites.GetSites() {
			table.row(
				site.GetZoneName(),
				label(site.GetState().String(), "SITE_STATE_"),
				label(site.GetFramework().String(), "FRAMEWORK_"),
				label(site.GetWwwMode().String(), "WWW_MODE_"),
				dnsSummary(site),
				orDash(site.GetLiveDeployId()),
				orDash(site.GetReason()),
			)
		}
		return table.flush()
	})
}

func dnsSummary(site *hostingv1.Site) string {
	if site.GetZoneMissing() {
		return "zone missing"
	}
	if site.GetDns().GetInSync() {
		return "in sync"
	}
	if problems := site.GetDns().GetProblems(); len(problems) > 0 {
		return problems[0]
	}
	return "out of sync"
}

func cmdSiteShow(ctx context.Context, e *env, o *options, args []string) error {
	fs, err := o.parse("deephost site show", siteUsage, args, nil)
	if err != nil {
		return err
	}
	reference, err := argument(fs, 0, "domain", siteUsage)
	if err != nil {
		return err
	}
	if err := noExtraArgs(fs, 1, siteUsage); err != nil {
		return err
	}
	clients, err := o.connect(e)
	if err != nil {
		return err
	}
	site, err := lookupSite(ctx, o, clients, reference)
	if err != nil {
		return err
	}
	return newPrinter(e, o).emit(site, func(w io.Writer) error {
		return writeSite(w, site)
	})
}

func writeSite(w io.Writer, site *hostingv1.Site) error {
	if err := fields(w,
		pair("domain", site.GetZoneName()),
		pair("state", label(site.GetState().String(), "SITE_STATE_")),
		pair("reason", orDash(site.GetReason())),
		pair("framework", label(site.GetFramework().String(), "FRAMEWORK_")),
		pair("app", orDash(site.GetApp())),
		pair("repository", orDash(site.GetRepository())),
		pair("branch", orDash(site.GetBranch())),
		pair("path", orDash(site.GetPath())),
		pair("www", label(site.GetWwwMode().String(), "WWW_MODE_")),
		pair("auto hostname", orDash(site.GetAutoHostname())),
		pair("dns", dnsSummary(site)),
		pair("live deploy", orDash(site.GetLiveDeployId())),
		pair("latest deploy", orDash(site.GetLatestDeployId())),
	); err != nil {
		return err
	}
	if hostnames := site.GetHostnames(); len(hostnames) > 0 {
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
		table := newTable(w, "HOSTNAME", "ROLE", "STATE", "TLS", "REDIRECT TO", "REASON")
		for _, hostname := range hostnames {
			table.row(
				hostname.GetHost(),
				label(hostname.GetRole().String(), "HOSTNAME_ROLE_"),
				label(hostname.GetState().String(), "HOSTNAME_STATE_"),
				orDash(hostname.GetTlsMode()),
				orDash(hostname.GetRedirectTo()),
				orDash(hostname.GetReason()),
			)
		}
		if err := table.flush(); err != nil {
			return err
		}
	}
	return writeList(w, "dns problems", site.GetDns().GetProblems())
}

func cmdSiteAttach(ctx context.Context, e *env, o *options, args []string) error {
	var framework, repository, branch, path, wwwMode string
	var skipWww, replaceConflicting, dryRun bool
	fs, err := o.parse("deephost site attach", siteUsage, args, func(fs *flag.FlagSet) {
		fs.StringVar(&framework, "framework", "", "static, node or nextjs")
		fs.StringVar(&repository, "repository", "", "https://github.com/owner/name")
		fs.StringVar(&branch, "branch", "", "default revision to deploy (default main)")
		fs.StringVar(&path, "path", "", "sub-directory of the repository to build")
		fs.StringVar(&wwwMode, "www-mode", "", "serve or redirect for www.<domain>")
		fs.BoolVar(&skipWww, "skip-www", false, "do not register www.<domain> at all")
		fs.BoolVar(&replaceConflicting, "replace-conflicting", false, "replace records that block the site's own")
		fs.BoolVar(&dryRun, "dry-run", false, "show the DNS plan and write nothing")
	})
	if err != nil {
		return err
	}
	reference, err := argument(fs, 0, "domain", siteUsage)
	if err != nil {
		return err
	}
	if err := noExtraArgs(fs, 1, siteUsage); err != nil {
		return err
	}
	if strings.TrimSpace(framework) == "" {
		return usagef("--framework: is required (static, node or nextjs)")
	}
	parsedFramework, err := parseFramework(framework)
	if err != nil {
		return err
	}
	parsedWww, err := parseWwwMode(wwwMode)
	if err != nil {
		return err
	}
	if parsedWww == hostingv1.WwwMode_WWW_MODE_REDIRECT && skipWww {
		return usagef("--www-mode: redirect needs the www hostname, so it cannot be used with --skip-www")
	}
	clients, err := o.connect(e)
	if err != nil {
		return err
	}
	zone, err := lookupZoneWithTimeout(ctx, o, clients, reference)
	if err != nil {
		return err
	}
	callCtx, cancel := o.call(ctx)
	defer cancel()
	response, err := clients.Hosting.AttachSite(callCtx, connect.NewRequest(&hostingv1.AttachSiteRequest{
		ZoneId:                    zone.GetId(),
		Framework:                 parsedFramework,
		Repository:                repository,
		Branch:                    branch,
		Path:                      path,
		SkipWww:                   skipWww,
		ReplaceConflictingRecords: replaceConflicting,
		DryRun:                    dryRun,
		WwwMode:                   parsedWww,
	}))
	if err != nil {
		return fail(clients, err)
	}
	result := response.Msg
	return newPrinter(e, o).emit(result, func(w io.Writer) error {
		if result.GetDryRun() {
			if err := writeLine(w, "dry run: nothing was written"); err != nil {
				return err
			}
		} else if err := writeSite(w, result.GetSite()); err != nil {
			return err
		}
		if err := writeChanges(w, "dns plan", result.GetDnsPlan()); err != nil {
			return err
		}
		return writeChanges(w, "conflicts", result.GetConflicts())
	})
}

func cmdSiteDetach(ctx context.Context, e *env, o *options, args []string) error {
	var keepDNS, keepApp bool
	fs, err := o.parse("deephost site detach", siteUsage, args, func(fs *flag.FlagSet) {
		fs.BoolVar(&keepDNS, "keep-dns", false, "keep the engine's records as ordinary user records")
		fs.BoolVar(&keepApp, "keep-app", false, "leave the hosting app and its releases in place")
	})
	if err != nil {
		return err
	}
	reference, err := argument(fs, 0, "domain", siteUsage)
	if err != nil {
		return err
	}
	if err := noExtraArgs(fs, 1, siteUsage); err != nil {
		return err
	}
	clients, err := o.connect(e)
	if err != nil {
		return err
	}
	site, err := lookupSite(ctx, o, clients, reference)
	if err != nil {
		return err
	}
	question := fmt.Sprintf("Detach the site from %s? It stops being served.", site.GetZoneName())
	if !keepApp {
		question = fmt.Sprintf("Detach the site from %s? The app and its releases are deleted.", site.GetZoneName())
	}
	if err := confirm(e, o, question); err != nil {
		return err
	}
	callCtx, cancel := o.call(ctx)
	defer cancel()
	response, err := clients.Hosting.DetachSite(callCtx, connect.NewRequest(&hostingv1.DetachSiteRequest{
		ZoneId: site.GetZoneId(), KeepDnsRecords: keepDNS, KeepApp: keepApp,
	}))
	if err != nil {
		return fail(clients, err)
	}
	result := response.Msg
	return newPrinter(e, o).emit(result, func(w io.Writer) error {
		if err := fields(w,
			pair("detached", site.GetZoneName()),
			pair("pending", yesNo(result.GetPending())),
			pair("note", orDash(result.GetNote())),
		); err != nil {
			return err
		}
		return writeList(w, "orphaned hosts", result.GetOrphanedHosts())
	})
}

func cmdSiteDeploy(ctx context.Context, e *env, o *options, args []string) error {
	var repository, revision, path, framework, uploadID string
	var gitTokenStdin bool
	fs, err := o.parse("deephost site deploy", siteUsage, args, func(fs *flag.FlagSet) {
		fs.StringVar(&repository, "repository", "", "repository to build (default the site's)")
		fs.StringVar(&revision, "revision", "", "branch, tag or commit (default the site's branch)")
		fs.StringVar(&path, "path", "", "sub-directory to build (default the site's)")
		fs.StringVar(&framework, "framework", "", "override the site's framework")
		fs.StringVar(&uploadID, "upload-id", "", "id from an upload, instead of a git build")
		fs.BoolVar(&gitTokenStdin, "git-token-stdin", false, "read a private repository token from stdin")
	})
	if err != nil {
		return err
	}
	reference, err := argument(fs, 0, "domain", siteUsage)
	if err != nil {
		return err
	}
	if err := noExtraArgs(fs, 1, siteUsage); err != nil {
		return err
	}
	request := &hostingv1.CreateDeployRequest{
		Repository: repository,
		Revision:   revision,
		Path:       path,
		UploadId:   uploadID,
	}
	if strings.TrimSpace(framework) != "" {
		parsedFramework, err := parseFramework(framework)
		if err != nil {
			return err
		}
		request.Framework = parsedFramework
	}
	// A token is never a flag: argv is world-readable on a shared machine.
	if gitTokenStdin {
		token, err := readSecretValue(e, "git token", "")
		if err != nil {
			return err
		}
		request.GitToken = token
	} else if token := strings.TrimSpace(e.getenv(envGitToken)); token != "" {
		request.GitToken = token
	}
	if request.GetUploadId() != "" && (request.GetRepository() != "" || request.GetRevision() != "" || request.GetGitToken() != "") {
		return usagef("--upload-id: cannot be combined with --repository, --revision or a git token")
	}
	clients, err := o.connect(e)
	if err != nil {
		return err
	}
	site, err := lookupSite(ctx, o, clients, reference)
	if err != nil {
		return err
	}
	request.ZoneId = site.GetZoneId()
	callCtx, cancel := o.call(ctx)
	defer cancel()
	response, err := clients.Hosting.CreateDeploy(callCtx, connect.NewRequest(request))
	if err != nil {
		return fail(clients, err)
	}
	deploy := response.Msg.GetDeploy()
	return newPrinter(e, o).emit(response.Msg, func(w io.Writer) error {
		if err := writeDeploy(w, deploy); err != nil {
			return err
		}
		return writeLine(w, "\nfollow it with `deephost site logs "+site.GetZoneName()+" --deploy-id "+deploy.GetId()+"`")
	})
}

func cmdSiteDeploys(ctx context.Context, e *env, o *options, args []string) error {
	var limit uint
	var cursor, deployID string
	fs, err := o.parse("deephost site deploys", siteUsage, args, func(fs *flag.FlagSet) {
		fs.UintVar(&limit, "limit", 0, "how many deploys to list (default 20, max 100)")
		fs.StringVar(&cursor, "cursor", "", "continue from the last id of the previous page")
		fs.StringVar(&deployID, "deploy-id", "", "show this one deploy instead of the list")
	})
	if err != nil {
		return err
	}
	if err := noExtraArgs(fs, 1, siteUsage); err != nil {
		return err
	}
	clients, err := o.connect(e)
	if err != nil {
		return err
	}
	request := &hostingv1.ListDeploysRequest{Limit: uint32(limit), Cursor: cursor} //nolint:gosec // bounded server-side
	if fs.NArg() == 1 {
		site, err := lookupSite(ctx, o, clients, fs.Arg(0))
		if err != nil {
			return err
		}
		request.ZoneId = site.GetZoneId()
	}
	if strings.TrimSpace(deployID) != "" {
		if request.GetZoneId() == "" {
			return usagef("--deploy-id: name the domain too, because a deploy id is scoped to one site")
		}
		callCtx, cancel := o.call(ctx)
		defer cancel()
		one, err := clients.Hosting.GetDeploy(callCtx, connect.NewRequest(&hostingv1.GetDeployRequest{
			ZoneId: request.GetZoneId(), DeployId: deployID,
		}))
		if err != nil {
			return fail(clients, err)
		}
		return newPrinter(e, o).emit(one.Msg, func(w io.Writer) error {
			return writeDeploy(w, one.Msg.GetDeploy())
		})
	}
	callCtx, cancel := o.call(ctx)
	defer cancel()
	response, err := clients.Hosting.ListDeploys(callCtx, connect.NewRequest(request))
	if err != nil {
		return fail(clients, err)
	}
	deploys := response.Msg
	return newPrinter(e, o).emit(deploys, func(w io.Writer) error {
		if len(deploys.GetDeploys()) == 0 {
			return writeLine(w, "no deploys")
		}
		table := newTable(w, "DEPLOY ID", "DOMAIN", "KIND", "PHASE", "LIVE", "REVISION", "REQUESTED", "REASON")
		for _, deploy := range deploys.GetDeploys() {
			table.row(
				deploy.GetId(),
				deploy.GetZoneName(),
				label(deploy.GetKind().String(), "DEPLOY_KIND_"),
				label(deploy.GetPhase().String(), "DEPLOY_PHASE_"),
				yesNo(deploy.GetLive()),
				orDash(deployRevision(deploy)),
				formatTime(deploy.GetRequestedAt()),
				orDash(deploy.GetReason()),
			)
		}
		if err := table.flush(); err != nil {
			return err
		}
		if cursor := deploys.GetNextCursor(); cursor != "" {
			return writeLine(w, "\nmore: --cursor "+cursor)
		}
		return nil
	})
}

func deployRevision(deploy *hostingv1.Deploy) string {
	if resolved := deploy.GetRevisionResolved(); resolved != "" && resolved != deploy.GetSource().GetRevision() {
		if len(resolved) > 7 {
			return resolved[:7]
		}
		return resolved
	}
	if upload := deploy.GetSource().GetUploadName(); upload != "" {
		return "upload " + upload
	}
	return deploy.GetSource().GetRevision()
}

func writeDeploy(w io.Writer, deploy *hostingv1.Deploy) error {
	return fields(w,
		pair("deploy", deploy.GetId()),
		pair("domain", deploy.GetZoneName()),
		pair("kind", label(deploy.GetKind().String(), "DEPLOY_KIND_")),
		pair("phase", label(deploy.GetPhase().String(), "DEPLOY_PHASE_")),
		pair("live", yesNo(deploy.GetLive())),
		pair("revision", orDash(deployRevision(deploy))),
		pair("requested", formatTime(deploy.GetRequestedAt())),
		pair("reason", orDash(deploy.GetReason())),
	)
}

func cmdSiteRollback(ctx context.Context, e *env, o *options, args []string) error {
	var deployID string
	fs, err := o.parse("deephost site rollback", siteUsage, args, func(fs *flag.FlagSet) {
		fs.StringVar(&deployID, "deploy-id", "", "the deploy to put back in front of traffic")
	})
	if err != nil {
		return err
	}
	reference, err := argument(fs, 0, "domain", siteUsage)
	if err != nil {
		return err
	}
	if err := noExtraArgs(fs, 1, siteUsage); err != nil {
		return err
	}
	if strings.TrimSpace(deployID) == "" {
		return usagef("--deploy-id: is required; list them with `deephost site deploys DOMAIN`")
	}
	clients, err := o.connect(e)
	if err != nil {
		return err
	}
	site, err := lookupSite(ctx, o, clients, reference)
	if err != nil {
		return err
	}
	callCtx, cancel := o.call(ctx)
	defer cancel()
	response, err := clients.Hosting.RollbackSite(callCtx, connect.NewRequest(&hostingv1.RollbackSiteRequest{
		ZoneId: site.GetZoneId(), DeployId: deployID,
	}))
	if err != nil {
		return fail(clients, err)
	}
	return newPrinter(e, o).emit(response.Msg, func(w io.Writer) error {
		return writeDeploy(w, response.Msg.GetDeploy())
	})
}

func cmdSiteLogs(ctx context.Context, e *env, o *options, args []string) error {
	var deployID string
	var tail uint
	fs, err := o.parse("deephost site logs", siteUsage, args, func(fs *flag.FlagSet) {
		fs.StringVar(&deployID, "deploy-id", "", "which deploy's log (default the latest)")
		fs.UintVar(&tail, "tail", 0, "how many lines from the end (default 500, max 1000)")
	})
	if err != nil {
		return err
	}
	reference, err := argument(fs, 0, "domain", siteUsage)
	if err != nil {
		return err
	}
	if err := noExtraArgs(fs, 1, siteUsage); err != nil {
		return err
	}
	clients, err := o.connect(e)
	if err != nil {
		return err
	}
	site, err := lookupSite(ctx, o, clients, reference)
	if err != nil {
		return err
	}
	if strings.TrimSpace(deployID) == "" {
		deployID = site.GetLatestDeployId()
		if deployID == "" {
			deployID = site.GetLiveDeployId()
		}
		if deployID == "" {
			return fmt.Errorf("deploy_id: %s has no deploys yet", site.GetZoneName())
		}
	}
	callCtx, cancel := o.call(ctx)
	defer cancel()
	response, err := clients.Hosting.GetDeployLog(callCtx, connect.NewRequest(&hostingv1.GetDeployLogRequest{
		ZoneId: site.GetZoneId(), DeployId: deployID, TailLines: uint32(tail), //nolint:gosec // bounded server-side
	}))
	if err != nil {
		return fail(clients, err)
	}
	result := response.Msg
	if o.json {
		return writeJSONTo(e.stdout, result)
	}
	// Log lines are data: they go to stdout unadorned so they can be grepped,
	// and the note about where they came from goes to stderr.
	for _, line := range result.GetLines() {
		if err := writeLine(e.stdout, line); err != nil {
			return err
		}
	}
	source := label(result.GetSource().String(), "LOG_SOURCE_")
	summary := fmt.Sprintf("%s: %s log, %d lines", deployID, source, len(result.GetLines()))
	if result.GetTruncated() {
		summary += ", truncated"
	}
	if note := strings.TrimSpace(result.GetNote()); note != "" {
		summary += "\n" + note
	}
	return writeLine(e.stderr, summary)
}

func cmdSiteEnv(ctx context.Context, e *env, o *options, args []string) error {
	if len(args) == 0 {
		return &usageError{message: "site env: choose get, set or unset", usage: siteUsage}
	}
	switch args[0] {
	case "get", "list", "ls":
		return cmdSiteEnvGet(ctx, e, o, args[1:])
	case "set":
		return cmdSiteEnvSet(ctx, e, o, args[1:])
	case "unset", "delete", "rm":
		return cmdSiteEnvUnset(ctx, e, o, args[1:])
	default:
		return &usageError{message: fmt.Sprintf("unknown site env command %q", args[0]), usage: siteUsage}
	}
}

func cmdSiteEnvGet(ctx context.Context, e *env, o *options, args []string) error {
	var environment string
	fs, err := o.parse("deephost site env get", siteUsage, args, func(fs *flag.FlagSet) {
		fs.StringVar(&environment, "environment", "", "production, preview or development (default all)")
	})
	if err != nil {
		return err
	}
	reference, err := argument(fs, 0, "domain", siteUsage)
	if err != nil {
		return err
	}
	if err := noExtraArgs(fs, 1, siteUsage); err != nil {
		return err
	}
	clients, err := o.connect(e)
	if err != nil {
		return err
	}
	site, err := lookupSite(ctx, o, clients, reference)
	if err != nil {
		return err
	}
	callCtx, cancel := o.call(ctx)
	defer cancel()
	response, err := clients.Hosting.ListSiteEnvVars(callCtx, connect.NewRequest(&hostingv1.ListSiteEnvVarsRequest{
		ZoneId: site.GetZoneId(), Environment: environment,
	}))
	if err != nil {
		return fail(clients, err)
	}
	result := response.Msg
	return newPrinter(e, o).emit(result, func(w io.Writer) error {
		if !result.GetSupported() {
			// An empty list from an engine that cannot be asked is not the
			// same as a site with no variables, and must not be shown as one.
			return writeLine(w, orDash(result.GetNote()))
		}
		if len(result.GetVariables()) == 0 {
			return writeLine(w, "no environment variables")
		}
		table := newTable(w, "NAME", "ENVIRONMENT", "LAST SET")
		for _, variable := range result.GetVariables() {
			table.row(variable.GetName(), orDash(variable.GetEnvironment()), formatTime(variable.GetLastSet()))
		}
		if err := table.flush(); err != nil {
			return err
		}
		// There is no field a value could travel in: say so rather than let
		// the absence of a VALUE column look like a bug.
		return writeLine(w, "\nvalues are write-only and cannot be read back")
	})
}

func cmdSiteEnvSet(ctx context.Context, e *env, o *options, args []string) error {
	var environment, valueFile string
	fs, err := o.parse("deephost site env set", siteUsage, args, func(fs *flag.FlagSet) {
		fs.StringVar(&environment, "environment", "", "production, preview or development (default production)")
		fs.StringVar(&valueFile, "value-file", "", "read the value from this file instead of stdin")
	})
	if err != nil {
		return err
	}
	reference, err := argument(fs, 0, "domain", siteUsage)
	if err != nil {
		return err
	}
	name, err := argument(fs, 1, "name", siteUsage)
	if err != nil {
		return err
	}
	if err := noExtraArgs(fs, 2, siteUsage); err != nil {
		return err
	}
	value, err := readSecretValue(e, "value", valueFile)
	if err != nil {
		return err
	}
	clients, err := o.connect(e)
	if err != nil {
		return err
	}
	site, err := lookupSite(ctx, o, clients, reference)
	if err != nil {
		return err
	}
	callCtx, cancel := o.call(ctx)
	defer cancel()
	response, err := clients.Hosting.SetSiteEnvVar(callCtx, connect.NewRequest(&hostingv1.SetSiteEnvVarRequest{
		ZoneId: site.GetZoneId(), Name: name, Value: value, Environment: environment,
	}))
	if err != nil {
		return fail(clients, err)
	}
	result := response.Msg
	printer := newPrinter(e, o)
	if err := printer.emit(result, func(w io.Writer) error {
		return fields(w,
			pair("set", result.GetVariable().GetName()),
			pair("environment", orDash(result.GetVariable().GetEnvironment())),
			pair("last set", formatTime(result.GetVariable().GetLastSet())),
		)
	}); err != nil {
		return err
	}
	return printer.note("%s", orDash(result.GetNote()))
}

func cmdSiteEnvUnset(ctx context.Context, e *env, o *options, args []string) error {
	var environment string
	fs, err := o.parse("deephost site env unset", siteUsage, args, func(fs *flag.FlagSet) {
		fs.StringVar(&environment, "environment", "", "production, preview or development (default production)")
	})
	if err != nil {
		return err
	}
	reference, err := argument(fs, 0, "domain", siteUsage)
	if err != nil {
		return err
	}
	name, err := argument(fs, 1, "name", siteUsage)
	if err != nil {
		return err
	}
	if err := noExtraArgs(fs, 2, siteUsage); err != nil {
		return err
	}
	clients, err := o.connect(e)
	if err != nil {
		return err
	}
	site, err := lookupSite(ctx, o, clients, reference)
	if err != nil {
		return err
	}
	if err := confirm(e, o, fmt.Sprintf("Delete %s from %s? The value cannot be recovered.",
		name, site.GetZoneName())); err != nil {
		return err
	}
	callCtx, cancel := o.call(ctx)
	defer cancel()
	response, err := clients.Hosting.DeleteSiteEnvVar(callCtx, connect.NewRequest(&hostingv1.DeleteSiteEnvVarRequest{
		ZoneId: site.GetZoneId(), Name: name, Environment: environment,
	}))
	if err != nil {
		return fail(clients, err)
	}
	document := struct {
		Deleted     bool   `json:"deleted"`
		ZoneID      string `json:"zone_id"`
		Name        string `json:"name"`
		Environment string `json:"environment"`
		Note        string `json:"note,omitempty"`
	}{
		Deleted: true, ZoneID: site.GetZoneId(), Name: name,
		Environment: environment, Note: response.Msg.GetNote(),
	}
	return newPrinter(e, o).emit(document, func(w io.Writer) error {
		return fields(w, pair("unset", name), pair("note", orDash(document.Note)))
	})
}

// cmdSiteUpdate changes what a later deploy defaults to: the repository, the
// branch and the sub-directory. An unmentioned field is left exactly as it is,
// because UpdateSite's optional fields mean "leave it" and nothing else.
func cmdSiteUpdate(ctx context.Context, e *env, o *options, args []string) error {
	var repository, branch, path, wwwMode string
	fs, err := o.parse("deephost site update", siteUsage, args, func(fs *flag.FlagSet) {
		fs.StringVar(&repository, "repository", "", "https://github.com/owner/name")
		fs.StringVar(&branch, "branch", "", "default revision to deploy")
		fs.StringVar(&path, "path", "", "sub-directory of the repository to build")
		fs.StringVar(&wwwMode, "www-mode", "", "serve or redirect for www.<domain>")
	})
	if err != nil {
		return err
	}
	reference, err := argument(fs, 0, "domain", siteUsage)
	if err != nil {
		return err
	}
	if err := noExtraArgs(fs, 1, siteUsage); err != nil {
		return err
	}
	parsedWww, err := parseWwwMode(wwwMode)
	if err != nil {
		return err
	}
	if !provided(fs, "repository") && !provided(fs, "branch") &&
		!provided(fs, "path") && !provided(fs, "www-mode") {
		return usagef("site update: give at least one of --repository, --branch, --path or --www-mode")
	}
	clients, err := o.connect(e)
	if err != nil {
		return err
	}
	site, err := lookupSite(ctx, o, clients, reference)
	if err != nil {
		return err
	}
	request := &hostingv1.UpdateSiteRequest{ZoneId: site.GetZoneId(), WwwMode: parsedWww}
	if provided(fs, "repository") {
		request.Repository = &repository
	}
	if provided(fs, "branch") {
		request.Branch = &branch
	}
	if provided(fs, "path") {
		request.Path = &path
	}
	callCtx, cancel := o.call(ctx)
	defer cancel()
	response, err := clients.Hosting.UpdateSite(callCtx, connect.NewRequest(request))
	if err != nil {
		return fail(clients, err)
	}
	return newPrinter(e, o).emit(response.Msg, func(w io.Writer) error {
		return writeSite(w, response.Msg.GetSite())
	})
}

func cmdSiteWww(ctx context.Context, e *env, o *options, args []string) error {
	var mode string
	fs, err := o.parse("deephost site www", siteUsage, args, func(fs *flag.FlagSet) {
		fs.StringVar(&mode, "mode", "", "serve or redirect")
	})
	if err != nil {
		return err
	}
	reference, err := argument(fs, 0, "domain", siteUsage)
	if err != nil {
		return err
	}
	if err := noExtraArgs(fs, 1, siteUsage); err != nil {
		return err
	}
	parsedMode, err := parseWwwMode(mode)
	if err != nil {
		return err
	}
	if parsedMode == hostingv1.WwwMode_WWW_MODE_UNSPECIFIED {
		return usagef("--mode: is required (serve or redirect)")
	}
	clients, err := o.connect(e)
	if err != nil {
		return err
	}
	site, err := lookupSite(ctx, o, clients, reference)
	if err != nil {
		return err
	}
	callCtx, cancel := o.call(ctx)
	defer cancel()
	response, err := clients.Hosting.UpdateSite(callCtx, connect.NewRequest(&hostingv1.UpdateSiteRequest{
		ZoneId: site.GetZoneId(), WwwMode: parsedMode,
	}))
	if err != nil {
		return fail(clients, err)
	}
	return newPrinter(e, o).emit(response.Msg, func(w io.Writer) error {
		return writeSite(w, response.Msg.GetSite())
	})
}

func cmdSiteReapplyDNS(ctx context.Context, e *env, o *options, args []string) error {
	var replaceConflicting, dryRun bool
	fs, err := o.parse("deephost site reapply-dns", siteUsage, args, func(fs *flag.FlagSet) {
		fs.BoolVar(&replaceConflicting, "replace-conflicting", false, "replace records that block the site's own")
		fs.BoolVar(&dryRun, "dry-run", false, "show the plan and write nothing")
	})
	if err != nil {
		return err
	}
	reference, err := argument(fs, 0, "domain", siteUsage)
	if err != nil {
		return err
	}
	if err := noExtraArgs(fs, 1, siteUsage); err != nil {
		return err
	}
	clients, err := o.connect(e)
	if err != nil {
		return err
	}
	site, err := lookupSite(ctx, o, clients, reference)
	if err != nil {
		return err
	}
	callCtx, cancel := o.call(ctx)
	defer cancel()
	response, err := clients.Hosting.ReapplySiteDns(callCtx, connect.NewRequest(&hostingv1.ReapplySiteDnsRequest{
		ZoneId: site.GetZoneId(), ReplaceConflictingRecords: replaceConflicting, DryRun: dryRun,
	}))
	if err != nil {
		return fail(clients, err)
	}
	result := response.Msg
	return newPrinter(e, o).emit(result, func(w io.Writer) error {
		if err := fields(w,
			pair("domain", site.GetZoneName()),
			pair("dry run", yesNo(result.GetDryRun())),
			pair("dns", dnsSummary(result.GetSite())),
		); err != nil {
			return err
		}
		if err := writeChanges(w, "dns plan", result.GetDnsPlan()); err != nil {
			return err
		}
		return writeChanges(w, "conflicts", result.GetConflicts())
	})
}

func cmdSiteGateway(ctx context.Context, e *env, o *options, args []string) error {
	var confirmAddresses string
	fs, err := o.parse("deephost site gateway", siteUsage, args, func(fs *flag.FlagSet) {
		fs.StringVar(&confirmAddresses, "confirm", "",
			"apply this comma-separated address set, which must equal the pending one")
	})
	if err != nil {
		return err
	}
	if err := noExtraArgs(fs, 0, siteUsage); err != nil {
		return err
	}
	clients, err := o.connect(e)
	if err != nil {
		return err
	}
	if strings.TrimSpace(confirmAddresses) != "" {
		addresses := make([]string, 0, 4)
		for _, address := range strings.Split(confirmAddresses, ",") {
			if trimmed := strings.TrimSpace(address); trimmed != "" {
				addresses = append(addresses, trimmed)
			}
		}
		if err := confirm(e, o, fmt.Sprintf(
			"Point every site's apex at %s? Traffic follows as the records propagate.",
			strings.Join(addresses, ", "))); err != nil {
			return err
		}
		callCtx, cancel := o.call(ctx)
		defer cancel()
		response, err := clients.Hosting.ConfirmGatewayAddresses(callCtx,
			connect.NewRequest(&hostingv1.ConfirmGatewayAddressesRequest{Addresses: addresses}))
		if err != nil {
			return fail(clients, err)
		}
		return newPrinter(e, o).emit(response.Msg, func(w io.Writer) error {
			return fields(w,
				pair("addresses", strings.Join(response.Msg.GetAddresses(), " ")),
				pair("sites queued", formatUint(response.Msg.GetSitesQueued())),
			)
		})
	}
	callCtx, cancel := o.call(ctx)
	defer cancel()
	response, err := clients.Hosting.GetHostingStatus(callCtx,
		connect.NewRequest(&hostingv1.GetHostingStatusRequest{}))
	if err != nil {
		return fail(clients, err)
	}
	status := response.Msg
	return newPrinter(e, o).emit(status, func(w io.Writer) error {
		if err := fields(w,
			pair("engine", engineDetail(status.GetEngine())),
			pair("gateway hostname", orDash(status.GetGatewayHostname())),
			pair("gateway addresses", orDash(strings.Join(status.GetGatewayAddresses(), " "))),
			pair("source", orDash(status.GetGatewaySource())),
			pair("resolver", orDash(status.GetGatewayResolver())),
			pair("resolved", formatTime(status.GetGatewayResolvedAt())),
			pair("error", orDash(status.GetGatewayError())),
			pair("apps suffix", orDash(status.GetAppsSuffix())),
			pair("sites", formatUint(status.GetSites())),
			pair("active deploys", formatUint(status.GetActiveDeploys())),
		); err != nil {
			return err
		}
		if pending := status.GetGatewayPendingAddresses(); len(pending) > 0 {
			return writeLine(w, fmt.Sprintf(
				"\na new address set is waiting since %s: apply it with\n  deephost site gateway --confirm %s",
				formatTime(status.GetGatewayPendingSince()), strings.Join(pending, ",")))
		}
		return nil
	})
}

// writeChanges renders a DNS plan: what would be written, and why.
func writeChanges(w io.Writer, heading string, changes []*platformv1.RecordChange) error {
	if len(changes) == 0 {
		return nil
	}
	if _, err := fmt.Fprintf(w, "\n%s:\n", heading); err != nil {
		return err
	}
	table := newTable(w, "OP", "NAME", "TYPE", "VALUE", "TTL", "WHY")
	for _, change := range changes {
		table.row(
			change.GetOp(),
			orDash(change.GetName()),
			change.GetType(),
			change.GetValue(),
			formatUint(change.GetTtl()),
			orDash(change.GetWhy()),
		)
	}
	return table.flush()
}

// lookupSite finds the site attached to a zone, naming the zone the way the
// operator did.
func lookupSite(ctx context.Context, o *options, clients *client.Clients, reference string) (*hostingv1.Site, error) {
	zone, err := lookupZoneWithTimeout(ctx, o, clients, reference)
	if err != nil {
		return nil, err
	}
	callCtx, cancel := o.call(ctx)
	defer cancel()
	response, err := clients.Hosting.GetSite(callCtx,
		connect.NewRequest(&hostingv1.GetSiteRequest{ZoneId: zone.GetId()}))
	if err != nil {
		return nil, fail(clients, err)
	}
	site := response.Msg.GetSite()
	if site == nil {
		return nil, fmt.Errorf("domain: %s has no site; attach one with `deephost site attach %s --framework static`",
			zone.GetName(), zone.GetName())
	}
	// The zone name the control plane knows is authoritative, but a site row
	// written before a rename may not carry one.
	if site.GetZoneName() == "" {
		site.ZoneName = zone.GetName()
	}
	return site, nil
}
