package maild

import (
	"errors"
	"strconv"
	"strings"
	"testing"
)

func TestParseAddressAcceptsValidAddresses(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"plain", "ada@acme.dev", "ada@acme.dev"},
		{"case is folded", "Ada.Lovelace@ACME.Dev", "ada.lovelace@acme.dev"},
		{"sub addressing", "ada+newsletter@acme.dev", "ada+newsletter@acme.dev"},
		{"dotted local part", "ada.b.c@acme.dev", "ada.b.c@acme.dev"},
		{"every atext character", "!#$%&'*+-/=?^_`{|}~a0@acme.dev", "!#$%&'*+-/=?^_`{|}~a0@acme.dev"},
		{"single label domain", "root@localhost", "root@localhost"},
		{"hyphenated label", "ada@mail-1.acme.dev", "ada@mail-1.acme.dev"},
		{"digit leading label", "ada@1acme.dev", "ada@1acme.dev"},
		{"deep subdomain", "ada@a.b.c.d.acme.dev", "ada@a.b.c.d.acme.dev"},
		{"surrounding spaces are stripped", "  ada@acme.dev ", "ada@acme.dev"},
		{"longest local part", strings.Repeat("a", MaxLocalPartBytes) + "@acme.dev",
			strings.Repeat("a", MaxLocalPartBytes) + "@acme.dev"},
		{"longest label", "ada@" + strings.Repeat("b", MaxDomainLabelBytes) + ".dev",
			"ada@" + strings.Repeat("b", MaxDomainLabelBytes) + ".dev"},
		{"longest address", strings.Repeat("a", MaxLocalPartBytes) + "@" + longestDomain(),
			strings.Repeat("a", MaxLocalPartBytes) + "@" + longestDomain()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			address, err := ParseAddress(test.input)
			if err != nil {
				t.Fatalf("ParseAddress(%q) = %v, want it accepted", test.input, err)
			}
			if got := address.String(); got != test.want {
				t.Fatalf("ParseAddress(%q).String() = %q, want %q", test.input, got, test.want)
			}
			local, domain, ok := strings.Cut(test.want, "@")
			if !ok || address.Local != local || address.Domain != domain {
				t.Fatalf("ParseAddress(%q) = {%q, %q}, want {%q, %q}",
					test.input, address.Local, address.Domain, local, domain)
			}
			if address.IsZero() {
				t.Fatal("an accepted address reports IsZero")
			}
			if !ValidAddress(test.input) {
				t.Fatalf("ValidAddress(%q) = false, want true", test.input)
			}
			normalized, err := NormalizeAddress(test.input)
			if err != nil {
				t.Fatalf("NormalizeAddress(%q) = %v", test.input, err)
			}
			if normalized != test.want {
				t.Fatalf("NormalizeAddress(%q) = %q, want %q", test.input, normalized, test.want)
			}
		})
	}
}

// TestParseAddressRefusesHostileInput is the table that matters. Every entry is
// a string that has, somewhere, been delivered to a mail server by someone who
// meant it. A pass here is a header injection, an ambiguous store key or an
// unbounded allocation.
func TestParseAddressRefusesHostileInput(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"empty", ""},
		{"only spaces", "   "},
		{"bare CR", "ada@acme.dev\r"},
		{"bare LF", "ada@acme.dev\n"},
		{"CRLF and an injected header", "ada@acme.dev\r\nBcc: mallory@evil.test"},
		{"CR inside the local part", "ada\rlovelace@acme.dev"},
		{"LF inside the domain", "ada@acme\n.dev"},
		{"leading CRLF", "\r\nada@acme.dev"},
		{"NUL", "ada@acme.dev\x00"},
		{"NUL inside", "ada\x00@acme.dev"},
		{"tab", "ada\t@acme.dev"},
		{"vertical tab", "ada@acme.dev\v"},
		{"DEL", "ada@acme.dev\x7f"},
		{"escape", "ada@acme.dev\x1b[0m"},
		{"angle brackets", "<ada@acme.dev>"},
		{"display name", "Ada Lovelace <ada@acme.dev>"},
		{"two addresses", "ada@acme.dev, mallory@evil.test"},
		{"semicolon separated", "ada@acme.dev;mallory@evil.test"},
		{"no at", "ada"},
		{"two ats", "ada@acme@dev"},
		{"doubled at", "ada@@acme.dev"},
		{"three ats", "a@b@c@d"},
		{"empty local part", "@acme.dev"},
		{"empty domain", "ada@"},
		{"leading dot in the local part", ".ada@acme.dev"},
		{"trailing dot in the local part", "ada.@acme.dev"},
		{"doubled dot in the local part", "ada..lovelace@acme.dev"},
		{"only a dot", ".@acme.dev"},
		{"leading dot in the domain", "ada@.acme.dev"},
		{"trailing root dot", "ada@acme.dev."},
		{"doubled dot in the domain", "ada@acme..dev"},
		{"label starts with a hyphen", "ada@-acme.dev"},
		{"label ends with a hyphen", "ada@acme-.dev"},
		{"underscore in the domain", "ada@acme_mail.dev"},
		{"address literal", "ada@[192.0.2.1]"},
		{"space in the local part", "ada lovelace@acme.dev"},
		{"space in the domain", "ada@acme dev"},
		{"quoted local part", `"ada lovelace"@acme.dev`},
		{"comment", "ada(the first programmer)@acme.dev"},
		{"backslash", `ada\@acme.dev`},
		{"colon route", "@relay.test:ada@acme.dev"},
		{"300 character local part", strings.Repeat("a", 300) + "@acme.dev"},
		{"65 character local part", strings.Repeat("a", MaxLocalPartBytes+1) + "@acme.dev"},
		{"64 character label", "ada@" + strings.Repeat("b", MaxDomainLabelBytes+1) + ".dev"},
		{"over long address", strings.Repeat("a", MaxLocalPartBytes) + "@" + longestDomain() + "b"},
		{"unicode confusable a", "аda@acme.dev"},
		{"unicode confusable domain", "ada@аcme.dev"},
		{"unicode full width at", "ada＠acme.dev"},
		{"zero width space", "ada\u200b@acme.dev"},
		{"non breaking space", "ada lovelace@acme.dev"},
		{"invalid utf8", "ada\xff@acme.dev"},
		{"idn label", "ada@bücher.dev"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			address, err := ParseAddress(test.input)
			if err == nil {
				t.Fatalf("ParseAddress(%q) = %q, want a refusal", test.input, address)
			}
			if !errors.Is(err, ErrInvalidAddress) {
				t.Fatalf("ParseAddress(%q) = %v, want it to wrap ErrInvalidAddress", test.input, err)
			}
			if address != (Address{}) {
				t.Fatalf("ParseAddress(%q) returned %#v alongside an error", test.input, address)
			}
			if ValidAddress(test.input) {
				t.Fatalf("ValidAddress(%q) = true, want false", test.input)
			}
			normalized, err := NormalizeAddress(test.input)
			if err == nil || normalized != "" {
				t.Fatalf("NormalizeAddress(%q) = %q, %v; want \"\" and a refusal",
					test.input, normalized, err)
			}
		})
	}
}

// TestParseAddressRefusesControlCharactersAsSuch pins the order of the checks,
// not just the outcome. The printable-ASCII scan runs over the whole string
// before it is split, so a control character is refused for being one wherever
// it appears — including in a position where the dot-atom or LDH rule would
// have caught it too. That redundancy is deliberate; this test is what stops
// the outer check from being quietly deleted as "already covered", after which
// the next relaxation of the inner one reopens header injection.
func TestParseAddressRefusesControlCharactersAsSuch(t *testing.T) {
	for _, hostile := range []string{
		"ada@acme.dev\r",
		"ada@acme.dev\n",
		"ada@acme.dev\r\nBcc: mallory@evil.test",
		"\rada@acme.dev",
		"ada\rlovelace@acme.dev",
		"ada@acme\r.dev",
		"ada@acme.dev\x00",
		"ada\x00@acme.dev",
		"ada\t@acme.dev",
		"ada@acme.dev\x7f",
		"ada@acme.dev\x1b",
		"ada\u200b@acme.dev",
		"аda@acme.dev",
		"ada\xff@acme.dev",
	} {
		t.Run(strconv.Quote(hostile), func(t *testing.T) {
			_, err := ParseAddress(hostile)
			if !errors.Is(err, errAddressNotASCII) {
				t.Fatalf("ParseAddress(%q) = %v, want the printable-ASCII refusal", hostile, err)
			}
		})
	}
	// A space is not a control character, and it is the one octet outside the
	// printable range that is stripped at the ends rather than refused.
	if _, err := ParseAddress("ada lovelace@acme.dev"); !errors.Is(err, errAddressNotASCII) {
		t.Fatalf("an interior space = %v, want the printable-ASCII refusal", err)
	}
	if _, err := ParseAddress("  ada@acme.dev  "); err != nil {
		t.Fatalf("surrounding spaces were refused: %v", err)
	}
}

// TestAddressRefusalNeverQuotesTheInput is the log-injection half of the same
// rule: a refusal reaches an SMTP reply and a log line, so if it echoed the
// rejected string the CR it just refused would arrive there instead.
func TestAddressRefusalNeverQuotesTheInput(t *testing.T) {
	const hostile = "mallor\ry\x00-marker@evil.test"
	_, err := ParseAddress(hostile)
	if err == nil {
		t.Fatal("the hostile address was accepted")
	}
	for _, fragment := range []string{"mallor", "marker", "evil.test", "\r", "\x00"} {
		if strings.Contains(err.Error(), fragment) {
			t.Fatalf("the refusal %q quotes the rejected input %q", err.Error(), fragment)
		}
	}
}

func TestAddressZeroValue(t *testing.T) {
	var zero Address
	if !zero.IsZero() {
		t.Fatal("the zero Address does not report IsZero")
	}
	if got := zero.String(); got != "" {
		t.Fatalf("Address{}.String() = %q, want \"\"", got)
	}
	if got := (Address{Local: "ada"}).String(); got != "" {
		t.Fatalf("a half-built Address rendered %q, want \"\"", got)
	}
}

// TestAddressCanonicalFormIsTheStoreKey pins the canonical form to the one the
// store already derives its ids from. If these ever disagree, an account
// created through JMAP is not the account SMTP delivers to.
func TestAddressCanonicalFormIsTheStoreKey(t *testing.T) {
	for _, raw := range []string{"Ada@Acme.Dev", " ada@acme.dev ", "ADA@ACME.DEV"} {
		address, err := ParseAddress(raw)
		if err != nil {
			t.Fatalf("ParseAddress(%q) = %v", raw, err)
		}
		local, domain, ok := SplitAddress(raw)
		if !ok {
			t.Fatalf("SplitAddress(%q) refused an address ParseAddress accepted", raw)
		}
		if address.Local != local || address.Domain != domain {
			t.Fatalf("ParseAddress(%q) = {%q, %q} but SplitAddress = {%q, %q}",
				raw, address.Local, address.Domain, local, domain)
		}
	}
}

func TestParseDomainName(t *testing.T) {
	valid := map[string]string{
		"acme.dev":                     "acme.dev",
		"ACME.Dev":                     "acme.dev",
		"  acme.dev  ":                 "acme.dev",
		"mail-1.acme.dev":              "mail-1.acme.dev",
		"localhost":                    "localhost",
		"1acme.dev":                    "1acme.dev",
		"a.b.c.d.e.f.g.h.i.j.acme.dev": "a.b.c.d.e.f.g.h.i.j.acme.dev",
	}
	for input, want := range valid {
		t.Run("valid/"+input, func(t *testing.T) {
			got, err := ParseDomainName(input)
			if err != nil {
				t.Fatalf("ParseDomainName(%q) = %v, want it accepted", input, err)
			}
			if got != want {
				t.Fatalf("ParseDomainName(%q) = %q, want %q", input, got, want)
			}
		})
	}

	hostile := []struct {
		name  string
		input string
	}{
		{"empty", ""},
		{"spaces", "  "},
		{"CR", "acme.dev\r"},
		{"LF", "acme.dev\n"},
		{"CRLF injection", "acme.dev\r\nv=spf1 +all"},
		{"NUL", "acme.dev\x00"},
		{"tab", "acme\t.dev"},
		{"leading dot", ".acme.dev"},
		{"root dot", "acme.dev."},
		{"doubled dot", "acme..dev"},
		{"leading hyphen", "-acme.dev"},
		{"trailing hyphen", "acme-.dev"},
		{"underscore", "_dmarc_acme.dev"},
		{"an address, not a domain", "ada@acme.dev"},
		{"space inside", "acme dev"},
		{"unicode", "bücher.dev"},
		{"over long label", strings.Repeat("b", MaxDomainLabelBytes+1) + ".dev"},
		{"over long domain", strings.Repeat("b.", 128) + "dev"},
	}
	for _, test := range hostile {
		t.Run("hostile/"+test.name, func(t *testing.T) {
			got, err := ParseDomainName(test.input)
			if err == nil {
				t.Fatalf("ParseDomainName(%q) = %q, want a refusal", test.input, got)
			}
			if !errors.Is(err, ErrInvalidAddress) {
				t.Fatalf("ParseDomainName(%q) = %v, want it to wrap ErrInvalidAddress", test.input, err)
			}
			if got != "" {
				t.Fatalf("ParseDomainName(%q) returned %q alongside an error", test.input, got)
			}
		})
	}
}

// TestParseAddressIsStricterThanPlausibleAddress records the relationship
// between the new gate and the SMTP-side syntax check it is meant to replace:
// everything ParseAddress accepts, plausibleAddress accepts too. The reverse
// does not hold, which is the point.
func TestParseAddressIsStricterThanPlausibleAddress(t *testing.T) {
	accepted := []string{
		"ada@acme.dev",
		"ada+news@acme.dev",
		"a@b",
		"ada.lovelace@mail-1.acme.dev",
		"!#$%&'*+-/=?^_`{|}~@acme.dev",
	}
	for _, raw := range accepted {
		if !ValidAddress(raw) {
			t.Fatalf("ValidAddress(%q) = false", raw)
		}
		if !plausibleAddress(raw) {
			t.Fatalf("plausibleAddress(%q) = false but ParseAddress accepted it", raw)
		}
	}
	// Shapes the old gate lets through and this one does not.
	for _, raw := range []string{"ada@[192.0.2.1]", "ada\u200b@acme.dev", ".ada@acme.dev", `"a b"@acme.dev`} {
		if ValidAddress(raw) {
			t.Fatalf("ValidAddress(%q) = true, want false", raw)
		}
	}
}

// longestDomain builds the longest domain that still fits an address of
// MaxAddressBytes octets beside a maximum-length local part, out of labels that
// are each within MaxDomainLabelBytes. It is a helper rather than a literal so
// that the boundary is computed from the constants it is testing.
func longestDomain() string {
	const room = MaxAddressBytes - MaxLocalPartBytes - 1
	var labels []string
	for left := room; left > 0; {
		size := min(left, MaxDomainLabelBytes)
		if left-size == 1 {
			// One octet left over would have to be a dot with no label after
			// it. Take one less now so the remainder is a label of its own.
			size--
		}
		labels = append(labels, strings.Repeat("b", size))
		left -= size + 1
	}
	return strings.Join(labels, ".")
}
