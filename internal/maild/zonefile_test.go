package maild

import (
	"strconv"
	"strings"
	"testing"
)

const (
	testAutoconfigZone = "acme.dev"
	testAutoconfigHost = "mail.example.net"
)

// facadeAllowList is CONTRACT.md §4.3 and internal/mail/zonefile.go's
// autoconfigSRVOwners + autoconfigCNAMEOwners, written out here on purpose. The
// facade publishes nothing whose owner is not on this list, so an owner
// invented in autoconfigEntries would be dropped in silence; this copy turns
// that into a failing test instead.
var facadeAllowList = []struct {
	owner string
	kind  string
}{
	{"_submissions._tcp", "SRV"},
	{"_imaps._tcp", "SRV"},
	{"_pop3s._tcp", "SRV"},
	{"_jmap._tcp", "SRV"},
	{"_caldavs._tcp", "SRV"},
	{"_carddavs._tcp", "SRV"},
	{"autoconfig", "CNAME"},
	{"autodiscover", "CNAME"},
}

// TestAutoconfigEntriesCoverTheFacadeAllowListExactly. Every owner the facade
// will accept must have an answer here — published or explicitly withheld — and
// no entry may name an owner the facade does not accept.
func TestAutoconfigEntriesCoverTheFacadeAllowListExactly(t *testing.T) {
	if len(autoconfigEntries) != len(facadeAllowList) {
		t.Fatalf("autoconfigEntries has %d owners, the facade allows %d", len(autoconfigEntries), len(facadeAllowList))
	}
	for index, want := range facadeAllowList {
		got := autoconfigEntries[index]
		if got.owner != want.owner {
			t.Errorf("entry %d is %q, the facade's allow-list has %q", index, got.owner, want.owner)
		}
		if got.kind != want.kind {
			t.Errorf("%s is rendered as %s, the facade parses it as %s", got.owner, got.kind, want.kind)
		}
		// A withheld entry with no reason is a record silently missing from a
		// customer's zone with nothing to explain it.
		if !got.offered && strings.TrimSpace(got.withheld) == "" {
			t.Errorf("%s is withheld with no reason given", got.owner)
		}
		if got.offered && got.withheld != "" {
			t.Errorf("%s is both published and marked withheld", got.owner)
		}
	}
}

// TestAutoconfigPublishesOnlyTheServicesThisServerListensOn is the honesty
// rule. maild serves submission and POP3; it serves no IMAP, no JMAP mail
// session, no CalDAV and no CardDAV, and no autoconfiguration document over
// HTTPS. Publishing an SRV for one of those does not degrade to "the client
// tries something else": the client believes the record, dials a port nothing
// answers on, and calls the account broken.
func TestAutoconfigPublishesOnlyTheServicesThisServerListensOn(t *testing.T) {
	rendered := AutoconfigZoneRecords(testAutoconfigZone, testAutoconfigHost)

	want := []string{
		"_submissions._tcp." + testAutoconfigZone + ". IN SRV 0 1 465 " + testAutoconfigHost + ".",
		"_pop3s._tcp." + testAutoconfigZone + ". IN SRV 0 1 995 " + testAutoconfigHost + ".",
	}
	got := publishedLines(rendered)
	if len(got) != len(want) {
		t.Fatalf("published %d records, want %d:\n%s", len(got), len(want), rendered)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Errorf("published record %d = %q, want %q", index, got[index], want[index])
		}
	}
}

// TestAutoconfigWithheldRecordsAreCommentsNotRecords. The withheld owners must
// still appear in the file — an operator debugging "Thunderbird will not
// configure itself" needs to see that the decision was deliberate — but every
// line naming one must be a comment, because internal/mail/zonefile.go cuts a
// line at the first unquoted ';' and would otherwise publish it.
func TestAutoconfigWithheldRecordsAreCommentsNotRecords(t *testing.T) {
	rendered := AutoconfigZoneRecords(testAutoconfigZone, testAutoconfigHost)

	for _, entry := range autoconfigEntries {
		if entry.offered {
			continue
		}
		owner := entry.owner + "." + testAutoconfigZone
		if !strings.Contains(rendered, owner) {
			t.Errorf("%s is withheld without saying so; the file does not mention it", entry.owner)
		}
		if !strings.Contains(rendered, entry.withheld) {
			t.Errorf("%s is withheld without its reason", entry.owner)
		}
		for _, line := range strings.Split(rendered, "\n") {
			if !strings.Contains(line, owner) {
				continue
			}
			if !strings.HasPrefix(strings.TrimSpace(line), ";") {
				t.Errorf("%s appears on a line the parser would publish: %q", entry.owner, line)
			}
		}
	}
}

// TestAutoconfigRecordsAreWellFormedForTheFacadesParser applies
// autoconfigValue's rules from internal/mail/zonefile.go: an SRV needs exactly
// four rdata fields with a numeric priority, weight and port, a CNAME exactly
// one, and either way the target must be MAIL_HOSTNAME with a trailing dot. A
// record that fails any of these is dropped in silence.
func TestAutoconfigRecordsAreWellFormedForTheFacadesParser(t *testing.T) {
	rendered := AutoconfigZoneRecords(testAutoconfigZone, testAutoconfigHost)
	suffix := "." + testAutoconfigZone + "."
	target := testAutoconfigHost + "."

	published := publishedLines(rendered)
	if len(published) == 0 {
		t.Fatal("nothing was published, so there is nothing to configure a client with")
	}
	for _, line := range published {
		fields := strings.Fields(line)
		// owner, class, type, then the rdata.
		if len(fields) < 4 {
			t.Errorf("%q is not a record", line)
			continue
		}
		owner, class, kind, rdata := fields[0], fields[1], fields[2], fields[3:]
		// The facade drops any owner that is not absolute and under the zone;
		// that is how it ignores the mail host's own lines.
		if !strings.HasSuffix(owner, suffix) {
			t.Errorf("owner %q is not absolute under %s", owner, testAutoconfigZone)
		}
		if class != "IN" {
			t.Errorf("%q has class %q, want IN", line, class)
		}
		switch kind {
		case "SRV":
			if len(rdata) != 4 {
				t.Errorf("%q has %d SRV fields, the parser requires 4", line, len(rdata))
				continue
			}
			for _, field := range rdata[:3] {
				if _, err := strconv.ParseUint(field, 10, 16); err != nil {
					t.Errorf("%q has a non-numeric SRV field %q", line, field)
				}
			}
			if rdata[2] == "0" {
				t.Errorf("%q advertises port 0", line)
			}
			if rdata[3] != target {
				t.Errorf("%q targets %q, the facade keeps only %q", line, rdata[3], target)
			}
		case "CNAME":
			if len(rdata) != 1 {
				t.Errorf("%q has %d CNAME fields, the parser requires 1", line, len(rdata))
				continue
			}
			if rdata[0] != target {
				t.Errorf("%q targets %q, the facade keeps only %q", line, rdata[0], target)
			}
		default:
			t.Errorf("%q is a %s record; only SRV and CNAME are on the allow-list", line, kind)
		}
	}
}

// TestAutoconfigRecordsNormaliseTheirNames. The facade compares the target
// against MAIL_HOSTNAME and the owner against the zone; a stray trailing dot,
// stray whitespace or capital letter in either name is how a whole block ends
// up silently unpublished.
func TestAutoconfigRecordsNormaliseTheirNames(t *testing.T) {
	rendered := AutoconfigZoneRecords("  ACME.dev.  ", "Mail.Example.NET.")
	want := "_pop3s._tcp.acme.dev. IN SRV 0 1 995 mail.example.net."
	if !strings.Contains(rendered, want) {
		t.Errorf("rendered\n%s\nwant a line %q", rendered, want)
	}
	if strings.Contains(rendered, "..") {
		t.Errorf("a name was double-dotted:\n%s", rendered)
	}
}

// TestAutoconfigRecordsRefuseAnEmptyName. Rendering with no zone or no
// hostname would emit records rooted at "." or targeting ".", which is a
// wildcard instruction to every mail client that reads it.
func TestAutoconfigRecordsRefuseAnEmptyName(t *testing.T) {
	for _, testCase := range []struct{ name, domain, hostname string }{
		{"no domain", "", testAutoconfigHost},
		{"no hostname", testAutoconfigZone, ""},
		{"neither", "", ""},
		{"whitespace", "   ", "   "},
		{"root dot", ".", "."},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := AutoconfigZoneRecords(testCase.domain, testCase.hostname); got != "" {
				t.Errorf("rendered %q, want nothing", got)
			}
		})
	}
}

// TestAppendAutoconfigRecordsJoinsOnItsOwnLine. The DKIM renderer terminates
// its output, but the hook is a deployment's to supply; concatenating a block
// onto an unterminated last line would corrupt the DKIM record above it rather
// than add a record below.
func TestAppendAutoconfigRecordsJoinsOnItsOwnLine(t *testing.T) {
	dkim := `v1-rsa-20260906._domainkey.acme.dev. IN TXT "v=DKIM1; k=rsa; h=sha256; p=AAAA"`

	for _, testCase := range []struct{ name, zone string }{
		{"unterminated", dkim},
		{"terminated", dkim + "\n"},
		{"empty", ""},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got := AppendAutoconfigRecords(testCase.zone, testAutoconfigZone, testAutoconfigHost)
			if testCase.zone != "" && !strings.Contains(got, dkim+"\n") {
				t.Errorf("the DKIM record lost its line ending:\n%s", got)
			}
			for _, line := range publishedLines(got) {
				if strings.Count(line, " IN ") != 1 {
					t.Errorf("two records share a line: %q", line)
				}
			}
			if !strings.Contains(got, "_pop3s._tcp."+testAutoconfigZone+".") {
				t.Errorf("the block was not appended:\n%s", got)
			}
		})
	}
}

// TestAppendAutoconfigRecordsLeavesTheZoneAloneWhenItCannotRender. A domain or
// hostname it cannot use must not cost the caller its DKIM records.
func TestAppendAutoconfigRecordsLeavesTheZoneAloneWhenItCannotRender(t *testing.T) {
	dkim := "v1-rsa-20260906._domainkey.acme.dev. IN TXT \"v=DKIM1; k=rsa; h=sha256; p=AAAA\"\n"
	if got := AppendAutoconfigRecords(dkim, testAutoconfigZone, ""); got != dkim {
		t.Errorf("AppendAutoconfigRecords with no hostname returned %q", got)
	}
}

// publishedLines returns the lines a resolver would load: comments stripped the
// way internal/mail/zonefile.go's indexOfComment strips them, blanks dropped.
func publishedLines(rendered string) []string {
	var lines []string
	for _, line := range strings.Split(rendered, "\n") {
		if index := strings.IndexByte(line, ';'); index >= 0 {
			line = line[:index]
		}
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			lines = append(lines, trimmed)
		}
	}
	return lines
}
