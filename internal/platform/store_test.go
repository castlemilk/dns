package platform_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/castlemilk/dns/internal/activity"
	"github.com/castlemilk/dns/internal/platform"
	"go.etcd.io/bbolt"
)

func openStore(t *testing.T, options ...platform.Option) *platform.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "platform.db")
	store, err := platform.Open(path, options...)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return store
}

func TestOpenCreatesTheFileWithTightPermissions(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "nested", "platform.db")
	store, err := platform.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	}()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
	version, err := store.SchemaVersion()
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if version != platform.DocVersion {
		t.Fatalf("SchemaVersion = %d, want %d", version, platform.DocVersion)
	}
	if store.Path() != path {
		t.Fatalf("Path = %q, want %q", store.Path(), path)
	}
}

func TestOpenRefusesANewerSchema(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "platform.db")
	db, err := bbolt.Open(path, 0o600, &bbolt.Options{Timeout: time.Second})
	if err != nil {
		t.Fatalf("bbolt.Open: %v", err)
	}
	err = db.Update(func(tx *bbolt.Tx) error {
		bucket, err := tx.CreateBucketIfNotExists([]byte("meta"))
		if err != nil {
			return err
		}
		payload, err := json.Marshal(map[string]any{"v": platform.DocVersion + 1})
		if err != nil {
			return err
		}
		return bucket.Put([]byte("schema"), payload)
	})
	if err != nil {
		t.Fatalf("seed schema: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close seed db: %v", err)
	}

	if _, err := platform.Open(path); err == nil {
		t.Fatal("expected Open to refuse a newer schema")
	} else if !strings.Contains(err.Error(), "newer than this binary") {
		t.Fatalf("error = %v", err)
	}
}

func TestSitesAndBindings(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t)

	if _, err := store.GetSite(ctx, "z1"); !errors.Is(err, platform.ErrNotFound) {
		t.Fatalf("GetSite on an empty store = %v, want ErrNotFound", err)
	}
	site, mail, err := store.HasBindings(ctx, "z1")
	if err != nil || site || mail {
		t.Fatalf("HasBindings = %v/%v/%v, want false/false/nil", site, mail, err)
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	if err := store.PutSite(ctx, platform.SiteDoc{ZoneID: "z1", ZoneName: "acme.dev", App: "acme-dev-1a2b3c", CreatedAt: now}); err != nil {
		t.Fatalf("PutSite: %v", err)
	}
	if err := store.PutMailDomain(ctx, platform.MailDomainDoc{ZoneID: "z2", ZoneName: "other.dev", CreatedAt: now}); err != nil {
		t.Fatalf("PutMailDomain: %v", err)
	}

	site, mail, err = store.HasBindings(ctx, "z1")
	if err != nil || !site || mail {
		t.Fatalf("HasBindings(z1) = %v/%v/%v, want true/false/nil", site, mail, err)
	}
	site, mail, err = store.HasBindings(ctx, "z2")
	if err != nil || site || !mail {
		t.Fatalf("HasBindings(z2) = %v/%v/%v, want false/true/nil", site, mail, err)
	}

	stored, err := store.GetSite(ctx, "z1")
	if err != nil {
		t.Fatalf("GetSite: %v", err)
	}
	if stored.ZoneName != "acme.dev" || stored.V != platform.DocVersion {
		t.Fatalf("site round-trip = %+v", stored)
	}

	if err := store.DeleteSite(ctx, "z1"); err != nil {
		t.Fatalf("DeleteSite: %v", err)
	}
	site, _, err = store.HasBindings(ctx, "z1")
	if err != nil || site {
		t.Fatalf("HasBindings after delete = %v/%v", site, err)
	}
}

func TestEmptyForRebuild(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t)

	empty, err := store.EmptyForRebuild(ctx)
	if err != nil || !empty {
		t.Fatalf("EmptyForRebuild on a fresh store = %v/%v", empty, err)
	}
	// A deploy row alone must not block a rebuild: only sites, mail domains and
	// subscriptions are reconstructable, and only those are checked.
	if err := store.PutDeploy(ctx, platform.DeployDoc{ID: "d1", ZoneID: "z1", Phase: platform.DeployPhaseLive}); err != nil {
		t.Fatalf("PutDeploy: %v", err)
	}
	empty, err = store.EmptyForRebuild(ctx)
	if err != nil || !empty {
		t.Fatalf("EmptyForRebuild with a deploy row = %v/%v, want true", empty, err)
	}
	if err := store.PutSubscription(ctx, platform.SubscriptionDoc{ZoneID: "z1", State: "ACTIVE"}); err != nil {
		t.Fatalf("PutSubscription: %v", err)
	}
	empty, err = store.EmptyForRebuild(ctx)
	if err != nil || empty {
		t.Fatalf("EmptyForRebuild with a subscription = %v/%v, want false", empty, err)
	}
}

func TestDeployPagingIsNewestFirst(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := time.Date(2026, time.September, 3, 12, 0, 0, 0, time.UTC)
	store := openStore(t, platform.WithClock(func() time.Time { return base }))

	var ids []string
	for index := range 7 {
		at := base.Add(time.Duration(index) * time.Second)
		id := store.NewDeployID(at)
		ids = append(ids, id)
		zone := "z1"
		if index%2 == 1 {
			zone = "z2"
		}
		doc := platform.DeployDoc{
			ID: id, ZoneID: zone, ZoneName: zone + ".dev", Kind: "git",
			BuildID: "b-shared", Phase: platform.DeployPhaseLive, RequestedAt: at,
		}
		if err := store.PutDeploy(ctx, doc); err != nil {
			t.Fatalf("PutDeploy: %v", err)
		}
	}

	page, next, err := store.ListDeploys(ctx, "", 3, "")
	if err != nil {
		t.Fatalf("ListDeploys: %v", err)
	}
	if len(page) != 3 || next == "" {
		t.Fatalf("first page = %d rows, next = %q", len(page), next)
	}
	if page[0].ID != ids[6] || page[2].ID != ids[4] {
		t.Fatalf("page order = %s..%s, want newest first", page[0].ID, page[2].ID)
	}
	second, _, err := store.ListDeploys(ctx, "", 3, next)
	if err != nil {
		t.Fatalf("ListDeploys page 2: %v", err)
	}
	if len(second) != 3 || second[0].ID != ids[3] {
		t.Fatalf("second page = %d rows starting %s", len(second), second[0].ID)
	}

	perZone, _, err := store.ListDeploys(ctx, "z2", 10, "")
	if err != nil {
		t.Fatalf("ListDeploys(z2): %v", err)
	}
	if len(perZone) != 3 {
		t.Fatalf("z2 rows = %d, want 3", len(perZone))
	}
	for _, doc := range perZone {
		if doc.ZoneID != "z2" {
			t.Fatalf("zone filter leaked %s", doc.ZoneID)
		}
	}

	shared, err := store.DeploysByBuild(ctx, "b-shared")
	if err != nil {
		t.Fatalf("DeploysByBuild: %v", err)
	}
	if len(shared) != 7 {
		t.Fatalf("DeploysByBuild = %d rows, want 7", len(shared))
	}
}

func TestActiveDeploys(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t)
	phases := []string{
		platform.DeployPhaseQueued, platform.DeployPhaseBuilding, platform.DeployPhaseReleasing,
		platform.DeployPhaseLive, platform.DeployPhaseFailed, platform.DeployPhaseAbandoned,
	}
	for index, phase := range phases {
		doc := platform.DeployDoc{ID: string(rune('a' + index)), ZoneID: "z1", Phase: phase}
		if err := store.PutDeploy(ctx, doc); err != nil {
			t.Fatalf("PutDeploy: %v", err)
		}
	}
	active, err := store.ActiveDeploys(ctx)
	if err != nil {
		t.Fatalf("ActiveDeploys: %v", err)
	}
	if len(active) != 3 {
		t.Fatalf("ActiveDeploys = %d rows, want 3", len(active))
	}
}

func TestDeployLogRoundTripAndCap(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t)

	if _, err := store.GetDeployLog(ctx, "missing"); !errors.Is(err, platform.ErrNotFound) {
		t.Fatalf("GetDeployLog(missing) = %v, want ErrNotFound", err)
	}
	captured := time.Date(2026, time.September, 3, 9, 0, 0, 0, time.UTC)
	if err := store.PutDeployLog(ctx, "d1", platform.DeployLog{
		Lines: []string{"step 1", "step 2"}, Complete: true, CapturedAt: captured,
	}); err != nil {
		t.Fatalf("PutDeployLog: %v", err)
	}
	log, err := store.GetDeployLog(ctx, "d1")
	if err != nil {
		t.Fatalf("GetDeployLog: %v", err)
	}
	if len(log.Lines) != 2 || !log.Complete || log.Truncated || !log.CapturedAt.Equal(captured) {
		t.Fatalf("log round-trip = %+v", log)
	}

	// A runaway build must be trimmed and say so, never stored whole.
	huge := make([]string, 0, 20_000)
	for range 20_000 {
		huge = append(huge, strings.Repeat("x", 100))
	}
	if err := store.PutDeployLog(ctx, "d2", platform.DeployLog{Lines: huge, Complete: true}); err != nil {
		t.Fatalf("PutDeployLog(huge): %v", err)
	}
	trimmed, err := store.GetDeployLog(ctx, "d2")
	if err != nil {
		t.Fatalf("GetDeployLog(d2): %v", err)
	}
	if !trimmed.Truncated {
		t.Fatal("an oversize log must report Truncated")
	}
	if len(trimmed.Lines) >= len(huge) {
		t.Fatalf("oversize log kept %d of %d lines", len(trimmed.Lines), len(huge))
	}
}

func TestEventIDsAreMonotonicUnderConcurrency(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	frozen := time.Date(2026, time.September, 3, 0, 0, 0, 0, time.UTC)
	store := openStore(t, platform.WithClock(func() time.Time { return frozen }))

	const writers = 8
	const perWriter = 25
	var wait sync.WaitGroup
	ids := make(chan string, writers*perWriter)
	for range writers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for range perWriter {
				id, err := store.AppendEvent(ctx, activity.EventDoc{
					Actor: activity.ActorSystem, Kind: string(activity.KindPlatformStarted),
					Severity: string(activity.SeverityInfo), Summary: "started",
				})
				if err != nil {
					t.Errorf("AppendEvent: %v", err)
					return
				}
				ids <- id
			}
		}()
	}
	wait.Wait()
	close(ids)

	seen := make(map[string]struct{}, writers*perWriter)
	for id := range ids {
		if _, duplicate := seen[id]; duplicate {
			t.Fatalf("duplicate event id %q under a frozen clock", id)
		}
		seen[id] = struct{}{}
	}
	count, err := store.CountEvents(ctx)
	if err != nil {
		t.Fatalf("CountEvents: %v", err)
	}
	if count != uint64(writers*perWriter) {
		t.Fatalf("CountEvents = %d, want %d", count, writers*perWriter)
	}
}

func TestEventListingFiltersAndPages(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t)

	base := time.Date(2026, time.September, 3, 8, 0, 0, 0, time.UTC)
	kinds := []activity.Kind{
		activity.KindZoneCreated, activity.KindDeployRequested, activity.KindDeployLive,
		activity.KindZoneDeleted, activity.KindDeployFailed,
	}
	for index, kind := range kinds {
		zone := "z1"
		if index%2 == 1 {
			zone = "z2"
		}
		if _, err := store.AppendEvent(ctx, activity.EventDoc{
			Time: base.Add(time.Duration(index) * time.Minute), ZoneID: zone, ZoneName: zone + ".dev",
			Actor: activity.ActorOperator, Kind: string(kind), Severity: string(activity.SeverityInfo),
			Summary: string(kind),
		}); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}

	all, next, err := store.ListEvents(ctx, activity.ListFilter{Limit: 2})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(all) != 2 || all[0].Kind != string(activity.KindDeployFailed) {
		t.Fatalf("first page = %+v", all)
	}
	if next == "" {
		t.Fatal("expected a cursor for the second page")
	}
	page2, _, err := store.ListEvents(ctx, activity.ListFilter{Limit: 2, Cursor: next})
	if err != nil {
		t.Fatalf("ListEvents page 2: %v", err)
	}
	if len(page2) != 2 || page2[0].Kind != string(activity.KindDeployLive) {
		t.Fatalf("second page = %+v", page2)
	}

	// A kind filter must not consume the page limit with rows it rejects.
	deploys, _, err := store.ListEvents(ctx, activity.ListFilter{Kinds: []string{"deploy."}, Limit: 10})
	if err != nil {
		t.Fatalf("ListEvents(deploy.): %v", err)
	}
	if len(deploys) != 3 {
		t.Fatalf("deploy. prefix matched %d events, want 3", len(deploys))
	}

	zoned, _, err := store.ListEvents(ctx, activity.ListFilter{ZoneID: "z2", Limit: 10})
	if err != nil {
		t.Fatalf("ListEvents(z2): %v", err)
	}
	if len(zoned) != 2 {
		t.Fatalf("zone filter matched %d events, want 2", len(zoned))
	}
	for _, doc := range zoned {
		if doc.ZoneID != "z2" {
			t.Fatalf("zone filter leaked %s", doc.ZoneID)
		}
	}

	recent, _, err := store.ListEvents(ctx, activity.ListFilter{Since: base.Add(3 * time.Minute), Limit: 10})
	if err != nil {
		t.Fatalf("ListEvents(since): %v", err)
	}
	if len(recent) != 2 {
		t.Fatalf("since filter matched %d events, want 2", len(recent))
	}

	one, err := store.GetEvent(ctx, all[0].ID)
	if err != nil || one.ID != all[0].ID {
		t.Fatalf("GetEvent = %+v/%v", one, err)
	}
	if _, err := store.GetEvent(ctx, "nope"); !errors.Is(err, activity.ErrEventNotFound) {
		t.Fatalf("GetEvent(nope) = %v, want activity.ErrEventNotFound", err)
	}
}

func TestPruneEventsKeepsTheNewest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t)

	base := time.Date(2026, time.September, 3, 8, 0, 0, 0, time.UTC)
	for index := range 20 {
		if _, err := store.AppendEvent(ctx, activity.EventDoc{
			Time: base.Add(time.Duration(index) * time.Second), ZoneID: "z1",
			Actor: activity.ActorSystem, Kind: string(activity.KindZoneCreated),
			Severity: string(activity.SeverityInfo), Summary: "created",
		}); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}
	removed, err := store.PruneEvents(ctx, 5)
	if err != nil {
		t.Fatalf("PruneEvents: %v", err)
	}
	if removed != 15 {
		t.Fatalf("PruneEvents removed %d, want 15", removed)
	}
	count, err := store.CountEvents(ctx)
	if err != nil || count != 5 {
		t.Fatalf("CountEvents = %d/%v, want 5", count, err)
	}
	// The zone index must shrink with the ring, or a per-zone listing keeps
	// pointing at events that no longer exist.
	zoned, _, err := store.ListEvents(ctx, activity.ListFilter{ZoneID: "z1", Limit: 50})
	if err != nil {
		t.Fatalf("ListEvents(z1): %v", err)
	}
	if len(zoned) != 5 {
		t.Fatalf("per-zone listing after prune = %d, want 5", len(zoned))
	}
}

func TestWebhookQueueSemantics(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, time.September, 3, 10, 0, 0, 0, time.UTC)
	store := openStore(t, platform.WithClock(func() time.Time { return now }))

	queued, err := store.EnqueueWebhook(ctx, "evt_1", "invoice.paid", []byte(`{"id":"evt_1"}`), now)
	if err != nil || !queued {
		t.Fatalf("EnqueueWebhook = %v/%v", queued, err)
	}
	// The same event id must never be applied twice, however often Stripe
	// redelivers it.
	queued, err = store.EnqueueWebhook(ctx, "evt_1", "invoice.paid", []byte(`{"id":"evt_1"}`), now)
	if err != nil || queued {
		t.Fatalf("duplicate EnqueueWebhook = %v/%v, want false", queued, err)
	}

	due, err := store.NextWebhooks(ctx, 10, now)
	if err != nil || len(due) != 1 {
		t.Fatalf("NextWebhooks = %d/%v", len(due), err)
	}
	entry := due[0]

	if err := store.AckWebhook(ctx, entry.Key, platform.WebhookOutcomeRetry, "engine down", now.Add(5*time.Second)); err != nil {
		t.Fatalf("AckWebhook(retry): %v", err)
	}
	stillDue, err := store.NextWebhooks(ctx, 10, now)
	if err != nil {
		t.Fatalf("NextWebhooks: %v", err)
	}
	if len(stillDue) != 0 {
		t.Fatal("a retried entry must not be due before its next attempt")
	}
	later, err := store.NextWebhooks(ctx, 10, now.Add(10*time.Second))
	if err != nil || len(later) != 1 || later[0].Attempts != 1 {
		t.Fatalf("NextWebhooks after backoff = %+v/%v", later, err)
	}

	if err := store.AckWebhook(ctx, entry.Key, platform.WebhookOutcomeDead, "gave up", now); err != nil {
		t.Fatalf("AckWebhook(dead): %v", err)
	}
	stats, err := store.WebhookStats(ctx)
	if err != nil || stats.Dead != 1 || stats.Pending != 0 {
		t.Fatalf("WebhookStats = %+v/%v", stats, err)
	}
	if !stats.LastReceivedAt.Equal(now) {
		t.Fatalf("LastReceivedAt = %s, want %s", stats.LastReceivedAt, now)
	}

	retried, err := store.RetryDeadWebhooks(ctx)
	if err != nil || retried != 1 {
		t.Fatalf("RetryDeadWebhooks = %d/%v", retried, err)
	}
	revived, err := store.NextWebhooks(ctx, 10, now)
	if err != nil || len(revived) != 1 || revived[0].Attempts != 0 {
		t.Fatalf("revived entry = %+v/%v", revived, err)
	}

	if err := store.AckWebhook(ctx, entry.Key, platform.WebhookOutcomeDone, "", time.Time{}); err != nil {
		t.Fatalf("AckWebhook(done): %v", err)
	}
	remaining, err := store.NextWebhooks(ctx, 10, now)
	if err != nil || len(remaining) != 0 {
		t.Fatalf("queue after done = %+v/%v", remaining, err)
	}

	// The replay guard outlives the queue entry, so a redelivery after the
	// queue drained is still a duplicate.
	queued, err = store.EnqueueWebhook(ctx, "evt_1", "invoice.paid", []byte(`{}`), now)
	if err != nil || queued {
		t.Fatalf("redelivery after processing = %v/%v, want duplicate", queued, err)
	}

	dropped, err := store.DeleteWebhookEventsBefore(ctx, now.Add(31*24*time.Hour))
	if err != nil || dropped != 1 {
		t.Fatalf("DeleteWebhookEventsBefore = %d/%v", dropped, err)
	}
}

func TestCheckoutsCustomerAndSubscriptions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t)

	if _, found, err := store.GetCustomer(ctx); err != nil || found {
		t.Fatalf("GetCustomer on an empty store = %v/%v", found, err)
	}
	if err := store.PutCustomer(ctx, platform.CustomerDoc{CustomerID: "cus_1", Email: "billing@example.com"}); err != nil {
		t.Fatalf("PutCustomer: %v", err)
	}
	customer, found, err := store.GetCustomer(ctx)
	if err != nil || !found || customer.CustomerID != "cus_1" {
		t.Fatalf("GetCustomer = %+v/%v/%v", customer, found, err)
	}

	if err := store.PutCheckout(ctx, platform.CheckoutDoc{SessionID: "cs_1", ZoneID: "z1"}); err != nil {
		t.Fatalf("PutCheckout: %v", err)
	}
	if err := store.PutCheckout(ctx, platform.CheckoutDoc{SessionID: "cs_2", ZoneID: "z2", Completed: true}); err != nil {
		t.Fatalf("PutCheckout: %v", err)
	}
	pending, err := store.ListPendingCheckouts(ctx)
	if err != nil || len(pending) != 1 || pending[0].SessionID != "cs_1" {
		t.Fatalf("ListPendingCheckouts = %+v/%v", pending, err)
	}

	if err := store.PutSubscription(ctx, platform.SubscriptionDoc{ZoneID: "z1", State: "ACTIVE"}); err != nil {
		t.Fatalf("PutSubscription: %v", err)
	}
	subscriptions, err := store.ListSubscriptions(ctx)
	if err != nil || len(subscriptions) != 1 {
		t.Fatalf("ListSubscriptions = %+v/%v", subscriptions, err)
	}
	if err := store.DeleteSubscription(ctx, "z1"); err != nil {
		t.Fatalf("DeleteSubscription: %v", err)
	}
	if _, err := store.GetSubscription(ctx, "z1"); !errors.Is(err, platform.ErrNotFound) {
		t.Fatalf("GetSubscription after delete = %v", err)
	}
}

func TestGatewayAndCapabilitiesDefaultToZeroValues(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t)

	// A store with no gateway doc must read as "nothing resolved yet", not as
	// an error the reconciler has to special-case.
	gateway, err := store.GetGateway(ctx)
	if err != nil {
		t.Fatalf("GetGateway: %v", err)
	}
	if len(gateway.Addresses) != 0 || gateway.Source != "" {
		t.Fatalf("GetGateway on an empty store = %+v", gateway)
	}
	capabilities, err := store.GetCapabilities(ctx)
	if err != nil {
		t.Fatalf("GetCapabilities: %v", err)
	}
	if capabilities.ListBuilds || capabilities.DeleteDomain {
		t.Fatalf("capabilities default = %+v, want all false", capabilities)
	}
	for _, key := range []string{"list_builds", "delete_domain", "build_timestamps", "domain_readiness", "tenant_scoped"} {
		if _, ok := capabilities.Map()[key]; !ok {
			t.Fatalf("capability map is missing %q", key)
		}
	}

	if err := store.PutGateway(ctx, platform.GatewayDoc{
		Addresses: []string{"198.51.100.10"}, Source: platform.GatewaySourceResolved, CandidateSeen: 2,
	}); err != nil {
		t.Fatalf("PutGateway: %v", err)
	}
	gateway, err = store.GetGateway(ctx)
	if err != nil || len(gateway.Addresses) != 1 || gateway.Source != platform.GatewaySourceResolved {
		t.Fatalf("gateway round-trip = %+v/%v", gateway, err)
	}
	if err := store.PutCapabilities(ctx, platform.CapabilitiesDoc{ListBuilds: true, DeleteDomain: true}); err != nil {
		t.Fatalf("PutCapabilities: %v", err)
	}
	capabilities, err = store.GetCapabilities(ctx)
	if err != nil || !capabilities.ListBuilds || !capabilities.DeleteDomain {
		t.Fatalf("capabilities round-trip = %+v/%v", capabilities, err)
	}
}

func TestUploadsExpire(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t)
	now := time.Date(2026, time.September, 3, 11, 0, 0, 0, time.UTC)

	first := store.NewUploadID(now)
	second := store.NewUploadID(now.Add(time.Millisecond))
	if err := store.PutUpload(ctx, platform.UploadDoc{ID: first, ZoneID: "z1", ExpiresAt: now.Add(-time.Minute)}); err != nil {
		t.Fatalf("PutUpload: %v", err)
	}
	if err := store.PutUpload(ctx, platform.UploadDoc{ID: second, ZoneID: "z1", ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatalf("PutUpload: %v", err)
	}
	expired, err := store.ListExpiredUploads(ctx, now)
	if err != nil || len(expired) != 1 || expired[0].ID != first {
		t.Fatalf("ListExpiredUploads = %+v/%v", expired, err)
	}
	if err := store.DeleteUpload(ctx, first); err != nil {
		t.Fatalf("DeleteUpload: %v", err)
	}
	if _, err := store.GetUpload(ctx, first); !errors.Is(err, platform.ErrNotFound) {
		t.Fatalf("GetUpload after delete = %v", err)
	}
}

// TestUploadIDsHaveTheDocumentedShape pins the invariant the janitor relies on:
// it deletes a directory under HOSTING_UPLOAD_DIR only when the name is an
// upload id, so an operator-shared directory keeps its other contents.
func TestUploadIDsHaveTheDocumentedShape(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t)
	now := time.Date(2026, time.September, 3, 11, 0, 0, 0, time.UTC)

	id := store.NewUploadID(now)
	if !platform.ValidUploadID(id) {
		t.Fatalf("NewUploadID returned %q, which does not match %s", id, platform.UploadIDPattern)
	}
	for _, bad := range []string{"", "u1", "node_modules", "../escape", strings.ToUpper(id), id + "x"} {
		if platform.ValidUploadID(bad) {
			t.Errorf("ValidUploadID(%q) = true", bad)
		}
		if err := store.PutUpload(ctx, platform.UploadDoc{ID: bad, ZoneID: "z1"}); err == nil {
			t.Errorf("PutUpload accepted the id %q", bad)
		}
	}
}

func TestBackupProducesAFileBboltReopens(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t)
	if err := store.PutSite(ctx, platform.SiteDoc{ZoneID: "z1", ZoneName: "acme.dev"}); err != nil {
		t.Fatalf("PutSite: %v", err)
	}

	copyPath := filepath.Join(t.TempDir(), "copy.db")
	file, err := os.Create(copyPath)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	written, err := store.Backup(ctx, file)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close copy: %v", err)
	}
	size, err := store.Size(ctx)
	if err != nil {
		t.Fatalf("Size: %v", err)
	}
	if written != size {
		t.Fatalf("Backup wrote %d bytes, Size reports %d", written, size)
	}

	// The live store keeps running: this is the whole point of serving the
	// backup from the process that owns the lock.
	if err := store.PutSite(ctx, platform.SiteDoc{ZoneID: "z2", ZoneName: "other.dev"}); err != nil {
		t.Fatalf("PutSite after backup: %v", err)
	}

	reopened, err := bbolt.Open(copyPath, 0o600, &bbolt.Options{ReadOnly: true, Timeout: time.Second})
	if err != nil {
		t.Fatalf("reopen backup read-only: %v", err)
	}
	defer func() {
		if err := reopened.Close(); err != nil {
			t.Errorf("close backup: %v", err)
		}
	}()
	err = reopened.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket([]byte("sites"))
		if bucket == nil {
			t.Fatal("backup has no sites bucket")
		}
		if bucket.Get([]byte("z1")) == nil {
			t.Fatal("backup is missing the site written before it")
		}
		if bucket.Get([]byte("z2")) != nil {
			t.Fatal("backup contains a site written after the snapshot")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
}

func TestEveryMethodChecksTheContext(t *testing.T) {
	t.Parallel()
	store := openStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := store.ListSites(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("ListSites with a cancelled context = %v", err)
	}
	if err := store.PutSite(ctx, platform.SiteDoc{ZoneID: "z1"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("PutSite with a cancelled context = %v", err)
	}
	if _, err := store.CountEvents(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("CountEvents with a cancelled context = %v", err)
	}
}

var _ activity.EventStore = (*platform.Store)(nil)
