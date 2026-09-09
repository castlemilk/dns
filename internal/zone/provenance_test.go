package zone_test

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/castlemilk/dns/internal/zone"
)

func provenanceStore(t *testing.T) (*zone.Store, zone.Zone) {
	t.Helper()
	now := time.Unix(1_800_000_000, 0).UTC()
	store := openTestStore(t, &now, []string{"ns1.provider.example"})
	created, err := store.Create(context.Background(), "acme.dev")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return store, created
}

func engineOp(t *testing.T, zoneName, name string, kind zone.RecordType, ttl uint32, value, source string) zone.Op {
	t.Helper()
	record := mustNormalizeRecord(t, zoneName, name, kind, ttl, value)
	record.Source = source
	return zone.Op{Kind: zone.OpCreate, Record: record}
}

func TestApplyRecordSetWritesEngineRecords(t *testing.T) {
	t.Parallel()

	store, created := provenanceStore(t)
	ctx := context.Background()
	ops := []zone.Op{
		engineOp(t, created.Name, "@", zone.TypeA, 300, "198.51.100.10", zone.SourceHosting),
		engineOp(t, created.Name, "@", zone.TypeA, 300, "198.51.100.11", zone.SourceHosting),
		engineOp(t, created.Name, "www", zone.TypeCNAME, 300, "acme.dev.", zone.SourceHosting),
	}
	updated, ids, err := store.ApplyRecordSet(ctx, created.ID, ops)
	if err != nil {
		t.Fatalf("ApplyRecordSet: %v", err)
	}
	if len(ids) != 3 {
		t.Fatalf("created ids = %v, want one per create op", ids)
	}
	for index := range ops {
		if ids[index] == "" {
			t.Errorf("op %d has no id", index)
		}
	}
	if updated.Serial <= created.Serial {
		t.Errorf("serial = %d, want it bumped past %d", updated.Serial, created.Serial)
	}
	for _, record := range updated.Records {
		if record.Managed {
			continue
		}
		if record.Source != zone.SourceHosting {
			t.Errorf("record %s %s carries source %q", record.Name, record.Type, record.Source)
		}
	}

	// A second, empty set is a no-op that still reads back the zone.
	same, none, err := store.ApplyRecordSet(ctx, created.ID, nil)
	if err != nil {
		t.Fatalf("ApplyRecordSet(nil): %v", err)
	}
	if len(none) != 0 || same.Serial != updated.Serial {
		t.Errorf("an empty set changed the zone: serial %d → %d", updated.Serial, same.Serial)
	}
}

func TestApplyRecordSetBumpsTheSerialOnce(t *testing.T) {
	t.Parallel()

	store, created := provenanceStore(t)
	updated, _, err := store.ApplyRecordSet(context.Background(), created.ID, []zone.Op{
		engineOp(t, created.Name, "@", zone.TypeA, 300, "198.51.100.10", zone.SourceHosting),
		engineOp(t, created.Name, "@", zone.TypeA, 300, "198.51.100.11", zone.SourceHosting),
	})
	if err != nil {
		t.Fatalf("ApplyRecordSet: %v", err)
	}
	soa := findRecord(t, updated, "@", zone.TypeSOA, "")
	if !strings.Contains(soa.Value, " "+strconv.FormatUint(uint64(updated.Serial), 10)+" ") {
		t.Errorf("SOA %q does not carry serial %d", soa.Value, updated.Serial)
	}
}

func TestApplyRecordSetIsAtomic(t *testing.T) {
	t.Parallel()

	store, created := provenanceStore(t)
	ctx := context.Background()
	good := engineOp(t, created.Name, "@", zone.TypeA, 300, "198.51.100.10", zone.SourceHosting)
	bad := zone.Op{Kind: zone.OpDelete, RecordID: "does-not-exist"}

	if _, _, err := store.ApplyRecordSet(ctx, created.ID, []zone.Op{good, bad}); !errors.Is(err, zone.ErrNotFound) {
		t.Fatalf("ApplyRecordSet = %v, want ErrNotFound", err)
	}
	after, err := store.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if countRecords(after, "@", zone.TypeA) != 0 {
		t.Error("a failed set left records behind")
	}
	if after.Serial != created.Serial {
		t.Errorf("serial = %d, want the original %d", after.Serial, created.Serial)
	}
}

func TestApplyRecordSetEnforcesTheProvenanceRules(t *testing.T) {
	t.Parallel()

	store, created := provenanceStore(t)
	ctx := context.Background()
	user, err := store.CreateRecord(ctx, created.ID, mustNormalizeRecord(t, created.Name, "blog", zone.TypeA, 300, "203.0.113.5"))
	if err != nil {
		t.Fatalf("CreateRecord: %v", err)
	}
	userRecord := findRecord(t, user, "blog", zone.TypeA, "203.0.113.5")
	soa := findRecord(t, user, "@", zone.TypeSOA, "")

	unsourced := mustNormalizeRecord(t, created.Name, "@", zone.TypeA, 300, "198.51.100.10")
	rewrite := mustNormalizeRecord(t, created.Name, "blog", zone.TypeA, 300, "203.0.113.9")

	tests := []struct {
		name string
		ops  []zone.Op
		want string
	}{
		{
			name: "a create without a source",
			ops:  []zone.Op{{Kind: zone.OpCreate, Record: unsourced}},
			want: "only create engine-owned records",
		},
		{
			name: "a create with an unknown source",
			ops: []zone.Op{{Kind: zone.OpCreate, Record: func() zone.Record {
				record := unsourced
				record.Source = "website"
				return record
			}()}},
			want: "must be empty, hosting, or mail",
		},
		{
			name: "a create with a zero TTL",
			ops: []zone.Op{{Kind: zone.OpCreate, Record: func() zone.Record {
				record := unsourced
				record.Source = zone.SourceHosting
				record.TTL = 0
				return record
			}()}},
			want: "at least 1 second",
		},
		{
			name: "rewriting a user record",
			ops:  []zone.Op{{Kind: zone.OpUpdate, RecordID: userRecord.ID, Record: rewrite}},
			want: "only change the TTL of a record it does not own",
		},
		{
			name: "touching a managed record",
			ops: []zone.Op{{Kind: zone.OpUpdate, RecordID: soa.ID, Record: func() zone.Record {
				record := unsourced
				record.Source = zone.SourceHosting
				return record
			}()}},
			want: "managed",
		},
		{
			name: "deleting a managed record",
			ops:  []zone.Op{{Kind: zone.OpDelete, RecordID: soa.ID}},
			want: "managed",
		},
		{
			name: "an unknown operation",
			ops:  []zone.Op{{Kind: "replace", RecordID: userRecord.ID}},
			want: "must be create, update, or delete",
		},
		{
			name: "an update without an id",
			ops:  []zone.Op{{Kind: zone.OpUpdate, Record: unsourced}},
			want: "is required",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := store.ApplyRecordSet(ctx, created.ID, test.ops)
			if err == nil {
				t.Fatalf("ApplyRecordSet = nil error, want %q", test.want)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Errorf("ApplyRecordSet = %v, want it to mention %q", err, test.want)
			}
		})
	}
}

func TestApplyRecordSetAlignsAUserRecordsTtlOnly(t *testing.T) {
	t.Parallel()

	store, created := provenanceStore(t)
	ctx := context.Background()
	// An imported TXT with TTL 0 next to which SPF must fit.
	imported, err := store.ImportZone(ctx, "acme.dev", []zone.Record{
		mustNormalizeImportedRecord(t, "acme.dev", "@", zone.TypeTXT, 0, `"MS=ms12345"`),
	}, zone.ImportReplace, false)
	if err != nil {
		t.Fatalf("ImportZone: %v", err)
	}
	verification := findRecord(t, imported, "@", zone.TypeTXT, `"MS=ms12345"`)

	aligned := verification
	aligned.TTL = 3600
	spf := mustNormalizeRecord(t, "acme.dev", "@", zone.TypeTXT, 3600, "v=spf1 mx -all")
	spf.Source = zone.SourceMail

	updated, ids, err := store.ApplyRecordSet(ctx, created.ID, []zone.Op{
		{Kind: zone.OpUpdate, RecordID: verification.ID, Record: aligned},
		{Kind: zone.OpCreate, Record: spf},
	})
	if err != nil {
		t.Fatalf("ApplyRecordSet: %v", err)
	}
	if len(ids) != 1 {
		t.Fatalf("created ids = %v", ids)
	}
	after := findRecord(t, updated, "@", zone.TypeTXT, `"MS=ms12345"`)
	if after.TTL != 3600 {
		t.Errorf("verification TXT TTL = %d, want it aligned to 3600", after.TTL)
	}
	if after.Source != zone.SourceUser {
		t.Errorf("aligning a user record changed its source to %q", after.Source)
	}
	if after.ID != verification.ID {
		t.Error("aligning replaced the record instead of updating it")
	}
}

func TestApplyRecordSetReleasesARecordBackToTheUser(t *testing.T) {
	t.Parallel()

	store, created := provenanceStore(t)
	ctx := context.Background()
	updated, ids, err := store.ApplyRecordSet(ctx, created.ID, []zone.Op{
		engineOp(t, created.Name, "@", zone.TypeA, 300, "198.51.100.10", zone.SourceHosting),
	})
	if err != nil {
		t.Fatalf("ApplyRecordSet: %v", err)
	}
	engineID := ids[0]

	released := findRecord(t, updated, "@", zone.TypeA, "198.51.100.10")
	released.Source = zone.SourceUser
	after, _, err := store.ApplyRecordSet(ctx, created.ID, []zone.Op{
		{Kind: zone.OpUpdate, RecordID: engineID, Record: released},
	})
	if err != nil {
		t.Fatalf("release ApplyRecordSet: %v", err)
	}
	record := findRecord(t, after, "@", zone.TypeA, "198.51.100.10")
	if record.Source != zone.SourceUser {
		t.Errorf("source = %q, want the record released to the user", record.Source)
	}
	if record.ID != engineID {
		t.Error("releasing replaced the record instead of updating it")
	}
	// Once released, the ordinary API may edit it again.
	edited := mustNormalizeRecord(t, created.Name, "@", zone.TypeA, 300, "198.51.100.99")
	if _, err := store.UpdateRecord(ctx, created.ID, engineID, edited); err != nil {
		t.Errorf("UpdateRecord after release: %v", err)
	}
}

func TestApplyRecordSetEnforcesRRSetRulesInsideOneSet(t *testing.T) {
	t.Parallel()

	store, created := provenanceStore(t)
	ctx := context.Background()

	mismatched := []zone.Op{
		engineOp(t, created.Name, "@", zone.TypeA, 300, "198.51.100.10", zone.SourceHosting),
		engineOp(t, created.Name, "@", zone.TypeA, 600, "198.51.100.11", zone.SourceHosting),
	}
	if _, _, err := store.ApplyRecordSet(ctx, created.ID, mismatched); err == nil ||
		!strings.Contains(err.Error(), "same TTL") {
		t.Errorf("ApplyRecordSet with mismatched TTLs = %v, want an RRset refusal", err)
	}

	cnameClash := []zone.Op{
		engineOp(t, created.Name, "www", zone.TypeA, 300, "198.51.100.10", zone.SourceHosting),
		engineOp(t, created.Name, "www", zone.TypeCNAME, 300, "acme.dev.", zone.SourceHosting),
	}
	if _, _, err := store.ApplyRecordSet(ctx, created.ID, cnameClash); err == nil ||
		!strings.Contains(err.Error(), "CNAME cannot coexist") {
		t.Errorf("ApplyRecordSet with a CNAME clash = %v, want a refusal", err)
	}

	duplicate := []zone.Op{
		engineOp(t, created.Name, "@", zone.TypeA, 300, "198.51.100.10", zone.SourceHosting),
		engineOp(t, created.Name, "@", zone.TypeA, 300, "198.51.100.10", zone.SourceHosting),
	}
	if _, _, err := store.ApplyRecordSet(ctx, created.ID, duplicate); !errors.Is(err, zone.ErrAlreadyExists) {
		t.Errorf("ApplyRecordSet with a duplicate = %v, want ErrAlreadyExists", err)
	}
}

func TestApplyRecordSetDeletesBeforeItCreates(t *testing.T) {
	t.Parallel()

	store, created := provenanceStore(t)
	ctx := context.Background()
	conflicting, err := store.CreateRecord(ctx, created.ID, mustNormalizeRecord(t, created.Name, "www", zone.TypeA, 300, "203.0.113.9"))
	if err != nil {
		t.Fatalf("CreateRecord: %v", err)
	}
	existing := findRecord(t, conflicting, "www", zone.TypeA, "203.0.113.9")

	// Creating the alias first would break the CNAME rule; the store orders the
	// delete ahead of it, so one set can replace a conflicting record.
	updated, _, err := store.ApplyRecordSet(ctx, created.ID, []zone.Op{
		engineOp(t, created.Name, "www", zone.TypeCNAME, 300, "acme.dev.", zone.SourceHosting),
		{Kind: zone.OpDelete, RecordID: existing.ID, Record: existing, ReplaceUserRecord: true},
	})
	if err != nil {
		t.Fatalf("ApplyRecordSet: %v", err)
	}
	if countRecords(updated, "www", zone.TypeA) != 0 {
		t.Error("the conflicting A record survived")
	}
	if countRecords(updated, "www", zone.TypeCNAME) != 1 {
		t.Error("the alias was not created")
	}
}

func TestEngineOwnedRecordsRefuseTheOrdinaryApi(t *testing.T) {
	t.Parallel()

	store, created := provenanceStore(t)
	ctx := context.Background()
	updated, ids, err := store.ApplyRecordSet(ctx, created.ID, []zone.Op{
		engineOp(t, created.Name, "@", zone.TypeA, 300, "198.51.100.10", zone.SourceHosting),
	})
	if err != nil {
		t.Fatalf("ApplyRecordSet: %v", err)
	}
	engineID := ids[0]

	edited := mustNormalizeRecord(t, created.Name, "@", zone.TypeA, 300, "198.51.100.99")
	if _, err := store.UpdateRecord(ctx, created.ID, engineID, edited); !errors.Is(err, zone.ErrEngineOwned) {
		t.Errorf("UpdateRecord = %v, want ErrEngineOwned", err)
	}
	if _, err := store.DeleteRecord(ctx, created.ID, engineID); !errors.Is(err, zone.ErrEngineOwned) {
		t.Errorf("DeleteRecord = %v, want ErrEngineOwned", err)
	}
	after, err := store.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if after.Serial != updated.Serial {
		t.Error("a refused edit changed the zone")
	}
}

func TestSourceRoundTripsThroughTheStore(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_800_000_000, 0).UTC()
	path := filepath.Join(t.TempDir(), "zones.db")
	store, err := zone.Open(path, []string{"ns1.provider.example"}, zone.WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("zone.Open: %v", err)
	}
	ctx := context.Background()
	created, err := store.Create(ctx, "acme.dev")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, _, err := store.ApplyRecordSet(ctx, created.ID, []zone.Op{
		engineOp(t, created.Name, "@", zone.TypeMX, 3600, "10 mx1.simple.host.", zone.SourceMail),
	}); err != nil {
		t.Fatalf("ApplyRecordSet: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := zone.Open(path, []string{"ns1.provider.example"})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() {
		if err := reopened.Close(); err != nil {
			t.Errorf("close reopened store: %v", err)
		}
	})
	value, err := reopened.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	if got := findRecord(t, value, "@", zone.TypeMX, "10 mx1.simple.host.").Source; got != zone.SourceMail {
		t.Errorf("source after reopen = %q, want %q", got, zone.SourceMail)
	}
}

func TestValidateSnapshotChecksProvenance(t *testing.T) {
	t.Parallel()

	store, created := provenanceStore(t)
	ctx := context.Background()
	updated, _, err := store.ApplyRecordSet(ctx, created.ID, []zone.Op{
		engineOp(t, created.Name, "@", zone.TypeA, 300, "198.51.100.10", zone.SourceHosting),
	})
	if err != nil {
		t.Fatalf("ApplyRecordSet: %v", err)
	}
	if err := zone.ValidateSnapshot([]zone.Zone{updated}); err != nil {
		t.Fatalf("ValidateSnapshot with a sourced record: %v", err)
	}

	for index := range updated.Records {
		if updated.Records[index].Source == zone.SourceHosting {
			updated.Records[index].Source = "website"
		}
	}
	if err := zone.ValidateSnapshot([]zone.Zone{updated}); err == nil ||
		!strings.Contains(err.Error(), "unsupported source") {
		t.Errorf("ValidateSnapshot with an unknown source = %v, want a refusal", err)
	}

	managed, err := store.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	for index := range managed.Records {
		if managed.Records[index].Managed {
			managed.Records[index].Source = zone.SourceHosting
			break
		}
	}
	if err := zone.ValidateSnapshot([]zone.Zone{managed}); err == nil ||
		!strings.Contains(err.Error(), "cannot carry a source") {
		t.Errorf("ValidateSnapshot with a sourced managed record = %v, want a refusal", err)
	}
}

func TestValidSource(t *testing.T) {
	t.Parallel()

	for _, source := range []string{zone.SourceUser, zone.SourceHosting, zone.SourceMail} {
		if !zone.ValidSource(source) {
			t.Errorf("ValidSource(%q) = false", source)
		}
	}
	if zone.ValidSource("website") {
		t.Error("ValidSource accepted an unknown source")
	}
}

// TestApplyRecordSetAlignsEveryMemberOfOneRRSetAtOnce covers the case the
// planner's alignment step exists for: two TTL-0 members of one RRset moving
// onto a shared TTL so a third can join them. Validating each operation against
// the siblings it has not written yet refused the first align against the
// second, which made the whole plan unappliable and repeated on every pass.
func TestApplyRecordSetAlignsEveryMemberOfOneRRSetAtOnce(t *testing.T) {
	t.Parallel()

	store, created := provenanceStore(t)
	ctx := context.Background()
	// NormalizeImportedRecord deliberately preserves an explicit zero TTL, so a
	// zone file full of TTL-0 verification records imports as written.
	verification := mustNormalizeImportedRecord(t, created.Name, "@", zone.TypeTXT, 0, `"google-site-verification=abc"`)
	docusign := mustNormalizeImportedRecord(t, created.Name, "@", zone.TypeTXT, 0, `"docusign=xyz"`)
	imported, err := store.ImportZone(ctx, created.Name, []zone.Record{verification, docusign}, zone.ImportReplace, false)
	if err != nil {
		t.Fatalf("ImportZone: %v", err)
	}

	aligned := make([]zone.Op, 0, 3)
	for _, record := range imported.Records {
		if record.Managed || record.Type != zone.TypeTXT {
			continue
		}
		record.TTL = 3600
		aligned = append(aligned, zone.Op{Kind: zone.OpUpdate, RecordID: record.ID, Record: record})
	}
	if len(aligned) != 2 {
		t.Fatalf("aligned %d records, want the two TTL-0 TXTs", len(aligned))
	}
	aligned = append(aligned, engineOp(t, created.Name, "@", zone.TypeTXT, 3600, "v=spf1 mx -all", zone.SourceMail))

	updated, _, err := store.ApplyRecordSet(ctx, created.ID, aligned)
	if err != nil {
		t.Fatalf("ApplyRecordSet with two aligns in one RRset: %v", err)
	}
	for _, record := range updated.Records {
		if record.Type == zone.TypeTXT && record.TTL != 3600 {
			t.Errorf("TXT %q kept TTL %d, want 3600", record.Value, record.TTL)
		}
	}
}

// TestEngineOwnedRRSetNamesTheEngineThatAnswers: an RRset answers as a whole,
// so a second apex A sends half the site's traffic to an address the hosting
// engine does not serve, and a lower-priority MX diverts every inbound message.
// The control handler refuses such a create with this; the store keeps the
// state constructible, because an import strips provenance and the reconcilers
// have to be exercised against exactly that.
func TestEngineOwnedRRSetNamesTheEngineThatAnswers(t *testing.T) {
	t.Parallel()

	store, created := provenanceStore(t)
	ctx := context.Background()
	updated, _, err := store.ApplyRecordSet(ctx, created.ID, []zone.Op{
		engineOp(t, created.Name, "@", zone.TypeA, 300, "203.0.113.10", zone.SourceHosting),
		engineOp(t, created.Name, "@", zone.TypeMX, 3600, "10 mail.simple.example.", zone.SourceMail),
	})
	if err != nil {
		t.Fatalf("ApplyRecordSet: %v", err)
	}

	tests := []struct {
		name       string
		candidate  zone.Record
		wantSource string
		wantTaken  bool
	}{
		{
			name:       "second apex A",
			candidate:  mustNormalizeRecord(t, created.Name, "@", zone.TypeA, 300, "198.51.100.7"),
			wantSource: zone.SourceHosting,
			wantTaken:  true,
		},
		{
			name:       "lower-priority MX",
			candidate:  mustNormalizeRecord(t, created.Name, "@", zone.TypeMX, 3600, "5 mx.zoho.com."),
			wantSource: zone.SourceMail,
			wantTaken:  true,
		},
		{
			name:      "a type no engine answers at this name",
			candidate: mustNormalizeRecord(t, created.Name, "@", zone.TypeTXT, 3600, "hello"),
		},
		{
			name:      "a name no engine answers at",
			candidate: mustNormalizeRecord(t, created.Name, "docs", zone.TypeA, 300, "198.51.100.7"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			source, taken := zone.EngineOwnedRRSet(updated.Records, tt.candidate, "")
			if taken != tt.wantTaken || (tt.wantTaken && source != tt.wantSource) {
				t.Errorf("EngineOwnedRRSet = %q/%t, want %q/%t", source, taken, tt.wantSource, tt.wantTaken)
			}
		})
	}
}

// The store, not each planner, is where provenance is unbreakable: a delete
// that names the wrong record, an engine deleting the other engine's record,
// and a user record deleted without an explicit conflict replacement are all
// refused inside the transaction.
func TestApplyRecordSetRefusesADeleteAcrossProvenance(t *testing.T) {
	t.Parallel()

	store, created := provenanceStore(t)
	ctx := context.Background()
	seeded, ids, err := store.ApplyRecordSet(ctx, created.ID, []zone.Op{
		engineOp(t, created.Name, "@", zone.TypeA, 300, "198.51.100.10", zone.SourceHosting),
		engineOp(t, created.Name, "@", zone.TypeMX, 3600, "10 mx.example.net.", zone.SourceMail),
	})
	if err != nil {
		t.Fatalf("seed ApplyRecordSet: %v", err)
	}
	hostingRecord := findRecord(t, seeded, "@", zone.TypeA, "198.51.100.10")
	mailRecord := findRecord(t, seeded, "@", zone.TypeMX, "10 mx.example.net.")
	_ = ids

	withUser, err := store.CreateRecord(ctx, created.ID, mustNormalizeRecord(t, created.Name, "docs", zone.TypeA, 300, "203.0.113.4"))
	if err != nil {
		t.Fatalf("CreateRecord: %v", err)
	}
	userRecord := findRecord(t, withUser, "docs", zone.TypeA, "203.0.113.4")

	// The mail engine hands the hosting record's id to a delete it describes
	// as its own. The description does not match what is stored.
	crossEngine := mailRecord
	crossEngine.ID = hostingRecord.ID
	if _, _, err := store.ApplyRecordSet(ctx, created.ID, []zone.Op{
		{Kind: zone.OpDelete, RecordID: hostingRecord.ID, Record: crossEngine},
	}); err == nil || !strings.Contains(err.Error(), "must describe the record it removes") {
		t.Errorf("cross-engine delete = %v, want a refusal", err)
	}

	// The same id, described as the user record it is not.
	if _, _, err := store.ApplyRecordSet(ctx, created.ID, []zone.Op{
		{Kind: zone.OpDelete, RecordID: hostingRecord.ID, Record: userRecord, ReplaceUserRecord: true},
	}); err == nil || !strings.Contains(err.Error(), "must describe the record it removes") {
		t.Errorf("mis-described delete = %v, want a refusal", err)
	}

	// A delete that describes nothing at all is refused: the description is
	// the only provenance the store has.
	if _, _, err := store.ApplyRecordSet(ctx, created.ID, []zone.Op{
		{Kind: zone.OpDelete, RecordID: hostingRecord.ID},
	}); err == nil || !strings.Contains(err.Error(), "must describe the record it removes") {
		t.Errorf("undescribed delete = %v, want a refusal", err)
	}

	// A user record goes only on an explicit conflict replacement.
	if _, _, err := store.ApplyRecordSet(ctx, created.ID, []zone.Op{
		{Kind: zone.OpDelete, RecordID: userRecord.ID, Record: userRecord},
	}); err == nil || !strings.Contains(err.Error(), "explicit conflict replacement") {
		t.Errorf("unflagged user delete = %v, want a refusal", err)
	}
	if _, _, err := store.ApplyRecordSet(ctx, created.ID, []zone.Op{
		{Kind: zone.OpDelete, RecordID: userRecord.ID, Record: userRecord, ReplaceUserRecord: true},
	}); err != nil {
		t.Errorf("flagged user delete: %v", err)
	}

	after, err := store.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if countRecords(after, "@", zone.TypeA) != 1 || countRecords(after, "@", zone.TypeMX) != 1 {
		t.Error("a refused delete removed a record anyway")
	}
}

// The mirror image of the release rule: one engine does not take a record the
// other owns, which its reconciler would rewrite on the next pass.
func TestApplyRecordSetRefusesAnUpdateAcrossEngines(t *testing.T) {
	t.Parallel()

	store, created := provenanceStore(t)
	ctx := context.Background()
	seeded, _, err := store.ApplyRecordSet(ctx, created.ID, []zone.Op{
		engineOp(t, created.Name, "www", zone.TypeA, 300, "198.51.100.10", zone.SourceHosting),
	})
	if err != nil {
		t.Fatalf("seed ApplyRecordSet: %v", err)
	}
	owned := findRecord(t, seeded, "www", zone.TypeA, "198.51.100.10")

	stolen := owned
	stolen.Source = zone.SourceMail
	if _, _, err := store.ApplyRecordSet(ctx, created.ID, []zone.Op{
		{Kind: zone.OpUpdate, RecordID: owned.ID, Record: stolen},
	}); err == nil || !strings.Contains(err.Error(), "take over a record another engine owns") {
		t.Errorf("cross-engine update = %v, want a refusal", err)
	}

	// Its own engine still aligns it, and a release to the user still works.
	aligned := owned
	aligned.TTL = 600
	if _, _, err := store.ApplyRecordSet(ctx, created.ID, []zone.Op{
		{Kind: zone.OpUpdate, RecordID: owned.ID, Record: aligned},
	}); err != nil {
		t.Errorf("aligning a record within its own source: %v", err)
	}

	after, err := store.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if record := findRecord(t, after, "www", zone.TypeA, "198.51.100.10"); record.Source != zone.SourceHosting {
		t.Errorf("source = %q, want the record still owned by hosting", record.Source)
	}
}
