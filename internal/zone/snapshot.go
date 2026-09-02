package zone

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/miekg/dns"
)

// BumpSnapshotSerials returns an independently owned snapshot with recovery
// SOA serials selected by the same clock-aware rule as live mutations: use the
// current Unix time when it is numerically ahead, otherwise advance one step
// modulo 2^32. Operators must still restore the latest verified backup and, if
// available, compare its recovery serial with the last live serial.
func BumpSnapshotSerials(values []Zone, now time.Time) ([]Zone, error) {
	if now.IsZero() {
		return nil, errors.New("serial bump time is required")
	}
	if err := ValidateSnapshot(values); err != nil {
		return nil, err
	}
	result := slices.Clone(values)
	for zoneIndex := range result {
		result[zoneIndex].Nameservers = slices.Clone(result[zoneIndex].Nameservers)
		result[zoneIndex].Records = slices.Clone(result[zoneIndex].Records)
		result[zoneIndex].Serial = nextSerial(result[zoneIndex].Serial, now.UTC())
		updatedAt := now.UTC()
		if updatedAt.Before(result[zoneIndex].UpdatedAt) {
			updatedAt = result[zoneIndex].UpdatedAt
		}
		result[zoneIndex].UpdatedAt = updatedAt
		for recordIndex := range result[zoneIndex].Records {
			record := &result[zoneIndex].Records[recordIndex]
			if record.Managed && record.Type == TypeSOA {
				record.Value = soaValue(result[zoneIndex])
				record.UpdatedAt = updatedAt
			}
		}
	}
	if err := ValidateSnapshot(result); err != nil {
		return nil, fmt.Errorf("validate serial-bumped snapshot: %w", err)
	}
	return result, nil
}

// ValidateSnapshot verifies the complete persistent representation accepted by
// RestoreSnapshot. It intentionally validates more than the DNS compiler: IDs,
// indexes, managed records, canonical forms, and timestamps must all be safe to
// persist and serve after a restart.
func ValidateSnapshot(values []Zone) error {
	zoneIDs := make(map[string]struct{}, len(values))
	zoneNames := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value.ID == "" {
			return &ValidationError{Field: "zone.id", Message: "is required"}
		}
		if _, exists := zoneIDs[value.ID]; exists {
			return fmt.Errorf("duplicate zone id %q", value.ID)
		}
		zoneIDs[value.ID] = struct{}{}
		name, err := NormalizeName(value.Name)
		if err != nil {
			return fmt.Errorf("zone %q name: %w", value.ID, err)
		}
		if name != value.Name {
			return fmt.Errorf("zone %q name is not canonical", value.Name)
		}
		if _, exists := zoneNames[value.Name]; exists {
			return fmt.Errorf("duplicate zone name %q", value.Name)
		}
		zoneNames[value.Name] = struct{}{}
		if value.CreatedAt.IsZero() || value.UpdatedAt.IsZero() || value.UpdatedAt.Before(value.CreatedAt) {
			return fmt.Errorf("zone %q has invalid timestamps", value.Name)
		}
		if len(value.Nameservers) == 0 {
			return fmt.Errorf("zone %q has no nameservers", value.Name)
		}
		if len(value.Records) > MaxRecordsPerZone {
			return fmt.Errorf("zone %q exceeds the %d-record limit", value.Name, MaxRecordsPerZone)
		}

		expectedNameservers := make(map[string]struct{}, len(value.Nameservers))
		for _, nameserver := range value.Nameservers {
			normalized, err := NormalizeNameserver(nameserver)
			if err != nil {
				return fmt.Errorf("zone %q nameserver: %w", value.Name, err)
			}
			if normalized != nameserver {
				return fmt.Errorf("zone %q nameserver %q is not canonical", value.Name, nameserver)
			}
			if _, exists := expectedNameservers[nameserver]; exists {
				return fmt.Errorf("zone %q repeats nameserver %q", value.Name, nameserver)
			}
			expectedNameservers[nameserver] = struct{}{}
		}

		recordIDs := make(map[string]struct{}, len(value.Records))
		validated := make([]Record, 0, len(value.Records))
		managedNameservers := make(map[string]int, len(value.Nameservers))
		soaCount := 0
		for _, record := range value.Records {
			if record.ID == "" {
				return fmt.Errorf("zone %q contains a record without an id", value.Name)
			}
			if _, exists := recordIDs[record.ID]; exists {
				return fmt.Errorf("zone %q repeats record id %q", value.Name, record.ID)
			}
			recordIDs[record.ID] = struct{}{}
			if record.CreatedAt.IsZero() || record.UpdatedAt.IsZero() || record.UpdatedAt.Before(record.CreatedAt) {
				return fmt.Errorf("zone %q record %q has invalid timestamps", value.Name, record.ID)
			}
			owner, err := normalizeOwner(value.Name, record.Name)
			if err != nil {
				return fmt.Errorf("zone %q record %q owner: %w", value.Name, record.ID, err)
			}
			if owner != record.Name {
				return fmt.Errorf("zone %q record %q owner is not canonical", value.Name, record.ID)
			}
			if !record.Type.Valid() {
				return fmt.Errorf("zone %q record %q has unsupported type %q", value.Name, record.ID, record.Type)
			}
			if record.TTL > 2_147_483_647 {
				return fmt.Errorf("zone %q record %q has invalid TTL", value.Name, record.ID)
			}

			if record.Type == TypeSOA {
				soaCount++
				if record.Name != "@" || !record.Managed || record.TTL != DefaultSOATTL || record.Value != soaValue(value) {
					return fmt.Errorf("zone %q has an invalid managed SOA", value.Name)
				}
			} else {
				normalized, err := NormalizeImportedRecord(value.Name, record.Name, record.Type, record.TTL, record.Value)
				if err != nil {
					return fmt.Errorf("zone %q record %q: %w", value.Name, record.ID, err)
				}
				if normalized.Name != record.Name || normalized.Type != record.Type || normalized.TTL != record.TTL || normalized.Value != record.Value {
					return fmt.Errorf("zone %q record %q is not canonical", value.Name, record.ID)
				}
				if record.Managed {
					if record.Name != "@" || record.Type != TypeNS || record.TTL != DefaultSOATTL {
						return fmt.Errorf("zone %q record %q has an invalid managed flag", value.Name, record.ID)
					}
					managedNameservers[record.Value]++
				}
			}

			rr, err := Compile(value.Name, record)
			if err != nil {
				return fmt.Errorf("zone %q record %q: %w", value.Name, record.ID, err)
			}
			if rr.Header().Class != dns.ClassINET {
				return fmt.Errorf("zone %q record %q is not IN class", value.Name, record.ID)
			}
			if err := validateWireRecord(rr); err != nil {
				return fmt.Errorf("zone %q record %q: %w", value.Name, record.ID, err)
			}
			if err := validateRRSet(value.Name, validated, record, ""); err != nil {
				return fmt.Errorf("zone %q record %q: %w", value.Name, record.ID, err)
			}
			validated = append(validated, record)
		}
		if soaCount != 1 {
			return fmt.Errorf("zone %q must contain exactly one managed SOA", value.Name)
		}
		for nameserver := range expectedNameservers {
			if managedNameservers[nameserver] != 1 {
				return fmt.Errorf("zone %q must contain one managed NS for %q", value.Name, nameserver)
			}
		}
		for nameserver, count := range managedNameservers {
			if _, expected := expectedNameservers[nameserver]; !expected || count != 1 {
				return fmt.Errorf("zone %q contains an unexpected managed NS %q", value.Name, nameserver)
			}
		}
	}
	return nil
}
