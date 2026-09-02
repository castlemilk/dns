package control

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	dnsv1 "github.com/castlemilk/dns/gen/go/dns/v1"
	"github.com/castlemilk/dns/internal/authoritative"
	"github.com/castlemilk/dns/internal/zone"
	"github.com/miekg/dns"
)

func TestHandlerCRUDPublishesAuthoritativeState(t *testing.T) {
	t.Parallel()

	handler, dnsServer, _ := newTestHandler(t)
	ctx := context.Background()
	created, err := handler.CreateZone(ctx, connect.NewRequest(&dnsv1.CreateZoneRequest{Name: " Example.Test. "}))
	if err != nil {
		t.Fatalf("CreateZone: %v", err)
	}
	zoneID := created.Msg.GetZone().GetId()
	assertCounts(t, dnsServer, 1, 2)
	if response := query(t, dnsServer, "example.test.", dns.TypeSOA); response.Rcode != dns.RcodeSuccess || !response.Authoritative {
		t.Errorf("published SOA response = rcode:%s AA:%t", dns.RcodeToString[response.Rcode], response.Authoritative)
	}

	createdRecord, err := handler.CreateRecord(ctx, connect.NewRequest(&dnsv1.CreateRecordRequest{
		ZoneId: zoneID,
		Name:   "www",
		Type:   dnsv1.RecordType_RECORD_TYPE_A,
		Ttl:    60,
		Value:  "192.0.2.10",
	}))
	if err != nil {
		t.Fatalf("CreateRecord: %v", err)
	}
	recordID := userRecordID(t, createdRecord.Msg.GetZone())
	assertCounts(t, dnsServer, 1, 3)
	assertPublishedA(t, dnsServer, "www.example.test.", "192.0.2.10")

	_, err = handler.UpdateRecord(ctx, connect.NewRequest(&dnsv1.UpdateRecordRequest{
		ZoneId:   zoneID,
		RecordId: recordID,
		Name:     "www",
		Type:     dnsv1.RecordType_RECORD_TYPE_A,
		Ttl:      120,
		Value:    "192.0.2.11",
	}))
	if err != nil {
		t.Fatalf("UpdateRecord: %v", err)
	}
	assertPublishedA(t, dnsServer, "www.example.test.", "192.0.2.11")

	listed, err := handler.ListZones(ctx, connect.NewRequest(&dnsv1.ListZonesRequest{}))
	if err != nil {
		t.Fatalf("ListZones: %v", err)
	}
	if len(listed.Msg.GetZones()) != 1 || listed.Msg.GetStatus().GetZoneCount() != 1 || listed.Msg.GetStatus().GetRecordCount() != 3 {
		t.Errorf("ListZones summary = zones:%d status:%#v", len(listed.Msg.GetZones()), listed.Msg.GetStatus())
	}
	if listed.Msg.GetStatus().GetQueriesTotal() != dnsServer.QueryCount() {
		t.Errorf("queries_total = %d, want %d", listed.Msg.GetStatus().GetQueriesTotal(), dnsServer.QueryCount())
	}

	if _, err := handler.DeleteRecord(ctx, connect.NewRequest(&dnsv1.DeleteRecordRequest{ZoneId: zoneID, RecordId: recordID})); err != nil {
		t.Fatalf("DeleteRecord: %v", err)
	}
	assertCounts(t, dnsServer, 1, 2)
	if response := query(t, dnsServer, "www.example.test.", dns.TypeA); response.Rcode != dns.RcodeNameError {
		t.Errorf("deleted record Rcode = %s, want NXDOMAIN", dns.RcodeToString[response.Rcode])
	}

	if _, err := handler.DeleteZone(ctx, connect.NewRequest(&dnsv1.DeleteZoneRequest{ZoneId: zoneID})); err != nil {
		t.Fatalf("DeleteZone: %v", err)
	}
	assertCounts(t, dnsServer, 0, 0)
	if response := query(t, dnsServer, "example.test.", dns.TypeSOA); response.Rcode != dns.RcodeRefused {
		t.Errorf("deleted zone Rcode = %s, want REFUSED", dns.RcodeToString[response.Rcode])
	}
}

func TestHandlerValidation(t *testing.T) {
	t.Parallel()

	handler, _, _ := newTestHandler(t)
	ctx := context.Background()
	if _, err := handler.CreateZone(ctx, connect.NewRequest(&dnsv1.CreateZoneRequest{Name: "*.example.test"})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("invalid zone code = %s, want invalid_argument", connect.CodeOf(err))
	}
	created, err := handler.CreateZone(ctx, connect.NewRequest(&dnsv1.CreateZoneRequest{Name: "example.test"}))
	if err != nil {
		t.Fatalf("CreateZone: %v", err)
	}
	if _, err := handler.CreateZone(ctx, connect.NewRequest(&dnsv1.CreateZoneRequest{Name: "EXAMPLE.TEST"})); connect.CodeOf(err) != connect.CodeAlreadyExists {
		t.Errorf("duplicate zone code = %s, want already_exists", connect.CodeOf(err))
	}
	if _, err := handler.CreateRecord(ctx, connect.NewRequest(&dnsv1.CreateRecordRequest{})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("missing zone ID code = %s, want invalid_argument", connect.CodeOf(err))
	}
	if _, err := handler.CreateRecord(ctx, connect.NewRequest(&dnsv1.CreateRecordRequest{
		ZoneId: created.Msg.GetZone().GetId(), Name: "www", Type: dnsv1.RecordType_RECORD_TYPE_SOA, Value: "invalid",
	})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("unsupported record type code = %s, want invalid_argument", connect.CodeOf(err))
	}
}

func TestPublicErrorCodes(t *testing.T) {
	t.Parallel()

	handler := NewHandler(nil, nil, discardLogger(), time.Time{})
	tests := []struct {
		name string
		err  error
		want connect.Code
	}{
		{name: "validation", err: &zone.ValidationError{Field: "name", Message: "invalid"}, want: connect.CodeInvalidArgument},
		{name: "not found", err: fmt.Errorf("wrapped: %w", zone.ErrNotFound), want: connect.CodeNotFound},
		{name: "duplicate", err: fmt.Errorf("wrapped: %w", zone.ErrAlreadyExists), want: connect.CodeAlreadyExists},
		{name: "managed", err: fmt.Errorf("wrapped: %w", zone.ErrManagedRecord), want: connect.CodeFailedPrecondition},
		{name: "canceled", err: context.Canceled, want: connect.CodeCanceled},
		{name: "deadline", err: context.DeadlineExceeded, want: connect.CodeDeadlineExceeded},
		{name: "unexpected", err: errors.New("boom"), want: connect.CodeInternal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := connect.CodeOf(handler.publicError(tt.err)); got != tt.want {
				t.Errorf("publicError code = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestReloadAfterMutationSurvivesCanceledRequest(t *testing.T) {
	t.Parallel()

	handler, dnsServer, store := newTestHandler(t)
	if _, err := store.Create(context.Background(), "committed.test"); err != nil {
		t.Fatalf("commit zone directly: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := handler.reloadAfterMutation(ctx); err != nil {
		t.Fatalf("reloadAfterMutation with canceled request: %v", err)
	}
	assertCounts(t, dnsServer, 1, 2)
	if response := query(t, dnsServer, "committed.test.", dns.TypeSOA); response.Rcode != dns.RcodeSuccess {
		t.Errorf("committed zone Rcode = %s, want NOERROR", dns.RcodeToString[response.Rcode])
	}
}

func TestControlOnlyHandlerValidatesWithoutDNSPublisher(t *testing.T) {
	t.Parallel()
	store, err := zone.Open(filepath.Join(t.TempDir(), "zones.db"), []string{"ns1.dns.test"})
	if err != nil {
		t.Fatalf("zone.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	handler := NewHandler(store, nil, discardLogger(), time.Unix(1_800_000_000, 0))
	ctx := context.Background()
	created, err := handler.CreateZone(ctx, connect.NewRequest(&dnsv1.CreateZoneRequest{Name: "control.test"}))
	if err != nil {
		t.Fatalf("CreateZone: %v", err)
	}
	if created.Msg.GetZone().GetName() != "control.test" {
		t.Fatalf("created zone = %#v", created.Msg.GetZone())
	}
	listed, err := handler.ListZones(ctx, connect.NewRequest(&dnsv1.ListZonesRequest{}))
	if err != nil {
		t.Fatalf("ListZones: %v", err)
	}
	if listed.Msg.GetStatus().GetZoneCount() != 1 || listed.Msg.GetStatus().GetQueriesTotal() != 0 {
		t.Fatalf("status = %#v", listed.Msg.GetStatus())
	}
	if err := handler.Reload(ctx); err != nil {
		t.Fatalf("Reload: %v", err)
	}
}

func TestHandlerImportExportIsAtomicAndPublishesOnlyCommittedState(t *testing.T) {
	t.Parallel()
	handler, dnsServer, store := newTestHandler(t)
	ctx := context.Background()
	source := `$ORIGIN imported.test.
@ 300 IN SOA old.provider. hostmaster.old.provider. 1 3600 600 1209600 300
@ 300 IN NS old.provider.
www 60 IN A 192.0.2.10
text 60 IN TXT "hello"
`
	dryRun, err := handler.ImportZone(ctx, connect.NewRequest(&dnsv1.ImportZoneRequest{
		Name: "imported.test", ZoneFile: source,
		Mode: dnsv1.ZoneImportMode_ZONE_IMPORT_MODE_CREATE, DryRun: true,
	}))
	if err != nil {
		t.Fatalf("dry-run ImportZone: %v", err)
	}
	if !dryRun.Msg.GetDryRun() || len(dryRun.Msg.GetWarnings()) != 2 {
		t.Fatalf("dry-run response = %#v", dryRun.Msg)
	}
	assertCounts(t, dnsServer, 0, 0)
	if listed, err := store.List(ctx); err != nil || len(listed) != 0 {
		t.Fatalf("store after dry-run = %#v/%v", listed, err)
	}

	created, err := handler.ImportZone(ctx, connect.NewRequest(&dnsv1.ImportZoneRequest{
		Name: "imported.test", ZoneFile: source,
		Mode: dnsv1.ZoneImportMode_ZONE_IMPORT_MODE_CREATE,
	}))
	if err != nil {
		t.Fatalf("ImportZone: %v", err)
	}
	zoneID := created.Msg.GetZone().GetId()
	assertCounts(t, dnsServer, 1, 4)
	assertPublishedA(t, dnsServer, "www.imported.test.", "192.0.2.10")

	exported, err := handler.ExportZone(ctx, connect.NewRequest(&dnsv1.ExportZoneRequest{ZoneId: zoneID}))
	if err != nil {
		t.Fatalf("ExportZone: %v", err)
	}
	if exported.Msg.GetName() != "imported.test" || !strings.Contains(exported.Msg.GetZoneFile(), "$ORIGIN imported.test.") ||
		!strings.Contains(exported.Msg.GetZoneFile(), "ns1.example.test.") {
		t.Fatalf("export = %#v", exported.Msg)
	}

	replacement := "api 120 IN A 192.0.2.20\n"
	if _, err := handler.ImportZone(ctx, connect.NewRequest(&dnsv1.ImportZoneRequest{
		Name: "imported.test", ZoneFile: replacement,
		Mode: dnsv1.ZoneImportMode_ZONE_IMPORT_MODE_REPLACE,
	})); err != nil {
		t.Fatalf("replace ImportZone: %v", err)
	}
	assertPublishedA(t, dnsServer, "api.imported.test.", "192.0.2.20")
	if response := query(t, dnsServer, "www.imported.test.", dns.TypeA); response.Rcode != dns.RcodeNameError {
		t.Fatalf("old imported name rcode = %s", dns.RcodeToString[response.Rcode])
	}

	before, err := store.Get(ctx, zoneID)
	if err != nil {
		t.Fatalf("Get before invalid import: %v", err)
	}
	_, err = handler.ImportZone(ctx, connect.NewRequest(&dnsv1.ImportZoneRequest{
		Name: "imported.test", ZoneFile: "@ 300 IN DS 1 13 2 aabb\n",
		Mode: dnsv1.ZoneImportMode_ZONE_IMPORT_MODE_REPLACE,
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("invalid import error = %v (%s)", err, connect.CodeOf(err))
	}
	after, err := store.Get(ctx, zoneID)
	if err != nil {
		t.Fatalf("Get after invalid import: %v", err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatal("invalid import mutated the zone")
	}
}

func newTestHandler(t *testing.T) (*Handler, *authoritative.Server, *zone.Store) {
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
	dnsServer := authoritative.New(discardLogger(), authoritative.DefaultMaxUDPSize)
	handler := NewHandler(store, dnsServer, discardLogger(), time.Unix(1_800_000_000, 0))
	return handler, dnsServer, store
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func assertCounts(t *testing.T, server *authoritative.Server, wantZones, wantRecords uint32) {
	t.Helper()
	gotZones, gotRecords := server.Counts()
	if gotZones != wantZones || gotRecords != wantRecords {
		t.Errorf("Counts = (%d, %d), want (%d, %d)", gotZones, gotRecords, wantZones, wantRecords)
	}
}

func userRecordID(t *testing.T, value *dnsv1.Zone) string {
	t.Helper()
	for _, record := range value.GetRecords() {
		if !record.GetManaged() {
			return record.GetId()
		}
	}
	t.Fatal("response contains no user-managed record")
	return ""
}

func assertPublishedA(t *testing.T, server *authoritative.Server, name, want string) {
	t.Helper()
	response := query(t, server, name, dns.TypeA)
	if response.Rcode != dns.RcodeSuccess || len(response.Answer) != 1 {
		t.Fatalf("A response = rcode:%s answers:%d", dns.RcodeToString[response.Rcode], len(response.Answer))
	}
	record, ok := response.Answer[0].(*dns.A)
	if !ok {
		t.Fatalf("answer type = %T, want *dns.A", response.Answer[0])
	}
	if got := record.A.String(); got != want {
		t.Errorf("published A = %s, want %s", got, want)
	}
}

func query(t *testing.T, server *authoritative.Server, name string, recordType uint16) *dns.Msg {
	t.Helper()
	request := new(dns.Msg)
	request.SetQuestion(name, recordType)
	writer := new(captureWriter)
	server.ServeDNS(writer, request)
	if writer.message == nil {
		t.Fatal("ServeDNS did not write a response")
	}
	return writer.message
}

type captureWriter struct{ message *dns.Msg }

func (*captureWriter) LocalAddr() net.Addr  { return &net.UDPAddr{} }
func (*captureWriter) RemoteAddr() net.Addr { return &net.UDPAddr{} }
func (w *captureWriter) WriteMsg(message *dns.Msg) error {
	w.message = message.Copy()
	return nil
}
func (w *captureWriter) Write(raw []byte) (int, error) {
	message := new(dns.Msg)
	if err := message.Unpack(raw); err != nil {
		return 0, err
	}
	w.message = message
	return len(raw), nil
}
func (*captureWriter) Close() error        { return nil }
func (*captureWriter) TsigStatus() error   { return nil }
func (*captureWriter) TsigTimersOnly(bool) {}
func (*captureWriter) Hijack()             {}
