package deephostmcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	dnsv1 "github.com/castlemilk/dns/gen/go/dns/v1"
)

// render is how every answer is built. It uses the proto field names, so what
// a tool returns matches the .proto an operator can read, and it emits
// unpopulated fields, so a false stays visible: `counts_available: false` and
// `in_sync: false` are the fields that stop a caller from believing a zero, and
// omitting them would let it read absence as "no problem".
var render = protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: true}

// view turns a response message into the map that becomes structuredContent.
// Going through a map rather than embedding raw JSON keeps the tool result one
// JSON document and lets a handler add a field of its own beside it.
func view(message proto.Message) (map[string]any, error) {
	data, err := render.Marshal(message)
	if err != nil {
		return nil, &toolError{Kind: KindControlPlane, Message: "the control plane's answer could not be rendered as JSON"}
	}
	value := map[string]any{}
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, &toolError{Kind: KindControlPlane, Message: "the control plane's answer could not be rendered as JSON"}
	}
	return value, nil
}

// viewWith is view plus fields this server adds: a note, a resolved zone name,
// a warning that belongs to the tool rather than to the message.
func viewWith(message proto.Message, extra map[string]any) (map[string]any, error) {
	value, err := view(message)
	if err != nil {
		return nil, err
	}
	for name, field := range extra {
		value[name] = field
	}
	return value, nil
}

// zoneRef is a zone identified for a tool: both the id the RPCs take and the
// name a human recognises, so every answer can name the domain it acted on.
type zoneRef struct {
	ID   string
	Name string
}

// findZone returns one whole zone, records included. It accepts either
// `domain` (the name a person uses) or `zone_id` (what the RPCs take), and
// resolves a name through ListZones because there is no lookup-by-name RPC —
// which is also the only call that carries a zone's records.
func (s *Server) findZone(ctx context.Context, a args) (*dnsv1.Zone, error) {
	id, err := stringArg(a, "zone_id", false)
	if err != nil {
		return nil, err
	}
	name, err := stringArg(a, "domain", false)
	if err != nil {
		return nil, err
	}
	if id == "" && name == "" {
		return nil, invalid("domain", "required (or zone_id)")
	}
	zones, err := s.listZones(ctx)
	if err != nil {
		return nil, err
	}
	for _, zone := range zones {
		if id != "" && zone.GetId() == id {
			return zone, nil
		}
		if id == "" && equalDomain(zone.GetName(), name) {
			return zone, nil
		}
	}
	if id != "" {
		return nil, notFound("zone_id: no domain with that id is hosted here; call deephost_domain_list to see the domains that are")
	}
	return nil, notFound("domain: " + name + " is not hosted here; call deephost_domain_list to see the domains that are")
}

// resolveZone is findZone for the tools that need only the id and the name. It
// differs in one way: when the lookup itself failed but the caller gave an id,
// the id is taken at face value, because the RPC the tool is about to make
// reports the trouble better than the listing that failed on the way to it.
func (s *Server) resolveZone(ctx context.Context, a args) (zoneRef, error) {
	zone, err := s.findZone(ctx, a)
	if err == nil {
		return zoneRef{ID: zone.GetId(), Name: zone.GetName()}, nil
	}
	var refusal *toolError
	if !errors.As(err, &refusal) {
		if id, idErr := stringArg(a, "zone_id", false); idErr == nil && id != "" {
			return zoneRef{ID: id}, nil
		}
	}
	return zoneRef{}, err
}

func (s *Server) listZones(ctx context.Context) ([]*dnsv1.Zone, error) {
	response, err := s.clients.DNS.ListZones(ctx, connect.NewRequest(&dnsv1.ListZonesRequest{}))
	if err != nil {
		return nil, err
	}
	return response.Msg.GetZones(), nil
}

// equalDomain compares domain names the way DNS does: case-insensitively, and
// without caring whether the caller wrote the trailing dot.
func equalDomain(left, right string) bool {
	return strings.EqualFold(strings.TrimSuffix(left, "."), strings.TrimSuffix(right, "."))
}
