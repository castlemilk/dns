package platform

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/castlemilk/dns/internal/config"
	"go.etcd.io/bbolt"
)

const (
	// JanitorInterval is the sweep cadence after the first pass at start.
	JanitorInterval = 15 * time.Minute
	// WebhookRetention is how long a replay guard outlives its delivery.
	WebhookRetention = 30 * 24 * time.Hour
	// DeploysPerSite bounds the deploy history one site keeps.
	DeploysPerSite = 200
	// DetachedDeployRetention is how long a detached zone's deploy history and
	// build logs survive. TrimDeploys is driven from the site list, so once the
	// site row is gone nothing reaches those rows: they are kept long enough to
	// stay readable across a detach and re-attach, then removed.
	DetachedDeployRetention = 30 * 24 * time.Hour
	// CheckoutRetention is how long a checkout row survives. It has to outlive
	// any session ConfirmCheckout might still be asked about — that lookup is
	// what stops the confirm route being used to read the Stripe account's
	// other sessions — so it matches WebhookRetention rather than the much
	// shorter checkout window.
	CheckoutRetention = 30 * 24 * time.Hour
	// janitorTimeout bounds one sweep.
	janitorTimeout = 20 * time.Second
)

// TrimDeploys keeps the newest `keep` deploys for a zone and deletes the rest
// with their logs and index entries. It never deletes an active row: a build
// still being polled must keep its row even if the history is long.
func (s *Store) TrimDeploys(ctx context.Context, zoneID string, keep int) (int, error) {
	if keep <= 0 {
		keep = DeploysPerSite
	}
	removed := 0
	err := s.update(ctx, func(tx *bbolt.Tx) error {
		deploys := tx.Bucket(deploysBucket)
		byTime := tx.Bucket(deploysByTimeBucket)
		byBuild := tx.Bucket(deploysByBuildBucket)
		logs := tx.Bucket(deployLogsBucket)

		prefix := []byte(zoneID + "/")
		type row struct {
			key []byte
			doc DeployDoc
		}
		var rows []row
		cursor := deploys.Cursor()
		for key, value := cursor.Seek(prefix); key != nil && strings.HasPrefix(string(key), string(prefix)); key, value = cursor.Next() {
			var doc DeployDoc
			if err := json.Unmarshal(value, &doc); err != nil {
				return err
			}
			rows = append(rows, row{key: append([]byte(nil), key...), doc: doc})
		}
		// rows are ascending by id, so the oldest come first.
		excess := len(rows) - keep
		for index := 0; index < len(rows) && excess > 0; index++ {
			item := rows[index]
			if item.doc.Active() || item.doc.Live {
				continue
			}
			if err := deleteDeployRow(deploys, byTime, byBuild, logs, item.key, item.doc); err != nil {
				return err
			}
			removed++
			excess--
		}
		return nil
	})
	return removed, err
}

// TrimOrphanedDeploys removes the deploy rows of zones that no longer have a
// site, along with their index entries and gzipped build logs, once they are
// older than the cutoff.
//
// DeleteSite removes only the sites-bucket key, and the sweep's per-site
// TrimDeploys is driven from the site list, so without this a detach strands
// the whole history — up to DeploysPerSite rows and MaxDeployLogBytes of
// compressed log each — with nothing able to reach it again. The cutoff keeps a
// recent detach readable, in particular across a detach and re-attach of the
// same zone. An active row is never removed, whatever its age: something is
// still polling it.
func (s *Store) TrimOrphanedDeploys(ctx context.Context, cutoff time.Time) (int, error) {
	removed := 0
	err := s.update(ctx, func(tx *bbolt.Tx) error {
		deploys := tx.Bucket(deploysBucket)
		byTime := tx.Bucket(deploysByTimeBucket)
		byBuild := tx.Bucket(deploysByBuildBucket)
		logs := tx.Bucket(deployLogsBucket)
		sites := tx.Bucket(sitesBucket)

		type row struct {
			key []byte
			doc DeployDoc
		}
		var expired []row
		attached := map[string]bool{}
		cursor := deploys.Cursor()
		for key, value := cursor.First(); key != nil; key, value = cursor.Next() {
			zoneID, _, found := strings.Cut(string(key), "/")
			if !found {
				continue
			}
			live, known := attached[zoneID]
			if !known {
				live = sites.Get([]byte(zoneID)) != nil
				attached[zoneID] = live
			}
			if live {
				continue
			}
			var doc DeployDoc
			if err := json.Unmarshal(value, &doc); err != nil {
				return err
			}
			if doc.Active() || !doc.RequestedAt.Before(cutoff) {
				continue
			}
			expired = append(expired, row{key: append([]byte(nil), key...), doc: doc})
		}
		for _, item := range expired {
			if err := deleteDeployRow(deploys, byTime, byBuild, logs, item.key, item.doc); err != nil {
				return err
			}
			removed++
		}
		return nil
	})
	return removed, err
}

// deleteDeployRow removes one deploy with everything that points at it: the row
// itself, both index entries and the captured build log.
func deleteDeployRow(deploys, byTime, byBuild, logs *bbolt.Bucket, key []byte, doc DeployDoc) error {
	if err := deploys.Delete(key); err != nil {
		return err
	}
	if err := byTime.Delete([]byte(doc.ID)); err != nil {
		return err
	}
	if doc.BuildID != "" {
		if err := byBuild.Delete([]byte(doc.BuildID + "/" + doc.ID)); err != nil {
			return err
		}
	}
	return logs.Delete([]byte(doc.ID))
}

// JanitorReport is what one sweep did. It is returned for tests and logged at
// debug so a growing store is diagnosable without turning on tracing.
type JanitorReport struct {
	EventsTrimmed   int
	WebhooksPruned  int
	DeploysTrimmed  int
	OrphansTrimmed  int
	CheckoutsPruned int
	UploadsRemoved  int
	DirsRemoved     int
}

// Janitor enforces every bound the store promises: the activity ring, the
// webhook replay-guard retention, the checkout retention, the per-site deploy
// history, the deploy history a detached zone leaves behind, and the upload
// scratch directory.
type Janitor struct {
	store     *Store
	deps      Deps
	maxEvents int
	uploadDir string
	logger    *slog.Logger
}

func NewJanitor(store *Store, cfg config.Config, deps Deps) *Janitor {
	maxEvents := cfg.Activity.MaxEvents
	if maxEvents <= 0 {
		maxEvents = config.DefaultActivityMaxEvents
	}
	uploadDir := ""
	if cfg.Hosting.Configured() && cfg.Hosting.UploadsEnabled {
		uploadDir = cfg.Hosting.UploadDir
	}
	return &Janitor{store: store, deps: deps, maxEvents: maxEvents, uploadDir: uploadDir, logger: deps.Log()}
}

// Run sweeps once at start, then every JanitorInterval until ctx is done.
func (j *Janitor) Run(ctx context.Context) error {
	j.sweepBounded(ctx)
	for {
		timer := time.NewTimer(jitter(JanitorInterval))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
			j.sweepBounded(ctx)
		}
	}
}

func (j *Janitor) sweepBounded(ctx context.Context) {
	sweepCtx, cancel := context.WithTimeout(ctx, janitorTimeout)
	defer cancel()
	report, err := j.Sweep(sweepCtx)
	if err != nil && !errors.Is(err, context.Canceled) {
		j.logger.Warn("platform janitor sweep", "error", err)
		return
	}
	j.logger.Debug("platform janitor sweep",
		"events_trimmed", report.EventsTrimmed,
		"webhooks_pruned", report.WebhooksPruned,
		"deploys_trimmed", report.DeploysTrimmed,
		"orphans_trimmed", report.OrphansTrimmed,
		"checkouts_pruned", report.CheckoutsPruned,
		"uploads_removed", report.UploadsRemoved,
		"dirs_removed", report.DirsRemoved,
	)
}

// Sweep runs one pass. Each step reports its own error but does not stop the
// others: a full upload volume must not stop the activity ring from being
// trimmed.
func (j *Janitor) Sweep(ctx context.Context) (JanitorReport, error) {
	if j.store == nil {
		return JanitorReport{}, nil
	}
	var report JanitorReport
	var problems []error

	trimmed, err := j.store.PruneEvents(ctx, j.maxEvents)
	if err != nil {
		problems = append(problems, err)
	}
	report.EventsTrimmed = trimmed

	now := j.deps.Now()
	pruned, err := j.store.DeleteWebhookEventsBefore(ctx, now.Add(-WebhookRetention))
	if err != nil {
		problems = append(problems, err)
	}
	report.WebhooksPruned = pruned

	dropped, err := j.store.DeleteCheckoutsBefore(ctx, now.Add(-CheckoutRetention))
	if err != nil {
		problems = append(problems, err)
	}
	report.CheckoutsPruned = dropped

	sites, err := j.store.ListSites(ctx)
	if err != nil {
		problems = append(problems, err)
	}
	for _, site := range sites {
		count, err := j.store.TrimDeploys(ctx, site.ZoneID, DeploysPerSite)
		if err != nil {
			problems = append(problems, err)
			continue
		}
		report.DeploysTrimmed += count
	}
	// TrimDeploys is driven from the site list, so a detached zone's rows,
	// index entries and gzipped build logs are unreachable by it — up to 200
	// rows and tens of megabytes of logs per detach, on the same file
	// MaxBackupBytes bounds. Sweep them here once they are old enough that a
	// re-attach is no longer reading them.
	orphaned, err := j.store.TrimOrphanedDeploys(ctx, now.Add(-DetachedDeployRetention))
	if err != nil {
		problems = append(problems, err)
	}
	report.OrphansTrimmed = orphaned

	expired, err := j.store.ListExpiredUploads(ctx, now)
	if err != nil {
		problems = append(problems, err)
	}
	for _, upload := range expired {
		if upload.Dir != "" {
			if err := os.RemoveAll(upload.Dir); err != nil {
				problems = append(problems, err)
			}
		}
		if err := j.store.DeleteUpload(ctx, upload.ID); err != nil {
			problems = append(problems, err)
			continue
		}
		report.UploadsRemoved++
	}
	if dirs, err := j.sweepStrayUploads(ctx); err != nil {
		problems = append(problems, err)
	} else {
		report.DirsRemoved = dirs
	}
	return report, errors.Join(problems...)
}

// UploadArchiveSuffix is the extension the facade gives the archive it packs
// from a staged upload before handing it to the engine. The janitor has to know
// it to sweep an archive left behind by a crash or a shutdown mid-deploy.
const UploadArchiveSuffix = ".tar.zst"

// sweepStrayUploads removes what no upload row references under
// HOSTING_UPLOAD_DIR: the staging directory of an upload whose row was never
// committed, and the `<upload-id>.tar.zst` archive of a deploy that died
// between packing and the engine accepting it. Either would otherwise leak
// bytes onto the volume forever — and the archive is not counted by the
// in-flight upload budget either, so a handful of interrupted deploys fills a
// small emptyDir and every later upload answers 507.
//
// It only ever considers an entry whose name is an upload id, or an upload id
// with the archive suffix (platform.UploadIDPattern, the shape the facade
// issues and PutUpload enforces). HOSTING_UPLOAD_DIR may point at a volume root
// or a directory an operator shares with something else, and a janitor that
// deleted every unrecognised entry there would destroy data it does not own.
func (j *Janitor) sweepStrayUploads(ctx context.Context) (int, error) {
	if j.uploadDir == "" {
		return 0, nil
	}
	entries, err := os.ReadDir(j.uploadDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	removed := 0
	for _, entry := range entries {
		uploadID := entry.Name()
		if !entry.IsDir() {
			trimmed, isArchive := strings.CutSuffix(uploadID, UploadArchiveSuffix)
			if !isArchive {
				continue
			}
			uploadID = trimmed
		}
		if !ValidUploadID(uploadID) {
			continue
		}
		if _, err := j.store.GetUpload(ctx, uploadID); err == nil {
			continue
		} else if !errors.Is(err, ErrNotFound) {
			return removed, err
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		// Only sweep entries old enough that no in-flight request can own them:
		// an upload writes its parts before the row is committed, and a deploy
		// packs its archive before the engine has taken it.
		if j.deps.Now().Sub(info.ModTime()) < time.Hour {
			continue
		}
		if err := os.RemoveAll(filepath.Join(j.uploadDir, entry.Name())); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}
