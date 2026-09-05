package hosting

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	hostingv1 "github.com/castlemilk/dns/gen/go/hosting/v1"
	"github.com/castlemilk/dns/internal/giturl"
	"github.com/castlemilk/dns/internal/platform"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Deploy kinds, stored in DeployDoc.Kind.
const (
	DeployKindGit      = "git"
	DeployKindUpload   = "upload"
	DeployKindRollback = "rollback"
)

// Reasons the facade writes itself. They are copy, not engine text.
const (
	ReasonRecovered      = "build id recovered from the engine"
	ReasonNeverConfirmed = "the hosting engine never confirmed this build"
	ReasonUploadExpired  = "the upload expired before the hosting engine confirmed the build"
	ReasonRollbackFailed = "the hosting engine did not confirm the rollback"
	ReasonNotFound       = "the hosting engine no longer reports this build"
	ReasonSiteDetached   = "the site was detached while this deploy was running"
)

// maxRevisionLength and maxGitTokenBytes bound the two free-form deploy inputs
// before anything is persisted.
const (
	maxRevisionLength = 255
	maxGitTokenBytes  = 512
)

// gitSource is a validated GitHub deploy request. Only the canonical URL is
// ever stored; the token stays in the request scope.
type gitSource struct {
	Repository string
	Revision   string
	Path       string
	Token      string
}

// validateGitSource runs every check spec2 §4.2 requires, in order, before the
// caller persists anything. It returns the canonical repository URL — without
// `.git`, without userinfo, without a query — which is the only form that
// reaches a document, an event or a log line.
func validateGitSource(repository, revision, path, token string) (gitSource, error) {
	repository = strings.TrimSpace(repository)
	if repository == "" {
		return gitSource{}, invalidArgument("repository", "is required")
	}
	if strings.HasPrefix(repository, "git@") || strings.HasPrefix(repository, "ssh://") {
		return gitSource{}, invalidArgument("repository", "use the https://github.com/owner/name URL")
	}

	parsed, err := url.Parse(repository)
	if err != nil {
		return gitSource{}, invalidArgument("repository", "must be a https://github.com/owner/name URL")
	}
	if parsed.Scheme != "https" || !strings.EqualFold(parsed.Hostname(), "github.com") ||
		parsed.Port() != "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return gitSource{}, invalidArgument("repository", "must be a https://github.com/owner/name URL")
	}
	owner, name, ok := splitRepoPath(parsed.Path)
	if !ok {
		return gitSource{}, invalidArgument("repository", "must be a https://github.com/owner/name URL")
	}
	// giturl.Validate is DeepHost's own check, copied verbatim: running it here
	// means the facade refuses exactly what the engine would refuse (and, as it
	// happens, rejects userinfo a second time).
	canonical := "https://github.com/" + owner + "/" + name
	if err := giturl.Validate(canonical, false); err != nil {
		return gitSource{}, invalidArgument("repository", "must be a https://github.com/owner/name URL")
	}

	revision = strings.TrimSpace(revision)
	if revision != "" {
		if len(revision) > maxRevisionLength {
			return gitSource{}, invalidArgument("revision", fmt.Sprintf("must be at most %d characters", maxRevisionLength))
		}
		if strings.Contains(revision, "..") || !validRevision(revision) {
			return gitSource{}, invalidArgument("revision", "may contain only letters, digits, '.', '_', '/' and '-'")
		}
	}

	path = strings.TrimSpace(path)
	if err := giturl.ValidateSubpath(path); err != nil {
		return gitSource{}, invalidArgument("path", "must be a relative path inside the repository")
	}

	if len(token) > maxGitTokenBytes {
		return gitSource{}, invalidArgument("git_token", fmt.Sprintf("must be at most %d bytes", maxGitTokenBytes))
	}
	for _, char := range token {
		if char < 0x21 || char > 0x7e {
			return gitSource{}, invalidArgument("git_token", "must be visible ASCII with no spaces")
		}
	}

	return gitSource{Repository: canonical, Revision: revision, Path: path, Token: token}, nil
}

func splitRepoPath(raw string) (owner, name string, ok bool) {
	trimmed := strings.Trim(raw, "/")
	trimmed = strings.TrimSuffix(trimmed, ".git")
	parts := strings.Split(trimmed, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	if !validRepoPart(parts[0]) || !validRepoPart(parts[1]) {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func validRepoPart(value string) bool {
	if value == "." || value == ".." {
		return false
	}
	for _, char := range value {
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z', char >= '0' && char <= '9':
		case char == '.' || char == '_' || char == '-':
		default:
			return false
		}
	}
	return true
}

func validRevision(value string) bool {
	for _, char := range value {
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z', char >= '0' && char <= '9':
		case char == '.' || char == '_' || char == '/' || char == '-':
		default:
			return false
		}
	}
	return true
}

// SanitizeUploadName strips control and bidirectional characters from a
// browser-supplied folder name and bounds it, so a display-only string cannot
// carry an override that reorders the rest of a line.
func SanitizeUploadName(value string) string {
	cleaned := strings.Map(func(char rune) rune {
		switch {
		case char < 0x20, char == 0x7f:
			return -1
		case char >= 0x200b && char <= 0x200f, char >= 0x202a && char <= 0x202e,
			char >= 0x2066 && char <= 0x2069, char == 0xfeff:
			return -1
		default:
			return char
		}
	}, value)
	cleaned = strings.TrimSpace(cleaned)
	runes := []rune(cleaned)
	if len(runes) > 80 {
		return strings.TrimSpace(string(runes[:80]))
	}
	return cleaned
}

func deployKindProto(kind string) hostingv1.DeployKind {
	switch kind {
	case DeployKindGit:
		return hostingv1.DeployKind_DEPLOY_KIND_GIT
	case DeployKindUpload:
		return hostingv1.DeployKind_DEPLOY_KIND_UPLOAD
	case DeployKindRollback:
		return hostingv1.DeployKind_DEPLOY_KIND_ROLLBACK
	default:
		return hostingv1.DeployKind_DEPLOY_KIND_UNSPECIFIED
	}
}

func deployPhaseProto(phase string) hostingv1.DeployPhase {
	switch phase {
	case platform.DeployPhaseQueued:
		return hostingv1.DeployPhase_DEPLOY_PHASE_QUEUED
	case platform.DeployPhaseBuilding:
		return hostingv1.DeployPhase_DEPLOY_PHASE_BUILDING
	case platform.DeployPhaseReleasing:
		return hostingv1.DeployPhase_DEPLOY_PHASE_RELEASING
	case platform.DeployPhaseLive:
		return hostingv1.DeployPhase_DEPLOY_PHASE_LIVE
	case platform.DeployPhaseSuperseded:
		return hostingv1.DeployPhase_DEPLOY_PHASE_SUPERSEDED
	case platform.DeployPhaseFailed:
		return hostingv1.DeployPhase_DEPLOY_PHASE_FAILED
	case platform.DeployPhaseAbandoned:
		return hostingv1.DeployPhase_DEPLOY_PHASE_ABANDONED
	case platform.DeployPhaseLost:
		return hostingv1.DeployPhase_DEPLOY_PHASE_LOST
	default:
		return hostingv1.DeployPhase_DEPLOY_PHASE_UNSPECIFIED
	}
}

// deployTimes resolves the started/finished pair and says where it came from.
// TIME_SOURCE_ENGINE is reported only when BOTH ends came from the engine —
// a mixed pair would render a duration the engine never measured.
func deployTimes(doc platform.DeployDoc) (started, finished time.Time, source hostingv1.TimeSource) {
	fromEngine := true
	switch {
	case doc.EngineStartedAt != nil && !doc.EngineStartedAt.IsZero():
		started = doc.EngineStartedAt.UTC()
	case doc.ObservedBuildingAt != nil:
		started = doc.ObservedBuildingAt.UTC()
		fromEngine = false
	default:
		fromEngine = false
	}
	switch {
	case doc.EngineFinishedAt != nil && !doc.EngineFinishedAt.IsZero():
		finished = doc.EngineFinishedAt.UTC()
	case doc.ObservedTerminalAt != nil:
		finished = doc.ObservedTerminalAt.UTC()
		fromEngine = false
	default:
		fromEngine = false
	}
	if started.IsZero() && finished.IsZero() {
		return time.Time{}, time.Time{}, hostingv1.TimeSource_TIME_SOURCE_UNSPECIFIED
	}
	if fromEngine {
		return started, finished, hostingv1.TimeSource_TIME_SOURCE_ENGINE
	}
	return started, finished, hostingv1.TimeSource_TIME_SOURCE_OBSERVED
}

func deployProto(doc platform.DeployDoc) *hostingv1.Deploy {
	started, finished, source := deployTimes(doc)
	deploy := &hostingv1.Deploy{
		Id:             doc.ID,
		ZoneId:         doc.ZoneID,
		ZoneName:       doc.ZoneName,
		Kind:           deployKindProto(doc.Kind),
		Phase:          deployPhaseProto(doc.Phase),
		Reason:         doc.Reason,
		BuildId:        doc.BuildID,
		ReleaseId:      doc.ReleaseID,
		Framework:      FrameworkProto(doc.Framework),
		TimeSource:     source,
		Live:           doc.Live,
		LogSnapshot:    doc.LogSnapshot,
		RolledBackFrom: doc.RolledBackFrom,
		Source:         deploySourceProto(doc),

		RevisionResolved: resolvedRevision(doc),
	}
	if !doc.RequestedAt.IsZero() {
		deploy.RequestedAt = timestamppb.New(doc.RequestedAt.UTC())
	}
	if !started.IsZero() {
		deploy.StartedAt = timestamppb.New(started)
	}
	if !finished.IsZero() {
		deploy.FinishedAt = timestamppb.New(finished)
	}
	if doc.ObservedLiveAt != nil {
		deploy.LiveAt = timestamppb.New(doc.ObservedLiveAt.UTC())
	}
	if !started.IsZero() && !finished.IsZero() && finished.After(started) {
		deploy.BuildSeconds = uint32(finished.Sub(started).Round(time.Second) / time.Second)
	}
	return deploy
}

// resolvedRevision is what the Deploy view reports as the revision that was
// actually built: the engine's commit when it reported one, and the requested
// revision unchanged when it did not. It never invents a value, so a caller
// that finds it equal to source.revision knows the engine reported no commit.
func resolvedRevision(doc platform.DeployDoc) string {
	if doc.RevisionResolved != "" {
		return doc.RevisionResolved
	}
	return doc.Source.Revision
}

// deploySourceProto renders the stored source. A git deploy never reports an
// upload name and an upload deploy never reports a private repository, so the
// two halves of DeploySourceDoc cannot be confused in the UI. The stored
// PrivateRepository flag is a boolean fact about the request, never the token.
func deploySourceProto(doc platform.DeployDoc) *hostingv1.DeploySource {
	source := &hostingv1.DeploySource{
		Repository: doc.Source.Repository,
		Revision:   doc.Source.Revision,
		Path:       doc.Source.Path,
	}
	if doc.Kind == DeployKindUpload {
		source.UploadName = doc.Source.UploadName
		source.UploadFiles = uint32(max(doc.Source.UploadFiles, 0))
		source.UploadBytes = uint64(max(doc.Source.UploadBytes, 0))
		return source
	}
	source.PrivateRepository = doc.Source.PrivateRepository
	return source
}
