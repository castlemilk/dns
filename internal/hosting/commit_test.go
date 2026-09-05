package hosting

import (
	"context"
	"testing"

	"github.com/castlemilk/dns/internal/config"
	"github.com/castlemilk/dns/internal/platform"
)

func TestALiveGitDeployReportsTheCommitTheEngineBuilt(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	zoneID := h.zoneID(t, "acme.dev")
	h.seedGateway(t, "198.51.100.10")
	attach(t, h, zoneID)

	deploy := createDeploy(t, h, zoneID, "https://github.com/owner/name", "main")
	// Before the engine has a release there is no commit, and the view falls
	// back to the requested revision rather than showing a placeholder.
	if got := deployProto(h.deploy(t, zoneID, deploy.GetId())).GetRevisionResolved(); got != "main" {
		t.Errorf("revision_resolved = %q before the build finished, want the requested revision", got)
	}

	final := h.settle(t, zoneID, deploy.GetId(), 10)
	if final.Phase != platform.DeployPhaseLive {
		t.Fatalf("phase = %s, want LIVE", final.Phase)
	}
	if !isCommitID(final.RevisionResolved) {
		t.Fatalf("stored revision_resolved = %q, want a commit id", final.RevisionResolved)
	}
	if len(final.RevisionResolved) != 40 {
		t.Errorf("commit is %d characters, want the engine's full id", len(final.RevisionResolved))
	}
	view := deployProto(final)
	if view.GetRevisionResolved() != final.RevisionResolved {
		t.Errorf("revision_resolved = %q, want the stored commit", view.GetRevisionResolved())
	}
	if view.GetSource().GetRevision() != "main" {
		t.Errorf("source.revision = %q, want the revision as requested", view.GetSource().GetRevision())
	}
	if !h.service.capabilities(context.Background()).ResolvedCommit {
		t.Error("capability resolved_commit stayed false after the engine reported a commit")
	}
}

func TestAnEngineThatReportsNoCommitLeavesTheRequestedRevisionAlone(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	zoneID := h.zoneID(t, "acme.dev")
	h.seedGateway(t, "198.51.100.10")
	attach(t, h, zoneID)
	h.engine.SetOldServer(true)

	deploy := createDeploy(t, h, zoneID, "https://github.com/owner/name", "main")
	final := h.settle(t, zoneID, deploy.GetId(), 12)

	if final.RevisionResolved != "" {
		t.Errorf("stored revision_resolved = %q, want none from a server that reports none", final.RevisionResolved)
	}
	view := deployProto(final)
	if view.GetRevisionResolved() != "main" {
		t.Errorf("revision_resolved = %q, want the requested revision alone", view.GetRevisionResolved())
	}
	if h.service.capabilities(context.Background()).ResolvedCommit {
		t.Error("capability resolved_commit turned true with no commit reported")
	}
}

func TestAnUploadDeployReportsNoCommitAtAll(t *testing.T) {
	t.Parallel()

	h := newHarness(t, func(cfg *config.Hosting) { cfg.UploadsEnabled = true })
	zoneID := h.zoneID(t, "acme.dev")
	h.seedGateway(t, "198.51.100.10")
	attach(t, h, zoneID)

	uploadID := uploadFolder(t, h, zoneID)
	deploy := createUploadDeploy(t, h, zoneID, uploadID)
	h.service.driveUploads(context.Background())
	final := h.settle(t, zoneID, deploy.GetId(), 12)

	if final.RevisionResolved != "" {
		t.Errorf("an upload deploy resolved to %q; an artifact has no commit", final.RevisionResolved)
	}
	if got := deployProto(final).GetRevisionResolved(); got != "" {
		t.Errorf("revision_resolved = %q, want empty for an upload", got)
	}
}

func TestIsCommitIDRejectsAnythingThatIsNotACommit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value string
		want  bool
	}{
		{name: "full sha", value: "4f2c1a9b3d5e7f0a1b2c3d4e5f60718293a4b5c6", want: true},
		{name: "short sha", value: "4f2c1a9", want: true},
		{name: "empty", value: "", want: false},
		{name: "branch name", value: "main", want: false},
		{name: "tag", value: "v1.2.3", want: false},
		{name: "too short", value: "4f2c1a", want: false},
		{name: "uppercase", value: "4F2C1A9B3D5E7F0A1B2C3D4E5F60718293A4B5C6", want: false},
		{name: "not hex", value: "4f2c1a9zz", want: false},
		{name: "too long", value: "0123456789012345678901234567890123456789012345678901234567890123456789", want: false},
	}
	for _, test := range tests {
		if got := isCommitID(test.value); got != test.want {
			t.Errorf("isCommitID(%q) = %t, want %t", test.value, got, test.want)
		}
	}
}

func TestAdoptResolvedCommitNeverClearsACommitItAlreadyHas(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	ctx := context.Background()
	sha := "4f2c1a9b3d5e7f0a1b2c3d4e5f60718293a4b5c6"
	doc := platform.DeployDoc{}

	h.service.adoptResolvedCommit(ctx, &doc, sha)
	if doc.RevisionResolved != sha {
		t.Fatalf("revision_resolved = %q, want the commit", doc.RevisionResolved)
	}
	// A later poll of the same release against a server that stopped reporting
	// one must not erase what was already observed.
	h.service.adoptResolvedCommit(ctx, &doc, "")
	h.service.adoptResolvedCommit(ctx, &doc, "main")
	if doc.RevisionResolved != sha {
		t.Errorf("revision_resolved = %q after a poll with no commit", doc.RevisionResolved)
	}
}
