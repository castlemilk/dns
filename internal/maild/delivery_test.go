package maild

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/castlemilk/dns/internal/mail/stalwart"
)

// discardLogger keeps a test's output about the test rather than about the
// server's ordinary chatter.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// newTestDeliverer builds a delivery path over a real store in a temporary
// directory. Everything here is real except the clock: the point of these tests
// is what ends up on disk, and a fake filesystem would test the fake.
func newTestDeliverer(t *testing.T) (*Deliverer, *Store) {
	t.Helper()
	store := newStore(t)
	deliverer, err := NewDeliverer(store, DeliveryConfig{
		Root:     filepath.Join(t.TempDir(), "mail"),
		Hostname: "mx.test",
		Logger:   discardLogger(),
		Now:      func() time.Time { return testClock },
	})
	if err != nil {
		t.Fatalf("build the deliverer: %v", err)
	}
	return deliverer, store
}

// seedAccount creates a mailbox with an optional set of aliases.
func seedAccount(t *testing.T, store *Store, domain Domain, localPart string, aliases ...string) Account {
	t.Helper()
	account := NewAccount(domain.ID, localPart, testClock)
	for _, alias := range aliases {
		account.Aliases = append(account.Aliases, Alias{LocalPart: alias, DomainID: domain.ID, Enabled: true})
	}
	stored, err := store.CreateAccount(context.Background(), account)
	if err != nil {
		t.Fatalf("create the mailbox %s: %v", localPart, err)
	}
	return stored
}

// setTestPassword sets a credential at a low work factor. The default 600,000
// iterations is right in production and wrong in a test loop; PasswordHash
// carries its own iteration count, so a cheap hash still verifies normally.
func setTestPassword(t *testing.T, store *Store, account Account, secret string) Account {
	t.Helper()
	hash, err := hashPasswordWith(secret, 4096)
	if err != nil {
		t.Fatalf("hash the password: %v", err)
	}
	account.Password = hash
	stored, err := store.PutAccount(context.Background(), account)
	if err != nil {
		t.Fatalf("store the credential: %v", err)
	}
	return stored
}

// mailboxMessages reads every delivered message out of one mailbox, and proves
// on the way that nothing was left behind in tmp — a message still in tmp is a
// message a reader would either miss or find half written.
func mailboxMessages(t *testing.T, deliverer *Deliverer, accountID string) []string {
	t.Helper()
	directory := deliverer.MailboxDir(accountID)
	leftovers, err := os.ReadDir(filepath.Join(directory, maildirTmp))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read the tmp directory: %v", err)
	}
	if len(leftovers) != 0 {
		t.Errorf("tmp still holds %d files; a partially written message is visible", len(leftovers))
	}
	entries, err := os.ReadDir(filepath.Join(directory, maildirNew))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		t.Fatalf("read the new directory: %v", err)
	}
	messages := make([]string, 0, len(entries))
	for _, entry := range entries {
		raw, err := os.ReadFile(filepath.Join(directory, maildirNew, entry.Name()))
		if err != nil {
			t.Fatalf("read a delivered message: %v", err)
		}
		messages = append(messages, string(raw))
	}
	sort.Strings(messages)
	return messages
}

// codeOf recovers the SMTP reply a failure means.
func codeOf(t *testing.T, err error) (int, string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected a failure")
	}
	var delivery *DeliveryError
	if !errors.As(err, &delivery) {
		t.Fatalf("the failure does not carry a reply: %v", err)
	}
	return delivery.Code, delivery.Enhanced
}

// -----------------------------------------------------------------------------
// Recipient resolution
// -----------------------------------------------------------------------------

func TestResolve(t *testing.T) {
	deliverer, store := newTestDeliverer(t)
	ctx := context.Background()

	plain := seedDomain(t, store, "example.test")
	ada := seedAccount(t, store, plain, "ada", "hello")
	team, err := store.CreateList(ctx, List{
		DomainID:   plain.ID,
		LocalPart:  "team",
		Recipients: []string{"ada@example.test", "outside@elsewhere.test"},
	})
	if err != nil {
		t.Fatalf("create the mailing list: %v", err)
	}
	full := seedAccount(t, store, plain, "full")
	full.QuotaBytes = 128
	full.UsedBytes = 128
	if _, err := store.PutAccount(ctx, full); err != nil {
		t.Fatalf("store the full mailbox: %v", err)
	}

	flexible := NewDomain("plus.test", testClock)
	flexible.SubAddressing = true
	flexible.CatchAllLocalPart = "grace"
	if _, err := store.CreateDomain(ctx, flexible); err != nil {
		t.Fatalf("create the flexible domain: %v", err)
	}
	grace := seedAccount(t, store, flexible, "grace")

	disabled := NewDomain("off.test", testClock)
	disabled.Enabled = false
	if _, err := store.CreateDomain(ctx, disabled); err != nil {
		t.Fatalf("create the disabled domain: %v", err)
	}

	tests := []struct {
		name          string
		address       string
		authenticated bool
		wantKind      RecipientKind
		wantAccount   string
		wantList      string
		wantAddress   string
		wantCode      int
		wantEnhanced  string
	}{
		{
			name:        "an exact mailbox",
			address:     "ada@example.test",
			wantKind:    RecipientMailbox,
			wantAccount: ada.ID,
			wantAddress: "ada@example.test",
		},
		{
			name:        "the address is normalised",
			address:     "  ADA@Example.TEST ",
			wantKind:    RecipientMailbox,
			wantAccount: ada.ID,
			wantAddress: "ada@example.test",
		},
		{
			name:        "an alias resolves to its mailbox but keeps its own address",
			address:     "hello@example.test",
			wantKind:    RecipientMailbox,
			wantAccount: ada.ID,
			wantAddress: "hello@example.test",
		},
		{
			name:        "a mailing list",
			address:     "team@example.test",
			wantKind:    RecipientList,
			wantList:    team.ID,
			wantAddress: "team@example.test",
		},
		{
			name:         "an unknown local part in a hosted domain is permanent",
			address:      "nobody@example.test",
			wantCode:     550,
			wantEnhanced: "5.1.1",
		},
		{
			name:         "a sub-address is not accepted unless the domain enables it",
			address:      "ada+news@example.test",
			wantCode:     550,
			wantEnhanced: "5.1.1",
		},
		{
			name:        "a sub-address resolves to the base mailbox",
			address:     "grace+news@plus.test",
			wantKind:    RecipientMailbox,
			wantAccount: grace.ID,
			wantAddress: "grace+news@plus.test",
		},
		{
			name:        "an unmatched address falls to the catch-all",
			address:     "anyone@plus.test",
			wantKind:    RecipientMailbox,
			wantAccount: grace.ID,
			wantAddress: "anyone@plus.test",
		},
		{
			name:         "an unhosted domain is refused without authentication",
			address:      "someone@elsewhere.test",
			wantCode:     550,
			wantEnhanced: "5.7.1",
		},
		{
			name:          "an unhosted domain is relayed for an authenticated sender",
			address:       "Someone@Elsewhere.test",
			authenticated: true,
			wantKind:      RecipientRelay,
			// The remote local part keeps its case; only the domain is folded.
			wantAddress: "Someone@elsewhere.test",
		},
		{
			name:         "a malformed address is permanent",
			address:      "not-an-address",
			wantCode:     501,
			wantEnhanced: "5.1.3",
		},
		{
			name:         "a disabled domain is temporary",
			address:      "anyone@off.test",
			wantCode:     450,
			wantEnhanced: "4.2.1",
		},
		{
			name:         "a mailbox that is already full is temporary",
			address:      "full@example.test",
			wantCode:     452,
			wantEnhanced: "4.2.2",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recipient, err := deliverer.Resolve(ctx, test.address, test.authenticated)
			if test.wantCode != 0 {
				code, enhanced := codeOf(t, err)
				if code != test.wantCode || enhanced != test.wantEnhanced {
					t.Fatalf("got %d %s, want %d %s", code, enhanced, test.wantCode, test.wantEnhanced)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolve %s: %v", test.address, err)
			}
			if recipient.Kind != test.wantKind {
				t.Errorf("kind = %s, want %s", recipient.Kind, test.wantKind)
			}
			if recipient.AccountID != test.wantAccount {
				t.Errorf("account = %q, want %q", recipient.AccountID, test.wantAccount)
			}
			if recipient.ListID != test.wantList {
				t.Errorf("list = %q, want %q", recipient.ListID, test.wantList)
			}
			if recipient.Address != test.wantAddress {
				t.Errorf("address = %q, want %q", recipient.Address, test.wantAddress)
			}
		})
	}
}

// TestResolveTemporaryFailureAfterClose proves the 4xx side of the split: a
// store that cannot answer must not look like an address that does not exist.
func TestResolveTemporaryFailureAfterClose(t *testing.T) {
	deliverer, store := newTestDeliverer(t)
	seedDomain(t, store, "example.test")

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := deliverer.Resolve(cancelled, "ada@example.test", false)
	code, enhanced := codeOf(t, err)
	if code != 451 || enhanced != "4.3.0" {
		t.Fatalf("got %d %s, want 451 4.3.0", code, enhanced)
	}
	var delivery *DeliveryError
	if errors.As(err, &delivery) && !delivery.Temporary() {
		t.Error("a store failure must be temporary so the sender retries")
	}
}

// -----------------------------------------------------------------------------
// Delivery
// -----------------------------------------------------------------------------

func TestDeliverToMailbox(t *testing.T) {
	deliverer, store := newTestDeliverer(t)
	ctx := context.Background()
	domain := seedDomain(t, store, "example.test")
	ada := seedAccount(t, store, domain, "ada")

	recipient, err := deliverer.Resolve(ctx, "ada@example.test", false)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	envelope := &Envelope{
		ID:         "m1",
		ReturnPath: "sender@elsewhere.test",
		Recipients: []Recipient{recipient},
		Received:   "Received: from client by mx.test;\r\n",
		Data:       []byte("Subject: hello\r\n\r\nbody\r\n"),
		ReceivedAt: testClock,
	}
	if err := deliverer.Deliver(ctx, envelope); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	messages := mailboxMessages(t, deliverer, ada.ID)
	if len(messages) != 1 {
		t.Fatalf("delivered %d messages, want 1", len(messages))
	}
	for _, want := range []string{
		"Return-Path: <sender@elsewhere.test>\r\n",
		"Delivered-To: ada@example.test\r\n",
		"Received: from client by mx.test;\r\n",
		"Subject: hello\r\n\r\nbody\r\n",
	} {
		if !strings.Contains(messages[0], want) {
			t.Errorf("the stored message is missing %q:\n%s", want, messages[0])
		}
	}

	stored, err := store.GetAccount(ctx, ada.ID)
	if err != nil {
		t.Fatalf("re-read the mailbox: %v", err)
	}
	if stored.UsedBytes != uint64(len(messages[0])) {
		t.Errorf("usedBytes = %d, want %d", stored.UsedBytes, len(messages[0]))
	}

	queued, err := store.ListQueuedMessages(ctx, 0)
	if err != nil {
		t.Fatalf("list the queue: %v", err)
	}
	if len(queued) != 0 {
		t.Errorf("a local delivery queued %d messages, want 0", len(queued))
	}
}

func TestDeliverNullSenderIsRecorded(t *testing.T) {
	deliverer, store := newTestDeliverer(t)
	ctx := context.Background()
	domain := seedDomain(t, store, "example.test")
	ada := seedAccount(t, store, domain, "ada")

	recipient, err := deliverer.Resolve(ctx, "ada@example.test", false)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if err := deliverer.Deliver(ctx, &Envelope{
		ID:         "m1",
		Recipients: []Recipient{recipient},
		Data:       []byte("a bounce\r\n"),
	}); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	messages := mailboxMessages(t, deliverer, ada.ID)
	if len(messages) != 1 || !strings.HasPrefix(messages[0], "Return-Path: <>\r\n") {
		t.Fatalf("the null sender was not recorded as <>:\n%q", messages)
	}
}

func TestDeliverToListFansOut(t *testing.T) {
	deliverer, store := newTestDeliverer(t)
	ctx := context.Background()
	domain := seedDomain(t, store, "example.test")
	ada := seedAccount(t, store, domain, "ada")
	if _, err := store.CreateList(ctx, List{
		DomainID:   domain.ID,
		LocalPart:  "team",
		Recipients: []string{"ada@example.test", "outside@elsewhere.test"},
	}); err != nil {
		t.Fatalf("create the mailing list: %v", err)
	}

	recipient, err := deliverer.Resolve(ctx, "team@example.test", false)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	envelope := &Envelope{
		ID:         "m1",
		ReturnPath: "sender@elsewhere.test",
		Recipients: []Recipient{recipient},
		Received:   "Received: from client by mx.test;\r\n",
		Data:       []byte("Subject: hello\r\n\r\nbody\r\n"),
	}
	if err := deliverer.Deliver(ctx, envelope); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	// The local target is stored now, not queued.
	if messages := mailboxMessages(t, deliverer, ada.ID); len(messages) != 1 {
		t.Fatalf("the local list target received %d messages, want 1", len(messages))
	}

	queued, err := store.ListQueuedMessages(ctx, 0)
	if err != nil {
		t.Fatalf("list the queue: %v", err)
	}
	if len(queued) != 1 {
		t.Fatalf("queued %d messages, want 1", len(queued))
	}
	if len(queued[0].Recipients) != 1 {
		t.Fatalf("queued %d recipients, want 1", len(queued[0].Recipients))
	}
	external := queued[0].Recipients[0]
	if external.Address != "outside@elsewhere.test" {
		t.Errorf("queued address = %q", external.Address)
	}
	// Without the ORCPT the message is attributable to no hosted zone.
	if external.ORCPT != "team@example.test" {
		t.Errorf("orcpt = %q, want the customer's own address", external.ORCPT)
	}
	if external.Status != QueueStatusScheduled || external.QueueName != remoteQueueName {
		t.Errorf("status = %q, queue = %q", external.Status, external.QueueName)
	}

	body, err := os.ReadFile(deliverer.SpoolPath(queued[0].MessagePath))
	if err != nil {
		t.Fatalf("read the spooled body: %v", err)
	}
	if !strings.Contains(string(body), "Subject: hello") {
		t.Errorf("the spooled body is not the message:\n%s", body)
	}
	if queued[0].SizeBytes != int64(len(body)) {
		t.Errorf("sizeBytes = %d, want %d", queued[0].SizeBytes, len(body))
	}
}

func TestDeliverRelayQueuesForSubmission(t *testing.T) {
	deliverer, store := newTestDeliverer(t)
	ctx := context.Background()
	seedDomain(t, store, "example.test")

	recipient, err := deliverer.Resolve(ctx, "friend@elsewhere.test", true)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if err := deliverer.Deliver(ctx, &Envelope{
		ID:         "m1",
		ReturnPath: "ada@example.test",
		Recipients: []Recipient{recipient},
		Data:       []byte("Subject: out\r\n\r\nbody\r\n"),
	}); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	queued, err := store.ListQueuedMessages(ctx, 0)
	if err != nil {
		t.Fatalf("list the queue: %v", err)
	}
	if len(queued) != 1 || len(queued[0].Recipients) != 1 {
		t.Fatalf("queued %+v", queued)
	}
	if queued[0].Recipients[0].Address != "friend@elsewhere.test" {
		t.Errorf("queued address = %q", queued[0].Recipients[0].Address)
	}
	if queued[0].ReturnPath != "ada@example.test" {
		t.Errorf("return path = %q", queued[0].ReturnPath)
	}
	if queued[0].Recipients[0].ORCPT != "" {
		t.Errorf("a direct submission needs no orcpt, got %q", queued[0].Recipients[0].ORCPT)
	}
}

func TestDeliverRefusesAFullMailbox(t *testing.T) {
	deliverer, store := newTestDeliverer(t)
	ctx := context.Background()
	domain := seedDomain(t, store, "example.test")
	ada := seedAccount(t, store, domain, "ada")

	recipient, err := deliverer.Resolve(ctx, "ada@example.test", false)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	// The quota is checked again at delivery, because RCPT time did not know
	// how big the message would be.
	ada.QuotaBytes = 16
	if _, err := store.PutAccount(ctx, ada); err != nil {
		t.Fatalf("set the quota: %v", err)
	}
	err = deliverer.Deliver(ctx, &Envelope{
		ID:         "m1",
		Recipients: []Recipient{recipient},
		Data:       []byte(strings.Repeat("x", 1024)),
	})
	code, enhanced := codeOf(t, err)
	if code != 452 || enhanced != "4.2.2" {
		t.Fatalf("got %d %s, want 452 4.2.2", code, enhanced)
	}
	if messages := mailboxMessages(t, deliverer, ada.ID); len(messages) != 0 {
		t.Errorf("a refused message was still stored: %d files", len(messages))
	}
}

func TestDeliverRefusesNoRecipients(t *testing.T) {
	deliverer, _ := newTestDeliverer(t)
	code, _ := codeOf(t, deliverer.Deliver(context.Background(), &Envelope{ID: "m1"}))
	if code != 554 {
		t.Fatalf("got %d, want 554", code)
	}
}

// TestDeliverIsAtomic delivers concurrently to the same mailbox: every message
// must land whole, with a distinct name, and nothing may be left in tmp.
func TestDeliverIsAtomic(t *testing.T) {
	deliverer, store := newTestDeliverer(t)
	ctx := context.Background()
	domain := seedDomain(t, store, "example.test")
	ada := seedAccount(t, store, domain, "ada")
	recipient, err := deliverer.Resolve(ctx, "ada@example.test", false)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	const count = 16
	errs := make(chan error, count)
	for index := range count {
		go func() {
			errs <- deliverer.Deliver(ctx, &Envelope{
				ID:         "m",
				Recipients: []Recipient{recipient},
				Data:       []byte("Subject: " + strings.Repeat("y", index) + "\r\n\r\nbody\r\n"),
			})
		}()
	}
	for range count {
		if err := <-errs; err != nil {
			t.Fatalf("deliver: %v", err)
		}
	}
	messages := mailboxMessages(t, deliverer, ada.ID)
	if len(messages) != count {
		t.Fatalf("delivered %d messages, want %d", len(messages), count)
	}
	for _, message := range messages {
		if !strings.HasSuffix(message, "\r\n\r\nbody\r\n") {
			t.Errorf("a message was stored truncated:\n%q", message)
		}
	}
}

// -----------------------------------------------------------------------------
// Authentication
// -----------------------------------------------------------------------------

func TestAuthenticate(t *testing.T) {
	deliverer, store := newTestDeliverer(t)
	ctx := context.Background()
	domain := seedDomain(t, store, "example.test")
	ada := seedAccount(t, store, domain, "ada", "hello")
	ada = setTestPassword(t, store, ada, "correct horse battery staple")

	disabled := NewDomain("off.test", testClock)
	disabled.Enabled = false
	if _, err := store.CreateDomain(ctx, disabled); err != nil {
		t.Fatalf("create the disabled domain: %v", err)
	}
	blocked := seedAccount(t, store, disabled, "blocked")
	setTestPassword(t, store, blocked, "correct horse battery staple")

	tests := []struct {
		name     string
		username string
		password string
		wantCode int
		wantID   string
	}{
		{
			name:     "the mailbox address and its password",
			username: "ada@example.test",
			password: "correct horse battery staple",
			wantID:   ada.ID,
		},
		{
			name:     "the address is case insensitive",
			username: "ADA@Example.Test",
			password: "correct horse battery staple",
			wantID:   ada.ID,
		},
		{
			name:     "an alias is an address the customer was given",
			username: "hello@example.test",
			password: "correct horse battery staple",
			wantID:   ada.ID,
		},
		{
			name:     "a wrong password",
			username: "ada@example.test",
			password: "correct horse battery stapler",
			wantCode: 535,
		},
		{
			name:     "an unknown mailbox answers exactly as a wrong password does",
			username: "nobody@example.test",
			password: "correct horse battery staple",
			wantCode: 535,
		},
		{
			name:     "an unhosted domain",
			username: "ada@elsewhere.test",
			password: "correct horse battery staple",
			wantCode: 535,
		},
		{
			name:     "an empty password never authenticates",
			username: "ada@example.test",
			wantCode: 535,
		},
		{
			name:     "a malformed username",
			username: "ada",
			password: "correct horse battery staple",
			wantCode: 535,
		},
		{
			name:     "a mailbox in a disabled domain may not send",
			username: "blocked@off.test",
			password: "correct horse battery staple",
			wantCode: 535,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			identity, err := deliverer.Authenticate(ctx, test.username, test.password)
			if test.wantCode != 0 {
				code, _ := codeOf(t, err)
				if code != test.wantCode {
					t.Fatalf("got %d, want %d", code, test.wantCode)
				}
				// The credential must not survive into anything a log or a
				// reply could carry.
				if test.password != "" && strings.Contains(err.Error(), test.password) {
					t.Fatal("the failure quotes the password")
				}
				if identity.AccountID != "" {
					t.Errorf("a failed login returned an identity: %+v", identity)
				}
				return
			}
			if err != nil {
				t.Fatalf("authenticate: %v", err)
			}
			if identity.AccountID != test.wantID {
				t.Errorf("account = %q, want %q", identity.AccountID, test.wantID)
			}
			if !identity.MayUse("ada@example.test") || !identity.MayUse("hello@example.test") {
				t.Errorf("the identity does not own its own addresses: %+v", identity.Addresses)
			}
		})
	}
}

func TestIdentityMayUse(t *testing.T) {
	identity := Identity{AccountID: "a1", Addresses: []string{"ada@example.test", "hello@example.test"}}
	tests := []struct {
		address string
		want    bool
	}{
		{"ada@example.test", true},
		{"ADA@EXAMPLE.TEST", true},
		{"hello@example.test", true},
		{"ada+news@example.test", true},
		{"grace@example.test", false},
		{"ada@elsewhere.test", false},
		{"ada", false},
		{"", false},
	}
	for _, test := range tests {
		t.Run(test.address, func(t *testing.T) {
			if got := identity.MayUse(test.address); got != test.want {
				t.Fatalf("MayUse(%q) = %v, want %v", test.address, got, test.want)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// The queue bucket
// -----------------------------------------------------------------------------

func TestQueuedMessageCRUD(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	base := testClock
	for index, id := range []string{"q3", "q1", "q2"} {
		if _, err := store.PutQueuedMessage(ctx, QueuedMessage{
			ID:         id,
			ReturnPath: "sender@elsewhere.test",
			CreatedAt:  base.Add(time.Duration(index) * time.Minute),
			Recipients: []QueuedRecipient{{Address: id + "@elsewhere.test", Status: QueueStatusScheduled}},
		}); err != nil {
			t.Fatalf("put %s: %v", id, err)
		}
	}

	all, err := store.ListQueuedMessages(ctx, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	gotOrder := make([]string, 0, len(all))
	for _, message := range all {
		gotOrder = append(gotOrder, message.ID)
	}
	if strings.Join(gotOrder, ",") != "q3,q1,q2" {
		t.Errorf("order = %v, want the oldest first", gotOrder)
	}

	page, err := store.ListQueuedMessages(ctx, 2)
	if err != nil {
		t.Fatalf("list a page: %v", err)
	}
	if len(page) != 2 {
		t.Errorf("page = %d entries, want 2", len(page))
	}

	one, err := store.GetQueuedMessage(ctx, "q1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if one.V != DocVersion || one.Recipients[0].Address != "q1@elsewhere.test" {
		t.Errorf("round trip lost data: %+v", one)
	}

	if err := store.DeleteQueuedMessage(ctx, "q1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := store.GetQueuedMessage(ctx, "q1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("after delete: %v, want ErrNotFound", err)
	}
	// Deleting one that is not there is success, as it is everywhere else in
	// this store.
	if err := store.DeleteQueuedMessage(ctx, "q1"); err != nil {
		t.Errorf("delete an absent entry: %v", err)
	}
	if _, err := store.PutQueuedMessage(ctx, QueuedMessage{}); !errors.Is(err, ErrInvalid) {
		t.Errorf("put without an id: %v, want ErrInvalid", err)
	}
}

func TestAddAccountUsage(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	domain := seedDomain(t, store, "example.test")
	ada := seedAccount(t, store, domain, "ada")

	tests := []struct {
		name  string
		delta int64
		want  uint64
	}{
		{name: "a delivery adds", delta: 1000, want: 1000},
		{name: "an expunge subtracts", delta: -400, want: 600},
		{name: "zero is a no-op", delta: 0, want: 600},
		{name: "usage never goes negative", delta: -10_000, want: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := store.AddAccountUsage(ctx, ada.ID, test.delta); err != nil {
				t.Fatalf("add usage: %v", err)
			}
			stored, err := store.GetAccount(ctx, ada.ID)
			if err != nil {
				t.Fatalf("re-read: %v", err)
			}
			if stored.UsedBytes != test.want {
				t.Fatalf("usedBytes = %d, want %d", stored.UsedBytes, test.want)
			}
		})
	}
	if err := store.AddAccountUsage(ctx, "missing", 10); !errors.Is(err, ErrNotFound) {
		t.Errorf("usage for an absent mailbox: %v, want ErrNotFound", err)
	}
}

// -----------------------------------------------------------------------------
// Replies
// -----------------------------------------------------------------------------

func TestReplyFor(t *testing.T) {
	tests := []struct {
		name         string
		err          error
		wantCode     int
		wantEnhanced string
	}{
		{
			name:         "a permanent failure keeps its code",
			err:          errUnknownUser,
			wantCode:     550,
			wantEnhanced: "5.1.1",
		},
		{
			name:         "a wrapped failure is still found",
			err:          errors.Join(errors.New("context"), errMailboxFull),
			wantCode:     452,
			wantEnhanced: "4.2.2",
		},
		{
			name:         "an unlabelled failure is temporary, so no mail is lost to a bug",
			err:          errors.New("something went wrong"),
			wantCode:     451,
			wantEnhanced: "4.3.0",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			code, enhanced, message := replyFor(test.err)
			if code != test.wantCode || enhanced != test.wantEnhanced {
				t.Fatalf("got %d %s, want %d %s", code, enhanced, test.wantCode, test.wantEnhanced)
			}
			if message == "" {
				t.Error("a reply needs text")
			}
		})
	}
}

func TestDeliveryErrorTemporary(t *testing.T) {
	tests := []struct {
		code int
		want bool
	}{{code: 421, want: true}, {code: 450, want: true}, {code: 452, want: true},
		{code: 500, want: false}, {code: 550, want: false}, {code: 250, want: false}}
	for _, test := range tests {
		failure := &DeliveryError{Code: test.code}
		if got := failure.Temporary(); got != test.want {
			t.Errorf("%d: Temporary() = %v, want %v", test.code, got, test.want)
		}
	}
}

func TestNewDelivererRefusesAnEmptyRoot(t *testing.T) {
	store := newStore(t)
	if _, err := NewDeliverer(store, DeliveryConfig{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("got %v, want ErrInvalid", err)
	}
	if _, err := NewDeliverer(nil, DeliveryConfig{Root: t.TempDir()}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("got %v, want ErrInvalid", err)
	}
}

func TestSanitizeFileToken(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{in: "mx.example.test", want: "mx.example.test"},
		{in: " MX-1.test ", want: "MX-1.test"},
		{in: "a/b:c", want: "a-b-c"},
		{in: "///", want: ""},
	}
	for _, test := range tests {
		if got := sanitizeFileToken(test.in); got != test.want {
			t.Errorf("sanitizeFileToken(%q) = %q, want %q", test.in, got, test.want)
		}
	}
}

// TestQueuedFanOutIsAttributableToTheCustomersZone is the contract check on this
// phase's one wire-visible output. A fan-out puts the external target in the map
// key and the customer's own address only in orcpt; if delivery gets that
// backwards the queue still looks fine here but the facade attributes the
// message to no zone at all, and the console shows the customer nothing.
func TestQueuedFanOutIsAttributableToTheCustomersZone(t *testing.T) {
	deliverer, store := newTestDeliverer(t)
	ctx := context.Background()
	domain := seedDomain(t, store, "example.test")
	if _, err := store.CreateList(ctx, List{
		DomainID:   domain.ID,
		LocalPart:  "team",
		Recipients: []string{"outside@elsewhere.test"},
	}); err != nil {
		t.Fatalf("create the mailing list: %v", err)
	}
	recipient, err := deliverer.Resolve(ctx, "team@example.test", false)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if err := deliverer.Deliver(ctx, &Envelope{
		ID:         "m1",
		ReturnPath: "sender@elsewhere.test",
		Recipients: []Recipient{recipient},
		Data:       []byte("Subject: hello\r\n\r\nbody\r\n"),
	}); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	queued, err := store.ListQueuedMessages(ctx, MaxObjectsPerCall)
	if err != nil {
		t.Fatalf("list the queue: %v", err)
	}
	if len(queued) != 1 {
		t.Fatalf("queued %d messages, want 1", len(queued))
	}

	// Render as the JMAP phase will, then rebuild what the frozen client decodes
	// and ask the facade's own attribution rule.
	wire := queued[0].Wire()
	message := stalwart.QueuedMessage{ID: wire.ID, ReturnPath: wire.ReturnPath}
	for address, entry := range wire.Recipients {
		orcpt := ""
		if entry.ORCPT != nil {
			if _, stripped, found := strings.Cut(*entry.ORCPT, ";"); found {
				orcpt = stripped
			}
		}
		message.Recipients = append(message.Recipients, stalwart.QueuedRecipient{
			Address:   address,
			ORCPT:     orcpt,
			Status:    entry.Status.Type,
			QueueName: entry.QueueName,
		})
	}
	if len(message.Recipients) != 1 {
		t.Fatalf("the wire form carries %d recipients, want 1", len(message.Recipients))
	}
	if message.Recipients[0].Address != "outside@elsewhere.test" {
		t.Errorf("the map key must be the external target, got %q", message.Recipients[0].Address)
	}
	if message.Recipients[0].ORCPT != "team@example.test" {
		t.Errorf("orcpt = %q, want the customer's own address", message.Recipients[0].ORCPT)
	}
	if message.Recipients[0].Status != stalwart.QueueStatusScheduled {
		t.Errorf("status = %q, want %q", message.Recipients[0].Status, stalwart.QueueStatusScheduled)
	}
	if !stalwart.BelongsToZone(message, "example.test") {
		t.Fatal("the queued fan-out is attributable to no zone, so the console cannot show it")
	}
	if stalwart.BelongsToZone(message, "someone-else.test") {
		t.Fatal("the queued fan-out is attributed to a zone that has nothing to do with it")
	}
}

// -----------------------------------------------------------------------------
// Loops
// -----------------------------------------------------------------------------

// A forwarder target on a hosted domain that does not resolve must never reach
// the queue. The queue delivers over SMTP to the domain's MX, the facade
// publishes this server as the MX for every domain it hosts, so queuing the
// customer's own address is an outbound session to ourselves for an address this
// server has just decided does not exist. The resolve error was logged at debug
// and the target appended anyway; now the reply the lookup produced is the
// answer to the message.
func TestExpandListNeverRelaysAHostedAddressThatDidNotResolve(t *testing.T) {
	tests := []struct {
		name     string
		targets  []string
		seed     func(t *testing.T, store *Store, domain Domain)
		code     int
		enhanced string
	}{
		{
			name:     "an address this server hosts and does not have",
			targets:  []string{"nobody@example.test"},
			code:     550,
			enhanced: "5.1.1",
		},
		{
			name:    "a mailbox on a hosted domain that is over quota",
			targets: []string{"full@example.test"},
			seed: func(t *testing.T, store *Store, domain Domain) {
				t.Helper()
				account := seedAccount(t, store, domain, "full")
				account.QuotaBytes = 8
				account.UsedBytes = 64
				if _, err := store.PutAccount(context.Background(), account); err != nil {
					t.Fatalf("set the quota: %v", err)
				}
			},
			// Temporary, so the sender retries rather than being told the
			// address does not exist.
			code:     452,
			enhanced: "4.2.2",
		},
		{
			name:     "a target that is not an address at all",
			targets:  []string{"not-an-address"},
			code:     501,
			enhanced: "5.1.3",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			deliverer, store := newTestDeliverer(t)
			ctx := context.Background()
			domain := seedDomain(t, store, "example.test")
			if test.seed != nil {
				test.seed(t, store, domain)
			}
			if _, err := store.CreateList(ctx, List{
				DomainID:   domain.ID,
				LocalPart:  "team",
				Recipients: test.targets,
			}); err != nil {
				t.Fatalf("create the mailing list: %v", err)
			}

			recipient, err := deliverer.Resolve(ctx, "team@example.test", false)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			err = deliverer.Deliver(ctx, &Envelope{
				ID:         "m1",
				ReturnPath: "sender@outside.test",
				Recipients: []Recipient{recipient},
				Received:   "Received: from client by mx.test;\r\n",
				Data:       []byte("From: <sender@outside.test>\r\n\r\nbody\r\n"),
			})
			code, enhanced := codeOf(t, err)
			if code != test.code || enhanced != test.enhanced {
				t.Errorf("got %d %s, want %d %s", code, enhanced, test.code, test.enhanced)
			}
			queued, err := store.ListQueuedMessages(ctx, 0)
			if err != nil {
				t.Fatalf("list the queue: %v", err)
			}
			if len(queued) != 0 {
				t.Fatalf("a hosted address was queued for remote delivery: %+v", queued)
			}
		})
	}
}

// A list whose targets are all external still fans out; the fix above must not
// have turned the ordinary forwarder into a refusal.
func TestExpandListStillQueuesExternalTargets(t *testing.T) {
	deliverer, store := newTestDeliverer(t)
	ctx := context.Background()
	domain := seedDomain(t, store, "example.test")
	ada := seedAccount(t, store, domain, "ada")
	if _, err := store.CreateList(ctx, List{
		DomainID:   domain.ID,
		LocalPart:  "team",
		Recipients: []string{"ada@example.test", "outside@elsewhere.test"},
	}); err != nil {
		t.Fatalf("create the mailing list: %v", err)
	}
	recipient, err := deliverer.Resolve(ctx, "team@example.test", false)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if err := deliverer.Deliver(ctx, &Envelope{
		ID:         "m1",
		ReturnPath: "sender@elsewhere.test",
		Recipients: []Recipient{recipient},
		Received:   "Received: from client by mx.test;\r\n",
		Data:       []byte("From: <sender@elsewhere.test>\r\n\r\nbody\r\n"),
	}); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if messages := mailboxMessages(t, deliverer, ada.ID); len(messages) != 1 {
		t.Fatalf("the local target received %d messages, want 1", len(messages))
	}
	queued, err := store.ListQueuedMessages(ctx, 0)
	if err != nil {
		t.Fatalf("list the queue: %v", err)
	}
	if len(queued) != 1 || len(queued[0].Recipients) != 1 ||
		queued[0].Recipients[0].Address != "outside@elsewhere.test" {
		t.Fatalf("the external target was not queued: %+v", queued)
	}
}

// A forwarder that names a second forwarder on a hosted domain is still queued:
// it is a hop out through this server's own MX and back, which is a wasteful but
// legitimate way to chain two forwarders, and unlike an address that does not
// resolve it will be accepted at the other end. What stops it being a mail loop
// is the hop ceiling, not this function — see
// TestDeliverRefusesAMessageThatHasLooped.
func TestExpandListQueuesASecondForwarderAndTheHopCeilingBoundsIt(t *testing.T) {
	deliverer, store := newTestDeliverer(t)
	ctx := context.Background()
	domain := seedDomain(t, store, "example.test")
	for local, targets := range map[string][]string{
		"team":     {"everyone@example.test"},
		"everyone": {"outside@elsewhere.test"},
	} {
		if _, err := store.CreateList(ctx, List{
			DomainID: domain.ID, LocalPart: local, Recipients: targets,
		}); err != nil {
			t.Fatalf("create the %s list: %v", local, err)
		}
	}
	recipient, err := deliverer.Resolve(ctx, "team@example.test", false)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if err := deliverer.Deliver(ctx, &Envelope{
		ID:         "m1",
		ReturnPath: "sender@elsewhere.test",
		Recipients: []Recipient{recipient},
		Received:   "Received: from client by mx.test;\r\n",
		Data:       []byte("From: <sender@elsewhere.test>\r\n\r\nbody\r\n"),
	}); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	queued, err := store.ListQueuedMessages(ctx, 0)
	if err != nil {
		t.Fatalf("list the queue: %v", err)
	}
	if len(queued) != 1 || len(queued[0].Recipients) != 1 {
		t.Fatalf("queued %+v", queued)
	}
	if got := queued[0].Recipients[0].Address; got != "everyone@example.test" {
		t.Errorf("queued address = %q, want the second forwarder", got)
	}
	if got := queued[0].Recipients[0].ORCPT; got != "team@example.test" {
		t.Errorf("orcpt = %q, want the address the sender wrote", got)
	}
}

// The hop ceiling is what ends a loop this server cannot see the shape of: one
// that runs out through somebody else's forwarder and back in again.
func TestDeliverRefusesAMessageThatHasLooped(t *testing.T) {
	deliverer, store := newTestDeliverer(t)
	ctx := context.Background()
	domain := seedDomain(t, store, "example.test")
	ada := seedAccount(t, store, domain, "ada")
	recipient, err := deliverer.Resolve(ctx, "ada@example.test", false)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	trace := func(count int) []byte {
		var builder strings.Builder
		for range count {
			builder.WriteString("Received: from a.test by b.test;\r\n\tFri, 4 Sep 2026 09:00:00 +0000\r\n")
		}
		builder.WriteString("From: <sender@elsewhere.test>\r\n\r\nbody\r\n")
		return []byte(builder.String())
	}
	deliver := func(carried int) error {
		return deliverer.Deliver(ctx, &Envelope{
			ID:         "m1",
			Recipients: []Recipient{recipient},
			// This server's own trace header is the one that takes the count to
			// the limit, which is why the message carries one fewer.
			Received: "Received: from client by mx.test;\r\n",
			Data:     trace(carried),
		})
	}

	if err := deliver(maxReceivedHops - 1); err != nil {
		t.Fatalf("a message at the hop limit was refused: %v", err)
	}
	if messages := mailboxMessages(t, deliverer, ada.ID); len(messages) != 1 {
		t.Fatalf("the message at the limit was not delivered: %d messages", len(messages))
	}
	code, enhanced := codeOf(t, deliver(maxReceivedHops))
	if code != 554 || enhanced != "5.4.6" {
		t.Errorf("got %d %s, want 554 5.4.6", code, enhanced)
	}
	if messages := mailboxMessages(t, deliverer, ada.ID); len(messages) != 1 {
		t.Errorf("a looping message was still delivered: %d messages", len(messages))
	}
}

func TestCountReceived(t *testing.T) {
	tests := []struct {
		name    string
		message string
		want    int
	}{
		{"nothing", "", 0},
		{"one field", "Received: a\r\n\r\nbody\r\n", 1},
		{"case is not significant", "RECEIVED: a\r\nreceived: b\r\n\r\nbody\r\n", 2},
		{
			// A folded field is one hop however many lines it takes.
			name:    "continuation lines are not hops",
			message: "Received: from a\r\n\tby b\r\n\tfor <c>\r\n\r\nbody\r\n",
			want:    1,
		},
		{
			// Otherwise a sender could make their own mail undeliverable by
			// quoting a long enough thread.
			name:    "a body that quotes trace headers is not a hop",
			message: "Received: a\r\n\r\nReceived: b\r\nReceived: c\r\n",
			want:    1,
		},
		{"bare LF is still a header block", "Received: a\nReceived: b\n\nbody\n", 2},
		{"headers with no body", "Received: a\r\nReceived: b\r\n", 2},
		{"a similar name is not a hop", "Received-SPF: pass\r\n\r\nbody\r\n", 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := countReceived([]byte(test.message)); got != test.want {
				t.Errorf("countReceived = %d, want %d", got, test.want)
			}
		})
	}
}

// A hop that leaves no trace is a hop nothing downstream — including this server
// on the next lap — can count.
func TestDeliverAddsATraceHeaderWhenTheCallerDidNot(t *testing.T) {
	deliverer, store := newTestDeliverer(t)
	ctx := context.Background()
	domain := seedDomain(t, store, "example.test")
	ada := seedAccount(t, store, domain, "ada")
	recipient, err := deliverer.Resolve(ctx, "ada@example.test", false)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if err := deliverer.Deliver(ctx, &Envelope{
		ID:         "m1",
		Recipients: []Recipient{recipient},
		// A HELO carrying a bare CRLF would end the field and start another.
		HELO:       "client.test\r\nX-Injected: yes",
		RemoteAddr: "198.51.100.7:2525",
		Data:       []byte("From: <sender@elsewhere.test>\r\n\r\nbody\r\n"),
	}); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	messages := mailboxMessages(t, deliverer, ada.ID)
	if len(messages) != 1 {
		t.Fatalf("delivered %d messages, want 1", len(messages))
	}
	if countReceived([]byte(messages[0])) != 1 {
		t.Fatalf("the stored copy carries no trace header:\n%s", messages[0])
	}
	if !strings.Contains(messages[0], "by mx.test with SMTP id m1") {
		t.Errorf("the trace header does not name this server and the message:\n%s", messages[0])
	}
	if strings.Contains(messages[0], "X-Injected") && strings.Contains(messages[0], "\r\nX-Injected") {
		t.Errorf("a HELO injected a header into the trace:\n%s", messages[0])
	}
}

// -----------------------------------------------------------------------------
// DKIM on the way out
// -----------------------------------------------------------------------------

// seedDkim gives a domain one active Ed25519 key. Ed25519 only, because RSA-2048
// generation costs more than every other assertion in this file put together and
// dkim_test.go already proves both algorithms sign.
func seedDkim(t *testing.T, store *Store, domain Domain) DkimKey {
	t.Helper()
	keys, err := ProvisionDkimKeys(context.Background(), store, domain.ID, DkimOptions{
		Algorithms: []string{DkimAlgorithmEd25519},
		Now:        testClock,
	})
	if err != nil {
		t.Fatalf("provision DKIM keys: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("provisioned %d keys, want 1", len(keys))
	}
	return keys[0]
}

// dkimTags reads the tags of the topmost DKIM-Signature header of a message.
func dkimTags(t *testing.T, message string) (map[string]string, bool) {
	t.Helper()
	header, _, _ := strings.Cut(message, "\r\n\r\n")
	unfolded := strings.NewReplacer("\r\n\t", " ", "\r\n ", " ").Replace(header)
	for _, line := range strings.Split(unfolded, "\r\n") {
		name, value, found := strings.Cut(line, ":")
		if !found || !strings.EqualFold(strings.TrimSpace(name), "dkim-signature") {
			continue
		}
		tags := map[string]string{}
		for _, part := range strings.Split(value, ";") {
			key, tagValue, ok := strings.Cut(part, "=")
			if !ok {
				continue
			}
			tags[strings.TrimSpace(key)] = strings.TrimSpace(tagValue)
		}
		return tags, true
	}
	return nil, false
}

// spooledBody reads the one message the queue is holding.
func spooledBody(t *testing.T, deliverer *Deliverer, store *Store) string {
	t.Helper()
	queued, err := store.ListQueuedMessages(context.Background(), 0)
	if err != nil {
		t.Fatalf("list the queue: %v", err)
	}
	if len(queued) != 1 {
		t.Fatalf("queued %d messages, want 1", len(queued))
	}
	raw, err := os.ReadFile(deliverer.SpoolPath(queued[0].MessagePath))
	if err != nil {
		t.Fatalf("read the spooled body: %v", err)
	}
	if queued[0].SizeBytes != int64(len(raw)) {
		t.Errorf("sizeBytes = %d, but the spooled body is %d bytes", queued[0].SizeBytes, len(raw))
	}
	return string(raw)
}

// The facade publishes a DKIM record into the customer's zone the moment a
// domain is bound, which tells every receiver to expect a signature. Sending
// unsigned under a published key is an invitation to be filed as spam.
func TestOutboundMailIsDkimSigned(t *testing.T) {
	t.Run("a forwarder signs as the customer's domain", func(t *testing.T) {
		deliverer, store := newTestDeliverer(t)
		ctx := context.Background()
		domain := seedDomain(t, store, "example.test")
		key := seedDkim(t, store, domain)
		if _, err := store.CreateList(ctx, List{
			DomainID:   domain.ID,
			LocalPart:  "team",
			Recipients: []string{"outside@elsewhere.test"},
		}); err != nil {
			t.Fatalf("create the mailing list: %v", err)
		}
		recipient, err := deliverer.Resolve(ctx, "team@example.test", false)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		// The envelope sender is the original author on somebody else's domain,
		// which is exactly why the return path cannot be the only place the
		// signing domain is looked for.
		if err := deliverer.Deliver(ctx, &Envelope{
			ID:         "m1",
			ReturnPath: "author@elsewhere.test",
			Recipients: []Recipient{recipient},
			Received:   "Received: from client by mx.test;\r\n",
			Data:       []byte("From: <author@elsewhere.test>\r\nSubject: hi\r\n\r\nbody\r\n"),
		}); err != nil {
			t.Fatalf("deliver: %v", err)
		}
		body := spooledBody(t, deliverer, store)
		tags, signed := dkimTags(t, body)
		if !signed {
			t.Fatalf("the relayed message carries no DKIM-Signature:\n%s", body)
		}
		if tags["d"] != "example.test" || tags["s"] != key.Selector {
			t.Errorf("d=%q s=%q, want d=example.test s=%s", tags["d"], tags["s"], key.Selector)
		}
		if tags["c"] != "relaxed/relaxed" {
			t.Errorf("c=%q, want relaxed/relaxed", tags["c"])
		}
		if tags["a"] != "ed25519-sha256" {
			t.Errorf("a=%q, want ed25519-sha256", tags["a"])
		}
		for _, name := range []string{"from", "subject"} {
			if !strings.Contains(tags["h"], name) {
				t.Errorf("h=%q does not cover %s", tags["h"], name)
			}
		}
		// The signature has to be the topmost header, above the trace header, or
		// it claims to have been added before a hop that came first.
		if !strings.HasPrefix(body, "DKIM-Signature:") {
			t.Errorf("the signature is not the topmost header:\n%s", body)
		}
	})

	t.Run("a submission signs as the sender's own domain", func(t *testing.T) {
		deliverer, store := newTestDeliverer(t)
		ctx := context.Background()
		domain := seedDomain(t, store, "example.test")
		seedDkim(t, store, domain)
		recipient, err := deliverer.Resolve(ctx, "friend@elsewhere.test", true)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if err := deliverer.Deliver(ctx, &Envelope{
			ID:         "m1",
			ReturnPath: "ada@example.test",
			Recipients: []Recipient{recipient},
			Data:       []byte("From: <ada@example.test>\r\nSubject: out\r\n\r\nbody\r\n"),
		}); err != nil {
			t.Fatalf("deliver: %v", err)
		}
		tags, signed := dkimTags(t, spooledBody(t, deliverer, store))
		if !signed {
			t.Fatal("a submission left unsigned")
		}
		if tags["d"] != "example.test" {
			t.Errorf("d=%q, want example.test", tags["d"])
		}
	})

	t.Run("a domain with no key sends unsigned rather than failing", func(t *testing.T) {
		deliverer, store := newTestDeliverer(t)
		ctx := context.Background()
		seedDomain(t, store, "example.test")
		recipient, err := deliverer.Resolve(ctx, "friend@elsewhere.test", true)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if err := deliverer.Deliver(ctx, &Envelope{
			ID:         "m1",
			ReturnPath: "ada@example.test",
			Recipients: []Recipient{recipient},
			Data:       []byte("From: <ada@example.test>\r\n\r\nbody\r\n"),
		}); err != nil {
			t.Fatalf("a domain with no active key could not send: %v", err)
		}
		if _, signed := dkimTags(t, spooledBody(t, deliverer, store)); signed {
			t.Error("a domain with no key produced a signature")
		}
	})

	t.Run("a message with no From header sends unsigned rather than failing", func(t *testing.T) {
		deliverer, store := newTestDeliverer(t)
		ctx := context.Background()
		domain := seedDomain(t, store, "example.test")
		seedDkim(t, store, domain)
		recipient, err := deliverer.Resolve(ctx, "friend@elsewhere.test", true)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		// RFC 6376 §5.4 makes From mandatory in h=, so dkim.go refuses to sign
		// this. Refusing to SEND it would be worse than sending it unsigned.
		if err := deliverer.Deliver(ctx, &Envelope{
			ID:         "m1",
			ReturnPath: "ada@example.test",
			Recipients: []Recipient{recipient},
			Data:       []byte("Subject: no from\r\n\r\nbody\r\n"),
		}); err != nil {
			t.Fatalf("a message that could not be signed was not sent: %v", err)
		}
		if _, signed := dkimTags(t, spooledBody(t, deliverer, store)); signed {
			t.Error("a message with no From header was signed")
		}
	})

	t.Run("mail for nobody this server hosts is not signed", func(t *testing.T) {
		deliverer, store := newTestDeliverer(t)
		ctx := context.Background()
		domain := seedDomain(t, store, "example.test")
		seedDkim(t, store, domain)
		recipient, err := deliverer.Resolve(ctx, "friend@elsewhere.test", true)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		// Nothing here names a hosted domain, so there is no key this server is
		// entitled to claim.
		if err := deliverer.Deliver(ctx, &Envelope{
			ID:         "m1",
			ReturnPath: "stranger@nowhere.test",
			Recipients: []Recipient{recipient},
			Data:       []byte("From: <stranger@nowhere.test>\r\n\r\nbody\r\n"),
		}); err != nil {
			t.Fatalf("deliver: %v", err)
		}
		if _, signed := dkimTags(t, spooledBody(t, deliverer, store)); signed {
			t.Error("this server signed for a domain it does not host")
		}
	})
}
