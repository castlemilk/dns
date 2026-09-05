package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	platformv1 "github.com/castlemilk/dns/gen/go/platform/v1"
	"github.com/castlemilk/dns/gen/go/platform/v1/platformv1connect"
)

// platformBackupPath is served by the control process itself, because it is the
// process that holds bbolt's exclusive lock on the platform store. dnsctl never
// opens that file.
const platformBackupPath = "/internal/v1/platform-backup"

// maxPlatformBackupBytes matches the control plane's own refusal threshold, so
// a store the server would not stream is not silently truncated here either.
const maxPlatformBackupBytes int64 = 256 << 20

// bboltMagicOffset and bboltMagic identify a bbolt database: the meta page
// writes magic 0xED0CDAED little-endian directly after the 16-byte page header.
// Checking it turns a proxy error page or a truncated stream into a failure
// here rather than at restore time.
const bboltMagicOffset = 16

var bboltMagic = []byte{0xed, 0xda, 0x0c, 0xed}

func runPlatformBackup(args []string, getenv func(string) string, stdout, stderr io.Writer, token string, transport http.RoundTripper) (resultErr error) {
	flags := flag.NewFlagSet("dnsctl platform-backup", flag.ContinueOnError)
	flags.SetOutput(stderr)
	controlURL := flags.String("control-url", envDefault(getenv, "DNS_CONTROL_URL", "http://127.0.0.1:8080"), "control plane base URL")
	out := flags.String("out", "", "file to write the platform store copy to")
	timeout := flags.Duration("timeout", 10*time.Minute, "request timeout")
	flags.Usage = func() {
		if _, err := fmt.Fprintln(stderr, "usage: dnsctl platform-backup --out PATH [--control-url URL] [--timeout DURATION]"); err != nil {
			return
		}
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || strings.TrimSpace(*out) == "" {
		flags.Usage()
		return errors.New("--out is required and positional arguments are not accepted")
	}
	if *out == "-" {
		return errors.New("--out must be a file path; the copy is verified before it is kept")
	}
	if *timeout <= 0 {
		return errors.New("--timeout must be positive")
	}
	baseURL, err := validateAPIURL(*controlURL)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+platformBackupPath, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/octet-stream")

	client := newHTTPClient(*timeout, transport)
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("fetch platform backup: %w", err)
	}
	defer func() {
		resultErr = errors.Join(resultErr, response.Body.Close())
	}()

	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return errors.New("the control plane does not serve a platform backup; it needs DNS_SNAPSHOT_BEARER_TOKEN and a writer role")
	case http.StatusUnauthorized, http.StatusForbidden:
		return errors.New("the platform backup route rejected DNS_SNAPSHOT_BEARER_TOKEN")
	default:
		return fmt.Errorf("platform backup endpoint returned HTTP %d", response.StatusCode)
	}

	digest := sha256.New()
	prefix := &prefixCapture{limit: bboltMagicOffset + len(bboltMagic)}
	written, err := writeStream(*out, io.LimitReader(response.Body, maxPlatformBackupBytes+1), digest, prefix)
	if err != nil {
		return fmt.Errorf("write platform backup: %w", err)
	}
	// A copy that fails any check is deleted rather than left on disk, so an
	// operator can never restore from a file this command already rejected.
	if err := verifyBackup(written, response.Header.Get("Content-Length"), prefix); err != nil {
		return errors.Join(err, os.Remove(*out))
	}

	summary := struct {
		Path   string `json:"path"`
		Bytes  int64  `json:"bytes"`
		SHA256 string `json:"sha256"`
	}{Path: *out, Bytes: written, SHA256: hex.EncodeToString(digest.Sum(nil))}
	if err := json.NewEncoder(stdout).Encode(summary); err != nil {
		return fmt.Errorf("write backup result: %w", err)
	}
	return nil
}

func verifyBackup(written int64, declaredLength string, prefix *prefixCapture) error {
	if written > maxPlatformBackupBytes {
		return fmt.Errorf("platform backup exceeds the %d-byte limit", maxPlatformBackupBytes)
	}
	if declaredLength != "" {
		expected, err := strconv.ParseInt(declaredLength, 10, 64)
		if err != nil {
			return errors.New("platform backup declared an unreadable Content-Length")
		}
		if expected != written {
			return fmt.Errorf("platform backup is truncated: %d bytes received, %d declared", written, expected)
		}
	}
	if !prefix.hasMagic() {
		return errors.New("platform backup does not carry the bbolt database magic")
	}
	return nil
}

func runPlatformRebuild(args []string, getenv func(string) string, stdout, stderr io.Writer, token string, factory platformClientFactory) error {
	flags := flag.NewFlagSet("dnsctl platform-rebuild", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiURL := flags.String("api-url", envDefault(getenv, "DNS_API_URL", "http://127.0.0.1:8080"), "Connect API base URL")
	dryRun := flags.Bool("dry-run", false, "report what would be recreated without writing")
	timeout := flags.Duration("timeout", 5*time.Minute, "request timeout")
	flags.Usage = func() {
		if _, err := fmt.Fprintln(stderr, "usage: dnsctl platform-rebuild [--dry-run] [--api-url URL] [--timeout DURATION]"); err != nil {
			return
		}
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		flags.Usage()
		return errors.New("positional arguments are not accepted")
	}
	if *timeout <= 0 {
		return errors.New("--timeout must be positive")
	}
	baseURL, err := validateAPIURL(*apiURL)
	if err != nil {
		return err
	}

	request := connect.NewRequest(&platformv1.RebuildPlatformStoreRequest{DryRun: *dryRun})
	request.Header().Set("Authorization", "Bearer "+token)
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	response, err := factory(baseURL, *timeout).RebuildPlatformStore(ctx, request)
	if err != nil {
		return fmt.Errorf("rebuild platform store: %w", err)
	}
	summary := struct {
		Sites         uint32   `json:"sites"`
		MailDomains   uint32   `json:"mail_domains"`
		Subscriptions uint32   `json:"subscriptions"`
		DryRun        bool     `json:"dry_run"`
		Warnings      []string `json:"warnings,omitempty"`
	}{
		Sites: response.Msg.GetSites(), MailDomains: response.Msg.GetMailDomains(),
		Subscriptions: response.Msg.GetSubscriptions(), DryRun: response.Msg.GetDryRun(),
		Warnings: response.Msg.GetWarnings(),
	}
	if err := json.NewEncoder(stdout).Encode(summary); err != nil {
		return fmt.Errorf("write rebuild result: %w", err)
	}
	return nil
}

func newPlatformClient(baseURL string, timeout time.Duration) platformv1connect.PlatformServiceClient {
	return platformv1connect.NewPlatformServiceClient(newHTTPClient(timeout, nil), baseURL,
		connect.WithReadMaxBytes(maxRPCBytes), connect.WithSendMaxBytes(maxRPCBytes))
}

// prefixCapture keeps the first limit bytes of a stream so the copy can be
// identified after it has been written, without buffering the whole file.
type prefixCapture struct {
	limit  int
	buffer []byte
}

func (p *prefixCapture) Write(raw []byte) (int, error) {
	if remaining := p.limit - len(p.buffer); remaining > 0 {
		if len(raw) < remaining {
			remaining = len(raw)
		}
		p.buffer = append(p.buffer, raw[:remaining]...)
	}
	return len(raw), nil
}

func (p *prefixCapture) hasMagic() bool {
	end := bboltMagicOffset + len(bboltMagic)
	if len(p.buffer) < end {
		return false
	}
	for index, want := range bboltMagic {
		if p.buffer[bboltMagicOffset+index] != want {
			return false
		}
	}
	return true
}
