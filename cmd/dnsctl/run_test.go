package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	dnsv1 "github.com/castlemilk/dns/gen/go/dns/v1"
	"github.com/castlemilk/dns/gen/go/dns/v1/dnsv1connect"
)

func TestRunImportUsesBearerAndBoundedRequest(t *testing.T) {
	t.Parallel()
	const secret = "api-super-secret"
	fake := &fakeDNSClient{}
	fake.importZone = func(_ context.Context, request *connect.Request[dnsv1.ImportZoneRequest]) (*connect.Response[dnsv1.ImportZoneResponse], error) {
		if got := request.Header().Get("Authorization"); got != "Bearer "+secret {
			t.Fatalf("Authorization = %q", got)
		}
		if request.Msg.GetName() != "example.com" || request.Msg.GetMode() != dnsv1.ZoneImportMode_ZONE_IMPORT_MODE_REPLACE || !request.Msg.GetDryRun() {
			t.Fatalf("request = %#v", request.Msg)
		}
		if request.Msg.GetZoneFile() != "www 60 IN A 192.0.2.1\n" {
			t.Fatalf("zone file = %q", request.Msg.GetZoneFile())
		}
		return connect.NewResponse(&dnsv1.ImportZoneResponse{
			Zone:     &dnsv1.Zone{Id: "zone-id", Name: "example.com", Records: []*dnsv1.Record{{Id: "record-id"}}},
			Warnings: []string{"managed SOA used"}, DryRun: true,
		}), nil
	}
	var factoryURL string
	factory := func(baseURL string, timeout time.Duration) dnsv1connect.DNSServiceClient {
		factoryURL = baseURL
		if timeout != 7*time.Second {
			t.Fatalf("timeout = %s", timeout)
		}
		return fake
	}
	getenv := func(key string) string {
		switch key {
		case "DNS_API_BEARER_TOKEN":
			return secret
		case "DNS_API_URL":
			return "https://dns.example/api/"
		default:
			return ""
		}
	}
	var stdout, stderr bytes.Buffer
	err := run([]string{"import", "--zone", "example.com", "--file", "-", "--mode", "replace", "--dry-run", "--timeout", "7s"},
		getenv, strings.NewReader("www 60 IN A 192.0.2.1\n"), &stdout, &stderr, dependencies{dns: factory})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if factoryURL != "https://dns.example/api" {
		t.Fatalf("factory URL = %q", factoryURL)
	}
	if !strings.Contains(stdout.String(), `"zone_id":"zone-id"`) || !strings.Contains(stdout.String(), `"dry_run":true`) {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if strings.Contains(stdout.String()+stderr.String(), secret) {
		t.Fatal("bearer token was written to command output")
	}
}

func TestRunExportWritesAtomicallyAndDoesNotLogToken(t *testing.T) {
	t.Parallel()
	const secret = "another-secret"
	fake := &fakeDNSClient{}
	fake.exportZone = func(_ context.Context, request *connect.Request[dnsv1.ExportZoneRequest]) (*connect.Response[dnsv1.ExportZoneResponse], error) {
		if request.Header().Get("Authorization") != "Bearer "+secret || request.Msg.GetZoneId() != "zone-id" {
			t.Fatalf("request/header = %#v/%#v", request.Msg, request.Header())
		}
		return connect.NewResponse(&dnsv1.ExportZoneResponse{Name: "example.com", ZoneFile: "$ORIGIN example.com.\n"}), nil
	}
	destination := filepath.Join(t.TempDir(), "nested", "example.zone")
	var stdout, stderr bytes.Buffer
	err := run([]string{"export", "--zone-id", "zone-id", "--output", destination},
		func(key string) string {
			if key == "DNS_API_BEARER_TOKEN" {
				return secret
			}
			return ""
		}, strings.NewReader(""), &stdout, &stderr,
		dependencies{dns: func(string, time.Duration) dnsv1connect.DNSServiceClient { return fake }})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	raw, err := os.ReadFile(destination)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if string(raw) != "$ORIGIN example.com.\n" {
		t.Fatalf("output = %q", raw)
	}
	info, err := os.Stat(destination)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("permissions = %v/%04o", err, info.Mode().Perm())
	}
	if strings.Contains(stdout.String()+stderr.String(), secret) {
		t.Fatal("bearer token was written to command output")
	}
}

func TestRunRejectsMissingTokenAndNeverIncludesTokenInRemoteError(t *testing.T) {
	t.Parallel()
	if err := run([]string{"import", "--zone", "example.com"}, func(string) string { return "" }, strings.NewReader(""), ioDiscard{}, ioDiscard{}, dependencies{}); err == nil || !strings.Contains(err.Error(), "DNS_API_BEARER_TOKEN") {
		t.Fatalf("missing token error = %v", err)
	}
	const secret = "do-not-print-me"
	fake := &fakeDNSClient{importZone: func(context.Context, *connect.Request[dnsv1.ImportZoneRequest]) (*connect.Response[dnsv1.ImportZoneResponse], error) {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("control unavailable"))
	}}
	err := run([]string{"import", "--zone", "example.com"}, func(key string) string {
		if key == "DNS_API_BEARER_TOKEN" {
			return secret
		}
		return ""
	}, strings.NewReader(""), ioDiscard{}, ioDiscard{}, dependencies{dns: func(string, time.Duration) dnsv1connect.DNSServiceClient { return fake }})
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("remote error = %v", err)
	}
}

func TestBoundedInputAndSafeOutput(t *testing.T) {
	t.Parallel()
	if _, err := readInput("-", strings.NewReader("1234"), 3); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("bounded input error = %v", err)
	}
	directory := t.TempDir()
	target := filepath.Join(directory, "target")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := writeOutput(link, ioDiscard{}, []byte("data")); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("symlink output error = %v", err)
	}
}

func TestValidateAPIURLRequiresHTTPSOffLoopback(t *testing.T) {
	t.Parallel()
	for _, accepted := range []string{
		"http://localhost:8080", "http://127.0.0.1:8080", "http://[::1]:8080", "https://dns.example/api",
	} {
		if _, err := validateAPIURL(accepted); err != nil {
			t.Errorf("validateAPIURL(%q): %v", accepted, err)
		}
	}
	for _, rejected := range []string{"http://dns.example", "http://192.0.2.1:8080", "http://dns-control:8080"} {
		if _, err := validateAPIURL(rejected); err == nil || !strings.Contains(err.Error(), "HTTPS") {
			t.Errorf("validateAPIURL(%q) error = %v", rejected, err)
		}
	}
}

type fakeDNSClient struct {
	dnsv1connect.DNSServiceClient
	importZone func(context.Context, *connect.Request[dnsv1.ImportZoneRequest]) (*connect.Response[dnsv1.ImportZoneResponse], error)
	exportZone func(context.Context, *connect.Request[dnsv1.ExportZoneRequest]) (*connect.Response[dnsv1.ExportZoneResponse], error)
}

func (f *fakeDNSClient) ImportZone(ctx context.Context, request *connect.Request[dnsv1.ImportZoneRequest]) (*connect.Response[dnsv1.ImportZoneResponse], error) {
	return f.importZone(ctx, request)
}

func (f *fakeDNSClient) ExportZone(ctx context.Context, request *connect.Request[dnsv1.ExportZoneRequest]) (*connect.Response[dnsv1.ExportZoneResponse], error) {
	return f.exportZone(ctx, request)
}

type ioDiscard struct{}

func (ioDiscard) Write(value []byte) (int, error) { return len(value), nil }
