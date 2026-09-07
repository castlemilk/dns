package maild

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// -----------------------------------------------------------------------------
// Fixtures
// -----------------------------------------------------------------------------

const (
	testMailboxAddress  = "mara@probe.test"
	testMailboxPassword = "correct horse battery staple five"
)

// storedMessage is one message written into a test spool.
type storedMessage struct {
	// folder is "new" or "cur".
	folder string
	// name is the maildir file name; the part before ':' is the unique name.
	name string
	// body is written verbatim, so a test can put a bare LF or a leading dot in
	// it deliberately.
	body string
}

// pop3Fixture is a server over a temporary store and spool, with one mailbox.
type pop3Fixture struct {
	server  *POP3Server
	store   *Store
	spool   Maildir
	account Account
	domain  Domain
}

func newPOP3Fixture(t *testing.T, messages ...storedMessage) *pop3Fixture {
	t.Helper()
	ctx := context.Background()

	store := openTestStore(t)
	domain, err := store.CreateDomain(ctx, NewDomain("probe.test", referenceTime))
	if err != nil {
		t.Fatalf("CreateDomain: %v", err)
	}

	account := NewAccount(domain.ID, "mara", referenceTime)
	// hashPasswordWith rather than HashPassword: the shape is identical and the
	// tests do not need to pay 600,000 iterations for every fixture.
	hash, err := hashPasswordWith(testMailboxPassword, 2)
	if err != nil {
		t.Fatalf("hashPasswordWith: %v", err)
	}
	account.Password = hash
	account, err = store.CreateAccount(ctx, account)
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}

	spool := NewMaildir(filepath.Join(t.TempDir(), "mail"))
	if err := spool.Provision(account.ID); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	for _, message := range messages {
		path := filepath.Join(spool.Path(account.ID), message.folder, message.name)
		if err := os.WriteFile(path, []byte(message.body), 0o600); err != nil {
			t.Fatalf("write a spool message: %v", err)
		}
	}

	server := NewPOP3Server(store, spool,
		POP3WithHostname("mail.local.test"),
		POP3WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	)
	return &pop3Fixture{server: server, store: store, spool: spool, account: account, domain: domain}
}

// run drives a whole session from a scripted command list and returns the
// transcript as lines. The session is exercised over an in-memory ReadWriter,
// so no socket and no TLS handshake is involved; secure is passed explicitly,
// which is the reason session takes it as an argument.
func (f *pop3Fixture) run(t *testing.T, secure bool, commands ...string) []string {
	t.Helper()
	return f.runWith(t, f.server, secure, commands...)
}

func (f *pop3Fixture) runWith(t *testing.T, server *POP3Server, secure bool, commands ...string) []string {
	t.Helper()

	input := ""
	for _, command := range commands {
		input += command + "\r\n"
	}
	var output strings.Builder
	stream := struct {
		io.Reader
		io.Writer
	}{strings.NewReader(input), &output}

	err := server.session(context.Background(), stream, secure, "test")
	// A script that does not end in QUIT runs the reader dry, which is the
	// client hanging up: the session reports EOF and must not have deleted
	// anything.
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("session: %v", err)
	}
	return splitTranscript(output.String())
}

func splitTranscript(text string) []string {
	return strings.Split(strings.TrimSuffix(text, "\r\n"), "\r\n")
}

// login is the two commands every transaction-state test starts with.
func login() []string {
	return []string{"USER " + testMailboxAddress, "PASS " + testMailboxPassword}
}

func script(extra ...string) []string {
	return append(login(), extra...)
}

// requireLine fails unless the transcript has the exact line.
func requireLine(t *testing.T, transcript []string, want string) {
	t.Helper()
	for _, line := range transcript {
		if line == want {
			return
		}
	}
	t.Fatalf("no line %q in transcript:\n%s", want, strings.Join(transcript, "\n"))
}

// requirePrefix fails unless some line starts with prefix, and returns it.
func requirePrefix(t *testing.T, transcript []string, prefix string) string {
	t.Helper()
	for _, line := range transcript {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	t.Fatalf("no line starting %q in transcript:\n%s", prefix, strings.Join(transcript, "\n"))
	return ""
}

// -----------------------------------------------------------------------------
// The spool
// -----------------------------------------------------------------------------

func TestMaildirMessages(t *testing.T) {
	t.Parallel()

	fixture := newPOP3Fixture(t,
		storedMessage{folder: "cur", name: "1788000200.M1P2Q2.mail-local-test:2,S", body: "second\r\n"},
		storedMessage{folder: "new", name: "1788000100.M1P2Q1.mail-local-test", body: "first\r\n"},
		storedMessage{folder: "new", name: ".ignored", body: "dotfile\r\n"},
	)

	messages, err := fixture.spool.Messages(fixture.account.ID)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("got %d messages, want 2 (a leading dot is not a message)", len(messages))
	}
	// Ordered by the maildir unique name, which begins with the delivery
	// timestamp — so new/ and cur/ interleave chronologically rather than
	// new/ always coming first.
	if !strings.HasSuffix(messages[0].Path, "1788000100.M1P2Q1.mail-local-test") {
		t.Errorf("first message is %q, want the earlier delivery", messages[0].Path)
	}
	if messages[0].Size != int64(len("first\r\n")) {
		t.Errorf("size = %d, want %d", messages[0].Size, len("first\r\n"))
	}

	// The UIDL is a digest of the unique name, so it does not change when a
	// message moves from new/ to cur/ and picks up a flag suffix. A UIDL that
	// changed would make a client download every message again.
	if got, want := maildirUID("1788000100.M1P2Q1.mail-local-test,S=11"), maildirUID("1788000100.M1P2Q1.mail-local-test,S=11:2,S"); got != want {
		t.Errorf("UIDL changed when the message was flagged: %q vs %q", got, want)
	}
	if len(messages[0].UID) != 32 {
		t.Errorf("UIDL is %d characters; RFC 1939 allows 1 to 70", len(messages[0].UID))
	}
	for _, character := range messages[0].UID {
		if character < 0x21 || character > 0x7e {
			t.Errorf("UIDL contains %q, outside the 0x21-0x7e alphabet", character)
		}
	}
}

func TestMaildirMessagesOfAnUndeliveredMailbox(t *testing.T) {
	t.Parallel()

	spool := NewMaildir(filepath.Join(t.TempDir(), "mail"))
	messages, err := spool.Messages(AccountID("d-nobody", "nobody"))
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(messages) != 0 {
		t.Errorf("got %d messages, want none", len(messages))
	}
}

// -----------------------------------------------------------------------------
// A whole session
// -----------------------------------------------------------------------------

const storedBody = "Return-Path: <ada@acme.dev>\r\n" +
	"From: Ada <ada@acme.dev>\r\n" +
	"To: Mara <mara@probe.test>\r\n" +
	"Subject: Hello\r\n" +
	"\r\n" +
	"First body line.\r\n" +
	"Second body line.\r\n"

// TestPOP3SessionListsAndRetrieves is the session the task asks for: log in,
// see the maildrop, and read a message back byte for byte.
func TestPOP3SessionListsAndRetrieves(t *testing.T) {
	t.Parallel()

	fixture := newPOP3Fixture(t,
		storedMessage{folder: "new", name: "1788000100.M1P2Q1.mail-local-test", body: storedBody},
		storedMessage{folder: "new", name: "1788000200.M1P2Q2.mail-local-test", body: "From: b@x\r\n\r\nsecond\r\n"},
	)
	transcript := fixture.run(t, true, script("STAT", "LIST", "UIDL", "RETR 1", "QUIT")...)

	requirePrefix(t, transcript, "+OK mail.local.test POP3 ready")
	requireLine(t, transcript, "+OK maildrop has 2 messages ("+
		strconv.Itoa(len(storedBody)+len("From: b@x\r\n\r\nsecond\r\n"))+" octets)")

	// STAT is the count and the total octets.
	requireLine(t, transcript, "+OK 2 "+strconv.Itoa(len(storedBody)+len("From: b@x\r\n\r\nsecond\r\n")))
	// LIST is one line per message, terminated by a lone dot.
	requireLine(t, transcript, "1 "+strconv.Itoa(len(storedBody)))
	requireLine(t, transcript, "2 "+strconv.Itoa(len("From: b@x\r\n\r\nsecond\r\n")))
	// UIDL gives the client something stable to remember.
	uidl := requirePrefix(t, transcript, "1 "+maildirUID("1788000100.M1P2Q1.mail-local-test"))
	if uidl == "" {
		t.Fatal("no UIDL for message 1")
	}

	// RETR returns the message, then the terminator.
	retr := strings.Index(strings.Join(transcript, "\n"), "+OK "+strconv.Itoa(len(storedBody))+" octets")
	if retr < 0 {
		t.Fatalf("no RETR response in transcript:\n%s", strings.Join(transcript, "\n"))
	}
	body := retrievedBody(t, strings.Join(transcript, "\r\n")+"\r\n", "+OK "+strconv.Itoa(len(storedBody))+" octets")
	if body != storedBody {
		t.Errorf("RETR returned:\n%q\nwant:\n%q", body, storedBody)
	}
	requireLine(t, transcript, "+OK bye")
}

// retrievedBody pulls the multi-line payload that follows a status line out of
// a transcript and un-stuffs it, which is what a POP3 client does.
func retrievedBody(t *testing.T, transcript, statusLine string) string {
	t.Helper()
	_, rest, found := strings.Cut(transcript, statusLine+"\r\n")
	if !found {
		t.Fatalf("no %q in the transcript", statusLine)
	}
	payload, _, found := strings.Cut(rest, "\r\n.\r\n")
	if !found {
		t.Fatalf("the multi-line response after %q is not terminated", statusLine)
	}
	var out strings.Builder
	for _, line := range strings.Split(payload, "\r\n") {
		out.WriteString(strings.TrimPrefix(line, "."))
		out.WriteString("\r\n")
	}
	return out.String()
}

// TestPOP3ByteStuffing covers the framing hazard: a body line that is a lone
// dot, or that starts with one, would otherwise end the transfer early and the
// client would file a truncated message as complete.
func TestPOP3ByteStuffing(t *testing.T) {
	t.Parallel()

	body := "From: a@x\r\n\r\n.\r\n.hidden\r\nplain\r\n"
	fixture := newPOP3Fixture(t, storedMessage{folder: "new", name: "1788000100.M1P2Q1.mail-local-test", body: body})

	transcript := strings.Join(fixture.run(t, true, script("RETR 1", "QUIT")...), "\r\n") + "\r\n"
	requireLine(t, splitTranscript(transcript), "..")
	requireLine(t, splitTranscript(transcript), "..hidden")

	if got := retrievedBody(t, transcript, "+OK "+strconv.Itoa(len(body))+" octets"); got != body {
		t.Errorf("the un-stuffed message is %q, want %q", got, body)
	}
}

// TestPOP3RetrievesAMessageWithBareNewlines is the defensive half: the spool is
// supposed to hold CRLF, but a bare LF before a lone dot would truncate the
// transfer, so the reader normalises.
func TestPOP3RetrievesAMessageWithBareNewlines(t *testing.T) {
	t.Parallel()

	fixture := newPOP3Fixture(t, storedMessage{folder: "new", name: "1788000100.M1P2Q1.mail-local-test", body: "From: a@x\n\nbody\n.\nmore\n"})
	transcript := strings.Join(fixture.run(t, true, script("RETR 1", "QUIT")...), "\r\n") + "\r\n"
	for _, line := range splitTranscript(transcript) {
		if strings.Contains(line, "\n") {
			t.Fatalf("a bare LF survived into the transcript: %q", line)
		}
	}
	requireLine(t, splitTranscript(transcript), "..")
	requireLine(t, splitTranscript(transcript), "more")
}

func TestPOP3TOP(t *testing.T) {
	t.Parallel()

	fixture := newPOP3Fixture(t, storedMessage{folder: "new", name: "1788000100.M1P2Q1.mail-local-test", body: storedBody})

	tests := []struct {
		name     string
		command  string
		wantBody []string
		wantGone []string
	}{
		{
			name:     "headers only",
			command:  "TOP 1 0",
			wantBody: []string{"Subject: Hello"},
			wantGone: []string{"First body line.", "Second body line."},
		},
		{
			name:     "headers and one line",
			command:  "TOP 1 1",
			wantBody: []string{"Subject: Hello", "First body line."},
			wantGone: []string{"Second body line."},
		},
		{
			name:     "more lines than the body has",
			command:  "TOP 1 50",
			wantBody: []string{"First body line.", "Second body line."},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			transcript := fixture.run(t, true, script(test.command, "QUIT")...)
			for _, want := range test.wantBody {
				requireLine(t, transcript, want)
			}
			for _, unwanted := range test.wantGone {
				for _, line := range transcript {
					if line == unwanted {
						t.Errorf("%q returned %q, which is past the requested line count", test.command, unwanted)
					}
				}
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Deletion
// -----------------------------------------------------------------------------

// TestPOP3DeleteSemantics is the one behaviour a POP3 user notices when it is
// wrong: mail must disappear on QUIT and only on QUIT, because a session that
// drops mid-download has to be safe to retry.
func TestPOP3DeleteSemantics(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		commands  []string
		wantFiles int
		wantLine  string
	}{
		{
			name:      "DELE then QUIT removes the file",
			commands:  []string{"DELE 1", "QUIT"},
			wantFiles: 1,
			wantLine:  "+OK bye",
		},
		{
			name: "DELE without QUIT keeps it",
			// No QUIT: the client hung up, which is what a dropped connection
			// looks like. RFC 1939 never enters UPDATE, so nothing is removed.
			commands:  []string{"DELE 1", "DELE 2"},
			wantFiles: 2,
		},
		{
			name:      "RSET undoes the marks",
			commands:  []string{"DELE 1", "DELE 2", "RSET", "QUIT"},
			wantFiles: 2,
			wantLine:  "+OK maildrop has 2 messages (22 octets)",
		},
		{
			name:      "both messages",
			commands:  []string{"DELE 1", "DELE 2", "QUIT"},
			wantFiles: 0,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fixture := newPOP3Fixture(t,
				storedMessage{folder: "new", name: "1788000100.M1P2Q1.mail-local-test", body: "From: a\r\n\r\n"},
				storedMessage{folder: "new", name: "1788000200.M1P2Q2.mail-local-test", body: "From: b\r\n\r\n"},
			)
			transcript := fixture.run(t, true, script(test.commands...)...)
			if test.wantLine != "" {
				requireLine(t, transcript, test.wantLine)
			}

			remaining, err := fixture.spool.Messages(fixture.account.ID)
			if err != nil {
				t.Fatalf("Messages: %v", err)
			}
			if len(remaining) != test.wantFiles {
				t.Errorf("%d messages left in the spool, want %d", len(remaining), test.wantFiles)
			}
		})
	}
}

// TestPOP3DeletedMessagesAreInvisible covers the in-session view: a deleted
// message keeps its number but is gone from STAT, LIST and RETR.
func TestPOP3DeletedMessagesAreInvisible(t *testing.T) {
	t.Parallel()

	fixture := newPOP3Fixture(t,
		storedMessage{folder: "new", name: "1788000100.M1P2Q1.mail-local-test", body: "From: a\r\n\r\n"},
		storedMessage{folder: "new", name: "1788000200.M1P2Q2.mail-local-test", body: "From: b\r\n\r\n"},
	)
	transcript := fixture.run(t, true, script("DELE 1", "STAT", "RETR 1", "LIST 1", "LIST", "QUIT")...)

	requireLine(t, transcript, "+OK 1 11")
	if count := strings.Count(strings.Join(transcript, "\n"), "-ERR that message is deleted"); count != 2 {
		t.Errorf("got %d 'already deleted' refusals, want 2 (RETR and LIST)", count)
	}
	// Message 2 keeps its number: renumbering mid-session would make a client's
	// pending DELE hit the wrong message.
	requireLine(t, transcript, "2 11")
}

// -----------------------------------------------------------------------------
// Authentication
// -----------------------------------------------------------------------------

// TestPOP3RequiresTLS is the rule the whole credential model rests on.
func TestPOP3RequiresTLS(t *testing.T) {
	t.Parallel()

	fixture := newPOP3Fixture(t)
	transcript := fixture.run(t, false, script("STAT")...)

	if count := strings.Count(strings.Join(transcript, "\n"), "-ERR a mailbox password may only be sent over TLS"); count != 2 {
		t.Errorf("USER and PASS were not both refused on a cleartext connection:\n%s", strings.Join(transcript, "\n"))
	}
	requireLine(t, transcript, "-ERR authenticate first")
}

func TestPOP3RefusesSTLS(t *testing.T) {
	t.Parallel()

	// Implicit TLS only. A STARTTLS-style upgrade on a cleartext port is
	// strippable by anything on the path, and the credential here is a mailbox
	// password.
	transcript := newPOP3Fixture(t).run(t, false, "STLS", "QUIT")
	requireLine(t, transcript, "-ERR this port is implicit TLS; there is no STLS")
}

// TestPOP3AuthenticationFailures checks that every way of getting it wrong
// gives the same sentence. A message that distinguished "no such mailbox" from
// "wrong password" turns the login into an address-book oracle.
func TestPOP3AuthenticationFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		commands []string
	}{
		{name: "wrong password", commands: []string{"USER " + testMailboxAddress, "PASS not the passphrase"}},
		{name: "unknown mailbox", commands: []string{"USER nobody@probe.test", "PASS " + testMailboxPassword}},
		{name: "unknown domain", commands: []string{"USER mara@elsewhere.test", "PASS " + testMailboxPassword}},
		{name: "a local part with no domain", commands: []string{"USER mara", "PASS " + testMailboxPassword}},
		{name: "an empty password", commands: []string{"USER " + testMailboxAddress, "PASS "}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			transcript := newPOP3Fixture(t).run(t, true, append(test.commands, "STAT", "QUIT")...)
			requireLine(t, transcript, "-ERR authentication failed")
			// And the session is still in AUTHORIZATION.
			requireLine(t, transcript, "-ERR authenticate first")
		})
	}
}

func TestPOP3PassBeforeUser(t *testing.T) {
	t.Parallel()

	transcript := newPOP3Fixture(t).run(t, true, "PASS "+testMailboxPassword, "QUIT")
	requireLine(t, transcript, "-ERR send USER first")
}

// TestPOP3RefusesADisabledDomain: a domain the operator turned off stops
// serving mail, and it does so without telling an unauthenticated caller that
// the mailbox exists.
func TestPOP3RefusesADisabledDomain(t *testing.T) {
	t.Parallel()

	fixture := newPOP3Fixture(t)
	domain := fixture.domain
	domain.Enabled = false
	if _, err := fixture.store.PutDomain(context.Background(), domain); err != nil {
		t.Fatalf("PutDomain: %v", err)
	}

	transcript := fixture.run(t, true, script("STAT", "QUIT")...)
	requireLine(t, transcript, "-ERR authentication failed")
}

// TestPOP3AuthenticationCostsTheSameForAnUnknownMailbox pins the reason
// the decoy hash exists: without it, an unknown address returns before the KDF
// runs and the difference is measurable from outside. The hash is delivery.go's
// — SMTP AUTH and POP3 must not disagree about what a failed login costs.
func TestPOP3AuthenticationCostsTheSameForAnUnknownMailbox(t *testing.T) {
	t.Parallel()

	fixture := newPOP3Fixture(t)
	ctx := context.Background()

	// A real login pays the account's own work factor, which this fixture sets
	// low. The decoy pays the production factor, so an unknown mailbox is never
	// the fast path — which is the property that matters. Assert the decoy is
	// at the production factor rather than timing anything, because a timing
	// assertion in a test is a flake.
	if decoyHash().Iterations != DefaultPasswordIterations {
		t.Errorf("the decoy hash runs %d iterations, want %d", decoyHash().Iterations, DefaultPasswordIterations)
	}
	if decoyHash().Verify(testMailboxPassword) {
		t.Error("the decoy hash verified a password")
	}

	if _, _, err := fixture.server.authenticate(ctx, "nobody@probe.test", "x"); err == nil {
		t.Fatal("an unknown mailbox authenticated")
	}
	// The refusal must not name the reason: it is handed to the client.
	_, _, err := fixture.server.authenticate(ctx, testMailboxAddress, "wrong")
	if err == nil {
		t.Fatal("a wrong password authenticated")
	}
	if strings.Contains(err.Error(), "wrong") && strings.Contains(err.Error(), "password") &&
		!strings.Contains(err.Error(), "mailbox") {
		t.Errorf("the refusal %q distinguishes the password from the mailbox", err)
	}
	if strings.Contains(err.Error(), testMailboxPassword) {
		t.Error("the refusal quotes the credential")
	}
}

// -----------------------------------------------------------------------------
// The maildrop lock
// -----------------------------------------------------------------------------

// TestPOP3MaildropLock: RFC 1939 gives one session exclusive access. Two
// sessions over one maildrop would each number the same messages and each
// delete by number.
func TestPOP3MaildropLock(t *testing.T) {
	t.Parallel()

	fixture := newPOP3Fixture(t, storedMessage{folder: "new", name: "1788000100.M1P2Q1.mail-local-test", body: "From: a\r\n\r\n"})

	if !fixture.server.acquire(fixture.account.ID) {
		t.Fatal("the lock was already held")
	}
	transcript := fixture.run(t, true, script("QUIT")...)
	requireLine(t, transcript, "-ERR [IN-USE] the maildrop is locked by another session")

	fixture.server.release(fixture.account.ID)
	transcript = fixture.run(t, true, script("STAT", "QUIT")...)
	requireLine(t, transcript, "+OK 1 11")
}

// TestPOP3LockIsReleasedOnADroppedSession covers the failure that would
// otherwise lock a customer out until a restart.
func TestPOP3LockIsReleasedOnADroppedSession(t *testing.T) {
	t.Parallel()

	fixture := newPOP3Fixture(t)
	// No QUIT: the reader runs dry, which is the client hanging up.
	fixture.run(t, true, login()...)

	if !fixture.server.acquire(fixture.account.ID) {
		t.Fatal("the maildrop is still locked after the session ended")
	}
	fixture.server.release(fixture.account.ID)
}

func TestPOP3LockIsExclusiveUnderConcurrency(t *testing.T) {
	t.Parallel()

	fixture := newPOP3Fixture(t)
	const sessions = 16

	var wait sync.WaitGroup
	var mutex sync.Mutex
	held := 0
	granted := 0

	for range sessions {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if !fixture.server.acquire(fixture.account.ID) {
				return
			}
			mutex.Lock()
			granted++
			held++
			if held > 1 {
				t.Error("two sessions hold the maildrop at once")
			}
			mutex.Unlock()

			time.Sleep(time.Millisecond)

			mutex.Lock()
			held--
			mutex.Unlock()
			fixture.server.release(fixture.account.ID)
		}()
	}
	wait.Wait()
	if granted == 0 {
		t.Fatal("no session acquired the lock")
	}
}

// -----------------------------------------------------------------------------
// Protocol details
// -----------------------------------------------------------------------------

func TestPOP3CapabilitiesAreAvailableBeforeLogin(t *testing.T) {
	t.Parallel()

	transcript := newPOP3Fixture(t).run(t, true, "CAPA", "QUIT")
	for _, capability := range []string{"TOP", "UIDL", "USER", "RESP-CODES", "IMPLEMENTATION maild"} {
		requireLine(t, transcript, capability)
	}
	requireLine(t, transcript, ".")
}

func TestPOP3CommandErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		command string
		want    string
	}{
		{name: "an unknown command", command: "FROB", want: "-ERR unknown command"},
		{name: "a message number that is not a number", command: "RETR x", want: "-ERR the message number is not a number"},
		{name: "a message number out of range", command: "RETR 9", want: "-ERR no such message"},
		{name: "message zero", command: "DELE 0", want: "-ERR no such message"},
		{name: "TOP without a line count", command: "TOP 1", want: "-ERR TOP needs a message number and a line count"},
		{name: "TOP with a bad line count", command: "TOP 1 x", want: "-ERR the line count is not a number"},
		{name: "APOP", command: "APOP a b", want: "-ERR unknown command"},
	}

	fixture := newPOP3Fixture(t, storedMessage{folder: "new", name: "1788000100.M1P2Q1.mail-local-test", body: "From: a\r\n\r\n"})
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			requireLine(t, fixture.run(t, true, script(test.command, "QUIT")...), test.want)
		})
	}
}

func TestPOP3APOPIsRefusedBeforeLogin(t *testing.T) {
	t.Parallel()

	// APOP needs the password recoverable to compute its digest, and the store
	// holds only a PBKDF2 digest. Refusing is the honest answer.
	transcript := newPOP3Fixture(t).run(t, true, "APOP mara deadbeef", "QUIT")
	requireLine(t, transcript, "-ERR APOP is not supported; use USER and PASS over TLS")
}

func TestPOP3NOOP(t *testing.T) {
	t.Parallel()

	fixture := newPOP3Fixture(t)
	requireLine(t, fixture.run(t, true, "NOOP", "QUIT"), "-ERR authenticate first")
	requireLine(t, fixture.run(t, true, script("NOOP", "QUIT")...), "+OK")
}

func TestPOP3RefusesAnOverlongCommand(t *testing.T) {
	t.Parallel()

	fixture := newPOP3Fixture(t)
	transcript := fixture.run(t, true, "USER "+strings.Repeat("a", 4096))
	requireLine(t, transcript, "-ERR the command line is too long")
}

func TestPOP3QuitFromAuthorization(t *testing.T) {
	t.Parallel()

	transcript := newPOP3Fixture(t).run(t, true, "QUIT")
	requireLine(t, transcript, "+OK bye")
}

// TestPOP3TranscriptCarriesNoCredential is the blunt instrument: whatever else
// changes, the password must never appear in anything the server writes.
func TestPOP3TranscriptCarriesNoCredential(t *testing.T) {
	t.Parallel()

	fixture := newPOP3Fixture(t, storedMessage{folder: "new", name: "1788000100.M1P2Q1.mail-local-test", body: "From: a\r\n\r\n"})
	transcript := strings.Join(fixture.run(t, true, script("STAT", "LIST", "RETR 1", "QUIT")...), "\n")
	if strings.Contains(transcript, testMailboxPassword) {
		t.Error("the server echoed the mailbox password")
	}
	if strings.Contains(transcript, string(fixture.account.Password.Digest.Bytes())) {
		t.Error("the server echoed the credential digest")
	}
}

// TestPOP3ServeConnOverAPipe drives the connection path end to end — the
// deadline wrapper, the TLS detection and the greeting — over net.Pipe, which
// is an in-memory connection, so the test opens no socket. A pipe is not a
// *tls.Conn, so this also covers the case that matters most: the server offers
// the protocol on a cleartext connection but not the credential.
func TestPOP3ServeConnOverAPipe(t *testing.T) {
	t.Parallel()

	fixture := newPOP3Fixture(t)
	client, server := net.Pipe()

	done := make(chan struct{})
	go func() {
		defer close(done)
		fixture.server.ServeConn(context.Background(), server)
	}()

	reader := bufio.NewReader(client)
	greeting, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read the greeting: %v", err)
	}
	if greeting != "+OK mail.local.test POP3 ready\r\n" {
		t.Errorf("greeting = %q", greeting)
	}

	for _, exchange := range []struct{ send, want string }{
		{send: "USER " + testMailboxAddress + "\r\n", want: "-ERR a mailbox password may only be sent over TLS\r\n"},
		{send: "QUIT\r\n", want: "+OK bye\r\n"},
	} {
		if _, err := io.WriteString(client, exchange.send); err != nil {
			t.Fatalf("write %q: %v", exchange.send, err)
		}
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read the reply to %q: %v", exchange.send, err)
		}
		if line != exchange.want {
			t.Errorf("reply to %q = %q, want %q", exchange.send, line, exchange.want)
		}
	}

	if err := client.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the session did not end after QUIT")
	}
}

func TestPOP3Options(t *testing.T) {
	t.Parallel()

	spool := NewMaildir(filepath.Join(t.TempDir(), "mail"))
	if spool.Root() == "" {
		t.Error("Root is empty")
	}
	server := NewPOP3Server(nil, spool,
		POP3WithHostname("  "),
		POP3WithLogger(nil),
		POP3WithTLS(&tls.Config{MinVersion: tls.VersionTLS12}),
		POP3WithIdleTimeout(0),
		POP3WithIdleTimeout(time.Minute),
	)
	if server.hostname != "localhost" {
		t.Errorf("a blank hostname replaced the default: %q", server.hostname)
	}
	if server.logger == nil {
		t.Error("a nil logger replaced the default")
	}
	if server.tls == nil {
		t.Error("the TLS configuration was dropped")
	}
	if server.idle != time.Minute {
		t.Errorf("idle timeout = %v, want a minute (a non-positive one is ignored)", server.idle)
	}
}

func TestPOP3UIDLAndListForOneMessage(t *testing.T) {
	t.Parallel()

	fixture := newPOP3Fixture(t, storedMessage{folder: "new", name: "1788000100.M1P2Q1.mail-local-test", body: "From: a\r\n\r\n"})
	transcript := fixture.run(t, true, script("UIDL 1", "LIST 1", "UIDL 9", "QUIT")...)

	requireLine(t, transcript, "+OK 1 "+maildirUID("1788000100.M1P2Q1.mail-local-test"))
	requireLine(t, transcript, "+OK 1 11")
	requireLine(t, transcript, "-ERR no such message")
}

// TestPOP3RetrievesAVanishedMessage: the session's view of the maildrop is
// taken at login, so a message removed underneath it must be refused rather
// than sent short — a truncated transfer would be filed by the client as a
// complete message. The session is built directly here because the removal has
// to happen after login and before the RETR.
func TestPOP3RetrievesAVanishedMessage(t *testing.T) {
	t.Parallel()

	const name = "1788000100.M1P2Q1.mail-local-test"
	fixture := newPOP3Fixture(t, storedMessage{folder: "new", name: name, body: "From: a\r\n\r\n"})

	messages, err := fixture.spool.Messages(fixture.account.ID)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("got %d messages, want 1", len(messages))
	}

	var output strings.Builder
	session := &pop3Session{
		server:   fixture.server,
		writer:   bufio.NewWriter(&output),
		secure:   true,
		account:  fixture.account,
		domain:   fixture.domain,
		messages: []pop3Entry{{MaildirMessage: messages[0]}},
	}

	if err := os.Remove(filepath.Join(fixture.spool.Path(fixture.account.ID), "new", name)); err != nil {
		t.Fatalf("remove the message: %v", err)
	}
	session.retr("1")
	session.top("1 1")
	if err := session.writer.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	transcript := splitTranscript(output.String())
	if count := strings.Count(strings.Join(transcript, "\n"), "-ERR the message could not be read"); count != 2 {
		t.Errorf("RETR and TOP did not both refuse:\n%s", strings.Join(transcript, "\n"))
	}
}

// -----------------------------------------------------------------------------
// Admission control
//
// Port 995 takes a mailbox password from anyone who can reach it, and checking
// one costs a PBKDF2 verification — the real one, or the decoy that makes an
// unknown mailbox cost the same. That makes an unauthenticated guess both a
// brute-force attempt and a way to buy this server's CPU, so the refusals below
// all have to be made before any of that work happens.
// -----------------------------------------------------------------------------

// limitedPOP3Server rebuilds the fixture's server with a limiter, keeping the
// same store and spool so the mailbox and its credential are the real ones.
func (f *pop3Fixture) limitedServer(limiter *Limiter) *POP3Server {
	return NewPOP3Server(f.store, f.spool,
		POP3WithHostname("mail.local.test"),
		POP3WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		POP3WithLimiter(limiter),
	)
}

const pop3LockedOut = "-ERR [SYS/TEMP] too many failed authentication attempts, try again later"

// TestPOP3LocksOutRepeatedPasswordGuesses.
//
// The decisive assertion is that the CORRECT password is refused while the
// lockout stands. That can only be true if the refusal is made before the
// credential is verified, which is the whole point: a guessing peer must cost
// this server a map lookup, not tens of milliseconds of a core per attempt.
func TestPOP3LocksOutRepeatedPasswordGuesses(t *testing.T) {
	t.Parallel()

	fixture := newPOP3Fixture(t)
	clock := newTestLimiterClock()
	limiter := NewLimiter(LimiterConfig{
		AuthFailures:   2,
		AuthLockout:    time.Minute,
		AuthLockoutMax: time.Minute,
		Now:            clock.Now,
	})
	server := fixture.limitedServer(limiter)

	for attempt := range 2 {
		transcript := fixture.runWith(t, server, true,
			"USER "+testMailboxAddress, "PASS not the passphrase", "QUIT")
		if attempt < 2 {
			requireLine(t, transcript, "-ERR authentication failed")
		}
	}

	locked := fixture.runWith(t, server, true,
		"USER "+testMailboxAddress, "PASS "+testMailboxPassword, "STAT", "QUIT")
	requireLine(t, locked, pop3LockedOut)
	// And the session is still in AUTHORIZATION: a lockout that let the
	// maildrop open would be worse than none.
	requireLine(t, locked, "-ERR authenticate first")
	if strings.Contains(strings.Join(locked, "\n"), testMailboxPassword) {
		t.Error("the transcript quotes the credential")
	}

	// The peer is locked out, not the mailbox alone: the same source guessing a
	// different address gets the same answer, so it cannot use one lockout to
	// map the address space around it.
	other := fixture.runWith(t, server, true,
		"USER nobody@probe.test", "PASS "+testMailboxPassword, "QUIT")
	requireLine(t, other, pop3LockedOut)

	// Nothing is locked forever. An account lockout is also a way to deny a
	// customer their own mail, so it has to expire on its own.
	clock.Advance(time.Minute)
	recovered := fixture.runWith(t, server, true, script("STAT", "QUIT")...)
	requireLine(t, recovered, "+OK 0 0")
}

// TestPOP3LoginSuccessClearsTheFailureCount: a customer whose client retried a
// stale password before being corrected must not spend the next quarter of an
// hour one mistake from a lockout.
func TestPOP3LoginSuccessClearsTheFailureCount(t *testing.T) {
	t.Parallel()

	fixture := newPOP3Fixture(t)
	limiter := NewLimiter(LimiterConfig{AuthFailures: 2, AuthLockout: time.Minute})
	server := fixture.limitedServer(limiter)

	requireLine(t, fixture.runWith(t, server, true,
		"USER "+testMailboxAddress, "PASS stale", "QUIT"), "-ERR authentication failed")
	requireLine(t, fixture.runWith(t, server, true, script("QUIT")...), "+OK bye")
	if keys := limiter.Stats().TrackedAuthKeys; keys != 0 {
		t.Fatalf("a successful login left %d failure counters standing", keys)
	}

	// So the next mistake starts from zero rather than from one.
	requireLine(t, fixture.runWith(t, server, true,
		"USER "+testMailboxAddress, "PASS stale", "QUIT"), "-ERR authentication failed")
	requireLine(t, fixture.runWith(t, server, true, script("STAT", "QUIT")...), "+OK 0 0")
}

// TestPOP3RefusesConnectionsOverTheLimits: without a per-address share of the
// connection count, one peer opening sockets and saying nothing takes every
// slot, and every customer's mail client is refused while the server looks
// perfectly healthy.
func TestPOP3RefusesConnectionsOverTheLimits(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		config LimiterConfig
	}{
		{name: "the global limit", config: LimiterConfig{MaxSessions: 1, MaxSessionsPerIP: 1}},
		{name: "one peer's share of it", config: LimiterConfig{MaxSessions: 8, MaxSessionsPerIP: 1}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			fixture := newPOP3Fixture(t)
			server := fixture.limitedServer(NewLimiter(test.config))

			first := dialPOP3(t, server)
			if greeting := first.line(t); greeting != "+OK mail.local.test POP3 ready\r\n" {
				t.Fatalf("greeting = %q", greeting)
			}

			second := dialPOP3(t, server)
			if refusal := second.line(t); refusal != "-ERR [SYS/TEMP] too many connections, try again later\r\n" {
				t.Fatalf("the session over the limit got %q", refusal)
			}
			second.close(t)

			// The session already in progress is untouched, and ending it frees
			// the slot again — a slot that is never returned is a slot lost for
			// the life of the process.
			first.send(t, "QUIT")
			if bye := first.line(t); bye != "+OK bye\r\n" {
				t.Fatalf("QUIT = %q", bye)
			}
			first.close(t)

			third := dialPOP3(t, server)
			if greeting := third.line(t); greeting != "+OK mail.local.test POP3 ready\r\n" {
				t.Fatalf("the freed slot was not reused: %q", greeting)
			}
			third.close(t)
		})
	}
}

// pop3Conn is one live connection to a POP3Server: the client end of a pipe,
// one reader over it for the whole session, and the session's own goroutine.
type pop3Conn struct {
	conn   net.Conn
	reader *bufio.Reader
	done   chan struct{}
}

// dialPOP3 runs one ServeConn over a pipe and hands back the client end.
func dialPOP3(t *testing.T, server *POP3Server) *pop3Conn {
	t.Helper()
	client, peer := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		server.ServeConn(context.Background(), peer)
	}()
	return &pop3Conn{conn: client, reader: bufio.NewReader(client), done: done}
}

func (c *pop3Conn) send(t *testing.T, command string) {
	t.Helper()
	if err := c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set a deadline: %v", err)
	}
	if _, err := io.WriteString(c.conn, command+"\r\n"); err != nil {
		t.Fatalf("write %q: %v", command, err)
	}
}

func (c *pop3Conn) line(t *testing.T) string {
	t.Helper()
	if err := c.conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set a deadline: %v", err)
	}
	line, err := c.reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read a line: %v", err)
	}
	return line
}

// close hangs up and waits for the session to finish, so the slot it held has
// certainly been released before the next assertion looks at the count.
func (c *pop3Conn) close(t *testing.T) {
	t.Helper()
	if err := c.conn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	select {
	case <-c.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the session did not end after the client went away")
	}
}
