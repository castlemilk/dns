package platform_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	platformv1 "github.com/castlemilk/dns/gen/go/platform/v1"
	"github.com/castlemilk/dns/internal/activity"
	"github.com/castlemilk/dns/internal/config"
	"github.com/castlemilk/dns/internal/platform"
	"go.etcd.io/bbolt"

	"connectrpc.com/connect"
)

// recorder captures events without a store, so a test can assert exactly what
// a component recorded.
type recorder struct {
	mu     sync.Mutex
	events []activity.Event
}

func (r *recorder) Record(_ context.Context, event activity.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *recorder) kinds() []activity.Kind {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]activity.Kind, 0, len(r.events))
	for _, event := range r.events {
		result = append(result, event.Kind)
	}
	return result
}

// fakeProbe is one engine under the prober's control.
type fakeProbe struct {
	kind   platformv1.EngineKind
	mu     sync.Mutex
	err    error
	status platform.EngineStatus
	calls  int
}

func (p *fakeProbe) Kind() platformv1.EngineKind { return p.kind }

func (p *fakeProbe) Probe(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.status.Reachable = p.err == nil
	if p.err != nil {
		p.status.Reason = p.err.Error()
	} else {
		p.status.Reason = ""
	}
	return p.err
}

func (p *fakeProbe) Status() platform.EngineStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	status := p.status
	status.Kind = p.kind
	return status
}

func (p *fakeProbe) fail(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.err = err
}

// fakeRebuilder reports a fixed count.
type fakeRebuilder struct {
	kind     platformv1.EngineKind
	count    uint32
	warnings []string
	err      error
	dryRuns  int
}

func (r *fakeRebuilder) Kind() platformv1.EngineKind { return r.kind }

func (r *fakeRebuilder) Rebuild(_ context.Context, dryRun bool) (platform.RebuildReport, error) {
	if dryRun {
		r.dryRuns++
	}
	if r.err != nil {
		return platform.RebuildReport{}, r.err
	}
	return platform.RebuildReport{Count: r.count, Warnings: r.warnings}, nil
}

func testConfig(t *testing.T) config.Config {
	t.Helper()
	return config.Config{
		Role:         config.RoleAll,
		Nameservers:  []string{"ns1.dns.test.", "ns2.dns.test."},
		PlatformPath: filepath.Join(t.TempDir(), "platform.db"),
		Activity:     config.Activity{MaxEvents: config.DefaultActivityMaxEvents},
	}
}

func TestGetPlatformStatusReportsEveryEngineWithoutSecrets(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t)
	cfg := testConfig(t)
	deps := platform.Deps{Store: store, Recorder: activity.Nop()}

	probes := []platform.Probe{
		platform.NewDNSProbe(cfg, deps),
		&fakeProbe{kind: platformv1.EngineKind_ENGINE_KIND_HOSTING, status: platform.EngineStatus{
			MissingEnv: []string{"HOSTING_API_URL", "HOSTING_API_TOKEN"},
		}},
		&fakeProbe{kind: platformv1.EngineKind_ENGINE_KIND_MAIL, status: platform.EngineStatus{
			MissingEnv: []string{"MAIL_API_URL"},
		}},
		&fakeProbe{kind: platformv1.EngineKind_ENGINE_KIND_BILLING, status: platform.EngineStatus{
			MissingEnv: []string{"BILLING_PROVIDER"},
		}},
	}
	service := platform.NewStatusService(cfg, deps, probes, nil)

	response, err := service.GetPlatformStatus(ctx, connect.NewRequest(&platformv1.GetPlatformStatusRequest{}))
	if err != nil {
		t.Fatalf("GetPlatformStatus: %v", err)
	}
	engines := response.Msg.GetEngines()
	if len(engines) != 4 {
		t.Fatalf("engines = %d, want 4", len(engines))
	}
	if engines[0].GetKind() != platformv1.EngineKind_ENGINE_KIND_DNS {
		t.Fatalf("first engine = %s, want DNS", engines[0].GetKind())
	}
	if !engines[0].GetConfigured() || !engines[0].GetReachable() {
		t.Fatal("the control plane must report itself configured and reachable")
	}
	for _, engine := range engines[1:] {
		if engine.GetConfigured() || engine.GetReachable() {
			t.Fatalf("engine %s reported configured/reachable with no config", engine.GetKind())
		}
		if len(engine.GetMissingEnv()) == 0 {
			t.Fatalf("engine %s must name the variables to set", engine.GetKind())
		}
		for _, name := range engine.GetMissingEnv() {
			if strings.ContainsAny(name, "=:/") {
				t.Fatalf("missing_env entry %q looks like a value, not a variable name", name)
			}
		}
	}
	if response.Msg.GetPlatformStoreName() != "platform.db" {
		t.Fatalf("platform_store_name = %q", response.Msg.GetPlatformStoreName())
	}
	if !response.Msg.GetBillingInformational() {
		t.Fatal("billing_informational must be true in this release")
	}
	if response.Msg.GetPlatformBackupRoute() {
		t.Fatal("platform_backup_route must be false without a snapshot token")
	}
	if response.Msg.GetMailboxesPerDomain() != 0 {
		t.Fatal("mailboxes_per_domain must be 0 when mail is not configured")
	}
}

func TestGetPlatformStatusProbeIsRateLimited(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cfg := testConfig(t)
	deps := platform.Deps{Recorder: activity.Nop()}
	hosting := &fakeProbe{kind: platformv1.EngineKind_ENGINE_KIND_HOSTING, status: platform.EngineStatus{Configured: true}}
	service := platform.NewStatusService(cfg, deps, []platform.Probe{hosting}, nil)

	for range 5 {
		if _, err := service.GetPlatformStatus(ctx, connect.NewRequest(&platformv1.GetPlatformStatusRequest{Probe: true})); err != nil {
			t.Fatalf("GetPlatformStatus: %v", err)
		}
	}
	hosting.mu.Lock()
	calls := hosting.calls
	hosting.mu.Unlock()
	if calls != 1 {
		t.Fatalf("engine probed %d times for 5 rapid requests, want 1", calls)
	}
}

func TestRebuildPlatformStore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t)
	cfg := testConfig(t)
	events := &recorder{}
	deps := platform.Deps{Store: store, Recorder: events}

	rebuilders := []platform.Rebuilder{
		&fakeRebuilder{kind: platformv1.EngineKind_ENGINE_KIND_HOSTING, count: 2},
		&fakeRebuilder{kind: platformv1.EngineKind_ENGINE_KIND_MAIL, count: 1, warnings: []string{"mail domain without a zone"}},
		&fakeRebuilder{kind: platformv1.EngineKind_ENGINE_KIND_BILLING, err: errors.New("billing unreachable")},
	}
	service := platform.NewStatusService(cfg, deps, nil, rebuilders)

	dry, err := service.RebuildPlatformStore(ctx, connect.NewRequest(&platformv1.RebuildPlatformStoreRequest{DryRun: true}))
	if err != nil {
		t.Fatalf("RebuildPlatformStore(dry): %v", err)
	}
	if dry.Msg.GetSites() != 2 || dry.Msg.GetMailDomains() != 1 || dry.Msg.GetSubscriptions() != 0 {
		t.Fatalf("dry run counts = %+v", dry.Msg)
	}
	if !dry.Msg.GetDryRun() {
		t.Fatal("dry_run must be echoed")
	}
	// One unreachable engine is reported, not fatal: the other two rebuilt.
	if len(dry.Msg.GetWarnings()) != 2 {
		t.Fatalf("warnings = %v, want the mail warning and the billing failure", dry.Msg.GetWarnings())
	}
	if len(events.kinds()) != 0 {
		t.Fatalf("a dry run must record nothing, recorded %v", events.kinds())
	}

	applied, err := service.RebuildPlatformStore(ctx, connect.NewRequest(&platformv1.RebuildPlatformStoreRequest{}))
	if err != nil {
		t.Fatalf("RebuildPlatformStore: %v", err)
	}
	if applied.Msg.GetDryRun() {
		t.Fatal("dry_run must be false on an applied rebuild")
	}
	if kinds := events.kinds(); len(kinds) != 1 || kinds[0] != activity.KindPlatformRebuilt {
		t.Fatalf("recorded %v, want one platform.rebuilt event", kinds)
	}

	// A store with live rows refuses: a rebuild over them would replace the
	// operator's state with the engines' view of it.
	if err := store.PutSite(ctx, platform.SiteDoc{ZoneID: "z1", ZoneName: "acme.dev"}); err != nil {
		t.Fatalf("PutSite: %v", err)
	}
	_, err = service.RebuildPlatformStore(ctx, connect.NewRequest(&platformv1.RebuildPlatformStoreRequest{}))
	if err == nil {
		t.Fatal("expected a refusal on a non-empty store")
	}
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("code = %s, want failed_precondition", connect.CodeOf(err))
	}
	if !strings.Contains(err.Error(), "the platform store is not empty") {
		t.Fatalf("error = %v", err)
	}
}

func TestPlanRouteCarriesNoHostsOrSecrets(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	cfg.Mail = config.Mail{APIURL: "http://mail.internal:8080", APIToken: "API_secret", Hostname: "mx1.simple.host", MailboxesPerDomain: 5}
	cfg.Hosting = config.Hosting{APIURL: "https://deephost.internal", APIToken: "super-secret", UploadsEnabled: true}
	deps := platform.Deps{Recorder: activity.Nop()}
	service := platform.NewStatusService(cfg, deps, nil, nil)

	response := httptest.NewRecorder()
	service.PlanHandler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/public/v1/plan", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	if got := response.Header().Get("Cache-Control"); got != "public, max-age=300" {
		t.Fatalf("Cache-Control = %q", got)
	}

	body := response.Body.String()
	for _, forbidden := range []string{"super-secret", "API_secret", "deephost.internal", "mail.internal", "mx1.simple.host"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("plan body leaked %q: %s", forbidden, body)
		}
	}
	var doc struct {
		HostingConfigured  bool   `json:"hosting_configured"`
		UploadsEnabled     bool   `json:"uploads_enabled"`
		MailConfigured     bool   `json:"mail_configured"`
		MailboxesPerDomain int    `json:"mailboxes_per_domain"`
		BillingConfigured  bool   `json:"billing_configured"`
		PriceLabel         string `json:"price_label"`
		PolicyNote         string `json:"policy_note"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode plan: %v", err)
	}
	if !doc.HostingConfigured || !doc.UploadsEnabled || !doc.MailConfigured {
		t.Fatalf("plan = %+v", doc)
	}
	if doc.MailboxesPerDomain != 5 {
		t.Fatalf("mailboxes_per_domain = %d, want 5", doc.MailboxesPerDomain)
	}
	if doc.BillingConfigured {
		t.Fatal("billing_configured must be false with no provider")
	}
	if doc.PriceLabel != "" {
		t.Fatalf("price_label = %q, want empty when billing is not configured", doc.PriceLabel)
	}
	if doc.PolicyNote != platform.PolicyNote {
		t.Fatalf("policy_note = %q", doc.PolicyNote)
	}
}

func TestPlanRouteRejectsWrites(t *testing.T) {
	t.Parallel()
	service := platform.NewStatusService(testConfig(t), platform.Deps{Recorder: activity.Nop()}, nil, nil)
	response := httptest.NewRecorder()
	service.PlanHandler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/public/v1/plan", nil))
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", response.Code)
	}
}

func TestBackupRouteStreamsAReopenableCopy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t)
	if err := store.PutSite(ctx, platform.SiteDoc{ZoneID: "z1", ZoneName: "acme.dev"}); err != nil {
		t.Fatalf("PutSite: %v", err)
	}
	service := platform.NewStatusService(testConfig(t), platform.Deps{Store: store, Recorder: activity.Nop()}, nil, nil)

	response := httptest.NewRecorder()
	service.BackupHandler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/internal/v1/platform-backup", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	if got := response.Header().Get("Content-Type"); got != "application/octet-stream" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := response.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
	body := response.Body.Bytes()
	if got := response.Header().Get("Content-Length"); got == "" {
		t.Fatal("Content-Length must be set from the transaction size")
	}
	// bbolt's magic sits at byte offset 16 of the first meta page; the backup
	// script checks exactly this before trusting a downloaded copy.
	if len(body) < 20 {
		t.Fatalf("backup is %d bytes", len(body))
	}

	path := filepath.Join(t.TempDir(), "copy.db")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write copy: %v", err)
	}
	db, err := bbolt.Open(path, 0o600, &bbolt.Options{ReadOnly: true, Timeout: time.Second})
	if err != nil {
		t.Fatalf("reopen streamed backup: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close copy: %v", err)
	}
}

func TestProberRecordsOnlyTransitions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	events := &recorder{}
	hosting := &fakeProbe{kind: platformv1.EngineKind_ENGINE_KIND_HOSTING, status: platform.EngineStatus{Configured: true}}
	prober := platform.NewProber([]platform.Probe{hosting}, platform.Deps{Recorder: events})

	// A process start is not a recovery: the cache is seeded Reachable=false,
	// and a healthy engine must not record engine.recovered on every rollout.
	prober.ProbeAll(ctx, true)
	prober.ProbeAll(ctx, true)
	if kinds := events.kinds(); len(kinds) != 0 {
		t.Fatalf("first healthy probes recorded %v, want nothing", kinds)
	}

	hosting.fail(errors.New("dial tcp: connection refused"))
	prober.ProbeAll(ctx, true)
	prober.ProbeAll(ctx, true)
	kinds := events.kinds()
	if len(kinds) != 1 || kinds[0] != activity.KindEngineUnreachable {
		t.Fatalf("recorded %v, want a single engine.unreachable transition", kinds)
	}

	status, ok := prober.Status(platformv1.EngineKind_ENGINE_KIND_HOSTING)
	if !ok || status.Reachable {
		t.Fatalf("status = %+v/%v", status, ok)
	}

	hosting.fail(nil)
	prober.ProbeAll(ctx, true)
	prober.ProbeAll(ctx, true)
	kinds = events.kinds()
	if len(kinds) != 2 || kinds[1] != activity.KindEngineRecovered {
		t.Fatalf("recorded %v, want the recovery after the outage", kinds)
	}
}

// TestProberRecordsAnEngineThatIsDownAtStart is the other half of the
// first-probe rule: skipping the event for a healthy engine must not also
// silence an engine that never came up.
func TestProberRecordsAnEngineThatIsDownAtStart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	events := &recorder{}
	mail := &fakeProbe{kind: platformv1.EngineKind_ENGINE_KIND_MAIL, status: platform.EngineStatus{Configured: true}}
	mail.fail(errors.New("dial tcp: connection refused"))
	prober := platform.NewProber([]platform.Probe{mail}, platform.Deps{Recorder: events})

	prober.ProbeAll(ctx, true)
	prober.ProbeAll(ctx, true)
	if kinds := events.kinds(); len(kinds) != 1 || kinds[0] != activity.KindEngineUnreachable {
		t.Fatalf("recorded %v, want one engine.unreachable", kinds)
	}
}

// TestProbeAllProbesEnginesConcurrently pins the request-path bound: three
// engines that each take most of the budget must not cost their sum, or
// GetPlatformStatus{probe:true} would outlive the web client's transport
// timeout exactly when engines are slow.
func TestProbeAllProbesEnginesConcurrently(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	blocking := make([]*blockingProbe, 0, 3)
	probes := make([]platform.Probe, 0, 3)
	for _, kind := range []platformv1.EngineKind{
		platformv1.EngineKind_ENGINE_KIND_HOSTING,
		platformv1.EngineKind_ENGINE_KIND_MAIL,
		platformv1.EngineKind_ENGINE_KIND_BILLING,
	} {
		probe := &blockingProbe{kind: kind, release: release}
		blocking = append(blocking, probe)
		probes = append(probes, probe)
	}
	prober := platform.NewProber(probes, platform.Deps{Recorder: activity.Nop()})

	started := time.Now()
	go func() {
		time.Sleep(50 * time.Millisecond)
		close(release)
	}()
	prober.ProbeAll(context.Background(), true)
	if elapsed := time.Since(started); elapsed > 5*50*time.Millisecond {
		t.Fatalf("ProbeAll took %s for three 50ms probes; they are not concurrent", elapsed)
	}
	for _, probe := range blocking {
		if got := probe.probes(); got != 1 {
			t.Errorf("%v probed %d times, want 1", probe.Kind(), got)
		}
	}
}

// blockingProbe answers only once release is closed.
type blockingProbe struct {
	kind    platformv1.EngineKind
	release <-chan struct{}
	mu      sync.Mutex
	calls   int
}

func (p *blockingProbe) Kind() platformv1.EngineKind { return p.kind }

func (p *blockingProbe) Probe(ctx context.Context) error {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	select {
	case <-p.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *blockingProbe) Status() platform.EngineStatus {
	return platform.EngineStatus{Kind: p.kind, Configured: true, Reachable: true}
}

func (p *blockingProbe) probes() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func TestProberSaysNothingForAnUnconfiguredEngine(t *testing.T) {
	t.Parallel()
	events := &recorder{}
	probe := &fakeProbe{kind: platformv1.EngineKind_ENGINE_KIND_MAIL}
	prober := platform.NewProber([]platform.Probe{probe}, platform.Deps{Recorder: events})
	prober.ProbeAll(context.Background(), true)
	if kinds := events.kinds(); len(kinds) != 0 {
		t.Fatalf("recorded %v for an unconfigured engine, want nothing", kinds)
	}
}

func TestJanitorEnforcesEveryBound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, time.September, 3, 12, 0, 0, 0, time.UTC)
	store := openStore(t, platform.WithClock(func() time.Time { return now }))

	uploadDir := t.TempDir()
	cfg := testConfig(t)
	cfg.Activity = config.Activity{MaxEvents: 1000}
	cfg.Hosting = config.Hosting{
		APIURL: "http://127.0.0.1:30081", UploadsEnabled: true, UploadDir: uploadDir,
	}
	deps := platform.Deps{Store: store, Recorder: activity.Nop(), Clock: func() time.Time { return now }}
	janitor := platform.NewJanitor(store, cfg, deps)

	// Events beyond the ring.
	for index := range 1_010 {
		if _, err := store.AppendEvent(ctx, activity.EventDoc{
			Time: now.Add(time.Duration(index) * time.Millisecond), ZoneID: "z1",
			Actor: activity.ActorSystem, Kind: string(activity.KindZoneCreated),
			Severity: string(activity.SeverityInfo), Summary: "created",
		}); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}
	// A stale replay guard.
	if _, err := store.EnqueueWebhook(ctx, "evt_old", "invoice.paid", []byte(`{}`), now.Add(-40*24*time.Hour)); err != nil {
		t.Fatalf("EnqueueWebhook: %v", err)
	}
	// One site with more than the deploy bound.
	if err := store.PutSite(ctx, platform.SiteDoc{ZoneID: "z1", ZoneName: "acme.dev"}); err != nil {
		t.Fatalf("PutSite: %v", err)
	}
	for index := range platform.DeploysPerSite + 5 {
		id := store.NewDeployID(now.Add(time.Duration(index) * time.Second))
		if err := store.PutDeploy(ctx, platform.DeployDoc{
			ID: id, ZoneID: "z1", Phase: platform.DeployPhaseSuperseded,
		}); err != nil {
			t.Fatalf("PutDeploy: %v", err)
		}
		if err := store.PutDeployLog(ctx, id, platform.DeployLog{Lines: []string{"line"}}); err != nil {
			t.Fatalf("PutDeployLog: %v", err)
		}
	}
	// An expired upload with a directory, a stray directory with no row, and a
	// directory that is not ours at all: HOSTING_UPLOAD_DIR may be shared, so
	// only names shaped like an upload id are ever swept.
	expiredID := store.NewUploadID(now)
	expiredDir := filepath.Join(uploadDir, expiredID)
	if err := os.MkdirAll(expiredDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := store.PutUpload(ctx, platform.UploadDoc{
		ID: expiredID, ZoneID: "z1", Dir: expiredDir, ExpiresAt: now.Add(-time.Minute),
	}); err != nil {
		t.Fatalf("PutUpload: %v", err)
	}
	old := now.Add(-2 * time.Hour)
	strayDir := filepath.Join(uploadDir, store.NewUploadID(now.Add(-time.Hour)))
	if err := os.MkdirAll(strayDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.Chtimes(strayDir, old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	foreignDir := filepath.Join(uploadDir, "lost+found")
	if err := os.MkdirAll(foreignDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.Chtimes(foreignDir, old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	// The packed archive of a deploy that died between packing and the engine
	// accepting it. It is neither a directory nor a bare upload id, so it used
	// to survive every sweep — and it is not counted by the in-flight upload
	// budget either, so a few of them fill a small emptyDir for good.
	strayArchive := filepath.Join(uploadDir, store.NewUploadID(now.Add(-2*time.Hour))+platform.UploadArchiveSuffix)
	if err := os.WriteFile(strayArchive, make([]byte, 4096), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Chtimes(strayArchive, old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	foreignFile := filepath.Join(uploadDir, "notes.txt")
	if err := os.WriteFile(foreignFile, []byte("not ours"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Chtimes(foreignFile, old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	// A detached zone's deploy history: DeleteSite removes only the sites-bucket
	// key, and the per-site trim is driven from the site list, so nothing else
	// can ever reach these rows or their gzipped logs.
	orphanID := store.NewDeployID(now.Add(-90 * 24 * time.Hour))
	if err := store.PutDeploy(ctx, platform.DeployDoc{
		ID: orphanID, ZoneID: "gone", BuildID: "b-orphan",
		Phase: platform.DeployPhaseSuperseded, RequestedAt: now.Add(-90 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("PutDeploy: %v", err)
	}
	if err := store.PutDeployLog(ctx, orphanID, platform.DeployLog{Lines: []string{"line"}}); err != nil {
		t.Fatalf("PutDeployLog: %v", err)
	}
	recentOrphanID := store.NewDeployID(now.Add(-time.Hour))
	if err := store.PutDeploy(ctx, platform.DeployDoc{
		ID: recentOrphanID, ZoneID: "gone", Phase: platform.DeployPhaseSuperseded,
		RequestedAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("PutDeploy: %v", err)
	}

	// Checkout rows: one written on every CreateCheckoutSession, none of them
	// ever deleted. This was the only unbounded bucket in the store.
	if err := store.PutCheckout(ctx, platform.CheckoutDoc{
		SessionID: "cs_old", ZoneID: "z1", CreatedAt: now.Add(-90 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("PutCheckout: %v", err)
	}
	if err := store.PutCheckout(ctx, platform.CheckoutDoc{
		SessionID: "cs_recent", ZoneID: "z1", CreatedAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("PutCheckout: %v", err)
	}

	report, err := janitor.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if report.EventsTrimmed != 10 {
		t.Fatalf("EventsTrimmed = %d, want 10", report.EventsTrimmed)
	}
	if report.WebhooksPruned != 1 {
		t.Fatalf("WebhooksPruned = %d, want 1", report.WebhooksPruned)
	}
	if report.DeploysTrimmed != 5 {
		t.Fatalf("DeploysTrimmed = %d, want 5", report.DeploysTrimmed)
	}
	if report.UploadsRemoved != 1 {
		t.Fatalf("UploadsRemoved = %d, want 1", report.UploadsRemoved)
	}
	if report.DirsRemoved != 2 {
		t.Fatalf("DirsRemoved = %d, want the stray directory and the stray archive", report.DirsRemoved)
	}
	if report.OrphansTrimmed != 1 {
		t.Fatalf("OrphansTrimmed = %d, want only the detached zone's expired row", report.OrphansTrimmed)
	}
	if report.CheckoutsPruned != 1 {
		t.Fatalf("CheckoutsPruned = %d, want only the expired checkout", report.CheckoutsPruned)
	}
	if _, err := os.Stat(expiredDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired upload directory survived: %v", err)
	}
	if _, err := os.Stat(strayDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stray upload directory survived: %v", err)
	}
	if _, err := os.Stat(foreignDir); err != nil {
		t.Fatalf("the janitor deleted a directory it does not own: %v", err)
	}
	if _, err := os.Stat(strayArchive); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stray upload archive survived: %v", err)
	}
	if _, err := os.Stat(foreignFile); err != nil {
		t.Fatalf("the janitor deleted a file it does not own: %v", err)
	}

	count, err := store.CountEvents(ctx)
	if err != nil || count != 1000 {
		t.Fatalf("CountEvents = %d/%v, want 1000", count, err)
	}
	remaining, _, err := store.ListDeploys(ctx, "z1", 500, "")
	if err != nil {
		t.Fatalf("ListDeploys: %v", err)
	}
	if len(remaining) != platform.DeploysPerSite {
		t.Fatalf("deploys after trim = %d, want %d", len(remaining), platform.DeploysPerSite)
	}
	if _, err := store.GetDeploy(ctx, "gone", orphanID); !errors.Is(err, platform.ErrNotFound) {
		t.Fatalf("GetDeploy(expired orphan) = %v, want it swept", err)
	}
	if _, err := store.GetDeployLog(ctx, orphanID); !errors.Is(err, platform.ErrNotFound) {
		t.Fatalf("GetDeployLog(expired orphan) = %v, want the log swept with the row", err)
	}
	if _, err := store.GetDeploy(ctx, "gone", recentOrphanID); err != nil {
		t.Fatalf("GetDeploy(recent orphan) = %v, want a recent detach still readable", err)
	}
	if _, err := store.GetCheckout(ctx, "cs_old"); !errors.Is(err, platform.ErrNotFound) {
		t.Fatalf("GetCheckout(expired) = %v, want it swept", err)
	}
	if _, err := store.GetCheckout(ctx, "cs_recent"); err != nil {
		t.Fatalf("GetCheckout(recent) = %v, want it kept", err)
	}
}

func TestJanitorNeverTrimsALiveOrActiveDeploy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, time.September, 3, 12, 0, 0, 0, time.UTC)
	store := openStore(t, platform.WithClock(func() time.Time { return now }))

	// The oldest row is the live one: trimming it would leave the site with no
	// record of what is actually serving.
	liveID := store.NewDeployID(now)
	if err := store.PutDeploy(ctx, platform.DeployDoc{ID: liveID, ZoneID: "z1", Phase: platform.DeployPhaseLive, Live: true}); err != nil {
		t.Fatalf("PutDeploy: %v", err)
	}
	for index := range 5 {
		id := store.NewDeployID(now.Add(time.Duration(index+1) * time.Second))
		if err := store.PutDeploy(ctx, platform.DeployDoc{ID: id, ZoneID: "z1", Phase: platform.DeployPhaseSuperseded}); err != nil {
			t.Fatalf("PutDeploy: %v", err)
		}
	}
	removed, err := store.TrimDeploys(ctx, "z1", 2)
	if err != nil {
		t.Fatalf("TrimDeploys: %v", err)
	}
	if removed != 4 {
		t.Fatalf("TrimDeploys removed %d, want 4", removed)
	}
	if _, err := store.GetDeploy(ctx, "z1", liveID); err != nil {
		t.Fatalf("the live deploy was trimmed: %v", err)
	}
}

func TestRecordStartedNamesTheConfiguredEngines(t *testing.T) {
	t.Parallel()
	events := &recorder{}
	cfg := testConfig(t)
	cfg.Hosting = config.Hosting{APIURL: "http://127.0.0.1:30081"}
	service := platform.NewStatusService(cfg, platform.Deps{Recorder: events}, nil, nil)
	service.RecordStarted(context.Background())

	events.mu.Lock()
	defer events.mu.Unlock()
	if len(events.events) != 1 {
		t.Fatalf("recorded %d events, want 1", len(events.events))
	}
	event := events.events[0]
	if event.Kind != activity.KindPlatformStarted {
		t.Fatalf("kind = %s", event.Kind)
	}
	if event.Details["engines"] != "hosting" {
		t.Fatalf("engines detail = %q, want %q", event.Details["engines"], "hosting")
	}
	if !strings.Contains(event.Summary, "hosting") {
		t.Fatalf("summary = %q", event.Summary)
	}
}
