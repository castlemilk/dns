package snapshot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/castlemilk/dns/internal/zone"
)

func TestBuildDecodeRoundTrip(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.September, 2, 3, 4, 5, 0, time.UTC)
	values := []zone.Zone{validZone("example.test", "ns1.dns.test.", now)}

	raw, built, err := Build(values, now, false)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	decoded, err := Decode(raw)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if decoded.Version != Version || decoded.Checksum != built.Checksum || !decoded.GeneratedAt.Equal(now) {
		t.Fatalf("decoded document = %#v", decoded)
	}
	if len(decoded.Zones) != 1 || decoded.Zones[0].Name != "example.test" {
		t.Fatalf("decoded zones = %#v", decoded.Zones)
	}
}

func TestDecodeRejectsMalformedDocuments(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.September, 2, 3, 4, 5, 0, time.UTC)
	raw, document, err := Build([]zone.Zone{validZone("example.test", "ns1.dns.test.", now)}, now, false)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	wrongVersion := document
	wrongVersion.Version++
	wrongVersion.Checksum, err = calculateChecksum(wrongVersion)
	if err != nil {
		t.Fatalf("calculate checksum: %v", err)
	}
	wrongVersionRaw, err := json.Marshal(wrongVersion)
	if err != nil {
		t.Fatalf("marshal wrong version: %v", err)
	}

	tests := []struct {
		name string
		raw  []byte
		want string
	}{
		{name: "tampered contents", raw: bytes.Replace(raw, []byte("example.test"), []byte("changed.test"), 1), want: "checksum does not match"},
		{name: "unknown field", raw: bytes.Replace(raw, []byte(`{"version":1,`), []byte(`{"unknown":true,"version":1,`), 1), want: "unknown field"},
		{name: "trailing value", raw: append(append([]byte(nil), raw...), []byte(` {}`)...), want: "trailing JSON value"},
		{name: "unsupported version", raw: wrongVersionRaw, want: "unsupported"},
		{name: "malformed checksum", raw: bytes.Replace(raw, []byte(document.Checksum), []byte("abcd"), 1), want: "64-character"},
		{name: "empty", raw: nil, want: "decode snapshot"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := Decode(tt.raw)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Decode error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestBuildRejectsInvalidOrProductionPlaceholderZones(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.September, 2, 3, 4, 5, 0, time.UTC)

	for _, nameserver := range []string{"ns1.example.com.", "ns1.example.net.", "ns1.example.org.", "ns.invalid.", "localhost.", "ns.test."} {
		if _, _, err := Build([]zone.Zone{validZone("example.test", nameserver, now)}, now, true); err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Fatalf("production reserved nameserver %q error = %v", nameserver, err)
		}
	}
	invalid := validZone("example.test", "ns1.dns.test.", now)
	invalid.Records = invalid.Records[1:]
	if _, _, err := Build([]zone.Zone{invalid}, now, false); err == nil || !strings.Contains(err.Error(), "SOA") {
		t.Fatalf("invalid zone error = %v", err)
	}
	if _, _, err := Build(nil, time.Time{}, false); err == nil || !strings.Contains(err.Error(), "generation time") {
		t.Fatalf("zero time error = %v", err)
	}
}

func TestValidateSizeUsesFailClosedEncodingBound(t *testing.T) {
	t.Parallel()
	values := []zone.Zone{validZone("example.test", "ns1.dns.test.", maximumEncodedTimestamp)}
	raw, _, err := Build(values, maximumEncodedTimestamp, false)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if err := ValidateSize(values, int64(len(raw)), false); err != nil {
		t.Fatalf("ValidateSize at exact bound: %v", err)
	}
	if err := ValidateSize(values, int64(len(raw)-1), false); err == nil || !strings.Contains(err.Error(), "aggregate snapshot") {
		t.Fatalf("ValidateSize below bound error = %v", err)
	}
}

func TestFeedHandler(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.September, 2, 3, 4, 5, 0, time.UTC)
	values := []zone.Zone{validZone("example.test", "ns1.dns.test.", now)}
	handler := NewFeedHandler(staticLister{values: values}, slog.New(slog.NewTextHandler(io.Discard, nil)), 1<<20, false)
	handler.now = func() time.Time { return now }

	request := httptest.NewRequest(http.MethodGet, "http://control"+Path, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", response.Code, response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("ETag") == "" {
		t.Fatalf("headers = %#v", response.Header())
	}
	if _, err := Decode(response.Body.Bytes()); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	t.Run("method", func(t *testing.T) {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "http://control"+Path, nil))
		if response.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d", response.Code)
		}
	})

	t.Run("bounded response", func(t *testing.T) {
		small := NewFeedHandler(staticLister{values: values}, slog.New(slog.NewTextHandler(io.Discard, nil)), 1, false)
		response := httptest.NewRecorder()
		small.ServeHTTP(response, request)
		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d", response.Code)
		}
	})

	t.Run("source error", func(t *testing.T) {
		broken := NewFeedHandler(staticLister{err: errors.New("storage unavailable")}, slog.New(slog.NewTextHandler(io.Discard, nil)), 1<<20, false)
		response := httptest.NewRecorder()
		broken.ServeHTTP(response, request)
		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d", response.Code)
		}
	})
}

func TestFeedHandlerReusesDocumentUntilZoneContentChanges(t *testing.T) {
	t.Parallel()
	generatedAt := time.Date(2026, time.September, 2, 3, 4, 5, 0, time.UTC)
	clock := generatedAt
	values := []zone.Zone{validZone("example.test", "ns1.dns.test.", generatedAt)}
	lister := &mutableLister{values: values}
	handler := NewFeedHandler(lister, discardLogger(), 1<<20, false)
	handler.now = func() time.Time { return clock }

	first := httptest.NewRecorder()
	handler.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "http://control"+Path, nil))
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d", first.Code)
	}
	firstDocument, err := Decode(first.Body.Bytes())
	if err != nil {
		t.Fatalf("decode first: %v", err)
	}
	firstETag := first.Header().Get("ETag")

	secondRequest := httptest.NewRequest(http.MethodGet, "http://control"+Path, nil)
	secondRequest.Header.Set("If-None-Match", firstETag)
	second := httptest.NewRecorder()
	handler.ServeHTTP(second, secondRequest)
	if second.Code != http.StatusNotModified || second.Body.Len() != 0 {
		t.Fatalf("unchanged response = %d/%q", second.Code, second.Body.String())
	}
	if second.Header().Get("ETag") != firstETag {
		t.Fatalf("unchanged ETag = %q, want %q", second.Header().Get("ETag"), firstETag)
	}

	lister.mu.Lock()
	lister.values[0].Records[2].Value = "192.0.2.11"
	lister.mu.Unlock()
	third := httptest.NewRecorder()
	handler.ServeHTTP(third, httptest.NewRequest(http.MethodGet, "http://control"+Path, nil))
	if third.Code != http.StatusOK {
		t.Fatalf("changed status = %d, body = %q", third.Code, third.Body.String())
	}
	thirdDocument, err := Decode(third.Body.Bytes())
	if err != nil {
		t.Fatalf("decode changed: %v", err)
	}
	if !thirdDocument.GeneratedAt.After(firstDocument.GeneratedAt) {
		t.Fatalf("changed generated_at = %s, want after %s", thirdDocument.GeneratedAt, firstDocument.GeneratedAt)
	}
	if third.Header().Get("ETag") == firstETag || thirdDocument.Checksum == firstDocument.Checksum {
		t.Fatal("changed content reused the prior document")
	}
}

type staticLister struct {
	values []zone.Zone
	err    error
}

type mutableLister struct {
	mu     sync.Mutex
	values []zone.Zone
}

func (s *mutableLister) List(context.Context) ([]zone.Zone, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := json.Marshal(s.values)
	if err != nil {
		return nil, err
	}
	var cloned []zone.Zone
	if err := json.Unmarshal(raw, &cloned); err != nil {
		return nil, err
	}
	return cloned, nil
}

func (s staticLister) List(context.Context) ([]zone.Zone, error) {
	return s.values, s.err
}

func validZone(name, nameserver string, now time.Time) zone.Zone {
	serial := uint32(1_700_000_000)
	return zone.Zone{
		ID:          "zone-" + name,
		Name:        name,
		Serial:      serial,
		Nameservers: []string{nameserver},
		CreatedAt:   now,
		UpdatedAt:   now,
		Records: []zone.Record{
			{
				ID:        "soa-" + name,
				Name:      "@",
				Type:      zone.TypeSOA,
				TTL:       3600,
				Value:     nameserver + " hostmaster." + name + ". 1700000000 3600 600 1209600 300",
				Managed:   true,
				CreatedAt: now,
				UpdatedAt: now,
			},
			{
				ID:        "ns-" + name,
				Name:      "@",
				Type:      zone.TypeNS,
				TTL:       3600,
				Value:     nameserver,
				Managed:   true,
				CreatedAt: now,
				UpdatedAt: now,
			},
			{
				ID:        "a-" + name,
				Name:      "www",
				Type:      zone.TypeA,
				TTL:       300,
				Value:     "192.0.2.10",
				CreatedAt: now,
				UpdatedAt: now,
			},
		},
	}
}
