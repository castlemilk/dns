package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/castlemilk/dns/internal/activity"
	"github.com/castlemilk/dns/internal/platform"
)

// Queue tuning. The retry horizon matches Stripe's own: after seven days it
// stops redelivering, so an entry that never applied in that window is
// dead-lettered rather than retried forever.
const (
	queueSweepInterval = 30 * time.Second
	queueBatch         = 16
	retryBase          = 5 * time.Second
	retryCap           = time.Hour
	deadAfter          = 7 * 24 * time.Hour
	// minAttemptsBeforeDead is how many attempts an entry must have made before
	// the seven-day horizon may set it aside. The horizon is measured from the
	// queue key, and re-driving a dead letter re-uses that key, so an entry an
	// operator retried is already past the horizon and would otherwise be
	// dead-lettered again on its first failure — the Retry button would grant
	// exactly one attempt and none of the back-off below. Re-driving resets the
	// attempt count, so counting attempts is what tells a freshly re-driven
	// entry from an exhausted one; ten attempts is about ninety minutes of the
	// 5s→1h schedule.
	minAttemptsBeforeDead = 10
	// queueIterationBudget bounds one drain so a slow provider cannot hold the
	// worker past a shutdown.
	queueIterationBudget = 20 * time.Second
	// maxDrainBatches bounds one drain pass. Without it a store that refuses
	// every acknowledgement would leave the same batch due forever and the
	// worker would spin; with it the pass ends and the next sweep retries.
	maxDrainBatches = 64
)

// runQueue applies queued deliveries in order. It wakes on a new delivery and
// sweeps on a timer, so a retry scheduled while the process was idle is still
// picked up.
func (s *Service) runQueue(ctx context.Context) error {
	for {
		s.drainQueue(ctx)
		timer := time.NewTimer(queueSweepInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-s.queueWake:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// wakeQueue nudges the worker without blocking the caller. The channel has one
// slot: several deliveries landing together produce one wake-up.
func (s *Service) wakeQueue() {
	select {
	case s.queueWake <- struct{}{}:
	default:
	}
}

// drainQueue processes every due entry it can within one bounded iteration.
func (s *Service) drainQueue(parent context.Context) {
	for range maxDrainBatches {
		if parent.Err() != nil {
			return
		}
		ctx, cancel := context.WithTimeout(parent, queueIterationBudget)
		entries, err := s.deps.Store.NextWebhooks(ctx, queueBatch, s.deps.Now())
		if err != nil {
			cancel()
			s.deps.Log().Error("read the billing webhook queue", "error", err)
			return
		}
		if len(entries) == 0 {
			cancel()
			return
		}
		for _, entry := range entries {
			s.processQueued(ctx, entry)
		}
		cancel()
	}
}

// processQueued applies one delivery and acknowledges it. An error reschedules
// the entry with exponential back-off until the seven-day horizon, then
// dead-letters it with its payload intact so RetryDeadWebhooks can re-drive it.
func (s *Service) processQueued(ctx context.Context, entry platform.QueuedWebhook) {
	event, err := decodeQueuedEvent(entry.Payload)
	if err != nil {
		// The payload was verified when it was queued, so this can only be a
		// corrupt store entry. Retrying it would never succeed.
		s.deps.Log().Error("decode a queued stripe webhook", "event_id", entry.EventID, "error", err)
		s.ack(ctx, entry, outcomeIgnored, reason(err), time.Time{})
		return
	}
	outcome, err := s.applyEvent(ctx, event)
	if err != nil {
		s.rescheduleQueued(ctx, entry, event, err)
		return
	}
	if outcome == "" {
		outcome = outcomeDone
	}
	s.ack(ctx, entry, outcome, "", time.Time{})
}

func (s *Service) rescheduleQueued(ctx context.Context, entry platform.QueuedWebhook, event Event, cause error) {
	now := s.deps.Now()
	received := receivedAt(entry.Key)
	attempts := entry.Attempts + 1
	if !received.IsZero() && now.Sub(received) > deadAfter && attempts >= minAttemptsBeforeDead {
		s.ack(ctx, entry, platform.WebhookOutcomeDead, reason(cause), now)
		s.deps.Record(ctx, activity.Event{
			Actor:    activity.ActorBillingWebhook,
			Kind:     activity.KindBillingWebhookDead,
			Severity: activity.SeverityWarn,
			Summary:  "A Stripe webhook could not be applied within seven days and was set aside",
			Details: map[string]string{
				"stripe.event_id": entry.EventID,
				"stripe.type":     event.Type,
				"attempts":        strconv.Itoa(attempts),
			},
			CorrelationID: entry.EventID,
		})
		s.deps.Meter().BillingWebhook(ctx, outcomeProcessingError, event.Type, 0)
		return
	}
	s.deps.Log().Warn("apply a stripe webhook",
		"event_id", entry.EventID, "event_type", event.Type, "attempts", attempts, "error", reason(cause))
	s.ack(ctx, entry, platform.WebhookOutcomeRetry, reason(cause), now.Add(backoff(attempts)))
	s.deps.Meter().BillingWebhook(ctx, outcomeProcessingError, event.Type, 0)
}

func (s *Service) ack(ctx context.Context, entry platform.QueuedWebhook, outcome, attemptErr string, next time.Time) {
	if err := s.deps.Store.AckWebhook(ctx, entry.Key, outcome, attemptErr, next); err != nil &&
		!errors.Is(err, platform.ErrNotFound) {
		s.deps.Log().Error("acknowledge a stripe webhook", "event_id", entry.EventID, "error", err)
	}
}

// backoff doubles from five seconds to a one-hour ceiling.
func backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := retryBase
	for range attempt - 1 {
		delay *= 2
		if delay >= retryCap {
			return retryCap
		}
	}
	return delay
}

// receivedAt reads the instant a queue key encodes ("<unixms:016x>-<seq:04x>").
func receivedAt(key string) time.Time {
	milli, _, found := strings.Cut(key, "-")
	if !found {
		return time.Time{}
	}
	value, err := strconv.ParseUint(milli, 16, 64)
	if err != nil {
		return time.Time{}
	}
	return time.UnixMilli(int64(value)).UTC()
}

// decodeQueuedEvent reads the stored raw delivery. The payload is exactly the
// bytes whose signature was verified at the route.
func decodeQueuedEvent(payload []byte) (Event, error) {
	var wire struct {
		ID         string `json:"id"`
		Type       string `json:"type"`
		APIVersion string `json:"api_version"`
		Created    int64  `json:"created"`
		Livemode   bool   `json:"livemode"`
		Data       struct {
			Object json.RawMessage `json:"object"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &wire); err != nil {
		return Event{}, err
	}
	if wire.ID == "" || wire.Type == "" {
		return Event{}, fmt.Errorf("billing: a queued delivery has no event id or type")
	}
	event := Event{
		ID:         wire.ID,
		Type:       wire.Type,
		APIVersion: wire.APIVersion,
		Livemode:   wire.Livemode,
		Object:     wire.Data.Object,
	}
	if wire.Created > 0 {
		event.Created = time.Unix(wire.Created, 0).UTC()
	}
	return event, nil
}
