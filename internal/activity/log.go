package activity

import (
	"context"
	"log/slog"
	"regexp"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/castlemilk/dns/internal/secretguard"
	"github.com/castlemilk/dns/internal/telemetry"
)

// secretKeyPattern names detail keys whose value is never safe to store, no
// matter what shape it has. A generated mailbox password has no recognisable
// shape, so it is excluded by construction instead (the mail service never
// hands it to a recorder) — this is the second line of defence.
//
// Every alternative is a plain substring, "key" included. It used to be `key$`,
// but alternation binds looser than the anchor, so only that last branch was
// anchored and a detail key of key_material, api_key_id or keyring passed the
// filter into secretguard.Redact — which is shape-based and cannot recognise a
// generated secret. The cost of a false positive here is one lost detail line;
// the cost of a false negative is a stored credential.
var secretKeyPattern = regexp.MustCompile(`(?i)token|secret|password|authorization|credential|key`)

// eventCounter and eventDropCounter are satisfied by *telemetry.Metrics once it
// carries the activity instruments. They are optional interfaces so this leaf
// package compiles against any telemetry build.
type eventCounter interface {
	ActivityEvent(ctx context.Context, kind, severity string)
}

type eventDropCounter interface {
	ActivityDropped(ctx context.Context, reason string)
}

// Log is the recorder and the ActivityService handler. One instance is shared
// by every producer.
type Log struct {
	store   EventStore
	logger  *slog.Logger
	metrics *telemetry.Metrics
	now     func() time.Time
}

// LogOption customises a Log.
type LogOption func(*Log)

// WithClock replaces the clock. Tests use it; production does not.
func WithClock(now func() time.Time) LogOption {
	return func(l *Log) {
		if now != nil {
			l.now = now
		}
	}
}

// NewLog builds the shared log. A nil logger falls back to slog.Default; the
// logger is always wrapped in the redacting handler, because an event summary
// that carried a credential would otherwise reach the log verbatim.
func NewLog(store EventStore, logger *slog.Logger, metrics *telemetry.Metrics, options ...LogOption) *Log {
	if logger == nil {
		logger = slog.Default()
	}
	log := &Log{
		store:   store,
		logger:  secretguard.NewLogger(logger).With("component", "activity"),
		metrics: telemetry.Select(metrics),
		now:     time.Now,
	}
	for _, option := range options {
		option(log)
	}
	return log
}

// Record sanitises and appends one event. It never returns an error: an audit
// line that cannot be stored is logged and counted, so the operation that
// produced it still succeeds.
func (l *Log) Record(ctx context.Context, event Event) {
	doc := l.sanitise(event)
	if l.store == nil {
		l.drop(ctx, doc, "no_store", nil)
		return
	}
	// The caller's context may already be cancelled (a client that hung up
	// still produced a real mutation), so the append gets its own budget.
	appendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	id, err := l.store.AppendEvent(appendCtx, doc)
	if err != nil {
		l.drop(ctx, doc, "store_error", err)
		return
	}
	doc.ID = id
	if counter, ok := any(l.metrics).(eventCounter); ok {
		counter.ActivityEvent(ctx, doc.Kind, doc.Severity)
	}
	l.logger.Log(ctx, levelFor(Severity(doc.Severity)), doc.Summary,
		"event.id", doc.ID,
		"event.kind", doc.Kind,
		"event.actor", doc.Actor,
		"zone.id", doc.ZoneID,
	)
}

func (l *Log) drop(ctx context.Context, doc EventDoc, reason string, err error) {
	if counter, ok := any(l.metrics).(eventDropCounter); ok {
		counter.ActivityDropped(ctx, reason)
	}
	l.logger.Error("activity event dropped", "reason", reason, "event.kind", doc.Kind, "error", err)
}

func levelFor(severity Severity) slog.Level {
	switch severity {
	case SeverityError:
		return slog.LevelError
	case SeverityWarn:
		return slog.LevelWarn
	default:
		return slog.LevelInfo
	}
}

func (l *Log) sanitise(event Event) EventDoc {
	severity := event.Severity
	if !severity.Valid() {
		severity = SeverityInfo
	}
	doc := EventDoc{
		V:             DocVersion,
		Time:          l.now().UTC(),
		ZoneID:        truncateBytes(secretguard.Redact(event.ZoneID), MaxDetailValueSize),
		ZoneName:      truncateBytes(secretguard.Redact(event.ZoneName), MaxDetailValueSize),
		Actor:         truncateBytes(secretguard.Redact(event.Actor), MaxDetailValueSize),
		Kind:          string(NormalizeKind(event.Kind)),
		Severity:      string(severity),
		Summary:       truncateRunes(secretguard.Redact(event.Summary), MaxSummaryRunes),
		CorrelationID: truncateBytes(secretguard.Redact(event.CorrelationID), MaxDetailValueSize),
	}
	if doc.Actor == "" {
		doc.Actor = ActorSystem
	}
	doc.Details = sanitiseDetails(event.Details)
	return doc
}

func sanitiseDetails(details map[string]string) map[string]string {
	if len(details) == 0 {
		return nil
	}
	keys := make([]string, 0, len(details))
	for key := range details {
		keys = append(keys, key)
	}
	slices.Sort(keys)

	kept := make([]string, 0, len(keys))
	dropped := false
	for _, key := range keys {
		if secretKeyPattern.MatchString(key) {
			dropped = true
			continue
		}
		kept = append(kept, key)
	}
	if len(kept) > MaxDetailKeys {
		kept = kept[:MaxDetailKeys]
		dropped = true
	}
	if dropped && len(kept) == MaxDetailKeys {
		// Leave room for the marker: hiding that something was dropped would
		// be worse than losing the last key.
		kept = kept[:MaxDetailKeys-1]
	}

	cleaned := make(map[string]string, len(kept)+1)
	for _, key := range kept {
		cleaned[key] = truncateBytes(secretguard.Redact(details[key]), MaxDetailValueSize)
	}
	if dropped {
		cleaned["redacted"] = "true"
	}
	if len(cleaned) == 0 {
		return nil
	}
	return cleaned
}

func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}

// truncateBytes caps a value at limit bytes without leaving half a rune
// behind, so the stored JSON says what actually happened.
func truncateBytes(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	trimmed := value[:limit]
	for len(trimmed) > 0 {
		last, size := utf8.DecodeLastRuneInString(trimmed)
		if last != utf8.RuneError || size > 1 {
			break
		}
		trimmed = trimmed[:len(trimmed)-1]
	}
	return trimmed
}

var _ Recorder = (*Log)(nil)
