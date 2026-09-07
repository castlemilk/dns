package maild

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seedMailbox writes a maildir with one message in new/ and one in cur/, and
// returns the mailbox directory.
func seedMailbox(t *testing.T, spool Maildir, accountID string) string {
	t.Helper()
	if err := spool.Provision(accountID); err != nil {
		t.Fatalf("provision the maildir: %v", err)
	}
	base := spool.Path(accountID)
	for _, name := range []string{
		filepath.Join(maildirNew, "1757000000.M1P1Q1.host,S=12"),
		filepath.Join(maildirCur, "1757000001.M1P1Q2.host,S=12:2,S"),
	} {
		if err := os.WriteFile(filepath.Join(base, name), []byte("Subject: hi\r\n\r\nhi\r\n"), 0o600); err != nil {
			t.Fatalf("write a message: %v", err)
		}
	}
	return base
}

// TestPurgeMailboxRemovesTheMailAndTheDirectory is the whole point: the account
// row is gone, so the plaintext mail it authorised must not outlive it on the
// volume with no owner left to attribute or authorise it.
func TestPurgeMailboxRemovesTheMailAndTheDirectory(t *testing.T) {
	root := t.TempDir()
	spool := NewMaildir(root)
	mailbox := seedMailbox(t, spool, "keepme")
	other := seedMailbox(t, spool, "neighbour")

	if err := PurgeMailbox(spool, "keepme"); err != nil {
		t.Fatalf("PurgeMailbox: %v", err)
	}
	if _, err := os.Stat(mailbox); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the mailbox survived the purge: %v", err)
	}
	// A purge that took the whole spool with it would pass the assertion above
	// and lose every other customer's mail.
	if _, err := os.Stat(other); err != nil {
		t.Errorf("the purge removed a neighbouring mailbox: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, MailboxesDir)); err != nil {
		t.Errorf("the purge removed the spool itself: %v", err)
	}
}

// TestPurgeMailboxIsIdempotent. The facade retries a destroy, and a mailbox
// that was never delivered to has no directory at all; neither is an error.
func TestPurgeMailboxIsIdempotent(t *testing.T) {
	spool := NewMaildir(t.TempDir())
	seedMailbox(t, spool, "twice")

	for attempt := 1; attempt <= 3; attempt++ {
		if err := PurgeMailbox(spool, "twice"); err != nil {
			t.Fatalf("purge attempt %d: %v", attempt, err)
		}
	}
	if err := PurgeMailbox(spool, "never-delivered-to"); err != nil {
		t.Errorf("purging a mailbox that was never created: %v", err)
	}
}

// TestPurgeMailboxToleratesAPartiallyRemovedDirectory. A purge interrupted
// halfway leaves a maildir missing some of its folders; the retry must finish
// the job rather than fail on what is already gone.
func TestPurgeMailboxToleratesAPartiallyRemovedDirectory(t *testing.T) {
	spool := NewMaildir(t.TempDir())
	mailbox := seedMailbox(t, spool, "halfgone")
	if err := os.RemoveAll(filepath.Join(mailbox, maildirNew)); err != nil {
		t.Fatalf("simulate a partial purge: %v", err)
	}

	if err := PurgeMailbox(spool, "halfgone"); err != nil {
		t.Fatalf("PurgeMailbox: %v", err)
	}
	if _, err := os.Stat(mailbox); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the half-removed mailbox survived: %v", err)
	}
}

// TestPurgeMailboxRefusesToLeaveTheSpool. The account id arrives over the
// network inside x:Account/set destroy, and os.RemoveAll on the wrong argument
// cannot be walked back. Every one of these must be refused before anything is
// deleted, and the refusal must name the reason rather than the input.
func TestPurgeMailboxRefusesToLeaveTheSpool(t *testing.T) {
	// reason names which of the two checks must refuse this id. The pair is
	// asserted rather than just "some refusal" because the checks are not
	// redundant — see mailboxPath — and a test that accepted either would let
	// one of them be deleted as already-covered, after which the next input in
	// its column would be purged for real.
	const (
		notContained = "the target is not inside"
		notAnElement = "not a single path element"
	)
	for _, testCase := range []struct{ name, id, reason string }{
		{"empty", "", notContained},
		{"dot", ".", notContained},
		{"parent", "..", notContained},
		{"traversal", "../../etc", notContained},
		{"absolute", "/etc", notAnElement},
		{"nested", "a/b", notAnElement},
		{"backslash", `a\b`, notAnElement},
		{"trailing slash", "account/", notAnElement},
		{"nul byte", "acc\x00ount", notAnElement},
		{"windows volume", "c:", notAnElement},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			root := t.TempDir()
			spool := NewMaildir(root)
			seedMailbox(t, spool, "bystander")
			sentinel := filepath.Join(root, "sentinel")
			if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
				t.Fatalf("write the sentinel: %v", err)
			}

			err := PurgeMailbox(spool, testCase.id)
			if !errors.Is(err, ErrUnsafeMailboxPath) {
				t.Fatalf("PurgeMailbox(%q) = %v, want ErrUnsafeMailboxPath", testCase.id, err)
			}
			if !strings.Contains(err.Error(), testCase.reason) {
				t.Errorf("PurgeMailbox(%q) refused with %v, want the reason %q", testCase.id, err, testCase.reason)
			}
			if strings.Contains(err.Error(), testCase.id) && testCase.id != "" {
				t.Errorf("the refusal quotes the untrusted id: %v", err)
			}
			if _, statErr := os.Stat(sentinel); statErr != nil {
				t.Errorf("the refused purge deleted outside the spool: %v", statErr)
			}
			if _, statErr := os.Stat(spool.Path("bystander")); statErr != nil {
				t.Errorf("the refused purge deleted a real mailbox: %v", statErr)
			}
			if _, statErr := os.Stat(filepath.Join(root, MailboxesDir)); statErr != nil {
				t.Errorf("the refused purge deleted the spool: %v", statErr)
			}
		})
	}
}

// TestPurgeMailboxRefusesADangerousSpoolRoot. filepath.Clean turns "" into ".",
// the working directory, and a spool at "/" would put the mailboxes directory
// at the root of the filesystem. Neither is a spool this server configured.
func TestPurgeMailboxRefusesADangerousSpoolRoot(t *testing.T) {
	for _, testCase := range []struct{ name, root string }{
		{"empty", ""},
		{"dot", "."},
		{"filesystem root", string(filepath.Separator)},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if err := PurgeMailbox(NewMaildir(testCase.root), "account"); !errors.Is(err, ErrUnsafeMailboxPath) {
				t.Errorf("PurgeMailbox with root %q = %v, want ErrUnsafeMailboxPath", testCase.root, err)
			}
		})
	}
	// A nil store yields the zero Maildir, whose root is "".
	if err := PurgeMailbox(MailboxSpool(nil), "account"); !errors.Is(err, ErrUnsafeMailboxPath) {
		t.Errorf("PurgeMailbox on a nil store = %v, want ErrUnsafeMailboxPath", err)
	}
}

// TestMailboxSpoolIsTheDirectoryDeliveryWritesTo pins the derivation the
// destroy path depends on. server.go opens the store at <data dir>/maild.db and
// hands the same <data dir> to the services, which pass it to NewMaildir. If
// the two ever disagree the purge would quietly remove nothing, and the test
// that matters would be the one nobody wrote.
func TestMailboxSpoolIsTheDirectoryDeliveryWritesTo(t *testing.T) {
	dataDir := t.TempDir()
	store, err := Open(filepath.Join(dataDir, storeFileName))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { drop(store.Close()) })

	spool := MailboxSpool(store)
	if got, want := spool.Root(), filepath.Clean(dataDir); got != want {
		t.Errorf("MailboxSpool root = %q, want %q", got, want)
	}
	if got, want := spool.Path("abc"), NewMaildir(dataDir).Path("abc"); got != want {
		t.Errorf("MailboxSpool path = %q, delivery writes to %q", got, want)
	}
}

// drop names an error a test deliberately ignores; errcheck's check-blank rule
// refuses the blank identifier for it.
func drop(error) {}
