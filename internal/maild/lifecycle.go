package maild

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Mailbox lifecycle: what has to happen on disk when x:Account/set destroy
// removes an account.
//
// Deleting the account row is not the end of it. The maildir under
// <data dir>/mailboxes/<account id> holds the customer's mail in plain files,
// and CONTRACT.md §3.6 says only that "the purge behind it is asynchronous" —
// which the store took as permission never to purge at all. An unbind with
// delete_mailboxes then leaves every message the customer ever received sitting
// on the node, readable by anything that can read the volume, with no account
// left to attribute it to or authorise its deletion. The bytes outlive their
// only owner.
//
// So the destroy path purges. The rules are deliberately dull:
//
//   - the store row goes first, the files second. The row is the authority; if
//     the order were reversed and the row delete then failed, a live account
//     would have had its mail destroyed under it;
//   - never delete outside the mailboxes directory. The account id reaches this
//     server over the network, so it is treated as untrusted input and must be
//     a single plain path element before it is joined to anything;
//   - a missing or half-removed directory is success. Destroy is retried by the
//     facade, and a purge that fails the second time because the first one
//     already worked would report a live account.

// ErrUnsafeMailboxPath refuses a purge whose target is not inside the spool.
// Every refusal wraps it, so a caller can tell "I would have deleted the wrong
// thing" from "the disk failed".
var ErrUnsafeMailboxPath = errors.New("maild: refusing to purge a path outside the mailbox spool")

// MailboxSpool names the maildir spool that belongs to a store.
//
// The spool root is the directory the store file lives in, which is Config
// .DataDir — server.go opens the store at <data dir>/maild.db and hands the
// same <data dir> to the services as Deps.DataDir, which is what
// NewMaildir is given. Deriving it here rather than plumbing a second copy of
// the path keeps the two from drifting apart; lifecycle_test.go pins the
// derivation against NewMaildir so a change to either fails the build's tests
// rather than silently purging nothing.
func MailboxSpool(store *Store) Maildir {
	if store == nil {
		return Maildir{}
	}
	return NewMaildir(filepath.Dir(store.Path()))
}

// PurgeMailbox removes an account's maildir and everything in it.
//
// It is idempotent: a mailbox that was never delivered to, or that a previous
// purge already removed, is success. It tolerates a partially removed directory
// for the same reason.
//
// It refuses, with ErrUnsafeMailboxPath, anything that would reach outside the
// spool's mailboxes directory — an empty or traversing account id, or a spool
// rooted at "/" or at nothing. accountID arrives from the network, and
// os.RemoveAll on the wrong argument is not a mistake that can be walked back.
func PurgeMailbox(spool Maildir, accountID string) error {
	target, err := mailboxPath(spool, accountID)
	if err != nil {
		return err
	}
	// RemoveAll reports success for a path that is not there, which is exactly
	// the idempotence wanted here, and it removes a symlink rather than
	// following it, so a symlinked mailbox cannot be used to delete elsewhere.
	if err := os.RemoveAll(target); err != nil {
		return fmt.Errorf("maild: purge a destroyed mailbox: %w", err)
	}
	return nil
}

// mailboxPath resolves and vets the directory PurgeMailbox would delete. It is
// separate so the safety rules can be tested without deleting anything.
func mailboxPath(spool Maildir, accountID string) (string, error) {
	root := filepath.Clean(spool.Root())
	// filepath.Clean turns "" into "." — the process's working directory, which
	// is never a spool and is a catastrophic thing to delete under.
	switch root {
	case "", ".", string(filepath.Separator):
		return "", fmt.Errorf("%w: the spool root is %q", ErrUnsafeMailboxPath, spool.Root())
	}
	// Two checks, in this order, because each catches inputs the other does
	// not and neither is redundant:
	//
	//  1. containment. An id that climbs out ("..", "../../etc", or the empty
	//     string, which resolves to the mailboxes directory itself) lands
	//     somewhere that is not a mailbox, and deleting the spool or its parent
	//     would take every other customer's mail with it;
	//  2. a single path element. "/etc", "a/b" and "account/" all stay inside
	//     the mailboxes directory once joined, so containment passes them, but
	//     none of them is an account id — they name a subdirectory of some
	//     mailbox, or a mailbox the caller did not ask for.
	mailboxes := filepath.Join(root, MailboxesDir)
	target := filepath.Clean(spool.Path(accountID))
	if target == mailboxes || !strings.HasPrefix(target, mailboxes+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: the target is not inside %s", ErrUnsafeMailboxPath, MailboxesDir)
	}
	if !safeMailboxID(accountID) {
		// The id is not quoted into the message. It is attacker-controlled
		// text on its way to a log line.
		return "", fmt.Errorf("%w: the account id is not a single path element", ErrUnsafeMailboxPath)
	}
	return target, nil
}

// safeMailboxID reports whether accountID is a single, plain path element: no
// separator, no traversal, no NUL, nothing empty. Store ids are generated
// lowercase base32, so this refuses nothing a real account can be called.
func safeMailboxID(accountID string) bool {
	if accountID == "" || accountID == "." || accountID == ".." {
		return false
	}
	if strings.ContainsAny(accountID, `/\`+"\x00") {
		return false
	}
	// A Windows volume name ("c:") would also escape a join; separators are
	// checked above, so this only has to catch the drive-relative form.
	return !strings.ContainsRune(accountID, ':')
}
