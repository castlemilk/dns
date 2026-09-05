package snapshot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/castlemilk/dns/internal/authoritative"
	"github.com/castlemilk/dns/internal/telemetry"
	"github.com/castlemilk/dns/internal/zone"
)

const maxFutureClockSkew = 5 * time.Minute

type Status struct {
	mu                  sync.RWMutex
	generatedAt         time.Time
	lastSuccessfulFetch time.Time
	maxStaleness        time.Duration
	now                 func() time.Time
	metrics             *telemetry.Metrics
}

func NewStatus(maxStaleness time.Duration, metricSets ...*telemetry.Metrics) *Status {
	return &Status{maxStaleness: maxStaleness, now: time.Now, metrics: telemetry.Select(metricSets...)}
}

func (s *Status) Ready() (bool, string) {
	s.mu.RLock()
	lastSuccessfulFetch := s.lastSuccessfulFetch
	s.mu.RUnlock()
	if lastSuccessfulFetch.IsZero() {
		return false, "no valid snapshot"
	}
	age := s.now().UTC().Sub(lastSuccessfulFetch)
	if age < 0 {
		age = 0
	}
	if age > s.maxStaleness {
		return false, "snapshot is stale"
	}
	return true, "ready"
}

func (s *Status) GeneratedAt() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.generatedAt
}

func (s *Status) markValid(generatedAt, fetchedAt time.Time) {
	s.mu.Lock()
	s.generatedAt = generatedAt.UTC()
	s.lastSuccessfulFetch = fetchedAt.UTC()
	s.mu.Unlock()
	s.metrics.SetSnapshotStatus(fetchedAt, s.maxStaleness)
}

func (s *Status) markFetched(fetchedAt time.Time) bool {
	s.mu.Lock()
	if s.generatedAt.IsZero() {
		s.mu.Unlock()
		return false
	}
	s.lastSuccessfulFetch = fetchedAt.UTC()
	s.mu.Unlock()
	s.metrics.SetSnapshotStatus(fetchedAt, s.maxStaleness)
	return true
}

type Consumer struct {
	applyMu    sync.Mutex
	server     *authoritative.Server
	status     *Status
	client     *http.Client
	url        string
	token      string
	cachePath  string
	maxBytes   int64
	production bool
	logger     *slog.Logger
	now        func() time.Time
	metrics    *telemetry.Metrics
	checksum   string
	etag       string
	compile    func([]zone.Zone) (*authoritative.CompiledSnapshot, error)
	writeCache func(string, []byte) error
}

func NewConsumer(
	server *authoritative.Server,
	status *Status,
	client *http.Client,
	url string,
	token string,
	cachePath string,
	maxBytes int64,
	production bool,
	logger *slog.Logger,
	metricSets ...*telemetry.Metrics,
) *Consumer {
	if logger == nil {
		logger = slog.Default()
	}
	if client == nil {
		client = http.DefaultClient
	}
	metrics := telemetry.Select(metricSets...)
	if len(metricSets) == 0 && status != nil {
		metrics = status.metrics
	}
	return &Consumer{
		server:     server,
		status:     status,
		client:     client,
		url:        url,
		token:      token,
		cachePath:  cachePath,
		maxBytes:   maxBytes,
		production: production,
		logger:     logger,
		now:        time.Now,
		metrics:    metrics,
		compile:    authoritative.Compile,
		writeCache: writeAtomic,
	}
}

func (c *Consumer) LoadCache() (resultErr error) {
	started := time.Time{}
	if c.metrics.MetricsEnabled() {
		started = time.Now()
	}
	size := -1
	defer func() {
		outcome := "success"
		if resultErr != nil {
			outcome = "error"
		}
		elapsed := time.Duration(0)
		if !started.IsZero() {
			elapsed = time.Since(started)
		}
		c.metrics.SnapshotCache(context.Background(), "load", outcome, elapsed, size)
	}()
	c.applyMu.Lock()
	defer c.applyMu.Unlock()
	raw, err := readSecureFile(c.cachePath, c.maxBytes)
	if err != nil {
		return fmt.Errorf("read snapshot cache: %w", err)
	}
	size = len(raw)
	if err := c.applyLocked(raw, false, false, "cache"); err != nil {
		return fmt.Errorf("load snapshot cache: %w", err)
	}
	return nil
}

func (c *Consumer) Fetch(ctx context.Context) (resultErr error) {
	started := time.Time{}
	if c.metrics.MetricsEnabled() {
		started = time.Now()
	}
	outcome := "success"
	size := -1
	ctx, finishSpan := c.metrics.StartSnapshotFetch(ctx)
	defer func() {
		elapsed := time.Duration(0)
		if !started.IsZero() {
			elapsed = time.Since(started)
		}
		c.metrics.SnapshotFetch(ctx, outcome, elapsed, size)
		finishSpan(outcome)
	}()
	c.applyMu.Lock()
	defer c.applyMu.Unlock()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		outcome = "request_error"
		return fmt.Errorf("create snapshot request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+c.token)
	if c.etag != "" {
		request.Header.Set("If-None-Match", c.etag)
	}
	telemetry.InjectHTTPTrace(request.Context(), request.Header)
	response, err := c.client.Do(request)
	if err != nil {
		outcome = "transport_error"
		return fmt.Errorf("fetch snapshot: %w", err)
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			c.logger.Debug("close snapshot response body", "error", err)
		}
	}()
	if response.StatusCode == http.StatusNotModified {
		outcome = "not_modified"
		c.discardResponseBody(response.Body)
		if !c.status.markFetched(c.now().UTC()) {
			outcome = "apply_error"
			return errors.New("fetch snapshot: control returned 304 before any valid snapshot")
		}
		return nil
	}
	if response.StatusCode != http.StatusOK {
		outcome = "http_error"
		c.discardResponseBody(response.Body)
		return fmt.Errorf("fetch snapshot: control returned %s", response.Status)
	}
	raw, err := readBounded(response.Body, c.maxBytes)
	if err != nil {
		outcome = "read_error"
		return fmt.Errorf("read snapshot response: %w", err)
	}
	size = len(raw)
	if err := c.applyLocked(raw, true, true, "fetch"); err != nil {
		outcome = "apply_error"
		return fmt.Errorf("apply fetched snapshot: %w", err)
	}
	return nil
}

func (c *Consumer) discardResponseBody(body io.Reader) {
	if _, err := io.CopyN(io.Discard, body, 4096); err != nil && !errors.Is(err, io.EOF) {
		c.logger.Debug("discard snapshot response body", "error", err)
	}
}

func (c *Consumer) Run(ctx context.Context, interval time.Duration) {
	c.fetchAndLog(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.fetchAndLog(ctx)
		}
	}
}

func (c *Consumer) fetchAndLog(ctx context.Context) {
	if err := c.Fetch(ctx); err != nil && ctx.Err() == nil {
		c.logger.Warn("refresh authoritative snapshot", "error", err)
	}
}

func (c *Consumer) applyLocked(raw []byte, persist, fetched bool, source string) (resultErr error) {
	started := time.Time{}
	if c.metrics.MetricsEnabled() {
		started = time.Now()
	}
	outcome := "success"
	defer func() {
		if resultErr != nil && outcome == "success" {
			outcome = "other"
		}
		elapsed := time.Duration(0)
		if !started.IsZero() {
			elapsed = time.Since(started)
		}
		c.metrics.SnapshotApply(context.Background(), source, outcome, elapsed)
	}()
	document, err := Decode(raw)
	if err != nil {
		outcome = snapshotDecodeOutcome(err)
		return err
	}
	now := c.now().UTC()
	if document.GeneratedAt.After(now.Add(maxFutureClockSkew)) {
		outcome = "clock_error"
		return fmt.Errorf("snapshot generated_at is more than %s in the future", maxFutureClockSkew)
	}
	if current := c.status.GeneratedAt(); !current.IsZero() && document.GeneratedAt.Before(current) {
		outcome = "rollback_error"
		return fmt.Errorf("snapshot generated_at %s predates current snapshot %s", document.GeneratedAt, current)
	}
	if c.production {
		if err := ValidateProductionNameservers(document.Zones); err != nil {
			outcome = "nameserver_error"
			return err
		}
	}
	freshness := document.GeneratedAt
	if fetched {
		freshness = now
	}
	if document.Checksum == c.checksum && !c.status.GeneratedAt().IsZero() {
		c.etag = checksumETag(document.Checksum)
		c.status.markValid(document.GeneratedAt, freshness)
		return nil
	}
	compileStarted := time.Time{}
	if c.metrics.MetricsEnabled() {
		compileStarted = time.Now()
	}
	compiled, err := c.compile(document.Zones)
	compileOutcome := "success"
	if err != nil {
		compileOutcome = "error"
	}
	compileElapsed := time.Duration(0)
	if !compileStarted.IsZero() {
		compileElapsed = time.Since(compileStarted)
	}
	c.metrics.AuthoritativeCompile(context.Background(), compileOutcome, compileElapsed)
	if err != nil {
		outcome = "compile_error"
		return err
	}
	if persist {
		cacheStarted := time.Time{}
		if c.metrics.MetricsEnabled() {
			cacheStarted = time.Now()
		}
		if err := c.writeCache(c.cachePath, raw); err != nil {
			elapsed := time.Duration(0)
			if !cacheStarted.IsZero() {
				elapsed = time.Since(cacheStarted)
			}
			c.metrics.SnapshotCache(context.Background(), "write", "error", elapsed, len(raw))
			outcome = "cache_write_error"
			return fmt.Errorf("persist snapshot cache: %w", err)
		}
		elapsed := time.Duration(0)
		if !cacheStarted.IsZero() {
			elapsed = time.Since(cacheStarted)
		}
		c.metrics.SnapshotCache(context.Background(), "write", "success", elapsed, len(raw))
	}
	c.server.ReplaceCompiled(compiled)
	c.checksum = document.Checksum
	c.etag = checksumETag(document.Checksum)
	c.status.markValid(document.GeneratedAt, freshness)
	return nil
}

func snapshotDecodeOutcome(err error) string {
	if strings.Contains(strings.ToLower(err.Error()), "checksum") {
		return "checksum_error"
	}
	return "decode_error"
}

func checksumETag(checksum string) string {
	return `"sha256:` + checksum + `"`
}

func readSecureFile(path string, maxBytes int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s permissions %04o allow group or other access", path, info.Mode().Perm())
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	raw, readErr := readBounded(file, maxBytes)
	if closeErr := file.Close(); closeErr != nil {
		return nil, errors.Join(readErr, fmt.Errorf("close secure snapshot file: %w", closeErr))
	}
	return raw, readErr
}

func readBounded(reader io.Reader, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		return nil, errors.New("maximum snapshot size must be positive")
	}
	raw, err := io.ReadAll(io.LimitReader(reader, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > maxBytes {
		return nil, fmt.Errorf("snapshot exceeds %d-byte limit", maxBytes)
	}
	return raw, nil
}

func writeAtomic(path string, raw []byte) (resultErr error) {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create snapshot cache directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return fmt.Errorf("create temporary snapshot cache: %w", err)
	}
	temporaryPath := temporary.Name()
	temporaryOpen := true
	defer func() {
		if resultErr != nil {
			if temporaryOpen {
				if err := temporary.Close(); err != nil {
					resultErr = errors.Join(resultErr, fmt.Errorf("close failed temporary snapshot cache: %w", err))
				}
			}
			if err := os.Remove(temporaryPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				resultErr = errors.Join(resultErr, fmt.Errorf("remove failed temporary snapshot cache: %w", err))
			}
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("secure temporary snapshot cache: %w", err)
	}
	if _, err := temporary.Write(raw); err != nil {
		return fmt.Errorf("write temporary snapshot cache: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync temporary snapshot cache: %w", err)
	}
	temporaryOpen = false
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary snapshot cache: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace snapshot cache: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("secure snapshot cache: %w", err)
	}
	directoryHandle, err := os.Open(directory)
	if err != nil {
		return fmt.Errorf("open snapshot cache directory: %w", err)
	}
	if err := directoryHandle.Sync(); err != nil {
		syncErr := fmt.Errorf("sync snapshot cache directory: %w", err)
		if closeErr := directoryHandle.Close(); closeErr != nil {
			return errors.Join(syncErr, fmt.Errorf("close snapshot cache directory: %w", closeErr))
		}
		return syncErr
	}
	if err := directoryHandle.Close(); err != nil {
		return fmt.Errorf("close snapshot cache directory: %w", err)
	}
	return nil
}
