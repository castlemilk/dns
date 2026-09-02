package zonefile_test

import (
	"strings"
	"testing"
	"time"

	"github.com/castlemilk/dns/internal/zone"
	"github.com/castlemilk/dns/internal/zonefile"
)

func TestParseSupportedBINDZone(t *testing.T) {
	t.Parallel()
	source := `$ORIGIN example.com.
$TTL 0
@ IN SOA old.example. hostmaster.example. 1 2 3 4 5
@ IN NS old.example.
www IN A 192.0.2.10
v6 IN AAAA 2001:db8::1
alias IN CNAME www
@ IN MX 10 mail
text IN TXT "hello world" "second"
delegated IN NS ns.child
_sip._tcp IN SRV 10 20 5060 sip
@ IN CAA 0 issue "letsencrypt.org"
`
	parsed, err := zonefile.Parse("Example.COM.", source)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(parsed.Records) != 8 {
		t.Fatalf("records = %d, want 8: %#v", len(parsed.Records), parsed.Records)
	}
	if len(parsed.Warnings) != 2 || !strings.Contains(parsed.Warnings[0], "SOA") || !strings.Contains(parsed.Warnings[1], "NS") {
		t.Fatalf("warnings = %#v", parsed.Warnings)
	}
	for _, record := range parsed.Records {
		if record.TTL != 0 {
			t.Errorf("%s TTL = %d, want explicit zero", record.Name, record.TTL)
		}
		if record.Managed || record.ID != "" {
			t.Errorf("parsed record has persistence fields: %#v", record)
		}
	}
	assertRecord(t, parsed.Records, "alias", zone.TypeCNAME, "www.example.com.")
	assertRecord(t, parsed.Records, "@", zone.TypeMX, "10 mail.example.com.")
	assertRecord(t, parsed.Records, "delegated", zone.TypeNS, "ns.child.example.com.")
	assertRecord(t, parsed.Records, "_sip._tcp", zone.TypeSRV, "10 20 5060 sip.example.com.")
}

func TestParseRejectsUnsupportedOrUnsafeInput(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		source string
		want   string
	}{
		{name: "unsupported signed record", source: "@ 300 IN DS 12345 13 2 aabb\n", want: "unsupported type DS"},
		{name: "outside owner", source: "outside.example.net. 300 IN A 192.0.2.1\n", want: "outside zone"},
		{name: "include", source: "$INCLUDE other.zone\n", want: "include"},
		{name: "malformed", source: "www IN A not-an-address\n", want: "parse BIND"},
		{name: "unsupported class", source: "www 300 CH TXT \"x\"\n", want: "class"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := zonefile.Parse("example.com", tt.source)
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tt.want)) {
				t.Fatalf("Parse error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestExportIsCanonicalDeterministicAndRoundTrips(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0).UTC()
	value := zone.Zone{
		ID: "zone-id", Name: "example.com", Serial: 1_800_000_000,
		Nameservers: []string{"ns1.dns.example."}, CreatedAt: now, UpdatedAt: now,
		Records: []zone.Record{
			record("b", "www", zone.TypeA, 300, "192.0.2.2", false, now),
			record("soa", "@", zone.TypeSOA, 3600, "ns1.dns.example. hostmaster.example.com. 1800000000 3600 600 1209600 300", true, now),
			record("ns", "@", zone.TypeNS, 3600, "ns1.dns.example.", true, now),
			record("a", "www", zone.TypeA, 300, "192.0.2.1", false, now),
		},
	}
	first, err := zonefile.Export(value)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	second, err := zonefile.Export(value)
	if err != nil {
		t.Fatalf("second Export: %v", err)
	}
	if first != second || !strings.HasPrefix(first, "$ORIGIN example.com.\n") {
		t.Fatalf("noncanonical export:\n%s", first)
	}
	parsed, err := zonefile.Parse(value.Name, first)
	if err != nil {
		t.Fatalf("parse exported zone: %v", err)
	}
	if len(parsed.Records) != 2 || len(parsed.Warnings) != 2 {
		t.Fatalf("round trip = records:%d warnings:%#v", len(parsed.Records), parsed.Warnings)
	}
	assertRecord(t, parsed.Records, "www", zone.TypeA, "192.0.2.1")
	assertRecord(t, parsed.Records, "www", zone.TypeA, "192.0.2.2")
}

func record(id, name string, recordType zone.RecordType, ttl uint32, value string, managed bool, now time.Time) zone.Record {
	return zone.Record{ID: id, Name: name, Type: recordType, TTL: ttl, Value: value, Managed: managed, CreatedAt: now, UpdatedAt: now}
}

func assertRecord(t *testing.T, values []zone.Record, name string, recordType zone.RecordType, value string) {
	t.Helper()
	for _, record := range values {
		if record.Name == name && record.Type == recordType && record.Value == value {
			return
		}
	}
	t.Errorf("record %s %s %q not found in %#v", name, recordType, value, values)
}
