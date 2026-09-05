package mail

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/castlemilk/dns/internal/enginedns"
)

// probeZoneFile is the exact text a v0.16.20 server rendered for a domain with
// both DKIM algorithms. It is shared with the client package's fixtures so one
// recording drives both the parser and the transport tests.
func probeZoneFile(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("stalwart", "testdata", "zonefile_probe.txt"))
	if err != nil {
		t.Fatalf("read the recorded zone file: %v", err)
	}
	return string(raw)
}

func TestZoneFileKeepsOnlyTheDkimRecords(t *testing.T) {
	active := map[string]struct{}{
		"v1-ed25519-20260903": {},
		"v1-rsa-20260903":     {},
	}
	records, invalid := parseDkimRecords(probeZoneFile(t), "probe.test", active)
	if len(invalid) != 0 {
		t.Fatalf("valid keys were rejected: %v", invalid)
	}
	if len(records) != 2 {
		t.Fatalf("parsed %d records: %#v", len(records), records)
	}

	var names []string
	for _, record := range records {
		names = append(names, record.Name)
	}
	slices.Sort(names)
	want := []string{"v1-ed25519-20260903._domainkey", "v1-rsa-20260903._domainkey"}
	if !slices.Equal(names, want) {
		t.Errorf("names = %v, want %v", names, want)
	}

	for _, record := range records {
		if !strings.HasPrefix(record.Value, "v=DKIM1;") {
			t.Errorf("%s value = %q", record.Name, record.Value)
		}
		if strings.Contains(record.Value, `"`) {
			t.Errorf("%s value is still quoted: %q", record.Name, record.Value)
		}
	}
}

// The RSA key arrives wrapped in parentheses as two character-strings; the
// parser must concatenate them into the text a resolver sees.
func TestZoneFileJoinsTheParenthesisedRsaKey(t *testing.T) {
	active := map[string]struct{}{"v1-rsa-20260903": {}}
	records, _ := parseDkimRecords(probeZoneFile(t), "probe.test", active)
	if len(records) != 1 {
		t.Fatalf("parsed %d records", len(records))
	}
	value := records[0].Value
	if len(value) < 400 {
		t.Fatalf("the split strings were not joined: %d characters", len(value))
	}
	if strings.Contains(value, `" "`) || strings.Contains(value, "(") || strings.Contains(value, ")") {
		t.Errorf("value carries the wrapper: %q", value)
	}
	if !strings.HasSuffix(value, "IDAQAB") {
		t.Errorf("value ends %q", value[len(value)-12:])
	}
	// The same key stored as one string and as two must compare equal, or the
	// planner would publish a duplicate.
	split := `"` + value[:200] + `" "` + value[200:] + `"`
	if !enginedns.TxtEqual(split, `"`+value+`"`) {
		t.Error("the split and joined forms are not equal")
	}
}

// Everything the deployment cannot honestly serve is dropped: MX, SPF and DMARC
// come from configuration, and SRV/MTA-STS/autoconfig/TLS-RPT need a
// certificate covering per-customer names.
func TestZoneFileDropsEveryNonDkimLine(t *testing.T) {
	active := map[string]struct{}{
		"v1-ed25519-20260903": {},
		"v1-rsa-20260903":     {},
	}
	records, _ := parseDkimRecords(probeZoneFile(t), "probe.test", active)
	for _, record := range records {
		if !strings.Contains(record.Name, "._domainkey") {
			t.Errorf("a non-DKIM record survived: %#v", record)
		}
	}
}

// The server emits an SPF record for the domain that owns its own hostname.
// That owner is outside the customer's zone and must never be published in it.
func TestZoneFileIgnoresOtherOwners(t *testing.T) {
	text := probeZoneFile(t) + "\n" +
		"v1-rsa-20260903._domainkey.other.test. IN TXT \"v=DKIM1; k=rsa; h=sha256; p=AAAA\"\n"
	active := map[string]struct{}{"v1-rsa-20260903": {}}
	records, _ := parseDkimRecords(text, "probe.test", active)
	if len(records) != 1 {
		t.Fatalf("parsed %d records: %#v", len(records), records)
	}
	if strings.Contains(records[0].Value, "p=AAAA") {
		t.Error("a record for another zone was adopted")
	}
}

func TestZoneFileSkipsSelectorsThatAreNotActive(t *testing.T) {
	records, _ := parseDkimRecords(probeZoneFile(t), "probe.test", map[string]struct{}{"v1-rsa-20260903": {}})
	if len(records) != 1 || !strings.Contains(records[0].Name, "v1-rsa") {
		t.Errorf("records = %#v", records)
	}
}

// A malformed key is reported, not published: receivers treat a broken DKIM
// record as a failed signature, which is worse than an unsigned message.
func TestZoneFileReportsInvalidKeys(t *testing.T) {
	text := "bad._domainkey.probe.test. IN TXT \"v=DKIM1; k=rsa; h=sha256; p=not base64!!\"\n" +
		"worse._domainkey.probe.test. IN TXT \"this is not a dkim record\"\n" +
		"cipher._domainkey.probe.test. IN TXT \"v=DKIM1; k=quantum; h=sha256; p=AAAA\"\n"
	active := map[string]struct{}{"bad": {}, "worse": {}, "cipher": {}}
	records, invalid := parseDkimRecords(text, "probe.test", active)
	if len(records) != 0 {
		t.Errorf("published %#v", records)
	}
	slices.Sort(invalid)
	if !slices.Equal(invalid, []string{"bad", "cipher", "worse"}) {
		t.Errorf("invalid = %v", invalid)
	}
}

func TestValidDkimValue(t *testing.T) {
	cases := []struct {
		value string
		want  bool
	}{
		{"v=DKIM1; k=ed25519; h=sha256; p=5t9iUKLc7NJ5SmfF8N9DBNAn74vuywt3j5mCnUDQmCM=", true},
		{"v=DKIM1; k=rsa; p=MIIBIjANBgkq", true},
		{"k=rsa; v=DKIM1; p=MIIBIjANBgkq", false}, // v must come first
		{"v=DKIM2; k=rsa; p=MIIBIjANBgkq", false},
		{"v=DKIM1; k=rsa; p=", false},
		{"v=DKIM1; p=MIIBIjANBgkq", false}, // no key type
		{"v=spf1 mx -all", false},
		{"", false},
	}
	for _, testCase := range cases {
		if got := validDkimValue(testCase.value); got != testCase.want {
			t.Errorf("validDkimValue(%q) = %v, want %v", testCase.value, got, testCase.want)
		}
	}
}

// A comment inside a quoted string is data, not a comment: DKIM values are full
// of semicolons.
func TestZoneFileDoesNotTreatSemicolonsInsideAValueAsComments(t *testing.T) {
	text := "sel._domainkey.probe.test. IN TXT \"v=DKIM1; k=rsa; h=sha256; p=AAAA\" ; a real comment\n"
	records, invalid := parseDkimRecords(text, "probe.test", map[string]struct{}{"sel": {}})
	if len(invalid) != 0 {
		t.Fatalf("invalid = %v", invalid)
	}
	if len(records) != 1 || records[0].Value != "v=DKIM1; k=rsa; h=sha256; p=AAAA" {
		t.Errorf("records = %#v", records)
	}
}

func TestZoneFileAcceptsAnExplicitTtlAndClassOrder(t *testing.T) {
	text := "sel._domainkey.probe.test. 3600 IN TXT \"v=DKIM1; k=rsa; p=AAAA\"\n"
	records, _ := parseDkimRecords(text, "probe.test", map[string]struct{}{"sel": {}})
	if len(records) != 1 {
		t.Fatalf("records = %#v", records)
	}
}
