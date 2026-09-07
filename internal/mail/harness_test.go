package mail

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/castlemilk/dns/internal/activity"
	"github.com/castlemilk/dns/internal/config"
	"github.com/castlemilk/dns/internal/enginedns"
	"github.com/castlemilk/dns/internal/mail/fakemail"
	"github.com/castlemilk/dns/internal/platform"
	"github.com/castlemilk/dns/internal/secretguard"
	"github.com/castlemilk/dns/internal/telemetry"
	"github.com/castlemilk/dns/internal/zone"
)

// harness wires the facade against a real zone store, a real platform store and
// the in-memory engine. The zone store is real on purpose: the RRset rules, the
// engine-source rules and the TTL alignment ApplyRecordSet enforces are exactly
// what the published record set has to survive.
type harness struct {
	service *Service
	engine  *fakemail.Engine
	zones   *zone.Store
	store   *platform.Store
	events  *eventRecorder
	logs    *bytes.Buffer

	// clock is atomic because a test may advance it from a goroutine while the
	// facade's DKIM wait is reading it.
	clock atomic.Int64

	// cfg and deps are kept so the integration test can rebuild the service
	// around a real client without duplicating the wiring.
	cfg  config.Mail
	deps platform.Deps
}

func newHarness(t *testing.T, options ...func(*config.Mail)) *harness {
	t.Helper()
	dir := t.TempDir()

	zones, err := zone.Open(filepath.Join(dir, "dns.db"), []string{"ns1.deephost.test.", "ns2.deephost.test."})
	if err != nil {
		t.Fatalf("open zone store: %v", err)
	}
	t.Cleanup(func() {
		if err := zones.Close(); err != nil {
			t.Errorf("close the zone store: %v", err)
		}
	})

	store, err := platform.Open(filepath.Join(dir, "platform.db"))
	if err != nil {
		t.Fatalf("open platform store: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close the platform store: %v", err)
		}
	})

	logs := &bytes.Buffer{}
	logger := slog.New(secretguard.NewHandler(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	events := &eventRecorder{}

	cfg := config.Mail{
		APIURL:             config.FakeURL,
		APIToken:           "API_" + "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
		Hostname:           fakemail.DefaultHostname,
		MXPriority:         10,
		DMARCPolicy:        "quarantine",
		MailboxesPerDomain: 5,
		DefaultQuotaBytes:  5 << 30,
	}
	for _, option := range options {
		option(&cfg)
	}

	value := &harness{
		engine: fakemail.New(),
		zones:  zones,
		store:  store,
		events: events,
		logs:   logs,
	}
	value.clock.Store(time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC).UnixNano())
	deps := platform.Deps{
		Store:    store,
		Zones:    &memoryMutator{store: zones},
		Serial:   enginedns.NewSerializer(),
		Recorder: events,
		Logger:   logger,
		Metrics:  telemetry.Disabled(),
		Clock:    value.now,
	}
	value.cfg = cfg
	value.deps = deps
	value.service = newWithEngine(cfg, deps, value.engine)
	value.engine.Clock = value.now
	return value
}

// now is the shared time source of the facade and the fake engine.
func (h *harness) now() time.Time { return time.Unix(0, h.clock.Load()).UTC() }

// advance moves both clocks, so a DKIM delay and a row timestamp agree.
func (h *harness) advance(d time.Duration) { h.clock.Add(int64(d)) }

// zoneNamed creates a zone in the store and returns it.
func (h *harness) zoneNamed(t *testing.T, name string) zone.Zone {
	t.Helper()
	created, err := h.zones.Create(t.Context(), name)
	if err != nil {
		t.Fatalf("create zone %s: %v", name, err)
	}
	return created
}

// record adds a user record to a zone, as an operator would.
func (h *harness) record(t *testing.T, zoneID, name string, kind zone.RecordType, ttl uint32, value string) zone.Record {
	t.Helper()
	normalized, err := zone.NormalizeRecord(h.zoneName(t, zoneID), name, kind, ttl, value)
	if err != nil {
		t.Fatalf("normalize %s %s: %v", kind, name, err)
	}
	updated, err := h.zones.CreateRecord(t.Context(), zoneID, normalized)
	if err != nil {
		t.Fatalf("create record %s %s: %v", kind, name, err)
	}
	for _, existing := range updated.Records {
		if existing.Name == normalized.Name && existing.Type == normalized.Type && existing.Value == normalized.Value {
			return existing
		}
	}
	t.Fatalf("record %s %s was not stored", kind, name)
	return zone.Record{}
}

func (h *harness) zoneName(t *testing.T, zoneID string) string {
	t.Helper()
	value, err := h.zones.Get(t.Context(), zoneID)
	if err != nil {
		t.Fatalf("get zone: %v", err)
	}
	return value.Name
}

// records returns the zone's records for assertions.
func (h *harness) records(t *testing.T, zoneID string) []zone.Record {
	t.Helper()
	value, err := h.zones.Get(t.Context(), zoneID)
	if err != nil {
		t.Fatalf("get zone: %v", err)
	}
	return value.Records
}

// memoryMutator adapts the real zone store to enginedns.ZoneMutator. The
// control handler's own adapter adds the authoritative reload and the activity
// events, neither of which this package owns; the store semantics are what
// matter here and they are the real ones.
type memoryMutator struct {
	store *zone.Store
	mu    sync.Mutex
}

func (m *memoryMutator) GetZone(ctx context.Context, zoneID string) (zone.Zone, error) {
	value, err := m.store.Get(ctx, zoneID)
	if err != nil {
		return zone.Zone{}, notFoundError(err)
	}
	return value, nil
}

func (m *memoryMutator) ListZones(ctx context.Context) ([]zone.Zone, error) {
	return m.store.List(ctx)
}

func (m *memoryMutator) ApplyRecordSet(
	ctx context.Context,
	zoneID string,
	ops []enginedns.Op,
	_ string,
	_ string,
) (enginedns.Applied, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	updated, created, err := m.store.ApplyRecordSet(ctx, zoneID, ops)
	if err != nil {
		return enginedns.Applied{}, err
	}
	return enginedns.Applied{Zone: updated, RecordIDs: created}, nil
}

// eventRecorder captures what the facade recorded, so a test can assert both
// that an event happened and that it carries no credential.
type eventRecorder struct {
	mu     sync.Mutex
	events []activity.Event
}

func (r *eventRecorder) Record(_ context.Context, event activity.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *eventRecorder) all() []activity.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]activity.Event(nil), r.events...)
}

func (r *eventRecorder) kinds() []string {
	kinds := make([]string, 0)
	for _, event := range r.all() {
		kinds = append(kinds, string(event.Kind))
	}
	return kinds
}

func (r *eventRecorder) has(kind activity.Kind) bool {
	for _, event := range r.all() {
		if event.Kind == kind {
			return true
		}
	}
	return false
}

// engineDomains lists the fake engine's domains, failing the test on an error
// so a caller can assert on the result alone.
func (h *harness) engineDomains(t *testing.T) []Domain {
	t.Helper()
	domains, err := h.engine.ListDomains(t.Context())
	if err != nil {
		t.Fatalf("ListDomains: %v", err)
	}
	return domains
}

// notFoundError translates the store's sentinel into the Connect code the
// control handler's real adapter produces, because the facade distinguishes
// "the zone is gone" from "the store failed" by that code.
func notFoundError(err error) error {
	if errors.Is(err, zone.ErrNotFound) {
		return connect.NewError(connect.CodeNotFound, err)
	}
	return err
}
