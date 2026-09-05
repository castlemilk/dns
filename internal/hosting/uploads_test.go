package hosting

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"
	hostingv1 "github.com/castlemilk/dns/gen/go/hosting/v1"
	"github.com/castlemilk/dns/internal/config"
	"github.com/castlemilk/dns/internal/platform"
	"github.com/klauspost/compress/zstd"
)

type uploadFile struct {
	path string
	body string
}

func uploadBody(t *testing.T, zoneID string, files []uploadFile) (io.Reader, string) {
	t.Helper()
	buffer := &bytes.Buffer{}
	writer := multipart.NewWriter(buffer)
	if err := writer.WriteField("zone_id", zoneID); err != nil {
		t.Fatalf("write zone_id: %v", err)
	}
	for _, file := range files {
		part, err := writer.CreateFormFile("files", file.path)
		if err != nil {
			t.Fatalf("create part %q: %v", file.path, err)
		}
		if _, err := part.Write([]byte(file.body)); err != nil {
			t.Fatalf("write part %q: %v", file.path, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	return buffer, writer.FormDataContentType()
}

func postUpload(t *testing.T, h *harness, zoneID string, files []uploadFile) *httptest.ResponseRecorder {
	t.Helper()
	body, contentType := uploadBody(t, zoneID, files)
	request := httptest.NewRequest(http.MethodPost, "/hosting/v1/uploads", body)
	request.Header.Set("Content-Type", contentType)
	recorder := httptest.NewRecorder()
	h.service.UploadHandler().ServeHTTP(recorder, request)
	return recorder
}

func uploadsEnabled(cfg *config.Hosting) { cfg.UploadsEnabled = true }

func TestUploadStoresTheFolderAndSkipsSensitiveFiles(t *testing.T) {
	t.Parallel()

	h := newHarness(t, uploadsEnabled)
	zoneID := h.zoneID(t, "acme.dev")

	recorder := postUpload(t, h, zoneID, []uploadFile{
		{path: "site/index.html", body: "<h1>hi</h1>"},
		{path: "site/assets/app.css", body: "body{}"},
		{path: "site/.env", body: "SECRET=do-not-upload"},
		{path: "site/.git/config", body: "[remote]"},
		{path: "site/tls.key", body: "private"},
		{path: "site/.npmrc", body: "//registry/:_authToken=secret"},
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", recorder.Code, recorder.Body.String())
	}
	var response uploadResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.Files != 2 {
		t.Errorf("files = %d, want the two publishable files", response.Files)
	}
	if response.Name != "site" {
		t.Errorf("name = %q, want the top-level folder", response.Name)
	}

	doc, err := h.store.GetUpload(context.Background(), response.UploadID)
	if err != nil {
		t.Fatalf("get upload: %v", err)
	}
	for _, skipped := range []string{".env", ".git/config", "tls.key", ".npmrc"} {
		if _, err := os.Stat(filepath.Join(doc.Dir, "site", skipped)); !os.IsNotExist(err) {
			t.Errorf("%s was stored", skipped)
		}
	}
	if _, err := os.Stat(filepath.Join(doc.Dir, "site", "index.html")); err != nil {
		t.Errorf("index.html was not stored: %v", err)
	}
}

// TestUploadLeavesOutAManifestThatWouldShadowTheBuildManifest covers the file
// the packer cannot carry. The engine scans the archive for manifest.json by
// base name at any depth and takes the first entry it finds, while the manifest
// the deploy injects is written last — so a PWA's own manifest would decide the
// framework the engine builds with. It is dropped, and the response says so
// rather than letting a folder look as though it was deployed whole.
func TestUploadLeavesOutAManifestThatWouldShadowTheBuildManifest(t *testing.T) {
	t.Parallel()

	h := newHarness(t, uploadsEnabled)
	zoneID := h.zoneID(t, "acme.dev")

	recorder := postUpload(t, h, zoneID, []uploadFile{
		{path: "site/index.html", body: "<h1>hi</h1>"},
		{path: "site/assets/manifest.json", body: `{"name":"pwa"}`},
		{path: "site/manifest.json", body: `{"framework":"docusaurus"}`},
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", recorder.Code, recorder.Body.String())
	}
	var response uploadResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.Files != 1 {
		t.Errorf("files = %d, want only index.html staged", response.Files)
	}
	if !strings.Contains(response.Note, "manifest.json") ||
		!strings.Contains(response.Note, "assets/manifest.json") {
		t.Errorf("note = %q, want it to name the files left out", response.Note)
	}

	doc, err := h.store.GetUpload(context.Background(), response.UploadID)
	if err != nil {
		t.Fatalf("get upload: %v", err)
	}
	for _, dropped := range []string{"manifest.json", "assets/manifest.json"} {
		if _, err := os.Stat(filepath.Join(doc.Dir, "site", dropped)); !os.IsNotExist(err) {
			t.Errorf("%s was staged", dropped)
		}
	}
}

func TestUploadRejectsPathsThatEscapeTheFolder(t *testing.T) {
	t.Parallel()

	h := newHarness(t, uploadsEnabled)
	zoneID := h.zoneID(t, "acme.dev")

	for _, path := range []string{"../evil", "/etc/passwd", "site/../../evil", `site\evil`} {
		recorder := postUpload(t, h, zoneID, []uploadFile{{path: path, body: "x"}})
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("path %q: status = %d, want 400", path, recorder.Code)
		}
	}
}

func TestUploadRefusesAFolderLargerThanTheCap(t *testing.T) {
	t.Parallel()

	h := newHarness(t, func(cfg *config.Hosting) {
		uploadsEnabled(cfg)
		cfg.UploadMaxBytes = 1 << 20
		cfg.UploadMaxTotalBytes = 2 << 20
	})
	zoneID := h.zoneID(t, "acme.dev")

	recorder := postUpload(t, h, zoneID, []uploadFile{
		{path: "site/big.bin", body: strings.Repeat("x", (1<<20)+1)},
	})
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "at most") {
		t.Errorf("body = %s", recorder.Body.String())
	}
}

func TestUploadRefusesWhenTheInFlightCapIsFull(t *testing.T) {
	t.Parallel()

	h := newHarness(t, func(cfg *config.Hosting) {
		uploadsEnabled(cfg)
		cfg.UploadMaxBytes = 1 << 20
		cfg.UploadMaxTotalBytes = 1 << 20
	})
	zoneID := h.zoneID(t, "acme.dev")

	first := postUpload(t, h, zoneID, []uploadFile{{path: "site/index.html", body: "hi"}})
	if first.Code != http.StatusOK {
		t.Fatalf("first upload status = %d", first.Code)
	}
	second := postUpload(t, h, zoneID, []uploadFile{{path: "site/index.html", body: "hi"}})
	if second.Code != http.StatusInsufficientStorage {
		t.Fatalf("second upload status = %d, want 507", second.Code)
	}
	if !strings.Contains(second.Body.String(), "upload storage is full") {
		t.Errorf("body = %s", second.Body.String())
	}
}

func TestUploadHandlerIsAbsentWhenUploadsAreDisabled(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	if _, handler := New(h.service.cfg, h.service.deps); handler != nil {
		t.Fatal("New returned an upload handler with uploads disabled")
	}
}

func TestUploadDeployPacksAndStreamsTheFolder(t *testing.T) {
	t.Parallel()

	h := newHarness(t, uploadsEnabled)
	zoneID := h.zoneID(t, "acme.dev")
	h.seedGateway(t, "198.51.100.10")
	attach(t, h, zoneID)

	uploadID := uploadFolder(t, h, zoneID)
	deploy := createUploadDeploy(t, h, zoneID, uploadID)
	if deploy.GetPhase() != hostingv1.DeployPhase_DEPLOY_PHASE_QUEUED {
		t.Fatalf("phase = %v, want the RPC to return immediately", deploy.GetPhase())
	}
	if deploy.GetSource().GetUploadName() != "site" || deploy.GetSource().GetUploadFiles() != 1 {
		t.Errorf("source = %+v", deploy.GetSource())
	}

	h.service.driveUploads(context.Background())
	row := h.deploy(t, zoneID, deploy.GetId())
	if row.BuildID == "" {
		t.Fatalf("the upload deployer did not open the stream: %+v", row)
	}
	if _, err := h.store.GetUpload(context.Background(), uploadID); err == nil {
		t.Error("the upload row survived the stream")
	}

	final := h.settle(t, zoneID, deploy.GetId(), 8)
	if final.Phase != platform.DeployPhaseLive {
		t.Fatalf("phase = %q, want LIVE", final.Phase)
	}
}

func TestIdenticalUploadReusesTheEnginesExistingBuildInsteadOfWaitingForANewOne(t *testing.T) {
	t.Parallel()

	h := newHarness(t, uploadsEnabled)
	zoneID := h.zoneID(t, "acme.dev")
	h.seedGateway(t, "198.51.100.10")
	attach(t, h, zoneID)

	first := createUploadDeploy(t, h, zoneID, uploadFolder(t, h, zoneID))
	h.service.driveUploads(context.Background())
	if phase := h.settle(t, zoneID, first.GetId(), 8).Phase; phase != platform.DeployPhaseLive {
		t.Fatalf("first upload phase = %q", phase)
	}

	second := createUploadDeploy(t, h, zoneID, uploadFolder(t, h, zoneID))
	h.service.driveUploads(context.Background())
	row := h.deploy(t, zoneID, second.GetId())
	if row.BuildID != h.deploy(t, zoneID, first.GetId()).BuildID {
		t.Errorf("build id = %q, want the engine's content-addressed build", row.BuildID)
	}
	// The facade never names a release the engine has not reported: an id only
	// ever reaches the row from GetBuild or ListReleases.
	if row.ReleaseID != "" {
		t.Errorf("release id = %q, want none until the engine reports one", row.ReleaseID)
	}
	final := h.settle(t, zoneID, second.GetId(), 6)
	if final.Phase != platform.DeployPhaseLive {
		t.Fatalf("phase = %q, want LIVE", final.Phase)
	}
}

// TestReUploadingASupersededFolderIsPromotedBackOntoItsRelease covers the
// branch the removed guess used to short-circuit: the engine already has the
// build, its release is ready but carries no traffic, and the poller turns the
// row into a rollback onto the release the engine itself names.
func TestReUploadingASupersededFolderIsPromotedBackOntoItsRelease(t *testing.T) {
	t.Parallel()

	h := newHarness(t, uploadsEnabled)
	zoneID := h.zoneID(t, "acme.dev")
	h.seedGateway(t, "198.51.100.10")
	attach(t, h, zoneID)

	first := createUploadDeploy(t, h, zoneID, uploadFolderBody(t, h, zoneID, "<h1>one</h1>"))
	h.service.driveUploads(context.Background())
	if phase := h.settle(t, zoneID, first.GetId(), 8).Phase; phase != platform.DeployPhaseLive {
		t.Fatalf("first upload phase = %q", phase)
	}
	second := createUploadDeploy(t, h, zoneID, uploadFolderBody(t, h, zoneID, "<h1>two</h1>"))
	h.service.driveUploads(context.Background())
	if phase := h.settle(t, zoneID, second.GetId(), 8).Phase; phase != platform.DeployPhaseLive {
		t.Fatalf("second upload phase = %q", phase)
	}

	third := createUploadDeploy(t, h, zoneID, uploadFolderBody(t, h, zoneID, "<h1>one</h1>"))
	h.service.driveUploads(context.Background())
	final := h.settle(t, zoneID, third.GetId(), 8)
	if final.Phase != platform.DeployPhaseLive {
		t.Fatalf("phase = %q / %q, want LIVE", final.Phase, final.Reason)
	}
	if final.Kind != DeployKindRollback {
		t.Errorf("kind = %q, want the already-built folder to become a rollback", final.Kind)
	}
	if final.BuildID != h.deploy(t, zoneID, first.GetId()).BuildID {
		t.Errorf("build id = %q, want the first upload's build", final.BuildID)
	}
}

// TestReUploadingAFolderWhoseBuildFailedReportsThatBuildsOwnReason is the
// regression for the guessed release id. DeepHost only creates rel-<build> when
// a build succeeds, so promoting that name after a failed build answered
// NotFound and told the operator to detach a healthy site.
func TestReUploadingAFolderWhoseBuildFailedReportsThatBuildsOwnReason(t *testing.T) {
	t.Parallel()

	h := newHarness(t, uploadsEnabled)
	zoneID := h.zoneID(t, "acme.dev")
	h.seedGateway(t, "198.51.100.10")
	attach(t, h, zoneID)

	h.engine.FailNextBuild("build script exited 1")
	first := createUploadDeploy(t, h, zoneID, uploadFolder(t, h, zoneID))
	h.service.driveUploads(context.Background())
	if phase := h.settle(t, zoneID, first.GetId(), 8).Phase; phase != platform.DeployPhaseFailed {
		t.Fatalf("first upload phase = %q, want FAILED", phase)
	}

	// The same folder again: the engine answers with the same
	// content-addressed build, which has no release at all.
	second := createUploadDeploy(t, h, zoneID, uploadFolder(t, h, zoneID))
	h.service.driveUploads(context.Background())
	final := h.settle(t, zoneID, second.GetId(), 8)
	if final.Phase != platform.DeployPhaseFailed {
		t.Fatalf("phase = %q / %q, want FAILED", final.Phase, final.Reason)
	}
	if !strings.Contains(final.Reason, "build script exited 1") {
		t.Errorf("reason = %q, want the build's own reason", final.Reason)
	}
	if strings.Contains(final.Reason, CopyAppGone) {
		t.Errorf("reason = %q, want no advice to detach a healthy site", final.Reason)
	}
	if final.ReleaseID != "" {
		t.Errorf("release id = %q, want none for a build that has no release", final.ReleaseID)
	}
}

func TestPackedUploadCarriesTheManifestDeepHostRequires(t *testing.T) {
	t.Parallel()

	h := newHarness(t, uploadsEnabled)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "site"), 0o755); err != nil {
		t.Fatalf("create folder: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "site", "index.html"), []byte("hi"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	archive := filepath.Join(t.TempDir(), "out.tar.zst")
	if err := h.service.packUpload(context.Background(), dir, archive, string(FrameworkStatic), "simple-test-acme-dev"); err != nil {
		t.Fatalf("packUpload: %v", err)
	}

	names := tarNames(t, archive)
	if !contains(names, "manifest.json") {
		t.Fatalf("archive entries = %v, want manifest.json", names)
	}
	if !contains(names, "site/index.html") {
		t.Errorf("archive entries = %v, want the uploaded file", names)
	}
}

func tarNames(t *testing.T, path string) []string {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open archive: %v", err)
	}
	defer func() { ignore(file.Close()) }()
	decoder, err := zstd.NewReader(file)
	if err != nil {
		t.Fatalf("zstd reader: %v", err)
	}
	defer decoder.Close()

	names := []string{}
	reader := tar.NewReader(decoder)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read archive: %v", err)
		}
		names = append(names, header.Name)
	}
	return names
}

func uploadFolder(t *testing.T, h *harness, zoneID string) string {
	t.Helper()
	return uploadFolderBody(t, h, zoneID, "<h1>hi</h1>")
}

// uploadFolderBody stages a one-file folder whose bytes the caller chooses, so
// a test can make two uploads the engine's content addressing tells apart.
func uploadFolderBody(t *testing.T, h *harness, zoneID, body string) string {
	t.Helper()
	recorder := postUpload(t, h, zoneID, []uploadFile{{path: "site/index.html", body: body}})
	if recorder.Code != http.StatusOK {
		t.Fatalf("upload status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var response uploadResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode upload response: %v", err)
	}
	return response.UploadID
}

func createUploadDeploy(t *testing.T, h *harness, zoneID, uploadID string) *hostingv1.Deploy {
	t.Helper()
	response, err := h.service.CreateDeploy(context.Background(), connect.NewRequest(&hostingv1.CreateDeployRequest{
		ZoneId: zoneID, UploadId: uploadID,
	}))
	if err != nil {
		t.Fatalf("CreateDeploy: %v", err)
	}
	return response.Msg.GetDeploy()
}

// TestAPackedArchiveCountsAgainstTheUploadBudget covers the other half of the
// upload accounting: the deployer packs each upload into a <upload-id>.tar.zst
// beside it in the same directory, and that archive is roughly a second copy of
// the folder. Counting only the upload rows let the volume reach twice
// HOSTING_UPLOAD_MAX_TOTAL_BYTES while admission still said there was room.
func TestAPackedArchiveCountsAgainstTheUploadBudget(t *testing.T) {
	t.Parallel()

	h := newHarness(t, func(cfg *config.Hosting) {
		uploadsEnabled(cfg)
		cfg.UploadMaxBytes = 1 << 20
		cfg.UploadMaxTotalBytes = 3 << 20
	})
	zoneID := h.zoneID(t, "acme.dev")

	// Nothing staged: there is room.
	first := postUpload(t, h, zoneID, []uploadFile{{path: "site/index.html", body: "hi"}})
	if first.Code != http.StatusOK {
		t.Fatalf("first upload status = %d, want 200", first.Code)
	}

	// A packed archive on disk is real bytes in the same directory. With it the
	// budget is spent, whatever the upload rows say.
	archive := filepath.Join(h.service.cfg.UploadDir,
		h.store.NewUploadID(h.clock.Now())+platform.UploadArchiveSuffix)
	if err := os.WriteFile(archive, make([]byte, 2<<20), 0o600); err != nil {
		t.Fatalf("write packed archive: %v", err)
	}

	second := postUpload(t, h, zoneID, []uploadFile{{path: "site/index.html", body: "hi"}})
	if second.Code != http.StatusInsufficientStorage {
		t.Fatalf("second upload status = %d, want 507 with the archive on disk", second.Code)
	}

	// Removing it — as the deployer's defer and the janitor both do — gives the
	// space straight back.
	if err := os.Remove(archive); err != nil {
		t.Fatalf("remove packed archive: %v", err)
	}
	third := postUpload(t, h, zoneID, []uploadFile{{path: "site/index.html", body: "hi"}})
	if third.Code != http.StatusOK {
		t.Fatalf("third upload status = %d, want 200 once the archive is gone", third.Code)
	}
}
