package platform

import (
	"encoding/json"
	"fmt"
	"time"

	"go.etcd.io/bbolt"
)

// migrate creates the buckets and stamps the schema version. Adding a field to
// a document is a no-op migration because every reader ignores unknown fields;
// a change that needs a real migration bumps DocVersion, and a file written by
// a newer binary is refused rather than silently downgraded.
func (s *Store) migrate() error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		for _, name := range buckets {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return fmt.Errorf("create platform bucket %s: %w", name, err)
			}
		}
		if err := backfillWebhookReceipt(tx); err != nil {
			return err
		}
		meta := tx.Bucket(metaBucket)
		raw := meta.Get(schemaKey)
		if raw == nil {
			doc := metaDoc{V: DocVersion, CreatedAt: s.now().UTC()}
			payload, err := json.Marshal(doc)
			if err != nil {
				return fmt.Errorf("encode platform schema: %w", err)
			}
			return meta.Put(schemaKey, payload)
		}
		var doc metaDoc
		if err := json.Unmarshal(raw, &doc); err != nil {
			return fmt.Errorf("decode platform schema: %w", err)
		}
		if doc.V > DocVersion {
			return fmt.Errorf("platform store schema v%d is newer than this binary", doc.V)
		}
		return nil
	})
}

// backfillWebhookReceipt derives the newest delivery time from the events
// bucket for a file written before that time was kept as its own meta key. It
// runs once — after it the key exists — and reports zero deliveries honestly by
// leaving the key absent for an empty bucket.
func backfillWebhookReceipt(tx *bbolt.Tx) error {
	if tx.Bucket(metaBucket).Get(webhookReceiptKey) != nil {
		return nil
	}
	newest := time.Time{}
	cursor := tx.Bucket(webhookEventsBucket).Cursor()
	for key, value := cursor.First(); key != nil; key, value = cursor.Next() {
		var record WebhookEventDoc
		if err := json.Unmarshal(value, &record); err != nil {
			return fmt.Errorf("decode webhook event during backfill: %w", err)
		}
		if record.ReceivedAt.After(newest) {
			newest = record.ReceivedAt
		}
	}
	return recordWebhookReceipt(tx, newest)
}

// SchemaVersion reports the version stamped in the file.
func (s *Store) SchemaVersion() (int, error) {
	var doc metaDoc
	err := s.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(metaBucket).Get(schemaKey)
		if raw == nil {
			return ErrNotFound
		}
		return json.Unmarshal(raw, &doc)
	})
	return doc.V, err
}
