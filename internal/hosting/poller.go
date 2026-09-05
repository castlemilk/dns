package hosting

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/castlemilk/dns/internal/activity"
	"github.com/castlemilk/dns/internal/platform"
)

// Build poller timings.
const (
	// pollInterval matches the cadence DeepHost's own console uses.
	pollInterval = 3 * time.Second
	// pollIdleInterval is how often the poller looks for work when there is
	// none.
	pollIdleInterval = 15 * time.Second
	// logSnapshotInterval is how often a running build's log is re-captured,
	// so the sheet has something to show if the engine drops the pod.
	logSnapshotInterval = 15 * time.Second
	// logSnapshotTail is how many lines a snapshot keeps.
	logSnapshotTail = 1000
	// confirmWindow is how long a deploy may sit with no build id before the
	// facade tries to adopt one and then gives up.
	confirmWindow = 2 * time.Minute
	// engineCallWindow is how long an in-flight engine call is trusted: the
	// Deploy stream's own 5-minute bound plus slack.
	engineCallWindow = 6 * time.Minute
	// notFoundPollLimit is how many consecutive NotFound polls make a build
	// LOST rather than a blip.
	notFoundPollLimit = 5
	// rollbackConfirmWindow is how long RollbackSite's release flip is waited
	// for before the row is marked failed.
	rollbackConfirmWindow = 60 * time.Second
)

// runPoller drives every active deploy. It polls at 3 s while anything is
// active and idles otherwise, so a control plane with no deploys makes no
// engine calls at all.
func (s *Service) runPoller(ctx context.Context) {
	for {
		interval := pollIdleInterval
		if s.hasActiveDeploys(ctx) {
			interval = pollInterval
		}
		timer := time.NewTimer(jitter(interval))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.pollWake:
			timer.Stop()
		case <-timer.C:
		}
		passCtx, cancel := context.WithTimeout(ctx, workerBudget)
		s.pollOnce(passCtx)
		cancel()
	}
}

// pokePoller asks for a poll now.
func (s *Service) pokePoller() {
	select {
	case s.pollWake <- struct{}{}:
	default:
	}
}

func (s *Service) hasActiveDeploys(ctx context.Context) bool {
	active, err := s.deps.Store.ActiveDeploys(ctx)
	if err != nil {
		return false
	}
	return len(active) > 0
}

// pollOnce advances every active deploy by whatever the engine now reports.
func (s *Service) pollOnce(ctx context.Context) {
	if !s.configured() {
		return
	}
	active, err := s.deps.Store.ActiveDeploys(ctx)
	if err != nil {
		s.log().Warn("list active deploys", "error", err)
		return
	}
	for _, deploy := range active {
		if ctx.Err() != nil {
			return
		}
		if err := s.pollDeploy(ctx, deploy); err != nil {
			s.log().Warn("poll deploy", "error", err)
		}
	}
}

func (s *Service) pollDeploy(ctx context.Context, doc platform.DeployDoc) error {
	site, err := s.deps.Store.GetSite(ctx, doc.ZoneID)
	if err != nil {
		// The site is gone; the deploy history stays, but there is nothing to
		// poll it against. Ending the row here is what stops it staying active
		// for ever — an active row is never trimmed and blocks every later
		// deploy of the same zone.
		if errors.Is(err, platform.ErrNotFound) {
			return s.finishDeploy(ctx, doc, platform.DeployPhaseAbandoned, ReasonSiteDetached,
				activity.KindDeployAbandoned, activity.SeverityWarn)
		}
		return nil
	}
	doc.LastPolledAt = s.now()

	if doc.BuildID == "" {
		return s.pollUnconfirmed(ctx, doc, site)
	}

	build, err := s.engine.GetBuild(ctx, s.cfg.Tenant, site.App, doc.BuildID)
	if err != nil {
		if isNotFound(err) {
			doc.NotFoundPolls++
			if doc.NotFoundPolls >= notFoundPollLimit {
				return s.finishDeploy(ctx, doc, platform.DeployPhaseLost, ReasonNotFound,
					activity.KindDeployLost, activity.SeverityWarn)
			}
			return s.deps.Store.PutDeploy(ctx, doc)
		}
		return err
	}
	doc.NotFoundPolls = 0
	applyEngineTimes(&doc, build)

	switch build.Phase {
	case BuildPhaseFailed:
		s.snapshotLog(ctx, &doc, site)
		if doc.ObservedTerminalAt == nil {
			now := s.now()
			doc.ObservedTerminalAt = &now
		}
		return s.finishDeploy(ctx, doc, platform.DeployPhaseFailed, reason(build.Reason),
			activity.KindDeployFailed, activity.SeverityError)

	case BuildPhaseSucceeded:
		if doc.ReleaseID == "" {
			doc.ReleaseID = build.ReleaseID
		}
		if doc.Phase != platform.DeployPhaseReleasing {
			if doc.ObservedTerminalAt == nil {
				now := s.now()
				doc.ObservedTerminalAt = &now
			}
			doc.Phase = platform.DeployPhaseReleasing
			s.snapshotLog(ctx, &doc, site)
			if err := s.deps.Store.PutDeploy(ctx, doc); err != nil {
				return err
			}
		}
		return s.checkRelease(ctx, doc, site)

	case BuildPhaseBuilding:
		if doc.Phase != platform.DeployPhaseBuilding {
			now := s.now()
			if doc.ObservedBuildingAt == nil {
				doc.ObservedBuildingAt = &now
			}
			doc.Phase = platform.DeployPhaseBuilding
			s.deps.Record(ctx, activity.Event{
				ZoneID: doc.ZoneID, ZoneName: doc.ZoneName, Actor: activity.ActorHostingEngine,
				Kind: activity.KindDeployBuilding, Severity: activity.SeverityInfo,
				Summary: fmt.Sprintf("Building %s", doc.ZoneName),
			})
		}
		s.snapshotLogThrottled(ctx, &doc, site)
	}

	if s.pastDeadline(doc) {
		return s.finishDeploy(ctx, doc, platform.DeployPhaseAbandoned,
			fmt.Sprintf("no terminal phase reported within %s", s.cfg.BuildDeadline),
			activity.KindDeployAbandoned, activity.SeverityWarn)
	}
	return s.deps.Store.PutDeploy(ctx, doc)
}

// pollUnconfirmed handles a row whose engine call never returned a build id: a
// crash between CreateGitBuild and persisting the id, or an upload the deployer
// has not picked up yet.
func (s *Service) pollUnconfirmed(ctx context.Context, doc platform.DeployDoc, site platform.SiteDoc) error {
	now := s.now()
	if doc.EngineCallStartedAt != nil && now.Sub(*doc.EngineCallStartedAt) < engineCallWindow {
		return s.deps.Store.PutDeploy(ctx, doc)
	}
	base := doc.RequestedAt
	if doc.Kind == DeployKindUpload {
		if doc.EngineCallStartedAt == nil {
			// The upload deployer has not opened the stream yet; only the
			// build deadline applies.
			if s.pastDeadline(doc) {
				return s.finishDeploy(ctx, doc, platform.DeployPhaseAbandoned,
					fmt.Sprintf("no terminal phase reported within %s", s.cfg.BuildDeadline),
					activity.KindDeployAbandoned, activity.SeverityWarn)
			}
			return s.deps.Store.PutDeploy(ctx, doc)
		}
		base = *doc.EngineCallStartedAt
	}
	if now.Sub(base) < confirmWindow {
		return s.deps.Store.PutDeploy(ctx, doc)
	}

	adopted, ok, err := s.adoptBuild(ctx, doc, site)
	if err != nil {
		// The engine did not answer. A row is only ever failed on an answer, so
		// this retries on the next pass and gives up only once the build
		// deadline has passed — where the honest verdict is that nothing was
		// ever reported, not that the build failed.
		if s.pastDeadline(doc) {
			return s.finishDeploy(ctx, doc, platform.DeployPhaseAbandoned,
				fmt.Sprintf("no terminal phase reported within %s", s.cfg.BuildDeadline),
				activity.KindDeployAbandoned, activity.SeverityWarn)
		}
		return err
	}
	if ok {
		doc = adopted
		doc.Reason = ReasonRecovered
		doc.EngineCallStartedAt = nil
		if err := s.deps.Store.PutDeploy(ctx, doc); err != nil {
			return err
		}
		s.deps.Record(ctx, activity.Event{
			ZoneID: doc.ZoneID, ZoneName: doc.ZoneName, Actor: activity.ActorHostingEngine,
			Kind: activity.KindDeployRecovered, Severity: activity.SeverityInfo,
			Summary: fmt.Sprintf("Recovered the build id for the deploy of %s from the hosting engine", doc.ZoneName),
		})
		return nil
	}
	return s.finishDeploy(ctx, doc, platform.DeployPhaseFailed, ReasonNeverConfirmed,
		activity.KindDeployFailed, activity.SeverityError)
}

// adoptBuild looks for a build the engine started for this site that no deploy
// row references. A build DeepHost completed and auto-promoted while the facade
// lost its id is therefore never shown as FAILED while it is serving traffic.
func (s *Service) adoptBuild(
	ctx context.Context,
	doc platform.DeployDoc,
	site platform.SiteDoc,
) (platform.DeployDoc, bool, error) {
	caps := s.capabilities(ctx)
	cutoff := doc.RequestedAt.Add(-5 * time.Second)

	if caps.ListBuilds {
		builds, err := s.engine.ListBuilds(ctx, s.cfg.Tenant, site.App, 5)
		if err != nil && !isUnsupported(err) {
			// The engine could not be asked. That is not an answer, so it must
			// never end the row: the caller retries on the next pass.
			return doc, false, err
		}
		for _, build := range builds {
			if !build.CreatedAt.IsZero() && build.CreatedAt.Before(cutoff) {
				continue
			}
			claimed, err := s.buildClaimed(ctx, build.ID)
			if err != nil || claimed {
				continue
			}
			doc.BuildID = build.ID
			doc.ReleaseID = build.ReleaseID
			return doc, true, nil
		}
	}

	releases, err := s.engine.ListReleases(ctx, s.cfg.Tenant, site.App)
	if err != nil && !isUnsupported(err) {
		return doc, false, err
	}
	for _, release := range releases {
		if release.BuildID == "" {
			continue
		}
		// A release is only this deploy's if the engine dates it inside this
		// deploy's own window. An app name is a pure function of tenant and
		// zone, so a zone that was deleted and recreated lands on the same app
		// with its old releases still on it, and a release the engine cannot
		// date could have been created at any time — adopting either would
		// report an old release as this deploy and roll production back onto
		// it.
		if release.CreatedAt.IsZero() || release.CreatedAt.Before(cutoff) {
			continue
		}
		claimed, err := s.buildClaimed(ctx, release.BuildID)
		if err != nil || claimed {
			continue
		}
		doc.BuildID = release.BuildID
		doc.ReleaseID = release.ID
		return doc, true, nil
	}
	return doc, false, nil
}

func (s *Service) buildClaimed(ctx context.Context, buildID string) (bool, error) {
	deploys, err := s.deps.Store.DeploysByBuild(ctx, buildID)
	if err != nil {
		return false, err
	}
	return len(deploys) > 0, nil
}

// checkRelease decides when a deploy is LIVE. The sibling check matters: after
// a promotion the target and the previous release both carry weight 100 until
// the release controller zeroes the siblings, and the router tie-breaks by
// newest creation — reporting LIVE earlier would claim traffic the older
// release does not yet receive.
func (s *Service) checkRelease(ctx context.Context, doc platform.DeployDoc, site platform.SiteDoc) error {
	releases, err := s.engine.ListReleases(ctx, s.cfg.Tenant, site.App)
	if err != nil {
		return err
	}
	target := doc.ReleaseID
	if target == "" {
		for _, release := range releases {
			if release.BuildID == doc.BuildID {
				target = release.ID
				break
			}
		}
	}
	if target == "" {
		if s.pastDeadline(doc) {
			return s.finishDeploy(ctx, doc, platform.DeployPhaseAbandoned,
				fmt.Sprintf("no terminal phase reported within %s", s.cfg.BuildDeadline),
				activity.KindDeployAbandoned, activity.SeverityWarn)
		}
		return s.deps.Store.PutDeploy(ctx, doc)
	}
	doc.ReleaseID = target

	live := false
	ready := false
	weight := int32(0)
	siblingsZero := true
	for _, release := range releases {
		if release.ID == target {
			ready = release.Ready
			weight = release.TrafficWeight
			live = release.Ready && release.TrafficWeight == 100
			s.adoptResolvedCommit(ctx, &doc, release.GitSHA)
			continue
		}
		if release.TrafficWeight != 0 {
			siblingsZero = false
		}
	}

	// A build the engine had already completed (an identical upload, or a build
	// adopted after a crash) leaves a ready release that carries no traffic. It
	// will never be promoted on its own, so this deploy becomes a rollback onto
	// it rather than waiting for a build that will not run again.
	if ready && weight == 0 && doc.Kind != DeployKindRollback {
		doc.Kind = DeployKindRollback
		if err := s.deps.Store.PutDeploy(ctx, doc); err != nil {
			return err
		}
		if err := s.engine.PromoteRelease(ctx, s.cfg.Tenant, site.App, target); err != nil {
			return s.finishDeploy(ctx, doc, platform.DeployPhaseFailed,
				reason(s.mapEngineError(err).Error()), activity.KindDeployFailed, activity.SeverityError)
		}
		return nil
	}

	if !live || !siblingsZero {
		if s.rollbackTimedOut(doc) {
			return s.finishDeploy(ctx, doc, platform.DeployPhaseFailed, ReasonRollbackFailed,
				activity.KindDeployFailed, activity.SeverityError)
		}
		if s.pastDeadline(doc) {
			return s.finishDeploy(ctx, doc, platform.DeployPhaseAbandoned,
				fmt.Sprintf("no terminal phase reported within %s", s.cfg.BuildDeadline),
				activity.KindDeployAbandoned, activity.SeverityWarn)
		}
		return s.deps.Store.PutDeploy(ctx, doc)
	}

	now := s.now()
	if doc.ObservedLiveAt == nil {
		doc.ObservedLiveAt = &now
	}
	doc.Phase = platform.DeployPhaseLive
	doc.Live = true
	if err := s.deps.Store.PutDeploy(ctx, doc); err != nil {
		return err
	}
	if err := s.supersedePrevious(ctx, doc, site); err != nil {
		return err
	}

	site.LiveDeployID = doc.ID
	touch(&site, now)
	if err := s.deps.Store.PutSite(ctx, site); err != nil {
		return err
	}
	kind := activity.KindDeployLive
	summary := fmt.Sprintf("%s is live", doc.ZoneName)
	if doc.Kind == DeployKindRollback {
		kind = activity.KindDeployRollback
		summary = fmt.Sprintf("Rolled %s back to an earlier deploy", doc.ZoneName)
	}
	s.deps.Record(ctx, activity.Event{
		ZoneID: doc.ZoneID, ZoneName: doc.ZoneName, Actor: activity.ActorHostingEngine,
		Kind: kind, Severity: activity.SeverityInfo, Summary: summary,
	})
	return nil
}

// adoptResolvedCommit records the commit the engine says it built. It is
// deliberately one-way: the engine reports an empty git_sha for an upload
// deploy, for a build made before it could resolve one, and whenever it cannot
// read the build manifest, and none of those mean "the commit changed to
// nothing". A value that is not a commit id is ignored rather than shown,
// because the whole point of the field is that it is a commit.
func (s *Service) adoptResolvedCommit(ctx context.Context, doc *platform.DeployDoc, sha string) {
	if !isCommitID(sha) || doc.RevisionResolved == sha {
		return
	}
	doc.RevisionResolved = sha
	s.updateCapabilities(ctx, func(caps *platform.CapabilitiesDoc) { caps.ResolvedCommit = true })
}

// isCommitID reports whether value is a lowercase hex commit id. DeepHost
// reports a full 40-character id; a short one is accepted because a shorter
// abbreviation is still a commit and not a branch name, which is the confusion
// this guards against.
func isCommitID(value string) bool {
	if len(value) < 7 || len(value) > 64 {
		return false
	}
	for _, char := range value {
		switch {
		case char >= '0' && char <= '9', char >= 'a' && char <= 'f':
		default:
			return false
		}
	}
	return true
}

// supersedePrevious retires whatever was live before this deploy.
func (s *Service) supersedePrevious(ctx context.Context, live platform.DeployDoc, site platform.SiteDoc) error {
	deploys, err := s.liveDeploys(ctx, site, live.ID)
	if err != nil {
		return err
	}
	for _, deploy := range deploys {
		deploy.Live = false
		deploy.Phase = platform.DeployPhaseSuperseded
		if err := s.deps.Store.PutDeploy(ctx, deploy); err != nil {
			return err
		}
		s.deps.Record(ctx, activity.Event{
			ZoneID: deploy.ZoneID, ZoneName: deploy.ZoneName, Actor: activity.ActorHostingEngine,
			Kind: activity.KindDeploySuperseded, Severity: activity.SeverityInfo,
			Summary: fmt.Sprintf("An earlier deploy of %s was superseded", deploy.ZoneName),
		})
	}
	return nil
}

// finishDeploy writes a terminal phase and records it once.
func (s *Service) finishDeploy(
	ctx context.Context,
	doc platform.DeployDoc,
	phase, why string,
	kind activity.Kind,
	severity activity.Severity,
) error {
	doc.Phase = phase
	if why != "" {
		doc.Reason = why
	}
	doc.Live = false
	if err := s.deps.Store.PutDeploy(ctx, doc); err != nil {
		return err
	}
	s.deps.Record(ctx, activity.Event{
		ZoneID: doc.ZoneID, ZoneName: doc.ZoneName, Actor: activity.ActorHostingEngine,
		Kind: kind, Severity: severity,
		Summary: fmt.Sprintf("The deploy of %s ended: %s", doc.ZoneName, phaseSentence(phase)),
		Details: detailsWithReason(doc.Reason),
	})
	return nil
}

func detailsWithReason(why string) map[string]string {
	if why == "" {
		return nil
	}
	return map[string]string{"reason": why}
}

func phaseSentence(phase string) string {
	switch phase {
	case platform.DeployPhaseFailed:
		return "failed"
	case platform.DeployPhaseAbandoned:
		return "abandoned"
	case platform.DeployPhaseLost:
		return "lost"
	default:
		return phase
	}
}

func (s *Service) pastDeadline(doc platform.DeployDoc) bool {
	return !doc.PollDeadline.IsZero() && s.now().After(doc.PollDeadline)
}

// rollbackTimedOut bounds a rollback: promoting an existing release takes
// seconds, so a minute without the weight flip is a failure, not a build.
//
// The window runs from when the flip was asked for, which is the request for a
// row RollbackSite created and the build's observed success for a deploy that
// became a rollback onto an already-built release. It deliberately does not
// require an empty build id: every rollback target has been LIVE or SUPERSEDED,
// which only a build can make it, so that test could never be true and left the
// bound dead.
func (s *Service) rollbackTimedOut(doc platform.DeployDoc) bool {
	if doc.Kind != DeployKindRollback || doc.Phase != platform.DeployPhaseReleasing {
		return false
	}
	started := doc.RequestedAt
	if doc.ObservedTerminalAt != nil && doc.ObservedTerminalAt.After(started) {
		started = *doc.ObservedTerminalAt
	}
	return s.now().Sub(started) > rollbackConfirmWindow
}

// applyEngineTimes copies whatever timestamps the engine reported. A zero stays
// zero, so the facade can tell "not reported" from "reported as the epoch".
func applyEngineTimes(doc *platform.DeployDoc, build BuildView) {
	if !build.CreatedAt.IsZero() {
		created := build.CreatedAt.UTC()
		doc.EngineCreatedAt = &created
	}
	if !build.StartedAt.IsZero() {
		started := build.StartedAt.UTC()
		doc.EngineStartedAt = &started
	}
	if !build.FinishedAt.IsZero() {
		finished := build.FinishedAt.UTC()
		doc.EngineFinishedAt = &finished
	}
}

// snapshotLogThrottled captures a running build's log at most every 15 s.
func (s *Service) snapshotLogThrottled(ctx context.Context, doc *platform.DeployDoc, site platform.SiteDoc) {
	if doc.LogCapturedAt != nil && s.now().Sub(*doc.LogCapturedAt) < logSnapshotInterval {
		return
	}
	s.snapshotLog(ctx, doc, site)
}

// snapshotLog stores the build log the facade can still read. DeepHost keeps
// build logs for about an hour; after that only this snapshot exists.
func (s *Service) snapshotLog(ctx context.Context, doc *platform.DeployDoc, site platform.SiteDoc) {
	if doc.BuildID == "" {
		return
	}
	lines, complete, err := s.engine.GetBuildLogs(ctx, s.cfg.Tenant, site.App, doc.BuildID, logSnapshotTail)
	if err != nil || len(lines) == 0 {
		return
	}
	now := s.now()
	log := platform.DeployLog{Lines: lines, Complete: complete, CapturedAt: now}
	if err := s.deps.Store.PutDeployLog(ctx, doc.ID, log); err != nil {
		s.log().Warn("store deploy log snapshot", "error", err)
		return
	}
	doc.LogSnapshot = true
	doc.LogCapturedAt = &now
}
