package hosting

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	hostingv1 "github.com/castlemilk/dns/gen/go/hosting/v1"
	"github.com/castlemilk/dns/internal/activity"
	"github.com/castlemilk/dns/internal/config"
	"github.com/castlemilk/dns/internal/enginedns"
	"github.com/castlemilk/dns/internal/hosting/fakehosting"
	"github.com/castlemilk/dns/internal/platform"
	"github.com/castlemilk/dns/internal/zone"
)

func attach(t *testing.T, h *harness, zoneID string, mutate ...func(*hostingv1.AttachSiteRequest)) *hostingv1.AttachSiteResponse {
	t.Helper()
	request := &hostingv1.AttachSiteRequest{
		ZoneId:    zoneID,
		Framework: hostingv1.Framework_FRAMEWORK_STATIC,
	}
	for _, apply := range mutate {
		apply(request)
	}
	response, err := h.service.AttachSite(context.Background(), connect.NewRequest(request))
	if err != nil {
		t.Fatalf("AttachSite: %v", err)
	}
	return response.Msg
}

func TestAttachSiteWritesRecordsThenRegistersHosts(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	zoneID := h.zoneID(t, "acme.dev")
	h.seedGateway(t, "198.51.100.10")

	response := attach(t, h, zoneID)
	if response.GetSite().GetState() != hostingv1.SiteState_SITE_STATE_ATTACHING {
		t.Fatalf("state = %v, want ATTACHING", response.GetSite().GetState())
	}

	// DNS is written before the hosts are registered: cert-manager's HTTP-01
	// challenge only succeeds once the name already resolves to the gateway.
	owned := h.engineRecords(t, zoneID)
	if len(owned) != 2 {
		t.Fatalf("engine records = %d, want an apex A and a www CNAME: %+v", len(owned), owned)
	}
	var apex, www *zone.Record
	for index := range owned {
		switch owned[index].Name {
		case "@":
			apex = &owned[index]
		case "www":
			www = &owned[index]
		}
	}
	if apex == nil || apex.Type != zone.TypeA || apex.Value != "198.51.100.10" {
		t.Errorf("apex record = %+v", apex)
	}
	if www == nil || www.Type != zone.TypeCNAME || www.Value != "acme.dev." {
		t.Errorf("www record = %+v", www)
	}

	h.service.watchDomains(context.Background())
	site := h.site(t, zoneID)
	if site.State != SiteStateReady {
		t.Fatalf("state after the watcher = %q (%q)", site.State, site.Reason)
	}
	if !h.events.has(activity.KindSiteAttached) || !h.events.has(activity.KindSiteReady) {
		t.Errorf("events = %v, want site.attached and site.ready", h.events.kinds())
	}

	// CreateApp is only ever issued after GetApp reported NotFound: DeepHost's
	// CreateApp is an upsert that would replace the whole spec.
	calls := h.engine.Calls()
	getIndex, createIndex := -1, -1
	creates := 0
	for index, call := range calls {
		switch call.Method {
		case "GetApp":
			if getIndex < 0 {
				getIndex = index
			}
		case "CreateApp":
			creates++
			if createIndex < 0 {
				createIndex = index
			}
		}
	}
	if creates != 1 {
		t.Errorf("CreateApp calls = %d, want exactly 1", creates)
	}
	if getIndex < 0 || createIndex < getIndex {
		t.Errorf("call order = %v, want GetApp before CreateApp", methodsOf(calls))
	}
}

func TestAttachSiteRefusesConflictsUnlessTheyAreReplaced(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	zoneID := h.zoneID(t, "acme.dev")
	h.seedGateway(t, "198.51.100.10")

	// A user A record at www stands in the way of the www CNAME.
	if _, err := h.zones.CreateRecord(context.Background(), zoneID, zone.Record{
		Name: "www", Type: zone.TypeA, TTL: 300, Value: "203.0.113.9",
	}); err != nil {
		t.Fatalf("seed conflicting record: %v", err)
	}

	_, err := h.service.AttachSite(context.Background(), connect.NewRequest(&hostingv1.AttachSiteRequest{
		ZoneId:    zoneID,
		Framework: hostingv1.Framework_FRAMEWORK_STATIC,
	}))
	if err == nil {
		t.Fatal("AttachSite accepted a zone with a conflicting record")
	}
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("code = %s, want failed_precondition", connect.CodeOf(err))
	}
	if !strings.Contains(err.Error(), "conflict") {
		t.Errorf("message = %q, want it to name the conflict", err.Error())
	}
	if _, storeErr := h.store.GetSite(context.Background(), zoneID); !errors.Is(storeErr, platform.ErrNotFound) {
		t.Error("a refused attach left a site row behind")
	}

	response := attach(t, h, zoneID, func(request *hostingv1.AttachSiteRequest) {
		request.ReplaceConflictingRecords = true
	})
	if response.GetSite() == nil {
		t.Fatal("AttachSite with replacement returned no site")
	}
	for _, record := range h.records(t, zoneID) {
		if record.Name == "www" && record.Type == zone.TypeA {
			t.Error("the conflicting A record survived the replacement")
		}
	}
}

func TestAttachSiteDryRunWritesNothing(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	zoneID := h.zoneID(t, "acme.dev")
	h.seedGateway(t, "198.51.100.10")

	response := attach(t, h, zoneID, func(request *hostingv1.AttachSiteRequest) { request.DryRun = true })
	if !response.GetDryRun() || len(response.GetDnsPlan()) == 0 {
		t.Fatalf("dry run response = %+v", response)
	}
	if _, err := h.store.GetSite(context.Background(), zoneID); !errors.Is(err, platform.ErrNotFound) {
		t.Error("a dry run created a site row")
	}
	if len(h.engineRecords(t, zoneID)) != 0 {
		t.Error("a dry run wrote records")
	}
	if len(h.engine.Calls()) != 0 {
		t.Errorf("a dry run called the engine: %v", methodsOf(h.engine.Calls()))
	}
}

func TestAttachSiteRefusesAZoneNameThatIsNotAHostname(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.seedGateway(t, "198.51.100.10")
	// Phase-1 zone names are looser than hostnames; the store accepts this one.
	zoneID := h.zoneID(t, "single")

	_, err := h.service.AttachSite(context.Background(), connect.NewRequest(&hostingv1.AttachSiteRequest{
		ZoneId:    zoneID,
		Framework: hostingv1.Framework_FRAMEWORK_STATIC,
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("code = %s, want invalid_argument", connect.CodeOf(err))
	}
}

func TestAttachSiteWithoutAGatewayIsRefusedWithTheVariableNames(t *testing.T) {
	t.Parallel()

	h := newHarness(t, func(cfg *config.Hosting) {
		cfg.GatewayAddresses = nil
		cfg.GatewayHostname = "gateway.local.test"
	})
	h.service.WithResolver(func(context.Context) ([]netip.Addr, error) { return nil, errors.New("no resolver") })
	zoneID := h.zoneID(t, "acme.dev")

	_, err := h.service.AttachSite(context.Background(), connect.NewRequest(&hostingv1.AttachSiteRequest{
		ZoneId:    zoneID,
		Framework: hostingv1.Framework_FRAMEWORK_STATIC,
	}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("code = %s, want failed_precondition", connect.CodeOf(err))
	}
	if !strings.Contains(err.Error(), "HOSTING_GATEWAY_HOSTNAME") {
		t.Errorf("message = %q, want the variable names", err.Error())
	}
}

func TestAttachResumesAfterACrashBetweenTheRowAndTheEngine(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	zoneID := h.zoneID(t, "acme.dev")
	h.seedGateway(t, "198.51.100.10")

	// The shape a crash leaves behind: the row is persisted (it is written
	// before any engine or DNS write) and nothing else happened.
	doc := platform.SiteDoc{
		V:         platform.DocVersion,
		ZoneID:    zoneID,
		ZoneName:  "acme.dev",
		App:       AppName("simple-test", "acme.dev"),
		Framework: string(FrameworkStatic),
		State:     SiteStateAttaching,
		WWW:       true,
		CreatedAt: h.clock.Now(),
		UpdatedAt: h.clock.Now(),
	}
	doc.AppRef = AppRef("simple-test", doc.App)
	doc.Hostnames = h.service.desiredHosts(doc)
	if err := h.store.PutSite(context.Background(), doc); err != nil {
		t.Fatalf("seed attaching site: %v", err)
	}

	if err := h.service.ReconcileAll(context.Background()); err != nil {
		t.Fatalf("ReconcileAll: %v", err)
	}
	h.service.watchDomains(context.Background())

	site := h.site(t, zoneID)
	if site.State != SiteStateReady {
		t.Fatalf("state = %q (%q), want the resumed attach to finish", site.State, site.Reason)
	}
	if len(h.engineRecords(t, zoneID)) != 2 {
		t.Errorf("engine records = %+v, want the apex and www records", h.engineRecords(t, zoneID))
	}
	if !h.events.has(activity.KindDNSEngineRestored) {
		t.Errorf("events = %v, want dns.engine.restored for the records the resume created", h.events.kinds())
	}
}

func TestOrphanSweepReleasesRecordsWithNoSiteRow(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	zoneID := h.zoneID(t, "acme.dev")
	h.seedGateway(t, "198.51.100.10")
	attach(t, h, zoneID)

	// The shape a crash between deleting the row and clearing its records
	// leaves behind.
	if err := h.store.DeleteSite(context.Background(), zoneID); err != nil {
		t.Fatalf("delete site: %v", err)
	}
	if err := h.service.sweepOrphans(context.Background()); err != nil {
		t.Fatalf("sweepOrphans: %v", err)
	}
	if owned := h.engineRecords(t, zoneID); len(owned) != 0 {
		t.Errorf("engine records = %+v, want them released to the user", owned)
	}
	if len(h.records(t, zoneID)) < 2 {
		t.Error("the sweep deleted the records instead of releasing them")
	}
	if !h.events.has(activity.KindDNSEngineOrphaned) {
		t.Errorf("events = %v, want dns.engine.orphaned", h.events.kinds())
	}
}

func TestDeployAdvancesThroughBuildingToLive(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	zoneID := h.zoneID(t, "acme.dev")
	h.seedGateway(t, "198.51.100.10")
	attach(t, h, zoneID)
	h.service.watchDomains(context.Background())

	deploy := createDeploy(t, h, zoneID, "https://github.com/owner/name", "main")
	if deploy.GetPhase() != hostingv1.DeployPhase_DEPLOY_PHASE_QUEUED {
		t.Fatalf("phase = %v, want QUEUED", deploy.GetPhase())
	}

	phases := []string{}
	ctx := context.Background()
	for range 8 {
		h.engine.Advance()
		h.clock.Advance(time.Second)
		h.service.pollOnce(ctx)
		phases = append(phases, h.deploy(t, zoneID, deploy.GetId()).Phase)
	}
	if !contains(phases, platform.DeployPhaseBuilding) {
		t.Errorf("phases = %v, want BUILDING among them", phases)
	}
	final := h.deploy(t, zoneID, deploy.GetId())
	if final.Phase != platform.DeployPhaseLive || !final.Live {
		t.Fatalf("final phase = %q live=%t", final.Phase, final.Live)
	}
	if h.site(t, zoneID).LiveDeployID != final.ID {
		t.Error("the site does not point at the live deploy")
	}
	if !h.events.has(activity.KindDeployLive) {
		t.Errorf("events = %v, want deploy.live", h.events.kinds())
	}
}

func TestDeployFailureCarriesTheEngineReason(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	zoneID := h.zoneID(t, "acme.dev")
	h.seedGateway(t, "198.51.100.10")
	attach(t, h, zoneID)

	h.engine.FailNextBuild("job failed (1 failures)")
	deploy := createDeploy(t, h, zoneID, "https://github.com/owner/name", "bad-branch")
	final := h.settle(t, zoneID, deploy.GetId(), 8)
	if final.Phase != platform.DeployPhaseFailed {
		t.Fatalf("phase = %q, want FAILED", final.Phase)
	}
	if final.Reason != "job failed (1 failures)" {
		t.Errorf("reason = %q, want the engine's reason", final.Reason)
	}
}

func TestDeployWithoutATerminalPhaseIsAbandonedAtTheDeadline(t *testing.T) {
	t.Parallel()

	h := newHarness(t, func(cfg *config.Hosting) { cfg.BuildDeadline = 5 * time.Minute })
	zoneID := h.zoneID(t, "acme.dev")
	h.seedGateway(t, "198.51.100.10")
	attach(t, h, zoneID)

	deploy := createDeploy(t, h, zoneID, "https://github.com/owner/name", "main")
	// One step so the build is Pending, then jump past the deadline without
	// ever letting the engine finish.
	h.engine.Advance()
	h.service.pollOnce(context.Background())
	h.clock.Advance(6 * time.Minute)
	h.service.pollOnce(context.Background())

	final := h.deploy(t, zoneID, deploy.GetId())
	if final.Phase != platform.DeployPhaseAbandoned {
		t.Fatalf("phase = %q, want ABANDONED", final.Phase)
	}
	if !strings.Contains(final.Reason, "no terminal phase") {
		t.Errorf("reason = %q", final.Reason)
	}
}

func TestDeployBecomesLostAfterFiveNotFoundPolls(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	zoneID := h.zoneID(t, "acme.dev")
	h.seedGateway(t, "198.51.100.10")
	attach(t, h, zoneID)

	deploy := createDeploy(t, h, zoneID, "https://github.com/owner/name", "main")
	h.engine.LoseBuild(h.deploy(t, zoneID, deploy.GetId()).BuildID)
	for range notFoundPollLimit {
		h.service.pollOnce(context.Background())
	}
	if phase := h.deploy(t, zoneID, deploy.GetId()).Phase; phase != platform.DeployPhaseLost {
		t.Fatalf("phase = %q, want LOST", phase)
	}
}

func TestDeployAdoptsABuildTheFacadeLostTheIdOf(t *testing.T) {
	t.Parallel()

	for _, noListBuilds := range []bool{false, true} {
		t.Run(map[bool]string{false: "via ListBuilds", true: "via ListReleases"}[noListBuilds], func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			zoneID := h.zoneID(t, "acme.dev")
			h.seedGateway(t, "198.51.100.10")
			attach(t, h, zoneID)
			site := h.site(t, zoneID)

			// The engine started a build; the facade crashed before it stored
			// the id.
			buildID, err := h.engine.CreateGitBuild(context.Background(), "simple-test", site.App, GitBuildInput{
				Framework: FrameworkStatic, RepoURL: "https://github.com/owner/name", Revision: "main",
			})
			if err != nil {
				t.Fatalf("seed engine build: %v", err)
			}
			for range 3 {
				h.engine.Advance()
			}
			// With the capability probed, the facade adopts through
			// ListBuilds; without it, caps.ListBuilds is false and only the
			// ListReleases fallback can find the build.
			if !noListBuilds {
				h.service.probeCapabilities(context.Background())
			}

			requested := h.clock.Now()
			doc := platform.DeployDoc{
				V: platform.DocVersion, ID: h.store.NewDeployID(requested),
				ZoneID: zoneID, ZoneName: "acme.dev", Kind: DeployKindGit,
				Framework: string(FrameworkStatic), Phase: platform.DeployPhaseQueued,
				RequestedAt: requested, PollDeadline: requested.Add(time.Hour),
			}
			if err := h.store.PutDeploy(context.Background(), doc); err != nil {
				t.Fatalf("seed deploy: %v", err)
			}

			h.clock.Advance(3 * time.Minute)
			h.service.pollOnce(context.Background())

			adopted := h.deploy(t, zoneID, doc.ID)
			if adopted.BuildID != buildID {
				t.Fatalf("build id = %q, want the adopted %q (phase %q)", adopted.BuildID, buildID, adopted.Phase)
			}
			if adopted.Reason != ReasonRecovered {
				t.Errorf("reason = %q, want %q", adopted.Reason, ReasonRecovered)
			}
			if adopted.Phase == platform.DeployPhaseFailed {
				t.Error("an adopted build was reported as FAILED")
			}
			if !h.events.has(activity.KindDeployRecovered) {
				t.Errorf("events = %v, want deploy.recovered", h.events.kinds())
			}
		})
	}
}

func TestDeployWithNothingToAdoptFails(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	zoneID := h.zoneID(t, "acme.dev")
	h.seedGateway(t, "198.51.100.10")
	attach(t, h, zoneID)

	requested := h.clock.Now()
	doc := platform.DeployDoc{
		V: platform.DocVersion, ID: h.store.NewDeployID(requested),
		ZoneID: zoneID, ZoneName: "acme.dev", Kind: DeployKindGit,
		Phase: platform.DeployPhaseQueued, RequestedAt: requested,
		PollDeadline: requested.Add(time.Hour),
	}
	if err := h.store.PutDeploy(context.Background(), doc); err != nil {
		t.Fatalf("seed deploy: %v", err)
	}
	h.clock.Advance(3 * time.Minute)
	h.service.pollOnce(context.Background())

	final := h.deploy(t, zoneID, doc.ID)
	if final.Phase != platform.DeployPhaseFailed || final.Reason != ReasonNeverConfirmed {
		t.Fatalf("deploy = %q / %q", final.Phase, final.Reason)
	}
}

func TestUploadDeployWithAnInFlightCallIsNotFailedEarly(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	zoneID := h.zoneID(t, "acme.dev")
	h.seedGateway(t, "198.51.100.10")
	attach(t, h, zoneID)

	requested := h.clock.Now()
	started := requested
	doc := platform.DeployDoc{
		V: platform.DocVersion, ID: h.store.NewDeployID(requested),
		ZoneID: zoneID, ZoneName: "acme.dev", Kind: DeployKindUpload,
		Phase: platform.DeployPhaseQueued, RequestedAt: requested,
		EngineCallStartedAt: &started, UploadID: "upload-1",
		PollDeadline: requested.Add(time.Hour),
	}
	if err := h.store.PutDeploy(context.Background(), doc); err != nil {
		t.Fatalf("seed deploy: %v", err)
	}
	h.clock.Advance(3 * time.Minute)
	h.service.pollOnce(context.Background())

	if phase := h.deploy(t, zoneID, doc.ID).Phase; phase != platform.DeployPhaseQueued {
		t.Fatalf("phase = %q, want the in-flight upload to stay QUEUED", phase)
	}
}

func TestRollbackCreatesARollbackRowAndFlipsLive(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	zoneID := h.zoneID(t, "acme.dev")
	h.seedGateway(t, "198.51.100.10")
	attach(t, h, zoneID)

	first := createDeploy(t, h, zoneID, "https://github.com/owner/name", "main")
	if phase := h.settle(t, zoneID, first.GetId(), 8).Phase; phase != platform.DeployPhaseLive {
		t.Fatalf("first deploy phase = %q", phase)
	}
	second := createDeploy(t, h, zoneID, "https://github.com/owner/name", "v2")
	if phase := h.settle(t, zoneID, second.GetId(), 8).Phase; phase != platform.DeployPhaseLive {
		t.Fatalf("second deploy phase = %q", phase)
	}
	if phase := h.deploy(t, zoneID, first.GetId()).Phase; phase != platform.DeployPhaseSuperseded {
		t.Fatalf("first deploy after the second = %q, want SUPERSEDED", phase)
	}

	response, err := h.service.RollbackSite(context.Background(), connect.NewRequest(&hostingv1.RollbackSiteRequest{
		ZoneId: zoneID, DeployId: first.GetId(),
	}))
	if err != nil {
		t.Fatalf("RollbackSite: %v", err)
	}
	rollback := response.Msg.GetDeploy()
	if rollback.GetKind() != hostingv1.DeployKind_DEPLOY_KIND_ROLLBACK {
		t.Fatalf("kind = %v, want ROLLBACK", rollback.GetKind())
	}
	if rollback.GetPhase() != hostingv1.DeployPhase_DEPLOY_PHASE_RELEASING {
		t.Fatalf("phase = %v, want RELEASING", rollback.GetPhase())
	}

	// The promoted release and the current one both carry weight 100 until the
	// engine's release controller zeroes the siblings, and the router
	// tie-breaks by newest creation: reporting LIVE before that would claim
	// traffic the rollback target does not yet receive.
	h.service.pollOnce(context.Background())
	if phase := h.deploy(t, zoneID, rollback.GetId()).Phase; phase != platform.DeployPhaseReleasing {
		t.Fatalf("phase = %q immediately after the promotion, want RELEASING until the siblings hit 0", phase)
	}

	final := h.settle(t, zoneID, rollback.GetId(), 6)
	if final.Phase != platform.DeployPhaseLive {
		t.Fatalf("rollback phase = %q", final.Phase)
	}
	if final.RolledBackFrom != second.GetId() {
		t.Errorf("rolled_back_from = %q, want the previously live deploy", final.RolledBackFrom)
	}
	if !h.events.has(activity.KindDeployRollback) {
		t.Errorf("events = %v, want deploy.rollback", h.events.kinds())
	}
}

func TestOneDeployAtATimePerSite(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	zoneID := h.zoneID(t, "acme.dev")
	h.seedGateway(t, "198.51.100.10")
	attach(t, h, zoneID)

	createDeploy(t, h, zoneID, "https://github.com/owner/name", "main")
	_, err := h.service.CreateDeploy(context.Background(), connect.NewRequest(&hostingv1.CreateDeployRequest{
		ZoneId: zoneID, Repository: "https://github.com/owner/name", Revision: "main",
	}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("code = %s, want failed_precondition", connect.CodeOf(err))
	}
	if !strings.Contains(err.Error(), "acme.dev") {
		t.Errorf("message = %q, want it to name the domain", err.Error())
	}
}

func TestDeployLogIsLiveThenSnapshotThenNone(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	zoneID := h.zoneID(t, "acme.dev")
	h.seedGateway(t, "198.51.100.10")
	attach(t, h, zoneID)

	deploy := createDeploy(t, h, zoneID, "https://github.com/owner/name", "main")
	buildID := h.deploy(t, zoneID, deploy.GetId()).BuildID
	h.engine.SetLogs(buildID, []string{"npm ci", "next build"}, false)

	log, err := h.service.GetDeployLog(context.Background(), connect.NewRequest(&hostingv1.GetDeployLogRequest{
		ZoneId: zoneID, DeployId: deploy.GetId(),
	}))
	if err != nil {
		t.Fatalf("GetDeployLog: %v", err)
	}
	if log.Msg.GetSource() != hostingv1.LogSource_LOG_SOURCE_LIVE {
		t.Fatalf("source = %v, want LIVE while the build runs", log.Msg.GetSource())
	}

	h.settle(t, zoneID, deploy.GetId(), 8)
	h.engine.SetLogs(buildID, nil, false)
	log, err = h.service.GetDeployLog(context.Background(), connect.NewRequest(&hostingv1.GetDeployLogRequest{
		ZoneId: zoneID, DeployId: deploy.GetId(),
	}))
	if err != nil {
		t.Fatalf("GetDeployLog: %v", err)
	}
	if log.Msg.GetSource() != hostingv1.LogSource_LOG_SOURCE_SNAPSHOT {
		t.Fatalf("source = %v, want SNAPSHOT after the build", log.Msg.GetSource())
	}
	if !strings.Contains(log.Msg.GetNote(), "about an hour") {
		t.Errorf("note = %q", log.Msg.GetNote())
	}

	// A deploy the facade never captured a log for says so, rather than
	// showing an empty pane.
	requested := h.clock.Now()
	bare := platform.DeployDoc{
		V: platform.DocVersion, ID: h.store.NewDeployID(requested), ZoneID: zoneID,
		ZoneName: "acme.dev", Kind: DeployKindGit, Phase: platform.DeployPhaseFailed,
		RequestedAt: requested,
	}
	if err := h.store.PutDeploy(context.Background(), bare); err != nil {
		t.Fatalf("seed deploy: %v", err)
	}
	log, err = h.service.GetDeployLog(context.Background(), connect.NewRequest(&hostingv1.GetDeployLogRequest{
		ZoneId: zoneID, DeployId: bare.ID,
	}))
	if err != nil {
		t.Fatalf("GetDeployLog: %v", err)
	}
	if log.Msg.GetSource() != hostingv1.LogSource_LOG_SOURCE_NONE {
		t.Fatalf("source = %v, want NONE", log.Msg.GetSource())
	}
	if !strings.Contains(log.Msg.GetNote(), "no longer available") {
		t.Errorf("note = %q", log.Msg.GetNote())
	}
}

func TestOldServerDegradesWithoutLyingAboutTimesOrReadiness(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.engine.SetOldServer(true)
	zoneID := h.zoneID(t, "acme.dev")
	h.seedGateway(t, "198.51.100.10")
	attach(t, h, zoneID)

	deploy := createDeploy(t, h, zoneID, "https://github.com/owner/name", "main")
	final := h.settle(t, zoneID, deploy.GetId(), 8)
	if final.Phase != platform.DeployPhaseLive {
		t.Fatalf("phase = %q on an old server", final.Phase)
	}
	rendered := deployProto(final)
	if rendered.GetTimeSource() != hostingv1.TimeSource_TIME_SOURCE_OBSERVED {
		t.Errorf("time source = %v, want OBSERVED on a server without build timestamps", rendered.GetTimeSource())
	}

	h.service.watchDomains(context.Background())
	for _, host := range h.site(t, zoneID).Hostnames {
		if host.State != HostStateRegistered {
			t.Errorf("host %s state = %q, want REGISTERED on a server that reports no readiness", host.Host, host.State)
		}
	}

	// ListBuilds and DeleteDomain are absent, and the capability probe says so
	// rather than surfacing an error.
	h.service.probeCapabilities(context.Background())
	caps := h.service.capabilities(context.Background())
	if caps.ListBuilds {
		t.Error("list_builds = true against an old server")
	}
}

func TestHostAlreadyServedElsewhereStopsRetrying(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	zoneID := h.zoneID(t, "acme.dev")
	h.seedGateway(t, "198.51.100.10")
	h.engine.SeedForeignHost("www.acme.dev", "other-tenant-site")

	attach(t, h, zoneID)
	h.service.watchDomains(context.Background())
	before := countCalls(h.engine.Calls(), "CreateDomain")
	h.service.watchDomains(context.Background())
	after := countCalls(h.engine.Calls(), "CreateDomain")

	site := h.site(t, zoneID)
	if site.State != SiteStateDegraded {
		t.Fatalf("state = %q, want DEGRADED", site.State)
	}
	var www *platform.HostnameDoc
	for index := range site.Hostnames {
		if site.Hostnames[index].Role == HostRoleWWW {
			www = &site.Hostnames[index]
		}
	}
	if www == nil || www.State != HostStateMissing || www.Retrying {
		t.Fatalf("www hostname = %+v, want MISSING and not retrying", www)
	}
	if !strings.Contains(www.Reason, "already served by another site") {
		t.Errorf("reason = %q, want the neutral message", www.Reason)
	}
	if strings.Contains(www.Reason, "other-tenant-site") {
		t.Errorf("reason = %q, want the other tenant's app name kept out of it", www.Reason)
	}
	if after-before > 1 {
		t.Errorf("CreateDomain retried %d times for a host that can never be registered", after-before)
	}
}

func TestDetachDeletesTheAppAndReleasesOrRemovesRecords(t *testing.T) {
	t.Parallel()

	for _, keep := range []bool{false, true} {
		t.Run(map[bool]string{false: "records removed", true: "records kept"}[keep], func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			zoneID := h.zoneID(t, "acme.dev")
			h.seedGateway(t, "198.51.100.10")
			attach(t, h, zoneID)
			h.service.watchDomains(context.Background())

			_, err := h.service.DetachSite(context.Background(), connect.NewRequest(&hostingv1.DetachSiteRequest{
				ZoneId: zoneID, KeepDnsRecords: keep,
			}))
			if err != nil {
				t.Fatalf("DetachSite: %v", err)
			}
			if _, err := h.store.GetSite(context.Background(), zoneID); !errors.Is(err, platform.ErrNotFound) {
				t.Fatalf("the site row survived a completed detach: %v", err)
			}
			if owned := h.engineRecords(t, zoneID); len(owned) != 0 {
				t.Errorf("engine-owned records survived: %+v", owned)
			}
			records := h.records(t, zoneID)
			userRecords := 0
			for _, record := range records {
				if !record.Managed && record.Source == enginedns.SourceUser {
					userRecords++
				}
			}
			if keep && userRecords != 2 {
				t.Errorf("kept records = %d, want the apex and www records to become editable", userRecords)
			}
			if !keep && userRecords != 0 {
				t.Errorf("records = %d, want them deleted", userRecords)
			}
			if !h.events.has(activity.KindSiteDetached) {
				t.Errorf("events = %v, want site.detached", h.events.kinds())
			}
		})
	}
}

// A detach that deletes the app removes the hosts itself, because DeepHost does
// not garbage-collect Domain CRs with the App. On a server that has
// DeleteDomain the row is therefore gone when the RPC returns.
func TestDetachDeletingTheAppRemovesTheHostsItself(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	zoneID := h.zoneID(t, "acme.dev")
	h.seedGateway(t, "198.51.100.10")
	attach(t, h, zoneID)
	h.service.watchDomains(context.Background())

	// The engine keeps the app's hosts listed long after DeleteApp; only the
	// facade's own DeleteDomain calls can clear them.
	h.engine.SetSlowDelete(10 * time.Minute)
	response, err := h.service.DetachSite(context.Background(), connect.NewRequest(&hostingv1.DetachSiteRequest{ZoneId: zoneID}))
	if err != nil {
		t.Fatalf("DetachSite: %v", err)
	}
	if response.Msg.GetPending() {
		t.Error("pending = true, want the detach to finish inline once the hosts are removed")
	}
	if _, err := h.store.GetSite(context.Background(), zoneID); !errors.Is(err, platform.ErrNotFound) {
		t.Fatalf("the site row survived the detach: %v", err)
	}
	var deleted []string
	for _, call := range h.engine.Calls() {
		if call.Method == "DeleteDomain" {
			deleted = append(deleted, call.Args["host"])
		}
	}
	if len(deleted) != 2 {
		t.Errorf("DeleteDomain calls = %v, want one per registered host", deleted)
	}
}

// On a server without DeleteDomain the hosts stay until the engine collects
// them, so the RPC answers pending and the watcher finishes the row.
func TestDetachReportsPendingWhileTheEngineIsStillRemovingHosts(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.engine.SetOldServer(true)
	zoneID := h.zoneID(t, "acme.dev")
	h.seedGateway(t, "198.51.100.10")
	attach(t, h, zoneID)
	h.service.watchDomains(context.Background())

	h.engine.SetSlowDelete(10 * time.Minute)
	response, err := h.service.DetachSite(context.Background(), connect.NewRequest(&hostingv1.DetachSiteRequest{ZoneId: zoneID}))
	if err != nil {
		t.Fatalf("DetachSite: %v", err)
	}
	if !response.Msg.GetPending() {
		t.Fatal("pending = false while the engine still lists the hosts")
	}
	if h.site(t, zoneID).State != SiteStateDetaching {
		t.Fatal("the row is not DETACHING")
	}

	// Re-attaching is refused until the removal finishes.
	_, err = h.service.AttachSite(context.Background(), connect.NewRequest(&hostingv1.AttachSiteRequest{
		ZoneId: zoneID, Framework: hostingv1.Framework_FRAMEWORK_STATIC,
	}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("re-attach code = %s, want failed_precondition", connect.CodeOf(err))
	}

	// Once the engine's garbage collection catches up, the watcher finishes.
	h.clock.Advance(11 * time.Minute)
	h.service.watchDomains(context.Background())
	if _, err := h.store.GetSite(context.Background(), zoneID); !errors.Is(err, platform.ErrNotFound) {
		t.Fatalf("the watcher did not finish the detach: %v", err)
	}
}

func TestDetachKeepingTheAppReportsOrphanedHostsOnAnOldServer(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.engine.SetOldServer(true)
	zoneID := h.zoneID(t, "acme.dev")
	h.seedGateway(t, "198.51.100.10")
	attach(t, h, zoneID)
	h.service.watchDomains(context.Background())

	response, err := h.service.DetachSite(context.Background(), connect.NewRequest(&hostingv1.DetachSiteRequest{
		ZoneId: zoneID, KeepApp: true,
	}))
	if err != nil {
		t.Fatalf("DetachSite: %v", err)
	}
	if len(response.Msg.GetOrphanedHosts()) == 0 {
		t.Fatal("orphaned_hosts is empty on a server without DeleteDomain")
	}
	if !strings.Contains(response.Msg.GetNote(), "no domain removal API") {
		t.Errorf("note = %q", response.Msg.GetNote())
	}
	if !h.events.has(activity.KindSiteDetachPartial) {
		t.Errorf("events = %v, want site.detach.partial", h.events.kinds())
	}
}

func TestErrorMappingNeverSurfacesEngineInternals(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	cases := []struct {
		name string
		err  error
		code connect.Code
		copy string
	}{
		{"unauthenticated", connect.NewError(connect.CodeUnauthenticated, errors.New("bad token abc")), connect.CodeUnavailable, CopyCredentialsRejected},
		{"tenant scope", connect.NewError(connect.CodePermissionDenied, errors.New("outside scope")), connect.CodeUnavailable, "not scoped to tenant simple-test"},
		{"app name taken", connect.NewError(connect.CodeAlreadyExists, errors.New(`app "x" belongs to another tenant or app`)), connect.CodeFailedPrecondition, CopyAppNameTaken},
		{"app gone", connect.NewError(connect.CodeNotFound, errors.New("app not found")), connect.CodeFailedPrecondition, CopyAppGone},
		{"too large", connect.NewError(connect.CodeResourceExhausted, errors.New("deployment exceeds")), connect.CodeResourceExhausted, CopyUploadTooLarge},
		{"transport", errors.New("dial tcp 10.0.0.1:80: connect: refused"), connect.CodeUnavailable, CopyUnreachable},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			mapped := h.service.mapEngineError(test.err)
			if connect.CodeOf(mapped) != test.code {
				t.Errorf("code = %s, want %s", connect.CodeOf(mapped), test.code)
			}
			if !strings.Contains(mapped.Error(), test.copy) {
				t.Errorf("message = %q, want it to contain %q", mapped.Error(), test.copy)
			}
			if strings.Contains(mapped.Error(), "10.0.0.1") || strings.Contains(mapped.Error(), "abc") {
				t.Errorf("message leaked engine internals: %q", mapped.Error())
			}
		})
	}
}

func TestUnconfiguredServiceReadsEmptyAndRefusesWrites(t *testing.T) {
	t.Parallel()

	deps := platform.Deps{}
	service, uploads := New(config.Hosting{}, deps)
	if uploads != nil {
		t.Error("an unconfigured facade returned an upload handler")
	}

	list, err := service.ListSites(context.Background(), connect.NewRequest(&hostingv1.ListSitesRequest{}))
	if err != nil || len(list.Msg.GetSites()) != 0 {
		t.Fatalf("ListSites = %+v, %v", list, err)
	}
	deploys, err := service.ListDeploys(context.Background(), connect.NewRequest(&hostingv1.ListDeploysRequest{}))
	if err != nil || len(deploys.Msg.GetDeploys()) != 0 {
		t.Fatalf("ListDeploys = %+v, %v", deploys, err)
	}
	status, err := service.GetHostingStatus(context.Background(), connect.NewRequest(&hostingv1.GetHostingStatusRequest{}))
	if err != nil {
		t.Fatalf("GetHostingStatus: %v", err)
	}
	if status.Msg.GetEngine().GetConfigured() {
		t.Error("configured = true with no configuration")
	}
	if len(status.Msg.GetEngine().GetMissingEnv()) == 0 {
		t.Error("missing_env is empty; the UI has no variable names to show")
	}

	_, err = service.AttachSite(context.Background(), connect.NewRequest(&hostingv1.AttachSiteRequest{ZoneId: "z"}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "HOSTING_API_URL") {
		t.Fatalf("AttachSite error = %v", err)
	}
}

// -- helpers ---------------------------------------------------------------

func createDeploy(t *testing.T, h *harness, zoneID, repository, revision string) *hostingv1.Deploy {
	t.Helper()
	response, err := h.service.CreateDeploy(context.Background(), connect.NewRequest(&hostingv1.CreateDeployRequest{
		ZoneId: zoneID, Repository: repository, Revision: revision,
	}))
	if err != nil {
		t.Fatalf("CreateDeploy: %v", err)
	}
	return response.Msg.GetDeploy()
}

func methodsOf(calls []fakehosting.Call) []string {
	methods := make([]string, 0, len(calls))
	for _, call := range calls {
		methods = append(methods, call.Method)
	}
	return methods
}

func countCalls(calls []fakehosting.Call, method string) int {
	count := 0
	for _, call := range calls {
		if call.Method == method {
			count++
		}
	}
	return count
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// DetachSite answers inside one HTTP response, so its inline drain must finish
// before newHTTPServer's 30 s WriteTimeout (internal/app/app.go). A longer wait
// costs the caller its response even though the detach succeeded; the pending
// path plus the domain watcher is what covers a slower engine.
func TestTheInlineDetachDrainFitsInsideTheServerWriteTimeout(t *testing.T) {
	t.Parallel()

	const serverWriteTimeout = 30 * time.Second
	if detachDrainTimeout >= serverWriteTimeout {
		t.Fatalf("detachDrainTimeout = %s, want well under the %s write timeout", detachDrainTimeout, serverWriteTimeout)
	}
}

// The deploy history survives a detach, so the row that was live must stop
// claiming to be: nothing serves a detached site.
func TestDetachRetiresTheLiveDeploy(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	zoneID := h.zoneID(t, "acme.dev")
	h.seedGateway(t, "198.51.100.10")
	attach(t, h, zoneID)
	h.service.watchDomains(context.Background())

	deploy := createDeploy(t, h, zoneID, "https://github.com/owner/name", "main")
	live := h.settle(t, zoneID, deploy.GetId(), 8)
	if live.Phase != platform.DeployPhaseLive || !live.Live {
		t.Fatalf("deploy = %q live=%v, want a LIVE deploy to detach", live.Phase, live.Live)
	}

	if _, err := h.service.DetachSite(context.Background(), connect.NewRequest(&hostingv1.DetachSiteRequest{
		ZoneId: zoneID,
	})); err != nil {
		t.Fatalf("DetachSite: %v", err)
	}

	retired := h.deploy(t, zoneID, deploy.GetId())
	if retired.Live {
		t.Errorf("the deploy of a detached site is still live")
	}
	if retired.Phase != platform.DeployPhaseSuperseded {
		t.Errorf("phase = %q, want SUPERSEDED", retired.Phase)
	}
}
