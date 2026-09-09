package control

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	dnsv1 "github.com/castlemilk/dns/gen/go/dns/v1"
	"github.com/castlemilk/dns/internal/activity"
	"github.com/castlemilk/dns/internal/authoritative"
	"github.com/castlemilk/dns/internal/enginedns"
	"github.com/castlemilk/dns/internal/zone"
)

type capturedEvents struct {
	mu     sync.Mutex
	events []activity.Event
}

func (c *capturedEvents) Record(_ context.Context, event activity.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, event)
}

func (c *capturedEvents) all() []activity.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.events)
}

func (c *capturedEvents) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = nil
}

func (c *capturedEvents) kinds() []string {
	kinds := make([]string, 0)
	for _, event := range c.all() {
		kinds = append(kinds, string(event.Kind))
	}
	return kinds
}

type observerCall struct {
	Method   string
	ZoneID   string
	RecordID string
}

type capturingObserver struct {
	mu    sync.Mutex
	calls []observerCall
}

func (o *capturingObserver) ZoneReplaced(_ context.Context, zoneID string) {
	o.add(observerCall{Method: "ZoneReplaced", ZoneID: zoneID})
}

func (o *capturingObserver) RecordDeleted(_ context.Context, zoneID string, record zone.Record) {
	o.add(observerCall{Method: "RecordDeleted", ZoneID: zoneID, RecordID: record.ID})
}

func (o *capturingObserver) ZoneDeleted(_ context.Context, zoneID string) {
	o.add(observerCall{Method: "ZoneDeleted", ZoneID: zoneID})
}

func (o *capturingObserver) add(call observerCall) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.calls = append(o.calls, call)
}

func (o *capturingObserver) all() []observerCall {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.calls)
}

func (o *capturingObserver) reset() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.calls = nil
}

type stubBindings struct {
	site bool
	mail bool
	err  error
}

func (s stubBindings) HasBindings(context.Context, string) (bool, bool, error) {
	return s.site, s.mail, s.err
}

func newEngineHandler(t *testing.T, options ...Option) (*Handler, *zone.Store, *capturedEvents, *capturingObserver) {
	t.Helper()
	store, err := zone.Open(filepath.Join(t.TempDir(), "zones.db"), []string{"ns1.example.test"})
	if err != nil {
		t.Fatalf("zone.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	recorder := &capturedEvents{}
	observer := &capturingObserver{}
	dnsServer := authoritative.New(discardLogger(), authoritative.DefaultMaxUDPSize)
	options = append([]Option{WithRecorder(recorder)}, options...)
	handler := NewHandler(store, dnsServer, discardLogger(), time.Unix(1_800_000_000, 0), nil, options...)
	handler.AddZoneObserver(observer)
	return handler, store, recorder, observer
}

func createZone(t *testing.T, handler *Handler, name string) string {
	t.Helper()
	created, err := handler.CreateZone(context.Background(), connect.NewRequest(&dnsv1.CreateZoneRequest{Name: name}))
	if err != nil {
		t.Fatalf("CreateZone(%q): %v", name, err)
	}
	return created.Msg.GetZone().GetId()
}

func gatewayOps(t *testing.T, zoneName string) []enginedns.Op {
	t.Helper()
	plan, err := enginedns.Plan(zone.Zone{Name: zoneName}, enginedns.SourceHosting, []zone.Record{
		{Name: "@", Type: zone.TypeA, TTL: 300, Value: "198.51.100.10"},
		{Name: "www", Type: zone.TypeCNAME, TTL: 300, Value: zoneName + "."},
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	return plan.WorkerOps()
}

func TestListZonesReportsProvenance(t *testing.T) {
	t.Parallel()

	handler, _, _, _ := newEngineHandler(t)
	ctx := context.Background()
	zoneID := createZone(t, handler, "acme.dev")
	if _, err := handler.ApplyRecordSet(ctx, zoneID, gatewayOps(t, "acme.dev"), activity.ActorHostingEngine, "site-1"); err != nil {
		t.Fatalf("ApplyRecordSet: %v", err)
	}

	listed, err := handler.ListZones(ctx, connect.NewRequest(&dnsv1.ListZonesRequest{}))
	if err != nil {
		t.Fatalf("ListZones: %v", err)
	}
	seen := map[dnsv1.RecordSource]int{}
	for _, record := range listed.Msg.GetZones()[0].GetRecords() {
		seen[record.GetSource()]++
	}
	if seen[dnsv1.RecordSource_RECORD_SOURCE_UNSPECIFIED] != 0 {
		t.Errorf("%d records report an unspecified source; every record must say who wrote it",
			seen[dnsv1.RecordSource_RECORD_SOURCE_UNSPECIFIED])
	}
	if seen[dnsv1.RecordSource_RECORD_SOURCE_HOSTING] != 2 {
		t.Errorf("hosting records = %d, want 2", seen[dnsv1.RecordSource_RECORD_SOURCE_HOSTING])
	}
	if seen[dnsv1.RecordSource_RECORD_SOURCE_USER] == 0 {
		t.Error("the managed SOA and NS records do not report the user source")
	}
}

func TestEngineOwnedRecordsRefuseEditsWithTheReleasePath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		source  string
		ops     func(t *testing.T, zoneName string) []enginedns.Op
		wantSub string
	}{
		{
			name:    "website",
			source:  enginedns.SourceHosting,
			ops:     gatewayOps,
			wantSub: "Website card → Detach",
		},
		{
			name:   "email",
			source: enginedns.SourceMail,
			ops: func(t *testing.T, zoneName string) []enginedns.Op {
				t.Helper()
				plan, err := enginedns.Plan(zone.Zone{Name: zoneName}, enginedns.SourceMail, []zone.Record{
					{Name: "@", Type: zone.TypeMX, TTL: 3600, Value: "10 mx1.simple.host."},
				})
				if err != nil {
					t.Fatalf("Plan: %v", err)
				}
				return plan.WorkerOps()
			},
			wantSub: "Email card → Unbind",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			handler, store, _, _ := newEngineHandler(t)
			ctx := context.Background()
			zoneID := createZone(t, handler, "acme.dev")
			applied, err := handler.ApplyRecordSet(ctx, zoneID, test.ops(t, "acme.dev"), activity.ActorHostingEngine, "")
			if err != nil {
				t.Fatalf("ApplyRecordSet: %v", err)
			}
			var target zone.Record
			for _, record := range applied.Zone.Records {
				if record.Source == test.source {
					target = record
					break
				}
			}
			if target.ID == "" {
				t.Fatal("no engine record was written")
			}

			_, err = handler.UpdateRecord(ctx, connect.NewRequest(&dnsv1.UpdateRecordRequest{
				ZoneId: zoneID, RecordId: target.ID, Name: target.Name,
				Type: recordTypeToProto(target.Type), Ttl: target.TTL, Value: "203.0.113.9",
			}))
			if target.Type == zone.TypeMX {
				_, err = handler.UpdateRecord(ctx, connect.NewRequest(&dnsv1.UpdateRecordRequest{
					ZoneId: zoneID, RecordId: target.ID, Name: target.Name,
					Type: recordTypeToProto(target.Type), Ttl: target.TTL, Value: "10 mail.elsewhere.test.",
				}))
			}
			if connect.CodeOf(err) != connect.CodeFailedPrecondition {
				t.Fatalf("UpdateRecord = %v (%s), want FailedPrecondition", err, connect.CodeOf(err))
			}
			if !strings.Contains(err.Error(), test.wantSub) {
				t.Errorf("UpdateRecord error = %q, want it to name the release path %q", err, test.wantSub)
			}

			_, err = handler.DeleteRecord(ctx, connect.NewRequest(&dnsv1.DeleteRecordRequest{
				ZoneId: zoneID, RecordId: target.ID,
			}))
			if connect.CodeOf(err) != connect.CodeFailedPrecondition {
				t.Fatalf("DeleteRecord = %v (%s), want FailedPrecondition", err, connect.CodeOf(err))
			}
			if !strings.Contains(err.Error(), test.wantSub) {
				t.Errorf("DeleteRecord error = %q, want it to name the release path", err)
			}

			after, err := store.Get(ctx, zoneID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			found, ok := findRecord(after, target.ID)
			if !ok || found.Value != target.Value {
				t.Errorf("the refused edit changed the record: %+v", found)
			}
		})
	}
}

func TestPublicErrorMapsTheEngineOwnedSentinel(t *testing.T) {
	t.Parallel()

	handler := NewHandler(nil, nil, discardLogger(), time.Time{}, nil)
	err := handler.publicError(errors.New("wrapped: " + zone.ErrEngineOwned.Error()))
	if connect.CodeOf(err) == connect.CodeFailedPrecondition {
		t.Fatal("publicError matched on the message text rather than the sentinel")
	}
	err = handler.publicError(errors.Join(errors.New("apply"), zone.ErrEngineOwned))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("publicError(ErrEngineOwned) = %s, want FailedPrecondition", connect.CodeOf(err))
	}
	if !strings.Contains(err.Error(), "written by an engine") {
		t.Errorf("publicError message = %q", err)
	}
}

func TestDeleteZoneIsRefusedWhileAnEngineHoldsTheDomain(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		bindings stubBindings
		wantCode connect.Code
	}{
		{name: "site bound", bindings: stubBindings{site: true}, wantCode: connect.CodeFailedPrecondition},
		{name: "mail bound", bindings: stubBindings{mail: true}, wantCode: connect.CodeFailedPrecondition},
		{name: "unbound", bindings: stubBindings{}, wantCode: 0},
		{name: "checker failed", bindings: stubBindings{err: errors.New("store is down")}, wantCode: connect.CodeInternal},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			handler, _, _, _ := newEngineHandler(t, WithBindingChecker(test.bindings))
			zoneID := createZone(t, handler, "acme.dev")
			_, err := handler.DeleteZone(context.Background(), connect.NewRequest(&dnsv1.DeleteZoneRequest{ZoneId: zoneID}))
			if test.wantCode == 0 {
				if err != nil {
					t.Fatalf("DeleteZone = %v, want success", err)
				}
				return
			}
			if connect.CodeOf(err) != test.wantCode {
				t.Fatalf("DeleteZone = %v (%s), want %s", err, connect.CodeOf(err), test.wantCode)
			}
			if test.wantCode == connect.CodeFailedPrecondition &&
				!strings.Contains(err.Error(), "detach the website and unbind email") {
				t.Errorf("DeleteZone error = %q", err)
			}
		})
	}
}

func TestWithBindingCheckerFunc(t *testing.T) {
	t.Parallel()

	handler, _, _, _ := newEngineHandler(t, WithBindingCheckerFunc(func(context.Context, string) (bool, bool, error) {
		return false, true, nil
	}))
	zoneID := createZone(t, handler, "acme.dev")
	_, err := handler.DeleteZone(context.Background(), connect.NewRequest(&dnsv1.DeleteZoneRequest{ZoneId: zoneID}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("DeleteZone = %v, want the function-shaped checker to refuse", err)
	}
	nilChecker := NewHandler(nil, nil, discardLogger(), time.Time{}, nil, WithBindingCheckerFunc(nil))
	if nilChecker.bindings != nil {
		t.Error("a nil checker was installed")
	}
}

func TestImportWarnsAboutEngineRecords(t *testing.T) {
	t.Parallel()

	handler, _, _, observer := newEngineHandler(t)
	ctx := context.Background()
	zoneID := createZone(t, handler, "acme.dev")
	if _, err := handler.ApplyRecordSet(ctx, zoneID, gatewayOps(t, "acme.dev"), activity.ActorHostingEngine, ""); err != nil {
		t.Fatalf("ApplyRecordSet: %v", err)
	}
	mailOps, err := enginedns.Plan(zone.Zone{Name: "acme.dev"}, enginedns.SourceMail, []zone.Record{
		{Name: "@", Type: zone.TypeMX, TTL: 3600, Value: "10 mx1.simple.host."},
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if _, err := handler.ApplyRecordSet(ctx, zoneID, mailOps.WorkerOps(), activity.ActorMailEngine, ""); err != nil {
		t.Fatalf("ApplyRecordSet(mail): %v", err)
	}
	observer.reset()

	const zoneFile = "@ 300 IN A 203.0.113.9\n"
	for _, dryRun := range []bool{true, false} {
		response, err := handler.ImportZone(ctx, connect.NewRequest(&dnsv1.ImportZoneRequest{
			Name:     "acme.dev",
			ZoneFile: zoneFile,
			Mode:     dnsv1.ZoneImportMode_ZONE_IMPORT_MODE_REPLACE,
			DryRun:   dryRun,
		}))
		if err != nil {
			t.Fatalf("ImportZone(dry_run=%t): %v", dryRun, err)
		}
		warnings := strings.Join(response.Msg.GetWarnings(), "\n")
		if !strings.Contains(warnings, "2 records written by the Website engine will be removed by this import") {
			t.Errorf("dry_run=%t warnings = %q, want the Website warning", dryRun, warnings)
		}
		// The warning is operator-facing copy, so it has a singular form.
		if !strings.Contains(warnings, "1 record written by the Email engine will be removed by this import") {
			t.Errorf("dry_run=%t warnings = %q, want the Email warning", dryRun, warnings)
		}
	}

	handler.WaitForObservers()
	calls := observer.all()
	if len(calls) != 1 || calls[0].Method != "ZoneReplaced" || calls[0].ZoneID != zoneID {
		t.Fatalf("observer calls = %+v, want exactly one ZoneReplaced for the applied import", calls)
	}
}

func TestImportInCreateModeWarnsAboutNothing(t *testing.T) {
	t.Parallel()

	handler, _, _, observer := newEngineHandler(t)
	response, err := handler.ImportZone(context.Background(), connect.NewRequest(&dnsv1.ImportZoneRequest{
		Name:     "fresh.dev",
		ZoneFile: "@ 300 IN A 203.0.113.9\n",
		Mode:     dnsv1.ZoneImportMode_ZONE_IMPORT_MODE_CREATE,
	}))
	if err != nil {
		t.Fatalf("ImportZone: %v", err)
	}
	for _, warning := range response.Msg.GetWarnings() {
		if strings.Contains(warning, "engine") {
			t.Errorf("unexpected engine warning on a fresh zone: %q", warning)
		}
	}
	handler.WaitForObservers()
	if calls := observer.all(); len(calls) != 0 {
		t.Errorf("observer calls = %+v, want none for a create import", calls)
	}
}

func TestEachMutationFiresExactlyOneObserverCall(t *testing.T) {
	t.Parallel()

	handler, _, _, observer := newEngineHandler(t)
	ctx := context.Background()
	zoneID := createZone(t, handler, "acme.dev")

	created, err := handler.CreateRecord(ctx, connect.NewRequest(&dnsv1.CreateRecordRequest{
		ZoneId: zoneID, Name: "blog", Type: dnsv1.RecordType_RECORD_TYPE_A, Ttl: 300, Value: "203.0.113.5",
	}))
	if err != nil {
		t.Fatalf("CreateRecord: %v", err)
	}
	handler.WaitForObservers()
	if calls := observer.all(); len(calls) != 0 {
		t.Fatalf("creating a record fired %+v, want no observer call", calls)
	}

	recordID := ""
	for _, record := range created.Msg.GetZone().GetRecords() {
		if record.GetName() == "blog" {
			recordID = record.GetId()
		}
	}
	if recordID == "" {
		t.Fatal("the created record is missing from the response")
	}

	if _, err := handler.UpdateRecord(ctx, connect.NewRequest(&dnsv1.UpdateRecordRequest{
		ZoneId: zoneID, RecordId: recordID, Name: "blog",
		Type: dnsv1.RecordType_RECORD_TYPE_A, Ttl: 300, Value: "203.0.113.6",
	})); err != nil {
		t.Fatalf("UpdateRecord: %v", err)
	}
	handler.WaitForObservers()
	if calls := observer.all(); len(calls) != 0 {
		t.Fatalf("updating a record fired %+v, want no observer call", calls)
	}

	if _, err := handler.DeleteRecord(ctx, connect.NewRequest(&dnsv1.DeleteRecordRequest{
		ZoneId: zoneID, RecordId: recordID,
	})); err != nil {
		t.Fatalf("DeleteRecord: %v", err)
	}
	handler.WaitForObservers()
	calls := observer.all()
	if len(calls) != 1 || calls[0].Method != "RecordDeleted" || calls[0].RecordID != recordID {
		t.Fatalf("deleting a record fired %+v, want one RecordDeleted", calls)
	}
	observer.reset()

	if _, err := handler.DeleteZone(ctx, connect.NewRequest(&dnsv1.DeleteZoneRequest{ZoneId: zoneID})); err != nil {
		t.Fatalf("DeleteZone: %v", err)
	}
	handler.WaitForObservers()
	calls = observer.all()
	if len(calls) != 1 || calls[0].Method != "ZoneDeleted" || calls[0].ZoneID != zoneID {
		t.Fatalf("deleting a zone fired %+v, want one ZoneDeleted", calls)
	}
}

func TestARefusedMutationFiresNoObserverCall(t *testing.T) {
	t.Parallel()

	handler, _, _, observer := newEngineHandler(t)
	ctx := context.Background()
	if _, err := handler.DeleteRecord(ctx, connect.NewRequest(&dnsv1.DeleteRecordRequest{
		ZoneId: "missing", RecordId: "missing",
	})); err == nil {
		t.Fatal("DeleteRecord on a missing zone succeeded")
	}
	handler.WaitForObservers()
	if calls := observer.all(); len(calls) != 0 {
		t.Errorf("a refused mutation fired %+v", calls)
	}
}

func TestEveryMutationRecordsExactlyOneEvent(t *testing.T) {
	t.Parallel()

	handler, _, recorder, _ := newEngineHandler(t)
	ctx := context.Background()

	zoneID := createZone(t, handler, "acme.dev")
	if got := recorder.kinds(); len(got) != 1 || got[0] != string(activity.KindZoneCreated) {
		t.Fatalf("CreateZone recorded %v", got)
	}
	if event := recorder.all()[0]; event.ZoneName != "acme.dev" || event.Actor != activity.ActorOperator {
		t.Errorf("CreateZone event = %+v", event)
	}
	recorder.reset()

	created, err := handler.CreateRecord(ctx, connect.NewRequest(&dnsv1.CreateRecordRequest{
		ZoneId: zoneID, Name: "blog", Type: dnsv1.RecordType_RECORD_TYPE_A, Ttl: 300, Value: "203.0.113.5",
	}))
	if err != nil {
		t.Fatalf("CreateRecord: %v", err)
	}
	events := recorder.all()
	if len(events) != 1 || events[0].Kind != activity.KindDNSRecordCreated {
		t.Fatalf("CreateRecord recorded %v", recorder.kinds())
	}
	if events[0].Details["record.value"] != "203.0.113.5" || events[0].Details["record.source"] != "Custom" {
		t.Errorf("CreateRecord details = %v", events[0].Details)
	}
	recorder.reset()

	recordID := ""
	for _, record := range created.Msg.GetZone().GetRecords() {
		if record.GetName() == "blog" {
			recordID = record.GetId()
		}
	}
	if _, err := handler.UpdateRecord(ctx, connect.NewRequest(&dnsv1.UpdateRecordRequest{
		ZoneId: zoneID, RecordId: recordID, Name: "blog",
		Type: dnsv1.RecordType_RECORD_TYPE_A, Ttl: 300, Value: "203.0.113.6",
	})); err != nil {
		t.Fatalf("UpdateRecord: %v", err)
	}
	events = recorder.all()
	if len(events) != 1 || events[0].Kind != activity.KindDNSRecordUpdated {
		t.Fatalf("UpdateRecord recorded %v", recorder.kinds())
	}
	if events[0].Details["record.before_value"] != "203.0.113.5" {
		t.Errorf("UpdateRecord details = %v, want the previous value", events[0].Details)
	}
	if events[0].Details["record.value"] != "203.0.113.6" {
		t.Errorf("UpdateRecord details = %v, want the new value", events[0].Details)
	}
	recorder.reset()

	if _, err := handler.DeleteRecord(ctx, connect.NewRequest(&dnsv1.DeleteRecordRequest{
		ZoneId: zoneID, RecordId: recordID,
	})); err != nil {
		t.Fatalf("DeleteRecord: %v", err)
	}
	events = recorder.all()
	if len(events) != 1 || events[0].Kind != activity.KindDNSRecordDeleted {
		t.Fatalf("DeleteRecord recorded %v", recorder.kinds())
	}
	if events[0].Details["record.value"] != "203.0.113.6" {
		t.Errorf("DeleteRecord details = %v", events[0].Details)
	}
	recorder.reset()

	if _, err := handler.ImportZone(ctx, connect.NewRequest(&dnsv1.ImportZoneRequest{
		Name: "acme.dev", ZoneFile: "@ 300 IN A 203.0.113.9\n",
		Mode: dnsv1.ZoneImportMode_ZONE_IMPORT_MODE_REPLACE, DryRun: true,
	})); err != nil {
		t.Fatalf("ImportZone dry run: %v", err)
	}
	if got := recorder.kinds(); len(got) != 0 {
		t.Errorf("a dry-run import recorded %v, want nothing", got)
	}
	if _, err := handler.ImportZone(ctx, connect.NewRequest(&dnsv1.ImportZoneRequest{
		Name: "acme.dev", ZoneFile: "@ 300 IN A 203.0.113.9\n",
		Mode: dnsv1.ZoneImportMode_ZONE_IMPORT_MODE_REPLACE,
	})); err != nil {
		t.Fatalf("ImportZone: %v", err)
	}
	if got := recorder.kinds(); len(got) != 1 || got[0] != string(activity.KindZoneImported) {
		t.Fatalf("ImportZone recorded %v", got)
	}
	recorder.reset()

	if _, err := handler.DeleteZone(ctx, connect.NewRequest(&dnsv1.DeleteZoneRequest{ZoneId: zoneID})); err != nil {
		t.Fatalf("DeleteZone: %v", err)
	}
	if got := recorder.kinds(); len(got) != 1 || got[0] != string(activity.KindZoneDeleted) {
		t.Errorf("DeleteZone recorded %v", got)
	}
}

func TestAFailedMutationIsRecordedAsAnError(t *testing.T) {
	t.Parallel()

	handler, _, recorder, _ := newEngineHandler(t)
	createZone(t, handler, "acme.dev")
	recorder.reset()

	if _, err := handler.CreateZone(context.Background(), connect.NewRequest(&dnsv1.CreateZoneRequest{Name: "acme.dev"})); err == nil {
		t.Fatal("CreateZone on a duplicate name succeeded")
	}
	events := recorder.all()
	if len(events) != 1 {
		t.Fatalf("recorded %d events, want 1", len(events))
	}
	if events[0].Severity != activity.SeverityError {
		t.Errorf("severity = %q, want error", events[0].Severity)
	}
	if events[0].Details["code"] != connect.CodeAlreadyExists.String() {
		t.Errorf("details = %v, want the Connect code", events[0].Details)
	}
	if events[0].ZoneName != "acme.dev" {
		t.Errorf("zone name = %q, want the requested name", events[0].ZoneName)
	}
	if !strings.Contains(events[0].Summary, "failed") {
		t.Errorf("summary = %q", events[0].Summary)
	}
}

func TestApplyRecordSetPublishesAndRecords(t *testing.T) {
	t.Parallel()

	handler, store, recorder, _ := newEngineHandler(t)
	ctx := context.Background()
	zoneID := createZone(t, handler, "acme.dev")
	recorder.reset()

	applied, err := handler.ApplyRecordSet(ctx, zoneID, gatewayOps(t, "acme.dev"), activity.ActorHostingEngine, "site-1")
	if err != nil {
		t.Fatalf("ApplyRecordSet: %v", err)
	}
	if len(applied.RecordIDs) != 2 {
		t.Fatalf("RecordIDs = %v, want one per create", applied.RecordIDs)
	}
	assertCounts(t, handler.dns, 1, 4)

	events := recorder.all()
	if len(events) != 2 {
		t.Fatalf("recorded %v, want one event per operation", recorder.kinds())
	}
	for _, event := range events {
		if event.Kind != activity.KindDNSRecordCreated {
			t.Errorf("event kind = %q", event.Kind)
		}
		if event.Actor != activity.ActorHostingEngine {
			t.Errorf("actor = %q, want the engine that asked", event.Actor)
		}
		if event.CorrelationID != "site-1" {
			t.Errorf("correlation = %q", event.CorrelationID)
		}
		if event.Details["record.source"] != "Website" {
			t.Errorf("details = %v, want the Website source", event.Details)
		}
		if event.Details["record.id"] == "" {
			t.Errorf("details = %v, want the id the store assigned", event.Details)
		}
	}
	recorder.reset()

	// Removing the alias again produces a delete event carrying the value that
	// disappeared, which is the only place it is still visible.
	stored, err := store.Get(ctx, zoneID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	alias := zone.Record{}
	for _, record := range stored.Records {
		if record.Type == zone.TypeCNAME {
			alias = record
		}
	}
	if alias.ID == "" {
		t.Fatal("the alias is missing")
	}
	if _, err := handler.ApplyRecordSet(ctx, zoneID, []enginedns.Op{
		{Kind: zone.OpDelete, RecordID: alias.ID, Record: alias},
	}, activity.ActorHostingEngine, ""); err != nil {
		t.Fatalf("ApplyRecordSet(delete): %v", err)
	}
	events = recorder.all()
	if len(events) != 1 || events[0].Kind != activity.KindDNSRecordDeleted {
		t.Fatalf("recorded %v", recorder.kinds())
	}
	if events[0].Details["record.value"] != alias.Value {
		t.Errorf("details = %v, want the removed value", events[0].Details)
	}
	assertCounts(t, handler.dns, 1, 3)
}

func TestApplyRecordSetRefusesAnEmptyZoneID(t *testing.T) {
	t.Parallel()

	handler, _, _, _ := newEngineHandler(t)
	if _, err := handler.ApplyRecordSet(context.Background(), "", nil, "", ""); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("ApplyRecordSet(\"\") = %v, want InvalidArgument", err)
	}
	if _, err := handler.ApplyRecordSet(context.Background(), "missing", nil, "", ""); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("ApplyRecordSet on a missing zone = %v, want NotFound", err)
	}
}

func TestZoneMutatorSatisfiesTheEngineContract(t *testing.T) {
	t.Parallel()

	handler, _, _, _ := newEngineHandler(t)
	ctx := context.Background()
	zoneID := createZone(t, handler, "acme.dev")

	mutator := handler.ZoneMutator()
	value, err := mutator.GetZone(ctx, zoneID)
	if err != nil {
		t.Fatalf("GetZone: %v", err)
	}
	if value.Name != "acme.dev" {
		t.Errorf("GetZone = %q", value.Name)
	}
	values, err := mutator.ListZones(ctx)
	if err != nil {
		t.Fatalf("ListZones: %v", err)
	}
	if len(values) != 1 {
		t.Errorf("ListZones = %d zones, want 1", len(values))
	}
	if _, err := mutator.ApplyRecordSet(ctx, zoneID, gatewayOps(t, "acme.dev"), activity.ActorHostingEngine, ""); err != nil {
		t.Fatalf("ApplyRecordSet: %v", err)
	}
	if _, err := mutator.GetZone(ctx, "missing"); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("GetZone(missing) = %v, want NotFound", err)
	}
}

func TestHandlerWithoutARecorderStillMutates(t *testing.T) {
	t.Parallel()

	store, err := zone.Open(filepath.Join(t.TempDir(), "zones.db"), []string{"ns1.example.test"})
	if err != nil {
		t.Fatalf("zone.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	handler := NewHandler(store, nil, discardLogger(), time.Unix(1_800_000_000, 0), nil)
	if _, err := handler.CreateZone(context.Background(), connect.NewRequest(&dnsv1.CreateZoneRequest{Name: "acme.dev"})); err != nil {
		t.Fatalf("CreateZone without a recorder: %v", err)
	}
	handler.AddZoneObserver(nil)
	handler.WaitForObservers()
}

// TestImportInReplaceModeIsRefusedWhileAnEngineHoldsTheDomain: REPLACE rebuilds
// the zone from the file alone, so it drops every engine-owned record exactly
// as DeleteZone would — and DeleteZone is refused. What the imported file does
// not itself carry, the site or the mailboxes stop having, and the reconciler
// cannot always put it back: a record in the file at the same name blocks it.
// The dry run is refused too, so the preview says so before anything is
// uploaded.
func TestImportInReplaceModeIsRefusedWhileAnEngineHoldsTheDomain(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		bindings stubBindings
		wantCode connect.Code
	}{
		{name: "site bound", bindings: stubBindings{site: true}, wantCode: connect.CodeFailedPrecondition},
		{name: "mail bound", bindings: stubBindings{mail: true}, wantCode: connect.CodeFailedPrecondition},
		{name: "unbound", bindings: stubBindings{}, wantCode: 0},
		{name: "checker failed", bindings: stubBindings{err: errors.New("store is down")}, wantCode: connect.CodeInternal},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			handler, _, _, _ := newEngineHandler(t, WithBindingChecker(test.bindings))
			createZone(t, handler, "acme.dev")
			for _, dryRun := range []bool{true, false} {
				_, err := handler.ImportZone(context.Background(), connect.NewRequest(&dnsv1.ImportZoneRequest{
					Name:     "acme.dev",
					ZoneFile: "@ 300 IN A 203.0.113.9\n",
					Mode:     dnsv1.ZoneImportMode_ZONE_IMPORT_MODE_REPLACE,
					DryRun:   dryRun,
				}))
				if test.wantCode == 0 {
					if err != nil {
						t.Fatalf("ImportZone(dry_run=%t) = %v, want success", dryRun, err)
					}
					continue
				}
				if connect.CodeOf(err) != test.wantCode {
					t.Fatalf("ImportZone(dry_run=%t) = %v (%s), want %s", dryRun, err, connect.CodeOf(err), test.wantCode)
				}
				if test.wantCode == connect.CodeFailedPrecondition &&
					!strings.Contains(err.Error(), "before replacing every record in this domain") {
					t.Errorf("ImportZone error = %q, want it to name what is being refused", err)
				}
			}
		})
	}
}

// TestCreateRecordIsRefusedInsideAnEngineRRSet: adding a member to an RRset an
// engine owns changes what that engine's record answers, so it is an edit of
// the engine's record through a door update and delete already guard. The copy
// has to name the engine and the way out without claiming the operator's new
// record belongs to one.
func TestCreateRecordIsRefusedInsideAnEngineRRSet(t *testing.T) {
	t.Parallel()

	handler, _, _, _ := newEngineHandler(t)
	ctx := context.Background()
	zoneID := createZone(t, handler, "acme.dev")
	if _, err := handler.ApplyRecordSet(ctx, zoneID, gatewayOps(t, "acme.dev"), activity.ActorHostingEngine, ""); err != nil {
		t.Fatalf("ApplyRecordSet: %v", err)
	}

	_, err := handler.CreateRecord(ctx, connect.NewRequest(&dnsv1.CreateRecordRequest{
		ZoneId: zoneID,
		Name:   "@",
		Type:   dnsv1.RecordType_RECORD_TYPE_A,
		Ttl:    300,
		Value:  "198.51.100.7",
	}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("CreateRecord(apex A sibling) = %v (%s), want FailedPrecondition", err, connect.CodeOf(err))
	}
	for _, want := range []string{"Website", "answers together", "Detach"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err, want)
		}
	}

	// A name the engine does not answer at is still the operator's.
	if _, err := handler.CreateRecord(ctx, connect.NewRequest(&dnsv1.CreateRecordRequest{
		ZoneId: zoneID,
		Name:   "docs",
		Type:   dnsv1.RecordType_RECORD_TYPE_A,
		Ttl:    300,
		Value:  "198.51.100.7",
	})); err != nil {
		t.Errorf("CreateRecord(unrelated name) = %v, want it accepted", err)
	}
}

// TestEditingCannotJoinAnEngineOwnedRRSet closes the door the Add dialog already
// held shut.
//
// CreateRecord refused a record that would join an RRset an engine answers.
// UpdateRecord checked only the provenance of the record being edited, never the
// provenance of the RRset its new name and type landed in — so the same change
// the console refused in Add succeeded silently from Edit. The apex A RRset then
// held one address the hosting engine serves and one it does not, and roughly
// half of all resolver answers sent visitors to a dead IP. The reconciler will
// not clear it either: a user record in the engine's way is a Conflict that only
// an explicit replace_conflicting_records call removes.
func TestEditingCannotJoinAnEngineOwnedRRSet(t *testing.T) {
	t.Parallel()

	handler, _, _, _ := newEngineHandler(t)
	ctx := context.Background()
	zoneID := createZone(t, handler, "acme.dev")

	// The hosting engine publishes the apex A it answers for.
	if _, err := handler.ApplyRecordSet(ctx, zoneID, gatewayOps(t, "acme.dev"), activity.ActorHostingEngine, "site-1"); err != nil {
		t.Fatalf("ApplyRecordSet: %v", err)
	}

	// Adding a second apex A is refused, and always was.
	_, err := handler.CreateRecord(ctx, connect.NewRequest(&dnsv1.CreateRecordRequest{
		ZoneId: zoneID, Name: "@", Type: dnsv1.RecordType_RECORD_TYPE_A, Ttl: 300, Value: "203.0.113.9",
	}))
	if err == nil {
		t.Fatal("CreateRecord joined the engine's apex A RRset")
	}

	// A record of the operator's own, somewhere harmless.
	created, err := handler.CreateRecord(ctx, connect.NewRequest(&dnsv1.CreateRecordRequest{
		ZoneId: zoneID, Name: "staging", Type: dnsv1.RecordType_RECORD_TYPE_A, Ttl: 300, Value: "203.0.113.9",
	}))
	if err != nil {
		t.Fatalf("CreateRecord for an unrelated name: %v", err)
	}
	var recordID string
	for _, record := range created.Msg.GetZone().GetRecords() {
		if record.GetName() == "staging" {
			recordID = record.GetId()
		}
	}
	if recordID == "" {
		t.Fatal("could not find the record just created")
	}

	// Editing it onto the apex is the same change by another route, and must be
	// refused the same way.
	_, err = handler.UpdateRecord(ctx, connect.NewRequest(&dnsv1.UpdateRecordRequest{
		ZoneId: zoneID, RecordId: recordID, Name: "@", Type: dnsv1.RecordType_RECORD_TYPE_A, Ttl: 300, Value: "203.0.113.9",
	}))
	if err == nil {
		t.Fatal("UpdateRecord moved a record into the engine's apex A RRset; half of all answers would send visitors to an address the engine does not serve")
	}
	if !strings.Contains(err.Error(), "Website") && !strings.Contains(err.Error(), "hosting") {
		t.Errorf("error = %v, want it to name the engine that answers there", err)
	}

	// Editing the record within its own name is still allowed: the guard is
	// about where a record is going, not that it is being touched at all.
	if _, err := handler.UpdateRecord(ctx, connect.NewRequest(&dnsv1.UpdateRecordRequest{
		ZoneId: zoneID, RecordId: recordID, Name: "staging", Type: dnsv1.RecordType_RECORD_TYPE_A, Ttl: 600, Value: "203.0.113.40",
	})); err != nil {
		t.Fatalf("editing an operator's own record must still work: %v", err)
	}
}
