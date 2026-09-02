package control

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"connectrpc.com/connect"
	dnsv1 "github.com/castlemilk/dns/gen/go/dns/v1"
	"github.com/castlemilk/dns/gen/go/dns/v1/dnsv1connect"
	"github.com/castlemilk/dns/internal/authoritative"
	"github.com/castlemilk/dns/internal/zone"
	"github.com/castlemilk/dns/internal/zonefile"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type Handler struct {
	store     *zone.Store
	dns       *authoritative.Server
	logger    *slog.Logger
	startedAt time.Time
	stateMu   sync.RWMutex
}

func NewHandler(store *zone.Store, dnsServer *authoritative.Server, logger *slog.Logger, startedAt time.Time) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{store: store, dns: dnsServer, logger: logger, startedAt: startedAt.UTC()}
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

func (h *Handler) CreateZone(ctx context.Context, request *connect.Request[dnsv1.CreateZoneRequest]) (*connect.Response[dnsv1.CreateZoneResponse], error) {
	h.stateMu.Lock()
	defer h.stateMu.Unlock()

	value, err := h.store.Create(ctx, request.Msg.GetName())
	if err != nil {
		return nil, h.publicError(err)
	}
	if err := h.reloadAfterMutation(ctx); err != nil {
		return nil, h.internalError("reload after creating zone", err)
	}
	return connect.NewResponse(&dnsv1.CreateZoneResponse{Zone: zoneToProto(value)}), nil
}

func (h *Handler) DeleteZone(ctx context.Context, request *connect.Request[dnsv1.DeleteZoneRequest]) (*connect.Response[dnsv1.DeleteZoneResponse], error) {
	if request.Msg.GetZoneId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("zone_id is required"))
	}
	h.stateMu.Lock()
	defer h.stateMu.Unlock()

	if err := h.store.Delete(ctx, request.Msg.GetZoneId()); err != nil {
		return nil, h.publicError(err)
	}
	if err := h.reloadAfterMutation(ctx); err != nil {
		return nil, h.internalError("reload after deleting zone", err)
	}
	return connect.NewResponse(&dnsv1.DeleteZoneResponse{}), nil
}

func (h *Handler) CreateRecord(ctx context.Context, request *connect.Request[dnsv1.CreateRecordRequest]) (*connect.Response[dnsv1.CreateRecordResponse], error) {
	h.stateMu.Lock()
	defer h.stateMu.Unlock()

	value, record, err := h.recordInput(ctx, request.Msg.GetZoneId(), request.Msg.GetName(), request.Msg.GetType(), request.Msg.GetTtl(), request.Msg.GetValue())
	if err != nil {
		return nil, h.publicError(err)
	}
	updated, err := h.store.CreateRecord(ctx, value.ID, record)
	if err != nil {
		return nil, h.publicError(err)
	}
	if err := h.reloadAfterMutation(ctx); err != nil {
		return nil, h.internalError("reload after creating record", err)
	}
	return connect.NewResponse(&dnsv1.CreateRecordResponse{Zone: zoneToProto(updated)}), nil
}

func (h *Handler) UpdateRecord(ctx context.Context, request *connect.Request[dnsv1.UpdateRecordRequest]) (*connect.Response[dnsv1.UpdateRecordResponse], error) {
	if request.Msg.GetRecordId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("record_id is required"))
	}
	h.stateMu.Lock()
	defer h.stateMu.Unlock()

	value, record, err := h.recordInput(ctx, request.Msg.GetZoneId(), request.Msg.GetName(), request.Msg.GetType(), request.Msg.GetTtl(), request.Msg.GetValue())
	if err != nil {
		return nil, h.publicError(err)
	}
	updated, err := h.store.UpdateRecord(ctx, value.ID, request.Msg.GetRecordId(), record)
	if err != nil {
		return nil, h.publicError(err)
	}
	if err := h.reloadAfterMutation(ctx); err != nil {
		return nil, h.internalError("reload after updating record", err)
	}
	return connect.NewResponse(&dnsv1.UpdateRecordResponse{Zone: zoneToProto(updated)}), nil
}

func (h *Handler) DeleteRecord(ctx context.Context, request *connect.Request[dnsv1.DeleteRecordRequest]) (*connect.Response[dnsv1.DeleteRecordResponse], error) {
	if request.Msg.GetZoneId() == "" || request.Msg.GetRecordId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("zone_id and record_id are required"))
	}
	h.stateMu.Lock()
	defer h.stateMu.Unlock()

	updated, err := h.store.DeleteRecord(ctx, request.Msg.GetZoneId(), request.Msg.GetRecordId())
	if err != nil {
		return nil, h.publicError(err)
	}
	if err := h.reloadAfterMutation(ctx); err != nil {
		return nil, h.internalError("reload after deleting record", err)
	}
	return connect.NewResponse(&dnsv1.DeleteRecordResponse{Zone: zoneToProto(updated)}), nil
}

func (h *Handler) ImportZone(ctx context.Context, request *connect.Request[dnsv1.ImportZoneRequest]) (*connect.Response[dnsv1.ImportZoneResponse], error) {
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
	value, err := h.store.ImportZone(ctx, request.Msg.GetName(), parsed.Records, mode, request.Msg.GetDryRun())
	if err != nil {
		return nil, h.publicError(err)
	}
	if !request.Msg.GetDryRun() {
		if err := h.reloadAfterMutation(ctx); err != nil {
			return nil, h.internalError("reload after importing zone", err)
		}
	}
	return connect.NewResponse(&dnsv1.ImportZoneResponse{
		Zone:     zoneToProto(value),
		Warnings: parsed.Warnings,
		DryRun:   request.Msg.GetDryRun(),
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
	case errors.Is(err, context.Canceled):
		return connect.NewError(connect.CodeCanceled, context.Canceled)
	case errors.Is(err, context.DeadlineExceeded):
		return connect.NewError(connect.CodeDeadlineExceeded, context.DeadlineExceeded)
	default:
		return h.internalError("control API", err)
	}
}

func (h *Handler) internalError(operation string, err error) error {
	h.logger.Error(operation, "error", err)
	return connect.NewError(connect.CodeInternal, errors.New("internal server error"))
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

var _ dnsv1connect.DNSServiceHandler = (*Handler)(nil)
