package snapshot

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/castlemilk/dns/internal/authoritative"
	"github.com/castlemilk/dns/internal/zone"
)

const (
	Version = 1
	Path    = "/internal/v1/snapshot"
)

var maximumEncodedTimestamp = time.Date(9999, time.December, 31, 23, 59, 59, 999999999, time.UTC)

type Document struct {
	Version     int         `json:"version"`
	GeneratedAt time.Time   `json:"generated_at"`
	Zones       []zone.Zone `json:"zones"`
	Checksum    string      `json:"checksum"`
}

type checksumPayload struct {
	Version     int         `json:"version"`
	GeneratedAt time.Time   `json:"generated_at"`
	Zones       []zone.Zone `json:"zones"`
}

type ZoneLister interface {
	List(context.Context) ([]zone.Zone, error)
}

type FeedHandler struct {
	source     ZoneLister
	logger     *slog.Logger
	maxBytes   int64
	production bool
	now        func() time.Time

	mu          sync.Mutex
	contentHash [sha256.Size]byte
	hasCached   bool
	cachedRaw   []byte
	cachedETag  string
	cachedAt    time.Time
}

func NewFeedHandler(source ZoneLister, logger *slog.Logger, maxBytes int64, production bool) *FeedHandler {
	if logger == nil {
		logger = slog.Default()
	}
	return &FeedHandler{
		source:     source,
		logger:     logger,
		maxBytes:   maxBytes,
		production: production,
		now:        time.Now,
	}
}

func (h *FeedHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writer.Header().Set("Allow", http.MethodGet)
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	values, err := h.source.List(request.Context())
	if err != nil {
		h.logger.Error("list zones for snapshot", "error", err)
		http.Error(writer, "snapshot unavailable", http.StatusServiceUnavailable)
		return
	}
	raw, etag, err := h.snapshot(values)
	if err != nil {
		h.logger.Error("build zone snapshot", "error", err)
		http.Error(writer, "snapshot unavailable", http.StatusServiceUnavailable)
		return
	}
	if h.maxBytes <= 0 || int64(len(raw)) > h.maxBytes {
		h.logger.Error("zone snapshot exceeds configured limit", "bytes", len(raw), "limit", h.maxBytes)
		http.Error(writer, "snapshot unavailable", http.StatusServiceUnavailable)
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("ETag", etag)
	if matchesETag(request.Header.Get("If-None-Match"), etag) {
		writer.WriteHeader(http.StatusNotModified)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Content-Length", fmt.Sprintf("%d", len(raw)))
	writer.WriteHeader(http.StatusOK)
	if _, err := writer.Write(raw); err != nil {
		h.logger.Debug("write zone snapshot", "error", err)
	}
}

func (h *FeedHandler) snapshot(values []zone.Zone) ([]byte, string, error) {
	content, err := json.Marshal(values)
	if err != nil {
		return nil, "", fmt.Errorf("encode zone snapshot content: %w", err)
	}
	digest := sha256.Sum256(content)

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.hasCached && digest == h.contentHash {
		return h.cachedRaw, h.cachedETag, nil
	}
	generatedAt := h.now().UTC()
	if h.hasCached && !generatedAt.After(h.cachedAt) {
		generatedAt = h.cachedAt.Add(time.Nanosecond)
	}
	raw, document, err := Build(values, generatedAt, h.production)
	if err != nil {
		return nil, "", err
	}
	h.contentHash = digest
	h.hasCached = true
	h.cachedRaw = raw
	h.cachedETag = `"sha256:` + document.Checksum + `"`
	h.cachedAt = document.GeneratedAt
	return h.cachedRaw, h.cachedETag, nil
}

func matchesETag(header, etag string) bool {
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || candidate == etag || strings.TrimPrefix(candidate, "W/") == etag {
			return true
		}
	}
	return false
}

func Build(values []zone.Zone, generatedAt time.Time, production bool) ([]byte, Document, error) {
	if generatedAt.IsZero() {
		return nil, Document{}, errors.New("snapshot generation time is required")
	}
	if production {
		if err := ValidateProductionNameservers(values); err != nil {
			return nil, Document{}, err
		}
	}
	if _, err := authoritative.Compile(values); err != nil {
		return nil, Document{}, fmt.Errorf("validate snapshot zones: %w", err)
	}
	document := Document{
		Version:     Version,
		GeneratedAt: generatedAt.UTC(),
		Zones:       values,
	}
	checksum, err := calculateChecksum(document)
	if err != nil {
		return nil, Document{}, err
	}
	document.Checksum = checksum
	raw, err := json.Marshal(document)
	if err != nil {
		return nil, Document{}, fmt.Errorf("encode snapshot: %w", err)
	}
	return raw, document, nil
}

// ValidateSize uses the longest timestamp representation emitted by the JSON
// encoder, so a state admitted here cannot later exceed maxBytes solely due to
// GeneratedAt formatting.
func ValidateSize(values []zone.Zone, maxBytes int64, production bool) error {
	if maxBytes <= 0 {
		return errors.New("maximum snapshot size must be positive")
	}
	raw, _, err := Build(values, maximumEncodedTimestamp, production)
	if err != nil {
		return err
	}
	if int64(len(raw)) > maxBytes {
		return fmt.Errorf("aggregate snapshot requires %d bytes, exceeding the %d-byte limit", len(raw), maxBytes)
	}
	return nil
}

func Decode(raw []byte) (Document, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var document Document
	if err := decoder.Decode(&document); err != nil {
		return Document{}, fmt.Errorf("decode snapshot: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return Document{}, errors.New("decode snapshot: trailing JSON value")
		}
		return Document{}, fmt.Errorf("decode snapshot trailer: %w", err)
	}
	if document.Version != Version {
		return Document{}, fmt.Errorf("snapshot version %d is unsupported", document.Version)
	}
	if document.GeneratedAt.IsZero() {
		return Document{}, errors.New("snapshot generated_at is required")
	}
	provided, err := hex.DecodeString(document.Checksum)
	if err != nil || len(provided) != sha256.Size {
		return Document{}, errors.New("snapshot checksum must be a 64-character SHA-256 hex value")
	}
	want, err := calculateChecksum(document)
	if err != nil {
		return Document{}, err
	}
	expected, err := hex.DecodeString(want)
	if err != nil {
		return Document{}, fmt.Errorf("decode calculated snapshot checksum: %w", err)
	}
	if subtle.ConstantTimeCompare(provided, expected) != 1 {
		return Document{}, errors.New("snapshot checksum does not match its contents")
	}
	return document, nil
}

func ValidateProductionNameservers(values []zone.Zone) error {
	for _, value := range values {
		for _, nameserver := range value.Nameservers {
			if isReservedNameserver(nameserver) {
				return fmt.Errorf("zone %q uses a reserved nameserver", value.Name)
			}
		}
		for _, record := range value.Records {
			if record.Type == zone.TypeNS && isReservedNameserver(record.Value) {
				return fmt.Errorf("zone %q contains a reserved NS name", value.Name)
			}
		}
	}
	return nil
}

func calculateChecksum(document Document) (string, error) {
	payload := checksumPayload{
		Version:     document.Version,
		GeneratedAt: document.GeneratedAt.UTC(),
		Zones:       document.Zones,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode snapshot checksum payload: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func isReservedNameserver(value string) bool {
	value = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(value), "."))
	for _, suffix := range []string{"example.com", "example.net", "example.org", "invalid", "localhost", "test"} {
		if value == suffix || strings.HasSuffix(value, "."+suffix) {
			return true
		}
	}
	return false
}
