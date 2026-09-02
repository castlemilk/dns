package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/castlemilk/dns/internal/snapshot"
	"github.com/castlemilk/dns/internal/zone"
)

func TestVerifyOnlyValidatesWithoutOpeningDatabase(t *testing.T) {
	t.Parallel()
	raw, _ := snapshotFixture(t)
	var stdout, stderr bytes.Buffer
	if err := run([]string{"--snapshot", "-", "--verify-only"}, bytes.NewReader(raw), &stdout, &stderr); err != nil {
		t.Fatalf("run verify-only: %v", err)
	}
	if !strings.Contains(stdout.String(), `"status":"valid"`) || !strings.Contains(stdout.String(), `"zones":1`) {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRestoreExactSnapshotAndOptionalSerialBump(t *testing.T) {
	t.Parallel()
	raw, values := snapshotFixture(t)
	for _, tt := range []struct {
		name string
		args []string
		bump bool
	}{
		{name: "exact", args: nil},
		{name: "serial bump", args: []string{"--bump-serials"}, bump: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			target := filepath.Join(t.TempDir(), "restored.db")
			args := []string{"--snapshot", "-", "--database", target}
			args = append(args, tt.args...)
			var stdout bytes.Buffer
			recoveryTime := time.Unix(1_900_000_000, 0).UTC()
			if err := runWithClock(args, bytes.NewReader(raw), &stdout, ioDiscard{}, func() time.Time { return recoveryTime }); err != nil {
				t.Fatalf("run restore: %v", err)
			}
			store, err := zone.Open(target, []string{"ns1.provider.example"})
			if err != nil {
				t.Fatalf("open restored store: %v", err)
			}
			restored, listErr := store.List(context.Background())
			closeErr := store.Close()
			if listErr != nil || closeErr != nil {
				t.Fatalf("read restored store: %v / %v", listErr, closeErr)
			}
			if !tt.bump {
				if !reflect.DeepEqual(restored, values) {
					t.Fatalf("exact restore differs\n got: %#v\nwant: %#v", restored, values)
				}
			} else {
				if restored[0].Serial != uint32(recoveryTime.Unix()) || !strings.Contains(stdout.String(), `"serials_bumped":true`) {
					t.Fatalf("bumped restore = serial:%d output:%q", restored[0].Serial, stdout.String())
				}
				soa := restored[0].Records[0]
				for _, record := range restored[0].Records {
					if record.Type == zone.TypeSOA {
						soa = record
					}
				}
				if !strings.Contains(soa.Value, " "+jsonNumber(restored[0].Serial)+" ") {
					t.Fatalf("SOA was not bumped consistently: %q", soa.Value)
				}
			}
		})
	}
}

func TestRestoreRejectsTamperBeforeCreatingTarget(t *testing.T) {
	t.Parallel()
	raw, _ := snapshotFixture(t)
	raw = bytes.Replace(raw, []byte("restore.example"), []byte("changed.example"), 1)
	target := filepath.Join(t.TempDir(), "must-not-exist.db")
	err := run([]string{"--snapshot", "-", "--database", target}, bytes.NewReader(raw), ioDiscard{}, ioDiscard{})
	if err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("tamper error = %v", err)
	}
	if _, statErr := os.Stat(target); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("target stat after tamper = %v", statErr)
	}
}

func TestRestoreRejectsChecksummedInvalidZonesAndUnsafeTargets(t *testing.T) {
	t.Parallel()
	raw, values := snapshotFixture(t)
	document, err := snapshot.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	document.Zones[0].Records[0].ID = ""
	document.Checksum = testChecksum(t, document)
	invalidRaw, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"--snapshot", "-", "--verify-only"}, bytes.NewReader(invalidRaw), ioDiscard{}, ioDiscard{}); err == nil || !strings.Contains(err.Error(), "record without an id") {
		t.Fatalf("invalid zone error = %v", err)
	}

	directory := t.TempDir()
	nonemptyPath := filepath.Join(directory, "nonempty.db")
	nonempty, err := zone.Open(nonemptyPath, []string{"ns1.provider.example"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := nonempty.Create(context.Background(), "existing.example"); err != nil {
		t.Fatal(err)
	}
	if err := nonempty.Close(); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"--snapshot", "-", "--database", nonemptyPath}, bytes.NewReader(raw), ioDiscard{}, ioDiscard{}); !errors.Is(err, zone.ErrStoreNotEmpty) {
		t.Fatalf("nonempty target error = %v", err)
	}
	reopened, err := zone.Open(nonemptyPath, []string{"ns1.provider.example"})
	if err != nil {
		t.Fatal(err)
	}
	listed, err := reopened.List(context.Background())
	closeErr := reopened.Close()
	if err != nil || closeErr != nil || len(listed) != 1 || listed[0].Name != "existing.example" {
		t.Fatalf("nonempty target was changed: %#v/%v/%v", listed, err, closeErr)
	}

	link := filepath.Join(directory, "link.db")
	if err := os.Symlink(nonemptyPath, link); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"--snapshot", "-", "--database", link}, bytes.NewReader(raw), ioDiscard{}, ioDiscard{}); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("symlink target error = %v", err)
	}
	_ = values
}

func TestRestoreUsageAndBounds(t *testing.T) {
	t.Parallel()
	var stderr bytes.Buffer
	if err := run(nil, strings.NewReader(""), ioDiscard{}, &stderr); err == nil || !strings.Contains(stderr.String(), "--verify-only") {
		t.Fatalf("usage error/output = %v/%q", err, stderr.String())
	}
	if _, err := readSnapshotBounded(strings.NewReader("1234"), 3); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("bounded read error = %v", err)
	}
	if err := run([]string{"--snapshot", "-", "--verify-only", "--bump-serials"}, strings.NewReader(""), ioDiscard{}, ioDiscard{}); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("verify/bump error = %v", err)
	}
}

func snapshotFixture(t *testing.T) ([]byte, []zone.Zone) {
	t.Helper()
	directory := t.TempDir()
	store, err := zone.Open(filepath.Join(directory, "source.db"), []string{"ns1.provider.example"}, zone.WithClock(func() time.Time {
		return time.Unix(1_800_000_000, 0).UTC()
	}))
	if err != nil {
		t.Fatalf("zone.Open: %v", err)
	}
	value, err := store.Create(context.Background(), "restore.example")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	record, err := zone.NormalizeRecord(value.Name, "www", zone.TypeA, 300, "192.0.2.10")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateRecord(context.Background(), value.ID, record); err != nil {
		t.Fatal(err)
	}
	values, err := store.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	generatedAt := time.Unix(1_800_000_100, 0).UTC()
	raw, _, err := snapshot.Build(values, generatedAt, false)
	if err != nil {
		t.Fatalf("snapshot.Build: %v", err)
	}
	return raw, values
}

func testChecksum(t *testing.T, document snapshot.Document) string {
	t.Helper()
	payload := struct {
		Version     int         `json:"version"`
		GeneratedAt time.Time   `json:"generated_at"`
		Zones       []zone.Zone `json:"zones"`
	}{document.Version, document.GeneratedAt.UTC(), document.Zones}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal checksum payload: %v", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func jsonNumber(value uint32) string {
	return strconv.FormatUint(uint64(value), 10)
}

type ioDiscard struct{}

func (ioDiscard) Write(value []byte) (int, error) { return len(value), nil }
