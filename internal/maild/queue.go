package maild

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// This file is the queue runner: the thing that makes an accepted message
// actually leave the building.
//
// Without it the delivery path is a lie. A forwarder target on another server is
// answered 250, spooled, and then sits forever — the sender believes the message
// was delivered, the recipient never sees it, and nothing in the system ever
// says so. Accepting responsibility for a message and then silently not
// delivering it is the worst failure a mail server has, worse than refusing it,
// because a refusal is visible to the sender at the moment it happens.
//
// So the runner has exactly three jobs, and every one of them is about not lying:
//
//  1. deliver, retrying on a bounded schedule for a bounded lifetime;
//  2. bounce to the envelope sender the moment delivery becomes impossible, or
//     when the lifetime runs out, so silence is never the outcome;
//  3. never bounce a bounce. A message with a null envelope sender is already a
//     delivery report; reporting on a report is how two mail servers keep each
//     other busy until someone notices.
//
// The recipient statuses it writes are the ones x:QueuedMessage already exposes
// (CONTRACT.md §3.8), so the control plane sees the same queue this runner acts
// on rather than a second, private one.

const (
	// DefaultQueueInterval is how often the runner looks for due work. Delivery
	// of a fresh message does not wait for it — Kick makes a pass at once.
	DefaultQueueInterval = 60 * time.Second
	// DefaultQueueBaseBackoff is the wait after the first temporary failure.
	DefaultQueueBaseBackoff = 5 * time.Minute
	// DefaultQueueMaxBackoff caps the doubling. Past a couple of hours a longer
	// wait no longer helps the peer and only delays the bounce.
	DefaultQueueMaxBackoff = 2 * time.Hour
	// DefaultQueueLifetime is how long a message may keep failing temporarily
	// before it is bounced. RFC 5321 §4.5.4.1 asks for at least 4–5 days, which
	// is long enough to survive a weekend outage at the far end.
	DefaultQueueLifetime = 5 * 24 * time.Hour
	// DefaultQueueConcurrency is the hard cap on concurrent outbound sessions.
	// Each in-flight message runs its domains one at a time, so this is also
	// the cap on sockets, on spooled bodies held in memory, and on how much of
	// somebody else's mail server this one can occupy at once.
	DefaultQueueConcurrency = 8

	// maxBounceHeaderBytes bounds the copy of the failed message a bounce
	// carries. The bounce quotes headers, never the body: quoting the body
	// makes a 25 MiB message into a 25 MiB bounce, and two servers doing that
	// to each other is an amplifier.
	maxBounceHeaderBytes = 32 << 10
)

// ErrQueueClosed is returned by Start and RunOnce after Shutdown.
var ErrQueueClosed = errors.New("maild: the queue runner is closed")

// -----------------------------------------------------------------------------
// Waking the runner
// -----------------------------------------------------------------------------

// runners is how the delivery path finds the runner that drains the queue it
// just wrote to.
//
// It exists because the two halves are never introduced. A deployment builds
// them inside one services hook (server.go's Deps), and that hook is handed a
// *Store, a spool directory, a hostname and a logger, and returns protocol
// handlers — the Deliverer it built is not part of what it returns, and the
// Queue is built afterwards from the same Deps. So at no point does any caller
// hold both objects, and there is no constructor argument or setter either half
// could be given. The store is the one thing both are handed, so the store is
// where the link between them lives.
//
// The value is *Queue rather than an interface so that a stale entry cannot
// outlive its runner unnoticed: Shutdown removes exactly the entry it put in,
// with CompareAndDelete, so a second runner opened over the same store keeps its
// own registration when the first one stops.
var runners sync.Map // *Store -> *Queue

// attachRunner records that queue drains store.
func attachRunner(store *Store, queue *Queue) {
	if store == nil || queue == nil {
		return
	}
	runners.Store(store, queue)
}

// detachRunner removes queue's registration, and only queue's.
func detachRunner(store *Store, queue *Queue) {
	if store == nil || queue == nil {
		return
	}
	runners.CompareAndDelete(store, queue)
}

// wakeRunner asks whichever runner drains store to make a pass now.
//
// It is called from the delivery path with an SMTP session waiting on the
// answer to the final dot, so it must not block and must not start anything:
// Kick is a non-blocking send on a one-deep channel, and a store with no runner
// — every unit test of the delivery path, and a deployment configured without an
// outbound leg — is simply nothing to do.
func wakeRunner(store *Store) {
	if store == nil {
		return
	}
	if found, ok := runners.Load(store); ok {
		if queue, isQueue := found.(*Queue); isQueue {
			queue.Kick()
		}
	}
}

// The failures the runner decides for itself, rather than reading off the wire.
// They are values so the codes are visible in one place.
var (
	// errQueueExpired is permanent with a 4.x.x enhanced status on purpose:
	// RFC 3464 §2.3.4 reports an expiry as "Action: failed" with the temporary
	// status of the last attempt, because nothing was wrong with the address.
	errQueueExpired = &DeliveryError{
		Code: 554, Enhanced: "4.4.7",
		Message: "Delivery time expired",
	}
	errQueueBodyMissing = &DeliveryError{
		Code: 554, Enhanced: "5.3.0",
		Message: "The spooled message could not be read",
	}
	errQueueBadRecipient = &DeliveryError{
		Code: 550, Enhanced: "5.1.3",
		Message: "That recipient address is not valid",
	}
)

// QueueConfig configures the runner. Every zero value has a safe default.
type QueueConfig struct {
	// SpoolDir is where the delivery path wrote the bodies:
	// DeliveryConfig.Root/spool, which is what Deps.SpoolDir already holds.
	// QueuedMessage.MessagePath is relative to it.
	SpoolDir string
	// Hostname names this server in the bounces it writes. It should be the
	// name the MX record points at.
	Hostname string
	// Sender hands a message to another server. Required.
	Sender Sender

	Interval    time.Duration
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
	Lifetime    time.Duration
	// MaxConcurrent is the hard cap on concurrent outbound sessions.
	MaxConcurrent int
	// Batch bounds one pass. Zero takes MaxObjectsPerCall, which is the page
	// the management API reads anyway.
	Batch  int
	Logger *slog.Logger
	// Now is the clock the retry schedule is computed from. It is injectable so
	// a test can expire a five-day lifetime without sleeping. It is never used
	// for a socket deadline — the relay uses the real clock for those.
	Now func() time.Time
}

// Queue delivers spooled messages and bounces the ones it cannot.
type Queue struct {
	store    *Store
	sender   Sender
	spoolDir string
	hostname string
	logger   *slog.Logger
	now      func() time.Time

	interval    time.Duration
	baseBackoff time.Duration
	maxBackoff  time.Duration
	lifetime    time.Duration
	batch       int

	// slots is the concurrency cap. Holding one is what entitles a goroutine to
	// open an outbound session.
	slots chan struct{}
	// wake is a Kick that has not been served yet. It holds one because two
	// kicks before a pass are the same pass.
	wake chan struct{}
	quit chan struct{}
	wg   sync.WaitGroup

	mu sync.Mutex
	// inflight is the set of message ids a pass has claimed. It is what stops
	// two passes delivering one message twice.
	inflight map[string]struct{}
	started  bool
	closed   bool
	cancel   context.CancelFunc
}

// NewQueue prepares a runner. It does not deliver anything until Start or
// RunOnce is called.
func NewQueue(store *Store, config QueueConfig) (*Queue, error) {
	switch {
	case store == nil:
		return nil, fmt.Errorf("%w: the queue runner needs a store", ErrInvalid)
	case config.Sender == nil:
		return nil, fmt.Errorf("%w: the queue runner needs a sender", ErrInvalid)
	case strings.TrimSpace(config.SpoolDir) == "":
		return nil, fmt.Errorf("%w: the queue runner needs a spool directory", ErrInvalid)
	}
	queue := &Queue{
		store:       store,
		sender:      config.Sender,
		spoolDir:    filepath.Clean(config.SpoolDir),
		hostname:    sanitizeHeaderValue(config.Hostname),
		logger:      config.Logger,
		now:         config.Now,
		interval:    config.Interval,
		baseBackoff: config.BaseBackoff,
		maxBackoff:  config.MaxBackoff,
		lifetime:    config.Lifetime,
		batch:       config.Batch,
		wake:        make(chan struct{}, 1),
		quit:        make(chan struct{}),
		inflight:    make(map[string]struct{}),
	}
	if queue.hostname == "" {
		queue.hostname = "localhost"
	}
	if queue.logger == nil {
		queue.logger = slog.Default()
	}
	if queue.now == nil {
		queue.now = time.Now
	}
	if queue.interval <= 0 {
		queue.interval = DefaultQueueInterval
	}
	if queue.baseBackoff <= 0 {
		queue.baseBackoff = DefaultQueueBaseBackoff
	}
	if queue.maxBackoff <= 0 {
		queue.maxBackoff = DefaultQueueMaxBackoff
	}
	if queue.maxBackoff < queue.baseBackoff {
		queue.maxBackoff = queue.baseBackoff
	}
	if queue.lifetime <= 0 {
		queue.lifetime = DefaultQueueLifetime
	}
	if queue.batch <= 0 {
		queue.batch = MaxObjectsPerCall
	}
	concurrency := config.MaxConcurrent
	if concurrency <= 0 {
		concurrency = DefaultQueueConcurrency
	}
	queue.slots = make(chan struct{}, concurrency)
	// Registered at construction rather than at Start, because a message queued
	// between the two would otherwise be the one message that still waits out a
	// whole interval, and Kick on a runner that has not started yet is harmless:
	// the wake channel holds it and the first pass consumes it.
	attachRunner(store, queue)
	return queue, nil
}

// -----------------------------------------------------------------------------
// Running
// -----------------------------------------------------------------------------

// Start begins the poll loop and returns immediately. The loop makes a pass at
// once rather than waiting out the first interval, so a restart delivers what
// was queued before it.
func (q *Queue) Start(ctx context.Context) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return ErrQueueClosed
	}
	if q.started {
		return fmt.Errorf("%w: the queue runner is already started", ErrInvalid)
	}
	runCtx, cancel := context.WithCancel(ctx)
	q.started = true
	q.cancel = cancel
	q.wg.Add(1)
	go q.loop(runCtx)
	return nil
}

// Shutdown stops scheduling new work and waits for what is in flight.
//
// The wait is the point: a session that is halfway through DATA has already told
// the far end a message is coming, and abandoning it there is how a message gets
// delivered twice. Only when ctx expires is the in-flight work cancelled, which
// closes the outbound connections underneath it.
//
// It is idempotent, and safe to call on a runner that was never started.
func (q *Queue) Shutdown(ctx context.Context) error {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return nil
	}
	q.closed = true
	cancel := q.cancel
	q.mu.Unlock()
	// Deregister first: from here on the delivery path has nothing to wake, so a
	// message accepted during the drain is left for the next process rather than
	// kicking a runner that is on its way out.
	detachRunner(q.store, q)
	close(q.quit)

	drained := make(chan struct{})
	go func() {
		q.wg.Wait()
		close(drained)
	}()
	select {
	case <-drained:
		if cancel != nil {
			cancel()
		}
		return nil
	case <-ctx.Done():
		if cancel != nil {
			cancel()
		}
		// The cancellation closes every outbound socket, so this wait is
		// bounded by an unwind rather than by the network.
		<-drained
		return ctx.Err()
	}
}

// Kick asks for a pass now instead of at the next tick. It never blocks: a kick
// that arrives while one is already pending is the same pass.
func (q *Queue) Kick() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// RunOnce makes one pass over the due messages and waits for it to finish.
// It is what a test drives, and what an operator-triggered flush would call.
func (q *Queue) RunOnce(ctx context.Context) error {
	q.mu.Lock()
	closed := q.closed
	q.mu.Unlock()
	if closed {
		return ErrQueueClosed
	}
	return q.runOnce(ctx)
}

func (q *Queue) loop(ctx context.Context) {
	defer q.wg.Done()
	ticker := time.NewTicker(q.interval)
	defer ticker.Stop()
	for {
		if err := q.runOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
			q.logger.Warn("maild: a queue pass failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-q.quit:
			return
		case <-q.wake:
		case <-ticker.C:
		}
	}
}

// runOnce dispatches every due message and waits for the whole pass. Waiting is
// what makes the concurrency cap a cap on the process rather than on one pass,
// and what lets Shutdown drain by waiting for the loop alone.
func (q *Queue) runOnce(ctx context.Context) error {
	messages, err := q.store.ListQueuedMessages(ctx, q.batch)
	if err != nil {
		return fmt.Errorf("maild: read the outbound queue: %w", err)
	}
	now := q.now().UTC()
	var pass sync.WaitGroup
	defer pass.Wait()

	for _, message := range messages {
		if !message.NextRetry.IsZero() && message.NextRetry.After(now) {
			continue
		}
		if !q.claim(message.ID) {
			continue
		}
		select {
		case q.slots <- struct{}{}:
		case <-ctx.Done():
			q.release(message.ID)
			return ctx.Err()
		}
		pass.Add(1)
		go func() {
			defer pass.Done()
			defer func() { <-q.slots }()
			defer q.release(message.ID)
			defer func() {
				// One malformed message must not take the daemon with it.
				if recovered := recover(); recovered != nil {
					q.logger.Error("maild: a queue delivery panicked",
						"message", message.ID, "panic", recovered)
				}
			}()
			q.deliverMessage(ctx, message)
		}()
	}
	return nil
}

func (q *Queue) claim(id string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, taken := q.inflight[id]; taken {
		return false
	}
	q.inflight[id] = struct{}{}
	return true
}

func (q *Queue) release(id string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.inflight, id)
}

// -----------------------------------------------------------------------------
// One message
// -----------------------------------------------------------------------------

// outcome is what this pass concluded about one recipient. The distinction that
// matters is attempted-and-delivered from not-attempted-at-all: both leave
// failure nil, and only one of them may clear the recipient's status.
type outcome struct {
	attempted bool
	failure   *DeliveryError
}

// deliverMessage runs one queued message to whatever conclusion this pass
// reaches, and writes that conclusion down.
func (q *Queue) deliverMessage(ctx context.Context, message QueuedMessage) {
	now := q.now().UTC()
	pending := pendingRecipients(message, now)
	outcomes := make([]outcome, len(message.Recipients))
	var body []byte

	// Nothing due still runs the commit below: that is what recomputes
	// NextRetry and retires an entry whose recipients have all finished.
	if len(pending) > 0 {
		var bodyErr error
		body, bodyErr = q.readSpool(message)
		switch {
		case q.expired(message, now):
			// Expiry is decided before an attempt, not after one: the lifetime
			// is over either way, and one more doomed session only delays the
			// bounce the sender has been waiting five days for.
			markAll(outcomes, pending, errQueueExpired)
		case bodyErr != nil && errors.Is(bodyErr, os.ErrNotExist):
			// No body means no attempt can ever succeed. Bouncing is the
			// honest answer; retrying forever over a file that is not there is
			// not.
			q.logger.Error("maild: a queued message has no spooled body",
				"message", message.ID, "error", bodyErr)
			markAll(outcomes, pending, errQueueBodyMissing)
		case bodyErr != nil:
			q.logger.Warn("maild: could not read a spooled message",
				"message", message.ID, "error", bodyErr)
			markAll(outcomes, pending, temporaryf(451, "4.3.0", bodyErr,
				"The message could not be read from the spool"))
		default:
			q.attempt(ctx, message, pending, body, outcomes)
		}
	}
	q.commit(ctx, message, outcomes, body, now)
}

// attempt offers the message to each pending recipient's domain, one session per
// domain: the recipients of one domain share the SMTP conversation the way an
// inbound transaction shares one.
func (q *Queue) attempt(ctx context.Context, message QueuedMessage, pending []int, body []byte, outcomes []outcome) {
	byDomain := make(map[string][]target, len(pending))
	domains := make([]string, 0, len(pending))
	for _, index := range pending {
		// The stored address is validated here rather than trusted. It reaches
		// a wire protocol and, on failure, a bounce header; a control character
		// in either is an injection. Address literals and over-long addresses
		// are refused too, which is a permanent failure and so a bounce.
		address, err := ParseAddress(message.Recipients[index].Address)
		if err != nil {
			outcomes[index] = outcome{attempted: true, failure: &DeliveryError{
				Code:     errQueueBadRecipient.Code,
				Enhanced: errQueueBadRecipient.Enhanced,
				Message:  errQueueBadRecipient.Message,
				cause:    err,
			}}
			continue
		}
		if _, seen := byDomain[address.Domain]; !seen {
			domains = append(domains, address.Domain)
		}
		// The canonical form travels, not the stored one: it is the exact
		// string ParseAddress vouched for, and this server folds local-part
		// case everywhere else already.
		byDomain[address.Domain] = append(byDomain[address.Domain],
			target{index: index, address: address.String()})
	}
	sort.Strings(domains)

	for _, domain := range domains {
		targets := byDomain[domain]
		if err := ctx.Err(); err != nil {
			for _, entry := range targets {
				outcomes[entry.index] = outcome{attempted: true, failure: temporaryf(451, "4.3.0", err,
					"The server is shutting down, this will be retried")}
			}
			continue
		}
		to := make([]string, len(targets))
		for i, entry := range targets {
			to[i] = entry.address
		}
		reports := q.sender.Send(ctx, domain, message.ReturnPath, to, body)
		for i, entry := range targets {
			if i >= len(reports) {
				// A Sender that returned fewer reports than recipients is a
				// bug, and a bug must defer the message rather than lose it.
				outcomes[entry.index] = outcome{attempted: true, failure: temporaryf(451, "4.3.0", nil,
					"The delivery attempt did not report an outcome")}
				continue
			}
			outcomes[entry.index] = outcome{attempted: true, failure: asDeliveryError(reports[i].Err)}
		}
	}
}

// target is one recipient of one domain: where its answer belongs, and the
// canonical address that goes on the wire.
type target struct {
	index   int
	address string
}

// commit writes this pass's conclusions to the queue entry, sends whatever
// bounce they call for, and retires the entry when every recipient is done.
func (q *Queue) commit(ctx context.Context, message QueuedMessage, outcomes []outcome, body []byte, now time.Time) {
	var bounces []bounceEntry
	for i := range message.Recipients {
		if !outcomes[i].attempted {
			continue
		}
		recipient := &message.Recipients[i]
		failure := outcomes[i].failure
		switch {
		case failure == nil:
			recipient.Status = QueueStatusCompleted
			recipient.LastCode = 250
			recipient.LastMessage = "Delivered"
			recipient.RetryDue = time.Time{}
		case failure.Temporary():
			recipient.Status = QueueStatusTemporaryFailure
			recipient.RetryCount++
			recipient.RetryDue = now.Add(q.backoff(recipient.RetryCount))
			recipient.LastCode = failure.Code
			recipient.LastMessage = failure.Message
		default:
			recipient.Status = QueueStatusPermanentFailure
			recipient.RetryCount++
			recipient.RetryDue = time.Time{}
			recipient.LastCode = failure.Code
			recipient.LastMessage = failure.Message
			bounces = append(bounces, bounceEntry{
				Address: recipient.Address,
				ORCPT:   recipient.ORCPT,
				Status:  failure.Enhanced,
				Code:    failure.Code,
				Message: failure.Message,
			})
		}
	}

	// The bounce is written before the queue entry is updated. A crash between
	// the two sends the bounce twice; the other order loses it, and a lost
	// bounce leaves the sender believing a message was delivered — which is the
	// exact failure this whole file exists to remove.
	if len(bounces) > 0 {
		q.bounce(ctx, message, bounces, body, now)
	}

	if allTerminal(message.Recipients) {
		q.retire(ctx, message)
		return
	}
	message.NextRetry = earliestRetry(message.Recipients)
	if _, err := q.store.PutQueuedMessage(ctx, message); err != nil {
		q.logger.Error("maild: could not update a queued message",
			"message", message.ID, "error", err)
	}
}

// retire removes a finished message. The store entry goes first: an orphaned
// spool file is a byte leak an operator can sweep, while an orphaned entry is a
// message that is retried forever over a body that is not there.
func (q *Queue) retire(ctx context.Context, message QueuedMessage) {
	if err := q.store.DeleteQueuedMessage(ctx, message.ID); err != nil {
		q.logger.Error("maild: could not remove a finished queued message",
			"message", message.ID, "error", err)
		return
	}
	path, err := q.spoolPath(message.MessagePath)
	if err != nil {
		return
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		q.logger.Warn("maild: could not remove a spooled message",
			"message", message.ID, "error", err)
	}
}

func (q *Queue) readSpool(message QueuedMessage) ([]byte, error) {
	path, err := q.spoolPath(message.MessagePath)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

// spoolPath resolves a QueuedMessage.MessagePath.
//
// A path that leaves the spool directory is refused rather than cleaned. This
// server wrote every MessagePath itself, so one that has climbed out is a
// corrupted record; repairing it quietly would turn a corrupted record into an
// arbitrary file read.
func (q *Queue) spoolPath(relative string) (string, error) {
	cleaned := filepath.Clean(filepath.FromSlash(strings.TrimSpace(relative)))
	if relative == "" || cleaned == "." || cleaned == string(filepath.Separator) ||
		filepath.IsAbs(cleaned) || cleaned == ".." ||
		strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: a queued message has an unusable body path", ErrInvalid)
	}
	return filepath.Join(q.spoolDir, cleaned), nil
}

// expired reports whether the message has run out of lifetime.
func (q *Queue) expired(message QueuedMessage, now time.Time) bool {
	if q.lifetime <= 0 || message.CreatedAt.IsZero() {
		return false
	}
	return !now.Before(message.CreatedAt.Add(q.lifetime))
}

// backoff is the wait before the attempt after retryCount failures: the base
// delay doubled once per failure, capped.
//
// The doubling is a loop rather than a shift because retryCount comes off disk.
// A shift by a large count does not produce a long delay, it produces a wrong
// one — and the wrong one is short, which turns a corrupted record into a tight
// retry loop against someone else's server.
func (q *Queue) backoff(retryCount uint32) time.Duration {
	delay := q.baseBackoff
	for i := uint32(1); i < retryCount; i++ {
		if delay >= q.maxBackoff {
			return q.maxBackoff
		}
		delay *= 2
	}
	if delay > q.maxBackoff {
		return q.maxBackoff
	}
	return delay
}

// pendingRecipients is the recipients this pass is responsible for: not
// finished, and due.
func pendingRecipients(message QueuedMessage, now time.Time) []int {
	var pending []int
	for i, recipient := range message.Recipients {
		if terminalStatus(recipient.Status) {
			continue
		}
		if !recipient.RetryDue.IsZero() && recipient.RetryDue.After(now) {
			continue
		}
		pending = append(pending, i)
	}
	return pending
}

func terminalStatus(status string) bool {
	return status == QueueStatusCompleted || status == QueueStatusPermanentFailure
}

func allTerminal(recipients []QueuedRecipient) bool {
	for _, recipient := range recipients {
		if !terminalStatus(recipient.Status) {
			return false
		}
	}
	return true
}

// earliestRetry is when the message next becomes due: the soonest of its
// unfinished recipients.
func earliestRetry(recipients []QueuedRecipient) time.Time {
	var earliest time.Time
	for _, recipient := range recipients {
		if terminalStatus(recipient.Status) {
			continue
		}
		if recipient.RetryDue.IsZero() {
			return time.Time{}
		}
		if earliest.IsZero() || recipient.RetryDue.Before(earliest) {
			earliest = recipient.RetryDue
		}
	}
	return earliest
}

func markAll(outcomes []outcome, indexes []int, failure *DeliveryError) {
	for _, index := range indexes {
		outcomes[index] = outcome{attempted: true, failure: failure}
	}
}

// asDeliveryError normalises whatever a Sender reported. An error that does not
// say what it means is temporary on purpose: a bug in this server should defer a
// message, never bounce one.
func asDeliveryError(err error) *DeliveryError {
	if err == nil {
		return nil
	}
	var delivery *DeliveryError
	if errors.As(err, &delivery) {
		return delivery
	}
	return temporaryf(451, "4.3.0", err, "The delivery attempt did not complete")
}

// -----------------------------------------------------------------------------
// Bounces
// -----------------------------------------------------------------------------

// bounceEntry is one recipient a bounce reports on.
type bounceEntry struct {
	Address string
	ORCPT   string
	Status  string
	Code    int
	Message string
}

// bounce queues a delivery status notification back to the envelope sender.
//
// It is queued rather than sent inline so that a bounce gets the same retries,
// the same lifetime and the same concurrency cap as any other message — and so
// that a slow bounce cannot hold the worker that produced it.
func (q *Queue) bounce(ctx context.Context, original QueuedMessage, failures []bounceEntry, body []byte, now time.Time) {
	if original.ReturnPath == "" {
		// Never bounce a bounce. A null envelope sender means this message is
		// already a delivery report, and a report about a report is a loop: the
		// two servers hand it back and forth until one of them runs out of
		// disk. RFC 5321 §6.1 requires the null sender for exactly this reason,
		// and this branch is the other half of the bargain.
		q.logger.Warn("maild: a bounce could not be delivered and was dropped",
			"message", original.ID, "recipients", len(failures))
		return
	}
	sender, err := ParseAddress(original.ReturnPath)
	if err != nil {
		// The return path is not an address, so there is nowhere to report to.
		// The error is not quoted: it is the untrusted string that wanted to
		// reach a log line.
		q.logger.Warn("maild: a message failed but its return path is not an address",
			"message", original.ID, "error", err)
		return
	}
	report, err := q.composeBounce(sender.String(), original, failures, body, now)
	if err != nil {
		q.logger.Error("maild: could not compose a bounce",
			"message", original.ID, "error", err)
		return
	}
	if err := q.enqueueBounce(ctx, sender.String(), report, now); err != nil {
		q.logger.Error("maild: could not queue a bounce",
			"message", original.ID, "error", err)
		return
	}
	q.logger.Info("maild: a message was bounced to its sender",
		"message", original.ID, "recipients", len(failures))
}

// enqueueBounce spools the report and queues it with a null envelope sender,
// which is what makes it un-bounceable in turn.
func (q *Queue) enqueueBounce(ctx context.Context, recipient string, body []byte, now time.Time) error {
	id, err := randomID("q")
	if err != nil {
		return err
	}
	name := id + ".eml"
	if err := writeThenRename(
		filepath.Join(q.spoolDir, maildirTmp, name),
		filepath.Join(q.spoolDir, name),
		body,
	); err != nil {
		return err
	}
	message := QueuedMessage{
		V:  DocVersion,
		ID: id,
		// The null envelope sender. Nothing about this message may ever be
		// reported anywhere.
		ReturnPath: "",
		Recipients: []QueuedRecipient{{
			Address:   recipient,
			Status:    QueueStatusScheduled,
			QueueName: remoteQueueName,
			RetryDue:  now,
		}},
		MessagePath: name,
		SizeBytes:   int64(len(body)),
		CreatedAt:   now,
		NextRetry:   now,
	}
	if _, err := q.store.PutQueuedMessage(ctx, message); err != nil {
		if removeErr := os.Remove(filepath.Join(q.spoolDir, name)); removeErr != nil {
			q.logger.Debug("maild: could not remove an orphaned bounce body", "error", removeErr)
		}
		return err
	}
	return nil
}

// composeBounce renders the RFC 3464 multipart/report the sender receives.
//
// Every value interpolated into a header is either produced here or passed
// through sanitizeHeaderValue, because two of them — the recipient address and
// the remote server's reply text — arrived from somewhere else, and a CR in a
// header is a second message.
func (q *Queue) composeBounce(to string, original QueuedMessage, failures []bounceEntry, body []byte, now time.Time) ([]byte, error) {
	boundary, err := randomID("b")
	if err != nil {
		return nil, err
	}
	messageID, err := randomID("m")
	if err != nil {
		return nil, err
	}

	var builder strings.Builder
	line := func(text string) {
		builder.WriteString(text)
		builder.WriteString("\r\n")
	}

	line("From: Mail Delivery Subsystem <MAILER-DAEMON@" + q.hostname + ">")
	line("To: <" + sanitizeHeaderValue(to) + ">")
	line("Subject: Undelivered mail returned to sender")
	line("Date: " + now.Format(time.RFC1123Z))
	line("Message-ID: <" + messageID + "@" + q.hostname + ">")
	// RFC 3834: an auto-responder must not answer this, and this server must
	// not answer an auto-response. Both halves keep bounces from looping.
	line("Auto-Submitted: auto-replied")
	line("MIME-Version: 1.0")
	line(`Content-Type: multipart/report; report-type=delivery-status; boundary="` + boundary + `"`)
	line("")

	line("--" + boundary)
	line("Content-Type: text/plain; charset=us-ascii")
	line("")
	line("This is the mail system at " + q.hostname + ".")
	line("")
	line("Your message could not be delivered to one or more recipients.")
	line("No further attempt will be made to deliver it.")
	line("")
	for _, failure := range failures {
		line("<" + sanitizeHeaderValue(failure.Address) + ">: " + diagnostic(failure))
	}
	line("")

	line("--" + boundary)
	line("Content-Type: message/delivery-status")
	line("")
	line("Reporting-MTA: dns; " + q.hostname)
	if !original.CreatedAt.IsZero() {
		line("Arrival-Date: " + original.CreatedAt.UTC().Format(time.RFC1123Z))
	}
	for _, failure := range failures {
		line("")
		if failure.ORCPT != "" {
			line("Original-Recipient: rfc822; " + sanitizeHeaderValue(failure.ORCPT))
		}
		line("Final-Recipient: rfc822; " + sanitizeHeaderValue(failure.Address))
		line("Action: failed")
		line("Status: " + statusOrDefault(failure.Status))
		if failure.Code > 0 {
			line("Diagnostic-Code: smtp; " + strconv.Itoa(failure.Code) + " " +
				sanitizeHeaderValue(failure.Message))
		}
	}
	line("")

	line("--" + boundary)
	line("Content-Type: text/rfc822-headers")
	line("")
	builder.Write(originalHeaders(body))
	line("")
	line("--" + boundary + "--")
	return []byte(builder.String()), nil
}

// diagnostic is the human-readable line for one failed recipient.
func diagnostic(failure bounceEntry) string {
	text := sanitizeHeaderValue(failure.Message)
	if text == "" {
		text = "delivery failed"
	}
	if failure.Code > 0 {
		return strconv.Itoa(failure.Code) + " " + text
	}
	return text
}

// statusOrDefault keeps the DSN status field well formed even if a failure
// arrived without one.
func statusOrDefault(status string) string {
	if _, ok := leadingStatus(status); ok {
		return status
	}
	return "5.0.0"
}

// originalHeaders is the header block of the failed message: everything up to
// the first empty line, bounded, and always ending in CRLF so the MIME part
// after it starts on a line of its own.
func originalHeaders(body []byte) []byte {
	headers := body
	if index := bytes.Index(headers, []byte("\r\n\r\n")); index >= 0 {
		headers = headers[:index+2]
	} else if index := bytes.Index(headers, []byte("\n\n")); index >= 0 {
		headers = headers[:index+1]
	}
	if len(headers) > maxBounceHeaderBytes {
		headers = headers[:maxBounceHeaderBytes]
	}
	if len(headers) == 0 {
		return nil
	}
	if !bytes.HasSuffix(headers, []byte("\n")) {
		return append(append([]byte(nil), headers...), '\r', '\n')
	}
	return headers
}
