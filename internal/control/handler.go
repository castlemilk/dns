package control

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	dnsv1 "github.com/castlemilk/dns/gen/go/dns/v1"
	"github.com/castlemilk/dns/gen/go/dns/v1/dnsv1connect"
	"github.com/castlemilk/dns/internal/activity"
	"github.com/castlemilk/dns/internal/authoritative"
	"github.com/castlemilk/dns/internal/enginedns"
	"github.com/castlemilk/dns/internal/telemetry"
	"github.com/castlemilk/dns/internal/zone"
	"github.com/castlemilk/dns/internal/zonefile"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// defaultObserverTimeout bounds one round of zone-observer notifications. The
// observers only enqueue work for their reconcilers, so this is generous.
const defaultObserverTimeout = 30 * time.Second

// maxDetailValueRunes truncates a record value inside an activity detail. DNS
// values are public, but a 4 KiB TXT record does not belong in an audit line.
const maxDetailValueRunes = 200

type Handler struct {
	store     *zone.Store
	dns       *authoritative.Server
	logger    *slog.Logger
	startedAt time.Time
	stateMu   sync.RWMutex
	metrics   *telemetry.Metrics
	recorder  activity.Recorder
	bindings  BindingChecker

	observerMu      sync.RWMutex
	observers       []enginedns.ZoneObserver
	observerWG      sync.WaitGroup
	observerTimeout time.Duration
}

func NewHandler(
	store *zone.Store,
	dnsServer *authoritative.Server,
	logger *slog.Logger,
	startedAt time.Time,
	metrics *telemetry.Metrics,
	options ...Option,
) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	handler := &Handler{
		store:           store,
		dns:             dnsServer,
		logger:          logger,
		startedAt:       startedAt.UTC(),
		metrics:         telemetry.Select(metrics),
		recorder:        activity.Nop(),
		observerTimeout: defaultObserverTimeout,
	}
	for _, option := range options {
		option(handler)
	}
	return handler
}

// AddZoneObserver registers an engine's observer. It is callable after
// construction because the engines are built from the handler, not before it.
func (h *Handler) AddZoneObserver(observer enginedns.ZoneObserver) {
	if observer == nil {
		return
	}
	h.observerMu.Lock()
	defer h.observerMu.Unlock()
	h.observers = append(h.observers, observer)
}

// WaitForObservers blocks until every notification started so far has finished.
// Shutdown and tests use it; the request path never does.
func (h *Handler) WaitForObservers() {
	h.observerWG.Wait()
}

func (h *Handler) Reload(ctx context.Context) error {
	h.stateMu.Lock()
	defer h.stateMu.Unlock()
	return h.reload(ctx)
}

func (h *Handler) reload(ctx context.Context) error {
	values, err := h.store.List(ctx)
	if err != nil {
		return fmt.Errorf("load zones: %w", err)
	}
	if h.dns == nil {
		if _, err := authoritative.Compile(values); err != nil {
			return fmt.Errorf("validate zones: %w", err)
		}
		var records uint32
		for _, value := range values {
			records += uint32(len(value.Records))
		}
		h.metrics.SetInventory(uint32(len(values)), records)
		return nil
	}
	if err := h.dns.Replace(values); err != nil {
		return fmt.Errorf("publish zones: %w", err)
	}
	return nil
}

func (h *Handler) ListZones(ctx context.Context, _ *connect.Request[dnsv1.ListZonesRequest]) (*connect.Response[dnsv1.ListZonesResponse], error) {
	h.stateMu.RLock()
	defer h.stateMu.RUnlock()

	values, err := h.store.List(ctx)
	if err != nil {
		return nil, h.internalError("list zones", err)
	}
	var queries uint64
	if h.dns != nil {
		queries = h.dns.QueryCount()
	}
	response := &dnsv1.ListZonesResponse{
		Zones: make([]*dnsv1.Zone, 0, len(values)),
		Status: &dnsv1.ServerStatus{
			QueriesTotal: queries,
			StartedAt:    timestamppb.New(h.startedAt),
		},
	}
	for _, value := range values {
		response.Zones = append(response.Zones, zoneToProto(value))
		response.Status.RecordCount += uint32(len(value.Records))
	}
	response.Status.ZoneCount = uint32(len(values))
	return connect.NewResponse(response), nil
}

func (h *Handler) CreateZone(ctx context.Context, request *connect.Request[dnsv1.CreateZoneRequest]) (response *connect.Response[dnsv1.CreateZoneResponse], resultErr error) {
	started := h.mutationStarted()
	requested := request.Msg.GetName()
	var value zone.Zone
	defer func() {
		h.recordMutation(ctx, "zone", "create", resultErr, started)
		h.event(ctx, resultErr, activity.KindZoneCreated, value, requested,
			fmt.Sprintf("Added the domain %s", displayName(value, requested)),
			fmt.Sprintf("Adding the domain %s failed", displayName(value, requested)),
			nil)
	}()
	h.stateMu.Lock()
	defer h.stateMu.Unlock()

	created, err := h.store.Create(ctx, requested)
	if err != nil {
		return nil, h.publicError(err)
	}
	value = created
	if err := h.reloadAfterMutation(ctx); err != nil {
		return nil, h.internalError("reload after creating zone", err)
	}
	return connect.NewResponse(&dnsv1.CreateZoneResponse{Zone: zoneToProto(value)}), nil
}

func (h *Handler) DeleteZone(ctx context.Context, request *connect.Request[dnsv1.DeleteZoneRequest]) (response *connect.Response[dnsv1.DeleteZoneResponse], resultErr error) {
	started := h.mutationStarted()
	zoneID := request.Msg.GetZoneId()
	var value zone.Zone
	defer func() {
		h.recordMutation(ctx, "zone", "delete", resultErr, started)
		h.event(ctx, resultErr, activity.KindZoneDeleted, value, zoneID,
			fmt.Sprintf("Removed the domain %s", displayName(value, zoneID)),
			fmt.Sprintf("Removing the domain %s failed", displayName(value, zoneID)),
			nil)
	}()
	if zoneID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("zone_id is required"))
	}
	h.stateMu.Lock()
	defer h.stateMu.Unlock()

	stored, err := h.store.Get(ctx, zoneID)
	if err != nil {
		return nil, h.publicError(err)
	}
	value = stored
	if err := h.refuseBoundZone(ctx, zoneID, "deleting this domain"); err != nil {
		return nil, err
	}

	if err := h.store.Delete(ctx, zoneID); err != nil {
		return nil, h.publicError(err)
	}
	if err := h.reloadAfterMutation(ctx); err != nil {
		return nil, h.internalError("reload after deleting zone", err)
	}
	h.notify(ctx, func(notifyCtx context.Context, observer enginedns.ZoneObserver) {
		observer.ZoneDeleted(notifyCtx, zoneID)
	})
	return connect.NewResponse(&dnsv1.DeleteZoneResponse{}), nil
}

// refuseBoundZone keeps a domain that a site or a mailbox still depends on from
// being taken out from under its engine: the engine would keep serving it while
// its DNS vanished. action names what is being refused, so the same guard reads
// correctly for a delete and for a replacing import.
func (h *Handler) refuseBoundZone(ctx context.Context, zoneID, action string) error {
	if h.bindings == nil {
		return nil
	}
	site, mail, err := h.bindings.HasBindings(ctx, zoneID)
	if err != nil {
		return h.internalError("check engine bindings", err)
	}
	if site || mail {
		return connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("detach the website and unbind email before %s", action))
	}
	return nil
}

func (h *Handler) CreateRecord(ctx context.Context, request *connect.Request[dnsv1.CreateRecordRequest]) (response *connect.Response[dnsv1.CreateRecordResponse], resultErr error) {
	started := h.mutationStarted()
	var value zone.Zone
	var record zone.Record
	defer func() {
		h.recordMutation(ctx, "record", "create", resultErr, started)
		h.event(ctx, resultErr, activity.KindDNSRecordCreated, value, request.Msg.GetZoneId(),
			fmt.Sprintf("Added %s to %s", describeRecord(record), displayName(value, request.Msg.GetZoneId())),
			fmt.Sprintf("Adding %s to %s failed", describeRecord(record), displayName(value, request.Msg.GetZoneId())),
			recordDetails(record))
	}()
	h.stateMu.Lock()
	defer h.stateMu.Unlock()

	stored, candidate, err := h.recordInput(ctx, request.Msg.GetZoneId(), request.Msg.GetName(), request.Msg.GetType(), request.Msg.GetTtl(), request.Msg.GetValue())
	if err != nil {
		return nil, h.publicError(err)
	}
	value, record = stored, candidate
	// An RRset answers as a whole, so a new member of a set an engine owns
	// changes what that engine's record answers. Update and delete already
	// refuse an engine-owned record; refuse the sibling here too, and name the
	// engine and the way out exactly as they do.
	if source, taken := zone.EngineOwnedRRSet(stored.Records, candidate, ""); taken {
		return nil, engineOwnedSetError(source, candidate)
	}
	updated, err := h.store.CreateRecord(ctx, stored.ID, candidate)
	if err != nil {
		return nil, h.publicError(err)
	}
	value = updated
	if err := h.reloadAfterMutation(ctx); err != nil {
		return nil, h.internalError("reload after creating record", err)
	}
	return connect.NewResponse(&dnsv1.CreateRecordResponse{Zone: zoneToProto(updated)}), nil
}

func (h *Handler) UpdateRecord(ctx context.Context, request *connect.Request[dnsv1.UpdateRecordRequest]) (response *connect.Response[dnsv1.UpdateRecordResponse], resultErr error) {
	started := h.mutationStarted()
	recordID := request.Msg.GetRecordId()
	var value zone.Zone
	var record zone.Record
	var previous zone.Record
	defer func() {
		details := recordDetails(record)
		if previous.ID != "" {
			details["record.before_value"] = truncateValue(previous.Value)
		}
		h.recordMutation(ctx, "record", "update", resultErr, started)
		h.event(ctx, resultErr, activity.KindDNSRecordUpdated, value, request.Msg.GetZoneId(),
			fmt.Sprintf("Changed %s on %s", describeRecord(record), displayName(value, request.Msg.GetZoneId())),
			fmt.Sprintf("Changing %s on %s failed", describeRecord(record), displayName(value, request.Msg.GetZoneId())),
			details)
	}()
	if recordID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("record_id is required"))
	}
	h.stateMu.Lock()
	defer h.stateMu.Unlock()

	stored, candidate, err := h.recordInput(ctx, request.Msg.GetZoneId(), request.Msg.GetName(), request.Msg.GetType(), request.Msg.GetTtl(), request.Msg.GetValue())
	if err != nil {
		return nil, h.publicError(err)
	}
	value, record = stored, candidate
	current, found := findRecord(stored, recordID)
	if !found {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("zone or record not found"))
	}
	previous = current
	if current.Source != zone.SourceUser {
		return nil, engineOwnedError(current.Source)
	}
	// The record being edited is allowed to be user-owned and still land in an
	// RRset an engine owns — an edit can change a record's name or type, which
	// is how the same change the Add dialog refuses used to succeed from the
	// Edit dialog. Skipping this record's own ID is what makes the check about
	// where it is going rather than where it is.
	if source, taken := zone.EngineOwnedRRSet(stored.Records, candidate, recordID); taken {
		return nil, engineOwnedSetError(source, candidate)
	}

	updated, err := h.store.UpdateRecord(ctx, stored.ID, recordID, candidate)
	if err != nil {
		return nil, h.publicError(err)
	}
	value = updated
	if err := h.reloadAfterMutation(ctx); err != nil {
		return nil, h.internalError("reload after updating record", err)
	}
	return connect.NewResponse(&dnsv1.UpdateRecordResponse{Zone: zoneToProto(updated)}), nil
}

func (h *Handler) DeleteRecord(ctx context.Context, request *connect.Request[dnsv1.DeleteRecordRequest]) (response *connect.Response[dnsv1.DeleteRecordResponse], resultErr error) {
	started := h.mutationStarted()
	zoneID := request.Msg.GetZoneId()
	recordID := request.Msg.GetRecordId()
	var value zone.Zone
	var record zone.Record
	defer func() {
		h.recordMutation(ctx, "record", "delete", resultErr, started)
		h.event(ctx, resultErr, activity.KindDNSRecordDeleted, value, zoneID,
			fmt.Sprintf("Removed %s from %s", describeRecord(record), displayName(value, zoneID)),
			fmt.Sprintf("Removing %s from %s failed", describeRecord(record), displayName(value, zoneID)),
			recordDetails(record))
	}()
	if zoneID == "" || recordID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("zone_id and record_id are required"))
	}
	h.stateMu.Lock()
	defer h.stateMu.Unlock()

	stored, err := h.store.Get(ctx, zoneID)
	if err != nil {
		return nil, h.publicError(err)
	}
	value = stored
	current, found := findRecord(stored, recordID)
	if !found {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("zone or record not found"))
	}
	record = current
	if current.Source != zone.SourceUser {
		return nil, engineOwnedError(current.Source)
	}

	updated, err := h.store.DeleteRecord(ctx, zoneID, recordID)
	if err != nil {
		return nil, h.publicError(err)
	}
	value = updated
	if err := h.reloadAfterMutation(ctx); err != nil {
		return nil, h.internalError("reload after deleting record", err)
	}
	h.notify(ctx, func(notifyCtx context.Context, observer enginedns.ZoneObserver) {
		observer.RecordDeleted(notifyCtx, zoneID, current)
	})
	return connect.NewResponse(&dnsv1.DeleteRecordResponse{Zone: zoneToProto(updated)}), nil
}

func (h *Handler) ImportZone(ctx context.Context, request *connect.Request[dnsv1.ImportZoneRequest]) (response *connect.Response[dnsv1.ImportZoneResponse], resultErr error) {
	started := h.mutationStarted()
	dryRun := request.Msg.GetDryRun()
	var value zone.Zone
	defer func() {
		h.recordMutation(ctx, "zone", "import", resultErr, started)
		if dryRun {
			return
		}
		h.event(ctx, resultErr, activity.KindZoneImported, value, request.Msg.GetName(),
			fmt.Sprintf("Imported a zone file into %s", displayName(value, request.Msg.GetName())),
			fmt.Sprintf("Importing a zone file into %s failed", displayName(value, request.Msg.GetName())),
			map[string]string{"import.mode": strings.ToLower(strings.TrimPrefix(request.Msg.GetMode().String(), "ZONE_IMPORT_MODE_"))})
	}()
	if len(request.Msg.GetZoneFile()) > zonefile.MaxBytes {
		return nil, h.publicError(&zone.ValidationError{Field: "zone_file", Message: fmt.Sprintf("exceeds the %d-byte limit", zonefile.MaxBytes)})
	}
	mode := zone.ImportMode(0)
	switch request.Msg.GetMode() {
	case dnsv1.ZoneImportMode_ZONE_IMPORT_MODE_CREATE:
		mode = zone.ImportCreate
	case dnsv1.ZoneImportMode_ZONE_IMPORT_MODE_REPLACE:
		mode = zone.ImportReplace
	default:
		return nil, h.publicError(&zone.ValidationError{Field: "mode", Message: "choose create or replace"})
	}
	parsed, err := zonefile.Parse(request.Msg.GetName(), request.Msg.GetZoneFile())
	if err != nil {
		return nil, h.publicError(&zone.ValidationError{Field: "zone_file", Message: err.Error()})
	}

	h.stateMu.Lock()
	defer h.stateMu.Unlock()

	// REPLACE rebuilds the zone from the file alone, so it drops every
	// engine-owned record exactly as DeleteZone would. It is refused for the
	// same reason and through the same guard: whatever the imported file does
	// not itself carry, the site or the mailboxes stop having. The dry run is
	// refused too, so the preview says so before anything is uploaded.
	if mode == zone.ImportReplace {
		existing, found, err := h.zoneByName(ctx, request.Msg.GetName())
		if err != nil {
			return nil, h.internalError("look up the zone before an import", err)
		}
		if found {
			if err := h.refuseBoundZone(ctx, existing.ID, "replacing every record in this domain"); err != nil {
				return nil, err
			}
		}
	}

	// The import rebuilds the zone from the file, so any engine record left
	// behind by a detached site or an unbound domain goes with it. Say so
	// before and after.
	warnings := append(slices.Clone(parsed.Warnings), h.engineRecordWarnings(ctx, request.Msg.GetName())...)

	imported, err := h.store.ImportZone(ctx, request.Msg.GetName(), parsed.Records, mode, dryRun)
	if err != nil {
		return nil, h.publicError(err)
	}
	value = imported
	if !dryRun {
		if err := h.reloadAfterMutation(ctx); err != nil {
			return nil, h.internalError("reload after importing zone", err)
		}
		if mode == zone.ImportReplace {
			h.notify(ctx, func(notifyCtx context.Context, observer enginedns.ZoneObserver) {
				observer.ZoneReplaced(notifyCtx, imported.ID)
			})
		}
	}
	return connect.NewResponse(&dnsv1.ImportZoneResponse{
		Zone:     zoneToProto(imported),
		Warnings: warnings,
		DryRun:   dryRun,
	}), nil
}

func (h *Handler) ExportZone(ctx context.Context, request *connect.Request[dnsv1.ExportZoneRequest]) (*connect.Response[dnsv1.ExportZoneResponse], error) {
	if request.Msg.GetZoneId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("zone_id is required"))
	}
	h.stateMu.RLock()
	defer h.stateMu.RUnlock()
	value, err := h.store.Get(ctx, request.Msg.GetZoneId())
	if err != nil {
		return nil, h.publicError(err)
	}
	raw, err := zonefile.Export(value)
	if err != nil {
		return nil, h.internalError("export zone", err)
	}
	return connect.NewResponse(&dnsv1.ExportZoneResponse{Name: value.Name, ZoneFile: raw}), nil
}

// GetZone reads one zone for an engine. It takes the read side of the state
// mutex, so it never observes a half-applied mutation.
func (h *Handler) GetZone(ctx context.Context, zoneID string) (zone.Zone, error) {
	h.stateMu.RLock()
	defer h.stateMu.RUnlock()
	value, err := h.store.Get(ctx, zoneID)
	if err != nil {
		return zone.Zone{}, h.publicError(err)
	}
	return value, nil
}

// ApplyRecordSet is the only way an engine writes DNS. It takes the same state
// mutex as every operator mutation, republishes the authoritative state and
// records one activity event per operation under the caller's actor.
func (h *Handler) ApplyRecordSet(ctx context.Context, zoneID string, ops []enginedns.Op, actor, correlationID string) (applied enginedns.Applied, resultErr error) {
	started := h.mutationStarted()
	defer func() { h.recordMutation(ctx, "record", "engine_apply", resultErr, started) }()

	if zoneID == "" {
		return enginedns.Applied{}, connect.NewError(connect.CodeInvalidArgument, errors.New("zone_id is required"))
	}
	if actor == "" {
		actor = activity.ActorReconciler
	}

	h.stateMu.Lock()
	defer h.stateMu.Unlock()

	before, err := h.store.Get(ctx, zoneID)
	if err != nil {
		return enginedns.Applied{}, h.publicError(err)
	}
	previous := make(map[string]zone.Record, len(before.Records))
	for _, record := range before.Records {
		previous[record.ID] = record
	}

	updated, created, err := h.store.ApplyRecordSet(ctx, zoneID, ops)
	if err != nil {
		return enginedns.Applied{}, h.publicError(err)
	}
	if err := h.reloadAfterMutation(ctx); err != nil {
		return enginedns.Applied{}, h.internalError("reload after applying an engine record set", err)
	}
	h.recordApplied(ctx, updated, ops, created, previous, actor, correlationID)
	return enginedns.Applied{Zone: updated, RecordIDs: created}, nil
}

// ZoneMutator adapts the handler to enginedns.ZoneMutator. The adapter exists
// because the interface's ListZones(ctx) collides with the DNSService RPC of
// the same name, which the handler must keep.
func (h *Handler) ZoneMutator() enginedns.ZoneMutator {
	return zoneMutator{handler: h}
}

type zoneMutator struct {
	handler *Handler
}

func (m zoneMutator) GetZone(ctx context.Context, zoneID string) (zone.Zone, error) {
	return m.handler.GetZone(ctx, zoneID)
}

func (m zoneMutator) ListZones(ctx context.Context) ([]zone.Zone, error) {
	m.handler.stateMu.RLock()
	defer m.handler.stateMu.RUnlock()
	values, err := m.handler.store.List(ctx)
	if err != nil {
		return nil, m.handler.internalError("list zones", err)
	}
	return values, nil
}

func (m zoneMutator) ApplyRecordSet(ctx context.Context, zoneID string, ops []enginedns.Op, actor, correlationID string) (enginedns.Applied, error) {
	return m.handler.ApplyRecordSet(ctx, zoneID, ops, actor, correlationID)
}

func (h *Handler) recordApplied(
	ctx context.Context,
	value zone.Zone,
	ops []enginedns.Op,
	created map[int]string,
	previous map[string]zone.Record,
	actor, correlationID string,
) {
	current := make(map[string]zone.Record, len(value.Records))
	for _, record := range value.Records {
		current[record.ID] = record
	}
	for index, op := range ops {
		var kind activity.Kind
		var summary string
		var record zone.Record
		details := map[string]string{}
		switch op.Kind {
		case zone.OpCreate:
			kind = activity.KindDNSRecordCreated
			record = op.Record
			if id, ok := created[index]; ok {
				record = current[id]
			}
			summary = fmt.Sprintf("Added %s to %s", describeRecord(record), value.Name)
		case zone.OpUpdate:
			kind = activity.KindDNSRecordUpdated
			record = current[op.RecordID]
			summary = fmt.Sprintf("Changed %s on %s", describeRecord(record), value.Name)
			if before, ok := previous[op.RecordID]; ok {
				details["record.before_value"] = truncateValue(before.Value)
				if before.Source != record.Source {
					details["record.before_source"] = sourceLabel(before.Source)
				}
			}
		case zone.OpDelete:
			kind = activity.KindDNSRecordDeleted
			record = previous[op.RecordID]
			summary = fmt.Sprintf("Removed %s from %s", describeRecord(record), value.Name)
			details["replaced_by"] = sourceLabel(actorSource(actor)) + " records"
		default:
			continue
		}
		for key, detail := range recordDetails(record) {
			details[key] = detail
		}
		h.recorder.Record(ctx, activity.Event{
			ZoneID:        value.ID,
			ZoneName:      value.Name,
			Actor:         actor,
			Kind:          kind,
			Severity:      activity.SeverityInfo,
			Summary:       summary,
			Details:       details,
			CorrelationID: correlationID,
		})
	}
}

// zoneByName finds a stored zone by its name. It reports the store's error
// rather than swallowing it, so a caller that has to fail closed — the bound
// check an import runs — can tell "no such zone" from "could not look".
func (h *Handler) zoneByName(ctx context.Context, rawName string) (zone.Zone, bool, error) {
	name, err := zone.NormalizeName(rawName)
	if err != nil {
		return zone.Zone{}, false, nil
	}
	values, err := h.store.List(ctx)
	if err != nil {
		return zone.Zone{}, false, err
	}
	for _, value := range values {
		if value.Name == name {
			return value, true, nil
		}
	}
	return zone.Zone{}, false, nil
}

// engineRecordWarnings reports how many engine records an import is about to
// drop. It runs for dry-run and for the real thing, so the preview and the
// result say the same thing.
//
// A zone whose site is still attached or whose email is still bound is refused
// outright, so what this counts is the records a detached site or an unbound
// domain left behind. The warning says they are removed and stops there: it
// used to promise the engine would re-create them, which is false whenever the
// imported file occupies one of those names, and false again when there is no
// engine left to do it.
func (h *Handler) engineRecordWarnings(ctx context.Context, rawName string) []string {
	value, found, err := h.zoneByName(ctx, rawName)
	if err != nil {
		h.logger.Warn("count engine records before an import", "error", err)
		return nil
	}
	if !found {
		return nil
	}
	counts := map[string]int{}
	for _, record := range value.Records {
		if record.Source != zone.SourceUser {
			counts[record.Source]++
		}
	}
	warnings := make([]string, 0, 2)
	for _, source := range []string{zone.SourceHosting, zone.SourceMail} {
		if counts[source] == 0 {
			continue
		}
		noun := "records"
		if counts[source] == 1 {
			noun = "record"
		}
		warnings = append(warnings, fmt.Sprintf(
			"%d %s written by the %s engine will be removed by this import",
			counts[source], noun, sourceLabel(source)))
	}
	return warnings
}

// notify hands the observers to a goroutine: the reload has already happened,
// the state mutex is still held by the caller's defer, and an RPC must not wait
// on a reconciler.
func (h *Handler) notify(ctx context.Context, deliver func(context.Context, enginedns.ZoneObserver)) {
	h.observerMu.RLock()
	observers := slices.Clone(h.observers)
	h.observerMu.RUnlock()
	if len(observers) == 0 {
		return
	}
	notifyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), h.observerTimeout)
	h.observerWG.Add(1)
	go func() {
		defer h.observerWG.Done()
		defer cancel()
		for _, observer := range observers {
			deliver(notifyCtx, observer)
		}
	}()
}

func (h *Handler) event(ctx context.Context, err error, kind activity.Kind, value zone.Zone, fallbackName, summary, failure string, details map[string]string) {
	if h.recorder == nil {
		return
	}
	event := activity.Event{
		ZoneID:   value.ID,
		ZoneName: value.Name,
		Actor:    activity.ActorOperator,
		Kind:     kind,
		Severity: activity.SeverityInfo,
		Summary:  summary,
		Details:  details,
	}
	if event.ZoneName == "" {
		event.ZoneName = fallbackName
	}
	if err != nil {
		event.Severity = activity.SeverityError
		event.Summary = failure
		if event.Details == nil {
			event.Details = map[string]string{}
		}
		event.Details["code"] = connect.CodeOf(err).String()
	}
	h.recorder.Record(ctx, event)
}

func (h *Handler) reloadAfterMutation(requestContext context.Context) error {
	reloadContext, cancel := context.WithTimeout(context.WithoutCancel(requestContext), 5*time.Second)
	defer cancel()
	return h.reload(reloadContext)
}

func (h *Handler) recordInput(ctx context.Context, zoneID, name string, recordType dnsv1.RecordType, ttl uint32, value string) (zone.Zone, zone.Record, error) {
	if zoneID == "" {
		return zone.Zone{}, zone.Record{}, &zone.ValidationError{Field: "zone_id", Message: "is required"}
	}
	stored, err := h.store.Get(ctx, zoneID)
	if err != nil {
		return zone.Zone{}, zone.Record{}, err
	}
	domainType, ok := recordTypeFromProto(recordType)
	if !ok {
		return zone.Zone{}, zone.Record{}, &zone.ValidationError{Field: "type", Message: "choose a supported record type"}
	}
	record, err := zone.NormalizeRecord(stored.Name, name, domainType, ttl, value)
	if err != nil {
		return zone.Zone{}, zone.Record{}, err
	}
	return stored, record, nil
}

func (h *Handler) publicError(err error) error {
	var validation *zone.ValidationError
	switch {
	case errors.As(err, &validation):
		return connect.NewError(connect.CodeInvalidArgument, validation)
	case errors.Is(err, zone.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, errors.New("zone or record not found"))
	case errors.Is(err, zone.ErrAlreadyExists):
		return connect.NewError(connect.CodeAlreadyExists, errors.New("zone or record already exists"))
	case errors.Is(err, zone.ErrManagedRecord):
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("managed SOA and NS records cannot be changed"))
	case errors.Is(err, zone.ErrEngineOwnedRRSet):
		return connect.NewError(connect.CodeFailedPrecondition,
			errors.New("an engine already answers that name and type; detach the site or unbind email to take it over"))
	case errors.Is(err, zone.ErrEngineOwned):
		return engineOwnedError("")
	case errors.Is(err, context.Canceled):
		return connect.NewError(connect.CodeCanceled, context.Canceled)
	case errors.Is(err, context.DeadlineExceeded):
		return connect.NewError(connect.CodeDeadlineExceeded, context.DeadlineExceeded)
	default:
		return h.internalError("control API", err)
	}
}

// engineOwnedError refuses an edit rather than warning about one. The
// reconcilers rewrite an edited record within one pass, so a warning would
// promise an outcome that does not happen — and for the apex A or the MX it
// would break the site or bounce mail in the meantime. The release path is one
// click, and the message says which one.
func engineOwnedError(source string) error {
	switch source {
	case zone.SourceHosting:
		return connect.NewError(connect.CodeFailedPrecondition, errors.New(
			"this record is written by the Website engine; detach the site (Website card → Detach) or choose 'keep the records' there to take it over"))
	case zone.SourceMail:
		return connect.NewError(connect.CodeFailedPrecondition, errors.New(
			"this record is written by the Email engine; unbind email (Email card → Unbind) or choose 'keep the records' there to take it over"))
	default:
		return connect.NewError(connect.CodeFailedPrecondition, errors.New(
			"this record is written by an engine; detach the site or unbind email to take it over"))
	}
}

// engineOwnedSetError refuses a new record that would join an RRset an engine
// owns. The remedy is the same as engineOwnedError's, but the sentence has to
// be different: the record being written is the operator's, and claiming an
// engine wrote it would be untrue. What is true is that the answer at that name
// is the engine's, and a second member changes it.
func engineOwnedSetError(source string, record zone.Record) error {
	where := record.Name
	if where == "@" {
		where = "the domain itself"
	}
	remedy := "detach the site or unbind email to take these records over"
	switch source {
	case zone.SourceHosting:
		remedy = "detach the site (Website card → Detach) or choose 'keep the records' there to take them over"
	case zone.SourceMail:
		remedy = "unbind email (Email card → Unbind) or choose 'keep the records' there to take them over"
	}
	return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
		"the %s engine answers %s at %s, and every record at one name answers together, so another %s record there would change what it answers; %s",
		sourceLabel(source), record.Type, where, record.Type, remedy))
}

func (h *Handler) internalError(operation string, err error) error {
	h.logger.Error(operation, "error", err)
	return connect.NewError(connect.CodeInternal, errors.New("internal server error"))
}

func (h *Handler) mutationStarted() time.Time {
	if h.metrics.MetricsEnabled() {
		return time.Now()
	}
	return time.Time{}
}

func (h *Handler) recordMutation(ctx context.Context, entity, operation string, err error, started time.Time) {
	outcome := "success"
	errorType := "none"
	if err != nil {
		outcome = "error"
		errorType = connect.CodeOf(err).String()
	}
	elapsed := time.Duration(0)
	if !started.IsZero() {
		elapsed = time.Since(started)
	}
	h.metrics.ControlMutation(ctx, entity, operation, outcome, errorType, elapsed)
}

func findRecord(value zone.Zone, recordID string) (zone.Record, bool) {
	index := slices.IndexFunc(value.Records, func(candidate zone.Record) bool { return candidate.ID == recordID })
	if index < 0 {
		return zone.Record{}, false
	}
	return value.Records[index], true
}

func displayName(value zone.Zone, fallback string) string {
	if value.Name != "" {
		return value.Name
	}
	return fallback
}

func describeRecord(record zone.Record) string {
	if record.Type == "" {
		return "a record"
	}
	return fmt.Sprintf("%s %s %s", record.Type, record.Name, truncateValue(record.Value))
}

func recordDetails(record zone.Record) map[string]string {
	if record.Type == "" {
		return map[string]string{}
	}
	details := map[string]string{
		"record.name":  record.Name,
		"record.type":  string(record.Type),
		"record.ttl":   strconv.FormatUint(uint64(record.TTL), 10),
		"record.value": truncateValue(record.Value),
	}
	if record.ID != "" {
		details["record.id"] = record.ID
	}
	details["record.source"] = sourceLabel(record.Source)
	return details
}

func truncateValue(value string) string {
	runes := []rune(value)
	if len(runes) <= maxDetailValueRunes {
		return value
	}
	return string(runes[:maxDetailValueRunes]) + "…"
}

func sourceLabel(source string) string {
	switch source {
	case zone.SourceHosting:
		return "Website"
	case zone.SourceMail:
		return "Email"
	default:
		return "Custom"
	}
}

func actorSource(actor string) string {
	switch actor {
	case activity.ActorHostingEngine:
		return zone.SourceHosting
	case activity.ActorMailEngine:
		return zone.SourceMail
	default:
		return zone.SourceUser
	}
}

func zoneToProto(value zone.Zone) *dnsv1.Zone {
	result := &dnsv1.Zone{
		Id:          value.ID,
		Name:        value.Name,
		Serial:      value.Serial,
		CreatedAt:   timestamppb.New(value.CreatedAt),
		UpdatedAt:   timestamppb.New(value.UpdatedAt),
		Nameservers: value.Nameservers,
		Records:     make([]*dnsv1.Record, 0, len(value.Records)),
	}
	for _, record := range value.Records {
		result.Records = append(result.Records, &dnsv1.Record{
			Id:        record.ID,
			Name:      record.Name,
			Type:      recordTypeToProto(record.Type),
			Ttl:       record.TTL,
			Value:     record.Value,
			Managed:   record.Managed,
			CreatedAt: timestamppb.New(record.CreatedAt),
			UpdatedAt: timestamppb.New(record.UpdatedAt),
			Source:    enginedns.SourceToProto(record.Source),
		})
	}
	return result
}

func recordTypeFromProto(value dnsv1.RecordType) (zone.RecordType, bool) {
	switch value {
	case dnsv1.RecordType_RECORD_TYPE_A:
		return zone.TypeA, true
	case dnsv1.RecordType_RECORD_TYPE_AAAA:
		return zone.TypeAAAA, true
	case dnsv1.RecordType_RECORD_TYPE_CNAME:
		return zone.TypeCNAME, true
	case dnsv1.RecordType_RECORD_TYPE_MX:
		return zone.TypeMX, true
	case dnsv1.RecordType_RECORD_TYPE_TXT:
		return zone.TypeTXT, true
	case dnsv1.RecordType_RECORD_TYPE_NS:
		return zone.TypeNS, true
	case dnsv1.RecordType_RECORD_TYPE_SRV:
		return zone.TypeSRV, true
	case dnsv1.RecordType_RECORD_TYPE_CAA:
		return zone.TypeCAA, true
	default:
		return "", false
	}
}

func recordTypeToProto(value zone.RecordType) dnsv1.RecordType {
	switch value {
	case zone.TypeA:
		return dnsv1.RecordType_RECORD_TYPE_A
	case zone.TypeAAAA:
		return dnsv1.RecordType_RECORD_TYPE_AAAA
	case zone.TypeCNAME:
		return dnsv1.RecordType_RECORD_TYPE_CNAME
	case zone.TypeMX:
		return dnsv1.RecordType_RECORD_TYPE_MX
	case zone.TypeTXT:
		return dnsv1.RecordType_RECORD_TYPE_TXT
	case zone.TypeNS:
		return dnsv1.RecordType_RECORD_TYPE_NS
	case zone.TypeSRV:
		return dnsv1.RecordType_RECORD_TYPE_SRV
	case zone.TypeCAA:
		return dnsv1.RecordType_RECORD_TYPE_CAA
	case zone.TypeSOA:
		return dnsv1.RecordType_RECORD_TYPE_SOA
	default:
		return dnsv1.RecordType_RECORD_TYPE_UNSPECIFIED
	}
}

var (
	_ dnsv1connect.DNSServiceHandler = (*Handler)(nil)
	_ enginedns.ZoneMutator          = zoneMutator{}
)
