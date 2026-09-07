package maild

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The point of every test here is the same: a message this server accepted must
// either be delivered or be reported back to the sender. Silence is the bug.
//
// Nothing reaches the network. Delivery goes to a fakeMX on a loopback listener
// (relay_test.go) and every domain is resolved by an injected Resolver, so these
// are real SMTP conversations against a fake peer rather than mocked ones.

const testQueuedBody = "Received: from probe.test by mail.probe.test; Fri, 4 Sep 2026 09:00:00 +0000\r\n" +
	"From: <sender@probe.test>\r\n" +
	"To: <someone@example.com>\r\n" +
	"Message-ID: <original@probe.test>\r\n" +
	"Subject: a forwarded message\r\n" +
	"\r\n" +
	"the body\r\n"

// stepClock is the injected clock. It moves only when a test moves it, so a
// five-day lifetime expires in a microsecond and no test sleeps.
type stepClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *stepClock) Advance(delta time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(delta)
}

// quietLogger keeps the deliberate warnings and errors these tests provoke out
// of the test output.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

// logRecorder captures what the runner logged.
//
// It exists for one assertion: a dropped bounce must be dropped BECAUSE the
// envelope sender was null, not because some later check happened to refuse it
// as well. Pinning the reason is what stops the null-sender guard from being
// deleted as redundant — after which the next change to the return path reopens
// the mail loop.
type logRecorder struct {
	mu       sync.Mutex
	messages []string
}

func (h *logRecorder) Enabled(context.Context, slog.Level) bool { return true }

func (h *logRecorder) Handle(_ context.Context, record slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.messages = append(h.messages, record.Message)
	return nil
}

func (h *logRecorder) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *logRecorder) WithGroup(string) slog.Handler { return h }

func (h *logRecorder) logged(substring string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, message := range h.messages {
		if strings.Contains(message, substring) {
			return true
		}
	}
	return false
}

type queueFixture struct {
	t     *testing.T
	store *Store
	queue *Queue
	host  *fakeMX
	clock *stepClock
	spool string
}

// newQueueFixture wires a real store, a real relay and a real receiving server.
// configure adjusts the queue's own policy — and may swap the Sender, which is
// what the concurrency and shutdown tests need.
func newQueueFixture(t *testing.T, configure func(*QueueConfig)) *queueFixture {
	t.Helper()
	root := t.TempDir()
	spool := filepath.Join(root, SpoolDir)
	if err := os.MkdirAll(spool, mailDirPerm); err != nil {
		t.Fatalf("create the spool directory: %v", err)
	}
	clock := &stepClock{now: time.Date(2026, 9, 4, 9, 0, 0, 0, time.UTC)}
	store, err := Open(filepath.Join(root, "maild.db"), WithClock(clock.Now))
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close the store: %v", err)
		}
	})

	host := newFakeMX(t, nil)
	relay := newTestRelay(t, RelayConfig{
		Resolver:    fixedResolver("mx.example.com"),
		DialContext: routedDialer(map[string]string{"mx.example.com": host.address()}),
	})
	config := QueueConfig{
		SpoolDir: spool,
		Hostname: "mail.probe.test",
		Sender:   relay,
		Logger:   quietLogger(),
		Now:      clock.Now,
	}
	if configure != nil {
		configure(&config)
	}
	queue, err := NewQueue(store, config)
	if err != nil {
		t.Fatalf("build the queue runner: %v", err)
	}
	return &queueFixture{t: t, store: store, queue: queue, host: host, clock: clock, spool: spool}
}

// enqueue spools a body and writes the queue entry, exactly as the delivery path
// does for a recipient this server does not host.
func (f *queueFixture) enqueue(returnPath string, recipients ...string) QueuedMessage {
	f.t.Helper()
	id, err := randomID("q")
	if err != nil {
		f.t.Fatalf("mint an id: %v", err)
	}
	name := id + ".eml"
	body := []byte(testQueuedBody)
	if err := writeThenRename(
		filepath.Join(f.spool, maildirTmp, name),
		filepath.Join(f.spool, name),
		body,
	); err != nil {
		f.t.Fatalf("spool the body: %v", err)
	}
	now := f.clock.Now()
	message := QueuedMessage{
		V:           DocVersion,
		ID:          id,
		ReturnPath:  returnPath,
		MessagePath: name,
		SizeBytes:   int64(len(body)),
		CreatedAt:   now,
		NextRetry:   now,
	}
	for _, recipient := range recipients {
		message.Recipients = append(message.Recipients, QueuedRecipient{
			Address:   recipient,
			Status:    QueueStatusScheduled,
			QueueName: remoteQueueName,
			RetryDue:  now,
		})
	}
	stored, err := f.store.PutQueuedMessage(context.Background(), message)
	if err != nil {
		f.t.Fatalf("queue the message: %v", err)
	}
	return stored
}

func (f *queueFixture) run() {
	f.t.Helper()
	if err := f.queue.RunOnce(context.Background()); err != nil {
		f.t.Fatalf("run the queue: %v", err)
	}
}

func (f *queueFixture) messages() []QueuedMessage {
	f.t.Helper()
	messages, err := f.store.ListQueuedMessages(context.Background(), 0)
	if err != nil {
		f.t.Fatalf("list the queue: %v", err)
	}
	return messages
}

func (f *queueFixture) get(id string) (QueuedMessage, bool) {
	f.t.Helper()
	message, err := f.store.GetQueuedMessage(context.Background(), id)
	if errors.Is(err, ErrNotFound) {
		return QueuedMessage{}, false
	}
	if err != nil {
		f.t.Fatalf("read a queued message: %v", err)
	}
	return message, true
}

func (f *queueFixture) spoolBody(message QueuedMessage) string {
	f.t.Helper()
	body, err := os.ReadFile(filepath.Join(f.spool, message.MessagePath))
	if err != nil {
		f.t.Fatalf("read a spooled body: %v", err)
	}
	return string(body)
}

// only returns the queue's single entry, failing if there is not exactly one.
func (f *queueFixture) only() QueuedMessage {
	f.t.Helper()
	messages := f.messages()
	if len(messages) != 1 {
		f.t.Fatalf("want one queued message, got %d", len(messages))
	}
	return messages[0]
}

// -----------------------------------------------------------------------------
// Delivery
// -----------------------------------------------------------------------------

// The defect this whole file exists for: a forwarder target on another server
// was answered 250, spooled, and then never sent.
func TestQueueDeliversAMessageToAnExternalTarget(t *testing.T) {
	t.Parallel()
	fixture := newQueueFixture(t, nil)
	message := fixture.enqueue("sender@probe.test", "someone@example.com")

	fixture.run()

	delivered := fixture.host.received()
	if len(delivered) != 1 {
		t.Fatalf("want one delivered message, got %d", len(delivered))
	}
	if delivered[0].From != "sender@probe.test" {
		t.Errorf("envelope sender = %q", delivered[0].From)
	}
	if len(delivered[0].To) != 1 || delivered[0].To[0] != "someone@example.com" {
		t.Errorf("envelope recipients = %v", delivered[0].To)
	}
	if delivered[0].Body != testQueuedBody {
		t.Errorf("the delivered body is not the spooled one:\n%q", delivered[0].Body)
	}
	if _, found := fixture.get(message.ID); found {
		t.Error("the queue entry survived a completed delivery")
	}
	if _, err := os.Stat(filepath.Join(fixture.spool, message.MessagePath)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the spooled body survived a completed delivery: %v", err)
	}
}

// A message that is not due must not be touched, or a 4xx would turn into a
// retry loop at whatever rate the runner happens to poll.
func TestQueueLeavesAMessageThatIsNotDueYetAlone(t *testing.T) {
	t.Parallel()
	fixture := newQueueFixture(t, nil)
	message := fixture.enqueue("sender@probe.test", "someone@example.com")
	message.NextRetry = fixture.clock.Now().Add(time.Hour)
	if _, err := fixture.store.PutQueuedMessage(context.Background(), message); err != nil {
		t.Fatalf("defer the message: %v", err)
	}

	fixture.run()

	if got := fixture.host.sessionCount(); got != 0 {
		t.Errorf("sessions = %d, want none before the message is due", got)
	}
	if _, found := fixture.get(message.ID); !found {
		t.Error("the queue entry was removed before it was ever attempted")
	}
}

// -----------------------------------------------------------------------------
// Retry
// -----------------------------------------------------------------------------

func TestQueueRetriesAfterATemporaryFailureAndThenDelivers(t *testing.T) {
	t.Parallel()
	fixture := newQueueFixture(t, func(config *QueueConfig) {
		config.BaseBackoff = 5 * time.Minute
	})
	// The first RCPT is deferred; every one after it is accepted.
	fixture.host.script("RCPT", "451 4.3.2 Come back later", "250 2.1.5 Ok")
	message := fixture.enqueue("sender@probe.test", "someone@example.com")

	fixture.run()

	deferred, found := fixture.get(message.ID)
	if !found {
		t.Fatal("a temporary failure destroyed the queue entry")
	}
	recipient := deferred.Recipients[0]
	if recipient.Status != QueueStatusTemporaryFailure {
		t.Errorf("status = %q, want %q", recipient.Status, QueueStatusTemporaryFailure)
	}
	if recipient.RetryCount != 1 {
		t.Errorf("retry count = %d, want 1", recipient.RetryCount)
	}
	if recipient.LastCode != 451 {
		t.Errorf("last code = %d, want 451", recipient.LastCode)
	}
	wantDue := fixture.clock.Now().Add(5 * time.Minute)
	if !recipient.RetryDue.Equal(wantDue) {
		t.Errorf("retry due = %v, want %v", recipient.RetryDue, wantDue)
	}
	if !deferred.NextRetry.Equal(wantDue) {
		t.Errorf("next retry = %v, want %v", deferred.NextRetry, wantDue)
	}
	if got := len(fixture.host.received()); got != 0 {
		t.Fatalf("a deferred message was delivered anyway (%d)", got)
	}

	// A pass before the backoff has elapsed must not open a second session.
	fixture.clock.Advance(4 * time.Minute)
	fixture.run()
	if got := fixture.host.sessionCount(); got != 1 {
		t.Fatalf("sessions = %d, want 1: the backoff was not respected", got)
	}

	fixture.clock.Advance(time.Minute)
	fixture.run()
	if got := len(fixture.host.received()); got != 1 {
		t.Fatalf("want the message delivered on the retry, got %d", got)
	}
	if _, found := fixture.get(message.ID); found {
		t.Error("the queue entry survived a completed delivery")
	}
}

// Partial progress must be kept, or the retry delivers a second copy to the
// recipient that already had one.
func TestQueueKeepsARecipientThatSucceededWhileAnotherIsRetried(t *testing.T) {
	t.Parallel()
	fixture := newQueueFixture(t, nil)
	// The two recipients are in different domains, so they get one session
	// each and the script lines up one reply per session: the first is taken,
	// the second is deferred, and the retry of the second is taken.
	fixture.host.script("RCPT", "250 2.1.5 Ok", "451 4.3.2 Come back later", "250 2.1.5 Ok")
	message := fixture.enqueue("sender@probe.test", "first@a-example.com", "second@b-example.com")

	fixture.run()

	stored, found := fixture.get(message.ID)
	if !found {
		t.Fatal("the queue entry was destroyed while a recipient was still pending")
	}
	if stored.Recipients[0].Status != QueueStatusCompleted {
		t.Errorf("first recipient = %q, want %q", stored.Recipients[0].Status, QueueStatusCompleted)
	}
	if stored.Recipients[1].Status != QueueStatusTemporaryFailure {
		t.Errorf("second recipient = %q, want %q", stored.Recipients[1].Status, QueueStatusTemporaryFailure)
	}

	// The retry must offer the message only to the recipient that is still
	// pending.
	fixture.clock.Advance(DefaultQueueBaseBackoff)
	fixture.run()
	delivered := fixture.host.received()
	if len(delivered) != 2 {
		t.Fatalf("want two delivered messages, got %d", len(delivered))
	}
	for _, message := range delivered {
		if len(message.To) != 1 {
			t.Fatalf("a delivery carried %d recipients, want 1", len(message.To))
		}
	}
	if delivered[0].To[0] != "first@a-example.com" || delivered[1].To[0] != "second@b-example.com" {
		t.Errorf("deliveries = %v then %v, want each recipient exactly once",
			delivered[0].To, delivered[1].To)
	}
}

func TestQueueBackoffDoublesFromTheBaseAndStopsAtTheCap(t *testing.T) {
	t.Parallel()
	queue, err := NewQueue(newQueueFixture(t, nil).store, QueueConfig{
		SpoolDir:    t.TempDir(),
		Sender:      &gatedSender{},
		BaseBackoff: time.Minute,
		MaxBackoff:  8 * time.Minute,
		Logger:      quietLogger(),
	})
	if err != nil {
		t.Fatalf("build the queue runner: %v", err)
	}
	tests := []struct {
		retries uint32
		want    time.Duration
	}{
		{0, time.Minute},
		{1, time.Minute},
		{2, 2 * time.Minute},
		{3, 4 * time.Minute},
		{4, 8 * time.Minute},
		{5, 8 * time.Minute},
		// A retry count read off a corrupted record must still produce the cap,
		// not a short delay and a tight loop against someone else's server.
		{1 << 20, 8 * time.Minute},
		{^uint32(0), 8 * time.Minute},
	}
	for _, test := range tests {
		if got := queue.backoff(test.retries); got != test.want {
			t.Errorf("backoff(%d) = %v, want %v", test.retries, got, test.want)
		}
	}
}

// -----------------------------------------------------------------------------
// Bounces
// -----------------------------------------------------------------------------

func TestQueueBouncesAPermanentFailureToTheSender(t *testing.T) {
	t.Parallel()
	fixture := newQueueFixture(t, nil)
	// The forwarded message is refused; the bounce that follows it is taken,
	// because the script's last entry is the one that repeats.
	fixture.host.script("RCPT", "550 5.1.1 No such user here", "250 2.1.5 Ok")
	original := fixture.enqueue("sender@probe.test", "someone@example.com")

	fixture.run()

	if _, found := fixture.get(original.ID); found {
		t.Error("the failed message stayed in the queue")
	}
	bounce := fixture.only()
	if bounce.ReturnPath != "" {
		t.Errorf("the bounce has envelope sender %q, want the null sender", bounce.ReturnPath)
	}
	if len(bounce.Recipients) != 1 || bounce.Recipients[0].Address != "sender@probe.test" {
		t.Fatalf("the bounce is addressed to %+v", bounce.Recipients)
	}

	body := fixture.spoolBody(bounce)
	for _, want := range []string{
		"multipart/report; report-type=delivery-status",
		"Auto-Submitted: auto-replied",
		"Content-Type: message/delivery-status",
		"Final-Recipient: rfc822; someone@example.com",
		"Action: failed",
		"Status: 5.1.1",
		"Diagnostic-Code: smtp; 550 5.1.1 No such user here",
		// The headers of the failed message, so the sender can tell which one
		// it was.
		"Message-ID: <original@probe.test>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the bounce is missing %q:\n%s", want, body)
		}
	}
	// Headers only: a bounce that quoted the body would be as large as the
	// message that failed.
	if strings.Contains(body, "the body") {
		t.Error("the bounce quotes the failed message's body")
	}

	// And the bounce is itself delivered.
	fixture.run()
	delivered := fixture.host.received()
	if len(delivered) != 1 {
		t.Fatalf("want the bounce delivered, got %d messages", len(delivered))
	}
	if delivered[0].From != "" {
		t.Errorf("the bounce was sent from %q, want the null sender", delivered[0].From)
	}
	if len(delivered[0].To) != 1 || delivered[0].To[0] != "sender@probe.test" {
		t.Errorf("the bounce went to %v", delivered[0].To)
	}
}

func TestQueueBouncesWhenTheLifetimeExpires(t *testing.T) {
	t.Parallel()
	fixture := newQueueFixture(t, func(config *QueueConfig) {
		config.Lifetime = 48 * time.Hour
		config.BaseBackoff = time.Hour
		config.MaxBackoff = time.Hour
	})
	fixture.host.script("RCPT", "451 4.3.2 Come back later")
	original := fixture.enqueue("sender@probe.test", "someone@example.com")

	fixture.run()
	if _, found := fixture.get(original.ID); !found {
		t.Fatal("a temporary failure destroyed the queue entry")
	}

	fixture.clock.Advance(48 * time.Hour)
	fixture.run()

	if _, found := fixture.get(original.ID); found {
		t.Error("an expired message stayed in the queue")
	}
	if got := fixture.host.sessionCount(); got != 1 {
		t.Errorf("sessions = %d, want 1: an expired message must not be attempted again", got)
	}
	bounce := fixture.only()
	if bounce.ReturnPath != "" {
		t.Errorf("the bounce has envelope sender %q, want the null sender", bounce.ReturnPath)
	}
	body := fixture.spoolBody(bounce)
	// RFC 3464 §2.3.4: an expiry is reported as a failure carrying the
	// temporary status of the attempts that ran out of time.
	for _, want := range []string{"Action: failed", "Status: 4.4.7", "Delivery time expired"} {
		if !strings.Contains(body, want) {
			t.Errorf("the expiry bounce is missing %q:\n%s", want, body)
		}
	}
}

// The rule that keeps two mail servers from talking to each other forever.
func TestQueueNeverBouncesABounce(t *testing.T) {
	t.Parallel()
	recorder := &logRecorder{}
	fixture := newQueueFixture(t, func(config *QueueConfig) {
		config.Logger = slog.New(recorder)
	})
	fixture.host.script("RCPT", "550 5.1.1 No such user here")
	// A message with the null envelope sender is itself a delivery report.
	original := fixture.enqueue("", "someone@example.com")

	fixture.run()

	if _, found := fixture.get(original.ID); found {
		t.Error("the failed bounce stayed in the queue")
	}
	if messages := fixture.messages(); len(messages) != 0 {
		t.Fatalf("a bounce was generated for a bounce: %+v", messages)
	}
	// The reason, not just the outcome: the null envelope sender is what
	// refused this, and it must be the check that fired.
	if !recorder.logged("a bounce could not be delivered and was dropped") {
		t.Error("a dropped bounce left no trace an operator could find")
	}
	if recorder.logged("its return path is not an address") {
		t.Error("the null sender was refused by the address parser rather than by the loop guard")
	}
}

// An expiring bounce is the other half of the same rule: the loop can start at
// either end.
func TestQueueNeverBouncesABounceThatExpires(t *testing.T) {
	t.Parallel()
	fixture := newQueueFixture(t, func(config *QueueConfig) {
		config.Lifetime = time.Hour
	})
	fixture.host.script("RCPT", "451 4.3.2 Come back later")
	original := fixture.enqueue("", "someone@example.com")

	fixture.run()
	fixture.clock.Advance(2 * time.Hour)
	fixture.run()

	if _, found := fixture.get(original.ID); found {
		t.Error("the expired bounce stayed in the queue")
	}
	if messages := fixture.messages(); len(messages) != 0 {
		t.Fatalf("a bounce was generated for an expired bounce: %+v", messages)
	}
}

// A body that is gone can never be delivered, so retrying over it forever is the
// silent failure this runner exists to remove.
func TestQueueBouncesAMessageWhoseSpooledBodyIsGone(t *testing.T) {
	t.Parallel()
	fixture := newQueueFixture(t, nil)
	original := fixture.enqueue("sender@probe.test", "someone@example.com")
	if err := os.Remove(filepath.Join(fixture.spool, original.MessagePath)); err != nil {
		t.Fatalf("remove the spooled body: %v", err)
	}

	fixture.run()

	if _, found := fixture.get(original.ID); found {
		t.Error("a message with no body stayed in the queue")
	}
	if got := fixture.host.sessionCount(); got != 0 {
		t.Errorf("sessions = %d, want none: there was nothing to send", got)
	}
	bounce := fixture.only()
	if !strings.Contains(fixture.spoolBody(bounce), "Action: failed") {
		t.Error("the bounce does not report a failure")
	}
}

// A stored address is untrusted: it reaches an SMTP command and a bounce header.
func TestQueueRefusesAnUnusableRecipientAndCannotBeInjectedThroughIt(t *testing.T) {
	t.Parallel()
	fixture := newQueueFixture(t, nil)
	original := fixture.enqueue("sender@probe.test", "victim@example.com\r\nX-Injected: yes")

	fixture.run()

	if got := fixture.host.sessionCount(); got != 0 {
		t.Errorf("sessions = %d, want none: the address never had to be dialled", got)
	}
	if _, found := fixture.get(original.ID); found {
		t.Error("a message with an unusable recipient stayed in the queue")
	}
	body := fixture.spoolBody(fixture.only())
	for _, line := range strings.Split(body, "\r\n") {
		if strings.HasPrefix(line, "X-Injected:") {
			t.Fatalf("the recipient injected a header into the bounce:\n%s", body)
		}
	}
	if !strings.Contains(body, "Status: 5.1.3") {
		t.Errorf("the bounce does not report an invalid address:\n%s", body)
	}
}

// The bounce is written before the queue entry is updated, so a failure to
// persist the update cannot lose it. This pins the direction of that trade.
func TestQueueWritesTheBounceBeforeRetiringTheMessage(t *testing.T) {
	t.Parallel()
	fixture := newQueueFixture(t, nil)
	fixture.host.script("RCPT", "550 5.1.1 No such user here")
	original := fixture.enqueue("sender@probe.test", "someone@example.com")

	fixture.run()

	messages := fixture.messages()
	if len(messages) != 1 || messages[0].ID == original.ID {
		t.Fatalf("want exactly the bounce left in the queue, got %+v", messages)
	}
	if messages[0].CreatedAt.Before(original.CreatedAt) {
		t.Error("the bounce predates the message it reports on")
	}
}

// -----------------------------------------------------------------------------
// Concurrency and shutdown
// -----------------------------------------------------------------------------

// gatedSender blocks inside Send until it is released, which is what lets a test
// observe how many outbound sessions the runner allows at once without a sleep.
type gatedSender struct {
	entered chan struct{}
	release chan struct{}

	mu   sync.Mutex
	live int
	peak int
	sent int
}

func (s *gatedSender) Send(_ context.Context, _, _ string, to []string, _ []byte) []RelayReport {
	s.mu.Lock()
	s.live++
	s.sent++
	if s.live > s.peak {
		s.peak = s.live
	}
	s.mu.Unlock()

	if s.entered != nil {
		s.entered <- struct{}{}
	}
	if s.release != nil {
		<-s.release
	}

	s.mu.Lock()
	s.live--
	s.mu.Unlock()

	reports := make([]RelayReport, len(to))
	for i, address := range to {
		reports[i] = RelayReport{Address: address}
	}
	return reports
}

func (s *gatedSender) peakConcurrency() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.peak
}

func (s *gatedSender) sendCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sent
}

// The cap has to be real: without it a queue that drained after an outage would
// open one outbound socket per queued message.
func TestQueueCapsConcurrentOutboundSessions(t *testing.T) {
	t.Parallel()
	sender := &gatedSender{
		entered: make(chan struct{}, 16),
		release: make(chan struct{}),
	}
	fixture := newQueueFixture(t, func(config *QueueConfig) {
		config.Sender = sender
		config.MaxConcurrent = 2
	})
	for i := 0; i < 6; i++ {
		fixture.enqueue("sender@probe.test", "someone@example.com")
	}

	done := make(chan error, 1)
	go func() { done <- fixture.queue.RunOnce(context.Background()) }()

	// Two senders get in; a third must not, however long it is given. This is
	// the one negative assertion in the file, so the window is generous: the
	// remaining four goroutines have long since been dispatched by then, and an
	// unbounded runner would have let every one of them through.
	<-sender.entered
	<-sender.entered
	select {
	case <-sender.entered:
		t.Fatal("a third outbound session opened while two were already open")
	case <-time.After(250 * time.Millisecond):
	}
	close(sender.release)

	if err := <-done; err != nil {
		t.Fatalf("run the queue: %v", err)
	}
	if got := sender.sendCount(); got != 6 {
		t.Errorf("sends = %d, want all six messages attempted", got)
	}
	if got := sender.peakConcurrency(); got != 2 {
		t.Errorf("peak concurrency = %d, want exactly the cap of 2", got)
	}
}

// A session that is halfway through DATA has already told the far end a message
// is coming. Abandoning it there is how a message gets delivered twice.
func TestQueueShutdownDrainsWorkThatIsAlreadyInFlight(t *testing.T) {
	t.Parallel()
	sender := &gatedSender{
		entered: make(chan struct{}, 4),
		release: make(chan struct{}),
	}
	fixture := newQueueFixture(t, func(config *QueueConfig) {
		config.Sender = sender
		config.MaxConcurrent = 1
	})
	message := fixture.enqueue("sender@probe.test", "someone@example.com")

	if err := fixture.queue.Start(context.Background()); err != nil {
		t.Fatalf("start the queue: %v", err)
	}
	<-sender.entered

	shutdown := make(chan error, 1)
	go func() { shutdown <- fixture.queue.Shutdown(context.Background()) }()

	// Shutdown must still be waiting: the delivery has not finished, and a
	// session that is halfway through DATA is not a session to walk away from.
	select {
	case err := <-shutdown:
		t.Fatalf("shutdown returned %v while a delivery was in flight", err)
	case <-time.After(250 * time.Millisecond):
	}

	close(sender.release)
	if err := <-shutdown; err != nil {
		t.Fatalf("shut the queue down: %v", err)
	}
	if _, found := fixture.get(message.ID); found {
		t.Error("the in-flight delivery was abandoned rather than drained")
	}
}

func TestQueueShutdownIsIdempotentAndRefusesLaterWork(t *testing.T) {
	t.Parallel()
	fixture := newQueueFixture(t, nil)
	if err := fixture.queue.Shutdown(context.Background()); err != nil {
		t.Fatalf("shut down a runner that never started: %v", err)
	}
	if err := fixture.queue.Shutdown(context.Background()); err != nil {
		t.Fatalf("the second shutdown failed: %v", err)
	}
	if err := fixture.queue.RunOnce(context.Background()); !errors.Is(err, ErrQueueClosed) {
		t.Errorf("RunOnce after shutdown = %v, want ErrQueueClosed", err)
	}
	if err := fixture.queue.Start(context.Background()); !errors.Is(err, ErrQueueClosed) {
		t.Errorf("Start after shutdown = %v, want ErrQueueClosed", err)
	}
}

func TestQueueStartRefusesToRunTwice(t *testing.T) {
	t.Parallel()
	fixture := newQueueFixture(t, nil)
	if err := fixture.queue.Start(context.Background()); err != nil {
		t.Fatalf("start the queue: %v", err)
	}
	t.Cleanup(func() {
		if err := fixture.queue.Shutdown(context.Background()); err != nil {
			t.Errorf("shut the queue down: %v", err)
		}
	})
	if err := fixture.queue.Start(context.Background()); !errors.Is(err, ErrInvalid) {
		t.Errorf("the second Start = %v, want ErrInvalid", err)
	}
}

// Kick is how a freshly accepted message goes out now rather than at the next
// tick. The interval here is long enough that only a kick can explain a pass.
func TestQueueKickMakesAPassBeforeTheNextTick(t *testing.T) {
	t.Parallel()
	sender := &gatedSender{entered: make(chan struct{}, 4)}
	fixture := newQueueFixture(t, func(config *QueueConfig) {
		config.Sender = sender
		config.Interval = time.Hour
	})
	if err := fixture.queue.Start(context.Background()); err != nil {
		t.Fatalf("start the queue: %v", err)
	}
	t.Cleanup(func() {
		if err := fixture.queue.Shutdown(context.Background()); err != nil {
			t.Errorf("shut the queue down: %v", err)
		}
	})
	// The pass the runner makes at startup finds nothing.
	fixture.enqueue("sender@probe.test", "someone@example.com")
	fixture.queue.Kick()
	<-sender.entered
}

// -----------------------------------------------------------------------------
// Construction and paths
// -----------------------------------------------------------------------------

func TestNewQueueRefusesAnIncompleteConfiguration(t *testing.T) {
	t.Parallel()
	store := newQueueFixture(t, nil).store
	tests := []struct {
		name   string
		store  *Store
		config QueueConfig
	}{
		{"no store", nil, QueueConfig{SpoolDir: "/tmp", Sender: &gatedSender{}}},
		{"no sender", store, QueueConfig{SpoolDir: "/tmp"}},
		{"no spool directory", store, QueueConfig{Sender: &gatedSender{}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewQueue(test.store, test.config); !errors.Is(err, ErrInvalid) {
				t.Errorf("err = %v, want ErrInvalid", err)
			}
		})
	}
}

// MessagePath is written by this server, so one that has climbed out of the
// spool is a corrupted record. Repairing it quietly would turn that into an
// arbitrary file read.
func TestQueueRefusesASpoolPathThatLeavesTheSpoolDirectory(t *testing.T) {
	t.Parallel()
	queue := newQueueFixture(t, nil).queue
	for _, relative := range []string{
		"",
		".",
		"..",
		"../maild.db",
		"../../etc/passwd",
		"/etc/passwd",
	} {
		if _, err := queue.spoolPath(relative); !errors.Is(err, ErrInvalid) {
			t.Errorf("spoolPath(%q) = %v, want ErrInvalid", relative, err)
		}
	}
	path, err := queue.spoolPath("qabc.eml")
	if err != nil {
		t.Fatalf("spoolPath of an ordinary name: %v", err)
	}
	if filepath.Dir(path) != queue.spoolDir {
		t.Errorf("spoolPath resolved outside the spool: %q", path)
	}
}

// A Sender that breaks its contract must defer the message, never lose it.
func TestQueueDefersWhenTheSenderReportsNothing(t *testing.T) {
	t.Parallel()
	fixture := newQueueFixture(t, func(config *QueueConfig) {
		config.Sender = senderFunc(func(context.Context, string, string, []string, []byte) []RelayReport {
			return nil
		})
	})
	message := fixture.enqueue("sender@probe.test", "someone@example.com")

	fixture.run()

	stored, found := fixture.get(message.ID)
	if !found {
		t.Fatal("a sender that reported nothing destroyed the message")
	}
	if stored.Recipients[0].Status != QueueStatusTemporaryFailure {
		t.Errorf("status = %q, want %q", stored.Recipients[0].Status, QueueStatusTemporaryFailure)
	}
	if messages := fixture.messages(); len(messages) != 1 {
		t.Errorf("a bounce was generated for a bug: %+v", messages)
	}
}

// A sender error this server cannot read is a bug in this server, and a bug
// must defer a message rather than tell the sender it will never be delivered.
func TestQueueDefersWhenTheSenderReportsAnErrorItCannotClassify(t *testing.T) {
	t.Parallel()
	fixture := newQueueFixture(t, func(config *QueueConfig) {
		config.Sender = senderFunc(func(_ context.Context, _, _ string, to []string, _ []byte) []RelayReport {
			reports := make([]RelayReport, len(to))
			for i, address := range to {
				reports[i] = RelayReport{Address: address, Err: errors.New("something unexpected")}
			}
			return reports
		})
	})
	message := fixture.enqueue("sender@probe.test", "someone@example.com")

	fixture.run()

	stored, found := fixture.get(message.ID)
	if !found {
		t.Fatal("an unreadable error destroyed the message")
	}
	if stored.Recipients[0].Status != QueueStatusTemporaryFailure {
		t.Errorf("status = %q, want %q", stored.Recipients[0].Status, QueueStatusTemporaryFailure)
	}
	if messages := fixture.messages(); len(messages) != 1 {
		t.Errorf("a bounce was generated for an error nobody could read: %+v", messages)
	}
}

// senderFunc adapts a function to Sender.
type senderFunc func(ctx context.Context, domain, from string, to []string, body []byte) []RelayReport

func (f senderFunc) Send(ctx context.Context, domain, from string, to []string, body []byte) []RelayReport {
	return f(ctx, domain, from, to, body)
}

// -----------------------------------------------------------------------------
// The delivery path waking the runner
// -----------------------------------------------------------------------------

// newKickFixture puts a real Deliverer and a real Queue over one store, which is
// the arrangement a deployment ends up with: server.go hands a services hook a
// *Store, the hook builds the delivery path from it, and the queue runner is
// built from the same store afterwards. Neither half is ever handed the other.
func newKickFixture(t *testing.T, configure func(*QueueConfig)) (*Deliverer, *queueFixture) {
	t.Helper()
	fixture := newQueueFixture(t, configure)
	deliverer, err := NewDeliverer(fixture.store, DeliveryConfig{
		Root:     filepath.Dir(fixture.spool),
		Hostname: "mail.probe.test",
		Logger:   quietLogger(),
		Now:      fixture.clock.Now,
	})
	if err != nil {
		t.Fatalf("build the deliverer: %v", err)
	}
	return deliverer, fixture
}

// kickRelay is the one message a forwarder to an external address produces.
func kickDeliver(t *testing.T, deliverer *Deliverer) {
	t.Helper()
	if err := deliverer.Deliver(context.Background(), &Envelope{
		ID:         "m1",
		ReturnPath: "sender@probe.test",
		Recipients: []Recipient{{Address: "someone@example.com", Kind: RecipientRelay}},
		Received:   "Received: from client by mail.probe.test;\r\n",
		Data:       []byte("From: <sender@probe.test>\r\n\r\nthe body\r\n"),
	}); err != nil {
		t.Fatalf("deliver: %v", err)
	}
}

// The defect: enqueue wrote the queue entry and returned, so a forwarded message
// waited out the whole poll interval — a minute in production — before its first
// attempt. The interval here is an hour, so only a kick can explain a delivery.
func TestEnqueueKicksTheRunnerThatDrainsTheSameStore(t *testing.T) {
	t.Parallel()
	sender := &gatedSender{entered: make(chan struct{}, 4)}
	deliverer, fixture := newKickFixture(t, func(config *QueueConfig) {
		config.Sender = sender
		config.Interval = time.Hour
	})
	if err := fixture.queue.Start(context.Background()); err != nil {
		t.Fatalf("start the queue: %v", err)
	}
	t.Cleanup(func() {
		if err := fixture.queue.Shutdown(context.Background()); err != nil {
			t.Errorf("shut the queue down: %v", err)
		}
	})

	kickDeliver(t, deliverer)
	select {
	case <-sender.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the runner did not attempt a freshly queued message; enqueue is not kicking it")
	}
}

// The kick must not depend on the runner having started, or a message queued
// between NewQueue and Start is the one message that still waits an interval.
func TestEnqueueKicksARunnerThatHasNotStartedYet(t *testing.T) {
	t.Parallel()
	deliverer, fixture := newKickFixture(t, nil)
	kickDeliver(t, deliverer)
	select {
	case <-fixture.queue.wake:
	default:
		t.Fatal("the queue was not woken by an enqueue")
	}
}

// A runner that has stopped must not be woken: the delivery path has nothing to
// hand work to, and a kick into a closed runner would be a wake nobody serves.
func TestShutdownStopsTheDeliveryPathWakingTheRunner(t *testing.T) {
	t.Parallel()
	deliverer, fixture := newKickFixture(t, nil)
	if err := fixture.queue.Shutdown(context.Background()); err != nil {
		t.Fatalf("shut the queue down: %v", err)
	}
	kickDeliver(t, deliverer)
	select {
	case <-fixture.queue.wake:
		t.Fatal("a stopped runner was still woken")
	default:
	}
}

// Two runners over two stores must not wake each other: a kick is for the runner
// draining the queue that was written to, and nothing else.
func TestAKickReachesOnlyTheRunnerForThatStore(t *testing.T) {
	t.Parallel()
	deliverer, fixture := newKickFixture(t, nil)
	_, other := newKickFixture(t, nil)

	kickDeliver(t, deliverer)
	select {
	case <-fixture.queue.wake:
	default:
		t.Fatal("the runner for this store was not woken")
	}
	select {
	case <-other.queue.wake:
		t.Fatal("a runner draining a different store was woken")
	default:
	}
}

// A delivery path with no runner behind it — every unit test in this package, and
// a deployment configured without an outbound leg — must queue without a panic
// and without waiting for anything.
func TestEnqueueWithNoRunnerAttachedIsHarmless(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := Open(filepath.Join(root, "maild.db"))
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close the store: %v", err)
		}
	})
	deliverer, err := NewDeliverer(store, DeliveryConfig{
		Root:     root,
		Hostname: "mail.probe.test",
		Logger:   quietLogger(),
	})
	if err != nil {
		t.Fatalf("build the deliverer: %v", err)
	}
	kickDeliver(t, deliverer)
	messages, err := store.ListQueuedMessages(context.Background(), 0)
	if err != nil {
		t.Fatalf("list the queue: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("queued %d messages, want 1", len(messages))
	}
}
