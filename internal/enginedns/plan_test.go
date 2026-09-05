package enginedns_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/castlemilk/dns/internal/enginedns"
	"github.com/castlemilk/dns/internal/zone"
)

func record(id, name string, kind zone.RecordType, ttl uint32, value, source string) zone.Record {
	return zone.Record{ID: id, Name: name, Type: kind, TTL: ttl, Value: value, Source: source}
}

func testZone(records ...zone.Record) zone.Zone {
	all := []zone.Record{
		{ID: "soa", Name: "@", Type: zone.TypeSOA, TTL: 3600, Value: "ns1.simple.test. hostmaster.acme.dev. 1 3600 600 1209600 300", Managed: true},
		{ID: "ns", Name: "@", Type: zone.TypeNS, TTL: 3600, Value: "ns1.simple.test.", Managed: true},
	}
	all = append(all, records...)
	return zone.Zone{ID: "z1", Name: "acme.dev", Nameservers: []string{"ns1.simple.test."}, Records: all}
}

func desiredGateway() []zone.Record {
	return []zone.Record{
		{Name: "@", Type: zone.TypeA, TTL: 300, Value: "198.51.100.10"},
		{Name: "@", Type: zone.TypeA, TTL: 300, Value: "198.51.100.11"},
		{Name: "www", Type: zone.TypeCNAME, TTL: 300, Value: "acme.dev."},
	}
}

func opValues(ops []enginedns.Op) []string {
	values := make([]string, 0, len(ops))
	for _, op := range ops {
		values = append(values, op.Record.Name+" "+string(op.Record.Type)+" "+op.Record.Value)
	}
	slices.Sort(values)
	return values
}

func opTTLs(ops []enginedns.Op) []uint32 {
	ttls := make([]uint32, 0, len(ops))
	for _, op := range ops {
		ttls = append(ttls, op.Record.TTL)
	}
	return ttls
}

func TestPlanRejectsNonEngineSource(t *testing.T) {
	t.Parallel()

	for _, source := range []string{enginedns.SourceUser, "website"} {
		if _, err := enginedns.Plan(testZone(), source, nil); err == nil {
			t.Errorf("Plan(source=%q) = nil error, want a refusal", source)
		}
	}
}

func TestPlanCreatesTheWholeSetOnAnEmptyZone(t *testing.T) {
	t.Parallel()

	plan, err := enginedns.Plan(testZone(), enginedns.SourceHosting, desiredGateway())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := len(plan.Create); got != 3 {
		t.Fatalf("Create = %d ops (%v), want 3", got, opValues(plan.Create))
	}
	if len(plan.Adopt) != 0 || len(plan.Align) != 0 || len(plan.Remove) != 0 || len(plan.Conflicts) != 0 {
		t.Errorf("plan = adopt:%d align:%d remove:%d conflicts:%d, want only creates",
			len(plan.Adopt), len(plan.Align), len(plan.Remove), len(plan.Conflicts))
	}
	if plan.InSync() {
		t.Error("InSync = true for a zone that is missing every record")
	}
	for _, op := range plan.Create {
		if op.Record.Source != enginedns.SourceHosting {
			t.Errorf("create %s carries source %q", op.Record.Value, op.Record.Source)
		}
	}
}

func TestPlanAdoptsAnEqualUserRecordInPlace(t *testing.T) {
	t.Parallel()

	existing := testZone(record("r1", "@", zone.TypeA, 300, "198.51.100.10", enginedns.SourceUser))
	plan, err := enginedns.Plan(existing, enginedns.SourceHosting, desiredGateway())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Adopt) != 1 {
		t.Fatalf("Adopt = %v, want the existing apex A record", opValues(plan.Adopt))
	}
	adopted := plan.Adopt[0]
	if adopted.RecordID != "r1" || adopted.Kind != enginedns.OpUpdate {
		t.Errorf("adopt op = %+v, want an update of r1", adopted)
	}
	if adopted.Record.Source != enginedns.SourceHosting {
		t.Errorf("adopted source = %q", adopted.Record.Source)
	}
	if adopted.Record.TTL != 300 {
		t.Errorf("adopted TTL = %d, want the existing RRset TTL 300", adopted.Record.TTL)
	}
	if len(plan.Create) != 2 {
		t.Errorf("Create = %v, want the second address and the www alias", opValues(plan.Create))
	}
	if len(plan.Conflicts) != 0 {
		t.Errorf("Conflicts = %v, want none", plan.Problems())
	}
}

func TestPlanAlignsATtlZeroRRSetBeforeAddingAMember(t *testing.T) {
	t.Parallel()

	existing := testZone(record("r1", "@", zone.TypeA, 0, "198.51.100.10", enginedns.SourceUser))
	plan, err := enginedns.Plan(existing, enginedns.SourceHosting, desiredGateway())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Adopt) != 1 || plan.Adopt[0].Record.TTL != 300 {
		t.Fatalf("Adopt = %+v, want the imported record adopted at TTL 300", plan.Adopt)
	}
	for _, op := range plan.Create {
		if op.Record.TTL != 300 {
			t.Errorf("create %s TTL = %d, want 300 so the RRset stays consistent", op.Record.Value, op.Record.TTL)
		}
	}
}

func TestPlanAlignsAUserTxtSoSpfCanJoinTheRRSet(t *testing.T) {
	t.Parallel()

	existing := testZone(record("r1", "@", zone.TypeTXT, 0, `"google-site-verification=abc"`, enginedns.SourceUser))
	desired := []zone.Record{{Name: "@", Type: zone.TypeTXT, TTL: 3600, Value: "v=spf1 mx -all"}}
	plan, err := enginedns.Plan(existing, enginedns.SourceMail, desired)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Conflicts) != 0 {
		t.Fatalf("Conflicts = %v, want the verification TXT left alone", plan.Problems())
	}
	if len(plan.Align) != 1 || plan.Align[0].RecordID != "r1" {
		t.Fatalf("Align = %+v, want the TTL-0 verification TXT aligned", plan.Align)
	}
	if plan.Align[0].Record.TTL != 3600 {
		t.Errorf("aligned TTL = %d, want 3600", plan.Align[0].Record.TTL)
	}
	if plan.Align[0].Record.Source != enginedns.SourceUser {
		t.Errorf("aligned source = %q, want the record to stay the user's", plan.Align[0].Record.Source)
	}
	if plan.Align[0].Record.Value != `"google-site-verification=abc"` {
		t.Errorf("aligned value = %q, want it unchanged", plan.Align[0].Record.Value)
	}
	if len(plan.Create) != 1 || plan.Create[0].Record.TTL != 3600 {
		t.Errorf("Create = %+v, want SPF created at 3600", plan.Create)
	}
}

func TestPlanAlignsATtlZeroMxBeforeReplacingIt(t *testing.T) {
	t.Parallel()

	existing := testZone(record("r1", "@", zone.TypeMX, 0, "5 mail.elsewhere.test.", enginedns.SourceUser))
	desired := []zone.Record{{Name: "@", Type: zone.TypeMX, TTL: 3600, Value: "10 mx1.simple.host."}}
	plan, err := enginedns.Plan(existing, enginedns.SourceMail, desired)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Conflicts) != 1 || plan.Conflicts[0].RecordID != "r1" {
		t.Fatalf("Conflicts = %+v, want the foreign MX reported", plan.Conflicts)
	}
	// Worker mode keeps the conflict, so the RRset must be aligned for the new
	// member to be accepted at all.
	worker := plan.ApplyOps(false)
	if !slices.ContainsFunc(worker, func(op enginedns.Op) bool { return op.RecordID == "r1" && op.Kind == enginedns.OpUpdate }) {
		t.Errorf("worker ops = %+v, want the TTL-0 MX aligned", worker)
	}
	// Replace mode deletes it, so the alignment must be dropped with it.
	replace := plan.ApplyOps(true)
	for _, op := range replace {
		if op.RecordID == "r1" && op.Kind != enginedns.OpDelete {
			t.Errorf("replace ops contain %+v for a record that is being deleted", op)
		}
	}
	if !slices.ContainsFunc(replace, func(op enginedns.Op) bool { return op.RecordID == "r1" && op.Kind == enginedns.OpDelete }) {
		t.Errorf("replace ops = %+v, want a delete for the foreign MX", replace)
	}
}

func TestPlanReportsAConflictForADifferentValue(t *testing.T) {
	t.Parallel()

	existing := testZone(record("r1", "@", zone.TypeA, 300, "203.0.113.9", enginedns.SourceUser))
	plan, err := enginedns.Plan(existing, enginedns.SourceHosting, desiredGateway())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Conflicts) != 1 || plan.Conflicts[0].RecordID != "r1" {
		t.Fatalf("Conflicts = %+v, want the custom apex A reported", plan.Conflicts)
	}
	if !plan.Conflicts[0].Replaceable() {
		t.Error("a user record is not replaceable")
	}
	if got := plan.Problems(); len(got) != 1 || !strings.Contains(got[0], "203.0.113.9") {
		t.Errorf("Problems = %v", got)
	}
	if plan.InSync() {
		t.Error("InSync = true with a conflict outstanding")
	}
	for _, op := range plan.ApplyOps(false) {
		if op.Kind == enginedns.OpDelete && op.RecordID == "r1" {
			t.Fatal("worker ops delete a user record")
		}
	}
	// The store refuses to delete a user record unless the operation says it
	// is a conflict replacement, so the flag has to travel with the op.
	if !slices.ContainsFunc(plan.ApplyOps(true), func(op enginedns.Op) bool {
		return op.Kind == enginedns.OpDelete && op.RecordID == "r1" && op.ReplaceUserRecord
	}) {
		t.Error("replace ops do not delete the conflicting record as an explicit replacement")
	}
}

func TestPlanCnameCoexistence(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		existing zone.Record
		reason   string
	}{
		{
			name:     "an A record blocks the www alias",
			existing: record("r1", "www", zone.TypeA, 300, "203.0.113.9", enginedns.SourceUser),
			reason:   "prevents the alias",
		},
		{
			name:     "an alias blocks the apex records",
			existing: record("r1", "www", zone.TypeCNAME, 300, "elsewhere.test.", enginedns.SourceUser),
			reason:   "points somewhere else",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			plan, err := enginedns.Plan(testZone(test.existing), enginedns.SourceHosting, desiredGateway())
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			if len(plan.Conflicts) != 1 || plan.Conflicts[0].RecordID != "r1" {
				t.Fatalf("Conflicts = %+v", plan.Conflicts)
			}
			if !strings.Contains(plan.Conflicts[0].Reason, test.reason) {
				t.Errorf("reason = %q, want it to mention %q", plan.Conflicts[0].Reason, test.reason)
			}
		})
	}
}

func TestPlanAdoptsASplitTxtValueInsteadOfDuplicatingIt(t *testing.T) {
	t.Parallel()

	const key = "v=DKIM1; k=rsa; p=MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEA"
	split := `"v=DKIM1; k=rsa; p=MIIBIjANBgkqhkiG9w0BAQEFA" "AOCAQ8AMIIBCgKCAQEA"`
	existing := testZone(record("r1", "sel._domainkey", zone.TypeTXT, 3600, split, enginedns.SourceUser))
	desired := []zone.Record{{Name: "sel._domainkey", Type: zone.TypeTXT, TTL: 3600, Value: key}}

	plan, err := enginedns.Plan(existing, enginedns.SourceMail, desired)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Create) != 0 {
		t.Fatalf("Create = %v, want the split value adopted instead", opValues(plan.Create))
	}
	if len(plan.Adopt) != 1 || plan.Adopt[0].RecordID != "r1" {
		t.Fatalf("Adopt = %+v", plan.Adopt)
	}
	if plan.Adopt[0].Record.Value != split {
		t.Errorf("adopted value = %q, want the stored split form kept", plan.Adopt[0].Record.Value)
	}
	if len(plan.Conflicts) != 0 {
		t.Errorf("Conflicts = %v", plan.Problems())
	}
}

func TestPlanConflictsOnlyWithATxtOfTheSameKind(t *testing.T) {
	t.Parallel()

	existing := testZone(
		record("spf", "@", zone.TypeTXT, 3600, `"v=spf1 include:elsewhere.test -all"`, enginedns.SourceUser),
		record("verify", "@", zone.TypeTXT, 3600, `"MS=ms12345"`, enginedns.SourceUser),
	)
	desired := []zone.Record{{Name: "@", Type: zone.TypeTXT, TTL: 3600, Value: "v=spf1 mx -all"}}
	plan, err := enginedns.Plan(existing, enginedns.SourceMail, desired)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Conflicts) != 1 || plan.Conflicts[0].RecordID != "spf" {
		t.Fatalf("Conflicts = %+v, want only the existing SPF record", plan.Conflicts)
	}
}

func TestPlanRemovesEngineRecordsThatAreNoLongerDesired(t *testing.T) {
	t.Parallel()

	existing := testZone(
		record("a1", "@", zone.TypeA, 300, "198.51.100.10", enginedns.SourceHosting),
		record("a2", "@", zone.TypeA, 300, "198.51.100.11", enginedns.SourceHosting),
		record("www", "www", zone.TypeCNAME, 300, "acme.dev.", enginedns.SourceHosting),
	)
	desired := []zone.Record{{Name: "@", Type: zone.TypeA, TTL: 300, Value: "198.51.100.10"}}
	plan, err := enginedns.Plan(existing, enginedns.SourceHosting, desired)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	removed := make([]string, 0, len(plan.Remove))
	for _, op := range plan.Remove {
		removed = append(removed, op.RecordID)
	}
	slices.Sort(removed)
	if !slices.Equal(removed, []string{"a2", "www"}) {
		t.Errorf("Remove = %v, want the second address and the alias", removed)
	}
	if len(plan.Keep) != 1 || plan.Keep[0].RecordID != "a1" {
		t.Errorf("Keep = %+v", plan.Keep)
	}
	for _, op := range plan.WorkerOps() {
		if op.Kind == enginedns.OpKeep {
			t.Error("worker ops contain a keep operation, which the store would refuse")
		}
	}
}

func TestPlanIsIdempotent(t *testing.T) {
	t.Parallel()

	existing := testZone(
		record("a1", "@", zone.TypeA, 300, "198.51.100.10", enginedns.SourceHosting),
		record("a2", "@", zone.TypeA, 300, "198.51.100.11", enginedns.SourceHosting),
		record("www", "www", zone.TypeCNAME, 300, "acme.dev.", enginedns.SourceHosting),
	)
	plan, err := enginedns.Plan(existing, enginedns.SourceHosting, desiredGateway())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if !plan.InSync() {
		t.Fatalf("InSync = false; create:%v remove:%v conflicts:%v", opValues(plan.Create), plan.Remove, plan.Problems())
	}
	if len(plan.WorkerOps()) != 0 {
		t.Errorf("WorkerOps = %+v, want nothing to do", plan.WorkerOps())
	}
	if len(plan.Keep) != 3 {
		t.Errorf("Keep = %d, want every record kept", len(plan.Keep))
	}
}

func TestPlanNormalisesCaseAndAddressForm(t *testing.T) {
	t.Parallel()

	existing := testZone(record("r1", "www", zone.TypeCNAME, 300, "ACME.DEV.", enginedns.SourceUser))
	desired := []zone.Record{{Name: "WWW", Type: zone.TypeCNAME, TTL: 300, Value: "acme.dev."}}
	plan, err := enginedns.Plan(existing, enginedns.SourceHosting, desired)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Adopt) != 1 {
		t.Fatalf("Adopt = %+v, want the differently-cased alias adopted", plan.Adopt)
	}
}

func TestPlanDeduplicatesTheDesiredSet(t *testing.T) {
	t.Parallel()

	desired := []zone.Record{
		{Name: "@", Type: zone.TypeA, TTL: 300, Value: "198.51.100.10"},
		{Name: "@", Type: zone.TypeA, TTL: 300, Value: "198.51.100.10"},
	}
	plan, err := enginedns.Plan(testZone(), enginedns.SourceHosting, desired)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Create) != 1 {
		t.Errorf("Create = %v, want one record", opValues(plan.Create))
	}
	if got := opTTLs(plan.Create); len(got) != 1 || got[0] != 300 {
		t.Errorf("create TTLs = %v", got)
	}
}

func TestPlanChangesDescribeEveryOperation(t *testing.T) {
	t.Parallel()

	existing := testZone(
		record("r1", "@", zone.TypeA, 300, "198.51.100.10", enginedns.SourceUser),
		record("r2", "www", zone.TypeA, 300, "203.0.113.9", enginedns.SourceUser),
	)
	plan, err := enginedns.Plan(existing, enginedns.SourceHosting, desiredGateway())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	ops := map[string]int{}
	for _, change := range plan.Changes {
		ops[change.Op]++
	}
	for _, want := range []string{enginedns.ChangeAdopt, enginedns.ChangeCreate, enginedns.ChangeReplace} {
		if ops[want] == 0 {
			t.Errorf("no %q change in %+v", want, plan.Changes)
		}
	}
	protos := plan.ChangesProto()
	if len(protos) != len(plan.Changes) {
		t.Fatalf("ChangesProto = %d entries, want %d", len(protos), len(plan.Changes))
	}
	for index, change := range plan.Changes {
		if protos[index].GetOp() != change.Op || protos[index].GetName() != change.Name {
			t.Errorf("proto[%d] = %+v, want %+v", index, protos[index], change)
		}
	}
}

func TestPlanReportsAnotherEnginesRecordAsAConflict(t *testing.T) {
	t.Parallel()

	existing := testZone(record("r1", "@", zone.TypeA, 300, "203.0.113.9", enginedns.SourceMail))
	plan, err := enginedns.Plan(existing, enginedns.SourceHosting, desiredGateway())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Conflicts) != 1 || !strings.Contains(plan.Conflicts[0].Reason, "email engine") {
		t.Fatalf("Conflicts = %+v", plan.Conflicts)
	}
	if len(plan.Remove) != 0 {
		t.Errorf("Remove = %+v, want another engine's record left alone", plan.Remove)
	}
	if plan.Conflicts[0].Source != enginedns.SourceMail {
		t.Errorf("conflict source = %q, want the owning engine", plan.Conflicts[0].Source)
	}
	if plan.Conflicts[0].Replaceable() {
		t.Error("another engine's record is marked replaceable; its reconciler would re-create it")
	}
	for _, op := range plan.ApplyOps(true) {
		if op.Kind == enginedns.OpDelete && op.RecordID == "r1" {
			t.Error("an explicit replace deleted another engine's record")
		}
	}
}

// TestPlanConflictsWithAUserSRVAtAnOwnerItPublishes: mail is the only engine
// that publishes SRV, and it publishes six owners for client autoconfiguration.
// An RFC 2782 client picks uniformly at random between equal-priority,
// equal-weight targets, so leaving the old provider's `_imaps._tcp` beside ours
// sent roughly half of every Thunderbird/iOS/Outlook autoconfiguration to a
// server with no such mailbox — with no conflict reported and a green Records
// row.
func TestPlanConflictsWithAUserSRVAtAnOwnerItPublishes(t *testing.T) {
	t.Parallel()

	value := testZone(
		record("r1", "_imaps._tcp", zone.TypeSRV, 3600, "0 1 993 imap.gmail.com.", zone.SourceUser),
		record("r2", "_sip._tcp", zone.TypeSRV, 3600, "0 1 5060 sip.example.net.", zone.SourceUser),
	)
	plan, err := enginedns.Plan(value, enginedns.SourceMail, []zone.Record{
		{Name: "_imaps._tcp", Type: zone.TypeSRV, TTL: 3600, Value: "0 1 993 mail.local.test."},
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Conflicts) != 1 {
		t.Fatalf("conflicts = %v, want exactly the _imaps._tcp record", plan.Conflicts)
	}
	conflict := plan.Conflicts[0]
	if conflict.RecordID != "r1" {
		t.Errorf("conflicted %q, want r1; an SRV owner the engine does not publish is none of its business", conflict.RecordID)
	}
	if !conflict.Replaceable() {
		t.Error("a user SRV should be replaceable")
	}
	if plan.InSync() {
		t.Error("InSync() = true while a conflict stands")
	}
}

// TestWorkerOpsOmitsWhatACNAMEConflictBlocks: ApplyRecordSet is atomic, so an
// operation the CNAME rule is guaranteed to refuse used to roll back every
// unrelated record with it — one leftover alias at `www` stopped the apex A
// that would have restored the site on its own, on every reconcile pass.
func TestWorkerOpsOmitsWhatACNAMEConflictBlocks(t *testing.T) {
	t.Parallel()

	value := testZone(
		record("apex", "@", zone.TypeA, 3600, "198.51.100.7", zone.SourceUser),
		record("alias", "www", zone.TypeCNAME, 3600, "oldhost.provider.example.", zone.SourceUser),
	)
	plan, err := enginedns.Plan(value, enginedns.SourceHosting, []zone.Record{
		{Name: "@", Type: zone.TypeA, TTL: 300, Value: "203.0.113.10"},
		{Name: "www", Type: zone.TypeCNAME, TTL: 300, Value: "acme.dev."},
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Create) != 2 {
		t.Fatalf("Create = %v, want the whole desired set described", opValues(plan.Create))
	}
	if len(plan.Blocked) != 1 || plan.Blocked[0].Op.Record.Name != "www" {
		t.Fatalf("Blocked = %+v, want exactly the www CNAME", plan.Blocked)
	}
	if got := plan.Blocked[0].ConflictIDs; len(got) != 1 || got[0] != "alias" {
		t.Errorf("blocking conflicts = %v, want the user alias", got)
	}
	if got := opValues(plan.WorkerOps()); !slices.Equal(got, []string{"@ A 203.0.113.10"}) {
		t.Errorf("WorkerOps() = %v, want only the apex A — the record that restores the site", got)
	}
	// The change list has to say the record was not written, not imply it was.
	blocked := 0
	for _, change := range plan.Changes {
		if change.Op == enginedns.ChangeBlocked {
			blocked++
			if change.Name != "www" || change.Why == "" {
				t.Errorf("blocked change = %+v, want it to name www and say why", change)
			}
		}
	}
	if blocked != 1 {
		t.Errorf("blocked changes = %d, want 1", blocked)
	}
	// Replacing the conflict is what makes the operation appliable again.
	if got := opValues(plan.ApplyOps(true)); !slices.Contains(got, "www CNAME acme.dev.") {
		t.Errorf("ApplyOps(true) = %v, want the www CNAME back once its conflict is deleted", got)
	}
}
