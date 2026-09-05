package activity_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	activityv1 "github.com/castlemilk/dns/gen/go/activity/v1"
	"github.com/castlemilk/dns/internal/activity"
)

// memoryStore is the smallest honest EventStore: it keeps documents in key
// order and answers exactly the queries the handler asks for.
type memoryStore struct {
	mu     sync.Mutex
	docs   []activity.EventDoc
	seq    int
	failOn string
}

func (s *memoryStore) AppendEvent(_ context.Context, doc activity.EventDoc) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failOn == "append" {
		return "", errors.New("store is down")
	}
	s.seq++
	doc.ID = fmt.Sprintf("%016x-%04x", doc.Time.UnixMilli(), s.seq)
	s.docs = append(s.docs, doc)
	return doc.ID, nil
}

func (s *memoryStore) ListEvents(_ context.Context, filter activity.ListFilter) ([]activity.EventDoc, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failOn == "list" {
		return nil, "", errors.New("store is down")
	}
	newest := slices.Clone(s.docs)
	slices.Reverse(newest)

	matched := make([]activity.EventDoc, 0, len(newest))
	for _, doc := range newest {
		if filter.ZoneID != "" && doc.ZoneID != filter.ZoneID {
			continue
		}
		if !filter.MatchesKind(doc.Kind) {
			continue
		}
		if !filter.Since.IsZero() && doc.Time.Before(filter.Since) {
			continue
		}
		if filter.Cursor != "" && doc.ID >= filter.Cursor {
			continue
		}
		matched = append(matched, doc)
	}
	next := ""
	if filter.Limit > 0 && len(matched) > filter.Limit {
		next = matched[filter.Limit-1].ID
		matched = matched[:filter.Limit]
	}
	return matched, next, nil
}

func (s *memoryStore) GetEvent(_ context.Context, id string) (activity.EventDoc, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, doc := range s.docs {
		if doc.ID == id {
			return doc, nil
		}
	}
	return activity.EventDoc{}, activity.ErrEventNotFound
}

func (s *memoryStore) CountEvents(context.Context) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return uint64(len(s.docs)), nil
}

func (s *memoryStore) PruneEvents(_ context.Context, keep int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.docs) <= keep {
		return 0, nil
	}
	removed := len(s.docs) - keep
	s.docs = s.docs[removed:]
	return removed, nil
}

func (s *memoryStore) all() []activity.EventDoc {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.docs)
}

func newLog(t *testing.T) (*activity.Log, *memoryStore, *bytes.Buffer) {
	t.Helper()
	store := &memoryStore{}
	buffer := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(buffer, &slog.HandlerOptions{Level: slog.LevelDebug}))
	instant := time.Date(2026, time.September, 3, 12, 0, 0, 0, time.UTC)
	log := activity.NewLog(store, logger, nil, activity.WithClock(func() time.Time {
		instant = instant.Add(time.Millisecond)
		return instant
	}))
	return log, store, buffer
}

func TestEveryKindConstantIsAllowlisted(t *testing.T) {
	t.Parallel()

	all := activity.Kinds()
	if len(all) < 60 {
		t.Fatalf("Kinds() = %d entries, want the full §7.1 list", len(all))
	}
	if !slices.IsSorted(all) {
		t.Error("Kinds() is not sorted")
	}
	for _, kind := range all {
		if !activity.ValidKind(kind) {
			t.Errorf("Kinds() returned %q, which ValidKind rejects", kind)
		}
		if activity.NormalizeKind(kind) != kind {
			t.Errorf("NormalizeKind(%q) rewrote an allowlisted kind", kind)
		}
	}
	if !slices.Contains(all, activity.KindOther) {
		t.Error("the allowlist is missing the catch-all kind")
	}
	if got := activity.NormalizeKind(activity.Kind("something.invented")); got != activity.KindOther {
		t.Errorf("NormalizeKind(unknown) = %q, want %q", got, activity.KindOther)
	}
}

func TestRecordSanitisesTheEvent(t *testing.T) {
	t.Parallel()

	log, store, buffer := newLog(t)
	log.Record(context.Background(), activity.Event{
		ZoneID:   "z1",
		ZoneName: "acme.dev",
		Actor:    activity.ActorHostingEngine,
		Kind:     activity.Kind("deploy.invented"),
		Severity: activity.Severity("loud"),
		Summary:  strings.Repeat("é", 400),
		Details: map[string]string{
			"git_token":     "ghp_0123456789abcdefghijABCDEF",
			"api_key":       "API_" + strings.Repeat("x", 38),
			"Authorization": "Bearer nope",
			// The pattern used to anchor only its last alternative, so a key
			// whose name merely contains "key" reached secretguard.Redact —
			// which is shape-based and cannot recognise a generated secret.
			"key_material":        "correct horse battery staple",
			"api_key_id":          "ak_live_0123456789",
			"keyring":             "s3kr3t",
			"deploy.repository":   "https://user:ghp_0123456789abcdefghijABCDEF@github.com/o/r",
			"record.value":        strings.Repeat("v", 900),
			"stripe.subscription": "sub_123",
		},
	})

	docs := store.all()
	if len(docs) != 1 {
		t.Fatalf("stored %d events, want 1", len(docs))
	}
	doc := docs[0]
	if doc.Kind != string(activity.KindOther) {
		t.Errorf("kind = %q, want the unknown kind folded to other", doc.Kind)
	}
	if doc.Severity != string(activity.SeverityInfo) {
		t.Errorf("severity = %q, want an invalid severity folded to info", doc.Severity)
	}
	if runes := []rune(doc.Summary); len(runes) != activity.MaxSummaryRunes {
		t.Errorf("summary = %d runes, want %d", len(runes), activity.MaxSummaryRunes)
	}
	for _, dropped := range []string{
		"git_token", "api_key", "Authorization", "key_material", "api_key_id", "keyring",
	} {
		if _, present := doc.Details[dropped]; present {
			t.Errorf("detail %q was stored", dropped)
		}
	}
	if doc.Details["redacted"] != "true" {
		t.Errorf("details = %v, want the redaction marker", doc.Details)
	}
	if got := doc.Details["deploy.repository"]; got != "https://[redacted]@github.com/o/r" {
		t.Errorf("deploy.repository = %q", got)
	}
	if got := len(doc.Details["record.value"]); got != activity.MaxDetailValueSize {
		t.Errorf("record.value = %d bytes, want %d", got, activity.MaxDetailValueSize)
	}
	if doc.Details["stripe.subscription"] != "sub_123" {
		t.Errorf("an innocent detail was rewritten: %q", doc.Details["stripe.subscription"])
	}
	if doc.V != activity.DocVersion || doc.ID == "" || doc.Time.IsZero() {
		t.Errorf("doc envelope = %+v", doc)
	}
	for _, forbidden := range []string{"ghp_", "API_x", "Bearer nope"} {
		if strings.Contains(buffer.String(), forbidden) {
			t.Errorf("log output contains %q: %s", forbidden, buffer.String())
		}
	}
}

func TestRecordCapsTheDetailCount(t *testing.T) {
	t.Parallel()

	log, store, _ := newLog(t)
	details := map[string]string{}
	for index := range 40 {
		details[fmt.Sprintf("k%02d", index)] = "v"
	}
	log.Record(context.Background(), activity.Event{Kind: activity.KindPlatformStarted, Details: details})

	doc := store.all()[0]
	if len(doc.Details) != activity.MaxDetailKeys {
		t.Fatalf("details = %d keys, want %d", len(doc.Details), activity.MaxDetailKeys)
	}
	if doc.Details["redacted"] != "true" {
		t.Errorf("details = %v, want the marker to survive the cap", doc.Details)
	}
	if doc.Actor != activity.ActorSystem {
		t.Errorf("actor = %q, want the system default", doc.Actor)
	}
}

func TestRecordSurvivesAStoreFailure(t *testing.T) {
	t.Parallel()

	store := &memoryStore{failOn: "append"}
	buffer := &bytes.Buffer{}
	log := activity.NewLog(store, slog.New(slog.NewJSONHandler(buffer, nil)), nil)
	log.Record(context.Background(), activity.Event{Kind: activity.KindZoneCreated, Summary: "created acme.dev"})

	if !strings.Contains(buffer.String(), "activity event dropped") {
		t.Errorf("a dropped event was not reported: %s", buffer.String())
	}
}

func TestRecordWithACancelledContextStillStores(t *testing.T) {
	t.Parallel()

	log, store, _ := newLog(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	log.Record(ctx, activity.Event{Kind: activity.KindZoneCreated, Summary: "created acme.dev"})

	if len(store.all()) != 1 {
		t.Error("a mutation's audit line was lost because the client hung up")
	}
}

func TestNopRecorder(t *testing.T) {
	t.Parallel()

	activity.Nop().Record(context.Background(), activity.Event{Kind: activity.KindZoneCreated})
}

func TestListEventsPagesNewestFirst(t *testing.T) {
	t.Parallel()

	log, _, _ := newLog(t)
	ctx := context.Background()
	for index := range 5 {
		log.Record(ctx, activity.Event{
			ZoneID:   "z1",
			Kind:     activity.KindDeployRequested,
			Actor:    activity.ActorOperator,
			Summary:  fmt.Sprintf("deploy %d", index),
			Severity: activity.SeverityInfo,
		})
	}
	log.Record(ctx, activity.Event{ZoneID: "z2", Kind: activity.KindZoneCreated, Summary: "other zone"})

	first, err := log.ListEvents(ctx, connect.NewRequest(&activityv1.ListEventsRequest{Limit: 2}))
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(first.Msg.GetEvents()) != 2 {
		t.Fatalf("page 1 = %d events, want 2", len(first.Msg.GetEvents()))
	}
	if first.Msg.GetEvents()[0].GetSummary() != "other zone" {
		t.Errorf("page 1 starts with %q, want the newest event", first.Msg.GetEvents()[0].GetSummary())
	}
	if first.Msg.GetTotalRetained() != 6 {
		t.Errorf("total_retained = %d, want 6", first.Msg.GetTotalRetained())
	}
	if first.Msg.GetNextCursor() == "" {
		t.Fatal("next_cursor is empty with more events available")
	}

	second, err := log.ListEvents(ctx, connect.NewRequest(&activityv1.ListEventsRequest{
		Limit:  2,
		Cursor: first.Msg.GetNextCursor(),
	}))
	if err != nil {
		t.Fatalf("ListEvents page 2: %v", err)
	}
	if len(second.Msg.GetEvents()) != 2 {
		t.Fatalf("page 2 = %d events", len(second.Msg.GetEvents()))
	}
	if second.Msg.GetEvents()[0].GetId() >= first.Msg.GetEvents()[1].GetId() {
		t.Error("page 2 overlaps page 1")
	}
}

func TestListEventsFiltersByZoneKindAndSince(t *testing.T) {
	t.Parallel()

	log, store, _ := newLog(t)
	ctx := context.Background()
	log.Record(ctx, activity.Event{ZoneID: "z1", Kind: activity.KindDeployRequested, Summary: "requested"})
	log.Record(ctx, activity.Event{ZoneID: "z1", Kind: activity.KindDeployLive, Summary: "live"})
	log.Record(ctx, activity.Event{ZoneID: "z2", Kind: activity.KindDeployLive, Summary: "elsewhere"})
	log.Record(ctx, activity.Event{ZoneID: "z1", Kind: activity.KindZoneCreated, Summary: "created"})

	byZone, err := log.ListEvents(ctx, connect.NewRequest(&activityv1.ListEventsRequest{ZoneId: "z1"}))
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(byZone.Msg.GetEvents()) != 3 {
		t.Errorf("zone filter = %d events, want 3", len(byZone.Msg.GetEvents()))
	}

	byPrefix, err := log.ListEvents(ctx, connect.NewRequest(&activityv1.ListEventsRequest{Kinds: []string{"deploy."}}))
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(byPrefix.Msg.GetEvents()) != 3 {
		t.Errorf("prefix filter = %d events, want 3", len(byPrefix.Msg.GetEvents()))
	}

	byExact, err := log.ListEvents(ctx, connect.NewRequest(&activityv1.ListEventsRequest{
		Kinds: []string{string(activity.KindZoneCreated)},
	}))
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(byExact.Msg.GetEvents()) != 1 {
		t.Errorf("exact filter = %d events, want 1", len(byExact.Msg.GetEvents()))
	}

	docs := store.all()
	filter := activity.ListFilter{Since: docs[2].Time}
	if !filter.MatchesKind(docs[0].Kind) {
		t.Error("an empty kind list should match everything")
	}
}

func TestListEventsRejectsAnUnboundedFilter(t *testing.T) {
	t.Parallel()

	log, _, _ := newLog(t)
	kinds := make([]string, activity.MaxListKinds+1)
	for index := range kinds {
		kinds[index] = "deploy.live"
	}
	if _, err := log.ListEvents(context.Background(), connect.NewRequest(&activityv1.ListEventsRequest{Kinds: kinds})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("ListEvents with too many kinds = %v, want InvalidArgument", err)
	}
	if _, err := log.ListEvents(context.Background(), connect.NewRequest(&activityv1.ListEventsRequest{Kinds: []string{"a b"}})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("ListEvents with a malformed kind = %v, want InvalidArgument", err)
	}
}

func TestGetEvent(t *testing.T) {
	t.Parallel()

	log, store, _ := newLog(t)
	ctx := context.Background()
	log.Record(ctx, activity.Event{
		ZoneID:        "z1",
		ZoneName:      "acme.dev",
		Actor:         activity.ActorOperator,
		Kind:          activity.KindDNSRecordCreated,
		Severity:      activity.SeverityWarn,
		Summary:       "created A @",
		Details:       map[string]string{"record.type": "A"},
		CorrelationID: "corr-1",
	})
	id := store.all()[0].ID

	response, err := log.GetEvent(ctx, connect.NewRequest(&activityv1.GetEventRequest{Id: id}))
	if err != nil {
		t.Fatalf("GetEvent: %v", err)
	}
	event := response.Msg.GetEvent()
	if event.GetId() != id || event.GetZoneName() != "acme.dev" || event.GetCorrelationId() != "corr-1" {
		t.Errorf("event = %+v", event)
	}
	if event.GetSeverity() != activityv1.Severity_SEVERITY_WARN {
		t.Errorf("severity = %v", event.GetSeverity())
	}
	if event.GetDetails()["record.type"] != "A" {
		t.Errorf("details = %v", event.GetDetails())
	}
	if event.GetTime().AsTime().IsZero() {
		t.Error("time is missing")
	}

	if _, err := log.GetEvent(ctx, connect.NewRequest(&activityv1.GetEventRequest{Id: "nope"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("GetEvent(unknown) = %v, want NotFound", err)
	}
	if _, err := log.GetEvent(ctx, connect.NewRequest(&activityv1.GetEventRequest{})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("GetEvent(\"\") = %v, want InvalidArgument", err)
	}
}

func TestHandlerWithoutAStore(t *testing.T) {
	t.Parallel()

	log := activity.NewLog(nil, slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)), nil)
	ctx := context.Background()
	if _, err := log.ListEvents(ctx, connect.NewRequest(&activityv1.ListEventsRequest{})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("ListEvents without a store = %v, want FailedPrecondition", err)
	}
	if _, err := log.GetEvent(ctx, connect.NewRequest(&activityv1.GetEventRequest{Id: "x"})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("GetEvent without a store = %v, want FailedPrecondition", err)
	}
	log.Record(ctx, activity.Event{Kind: activity.KindZoneCreated})
}

func TestPrunerTrimsToTheRingSize(t *testing.T) {
	t.Parallel()

	log, store, _ := newLog(t)
	ctx := context.Background()
	for index := range 10 {
		log.Record(ctx, activity.Event{Kind: activity.KindZoneCreated, Summary: fmt.Sprintf("zone %d", index)})
	}

	pruner := activity.NewPruner(store, 4, nil)
	if pruner.Keep() != 4 {
		t.Errorf("Keep = %d", pruner.Keep())
	}
	removed, err := pruner.Prune(ctx)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if removed != 6 {
		t.Errorf("Prune removed %d, want 6", removed)
	}
	if got := len(store.all()); got != 4 {
		t.Errorf("store holds %d events, want 4", got)
	}
	if again, err := pruner.Prune(ctx); err != nil || again != 0 {
		t.Errorf("second Prune = (%d, %v), want (0, nil)", again, err)
	}
	if activity.NewPruner(store, 0, nil).Keep() != activity.DefaultMaxEvents {
		t.Error("a zero ring size did not fall back to the default")
	}
	if _, err := activity.NewPruner(nil, 4, nil).Prune(ctx); err != nil {
		t.Errorf("Prune without a store = %v, want nil", err)
	}
}
