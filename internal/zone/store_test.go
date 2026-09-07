package zone_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/castlemilk/dns/internal/telemetry"
	"github.com/castlemilk/dns/internal/zone"
	"github.com/miekg/dns"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestStoreZoneCRUD(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_800_000_000, 0).UTC()
	store := openTestStore(t, &now, []string{" NS1.Example.NET ", "ns2.example.net."})
	ctx := context.Background()

	created, err := store.Create(ctx, " Example.COM. ")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.ID == "" {
		t.Error("created zone has empty ID")
	}
	if created.Name != "example.com" {
		t.Errorf("Name = %q, want example.com", created.Name)
	}
	if want := uint32(now.Unix()); created.Serial != want {
		t.Errorf("Serial = %d, want %d", created.Serial, want)
	}
	if !created.CreatedAt.Equal(now) || !created.UpdatedAt.Equal(now) {
		t.Errorf("timestamps = (%s, %s), want %s", created.CreatedAt, created.UpdatedAt, now)
	}
	if want := []string{"ns1.example.net.", "ns2.example.net."}; !slices.Equal(created.Nameservers, want) {
		t.Errorf("Nameservers = %v, want %v", created.Nameservers, want)
	}
	assertManagedApex(t, created, 2)

	if _, err := store.Create(ctx, "EXAMPLE.COM"); !errors.Is(err, zone.ErrAlreadyExists) {
		t.Fatalf("duplicate Create error = %v, want ErrAlreadyExists", err)
	}
	if _, err := store.Create(ctx, "zeta.test"); err != nil {
		t.Fatalf("Create zeta.test: %v", err)
	}
	if _, err := store.Create(ctx, "alpha.test"); err != nil {
		t.Fatalf("Create alpha.test: %v", err)
	}

	listed, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	gotNames := make([]string, 0, len(listed))
	for _, value := range listed {
		gotNames = append(gotNames, value.Name)
	}
	if want := []string{"alpha.test", "example.com", "zeta.test"}; !slices.Equal(gotNames, want) {
		t.Errorf("List names = %v, want %v", gotNames, want)
	}

	got, err := store.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !reflect.DeepEqual(got, created) {
		t.Errorf("Get mismatch\n got: %#v\nwant: %#v", got, created)
	}

	if err := store.Delete(ctx, created.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Get(ctx, created.ID); !errors.Is(err, zone.ErrNotFound) {
		t.Errorf("Get deleted error = %v, want ErrNotFound", err)
	}
	if err := store.Delete(ctx, created.ID); !errors.Is(err, zone.ErrNotFound) {
		t.Errorf("second Delete error = %v, want ErrNotFound", err)
	}
}

func TestStoreMetricsRecordFailedWriteAndAdmission(t *testing.T) {
	t.Parallel()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() {
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown meter provider: %v", err)
		}
	})
	metrics, err := telemetry.NewMetrics(provider.Meter("store-test"))
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	reject := false
	store, err := zone.Open(
		filepath.Join(t.TempDir(), "zones.db"),
		[]string{"ns1.provider.example"},
		zone.WithMetrics(metrics),
		zone.WithSnapshotAdmission(func([]zone.Zone) error {
			if reject {
				return errors.New("test admission rejection")
			}
			return nil
		}),
	)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	reject = true
	if _, err := store.Create(context.Background(), "rejected.test"); err == nil {
		t.Fatal("Create succeeded despite admission rejection")
	}

	var collected metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &collected); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if !hasStorePoint(collected, "deephost.store.transactions", map[string]string{
		"db.operation":        "create_zone",
		"db.transaction.type": "write",
		"outcome":             "error",
	}) {
		t.Fatal("failed create transaction metric is absent")
	}
	if !hasStorePoint(collected, "deephost.snapshot.admissions", map[string]string{"outcome": "error"}) {
		t.Fatal("failed snapshot admission metric is absent")
	}
}

func hasStorePoint(collected metricdata.ResourceMetrics, name string, want map[string]string) bool {
	for _, scope := range collected.ScopeMetrics {
		for _, metricValue := range scope.Metrics {
			if metricValue.Name != name {
				continue
			}
			sum, ok := metricValue.Data.(metricdata.Sum[int64])
			if !ok {
				continue
			}
			for _, point := range sum.DataPoints {
				matched := true
				for key, wantValue := range want {
					value, exists := point.Attributes.Value(attribute.Key(key))
					if !exists || value.AsString() != wantValue {
						matched = false
						break
					}
				}
				if matched {
					return true
				}
			}
		}
	}
	return false
}

func TestStoreRecordCRUDUpdatesSerialAndSOA(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_800_100_000, 0).UTC()
	store := openTestStore(t, &now, []string{"ns1.example.com"})
	ctx := context.Background()
	value, err := store.Create(ctx, "example.com")
	if err != nil {
		t.Fatalf("Create zone: %v", err)
	}
	initialSerial := value.Serial

	first := mustNormalizeRecord(t, value.Name, "www", zone.TypeA, 60, "192.0.2.10")
	value, err = store.CreateRecord(ctx, value.ID, first)
	if err != nil {
		t.Fatalf("CreateRecord: %v", err)
	}
	if value.Serial != initialSerial+1 {
		t.Errorf("serial after same-second create = %d, want %d", value.Serial, initialSerial+1)
	}
	created := findRecord(t, value, "www", zone.TypeA, "192.0.2.10")
	if created.ID == "" {
		t.Error("created record has empty ID")
	}
	if !created.CreatedAt.Equal(now) || !created.UpdatedAt.Equal(now) {
		t.Errorf("created record timestamps = (%s, %s), want %s", created.CreatedAt, created.UpdatedAt, now)
	}
	assertSOASerial(t, value, value.Serial)

	now = now.Add(10 * time.Second)
	updatedInput := mustNormalizeRecord(t, value.Name, "www", zone.TypeA, 120, "192.0.2.11")
	value, err = store.UpdateRecord(ctx, value.ID, created.ID, updatedInput)
	if err != nil {
		t.Fatalf("UpdateRecord: %v", err)
	}
	if want := uint32(now.Unix()); value.Serial != want {
		t.Errorf("serial after later update = %d, want %d", value.Serial, want)
	}
	updated := findRecord(t, value, "www", zone.TypeA, "192.0.2.11")
	if updated.ID != created.ID {
		t.Errorf("updated ID = %q, want %q", updated.ID, created.ID)
	}
	if !updated.CreatedAt.Equal(created.CreatedAt) {
		t.Errorf("updated CreatedAt = %s, want %s", updated.CreatedAt, created.CreatedAt)
	}
	if !updated.UpdatedAt.Equal(now) {
		t.Errorf("updated UpdatedAt = %s, want %s", updated.UpdatedAt, now)
	}
	if updated.TTL != 120 {
		t.Errorf("updated TTL = %d, want 120", updated.TTL)
	}
	assertSOASerial(t, value, value.Serial)

	value, err = store.DeleteRecord(ctx, value.ID, updated.ID)
	if err != nil {
		t.Fatalf("DeleteRecord: %v", err)
	}
	if want := uint32(now.Unix()) + 1; value.Serial != want {
		t.Errorf("serial after same-second delete = %d, want %d", value.Serial, want)
	}
	for _, record := range value.Records {
		if record.ID == updated.ID {
			t.Errorf("deleted record %q remains in zone", updated.ID)
		}
	}
	assertSOASerial(t, value, value.Serial)

	persisted, err := store.Get(ctx, value.ID)
	if err != nil {
		t.Fatalf("Get persisted zone: %v", err)
	}
	if !reflect.DeepEqual(persisted, value) {
		t.Errorf("persisted zone differs after record CRUD")
	}
}

func TestStoreRRSetAndCNAMEInvariants(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		seedType  zone.RecordType
		seedValue string
		seedTTL   uint32
		newType   zone.RecordType
		newValue  string
		newTTL    uint32
		wantError error
		wantField string
	}{
		{
			name: "distinct values in one RRset", seedType: zone.TypeA, seedValue: "192.0.2.1", seedTTL: 60,
			newType: zone.TypeA, newValue: "192.0.2.2", newTTL: 60,
		},
		{
			name: "duplicate value", seedType: zone.TypeA, seedValue: "192.0.2.1", seedTTL: 60,
			newType: zone.TypeA, newValue: "192.0.2.1", newTTL: 60, wantError: zone.ErrAlreadyExists,
		},
		{
			name: "mixed TTL in RRset", seedType: zone.TypeA, seedValue: "192.0.2.1", seedTTL: 60,
			newType: zone.TypeA, newValue: "192.0.2.2", newTTL: 61, wantField: "ttl",
		},
		{
			name: "CNAME added beside address", seedType: zone.TypeA, seedValue: "192.0.2.1", seedTTL: 60,
			newType: zone.TypeCNAME, newValue: "target", newTTL: 60, wantField: "type",
		},
		{
			name: "address added beside CNAME", seedType: zone.TypeCNAME, seedValue: "target", seedTTL: 60,
			newType: zone.TypeA, newValue: "192.0.2.1", newTTL: 60, wantField: "type",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			now := time.Unix(1_800_200_000, 0).UTC()
			store := openTestStore(t, &now, []string{"ns1.example.com"})
			ctx := context.Background()
			value, err := store.Create(ctx, "example.com")
			if err != nil {
				t.Fatalf("Create zone: %v", err)
			}
			seed := mustNormalizeRecord(t, value.Name, "www", tt.seedType, tt.seedTTL, tt.seedValue)
			value, err = store.CreateRecord(ctx, value.ID, seed)
			if err != nil {
				t.Fatalf("seed CreateRecord: %v", err)
			}
			before := value

			candidate := mustNormalizeRecord(t, value.Name, "www", tt.newType, tt.newTTL, tt.newValue)
			got, err := store.CreateRecord(ctx, value.ID, candidate)
			switch {
			case tt.wantError != nil:
				if !errors.Is(err, tt.wantError) {
					t.Fatalf("CreateRecord error = %v, want %v", err, tt.wantError)
				}
			case tt.wantField != "":
				assertValidationField(t, err, tt.wantField)
			default:
				if err != nil {
					t.Fatalf("CreateRecord: %v", err)
				}
				if countRecords(got, "www", zone.TypeA) != 2 {
					t.Errorf("A RRset size = %d, want 2", countRecords(got, "www", zone.TypeA))
				}
				return
			}

			after, getErr := store.Get(ctx, value.ID)
			if getErr != nil {
				t.Fatalf("Get after rejected mutation: %v", getErr)
			}
			if !reflect.DeepEqual(after, before) {
				t.Error("rejected record mutation changed the stored zone")
			}
		})
	}
}

func TestStoreUpdateHonorsCNAMEInvariantAndSkipRecord(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_800_300_000, 0).UTC()
	store := openTestStore(t, &now, []string{"ns1.example.com"})
	ctx := context.Background()
	value, err := store.Create(ctx, "example.com")
	if err != nil {
		t.Fatalf("Create zone: %v", err)
	}
	for _, address := range []string{"192.0.2.1", "192.0.2.2"} {
		record := mustNormalizeRecord(t, value.Name, "www", zone.TypeA, 60, address)
		value, err = store.CreateRecord(ctx, value.ID, record)
		if err != nil {
			t.Fatalf("CreateRecord(%s): %v", address, err)
		}
	}
	first := findRecord(t, value, "www", zone.TypeA, "192.0.2.1")
	cname := mustNormalizeRecord(t, value.Name, "www", zone.TypeCNAME, 60, "target")
	if _, err := store.UpdateRecord(ctx, value.ID, first.ID, cname); err == nil {
		t.Fatal("UpdateRecord to CNAME with another A record succeeded, want validation error")
	} else {
		assertValidationField(t, err, "type")
	}

	lone := mustNormalizeRecord(t, value.Name, "solo", zone.TypeA, 60, "192.0.2.3")
	value, err = store.CreateRecord(ctx, value.ID, lone)
	if err != nil {
		t.Fatalf("CreateRecord(solo): %v", err)
	}
	loneStored := findRecord(t, value, "solo", zone.TypeA, "192.0.2.3")
	loneCNAME := mustNormalizeRecord(t, value.Name, "solo", zone.TypeCNAME, 60, "target")
	value, err = store.UpdateRecord(ctx, value.ID, loneStored.ID, loneCNAME)
	if err != nil {
		t.Fatalf("UpdateRecord lone A to CNAME: %v", err)
	}
	findRecord(t, value, "solo", zone.TypeCNAME, "target.example.com.")
}

func TestStoreProtectsManagedRecords(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_800_400_000, 0).UTC()
	store := openTestStore(t, &now, []string{"ns1.example.com"})
	ctx := context.Background()
	value, err := store.Create(ctx, "example.com")
	if err != nil {
		t.Fatalf("Create zone: %v", err)
	}
	soa := findRecord(t, value, "@", zone.TypeSOA, "")
	before := value

	if _, err := store.DeleteRecord(ctx, value.ID, soa.ID); !errors.Is(err, zone.ErrManagedRecord) {
		t.Errorf("DeleteRecord managed SOA error = %v, want ErrManagedRecord", err)
	}
	replacement := mustNormalizeRecord(t, value.Name, "replacement", zone.TypeA, 60, "192.0.2.1")
	if _, err := store.UpdateRecord(ctx, value.ID, soa.ID, replacement); !errors.Is(err, zone.ErrManagedRecord) {
		t.Errorf("UpdateRecord managed SOA error = %v, want ErrManagedRecord", err)
	}

	after, err := store.Get(ctx, value.ID)
	if err != nil {
		t.Fatalf("Get after managed-record attempts: %v", err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Error("managed-record mutation attempt changed the stored zone")
	}
}

func TestStoreLimitsRRSetCardinality(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_800_500_000, 0).UTC()
	store := openTestStore(t, &now, []string{"ns1.example.com"})
	ctx := context.Background()
	value, err := store.Create(ctx, "example.com")
	if err != nil {
		t.Fatalf("Create zone: %v", err)
	}

	for index := 0; index < zone.MaxRRSetRecords; index++ {
		record := mustNormalizeRecord(t, value.Name, "bulk", zone.TypeTXT, 60, fmt.Sprintf("value-%03d", index))
		value, err = store.CreateRecord(ctx, value.ID, record)
		if err != nil {
			t.Fatalf("CreateRecord(%d): %v", index, err)
		}
	}

	extra := mustNormalizeRecord(t, value.Name, "bulk", zone.TypeTXT, 60, "one-too-many")
	if _, err := store.CreateRecord(ctx, value.ID, extra); err == nil {
		t.Fatal("CreateRecord beyond RRset limit succeeded")
	} else {
		assertValidationField(t, err, "value")
	}
}

func TestStoreImportZoneDryRunCreateAndReplace(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_600_000, 0).UTC()
	store := openTestStore(t, &now, []string{"ns1.provider.example"})
	ctx := context.Background()
	records := []zone.Record{
		mustNormalizeImportedRecord(t, "example.com", "www", zone.TypeA, 0, "192.0.2.10"),
		mustNormalizeImportedRecord(t, "example.com", "@", zone.TypeMX, 300, "10 mail.example.com."),
	}
	dryRun, err := store.ImportZone(ctx, "Example.COM.", records, zone.ImportCreate, true)
	if err != nil {
		t.Fatalf("dry-run ImportZone: %v", err)
	}
	if dryRun.ID == "" || dryRun.Name != "example.com" || len(dryRun.Records) != 4 {
		t.Fatalf("dry-run zone = %#v", dryRun)
	}
	if listed, err := store.List(ctx); err != nil || len(listed) != 0 {
		t.Fatalf("list after dry-run = %#v, %v", listed, err)
	}

	created, err := store.ImportZone(ctx, "example.com", records, zone.ImportCreate, false)
	if err != nil {
		t.Fatalf("create ImportZone: %v", err)
	}
	if findRecord(t, created, "www", zone.TypeA, "192.0.2.10").TTL != 0 {
		t.Fatal("import did not preserve explicit zero TTL")
	}
	if _, err := store.ImportZone(ctx, "example.com", records, zone.ImportCreate, false); !errors.Is(err, zone.ErrAlreadyExists) {
		t.Fatalf("duplicate create error = %v", err)
	}

	now = now.Add(time.Second)
	replacement := []zone.Record{mustNormalizeImportedRecord(t, "example.com", "api", zone.TypeAAAA, 120, "2001:db8::1")}
	replaced, err := store.ImportZone(ctx, "example.com", replacement, zone.ImportReplace, false)
	if err != nil {
		t.Fatalf("replace ImportZone: %v", err)
	}
	if replaced.ID != created.ID || !replaced.CreatedAt.Equal(created.CreatedAt) || replaced.Serial <= created.Serial {
		t.Fatalf("replace identity/serial = %#v, created %#v", replaced, created)
	}
	if countRecords(replaced, "www", zone.TypeA) != 0 || countRecords(replaced, "api", zone.TypeAAAA) != 1 {
		t.Fatalf("replace records = %#v", replaced.Records)
	}
	if _, err := store.ImportZone(ctx, "missing.example", nil, zone.ImportReplace, false); !errors.Is(err, zone.ErrNotFound) {
		t.Fatalf("missing replace error = %v", err)
	}
}

func TestStoreImportZoneRejectsWholeInvalidBatch(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_700_000, 0).UTC()
	store := openTestStore(t, &now, []string{"ns1.provider.example"})
	record := mustNormalizeImportedRecord(t, "example.com", "www", zone.TypeA, 300, "192.0.2.10")
	if _, err := store.ImportZone(context.Background(), "example.com", []zone.Record{record, record}, zone.ImportCreate, false); !errors.Is(err, zone.ErrAlreadyExists) {
		t.Fatalf("invalid batch error = %v", err)
	}
	listed, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listed) != 0 {
		t.Fatalf("invalid batch partially created %#v", listed)
	}
}

func TestRestoreSnapshotIsAtomicAndRequiresEmptyStore(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_800_000, 0).UTC()
	source := openTestStore(t, &now, []string{"ns1.provider.example"})
	first, err := source.Create(context.Background(), "one.example")
	if err != nil {
		t.Fatalf("Create first: %v", err)
	}
	if _, err := source.CreateRecord(context.Background(), first.ID, mustNormalizeRecord(t, first.Name, "www", zone.TypeA, 300, "192.0.2.1")); err != nil {
		t.Fatalf("CreateRecord: %v", err)
	}
	if _, err := source.Create(context.Background(), "two.example"); err != nil {
		t.Fatalf("Create second: %v", err)
	}
	values, err := source.List(context.Background())
	if err != nil {
		t.Fatalf("source List: %v", err)
	}

	targetPath := filepath.Join(t.TempDir(), "restore", "zones.db")
	target, err := zone.OpenEmptyForRestore(targetPath)
	if err != nil {
		t.Fatalf("OpenEmptyForRestore: %v", err)
	}
	t.Cleanup(func() {
		if err := target.Close(); err != nil {
			t.Errorf("close restored target: %v", err)
		}
	})
	if err := target.RestoreSnapshot(context.Background(), values); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}
	restored, err := target.List(context.Background())
	if err != nil {
		t.Fatalf("restored List: %v", err)
	}
	if !reflect.DeepEqual(restored, values) {
		t.Fatalf("restored snapshot differs\n got: %#v\nwant: %#v", restored, values)
	}
	if err := target.RestoreSnapshot(context.Background(), values); !errors.Is(err, zone.ErrStoreNotEmpty) {
		t.Fatalf("second restore error = %v", err)
	}
	info, err := os.Stat(targetPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("restore permissions = %v/%04o", err, info.Mode().Perm())
	}

	bad := slices.Clone(values)
	bad[1] = values[1]
	bad[1].ID = values[0].ID
	empty, err := zone.OpenEmptyForRestore(filepath.Join(t.TempDir(), "empty.db"))
	if err != nil {
		t.Fatalf("open second target: %v", err)
	}
	t.Cleanup(func() {
		if err := empty.Close(); err != nil {
			t.Errorf("close invalid restore target: %v", err)
		}
	})
	if err := empty.RestoreSnapshot(context.Background(), bad); err == nil {
		t.Fatal("invalid snapshot restored")
	}
	if listed, err := empty.List(context.Background()); err != nil || len(listed) != 0 {
		t.Fatalf("invalid restore partially wrote %#v, %v", listed, err)
	}
}

func TestOpenEmptyForRestoreRejectsSymlinkAndNonemptyDatabase(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	target := filepath.Join(directory, "target.db")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	link := filepath.Join(directory, "link.db")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := zone.OpenEmptyForRestore(link); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("symlink error = %v", err)
	}

	nonemptyPath := filepath.Join(directory, "nonempty.db")
	nonempty, err := zone.Open(nonemptyPath, []string{"ns1.provider.example"})
	if err != nil {
		t.Fatalf("open nonempty database: %v", err)
	}
	if _, err := nonempty.Create(context.Background(), "occupied.example"); err != nil {
		t.Fatalf("create occupied zone: %v", err)
	}
	if err := nonempty.Close(); err != nil {
		t.Fatalf("close nonempty database: %v", err)
	}
	if _, err := zone.OpenEmptyForRestore(nonemptyPath); !errors.Is(err, zone.ErrStoreNotEmpty) {
		t.Fatalf("nonempty database error = %v", err)
	}

	garbage := filepath.Join(directory, "not-bbolt.db")
	if err := os.WriteFile(garbage, []byte("not a database"), 0o600); err != nil {
		t.Fatalf("write garbage: %v", err)
	}
	if _, err := zone.OpenEmptyForRestore(garbage); err == nil || !strings.Contains(err.Error(), "bbolt") {
		t.Fatalf("garbage error = %v", err)
	}
}

func TestBumpSnapshotSerialsUsesRFC1982WrapAndKeepsManagedSOAConsistent(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_900_000, 0).UTC()
	store := openTestStore(t, &now, []string{"ns1.provider.example"})
	value, err := store.Create(context.Background(), "wrap.example")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	value.Serial = ^uint32(0)
	for index := range value.Records {
		if value.Records[index].Type == zone.TypeSOA {
			value.Records[index].Value = "ns1.provider.example. hostmaster.wrap.example. 4294967295 3600 600 1209600 300"
		}
	}
	bumped, err := zone.BumpSnapshotSerials([]zone.Zone{value}, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("BumpSnapshotSerials: %v", err)
	}
	if bumped[0].Serial != 0 {
		t.Fatalf("wrapped serial = %d, want 0", bumped[0].Serial)
	}
	if value.Serial != ^uint32(0) {
		t.Fatal("serial bump mutated the source snapshot")
	}
	soa := findRecord(t, bumped[0], "@", zone.TypeSOA, "")
	rr, err := zone.Compile(bumped[0].Name, soa)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	compiledSOA, ok := rr.(*dns.SOA)
	if !ok {
		t.Fatalf("Compile returned %T, want *dns.SOA", rr)
	}
	if compiledSOA.Serial != 0 || !soa.UpdatedAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("bumped SOA = %#v", soa)
	}
}

func TestSnapshotAdmissionRollsBackEveryTentativeMutation(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_801_000_000, 0).UTC()
	admit := func(values []zone.Zone) error {
		records := 0
		for _, value := range values {
			records += len(value.Records)
			for _, record := range value.Records {
				if record.Value == "192.0.2.99" {
					return &zone.ValidationError{Field: "zones", Message: "aggregate snapshot is too large"}
				}
			}
		}
		if records > 3 {
			return &zone.ValidationError{Field: "zones", Message: "aggregate snapshot is too large"}
		}
		return nil
	}
	store, err := zone.Open(
		filepath.Join(t.TempDir(), "admitted.db"), []string{"ns1.provider.example"},
		zone.WithClock(func() time.Time { return now }), zone.WithSnapshotAdmission(admit),
	)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close admitted store: %v", err)
		}
	})
	value, err := store.Create(context.Background(), "admitted.example")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	value, err = store.CreateRecord(context.Background(), value.ID, mustNormalizeRecord(t, value.Name, "one", zone.TypeA, 300, "192.0.2.1"))
	if err != nil {
		t.Fatalf("first CreateRecord: %v", err)
	}
	before := value
	userRecord := findRecord(t, value, "one", zone.TypeA, "192.0.2.1")
	if _, err := store.UpdateRecord(context.Background(), value.ID, userRecord.ID, mustNormalizeRecord(t, value.Name, "one", zone.TypeA, 300, "192.0.2.99")); err == nil || !strings.Contains(err.Error(), "snapshot admission") {
		t.Fatalf("oversized UpdateRecord error = %v", err)
	}
	after, err := store.Get(context.Background(), value.ID)
	if err != nil || !reflect.DeepEqual(after, before) {
		t.Fatalf("failed update changed state: %#v/%v", after, err)
	}
	if _, err := store.CreateRecord(context.Background(), value.ID, mustNormalizeRecord(t, value.Name, "two", zone.TypeA, 300, "192.0.2.2")); err == nil || !strings.Contains(err.Error(), "snapshot admission") {
		t.Fatalf("oversized CreateRecord error = %v", err)
	}
	after, err = store.Get(context.Background(), value.ID)
	if err != nil || !reflect.DeepEqual(after, before) {
		t.Fatalf("failed mutation changed state: %#v/%v", after, err)
	}
	if _, err := store.Create(context.Background(), "second.example"); err == nil || !strings.Contains(err.Error(), "snapshot admission") {
		t.Fatalf("oversized Create error = %v", err)
	}
	replacement := []zone.Record{
		mustNormalizeImportedRecord(t, value.Name, "one", zone.TypeA, 300, "192.0.2.1"),
		mustNormalizeImportedRecord(t, value.Name, "two", zone.TypeA, 300, "192.0.2.2"),
	}
	if _, err := store.ImportZone(context.Background(), value.Name, replacement, zone.ImportReplace, true); err == nil || !strings.Contains(err.Error(), "snapshot admission") {
		t.Fatalf("oversized dry-run import error = %v", err)
	}
	if _, err := store.ImportZone(context.Background(), value.Name, replacement, zone.ImportReplace, false); err == nil || !strings.Contains(err.Error(), "snapshot admission") {
		t.Fatalf("oversized import error = %v", err)
	}
	after, err = store.Get(context.Background(), value.ID)
	if err != nil || !reflect.DeepEqual(after, before) {
		t.Fatalf("failed import changed state: %#v/%v", after, err)
	}
}

func TestSnapshotAdmissionRejectsExistingDatabaseAtOpen(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "existing.db")
	store, err := zone.Open(path, []string{"ns1.provider.example"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(context.Background(), "existing.example"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = zone.Open(path, []string{"ns1.provider.example"}, zone.WithSnapshotAdmission(func(values []zone.Zone) error {
		if len(values) > 0 {
			return errors.New("existing snapshot exceeds limit")
		}
		return nil
	}))
	if err == nil || !strings.Contains(err.Error(), "validate existing zone database") {
		t.Fatalf("reopen error = %v", err)
	}
}

func openTestStore(t *testing.T, now *time.Time, nameservers []string) *zone.Store {
	t.Helper()
	store, err := zone.Open(
		filepath.Join(t.TempDir(), "zones.db"),
		nameservers,
		zone.WithClock(func() time.Time { return *now }),
	)
	if err != nil {
		t.Fatalf("zone.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return store
}

func mustNormalizeRecord(
	t *testing.T,
	zoneName, name string,
	recordType zone.RecordType,
	ttl uint32,
	value string,
) zone.Record {
	t.Helper()
	record, err := zone.NormalizeRecord(zoneName, name, recordType, ttl, value)
	if err != nil {
		t.Fatalf("NormalizeRecord(%q, %q, %q): %v", name, recordType, value, err)
	}
	return record
}

func mustNormalizeImportedRecord(t *testing.T, zoneName, name string, recordType zone.RecordType, ttl uint32, value string) zone.Record {
	t.Helper()
	record, err := zone.NormalizeImportedRecord(zoneName, name, recordType, ttl, value)
	if err != nil {
		t.Fatalf("NormalizeImportedRecord(%q, %q, %q): %v", name, recordType, value, err)
	}
	return record
}

func findRecord(t *testing.T, value zone.Zone, name string, recordType zone.RecordType, recordValue string) zone.Record {
	t.Helper()
	for _, record := range value.Records {
		if record.Name == name && record.Type == recordType && (recordValue == "" || record.Value == recordValue) {
			return record
		}
	}
	t.Fatalf("record %s %s %q not found in %#v", name, recordType, recordValue, value.Records)
	return zone.Record{}
}

func countRecords(value zone.Zone, name string, recordType zone.RecordType) int {
	count := 0
	for _, record := range value.Records {
		if record.Name == name && record.Type == recordType {
			count++
		}
	}
	return count
}

func assertManagedApex(t *testing.T, value zone.Zone, wantNS int) {
	t.Helper()
	soaCount := 0
	nsCount := 0
	for _, record := range value.Records {
		if record.Name != "@" || !record.Managed {
			t.Errorf("initial record = %#v, want managed apex record", record)
		}
		switch record.Type {
		case zone.TypeSOA:
			soaCount++
		case zone.TypeNS:
			nsCount++
		default:
			t.Errorf("unexpected initial record type %s", record.Type)
		}
	}
	if soaCount != 1 || nsCount != wantNS {
		t.Errorf("managed apex counts = SOA:%d NS:%d, want SOA:1 NS:%d", soaCount, nsCount, wantNS)
	}
	assertSOASerial(t, value, value.Serial)
}

func assertSOASerial(t *testing.T, value zone.Zone, want uint32) {
	t.Helper()
	record := findRecord(t, value, "@", zone.TypeSOA, "")
	rr, err := zone.Compile(value.Name, record)
	if err != nil {
		t.Fatalf("Compile SOA: %v", err)
	}
	soa, ok := rr.(*dns.SOA)
	if !ok {
		t.Fatalf("compiled SOA has type %T", rr)
	}
	if soa.Serial != want {
		t.Errorf("SOA serial = %d, want %d", soa.Serial, want)
	}
}
