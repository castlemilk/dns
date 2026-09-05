package hosting

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/castlemilk/dns/internal/activity"
	"github.com/castlemilk/dns/internal/artifacts"
	"github.com/castlemilk/dns/internal/platform"
)

// uploadSweepInterval is how often the deployer looks for upload rows whose
// stream never started — after a restart, say.
const uploadSweepInterval = 10 * time.Second

// runUploadDeployer packs accepted folder uploads and streams them to the
// engine. It runs outside the request path because the stream can take minutes.
func (s *Service) runUploadDeployer(ctx context.Context) {
	for {
		timer := time.NewTimer(jitter(uploadSweepInterval))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.uploadWake:
			timer.Stop()
		case <-timer.C:
		}
		s.driveUploads(ctx)
	}
}

// pokeUploads asks the deployer to look now.
func (s *Service) pokeUploads() {
	select {
	case s.uploadWake <- struct{}{}:
	default:
	}
}

// driveUploads streams every upload deploy that has no build id and no call in
// flight. A row whose call was in flight when the process died is re-driven if
// its directory still exists: the engine dedupes identical bytes, so a second
// stream of the same folder is not a second build.
func (s *Service) driveUploads(ctx context.Context) {
	if !s.configured() {
		return
	}
	active, err := s.deps.Store.ActiveDeploys(ctx)
	if err != nil {
		s.log().Warn("list active deploys for uploads", "error", err)
		return
	}
	now := s.now()
	for _, deploy := range active {
		if ctx.Err() != nil {
			return
		}
		if deploy.Kind != DeployKindUpload || deploy.BuildID != "" || deploy.UploadID == "" {
			continue
		}
		if deploy.EngineCallStartedAt != nil && now.Sub(*deploy.EngineCallStartedAt) < engineCallWindow {
			continue
		}
		if err := s.deployUpload(ctx, deploy); err != nil {
			s.log().Warn("stream folder upload to the hosting engine", "error", err)
		}
	}
}

// deployUpload packs one upload and drives the client stream.
func (s *Service) deployUpload(ctx context.Context, doc platform.DeployDoc) error {
	site, err := s.deps.Store.GetSite(ctx, doc.ZoneID)
	if err != nil {
		return err
	}
	upload, err := s.deps.Store.GetUpload(ctx, doc.UploadID)
	if err != nil {
		return s.finishDeploy(ctx, doc, platform.DeployPhaseFailed, ReasonUploadExpired,
			activity.KindDeployFailed, activity.SeverityError)
	}
	if _, statErr := os.Stat(upload.Dir); statErr != nil {
		return s.finishDeploy(ctx, doc, platform.DeployPhaseFailed, ReasonUploadExpired,
			activity.KindDeployFailed, activity.SeverityError)
	}

	started := s.now()
	doc.EngineCallStartedAt = &started
	if err := s.deps.Store.PutDeploy(ctx, doc); err != nil {
		return err
	}

	archivePath := filepath.Join(s.cfg.UploadDir, upload.ID+".tar.zst")
	if err := s.packUpload(ctx, upload.Dir, archivePath, doc.Framework, site.AppRef); err != nil {
		ignore(os.Remove(archivePath))
		return s.finishDeploy(ctx, doc, platform.DeployPhaseFailed,
			"the uploaded folder could not be packaged", activity.KindDeployFailed, activity.SeverityError)
	}
	defer func() { ignore(os.Remove(archivePath)) }()

	archive, err := os.Open(archivePath)
	if err != nil {
		return s.finishDeploy(ctx, doc, platform.DeployPhaseFailed,
			"the uploaded folder could not be read back", activity.KindDeployFailed, activity.SeverityError)
	}
	buildID, streamErr := s.engine.Deploy(ctx, s.cfg.Tenant, site.App, Framework(doc.Framework), archive)
	closeErr := archive.Close()
	if closeErr != nil {
		s.log().Warn("close packed upload", "error", closeErr)
	}
	if streamErr != nil {
		return s.finishDeploy(ctx, doc, platform.DeployPhaseFailed,
			reason(s.mapEngineError(streamErr).Error()), activity.KindDeployFailed, activity.SeverityError)
	}

	// An identical folder yields the engine's existing, content-addressed
	// build, so no new build will ever run for this row — but the release id
	// is the engine's to report, never this facade's to guess: a build that
	// failed has no release at all, and promoting a name that was never
	// created answers "the hosting engine no longer knows this site" and tells
	// the operator to detach a healthy one. The poller resolves it from the
	// build the engine actually has: a failed build ends the row with that
	// build's own reason, and a succeeded one whose release carries no traffic
	// is turned into a rollback onto the release the engine names.
	doc.BuildID = buildID
	doc.EngineCallStartedAt = nil
	if err := s.deps.Store.PutDeploy(ctx, doc); err != nil {
		return err
	}

	s.finishUpload(ctx, upload)
	s.pokePoller()
	return nil
}

// packUpload writes the tar.zst DeepHost's Deploy stream expects. It is written
// under HOSTING_UPLOAD_DIR, never beside the DNS database.
func (s *Service) packUpload(ctx context.Context, dir, archivePath, framework, appRef string) error {
	file, err := os.OpenFile(archivePath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, uploadFileMode)
	if err != nil {
		return fmt.Errorf("create upload archive: %w", err)
	}
	// buildRef is left empty on purpose: the engine's builder refuses any value
	// that is not the build's own name (or the literal "cli"), and it stamps the
	// real build name into the manifest itself.
	manifest := &artifacts.Manifest{Framework: framework, AppRef: appRef}
	packErr := artifacts.Pack(ctx, dir, manifest, file)
	closeErr := file.Close()
	if packErr != nil {
		return packErr
	}
	return closeErr
}

// finishUpload removes the staged directory and its row once the engine has the
// bytes; the row's disappearance is what frees headroom under the in-flight cap.
func (s *Service) finishUpload(ctx context.Context, upload platform.UploadDoc) {
	if err := os.RemoveAll(upload.Dir); err != nil {
		s.log().Warn("remove staged upload", "error", err)
	}
	if err := s.deps.Store.DeleteUpload(ctx, upload.ID); err != nil {
		s.log().Warn("delete upload row", "error", err)
	}
}
