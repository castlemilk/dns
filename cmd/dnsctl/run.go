package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"connectrpc.com/connect"
	dnsv1 "github.com/castlemilk/dns/gen/go/dns/v1"
	"github.com/castlemilk/dns/gen/go/dns/v1/dnsv1connect"
	"github.com/castlemilk/dns/gen/go/platform/v1/platformv1connect"
	"github.com/castlemilk/dns/internal/zonefile"
)

const maxRPCBytes = zonefile.MaxBytes + 1<<20

type clientFactory func(string, time.Duration) dnsv1connect.DNSServiceClient

type platformClientFactory func(string, time.Duration) platformv1connect.PlatformServiceClient

// dependencies are the process boundaries the commands cross. A zero value uses
// the real ones; tests replace exactly the field they exercise.
type dependencies struct {
	dns       clientFactory
	platform  platformClientFactory
	transport http.RoundTripper
}

func (d dependencies) dnsClients() clientFactory {
	if d.dns != nil {
		return d.dns
	}
	return newConnectClient
}

func (d dependencies) platformClients() platformClientFactory {
	if d.platform != nil {
		return d.platform
	}
	return newPlatformClient
}

func run(args []string, getenv func(string) string, stdin io.Reader, stdout, stderr io.Writer, deps dependencies) error {
	if len(args) == 0 {
		writeUsage(stderr)
		return errors.New("choose import, export, platform-backup or platform-rebuild")
	}
	switch args[0] {
	case "help", "-h", "--help":
		writeUsage(stdout)
		return nil
	case "import", "export", "platform-rebuild", "platform-backup":
	default:
		writeUsage(stderr)
		return fmt.Errorf("unknown command %q", args[0])
	}
	// platform-backup reads the snapshot feed's credential, because it is the
	// snapshot bearer token that guards the platform backup route; every other
	// command spends the operator token.
	if args[0] == "platform-backup" {
		token, err := bearerToken("DNS_SNAPSHOT_BEARER_TOKEN", getenv("DNS_SNAPSHOT_BEARER_TOKEN"))
		if err != nil {
			return err
		}
		return runPlatformBackup(args[1:], getenv, stdout, stderr, token, deps.transport)
	}
	token, err := bearerToken("DNS_API_BEARER_TOKEN", getenv("DNS_API_BEARER_TOKEN"))
	if err != nil {
		return err
	}
	switch args[0] {
	case "import":
		return runImport(args[1:], getenv, stdin, stdout, stderr, token, deps.dnsClients())
	case "export":
		return runExport(args[1:], getenv, stdout, stderr, token, deps.dnsClients())
	case "platform-rebuild":
		return runPlatformRebuild(args[1:], getenv, stdout, stderr, token, deps.platformClients())
	}
	return nil
}

func runImport(args []string, getenv func(string) string, stdin io.Reader, stdout, stderr io.Writer, token string, factory clientFactory) error {
	flags := flag.NewFlagSet("dnsctl import", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiURL := flags.String("api-url", envDefault(getenv, "DNS_API_URL", "http://127.0.0.1:8080"), "Connect API base URL")
	zoneName := flags.String("zone", "", "zone apex to import")
	file := flags.String("file", "-", "BIND zone file path, or - for stdin")
	mode := flags.String("mode", "create", "import mode: create or replace")
	dryRun := flags.Bool("dry-run", false, "validate and preview without mutation")
	timeout := flags.Duration("timeout", 30*time.Second, "request timeout")
	flags.Usage = func() {
		if _, err := fmt.Fprintln(stderr, "usage: dnsctl import --zone NAME --file PATH|- [--mode create|replace] [--dry-run] [--api-url URL]"); err != nil {
			return
		}
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || strings.TrimSpace(*zoneName) == "" {
		flags.Usage()
		return errors.New("--zone is required and positional arguments are not accepted")
	}
	if *timeout <= 0 {
		return errors.New("--timeout must be positive")
	}
	baseURL, err := validateAPIURL(*apiURL)
	if err != nil {
		return err
	}
	var importMode dnsv1.ZoneImportMode
	switch strings.ToLower(strings.TrimSpace(*mode)) {
	case "create":
		importMode = dnsv1.ZoneImportMode_ZONE_IMPORT_MODE_CREATE
	case "replace":
		importMode = dnsv1.ZoneImportMode_ZONE_IMPORT_MODE_REPLACE
	default:
		return errors.New("--mode must be create or replace")
	}
	raw, err := readInput(*file, stdin, zonefile.MaxBytes)
	if err != nil {
		return fmt.Errorf("read zone file: %w", err)
	}
	request := connect.NewRequest(&dnsv1.ImportZoneRequest{
		Name: *zoneName, ZoneFile: string(raw), Mode: importMode, DryRun: *dryRun,
	})
	request.Header().Set("Authorization", "Bearer "+token)
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	response, err := factory(baseURL, *timeout).ImportZone(ctx, request)
	if err != nil {
		return fmt.Errorf("import zone: %w", err)
	}
	summary := struct {
		Name        string   `json:"name"`
		ZoneID      string   `json:"zone_id"`
		Mode        string   `json:"mode"`
		DryRun      bool     `json:"dry_run"`
		RecordCount int      `json:"record_count"`
		Warnings    []string `json:"warnings,omitempty"`
	}{
		Name: response.Msg.GetZone().GetName(), ZoneID: response.Msg.GetZone().GetId(), Mode: strings.ToLower(strings.TrimSpace(*mode)),
		DryRun: response.Msg.GetDryRun(), RecordCount: len(response.Msg.GetZone().GetRecords()), Warnings: response.Msg.GetWarnings(),
	}
	if err := json.NewEncoder(stdout).Encode(summary); err != nil {
		return fmt.Errorf("write import result: %w", err)
	}
	return nil
}

func runExport(args []string, getenv func(string) string, stdout, stderr io.Writer, token string, factory clientFactory) error {
	flags := flag.NewFlagSet("dnsctl export", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiURL := flags.String("api-url", envDefault(getenv, "DNS_API_URL", "http://127.0.0.1:8080"), "Connect API base URL")
	zoneID := flags.String("zone-id", "", "zone ID to export")
	output := flags.String("output", "-", "output path, or - for stdout")
	timeout := flags.Duration("timeout", 30*time.Second, "request timeout")
	flags.Usage = func() {
		if _, err := fmt.Fprintln(stderr, "usage: dnsctl export --zone-id ID [--output PATH|-] [--api-url URL]"); err != nil {
			return
		}
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || strings.TrimSpace(*zoneID) == "" {
		flags.Usage()
		return errors.New("--zone-id is required and positional arguments are not accepted")
	}
	if *timeout <= 0 {
		return errors.New("--timeout must be positive")
	}
	baseURL, err := validateAPIURL(*apiURL)
	if err != nil {
		return err
	}
	request := connect.NewRequest(&dnsv1.ExportZoneRequest{ZoneId: *zoneID})
	request.Header().Set("Authorization", "Bearer "+token)
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	response, err := factory(baseURL, *timeout).ExportZone(ctx, request)
	if err != nil {
		return fmt.Errorf("export zone: %w", err)
	}
	raw := []byte(response.Msg.GetZoneFile())
	if len(raw) > zonefile.MaxBytes {
		return fmt.Errorf("export exceeds the %d-byte limit", zonefile.MaxBytes)
	}
	if err := writeOutput(*output, stdout, raw); err != nil {
		return fmt.Errorf("write zone file: %w", err)
	}
	return nil
}

func newConnectClient(baseURL string, timeout time.Duration) dnsv1connect.DNSServiceClient {
	return dnsv1connect.NewDNSServiceClient(newHTTPClient(timeout, nil), baseURL,
		connect.WithReadMaxBytes(maxRPCBytes), connect.WithSendMaxBytes(maxRPCBytes))
}

// newHTTPClient is the one HTTP client every command uses. Redirects are never
// followed: a redirect would resend the bearer token to whatever host answered.
func newHTTPClient(timeout time.Duration, transport http.RoundTripper) *http.Client {
	if transport == nil {
		dialer := &net.Dialer{Timeout: min(timeout, 10*time.Second), KeepAlive: 30 * time.Second}
		transport = &http.Transport{
			Proxy: http.ProxyFromEnvironment, DialContext: dialer.DialContext,
			ForceAttemptHTTP2: true, MaxIdleConns: 10, IdleConnTimeout: 30 * time.Second,
			TLSHandshakeTimeout: min(timeout, 10*time.Second), ResponseHeaderTimeout: min(timeout, time.Minute),
		}
	}
	return &http.Client{
		Transport: transport, Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func validateAPIURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("--api-url must be an absolute HTTP or HTTPS URL without userinfo, a query, or a fragment")
	}
	if parsed.Scheme == "http" && !isLoopbackHost(parsed.Hostname()) {
		return "", errors.New("--api-url must use HTTPS unless its host is localhost or a loopback IP")
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

func isLoopbackHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "localhost" {
		return true
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}

func bearerToken(name, raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	if len(value) > 4096 {
		return "", fmt.Errorf("%s is invalid", name)
	}
	for _, char := range value {
		if char <= 0x20 || char > 0x7e {
			return "", fmt.Errorf("%s is invalid", name)
		}
	}
	return value, nil
}

func readInput(path string, stdin io.Reader, limit int) (raw []byte, resultErr error) {
	reader := stdin
	var file *os.File
	if path != "-" {
		info, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("%s is not a regular file", path)
		}
		file, err = os.Open(path)
		if err != nil {
			return nil, err
		}
		defer func() {
			resultErr = errors.Join(resultErr, file.Close())
		}()
		reader = file
	}
	raw, err := io.ReadAll(io.LimitReader(reader, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > limit {
		return nil, fmt.Errorf("input exceeds the %d-byte limit", limit)
	}
	return raw, nil
}

func writeOutput(path string, stdout io.Writer, raw []byte) error {
	if path == "-" {
		_, err := stdout.Write(raw)
		return err
	}
	return atomicWrite(path, func(file io.Writer) error {
		_, err := file.Write(raw)
		return err
	})
}

// writeStream copies reader into path atomically, mirroring every byte into the
// observers so the finished file can be verified without reading it back.
func writeStream(path string, reader io.Reader, observers ...io.Writer) (int64, error) {
	var written int64
	err := atomicWrite(path, func(file io.Writer) error {
		destination := file
		if len(observers) > 0 {
			destination = io.MultiWriter(append([]io.Writer{file}, observers...)...)
		}
		count, err := io.Copy(destination, reader)
		written = count
		return err
	})
	return written, err
}

func atomicWrite(path string, write func(io.Writer) error) (resultErr error) {
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".dnsctl-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	temporaryOpen := true
	defer func() {
		if resultErr != nil && temporaryOpen {
			resultErr = errors.Join(resultErr, temporary.Close())
		}
		if err := os.Remove(temporaryPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			resultErr = errors.Join(resultErr, err)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return err
	}
	if err := write(temporary); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	temporaryOpen = false
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	directoryHandle, err := os.Open(directory)
	if err != nil {
		return err
	}
	syncErr := directoryHandle.Sync()
	closeErr := directoryHandle.Close()
	return errors.Join(syncErr, closeErr)
}

func envDefault(getenv func(string) string, name, fallback string) string {
	if value := strings.TrimSpace(getenv(name)); value != "" {
		return value
	}
	return fallback
}

func writeUsage(writer io.Writer) {
	if _, err := fmt.Fprintln(writer, "usage: dnsctl import|export|platform-backup|platform-rebuild [options]"); err != nil {
		return
	}
}
