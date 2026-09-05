package snapshot

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/castlemilk/dns/internal/zone"
)

// phase1Fixture is the shape the authoritative servers already accept: no
// provenance key anywhere. Their decoder uses DisallowUnknownFields, so a feed
// that grew a "source" key would be rejected by every server that has not been
// upgraded yet.
func phase1Fixture(t *testing.T) []zone.Zone {
	t.Helper()
	created := time.Date(2026, time.September, 1, 10, 0, 0, 0, time.UTC)
	value := zone.Zone{
		ID:          "0123456789abcdef0123456789abcdef",
		Name:        "acme.dev",
		Serial:      1_800_000_000,
		Nameservers: []string{"ns1.provider.example."},
		CreatedAt:   created,
		UpdatedAt:   created,
	}
	value.Records = []zone.Record{
		{
			ID: "aaaa0000000000000000000000000001", Name: "@", Type: zone.TypeSOA, TTL: zone.DefaultSOATTL,
			Value:     "ns1.provider.example. hostmaster.acme.dev. 1800000000 3600 600 1209600 300",
			Managed:   true,
			CreatedAt: created, UpdatedAt: created,
		},
		{
			ID: "aaaa0000000000000000000000000002", Name: "@", Type: zone.TypeNS, TTL: zone.DefaultSOATTL,
			Value: "ns1.provider.example.", Managed: true, CreatedAt: created, UpdatedAt: created,
		},
		{
			ID: "aaaa0000000000000000000000000003", Name: "@", Type: zone.TypeA, TTL: 300,
			Value: "198.51.100.10", CreatedAt: created, UpdatedAt: created,
		},
		{
			ID: "aaaa0000000000000000000000000004", Name: "www", Type: zone.TypeCNAME, TTL: 300,
			Value: "acme.dev.", CreatedAt: created, UpdatedAt: created,
		},
	}
	if err := zone.ValidateSnapshot([]zone.Zone{value}); err != nil {
		t.Fatalf("the fixture is not a valid zone: %v", err)
	}
	return []zone.Zone{value}
}

func withSources(values []zone.Zone) []zone.Zone {
	sourced := make([]zone.Zone, len(values))
	for zoneIndex, value := range values {
		value.Records = append([]zone.Record(nil), value.Records...)
		for recordIndex := range value.Records {
			switch value.Records[recordIndex].Type {
			case zone.TypeA:
				value.Records[recordIndex].Source = zone.SourceHosting
			case zone.TypeCNAME:
				value.Records[recordIndex].Source = zone.SourceHosting
			}
		}
		sourced[zoneIndex] = value
	}
	return sourced
}

// phase1Golden is the exact feed a phase-1 control plane produced for
// phase1Fixture. It is written out in full, checksum included, because the
// authorities in production were built against these bytes: adding a field to
// zone.Record must not change a single one of them, and a changed checksum
// would make every consumer re-apply a snapshot that did not change.
const phase1Golden = `{"version":1,"generated_at":"2026-09-03T12:00:00Z","zones":[{"id":"0123456789abcdef0123456789abcdef","name":"acme.dev","serial":1800000000,"nameservers":["ns1.provider.example."],"records":[{"id":"aaaa0000000000000000000000000001","name":"@","type":"SOA","ttl":3600,"value":"ns1.provider.example. hostmaster.acme.dev. 1800000000 3600 600 1209600 300","managed":true,"created_at":"2026-09-01T10:00:00Z","updated_at":"2026-09-01T10:00:00Z"},{"id":"aaaa0000000000000000000000000002","name":"@","type":"NS","ttl":3600,"value":"ns1.provider.example.","managed":true,"created_at":"2026-09-01T10:00:00Z","updated_at":"2026-09-01T10:00:00Z"},{"id":"aaaa0000000000000000000000000003","name":"@","type":"A","ttl":300,"value":"198.51.100.10","managed":false,"created_at":"2026-09-01T10:00:00Z","updated_at":"2026-09-01T10:00:00Z"},{"id":"aaaa0000000000000000000000000004","name":"www","type":"CNAME","ttl":300,"value":"acme.dev.","managed":false,"created_at":"2026-09-01T10:00:00Z","updated_at":"2026-09-01T10:00:00Z"}],"created_at":"2026-09-01T10:00:00Z","updated_at":"2026-09-01T10:00:00Z"}],"checksum":"34937d62fbf9631edb06a3b97a6ba73914a5951e137d21b4160c2ea9df3f6466"}`

func TestFeedMatchesThePhase1Golden(t *testing.T) {
	t.Parallel()

	generatedAt := time.Date(2026, time.September, 3, 12, 0, 0, 0, time.UTC)
	raw, document, err := Build(phase1Fixture(t), generatedAt, false)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if string(raw) != phase1Golden {
		t.Errorf("the feed no longer matches the phase-1 golden\n got: %s\nwant: %s", raw, phase1Golden)
	}
	if document.Checksum != "34937d62fbf9631edb06a3b97a6ba73914a5951e137d21b4160c2ea9df3f6466" {
		t.Errorf("checksum = %s", document.Checksum)
	}

	sourced, _, err := Build(withSources(phase1Fixture(t)), generatedAt, false)
	if err != nil {
		t.Fatalf("Build(sourced): %v", err)
	}
	if string(sourced) != phase1Golden {
		t.Errorf("a zone with engine records produced a different feed\n got: %s\nwant: %s", sourced, phase1Golden)
	}
	if _, err := Decode([]byte(phase1Golden)); err != nil {
		t.Errorf("the golden feed does not decode: %v", err)
	}
}

func TestBuildIsByteIdenticalWithAndWithoutProvenance(t *testing.T) {
	t.Parallel()

	generatedAt := time.Date(2026, time.September, 3, 12, 0, 0, 0, time.UTC)
	plain := phase1Fixture(t)
	sourced := withSources(plain)

	plainRaw, plainDocument, err := Build(plain, generatedAt, false)
	if err != nil {
		t.Fatalf("Build(phase-1 fixture): %v", err)
	}
	sourcedRaw, sourcedDocument, err := Build(sourced, generatedAt, false)
	if err != nil {
		t.Fatalf("Build(sourced fixture): %v", err)
	}

	if !bytes.Equal(plainRaw, sourcedRaw) {
		t.Fatalf("the feed changed when records gained provenance:\n phase 1: %s\n sourced: %s", plainRaw, sourcedRaw)
	}
	if plainDocument.Checksum != sourcedDocument.Checksum {
		t.Errorf("checksum changed: %s vs %s", plainDocument.Checksum, sourcedDocument.Checksum)
	}
	if strings.Contains(string(sourcedRaw), `"source"`) {
		t.Errorf("the feed carries a provenance key: %s", sourcedRaw)
	}
}

func TestBuildDoesNotMutateItsInput(t *testing.T) {
	t.Parallel()

	sourced := withSources(phase1Fixture(t))
	if _, _, err := Build(sourced, time.Date(2026, time.September, 3, 12, 0, 0, 0, time.UTC), false); err != nil {
		t.Fatalf("Build: %v", err)
	}
	found := false
	for _, record := range sourced[0].Records {
		if record.Source == zone.SourceHosting {
			found = true
		}
	}
	if !found {
		t.Error("Build cleared provenance on the caller's zones instead of on a copy")
	}
}

func TestDecodeAcceptsAFeedBuiltFromSourcedZones(t *testing.T) {
	t.Parallel()

	raw, _, err := Build(withSources(phase1Fixture(t)), time.Date(2026, time.September, 3, 12, 0, 0, 0, time.UTC), false)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	document, err := Decode(raw)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	for _, value := range document.Zones {
		for _, record := range value.Records {
			if record.Source != zone.SourceUser {
				t.Errorf("decoded record %s carries source %q", record.ID, record.Source)
			}
		}
	}
}

// TestDecodeRejectsAFeedThatCarriesProvenance is the reason Build strips: an
// un-upgraded authority decodes with DisallowUnknownFields, and this is what it
// would do with a feed that leaked the key.
func TestDecodeRejectsAnUnknownRecordField(t *testing.T) {
	t.Parallel()

	raw, _, err := Build(phase1Fixture(t), time.Date(2026, time.September, 3, 12, 0, 0, 0, time.UTC), false)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	zones, ok := document["zones"].([]any)
	if !ok || len(zones) == 0 {
		t.Fatal("the fixture produced no zones")
	}
	first, ok := zones[0].(map[string]any)
	if !ok {
		t.Fatal("the first zone is not an object")
	}
	records, ok := first["records"].([]any)
	if !ok || len(records) == 0 {
		t.Fatal("the first zone has no records")
	}
	record, ok := records[0].(map[string]any)
	if !ok {
		t.Fatal("the first record is not an object")
	}
	record["invented_field"] = "x"
	tampered, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := Decode(tampered); err == nil {
		t.Error("Decode accepted a record field it does not know")
	}
}

func TestFeedHandlerServesTheStrippedDocument(t *testing.T) {
	t.Parallel()

	source := &fixtureLister{values: withSources(phase1Fixture(t))}
	handler := NewFeedHandler(source, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), 1<<20, false)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, Path, nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	if strings.Contains(recorder.Body.String(), `"source"`) {
		t.Errorf("the served feed carries provenance: %s", recorder.Body.String())
	}
	if _, err := Decode(recorder.Body.Bytes()); err != nil {
		t.Errorf("the served feed does not decode: %v", err)
	}
}

type fixtureLister struct {
	values []zone.Zone
}

func (l *fixtureLister) List(context.Context) ([]zone.Zone, error) {
	return l.values, nil
}
