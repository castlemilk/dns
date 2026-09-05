package hosting

import (
	"bytes"
	"context"
	"log/slog"
	"net/netip"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/castlemilk/dns/internal/activity"
	"github.com/castlemilk/dns/internal/config"
	"github.com/castlemilk/dns/internal/enginedns"
	"github.com/castlemilk/dns/internal/hosting/fakehosting"
	"github.com/castlemilk/dns/internal/platform"
	"github.com/castlemilk/dns/internal/zone"
)

// harness wires the real facade to a real platform store, a real zone store and
// the in-memory engine. Nothing here is a mock: the DNS writes go through
// zone.Store.ApplyRecordSet, so the RRset and provenance rules are the ones
// production enforces.
type harness struct {
	service *Service
	engine  *fakehosting.Engine
	store   *platform.Store
	zones   *zone.Store
	events  *recorder
	logs    *bytes.Buffer
	clock   *testClock
	faults  *zoneFaults
}

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// recorder captures every activity event the facade records.
type recorder struct {
	mu     sync.Mutex
	events []activity.Event
}

func (r *recorder) Record(_ context.Context, event activity.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *recorder) all() []activity.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]activity.Event(nil), r.events...)
}

func (r *recorder) kinds() []string {
	kinds := make([]string, 0)
	for _, event := range r.all() {
		kinds = append(kinds, string(event.Kind))
	}
	return kinds
}

func (r *recorder) has(kind activity.Kind) bool {
	for _, event := range r.all() {
		if event.Kind == kind {
			return true
		}
	}
	return false
}

// zoneFaults scripts the zone API a test wants to misbehave: a write that
// fails, and a hook that runs while the mutator is listing zones. Both are what
// a race or an unavailable DNS store looks like from the facade's side.
type zoneFaults struct {
	mu        sync.Mutex
	applyErr  error
	listZones func()
}

func (f *zoneFaults) failApply(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applyErr = err
}

func (f *zoneFaults) apply() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.applyErr
}

// duringListZones runs hook once, the next time the facade lists zones.
func (f *zoneFaults) duringListZones(hook func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listZones = hook
}

func (f *zoneFaults) takeListZones() func() {
	f.mu.Lock()
	defer f.mu.Unlock()
	hook := f.listZones
	f.listZones = nil
	return hook
}

// zoneMutator is the enginedns.ZoneMutator over a real zone store. The control
// handler adds the state mutex, the authoritative reload and the DNS events;
// none of that changes what a plan writes.
type zoneMutator struct {
	store  *zone.Store
	faults *zoneFaults
}

func (m zoneMutator) GetZone(ctx context.Context, zoneID string) (zone.Zone, error) {
	return m.store.Get(ctx, zoneID)
}

func (m zoneMutator) ListZones(ctx context.Context) ([]zone.Zone, error) {
	zones, err := m.store.List(ctx)
	if hook := m.faults.takeListZones(); hook != nil {
		hook()
	}
	return zones, err
}

func (m zoneMutator) ApplyRecordSet(
	ctx context.Context,
	zoneID string,
	ops []enginedns.Op,
	_, _ string,
) (enginedns.Applied, error) {
	if err := m.faults.apply(); err != nil {
		return enginedns.Applied{}, err
	}
	value, created, err := m.store.ApplyRecordSet(ctx, zoneID, ops)
	if err != nil {
		return enginedns.Applied{}, err
	}
	return enginedns.Applied{Zone: value, RecordIDs: created}, nil
}

func newHarness(t *testing.T, mutate ...func(*config.Hosting)) *harness {
	t.Helper()

	dir := t.TempDir()
	zones, err := zone.Open(filepath.Join(dir, "dns.db"), []string{"ns1.simple.test"})
	if err != nil {
		t.Fatalf("zone.Open: %v", err)
	}
	t.Cleanup(func() { ignore(zones.Close()) })

	store, err := platform.Open(filepath.Join(dir, "platform.db"))
	if err != nil {
		t.Fatalf("platform.Open: %v", err)
	}
	t.Cleanup(func() { ignore(store.Close()) })

	clock := &testClock{now: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	events := &recorder{}
	logs := &bytes.Buffer{}

	cfg := config.Hosting{
		APIURL:           "http://127.0.0.1:30081",
		Tenant:           "simple-test",
		GatewayAddresses: []netip.Addr{netip.MustParseAddr("198.51.100.10")},
		BuildDeadline:    config.DefaultBuildDeadline,
		UploadMaxBytes:   config.DefaultUploadMaxBytes,
		UploadDir:        filepath.Join(dir, "uploads"),
	}
	cfg.UploadMaxTotalBytes = 2 * cfg.UploadMaxBytes
	for _, apply := range mutate {
		apply(&cfg)
	}

	faults := &zoneFaults{}
	deps := platform.Deps{
		Store:    store,
		Zones:    zoneMutator{store: zones, faults: faults},
		Serial:   enginedns.NewSerializer(),
		Recorder: events,
		Logger:   slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Clock:    clock.Now,
	}

	service, uploads := New(cfg, deps)
	engine := fakehosting.New()
	engine.SetClock(clock.Now)
	service.WithEngine(engine)
	// A test must not wait a real minute for the engine's garbage collection.
	service.WithDrainTimeout(20 * time.Millisecond)
	if cfg.UploadsEnabled && uploads == nil {
		t.Fatal("New returned no upload handler with uploads enabled")
	}

	return &harness{
		service: service,
		engine:  engine,
		store:   store,
		zones:   zones,
		events:  events,
		logs:    logs,
		clock:   clock,
		faults:  faults,
	}
}

// zoneID creates a zone and returns its id.
func (h *harness) zoneID(t *testing.T, name string) string {
	t.Helper()
	value, err := h.zones.Create(context.Background(), name)
	if err != nil {
		t.Fatalf("create zone %q: %v", name, err)
	}
	return value.ID
}

// seedGateway writes the gateway document directly, the way the reconciler
// would after a successful resolution.
func (h *harness) seedGateway(t *testing.T, addresses ...string) {
	t.Helper()
	err := h.store.PutGateway(context.Background(), platform.GatewayDoc{
		V:          platform.DocVersion,
		Addresses:  addresses,
		Source:     platform.GatewaySourceStatic,
		ResolvedAt: h.clock.Now(),
	})
	if err != nil {
		t.Fatalf("seed gateway: %v", err)
	}
}

// records returns the zone's records, so a test can assert on provenance.
func (h *harness) records(t *testing.T, zoneID string) []zone.Record {
	t.Helper()
	value, err := h.zones.Get(context.Background(), zoneID)
	if err != nil {
		t.Fatalf("get zone: %v", err)
	}
	return value.Records
}

// engineRecords returns only the records the hosting engine owns.
func (h *harness) engineRecords(t *testing.T, zoneID string) []zone.Record {
	t.Helper()
	owned := make([]zone.Record, 0, 3)
	for _, record := range h.records(t, zoneID) {
		if record.Source == enginedns.SourceHosting {
			owned = append(owned, record)
		}
	}
	return owned
}

func (h *harness) site(t *testing.T, zoneID string) platform.SiteDoc {
	t.Helper()
	doc, err := h.store.GetSite(context.Background(), zoneID)
	if err != nil {
		t.Fatalf("get site: %v", err)
	}
	return doc
}

func (h *harness) deploy(t *testing.T, zoneID, deployID string) platform.DeployDoc {
	t.Helper()
	doc, err := h.store.GetDeploy(context.Background(), zoneID, deployID)
	if err != nil {
		t.Fatalf("get deploy: %v", err)
	}
	return doc
}

// settle runs the poller and the watcher until the deploy reaches a terminal
// phase or the step budget runs out, advancing the fake engine each round.
func (h *harness) settle(t *testing.T, zoneID, deployID string, steps int) platform.DeployDoc {
	t.Helper()
	ctx := context.Background()
	for range steps {
		h.engine.Advance()
		h.clock.Advance(time.Second)
		h.service.pollOnce(ctx)
		h.service.watchDomains(ctx)
		doc := h.deploy(t, zoneID, deployID)
		switch doc.Phase {
		case platform.DeployPhaseLive, platform.DeployPhaseFailed,
			platform.DeployPhaseAbandoned, platform.DeployPhaseLost:
			return doc
		}
	}
	return h.deploy(t, zoneID, deployID)
}

// zoneRecordA is a user A record, the shape a person creates in the raw zone.
func zoneRecordA(name, value string) zone.Record {
	return zone.Record{Name: name, Type: zone.TypeA, TTL: zone.DefaultRecordTTL, Value: value}
}
