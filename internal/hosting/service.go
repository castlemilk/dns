package hosting

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	hostingv1 "github.com/castlemilk/dns/gen/go/hosting/v1"
	"github.com/castlemilk/dns/gen/go/hosting/v1/hostingv1connect"
	platformv1 "github.com/castlemilk/dns/gen/go/platform/v1"
	"github.com/castlemilk/dns/internal/activity"
	"github.com/castlemilk/dns/internal/config"
	"github.com/castlemilk/dns/internal/enginedns"
	"github.com/castlemilk/dns/internal/hosting/deephost"
	"github.com/castlemilk/dns/internal/hosting/fakehosting"
	"github.com/castlemilk/dns/internal/platform"
	"github.com/castlemilk/dns/internal/zone"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// workerBudget bounds one iteration of every background worker, so a wedged
// engine costs one pass rather than the worker.
const workerBudget = 20 * time.Second

// defaultBranch is what a site deploys from when no branch was given.
const defaultBranch = "main"

// deployRetireLimit bounds the page liveDeploys scans. The scan is the belt to
// the site's own LiveDeployID braces: past this many deploys the live row can
// be off the newest page, so it is looked up by id as well.
const deployRetireLimit = 200

// Service implements hosting.v1.HostingService.
type Service struct {
	cfg    config.Hosting
	deps   platform.Deps
	engine Engine
	// production mirrors config.Hosting.Production (DNS_PRODUCTION). It only
	// ever tightens the gateway address filter (§3.5).
	production bool

	mu                sync.Mutex
	uploadBytes       int64
	reachable         bool
	statusReason      string
	checkedAt         time.Time
	gatewayRejectedAt time.Time
	gatewayFailedAt   time.Time
	pendingZones      map[string]struct{}

	// drainTimeout is how long DetachSite waits inline for the engine to stop
	// listing the site's hosts. It is a field so a test does not have to wait a
	// real minute for the engine's garbage collection.
	drainTimeout time.Duration

	// resolve is the gateway hostname lookup. It is a field so a test can
	// script an answer without a DNS server; production always uses
	// lookupGateway.
	resolve func(context.Context) ([]netip.Addr, error)

	gatewayWake chan struct{}
	watcherWake chan struct{}
	pollWake    chan struct{}
	uploadWake  chan struct{}
	recordWake  chan struct{}
}

// New builds the hosting facade. The second result is the folder-upload HTTP
// handler, non-nil only when uploads are enabled. The signature is frozen by
// spec2 §11.
func New(cfg config.Hosting, deps platform.Deps) (*Service, http.Handler) {
	service := &Service{
		cfg:          cfg,
		deps:         deps,
		production:   cfg.Production,
		pendingZones: map[string]struct{}{},
		drainTimeout: detachDrainTimeout,
		gatewayWake:  make(chan struct{}, 1),
		watcherWake:  make(chan struct{}, 1),
		pollWake:     make(chan struct{}, 1),
		uploadWake:   make(chan struct{}, 1),
		recordWake:   make(chan struct{}, 1),
	}
	service.resolve = service.lookupGateway
	switch {
	case !cfg.Configured():
	case cfg.Fake():
		service.engine = newMeteredEngine(fakehosting.New(), deps.Metrics)
	default:
		service.engine = newMeteredEngine(deephost.New(cfg.APIURL, cfg.APIToken), deps.Metrics)
	}
	if !cfg.UploadsEnabled || !cfg.Configured() {
		return service, nil
	}
	return service, service.UploadHandler()
}

// WithDrainTimeout bounds the inline wait for the engine's host removal. Tests
// shorten it; production uses detachDrainTimeout.
func (s *Service) WithDrainTimeout(d time.Duration) *Service {
	if d > 0 {
		s.drainTimeout = d
	}
	return s
}

// WithResolver replaces the gateway hostname lookup. Tests use it to script a
// resolution; production never calls it.
func (s *Service) WithResolver(resolve func(context.Context) ([]netip.Addr, error)) *Service {
	if resolve != nil {
		s.resolve = resolve
	}
	return s
}

// WithEngine replaces the engine. Tests use it to run the whole facade against
// fakehosting with their own clock and scripted phases.
func (s *Service) WithEngine(engine Engine) *Service {
	s.engine = newMeteredEngine(engine, s.deps.Metrics)
	return s
}

func (s *Service) configured() bool { return s.cfg.Configured() && s.engine != nil }

func (s *Service) now() time.Time { return s.deps.Now() }

func (s *Service) log() *slog.Logger { return s.deps.Log().With("engine", "hosting") }

// jitter spreads a cadence by ±10 % so every control-plane replica does not
// call the engine on the same second.
func jitter(interval time.Duration) time.Duration {
	spread := float64(interval) * 0.1
	return interval + time.Duration((rand.Float64()*2-1)*spread)
}

// Run drives the hosting workers until ctx is done.
func (s *Service) Run(ctx context.Context) error {
	if !s.configured() {
		<-ctx.Done()
		return nil
	}
	workers := []func(context.Context){
		s.runPoller,
		s.runDomainWatcher,
		s.runGatewayReconciler,
		s.runRecordReconciler,
		s.runUploadDeployer,
		s.runCapabilityProber,
	}
	var running sync.WaitGroup
	for _, worker := range workers {
		running.Add(1)
		go func() {
			defer running.Done()
			worker(ctx)
		}()
	}
	<-ctx.Done()
	running.Wait()
	return nil
}

// Probe checks the engine and refreshes what Settings shows. It never blocks a
// request path: the prober calls it on its own cadence.
func (s *Service) Probe(ctx context.Context) error {
	if !s.configured() {
		return nil
	}
	err := s.engine.Health(ctx)
	if err == nil {
		_, err = s.engine.ListApps(ctx, s.cfg.Tenant)
	}
	if err == nil {
		s.probeTenantScope(ctx)
	}

	s.mu.Lock()
	s.reachable = err == nil
	s.checkedAt = s.now()
	s.statusReason = ""
	if err != nil {
		s.statusReason = reason(s.mapEngineError(err).Error())
	}
	s.mu.Unlock()
	return err
}

// Status is what the Settings page renders. It reads the cache the prober
// filled and the persisted capability set; it never calls the engine.
func (s *Service) Status() platform.EngineStatus {
	s.mu.Lock()
	reachable, checkedAt, why := s.reachable, s.checkedAt, s.statusReason
	s.mu.Unlock()

	if checkedAt.IsZero() {
		checkedAt = s.now()
	}
	status := platform.EngineStatus{
		Kind:         platformv1.EngineKind_ENGINE_KIND_HOSTING,
		Configured:   s.cfg.Configured(),
		Reachable:    reachable,
		CheckedAt:    checkedAt,
		Reason:       why,
		MissingEnv:   s.cfg.MissingEnv(),
		Provider:     s.provider(),
		EndpointHost: s.cfg.EndpointHost(),
	}
	if s.cfg.Configured() && s.deps.Store != nil {
		status.Capabilities = s.capabilities(context.Background()).Map()
	}
	return status
}

func (s *Service) provider() string {
	switch {
	case !s.cfg.Configured():
		return ""
	case s.cfg.Fake():
		return "fake"
	default:
		return "deephost"
	}
}

// Observer receives operator DNS mutations that can invalidate a site's
// records, and asks the reconciler to repair them.
func (s *Service) Observer() enginedns.ZoneObserver { return siteObserver{service: s} }

type siteObserver struct{ service *Service }

func (o siteObserver) ZoneReplaced(_ context.Context, zoneID string) {
	o.service.pokeRecords(zoneID)
}

// RecordDeleted wakes the reconciler for any deletion. DeleteRecord refuses
// every record an engine owns before it notifies, so the deleted record is
// always the operator's — and the operator's record is exactly the thing that
// can be standing in the way of a record this engine could not publish.
func (o siteObserver) RecordDeleted(_ context.Context, zoneID string, _ zone.Record) {
	o.service.pokeRecords(zoneID)
}

func (o siteObserver) ZoneDeleted(_ context.Context, zoneID string) {
	o.service.pokeRecords(zoneID)
}

// pokeRecords queues one zone for the site DNS reconciler.
func (s *Service) pokeRecords(zoneID string) {
	if zoneID == "" {
		return
	}
	s.mu.Lock()
	s.pendingZones[zoneID] = struct{}{}
	s.mu.Unlock()
	select {
	case s.recordWake <- struct{}{}:
	default:
	}
}

// takePendingZones drains the queue.
func (s *Service) takePendingZones() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pendingZones) == 0 {
		return nil
	}
	zones := make([]string, 0, len(s.pendingZones))
	for zoneID := range s.pendingZones {
		zones = append(zones, zoneID)
	}
	clear(s.pendingZones)
	slices.Sort(zones)
	return zones
}

// runRecordReconciler is the single writer for hosting DNS records: a periodic
// full pass plus an immediate pass for every poked zone.
func (s *Service) runRecordReconciler(ctx context.Context) {
	for {
		timer := time.NewTimer(jitter(recordReconcileInterval))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.recordWake:
			timer.Stop()
			passCtx, cancel := context.WithTimeout(ctx, workerBudget)
			for _, zoneID := range s.takePendingZones() {
				if err := s.reconcileSiteDNS(passCtx, zoneID); err != nil && !isNotFound(err) {
					s.log().Warn("reconcile website records", "error", err)
				}
			}
			cancel()
		case <-timer.C:
			passCtx, cancel := context.WithTimeout(ctx, workerBudget)
			if err := s.ReconcileAll(passCtx); err != nil {
				s.log().Warn("reconcile website records", "error", err)
			}
			cancel()
		}
	}
}

// ReconcileAll is the start-up pass: it re-adopts engine records after a
// restore, sweeps records whose site row is gone, and adopts builds for deploy
// rows whose engine call never confirmed.
func (s *Service) ReconcileAll(ctx context.Context) error {
	if !s.configured() {
		return nil
	}
	sites, err := s.deps.Store.ListSites(ctx)
	if err != nil {
		return err
	}
	var problems []error
	for _, site := range sites {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if site.State == SiteStateDetaching {
			continue
		}
		if err := s.reconcileSiteDNS(ctx, site.ZoneID); err != nil && !isNotFound(err) {
			problems = append(problems, err)
		}
	}
	if err := s.sweepOrphans(ctx); err != nil {
		problems = append(problems, err)
	}
	s.pokeWatcher()
	s.pokePoller()
	s.pokeUploads()
	return errors.Join(problems...)
}

// -- RPCs ------------------------------------------------------------------

func (s *Service) GetHostingStatus(
	ctx context.Context,
	_ *connect.Request[hostingv1.GetHostingStatusRequest],
) (*connect.Response[hostingv1.GetHostingStatusResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, connect.NewError(connect.CodeCanceled, err)
	}
	response := &hostingv1.GetHostingStatusResponse{
		Engine:               s.Status().Proto(),
		GatewayHostname:      s.cfg.GatewayHostname,
		AppsSuffix:           s.cfg.AppsSuffix,
		UploadsEnabled:       s.cfg.UploadsEnabled,
		UploadMaxBytes:       uint64(max(s.cfg.UploadMaxBytes, 0)),
		BuildDeadlineSeconds: uint32(s.cfg.BuildDeadline / time.Second),
		GatewayResolver:      s.resolverLabel(),
	}
	if !s.cfg.Configured() {
		return connect.NewResponse(response), nil
	}

	gateway, err := s.deps.Store.GetGateway(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, engineError("the gateway state could not be read"))
	}
	response.GatewayAddresses = append([]string(nil), gateway.Addresses...)
	response.GatewaySource = gateway.Source
	response.GatewayError = gateway.LastError
	response.GatewayPendingAddresses = append([]string(nil), gateway.Pending...)
	if !gateway.ResolvedAt.IsZero() {
		response.GatewayResolvedAt = timestamppb.New(gateway.ResolvedAt.UTC())
	}
	if !gateway.PendingSince.IsZero() {
		response.GatewayPendingSince = timestamppb.New(gateway.PendingSince.UTC())
	}

	sites, err := s.deps.Store.ListSites(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, engineError("the site list could not be read"))
	}
	response.Sites = uint32(len(sites))
	active, err := s.deps.Store.ActiveDeploys(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, engineError("the deploy list could not be read"))
	}
	response.ActiveDeploys = uint32(len(active))
	return connect.NewResponse(response), nil
}

func (s *Service) resolverLabel() string {
	if s.cfg.GatewayResolver == "" {
		return "system"
	}
	return s.cfg.GatewayResolver
}

func (s *Service) ListSites(
	ctx context.Context,
	_ *connect.Request[hostingv1.ListSitesRequest],
) (*connect.Response[hostingv1.ListSitesResponse], error) {
	if !s.configured() {
		return connect.NewResponse(&hostingv1.ListSitesResponse{}), nil
	}
	docs, err := s.deps.Store.ListSites(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, engineError("the site list could not be read"))
	}
	response := &hostingv1.ListSitesResponse{Sites: make([]*hostingv1.Site, 0, len(docs))}
	for _, doc := range docs {
		response.Sites = append(response.Sites, s.renderSite(ctx, doc))
	}
	return connect.NewResponse(response), nil
}

func (s *Service) GetSite(
	ctx context.Context,
	request *connect.Request[hostingv1.GetSiteRequest],
) (*connect.Response[hostingv1.GetSiteResponse], error) {
	if !s.configured() {
		return nil, notConfigured()
	}
	doc, err := s.site(ctx, request.Msg.GetZoneId())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&hostingv1.GetSiteResponse{Site: s.renderSite(ctx, doc)}), nil
}

// renderSite joins a stored site with its live and latest deploy rows.
func (s *Service) renderSite(ctx context.Context, doc platform.SiteDoc) *hostingv1.Site {
	var live, latest *hostingv1.Deploy
	if doc.LiveDeployID != "" {
		if deploy, err := s.deps.Store.GetDeploy(ctx, doc.ZoneID, doc.LiveDeployID); err == nil {
			live = deployProto(deploy)
		}
	}
	if doc.LatestDeployID != "" {
		if deploy, err := s.deps.Store.GetDeploy(ctx, doc.ZoneID, doc.LatestDeployID); err == nil {
			latest = deployProto(deploy)
		}
	}
	return siteProto(doc, live, latest)
}

func (s *Service) site(ctx context.Context, zoneID string) (platform.SiteDoc, error) {
	if strings.TrimSpace(zoneID) == "" {
		return platform.SiteDoc{}, invalidArgument("zone_id", "is required")
	}
	doc, err := s.deps.Store.GetSite(ctx, zoneID)
	if errors.Is(err, platform.ErrNotFound) {
		return platform.SiteDoc{}, notFound("this domain is not hosted here")
	}
	if err != nil {
		return platform.SiteDoc{}, connect.NewError(connect.CodeInternal, engineError("the site could not be read"))
	}
	return doc, nil
}

func (s *Service) AttachSite(
	ctx context.Context,
	request *connect.Request[hostingv1.AttachSiteRequest],
) (*connect.Response[hostingv1.AttachSiteResponse], error) {
	if !s.configured() {
		return nil, notConfigured()
	}
	message := request.Msg

	value, err := s.deps.Zones.GetZone(ctx, strings.TrimSpace(message.GetZoneId()))
	if err != nil {
		return nil, notFound("this domain does not exist")
	}
	if !validHostZone(value.Name) {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			engineError("this zone name is not a valid hostname"))
	}

	switch existing, err := s.deps.Store.GetSite(ctx, value.ID); {
	case err == nil && existing.State == SiteStateDetaching:
		return nil, failedPrecondition("the previous site is still being removed")
	case err == nil:
		return nil, failedPrecondition(fmt.Sprintf("%s is already hosted here", value.Name))
	case !errors.Is(err, platform.ErrNotFound):
		return nil, connect.NewError(connect.CodeInternal, engineError("the site could not be read"))
	}

	framework := FrameworkFromProto(message.GetFramework())
	if !framework.Valid() {
		return nil, invalidArgument("framework", "choose static, node or nextjs")
	}
	repository := ""
	branch := strings.TrimSpace(message.GetBranch())
	if branch == "" {
		branch = defaultBranch
	}
	path := strings.TrimSpace(message.GetPath())
	if strings.TrimSpace(message.GetRepository()) != "" {
		source, err := validateGitSource(message.GetRepository(), branch, path, "")
		if err != nil {
			return nil, err
		}
		repository, branch, path = source.Repository, orDefault(source.Revision, defaultBranch), source.Path
	}

	wwwMode := wwwModeFromProto(message.GetWwwMode())
	if wwwMode == "" {
		wwwMode = WWWModeServe
	}
	if wwwMode == WWWModeRedirect && message.GetSkipWww() {
		return nil, invalidArgument("www_mode", "redirect needs the www host, so skip_www must be false")
	}

	gateway, err := s.gatewayForAttach(ctx)
	if err != nil {
		return nil, err
	}

	doc := platform.SiteDoc{
		V:          platform.DocVersion,
		ZoneID:     value.ID,
		ZoneName:   value.Name,
		App:        AppName(s.cfg.Tenant, value.Name),
		Framework:  string(framework),
		Repository: repository,
		Branch:     branch,
		Path:       path,
		State:      SiteStateAttaching,
		WWW:        !message.GetSkipWww(),
		WWWMode:    wwwMode,
		CreatedAt:  s.now(),
		UpdatedAt:  s.now(),
	}
	doc.AppRef = AppRef(s.cfg.Tenant, doc.App)
	if s.cfg.AppsSuffix != "" {
		doc.AutoHostname = doc.AppRef + "." + s.cfg.AppsSuffix
	}
	doc.Hostnames = s.desiredHosts(doc)

	response := &hostingv1.AttachSiteResponse{}
	err = s.deps.Serial.Run(ctx, value.ID, func(ctx context.Context) error {
		plan, err := s.planSite(ctx, doc, gateway)
		if err != nil {
			return err
		}
		response.DnsPlan = plan.ChangesProto()
		response.Conflicts = conflictChanges(plan)
		if message.GetDryRun() {
			response.DryRun = true
			return nil
		}
		if len(plan.Conflicts) > 0 && !message.GetReplaceConflictingRecords() {
			return conflictError(len(plan.Conflicts), response)
		}

		// The row is written before any engine or DNS write, so a crash always
		// leaves something the reconcilers can finish or release.
		if err := s.deps.Store.PutSite(ctx, doc); err != nil {
			return err
		}
		if err := s.ensureApp(ctx, &doc); err != nil {
			return err
		}
		if err := s.applyPlan(ctx, &doc, plan, plan.ApplyOps(message.GetReplaceConflictingRecords()), "attach-site"); err != nil {
			return err
		}
		return s.deps.Store.PutSite(ctx, doc)
	})
	if err != nil {
		return nil, s.publicError(err)
	}
	if message.GetDryRun() {
		return connect.NewResponse(response), nil
	}

	s.deps.Record(ctx, activity.Event{
		ZoneID: doc.ZoneID, ZoneName: doc.ZoneName, Actor: activity.ActorHostingEngine,
		Kind: activity.KindSiteAttached, Severity: activity.SeverityInfo,
		Summary: fmt.Sprintf("%s is now hosted here", doc.ZoneName),
		Details: map[string]string{"framework": doc.Framework},
	})
	s.pokeWatcher()
	response.Site = s.renderSite(ctx, doc)
	return connect.NewResponse(response), nil
}

// gatewayForAttach reads the stored address set, resolving on demand when it is
// empty so the first attach after a restart does not have to wait for the
// reconciler's cadence.
func (s *Service) gatewayForAttach(ctx context.Context) ([]netip.Addr, error) {
	gateway, err := s.gatewayAddresses(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, engineError("the gateway state could not be read"))
	}
	if len(gateway) > 0 {
		return gateway, nil
	}
	if err := s.resolveGateway(ctx); err != nil {
		s.log().Warn("resolve gateway addresses for attach", "error", err)
	}
	gateway, err = s.gatewayAddresses(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, engineError("the gateway state could not be read"))
	}
	if len(gateway) == 0 {
		s.pokeGateway()
		return nil, failedPrecondition(CopyGatewayUnknown)
	}
	return gateway, nil
}

// ensureApp creates the engine app when it does not exist and only ever
// updates metadata otherwise. CreateApp is never called for an existing app:
// DeepHost's upsert replaces the whole spec, so a resumed attach with an empty
// domains list would wipe the router's host map for that app.
func (s *Service) ensureApp(ctx context.Context, doc *platform.SiteDoc) error {
	view, err := s.engine.GetApp(ctx, s.cfg.Tenant, doc.App)
	switch {
	case err == nil:
		patch := AppPatch{}
		if doc.Repository != "" && view.Repository != doc.Repository {
			patch.Repository = &doc.Repository
		}
		if doc.Branch != "" && view.Branch != doc.Branch {
			patch.Branch = &doc.Branch
		}
		if doc.Path != "" {
			patch.RootDirectory = &doc.Path
		}
		if patch.Repository == nil && patch.Branch == nil && patch.RootDirectory == nil {
			doc.AppRef = view.Name
			return nil
		}
		if err := s.engine.UpdateApp(ctx, s.cfg.Tenant, doc.App, patch); err != nil {
			return err
		}
		doc.AppRef = view.Name
		return nil
	case isNotFound(err):
		appRef, err := s.engine.CreateApp(ctx, s.cfg.Tenant, doc.App, AppSpec{
			Framework:     Framework(doc.Framework),
			Repository:    doc.Repository,
			Branch:        doc.Branch,
			RootDirectory: doc.Path,
		})
		if err != nil {
			return err
		}
		if appRef != "" {
			doc.AppRef = appRef
			if s.cfg.AppsSuffix != "" {
				doc.AutoHostname = appRef + "." + s.cfg.AppsSuffix
				doc.Hostnames = mergeHostnames(s.desiredHosts(*doc), doc.Hostnames)
			}
		}
		return nil
	default:
		return err
	}
}

func (s *Service) UpdateSite(
	ctx context.Context,
	request *connect.Request[hostingv1.UpdateSiteRequest],
) (*connect.Response[hostingv1.UpdateSiteResponse], error) {
	if !s.configured() {
		return nil, notConfigured()
	}
	doc, err := s.site(ctx, request.Msg.GetZoneId())
	if err != nil {
		return nil, err
	}

	patch := AppPatch{}
	if request.Msg.Repository != nil {
		repository := strings.TrimSpace(request.Msg.GetRepository())
		if repository != "" {
			source, err := validateGitSource(repository, doc.Branch, doc.Path, "")
			if err != nil {
				return nil, err
			}
			repository = source.Repository
		}
		doc.Repository = repository
		patch.Repository = &repository
	}
	if request.Msg.Branch != nil {
		branch := strings.TrimSpace(request.Msg.GetBranch())
		if branch == "" {
			branch = defaultBranch
		}
		if !validRevision(branch) || strings.Contains(branch, "..") {
			return nil, invalidArgument("branch", "may contain only letters, digits, '.', '_', '/' and '-'")
		}
		doc.Branch = branch
		patch.Branch = &branch
	}
	if request.Msg.Path != nil {
		path := strings.TrimSpace(request.Msg.GetPath())
		if err := validateSubpath(path); err != nil {
			return nil, err
		}
		doc.Path = path
		patch.RootDirectory = &path
	}
	wwwMode := wwwModeFromProto(request.Msg.GetWwwMode())
	if wwwMode != "" && wwwMode != orDefault(doc.WWWMode, WWWModeServe) {
		if !doc.WWW && wwwMode == WWWModeRedirect {
			return nil, invalidArgument("www_mode", "this site has no www host to redirect")
		}
		doc.WWWMode = wwwMode
	} else {
		wwwMode = ""
	}

	if err := s.engine.UpdateApp(ctx, s.cfg.Tenant, doc.App, patch); err != nil {
		return nil, s.mapEngineError(err)
	}
	if wwwMode != "" {
		// The www host is already registered, so the watcher's registration
		// step will not touch it. DeepHost has no UpdateDomain: CreateDomain on
		// an existing host is the update, and it is issued here so the change
		// takes effect with the request rather than on the next watcher pass.
		if err := s.applyWwwMode(ctx, &doc); err != nil {
			return nil, err
		}
	}
	touch(&doc, s.now())
	if err := s.deps.Store.PutSite(ctx, doc); err != nil {
		return nil, connect.NewError(connect.CodeInternal, engineError("the site could not be stored"))
	}
	s.deps.Record(ctx, activity.Event{
		ZoneID: doc.ZoneID, ZoneName: doc.ZoneName, Actor: activity.ActorOperator,
		Kind: activity.KindSiteUpdated, Severity: activity.SeverityInfo,
		Summary: fmt.Sprintf("Updated the website settings of %s", doc.ZoneName),
	})
	if wwwMode != "" {
		summary, severity := wwwModeSummary(doc, wwwMode)
		s.deps.Record(ctx, activity.Event{
			ZoneID: doc.ZoneID, ZoneName: doc.ZoneName, Actor: activity.ActorOperator,
			Kind: activity.KindSiteWwwModeChanged, Severity: severity,
			Summary: summary,
			Details: map[string]string{"www_mode": wwwMode},
		})
	}
	s.pokeWatcher()
	return connect.NewResponse(&hostingv1.UpdateSiteResponse{Site: s.renderSite(ctx, doc)}), nil
}

// wwwModeSummary describes what the engine reports for the www host after the
// mode was applied, not what was asked for. applyWwwMode re-reads the host, so
// doc.Hostnames carries the engine's answer: an engine version that ignores
// RedirectTo leaves the request stored and the host serving as before, and the
// log has to say that rather than announce a redirect nobody made.
func wwwModeSummary(doc platform.SiteDoc, wwwMode string) (string, activity.Severity) {
	host := "www." + doc.ZoneName
	registered := false
	redirect := ""
	for _, entry := range doc.Hostnames {
		if entry.Host != host {
			continue
		}
		registered = entry.State != HostStateMissing
		redirect = entry.RedirectTo
	}
	if wwwMode == WWWModeRedirect {
		switch {
		case !registered:
			return fmt.Sprintf(
				"Asked for %s to redirect to %s; it is registered with the hosting engine when the host comes up",
				host, doc.ZoneName), activity.SeverityInfo
		case redirect == "":
			return fmt.Sprintf(
				"Asked for %s to redirect to %s, but the hosting engine reports no redirect on that host, so www still serves the same site",
				host, doc.ZoneName), activity.SeverityWarn
		default:
			return fmt.Sprintf("%s now redirects to %s", host, redirect), activity.SeverityInfo
		}
	}
	if registered && redirect != "" {
		return fmt.Sprintf(
			"Asked for %s to serve the same site as %s, but the hosting engine still reports a redirect to %s on that host",
			host, doc.ZoneName, redirect), activity.SeverityWarn
	}
	return fmt.Sprintf("%s now serves the same site as %s", host, doc.ZoneName), activity.SeverityInfo
}

// applyWwwMode re-registers the www host with the redirect the site now wants,
// then re-reads what the engine reports for it. A host that is not registered
// yet needs nothing: the watcher registers it with the mode already set.
//
// The re-read is the honest half. The response would otherwise still carry the
// redirect observed before the switch, and on an engine that ignores the field
// entirely it would carry a redirect that was never applied.
func (s *Service) applyWwwMode(ctx context.Context, doc *platform.SiteDoc) error {
	host := "www." + doc.ZoneName
	registered := false
	for _, entry := range doc.Hostnames {
		if entry.Host == host && entry.State != HostStateMissing {
			registered = true
		}
	}
	if !registered {
		return nil
	}
	err := s.engine.CreateDomain(ctx, s.cfg.Tenant, doc.App, DomainSpec{
		Host:       host,
		RedirectTo: redirectTargetFor(*doc, HostRoleWWW),
	})
	if err != nil {
		return s.mapEngineError(err)
	}
	s.observeRedirects(ctx, doc)
	return nil
}

// observeRedirects refreshes every host's reported redirect from the engine. It
// is best effort: an engine that does not answer leaves the last observation in
// place, which the domain watcher corrects on its next pass.
func (s *Service) observeRedirects(ctx context.Context, doc *platform.SiteDoc) {
	domains, err := s.engine.ListDomains(ctx, s.cfg.Tenant, doc.App)
	if err != nil {
		return
	}
	listed := make(map[string]DomainView, len(domains))
	for _, domain := range domains {
		listed[domain.Host] = domain
	}
	for index := range doc.Hostnames {
		if view, ok := listed[doc.Hostnames[index].Host]; ok {
			doc.Hostnames[index].RedirectTo = view.RedirectTo
		}
	}
}

func (s *Service) DetachSite(
	ctx context.Context,
	request *connect.Request[hostingv1.DetachSiteRequest],
) (*connect.Response[hostingv1.DetachSiteResponse], error) {
	if !s.configured() {
		return nil, notConfigured()
	}
	doc, err := s.site(ctx, request.Msg.GetZoneId())
	if err != nil {
		return nil, err
	}
	if doc.State == SiteStateDetaching {
		return nil, failedPrecondition("the previous site is still being removed")
	}

	doc.State = SiteStateDetaching
	started := s.now()
	doc.DetachStartedAt = &started
	touch(&doc, started)
	if err := s.deps.Store.PutSite(ctx, doc); err != nil {
		return nil, connect.NewError(connect.CodeInternal, engineError("the site could not be stored"))
	}

	response := &hostingv1.DetachSiteResponse{}
	// A kept app on an engine without DeleteDomain keeps its hosts on purpose,
	// so there is nothing to wait for and the row is still removed: the zone is
	// free, and the response says the hosts stayed.
	hostsStay := false
	if request.Msg.GetKeepApp() {
		orphaned, err := s.removeHosts(ctx, doc)
		if err != nil {
			return nil, s.mapEngineError(err)
		}
		response.OrphanedHosts = orphaned
		hostsStay = len(orphaned) > 0
		if hostsStay {
			response.Note = "This hosting engine version has no domain removal API; the hosts stay registered with the kept app until an operator removes them."
			s.deps.Record(ctx, activity.Event{
				ZoneID: doc.ZoneID, ZoneName: doc.ZoneName, Actor: activity.ActorHostingEngine,
				Kind: activity.KindSiteDetachPartial, Severity: activity.SeverityWarn,
				Summary: fmt.Sprintf("%s was detached but its hosts stay registered with the kept app", doc.ZoneName),
			})
		}
	} else {
		// The hosts go first. DeepHost does NOT garbage-collect Domain CRs
		// with the App (verified against the kind cluster: GetApp answers
		// NotFound while ListDomains still returns both hosts), so leaving
		// them to the app delete strands a Domain CR for every detached site.
		// The app delete below is the authoritative step, so a failure here is
		// logged rather than fatal; an engine without DeleteDomain simply
		// leaves the hosts, which hostsDrained then resolves through GetApp.
		if _, err := s.removeHosts(ctx, doc); err != nil {
			s.log().Warn("remove site hosts before deleting the app", "error", err)
		}
		if err := s.engine.DeleteApp(ctx, s.cfg.Tenant, doc.App); err != nil && !isNotFound(err) {
			return nil, s.mapEngineError(err)
		}
	}

	// The DNS half runs now rather than after the engine's garbage collection,
	// so the records a person is looking at change when they press the button.
	if err := s.releaseOrKeepRecords(ctx, doc, request.Msg.GetKeepDnsRecords()); err != nil {
		return nil, s.publicError(err)
	}

	if hostsStay || s.waitForDrain(ctx, doc) {
		s.retireDeploys(ctx, doc)
		if err := s.deps.Store.DeleteSite(ctx, doc.ZoneID); err != nil {
			return nil, connect.NewError(connect.CodeInternal, engineError("the site could not be removed"))
		}
		s.deps.Record(ctx, activity.Event{
			ZoneID: doc.ZoneID, ZoneName: doc.ZoneName, Actor: activity.ActorOperator,
			Kind: activity.KindSiteDetached, Severity: activity.SeverityInfo,
			Summary: fmt.Sprintf("%s is no longer hosted here", doc.ZoneName),
		})
		return connect.NewResponse(response), nil
	}

	response.Pending = true
	response.Note = "The hosting engine is still removing the hosts; the site finishes detaching in the background."
	s.pokeWatcher()
	return connect.NewResponse(response), nil
}

// retireDeploys ends every deploy row a detached site leaves behind, before the
// site row goes. The deploy history is retained after a detach and nothing
// serves a detached site, so a row left at LIVE would claim a build is being
// served that is not, and a row left QUEUED, BUILDING or RELEASING would stay
// active: the janitor refuses to trim an active row and CreateDeploy refuses a
// zone that already has one, so re-attaching the same zone could never deploy
// again. The detach itself is the recorded event; this is its bookkeeping.
func (s *Service) retireDeploys(ctx context.Context, doc platform.SiteDoc) {
	deploys, err := s.liveDeploys(ctx, doc, "")
	if err != nil {
		s.log().Warn("list the deploys of a detached site", "error", err)
	}
	for _, deploy := range deploys {
		deploy.Live = false
		deploy.Phase = platform.DeployPhaseSuperseded
		if err := s.deps.Store.PutDeploy(ctx, deploy); err != nil {
			s.log().Warn("retire the deploy of a detached site", "error", err)
			return
		}
	}

	active, err := s.deps.Store.ActiveDeploys(ctx)
	if err != nil {
		s.log().Warn("list the active deploys of a detached site", "error", err)
		return
	}
	for _, deploy := range active {
		if deploy.ZoneID != doc.ZoneID {
			continue
		}
		if err := s.finishDeploy(ctx, deploy, platform.DeployPhaseAbandoned, ReasonSiteDetached,
			activity.KindDeployAbandoned, activity.SeverityWarn); err != nil {
			s.log().Warn("end the running deploy of a detached site", "error", err)
			return
		}
	}
}

// liveDeploys returns every deploy row of a site that still claims to be live,
// except the one named by skip. The row the site points at is read directly:
// once a site has more deploys than one page holds, the live row can be off the
// newest page, and a scan alone would then leave two rows reading LIVE.
func (s *Service) liveDeploys(
	ctx context.Context,
	site platform.SiteDoc,
	skip string,
) ([]platform.DeployDoc, error) {
	seen := map[string]struct{}{}
	if skip != "" {
		seen[skip] = struct{}{}
	}
	live := make([]platform.DeployDoc, 0, 2)
	if _, done := seen[site.LiveDeployID]; site.LiveDeployID != "" && !done {
		seen[site.LiveDeployID] = struct{}{}
		doc, err := s.deps.Store.GetDeploy(ctx, site.ZoneID, site.LiveDeployID)
		switch {
		case err == nil:
			if doc.Live || doc.Phase == platform.DeployPhaseLive {
				live = append(live, doc)
			}
		case errors.Is(err, platform.ErrNotFound):
		default:
			return nil, err
		}
	}
	deploys, _, err := s.deps.Store.ListDeploys(ctx, site.ZoneID, deployRetireLimit, "")
	if err != nil {
		return live, err
	}
	for _, deploy := range deploys {
		if _, done := seen[deploy.ID]; done {
			continue
		}
		if !deploy.Live && deploy.Phase != platform.DeployPhaseLive {
			continue
		}
		live = append(live, deploy)
	}
	return live, nil
}

// removeHosts deletes each registered host from a kept app. A server without
// DeleteDomain leaves them, which the response reports honestly.
func (s *Service) removeHosts(ctx context.Context, doc platform.SiteDoc) ([]string, error) {
	orphaned := make([]string, 0, len(doc.Hostnames))
	supported := true
	for _, host := range doc.Hostnames {
		err := s.engine.DeleteDomain(ctx, s.cfg.Tenant, doc.App, host.Host)
		switch {
		case err == nil:
		case isUnsupported(err):
			supported = false
			orphaned = append(orphaned, host.Host)
		case isNotFound(err):
		default:
			return nil, err
		}
	}
	s.updateCapabilities(ctx, func(caps *platform.CapabilitiesDoc) { caps.DeleteDomain = supported })
	return orphaned, nil
}

// waitForDrain polls the engine for up to drainTimeout. DetachSite removes the
// hosts itself, so a healthy engine finishes inside the RPC and a slow one
// hands the rest to the watcher.
func (s *Service) waitForDrain(ctx context.Context, doc platform.SiteDoc) bool {
	deadline := time.Now().Add(s.drainTimeout)
	for {
		drained, err := s.hostsDrained(ctx, doc)
		if err == nil && drained {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		timer := time.NewTimer(detachDrainPoll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
	}
}

func (s *Service) ReapplySiteDns(
	ctx context.Context,
	request *connect.Request[hostingv1.ReapplySiteDnsRequest],
) (*connect.Response[hostingv1.ReapplySiteDnsResponse], error) {
	if !s.configured() {
		return nil, notConfigured()
	}
	doc, err := s.site(ctx, request.Msg.GetZoneId())
	if err != nil {
		return nil, err
	}
	if doc.ZoneMissing {
		return nil, failedPrecondition("this domain no longer exists; detach the site")
	}
	gateway, err := s.gatewayForAttach(ctx)
	if err != nil {
		return nil, err
	}

	response := &hostingv1.ReapplySiteDnsResponse{}
	err = s.deps.Serial.Run(ctx, doc.ZoneID, func(ctx context.Context) error {
		plan, err := s.planSite(ctx, doc, gateway)
		if err != nil {
			return err
		}
		response.DnsPlan = plan.ChangesProto()
		response.Conflicts = conflictChanges(plan)
		if request.Msg.GetDryRun() {
			response.DryRun = true
			return nil
		}
		if len(plan.Conflicts) > 0 && !request.Msg.GetReplaceConflictingRecords() {
			return conflictError(len(plan.Conflicts), response)
		}
		applyErr := s.applyPlan(ctx, &doc, plan, plan.ApplyOps(request.Msg.GetReplaceConflictingRecords()), "reapply-site-dns")
		if err := s.persistAfterApply(ctx, &doc, applyErr); err != nil {
			return err
		}
		return applyErr
	})
	if err != nil {
		return nil, s.publicError(err)
	}
	if !request.Msg.GetDryRun() {
		s.deps.Record(ctx, activity.Event{
			ZoneID: doc.ZoneID, ZoneName: doc.ZoneName, Actor: activity.ActorOperator,
			Kind: activity.KindSiteDNSReapplied, Severity: activity.SeverityInfo,
			Summary: fmt.Sprintf("Re-applied the website records of %s", doc.ZoneName),
		})
	}
	response.Site = s.renderSite(ctx, doc)
	return connect.NewResponse(response), nil
}

func (s *Service) CreateDeploy(
	ctx context.Context,
	request *connect.Request[hostingv1.CreateDeployRequest],
) (*connect.Response[hostingv1.CreateDeployResponse], error) {
	if !s.configured() {
		return nil, notConfigured()
	}
	message := request.Msg
	doc, err := s.site(ctx, message.GetZoneId())
	if err != nil {
		return nil, err
	}
	if err := s.noActiveDeploy(ctx, doc); err != nil {
		return nil, err
	}

	framework := FrameworkFromProto(message.GetFramework())
	if framework == "" {
		framework = Framework(doc.Framework)
	}
	if !framework.Valid() {
		return nil, invalidArgument("framework", "choose static, node or nextjs")
	}

	if strings.TrimSpace(message.GetUploadId()) != "" {
		return s.createUploadDeploy(ctx, doc, message, framework)
	}
	return s.createGitDeploy(ctx, doc, message, framework)
}

// noActiveDeploy enforces one running deploy per site: two concurrent builds
// would race for the same release weight.
func (s *Service) noActiveDeploy(ctx context.Context, doc platform.SiteDoc) error {
	active, err := s.deps.Store.ActiveDeploys(ctx)
	if err != nil {
		return connect.NewError(connect.CodeInternal, engineError("the deploy list could not be read"))
	}
	for _, deploy := range active {
		if deploy.ZoneID == doc.ZoneID {
			return failedPrecondition(fmt.Sprintf("a deploy is already running for %s", doc.ZoneName))
		}
	}
	return nil
}

// createGitDeploy validates everything before it persists anything, so a
// rejected repository URL leaves no row and no event behind.
func (s *Service) createGitDeploy(
	ctx context.Context,
	site platform.SiteDoc,
	message *hostingv1.CreateDeployRequest,
	framework Framework,
) (*connect.Response[hostingv1.CreateDeployResponse], error) {
	repository := orDefault(strings.TrimSpace(message.GetRepository()), site.Repository)
	if repository == "" {
		return nil, invalidArgument("repository", "is required")
	}
	revision := orDefault(strings.TrimSpace(message.GetRevision()), orDefault(site.Branch, defaultBranch))
	path := orDefault(strings.TrimSpace(message.GetPath()), site.Path)

	source, err := validateGitSource(repository, revision, path, message.GetGitToken())
	if err != nil {
		return nil, err
	}

	now := s.now()
	doc := platform.DeployDoc{
		V:                   platform.DocVersion,
		ID:                  s.deps.Store.NewDeployID(now),
		ZoneID:              site.ZoneID,
		ZoneName:            site.ZoneName,
		Kind:                DeployKindGit,
		Framework:           string(framework),
		Phase:               platform.DeployPhaseQueued,
		RequestedAt:         now,
		EngineCallStartedAt: &now,
		PollDeadline:        now.Add(s.cfg.BuildDeadline),
		Source: platform.DeploySourceDoc{
			Repository: source.Repository,
			Revision:   source.Revision,
			Path:       source.Path,
		},
	}
	if source.Token != "" {
		doc.Source.PrivateRepository = true
	}
	if err := s.deps.Store.PutDeploy(ctx, doc); err != nil {
		return nil, connect.NewError(connect.CodeInternal, engineError("the deploy could not be stored"))
	}

	buildID, buildErr := s.engine.CreateGitBuild(ctx, s.cfg.Tenant, site.App, GitBuildInput{
		Framework: framework,
		RepoURL:   source.Repository,
		Revision:  source.Revision,
		Path:      source.Path,
		GitToken:  source.Token,
	})
	if buildErr != nil {
		mapped := s.mapEngineError(buildErr)
		doc.EngineCallStartedAt = nil
		if err := s.finishDeploy(ctx, doc, platform.DeployPhaseFailed, reason(mapped.Error()),
			activity.KindDeployFailed, activity.SeverityError); err != nil {
			s.log().Warn("record failed deploy", "error", err)
		}
		return nil, mapped
	}

	// The engine now has a build this row does not name yet: the recovery
	// window spec2 §10.3 step 11 asks to be able to reproduce.
	s.crashAfterCreateGitBuild()

	doc.BuildID = buildID
	doc.EngineCallStartedAt = nil
	if err := s.deps.Store.PutDeploy(ctx, doc); err != nil {
		return nil, connect.NewError(connect.CodeInternal, engineError("the deploy could not be stored"))
	}
	s.afterDeployCreated(ctx, &site, doc)
	return connect.NewResponse(&hostingv1.CreateDeployResponse{Deploy: deployProto(doc)}), nil
}

// createUploadDeploy records the row and returns immediately; the upload
// deployer packs and streams the folder in the background.
func (s *Service) createUploadDeploy(
	ctx context.Context,
	site platform.SiteDoc,
	message *hostingv1.CreateDeployRequest,
	framework Framework,
) (*connect.Response[hostingv1.CreateDeployResponse], error) {
	if message.GetRepository() != "" || message.GetRevision() != "" || message.GetGitToken() != "" {
		return nil, invalidArgument("upload_id", "cannot be combined with a repository, a revision or a token")
	}
	upload, err := s.deps.Store.GetUpload(ctx, strings.TrimSpace(message.GetUploadId()))
	if errors.Is(err, platform.ErrNotFound) {
		return nil, notFound("this upload is no longer available; upload the folder again")
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, engineError("the upload could not be read"))
	}
	if upload.ZoneID != site.ZoneID {
		return nil, invalidArgument("upload_id", "belongs to a different domain")
	}
	if !upload.ExpiresAt.IsZero() && s.now().After(upload.ExpiresAt) {
		return nil, failedPrecondition("this upload expired; upload the folder again")
	}

	now := s.now()
	doc := platform.DeployDoc{
		V:            platform.DocVersion,
		ID:           s.deps.Store.NewDeployID(now),
		ZoneID:       site.ZoneID,
		ZoneName:     site.ZoneName,
		Kind:         DeployKindUpload,
		Framework:    string(framework),
		Phase:        platform.DeployPhaseQueued,
		UploadID:     upload.ID,
		RequestedAt:  now,
		PollDeadline: now.Add(s.cfg.BuildDeadline),
		Source: platform.DeploySourceDoc{
			UploadName:  upload.Name,
			UploadFiles: upload.Files,
			UploadBytes: upload.Bytes,
		},
	}
	if err := s.deps.Store.PutDeploy(ctx, doc); err != nil {
		return nil, connect.NewError(connect.CodeInternal, engineError("the deploy could not be stored"))
	}
	s.afterDeployCreated(ctx, &site, doc)
	s.pokeUploads()
	return connect.NewResponse(&hostingv1.CreateDeployResponse{Deploy: deployProto(doc)}), nil
}

// afterDeployCreated points the site at its newest deploy and records it.
func (s *Service) afterDeployCreated(ctx context.Context, site *platform.SiteDoc, doc platform.DeployDoc) {
	site.LatestDeployID = doc.ID
	if doc.Source.Repository != "" {
		site.Repository = doc.Source.Repository
	}
	touch(site, s.now())
	if err := s.deps.Store.PutSite(ctx, *site); err != nil {
		s.log().Warn("store site after deploy", "error", err)
	}
	details := map[string]string{"kind": doc.Kind}
	if doc.Source.Repository != "" {
		details["repository"] = doc.Source.Repository
		details["revision"] = doc.Source.Revision
	}
	s.deps.Record(ctx, activity.Event{
		ZoneID: doc.ZoneID, ZoneName: doc.ZoneName, Actor: activity.ActorOperator,
		Kind: activity.KindDeployRequested, Severity: activity.SeverityInfo,
		Summary: fmt.Sprintf("Started a deploy of %s", doc.ZoneName),
		Details: details,
	})
	s.pokePoller()
}

func (s *Service) ListDeploys(
	ctx context.Context,
	request *connect.Request[hostingv1.ListDeploysRequest],
) (*connect.Response[hostingv1.ListDeploysResponse], error) {
	if !s.configured() {
		return connect.NewResponse(&hostingv1.ListDeploysResponse{}), nil
	}
	limit := int(request.Msg.GetLimit())
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	docs, next, err := s.deps.Store.ListDeploys(ctx, strings.TrimSpace(request.Msg.GetZoneId()), limit, request.Msg.GetCursor())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, engineError("the deploy list could not be read"))
	}
	response := &hostingv1.ListDeploysResponse{
		Deploys:    make([]*hostingv1.Deploy, 0, len(docs)),
		NextCursor: next,
	}
	for _, doc := range docs {
		response.Deploys = append(response.Deploys, deployProto(doc))
	}
	return connect.NewResponse(response), nil
}

func (s *Service) GetDeploy(
	ctx context.Context,
	request *connect.Request[hostingv1.GetDeployRequest],
) (*connect.Response[hostingv1.GetDeployResponse], error) {
	if !s.configured() {
		return nil, notConfigured()
	}
	doc, err := s.deploy(ctx, request.Msg.GetZoneId(), request.Msg.GetDeployId())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&hostingv1.GetDeployResponse{Deploy: deployProto(doc)}), nil
}

func (s *Service) deploy(ctx context.Context, zoneID, deployID string) (platform.DeployDoc, error) {
	if strings.TrimSpace(zoneID) == "" {
		return platform.DeployDoc{}, invalidArgument("zone_id", "is required")
	}
	if strings.TrimSpace(deployID) == "" {
		return platform.DeployDoc{}, invalidArgument("deploy_id", "is required")
	}
	doc, err := s.deps.Store.GetDeploy(ctx, zoneID, deployID)
	if errors.Is(err, platform.ErrNotFound) {
		return platform.DeployDoc{}, notFound("this deploy no longer exists")
	}
	if err != nil {
		return platform.DeployDoc{}, connect.NewError(connect.CodeInternal, engineError("the deploy could not be read"))
	}
	return doc, nil
}

func (s *Service) GetDeployLog(
	ctx context.Context,
	request *connect.Request[hostingv1.GetDeployLogRequest],
) (*connect.Response[hostingv1.GetDeployLogResponse], error) {
	if !s.configured() {
		return nil, notConfigured()
	}
	doc, err := s.deploy(ctx, request.Msg.GetZoneId(), request.Msg.GetDeployId())
	if err != nil {
		return nil, err
	}
	tail := int(request.Msg.GetTailLines())
	if tail <= 0 {
		tail = 500
	}
	if tail > 1000 {
		tail = 1000
	}

	if doc.Active() && doc.BuildID != "" {
		if site, err := s.deps.Store.GetSite(ctx, doc.ZoneID); err == nil {
			lines, complete, err := s.engine.GetBuildLogs(ctx, s.cfg.Tenant, site.App, doc.BuildID, tail)
			if err == nil && len(lines) > 0 {
				return connect.NewResponse(&hostingv1.GetDeployLogResponse{
					Lines:    lines,
					Complete: complete,
					Source:   hostingv1.LogSource_LOG_SOURCE_LIVE,
				}), nil
			}
		}
	}

	log, err := s.deps.Store.GetDeployLog(ctx, doc.ID)
	if err == nil && len(log.Lines) > 0 {
		lines := log.Lines
		if len(lines) > tail {
			lines = lines[len(lines)-tail:]
		}
		response := &hostingv1.GetDeployLogResponse{
			Lines:     append([]string(nil), lines...),
			Complete:  log.Complete,
			Source:    hostingv1.LogSource_LOG_SOURCE_SNAPSHOT,
			Truncated: log.Truncated,
			Note: fmt.Sprintf("Snapshot captured %s — the hosting engine keeps logs for about an hour.",
				log.CapturedAt.UTC().Format(time.RFC3339)),
		}
		if !log.CapturedAt.IsZero() {
			response.CapturedAt = timestamppb.New(log.CapturedAt.UTC())
		}
		return connect.NewResponse(response), nil
	}

	return connect.NewResponse(&hostingv1.GetDeployLogResponse{
		Source: hostingv1.LogSource_LOG_SOURCE_NONE,
		Note:   "Build logs are kept by the hosting engine for about an hour after a build; this one is no longer available.",
	}), nil
}

func (s *Service) RollbackSite(
	ctx context.Context,
	request *connect.Request[hostingv1.RollbackSiteRequest],
) (*connect.Response[hostingv1.RollbackSiteResponse], error) {
	if !s.configured() {
		return nil, notConfigured()
	}
	site, err := s.site(ctx, request.Msg.GetZoneId())
	if err != nil {
		return nil, err
	}
	target, err := s.deploy(ctx, request.Msg.GetZoneId(), request.Msg.GetDeployId())
	if err != nil {
		return nil, err
	}
	if target.ReleaseID == "" {
		return nil, failedPrecondition("this deploy has no release to roll back to")
	}
	switch target.Phase {
	case platform.DeployPhaseLive, platform.DeployPhaseSuperseded:
	default:
		return nil, failedPrecondition("only a deploy that was live can be rolled back to")
	}
	if err := s.noActiveDeploy(ctx, site); err != nil {
		return nil, err
	}

	now := s.now()
	doc := platform.DeployDoc{
		V:              platform.DocVersion,
		ID:             s.deps.Store.NewDeployID(now),
		ZoneID:         site.ZoneID,
		ZoneName:       site.ZoneName,
		Kind:           DeployKindRollback,
		Framework:      target.Framework,
		Phase:          platform.DeployPhaseReleasing,
		BuildID:        target.BuildID,
		ReleaseID:      target.ReleaseID,
		Source:         target.Source,
		RolledBackFrom: site.LiveDeployID,
		RequestedAt:    now,
		PollDeadline:   now.Add(s.cfg.BuildDeadline),
	}
	if err := s.deps.Store.PutDeploy(ctx, doc); err != nil {
		return nil, connect.NewError(connect.CodeInternal, engineError("the rollback could not be stored"))
	}
	if err := s.engine.PromoteRelease(ctx, s.cfg.Tenant, site.App, target.ReleaseID); err != nil {
		mapped := s.mapEngineError(err)
		if failErr := s.finishDeploy(ctx, doc, platform.DeployPhaseFailed, reason(mapped.Error()),
			activity.KindDeployFailed, activity.SeverityError); failErr != nil {
			s.log().Warn("record failed rollback", "error", failErr)
		}
		return nil, mapped
	}
	s.afterDeployCreated(ctx, &site, doc)
	return connect.NewResponse(&hostingv1.RollbackSiteResponse{Deploy: deployProto(doc)}), nil
}

func (s *Service) ConfirmGatewayAddresses(
	ctx context.Context,
	request *connect.Request[hostingv1.ConfirmGatewayAddressesRequest],
) (*connect.Response[hostingv1.ConfirmGatewayAddressesResponse], error) {
	if !s.configured() {
		return nil, notConfigured()
	}
	gateway, err := s.deps.Store.GetGateway(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, engineError("the gateway state could not be read"))
	}
	if len(gateway.Pending) == 0 {
		return nil, failedPrecondition("no gateway addresses are waiting to be applied")
	}
	requested := formatAddresses(parseAddresses(request.Msg.GetAddresses()))
	if !slices.Equal(requested, gateway.Pending) {
		return nil, invalidArgument("addresses", "must be exactly the addresses waiting to be applied")
	}

	pending := append([]string(nil), gateway.Pending...)
	if err := s.applyGateway(ctx, gateway, pending, platform.GatewaySourceResolved, activity.ActorOperator); err != nil {
		return nil, connect.NewError(connect.CodeInternal, engineError("the gateway addresses could not be stored"))
	}
	sites, err := s.deps.Store.ListSites(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, engineError("the site list could not be read"))
	}
	queued := 0
	for _, site := range sites {
		switch site.State {
		case SiteStateAttaching, SiteStateReady, SiteStateDegraded:
			if !site.ZoneMissing {
				queued++
			}
		}
	}
	return connect.NewResponse(&hostingv1.ConfirmGatewayAddressesResponse{
		Addresses:   pending,
		SitesQueued: uint32(queued),
	}), nil
}

// -- helpers ---------------------------------------------------------------

// publicError keeps an already-mapped Connect error and turns anything else
// into a bounded, redacted message.
func (s *Service) publicError(err error) error {
	if err == nil {
		return nil
	}
	var connectErr *connect.Error
	if errors.As(err, &connectErr) {
		return connectErr
	}
	var validation *zone.ValidationError
	if errors.As(err, &validation) {
		return connect.NewError(connect.CodeInvalidArgument, engineError(validation.Error()))
	}
	return s.mapEngineError(err)
}

// conflictError refuses an explicit apply and carries the conflicting records
// as a Connect error detail, so the UI can offer "Replace these N records"
// without a second round trip.
func conflictError(count int, detail proto.Message) error {
	err := connect.NewError(connect.CodeFailedPrecondition,
		engineError(fmt.Sprintf("%d existing records conflict with the website records", count)))
	if item, detailErr := connect.NewErrorDetail(detail); detailErr == nil {
		err.AddDetail(item)
	}
	return err
}

// conflictChanges is the conflict subset of a plan, in the UI's vocabulary.
func conflictChanges(plan enginedns.ZonePlan) []*platformv1.RecordChange {
	changes := make([]*platformv1.RecordChange, 0, len(plan.Conflicts))
	for _, change := range plan.Changes {
		if change.Op != enginedns.ChangeReplace {
			continue
		}
		changes = append(changes, change.Proto())
	}
	return changes
}

// validHostZone applies the hostname-label rule the nameserver check uses.
// Phase-1 zone names may contain characters a host may not, so a zone that is
// not a hostname cannot be attached.
func validHostZone(name string) bool {
	name = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
	if name == "" || len(name) > 253 {
		return false
	}
	labels := strings.Split(name, ".")
	if len(labels) < 2 {
		return false
	}
	for _, label := range labels {
		if label == "" || len(label) > 63 {
			return false
		}
		for index, char := range label {
			switch {
			case char >= 'a' && char <= 'z', char >= '0' && char <= '9':
			case char == '-' && index > 0 && index < len(label)-1:
			default:
				return false
			}
		}
	}
	return true
}

func validateSubpath(value string) error {
	if value == "" {
		return nil
	}
	source, err := validateGitSource("https://github.com/owner/name", "", value, "")
	if err != nil {
		return invalidArgument("path", "must be a relative path inside the repository")
	}
	_ = source
	return nil
}

func orDefault(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

var (
	_ hostingv1connect.HostingServiceHandler = (*Service)(nil)
	_ platform.Probe                         = (*Service)(nil)
	_ platform.Rebuilder                     = (*Service)(nil)
)
