package enginedns

import (
	"context"

	"github.com/castlemilk/dns/internal/zone"
)

// ZoneObserver is notified after an operator mutation that can invalidate an
// engine's DNS state. The control handler calls observers after the
// authoritative reload, outside its state mutex and from a goroutine, so an
// observer may take its time — but it must not call back into a mutating RPC.
type ZoneObserver interface {
	// ZoneReplaced reports an applied ImportZone in REPLACE mode: every record
	// id in the zone is new, so an engine must re-plan from scratch.
	ZoneReplaced(ctx context.Context, zoneID string)
	// RecordDeleted reports a record removed through the DNS API.
	RecordDeleted(ctx context.Context, zoneID string, record zone.Record)
	// ZoneDeleted reports a zone removed through the DNS API.
	ZoneDeleted(ctx context.Context, zoneID string)
}

// NopObserver satisfies ZoneObserver and does nothing. Engines that are not
// configured return it, so wiring never has to branch on nil.
type NopObserver struct{}

func (NopObserver) ZoneReplaced(context.Context, string)               {}
func (NopObserver) RecordDeleted(context.Context, string, zone.Record) {}
func (NopObserver) ZoneDeleted(context.Context, string)                {}

var _ ZoneObserver = NopObserver{}
