package platform

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

// TestWebhookReceiptIsBackfilledForAnOlderFile: the newest delivery time used
// to be recomputed by decoding the whole 30-day events bucket on every
// GetBillingStatus, which the Billing and Settings pages poll. It is now one
// meta key, written inside the transaction that accepts the delivery — so a
// file written before that key existed has to be given one at open, or the
// console would report "never" for an account that has had deliveries.
func TestWebhookReceiptIsBackfilledForAnOlderFile(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "platform.db")
	now := time.Date(2026, time.September, 3, 12, 0, 0, 0, time.UTC)
	store, err := Open(path, WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for index, receivedAt := range []time.Time{now.Add(-2 * time.Hour), now, now.Add(-time.Hour)} {
		if _, err := store.EnqueueWebhook(ctx, "evt_"+string(rune('a'+index)), "invoice.paid", []byte(`{}`), receivedAt); err != nil {
			t.Fatalf("EnqueueWebhook: %v", err)
		}
	}
	stats, err := store.WebhookStats(ctx)
	if err != nil {
		t.Fatalf("WebhookStats: %v", err)
	}
	if !stats.LastReceivedAt.Equal(now) {
		t.Fatalf("LastReceivedAt = %s, want the newest delivery %s", stats.LastReceivedAt, now)
	}

	// Put the file back into its pre-key shape and reopen it.
	if err := store.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(metaBucket).Delete(webhookReceiptKey)
	}); err != nil {
		t.Fatalf("remove the meta key: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := Open(path, WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() {
		if err := reopened.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	backfilled, err := reopened.WebhookStats(ctx)
	if err != nil {
		t.Fatalf("WebhookStats after reopen: %v", err)
	}
	if !backfilled.LastReceivedAt.Equal(now) {
		t.Errorf("LastReceivedAt after backfill = %s, want %s", backfilled.LastReceivedAt, now)
	}
}

// TestWebhookReceiptIsAbsentUntilSomethingArrives keeps the honest answer for a
// control plane that has never received a delivery: a zero time, not a
// fabricated one.
func TestWebhookReceiptIsAbsentUntilSomethingArrives(t *testing.T) {
	t.Parallel()

	store, err := Open(filepath.Join(t.TempDir(), "platform.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	stats, err := store.WebhookStats(context.Background())
	if err != nil {
		t.Fatalf("WebhookStats: %v", err)
	}
	if !stats.LastReceivedAt.IsZero() {
		t.Errorf("LastReceivedAt = %s, want the zero time", stats.LastReceivedAt)
	}
}
