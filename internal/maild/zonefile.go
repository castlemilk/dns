package maild

import (
	"strconv"
	"strings"
)

// Client autoconfiguration: the RFC 6186 / RFC 6764 SRV records and the two
// autoconfiguration aliases that let Thunderbird, Apple Mail and Outlook set
// themselves up from the address alone.
//
// The facade parses these out of x:Domain.dnsZoneFile and publishes nothing it
// did not find there (internal/mail/zonefile.go, parseAutoconfigRecords). Its
// allow-list is eight owners; this server renders only the ones it can honour,
// and renders the rest as zone-file comments naming what was withheld and why.
//
// Why a comment rather than a record: an autoconfiguration record is an
// instruction to every mail client in the world about where to connect. A
// _imaps._tcp SRV published by a server that speaks no IMAP does not degrade to
// "try something else" — the client believes it, connects to a port nothing
// listens on, and reports the account as broken. That is strictly worse than
// publishing nothing, because publishing nothing leaves the client's own
// guessing heuristics intact. So the rule here is: advertise a service only
// where this binary has a listener for it.
//
// The comments are not decorative. internal/mail/zonefile.go's indexOfComment
// drops everything from an unquoted ';' to the end of the line before the
// record is parsed, so a withheld line publishes exactly nothing while still
// telling an operator reading the rendered file which records were deliberately
// not offered. zonefile_test.go pins both halves of that.
const (
	// autoconfigSubmissionsPort is implicit-TLS message submission, RFC 8314.
	// cmd/deephost-mail binds it by default (MAILD_SUBMISSIONS_ADDR :465).
	autoconfigSubmissionsPort = 465
	// autoconfigIMAPSPort and the two below are named only so the withheld
	// comment can show the record that was not published.
	autoconfigIMAPSPort = 993
	// autoconfigHTTPSPort is what JMAP, CalDAV and CardDAV would be found on.
	autoconfigHTTPSPort = 443
)

// autoconfigSRVPriority and autoconfigSRVWeight match the capture in
// internal/mail/stalwart/testdata/zonefile_probe.txt. There is one target, so
// neither number does any work; they exist because an SRV record has four
// fields and the facade's parser requires all four to be present and numeric.
const (
	autoconfigSRVPriority = 0
	autoconfigSRVWeight   = 1
)

// autoconfigEntry is one owner on the facade's allow-list, and this server's
// answer for it.
type autoconfigEntry struct {
	// owner is the name relative to the zone, exactly as the facade's
	// allow-list spells it. A name not on that list is never published, so
	// inventing one here would be silently dropped.
	owner string
	// kind is "SRV" or "CNAME".
	kind string
	// port is the SRV port; it is ignored for a CNAME.
	port int
	// offered is whether this deployment actually serves the protocol. False
	// renders the record as a comment instead.
	offered bool
	// withheld is the operator-facing reason, required when offered is false.
	// It is a plain sentence: it lands in a file a customer's DNS operator may
	// read, so it explains the refusal rather than naming an internal symbol.
	withheld string
}

// autoconfigEntries is the whole allow-list, in the facade's own order, with
// this server's answer for each.
//
// The offered set is a property of the binary, not of one process: maild
// implements submission and POP3 and does not implement IMAP, JMAP mail access,
// CalDAV or CardDAV, and serves no autoconfiguration document over HTTPS. A
// deployment that switches its submission or POP3 listeners off should leave
// publish_client_autoconfig off with them; there is no per-process signal here
// because x:Domain/get renders this text without knowing which sockets the
// server bound.
var autoconfigEntries = []autoconfigEntry{
	{owner: "_submissions._tcp", kind: "SRV", port: autoconfigSubmissionsPort, offered: true},
	{
		owner: "_imaps._tcp", kind: "SRV", port: autoconfigIMAPSPort,
		withheld: "this server serves POP3, not IMAP; nothing listens on 993",
	},
	{owner: "_pop3s._tcp", kind: "SRV", port: DefaultPOP3Port, offered: true},
	{
		owner: "_jmap._tcp", kind: "SRV", port: autoconfigHTTPSPort,
		withheld: "the JMAP endpoint here is the admin API, not a mail session for clients",
	},
	{
		owner: "_caldavs._tcp", kind: "SRV", port: autoconfigHTTPSPort,
		withheld: "this server stores no calendars",
	},
	{
		owner: "_carddavs._tcp", kind: "SRV", port: autoconfigHTTPSPort,
		withheld: "this server stores no contacts",
	},
	{
		owner: "autoconfig", kind: "CNAME",
		withheld: "no autoconfiguration document is served over HTTPS on this host",
	},
	{
		owner: "autodiscover", kind: "CNAME",
		withheld: "no Autodiscover document is served over HTTPS on this host",
	},
}

// autoconfigHeader introduces the block. It is a comment, so it publishes
// nothing; it is here because the file it lands in is read by operators
// diagnosing "my mail client will not configure itself".
const autoconfigHeader = "; client autoconfiguration (RFC 6186, RFC 6764).\n" +
	"; A commented line below is a record this server deliberately does not\n" +
	"; publish, because it has no listener for that service. Pointing a client\n" +
	"; at a port nothing answers on is worse than publishing nothing at all.\n"

// AutoconfigZoneRecords renders the client-autoconfiguration block for
// domainName, targeting hostname — which must be MAIL_HOSTNAME, because the
// facade drops any record whose target is not exactly that (it is the same
// check that stops a renamed mail server from redirecting a customer's clients).
//
// The result is newline-terminated, or "" when either name is empty.
func AutoconfigZoneRecords(domainName, hostname string) string {
	domain := NormalizeDomainName(domainName)
	host := NormalizeDomainName(hostname)
	if domain == "" || host == "" {
		return ""
	}

	var out strings.Builder
	out.WriteString(autoconfigHeader)
	for _, entry := range autoconfigEntries {
		record := entry.record(domain, host)
		if entry.offered {
			out.WriteString(record)
			out.WriteString("\n")
			continue
		}
		// One leading ';' is the whole mechanism: the facade's parser cuts the
		// line there and is left with nothing to publish.
		out.WriteString("; ")
		out.WriteString(record)
		out.WriteString(" -- withheld: ")
		out.WriteString(entry.withheld)
		out.WriteString("\n")
	}
	return out.String()
}

// record renders the single line this entry describes. Both branches produce
// the same text whether the line is published or commented out, so the comment
// shows the operator exactly what was withheld.
func (e autoconfigEntry) record(domain, host string) string {
	owner := e.owner + "." + domain + "."
	if e.kind == "CNAME" {
		return owner + " IN CNAME " + host + "."
	}
	return owner + " IN SRV " +
		strconv.Itoa(autoconfigSRVPriority) + " " +
		strconv.Itoa(autoconfigSRVWeight) + " " +
		strconv.Itoa(e.port) + " " + host + "."
}

// AppendAutoconfigRecords returns zoneFile with the autoconfiguration block
// appended.
//
// It exists so the one caller — x:Domain/get, which is where dnsZoneFile is
// rendered — cannot get the join wrong: the DKIM renderer's output is
// newline-terminated but a hook may return text that is not, and concatenating
// onto an unterminated last line would corrupt the record above rather than add
// one below.
func AppendAutoconfigRecords(zoneFile, domainName, hostname string) string {
	block := AutoconfigZoneRecords(domainName, hostname)
	if block == "" {
		return zoneFile
	}
	if zoneFile != "" && !strings.HasSuffix(zoneFile, "\n") {
		zoneFile += "\n"
	}
	return zoneFile + block
}
