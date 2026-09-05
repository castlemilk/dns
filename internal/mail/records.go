package mail

import (
	"fmt"
	"sort"
	"strings"

	mailv1 "github.com/castlemilk/dns/gen/go/mail/v1"
	"github.com/castlemilk/dns/internal/enginedns"
	"github.com/castlemilk/dns/internal/platform"
	"github.com/castlemilk/dns/internal/zone"
)

// recordTTL is the TTL every mail policy record carries. Mail routing and
// policy change rarely and a stale answer is expensive, so an hour is the
// conventional value; the planner keeps an existing RRset's TTL when it adopts
// one, so this only applies to records the facade creates.
const recordTTL = 3600

// Record names the facade owns inside a bound zone.
const (
	dmarcName = "_dmarc"
	apexName  = "@"
)

// desiredRecords is the set §3.3 prescribes — MX, SPF and DMARC derived from
// configuration, and one TXT per active DKIM key taken from the server — plus,
// when the binding asks for them, the client-autoconfiguration records the
// server's zone file offers.
//
// The mail server's own MX/SPF/DMARC lines are never copied. Its DMARC is
// hard-coded p=reject, and its MX names whatever the server currently calls
// itself — so copying either would let the engine, rather than this
// deployment's configuration, decide where a customer's mail goes.
//
// The autoconfiguration records are different: there is nothing to derive them
// from, they only ever name the mail host (parseAutoconfigRecords drops any
// that do not), and they carry no policy — so taking them from the server is
// safe in a way taking its MX is not.
func (s *Service) desiredRecords(
	zoneName, dmarcPolicy, reportAddress string,
	dkim []dkimRecord,
	autoconfig []autoconfigRecord,
) []zone.Record {
	records := []zone.Record{
		{
			Name:  apexName,
			Type:  zone.TypeMX,
			TTL:   recordTTL,
			Value: fmt.Sprintf("%d %s.", s.cfg.MXPriority, s.cfg.Hostname),
		},
		{
			Name:  apexName,
			Type:  zone.TypeTXT,
			TTL:   recordTTL,
			Value: s.spfValue(),
		},
		{
			Name:  dmarcName,
			Type:  zone.TypeTXT,
			TTL:   recordTTL,
			Value: dmarcValue(dmarcPolicy, reportAddress),
		},
	}
	for _, key := range dkim {
		records = append(records, zone.Record{
			Name:  key.Name,
			Type:  zone.TypeTXT,
			TTL:   recordTTL,
			Value: key.Value,
		})
	}
	for _, entry := range autoconfig {
		records = append(records, zone.Record{
			Name:  entry.Name,
			Type:  zone.RecordType(entry.Type),
			TTL:   recordTTL,
			Value: entry.Value,
		})
	}
	return records
}

// spfValue is `v=spf1 mx -all`, with the operator's include when one is set.
// `mx` rather than `a` because the record authorises the hosts this zone's MX
// names — which is the mail server this facade just published.
func (s *Service) spfValue() string {
	if s.cfg.SPFInclude != "" {
		return "v=spf1 mx include:" + s.cfg.SPFInclude + " -all"
	}
	return "v=spf1 mx -all"
}

func dmarcValue(policy, reportAddress string) string {
	return fmt.Sprintf("v=DMARC1; p=%s; rua=mailto:%s", policy, reportAddress)
}

// reportAddress is the rua target: the operator's central address when one is
// configured, else postmaster@<zone>. The Email card warns while that mailbox
// does not exist, because reports sent to an address that bounces are reports
// nobody reads.
func (s *Service) reportAddress(zoneName string) string {
	if s.cfg.ReportAddress != "" {
		return s.cfg.ReportAddress
	}
	return "postmaster@" + zoneName
}

// dmarcPolicy resolves the requested policy against the configured default and
// rejects anything else. DMARC has exactly three policies; a typo would publish
// a record receivers ignore.
func (s *Service) dmarcPolicy(requested string) (string, error) {
	value := strings.ToLower(strings.TrimSpace(requested))
	if value == "" {
		return s.cfg.DMARCPolicy, nil
	}
	switch value {
	case "none", "quarantine", "reject":
		return value, nil
	default:
		return "", invalidArgument("dmarc_policy", "choose none, quarantine, or reject")
	}
}

// recordDocs stores the desired set on the row, so the reconciler and the UI
// can describe the binding even while the engine is unreachable.
func recordDocs(records []zone.Record) []platform.MailRecordDoc {
	docs := make([]platform.MailRecordDoc, 0, len(records))
	for _, record := range records {
		docs = append(docs, platform.MailRecordDoc{
			Name:  record.Name,
			Type:  string(record.Type),
			Value: record.Value,
			TTL:   record.TTL,
		})
	}
	return docs
}

// recordState renders the desired set against the zone as it stands: which
// records exist, which carry the expected value, and which id each one has.
//
// TXT values are compared by their wire text, so the one-string and the
// parenthesised split form of a long DKIM key count as the same record.
func recordState(zoneValue zone.Zone, desired []zone.Record) ([]*mailv1.MailRecord, bool) {
	result := make([]*mailv1.MailRecord, 0, len(desired))
	inSync := true
	for _, want := range desired {
		normalized, err := zone.NormalizeRecord(zoneValue.Name, want.Name, want.Type, want.TTL, want.Value)
		if err != nil {
			// A value the store would refuse is reported as absent rather than
			// silently dropped: the reconciler's plan will report it too.
			result = append(result, &mailv1.MailRecord{
				Name:  want.Name,
				Type:  string(want.Type),
				Value: want.Value,
				Ttl:   want.TTL,
			})
			inSync = false
			continue
		}
		entry := &mailv1.MailRecord{
			Name:  normalized.Name,
			Type:  string(normalized.Type),
			Value: normalized.Value,
			Ttl:   normalized.TTL,
		}
		for _, existing := range zoneValue.Records {
			if existing.Name != normalized.Name || existing.Type != normalized.Type {
				continue
			}
			if !valuesEqual(normalized.Type, existing.Value, normalized.Value) {
				continue
			}
			entry.Present = true
			entry.Matches = true
			entry.RecordId = existing.ID
			entry.Ttl = existing.TTL
			break
		}
		if !entry.Present {
			// The name and type exist but with another value: report the record
			// as present-but-different rather than missing, so the card can say
			// "drifted" instead of "not published".
			for _, existing := range zoneValue.Records {
				if existing.Name == normalized.Name && existing.Type == normalized.Type && existing.Source == zone.SourceMail {
					entry.Present = true
					entry.RecordId = existing.ID
					break
				}
			}
		}
		if !entry.Matches {
			inSync = false
		}
		result = append(result, entry)
	}
	return result, inSync
}

func valuesEqual(kind zone.RecordType, left, right string) bool {
	if kind == zone.TypeTXT {
		return enginedns.TxtEqual(left, right)
	}
	return strings.EqualFold(left, right)
}

// publishedSelectors is the set of DKIM selectors whose TXT record is in the
// zone carrying the key the mail server actually signs with, so DkimKey.published
// is an observation rather than a hope.
//
// It matches on the value, not merely on the name. A record at
// "<selector>._domainkey" holding some other key is the worst case there is: a
// receiver treats a signature that fails to verify as a forgery rather than as
// an unsigned message, and the default DMARC policy quarantines it. Matching on
// the name alone reported that zone as published — and, because recordState does
// compare the value, made the same response say the record was both published
// and not present. It reads the state through recordState for that reason: one
// piece of evidence, so the key line and the record row cannot disagree.
func publishedSelectors(zoneValue zone.Zone, want []dkimRecord) map[string]struct{} {
	desired := make([]zone.Record, 0, len(want))
	for _, key := range want {
		desired = append(desired, zone.Record{
			Name:  key.Name,
			Type:  zone.TypeTXT,
			TTL:   recordTTL,
			Value: key.Value,
		})
	}
	state, _ := recordState(zoneValue, desired)
	return publishedFromRecords(state)
}

// publishedFromRecords reads the published selectors out of a rendered record
// state: a DKIM record row that matches is a key that is published, and one that
// does not is not, whatever else is at that name.
func publishedFromRecords(records []*mailv1.MailRecord) map[string]struct{} {
	published := map[string]struct{}{}
	for _, record := range records {
		if record.GetType() != string(zone.TypeTXT) || !record.GetMatches() {
			continue
		}
		selector, rest, found := strings.Cut(record.GetName(), "._domainkey")
		if !found || rest != "" || selector == "" {
			continue
		}
		published[selector] = struct{}{}
	}
	return published
}

// recordIDs collects the ids of every record this engine owns in the zone, so
// an unbind can delete or release exactly those.
func recordIDs(zoneValue zone.Zone) []string {
	var ids []string
	for _, record := range zoneValue.Records {
		if record.Source == zone.SourceMail {
			ids = append(ids, record.ID)
		}
	}
	sort.Strings(ids)
	return ids
}

// autoconfigPublished reports whether the zone already carries at least one
// engine-owned client-autoconfiguration record. It is how a rebuilt row
// recovers the switch that platform.db lost: the published set is the evidence.
func autoconfigPublished(zoneValue zone.Zone) bool {
	owners := make(map[string]zone.RecordType, len(autoconfigSRVOwners)+len(autoconfigCNAMEOwners))
	for _, owner := range autoconfigSRVOwners {
		owners[owner] = zone.TypeSRV
	}
	for _, owner := range autoconfigCNAMEOwners {
		owners[owner] = zone.TypeCNAME
	}
	for _, record := range zoneValue.Records {
		if record.Source != zone.SourceMail {
			continue
		}
		if kind, ok := owners[record.Name]; ok && record.Type == kind {
			return true
		}
	}
	return false
}
