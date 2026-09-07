package maild

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.etcd.io/bbolt"
)

// Sentinels. Every store failure that a JMAP handler has to turn into a
// specific error type is one of these, so the mapping lives in one place.
var (
	// ErrNotFound is a read or an update of an object that is not there.
	ErrNotFound = errors.New("maild: not found")
	// ErrExists is a create whose id, or whose address, is already taken. It
	// becomes primaryKeyViolation on the wire.
	ErrExists = errors.New("maild: already exists")
	// ErrLinked is a delete refused because children still reference the
	// object. It becomes objectIsLinked on the wire; errors.As for *LinkedError
	// recovers the children to name in linkedObjects.
	ErrLinked = errors.New("maild: still referenced")
	// ErrInvalid is a document the store refuses to write: a mismatched id, an
	// empty natural key, a missing parent.
	ErrInvalid = errors.New("maild: invalid document")
)

// JMAP object names, used in the linkedObjects of an objectIsLinked refusal.
const (
	ObjectDomain        = "Domain"
	ObjectAccount       = "Account"
	ObjectMailingList   = "MailingList"
	ObjectDkimSignature = "DkimSignature"
)

// maxLinkedObjects bounds the children named in a LinkedError. The facade only
// needs to know that children exist; naming every one of a thousand mailboxes
// would make the refusal larger than the request that caused it.
const maxLinkedObjects = 64

// LinkedRef names one child that keeps a parent from being deleted.
type LinkedRef struct {
	Object string
	ID     string
}

// LinkedError is the store's objectIsLinked refusal.
type LinkedError struct {
	ID     string
	Linked []LinkedRef
}

func (e *LinkedError) Error() string {
	return fmt.Sprintf("maild: %s is still referenced by %d objects", e.ID, len(e.Linked))
}

// Is makes errors.Is(err, ErrLinked) true, so a caller that does not need the
// children does not have to type-assert.
func (e *LinkedError) Is(target error) bool { return target == ErrLinked }

var (
	metaBucket    = []byte("meta")
	domainsBucket = []byte("domains")
	accountBucket = []byte("accounts")
	aliasBucket   = []byte("aliases")
	listsBucket   = []byte("lists")
	dkimBucket    = []byte("dkim")
	// queueBucket is created but not used here. The outbound queue belongs to
	// the delivery phase; the bucket exists already so adding it needs no
	// migration.
	queueBucket = []byte("queue")

	buckets = [][]byte{
		metaBucket, domainsBucket, accountBucket, aliasBucket,
		listsBucket, dkimBucket, queueBucket,
	}

	schemaKey = []byte("schema")
)

// Store is maild's state file: domains, mailboxes, mailing lists and DKIM keys.
// Message bodies are never held here — they live in the spool.
type Store struct {
	db   *bbolt.DB
	path string
	now  func() time.Time
}

// Option customises a Store at open time.
type Option func(*Store)

// WithClock replaces the store's clock. Tests use it so stamped timestamps are
// deterministic.
func WithClock(now func() time.Time) Option {
	return func(s *Store) {
		if now != nil {
			s.now = now
		}
	}
}

// Open creates or opens the store. It refuses a file written by a newer schema
// rather than silently dropping fields it cannot represent.
func Open(path string, options ...Option) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("maild: store path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("maild: create the data directory: %w", err)
	}
	db, err := bbolt.Open(path, 0o600, &bbolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("maild: open the database: %w", err)
	}
	store := &Store{db: db, path: path, now: time.Now}
	for _, option := range options {
		option(store)
	}
	if err := store.migrate(); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return store, nil
}

// Close releases the file lock.
func (s *Store) Close() error { return s.db.Close() }

// Path is the file this store was opened from.
func (s *Store) Path() string { return s.path }

func (s *Store) migrate() error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		for _, name := range buckets {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return fmt.Errorf("maild: create the %s bucket: %w", name, err)
			}
		}
		meta := tx.Bucket(metaBucket)
		raw := meta.Get(schemaKey)
		if raw == nil {
			return meta.Put(schemaKey, []byte(fmt.Sprint(DocVersion)))
		}
		var version int
		if _, err := fmt.Sscanf(string(raw), "%d", &version); err != nil {
			return fmt.Errorf("maild: the store has an unreadable schema version")
		}
		if version > DocVersion {
			return fmt.Errorf("maild: the store was written by schema %d, this build understands %d", version, DocVersion)
		}
		return nil
	})
}

// -----------------------------------------------------------------------------
// Deterministic identifiers
//
// An object's id is a hash of its natural key, so an address resolves to an
// account with one keyed read and no secondary index — which is what makes the
// SMTP path cheap. The cost is that a recreated domain reuses its old id, so
// DeleteDomain refuses while any child still exists (which is also the
// objectIsLinked refusal the facade expects).
// -----------------------------------------------------------------------------

// DomainID is the id of the domain called name.
func DomainID(name string) string {
	return deriveID("d", "domain", NormalizeDomainName(name))
}

// AccountID is the id of the mailbox at localPart in domainID.
func AccountID(domainID, localPart string) string {
	return deriveID("a", "account", domainID, NormalizeLocalPart(localPart))
}

// ListID is the id of the mailing list at localPart in domainID.
func ListID(domainID, localPart string) string {
	return deriveID("l", "list", domainID, NormalizeLocalPart(localPart))
}

// DkimID is the id of the signing key with selector in domainID.
func DkimID(domainID, selector string) string {
	return deriveID("k", "dkim", domainID, strings.ToLower(strings.TrimSpace(selector)))
}

// idEncoding is lowercase base32 without padding: 16 characters for 10 bytes of
// digest, safe in a JSON object key, a URL and a log line.
var idEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

func deriveID(prefix string, parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return prefix + idEncoding.EncodeToString(sum[:10])
}

// -----------------------------------------------------------------------------
// Domains
// -----------------------------------------------------------------------------

// CreateDomain writes a new domain and refuses one that already exists.
func (s *Store) CreateDomain(ctx context.Context, domain Domain) (Domain, error) {
	return s.writeDomain(ctx, domain, true)
}

// PutDomain writes a domain, creating or replacing it.
func (s *Store) PutDomain(ctx context.Context, domain Domain) (Domain, error) {
	return s.writeDomain(ctx, domain, false)
}

func (s *Store) writeDomain(ctx context.Context, domain Domain, mustBeNew bool) (Domain, error) {
	domain.Name = NormalizeDomainName(domain.Name)
	if domain.Name == "" {
		return Domain{}, fmt.Errorf("%w: a domain needs a name", ErrInvalid)
	}
	if domain.ID == "" {
		domain.ID = DomainID(domain.Name)
	}
	if domain.ID != DomainID(domain.Name) {
		return Domain{}, fmt.Errorf("%w: domain id %q does not derive from %q", ErrInvalid, domain.ID, domain.Name)
	}
	domain.V = DocVersion
	err := s.update(ctx, func(tx *bbolt.Tx) error {
		existing := tx.Bucket(domainsBucket).Get([]byte(domain.ID))
		if mustBeNew && existing != nil {
			return fmt.Errorf("%w: a domain called %q", ErrExists, domain.Name)
		}
		s.stamp(&domain.CreatedAt, &domain.UpdatedAt)
		return putJSON(tx, domainsBucket, []byte(domain.ID), domain)
	})
	if err != nil {
		return Domain{}, err
	}
	return domain, nil
}

// GetDomain reads one domain by id.
func (s *Store) GetDomain(ctx context.Context, id string) (Domain, error) {
	var domain Domain
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		return getJSON(tx, domainsBucket, []byte(id), &domain)
	})
	return domain, err
}

// FindDomain reads one domain by name. The id is derived from the name, so this
// is a keyed read rather than a scan.
func (s *Store) FindDomain(ctx context.Context, name string) (Domain, error) {
	return s.GetDomain(ctx, DomainID(name))
}

// ListDomains returns every domain, ordered by name.
func (s *Store) ListDomains(ctx context.Context) ([]Domain, error) {
	var domains []Domain
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		return eachJSON(tx, domainsBucket, func(decode func(any) error) error {
			var domain Domain
			if err := decode(&domain); err != nil {
				return err
			}
			domains = append(domains, domain)
			return nil
		})
	})
	sort.Slice(domains, func(i, j int) bool { return domains[i].Name < domains[j].Name })
	return domains, err
}

// DeleteDomain removes a domain. It refuses with a *LinkedError while any
// account, mailing list or DKIM key still references it, and reports a delete of
// an absent domain as success — both are what the facade's unbind path expects.
func (s *Store) DeleteDomain(ctx context.Context, id string) error {
	return s.update(ctx, func(tx *bbolt.Tx) error {
		if tx.Bucket(domainsBucket).Get([]byte(id)) == nil {
			return nil
		}
		linked, err := linkedChildren(tx, id)
		if err != nil {
			return err
		}
		if len(linked) > 0 {
			return &LinkedError{ID: id, Linked: linked}
		}
		return tx.Bucket(domainsBucket).Delete([]byte(id))
	})
}

func linkedChildren(tx *bbolt.Tx, domainID string) ([]LinkedRef, error) {
	var linked []LinkedRef
	add := func(object, id string) bool {
		if len(linked) >= maxLinkedObjects {
			return false
		}
		linked = append(linked, LinkedRef{Object: object, ID: id})
		return true
	}
	err := eachJSON(tx, accountBucket, func(decode func(any) error) error {
		var account Account
		if err := decode(&account); err != nil {
			return err
		}
		if account.DomainID == domainID {
			add(ObjectAccount, account.ID)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	err = eachJSON(tx, listsBucket, func(decode func(any) error) error {
		var list List
		if err := decode(&list); err != nil {
			return err
		}
		if list.DomainID == domainID {
			add(ObjectMailingList, list.ID)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	err = eachJSON(tx, dkimBucket, func(decode func(any) error) error {
		var key DkimKey
		if err := decode(&key); err != nil {
			return err
		}
		if key.DomainID == domainID {
			add(ObjectDkimSignature, key.ID)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return linked, nil
}

// -----------------------------------------------------------------------------
// Accounts
// -----------------------------------------------------------------------------

// CreateAccount writes a new mailbox and refuses an address that is taken by
// another mailbox, a mailing list or an alias in the same domain.
func (s *Store) CreateAccount(ctx context.Context, account Account) (Account, error) {
	return s.writeAccount(ctx, account, true)
}

// PutAccount writes a mailbox, creating or replacing it. Replacing rewrites the
// alias index, so an alias dropped from Aliases stops resolving.
func (s *Store) PutAccount(ctx context.Context, account Account) (Account, error) {
	return s.writeAccount(ctx, account, false)
}

func (s *Store) writeAccount(ctx context.Context, account Account, mustBeNew bool) (Account, error) {
	account.LocalPart = NormalizeLocalPart(account.LocalPart)
	if account.DomainID == "" || account.LocalPart == "" {
		return Account{}, fmt.Errorf("%w: a mailbox needs a domain and a local part", ErrInvalid)
	}
	if account.ID == "" {
		account.ID = AccountID(account.DomainID, account.LocalPart)
	}
	if account.ID != AccountID(account.DomainID, account.LocalPart) {
		return Account{}, fmt.Errorf("%w: mailbox id %q does not derive from its address", ErrInvalid, account.ID)
	}
	aliases, err := normalizeAliases(account.DomainID, account.Aliases)
	if err != nil {
		return Account{}, err
	}
	account.Aliases = aliases
	account.V = DocVersion

	err = s.update(ctx, func(tx *bbolt.Tx) error {
		if tx.Bucket(domainsBucket).Get([]byte(account.DomainID)) == nil {
			return fmt.Errorf("%w: domain %s", ErrNotFound, account.DomainID)
		}
		var previous Account
		hasPrevious := false
		if raw := tx.Bucket(accountBucket).Get([]byte(account.ID)); raw != nil {
			if mustBeNew {
				return fmt.Errorf("%w: a mailbox at this address", ErrExists)
			}
			if err := json.Unmarshal(raw, &previous); err != nil {
				return fmt.Errorf("maild: decode the stored mailbox: %w", err)
			}
			hasPrevious = true
		}
		// The mailbox's own address, then every alias, must be free of anything
		// but this mailbox.
		if err := requireAddressFree(tx, account.DomainID, account.LocalPart, account.ID); err != nil {
			return err
		}
		for _, alias := range account.Aliases {
			if err := requireAddressFree(tx, account.DomainID, alias.LocalPart, account.ID); err != nil {
				return err
			}
		}
		if hasPrevious {
			for _, alias := range previous.Aliases {
				if err := tx.Bucket(aliasBucket).Delete(aliasKey(alias.DomainID, alias.LocalPart)); err != nil {
					return err
				}
			}
			if account.CreatedAt.IsZero() {
				account.CreatedAt = previous.CreatedAt
			}
		}
		for _, alias := range account.Aliases {
			if err := tx.Bucket(aliasBucket).Put(aliasKey(alias.DomainID, alias.LocalPart), []byte(account.ID)); err != nil {
				return err
			}
		}
		s.stamp(&account.CreatedAt, &account.UpdatedAt)
		return putJSON(tx, accountBucket, []byte(account.ID), account)
	})
	if err != nil {
		return Account{}, err
	}
	return account, nil
}

// normalizeAliases lowercases, defaults the domain, drops duplicates and orders
// the set, so a stored account and every wire rendering of it are stable.
func normalizeAliases(domainID string, aliases []Alias) ([]Alias, error) {
	if len(aliases) == 0 {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(aliases))
	normalized := make([]Alias, 0, len(aliases))
	for _, alias := range aliases {
		alias.LocalPart = NormalizeLocalPart(alias.LocalPart)
		if alias.LocalPart == "" {
			return nil, fmt.Errorf("%w: an alias needs a local part", ErrInvalid)
		}
		if alias.DomainID == "" {
			alias.DomainID = domainID
		}
		if alias.DomainID != domainID {
			// The facade only ever writes aliases in the mailbox's own domain,
			// and an alias elsewhere would need an index entry under a domain
			// this account does not belong to.
			return nil, fmt.Errorf("%w: an alias must be in the mailbox's own domain", ErrInvalid)
		}
		if _, duplicate := seen[alias.LocalPart]; duplicate {
			continue
		}
		seen[alias.LocalPart] = struct{}{}
		normalized = append(normalized, alias)
	}
	sortAliases(normalized)
	return normalized, nil
}

// GetAccount reads one mailbox by id.
func (s *Store) GetAccount(ctx context.Context, id string) (Account, error) {
	var account Account
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		return getJSON(tx, accountBucket, []byte(id), &account)
	})
	return account, err
}

// FindAccount reads the mailbox at localPart in domainID.
func (s *Store) FindAccount(ctx context.Context, domainID, localPart string) (Account, error) {
	return s.GetAccount(ctx, AccountID(domainID, localPart))
}

// AccountByAlias reads the mailbox an alias delivers into. It is a keyed read
// against the alias index, so an SMTP RCPT TO costs one lookup rather than a
// scan of the domain.
func (s *Store) AccountByAlias(ctx context.Context, domainID, localPart string) (Account, error) {
	var account Account
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		id := tx.Bucket(aliasBucket).Get(aliasKey(domainID, localPart))
		if id == nil {
			return fmt.Errorf("%w: alias", ErrNotFound)
		}
		return getJSON(tx, accountBucket, id, &account)
	})
	return account, err
}

// ListAccounts returns one domain's mailboxes ordered by local part, or every
// mailbox when domainID is empty.
func (s *Store) ListAccounts(ctx context.Context, domainID string) ([]Account, error) {
	var accounts []Account
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		return eachJSON(tx, accountBucket, func(decode func(any) error) error {
			var account Account
			if err := decode(&account); err != nil {
				return err
			}
			if domainID != "" && account.DomainID != domainID {
				return nil
			}
			accounts = append(accounts, account)
			return nil
		})
	})
	sort.Slice(accounts, func(i, j int) bool {
		if accounts[i].DomainID != accounts[j].DomainID {
			return accounts[i].DomainID < accounts[j].DomainID
		}
		return accounts[i].LocalPart < accounts[j].LocalPart
	})
	return accounts, err
}

// DeleteAccount removes a mailbox and its aliases. Deleting one that is not
// there is success.
func (s *Store) DeleteAccount(ctx context.Context, id string) error {
	return s.update(ctx, func(tx *bbolt.Tx) error {
		raw := tx.Bucket(accountBucket).Get([]byte(id))
		if raw == nil {
			return nil
		}
		var account Account
		if err := json.Unmarshal(raw, &account); err != nil {
			return fmt.Errorf("maild: decode the stored mailbox: %w", err)
		}
		for _, alias := range account.Aliases {
			if err := tx.Bucket(aliasBucket).Delete(aliasKey(alias.DomainID, alias.LocalPart)); err != nil {
				return err
			}
		}
		return tx.Bucket(accountBucket).Delete([]byte(id))
	})
}

// SetPassword replaces a mailbox's credential. The plaintext is hashed here and
// is not held anywhere else; the caller drops it immediately afterwards.
func (s *Store) SetPassword(ctx context.Context, id, secret string) error {
	hash, err := HashPassword(secret)
	if err != nil {
		return err
	}
	return s.update(ctx, func(tx *bbolt.Tx) error {
		var account Account
		if err := getJSON(tx, accountBucket, []byte(id), &account); err != nil {
			return err
		}
		account.Password = hash
		account.UpdatedAt = s.now().UTC()
		return putJSON(tx, accountBucket, []byte(id), account)
	})
}

// -----------------------------------------------------------------------------
// Mailing lists
// -----------------------------------------------------------------------------

// CreateList writes a new mailing list and refuses a taken address.
func (s *Store) CreateList(ctx context.Context, list List) (List, error) {
	return s.writeList(ctx, list, true)
}

// PutList writes a mailing list, creating or replacing it.
func (s *Store) PutList(ctx context.Context, list List) (List, error) {
	return s.writeList(ctx, list, false)
}

func (s *Store) writeList(ctx context.Context, list List, mustBeNew bool) (List, error) {
	list.LocalPart = NormalizeLocalPart(list.LocalPart)
	if list.DomainID == "" || list.LocalPart == "" {
		return List{}, fmt.Errorf("%w: a mailing list needs a domain and a local part", ErrInvalid)
	}
	if list.ID == "" {
		list.ID = ListID(list.DomainID, list.LocalPart)
	}
	if list.ID != ListID(list.DomainID, list.LocalPart) {
		return List{}, fmt.Errorf("%w: mailing list id %q does not derive from its address", ErrInvalid, list.ID)
	}
	list.Recipients = normalizeRecipients(list.Recipients)
	list.V = DocVersion

	err := s.update(ctx, func(tx *bbolt.Tx) error {
		if tx.Bucket(domainsBucket).Get([]byte(list.DomainID)) == nil {
			return fmt.Errorf("%w: domain %s", ErrNotFound, list.DomainID)
		}
		if raw := tx.Bucket(listsBucket).Get([]byte(list.ID)); raw != nil {
			if mustBeNew {
				return fmt.Errorf("%w: a mailing list at this address", ErrExists)
			}
			var previous List
			if err := json.Unmarshal(raw, &previous); err != nil {
				return fmt.Errorf("maild: decode the stored mailing list: %w", err)
			}
			if list.CreatedAt.IsZero() {
				list.CreatedAt = previous.CreatedAt
			}
		}
		if err := requireAddressFree(tx, list.DomainID, list.LocalPart, list.ID); err != nil {
			return err
		}
		s.stamp(&list.CreatedAt, &list.UpdatedAt)
		return putJSON(tx, listsBucket, []byte(list.ID), list)
	})
	if err != nil {
		return List{}, err
	}
	return list, nil
}

func normalizeRecipients(recipients []string) []string {
	if len(recipients) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(recipients))
	normalized := make([]string, 0, len(recipients))
	for _, address := range recipients {
		address = strings.ToLower(strings.TrimSpace(address))
		if address == "" {
			continue
		}
		if _, duplicate := seen[address]; duplicate {
			continue
		}
		seen[address] = struct{}{}
		normalized = append(normalized, address)
	}
	sort.Strings(normalized)
	return normalized
}

// GetList reads one mailing list by id.
func (s *Store) GetList(ctx context.Context, id string) (List, error) {
	var list List
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		return getJSON(tx, listsBucket, []byte(id), &list)
	})
	return list, err
}

// FindList reads the mailing list at localPart in domainID.
func (s *Store) FindList(ctx context.Context, domainID, localPart string) (List, error) {
	return s.GetList(ctx, ListID(domainID, localPart))
}

// ListLists returns one domain's mailing lists, or every list when domainID is
// empty — which is what the facade's unfiltered x:MailingList/query needs.
func (s *Store) ListLists(ctx context.Context, domainID string) ([]List, error) {
	var lists []List
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		return eachJSON(tx, listsBucket, func(decode func(any) error) error {
			var list List
			if err := decode(&list); err != nil {
				return err
			}
			if domainID != "" && list.DomainID != domainID {
				return nil
			}
			lists = append(lists, list)
			return nil
		})
	})
	sort.Slice(lists, func(i, j int) bool {
		if lists[i].DomainID != lists[j].DomainID {
			return lists[i].DomainID < lists[j].DomainID
		}
		return lists[i].LocalPart < lists[j].LocalPart
	})
	return lists, err
}

// DeleteList removes a mailing list. Deleting one that is not there is success.
func (s *Store) DeleteList(ctx context.Context, id string) error {
	return s.update(ctx, func(tx *bbolt.Tx) error {
		return tx.Bucket(listsBucket).Delete([]byte(id))
	})
}

// -----------------------------------------------------------------------------
// DKIM keys
// -----------------------------------------------------------------------------

// PutDkimKey writes a signing key, creating or replacing it.
func (s *Store) PutDkimKey(ctx context.Context, key DkimKey) (DkimKey, error) {
	key.Selector = strings.ToLower(strings.TrimSpace(key.Selector))
	if key.DomainID == "" || key.Selector == "" {
		return DkimKey{}, fmt.Errorf("%w: a DKIM key needs a domain and a selector", ErrInvalid)
	}
	switch key.Algorithm {
	case DkimAlgorithmEd25519, DkimAlgorithmRSA:
	default:
		return DkimKey{}, fmt.Errorf("%w: unknown DKIM algorithm %q", ErrInvalid, key.Algorithm)
	}
	if key.ID == "" {
		key.ID = DkimID(key.DomainID, key.Selector)
	}
	if key.ID != DkimID(key.DomainID, key.Selector) {
		return DkimKey{}, fmt.Errorf("%w: DKIM key id %q does not derive from its selector", ErrInvalid, key.ID)
	}
	key.V = DocVersion
	err := s.update(ctx, func(tx *bbolt.Tx) error {
		if tx.Bucket(domainsBucket).Get([]byte(key.DomainID)) == nil {
			return fmt.Errorf("%w: domain %s", ErrNotFound, key.DomainID)
		}
		if raw := tx.Bucket(dkimBucket).Get([]byte(key.ID)); raw != nil && key.CreatedAt.IsZero() {
			var previous DkimKey
			if err := json.Unmarshal(raw, &previous); err != nil {
				return fmt.Errorf("maild: decode the stored DKIM key: %w", err)
			}
			key.CreatedAt = previous.CreatedAt
		}
		var updated time.Time
		s.stamp(&key.CreatedAt, &updated)
		return putJSON(tx, dkimBucket, []byte(key.ID), key)
	})
	if err != nil {
		return DkimKey{}, err
	}
	return key, nil
}

// GetDkimKey reads one signing key by id.
func (s *Store) GetDkimKey(ctx context.Context, id string) (DkimKey, error) {
	var key DkimKey
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		return getJSON(tx, dkimBucket, []byte(id), &key)
	})
	return key, err
}

// ListDkimKeys returns one domain's signing keys ordered by selector, which is
// the order the facade's client sorts into anyway.
func (s *Store) ListDkimKeys(ctx context.Context, domainID string) ([]DkimKey, error) {
	var keys []DkimKey
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		return eachJSON(tx, dkimBucket, func(decode func(any) error) error {
			var key DkimKey
			if err := decode(&key); err != nil {
				return err
			}
			if domainID != "" && key.DomainID != domainID {
				return nil
			}
			keys = append(keys, key)
			return nil
		})
	})
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].DomainID != keys[j].DomainID {
			return keys[i].DomainID < keys[j].DomainID
		}
		return keys[i].Selector < keys[j].Selector
	})
	return keys, err
}

// DeleteDkimKey removes a signing key. Deleting one that is not there is
// success, which is what the facade's destroy path relies on.
func (s *Store) DeleteDkimKey(ctx context.Context, id string) error {
	return s.update(ctx, func(tx *bbolt.Tx) error {
		return tx.Bucket(dkimBucket).Delete([]byte(id))
	})
}

// -----------------------------------------------------------------------------
// Address space
// -----------------------------------------------------------------------------

// AddressTaken reports whether localPart in domainID is already used by a
// mailbox, a mailing list or an alias.
func (s *Store) AddressTaken(ctx context.Context, domainID, localPart string) (bool, error) {
	taken := false
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		err := requireAddressFree(tx, domainID, localPart, "")
		if errors.Is(err, ErrExists) {
			taken = true
			return nil
		}
		return err
	})
	return taken, err
}

// requireAddressFree refuses an address held by anything other than ownerID.
// The three holders are a mailbox, a mailing list and an alias, and all three
// share one address space inside a domain: the facade's addressTaken check makes
// the same rule, and the two must not disagree.
func requireAddressFree(tx *bbolt.Tx, domainID, localPart, ownerID string) error {
	local := NormalizeLocalPart(localPart)
	if accountID := AccountID(domainID, local); accountID != ownerID {
		if tx.Bucket(accountBucket).Get([]byte(accountID)) != nil {
			return fmt.Errorf("%w: a mailbox holds %q", ErrExists, local)
		}
	}
	if listID := ListID(domainID, local); listID != ownerID {
		if tx.Bucket(listsBucket).Get([]byte(listID)) != nil {
			return fmt.Errorf("%w: a mailing list holds %q", ErrExists, local)
		}
	}
	if owner := tx.Bucket(aliasBucket).Get(aliasKey(domainID, local)); owner != nil && string(owner) != ownerID {
		return fmt.Errorf("%w: an alias holds %q", ErrExists, local)
	}
	return nil
}

func aliasKey(domainID, localPart string) []byte {
	return []byte(domainID + "/" + NormalizeLocalPart(localPart))
}

// -----------------------------------------------------------------------------
// Passwords
//
// PBKDF2-HMAC-SHA256 (RFC 2898 §5.2), implemented here because
// golang.org/x/crypto is not a dependency of this module and adding one is not
// allowed. The digest is compared in constant time; neither the plaintext nor
// the derived bytes are ever printed.
// -----------------------------------------------------------------------------

const (
	// PasswordAlgorithm names the KDF stored alongside every digest, so a
	// future change can re-hash rather than lock everyone out.
	PasswordAlgorithm = "pbkdf2-sha256"
	// DefaultPasswordIterations is the work factor for a new credential.
	// Mailbox passwords here are generated five-word passphrases (~61.6 bits),
	// not user-chosen ones, so the KDF is defence in depth against a stolen
	// store rather than against guessing; 600,000 is the OWASP figure for
	// PBKDF2-HMAC-SHA256 and costs a few hundred milliseconds per login.
	DefaultPasswordIterations = 600_000

	passwordSaltBytes = 16
	passwordKeyBytes  = 32
)

// PasswordHash is a derived mailbox credential. It never prints itself, and no
// wire object carries it.
type PasswordHash struct {
	Algorithm  string `json:"alg,omitempty"`
	Iterations int    `json:"iter,omitempty"`
	Salt       Secret `json:"salt,omitempty"`
	Digest     Secret `json:"digest,omitempty"`
}

// IsZero reports whether no credential is set. It also drives encoding/json's
// omitzero on Account.Password.
func (h PasswordHash) IsZero() bool {
	return h.Algorithm == "" && h.Iterations == 0 && len(h.Salt) == 0 && len(h.Digest) == 0
}

// String redacts. A mailbox formatted with %v must not print its credential,
// and the only way to guarantee that is for the type to refuse.
func (PasswordHash) String() string { return "[redacted]" }

// GoString covers %#v.
func (PasswordHash) GoString() string { return "[redacted]" }

// HashPassword derives a credential at the default work factor.
func HashPassword(secret string) (PasswordHash, error) {
	return hashPasswordWith(secret, DefaultPasswordIterations)
}

// hashPasswordWith is HashPassword with an explicit work factor, so tests can
// exercise the shape without paying for 600,000 iterations each time.
func hashPasswordWith(secret string, iterations int) (PasswordHash, error) {
	if secret == "" {
		return PasswordHash{}, fmt.Errorf("%w: a mailbox credential cannot be empty", ErrInvalid)
	}
	if iterations <= 0 {
		return PasswordHash{}, fmt.Errorf("%w: the password work factor must be positive", ErrInvalid)
	}
	salt := make([]byte, passwordSaltBytes)
	if _, err := rand.Read(salt); err != nil {
		// There is no weaker fallback on purpose: a predictable salt is worse
		// than an error the caller reports.
		return PasswordHash{}, fmt.Errorf("maild: generate a password salt: %w", err)
	}
	return PasswordHash{
		Algorithm:  PasswordAlgorithm,
		Iterations: iterations,
		Salt:       salt,
		Digest:     pbkdf2SHA256([]byte(secret), salt, iterations, passwordKeyBytes),
	}, nil
}

// Verify reports whether secret is this credential. An unset or unrecognised
// hash never verifies, so a mailbox with no credential cannot be authenticated
// into by sending nothing.
func (h PasswordHash) Verify(secret string) bool {
	if h.Algorithm != PasswordAlgorithm || h.Iterations <= 0 || len(h.Digest) == 0 || len(h.Salt) == 0 {
		return false
	}
	candidate := pbkdf2SHA256([]byte(secret), h.Salt, h.Iterations, len(h.Digest))
	return subtle.ConstantTimeCompare(candidate, h.Digest) == 1
}

// pbkdf2SHA256 is PBKDF2 with HMAC-SHA256 as the pseudorandom function.
func pbkdf2SHA256(password, salt []byte, iterations, keyLen int) []byte {
	mac := hmac.New(sha256.New, password)
	hashLen := mac.Size()
	blocks := (keyLen + hashLen - 1) / hashLen

	derived := make([]byte, 0, blocks*hashLen)
	counter := make([]byte, 4)
	previous := make([]byte, 0, hashLen)
	accumulated := make([]byte, hashLen)

	for block := 1; block <= blocks; block++ {
		mac.Reset()
		mac.Write(salt)
		binary.BigEndian.PutUint32(counter, uint32(block))
		mac.Write(counter)
		previous = mac.Sum(previous[:0])
		copy(accumulated, previous)

		for round := 2; round <= iterations; round++ {
			mac.Reset()
			mac.Write(previous)
			previous = mac.Sum(previous[:0])
			for index := range accumulated {
				accumulated[index] ^= previous[index]
			}
		}
		derived = append(derived, accumulated...)
	}
	return derived[:keyLen]
}

// -----------------------------------------------------------------------------
// Transaction helpers
// -----------------------------------------------------------------------------

func (s *Store) view(ctx context.Context, transaction func(*bbolt.Tx) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.db.View(transaction)
}

func (s *Store) update(ctx context.Context, transaction func(*bbolt.Tx) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.db.Update(transaction)
}

// stamp fills a zero CreatedAt and always refreshes UpdatedAt, from the store's
// clock so tests are deterministic.
func (s *Store) stamp(createdAt, updatedAt *time.Time) {
	now := s.now().UTC()
	if createdAt.IsZero() {
		*createdAt = now
	}
	*updatedAt = now
}

func putJSON(tx *bbolt.Tx, bucket, key []byte, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("maild: encode a %s document: %w", bucket, err)
	}
	return tx.Bucket(bucket).Put(key, payload)
}

func getJSON(tx *bbolt.Tx, bucket, key []byte, target any) error {
	raw := tx.Bucket(bucket).Get(key)
	if raw == nil {
		return fmt.Errorf("%w: %s %s", ErrNotFound, bucket, key)
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return fmt.Errorf("maild: decode a %s document: %w", bucket, err)
	}
	return nil
}

func eachJSON(tx *bbolt.Tx, bucket []byte, visit func(decode func(any) error) error) error {
	return tx.Bucket(bucket).ForEach(func(_, raw []byte) error {
		return visit(func(target any) error {
			if err := json.Unmarshal(raw, target); err != nil {
				return fmt.Errorf("maild: decode a %s document: %w", bucket, err)
			}
			return nil
		})
	})
}
