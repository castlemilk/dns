package platform

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/castlemilk/dns/internal/activity"
	"github.com/castlemilk/dns/internal/telemetry"
	"go.etcd.io/bbolt"
)

// ErrNotFound is the only sentinel the store returns for a missing document.
// GetEvent is the one exception: it returns activity.ErrEventNotFound, because
// the activity handler maps exactly that sentinel to CodeNotFound.
var ErrNotFound = errors.New("platform: not found")

// MaxDeployLogBytes bounds one captured build log after decompression.
const MaxDeployLogBytes = 256 << 10

var (
	metaBucket            = []byte("meta")
	sitesBucket           = []byte("sites")
	deploysBucket         = []byte("deploys")
	deploysByTimeBucket   = []byte("deploys_by_time")
	deploysByBuildBucket  = []byte("deploys_by_build")
	deployLogsBucket      = []byte("deploy_logs")
	uploadsBucket         = []byte("uploads")
	mailDomainsBucket     = []byte("mail_domains")
	billingCustomerBucket = []byte("billing_customer")
	subscriptionsBucket   = []byte("subscriptions")
	checkoutsBucket       = []byte("checkouts")
	webhookEventsBucket   = []byte("webhook_events")
	webhookQueueBucket    = []byte("webhook_queue")
	eventsBucket          = []byte("events")
	eventsZoneBucket      = []byte("events_zone")
	gatewayBucket         = []byte("gateway")
	capabilitiesBucket    = []byte("capabilities")

	buckets = [][]byte{
		metaBucket, sitesBucket, deploysBucket, deploysByTimeBucket, deploysByBuildBucket,
		deployLogsBucket, uploadsBucket, mailDomainsBucket, billingCustomerBucket,
		subscriptionsBucket, checkoutsBucket, webhookEventsBucket, webhookQueueBucket,
		eventsBucket, eventsZoneBucket, gatewayBucket, capabilitiesBucket,
	}

	schemaKey = []byte("schema")
	// webhookReceiptKey holds the newest webhook delivery time, so WebhookStats
	// does not have to scan the events bucket to find it.
	webhookReceiptKey = []byte("webhook_last_received_at")
	gatewayKey        = []byte("addresses")
	hostingKey        = []byte("hosting")
	customerKey       = []byte("default")
)

// Store is the platform state file. Every method takes a context, checks it
// first, and reports one bounded store metric.
type Store struct {
	db      *bbolt.DB
	path    string
	now     func() time.Time
	metrics *telemetry.Metrics

	// idMu makes "<unixms>-<seq>" monotonic within a millisecond for events and
	// webhook queue keys, which are ordered by that string.
	idMu         sync.Mutex
	lastIDMS     int64
	lastIDSeq    uint16
	lastQueueMS  int64
	lastQueueSeq uint16
}

// Option customises a Store at open time.
type Option func(*Store)

// WithMetrics installs the instrument set. Without it the store is silent.
func WithMetrics(metrics *telemetry.Metrics) Option {
	return func(s *Store) { s.metrics = telemetry.Select(metrics) }
}

// WithClock replaces the store's clock. Tests use it for deterministic ids.
func WithClock(now func() time.Time) Option {
	return func(s *Store) {
		if now != nil {
			s.now = now
		}
	}
}

// Open creates or opens the platform store. It refuses a file written by a
// newer schema rather than silently dropping fields it cannot represent.
func Open(path string, options ...Option) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("platform store path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("create platform data directory: %w", err)
	}
	db, err := bbolt.Open(path, 0o600, &bbolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open platform database: %w", err)
	}
	store := &Store{db: db, path: path, now: time.Now, metrics: telemetry.Disabled()}
	for _, option := range options {
		option(store)
	}
	if err := store.migrate(); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return store, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Path() string { return s.path }

// Backup streams a consistent copy of the file to w from inside a read
// transaction. The running process holds bbolt's exclusive lock, so this is the
// only way to take a backup without stopping the control plane.
func (s *Store) Backup(ctx context.Context, w io.Writer) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	var written int64
	err := s.viewTx(ctx, "platform_backup", func(tx *bbolt.Tx) error {
		count, err := tx.WriteTo(w)
		written = count
		return err
	})
	return written, err
}

// Size reports the on-disk size of the current transaction's view, which is
// what Backup will write.
func (s *Store) Size(ctx context.Context) (int64, error) {
	var size int64
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		size = tx.Size()
		return nil
	})
	return size, err
}

// EmptyForRebuild reports whether the three reconstructable buckets are empty.
// RebuildPlatformStore refuses to run otherwise: rebuilding over live rows
// would silently replace an operator's state with the engines' view of it.
func (s *Store) EmptyForRebuild(ctx context.Context) (bool, error) {
	empty := true
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		for _, name := range [][]byte{sitesBucket, mailDomainsBucket, subscriptionsBucket} {
			if bucket := tx.Bucket(name); bucket != nil && bucket.Stats().KeyN > 0 {
				empty = false
				return nil
			}
		}
		return nil
	})
	return empty, err
}

// --- sites ---------------------------------------------------------------

func (s *Store) PutSite(ctx context.Context, doc SiteDoc) error {
	if strings.TrimSpace(doc.ZoneID) == "" {
		return errors.New("platform: site zone id is required")
	}
	doc.V = DocVersion
	return s.update(ctx, func(tx *bbolt.Tx) error {
		return putJSON(tx, sitesBucket, []byte(doc.ZoneID), doc)
	})
}

func (s *Store) GetSite(ctx context.Context, zoneID string) (SiteDoc, error) {
	var doc SiteDoc
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		return getJSON(tx, sitesBucket, []byte(zoneID), &doc)
	})
	return doc, err
}

func (s *Store) ListSites(ctx context.Context) ([]SiteDoc, error) {
	var docs []SiteDoc
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		return eachJSON(tx, sitesBucket, func(_ []byte, decode func(any) error) error {
			var doc SiteDoc
			if err := decode(&doc); err != nil {
				return err
			}
			docs = append(docs, doc)
			return nil
		})
	})
	return docs, err
}

func (s *Store) DeleteSite(ctx context.Context, zoneID string) error {
	return s.update(ctx, func(tx *bbolt.Tx) error {
		return tx.Bucket(sitesBucket).Delete([]byte(zoneID))
	})
}

// --- deploys -------------------------------------------------------------

// NewDeployID returns a lexicographically time-ordered id, so a reverse cursor
// over deploys_by_time is a newest-first listing with no secondary sort.
func (s *Store) NewDeployID(now time.Time) string {
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		// crypto/rand cannot fail on any supported platform; falling back to
		// the clock keeps the id unique enough to be traceable if it ever does.
		return fmt.Sprintf("%016x-%08x", now.UTC().UnixMilli(), now.UTC().UnixNano()&0xffffffff)
	}
	return fmt.Sprintf("%016x-%s", now.UTC().UnixMilli(), hex.EncodeToString(suffix))
}

func deployKey(zoneID, deployID string) []byte {
	return []byte(zoneID + "/" + deployID)
}

func (s *Store) PutDeploy(ctx context.Context, doc DeployDoc) error {
	if strings.TrimSpace(doc.ZoneID) == "" || strings.TrimSpace(doc.ID) == "" {
		return errors.New("platform: deploy zone id and id are required")
	}
	doc.V = DocVersion
	return s.update(ctx, func(tx *bbolt.Tx) error {
		if err := putJSON(tx, deploysBucket, deployKey(doc.ZoneID, doc.ID), doc); err != nil {
			return err
		}
		if err := tx.Bucket(deploysByTimeBucket).Put([]byte(doc.ID), []byte(doc.ZoneID)); err != nil {
			return err
		}
		if doc.BuildID != "" {
			key := []byte(doc.BuildID + "/" + doc.ID)
			if err := tx.Bucket(deploysByBuildBucket).Put(key, []byte(doc.ZoneID)); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) GetDeploy(ctx context.Context, zoneID, deployID string) (DeployDoc, error) {
	var doc DeployDoc
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		return getJSON(tx, deploysBucket, deployKey(zoneID, deployID), &doc)
	})
	return doc, err
}

// ListDeploys pages newest first. zoneID "" lists every zone through the
// deploys_by_time index; cursor is the id to continue before.
func (s *Store) ListDeploys(ctx context.Context, zoneID string, limit int, cursor string) ([]DeployDoc, string, error) {
	if limit <= 0 {
		limit = 50
	}
	var (
		docs []DeployDoc
		next string
	)
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		if zoneID != "" {
			return eachDescending(tx.Bucket(deploysBucket), []byte(zoneID+"/"), cursorKey(zoneID, cursor), limit,
				func(_ []byte, value []byte) (bool, error) {
					var doc DeployDoc
					if err := json.Unmarshal(value, &doc); err != nil {
						return false, err
					}
					docs = append(docs, doc)
					return true, nil
				}, &next)
		}
		index := tx.Bucket(deploysByTimeBucket)
		deploys := tx.Bucket(deploysBucket)
		return eachDescending(index, nil, []byte(cursor), limit, func(key, value []byte) (bool, error) {
			raw := deploys.Get(deployKey(string(value), string(key)))
			if raw == nil {
				return false, nil
			}
			var doc DeployDoc
			if err := json.Unmarshal(raw, &doc); err != nil {
				return false, err
			}
			docs = append(docs, doc)
			return true, nil
		}, &next)
	})
	if err != nil {
		return nil, "", err
	}
	if next != "" && zoneID != "" {
		_, next, _ = strings.Cut(next, "/")
	}
	return docs, next, nil
}

func cursorKey(zoneID, cursor string) []byte {
	if cursor == "" {
		return nil
	}
	return deployKey(zoneID, cursor)
}

// DeploysByBuild returns every deploy row that shares one content-addressed
// build id, oldest first.
func (s *Store) DeploysByBuild(ctx context.Context, buildID string) ([]DeployDoc, error) {
	if strings.TrimSpace(buildID) == "" {
		return nil, nil
	}
	var docs []DeployDoc
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		deploys := tx.Bucket(deploysBucket)
		prefix := []byte(buildID + "/")
		cursor := tx.Bucket(deploysByBuildBucket).Cursor()
		for key, value := cursor.Seek(prefix); key != nil && bytes.HasPrefix(key, prefix); key, value = cursor.Next() {
			_, deployID, _ := strings.Cut(string(key), "/")
			raw := deploys.Get(deployKey(string(value), deployID))
			if raw == nil {
				continue
			}
			var doc DeployDoc
			if err := json.Unmarshal(raw, &doc); err != nil {
				return err
			}
			docs = append(docs, doc)
		}
		return nil
	})
	return docs, err
}

// ActiveDeploys returns every row the build poller still owns.
func (s *Store) ActiveDeploys(ctx context.Context) ([]DeployDoc, error) {
	var docs []DeployDoc
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		return eachJSON(tx, deploysBucket, func(_ []byte, decode func(any) error) error {
			var doc DeployDoc
			if err := decode(&doc); err != nil {
				return err
			}
			if doc.Active() {
				docs = append(docs, doc)
			}
			return nil
		})
	})
	return docs, err
}

func (s *Store) PutDeployLog(ctx context.Context, deployID string, log DeployLog) error {
	if strings.TrimSpace(deployID) == "" {
		return errors.New("platform: deploy id is required")
	}
	doc := deployLogDoc{
		V:          DocVersion,
		Lines:      log.Lines,
		Complete:   log.Complete,
		Truncated:  log.Truncated,
		CapturedAt: log.CapturedAt,
	}
	kept, dropped, err := fitLogLines(doc.Lines, MaxDeployLogBytes)
	if err != nil {
		return err
	}
	doc.Lines = kept
	if dropped {
		doc.Truncated = true
	}
	payload, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("encode deploy log: %w", err)
	}
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(payload); err != nil {
		return fmt.Errorf("compress deploy log: %w", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("compress deploy log: %w", err)
	}
	return s.update(ctx, func(tx *bbolt.Tx) error {
		return tx.Bucket(deployLogsBucket).Put([]byte(deployID), compressed.Bytes())
	})
}

// logEnvelopeBytes is the JSON overhead of a deploy log with no lines. It is a
// generous constant so the budget below never has to marshal twice.
const logEnvelopeBytes = 160

// fitLogLines keeps the newest lines that fit inside the budget, dropping whole
// lines from the front rather than storing half a line that would read like the
// build produced it. It encodes each line once, so a runaway build costs one
// pass, not one pass per dropped line.
func fitLogLines(lines []string, budget int) ([]string, bool, error) {
	remaining := budget - logEnvelopeBytes
	if remaining < 0 {
		remaining = 0
	}
	for index := len(lines) - 1; index >= 0; index-- {
		encoded, err := json.Marshal(lines[index])
		if err != nil {
			return nil, false, fmt.Errorf("encode deploy log line: %w", err)
		}
		cost := len(encoded) + 1 // the separating comma
		if cost > remaining {
			return lines[index+1:], true, nil
		}
		remaining -= cost
	}
	return lines, false, nil
}

func (s *Store) GetDeployLog(ctx context.Context, deployID string) (DeployLog, error) {
	var log DeployLog
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		raw := tx.Bucket(deployLogsBucket).Get([]byte(deployID))
		if raw == nil {
			return ErrNotFound
		}
		reader, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return fmt.Errorf("read deploy log: %w", err)
		}
		payload, readErr := io.ReadAll(io.LimitReader(reader, MaxDeployLogBytes+1))
		closeErr := reader.Close()
		if readErr != nil {
			return fmt.Errorf("read deploy log: %w", readErr)
		}
		if closeErr != nil {
			return fmt.Errorf("read deploy log: %w", closeErr)
		}
		var doc deployLogDoc
		if err := json.Unmarshal(payload, &doc); err != nil {
			return fmt.Errorf("decode deploy log: %w", err)
		}
		log = DeployLog{Lines: doc.Lines, Complete: doc.Complete, Truncated: doc.Truncated, CapturedAt: doc.CapturedAt}
		return nil
	})
	return log, err
}

// --- uploads -------------------------------------------------------------

// uploadIDPattern is the shape every upload id has: the same
// "<unixms:016x>-<8 hex>" form as a deploy id. It is anchored and hex-only so
// the janitor can decide from a directory name alone whether the facade
// created it — HOSTING_UPLOAD_DIR may be a directory shared with something
// else, and a sweep must never touch a stranger's files.
var uploadIDPattern = regexp.MustCompile(`^[0-9a-f]{16}-[0-9a-f]{8}$`)

// UploadIDPattern is the source of truth for the upload id shape, quoted in
// docs and asserted by tests.
const UploadIDPattern = `^[0-9a-f]{16}-[0-9a-f]{8}$`

// ValidUploadID reports whether id has the shape NewUploadID issues. The
// upload handler validates the id it parses out of a request with it, and the
// janitor uses it to decide whether a directory is one of ours.
func ValidUploadID(id string) bool { return uploadIDPattern.MatchString(id) }

// NewUploadID mints an upload id. It shares NewDeployID's shape so both are
// sortable by time and recognisable on sight.
func (s *Store) NewUploadID(now time.Time) string { return s.NewDeployID(now) }

func (s *Store) PutUpload(ctx context.Context, doc UploadDoc) error {
	if strings.TrimSpace(doc.ID) == "" {
		return errors.New("platform: upload id is required")
	}
	if !ValidUploadID(doc.ID) {
		return fmt.Errorf("platform: upload id must match %s", UploadIDPattern)
	}
	doc.V = DocVersion
	return s.update(ctx, func(tx *bbolt.Tx) error {
		return putJSON(tx, uploadsBucket, []byte(doc.ID), doc)
	})
}

func (s *Store) GetUpload(ctx context.Context, id string) (UploadDoc, error) {
	var doc UploadDoc
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		return getJSON(tx, uploadsBucket, []byte(id), &doc)
	})
	return doc, err
}

func (s *Store) DeleteUpload(ctx context.Context, id string) error {
	return s.update(ctx, func(tx *bbolt.Tx) error {
		return tx.Bucket(uploadsBucket).Delete([]byte(id))
	})
}

// ListUploads returns every staged upload, expired or not. The upload
// handler's in-flight byte cap is derived from these rows rather than from a
// counter, so it is exact after a janitor sweep and after a restart.
func (s *Store) ListUploads(ctx context.Context) ([]UploadDoc, error) {
	var docs []UploadDoc
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		return eachJSON(tx, uploadsBucket, func(_ []byte, decode func(any) error) error {
			var doc UploadDoc
			if err := decode(&doc); err != nil {
				return err
			}
			docs = append(docs, doc)
			return nil
		})
	})
	return docs, err
}

func (s *Store) ListExpiredUploads(ctx context.Context, now time.Time) ([]UploadDoc, error) {
	var docs []UploadDoc
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		return eachJSON(tx, uploadsBucket, func(_ []byte, decode func(any) error) error {
			var doc UploadDoc
			if err := decode(&doc); err != nil {
				return err
			}
			if !doc.ExpiresAt.IsZero() && !doc.ExpiresAt.After(now) {
				docs = append(docs, doc)
			}
			return nil
		})
	})
	return docs, err
}

// --- mail ----------------------------------------------------------------

func (s *Store) PutMailDomain(ctx context.Context, doc MailDomainDoc) error {
	if strings.TrimSpace(doc.ZoneID) == "" {
		return errors.New("platform: mail domain zone id is required")
	}
	doc.V = DocVersion
	return s.update(ctx, func(tx *bbolt.Tx) error {
		return putJSON(tx, mailDomainsBucket, []byte(doc.ZoneID), doc)
	})
}

func (s *Store) GetMailDomain(ctx context.Context, zoneID string) (MailDomainDoc, error) {
	var doc MailDomainDoc
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		return getJSON(tx, mailDomainsBucket, []byte(zoneID), &doc)
	})
	return doc, err
}

func (s *Store) ListMailDomains(ctx context.Context) ([]MailDomainDoc, error) {
	var docs []MailDomainDoc
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		return eachJSON(tx, mailDomainsBucket, func(_ []byte, decode func(any) error) error {
			var doc MailDomainDoc
			if err := decode(&doc); err != nil {
				return err
			}
			docs = append(docs, doc)
			return nil
		})
	})
	return docs, err
}

func (s *Store) DeleteMailDomain(ctx context.Context, zoneID string) error {
	return s.update(ctx, func(tx *bbolt.Tx) error {
		return tx.Bucket(mailDomainsBucket).Delete([]byte(zoneID))
	})
}

// --- billing -------------------------------------------------------------

func (s *Store) GetCustomer(ctx context.Context) (CustomerDoc, bool, error) {
	var (
		doc   CustomerDoc
		found bool
	)
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		err := getJSON(tx, billingCustomerBucket, customerKey, &doc)
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		found = err == nil
		return err
	})
	return doc, found, err
}

func (s *Store) PutCustomer(ctx context.Context, doc CustomerDoc) error {
	doc.V = DocVersion
	return s.update(ctx, func(tx *bbolt.Tx) error {
		return putJSON(tx, billingCustomerBucket, customerKey, doc)
	})
}

func (s *Store) PutSubscription(ctx context.Context, doc SubscriptionDoc) error {
	if strings.TrimSpace(doc.ZoneID) == "" {
		return errors.New("platform: subscription zone id is required")
	}
	doc.V = DocVersion
	return s.update(ctx, func(tx *bbolt.Tx) error {
		return putJSON(tx, subscriptionsBucket, []byte(doc.ZoneID), doc)
	})
}

func (s *Store) GetSubscription(ctx context.Context, zoneID string) (SubscriptionDoc, error) {
	var doc SubscriptionDoc
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		return getJSON(tx, subscriptionsBucket, []byte(zoneID), &doc)
	})
	return doc, err
}

func (s *Store) ListSubscriptions(ctx context.Context) ([]SubscriptionDoc, error) {
	var docs []SubscriptionDoc
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		return eachJSON(tx, subscriptionsBucket, func(_ []byte, decode func(any) error) error {
			var doc SubscriptionDoc
			if err := decode(&doc); err != nil {
				return err
			}
			docs = append(docs, doc)
			return nil
		})
	})
	return docs, err
}

func (s *Store) DeleteSubscription(ctx context.Context, zoneID string) error {
	return s.update(ctx, func(tx *bbolt.Tx) error {
		return tx.Bucket(subscriptionsBucket).Delete([]byte(zoneID))
	})
}

func (s *Store) PutCheckout(ctx context.Context, doc CheckoutDoc) error {
	if strings.TrimSpace(doc.SessionID) == "" {
		return errors.New("platform: checkout session id is required")
	}
	doc.V = DocVersion
	return s.update(ctx, func(tx *bbolt.Tx) error {
		return putJSON(tx, checkoutsBucket, []byte(doc.SessionID), doc)
	})
}

func (s *Store) GetCheckout(ctx context.Context, sessionID string) (CheckoutDoc, error) {
	var doc CheckoutDoc
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		return getJSON(tx, checkoutsBucket, []byte(sessionID), &doc)
	})
	return doc, err
}

// DeleteCheckoutsBefore drops checkout rows created before the cutoff. Every
// CreateCheckoutSession writes one, and an abandoned checkout is never
// completed, so without this the bucket is the only unbounded one in the store
// and ListPendingCheckouts scans more of it every month. The retention has to
// outlive any session ConfirmCheckout might still be asked about, which is what
// makes its "did this control plane create it?" check meaningful.
func (s *Store) DeleteCheckoutsBefore(ctx context.Context, cutoff time.Time) (int, error) {
	count := 0
	err := s.update(ctx, func(tx *bbolt.Tx) error {
		checkouts := tx.Bucket(checkoutsBucket)
		var expired [][]byte
		cursor := checkouts.Cursor()
		for key, value := cursor.First(); key != nil; key, value = cursor.Next() {
			var doc CheckoutDoc
			if err := json.Unmarshal(value, &doc); err != nil {
				return err
			}
			if doc.CreatedAt.Before(cutoff) {
				expired = append(expired, append([]byte(nil), key...))
			}
		}
		for _, key := range expired {
			if err := checkouts.Delete(key); err != nil {
				return err
			}
			count++
		}
		return nil
	})
	return count, err
}

func (s *Store) ListPendingCheckouts(ctx context.Context) ([]CheckoutDoc, error) {
	var docs []CheckoutDoc
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		return eachJSON(tx, checkoutsBucket, func(_ []byte, decode func(any) error) error {
			var doc CheckoutDoc
			if err := decode(&doc); err != nil {
				return err
			}
			if !doc.Completed {
				docs = append(docs, doc)
			}
			return nil
		})
	})
	return docs, err
}

// EnqueueWebhook records an event id and queues its payload. It reports false
// without writing anything when the id was already seen, which is what makes a
// Stripe redelivery a duplicate rather than a second application.
func (s *Store) EnqueueWebhook(ctx context.Context, eventID, eventType string, payload []byte, receivedAt time.Time) (bool, error) {
	if strings.TrimSpace(eventID) == "" {
		return false, errors.New("platform: webhook event id is required")
	}
	key := s.nextQueueKey(receivedAt)
	queued := false
	err := s.update(ctx, func(tx *bbolt.Tx) error {
		events := tx.Bucket(webhookEventsBucket)
		if events.Get([]byte(eventID)) != nil {
			return nil
		}
		record := WebhookEventDoc{
			V:          DocVersion,
			Type:       eventType,
			ReceivedAt: receivedAt,
			Outcome:    WebhookOutcomeQueued,
		}
		if err := putJSON(tx, webhookEventsBucket, []byte(eventID), record); err != nil {
			return err
		}
		entry := QueuedWebhook{V: DocVersion, EventID: eventID, Payload: payload, NextAttemptAt: receivedAt}
		if err := putJSON(tx, webhookQueueBucket, []byte(key), entry); err != nil {
			return err
		}
		if err := recordWebhookReceipt(tx, receivedAt); err != nil {
			return err
		}
		queued = true
		return nil
	})
	return queued, err
}

// recordWebhookReceipt keeps the newest delivery time in one meta key. It is
// written inside the transaction that accepts the delivery, so it cannot drift
// from the events bucket, and it lets WebhookStats answer without decoding a
// month of deliveries on a route the console polls.
func recordWebhookReceipt(tx *bbolt.Tx, receivedAt time.Time) error {
	if receivedAt.IsZero() {
		return nil
	}
	previous, err := lastWebhookReceipt(tx)
	if err != nil {
		return err
	}
	if !receivedAt.After(previous) {
		return nil
	}
	payload, err := receivedAt.UTC().MarshalText()
	if err != nil {
		return fmt.Errorf("encode webhook receipt time: %w", err)
	}
	return tx.Bucket(metaBucket).Put(webhookReceiptKey, payload)
}

func lastWebhookReceipt(tx *bbolt.Tx) (time.Time, error) {
	raw := tx.Bucket(metaBucket).Get(webhookReceiptKey)
	if raw == nil {
		return time.Time{}, nil
	}
	var value time.Time
	if err := value.UnmarshalText(raw); err != nil {
		return time.Time{}, fmt.Errorf("decode webhook receipt time: %w", err)
	}
	return value, nil
}

// NextWebhooks returns up to n due, non-dead entries in key order.
func (s *Store) NextWebhooks(ctx context.Context, n int, now time.Time) ([]QueuedWebhook, error) {
	if n <= 0 {
		n = 16
	}
	var entries []QueuedWebhook
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		cursor := tx.Bucket(webhookQueueBucket).Cursor()
		for key, value := cursor.First(); key != nil && len(entries) < n; key, value = cursor.Next() {
			var entry QueuedWebhook
			if err := json.Unmarshal(value, &entry); err != nil {
				return err
			}
			if entry.Dead || entry.NextAttemptAt.After(now) {
				continue
			}
			entry.Key = string(key)
			entries = append(entries, entry)
		}
		return nil
	})
	return entries, err
}

// AckWebhook records the result of one attempt. done/stale/ignored remove the
// queue entry; retry reschedules it; dead keeps the payload so
// RetryDeadWebhooks can re-drive it after an operator fixes the cause.
func (s *Store) AckWebhook(ctx context.Context, key, outcome, attemptErr string, nextAttemptAt time.Time) error {
	if strings.TrimSpace(key) == "" {
		return errors.New("platform: webhook queue key is required")
	}
	now := s.now().UTC()
	return s.update(ctx, func(tx *bbolt.Tx) error {
		queue := tx.Bucket(webhookQueueBucket)
		raw := queue.Get([]byte(key))
		if raw == nil {
			return ErrNotFound
		}
		var entry QueuedWebhook
		if err := json.Unmarshal(raw, &entry); err != nil {
			return err
		}
		entry.Attempts++

		var record WebhookEventDoc
		if err := getJSON(tx, webhookEventsBucket, []byte(entry.EventID), &record); err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		record.V = DocVersion
		record.Outcome = outcome
		record.Attempts = entry.Attempts
		record.Error = attemptErr

		switch outcome {
		case WebhookOutcomeRetry:
			entry.NextAttemptAt = nextAttemptAt
			record.NextAttemptAt = nextAttemptAt
			if err := putJSON(tx, webhookQueueBucket, []byte(key), entry); err != nil {
				return err
			}
		case WebhookOutcomeDead:
			entry.Dead = true
			entry.NextAttemptAt = nextAttemptAt
			record.ProcessedAt = now
			if err := putJSON(tx, webhookQueueBucket, []byte(key), entry); err != nil {
				return err
			}
		default:
			record.ProcessedAt = now
			if err := queue.Delete([]byte(key)); err != nil {
				return err
			}
		}
		return putJSON(tx, webhookEventsBucket, []byte(entry.EventID), record)
	})
}

// RetryDeadWebhooks re-queues every dead entry with its attempt count reset.
func (s *Store) RetryDeadWebhooks(ctx context.Context) (int, error) {
	now := s.now().UTC()
	count := 0
	err := s.update(ctx, func(tx *bbolt.Tx) error {
		queue := tx.Bucket(webhookQueueBucket)
		cursor := queue.Cursor()
		type change struct {
			key   []byte
			entry QueuedWebhook
		}
		var changes []change
		for key, value := cursor.First(); key != nil; key, value = cursor.Next() {
			var entry QueuedWebhook
			if err := json.Unmarshal(value, &entry); err != nil {
				return err
			}
			if !entry.Dead {
				continue
			}
			entry.Dead = false
			entry.Attempts = 0
			entry.NextAttemptAt = now
			changes = append(changes, change{key: append([]byte(nil), key...), entry: entry})
		}
		for _, item := range changes {
			if err := putJSON(tx, webhookQueueBucket, item.key, item.entry); err != nil {
				return err
			}
			var record WebhookEventDoc
			if err := getJSON(tx, webhookEventsBucket, []byte(item.entry.EventID), &record); err != nil && !errors.Is(err, ErrNotFound) {
				return err
			}
			record.V = DocVersion
			record.Outcome = WebhookOutcomeQueued
			record.Attempts = 0
			record.Error = ""
			record.NextAttemptAt = now
			if err := putJSON(tx, webhookEventsBucket, []byte(item.entry.EventID), record); err != nil {
				return err
			}
			count++
		}
		return nil
	})
	return count, err
}

func (s *Store) WebhookStats(ctx context.Context) (WebhookStats, error) {
	var stats WebhookStats
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		cursor := tx.Bucket(webhookQueueBucket).Cursor()
		for key, value := cursor.First(); key != nil; key, value = cursor.Next() {
			var entry QueuedWebhook
			if err := json.Unmarshal(value, &entry); err != nil {
				return err
			}
			if entry.Dead {
				stats.Dead++
				continue
			}
			stats.Pending++
		}
		// The newest delivery time is kept as one meta key by EnqueueWebhook.
		// It used to be recomputed by decoding the whole 30-day events bucket
		// on every GetBillingStatus, which the Billing and Settings pages poll.
		received, err := lastWebhookReceipt(tx)
		if err != nil {
			return err
		}
		stats.LastReceivedAt = received
		return nil
	})
	return stats, err
}

// DeleteWebhookEventsBefore drops replay guards older than the cutoff along
// with any dead queue entry that references them.
func (s *Store) DeleteWebhookEventsBefore(ctx context.Context, cutoff time.Time) (int, error) {
	count := 0
	err := s.update(ctx, func(tx *bbolt.Tx) error {
		events := tx.Bucket(webhookEventsBucket)
		queue := tx.Bucket(webhookQueueBucket)
		var expired []string
		cursor := events.Cursor()
		for key, value := cursor.First(); key != nil; key, value = cursor.Next() {
			var record WebhookEventDoc
			if err := json.Unmarshal(value, &record); err != nil {
				return err
			}
			if record.ReceivedAt.Before(cutoff) {
				expired = append(expired, string(key))
			}
		}
		dropped := make(map[string]struct{}, len(expired))
		for _, id := range expired {
			if err := events.Delete([]byte(id)); err != nil {
				return err
			}
			dropped[id] = struct{}{}
			count++
		}
		queueCursor := queue.Cursor()
		var stale [][]byte
		for key, value := queueCursor.First(); key != nil; key, value = queueCursor.Next() {
			var entry QueuedWebhook
			if err := json.Unmarshal(value, &entry); err != nil {
				return err
			}
			if _, ok := dropped[entry.EventID]; ok {
				stale = append(stale, append([]byte(nil), key...))
			}
		}
		for _, key := range stale {
			if err := queue.Delete(key); err != nil {
				return err
			}
		}
		return nil
	})
	return count, err
}

// --- events (activity.EventStore) ----------------------------------------

func (s *Store) AppendEvent(ctx context.Context, doc activity.EventDoc) (string, error) {
	doc.V = activity.DocVersion
	if doc.Time.IsZero() {
		doc.Time = s.now().UTC()
	}
	doc.ID = s.nextEventID(doc.Time)
	err := s.update(ctx, func(tx *bbolt.Tx) error {
		if err := putJSON(tx, eventsBucket, []byte(doc.ID), doc); err != nil {
			return err
		}
		if doc.ZoneID != "" {
			if err := tx.Bucket(eventsZoneBucket).Put([]byte(doc.ZoneID+"/"+doc.ID), nil); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return doc.ID, nil
}

func (s *Store) ListEvents(ctx context.Context, filter activity.ListFilter) ([]activity.EventDoc, string, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}
	var (
		docs []activity.EventDoc
		next string
	)
	sinceKey := ""
	if !filter.Since.IsZero() {
		sinceKey = fmt.Sprintf("%016x", filter.Since.UTC().UnixMilli())
	}
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		events := tx.Bucket(eventsBucket)
		accept := func(raw []byte) (activity.EventDoc, bool, error) {
			var doc activity.EventDoc
			if err := json.Unmarshal(raw, &doc); err != nil {
				return doc, false, err
			}
			if !filter.MatchesKind(doc.Kind) {
				return doc, false, nil
			}
			return doc, true, nil
		}
		if filter.ZoneID != "" {
			prefix := []byte(filter.ZoneID + "/")
			var start []byte
			if filter.Cursor != "" {
				start = []byte(filter.ZoneID + "/" + filter.Cursor)
			}
			return eachDescending(tx.Bucket(eventsZoneBucket), prefix, start, limit, func(key, _ []byte) (bool, error) {
				_, id, _ := strings.Cut(string(key), "/")
				if sinceKey != "" && id < sinceKey {
					return false, errStopIteration
				}
				raw := events.Get([]byte(id))
				if raw == nil {
					return false, nil
				}
				doc, ok, err := accept(raw)
				if err != nil || !ok {
					return false, err
				}
				docs = append(docs, doc)
				return true, nil
			}, &next)
		}
		return eachDescending(events, nil, []byte(filter.Cursor), limit, func(key, value []byte) (bool, error) {
			if sinceKey != "" && string(key) < sinceKey {
				return false, errStopIteration
			}
			doc, ok, err := accept(value)
			if err != nil || !ok {
				return false, err
			}
			docs = append(docs, doc)
			return true, nil
		}, &next)
	})
	if err != nil {
		return nil, "", err
	}
	if next != "" && filter.ZoneID != "" {
		_, next, _ = strings.Cut(next, "/")
	}
	return docs, next, nil
}

func (s *Store) GetEvent(ctx context.Context, id string) (activity.EventDoc, error) {
	var doc activity.EventDoc
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		raw := tx.Bucket(eventsBucket).Get([]byte(id))
		if raw == nil {
			return activity.ErrEventNotFound
		}
		return json.Unmarshal(raw, &doc)
	})
	return doc, err
}

func (s *Store) CountEvents(ctx context.Context) (uint64, error) {
	var count uint64
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		count = uint64(max(tx.Bucket(eventsBucket).Stats().KeyN, 0))
		return nil
	})
	return count, err
}

// PruneEvents trims the ring to the newest keep events, dropping their zone
// index entries with them.
func (s *Store) PruneEvents(ctx context.Context, keep int) (int, error) {
	if keep <= 0 {
		keep = activity.DefaultMaxEvents
	}
	removed := 0
	err := s.update(ctx, func(tx *bbolt.Tx) error {
		events := tx.Bucket(eventsBucket)
		zoneIndex := tx.Bucket(eventsZoneBucket)
		total := events.Stats().KeyN
		if total <= keep {
			return nil
		}
		excess := total - keep
		cursor := events.Cursor()
		type doomed struct {
			id     []byte
			zoneID string
		}
		victims := make([]doomed, 0, excess)
		for key, value := cursor.First(); key != nil && len(victims) < excess; key, value = cursor.Next() {
			var doc activity.EventDoc
			if err := json.Unmarshal(value, &doc); err != nil {
				return err
			}
			victims = append(victims, doomed{id: append([]byte(nil), key...), zoneID: doc.ZoneID})
		}
		for _, victim := range victims {
			if err := events.Delete(victim.id); err != nil {
				return err
			}
			if victim.zoneID != "" {
				if err := zoneIndex.Delete([]byte(victim.zoneID + "/" + string(victim.id))); err != nil {
					return err
				}
			}
			removed++
		}
		return nil
	})
	return removed, err
}

// --- gateway, capabilities, bindings -------------------------------------

func (s *Store) GetGateway(ctx context.Context) (GatewayDoc, error) {
	var doc GatewayDoc
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		err := getJSON(tx, gatewayBucket, gatewayKey, &doc)
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	})
	return doc, err
}

func (s *Store) PutGateway(ctx context.Context, doc GatewayDoc) error {
	doc.V = DocVersion
	return s.update(ctx, func(tx *bbolt.Tx) error {
		return putJSON(tx, gatewayBucket, gatewayKey, doc)
	})
}

func (s *Store) GetCapabilities(ctx context.Context) (CapabilitiesDoc, error) {
	var doc CapabilitiesDoc
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		err := getJSON(tx, capabilitiesBucket, hostingKey, &doc)
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	})
	return doc, err
}

func (s *Store) PutCapabilities(ctx context.Context, doc CapabilitiesDoc) error {
	doc.V = DocVersion
	return s.update(ctx, func(tx *bbolt.Tx) error {
		return putJSON(tx, capabilitiesBucket, hostingKey, doc)
	})
}

// HasBindings reports whether a zone still carries a site or a mail binding.
// control.Handler refuses DeleteZone while either is true.
func (s *Store) HasBindings(ctx context.Context, zoneID string) (bool, bool, error) {
	var site, mail bool
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		site = tx.Bucket(sitesBucket).Get([]byte(zoneID)) != nil
		mail = tx.Bucket(mailDomainsBucket).Get([]byte(zoneID)) != nil
		return nil
	})
	return site, mail, err
}

var _ activity.EventStore = (*Store)(nil)

// --- id allocation -------------------------------------------------------

func (s *Store) nextEventID(now time.Time) string {
	s.idMu.Lock()
	defer s.idMu.Unlock()
	milli := now.UTC().UnixMilli()
	if milli == s.lastIDMS {
		s.lastIDSeq++
	} else {
		s.lastIDMS = milli
		s.lastIDSeq = 0
	}
	return fmt.Sprintf("%016x-%04x", milli, s.lastIDSeq)
}

func (s *Store) nextQueueKey(now time.Time) string {
	s.idMu.Lock()
	defer s.idMu.Unlock()
	milli := now.UTC().UnixMilli()
	if milli == s.lastQueueMS {
		s.lastQueueSeq++
	} else {
		s.lastQueueMS = milli
		s.lastQueueSeq = 0
	}
	return fmt.Sprintf("%016x-%04x", milli, s.lastQueueSeq)
}

// --- transaction helpers -------------------------------------------------

var errStopIteration = errors.New("platform: stop iteration")

func (s *Store) view(ctx context.Context, transaction func(*bbolt.Tx) error) error {
	return s.viewTx(ctx, "platform_read", transaction)
}

func (s *Store) update(ctx context.Context, transaction func(*bbolt.Tx) error) error {
	return s.updateTx(ctx, "platform_write", transaction)
}

func (s *Store) viewTx(ctx context.Context, operation string, transaction func(*bbolt.Tx) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	started := time.Time{}
	if s.metrics.MetricsEnabled() {
		started = time.Now()
	}
	err := s.db.View(transaction)
	s.observe(ctx, operation, "read", err, started)
	return err
}

func (s *Store) updateTx(ctx context.Context, operation string, transaction func(*bbolt.Tx) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	started := time.Time{}
	if s.metrics.MetricsEnabled() {
		started = time.Now()
	}
	err := s.db.Update(transaction)
	s.observe(ctx, operation, "write", err, started)
	return err
}

func (s *Store) observe(ctx context.Context, operation, transactionType string, err error, started time.Time) {
	outcome := "success"
	if err != nil {
		outcome = "error"
	}
	elapsed := time.Duration(0)
	if !started.IsZero() {
		elapsed = time.Since(started)
	}
	s.metrics.StoreTransaction(ctx, operation, transactionType, outcome, elapsed)
}

func putJSON(tx *bbolt.Tx, bucket, key []byte, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode %s document: %w", bucket, err)
	}
	return tx.Bucket(bucket).Put(key, payload)
}

func getJSON(tx *bbolt.Tx, bucket, key []byte, target any) error {
	raw := tx.Bucket(bucket).Get(key)
	if raw == nil {
		return ErrNotFound
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return fmt.Errorf("decode %s document: %w", bucket, err)
	}
	return nil
}

func eachJSON(tx *bbolt.Tx, bucket []byte, visit func(key []byte, decode func(any) error) error) error {
	cursor := tx.Bucket(bucket).Cursor()
	for key, value := cursor.First(); key != nil; key, value = cursor.Next() {
		payload := value
		err := visit(key, func(target any) error {
			if err := json.Unmarshal(payload, target); err != nil {
				return fmt.Errorf("decode %s document: %w", bucket, err)
			}
			return nil
		})
		if errors.Is(err, errStopIteration) {
			return nil
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// eachDescending walks a bucket newest-key-first, optionally within a prefix
// and starting strictly before an exclusive cursor key. visit reports whether
// the entry counted towards the page, so a filtered-out row does not consume
// the limit; next is filled with the key to continue from when the page is full
// and more entries remain.
func eachDescending(bucket *bbolt.Bucket, prefix, exclusiveCursor []byte, limit int, visit func(key, value []byte) (bool, error), next *string) error {
	cursor := bucket.Cursor()
	key, value := lastInPrefix(cursor, prefix)

	collected := 0
	lastCounted := ""
	for ; key != nil; key, value = cursor.Prev() {
		if len(prefix) > 0 && !bytes.HasPrefix(key, prefix) {
			break
		}
		if len(exclusiveCursor) > 0 && bytes.Compare(key, exclusiveCursor) >= 0 {
			continue
		}
		if collected == limit {
			// The cursor is the last key returned, and the next call treats it
			// as exclusive — so a client that pages never skips a row.
			*next = lastCounted
			return nil
		}
		counted, err := visit(key, value)
		if errors.Is(err, errStopIteration) {
			return nil
		}
		if err != nil {
			return err
		}
		if counted {
			collected++
			lastCounted = string(key)
		}
	}
	return nil
}

// lastInPrefix positions a cursor on the greatest key carrying prefix, or on
// the bucket's greatest key when no prefix is given.
func lastInPrefix(cursor *bbolt.Cursor, prefix []byte) ([]byte, []byte) {
	if len(prefix) == 0 {
		return cursor.Last()
	}
	bound := prefixUpperBound(prefix)
	if bound == nil {
		return cursor.Last()
	}
	if key, _ := cursor.Seek(bound); key == nil {
		return cursor.Last()
	}
	return cursor.Prev()
}

// prefixUpperBound returns the first key after every key with this prefix, or
// nil when the prefix is all 0xff and has no successor.
func prefixUpperBound(prefix []byte) []byte {
	bound := append([]byte(nil), prefix...)
	for index := len(bound) - 1; index >= 0; index-- {
		if bound[index] < 0xff {
			bound[index]++
			return bound[:index+1]
		}
	}
	return nil
}
