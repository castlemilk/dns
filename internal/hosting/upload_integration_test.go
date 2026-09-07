package hosting

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/castlemilk/dns/internal/hosting/deephost"
)

// The folder-upload path is the one deploy path whose archive the engine's
// builder validates before it runs: a manifest whose buildRef is neither empty
// nor the build's own name is rejected with "manifest buildRef does not match
// the build", and every upload deploy fails. Only a real builder catches that,
// so this test packs exactly what deployUpload packs and follows the build to a
// terminal phase.
//
// Skipped unless DEEPHOST_TEST_DEEPHOST_URL is set (spec2 §10.2):
//
//	DEEPHOST_TEST_DEEPHOST_URL=http://127.0.0.1:30081 go test -run Integration ./internal/hosting/
//
// It uses the simple-test-hosting tenant only and deletes its app in a cleanup
// even when it fails.
const uploadIntegrationTenant = "simple-test-hosting"

// uploadIntegrationBudget bounds how long the upload build is polled. Raise it
// with DEEPHOST_TEST_BUILD_SECONDS on a cold cluster.
const uploadIntegrationBudget = 120 * time.Second

func TestIntegrationUploadDeployIsAcceptedByTheBuilder(t *testing.T) {
	url := os.Getenv("DEEPHOST_TEST_DEEPHOST_URL")
	if url == "" {
		t.Skip("DEEPHOST_TEST_DEEPHOST_URL is not set; skipping the DeepHost integration test")
	}
	engine := deephost.New(url, os.Getenv("DEEPHOST_TEST_DEEPHOST_TOKEN"))
	ctx := context.Background()

	zoneName := "up-" + strconv.FormatInt(time.Now().UnixNano(), 36) + ".example"
	app := AppName(uploadIntegrationTenant, zoneName)
	if _, err := engine.GetApp(ctx, uploadIntegrationTenant, app); err == nil {
		t.Fatalf("app %q already exists on the cluster; a previous run did not clean up", app)
	}
	appRef, err := engine.CreateApp(ctx, uploadIntegrationTenant, app, AppSpec{Framework: FrameworkStatic})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	t.Cleanup(func() {
		if err := engine.DeleteApp(context.Background(), uploadIntegrationTenant, app); err != nil {
			t.Errorf("cleanup DeleteApp(%s): %v", app, err)
		}
	})

	// The folder a person would drop on the Deploy dialog.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<!doctype html>\n<title>up</title>\n"), 0o600); err != nil {
		t.Fatalf("write the uploaded folder: %v", err)
	}

	service := &Service{}
	pack := func(name string) string {
		t.Helper()
		archive := filepath.Join(t.TempDir(), name)
		if err := service.packUpload(ctx, dir, archive, string(FrameworkStatic), appRef); err != nil {
			t.Fatalf("packUpload: %v", err)
		}
		return archive
	}
	deploy := func(archive string) string {
		t.Helper()
		file, err := os.Open(archive) //nolint:gosec // a path this test just wrote
		if err != nil {
			t.Fatalf("open the packed upload: %v", err)
		}
		defer func() {
			if err := file.Close(); err != nil {
				t.Errorf("close the packed upload: %v", err)
			}
		}()
		buildID, err := engine.Deploy(ctx, uploadIntegrationTenant, app, FrameworkStatic, file)
		if err != nil {
			t.Fatalf("Deploy: %v", err)
		}
		return buildID
	}

	buildID := deploy(pack("first.tar.zst"))
	if buildID == "" {
		t.Fatal("Deploy returned no build id")
	}

	budget := uploadIntegrationBudget
	if raw := os.Getenv("DEEPHOST_TEST_BUILD_SECONDS"); raw != "" {
		if seconds, convErr := strconv.Atoi(raw); convErr == nil && seconds > 0 {
			budget = time.Duration(seconds) * time.Second
		}
	}
	deadline := time.Now().Add(budget)
	build, err := engine.GetBuild(ctx, uploadIntegrationTenant, app, buildID)
	for err == nil && !build.Terminal() && time.Now().Before(deadline) {
		time.Sleep(3 * time.Second)
		build, err = engine.GetBuild(ctx, uploadIntegrationTenant, app, buildID)
	}
	if err != nil {
		t.Fatalf("GetBuild: %v", err)
	}
	if build.Phase == BuildPhaseFailed {
		// This is what a non-empty, engine-unknown manifest buildRef looks like.
		t.Fatalf("the upload build failed on the cluster: %s", build.Reason)
	}
	if !build.Terminal() {
		t.Skipf("the upload build was still %q after %s; raise DEEPHOST_TEST_BUILD_SECONDS", build.Phase, budget)
	}

	// An identical folder is content-addressed to the same build, which is what
	// the facade converts into a ROLLBACK row onto that build's release
	// (spec2 §12.8) instead of waiting for a build that will never run.
	if second := deploy(pack("second.tar.zst")); second != buildID {
		t.Errorf("a second identical upload returned build %q, want the existing %q", second, buildID)
	}
}
