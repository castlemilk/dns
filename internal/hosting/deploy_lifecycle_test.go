package hosting

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"
	hostingv1 "github.com/castlemilk/dns/gen/go/hosting/v1"
	"github.com/castlemilk/dns/internal/activity"
	"github.com/castlemilk/dns/internal/hosting/engineapi"
	"github.com/castlemilk/dns/internal/platform"
)

// errNoAnswer is the transport failure a control plane mid-rollout returns.
var errNoAnswer = errors.New("the hosting engine did not answer")

// flakyEngine fails one method while every other call goes to the real fake
// engine, which is what a control plane mid-rollout looks like: it is there,
// but it cannot answer this question right now.
type flakyEngine struct {
	Engine
	listBuildsErr   error
	listReleasesErr error
}

func (e flakyEngine) ListBuilds(
	ctx context.Context,
	tenant, name string,
	limit int,
) ([]engineapi.BuildView, error) {
	if e.listBuildsErr != nil {
		return nil, e.listBuildsErr
	}
	return e.Engine.ListBuilds(ctx, tenant, name, limit)
}

func (e flakyEngine) ListReleases(
	ctx context.Context,
	tenant, name string,
) ([]engineapi.ReleaseView, error) {
	if e.listReleasesErr != nil {
		return nil, e.listReleasesErr
	}
	return e.Engine.ListReleases(ctx, tenant, name)
}

// TestDetachingASiteMidDeployEndsTheDeploy covers the row a detach used to
// strand. retireDeploys only ever touched live rows, the poller returned on its
// first statement once the site was gone, and the janitor refuses to trim an
// active row — so a QUEUED deploy stayed active for ever and every later deploy
// of that zone was refused with "a deploy is already running".
func TestDetachingASiteMidDeployEndsTheDeploy(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := newHarness(t)
	zoneID := h.zoneID(t, "acme.dev")
	h.seedGateway(t, "198.51.100.10")
	attach(t, h, zoneID)

	created, err := h.service.CreateDeploy(ctx, connect.NewRequest(&hostingv1.CreateDeployRequest{
		ZoneId:     zoneID,
		Repository: "https://github.com/owner/name",
	}))
	if err != nil {
		t.Fatalf("CreateDeploy: %v", err)
	}
	deployID := created.Msg.GetDeploy().GetId()
	h.service.pollOnce(ctx)

	if _, err := h.service.DetachSite(ctx, connect.NewRequest(&hostingv1.DetachSiteRequest{
		ZoneId: zoneID,
	})); err != nil {
		t.Fatalf("DetachSite: %v", err)
	}

	// Thirty minutes of polling with no site row must not leave the deploy
	// running.
	for range 30 {
		h.clock.Advance(time.Minute)
		h.engine.Advance()
		h.service.pollOnce(ctx)
	}

	row := h.deploy(t, zoneID, deployID)
	if row.Active() {
		t.Fatalf("deploy phase = %q, want a terminal phase after the detach", row.Phase)
	}
	if row.Reason != ReasonSiteDetached {
		t.Errorf("reason = %q, want %q", row.Reason, ReasonSiteDetached)
	}

	// The zone can be attached and deployed again.
	attach(t, h, zoneID)
	if _, err := h.service.CreateDeploy(ctx, connect.NewRequest(&hostingv1.CreateDeployRequest{
		ZoneId:     zoneID,
		Repository: "https://github.com/owner/name",
	})); err != nil {
		t.Fatalf("CreateDeploy after re-attaching: %v", err)
	}
}

// TestAPolledDeployWhoseSiteIsGoneIsEnded is the fail-safe half: whatever left
// an active row behind a missing site, the poller ends it rather than skipping
// it for ever.
func TestAPolledDeployWhoseSiteIsGoneIsEnded(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := newHarness(t)
	zoneID := h.zoneID(t, "acme.dev")
	now := h.clock.Now()
	doc := platform.DeployDoc{
		V: platform.DocVersion, ID: h.store.NewDeployID(now),
		ZoneID: zoneID, ZoneName: "acme.dev", Kind: DeployKindGit,
		Phase: platform.DeployPhaseBuilding, BuildID: "b-orphan",
		RequestedAt: now, PollDeadline: now.Add(time.Hour),
	}
	if err := h.store.PutDeploy(ctx, doc); err != nil {
		t.Fatalf("seed deploy: %v", err)
	}

	h.service.pollOnce(ctx)

	row := h.deploy(t, zoneID, doc.ID)
	if row.Phase != platform.DeployPhaseAbandoned || row.Reason != ReasonSiteDetached {
		t.Fatalf("deploy = %q / %q, want ABANDONED with the detach reason", row.Phase, row.Reason)
	}
}

// TestTheLiveDeployIsRetiredEvenWhenItIsOffTheNewestPage covers a site with
// more deploys than one page holds. The row the site points at is the live one;
// scanning only the newest page missed it, so two rows read LIVE at once and a
// detach left one of them claiming to serve traffic.
func TestTheLiveDeployIsRetiredEvenWhenItIsOffTheNewestPage(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := newHarness(t)
	zoneID := h.zoneID(t, "acme.dev")
	h.seedGateway(t, "198.51.100.10")
	attach(t, h, zoneID)

	oldest := seedDeploy(t, h, zoneID, platform.DeployPhaseLive, true)
	for range deployRetireLimit {
		h.clock.Advance(time.Second)
		seedDeploy(t, h, zoneID, platform.DeployPhaseFailed, false)
	}
	site := h.site(t, zoneID)
	site.LiveDeployID = oldest.ID
	if err := h.store.PutSite(ctx, site); err != nil {
		t.Fatalf("point the site at its live deploy: %v", err)
	}

	page, _, err := h.store.ListDeploys(ctx, zoneID, deployRetireLimit, "")
	if err != nil {
		t.Fatalf("list deploys: %v", err)
	}
	for _, deploy := range page {
		if deploy.ID == oldest.ID {
			t.Fatal("the live deploy is still on the newest page; the test proves nothing")
		}
	}

	h.service.retireDeploys(ctx, h.site(t, zoneID))

	retired := h.deploy(t, zoneID, oldest.ID)
	if retired.Live || retired.Phase != platform.DeployPhaseSuperseded {
		t.Fatalf("deploy = %q / live %v, want SUPERSEDED", retired.Phase, retired.Live)
	}
}

// TestSupersedingRetiresALiveDeployOffTheNewestPage is the same page bound on
// the promotion path: without it a new deploy going live left the old one
// reading LIVE, so the console showed two live deploys for one site.
func TestSupersedingRetiresALiveDeployOffTheNewestPage(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := newHarness(t)
	zoneID := h.zoneID(t, "acme.dev")
	h.seedGateway(t, "198.51.100.10")
	attach(t, h, zoneID)

	oldest := seedDeploy(t, h, zoneID, platform.DeployPhaseLive, true)
	for range deployRetireLimit {
		h.clock.Advance(time.Second)
		seedDeploy(t, h, zoneID, platform.DeployPhaseFailed, false)
	}
	site := h.site(t, zoneID)
	site.LiveDeployID = oldest.ID
	if err := h.store.PutSite(ctx, site); err != nil {
		t.Fatalf("point the site at its live deploy: %v", err)
	}

	h.clock.Advance(time.Second)
	newest := seedDeploy(t, h, zoneID, platform.DeployPhaseLive, true)
	if err := h.service.supersedePrevious(ctx, newest, h.site(t, zoneID)); err != nil {
		t.Fatalf("supersedePrevious: %v", err)
	}

	retired := h.deploy(t, zoneID, oldest.ID)
	if retired.Live || retired.Phase != platform.DeployPhaseSuperseded {
		t.Fatalf("previous deploy = %q / live %v, want SUPERSEDED", retired.Phase, retired.Live)
	}
	if promoted := h.deploy(t, zoneID, newest.ID); !promoted.Live {
		t.Error("the new deploy stopped being live")
	}
}

func seedDeploy(t *testing.T, h *harness, zoneID, phase string, live bool) platform.DeployDoc {
	t.Helper()
	now := h.clock.Now()
	doc := platform.DeployDoc{
		V: platform.DocVersion, ID: h.store.NewDeployID(now),
		ZoneID: zoneID, ZoneName: "acme.dev", Kind: DeployKindGit,
		Phase: phase, Live: live, RequestedAt: now, PollDeadline: now.Add(time.Hour),
	}
	if err := h.store.PutDeploy(context.Background(), doc); err != nil {
		t.Fatalf("seed deploy: %v", err)
	}
	return doc
}

// TestAnOldOrUndatedReleaseIsNotAdopted bounds build adoption on the release
// side. An app name is a pure function of tenant and zone, so a zone that was
// deleted and recreated lands on the same app with its old releases on it;
// adopting one reported an ancient release as this deploy and rolled the site
// back onto it.
func TestAnOldOrUndatedReleaseIsNotAdopted(t *testing.T) {
	t.Parallel()

	for _, undated := range []bool{false, true} {
		t.Run(map[bool]string{false: "created long before this deploy", true: "not dated at all"}[undated],
			func(t *testing.T) {
				t.Parallel()

				ctx := context.Background()
				h := newHarness(t)
				zoneID := h.zoneID(t, "acme.dev")
				h.seedGateway(t, "198.51.100.10")
				attach(t, h, zoneID)
				site := h.site(t, zoneID)

				// One release from the zone's previous life, on the same app.
				if _, err := h.engine.CreateGitBuild(ctx, "simple-test", site.App, GitBuildInput{
					Framework: FrameworkStatic,
					RepoURL:   "https://github.com/owner/name",
					Revision:  "v1.0.0",
				}); err != nil {
					t.Fatalf("seed engine build: %v", err)
				}
				for range 3 {
					h.engine.Advance()
				}
				if undated {
					h.engine.SetOldServer(true)
				} else {
					h.clock.Advance(90 * 24 * time.Hour)
				}

				requested := h.clock.Now()
				doc := platform.DeployDoc{
					V: platform.DocVersion, ID: h.store.NewDeployID(requested),
					ZoneID: zoneID, ZoneName: "acme.dev", Kind: DeployKindGit,
					Phase: platform.DeployPhaseQueued, RequestedAt: requested,
					PollDeadline: requested.Add(time.Hour),
					Source: platform.DeploySourceDoc{
						Repository: "https://github.com/owner/name", Revision: "v2.0.0",
					},
				}
				if err := h.store.PutDeploy(ctx, doc); err != nil {
					t.Fatalf("seed deploy: %v", err)
				}

				h.clock.Advance(3 * time.Minute)
				h.service.pollOnce(ctx)

				row := h.deploy(t, zoneID, doc.ID)
				if row.BuildID != "" || row.ReleaseID != "" {
					t.Fatalf("deploy adopted build %q / release %q from another life of this zone",
						row.BuildID, row.ReleaseID)
				}
				if row.Phase != platform.DeployPhaseFailed || row.Reason != ReasonNeverConfirmed {
					t.Errorf("deploy = %q / %q, want FAILED never-confirmed", row.Phase, row.Reason)
				}
				if refreshed := h.site(t, zoneID); refreshed.LiveDeployID == doc.ID {
					t.Error("the site was pointed at a deploy that adopted an old release")
				}
			})
	}
}

// TestATransientListFailureDoesNotFailARunningDeploy is the other half of
// adoption: a row is only ever ended on an answer from the engine, never
// because the engine could not be asked. The build in this scenario really is
// running, and marking it FAILED left the console showing a failed deploy while
// the engine took it live.
func TestATransientListFailureDoesNotFailARunningDeploy(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := newHarness(t)
	zoneID := h.zoneID(t, "acme.dev")
	h.seedGateway(t, "198.51.100.10")
	attach(t, h, zoneID)
	site := h.site(t, zoneID)
	h.service.probeCapabilities(ctx)

	buildID, err := h.engine.CreateGitBuild(ctx, "simple-test", site.App, GitBuildInput{
		Framework: FrameworkStatic, RepoURL: "https://github.com/owner/name", Revision: "main",
	})
	if err != nil {
		t.Fatalf("seed engine build: %v", err)
	}

	requested := h.clock.Now()
	doc := platform.DeployDoc{
		V: platform.DocVersion, ID: h.store.NewDeployID(requested),
		ZoneID: zoneID, ZoneName: "acme.dev", Kind: DeployKindGit,
		Phase: platform.DeployPhaseQueued, RequestedAt: requested,
		PollDeadline: requested.Add(time.Hour),
	}
	if err := h.store.PutDeploy(ctx, doc); err != nil {
		t.Fatalf("seed deploy: %v", err)
	}

	unavailable := connect.NewError(connect.CodeUnavailable, errNoAnswer)
	h.service.WithEngine(flakyEngine{Engine: h.engine, listBuildsErr: unavailable, listReleasesErr: unavailable})
	h.clock.Advance(3 * time.Minute)
	h.service.pollOnce(ctx)

	row := h.deploy(t, zoneID, doc.ID)
	if row.Phase == platform.DeployPhaseFailed {
		t.Fatalf("deploy = %q / %q, want a row that is still running", row.Phase, row.Reason)
	}
	if h.events.has(activity.KindDeployFailed) {
		t.Errorf("events = %v, want no failure recorded for an engine that did not answer", h.events.kinds())
	}

	// The engine answers again and the build is adopted.
	h.service.WithEngine(h.engine)
	h.service.pollOnce(ctx)
	if adopted := h.deploy(t, zoneID, doc.ID); adopted.BuildID != buildID {
		t.Fatalf("build id = %q, want the adopted %q once the engine answered", adopted.BuildID, buildID)
	}
}

// TestARollbackThatNeverTakesEffectFailsInsideItsOwnWindow bounds an explicit
// rollback. Promoting an existing release is one update on one object, so a
// minute without the weight flip is a failure — not a build worth waiting the
// whole build deadline for and then blaming on a build that never ran.
func TestARollbackThatNeverTakesEffectFailsInsideItsOwnWindow(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := newHarness(t)
	zoneID := h.zoneID(t, "acme.dev")
	h.seedGateway(t, "198.51.100.10")
	attach(t, h, zoneID)

	first := gitDeploy(t, h, zoneID, "v1.0.0")
	if phase := h.settle(t, zoneID, first, 12).Phase; phase != platform.DeployPhaseLive {
		t.Fatalf("first deploy phase = %q", phase)
	}
	second := gitDeploy(t, h, zoneID, "v2.0.0")
	if phase := h.settle(t, zoneID, second, 12).Phase; phase != platform.DeployPhaseLive {
		t.Fatalf("second deploy phase = %q", phase)
	}

	response, err := h.service.RollbackSite(ctx, connect.NewRequest(&hostingv1.RollbackSiteRequest{
		ZoneId: zoneID, DeployId: first,
	}))
	if err != nil {
		t.Fatalf("RollbackSite: %v", err)
	}
	rollbackID := response.Msg.GetDeploy().GetId()

	// The engine never finishes the flip: the release the rollback targets and
	// the one it replaces both carry traffic.
	h.clock.Advance(rollbackConfirmWindow + time.Second)
	h.service.pollOnce(ctx)

	row := h.deploy(t, zoneID, rollbackID)
	if row.Phase != platform.DeployPhaseFailed {
		t.Fatalf("rollback = %q / %q, want FAILED inside the confirm window", row.Phase, row.Reason)
	}
	if row.Reason != ReasonRollbackFailed {
		t.Errorf("reason = %q, want %q", row.Reason, ReasonRollbackFailed)
	}
}

func gitDeploy(t *testing.T, h *harness, zoneID, revision string) string {
	t.Helper()
	response, err := h.service.CreateDeploy(context.Background(), connect.NewRequest(&hostingv1.CreateDeployRequest{
		ZoneId:     zoneID,
		Repository: "https://github.com/owner/name",
		Revision:   revision,
	}))
	if err != nil {
		t.Fatalf("CreateDeploy %s: %v", revision, err)
	}
	return response.Msg.GetDeploy().GetId()
}
