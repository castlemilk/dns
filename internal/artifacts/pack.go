// Package artifacts packs and unpacks deployment artifacts (tar + zstd) and
// defines the manifest contract shared by builder, controlplane and hydra.
//
// Copied from github.com/benebsworth/deephost internal/artifacts
// (internal/artifacts/artifacts.go, commit 3ab3e16 plus the owner's working
// tree) because Go-internal packages are not importable across modules and the
// folder-upload deploy path must produce exactly the archive DeepHost's Deploy
// stream accepts — same tar layout, same zstd framing, same ignore rules, same
// manifest.json contract. Do not diverge: if DeepHost changes this file,
// re-copy it. spec2 §11 (WP1) and hosting-facts.md §5 record the copy.
//
// The one deliberate divergence is error handling that this repository's
// .golangci.yml (errcheck with check-blank) rejects: `_ = c.Close()` became
// closeQuietly(c) and the manifest marshal now returns its error. No behaviour
// changed.
package artifacts

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// closeQuietly discards a Close error on a path that already has one to
// report. It exists only because errcheck's check-blank rejects the
// upstream `_ = c.Close()` form.
func closeQuietly(c io.Closer) {
	if c == nil {
		return
	}
	if err := c.Close(); err != nil {
		_ = err
	}
}

const ManifestName = "manifest.json"

const maxManifestBytes = 1 << 20
const maxUnpackedBytes = 1 << 30
const maxFileBytes = 256 << 20
const maxArchiveEntries = 100000

// Framework names shared across services (mirror of api/v1alpha1 strings to
// keep this package dependency-light).
const (
	FrameworkStaticName = "static"
	FrameworkNodeName   = "node"
	FrameworkNextJSName = "nextjs"
)

// Manifest is the JSON sidecar stored next to each artifact.
type Manifest struct {
	Framework      string   `json:"framework"`
	AppRef         string   `json:"appRef"`
	BuildRef       string   `json:"buildRef"`
	GitSHA         string   `json:"gitSha,omitempty"`
	StaticPrefixes []string `json:"staticPrefixes,omitempty"`
	ArtifactBytes  int64    `json:"artifactBytes,omitempty"`
}

// StaticPrefixesFor derives default static prefixes when the manifest omits
// them, keyed by framework.
func StaticPrefixesFor(framework string) []string {
	switch framework {
	case "nextjs":
		return []string{"_next/static"}
	default:
		return nil
	}
}

// Pack writes dir as a tar.zst stream to w. A manifest is injected at the
// archive root if one does not already exist in dir.
func Pack(ctx context.Context, dir string, man *Manifest, w io.Writer) error {
	if man == nil {
		man = &Manifest{}
	}
	enc, err := zstd.NewWriter(w)
	if err != nil {
		return fmt.Errorf("artifacts: zstd writer: %w", err)
	}
	tw := tar.NewWriter(enc)

	writeFile := func(name string, body []byte, mode int64) error {
		hdr := &tar.Header{Name: name, Mode: mode, Size: int64(len(body))}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		_, err := tw.Write(body)
		return err
	}

	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		name := filepath.ToSlash(rel)
		if shouldExclude(name, d.IsDir()) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return tw.WriteHeader(&tar.Header{Name: name + "/", Mode: 0o755, Typeflag: tar.TypeDir})
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil // skip sockets/symlinks for MVP determinism
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer closeQuietly(f)
		if name == ManifestName {
			return nil
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: int64(info.Mode().Perm()), Size: info.Size()}); err != nil {
			return err
		}
		_, err = io.Copy(tw, f)
		return err
	})
	if err != nil {
		closeQuietly(tw)
		closeQuietly(enc)
		return fmt.Errorf("artifacts: walk %s: %w", dir, err)
	}

	if man.Framework != "" {
		body, marshalErr := json.Marshal(man)
		if marshalErr != nil {
			closeQuietly(tw)
			closeQuietly(enc)
			return fmt.Errorf("artifacts: inject manifest: %w", marshalErr)
		}
		if err := writeFile(ManifestName, body, 0o644); err != nil {
			closeQuietly(tw)
			closeQuietly(enc)
			return fmt.Errorf("artifacts: inject manifest: %w", err)
		}
	}

	if err := tw.Close(); err != nil {
		closeQuietly(enc)
		return fmt.Errorf("artifacts: tar close: %w", err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("artifacts: zstd close: %w", err)
	}
	return nil
}

var ErrUnsafePath = fmt.Errorf("artifacts: unsafe path in archive")

// Unpack extracts a tar.zst reader into dst, rejecting path traversal and
// absolute entries.
func Unpack(ctx context.Context, r io.Reader, dst string) error {
	dec, err := zstd.NewReader(r)
	if err != nil {
		return fmt.Errorf("artifacts: zstd reader: %w", err)
	}
	defer dec.Close()

	tr := tar.NewReader(dec)
	var unpacked int64
	var entries int
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("artifacts: tar read: %w", err)
		}
		entries++
		if entries > maxArchiveEntries {
			return fmt.Errorf("artifacts: archive exceeds %d entries", maxArchiveEntries)
		}
		name := hdr.Name
		clean := filepath.Clean(filepath.FromSlash(name))
		if name == "" || filepath.IsAbs(clean) || clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
			return ErrUnsafePath
		}
		target := filepath.Join(dst, clean)
		rel, relErr := filepath.Rel(dst, target)
		if relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return ErrUnsafePath
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if hdr.Size < 0 || hdr.Size > maxFileBytes || unpacked > maxUnpackedBytes-hdr.Size {
				return fmt.Errorf("artifacts: archive exceeds size limits")
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode).Perm())
			if err != nil {
				return err
			}
			n, copyErr := io.CopyN(f, tr, hdr.Size)
			closeErr := f.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
			if n != hdr.Size {
				return fmt.Errorf("artifacts: short write %s (%d/%d)", name, n, hdr.Size)
			}
			unpacked += n
		default:
			// ignore others MVP
		}
	}
}

func shouldExclude(name string, directory bool) bool {
	parts := strings.Split(filepath.ToSlash(name), "/")
	base := parts[len(parts)-1]
	for _, part := range parts {
		if part == ".git" {
			return true
		}
	}
	if directory {
		return false
	}
	if base == ".npmrc" || base == "credentials" || base == "credentials.json" {
		return true
	}
	if base == ".env" || strings.HasPrefix(base, ".env.") {
		return true
	}
	for _, suffix := range []string{".pem", ".key", ".p12", ".pfx"} {
		if strings.HasSuffix(strings.ToLower(base), suffix) {
			return true
		}
	}
	return false
}

// ReadManifest parses a manifest.json body.
func ReadManifest(b []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(bytes.TrimSpace(b), &m); err != nil {
		return nil, fmt.Errorf("artifacts: parse manifest: %w", err)
	}
	if m.StaticPrefixes == nil {
		m.StaticPrefixes = StaticPrefixesFor(m.Framework)
	}
	return &m, nil
}

// ScanManifest streams a tar.zst archive and returns the manifest.json bytes
// found anywhere in it, without extracting the whole archive.
func ScanManifest(r io.Reader) ([]byte, error) {
	dec, err := zstd.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("artifacts: zstd: %w", err)
	}
	defer dec.Close()
	tr := tar.NewReader(dec)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("artifacts: archive has no %s", ManifestName)
		}
		if err != nil {
			return nil, err
		}
		if filepath.Base(hdr.Name) != ManifestName {
			continue
		}
		if hdr.Size < 0 || hdr.Size > maxManifestBytes {
			return nil, fmt.Errorf("artifacts: manifest exceeds %d bytes", maxManifestBytes)
		}
		var buf bytes.Buffer
		if _, err := io.CopyN(&buf, tr, hdr.Size); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}
}
