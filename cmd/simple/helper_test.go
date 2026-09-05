package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// harness is one CLI invocation's world: the fake control plane, the three
// streams and a fake environment. Nothing here touches the real filesystem
// outside t.TempDir(), the real network outside loopback, or the operator's own
// ~/.config.
type harness struct {
	t       *testing.T
	plane   *plane
	server  *httptest.Server
	env     *env
	stdout  bytes.Buffer
	stderr  bytes.Buffer
	environ map[string]string
	home    string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	home := t.TempDir()
	control := newPlane()
	server := control.start(t)
	h := &harness{
		t: t, plane: control, server: server, home: home,
		environ: map[string]string{
			"HOME":            home,
			"XDG_CONFIG_HOME": filepath.Join(home, ".config"),
			"SIMPLE_API_URL":  server.URL,
			"TERM":            "dumb", // belt and braces: the buffers are not TTYs either
		},
	}
	h.env = &env{
		stdin:   strings.NewReader(""),
		stdout:  &h.stdout,
		stderr:  &h.stderr,
		getenv:  func(name string) string { return h.environ[name] },
		version: "v1.2.3-test",
		now: func() time.Time {
			return time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
		},
	}
	return h
}

// withToken puts a credential in the environment, which is what most tests
// want: the config file paths are exercised on their own.
func (h *harness) withToken() *harness {
	h.environ["SIMPLE_API_TOKEN"] = testToken
	return h
}

func (h *harness) withStdin(text string) *harness {
	h.env.stdin = strings.NewReader(text)
	return h
}

// run executes one command and returns its exit code, resetting the streams so
// that consecutive runs in one test do not read each other's output.
func (h *harness) run(args ...string) int {
	h.t.Helper()
	h.stdout.Reset()
	h.stderr.Reset()
	return run(context.Background(), args, h.env)
}

func (h *harness) out() string { return h.stdout.String() }
func (h *harness) err() string { return h.stderr.String() }

func (h *harness) configPath() string {
	return filepath.Join(h.environ["XDG_CONFIG_HOME"], "simple", "config.json")
}

// decode parses stdout as the JSON document a --json run promises.
func (h *harness) decode() map[string]any {
	h.t.Helper()
	var document map[string]any
	if err := json.Unmarshal(h.stdout.Bytes(), &document); err != nil {
		h.t.Fatalf("stdout is not one JSON object: %v\n%s", err, h.out())
	}
	return document
}

// decodeError parses stderr as the JSON failure envelope.
func (h *harness) decodeError() string {
	h.t.Helper()
	var document struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(h.stderr.Bytes(), &document); err != nil {
		h.t.Fatalf("stderr is not the JSON error envelope: %v\n%s", err, h.err())
	}
	return document.Error
}

func (h *harness) mustRun(args ...string) {
	h.t.Helper()
	if code := h.run(args...); code != codeOK {
		h.t.Fatalf("simple %s: exit %d\nstdout: %s\nstderr: %s",
			strings.Join(args, " "), code, h.out(), h.err())
	}
}

// field reads one typed field out of a decoded JSON document, failing the test
// rather than panicking when the document does not have the shape the CLI's
// contract promises.
func field[T any](t *testing.T, document map[string]any, name string) T {
	t.Helper()
	value, ok := document[name].(T)
	if !ok {
		t.Fatalf("%q is not a %T in the document: %v", name, value, document[name])
	}
	return value
}

// contains is the assertion used throughout: the exact wording of a sentence is
// allowed to change, the fact it names the thing is not.
func contains(t *testing.T, haystack, needle, what string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Errorf("%s does not mention %q:\n%s", what, needle, haystack)
	}
}

func absent(t *testing.T, haystack, needle, what string) {
	t.Helper()
	if strings.Contains(haystack, needle) {
		t.Errorf("%s must not contain %q:\n%s", what, needle, haystack)
	}
}

var _ io.Reader = (*bytes.Buffer)(nil)

// writeTemp puts a value in a file for the commands that read one, so a test
// never has to put a value on a command line either.
func writeTemp(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "value")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}
