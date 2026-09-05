package mail

import (
	"context"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	mailv1 "github.com/castlemilk/dns/gen/go/mail/v1"
	"github.com/castlemilk/dns/internal/platform"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Delivery counting exists because the mail server can be asked to POST its own
// delivery events to this control plane. It is the only honest source of a
// delivered/bounced figure on the community edition: x:Metric/query is
// Enterprise-only and the Prometheus counters carry no domain label. See
// docs/mail-runbook.md, "Delivery statistics", for the experiment that
// established this, including
// the two events counted and why the others are not.
//
// Nothing here estimates. A window that was not fully observed reports
// available=false, and the console must say so rather than show a zero. Nor is
// a figure claimed on the strength of this side's configuration alone: the
// receiver has to have been reached before there is anything to report. See
// deliveryStats and deliveryReceiver below.
const (
	// deliveryWindow is the rolling window the console shows. The design asks
	// for "last 24 hours" and that is exactly what is counted.
	deliveryWindow = 24 * time.Hour

	// deliveryBuckets is how many whole hours each row keeps. It is one more
	// than the window, because the oldest hour is only partly inside it: the
	// extra bucket is dropped, never counted, and exists so the boundary hour
	// can age out cleanly.
	deliveryBuckets = 25

	// deliveryFlushInterval bounds how long a counted event can sit in memory
	// before it reaches platform.db. The mail server batches and throttles its
	// deliveries, so this is a handful of writes a minute at most.
	deliveryFlushInterval = 30 * time.Second
)

// Outcomes counted from the mail server's delivery events. These two are the
// per-recipient records the server writes for the DSN it may have to send, so
// there is exactly one of them per recipient per outcome — which is what makes
// them countable. delivery.delivered is deliberately NOT counted: it fires
// alongside dsn-success for remote deliveries only, so counting both would
// double every outbound message and count no local one.
const (
	eventDelivered = "delivery.dsn-success"
	eventBounced   = "delivery.dsn-perm-fail"
)

// deliveryReceiver is what this control plane has observed of the delivery-event
// wiring, as opposed to what it was configured to expect.
//
// The distinction is the whole point. MAIL_WEBHOOK_SECRET is one half of a
// two-sided arrangement: the other half is a webhook on the mail server, and
// this side cannot see it. A hook that was never created, one created without
// the restart that loads it (docs/mail-runbook.md, "Delivery statistics"), a URL
// the server cannot reach, and a signature key that does not match this one all
// look identical from here — and all of them used to render as a confident
// "0 delivered · 0 bounced" over a full day. So the counters are only reported
// once a delivery has actually been accepted, and until then the note names
// what is missing.
//
// Nothing here is persisted, and nothing needs to be: the counted window never
// predates this process (deliverySince), so "has anything been accepted since
// this process started counting?" is exactly the question the reported window
// asks.
type deliveryReceiver struct {
	mu       sync.Mutex
	accepted uint64
	firstAt  time.Time
	lastAt   time.Time
	// rejected is keyed by the bounded reason words this package passes to
	// reject, so the map holds at most a handful of entries and never anything
	// a caller chose.
	rejected map[string]*deliveryRejections
}

// deliveryRejections is one reason's tally and its own log throttle. The
// throttle is per reason on purpose: a flood of one reason must not hide the
// first sighting of another, which is how a signature mismatch stayed invisible
// behind ordinary unauthenticated noise.
type deliveryRejections struct {
	count    uint64
	lastAt   time.Time
	loggedAt time.Time
}

// deliveryReceiverState is a snapshot for the status copy.
type deliveryReceiverState struct {
	Accepted   uint64
	FirstAt    time.Time
	LastAt     time.Time
	Rejected   uint64
	Reason     string
	RejectedAt time.Time
}

func newDeliveryReceiver() *deliveryReceiver {
	return &deliveryReceiver{rejected: map[string]*deliveryRejections{}}
}

// accept records one authenticated, well-formed delivery. It is the evidence
// that the mail server is reaching this control plane; whether that delivery
// carried an event for a domain this deployment hosts is a separate question.
func (r *deliveryReceiver) accept(now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.accepted++
	if r.firstAt.IsZero() {
		r.firstAt = now.UTC()
	}
	r.lastAt = now.UTC()
}

// reject records one refused delivery and reports whether this reason may be
// logged again yet.
func (r *deliveryReceiver) reject(reason string, now time.Time) (uint64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.rejected[reason]
	if !ok {
		entry = &deliveryRejections{}
		r.rejected[reason] = entry
	}
	entry.count++
	entry.lastAt = now.UTC()
	if !entry.loggedAt.IsZero() && now.Sub(entry.loggedAt) < webhookRejectInterval {
		return entry.count, false
	}
	entry.loggedAt = now
	return entry.count, true
}

// reached reports whether the mail server has been observed posting here.
func (r *deliveryReceiver) reached() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.accepted > 0
}

// state summarises the receiver, naming the reason most deliveries were refused
// for. Ties go to the reason seen most recently, and then to the alphabetically
// first, so the sentence an operator reads does not change on its own.
func (r *deliveryReceiver) state() deliveryReceiverState {
	r.mu.Lock()
	defer r.mu.Unlock()
	snapshot := deliveryReceiverState{Accepted: r.accepted, FirstAt: r.firstAt, LastAt: r.lastAt}
	var leading uint64
	for _, reason := range slices.Sorted(maps.Keys(r.rejected)) {
		entry := r.rejected[reason]
		snapshot.Rejected += entry.count
		better := entry.count > leading || (entry.count == leading && entry.lastAt.After(snapshot.RejectedAt))
		if snapshot.Reason == "" || better {
			snapshot.Reason = reason
			snapshot.RejectedAt = entry.lastAt
			leading = entry.count
		}
	}
	return snapshot
}

// deliveryTally accumulates counted events in memory between flushes.
//
// It is keyed by zone id and bounded by the number of bound domains: an event
// for a name this control plane does not host is dropped at parse time and
// never reaches the map, so nothing an unauthenticated sender could do can grow
// it. Within a zone it holds at most deliveryBuckets hours.
type deliveryTally struct {
	mu      sync.Mutex
	hours   map[string]map[int64]platform.MailDeliveryHourDoc
	pending bool
}

func newDeliveryTally() *deliveryTally {
	return &deliveryTally{hours: map[string]map[int64]platform.MailDeliveryHourDoc{}}
}

// add counts one outcome for one zone at one instant.
func (t *deliveryTally) add(zoneID string, at time.Time, delivered bool) {
	hour := at.UTC().Truncate(time.Hour)
	t.mu.Lock()
	defer t.mu.Unlock()
	byHour, ok := t.hours[zoneID]
	if !ok {
		byHour = map[int64]platform.MailDeliveryHourDoc{}
		t.hours[zoneID] = byHour
	}
	entry, ok := byHour[hour.Unix()]
	if !ok {
		// A zone that somehow accumulates more than the window's worth of hours
		// (a clock that jumped, an event with a far-future timestamp) drops its
		// oldest rather than growing without bound.
		if len(byHour) >= deliveryBuckets {
			oldest := hour
			for stamp := range byHour {
				if candidate := time.Unix(stamp, 0).UTC(); candidate.Before(oldest) {
					oldest = candidate
				}
			}
			if !oldest.Equal(hour) {
				delete(byHour, oldest.Unix())
			}
		}
		entry = platform.MailDeliveryHourDoc{Hour: hour}
	}
	if delivered {
		entry.Delivered++
	} else {
		entry.Bounced++
	}
	byHour[hour.Unix()] = entry
	t.pending = true
}

// drain returns everything counted since the last drain and empties the tally.
func (t *deliveryTally) drain() map[string][]platform.MailDeliveryHourDoc {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.pending {
		return nil
	}
	drained := make(map[string][]platform.MailDeliveryHourDoc, len(t.hours))
	for zoneID, byHour := range t.hours {
		entries := make([]platform.MailDeliveryHourDoc, 0, len(byHour))
		for _, entry := range byHour {
			entries = append(entries, entry)
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Hour.Before(entries[j].Hour) })
		drained[zoneID] = entries
	}
	t.hours = map[string]map[int64]platform.MailDeliveryHourDoc{}
	t.pending = false
	return drained
}

// flushDeliveries merges the tally into the bound rows. It runs on the
// reconciler's goroutine, so it is the only writer of these counters and needs
// no lock beyond the tally's own.
//
// A zone whose row has gone (unbound between the event and the flush) is
// dropped: counts for a binding that no longer exists are not kept anywhere.
func (s *Service) flushDeliveries(ctx context.Context) {
	drained := s.deliveries.drain()
	if len(drained) == 0 {
		return
	}
	// One bucket older than the window is kept, so the oldest hour ages out on
	// the next flush rather than the moment the clock crosses it.
	cutoff := windowStart(s.deps.Now().UTC()).Add(-time.Hour)
	for zoneID, entries := range drained {
		doc, err := s.deps.Store.GetMailDomain(ctx, zoneID)
		if err != nil {
			if !isStoreNotFound(err) {
				s.deps.Log().Warn("read a mail domain to record delivery events", "error", err)
			}
			continue
		}
		doc.Delivery = mergeDeliveryHours(doc.Delivery, entries, cutoff)
		doc.UpdatedAt = s.deps.Now()
		if err := s.deps.Store.PutMailDomain(ctx, doc); err != nil {
			s.deps.Log().Warn("persist delivery events", "zone", doc.ZoneName, "error", err)
		}
	}
}

// mergeDeliveryHours folds new hours into the stored ones, drops anything older
// than cutoff and keeps at most deliveryBuckets entries, oldest first.
func mergeDeliveryHours(
	stored, added []platform.MailDeliveryHourDoc,
	cutoff time.Time,
) []platform.MailDeliveryHourDoc {
	byHour := make(map[int64]platform.MailDeliveryHourDoc, len(stored)+len(added))
	for _, entry := range append(slices.Clip(stored), added...) {
		hour := entry.Hour.UTC().Truncate(time.Hour)
		if hour.Before(cutoff) {
			continue
		}
		existing := byHour[hour.Unix()]
		existing.Hour = hour
		existing.Delivered += entry.Delivered
		existing.Bounced += entry.Bounced
		byHour[hour.Unix()] = existing
	}
	merged := make([]platform.MailDeliveryHourDoc, 0, len(byHour))
	for _, entry := range byHour {
		merged = append(merged, entry)
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].Hour.Before(merged[j].Hour) })
	if len(merged) > deliveryBuckets {
		merged = merged[len(merged)-deliveryBuckets:]
	}
	return merged
}

// deliveryStats renders one row's rolling counts for the wire, or nothing at
// all when a count would be a claim rather than an observation.
//
// Three things here are what keep the figure honest.
//
// The stats are absent unless a receiver is configured AND the mail server has
// been observed posting to it. Configuration on this side proves nothing: the
// hook lives on the mail server, and every way of getting it wrong is invisible
// from here. Once a delivery has been accepted a zero is a real zero — nothing
// was delivered and nothing bounced — and it is reported as one.
//
// The counts are of the mail this domain sent, not of the mail it received: see
// countedZone in webhook.go for why a mixed figure could not be read as either.
//
// window_hours is what was actually observed, not what was asked for. The
// counters live in this process, so a restart shortens the window; the response
// then says "the last 3 hours" and the console renders that, rather than
// labelling three hours of counting as a day of it.
func (s *Service) deliveryStats(doc platform.MailDomainDoc) *mailv1.DeliveryStats {
	if !s.deliveryCountsAvailable() {
		return nil
	}
	// The counted hours are the current (partial) one and the 23 before it:
	// exactly deliveryWindow's worth of whole buckets, which is what "the last
	// 24 hours" means once the counts are bucketed by hour.
	now := s.deps.Now().UTC()
	since := s.countingSince(now)

	stats := &mailv1.DeliveryStats{
		Available:   true,
		WindowHours: observedHours(since, now),
		Since:       timestamppb.New(since),
	}
	var newest time.Time
	for _, entry := range doc.Delivery {
		hour := entry.Hour.UTC()
		if hour.Before(since) {
			continue
		}
		stats.Delivered += entry.Delivered
		stats.Bounced += entry.Bounced
		if hour.After(newest) {
			newest = hour
		}
	}
	if !newest.IsZero() {
		stats.ObservedAt = timestamppb.New(newest)
	}
	return stats
}

// countingSince is the first hour deliveryStats will report, and so the first
// hour anything may be counted into: the start of the rolling window, or the
// hour this process started counting when that is later.
//
// It is one function because the reader and the writer have to agree. An event
// counted into an hour this response will never sum is an accepted delivery
// that silently disappears — the row keeps it, the console shows a zero, and
// nothing says the two disagree. webhook.go's count uses this to date such an
// event at arrival instead (see the comment there).
//
// The window never predates this process on purpose, and stored hours from
// before it are not reported even though they survived a restart. A count is a
// claim about a period, and this process cannot tell an hour in which nothing
// was delivered from an hour in which it was not running to be posted to: the
// mail server discards an undeliverable webhook after discardAfter (five
// minutes in the shipped script), so a control plane that was down for an hour
// has a real, unrecoverable hole in that hour's bucket. Summing those hours
// would present the hole as a quiet hour. Reporting the shorter window this
// process did observe says less and says it truthfully; docs/mail-runbook.md,
// "Delivery statistics", records the same reasoning.
func (s *Service) countingSince(now time.Time) time.Time {
	since := windowStart(now)
	if s.deliverySince.After(since) {
		since = s.deliverySince.UTC().Truncate(time.Hour)
	}
	return since
}

// deliveryCountsAvailable reports whether a delivered/bounced figure exists at
// all: a receiver is configured here and the mail server has reached it.
func (s *Service) deliveryCountsAvailable() bool {
	return s.cfg.DeliveryEventsConfigured() && s.receiver.reached()
}

// receiverLastEventAt is when the receiver last accepted a delivery, or the
// zero time if it never has. GetMailStatus reports it so a receiver that
// stopped being posted to is visible: the counts stay "available" and drift to
// zero over the window, and nothing else on the wire says why.
func (s *Service) receiverLastEventAt() time.Time {
	return s.receiver.state().LastAt
}

// deliveryStatsState is the pair GetMailStatus reports: whether counts exist,
// and the sentence that says why not when they do not. The sentence always
// names the half of the wiring this control plane cannot see, because that is
// the half that is wrong whenever the counts are missing with a secret set.
func (s *Service) deliveryStatsState() (bool, string) {
	if !s.cfg.DeliveryEventsConfigured() {
		return false, DeliveryStatsNote
	}
	state := s.receiver.state()
	switch {
	case state.Accepted > 0:
		return true, DeliveryStatsWebhookNote
	case state.Rejected > 0:
		// Something is posting and being refused. That is a stronger, more
		// specific statement than "nothing has arrived", so it is the one the
		// operator gets.
		return false, deliveryRejectedNote(state.Reason, state.Rejected)
	default:
		return false, DeliveryStatsPendingNote
	}
}

// observedHours is the width of the window actually counted, in whole hours and
// never less than one: the current hour is always at least partly observed.
func observedHours(since, now time.Time) uint32 {
	hours := int(now.UTC().Truncate(time.Hour).Sub(since)/time.Hour) + 1
	return uint32(max(1, min(hours, int(deliveryWindow/time.Hour))))
}

// windowStart is the first hour inside the rolling window at now.
func windowStart(now time.Time) time.Time {
	return now.UTC().Truncate(time.Hour).Add(-(deliveryWindow - time.Hour))
}

// zoneIDForAddress resolves the bound zone an address belongs to, or "" when it
// belongs to none.
//
// The lookup is over the bound rows only, so an event about a domain this
// control plane does not host costs one map read and is then forgotten. The
// map is rebuilt on each webhook delivery rather than cached, because a
// delivery carries a batch of events and the rows change rarely.
func zoneIDForAddress(byName map[string]string, address string) string {
	at := strings.LastIndex(address, "@")
	if at < 0 || at == len(address)-1 {
		return ""
	}
	return byName[strings.ToLower(strings.TrimSuffix(address[at+1:], "."))]
}
