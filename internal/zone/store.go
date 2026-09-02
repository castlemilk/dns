package zone

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/miekg/dns"
	"go.etcd.io/bbolt"
)

var (
	zonesBucket = []byte("zones")
	namesBucket = []byte("zone_names")

	ErrStoreNotEmpty  = errors.New("zone store is not empty")
	errDryRunRollback = errors.New("rollback successful import dry-run")
)

const (
	MaxRecordsPerZone = 10_000
	MaxRRSetRecords   = 128
	MaxRRSetWireSize  = 48 * 1024
)

type Store struct {
	db          *bbolt.DB
	nameservers []string
	now         func() time.Time
	admit       func([]Zone) error
}

type ImportMode uint8

const (
	ImportCreate ImportMode = iota + 1
	ImportReplace
)

type StoreOption func(*Store)

func WithClock(now func() time.Time) StoreOption {
	return func(store *Store) {
		store.now = now
	}
}

// WithSnapshotAdmission installs a fail-closed aggregate-state validator. It
// runs against existing data at open and against the tentative contents of
// every mutation's bbolt transaction before that transaction can commit.
func WithSnapshotAdmission(admit func([]Zone) error) StoreOption {
	return func(store *Store) {
		store.admit = admit
	}
}

func Open(path string, nameservers []string, opts ...StoreOption) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("data path is required")
	}
	if len(nameservers) == 0 {
		return nil, errors.New("at least one nameserver is required")
	}

	normalizedNameservers := make([]string, 0, len(nameservers))
	for _, nameserver := range nameservers {
		normalized, err := NormalizeNameserver(nameserver)
		if err != nil {
			return nil, fmt.Errorf("normalize nameserver %q: %w", nameserver, err)
		}
		normalizedNameservers = append(normalizedNameservers, normalized)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	db, err := bbolt.Open(path, 0o600, &bbolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open zone database: %w", err)
	}

	store := &Store{db: db, nameservers: normalizedNameservers, now: time.Now}
	for _, opt := range opts {
		opt(store)
	}
	if err := store.db.Update(func(tx *bbolt.Tx) error {
		if _, err := tx.CreateBucketIfNotExists(zonesBucket); err != nil {
			return fmt.Errorf("create zones bucket: %w", err)
		}
		if _, err := tx.CreateBucketIfNotExists(namesBucket); err != nil {
			return fmt.Errorf("create zone names bucket: %w", err)
		}
		return nil
	}); err != nil {
		if closeErr := db.Close(); closeErr != nil {
			return nil, fmt.Errorf("initialize zone database: %v; close database: %w", err, closeErr)
		}
		return nil, fmt.Errorf("initialize zone database: %w", err)
	}
	if err := store.db.View(store.validateAdmission); err != nil {
		if closeErr := db.Close(); closeErr != nil {
			return nil, fmt.Errorf("validate existing zone database: %v; close database: %w", err, closeErr)
		}
		return nil, fmt.Errorf("validate existing zone database: %w", err)
	}
	return store, nil
}

// OpenEmptyForRestore opens a new or demonstrably empty bbolt database. It
// refuses symlinks, non-regular files, corrupt/non-bbolt files, unknown buckets,
// and databases containing any keys before opening the target for writes.
func OpenEmptyForRestore(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("data path is required")
	}
	info, err := os.Lstat(path)
	switch {
	case err == nil:
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("restore target %q is not a regular file", path)
		}
		if info.Size() > 0 {
			if err := verifyEmptyDatabase(path); err != nil {
				return nil, err
			}
		}
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, fmt.Errorf("inspect restore target %q: %w", path, err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create restore directory: %w", err)
	}
	db, err := bbolt.Open(path, 0o600, &bbolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open empty restore database: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		secureErr := fmt.Errorf("secure restore database: %w", err)
		if closeErr := db.Close(); closeErr != nil {
			return nil, errors.Join(secureErr, fmt.Errorf("close restore database: %w", closeErr))
		}
		return nil, secureErr
	}
	store := &Store{db: db, now: time.Now}
	if err := store.db.Update(func(tx *bbolt.Tx) error {
		if _, err := tx.CreateBucketIfNotExists(zonesBucket); err != nil {
			return fmt.Errorf("create zones bucket: %w", err)
		}
		if _, err := tx.CreateBucketIfNotExists(namesBucket); err != nil {
			return fmt.Errorf("create zone names bucket: %w", err)
		}
		return ensureEmptyBuckets(tx)
	}); err != nil {
		initializeErr := fmt.Errorf("initialize empty restore database: %w", err)
		if closeErr := db.Close(); closeErr != nil {
			return nil, errors.Join(initializeErr, fmt.Errorf("close restore database: %w", closeErr))
		}
		return nil, initializeErr
	}
	return store, nil
}

func (s *Store) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("close zone database: %w", err)
	}
	return nil
}

func (s *Store) List(ctx context.Context) ([]Zone, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("list zones: %w", err)
	}
	zones := make([]Zone, 0)
	if err := s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(zonesBucket).ForEach(func(_, raw []byte) error {
			var value Zone
			if err := json.Unmarshal(raw, &value); err != nil {
				return fmt.Errorf("decode zone: %w", err)
			}
			sortRecords(&value)
			zones = append(zones, value)
			return nil
		})
	}); err != nil {
		return nil, fmt.Errorf("list zones: %w", err)
	}
	slices.SortFunc(zones, func(a, b Zone) int { return strings.Compare(a.Name, b.Name) })
	return zones, nil
}

func (s *Store) Get(ctx context.Context, zoneID string) (Zone, error) {
	if err := ctx.Err(); err != nil {
		return Zone{}, fmt.Errorf("get zone: %w", err)
	}
	var value Zone
	if err := s.db.View(func(tx *bbolt.Tx) error {
		stored, err := getZone(tx, zoneID)
		if err != nil {
			return err
		}
		value = stored
		return nil
	}); err != nil {
		return Zone{}, fmt.Errorf("get zone %q: %w", zoneID, err)
	}
	sortRecords(&value)
	return value, nil
}

func (s *Store) Create(ctx context.Context, rawName string) (Zone, error) {
	if err := ctx.Err(); err != nil {
		return Zone{}, fmt.Errorf("create zone: %w", err)
	}
	name, err := NormalizeName(rawName)
	if err != nil {
		return Zone{}, err
	}

	now := s.now().UTC()
	serial := nextSerial(0, now)
	zoneID, err := newID()
	if err != nil {
		return Zone{}, fmt.Errorf("create zone id: %w", err)
	}
	value := Zone{
		ID:          zoneID,
		Name:        name,
		Serial:      serial,
		Nameservers: slices.Clone(s.nameservers),
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	value.Records, err = managedRecords(value, now)
	if err != nil {
		return Zone{}, err
	}
	for _, record := range value.Records {
		rr, err := Compile(value.Name, record)
		if err != nil {
			return Zone{}, &ValidationError{Field: "name", Message: "the zone cannot be encoded as DNS data"}
		}
		if err := validateWireRecord(rr); err != nil {
			return Zone{}, &ValidationError{Field: "name", Message: "the zone cannot be encoded as DNS data"}
		}
	}

	if err := s.db.Update(func(tx *bbolt.Tx) error {
		names := tx.Bucket(namesBucket)
		if names.Get([]byte(name)) != nil {
			return fmt.Errorf("zone %q: %w", name, ErrAlreadyExists)
		}
		if err := putZone(tx, value); err != nil {
			return err
		}
		if err := names.Put([]byte(name), []byte(value.ID)); err != nil {
			return fmt.Errorf("index zone name: %w", err)
		}
		return s.validateAdmission(tx)
	}); err != nil {
		return Zone{}, fmt.Errorf("create zone: %w", err)
	}
	sortRecords(&value)
	return value, nil
}

func (s *Store) Delete(ctx context.Context, zoneID string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("delete zone: %w", err)
	}
	if err := s.db.Update(func(tx *bbolt.Tx) error {
		value, err := getZone(tx, zoneID)
		if err != nil {
			return err
		}
		if err := tx.Bucket(zonesBucket).Delete([]byte(zoneID)); err != nil {
			return fmt.Errorf("delete zone: %w", err)
		}
		if err := tx.Bucket(namesBucket).Delete([]byte(value.Name)); err != nil {
			return fmt.Errorf("delete zone name index: %w", err)
		}
		return s.validateAdmission(tx)
	}); err != nil {
		return fmt.Errorf("delete zone %q: %w", zoneID, err)
	}
	return nil
}

// ImportZone atomically creates or replaces one complete zone. All records are
// normalized and validated before the transaction commits. Dry-run executes
// the same tentative write and aggregate admission, then forces a rollback.
func (s *Store) ImportZone(ctx context.Context, rawName string, records []Record, mode ImportMode, dryRun bool) (Zone, error) {
	if err := ctx.Err(); err != nil {
		return Zone{}, fmt.Errorf("import zone: %w", err)
	}
	name, err := NormalizeName(rawName)
	if err != nil {
		return Zone{}, err
	}
	if mode != ImportCreate && mode != ImportReplace {
		return Zone{}, &ValidationError{Field: "mode", Message: "choose create or replace"}
	}
	now := s.now().UTC()
	var result Zone
	operation := func(tx *bbolt.Tx) error {
		names := tx.Bucket(namesBucket)
		existingID := names.Get([]byte(name))
		var existing *Zone
		switch mode {
		case ImportCreate:
			if existingID != nil {
				return fmt.Errorf("zone %q: %w", name, ErrAlreadyExists)
			}
		case ImportReplace:
			if existingID == nil {
				return fmt.Errorf("zone %q: %w", name, ErrNotFound)
			}
			value, err := getZone(tx, string(existingID))
			if err != nil {
				return err
			}
			existing = &value
		}

		value, err := s.buildImportedZone(name, records, existing, now)
		if err != nil {
			return err
		}
		result = value
		if err := putZone(tx, value); err != nil {
			return err
		}
		if err := names.Put([]byte(name), []byte(value.ID)); err != nil {
			return fmt.Errorf("index imported zone name: %w", err)
		}
		if err := s.validateAdmission(tx); err != nil {
			return err
		}
		if dryRun {
			return errDryRunRollback
		}
		return nil
	}
	err = s.db.Update(operation)
	if dryRun && errors.Is(err, errDryRunRollback) {
		err = nil
	}
	if err != nil {
		return Zone{}, fmt.Errorf("import zone %q: %w", name, err)
	}
	sortRecords(&result)
	return result, nil
}

func (s *Store) buildImportedZone(name string, records []Record, existing *Zone, now time.Time) (Zone, error) {
	value := Zone{
		Name:        name,
		Serial:      nextSerial(0, now),
		Nameservers: slices.Clone(s.nameservers),
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if existing == nil {
		zoneID, err := newID()
		if err != nil {
			return Zone{}, fmt.Errorf("create imported zone id: %w", err)
		}
		value.ID = zoneID
	} else {
		value.ID = existing.ID
		value.Serial = nextSerial(existing.Serial, now)
		value.CreatedAt = existing.CreatedAt
	}
	managed, err := managedRecords(value, now)
	if err != nil {
		return Zone{}, err
	}
	value.Records = managed
	for _, input := range records {
		if len(value.Records) >= MaxRecordsPerZone {
			return Zone{}, &ValidationError{Field: "records", Message: fmt.Sprintf("a zone can contain at most %d records", MaxRecordsPerZone)}
		}
		record, err := NormalizeImportedRecord(value.Name, input.Name, input.Type, input.TTL, input.Value)
		if err != nil {
			return Zone{}, err
		}
		if err := validateRRSet(value.Name, value.Records, record, ""); err != nil {
			return Zone{}, err
		}
		record.ID, err = newID()
		if err != nil {
			return Zone{}, fmt.Errorf("create imported record id: %w", err)
		}
		record.CreatedAt = now
		record.UpdatedAt = now
		value.Records = append(value.Records, record)
	}
	if err := ValidateSnapshot([]Zone{value}); err != nil {
		return Zone{}, fmt.Errorf("validate imported zone: %w", err)
	}
	return value, nil
}

// RestoreSnapshot replaces an empty store with a complete snapshot in one
// bbolt write transaction. It never deletes or overwrites existing data.
func (s *Store) RestoreSnapshot(ctx context.Context, values []Zone) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("restore snapshot: %w", err)
	}
	if err := ValidateSnapshot(values); err != nil {
		return fmt.Errorf("validate restore snapshot: %w", err)
	}
	encoded := make(map[string][]byte, len(values))
	for _, value := range values {
		raw, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("encode restored zone %q: %w", value.ID, err)
		}
		encoded[value.ID] = raw
	}
	if err := s.db.Update(func(tx *bbolt.Tx) error {
		if err := ensureEmptyBuckets(tx); err != nil {
			return err
		}
		zones := tx.Bucket(zonesBucket)
		names := tx.Bucket(namesBucket)
		for _, value := range values {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := zones.Put([]byte(value.ID), encoded[value.ID]); err != nil {
				return fmt.Errorf("restore zone %q: %w", value.ID, err)
			}
			if err := names.Put([]byte(value.Name), []byte(value.ID)); err != nil {
				return fmt.Errorf("restore zone name %q: %w", value.Name, err)
			}
		}
		return s.validateAdmission(tx)
	}); err != nil {
		return fmt.Errorf("restore snapshot: %w", err)
	}
	return nil
}

func (s *Store) CreateRecord(ctx context.Context, zoneID string, record Record) (Zone, error) {
	if err := ctx.Err(); err != nil {
		return Zone{}, fmt.Errorf("create record: %w", err)
	}
	recordID, err := newID()
	if err != nil {
		return Zone{}, fmt.Errorf("create record id: %w", err)
	}
	now := s.now().UTC()
	record.ID = recordID
	record.CreatedAt = now
	record.UpdatedAt = now

	value, err := s.updateZone(zoneID, func(value *Zone) error {
		if len(value.Records) >= MaxRecordsPerZone {
			return &ValidationError{Field: "records", Message: fmt.Sprintf("a zone can contain at most %d records", MaxRecordsPerZone)}
		}
		if err := validateRRSet(value.Name, value.Records, record, ""); err != nil {
			return err
		}
		value.Records = append(value.Records, record)
		bumpSerial(value, now)
		return nil
	})
	if err != nil {
		return Zone{}, fmt.Errorf("create record: %w", err)
	}
	return value, nil
}

func (s *Store) UpdateRecord(ctx context.Context, zoneID, recordID string, record Record) (Zone, error) {
	if err := ctx.Err(); err != nil {
		return Zone{}, fmt.Errorf("update record: %w", err)
	}
	now := s.now().UTC()
	value, err := s.updateZone(zoneID, func(value *Zone) error {
		index := slices.IndexFunc(value.Records, func(candidate Record) bool { return candidate.ID == recordID })
		if index < 0 {
			return fmt.Errorf("record %q: %w", recordID, ErrNotFound)
		}
		current := value.Records[index]
		if current.Managed {
			return fmt.Errorf("record %q: %w", recordID, ErrManagedRecord)
		}
		if err := validateRRSet(value.Name, value.Records, record, recordID); err != nil {
			return err
		}
		record.ID = current.ID
		record.CreatedAt = current.CreatedAt
		record.UpdatedAt = now
		value.Records[index] = record
		bumpSerial(value, now)
		return nil
	})
	if err != nil {
		return Zone{}, fmt.Errorf("update record: %w", err)
	}
	return value, nil
}

func (s *Store) DeleteRecord(ctx context.Context, zoneID, recordID string) (Zone, error) {
	if err := ctx.Err(); err != nil {
		return Zone{}, fmt.Errorf("delete record: %w", err)
	}
	now := s.now().UTC()
	value, err := s.updateZone(zoneID, func(value *Zone) error {
		index := slices.IndexFunc(value.Records, func(candidate Record) bool { return candidate.ID == recordID })
		if index < 0 {
			return fmt.Errorf("record %q: %w", recordID, ErrNotFound)
		}
		if value.Records[index].Managed {
			return fmt.Errorf("record %q: %w", recordID, ErrManagedRecord)
		}
		value.Records = slices.Delete(value.Records, index, index+1)
		bumpSerial(value, now)
		return nil
	})
	if err != nil {
		return Zone{}, fmt.Errorf("delete record: %w", err)
	}
	return value, nil
}

func (s *Store) updateZone(zoneID string, update func(*Zone) error) (Zone, error) {
	var value Zone
	if err := s.db.Update(func(tx *bbolt.Tx) error {
		stored, err := getZone(tx, zoneID)
		if err != nil {
			return err
		}
		if err := update(&stored); err != nil {
			return err
		}
		if err := putZone(tx, stored); err != nil {
			return err
		}
		if err := s.validateAdmission(tx); err != nil {
			return err
		}
		value = stored
		return nil
	}); err != nil {
		return Zone{}, err
	}
	sortRecords(&value)
	return value, nil
}

func managedRecords(value Zone, now time.Time) ([]Record, error) {
	soaID, err := newID()
	if err != nil {
		return nil, fmt.Errorf("create SOA id: %w", err)
	}
	records := []Record{{
		ID:        soaID,
		Name:      "@",
		Type:      TypeSOA,
		TTL:       DefaultSOATTL,
		Value:     soaValue(value),
		Managed:   true,
		CreatedAt: now,
		UpdatedAt: now,
	}}
	for _, nameserver := range value.Nameservers {
		recordID, err := newID()
		if err != nil {
			return nil, fmt.Errorf("create NS id: %w", err)
		}
		records = append(records, Record{
			ID:        recordID,
			Name:      "@",
			Type:      TypeNS,
			TTL:       DefaultSOATTL,
			Value:     nameserver,
			Managed:   true,
			CreatedAt: now,
			UpdatedAt: now,
		})
	}
	return records, nil
}

func validateRRSet(zoneName string, records []Record, candidate Record, skipID string) error {
	recordCount := 1
	candidateRR, err := Compile(zoneName, candidate)
	if err != nil {
		return fmt.Errorf("compile candidate record: %w", err)
	}
	wireSize := dns.Len(candidateRR)
	for _, existing := range records {
		if existing.ID == skipID || existing.Name != candidate.Name {
			continue
		}
		if existing.Type == TypeCNAME || candidate.Type == TypeCNAME {
			return &ValidationError{Field: "type", Message: "a CNAME cannot coexist with other records at the same name"}
		}
		if existing.Type != candidate.Type {
			continue
		}
		if existing.TTL != candidate.TTL {
			return &ValidationError{Field: "ttl", Message: "all records in an RRset must use the same TTL"}
		}
		if existing.Value == candidate.Value {
			return fmt.Errorf("record: %w", ErrAlreadyExists)
		}
		recordCount++
		existingRR, err := Compile(zoneName, existing)
		if err != nil {
			return fmt.Errorf("compile existing record %q: %w", existing.ID, err)
		}
		wireSize += dns.Len(existingRR)
	}
	if recordCount > MaxRRSetRecords {
		return &ValidationError{Field: "value", Message: fmt.Sprintf("an RRset can contain at most %d records", MaxRRSetRecords)}
	}
	if wireSize > MaxRRSetWireSize {
		return &ValidationError{Field: "value", Message: fmt.Sprintf("the RRset exceeds the %d-byte wire-size limit", MaxRRSetWireSize)}
	}
	return nil
}

func bumpSerial(value *Zone, now time.Time) {
	value.Serial = nextSerial(value.Serial, now)
	value.UpdatedAt = now
	for index := range value.Records {
		if value.Records[index].Managed && value.Records[index].Type == TypeSOA {
			value.Records[index].Value = soaValue(*value)
			value.Records[index].UpdatedAt = now
		}
	}
}

func soaValue(value Zone) string {
	return fmt.Sprintf("%s hostmaster.%s. %d 3600 600 1209600 300", value.Nameservers[0], value.Name, value.Serial)
}

func nextSerial(current uint32, now time.Time) uint32 {
	candidate := uint32(now.Unix())
	if candidate <= current {
		return current + 1
	}
	return candidate
}

func putZone(tx *bbolt.Tx, value Zone) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode zone %q: %w", value.ID, err)
	}
	if err := tx.Bucket(zonesBucket).Put([]byte(value.ID), raw); err != nil {
		return fmt.Errorf("store zone %q: %w", value.ID, err)
	}
	return nil
}

func getZone(tx *bbolt.Tx, zoneID string) (Zone, error) {
	raw := tx.Bucket(zonesBucket).Get([]byte(zoneID))
	if raw == nil {
		return Zone{}, fmt.Errorf("zone %q: %w", zoneID, ErrNotFound)
	}
	var value Zone
	if err := json.Unmarshal(raw, &value); err != nil {
		return Zone{}, fmt.Errorf("decode zone %q: %w", zoneID, err)
	}
	return value, nil
}

func newID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

func sortRecords(value *Zone) {
	slices.SortStableFunc(value.Records, func(a, b Record) int {
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
		return strings.Compare(a.Value, b.Value)
	})
}

func verifyEmptyDatabase(path string) (resultErr error) {
	db, err := bbolt.Open(path, 0o400, &bbolt.Options{ReadOnly: true, Timeout: 2 * time.Second})
	if err != nil {
		return fmt.Errorf("restore target %q is not an empty bbolt database: %w", path, err)
	}
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close restore target %q: %w", path, closeErr))
		}
	}()
	if err := db.View(ensureEmptyBuckets); err != nil {
		return fmt.Errorf("inspect restore target %q: %w", path, err)
	}
	return nil
}

func ensureEmptyBuckets(tx *bbolt.Tx) error {
	return tx.ForEach(func(name []byte, bucket *bbolt.Bucket) error {
		if !bytes.Equal(name, zonesBucket) && !bytes.Equal(name, namesBucket) {
			return fmt.Errorf("unexpected bucket %q: %w", name, ErrStoreNotEmpty)
		}
		key, _ := bucket.Cursor().First()
		if key != nil {
			return ErrStoreNotEmpty
		}
		return nil
	})
}

func (s *Store) validateAdmission(tx *bbolt.Tx) error {
	if s.admit == nil {
		return nil
	}
	values := make([]Zone, 0)
	if err := tx.Bucket(zonesBucket).ForEach(func(_, raw []byte) error {
		var value Zone
		if err := json.Unmarshal(raw, &value); err != nil {
			return fmt.Errorf("decode zone for snapshot admission: %w", err)
		}
		sortRecords(&value)
		values = append(values, value)
		return nil
	}); err != nil {
		return err
	}
	slices.SortFunc(values, func(a, b Zone) int { return strings.Compare(a.Name, b.Name) })
	if err := s.admit(values); err != nil {
		return fmt.Errorf("snapshot admission: %w", err)
	}
	return nil
}
