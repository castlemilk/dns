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

	"github.com/castlemilk/dns/internal/telemetry"
	"github.com/miekg/dns"
	"go.etcd.io/bbolt"
)

var (
	zonesBucket = []byte("zones")
	namesBucket = []byte("zone_names")

	ErrStoreNotEmpty = errors.New("zone store is not empty")
	// ErrEngineOwned is returned by UpdateRecord and DeleteRecord for a record
	// whose Source is not the user source. Engine-owned records are rewritten
	// by their reconciler within one pass, so a silent edit would not survive.
	ErrEngineOwned = errors.New("engine-owned record")
	// ErrEngineOwnedRRSet is returned when the record itself belongs to the
	// operator but the RRset its name and type land in is answered by an engine.
	// It is distinct from ErrEngineOwned because the remedy differs: nothing is
	// wrong with the record, it is the destination that is taken.
	ErrEngineOwnedRRSet = errors.New("engine-owned RRset")
	errDryRunRollback   = errors.New("rollback successful import dry-run")
)

// Record-set operation kinds accepted by ApplyRecordSet.
const (
	OpCreate = "create"
	OpUpdate = "update"
	OpDelete = "delete"
)

// Op is one change inside an ApplyRecordSet call. Creates ignore RecordID.
//
// A delete must describe its target in Record — every planner path does, by
// copying the stored record — and the store checks that the description
// matches, source included, so an operation that names the wrong id is refused
// instead of removing a record its author never meant to touch. That
// description is the only provenance the store has: ApplyRecordSet carries no
// caller identity, so a delete describing nothing could remove any record in
// the zone.
type Op struct {
	Kind     string
	RecordID string
	Record   Record
	// ReplaceUserRecord opts a delete in to removing a record the user owns.
	// That is the one deletion no reconciler may make on its own: it is only
	// ever reached through an explicit replace_conflicting_records RPC, and
	// leaving it implicit let any record id an engine held delete an
	// operator's record.
	ReplaceUserRecord bool
}

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
	metrics     *telemetry.Metrics
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

func WithMetrics(metrics *telemetry.Metrics) StoreOption {
	return func(store *Store) {
		store.metrics = telemetry.Select(metrics)
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

	store := &Store{db: db, nameservers: normalizedNameservers, now: time.Now, metrics: telemetry.Disabled()}
	for _, opt := range opts {
		opt(store)
	}
	if err := store.updateTx(context.Background(), "initialize", func(tx *bbolt.Tx) error {
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
	if err := store.updateTx(context.Background(), "reconcile_nameservers", store.reconcileNameservers); err != nil {
		if closeErr := db.Close(); closeErr != nil {
			return nil, fmt.Errorf("reconcile zone nameservers: %v; close database: %w", err, closeErr)
		}
		return nil, fmt.Errorf("reconcile zone nameservers: %w", err)
	}
	if err := store.viewTx(context.Background(), "validate_admission", store.validateAdmission); err != nil {
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
	store := &Store{db: db, now: time.Now, metrics: telemetry.Disabled()}
	if err := store.updateTx(context.Background(), "initialize_restore", func(tx *bbolt.Tx) error {
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
	if err := s.viewTx(ctx, "list_zones", func(tx *bbolt.Tx) error {
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
	if err := s.viewTx(ctx, "get_zone", func(tx *bbolt.Tx) error {
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

	if err := s.updateTx(ctx, "create_zone", func(tx *bbolt.Tx) error {
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
	if err := s.updateTx(ctx, "delete_zone", func(tx *bbolt.Tx) error {
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
	err = s.updateTx(ctx, "import_zone", operation)
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
	if err := s.updateTx(ctx, "restore_snapshot", func(tx *bbolt.Tx) error {
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

	value, err := s.updateZone(ctx, "create_record", zoneID, func(value *Zone) error {
		if len(value.Records) >= MaxRecordsPerZone {
			return &ValidationError{Field: "records", Message: fmt.Sprintf("a zone can contain at most %d records", MaxRecordsPerZone)}
		}
		// A create carries no provenance of its own, so unlike UpdateRecord and
		// DeleteRecord there is nothing here to refuse: whether the operator
		// may add a member to an RRset an engine owns is a policy the control
		// handler applies, with [EngineOwnedRRSet], while it holds the lock
		// that serialises operator and engine writes. Leaving it out here also
		// keeps a user record beside an engine record constructible, which is
		// a real state — an import strips provenance — that the reconcilers
		// have to be exercised against.
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
	value, err := s.updateZone(ctx, "update_record", zoneID, func(value *Zone) error {
		index := slices.IndexFunc(value.Records, func(candidate Record) bool { return candidate.ID == recordID })
		if index < 0 {
			return fmt.Errorf("record %q: %w", recordID, ErrNotFound)
		}
		current := value.Records[index]
		if current.Managed {
			return fmt.Errorf("record %q: %w", recordID, ErrManagedRecord)
		}
		if current.Source != SourceUser {
			return fmt.Errorf("record %q: %w", recordID, ErrEngineOwned)
		}
		// The handler refuses this too, with a friendlier message. It is
		// repeated here because the invariant belongs to the store: the comment
		// on EngineOwnedRRSet says any path that lets an operator write a record
		// must consult it, and for a long time UpdateRecord was a path that did
		// not. Editing a record checked the provenance of the record being
		// changed and never the provenance of the RRset its new name and type
		// landed in, so the same edit the Add dialog refused went through from
		// the Edit dialog.
		if source, taken := EngineOwnedRRSet(value.Records, record, recordID); taken {
			return fmt.Errorf("record %q joins the %s RRset at %q: %w", recordID, source, record.Name, ErrEngineOwnedRRSet)
		}
		if err := validateRRSet(value.Name, value.Records, record, recordID); err != nil {
			return err
		}
		record.ID = current.ID
		record.CreatedAt = current.CreatedAt
		record.UpdatedAt = now
		record.Source = current.Source
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
	value, err := s.updateZone(ctx, "delete_record", zoneID, func(value *Zone) error {
		index := slices.IndexFunc(value.Records, func(candidate Record) bool { return candidate.ID == recordID })
		if index < 0 {
			return fmt.Errorf("record %q: %w", recordID, ErrNotFound)
		}
		if value.Records[index].Managed {
			return fmt.Errorf("record %q: %w", recordID, ErrManagedRecord)
		}
		if value.Records[index].Source != SourceUser {
			return fmt.Errorf("record %q: %w", recordID, ErrEngineOwned)
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

// ApplyRecordSet applies one engine plan to a zone in a single bbolt write
// transaction: deletes first, then updates, then creates, each validated
// against the evolving record list, with the SOA serial bumped once. Any
// failing operation rolls the whole transaction back, so the zone is never
// left half-written.
//
// Provenance rules (the engine path is the only writer of sourced records):
//   - a create must carry a non-empty, valid Source;
//   - an update to a non-empty Source is always allowed;
//   - an update to the user source is allowed only when the stored record is
//     engine-owned (releasing a record back to the user), or when the operation
//     changes nothing but the TTL of a user record (RRset alignment, which the
//     engine must perform before it may add a member to a TTL-0 RRset);
//   - an update never moves a record from one engine source to the other;
//   - a delete must describe its target in Op.Record — name, type and source
//     as stored — and a record the user owns is only deleted by an operation
//     carrying Op.ReplaceUserRecord, the explicit conflict replacement. The
//     store cannot tell which engine is calling, so the description is the
//     whole of a delete's provenance: an engine that has not read a record
//     cannot describe it, and one that describes it as its own is refused when
//     it is not;
//   - managed SOA/NS records are never touched.
//
// The returned map keys are indexes into ops; it holds the ids of created
// records.
func (s *Store) ApplyRecordSet(ctx context.Context, zoneID string, ops []Op) (Zone, map[int]string, error) {
	if err := ctx.Err(); err != nil {
		return Zone{}, nil, fmt.Errorf("apply record set: %w", err)
	}
	if len(ops) == 0 {
		value, err := s.Get(ctx, zoneID)
		if err != nil {
			return Zone{}, nil, err
		}
		return value, map[int]string{}, nil
	}
	for index, op := range ops {
		switch op.Kind {
		case OpCreate, OpUpdate, OpDelete:
		default:
			return Zone{}, nil, &ValidationError{Field: fmt.Sprintf("ops[%d].kind", index), Message: "must be create, update, or delete"}
		}
		if op.Kind != OpCreate && op.RecordID == "" {
			return Zone{}, nil, &ValidationError{Field: fmt.Sprintf("ops[%d].record_id", index), Message: "is required"}
		}
	}

	now := s.now().UTC()
	created := make(map[int]string, len(ops))
	value, err := s.updateZone(ctx, "apply_record_set", zoneID, func(value *Zone) error {
		clear(created)
		// A set is applied as a batch and checked as a batch. Validating each
		// operation against the siblings it has not written yet refuses plans
		// that are correct as a whole: two members of one RRset moving from
		// TTL 0 to 3600 are only ever valid once both are written, and that is
		// exactly what the planner's alignment step emits.
		writer := map[string]int{}
		touched := map[string]struct{}{}
		for index, op := range ops {
			if op.Kind != OpDelete {
				continue
			}
			if err := deleteRecordOp(value, index, op); err != nil {
				return err
			}
		}
		for index, op := range ops {
			if op.Kind != OpUpdate {
				continue
			}
			if err := updateRecordOp(value, index, op, now); err != nil {
				return err
			}
			writer[op.RecordID] = index
			touched[recordName(value.Records, op.RecordID)] = struct{}{}
		}
		for index, op := range ops {
			if op.Kind != OpCreate {
				continue
			}
			recordID, err := createRecordOp(value, index, op, now)
			if err != nil {
				return err
			}
			created[index] = recordID
			writer[recordID] = index
			touched[recordName(value.Records, recordID)] = struct{}{}
		}
		if err := validateRecordSets(value.Name, value.Records, touched, writer); err != nil {
			return err
		}
		bumpSerial(value, now)
		return nil
	})
	if err != nil {
		return Zone{}, nil, fmt.Errorf("apply record set: %w", err)
	}
	return value, created, nil
}

func deleteRecordOp(value *Zone, index int, op Op) error {
	position := slices.IndexFunc(value.Records, func(candidate Record) bool { return candidate.ID == op.RecordID })
	if position < 0 {
		return fmt.Errorf("ops[%d]: record %q: %w", index, op.RecordID, ErrNotFound)
	}
	current := value.Records[position]
	if current.Managed {
		return fmt.Errorf("ops[%d]: record %q: %w", index, op.RecordID, ErrManagedRecord)
	}
	// A delete must describe the record it is about to remove. Every planner
	// path copies the stored record into Record, so this catches an operation
	// that carries the wrong id — an engine removing the other engine's record
	// — at the store, where the invariant belongs, rather than relying on each
	// planner to filter on Source. An op that describes nothing is refused by
	// the same comparison: a stored record's name is never empty, so an unset
	// Record can never match one.
	if op.Record.Name != current.Name || op.Record.Type != current.Type || op.Record.Source != current.Source {
		return &ValidationError{
			Field:   fmt.Sprintf("ops[%d].record", index),
			Message: "a delete must describe the record it removes, including its source",
		}
	}
	if current.Source == SourceUser && !op.ReplaceUserRecord {
		return &ValidationError{
			Field:   fmt.Sprintf("ops[%d].replace_user_record", index),
			Message: "a record the user owns is only deleted by an explicit conflict replacement",
		}
	}
	value.Records = slices.Delete(value.Records, position, position+1)
	return nil
}

func updateRecordOp(value *Zone, index int, op Op, now time.Time) error {
	position := slices.IndexFunc(value.Records, func(candidate Record) bool { return candidate.ID == op.RecordID })
	if position < 0 {
		return fmt.Errorf("ops[%d]: record %q: %w", index, op.RecordID, ErrNotFound)
	}
	current := value.Records[position]
	if current.Managed {
		return fmt.Errorf("ops[%d]: record %q: %w", index, op.RecordID, ErrManagedRecord)
	}
	record, err := normalizeOp(value.Name, index, op)
	if err != nil {
		return err
	}
	if record.Source == SourceUser && current.Source == SourceUser &&
		(record.Name != current.Name || record.Type != current.Type || record.Value != current.Value) {
		return &ValidationError{
			Field:   fmt.Sprintf("ops[%d].record.source", index),
			Message: "an engine may only change the TTL of a record it does not own",
		}
	}
	// The mirror image of the rule above: a record one engine owns is not
	// taken over by the other. Its reconciler would rewrite it on the next
	// pass, so the two would flap the RRset between them. Releasing a record
	// back to the user (record.Source == SourceUser) and aligning the TTL of a
	// record within its own source both stay allowed.
	if current.Source != SourceUser && record.Source != SourceUser && record.Source != current.Source {
		return &ValidationError{
			Field:   fmt.Sprintf("ops[%d].record.source", index),
			Message: "an engine may not take over a record another engine owns",
		}
	}
	// The RRset rules are checked once over the whole batch, in
	// validateRecordSets, after every operation has been written.
	record.ID = current.ID
	record.CreatedAt = current.CreatedAt
	record.UpdatedAt = now
	value.Records[position] = record
	return nil
}

func createRecordOp(value *Zone, index int, op Op, now time.Time) (string, error) {
	if len(value.Records) >= MaxRecordsPerZone {
		return "", &ValidationError{Field: "records", Message: fmt.Sprintf("a zone can contain at most %d records", MaxRecordsPerZone)}
	}
	record, err := normalizeOp(value.Name, index, op)
	if err != nil {
		return "", err
	}
	if record.Source == SourceUser {
		return "", &ValidationError{
			Field:   fmt.Sprintf("ops[%d].record.source", index),
			Message: "an engine record set can only create engine-owned records",
		}
	}
	// The RRset rules are checked once over the whole batch, in
	// validateRecordSets, after every operation has been written.
	recordID, err := newID()
	if err != nil {
		return "", fmt.Errorf("create record id: %w", err)
	}
	record.ID = recordID
	record.CreatedAt = now
	record.UpdatedAt = now
	value.Records = append(value.Records, record)
	return recordID, nil
}

func normalizeOp(zoneName string, index int, op Op) (Record, error) {
	if op.Record.Managed {
		return Record{}, &ValidationError{
			Field:   fmt.Sprintf("ops[%d].record.managed", index),
			Message: "managed SOA and NS records cannot be changed",
		}
	}
	if !ValidSource(op.Record.Source) {
		return Record{}, &ValidationError{
			Field:   fmt.Sprintf("ops[%d].record.source", index),
			Message: "must be empty, hosting, or mail",
		}
	}
	if op.Record.TTL == 0 {
		return Record{}, &ValidationError{
			Field:   fmt.Sprintf("ops[%d].record.ttl", index),
			Message: "must be at least 1 second",
		}
	}
	record, err := NormalizeImportedRecord(zoneName, op.Record.Name, op.Record.Type, op.Record.TTL, op.Record.Value)
	if err != nil {
		return Record{}, fmt.Errorf("ops[%d]: %w", index, err)
	}
	record.Source = op.Record.Source
	return record, nil
}

func (s *Store) updateZone(ctx context.Context, operation, zoneID string, update func(*Zone) error) (Zone, error) {
	var value Zone
	if err := s.updateTx(ctx, operation, func(tx *bbolt.Tx) error {
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

// validateDelegation refuses a change that would silently stop existing records
// resolving, in either direction.
//
// An NS RRset at a non-apex name is a zone cut: the authority answers a referral
// for that name and everything under it, so the A record already sitting there
// and every record below it stop being served the instant the NS appears. That
// is correct DNS and it is also a trap, because nothing said so — the store took
// the NS, the API returned all of it, and the console listed the blackholed
// records beside the delegation as though both were live. A customer following a
// third party's "delegate this subdomain to us" instructions took their own
// subdomain off the internet and had every screen tell them it was fine.
//
// Delegating is still allowed; it just cannot be done silently over records that
// would disappear. Glue is exempt, because an in-bailiwick nameserver needs its
// address inside the delegation to be reachable at all.
func validateDelegation(zoneName string, records []Record, candidate Record, skipID string) error {
	if candidate.Type == TypeNS && candidate.Name != "@" {
		for _, existing := range records {
			if existing.ID == skipID || !nameAtOrBelow(existing.Name, candidate.Name) {
				continue
			}
			if existing.Type == TypeNS || existing.Type == TypeSOA {
				continue
			}
			if isGlueFor(zoneName, existing, candidate) {
				continue
			}
			return &ValidationError{
				Field: "type",
				Message: fmt.Sprintf(
					"delegating %q would stop %q resolving: a non-apex NS makes this name a zone cut, so records at and below it are answered by the nameserver you delegate to, not here. Remove them first, or delegate a name that has none.",
					candidate.Name, recordLabel(existing)),
			}
		}
		return nil
	}
	if candidate.Type == TypeNS || candidate.Type == TypeSOA {
		return nil
	}
	for _, existing := range records {
		if existing.ID == skipID || existing.Type != TypeNS || existing.Name == "@" {
			continue
		}
		if !nameAtOrBelow(candidate.Name, existing.Name) {
			continue
		}
		if isGlueFor(zoneName, candidate, existing) {
			continue
		}
		return &ValidationError{
			Field: "name",
			Message: fmt.Sprintf(
				"%q is inside the delegation at %q, so this record would never be served: queries there are referred to the nameserver that name is delegated to. Remove the delegation first, or choose a name outside it.",
				candidate.Name, existing.Name),
		}
	}
	return nil
}

// nameAtOrBelow reports whether name is the delegated name itself or sits under
// it. Both are relative to the zone, so "www.shop" is below "shop" and "workshop"
// is not.
func nameAtOrBelow(name, delegated string) bool {
	if delegated == "@" {
		return false
	}
	return name == delegated || strings.HasSuffix(name, "."+delegated)
}

// isGlueFor reports whether record is the address of a nameserver named by the
// delegation, which is the one thing that legitimately lives inside a zone cut.
func isGlueFor(zoneName string, record Record, delegation Record) bool {
	if record.Type != TypeA && record.Type != TypeAAAA {
		return false
	}
	owner := dns.Fqdn(zoneName)
	if record.Name != "@" {
		owner = dns.Fqdn(record.Name + "." + zoneName)
	}
	return strings.EqualFold(owner, dns.Fqdn(delegation.Value))
}

// recordLabel names a record for an error message without echoing its value.
func recordLabel(record Record) string {
	return record.Name + " " + string(record.Type)
}

func validateRRSet(zoneName string, records []Record, candidate Record, skipID string) error {
	recordCount := 1
	if err := validateDelegation(zoneName, records, candidate, skipID); err != nil {
		return err
	}
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

// EngineOwnedRRSet reports whether an engine already owns the RRset a
// candidate record would join, and which engine that is. Adding a member to an
// engine's RRset changes what that RRset answers — a second apex A sends half
// the site's traffic to an address the hosting engine does not serve, a
// lower-priority MX takes every inbound message — so it is an edit of the
// engine's record by another route, and the create path has to refuse it the
// way update and delete refuse the record itself.
//
// Any path that lets an operator create a record must consult this. It is not
// enforced inside CreateRecord because the answer has to be read under the
// control handler's write lock, which is what makes the check race-free
// against a concurrent engine apply.
func EngineOwnedRRSet(records []Record, candidate Record, skipID string) (string, bool) {
	for _, existing := range records {
		if existing.ID == skipID {
			continue
		}
		if existing.Managed || existing.Name != candidate.Name || existing.Type != candidate.Type {
			continue
		}
		if existing.Source != SourceUser {
			return existing.Source, true
		}
	}
	return SourceUser, false
}

// recordName is the owner of a stored record, or "" when it is gone. It lets
// ApplyRecordSet collect the names a batch touched without repeating the
// index-of dance at every call site.
func recordName(records []Record, recordID string) string {
	position := slices.IndexFunc(records, func(candidate Record) bool { return candidate.ID == recordID })
	if position < 0 {
		return ""
	}
	return records[position].Name
}

// validateRecordSets checks the RRset rules over a finished record list rather
// than one candidate at a time, which is what a batch needs: an operation is
// only ever valid alongside the other operations in its set.
//
// Only the owners a batch wrote are checked — everything else was valid before
// the batch and nothing the batch did can have changed it — so the cost stays
// proportional to the plan, not to the zone. writer maps a record id to the
// operation that wrote it, so a refusal names the operation responsible
// instead of an innocent sibling.
func validateRecordSets(zoneName string, records []Record, owners map[string]struct{}, writer map[string]int) error {
	if len(owners) == 0 {
		return nil
	}
	names := make([]string, 0, len(owners))
	for name := range owners {
		if name == "" {
			continue
		}
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if err := validateRecordSetAt(zoneName, records, name, writer); err != nil {
			return err
		}
	}
	return nil
}

func validateRecordSetAt(zoneName string, records []Record, name string, writer map[string]int) error {
	members := make([]Record, 0, 4)
	for _, record := range records {
		if record.Name == name {
			members = append(members, record)
		}
	}
	fail := func(members []Record, err error) error {
		// Prefer to blame an operation this batch performed: it is the one the
		// caller can change, and the sibling it clashed with may be untouched.
		for _, member := range members {
			if index, ok := writer[member.ID]; ok {
				return fmt.Errorf("ops[%d]: %w", index, err)
			}
		}
		return err
	}

	byType := map[RecordType][]Record{}
	for _, member := range members {
		byType[member.Type] = append(byType[member.Type], member)
	}
	if aliases := len(byType[TypeCNAME]); aliases > 1 || (aliases == 1 && len(members) > 1) {
		return fail(members, &ValidationError{Field: "type", Message: "a CNAME cannot coexist with other records at the same name"})
	}

	kinds := make([]RecordType, 0, len(byType))
	for kind := range byType {
		kinds = append(kinds, kind)
	}
	slices.Sort(kinds)
	for _, kind := range kinds {
		set := byType[kind]
		if len(set) > MaxRRSetRecords {
			return fail(set, &ValidationError{Field: "value", Message: fmt.Sprintf("an RRset can contain at most %d records", MaxRRSetRecords)})
		}
		wireSize := 0
		for index, member := range set {
			if member.TTL != set[0].TTL {
				return fail([]Record{member, set[0]}, &ValidationError{Field: "ttl", Message: "all records in an RRset must use the same TTL"})
			}
			for _, earlier := range set[:index] {
				if earlier.Value == member.Value {
					return fail([]Record{member, earlier}, fmt.Errorf("record: %w", ErrAlreadyExists))
				}
			}
			rr, err := Compile(zoneName, member)
			if err != nil {
				return fail([]Record{member}, fmt.Errorf("compile record %q: %w", member.ID, err))
			}
			wireSize += dns.Len(rr)
		}
		if wireSize > MaxRRSetWireSize {
			return fail(set, &ValidationError{Field: "value", Message: fmt.Sprintf("the RRset exceeds the %d-byte wire-size limit", MaxRRSetWireSize)})
		}
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

// reconcileNameservers republishes every zone's delegation when the configured
// nameservers have changed since the zone was written.
//
// A zone stores the nameserver set it was created with, and managedRecords
// derives its NS RRset from that stored copy. Nothing used to update it, and
// nothing could: there is no UpdateZone RPC, managed SOA and NS records are
// rejected by UpdateRecord, and a REPLACE import is refused outright while a
// website or mailbox is bound. So moving the platform to a new domain left every
// existing zone publishing nameserver names that no longer resolve, permanently
// and silently — the zone still validated, because its NS records agreed with
// its own stale list. Delegating such a zone points the internet at hosts that
// answer nothing.
//
// The nameservers are a property of this deployment, not of the customer's zone,
// so the configured set is the truth and the stored copy follows it. The serial
// is bumped for every zone that changes, because a delegation that moves without
// a newer serial leaves secondaries and caches on the old answer.
//
// The store has no logger by design; this runs inside updateTx, so it is visible
// as a store transaction metric with operation "reconcile_nameservers".
func (s *Store) reconcileNameservers(tx *bbolt.Tx) error {
	zones := tx.Bucket(zonesBucket)
	if zones == nil {
		return nil
	}
	now := s.now()
	stale := make([]Zone, 0)
	if err := zones.ForEach(func(_, raw []byte) error {
		var value Zone
		if err := json.Unmarshal(raw, &value); err != nil {
			return fmt.Errorf("decode zone for nameserver reconcile: %w", err)
		}
		if !slices.Equal(value.Nameservers, s.nameservers) {
			stale = append(stale, value)
		}
		return nil
	}); err != nil {
		return err
	}
	for _, value := range stale {
		value.Nameservers = slices.Clone(s.nameservers)
		value.Serial = nextSerial(value.Serial, now)
		value.UpdatedAt = now

		// Rebuild only the managed SOA and NS records. Everything the customer
		// put in the zone is carried across untouched, which is the whole point
		// of doing this here rather than asking an operator to re-import.
		managed, err := managedRecords(value, now)
		if err != nil {
			return err
		}
		for _, record := range value.Records {
			if record.Managed {
				continue
			}
			managed = append(managed, record)
		}
		value.Records = managed
		if err := ValidateSnapshot([]Zone{value}); err != nil {
			return fmt.Errorf("validate zone %q after nameserver reconcile: %w", value.Name, err)
		}
		if err := putZone(tx, value); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) validateAdmission(tx *bbolt.Tx) error {
	if s.admit == nil {
		return nil
	}
	started := time.Time{}
	if s.metrics.MetricsEnabled() {
		started = time.Now()
	}
	result := "success"
	defer func() {
		elapsed := time.Duration(0)
		if !started.IsZero() {
			elapsed = time.Since(started)
		}
		s.metrics.SnapshotAdmission(context.Background(), result, elapsed)
	}()
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
		result = "error"
		return err
	}
	slices.SortFunc(values, func(a, b Zone) int { return strings.Compare(a.Name, b.Name) })
	if err := s.admit(values); err != nil {
		result = "error"
		return fmt.Errorf("snapshot admission: %w", err)
	}
	return nil
}

func (s *Store) viewTx(ctx context.Context, operation string, transaction func(*bbolt.Tx) error) (err error) {
	started := time.Time{}
	if s.metrics.MetricsEnabled() {
		started = time.Now()
	}
	err = s.db.View(transaction)
	outcome := "success"
	if err != nil {
		outcome = "error"
	}
	elapsed := time.Duration(0)
	if !started.IsZero() {
		elapsed = time.Since(started)
	}
	s.metrics.StoreTransaction(ctx, operation, "read", outcome, elapsed)
	return err
}

func (s *Store) updateTx(ctx context.Context, operation string, transaction func(*bbolt.Tx) error) (err error) {
	started := time.Time{}
	if s.metrics.MetricsEnabled() {
		started = time.Now()
	}
	err = s.db.Update(transaction)
	outcome := "success"
	if err != nil {
		outcome = "error"
	}
	elapsed := time.Duration(0)
	if !started.IsZero() {
		elapsed = time.Since(started)
	}
	s.metrics.StoreTransaction(ctx, operation, "write", outcome, elapsed)
	return err
}
