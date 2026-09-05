package main

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"connectrpc.com/connect"

	dnsv1 "github.com/castlemilk/dns/gen/go/dns/v1"
	"github.com/castlemilk/dns/internal/simplecli/client"
)

func cmdDomain(ctx context.Context, e *env, o *options, args []string) error {
	if len(args) == 0 {
		return &usageError{message: "domain: choose list, add, show or delete", usage: domainUsage}
	}
	switch args[0] {
	case "list", "ls":
		return cmdDomainList(ctx, e, o, args[1:])
	case "add", "create":
		return cmdDomainAdd(ctx, e, o, args[1:])
	case "show", "get":
		return cmdDomainShow(ctx, e, o, args[1:])
	case "delete", "rm":
		return cmdDomainDelete(ctx, e, o, args[1:])
	case "help", "-h", "--help":
		return &helpError{usage: domainUsage}
	default:
		return &usageError{message: fmt.Sprintf("unknown domain command %q", args[0]), usage: domainUsage}
	}
}

func cmdDomainList(ctx context.Context, e *env, o *options, args []string) error {
	fs, err := o.parse("simple domain list", domainUsage, args, nil)
	if err != nil {
		return err
	}
	if err := noExtraArgs(fs, 0, domainUsage); err != nil {
		return err
	}
	clients, err := o.connect(e)
	if err != nil {
		return err
	}
	callCtx, cancel := o.call(ctx)
	defer cancel()
	response, err := clients.DNS.ListZones(callCtx, connect.NewRequest(&dnsv1.ListZonesRequest{}))
	if err != nil {
		return fail(clients, err)
	}
	zones := response.Msg
	return newPrinter(e, o).emit(zones, func(w io.Writer) error {
		if len(zones.GetZones()) == 0 {
			return writeLine(w, "no zones yet; create one with `simple domain add NAME`")
		}
		table := newTable(w, "NAME", "ZONE ID", "SERIAL", "RECORDS", "UPDATED")
		for _, zone := range zones.GetZones() {
			table.row(
				zone.GetName(),
				zone.GetId(),
				formatUint(zone.GetSerial()),
				strconv.Itoa(len(zone.GetRecords())),
				formatTime(zone.GetUpdatedAt()),
			)
		}
		return table.flush()
	})
}

func cmdDomainAdd(ctx context.Context, e *env, o *options, args []string) error {
	fs, err := o.parse("simple domain add", domainUsage, args, nil)
	if err != nil {
		return err
	}
	name, err := argument(fs, 0, "domain", domainUsage)
	if err != nil {
		return err
	}
	if err := noExtraArgs(fs, 1, domainUsage); err != nil {
		return err
	}
	clients, err := o.connect(e)
	if err != nil {
		return err
	}
	callCtx, cancel := o.call(ctx)
	defer cancel()
	response, err := clients.DNS.CreateZone(callCtx, connect.NewRequest(&dnsv1.CreateZoneRequest{Name: name}))
	if err != nil {
		return fail(clients, err)
	}
	zone := response.Msg.GetZone()
	return newPrinter(e, o).emit(response.Msg, func(w io.Writer) error {
		return fields(w,
			pair("created", zone.GetName()),
			pair("zone id", zone.GetId()),
			pair("nameservers", orDash(strings.Join(zone.GetNameservers(), " "))),
		)
	})
}

func cmdDomainShow(ctx context.Context, e *env, o *options, args []string) error {
	fs, err := o.parse("simple domain show", domainUsage, args, nil)
	if err != nil {
		return err
	}
	reference, err := argument(fs, 0, "domain", domainUsage)
	if err != nil {
		return err
	}
	if err := noExtraArgs(fs, 1, domainUsage); err != nil {
		return err
	}
	clients, err := o.connect(e)
	if err != nil {
		return err
	}
	callCtx, cancel := o.call(ctx)
	defer cancel()
	zone, err := lookupZone(callCtx, clients, reference)
	if err != nil {
		return err
	}
	return newPrinter(e, o).emit(zone, func(w io.Writer) error {
		if err := fields(w,
			pair("name", zone.GetName()),
			pair("zone id", zone.GetId()),
			pair("serial", formatUint(zone.GetSerial())),
			pair("nameservers", orDash(strings.Join(zone.GetNameservers(), " "))),
			pair("created", formatTime(zone.GetCreatedAt())),
			pair("updated", formatTime(zone.GetUpdatedAt())),
		); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
		return writeRecords(w, zone.GetRecords())
	})
}

func cmdDomainDelete(ctx context.Context, e *env, o *options, args []string) error {
	fs, err := o.parse("simple domain delete", domainUsage, args, nil)
	if err != nil {
		return err
	}
	reference, err := argument(fs, 0, "domain", domainUsage)
	if err != nil {
		return err
	}
	if err := noExtraArgs(fs, 1, domainUsage); err != nil {
		return err
	}
	clients, err := o.connect(e)
	if err != nil {
		return err
	}
	lookupCtx, cancelLookup := o.call(ctx)
	defer cancelLookup()
	zone, err := lookupZone(lookupCtx, clients, reference)
	if err != nil {
		return err
	}
	if err := confirm(e, o, fmt.Sprintf(
		"Delete %s and its %d records? Sites and mail bound to it stop resolving.",
		zone.GetName(), len(zone.GetRecords()))); err != nil {
		return err
	}
	callCtx, cancel := o.call(ctx)
	defer cancel()
	if _, err := clients.DNS.DeleteZone(callCtx,
		connect.NewRequest(&dnsv1.DeleteZoneRequest{ZoneId: zone.GetId()})); err != nil {
		return fail(clients, err)
	}
	document := struct {
		Deleted bool   `json:"deleted"`
		ZoneID  string `json:"zone_id"`
		Name    string `json:"name"`
	}{Deleted: true, ZoneID: zone.GetId(), Name: zone.GetName()}
	return newPrinter(e, o).emit(document, func(w io.Writer) error {
		return writeLine(w, "deleted "+zone.GetName())
	})
}

// lookupZone finds a zone by name or by id. The control plane has no
// get-zone-by-name RPC, so one list is fetched and matched here; the same list
// is what every zone-scoped command needs anyway.
func lookupZone(ctx context.Context, clients *client.Clients, reference string) (*dnsv1.Zone, error) {
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return nil, usagef("domain: is required")
	}
	response, err := clients.DNS.ListZones(ctx, connect.NewRequest(&dnsv1.ListZonesRequest{}))
	if err != nil {
		return nil, fail(clients, err)
	}
	wanted := strings.ToLower(strings.TrimSuffix(reference, "."))
	for _, zone := range response.Msg.GetZones() {
		if zone.GetId() == reference {
			return zone, nil
		}
		if strings.ToLower(strings.TrimSuffix(zone.GetName(), ".")) == wanted {
			return zone, nil
		}
	}
	return nil, fmt.Errorf("domain: %q is not a zone on this control plane", reference)
}

// lookupZoneWithTimeout is lookupZone with this invocation's --timeout applied,
// for the commands that resolve a zone before doing something else with it.
func lookupZoneWithTimeout(ctx context.Context, o *options, clients *client.Clients, reference string) (*dnsv1.Zone, error) {
	callCtx, cancel := o.call(ctx)
	defer cancel()
	return lookupZone(callCtx, clients, reference)
}

// writeRecords is the one record table, shared by `domain show` and
// `record list`, so a record reads the same wherever it appears.
func writeRecords(w io.Writer, records []*dnsv1.Record) error {
	if len(records) == 0 {
		return writeLine(w, "no records")
	}
	table := newTable(w, "RECORD ID", "NAME", "TYPE", "TTL", "VALUE", "SOURCE")
	for _, record := range records {
		table.row(
			record.GetId(),
			orDash(record.GetName()),
			recordTypeLabel(record.GetType()),
			formatUint(record.GetTtl()),
			record.GetValue(),
			recordSource(record),
		)
	}
	return table.flush()
}

// recordSource says who owns a record, because an engine-owned record is not
// the operator's to edit: the reconciler would put it back.
func recordSource(record *dnsv1.Record) string {
	source := label(record.GetSource().String(), "RECORD_SOURCE_")
	if source == dash {
		if record.GetManaged() {
			return "managed"
		}
		return "user"
	}
	return source
}
