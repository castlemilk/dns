package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	platformv1 "github.com/castlemilk/dns/gen/go/platform/v1"
	"github.com/castlemilk/dns/gen/go/platform/v1/platformv1connect"
)

func platformFixture(magic []byte) []byte {
	page := make([]byte, 4096)
	copy(page[bboltMagicOffset:], magic)
	return page
}

// roundTripFunc answers without a socket, so the backup command is exercised
// end to end with no network in the unit test.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func backupResponse(t *testing.T, status int, body []byte, declaredLength string) roundTripFunc {
	t.Helper()
	return func(request *http.Request) (*http.Response, error) {
		if got := request.Header.Get("Authorization"); got != "Bearer snapshot-secret" {
			t.Errorf("Authorization = %q", got)
		}
		if request.URL.Path != platformBackupPath {
			t.Errorf("path = %q", request.URL.Path)
		}
		header := http.Header{"Content-Type": []string{"application/octet-stream"}}
		if declaredLength != "" {
			header.Set("Content-Length", declaredLength)
		}
		return &http.Response{
			StatusCode: status, Header: header, Body: io.NopCloser(bytes.NewReader(body)),
			Request: request,
		}, nil
	}
}

func runBackup(t *testing.T, destination string, transport http.RoundTripper) (string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	err := run([]string{"platform-backup", "--out", destination, "--control-url", "http://127.0.0.1:8080"},
		func(key string) string {
			if key == "DNS_SNAPSHOT_BEARER_TOKEN" {
				return "snapshot-secret"
			}
			return ""
		}, strings.NewReader(""), &stdout, &stderr, dependencies{transport: transport})
	if strings.Contains(stdout.String()+stderr.String(), "snapshot-secret") {
		t.Fatal("snapshot token was written to command output")
	}
	return stdout.String(), err
}

func TestPlatformBackupWritesVerifiedCopy(t *testing.T) {
	t.Parallel()
	body := platformFixture(bboltMagic)
	destination := filepath.Join(t.TempDir(), "nested", "platform.db")
	stdout, err := runBackup(t, destination,
		backupResponse(t, http.StatusOK, body, strconv.Itoa(len(body))))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	raw, err := os.ReadFile(destination)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if !bytes.Equal(raw, body) {
		t.Fatalf("output = %d bytes", len(raw))
	}
	info, err := os.Stat(destination)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("permissions = %v/%04o", err, info.Mode().Perm())
	}
	if !strings.Contains(stdout, `"bytes":4096`) || !strings.Contains(stdout, `"sha256":"`) {
		t.Fatalf("stdout = %q", stdout)
	}
}

func TestPlatformBackupRefusesUnusableCopies(t *testing.T) {
	t.Parallel()
	good := platformFixture(bboltMagic)
	for _, testCase := range []struct {
		name      string
		status    int
		body      []byte
		length    string
		wantError string
	}{
		{name: "wrong magic", status: http.StatusOK, body: platformFixture([]byte{0, 0, 0, 0}),
			length: "4096", wantError: "bbolt database magic"},
		{name: "truncated body", status: http.StatusOK, body: good,
			length: "99999", wantError: "truncated"},
		{name: "short body", status: http.StatusOK, body: []byte("not a database"),
			length: "14", wantError: "bbolt database magic"},
		{name: "route absent", status: http.StatusNotFound, body: nil,
			wantError: "does not serve a platform backup"},
		{name: "unauthorized", status: http.StatusUnauthorized, body: nil,
			wantError: "rejected DNS_SNAPSHOT_BEARER_TOKEN"},
		{name: "server error", status: http.StatusBadGateway, body: nil,
			wantError: "HTTP 502"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			destination := filepath.Join(t.TempDir(), "platform.db")
			_, err := runBackup(t, destination,
				backupResponse(t, testCase.status, testCase.body, testCase.length))
			if err == nil || !strings.Contains(err.Error(), testCase.wantError) {
				t.Fatalf("error = %v, want %q", err, testCase.wantError)
			}
			if _, statErr := os.Stat(destination); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("a rejected copy was retained: %v", statErr)
			}
		})
	}
}

func TestPlatformBackupRequiresSnapshotTokenAndOutput(t *testing.T) {
	t.Parallel()
	err := run([]string{"platform-backup", "--out", "copy.db"}, func(string) string { return "" },
		strings.NewReader(""), ioDiscard{}, ioDiscard{}, dependencies{})
	if err == nil || !strings.Contains(err.Error(), "DNS_SNAPSHOT_BEARER_TOKEN is required") {
		t.Fatalf("missing token error = %v", err)
	}
	getenv := func(key string) string {
		if key == "DNS_SNAPSHOT_BEARER_TOKEN" {
			return "snapshot-secret"
		}
		return ""
	}
	err = run([]string{"platform-backup"}, getenv, strings.NewReader(""), ioDiscard{}, ioDiscard{}, dependencies{})
	if err == nil || !strings.Contains(err.Error(), "--out is required") {
		t.Fatalf("missing --out error = %v", err)
	}
	err = run([]string{"platform-backup", "--out", "-"}, getenv, strings.NewReader(""), ioDiscard{}, ioDiscard{}, dependencies{})
	if err == nil || !strings.Contains(err.Error(), "must be a file path") {
		t.Fatalf("stdout target error = %v", err)
	}
	err = run([]string{"platform-backup", "--out", "copy.db", "--control-url", "http://control.example.com"},
		getenv, strings.NewReader(""), ioDiscard{}, ioDiscard{}, dependencies{})
	if err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("plaintext control URL error = %v", err)
	}
}

func TestPlatformRebuildSendsBearerAndReportsCounts(t *testing.T) {
	t.Parallel()
	const secret = "operator-secret"
	fake := &fakePlatformClient{rebuild: func(_ context.Context, request *connect.Request[platformv1.RebuildPlatformStoreRequest]) (*connect.Response[platformv1.RebuildPlatformStoreResponse], error) {
		if got := request.Header().Get("Authorization"); got != "Bearer "+secret {
			t.Fatalf("Authorization = %q", got)
		}
		if !request.Msg.GetDryRun() {
			t.Fatal("dry run was not requested")
		}
		return connect.NewResponse(&platformv1.RebuildPlatformStoreResponse{
			Sites: 2, MailDomains: 1, Subscriptions: 3, DryRun: true,
			Warnings: []string{"customer not found by email"},
		}), nil
	}}
	var factoryURL string
	var stdout, stderr bytes.Buffer
	err := run([]string{"platform-rebuild", "--dry-run", "--api-url", "https://dns.example/api/"},
		func(key string) string {
			if key == "DNS_API_BEARER_TOKEN" {
				return secret
			}
			return ""
		}, strings.NewReader(""), &stdout, &stderr,
		dependencies{platform: func(baseURL string, _ time.Duration) platformv1connect.PlatformServiceClient {
			factoryURL = baseURL
			return fake
		}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if factoryURL != "https://dns.example/api" {
		t.Fatalf("factory URL = %q", factoryURL)
	}
	for _, want := range []string{`"sites":2`, `"mail_domains":1`, `"subscriptions":3`, `"dry_run":true`, "customer not found"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("stdout = %q, want %q", stdout.String(), want)
		}
	}
	if strings.Contains(stdout.String()+stderr.String(), secret) {
		t.Fatal("bearer token was written to command output")
	}
}

func TestPlatformRebuildSurfacesRemoteRefusalWithoutTheToken(t *testing.T) {
	t.Parallel()
	const secret = "do-not-print-me"
	fake := &fakePlatformClient{rebuild: func(context.Context, *connect.Request[platformv1.RebuildPlatformStoreRequest]) (*connect.Response[platformv1.RebuildPlatformStoreResponse], error) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("the platform store is not empty"))
	}}
	err := run([]string{"platform-rebuild"}, func(key string) string {
		if key == "DNS_API_BEARER_TOKEN" {
			return secret
		}
		return ""
	}, strings.NewReader(""), ioDiscard{}, ioDiscard{},
		dependencies{platform: func(string, time.Duration) platformv1connect.PlatformServiceClient { return fake }})
	if err == nil || !strings.Contains(err.Error(), "not empty") || strings.Contains(err.Error(), secret) {
		t.Fatalf("remote error = %v", err)
	}
}

type fakePlatformClient struct {
	platformv1connect.PlatformServiceClient
	rebuild func(context.Context, *connect.Request[platformv1.RebuildPlatformStoreRequest]) (*connect.Response[platformv1.RebuildPlatformStoreResponse], error)
}

func (f *fakePlatformClient) RebuildPlatformStore(ctx context.Context, request *connect.Request[platformv1.RebuildPlatformStoreRequest]) (*connect.Response[platformv1.RebuildPlatformStoreResponse], error) {
	return f.rebuild(ctx, request)
}
