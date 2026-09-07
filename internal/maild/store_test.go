package maild

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/castlemilk/dns/internal/mail"
	"github.com/castlemilk/dns/internal/mail/stalwart"
)

// testClock is a fixed instant, so every stamped timestamp in these tests is
// exactly comparable.
var testClock = time.Date(2026, 9, 3, 8, 52, 28, 0, time.UTC)

func newStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "maild.db"), WithClock(func() time.Time { return testClock }))
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close the store: %v", err)
		}
	})
	return store
}

// seedDomain creates one domain and returns it.
func seedDomain(t *testing.T, store *Store, name string) Domain {
	t.Helper()
	domain, err := store.CreateDomain(context.Background(), NewDomain(name, testClock))
	if err != nil {
		t.Fatalf("create the domain %s: %v", name, err)
	}
	return domain
}

// -----------------------------------------------------------------------------
// The contract this package exists to satisfy
// -----------------------------------------------------------------------------

// TestWireVocabularyMatchesTheClient is the drift guard. model.go declares the
// wire constants itself rather than importing internal/mail/stalwart, so the
// server stays independent of the control plane — but a typo in one of these
// strings would not fail a build, it would silently degrade a customer's mail
// binding. Comparing them here makes the drift a test failure instead.
func TestWireVocabularyMatchesTheClient(t *testing.T) {
	tests := []struct {
		name string
		got  string
		want string
	}{
		{"ed25519 algorithm", DkimAlgorithmEd25519, stalwart.DkimAlgorithmEd25519},
		{"rsa algorithm", DkimAlgorithmRSA, stalwart.DkimAlgorithmRSA},
		{"stage pending", DkimStagePending, stalwart.DkimStagePending},
		{"stage active", DkimStageActive, stalwart.DkimStageActive},
		{"stage retiring", DkimStageRetiring, stalwart.DkimStageRetiring},
		{"stage retired", DkimStageRetired, stalwart.DkimStageRetired},
		{"queue scheduled", QueueStatusScheduled, stalwart.QueueStatusScheduled},
		{"queue completed", QueueStatusCompleted, stalwart.QueueStatusCompleted},
		{"queue temporary failure", QueueStatusTemporaryFailure, stalwart.QueueStatusTemporaryFailure},
		{"queue permanent failure", QueueStatusPermanentFailure, stalwart.QueueStatusPermanentFailure},
		{"forbidden", ErrTypeForbidden, stalwart.ErrorTypeForbidden},
		{"unsupported filter", ErrTypeUnsupportedFilter, stalwart.ErrorTypeUnsupportedFilter},
		{"invalid patch", ErrTypeInvalidPatch, stalwart.ErrorTypeInvalidPatch},
		{"invalid arguments", ErrTypeInvalidArguments, stalwart.ErrorTypeInvalidArguments},
		{"invalid properties", ErrTypeInvalidProperties, stalwart.ErrorTypeInvalidProperties},
		{"object is linked", ErrTypeObjectIsLinked, stalwart.ErrorTypeObjectIsLinked},
		{"not found", ErrTypeNotFound, stalwart.ErrorTypeNotFound},
		{"account not found", ErrTypeAccountNotFound, stalwart.ErrorTypeAccountNotFound},
		{"primary key violation", ErrTypePrimaryKeyViolation, stalwart.ErrorTypePrimaryKeyViolation},
		{"already exists", ErrTypeAlreadyExists, stalwart.ErrorTypeAlreadyExists},
		{"unknown method", ErrTypeUnknownMethod, stalwart.ErrorTypeUnknownMethod},
		{"state mismatch", ErrTypeStateMismatch, stalwart.ErrorTypeStateMismatch},
		{"over quota", ErrTypeOverQuota, stalwart.ErrorTypeOverQuota},
		{"too large", ErrTypeTooLarge, stalwart.ErrorTypeTooLarge},
		{"rate limit", ErrTypeRateLimit, stalwart.ErrorTypeRateLimit},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.got != test.want {
				t.Errorf("maild has %q, the client expects %q", test.got, test.want)
			}
		})
	}

	if !slices.Equal(RequestedDkimAlgorithms, stalwart.RequestedDkimAlgorithms) {
		t.Errorf("RequestedDkimAlgorithms = %v, the client waits for %v",
			RequestedDkimAlgorithms, stalwart.RequestedDkimAlgorithms)
	}
	if MaxObjectsPerCall != stalwart.MaxObjectsPerCall {
		t.Errorf("MaxObjectsPerCall = %d, the client pages at %d", MaxObjectsPerCall, stalwart.MaxObjectsPerCall)
	}
	if MaxCallsPerRequest != stalwart.MaxCallsPerRequest {
		t.Errorf("MaxCallsPerRequest = %d, the client sends up to %d", MaxCallsPerRequest, stalwart.MaxCallsPerRequest)
	}
	if MaxResponseBytes != stalwart.MaxResponseBytes {
		t.Errorf("MaxResponseBytes = %d, the client reads up to %d", MaxResponseBytes, stalwart.MaxResponseBytes)
	}
}

// TestGrantedPermissionsCoverTheFacade proves GET /api/account will not report a
// missing permission. Probe lists every required name the credential does not
// hold and shows the difference on the Settings page, so a name missing here
// becomes a warning an operator cannot act on.
func TestGrantedPermissionsCoverTheFacade(t *testing.T) {
	granted := make(map[string]struct{}, len(GrantedPermissions))
	for _, name := range GrantedPermissions {
		if _, duplicate := granted[name]; duplicate {
			t.Errorf("GrantedPermissions repeats %q", name)
		}
		granted[name] = struct{}{}
	}
	for _, name := range mail.RequiredPermissions {
		if _, ok := granted[name]; !ok {
			t.Errorf("the facade requires %q and GET /api/account does not report it", name)
		}
	}
}

// TestWireObjectsCarryNoCredential is the structural version of "secrets never
// leave": rendering any object the server returns must not produce the
// credential, the salt, the digest or a DKIM private key.
func TestWireObjectsCarryNoCredential(t *testing.T) {
	hash, err := hashPasswordWith("correct horse battery staple", 16)
	if err != nil {
		t.Fatalf("hash a password: %v", err)
	}
	secret := Secret("private key bytes")

	account := Account{
		ID:         "a1",
		DomainID:   "d1",
		LocalPart:  "ada",
		Password:   hash,
		QuotaBytes: 1 << 30,
		Aliases:    []Alias{{LocalPart: "hello", DomainID: "d1", Enabled: true}},
		CreatedAt:  testClock,
	}
	key := DkimKey{
		ID:         "k1",
		DomainID:   "d1",
		Selector:   "v1-rsa-20260903",
		Algorithm:  DkimAlgorithmRSA,
		Stage:      DkimStageActive,
		PublicKey:  "MIIBIjANBgkq",
		PrivateKey: secret,
		CreatedAt:  testClock,
	}

	forbidden := []string{
		hex.EncodeToString(hash.Digest),
		hex.EncodeToString(hash.Salt),
		base64.StdEncoding.EncodeToString(hash.Digest),
		base64.StdEncoding.EncodeToString(hash.Salt),
		base64.StdEncoding.EncodeToString(secret),
		string(secret),
		"password",
		"secret",
		"private",
		"digest",
	}

	tests := []struct {
		name  string
		value any
	}{
		{"account", account.Wire("acme.dev")},
		{"dkim signature", key.Wire()},
		{"mailing list", List{ID: "l1", DomainID: "d1", LocalPart: "team", Recipients: []string{"x@y.test"}}.Wire("acme.dev")},
		{"domain", Domain{ID: "d1", Name: "acme.dev"}.Wire("")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw, err := json.Marshal(test.value)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			rendered := strings.ToLower(string(raw))
			for _, needle := range forbidden {
				if needle == "" {
					continue
				}
				if strings.Contains(rendered, strings.ToLower(needle)) {
					t.Errorf("the wire object leaks %q:\n%s", needle, raw)
				}
			}
		})
	}
}

// TestSecretsRedactThemselves covers the accidental-log path: a struct formatted
// with %v, %s or %#v must not print its bytes.
func TestSecretsRedactThemselves(t *testing.T) {
	hash, err := hashPasswordWith("hunter2", 16)
	if err != nil {
		t.Fatalf("hash a password: %v", err)
	}
	account := Account{ID: "a1", LocalPart: "ada", Password: hash}
	key := DkimKey{ID: "k1", PrivateKey: Secret("top secret der")}

	tests := []struct {
		name     string
		rendered string
	}{
		{"password %v", format("%v", hash)},
		{"password %s", format("%s", hash)},
		{"password %#v", format("%#v", hash)},
		{"account %v", format("%v", account)},
		{"secret %v", format("%v", key.PrivateKey)},
		{"secret %s", format("%s", key.PrivateKey)},
		{"secret %#v", format("%#v", key.PrivateKey)},
		{"dkim key %v", format("%v", key)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if strings.Contains(test.rendered, "top secret der") {
				t.Errorf("printed a private key: %s", test.rendered)
			}
			if strings.Contains(test.rendered, hex.EncodeToString(hash.Digest)) {
				t.Errorf("printed a password digest: %s", test.rendered)
			}
			if !strings.Contains(test.rendered, "[redacted]") {
				t.Errorf("expected a redaction, got: %s", test.rendered)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Passwords
// -----------------------------------------------------------------------------

// TestPBKDF2SHA256 checks the in-file KDF against the published
// PBKDF2-HMAC-SHA256 vectors. They were independently reproduced with Python's
// hashlib.pbkdf2_hmac before being committed, because this package cannot
// import golang.org/x/crypto to cross-check at run time. The fourth case spans
// two output blocks and the fifth carries embedded NUL bytes, which are the two
// ways a hand-written PBKDF2 usually goes wrong.
func TestPBKDF2SHA256(t *testing.T) {
	tests := []struct {
		name       string
		password   string
		salt       string
		iterations int
		keyLen     int
		want       string
	}{
		{"one iteration", "password", "salt", 1, 32,
			"120fb6cffcf8b32c43e7225256c4f837a86548c92ccc35480805987cb70be17b"},
		{"two iterations", "password", "salt", 2, 32,
			"ae4d0c95af6b46d32d0adff928f06dd02a303f8ef3c251dfd6e2d85a95474c43"},
		{"4096 iterations", "password", "salt", 4096, 32,
			"c5e478d59288c841aa530db6845c4c8d962893a001ce4e11a4963873aa98134a"},
		{"two output blocks", "passwordPASSWORDpassword", "saltSALTsaltSALTsaltSALTsaltSALTsalt", 4096, 40,
			"348c89dbcbd32b2f32d814b8116e84cf2b17347ebc1800181c4e2a1fb8dd53e1c635518c7dac47e9"},
		{"embedded nul", "pass\x00word", "sa\x00lt", 4096, 16,
			"89b69d0516f829893c696226650a8687"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := hex.EncodeToString(pbkdf2SHA256([]byte(test.password), []byte(test.salt), test.iterations, test.keyLen))
			if got != test.want {
				t.Errorf("pbkdf2SHA256 = %s, want %s", got, test.want)
			}
		})
	}
}

func TestPasswordHashVerify(t *testing.T) {
	hash, err := hashPasswordWith("caper-mellow-ridge-plumb-tonic-47", 4096)
	if err != nil {
		t.Fatalf("hash a password: %v", err)
	}

	tests := []struct {
		name   string
		hash   PasswordHash
		secret string
		want   bool
	}{
		{"the right passphrase", hash, "caper-mellow-ridge-plumb-tonic-47", true},
		{"a wrong passphrase", hash, "caper-mellow-ridge-plumb-tonic-48", false},
		{"an empty passphrase", hash, "", false},
		{"an unset credential", PasswordHash{}, "", false},
		{"an unset credential and a guess", PasswordHash{}, "anything", false},
		{"an unknown algorithm", PasswordHash{
			Algorithm: "rot13", Iterations: 1, Salt: hash.Salt, Digest: hash.Digest,
		}, "caper-mellow-ridge-plumb-tonic-47", false},
		{"no iterations", PasswordHash{
			Algorithm: PasswordAlgorithm, Iterations: 0, Salt: hash.Salt, Digest: hash.Digest,
		}, "caper-mellow-ridge-plumb-tonic-47", false},
		{"no salt", PasswordHash{
			Algorithm: PasswordAlgorithm, Iterations: 4096, Digest: hash.Digest,
		}, "caper-mellow-ridge-plumb-tonic-47", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.hash.Verify(test.secret); got != test.want {
				t.Errorf("Verify = %v, want %v", got, test.want)
			}
		})
	}
}

func TestHashPasswordIsSaltedAndRefusesEmpty(t *testing.T) {
	first, err := hashPasswordWith("same secret", 16)
	if err != nil {
		t.Fatalf("hash a password: %v", err)
	}
	second, err := hashPasswordWith("same secret", 16)
	if err != nil {
		t.Fatalf("hash a password: %v", err)
	}
	if hex.EncodeToString(first.Salt) == hex.EncodeToString(second.Salt) {
		t.Error("two hashes of the same secret share a salt")
	}
	if hex.EncodeToString(first.Digest) == hex.EncodeToString(second.Digest) {
		t.Error("two hashes of the same secret share a digest")
	}
	if !first.Verify("same secret") || !second.Verify("same secret") {
		t.Error("a salted hash did not verify its own secret")
	}
	if _, err := hashPasswordWith("", 16); !errors.Is(err, ErrInvalid) {
		t.Errorf("hashing an empty secret returned %v, want ErrInvalid", err)
	}
	if _, err := hashPasswordWith("secret", 0); !errors.Is(err, ErrInvalid) {
		t.Errorf("hashing with no work factor returned %v, want ErrInvalid", err)
	}
	if first.IsZero() {
		t.Error("a derived hash reported itself as unset")
	}
	if !(PasswordHash{}).IsZero() {
		t.Error("an empty hash did not report itself as unset")
	}
}

// -----------------------------------------------------------------------------
// Identifiers
// -----------------------------------------------------------------------------

// TestDeriveIDs pins the derivation. The ids are the keys of every stored
// object, so a change here orphans an existing store — the goldens make that a
// deliberate act rather than an accident.
func TestDeriveIDs(t *testing.T) {
	domainID := DomainID("acme.dev")

	tests := []struct {
		name string
		got  string
		want string
	}{
		{"domain", domainID, "defandidhxhrieen6"},
		{"account", AccountID(domainID, "ada"), "a5w4sosr5safilary"},
		{"list", ListID(domainID, "team"), "lx6lbtphhx5i6wjrr"},
		{"dkim", DkimID(domainID, "v1-rsa-20260903"), "kycokkamw3dwjray5"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.got != test.want {
				t.Errorf("id = %q, want %q", test.got, test.want)
			}
			if len(test.got) != 17 {
				t.Errorf("id %q is %d characters, want 17", test.got, len(test.got))
			}
		})
	}
}

func TestDeriveIDsNormalize(t *testing.T) {
	domainID := DomainID("acme.dev")

	tests := []struct {
		name  string
		left  string
		right string
		equal bool
	}{
		{"domain case", DomainID("ACME.dev"), domainID, true},
		{"domain root dot", DomainID("acme.dev."), domainID, true},
		{"domain spaces", DomainID("  acme.dev  "), domainID, true},
		{"different domains", DomainID("acme.dev"), DomainID("acme.test"), false},
		{"account case", AccountID(domainID, "ADA"), AccountID(domainID, "ada"), true},
		{"different local parts", AccountID(domainID, "ada"), AccountID(domainID, "grace"), false},
		{"different domains, same local part", AccountID(domainID, "ada"), AccountID(DomainID("acme.test"), "ada"), false},
		{"an account is not a list", AccountID(domainID, "team"), ListID(domainID, "team"), false},
		{"a list is not a dkim key", ListID(domainID, "team"), DkimID(domainID, "team"), false},
		{"dkim selector case", DkimID(domainID, "V1-RSA-20260903"), DkimID(domainID, "v1-rsa-20260903"), true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if (test.left == test.right) != test.equal {
				t.Errorf("%q vs %q: equal = %v, want %v", test.left, test.right, test.left == test.right, test.equal)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Domains
// -----------------------------------------------------------------------------

func TestDomainCRUD(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	domain := NewDomain("Acme.Dev.", testClock)
	domain.Description = "simple zone z-abc123"
	created, err := store.CreateDomain(ctx, domain)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.Name != "acme.dev" {
		t.Errorf("name = %q, want the normalised form", created.Name)
	}
	if created.ID != DomainID("acme.dev") {
		t.Errorf("id = %q, want the derived id", created.ID)
	}
	if !created.CreatedAt.Equal(testClock) || !created.UpdatedAt.Equal(testClock) {
		t.Errorf("timestamps = %v/%v, want the store clock", created.CreatedAt, created.UpdatedAt)
	}

	// The description must round-trip byte for byte: the facade adopts an
	// existing domain only when it matches exactly.
	read, err := store.FindDomain(ctx, "ACME.DEV")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if read.Description != "simple zone z-abc123" {
		t.Errorf("description = %q, want it unchanged", read.Description)
	}

	if _, err := store.CreateDomain(ctx, NewDomain("acme.dev", testClock)); !errors.Is(err, ErrExists) {
		t.Errorf("creating a duplicate returned %v, want ErrExists", err)
	}

	if _, err := store.GetDomain(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("reading a missing domain returned %v, want ErrNotFound", err)
	}

	seedDomain(t, store, "beta.dev")
	domains, err := store.ListDomains(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(domains) != 2 || domains[0].Name != "acme.dev" || domains[1].Name != "beta.dev" {
		t.Errorf("list = %v, want acme.dev then beta.dev", domains)
	}

	if err := store.DeleteDomain(ctx, created.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := store.DeleteDomain(ctx, created.ID); err != nil {
		t.Errorf("deleting an absent domain returned %v, want success", err)
	}
}

func TestPutDomainRefusesAMismatchedID(t *testing.T) {
	store := newStore(t)
	domain := NewDomain("acme.dev", testClock)
	domain.ID = "handrolled"
	if _, err := store.CreateDomain(context.Background(), domain); !errors.Is(err, ErrInvalid) {
		t.Errorf("a hand-rolled id returned %v, want ErrInvalid", err)
	}
	if _, err := store.CreateDomain(context.Background(), Domain{}); !errors.Is(err, ErrInvalid) {
		t.Errorf("a nameless domain returned %v, want ErrInvalid", err)
	}
}

// TestDeleteDomainRefusesLinkedChildren is the objectIsLinked refusal the
// facade's unbind path expects, and the reason deterministic ids are safe: a
// recreated domain cannot inherit an orphan.
func TestDeleteDomainRefusesLinkedChildren(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name   string
		seed   func(t *testing.T, store *Store, domain Domain)
		object string
	}{
		{
			name: "a mailbox",
			seed: func(t *testing.T, store *Store, domain Domain) {
				if _, err := store.CreateAccount(ctx, NewAccount(domain.ID, "ada", testClock)); err != nil {
					t.Fatalf("create the mailbox: %v", err)
				}
			},
			object: ObjectAccount,
		},
		{
			name: "a mailing list",
			seed: func(t *testing.T, store *Store, domain Domain) {
				if _, err := store.CreateList(ctx, NewList(domain.ID, "team", testClock)); err != nil {
					t.Fatalf("create the list: %v", err)
				}
			},
			object: ObjectMailingList,
		},
		{
			name: "a dkim key",
			seed: func(t *testing.T, store *Store, domain Domain) {
				key := NewDkimKey(domain.ID, "v1-rsa-20260903", DkimAlgorithmRSA, testClock)
				if _, err := store.PutDkimKey(ctx, key); err != nil {
					t.Fatalf("create the key: %v", err)
				}
			},
			object: ObjectDkimSignature,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := newStore(t)
			domain := seedDomain(t, store, "acme.dev")
			test.seed(t, store, domain)

			err := store.DeleteDomain(ctx, domain.ID)
			if !errors.Is(err, ErrLinked) {
				t.Fatalf("delete returned %v, want ErrLinked", err)
			}
			var linked *LinkedError
			if !errors.As(err, &linked) {
				t.Fatalf("delete returned %T, want a *LinkedError", err)
			}
			if len(linked.Linked) != 1 || linked.Linked[0].Object != test.object {
				t.Errorf("linked = %v, want one %s", linked.Linked, test.object)
			}
			if strings.Contains(linked.Error(), "secret") {
				t.Errorf("the refusal mentions a secret: %s", linked.Error())
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Accounts, lists and the shared address space
// -----------------------------------------------------------------------------

func TestAccountCRUD(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	domain := seedDomain(t, store, "acme.dev")

	account := NewAccount(domain.ID, "Ada", testClock)
	account.Description = "Ada"
	account.QuotaBytes = 5 << 30
	hash, err := hashPasswordWith("a-generated-passphrase-42", 16)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	account.Password = hash

	created, err := store.CreateAccount(ctx, account)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.LocalPart != "ada" {
		t.Errorf("local part = %q, want the normalised form", created.LocalPart)
	}
	if created.Address(domain.Name) != "ada@acme.dev" {
		t.Errorf("address = %q", created.Address(domain.Name))
	}

	read, err := store.FindAccount(ctx, domain.ID, "ADA")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if !read.Password.Verify("a-generated-passphrase-42") {
		t.Error("the stored credential did not verify after a round trip")
	}

	if _, err := store.CreateAccount(ctx, NewAccount(domain.ID, "ada", testClock)); !errors.Is(err, ErrExists) {
		t.Errorf("a duplicate mailbox returned %v, want ErrExists", err)
	}

	if _, err := store.CreateAccount(ctx, NewAccount("dmissing", "ada", testClock)); !errors.Is(err, ErrNotFound) {
		t.Errorf("a mailbox in an unknown domain returned %v, want ErrNotFound", err)
	}

	if err := store.SetPassword(ctx, created.ID, "a-different-passphrase-07"); err != nil {
		t.Fatalf("set the password: %v", err)
	}
	reread, err := store.GetAccount(ctx, created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if reread.Password.Verify("a-generated-passphrase-42") {
		t.Error("the old credential still verifies after a reset")
	}
	if !reread.Password.Verify("a-different-passphrase-07") {
		t.Error("the new credential does not verify after a reset")
	}
	if reread.Password.Iterations != DefaultPasswordIterations {
		t.Errorf("iterations = %d, want the default work factor", reread.Password.Iterations)
	}

	if err := store.DeleteAccount(ctx, created.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := store.DeleteAccount(ctx, created.ID); err != nil {
		t.Errorf("deleting an absent mailbox returned %v, want success", err)
	}
	if err := store.DeleteDomain(ctx, domain.ID); err != nil {
		t.Errorf("the domain is still linked after its only mailbox was deleted: %v", err)
	}
}

// TestAliasIndex covers the read-modify-write the facade does on every
// forwarder change: x:Account/set replaces the whole aliases map, so an alias
// dropped from the patch must stop resolving.
func TestAliasIndex(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	domain := seedDomain(t, store, "acme.dev")

	account := NewAccount(domain.ID, "ada", testClock)
	account.Aliases = []Alias{
		{LocalPart: "Sales", Enabled: true},
		{LocalPart: "hello", Enabled: true},
		{LocalPart: "hello", Enabled: false}, // a duplicate the store drops
	}
	created, err := store.CreateAccount(ctx, account)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(created.Aliases) != 2 || created.Aliases[0].LocalPart != "hello" || created.Aliases[1].LocalPart != "sales" {
		t.Fatalf("aliases = %v, want hello then sales", created.Aliases)
	}
	if created.Aliases[0].DomainID != domain.ID {
		t.Errorf("an alias did not default to the mailbox's domain: %v", created.Aliases[0])
	}

	for _, local := range []string{"hello", "SALES"} {
		resolved, err := store.AccountByAlias(ctx, domain.ID, local)
		if err != nil {
			t.Fatalf("resolve %s: %v", local, err)
		}
		if resolved.ID != created.ID {
			t.Errorf("%s resolved to %q, want %q", local, resolved.ID, created.ID)
		}
	}

	// Replace the set, dropping "sales".
	created.Aliases = []Alias{{LocalPart: "hello", DomainID: domain.ID, Enabled: true}}
	if _, err := store.PutAccount(ctx, created); err != nil {
		t.Fatalf("replace the aliases: %v", err)
	}
	if _, err := store.AccountByAlias(ctx, domain.ID, "sales"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a dropped alias still resolves: %v", err)
	}
	if _, err := store.AccountByAlias(ctx, domain.ID, "hello"); err != nil {
		t.Errorf("a kept alias stopped resolving: %v", err)
	}

	// Deleting the mailbox retires its aliases with it.
	if err := store.DeleteAccount(ctx, created.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := store.AccountByAlias(ctx, domain.ID, "hello"); !errors.Is(err, ErrNotFound) {
		t.Errorf("an alias of a deleted mailbox still resolves: %v", err)
	}
}

func TestAliasRefusesAnotherDomain(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	domain := seedDomain(t, store, "acme.dev")
	other := seedDomain(t, store, "beta.dev")

	account := NewAccount(domain.ID, "ada", testClock)
	account.Aliases = []Alias{{LocalPart: "hello", DomainID: other.ID, Enabled: true}}
	if _, err := store.CreateAccount(ctx, account); !errors.Is(err, ErrInvalid) {
		t.Errorf("a cross-domain alias returned %v, want ErrInvalid", err)
	}

	account.Aliases = []Alias{{LocalPart: "  ", DomainID: domain.ID}}
	if _, err := store.CreateAccount(ctx, account); !errors.Is(err, ErrInvalid) {
		t.Errorf("an empty alias returned %v, want ErrInvalid", err)
	}
}

// TestAddressSpaceIsShared: a mailbox, a mailing list and an alias all occupy
// the same address inside a domain. The facade's own addressTaken check makes
// exactly this rule, and the two must not disagree — a collision it lets through
// would be a mailbox that shadows a forwarder.
func TestAddressSpaceIsShared(t *testing.T) {
	ctx := context.Background()

	seedAccount := func(t *testing.T, store *Store, domain Domain, local string) {
		t.Helper()
		if _, err := store.CreateAccount(ctx, NewAccount(domain.ID, local, testClock)); err != nil {
			t.Fatalf("seed the mailbox %s: %v", local, err)
		}
	}
	seedList := func(t *testing.T, store *Store, domain Domain, local string) {
		t.Helper()
		if _, err := store.CreateList(ctx, NewList(domain.ID, local, testClock)); err != nil {
			t.Fatalf("seed the list %s: %v", local, err)
		}
	}
	seedAlias := func(t *testing.T, store *Store, domain Domain, local string) {
		t.Helper()
		account := NewAccount(domain.ID, "owner", testClock)
		account.Aliases = []Alias{{LocalPart: local, DomainID: domain.ID, Enabled: true}}
		if _, err := store.CreateAccount(ctx, account); err != nil {
			t.Fatalf("seed the alias %s: %v", local, err)
		}
	}

	tests := []struct {
		name string
		seed func(t *testing.T, store *Store, domain Domain)
		add  func(store *Store, domain Domain) error
	}{
		{
			name: "a list cannot take a mailbox address",
			seed: func(t *testing.T, s *Store, d Domain) { seedAccount(t, s, d, "team") },
			add: func(s *Store, d Domain) error {
				_, err := s.CreateList(ctx, NewList(d.ID, "team", testClock))
				return err
			},
		},
		{
			name: "a mailbox cannot take a list address",
			seed: func(t *testing.T, s *Store, d Domain) { seedList(t, s, d, "team") },
			add: func(s *Store, d Domain) error {
				_, err := s.CreateAccount(ctx, NewAccount(d.ID, "team", testClock))
				return err
			},
		},
		{
			name: "a mailbox cannot take an alias address",
			seed: func(t *testing.T, s *Store, d Domain) { seedAlias(t, s, d, "team") },
			add: func(s *Store, d Domain) error {
				_, err := s.CreateAccount(ctx, NewAccount(d.ID, "team", testClock))
				return err
			},
		},
		{
			name: "a list cannot take an alias address",
			seed: func(t *testing.T, s *Store, d Domain) { seedAlias(t, s, d, "team") },
			add: func(s *Store, d Domain) error {
				_, err := s.CreateList(ctx, NewList(d.ID, "team", testClock))
				return err
			},
		},
		{
			name: "an alias cannot take a mailbox address",
			seed: func(t *testing.T, s *Store, d Domain) { seedAccount(t, s, d, "team") },
			add: func(s *Store, d Domain) error {
				account := NewAccount(d.ID, "other", testClock)
				account.Aliases = []Alias{{LocalPart: "team", DomainID: d.ID, Enabled: true}}
				_, err := s.CreateAccount(ctx, account)
				return err
			},
		},
		{
			name: "an alias cannot take a list address",
			seed: func(t *testing.T, s *Store, d Domain) { seedList(t, s, d, "team") },
			add: func(s *Store, d Domain) error {
				account := NewAccount(d.ID, "other", testClock)
				account.Aliases = []Alias{{LocalPart: "team", DomainID: d.ID, Enabled: true}}
				_, err := s.CreateAccount(ctx, account)
				return err
			},
		},
		{
			name: "an alias cannot take another mailbox's alias",
			seed: func(t *testing.T, s *Store, d Domain) { seedAlias(t, s, d, "team") },
			add: func(s *Store, d Domain) error {
				account := NewAccount(d.ID, "other", testClock)
				account.Aliases = []Alias{{LocalPart: "team", DomainID: d.ID, Enabled: true}}
				_, err := s.CreateAccount(ctx, account)
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := newStore(t)
			domain := seedDomain(t, store, "acme.dev")
			test.seed(t, store, domain)

			taken, err := store.AddressTaken(ctx, domain.ID, "team")
			if err != nil {
				t.Fatalf("AddressTaken: %v", err)
			}
			if !taken {
				t.Error("AddressTaken said team@acme.dev was free")
			}
			if err := test.add(store, domain); !errors.Is(err, ErrExists) {
				t.Errorf("the collision returned %v, want ErrExists", err)
			}

			// A different address in the same domain is still free.
			free, err := store.AddressTaken(ctx, domain.ID, "someone-else")
			if err != nil {
				t.Fatalf("AddressTaken: %v", err)
			}
			if free {
				t.Error("AddressTaken said an unused address was taken")
			}
		})
	}
}

// TestReplacingAnObjectKeepsItsOwnAddress: a read-modify-write of a mailbox or
// a list must not collide with itself.
func TestReplacingAnObjectKeepsItsOwnAddress(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	domain := seedDomain(t, store, "acme.dev")

	account, err := store.CreateAccount(ctx, NewAccount(domain.ID, "ada", testClock))
	if err != nil {
		t.Fatalf("create the mailbox: %v", err)
	}
	account.Description = "Ada Lovelace"
	if _, err := store.PutAccount(ctx, account); err != nil {
		t.Errorf("replacing a mailbox collided with itself: %v", err)
	}

	list, err := store.CreateList(ctx, NewList(domain.ID, "team", testClock))
	if err != nil {
		t.Fatalf("create the list: %v", err)
	}
	list.Recipients = []string{"someone@example.com"}
	if _, err := store.PutList(ctx, list); err != nil {
		t.Errorf("replacing a list collided with itself: %v", err)
	}
}

func TestListCRUD(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	domain := seedDomain(t, store, "acme.dev")
	other := seedDomain(t, store, "beta.dev")

	list := NewList(domain.ID, "Team", testClock)
	list.Description = "forwarder"
	list.Recipients = []string{"Someone@Example.com", "  ", "mara@acme.dev", "someone@example.com"}
	created, err := store.CreateList(ctx, list)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !slices.Equal(created.Recipients, []string{"mara@acme.dev", "someone@example.com"}) {
		t.Errorf("recipients = %v, want them lowercased, de-duplicated and ordered", created.Recipients)
	}
	if created.Address(domain.Name) != "team@acme.dev" {
		t.Errorf("address = %q", created.Address(domain.Name))
	}

	if _, err := store.CreateList(ctx, NewList(other.ID, "everyone", testClock)); err != nil {
		t.Fatalf("create in the other domain: %v", err)
	}

	// The facade queries mailing lists unfiltered and matches the domain
	// itself, so an empty domain id must return every list.
	all, err := store.ListLists(ctx, "")
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("unfiltered list = %v, want both lists", all)
	}
	mine, err := store.ListLists(ctx, domain.ID)
	if err != nil {
		t.Fatalf("list one domain: %v", err)
	}
	if len(mine) != 1 || mine[0].LocalPart != "team" {
		t.Errorf("filtered list = %v, want just team", mine)
	}

	if _, err := store.FindList(ctx, domain.ID, "TEAM"); err != nil {
		t.Errorf("find: %v", err)
	}
	if err := store.DeleteList(ctx, created.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := store.DeleteList(ctx, created.ID); err != nil {
		t.Errorf("deleting an absent list returned %v, want success", err)
	}
	if _, err := store.CreateList(ctx, NewList("dmissing", "team", testClock)); !errors.Is(err, ErrNotFound) {
		t.Errorf("a list in an unknown domain returned %v, want ErrNotFound", err)
	}
}

func TestDkimKeyCRUD(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	domain := seedDomain(t, store, "acme.dev")

	rsa := NewDkimKey(domain.ID, "V1-RSA-20260903", DkimAlgorithmRSA, testClock)
	rsa.PublicKey = "MIIBIjANBgkq"
	rsa.PrivateKey = Secret("pkcs8 der")
	rsa.Stage = DkimStageActive
	stored, err := store.PutDkimKey(ctx, rsa)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if stored.Selector != "v1-rsa-20260903" {
		t.Errorf("selector = %q, want the normalised form", stored.Selector)
	}
	if !stored.Active() {
		t.Error("an active key did not report itself active")
	}

	ed := NewDkimKey(domain.ID, "v1-ed25519-20260903", DkimAlgorithmEd25519, testClock)
	ed.Stage = DkimStageActive
	if _, err := store.PutDkimKey(ctx, ed); err != nil {
		t.Fatalf("put the ed25519 key: %v", err)
	}

	keys, err := store.ListDkimKeys(ctx, domain.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(keys) != 2 || keys[0].Selector != "v1-ed25519-20260903" || keys[1].Selector != "v1-rsa-20260903" {
		t.Fatalf("keys = %v, want both, ordered by selector", keys)
	}
	if string(keys[1].PrivateKey) != "pkcs8 der" {
		t.Error("the private key did not survive a round trip")
	}

	bad := NewDkimKey(domain.ID, "v1-md5", "Dkim1Md5", testClock)
	if _, err := store.PutDkimKey(ctx, bad); !errors.Is(err, ErrInvalid) {
		t.Errorf("an unknown algorithm returned %v, want ErrInvalid", err)
	}

	if err := store.DeleteDkimKey(ctx, stored.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := store.DeleteDkimKey(ctx, stored.ID); err != nil {
		t.Errorf("deleting an absent key returned %v, want success", err)
	}
}

// -----------------------------------------------------------------------------
// Wire rendering
// -----------------------------------------------------------------------------

// TestProjectKeepsRequestedProperties covers the /get "properties" argument.
func TestProjectKeepsRequestedProperties(t *testing.T) {
	domain := Domain{ID: "d1", Name: "acme.dev", Description: "simple zone z1", Enabled: true, CreatedAt: testClock}

	tests := []struct {
		name       string
		properties []string
		want       []string
	}{
		{"everything", nil, []string{"id", "name", "description", "isEnabled", "createdAt"}},
		{"the list subset", []string{"name", "isEnabled", "description", "createdAt"},
			[]string{"id", "name", "description", "isEnabled", "createdAt"}},
		{"just the name", []string{"name"}, []string{"id", "name"}},
		{"id is always kept", []string{}, []string{"id", "name", "description", "isEnabled", "createdAt"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			object, err := Project(domain.Wire(""), test.properties)
			if err != nil {
				t.Fatalf("Project: %v", err)
			}
			if len(object) != len(test.want) {
				t.Fatalf("kept %v, want %v", keysOf(object), test.want)
			}
			for _, key := range test.want {
				if _, ok := object[key]; !ok {
					t.Errorf("%q was dropped; kept %v", key, keysOf(object))
				}
			}
		})
	}
}

// TestDomainWireOmitsAnUnrequestedZoneFile: dnsZoneFile is the expensive
// property and the list query deliberately leaves it out.
func TestDomainWireOmitsAnUnrequestedZoneFile(t *testing.T) {
	domain := Domain{ID: "d1", Name: "acme.dev", Enabled: true, CreatedAt: testClock}

	without, err := json.Marshal(domain.Wire(""))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(without), "dnsZoneFile") {
		t.Errorf("an unrequested zone file was rendered: %s", without)
	}
	with, err := json.Marshal(domain.Wire("acme.dev. IN MX 10 mail.local.test.\n"))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(with), "dnsZoneFile") {
		t.Errorf("a requested zone file was dropped: %s", with)
	}
}

// TestWireShapesDecodeInTheClient is the end of the contract: every object this
// server renders is fed to the frozen client's own payload structs, using the
// exact JSON tags calls.go declares. A renamed tag fails here rather than in
// production.
func TestWireShapesDecodeInTheClient(t *testing.T) {
	t.Run("domain", func(t *testing.T) {
		raw := marshal(t, Domain{
			ID: "d1", Name: "acme.dev", Description: "simple zone z1",
			Enabled: true, CreatedAt: testClock,
		}.Wire("acme.dev. IN MX 10 mail.local.test.\n"))
		var payload struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			Description string `json:"description"`
			Enabled     bool   `json:"isEnabled"`
			DNSZoneFile string `json:"dnsZoneFile"`
			CreatedAt   string `json:"createdAt"`
		}
		decode(t, raw, &payload)
		if payload.ID != "d1" || payload.Name != "acme.dev" || !payload.Enabled {
			t.Errorf("decoded %+v", payload)
		}
		if payload.Description != "simple zone z1" {
			t.Errorf("description = %q, want it round-tripped exactly", payload.Description)
		}
		if !strings.Contains(payload.DNSZoneFile, "IN MX") {
			t.Errorf("zone file = %q", payload.DNSZoneFile)
		}
		if _, err := time.Parse(time.RFC3339, payload.CreatedAt); err != nil {
			t.Errorf("createdAt %q is not RFC 3339: %v", payload.CreatedAt, err)
		}
	})

	t.Run("account", func(t *testing.T) {
		raw := marshal(t, Account{
			ID: "a1", DomainID: "d1", LocalPart: "mara", Description: "Mara",
			QuotaBytes: 5368709120, UsedBytes: 1894, CreatedAt: testClock,
			Aliases: []Alias{{LocalPart: "hello", DomainID: "d1", Enabled: true}},
		}.Wire("acme.dev"))
		var payload struct {
			ID           string `json:"id"`
			EmailAddress string `json:"emailAddress"`
			Description  string `json:"description"`
			DomainID     string `json:"domainId"`
			Quotas       struct {
				MaxDiskQuota uint64 `json:"maxDiskQuota"`
			} `json:"quotas"`
			UsedDiskQuota uint64 `json:"usedDiskQuota"`
			Aliases       map[string]struct {
				Name        string `json:"name"`
				DomainID    string `json:"domainId"`
				Description string `json:"description"`
				Enabled     bool   `json:"enabled"`
			} `json:"aliases"`
			CreatedAt string `json:"createdAt"`
		}
		decode(t, raw, &payload)
		if payload.EmailAddress != "mara@acme.dev" {
			t.Errorf("emailAddress = %q", payload.EmailAddress)
		}
		if payload.Quotas.MaxDiskQuota != 5368709120 || payload.UsedDiskQuota != 1894 {
			t.Errorf("quotas decoded as %d/%d", payload.Quotas.MaxDiskQuota, payload.UsedDiskQuota)
		}
		alias, ok := payload.Aliases["0"]
		if !ok {
			t.Fatalf("aliases = %v, want an entry keyed \"0\"", payload.Aliases)
		}
		if alias.Name != "hello" || alias.DomainID != "d1" || !alias.Enabled {
			t.Errorf("alias decoded as %+v", alias)
		}
	})

	t.Run("dkim signature", func(t *testing.T) {
		raw := marshal(t, DkimKey{
			ID: "k1", DomainID: "d1", Selector: "v1-rsa-20260903",
			Algorithm: DkimAlgorithmRSA, Stage: DkimStageActive,
			PublicKey: "MIIBIjANBgkq", PrivateKey: Secret("pkcs8"), CreatedAt: testClock,
		}.Wire())
		var payload struct {
			ID        string `json:"id"`
			Selector  string `json:"selector"`
			Algorithm string `json:"@type"`
			Stage     string `json:"stage"`
			PublicKey string `json:"publicKey"`
			CreatedAt string `json:"createdAt"`
		}
		decode(t, raw, &payload)
		if payload.Algorithm != stalwart.DkimAlgorithmRSA || payload.Stage != stalwart.DkimStageActive {
			t.Errorf("decoded %+v", payload)
		}
		if payload.PublicKey != "MIIBIjANBgkq" {
			t.Errorf("publicKey = %q", payload.PublicKey)
		}
	})

	t.Run("mailing list", func(t *testing.T) {
		raw := marshal(t, List{
			ID: "l1", DomainID: "d1", LocalPart: "team", Description: "forwarder",
			Recipients: []string{"mara@acme.dev", "someone@example.com"},
		}.Wire("acme.dev"))
		var payload struct {
			ID           string          `json:"id"`
			Name         string          `json:"name"`
			DomainID     string          `json:"domainId"`
			Description  string          `json:"description"`
			EmailAddress string          `json:"emailAddress"`
			Recipients   map[string]bool `json:"recipients"`
		}
		decode(t, raw, &payload)
		if payload.EmailAddress != "team@acme.dev" || payload.Description != stalwart.ListDescription {
			t.Errorf("decoded %+v", payload)
		}
		if !payload.Recipients["mara@acme.dev"] || !payload.Recipients["someone@example.com"] {
			t.Errorf("recipients = %v", payload.Recipients)
		}
	})

	t.Run("system settings", func(t *testing.T) {
		raw := marshal(t, SystemSettings{
			DefaultHostname: "mail.local.test",
			MailExchangers:  []MailExchanger{{Priority: 10}, {Hostname: "mx2.local.test", Priority: 20}},
		}.Wire())
		var payload struct {
			DefaultHostname string `json:"defaultHostname"`
			MailExchangers  map[string]struct {
				Hostname *string `json:"hostname"`
				Priority uint16  `json:"priority"`
			} `json:"mailExchangers"`
		}
		decode(t, raw, &payload)
		if payload.DefaultHostname != "mail.local.test" {
			t.Errorf("defaultHostname = %q", payload.DefaultHostname)
		}
		first, ok := payload.MailExchangers["0"]
		if !ok || first.Hostname != nil || first.Priority != 10 {
			t.Errorf("mailExchangers = %v, want a null hostname at priority 10", payload.MailExchangers)
		}
		second := payload.MailExchangers["1"]
		if second.Hostname == nil || *second.Hostname != "mx2.local.test" {
			t.Errorf("the second exchanger decoded as %+v", second)
		}
	})

	t.Run("queued message", func(t *testing.T) {
		raw := marshal(t, QueuedMessage{
			ID: "j1", ReturnPath: "sender@example.org", CreatedAt: testClock,
			Recipients: []QueuedRecipient{{
				Address: "someone@example.com", ORCPT: "team@acme.dev",
				Status: QueueStatusTemporaryFailure, QueueName: "remote",
				RetryCount: 2, RetryDue: testClock, LastCode: 451, LastMessage: "deferred",
			}},
		}.Wire())
		var payload struct {
			ID         string `json:"id"`
			ReturnPath string `json:"returnPath"`
			NextRetry  string `json:"nextRetry"`
			Recipients map[string]struct {
				ORCPT      *string `json:"orcpt"`
				QueueName  string  `json:"queueName"`
				RetryCount uint32  `json:"retryCount"`
				RetryDue   string  `json:"retryDue"`
				Status     struct {
					Type string `json:"@type"`
				} `json:"status"`
			} `json:"recipients"`
		}
		decode(t, raw, &payload)
		if payload.NextRetry != "" {
			t.Errorf("an unset nextRetry decoded as %q, want the zero value", payload.NextRetry)
		}
		recipient, ok := payload.Recipients["someone@example.com"]
		if !ok {
			t.Fatalf("recipients = %v, want a key of the recipient address", payload.Recipients)
		}
		if recipient.ORCPT == nil || *recipient.ORCPT != "rfc822;team@acme.dev" {
			t.Errorf("orcpt = %v, want the rfc822 form the client strips", recipient.ORCPT)
		}
		if recipient.Status.Type != stalwart.QueueStatusTemporaryFailure || recipient.RetryCount != 2 {
			t.Errorf("recipient decoded as %+v", recipient)
		}
	})

	t.Run("account info", func(t *testing.T) {
		raw := marshal(t, AccountInfo())
		var payload struct {
			Permissions []string `json:"permissions"`
			Edition     string   `json:"edition"`
		}
		decode(t, raw, &payload)
		if payload.Edition == "" {
			t.Error("edition was empty")
		}
		if len(payload.Permissions) != len(GrantedPermissions) {
			t.Errorf("permissions = %v", payload.Permissions)
		}
	})
}

// TestNullDescriptionDecodesEmpty: JMAP spells "no value" as null, and the
// client decodes a null description into the empty string. A domain the facade
// did not create must therefore never look like one it did.
func TestNullDescriptionDecodesEmpty(t *testing.T) {
	raw := marshal(t, Domain{ID: "d1", Name: "acme.dev"}.Wire(""))
	if !strings.Contains(string(raw), `"description":null`) {
		t.Errorf("an empty description rendered as %s, want null", raw)
	}
	var payload struct {
		Description string `json:"description"`
	}
	decode(t, raw, &payload)
	if payload.Description != "" {
		t.Errorf("description = %q, want the empty string", payload.Description)
	}
}

func TestSplitAddress(t *testing.T) {
	tests := []struct {
		name    string
		address string
		local   string
		domain  string
		ok      bool
	}{
		{"plain", "ada@acme.dev", "ada", "acme.dev", true},
		{"uppercase", " ADA@ACME.DEV ", "ada", "acme.dev", true},
		{"root dot", "ada@acme.dev.", "ada", "acme.dev", true},
		{"no at", "ada", "", "", false},
		{"no local part", "@acme.dev", "", "", false},
		{"no domain", "ada@", "", "", false},
		{"two ats", "ada@acme@dev", "", "", false},
		{"empty", "", "", "", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			local, domain, ok := SplitAddress(test.address)
			if ok != test.ok || local != test.local || domain != test.domain {
				t.Errorf("SplitAddress(%q) = %q, %q, %v; want %q, %q, %v",
					test.address, local, domain, ok, test.local, test.domain, test.ok)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Store lifecycle
// -----------------------------------------------------------------------------

func TestOpenRefusesABadPath(t *testing.T) {
	if _, err := Open("  "); err == nil {
		t.Error("opening an empty path succeeded")
	}
}

func TestReopenKeepsState(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "maild.db")

	first, err := Open(path, WithClock(func() time.Time { return testClock }))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	domain, err := first.CreateDomain(ctx, NewDomain("acme.dev", testClock))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	account := NewAccount(domain.ID, "ada", testClock)
	hash, err := hashPasswordWith("a-generated-passphrase-42", 16)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	account.Password = hash
	if _, err := first.CreateAccount(ctx, account); err != nil {
		t.Fatalf("create the mailbox: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	second, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() {
		if err := second.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	}()
	if second.Path() != path {
		t.Errorf("Path = %q, want %q", second.Path(), path)
	}
	reread, err := second.FindAccount(ctx, domain.ID, "ada")
	if err != nil {
		t.Fatalf("find after reopen: %v", err)
	}
	if !reread.Password.Verify("a-generated-passphrase-42") {
		t.Error("the credential did not survive a reopen")
	}
}

func TestCancelledContextIsRefused(t *testing.T) {
	store := newStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := store.ListDomains(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("a read with a cancelled context returned %v", err)
	}
	if _, err := store.CreateDomain(ctx, NewDomain("acme.dev", testClock)); !errors.Is(err, context.Canceled) {
		t.Errorf("a write with a cancelled context returned %v", err)
	}
}

// -----------------------------------------------------------------------------
// helpers
// -----------------------------------------------------------------------------

func marshal(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

func decode(t *testing.T, raw []byte, target any) {
	t.Helper()
	if err := json.Unmarshal(raw, target); err != nil {
		t.Fatalf("the client could not decode %s: %v", raw, err)
	}
}

// format defers the verb past staticcheck's "use String() instead of Sprintf"
// rule: the point of these cases is to exercise the fmt path a stray log line
// would take, which is exactly what that rule would have us skip.
func format(verb string, value any) string { return fmt.Sprintf(verb, value) }

func keysOf(object map[string]any) []string {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
