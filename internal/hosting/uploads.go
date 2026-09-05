package hosting

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/castlemilk/dns/internal/artifacts"
	"github.com/castlemilk/dns/internal/platform"
)

// Upload limits and lifetimes.
const (
	// maxUploadFiles bounds one folder upload.
	maxUploadFiles = 5000
	// uploadTTL is how long an accepted upload waits for its deploy before the
	// janitor removes it.
	uploadTTL = time.Hour
	// uploadDeadline is how long the handler gives one upload's body. The
	// server's own ReadTimeout is 15 s for every other route; this handler
	// raises its own deadlines so a large folder on an ordinary link is not
	// cut off while the DNS API keeps its tight limits.
	uploadDeadline = 15 * time.Minute
	// uploadFileMode and uploadDirMode keep an upload readable only by the
	// control plane's own user.
	uploadDirMode  = 0o700
	uploadFileMode = 0o600
)

// uploadResponse is the JSON the browser gets back. It carries no path.
type uploadResponse struct {
	UploadID string `json:"upload_id"`
	Files    int    `json:"files"`
	Bytes    int64  `json:"bytes"`
	Name     string `json:"name"`
	// Note names what the upload left out, so a folder that was not deployed
	// whole never looks as though it was.
	Note string `json:"note,omitempty"`
}

// UploadHandler is the multipart folder-upload route. internal/app mounts it at
// POST /hosting/v1/uploads behind the operator bearer with its own body cap.
func (s *Service) UploadHandler() http.Handler {
	return http.HandlerFunc(s.handleUpload)
}

func (s *Service) handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeUploadError(w, http.StatusMethodNotAllowed, "use POST to upload a folder")
		return
	}
	if !s.configured() || !s.cfg.UploadsEnabled {
		s.deps.Meter().HostingUpload(r.Context(), "expired")
		writeUploadError(w, http.StatusServiceUnavailable, "folder uploads are not enabled on this control API")
		return
	}

	// The connection deadlines the DNS API uses are far too tight for a
	// 64 MiB body, so this route extends its own before reading a byte.
	controller := http.NewResponseController(w)
	deadline := time.Now().Add(uploadDeadline)
	if err := controller.SetReadDeadline(deadline); err != nil && !errors.Is(err, http.ErrNotSupported) {
		s.log().Warn("extend upload read deadline", "error", err)
	}
	if err := controller.SetWriteDeadline(deadline); err != nil && !errors.Is(err, http.ErrNotSupported) {
		s.log().Warn("extend upload write deadline", "error", err)
	}

	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") || params["boundary"] == "" {
		writeUploadError(w, http.StatusUnsupportedMediaType, "send the folder as multipart/form-data")
		return
	}

	if free, known := freeBytes(s.cfg.UploadDir); known && free < uint64(2*s.cfg.UploadMaxBytes) {
		s.deps.Meter().HostingUpload(r.Context(), "no_space")
		writeUploadError(w, http.StatusInsufficientStorage, "upload storage is full; try again later")
		return
	}
	if !s.admitUpload(r.Context()) {
		s.deps.Meter().HostingUpload(r.Context(), "no_space")
		writeUploadError(w, http.StatusInsufficientStorage, "upload storage is full; try again later")
		return
	}
	// The reservation covers only the request that is being read; once the row
	// exists, admitUpload counts its real size instead.
	defer s.releaseUploadBytes(s.cfg.UploadMaxBytes)

	id := s.deps.Store.NewUploadID(s.now())
	dir := filepath.Join(s.cfg.UploadDir, id)
	if err := os.MkdirAll(dir, uploadDirMode); err != nil {
		s.log().Error("create upload directory", "error", err)
		writeUploadError(w, http.StatusInternalServerError, "the upload could not be stored")
		return
	}
	cleanup := func() { ignore(os.RemoveAll(dir)) }

	reader := multipart.NewReader(r.Body, params["boundary"])
	result, status, message := s.readUploadParts(r.Context(), reader, dir)
	if status != 0 {
		cleanup()
		s.deps.Meter().HostingUpload(r.Context(), uploadOutcome(status))
		writeUploadError(w, status, message)
		return
	}
	if result.zoneID == "" {
		cleanup()
		writeUploadError(w, http.StatusBadRequest, "zone_id is required")
		return
	}
	if result.files == 0 {
		cleanup()
		writeUploadError(w, http.StatusBadRequest, "the folder contained no files that can be deployed")
		return
	}

	now := s.now()
	doc := platform.UploadDoc{
		V:         platform.DocVersion,
		ID:        id,
		ZoneID:    result.zoneID,
		Name:      result.name,
		Files:     result.files,
		Bytes:     result.bytes,
		SHA256:    result.digest,
		Dir:       dir,
		CreatedAt: now,
		ExpiresAt: now.Add(uploadTTL),
	}
	if err := s.deps.Store.PutUpload(r.Context(), doc); err != nil {
		cleanup()
		s.log().Error("record upload", "error", err)
		writeUploadError(w, http.StatusInternalServerError, "the upload could not be stored")
		return
	}
	s.deps.Meter().HostingUpload(r.Context(), "accepted")

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(uploadResponse{
		UploadID: id, Files: result.files, Bytes: result.bytes, Name: result.name,
		Note: droppedNote(result.dropped),
	}); err != nil {
		s.log().Warn("write upload response", "error", err)
	}
}

type uploadResult struct {
	zoneID string
	name   string
	files  int
	bytes  int64
	digest string
	// dropped names the files the upload refused to stage, in the order they
	// arrived. Only manifest.json ever lands here.
	dropped []string
}

// maxDroppedNamed bounds how many dropped paths the response names one by one.
const maxDroppedNamed = 5

// droppedNote is the sentence the response carries when an upload left files
// out. It names the reason and the files, because a folder that was silently
// changed on the way to the engine is a folder whose deploy nobody can explain.
func droppedNote(dropped []string) string {
	if len(dropped) == 0 {
		return ""
	}
	named := dropped
	suffix := ""
	if len(named) > maxDroppedNamed {
		named = named[:maxDroppedNamed]
		suffix = fmt.Sprintf(" and %d more", len(dropped)-maxDroppedNamed)
	}
	return fmt.Sprintf(
		"%s is the name of the build manifest the deploy adds, so %d file(s) were left out of this upload: %s%s.",
		artifacts.ManifestName, len(dropped), strings.Join(named, ", "), suffix)
}

// readUploadParts streams every part to disk while hashing the whole upload, so
// a duplicate folder is recognised by the engine's content-addressed build id.
// A non-zero status means the upload was refused and nothing was kept.
func (s *Service) readUploadParts(
	ctx context.Context,
	reader *multipart.Reader,
	dir string,
) (uploadResult, int, string) {
	result := uploadResult{}
	hash := sha256.New()
	root := ""

	for {
		if err := ctx.Err(); err != nil {
			return result, http.StatusRequestTimeout, "the upload was cancelled"
		}
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return result, http.StatusBadRequest, "the upload body could not be read"
		}

		if part.FormName() == "zone_id" {
			value, readErr := io.ReadAll(io.LimitReader(part, 256))
			ignore(part.Close())
			if readErr != nil {
				return result, http.StatusBadRequest, "the upload body could not be read"
			}
			result.zoneID = strings.TrimSpace(string(value))
			continue
		}
		if part.FormName() != "files" {
			ignore(part.Close())
			continue
		}

		relative, ok := cleanUploadPath(partFileName(part))
		if !ok {
			ignore(part.Close())
			return result, http.StatusBadRequest, "a file path in the upload is not a relative path inside the folder"
		}
		if top, _, cut := strings.Cut(relative, "/"); cut && root == "" {
			root = top
		}
		if artifacts.Excluded(relative, false) {
			ignore(part.Close())
			continue
		}
		// A manifest.json at any depth would shadow the build manifest the
		// packer injects: the engine scans the archive for that base name and
		// takes the first entry it finds, while the injected one is written
		// last. A PWA's own manifest would then decide the framework the engine
		// builds with. It is dropped here rather than staged, and the response
		// says so.
		if path.Base(relative) == artifacts.ManifestName {
			ignore(part.Close())
			result.dropped = append(result.dropped, relative)
			continue
		}
		if result.files >= maxUploadFiles {
			ignore(part.Close())
			return result, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("a folder upload may contain at most %d files", maxUploadFiles)
		}

		written, status, message := s.writeUploadPart(part, dir, relative, hash, result.bytes)
		ignore(part.Close())
		if status != 0 {
			return result, status, message
		}
		result.files++
		result.bytes += written
	}

	result.name = SanitizeUploadName(root)
	result.digest = hex.EncodeToString(hash.Sum(nil))
	return result, 0, ""
}

// writeUploadPart streams one file, refusing as soon as the running total would
// pass the cap rather than after the whole body has been read.
func (s *Service) writeUploadPart(
	part *multipart.Part,
	dir, relative string,
	hash io.Writer,
	written int64,
) (int64, int, string) {
	target := filepath.Join(dir, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(target), uploadDirMode); err != nil {
		s.log().Error("create upload subdirectory", "error", err)
		return 0, http.StatusInternalServerError, "the upload could not be stored"
	}
	file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, uploadFileMode)
	if err != nil {
		if os.IsExist(err) {
			return 0, http.StatusBadRequest, "the upload contains the same file twice"
		}
		s.log().Error("create upload file", "error", err)
		return 0, http.StatusInternalServerError, "the upload could not be stored"
	}

	remaining := s.cfg.UploadMaxBytes - written
	copied, copyErr := io.Copy(io.MultiWriter(file, hash), io.LimitReader(part, remaining+1))
	closeErr := file.Close()
	if copyErr != nil {
		return 0, http.StatusBadRequest, "the upload body could not be read"
	}
	if closeErr != nil {
		s.log().Error("close upload file", "error", closeErr)
		return 0, http.StatusInternalServerError, "the upload could not be stored"
	}
	if copied > remaining {
		return 0, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("a folder upload may be at most %d bytes", s.cfg.UploadMaxBytes)
	}
	return copied, 0, ""
}

// partFileName reads the filename exactly as the browser sent it.
// (*multipart.Part).FileName cannot be used: RFC 7578 §4.2 says a filename
// must not carry directory information, so Go strips everything but the base
// name — and the browser's webkitRelativePath, which is the whole point of a
// folder upload, is precisely that directory information.
func partFileName(part *multipart.Part) string {
	disposition := part.Header.Get("Content-Disposition")
	if disposition == "" {
		return ""
	}
	_, params, err := mime.ParseMediaType(disposition)
	if err != nil {
		return ""
	}
	return params["filename"]
}

// cleanUploadPath validates a browser-supplied webkitRelativePath. It must be a
// relative POSIX path with no absolute prefix, no `..` component and no NUL.
func cleanUploadPath(name string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" || strings.ContainsRune(name, 0) || strings.Contains(name, `\`) {
		return "", false
	}
	if strings.HasPrefix(name, "/") {
		return "", false
	}
	cleaned := path.Clean(name)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.HasPrefix(cleaned, "/") {
		return "", false
	}
	if !filepath.IsLocal(filepath.FromSlash(cleaned)) {
		return "", false
	}
	return cleaned, true
}

func uploadOutcome(status int) string {
	switch status {
	case http.StatusRequestEntityTooLarge:
		return "too_large"
	case http.StatusInsufficientStorage:
		return "no_space"
	case http.StatusBadRequest:
		return "invalid_path"
	default:
		return "expired"
	}
}

func writeUploadError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	ignore(json.NewEncoder(w).Encode(map[string]string{"error": message}))
}

// admitUpload decides whether one more folder fits under
// HOSTING_UPLOAD_MAX_TOTAL_BYTES. What is already staged is read from the
// store rather than from a counter, so the accounting survives a restart and
// cannot drift when the janitor removes an expired upload; the counter only
// covers requests still being read, whose final size is not known yet.
func (s *Service) admitUpload(ctx context.Context) bool {
	staged, err := s.stagedUploadBytes(ctx)
	if err != nil {
		s.log().Warn("read staged upload sizes", "error", err)
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if staged+s.uploadBytes+s.cfg.UploadMaxBytes > s.cfg.UploadMaxTotalBytes {
		return false
	}
	s.uploadBytes += s.cfg.UploadMaxBytes
	return true
}

func (s *Service) releaseUploadBytes(size int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.uploadBytes -= size
	if s.uploadBytes < 0 {
		s.uploadBytes = 0
	}
}

// stagedUploadBytes sums everything the upload directory is holding: the
// unpacked folders waiting for their deploy, read from the store so the
// accounting survives a restart, plus the packed <upload-id>.tar.zst archives
// the deployer writes beside them. The archive is roughly a second copy of the
// folder and lives in the same directory, so leaving it out let a deploy in
// flight put the volume at twice UploadMaxTotalBytes.
func (s *Service) stagedUploadBytes(ctx context.Context) (int64, error) {
	docs, err := s.deps.Store.ListUploads(ctx)
	if err != nil {
		return 0, err
	}
	total := int64(0)
	for _, doc := range docs {
		total += doc.Bytes
	}
	return total + s.stagedArchiveBytes(), nil
}

// stagedArchiveBytes sums the packed archives currently on disk. A directory
// that cannot be read is reported as zero rather than as an error: the folders
// are the larger half and the janitor sweeps stray archives hourly, so a
// transient read failure must not refuse every upload.
func (s *Service) stagedArchiveBytes() int64 {
	if s.cfg.UploadDir == "" {
		return 0
	}
	entries, err := os.ReadDir(s.cfg.UploadDir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			s.log().Warn("read upload directory", "error", err)
		}
		return 0
	}
	total := int64(0)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), platform.UploadArchiveSuffix) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		total += info.Size()
	}
	return total
}
