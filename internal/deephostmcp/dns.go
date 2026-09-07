package deephostmcp

import (
	"context"
	"strings"

	"connectrpc.com/connect"

	dnsv1 "github.com/castlemilk/dns/gen/go/dns/v1"
	platformv1 "github.com/castlemilk/dns/gen/go/platform/v1"
)

// serverVersion answers from this process. It is the one tool that works when
// there is no control plane and no credential, so an operator can always find
// out what this server thinks it is pointed at.
func (s *Server) serverVersion(context.Context, args) (any, error) {
	answer := map[string]any{
		"name":                  serverName,
		"version":               s.version,
		"protocol_version":      protocolVersion,
		"api_url":               s.target,
		"credential_configured": s.credentialed,
		"control_plane_usable":  s.clients != nil,
	}
	if s.unusable != "" {
		answer["reason"] = s.unusable
	}
	if s.credentialNote != "" {
		answer["credential_note"] = s.credentialNote
	}
	return answer, nil
}

func (s *Server) status(ctx context.Context, a args) (any, error) {
	probe, err := boolArg(a, "probe", false)
	if err != nil {
		return nil, err
	}
	response, err := s.clients.Platform.GetPlatformStatus(ctx, connect.NewRequest(&platformv1.GetPlatformStatusRequest{Probe: probe}))
	if err != nil {
		return nil, err
	}
	return view(response.Msg)
}

func (s *Server) domainList(ctx context.Context, _ args) (any, error) {
	response, err := s.clients.DNS.ListZones(ctx, connect.NewRequest(&dnsv1.ListZonesRequest{}))
	if err != nil {
		return nil, err
	}
	return view(response.Msg)
}

func (s *Server) domainShow(ctx context.Context, a args) (any, error) {
	zone, err := s.findZone(ctx, a)
	if err != nil {
		return nil, err
	}
	rendered, err := view(zone)
	if err != nil {
		return nil, err
	}
	return map[string]any{"zone": rendered}, nil
}

func (s *Server) domainAdd(ctx context.Context, a args) (any, error) {
	name, err := stringArg(a, "name", true)
	if err != nil {
		return nil, err
	}
	response, err := s.clients.DNS.CreateZone(ctx, connect.NewRequest(&dnsv1.CreateZoneRequest{Name: name}))
	if err != nil {
		return nil, err
	}
	return viewWith(response.Msg, map[string]any{
		"note": "The zone exists here. Traffic only reaches it once the domain's registrar delegates it to the nameservers deephost_status reports.",
	})
}

func (s *Server) domainDelete(ctx context.Context, a args) (any, error) {
	zone, err := s.resolveZone(ctx, a)
	if err != nil {
		return nil, err
	}
	if _, err := s.clients.DNS.DeleteZone(ctx, connect.NewRequest(&dnsv1.DeleteZoneRequest{ZoneId: zone.ID})); err != nil {
		return nil, err
	}
	return map[string]any{"deleted": true, "zone_id": zone.ID, "domain": zone.Name}, nil
}

func (s *Server) recordList(ctx context.Context, a args) (any, error) {
	zone, err := s.findZone(ctx, a)
	if err != nil {
		return nil, err
	}
	recordType, err := recordTypeArg(a, "type", false)
	if err != nil {
		return nil, err
	}
	name, err := stringArg(a, "name", false)
	if err != nil {
		return nil, err
	}
	records := make([]any, 0, len(zone.GetRecords()))
	for _, record := range zone.GetRecords() {
		if recordType != dnsv1.RecordType_RECORD_TYPE_UNSPECIFIED && record.GetType() != recordType {
			continue
		}
		if name != "" && !strings.EqualFold(record.GetName(), name) {
			continue
		}
		rendered, err := view(record)
		if err != nil {
			return nil, err
		}
		records = append(records, rendered)
	}
	return map[string]any{"zone_id": zone.GetId(), "domain": zone.GetName(), "records": records}, nil
}

func (s *Server) recordAdd(ctx context.Context, a args) (any, error) {
	zone, err := s.resolveZone(ctx, a)
	if err != nil {
		return nil, err
	}
	name, err := stringArg(a, "name", true)
	if err != nil {
		return nil, err
	}
	recordType, err := recordTypeArg(a, "type", true)
	if err != nil {
		return nil, err
	}
	value, err := stringArg(a, "value", true)
	if err != nil {
		return nil, err
	}
	ttl, err := uint32Arg(a, "ttl")
	if err != nil {
		return nil, err
	}
	response, err := s.clients.DNS.CreateRecord(ctx, connect.NewRequest(&dnsv1.CreateRecordRequest{
		ZoneId: zone.ID, Name: name, Type: recordType, Ttl: ttl, Value: value,
	}))
	if err != nil {
		return nil, err
	}
	return view(response.Msg)
}

// recordEdit fills the fields the caller left out from the record as it stands,
// so changing a TTL does not require restating the value that must not change.
func (s *Server) recordEdit(ctx context.Context, a args) (any, error) {
	zone, err := s.findZone(ctx, a)
	if err != nil {
		return nil, err
	}
	recordID, err := stringArg(a, "record_id", true)
	if err != nil {
		return nil, err
	}
	var current *dnsv1.Record
	for _, record := range zone.GetRecords() {
		if record.GetId() == recordID {
			current = record
			break
		}
	}
	if current == nil {
		return nil, notFound("record_id: " + zone.GetName() + " has no record with that id; call deephost_record_list to see the ones it has")
	}
	name := current.GetName()
	if a.present("name") {
		if name, err = stringArg(a, "name", true); err != nil {
			return nil, err
		}
	}
	recordType := current.GetType()
	if a.present("type") {
		if recordType, err = recordTypeArg(a, "type", true); err != nil {
			return nil, err
		}
	}
	value := current.GetValue()
	if a.present("value") {
		if value, err = stringArg(a, "value", true); err != nil {
			return nil, err
		}
	}
	ttl := current.GetTtl()
	if a.present("ttl") {
		if ttl, err = uint32Arg(a, "ttl"); err != nil {
			return nil, err
		}
	}
	response, err := s.clients.DNS.UpdateRecord(ctx, connect.NewRequest(&dnsv1.UpdateRecordRequest{
		ZoneId: zone.GetId(), RecordId: recordID, Name: name, Type: recordType, Ttl: ttl, Value: value,
	}))
	if err != nil {
		return nil, err
	}
	return view(response.Msg)
}

func (s *Server) recordDelete(ctx context.Context, a args) (any, error) {
	zone, err := s.resolveZone(ctx, a)
	if err != nil {
		return nil, err
	}
	recordID, err := stringArg(a, "record_id", true)
	if err != nil {
		return nil, err
	}
	response, err := s.clients.DNS.DeleteRecord(ctx, connect.NewRequest(&dnsv1.DeleteRecordRequest{
		ZoneId: zone.ID, RecordId: recordID,
	}))
	if err != nil {
		return nil, err
	}
	return viewWith(response.Msg, map[string]any{"deleted": true, "record_id": recordID})
}

func (s *Server) recordImport(ctx context.Context, a args) (any, error) {
	name, err := stringArg(a, "name", true)
	if err != nil {
		return nil, err
	}
	zoneFile, err := rawStringArg(a, "zone_file", true)
	if err != nil {
		return nil, err
	}
	mode, err := importModeArg(a, "mode")
	if err != nil {
		return nil, err
	}
	dryRun, err := boolArg(a, "dry_run", false)
	if err != nil {
		return nil, err
	}
	response, err := s.clients.DNS.ImportZone(ctx, connect.NewRequest(&dnsv1.ImportZoneRequest{
		Name: name, ZoneFile: zoneFile, Mode: mode, DryRun: dryRun,
	}))
	if err != nil {
		return nil, err
	}
	return view(response.Msg)
}

func (s *Server) recordExport(ctx context.Context, a args) (any, error) {
	zone, err := s.resolveZone(ctx, a)
	if err != nil {
		return nil, err
	}
	response, err := s.clients.DNS.ExportZone(ctx, connect.NewRequest(&dnsv1.ExportZoneRequest{ZoneId: zone.ID}))
	if err != nil {
		return nil, err
	}
	return viewWith(response.Msg, map[string]any{"zone_id": zone.ID})
}
