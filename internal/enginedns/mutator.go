package enginedns

import (
	"context"

	"github.com/castlemilk/dns/internal/zone"
)

// Op is one change inside a record set. spec2 §3.2 declares it as a struct in
// this package; it is an alias of zone.Op instead, so a plan can be handed
// straight to zone.Store.ApplyRecordSet without a translation layer. The fields
// are the ones the spec lists (Kind, RecordID, Record).
type Op = zone.Op

// Operation kinds. OpKeep never reaches the store: it labels the records a plan
// found already correct, so a caller can report "nothing to do" honestly.
const (
	OpCreate = zone.OpCreate
	OpUpdate = zone.OpUpdate
	OpDelete = zone.OpDelete
	OpKeep   = "keep"
)

// Applied is the result of one record set.
type Applied struct {
	// Zone is the zone as it stands after the write.
	Zone zone.Zone
	// RecordIDs maps the index of each create operation to the id it was given.
	RecordIDs map[int]string
}

// ZoneMutator is the only way an engine writes DNS. It is implemented by
// *control.Handler, which owns the state mutex and the authoritative reload, so
// engine writes and operator writes can never interleave and every engine write
// is republished and recorded exactly like an operator one.
type ZoneMutator interface {
	GetZone(ctx context.Context, zoneID string) (zone.Zone, error)
	ListZones(ctx context.Context) ([]zone.Zone, error)
	ApplyRecordSet(ctx context.Context, zoneID string, ops []Op, actor, correlationID string) (Applied, error)
}
