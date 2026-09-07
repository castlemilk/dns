package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"connectrpc.com/connect"

	dnsv1 "github.com/castlemilk/dns/gen/go/dns/v1"
	"github.com/castlemilk/dns/internal/zonefile"
)

// defaultTTL is what a record gets when the operator does not say. Five minutes
// is short enough that a mistake is cheap to correct.
const defaultTTL = 300

func cmdRecord(ctx context.Context, e *env, o *options, args []string) error {
	if len(args) == 0 {
		return &usageError{message: "record: choose list, add, edit, delete, import or export", usage: recordUsage}
	}
	switch args[0] {
	case "list", "ls":
		return cmdRecordList(ctx, e, o, args[1:])
	case "add", "create":
		return cmdRecordAdd(ctx, e, o, args[1:])
	case "edit", "update":
		return cmdRecordEdit(ctx, e, o, args[1:])
	case "delete", "rm":
		return cmdRecordDelete(ctx, e, o, args[1:])
	case "import":
		return cmdRecordImport(ctx, e, o, args[1:])
	case "export":
		return cmdRecordExport(ctx, e, o, args[1:])
	case "help", "-h", "--help":
		return &helpError{usage: recordUsage}
	default:
		return &usageError{message: fmt.Sprintf("unknown record command %q", args[0]), usage: recordUsage}
	}
}

func cmdRecordList(ctx context.Context, e *env, o *options, args []string) error {
	var typeFilter, nameFilter string
	fs, err := o.parse("deephost record list", recordUsage, args, func(fs *flag.FlagSet) {
		fs.StringVar(&typeFilter, "type", "", "show only this record type")
		fs.StringVar(&nameFilter, "name", "", "show only records with this owner name")
	})
	if err != nil {
		return err
	}
	reference, err := argument(fs, 0, "domain", recordUsage)
	if err != nil {
		return err
	}
	if err := noExtraArgs(fs, 1, recordUsage); err != nil {
		return err
	}
	wantedType := dnsv1.RecordType_RECORD_TYPE_UNSPECIFIED
	if strings.TrimSpace(typeFilter) != "" {
		if wantedType, err = parseRecordType(typeFilter); err != nil {
			return err
		}
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
	wantedName := strings.ToLower(strings.TrimSpace(nameFilter))
	matched := make([]*dnsv1.Record, 0, len(zone.GetRecords()))
	for _, record := range zone.GetRecords() {
		if wantedType != dnsv1.RecordType_RECORD_TYPE_UNSPECIFIED && record.GetType() != wantedType {
			continue
		}
		if wantedName != "" && strings.ToLower(record.GetName()) != wantedName {
			continue
		}
		matched = append(matched, record)
	}
	// The filtered view is still a zone, so `record list --json` and
	// `domain show --json` describe records with exactly the same shape.
	view := &dnsv1.Zone{
		Id:      zone.GetId(),
		Name:    zone.GetName(),
		Serial:  zone.GetSerial(),
		Records: matched,
	}
	return newPrinter(e, o).emit(view, func(w io.Writer) error {
		return writeRecords(w, matched)
	})
}

func cmdRecordAdd(ctx context.Context, e *env, o *options, args []string) error {
	var name, recordType, value string
	var ttl uint
	fs, err := o.parse("deephost record add", recordUsage, args, func(fs *flag.FlagSet) {
		fs.StringVar(&name, "name", "", "owner name, relative to the zone (@ for the apex)")
		fs.StringVar(&recordType, "type", "", "record type")
		fs.StringVar(&value, "value", "", "record value")
		fs.UintVar(&ttl, "ttl", defaultTTL, "time to live in seconds")
	})
	if err != nil {
		return err
	}
	reference, err := argument(fs, 0, "domain", recordUsage)
	if err != nil {
		return err
	}
	if err := noExtraArgs(fs, 1, recordUsage); err != nil {
		return err
	}
	switch {
	case strings.TrimSpace(name) == "":
		return usagef("--name: is required")
	case strings.TrimSpace(recordType) == "":
		return usagef("--type: is required")
	case strings.TrimSpace(value) == "":
		return usagef("--value: is required")
	}
	parsedType, err := parseRecordType(recordType)
	if err != nil {
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
	callCtx, cancel := o.call(ctx)
	defer cancel()
	response, err := clients.DNS.CreateRecord(callCtx, connect.NewRequest(&dnsv1.CreateRecordRequest{
		ZoneId: zone.GetId(),
		Name:   name,
		Type:   parsedType,
		Ttl:    uint32(ttl), //nolint:gosec // the flag is bounded by the control plane's own validation
		Value:  value,
	}))
	if err != nil {
		return fail(clients, err)
	}
	return emitZoneChange(e, o, response.Msg.GetZone(), "added")
}

func cmdRecordEdit(ctx context.Context, e *env, o *options, args []string) error {
	var name, recordType, value string
	var ttl uint
	fs, err := o.parse("deephost record edit", recordUsage, args, func(fs *flag.FlagSet) {
		fs.StringVar(&name, "name", "", "new owner name")
		fs.StringVar(&recordType, "type", "", "new record type")
		fs.StringVar(&value, "value", "", "new record value")
		fs.UintVar(&ttl, "ttl", 0, "new time to live in seconds")
	})
	if err != nil {
		return err
	}
	reference, err := argument(fs, 0, "domain", recordUsage)
	if err != nil {
		return err
	}
	recordID, err := argument(fs, 1, "record_id", recordUsage)
	if err != nil {
		return err
	}
	if err := noExtraArgs(fs, 2, recordUsage); err != nil {
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
	current := findRecord(zone, recordID)
	if current == nil {
		return fmt.Errorf("record_id: %q is not a record in %s", recordID, zone.GetName())
	}
	// UpdateRecord replaces the whole record, so an unspecified flag has to be
	// filled from what is there now: an edit must never blank a field the
	// operator did not mention.
	request := &dnsv1.UpdateRecordRequest{
		ZoneId:   zone.GetId(),
		RecordId: current.GetId(),
		Name:     current.GetName(),
		Type:     current.GetType(),
		Ttl:      current.GetTtl(),
		Value:    current.GetValue(),
	}
	if provided(fs, "name") {
		request.Name = name
	}
	if provided(fs, "value") {
		request.Value = value
	}
	if provided(fs, "ttl") {
		request.Ttl = uint32(ttl) //nolint:gosec // bounded by the control plane's own validation
	}
	if provided(fs, "type") {
		parsedType, err := parseRecordType(recordType)
		if err != nil {
			return err
		}
		request.Type = parsedType
	}
	if !provided(fs, "name") && !provided(fs, "value") && !provided(fs, "ttl") && !provided(fs, "type") {
		return usagef("record edit: give at least one of --name, --type, --ttl or --value")
	}
	callCtx, cancel := o.call(ctx)
	defer cancel()
	response, err := clients.DNS.UpdateRecord(callCtx, connect.NewRequest(request))
	if err != nil {
		return fail(clients, err)
	}
	return emitZoneChange(e, o, response.Msg.GetZone(), "updated")
}

func cmdRecordDelete(ctx context.Context, e *env, o *options, args []string) error {
	fs, err := o.parse("deephost record delete", recordUsage, args, nil)
	if err != nil {
		return err
	}
	reference, err := argument(fs, 0, "domain", recordUsage)
	if err != nil {
		return err
	}
	recordID, err := argument(fs, 1, "record_id", recordUsage)
	if err != nil {
		return err
	}
	if err := noExtraArgs(fs, 2, recordUsage); err != nil {
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
	current := findRecord(zone, recordID)
	if current == nil {
		return fmt.Errorf("record_id: %q is not a record in %s", recordID, zone.GetName())
	}
	if err := confirm(e, o, fmt.Sprintf("Delete %s %s %q from %s?",
		orDash(current.GetName()), recordTypeLabel(current.GetType()), current.GetValue(), zone.GetName())); err != nil {
		return err
	}
	callCtx, cancel := o.call(ctx)
	defer cancel()
	if _, err := clients.DNS.DeleteRecord(callCtx, connect.NewRequest(&dnsv1.DeleteRecordRequest{
		ZoneId: zone.GetId(), RecordId: current.GetId(),
	})); err != nil {
		return fail(clients, err)
	}
	document := struct {
		Deleted  bool   `json:"deleted"`
		ZoneID   string `json:"zone_id"`
		RecordID string `json:"record_id"`
	}{Deleted: true, ZoneID: zone.GetId(), RecordID: current.GetId()}
	return newPrinter(e, o).emit(document, func(w io.Writer) error {
		return writeLine(w, "deleted record "+current.GetId())
	})
}

func cmdRecordImport(ctx context.Context, e *env, o *options, args []string) error {
	var file, mode string
	var dryRun bool
	fs, err := o.parse("deephost record import", recordUsage, args, func(fs *flag.FlagSet) {
		fs.StringVar(&file, "file", "-", "BIND zone file to read, or - for stdin")
		fs.StringVar(&mode, "mode", "create", "create a new zone, or replace an existing one")
		fs.BoolVar(&dryRun, "dry-run", false, "validate and report without writing anything")
	})
	if err != nil {
		return err
	}
	name, err := argument(fs, 0, "domain", recordUsage)
	if err != nil {
		return err
	}
	if err := noExtraArgs(fs, 1, recordUsage); err != nil {
		return err
	}
	var importMode dnsv1.ZoneImportMode
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "create":
		importMode = dnsv1.ZoneImportMode_ZONE_IMPORT_MODE_CREATE
	case "replace":
		importMode = dnsv1.ZoneImportMode_ZONE_IMPORT_MODE_REPLACE
	default:
		return usagef("--mode: must be create or replace")
	}
	raw, err := readInput(e, file, zonefile.MaxBytes)
	if err != nil {
		return err
	}
	if importMode == dnsv1.ZoneImportMode_ZONE_IMPORT_MODE_REPLACE && !dryRun {
		if err := confirm(e, o, fmt.Sprintf(
			"Replace every record in %s with the contents of this file?", name)); err != nil {
			return err
		}
	}
	clients, err := o.connect(e)
	if err != nil {
		return err
	}
	callCtx, cancel := o.call(ctx)
	defer cancel()
	response, err := clients.DNS.ImportZone(callCtx, connect.NewRequest(&dnsv1.ImportZoneRequest{
		Name: name, ZoneFile: string(raw), Mode: importMode, DryRun: dryRun,
	}))
	if err != nil {
		return fail(clients, err)
	}
	result := response.Msg
	return newPrinter(e, o).emit(result, func(w io.Writer) error {
		if err := fields(w,
			pair("zone", result.GetZone().GetName()),
			pair("zone id", orDash(result.GetZone().GetId())),
			pair("records", fmt.Sprint(len(result.GetZone().GetRecords()))),
			pair("dry run", yesNo(result.GetDryRun())),
		); err != nil {
			return err
		}
		return writeList(w, "warnings", result.GetWarnings())
	})
}

func cmdRecordExport(ctx context.Context, e *env, o *options, args []string) error {
	var output string
	fs, err := o.parse("deephost record export", recordUsage, args, func(fs *flag.FlagSet) {
		fs.StringVar(&output, "output", "-", "file to write, or - for stdout")
	})
	if err != nil {
		return err
	}
	reference, err := argument(fs, 0, "domain", recordUsage)
	if err != nil {
		return err
	}
	if err := noExtraArgs(fs, 1, recordUsage); err != nil {
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
	callCtx, cancel := o.call(ctx)
	defer cancel()
	response, err := clients.DNS.ExportZone(callCtx,
		connect.NewRequest(&dnsv1.ExportZoneRequest{ZoneId: zone.GetId()}))
	if err != nil {
		return fail(clients, err)
	}
	result := response.Msg
	if strings.TrimSpace(output) == "" || output == "-" {
		if o.json {
			return writeJSONTo(e.stdout, result)
		}
		// A zone file is data, not a report: it goes out exactly as the control
		// plane rendered it, with no brand mark to strip before feeding it
		// back in.
		_, err := io.WriteString(e.stdout, result.GetZoneFile())
		return err
	}
	// --output means a file, in either mode: a caller that asked for both a
	// file and a document gets both, rather than one of them silently dropped.
	if err := os.WriteFile(output, []byte(result.GetZoneFile()), 0o600); err != nil {
		return fmt.Errorf("output: %w", err)
	}
	if o.json {
		return writeJSONTo(e.stdout, struct {
			Name  string `json:"name"`
			Path  string `json:"path"`
			Bytes int    `json:"bytes"`
		}{Name: result.GetName(), Path: output, Bytes: len(result.GetZoneFile())})
	}
	return writeLine(e.stderr, "wrote "+output)
}

func findRecord(zone *dnsv1.Zone, recordID string) *dnsv1.Record {
	for _, record := range zone.GetRecords() {
		if record.GetId() == recordID {
			return record
		}
	}
	return nil
}

// emitZoneChange reports a record write as the zone that came back, so the
// caller sees the serial that resulted as well as the record they asked for.
func emitZoneChange(e *env, o *options, zone *dnsv1.Zone, verb string) error {
	return newPrinter(e, o).emit(zone, func(w io.Writer) error {
		if err := fields(w,
			pair(verb, zone.GetName()),
			pair("serial", formatUint(zone.GetSerial())),
		); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
		return writeRecords(w, zone.GetRecords())
	})
}

// readInput reads a file or stdin under a hard byte limit, so a wrong path or a
// runaway pipe fails here rather than in the control plane.
func readInput(e *env, path string, limit int) (raw []byte, resultErr error) {
	reader := e.stdin
	if strings.TrimSpace(path) != "" && path != "-" {
		info, err := os.Lstat(path)
		if err != nil {
			return nil, fmt.Errorf("--file: %w", err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("--file: %s is not a regular file", path)
		}
		file, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("--file: %w", err)
		}
		defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
		reader = file
	}
	if reader == nil {
		return nil, usagef("--file: nothing was supplied on stdin")
	}
	raw, err := io.ReadAll(io.LimitReader(reader, int64(limit)+1))
	if err != nil {
		return nil, fmt.Errorf("--file: %w", err)
	}
	if len(raw) > limit {
		return nil, fmt.Errorf("--file: input exceeds the %d-byte limit", limit)
	}
	return raw, nil
}
