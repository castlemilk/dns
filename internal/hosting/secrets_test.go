package hosting

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"connectrpc.com/connect"
	hostingv1 "github.com/castlemilk/dns/gen/go/hosting/v1"
	"github.com/castlemilk/dns/internal/platform"
)

// deployToken is a token-shaped string. It is deliberately not a real GitHub
// token, and the redaction rules must not be what keeps it out of the store:
// the facade never writes it anywhere in the first place.
const deployToken = "ghp_notarealtoken0123456789abcdef"

func TestPerDeployTokenNeverReachesLogsEventsOrTheStore(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	zoneID := h.zoneID(t, "acme.dev")
	h.seedGateway(t, "198.51.100.10")
	attach(t, h, zoneID)

	response, err := h.service.CreateDeploy(context.Background(), connect.NewRequest(&hostingv1.CreateDeployRequest{
		ZoneId:     zoneID,
		Repository: "https://github.com/owner/private",
		Revision:   "main",
		GitToken:   deployToken,
	}))
	if err != nil {
		t.Fatalf("CreateDeploy: %v", err)
	}
	h.settle(t, zoneID, response.Msg.GetDeploy().GetId(), 8)

	// The response says a token was used, without carrying it.
	if !response.Msg.GetDeploy().GetSource().GetPrivateRepository() {
		t.Error("private_repository = false for a deploy that supplied a token")
	}
	rendered := response.Msg.GetDeploy().String()
	if strings.Contains(rendered, deployToken) {
		t.Error("the response carried the token")
	}

	if strings.Contains(h.logs.String(), deployToken) {
		t.Error("the token appears in the control-plane log")
	}
	for _, event := range h.events.all() {
		if strings.Contains(event.Summary, deployToken) {
			t.Errorf("the token appears in event summary %q", event.Summary)
		}
		for key, value := range event.Details {
			if strings.Contains(value, deployToken) {
				t.Errorf("the token appears in event detail %q", key)
			}
		}
	}

	// The engine recorded that a token was supplied, not the token.
	for _, call := range h.engine.Calls() {
		for key, value := range call.Args {
			if strings.Contains(value, deployToken) {
				t.Errorf("the fake engine recorded the token in %q", key)
			}
		}
		if call.Method == "CreateGitBuild" && call.Args["git_token"] != "true" {
			t.Errorf("CreateGitBuild args = %v, want the boolean fact", call.Args)
		}
	}

	// And the bytes of the platform store carry no trace of it.
	if err := h.store.Close(); err != nil {
		t.Fatalf("close platform store: %v", err)
	}
	raw, err := os.ReadFile(h.store.Path())
	if err != nil {
		t.Fatalf("read platform store: %v", err)
	}
	if bytes.Contains(raw, []byte(deployToken)) {
		t.Error("the token is present in platform.db")
	}
}

func TestARepositoryWithUserinfoIsRejectedBeforeAnythingIsPersisted(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	zoneID := h.zoneID(t, "acme.dev")
	h.seedGateway(t, "198.51.100.10")
	attach(t, h, zoneID)
	before := len(h.events.all())

	_, err := h.service.CreateDeploy(context.Background(), connect.NewRequest(&hostingv1.CreateDeployRequest{
		ZoneId:     zoneID,
		Repository: "https://x-access-token:" + deployToken + "@github.com/owner/private",
		Revision:   "main",
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("code = %s, want invalid_argument", connect.CodeOf(err))
	}
	if strings.Contains(err.Error(), deployToken) {
		t.Errorf("the error echoed the embedded credential: %v", err)
	}

	deploys, _, listErr := h.store.ListDeploys(context.Background(), zoneID, 10, "")
	if listErr != nil {
		t.Fatalf("list deploys: %v", listErr)
	}
	if len(deploys) != 0 {
		t.Errorf("a rejected repository left %d deploy rows behind", len(deploys))
	}
	if len(h.events.all()) != before {
		t.Errorf("a rejected repository recorded %d events", len(h.events.all())-before)
	}
	for _, call := range h.engine.Calls() {
		if call.Method == "CreateGitBuild" {
			t.Error("a rejected repository reached the engine")
		}
	}
}

func TestOnlyTheCanonicalRepositoryIsStoredAndRecorded(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	zoneID := h.zoneID(t, "acme.dev")
	h.seedGateway(t, "198.51.100.10")
	attach(t, h, zoneID)

	response, err := h.service.CreateDeploy(context.Background(), connect.NewRequest(&hostingv1.CreateDeployRequest{
		ZoneId:     zoneID,
		Repository: "https://github.com/Owner/Name.git",
		Revision:   "main",
	}))
	if err != nil {
		t.Fatalf("CreateDeploy: %v", err)
	}
	deploy := h.deploy(t, zoneID, response.Msg.GetDeploy().GetId())
	if deploy.Source.Repository != "https://github.com/Owner/Name" {
		t.Errorf("stored repository = %q, want the canonical form", deploy.Source.Repository)
	}
	if h.site(t, zoneID).Repository != "https://github.com/Owner/Name" {
		t.Errorf("site repository = %q", h.site(t, zoneID).Repository)
	}
	for _, event := range h.events.all() {
		if repository, ok := event.Details["repository"]; ok && strings.HasSuffix(repository, ".git") {
			t.Errorf("event recorded %q, want the canonical form", repository)
		}
	}
}

func TestRebuildReconstructsSitesFromTheEngine(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	zoneID := h.zoneID(t, "acme.dev")
	h.seedGateway(t, "198.51.100.10")
	attach(t, h, zoneID)
	h.service.watchDomains(context.Background())

	// The shape of a lost platform.db: the engine still has the app, the zone
	// still exists, the row does not.
	if err := h.store.DeleteSite(context.Background(), zoneID); err != nil {
		t.Fatalf("delete site: %v", err)
	}

	dry, err := h.service.Rebuild(context.Background(), true)
	if err != nil {
		t.Fatalf("Rebuild dry run: %v", err)
	}
	if dry.Count != 1 {
		t.Fatalf("dry-run count = %d, want 1", dry.Count)
	}
	if _, err := h.store.GetSite(context.Background(), zoneID); !errors.Is(err, platform.ErrNotFound) {
		t.Fatal("a dry run wrote a site row")
	}

	report, err := h.service.Rebuild(context.Background(), false)
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if report.Count != 1 {
		t.Fatalf("count = %d, want 1", report.Count)
	}
	site := h.site(t, zoneID)
	if site.App != AppName("simple-test", "acme.dev") {
		t.Errorf("app = %q, want the deterministic name", site.App)
	}
	if !site.WWW {
		t.Error("www = false, want it recovered from the engine's host list")
	}
	if len(report.Warnings) == 0 {
		t.Error("the report claims a complete recovery; deploy history is not recoverable")
	}
}
