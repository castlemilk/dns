package zone_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/castlemilk/dns/internal/zone"
)

func TestNormalizeName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		input     string
		want      string
		wantField string
	}{
		{name: "canonical name", input: "  Example.COM.  ", want: "example.com"},
		{name: "punycode", input: "XN--BCHER-KVA.Example.", want: "xn--bcher-kva.example"},
		{name: "empty", input: "", wantField: "name"},
		{name: "apex shorthand", input: "@", wantField: "name"},
		{name: "root", input: ".", wantField: "name"},
		{name: "wildcard", input: "*.example.com", wantField: "name"},
		{name: "unicode", input: "bücher.example", wantField: "name"},
		{name: "empty label", input: "bad..example.com", wantField: "name"},
		{name: "multiple trailing dots", input: "example.com..", wantField: "name"},
		{name: "embedded whitespace", input: "bad name.example", wantField: "name"},
		{name: "zone file comment", input: ";example.com", wantField: "name"},
		{name: "zone file directive", input: "$ORIGIN", wantField: "name"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := zone.NormalizeName(tt.input)
			if tt.wantField != "" {
				assertValidationField(t, err, tt.wantField)
				return
			}
			if err != nil {
				t.Fatalf("NormalizeName(%q): %v", tt.input, err)
			}
			if got != tt.want {
				t.Errorf("NormalizeName(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestNormalizeNameserver(t *testing.T) {
	t.Parallel()

	got, err := zone.NormalizeNameserver(" NS1.Example.COM ")
	if err != nil {
		t.Fatalf("NormalizeNameserver: %v", err)
	}
	if want := "ns1.example.com."; got != want {
		t.Errorf("NormalizeNameserver = %q, want %q", got, want)
	}

	_, err = zone.NormalizeNameserver("*")
	assertValidationField(t, err, "nameserver")
}

func TestNormalizeRecord(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		owner     string
		typeValue zone.RecordType
		ttl       uint32
		value     string
		wantName  string
		wantTTL   uint32
		wantValue string
		wantOwner string
	}{
		{
			name: "apex A defaults TTL", owner: "", typeValue: zone.TypeA,
			value: "192.0.2.1", wantName: "@", wantTTL: zone.DefaultRecordTTL,
			wantValue: "192.0.2.1", wantOwner: "example.com.",
		},
		{
			name: "absolute owner", owner: "WWW.Example.COM.", typeValue: zone.TypeA,
			ttl: 60, value: "192.0.2.2", wantName: "www", wantTTL: 60,
			wantValue: "192.0.2.2", wantOwner: "www.example.com.",
		},
		{
			name: "IPv6 canonical form", owner: "v6", typeValue: zone.TypeAAAA,
			ttl: 60, value: "2001:0db8:0:0:0:0:0:1", wantName: "v6", wantTTL: 60,
			wantValue: "2001:db8::1", wantOwner: "v6.example.com.",
		},
		{
			name: "relative CNAME target", owner: "Alias", typeValue: zone.TypeCNAME,
			ttl: 300, value: "WEB", wantName: "alias", wantTTL: 300,
			wantValue: "web.example.com.", wantOwner: "alias.example.com.",
		},
		{
			name: "external CNAME target", owner: "external", typeValue: zone.TypeCNAME,
			ttl: 300, value: "Target.Example.NET", wantName: "external", wantTTL: 300,
			wantValue: "target.example.net.", wantOwner: "external.example.com.",
		},
		{
			name: "MX", owner: "@", typeValue: zone.TypeMX,
			ttl: 600, value: "10 Mail", wantName: "@", wantTTL: 600,
			wantValue: "10 mail.example.com.", wantOwner: "example.com.",
		},
		{
			name: "TXT quoting", owner: "txt", typeValue: zone.TypeTXT,
			ttl: 300, value: "hello world", wantName: "txt", wantTTL: 300,
			wantValue: `"hello world"`, wantOwner: "txt.example.com.",
		},
		{
			name: "SRV", owner: "_https._tcp", typeValue: zone.TypeSRV,
			ttl: 300, value: "10 20 443 service", wantName: "_https._tcp", wantTTL: 300,
			wantValue: "10 20 443 service.example.com.", wantOwner: "_https._tcp.example.com.",
		},
		{
			name: "CAA quoting", owner: "@", typeValue: zone.TypeCAA,
			ttl: 300, value: "0 issue letsencrypt.org", wantName: "@", wantTTL: 300,
			wantValue: `0 issue "letsencrypt.org"`, wantOwner: "example.com.",
		},
		{
			name: "wildcard owner", owner: "*.API", typeValue: zone.TypeA,
			ttl: 60, value: "192.0.2.9", wantName: "*.api", wantTTL: 60,
			wantValue: "192.0.2.9", wantOwner: "*.api.example.com.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := zone.NormalizeRecord("example.com", tt.owner, tt.typeValue, tt.ttl, tt.value)
			if err != nil {
				t.Fatalf("NormalizeRecord: %v", err)
			}
			if got.Name != tt.wantName {
				t.Errorf("Name = %q, want %q", got.Name, tt.wantName)
			}
			if got.Type != tt.typeValue {
				t.Errorf("Type = %q, want %q", got.Type, tt.typeValue)
			}
			if got.TTL != tt.wantTTL {
				t.Errorf("TTL = %d, want %d", got.TTL, tt.wantTTL)
			}
			if got.Value != tt.wantValue {
				t.Errorf("Value = %q, want %q", got.Value, tt.wantValue)
			}

			rr, err := zone.Compile("example.com", got)
			if err != nil {
				t.Fatalf("Compile normalized record: %v", err)
			}
			if rr.Header().Name != tt.wantOwner {
				t.Errorf("compiled owner = %q, want %q", rr.Header().Name, tt.wantOwner)
			}
		})
	}
}

func TestNormalizeRecordRejectsInvalidInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		owner     string
		typeValue zone.RecordType
		ttl       uint32
		value     string
		wantField string
	}{
		{name: "unsupported SOA", owner: "www", typeValue: zone.TypeSOA, ttl: 300, value: "ignored", wantField: "type"},
		{name: "unknown type", owner: "www", typeValue: "BOGUS", ttl: 300, value: "ignored", wantField: "type"},
		{name: "apex CNAME", owner: "@", typeValue: zone.TypeCNAME, ttl: 300, value: "www", wantField: "name"},
		{name: "owner outside zone", owner: "www.example.net.", typeValue: zone.TypeA, ttl: 300, value: "192.0.2.1", wantField: "name"},
		{name: "misplaced wildcard", owner: "api.*", typeValue: zone.TypeA, ttl: 300, value: "192.0.2.1", wantField: "name"},
		{name: "multiple wildcards", owner: "*.api.*", typeValue: zone.TypeA, ttl: 300, value: "192.0.2.1", wantField: "name"},
		{name: "unicode owner", owner: "bücher", typeValue: zone.TypeA, ttl: 300, value: "192.0.2.1", wantField: "name"},
		{name: "TTL too large", owner: "www", typeValue: zone.TypeA, ttl: 2_147_483_648, value: "192.0.2.1", wantField: "ttl"},
		{name: "empty value", owner: "www", typeValue: zone.TypeA, ttl: 300, value: " ", wantField: "value"},
		{name: "invalid IPv4", owner: "www", typeValue: zone.TypeA, ttl: 300, value: "2001:db8::1", wantField: "value"},
		{name: "IPv4-mapped IPv6", owner: "www", typeValue: zone.TypeAAAA, ttl: 300, value: "::ffff:192.0.2.1", wantField: "value"},
		{name: "bad MX", owner: "@", typeValue: zone.TypeMX, ttl: 300, value: "70000 mail", wantField: "value"},
		{name: "bad SRV", owner: "_svc._tcp", typeValue: zone.TypeSRV, ttl: 300, value: "1 2 70000 target", wantField: "value"},
		{name: "bad CAA", owner: "@", typeValue: zone.TypeCAA, ttl: 300, value: "256 issue example.net", wantField: "value"},
		{name: "unicode target", owner: "alias", typeValue: zone.TypeCNAME, ttl: 300, value: "bücher.example", wantField: "value"},
		{name: "owner with empty label", owner: "bad..name", typeValue: zone.TypeA, ttl: 300, value: "192.0.2.1", wantField: "name"},
		{name: "owner with zone file comment", owner: ";", typeValue: zone.TypeA, ttl: 300, value: "192.0.2.1", wantField: "name"},
		{name: "target with empty label", owner: "alias", typeValue: zone.TypeCNAME, ttl: 300, value: "bad..target.", wantField: "value"},
		{name: "target with whitespace", owner: "alias", typeValue: zone.TypeCNAME, ttl: 300, value: "bad target.example", wantField: "value"},
		{name: "TXT record exceeds wire budget", owner: "txt", typeValue: zone.TypeTXT, ttl: 300, value: strings.Repeat("x", zone.MaxRecordWireSize), wantField: "value"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := zone.NormalizeRecord("example.com", tt.owner, tt.typeValue, tt.ttl, tt.value)
			assertValidationField(t, err, tt.wantField)
		})
	}
}

func TestCompileRejectsEmptyZoneFileInput(t *testing.T) {
	t.Parallel()

	_, err := zone.Compile("example.com", zone.Record{
		Name:  ";",
		Type:  zone.TypeA,
		TTL:   300,
		Value: "192.0.2.1",
	})
	if err == nil {
		t.Fatal("Compile accepted an owner parsed as an empty zone-file record")
	}
}

func assertValidationField(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected validation error for field %q", want)
	}
	var validationErr *zone.ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("error = %T %v, want *zone.ValidationError", err, err)
	}
	if validationErr.Field != want {
		t.Errorf("validation field = %q, want %q", validationErr.Field, want)
	}
}

// TestNormalizeRecordStoresWhatIsServed: a character-string value accepts input
// the compiler silently drops, so the stored string could carry bytes the RR
// does not. That made the RRset duplicate check, the planner's TXT comparison
// and the raw zone table all disagree with the resolver, and let one RRset hold
// two byte-identical TXT records — which RFC 2181 §5 forbids and which makes
// SPF a permanent permerror.
func TestNormalizeRecordStoresWhatIsServed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		kind  zone.RecordType
		input string
		want  string
	}{
		{
			name:  "a BIND line pasted with its trailing comment",
			kind:  zone.TypeTXT,
			input: `"v=spf1 mx -all" ; do not remove`,
			want:  `"v=spf1 mx -all"`,
		},
		{
			name:  "an already canonical TXT is untouched",
			kind:  zone.TypeTXT,
			input: `"v=spf1 mx -all"`,
			want:  `"v=spf1 mx -all"`,
		},
		{
			name:  "the split form of a long key an import produces",
			kind:  zone.TypeTXT,
			input: `"v=DKIM1; k=rsa; " "p=MIGfMA0GCSqGSIb3DQ"`,
			want:  `"v=DKIM1; k=rsa; " "p=MIGfMA0GCSqGSIb3DQ"`,
		},
		{
			name:  "unquoted text is still quoted",
			kind:  zone.TypeTXT,
			input: `hello world`,
			want:  `"hello world"`,
		},
		{
			name:  "a CAA tag is case-insensitive",
			kind:  zone.TypeCAA,
			input: `0 ISSUE "letsencrypt.org"`,
			want:  `0 issue "letsencrypt.org"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			record, err := zone.NormalizeRecord("example.com", "@", tt.kind, 3600, tt.input)
			if err != nil {
				t.Fatalf("NormalizeRecord: %v", err)
			}
			if record.Value != tt.want {
				t.Errorf("stored value = %q, want %q", record.Value, tt.want)
			}
			// Canonicality has to be idempotent: ValidateSnapshot re-normalises
			// every stored record and refuses a zone that does not round-trip.
			again, err := zone.NormalizeImportedRecord("example.com", "@", tt.kind, 3600, record.Value)
			if err != nil {
				t.Fatalf("re-normalising %q: %v", record.Value, err)
			}
			if again.Value != record.Value {
				t.Errorf("re-normalising %q gave %q; normalisation is not idempotent", record.Value, again.Value)
			}
		})
	}
}
