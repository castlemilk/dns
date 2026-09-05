package artifacts

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestPackUnpackRoundtrip(t *testing.T) {
	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "index.html"), "<h1>hi</h1>")
	mustWrite(t, filepath.Join(src, "assets", "a.css"), "body{}")
	mustWrite(t, filepath.Join(src, "nested", "deep", "f.txt"), "deep")

	man := &Manifest{Framework: "static", AppRef: "demo", BuildRef: "b-1"}
	var buf bytes.Buffer
	if err := Pack(context.Background(), src, man, &buf); err != nil {
		t.Fatalf("pack: %v", err)
	}

	dst := t.TempDir()
	if err := Unpack(context.Background(), bytes.NewReader(buf.Bytes()), dst); err != nil {
		t.Fatalf("unpack: %v", err)
	}
	for _, want := range []string{"index.html", "assets/a.css", "nested/deep/f.txt", ManifestName} {
		body, err := os.ReadFile(filepath.Join(dst, want))
		if err != nil {
			t.Fatalf("missing %s: %v", want, err)
		}
		if want == ManifestName {
			got, err := ReadManifest(body)
			if err != nil || got.Framework != "static" || got.AppRef != "demo" {
				t.Fatalf("manifest roundtrip: %+v %v", got, err)
			}
		}
	}

	// Manifest is discoverable by streaming scan.
	got, err := ScanManifest(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	m, err := ReadManifest(got)
	if err != nil {
		t.Fatalf("read scanned manifest: %v", err)
	}
	if m.Framework != "static" {
		t.Fatalf("scanned manifest framework = %q", m.Framework)
	}
}

func TestUnpackRejectsTraversal(t *testing.T) {
	mal := &bytes.Buffer{}
	zw, err := zstd.NewWriter(mal)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(zw)
	hdr := &tar.Header{Name: "../evil", Mode: 0o644, Size: 4, Typeflag: tar.TypeReg}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("evil")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	if err := Unpack(context.Background(), mal, t.TempDir()); err == nil {
		t.Fatal("expected ErrUnsafePath, got nil")
	}
}

func TestStaticPrefixesFor(t *testing.T) {
	got := StaticPrefixesFor("nextjs")
	if len(got) != 1 || got[0] != "_next/static" {
		t.Fatalf("nextjs prefixes = %v", got)
	}
	if len(StaticPrefixesFor("static")) != 0 {
		t.Fatal("static should have no prefixes")
	}
}

func TestPackExcludesCredentialsAndGitMetadata(t *testing.T) {
	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "index.html"), "ok")
	mustWrite(t, filepath.Join(src, ".env"), "SECRET=do-not-pack")
	mustWrite(t, filepath.Join(src, ".env.production"), "SECRET=do-not-pack")
	mustWrite(t, filepath.Join(src, ".npmrc"), "//registry.example/:_authToken=secret")
	mustWrite(t, filepath.Join(src, ".git", "config"), "[remote]\n")
	mustWrite(t, filepath.Join(src, "tls.key"), "private key")

	var buf bytes.Buffer
	if err := Pack(context.Background(), src, &Manifest{Framework: "static"}, &buf); err != nil {
		t.Fatalf("pack: %v", err)
	}
	dst := t.TempDir()
	if err := Unpack(context.Background(), bytes.NewReader(buf.Bytes()), dst); err != nil {
		t.Fatalf("unpack: %v", err)
	}
	for _, name := range []string{".env", ".env.production", ".npmrc", ".git/config", "tls.key"} {
		if _, err := os.Stat(filepath.Join(dst, name)); !os.IsNotExist(err) {
			t.Errorf("sensitive file %q was packed", name)
		}
	}
}

func mustWrite(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
