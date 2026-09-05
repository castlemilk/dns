package zone

import (
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/miekg/dns"
)

const (
	DefaultRecordTTL  = 300
	DefaultSOATTL     = 3600
	MaxRecordWireSize = 4 * 1024
)

var (
	ErrAlreadyExists = errors.New("already exists")
	ErrManagedRecord = errors.New("managed record")
	ErrNotFound      = errors.New("not found")
)

type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("%s: %s", e.Field, e.Message)
}

type RecordType string

const (
	TypeA     RecordType = "A"
	TypeAAAA  RecordType = "AAAA"
	TypeCNAME RecordType = "CNAME"
	TypeMX    RecordType = "MX"
	TypeTXT   RecordType = "TXT"
	TypeNS    RecordType = "NS"
	TypeSRV   RecordType = "SRV"
	TypeCAA   RecordType = "CAA"
	TypeSOA   RecordType = "SOA"
)

// Record provenance. The zero value is the user source, so records written
// before provenance existed need no migration and the JSON key is omitted for
// them — the authority feed therefore stays byte-identical (see
// internal/snapshot.Build).
const (
	SourceUser    = ""
	SourceHosting = "hosting"
	SourceMail    = "mail"
)

// ValidSource reports whether source is one of the three persisted values.
func ValidSource(source string) bool {
	switch source {
	case SourceUser, SourceHosting, SourceMail:
		return true
	default:
		return false
	}
}

type Record struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Type      RecordType `json:"type"`
	TTL       uint32     `json:"ttl"`
	Value     string     `json:"value"`
	Managed   bool       `json:"managed"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	// Source names the engine that owns this record: "" (the user), "hosting"
	// or "mail". Engine-owned records are refused by UpdateRecord and
	// DeleteRecord and are only written through ApplyRecordSet.
	Source string `json:"source,omitempty"`
}

type Zone struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Serial      uint32    `json:"serial"`
	Nameservers []string  `json:"nameservers"`
	Records     []Record  `json:"records"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func NormalizeName(name string) (string, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if invalidDomainDots(name) {
		return "", &ValidationError{Field: "name", Message: "enter a valid domain name"}
	}
	name = strings.TrimSuffix(name, ".")
	if name == "" || name == "@" {
		return "", &ValidationError{Field: "name", Message: "enter a domain name"}
	}
	if strings.Contains(name, "*") {
		return "", &ValidationError{Field: "name", Message: "a zone cannot be a wildcard"}
	}
	if !isASCII(name) {
		return "", &ValidationError{Field: "name", Message: "Unicode domains are not supported yet; use punycode"}
	}
	if !isSafeDomainText(name) {
		return "", &ValidationError{Field: "name", Message: "enter a valid domain name"}
	}
	if _, ok := dns.IsDomainName(dns.Fqdn(name)); !ok {
		return "", &ValidationError{Field: "name", Message: "enter a valid domain name"}
	}
	return name, nil
}

func NormalizeNameserver(name string) (string, error) {
	normalized, err := NormalizeName(name)
	if err != nil {
		return "", &ValidationError{Field: "nameserver", Message: err.Error()}
	}
	return dns.Fqdn(normalized), nil
}

func NormalizeRecord(zoneName, name string, recordType RecordType, ttl uint32, value string) (Record, error) {
	return normalizeRecord(zoneName, name, recordType, ttl, value, true)
}

// NormalizeImportedRecord applies the same validation as NormalizeRecord but
// preserves an explicit zero TTL from a BIND zone file. CRUD requests retain
// their existing behavior of treating zero as the default TTL.
func NormalizeImportedRecord(zoneName, name string, recordType RecordType, ttl uint32, value string) (Record, error) {
	return normalizeRecord(zoneName, name, recordType, ttl, value, false)
}

func normalizeRecord(zoneName, name string, recordType RecordType, ttl uint32, value string, defaultZeroTTL bool) (Record, error) {
	owner, err := normalizeOwner(zoneName, name)
	if err != nil {
		return Record{}, err
	}
	if !recordType.Valid() || recordType == TypeSOA {
		return Record{}, &ValidationError{Field: "type", Message: "choose a supported record type"}
	}
	if ttl == 0 && defaultZeroTTL {
		ttl = DefaultRecordTTL
	}
	if ttl > 2_147_483_647 {
		return Record{}, &ValidationError{Field: "ttl", Message: "must be at most 2147483647 seconds"}
	}

	record := Record{Name: owner, Type: recordType, TTL: ttl, Value: strings.TrimSpace(value)}
	if record.Value == "" {
		return Record{}, &ValidationError{Field: "value", Message: "enter a record value"}
	}
	if record.Type == TypeCNAME && owner == "@" {
		return Record{}, &ValidationError{Field: "name", Message: "the zone apex cannot be a CNAME"}
	}
	if err := normalizeValue(zoneName, &record); err != nil {
		return Record{}, err
	}
	rr, err := Compile(zoneName, record)
	if err != nil {
		return Record{}, &ValidationError{Field: "value", Message: err.Error()}
	}
	if err := validateWireRecord(rr); err != nil {
		return Record{}, &ValidationError{Field: "value", Message: err.Error()}
	}
	// The stored value has to be the value that is served. The two
	// character-string types accept input the compiler silently drops — a line
	// pasted out of a BIND file, `"v=spf1 mx -all" ; do not remove`, puts only
	// the quoted string on the wire — and a stored value carrying bytes the RR
	// does not makes the RRset duplicate check, enginedns.TxtText and the raw
	// zone table all disagree with the resolver. Take the rdata back off the
	// compiled record so the three agree. rdata is idempotent (re-parsing it
	// yields the same rendering), which ValidateSnapshot's canonicality
	// assertion requires.
	if record.Type == TypeTXT || record.Type == TypeCAA {
		record.Value = rdata(rr)
	}
	return record, nil
}

// rdata renders a compiled record's value without its owner/TTL/class/type
// header: what a resolver answers, in zone-file presentation form.
func rdata(rr dns.RR) string {
	return strings.TrimPrefix(rr.String(), rr.Header().String())
}

func (t RecordType) Valid() bool {
	switch t {
	case TypeA, TypeAAAA, TypeCNAME, TypeMX, TypeTXT, TypeNS, TypeSRV, TypeCAA, TypeSOA:
		return true
	default:
		return false
	}
}

func Compile(zoneName string, record Record) (dns.RR, error) {
	owner := dns.Fqdn(zoneName)
	if record.Name != "@" {
		owner = dns.Fqdn(record.Name + "." + zoneName)
	}
	rr, err := dns.NewRR(fmt.Sprintf("%s %d IN %s %s", owner, record.TTL, record.Type, record.Value))
	if err != nil {
		return nil, fmt.Errorf("parse %s record: %w", record.Type, err)
	}
	if rr == nil {
		return nil, fmt.Errorf("parse %s record: record is empty", record.Type)
	}
	return rr, nil
}

func normalizeOwner(zoneName, name string) (string, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" || name == "@" || name == dns.Fqdn(zoneName) || name == zoneName {
		return "@", nil
	}
	if invalidDomainDots(name) {
		return "", &ValidationError{Field: "name", Message: "enter a valid record name"}
	}

	if strings.HasSuffix(name, ".") {
		fqdn := dns.Fqdn(name)
		zoneFQDN := dns.Fqdn(zoneName)
		if !dns.IsSubDomain(zoneFQDN, fqdn) {
			return "", &ValidationError{Field: "name", Message: "must be inside the zone"}
		}
		name = strings.TrimSuffix(strings.TrimSuffix(fqdn, zoneFQDN), ".")
	} else {
		name = strings.Trim(name, ".")
	}

	if name == "" {
		return "@", nil
	}
	if !isASCII(name) {
		return "", &ValidationError{Field: "name", Message: "Unicode names are not supported yet; use punycode"}
	}
	if !isSafeDomainText(name) {
		return "", &ValidationError{Field: "name", Message: "enter a valid record name"}
	}
	wildcardCount := strings.Count(name, "*")
	validWildcard := wildcardCount == 1 && (name == "*" || strings.HasPrefix(name, "*."))
	if wildcardCount > 0 && !validWildcard {
		return "", &ValidationError{Field: "name", Message: "a wildcard must be the left-most label"}
	}
	if _, ok := dns.IsDomainName(dns.Fqdn(name + "." + zoneName)); !ok {
		return "", &ValidationError{Field: "name", Message: "enter a valid record name"}
	}
	return name, nil
}

func normalizeValue(zoneName string, record *Record) error {
	switch record.Type {
	case TypeA:
		addr, err := netip.ParseAddr(record.Value)
		if err != nil || !addr.Is4() {
			return &ValidationError{Field: "value", Message: "enter a valid IPv4 address"}
		}
		record.Value = addr.String()
	case TypeAAAA:
		addr, err := netip.ParseAddr(record.Value)
		if err != nil || !addr.Is6() || addr.Is4In6() {
			return &ValidationError{Field: "value", Message: "enter a valid IPv6 address"}
		}
		record.Value = addr.String()
	case TypeCNAME, TypeNS:
		target, err := normalizeTarget(zoneName, record.Value)
		if err != nil {
			return err
		}
		record.Value = target
	case TypeMX:
		parts := strings.Fields(record.Value)
		if len(parts) != 2 {
			return &ValidationError{Field: "value", Message: "use: priority mail.example.com"}
		}
		if _, err := parseUint16(parts[0]); err != nil {
			return &ValidationError{Field: "value", Message: "priority must be between 0 and 65535"}
		}
		target, err := normalizeTarget(zoneName, parts[1])
		if err != nil {
			return err
		}
		record.Value = parts[0] + " " + target
	case TypeSRV:
		parts := strings.Fields(record.Value)
		if len(parts) != 4 {
			return &ValidationError{Field: "value", Message: "use: priority weight port target.example.com"}
		}
		for _, raw := range parts[:3] {
			if _, err := parseUint16(raw); err != nil {
				return &ValidationError{Field: "value", Message: "priority, weight, and port must be between 0 and 65535"}
			}
		}
		target, err := normalizeTarget(zoneName, parts[3])
		if err != nil {
			return err
		}
		record.Value = strings.Join(append(parts[:3], target), " ")
	case TypeTXT:
		if !strings.HasPrefix(record.Value, "\"") {
			record.Value = strconv.Quote(record.Value)
		}
	case TypeCAA:
		parts := strings.Fields(record.Value)
		if len(parts) < 3 {
			return &ValidationError{Field: "value", Message: "use: flags tag value"}
		}
		flags, err := strconv.ParseUint(parts[0], 10, 8)
		if err != nil || flags > 255 {
			return &ValidationError{Field: "value", Message: "flags must be between 0 and 255"}
		}
		value := strings.Join(parts[2:], " ")
		if !strings.HasPrefix(value, "\"") {
			value = strconv.Quote(value)
		}
		// RFC 8659 §4.1 makes the property tag case-insensitive, so `ISSUE` and
		// `issue` are one record. Storing them as typed would put two identical
		// properties in one RRset and show the operator a difference that does
		// not exist.
		record.Value = parts[0] + " " + strings.ToLower(parts[1]) + " " + value
	}
	return nil
}

func normalizeTarget(zoneName, value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "@" {
		value = zoneName
	} else if invalidDomainDots(value) {
		return "", &ValidationError{Field: "value", Message: "enter a valid domain target"}
	} else if !strings.HasSuffix(value, ".") && !strings.Contains(value, ".") {
		value += "." + zoneName
	}
	value = strings.TrimSuffix(value, ".")
	if _, ok := dns.IsDomainName(dns.Fqdn(value)); !ok || !isASCII(value) || !isSafeDomainText(value) {
		return "", &ValidationError{Field: "value", Message: "enter a valid domain target"}
	}
	return dns.Fqdn(value), nil
}

func validateWireRecord(rr dns.RR) error {
	wireLength := dns.Len(rr)
	if wireLength > MaxRecordWireSize {
		return fmt.Errorf("record exceeds the %d-byte wire-size limit", MaxRecordWireSize)
	}
	buffer := make([]byte, wireLength)
	if _, err := dns.PackRR(rr, buffer, 0, nil, false); err != nil {
		return fmt.Errorf("record cannot be encoded on the DNS wire: %w", err)
	}
	return nil
}

func invalidDomainDots(value string) bool {
	if value == "" || value == "@" {
		return false
	}
	withoutRoot := strings.TrimSuffix(value, ".")
	return strings.HasPrefix(withoutRoot, ".") || strings.HasSuffix(withoutRoot, ".") || strings.Contains(withoutRoot, "..")
}

func parseUint16(value string) (uint64, error) {
	return strconv.ParseUint(value, 10, 16)
}

func isASCII(value string) bool {
	for _, char := range value {
		if char > 127 {
			return false
		}
	}
	return true
}

func isSafeDomainText(value string) bool {
	for _, char := range value {
		if char <= 0x20 || char == 0x7f || strings.ContainsRune(";$()\\\"", char) {
			return false
		}
	}
	return true
}
