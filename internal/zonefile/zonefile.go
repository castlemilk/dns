package zonefile

import (
	"bytes"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/castlemilk/dns/internal/zone"
	"github.com/miekg/dns"
)

const MaxBytes = 64 << 20

type Parsed struct {
	Records  []zone.Record
	Warnings []string
}

func Parse(rawName, source string) (Parsed, error) {
	if len(source) > MaxBytes {
		return Parsed{}, fmt.Errorf("zone file exceeds the %d-byte limit", MaxBytes)
	}
	name, err := zone.NormalizeName(rawName)
	if err != nil {
		return Parsed{}, err
	}
	origin := strings.ToLower(dns.Fqdn(name))
	parser := dns.NewZoneParser(strings.NewReader(source), origin, "zone-file")
	parser.SetIncludeAllowed(false)
	result := Parsed{Records: make([]zone.Record, 0)}
	skippedSOA := false
	skippedNS := false
	for rr, ok := parser.Next(); ok; rr, ok = parser.Next() {
		header := rr.Header()
		if header.Class != dns.ClassINET {
			return Parsed{}, fmt.Errorf("record %q uses unsupported DNS class %d", header.Name, header.Class)
		}
		owner := strings.ToLower(dns.Fqdn(header.Name))
		if !dns.IsSubDomain(origin, owner) {
			return Parsed{}, fmt.Errorf("record owner %q is outside zone %q", header.Name, name)
		}
		if owner == origin && header.Rrtype == dns.TypeSOA {
			skippedSOA = true
			continue
		}
		if owner == origin && header.Rrtype == dns.TypeNS {
			skippedNS = true
			continue
		}
		recordType, value, err := supportedRecord(rr)
		if err != nil {
			return Parsed{}, err
		}
		record, err := zone.NormalizeImportedRecord(name, owner, recordType, header.Ttl, value)
		if err != nil {
			return Parsed{}, fmt.Errorf("record %q %s: %w", header.Name, recordType, err)
		}
		result.Records = append(result.Records, record)
		if len(result.Records) > zone.MaxRecordsPerZone {
			return Parsed{}, fmt.Errorf("zone file exceeds the %d-record limit", zone.MaxRecordsPerZone)
		}
	}
	if err := parser.Err(); err != nil {
		return Parsed{}, fmt.Errorf("parse BIND zone file: %w", err)
	}
	if skippedSOA {
		result.Warnings = append(result.Warnings, "source apex SOA skipped; configured provider SOA will be used")
	}
	if skippedNS {
		result.Warnings = append(result.Warnings, "source apex NS records skipped; configured provider nameservers will be used")
	}
	return result, nil
}

func Export(value zone.Zone) (string, error) {
	records := slices.Clone(value.Records)
	slices.SortStableFunc(records, compareRecords)
	var output bytes.Buffer
	_, _ = fmt.Fprintf(&output, "$ORIGIN %s\n", dns.Fqdn(value.Name))
	for _, record := range records {
		rr, err := zone.Compile(value.Name, record)
		if err != nil {
			return "", fmt.Errorf("compile record %q: %w", record.ID, err)
		}
		line := rr.String() + "\n"
		if output.Len()+len(line) > MaxBytes {
			return "", fmt.Errorf("export exceeds the %d-byte limit", MaxBytes)
		}
		_, _ = output.WriteString(line)
	}
	return output.String(), nil
}

func supportedRecord(rr dns.RR) (zone.RecordType, string, error) {
	switch value := rr.(type) {
	case *dns.A:
		address, err := netip.ParseAddr(value.A.String())
		if err != nil || !address.Is4() {
			return "", "", fmt.Errorf("record %q contains an invalid A value", value.Hdr.Name)
		}
		return zone.TypeA, address.Unmap().String(), nil
	case *dns.AAAA:
		address, ok := netip.AddrFromSlice(value.AAAA)
		if !ok || !address.Is6() || address.Is4In6() {
			return "", "", fmt.Errorf("record %q contains an invalid AAAA value", value.Hdr.Name)
		}
		return zone.TypeAAAA, address.String(), nil
	case *dns.CNAME:
		return zone.TypeCNAME, value.Target, nil
	case *dns.MX:
		return zone.TypeMX, fmt.Sprintf("%d %s", value.Preference, value.Mx), nil
	case *dns.TXT:
		parts := make([]string, 0, len(value.Txt))
		for _, text := range value.Txt {
			parts = append(parts, quoteCharacterString(text))
		}
		return zone.TypeTXT, strings.Join(parts, " "), nil
	case *dns.NS:
		return zone.TypeNS, value.Ns, nil
	case *dns.SRV:
		return zone.TypeSRV, fmt.Sprintf("%d %d %d %s", value.Priority, value.Weight, value.Port, value.Target), nil
	case *dns.CAA:
		return zone.TypeCAA, fmt.Sprintf("%d %s %s", value.Flag, value.Tag, quoteCharacterString(value.Value)), nil
	default:
		recordType := dns.TypeToString[rr.Header().Rrtype]
		if recordType == "" {
			recordType = strconv.Itoa(int(rr.Header().Rrtype))
		}
		return "", "", fmt.Errorf("record %q uses unsupported type %s", rr.Header().Name, recordType)
	}
}

func quoteCharacterString(value string) string {
	var result strings.Builder
	result.Grow(len(value) + 2)
	result.WriteByte('"')
	for index := 0; index < len(value); index++ {
		char := value[index]
		switch {
		case char == '"' || char == '\\':
			result.WriteByte('\\')
			result.WriteByte(char)
		case char < 0x20 || char > 0x7e:
			_, _ = fmt.Fprintf(&result, "\\%03d", char)
		default:
			result.WriteByte(char)
		}
	}
	result.WriteByte('"')
	return result.String()
}

func compareRecords(a, b zone.Record) int {
	if a.Name != b.Name {
		if a.Name == "@" {
			return -1
		}
		if b.Name == "@" {
			return 1
		}
		return strings.Compare(a.Name, b.Name)
	}
	if a.Type != b.Type {
		return strings.Compare(string(a.Type), string(b.Type))
	}
	if a.TTL != b.TTL {
		if a.TTL < b.TTL {
			return -1
		}
		return 1
	}
	return strings.Compare(a.Value, b.Value)
}
