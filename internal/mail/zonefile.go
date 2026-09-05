package mail

import (
	"encoding/base64"
	"sort"
	"strconv"
	"strings"
)

// The mail server renders its wanted DNS as BIND zone text (there is no
// structured records API), so this file parses that text. It keeps two things:
// the `<selector>._domainkey` TXT records of the DKIM keys the server reports
// as active, and — when the binding asks for them — the client-autoconfiguration
// records listed in autoconfigSRVOwners and autoconfigCNAMEOwners.
//
// Everything else in that text is deliberately dropped:
//
//   - MX, SPF and DMARC are derived from configuration instead (§3.3), so a
//     changed or compromised mail server cannot redirect a customer's mail by
//     rewriting its own zone file;
//   - MTA-STS, TLS-RPT, the server's own ua-auto-config scheme and TLSA need an
//     HTTPS endpoint under the customer's own name, or DANE keys, which this
//     deployment does not serve or hold; publishing them would advertise
//     endpoints that do not work. See the allow-list below for the detail;
//   - lines whose owner is another name entirely (the server emits an SPF
//     record for the domain that owns its own hostname) belong to a zone this
//     control plane may not even hold.

// dkimRecord is one parsed `<selector>._domainkey` TXT record.
type dkimRecord struct {
	// Selector is the owner's first label.
	Selector string
	// Name is the record owner relative to the zone
	// ("v1-rsa-20260903._domainkey").
	Name string
	// Value is the concatenated character-string text, unquoted. The caller
	// hands it to zone.NormalizeRecord, which quotes it.
	Value string
}

// parseDkimRecords extracts the DKIM TXT records for zoneName from the server's
// zone file text, keeping only the selectors in active and only values that
// parse as a DKIM key.
//
// The second result names the selectors whose value did not parse. They are
// reported (mail.records.invalid) rather than published: a malformed DKIM
// record is worse than a missing one, because receivers treat a broken key as a
// failed signature instead of an unsigned message.
func parseDkimRecords(zoneFile, zoneName string, active map[string]struct{}) ([]dkimRecord, []string) {
	var records []dkimRecord
	var invalid []string
	suffix := "." + strings.ToLower(strings.TrimSuffix(zoneName, ".")) + "."

	for _, entry := range parseZoneFile(zoneFile) {
		if !strings.EqualFold(entry.Type, "TXT") {
			continue
		}
		owner := strings.ToLower(entry.Owner)
		if !strings.HasSuffix(owner, suffix) {
			continue
		}
		name := strings.TrimSuffix(owner, suffix)
		selector, rest, found := strings.Cut(name, "._domainkey")
		if !found || rest != "" || selector == "" {
			continue
		}
		if _, wanted := active[selector]; !wanted {
			continue
		}
		value := joinCharacterStrings(entry.RData)
		if !validDkimValue(value) {
			invalid = append(invalid, selector)
			continue
		}
		records = append(records, dkimRecord{Selector: selector, Name: name, Value: value})
	}
	return records, invalid
}

// zoneEntry is one record line of the server's zone file.
type zoneEntry struct {
	Owner string
	Type  string
	RData []string
}

// parseZoneFile splits BIND-style zone text into records. The server's output
// has absolute owner names, no $ORIGIN, no $TTL, no per-record TTL and one
// record per line except for long TXT values, which are wrapped in parentheses
// across several lines. Anything it cannot make sense of is skipped rather than
// guessed at.
func parseZoneFile(text string) []zoneEntry {
	var entries []zoneEntry
	var pending string
	depth := 0

	for _, line := range strings.Split(text, "\n") {
		if index := indexOfComment(line); index >= 0 {
			line = line[:index]
		}
		if strings.TrimSpace(line) == "" && depth == 0 {
			continue
		}
		pending += " " + line
		depth += parenDepth(line)
		if depth > 0 {
			continue
		}
		statement := strings.TrimSpace(strings.NewReplacer("(", " ", ")", " ").Replace(pending))
		pending = ""
		depth = 0
		if entry, ok := parseZoneEntry(statement); ok {
			entries = append(entries, entry)
		}
	}
	return entries
}

func parseZoneEntry(statement string) (zoneEntry, bool) {
	fields := splitFields(statement)
	if len(fields) < 3 {
		return zoneEntry{}, false
	}
	owner := fields[0]
	rest := fields[1:]
	// Skip an optional TTL and the class, in either order, exactly as a zone
	// file may write them.
	for len(rest) > 0 {
		if _, err := strconv.ParseUint(rest[0], 10, 32); err == nil {
			rest = rest[1:]
			continue
		}
		if strings.EqualFold(rest[0], "IN") {
			rest = rest[1:]
			continue
		}
		break
	}
	if len(rest) < 2 {
		return zoneEntry{}, false
	}
	return zoneEntry{Owner: owner, Type: rest[0], RData: rest[1:]}, true
}

// splitFields splits on whitespace while keeping quoted character-strings
// intact, so a TXT value containing spaces stays one field.
func splitFields(statement string) []string {
	var fields []string
	var current strings.Builder
	quoted := false
	started := false

	flush := func() {
		if started {
			fields = append(fields, current.String())
			current.Reset()
			started = false
		}
	}
	for index := 0; index < len(statement); index++ {
		char := statement[index]
		switch {
		case quoted && char == '\\' && index+1 < len(statement):
			current.WriteByte(char)
			index++
			current.WriteByte(statement[index])
		case char == '"':
			quoted = !quoted
			started = true
			current.WriteByte(char)
		case !quoted && (char == ' ' || char == '\t' || char == '\r'):
			flush()
		default:
			started = true
			current.WriteByte(char)
		}
	}
	flush()
	return fields
}

// joinCharacterStrings concatenates the quoted parts of a TXT rdata into the
// text a resolver would see. The server splits a long key into 255-byte
// character-strings; both forms carry identical bytes on the wire.
func joinCharacterStrings(rdata []string) string {
	var text strings.Builder
	for _, field := range rdata {
		if strings.HasPrefix(field, `"`) && strings.HasSuffix(field, `"`) && len(field) >= 2 {
			if unquoted, err := strconv.Unquote(field); err == nil {
				text.WriteString(unquoted)
				continue
			}
			text.WriteString(field[1 : len(field)-1])
			continue
		}
		text.WriteString(field)
	}
	return text.String()
}

// indexOfComment finds a `;` that starts a comment, ignoring one inside a
// quoted string — DKIM values are full of semicolons.
func indexOfComment(line string) int {
	quoted := false
	for index := 0; index < len(line); index++ {
		switch line[index] {
		case '\\':
			index++
		case '"':
			quoted = !quoted
		case ';':
			if !quoted {
				return index
			}
		}
	}
	return -1
}

func parenDepth(line string) int {
	depth := 0
	quoted := false
	for index := 0; index < len(line); index++ {
		switch line[index] {
		case '\\':
			index++
		case '"':
			quoted = !quoted
		case '(':
			if !quoted {
				depth++
			}
		case ')':
			if !quoted {
				depth--
			}
		}
	}
	return depth
}

// validDkimValue reports whether text is a DKIM public-key record this facade
// is willing to publish: v=DKIM1, a key type it recognises, and a non-empty
// base64 public key.
func validDkimValue(text string) bool {
	tags := map[string]string{}
	var order []string
	for _, part := range strings.Split(text, ";") {
		key, value, found := strings.Cut(strings.TrimSpace(part), "=")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		if _, seen := tags[key]; !seen {
			order = append(order, key)
		}
		tags[key] = strings.TrimSpace(value)
	}
	if len(order) == 0 || order[0] != "v" || tags["v"] != "DKIM1" {
		return false
	}
	switch tags["k"] {
	case "rsa", "ed25519":
	default:
		return false
	}
	key := tags["p"]
	if key == "" {
		return false
	}
	// A revoked key is published as an empty p=; anything else must decode.
	if _, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(key), "")); err != nil {
		return false
	}
	return true
}

// The client-autoconfiguration records this facade is willing to republish.
//
// The rule is an explicit allow-list of owner labels, not "everything that
// points at the mail host", because several of the lines the server emits point
// at the mail host and still must not be published:
//
//   - `mta-sts` (CNAME) and `_mta-sts` (TXT) describe an MTA-STS policy served
//     over HTTPS at mta-sts.<domain>. This product serves no such endpoint, and
//     a policy senders cannot fetch is worse than none;
//   - `_smtp._tls` (TXT) asks receivers to report on the MTA-STS and DANE
//     policies we just declined to publish;
//   - `ua-auto-config` (CNAME) and `_ua-auto-config` (TXT) are the mail
//     server's own draft scheme: the TXT carries a digest of a document served
//     over HTTPS under the customer's own name, so it has MTA-STS's certificate
//     problem with none of MTA-STS's client support;
//   - TLSA is not emitted at all, and would need DANE keys this deployment does
//     not hold.
//
// What is left is the RFC 6186 / RFC 6764 SRV records and the two
// autoconfiguration aliases, all of which name the mail host itself, on ports
// this deployment actually serves.
var (
	// autoconfigSRVOwners are the SRV owners, relative to the zone.
	autoconfigSRVOwners = []string{
		"_submissions._tcp", // RFC 6186 message submission over implicit TLS (465)
		"_imaps._tcp",       // RFC 6186 IMAP over implicit TLS (993)
		"_pop3s._tcp",       // RFC 6186 POP3 over implicit TLS (995)
		"_jmap._tcp",        // RFC 8620 JMAP session discovery (443)
		"_caldavs._tcp",     // RFC 6764 CalDAV (443)
		"_carddavs._tcp",    // RFC 6764 CardDAV (443)
	}
	// autoconfigCNAMEOwners are the alias owners, relative to the zone.
	// Thunderbird reads the first and Outlook the second; both fall back to the
	// SRV records above when the alias cannot be reached, so a deployment whose
	// certificate does not cover these names loses nothing it had before.
	autoconfigCNAMEOwners = []string{"autoconfig", "autodiscover"}
)

// autoconfigRecord is one parsed client-autoconfiguration record.
type autoconfigRecord struct {
	// Name is the owner relative to the zone ("_imaps._tcp", "autoconfig").
	Name string
	// Type is "SRV" or "CNAME".
	Type string
	// Value is the rdata as the zone store will store it.
	Value string
}

// parseAutoconfigRecords extracts the allow-listed client-autoconfiguration
// records for zoneName from the server's zone file.
//
// A record is kept only when its target is hostname — MAIL_HOSTNAME, the same
// host the MX record names. The server renders its own current hostname, so
// this check is what stops a renamed or replaced mail server from pointing a
// customer's clients at a host this deployment does not vouch for; it is the
// same reason the MX value is derived from configuration rather than copied.
func parseAutoconfigRecords(zoneFile, zoneName, hostname string) []autoconfigRecord {
	suffix := "." + strings.ToLower(strings.TrimSuffix(zoneName, ".")) + "."
	target := strings.ToLower(strings.TrimSuffix(hostname, ".")) + "."
	if strings.TrimSuffix(target, ".") == "" {
		return nil
	}

	wanted := map[string]string{}
	for _, owner := range autoconfigSRVOwners {
		wanted[owner] = "SRV"
	}
	for _, owner := range autoconfigCNAMEOwners {
		wanted[owner] = "CNAME"
	}

	seen := map[string]struct{}{}
	var records []autoconfigRecord
	for _, entry := range parseZoneFile(zoneFile) {
		owner := strings.ToLower(entry.Owner)
		if !strings.HasSuffix(owner, suffix) {
			continue
		}
		name := strings.TrimSuffix(owner, suffix)
		kind, ok := wanted[name]
		if !ok || !strings.EqualFold(entry.Type, kind) {
			continue
		}
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		value, ok := autoconfigValue(kind, entry.RData, target)
		if !ok {
			continue
		}
		seen[name] = struct{}{}
		records = append(records, autoconfigRecord{Name: name, Type: kind, Value: value})
	}
	// The order is the allow-list's, not the file's, so the desired set — and
	// therefore every plan rendered from it — is stable whatever the server
	// emits.
	sort.Slice(records, func(i, j int) bool {
		return autoconfigOrder(records[i].Name) < autoconfigOrder(records[j].Name)
	})
	return records
}

// autoconfigValue validates one record's rdata and renders the stored form. An
// SRV needs four fields with a numeric priority, weight and port; a CNAME needs
// one target. Either way the target must be the mail host.
func autoconfigValue(kind string, rdata []string, target string) (string, bool) {
	switch kind {
	case "SRV":
		if len(rdata) != 4 {
			return "", false
		}
		for _, field := range rdata[:3] {
			if _, err := strconv.ParseUint(field, 10, 16); err != nil {
				return "", false
			}
		}
		if !strings.EqualFold(strings.TrimSuffix(rdata[3], ".")+".", target) {
			return "", false
		}
		return strings.Join([]string{rdata[0], rdata[1], rdata[2], target}, " "), true
	case "CNAME":
		if len(rdata) != 1 {
			return "", false
		}
		if !strings.EqualFold(strings.TrimSuffix(rdata[0], ".")+".", target) {
			return "", false
		}
		return target, true
	default:
		return "", false
	}
}

func autoconfigOrder(name string) int {
	for index, owner := range autoconfigSRVOwners {
		if owner == name {
			return index
		}
	}
	for index, owner := range autoconfigCNAMEOwners {
		if owner == name {
			return len(autoconfigSRVOwners) + index
		}
	}
	return len(autoconfigSRVOwners) + len(autoconfigCNAMEOwners)
}
