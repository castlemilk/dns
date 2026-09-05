package simplemcp

import (
	"encoding/json"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	dnsv1 "github.com/castlemilk/dns/gen/go/dns/v1"
	hostingv1 "github.com/castlemilk/dns/gen/go/hosting/v1"
)

// args is one tools/call arguments object, left as raw JSON so each field can
// be reported against its own name when it is the wrong shape.
type args map[string]json.RawMessage

// present reports whether the caller supplied name at all. A JSON null counts
// as absent, because that is what a model emits for "I have no value here".
func (a args) present(name string) bool {
	raw, found := a[name]
	return found && len(raw) > 0 && string(raw) != "null"
}

func stringArg(a args, name string, required bool) (string, error) {
	if !a.present(name) {
		if required {
			return "", invalid(name, "required")
		}
		return "", nil
	}
	var value string
	if err := json.Unmarshal(a[name], &value); err != nil {
		return "", invalid(name, "must be a string")
	}
	value = strings.TrimSpace(value)
	if value == "" && required {
		return "", invalid(name, "required")
	}
	return value, nil
}

// rawStringArg is stringArg without the trim, for values whose leading or
// trailing whitespace is part of them: a zone file, an environment variable.
func rawStringArg(a args, name string, required bool) (string, error) {
	if !a.present(name) {
		if required {
			return "", invalid(name, "required")
		}
		return "", nil
	}
	var value string
	if err := json.Unmarshal(a[name], &value); err != nil {
		return "", invalid(name, "must be a string")
	}
	if value == "" && required {
		return "", invalid(name, "required")
	}
	return value, nil
}

func boolArg(a args, name string, fallback bool) (bool, error) {
	if !a.present(name) {
		return fallback, nil
	}
	var value bool
	if err := json.Unmarshal(a[name], &value); err != nil {
		return false, invalid(name, "must be true or false")
	}
	return value, nil
}

// optionalBoolArg distinguishes "leave it as it is" from "set it to false",
// which the proto3 optional fields on the mail and hosting updates need.
func optionalBoolArg(a args, name string) (*bool, error) {
	if !a.present(name) {
		return nil, nil
	}
	var value bool
	if err := json.Unmarshal(a[name], &value); err != nil {
		return nil, invalid(name, "must be true or false")
	}
	return &value, nil
}

func uint32Arg(a args, name string) (uint32, error) {
	if !a.present(name) {
		return 0, nil
	}
	var value int64
	if err := json.Unmarshal(a[name], &value); err != nil {
		return 0, invalid(name, "must be a whole number")
	}
	if value < 0 || value > 1<<32-1 {
		return 0, invalid(name, "must be between 0 and 4294967295")
	}
	return uint32(value), nil
}

func uint64Arg(a args, name string) (uint64, error) {
	if !a.present(name) {
		return 0, nil
	}
	var value int64
	if err := json.Unmarshal(a[name], &value); err != nil {
		return 0, invalid(name, "must be a whole number")
	}
	if value < 0 {
		return 0, invalid(name, "must not be negative")
	}
	return uint64(value), nil
}

func stringsArg(a args, name string, required bool) ([]string, error) {
	if !a.present(name) {
		if required {
			return nil, invalid(name, "required")
		}
		return nil, nil
	}
	var values []string
	if err := json.Unmarshal(a[name], &values); err != nil {
		return nil, invalid(name, "must be an array of strings")
	}
	trimmed := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			trimmed = append(trimmed, value)
		}
	}
	if len(trimmed) == 0 && required {
		return nil, invalid(name, "required")
	}
	return trimmed, nil
}

// recordTypes is the set the control plane will create. SOA is absent because
// the control plane writes it and refuses to be told what it should say.
var recordTypes = map[string]dnsv1.RecordType{
	"A":     dnsv1.RecordType_RECORD_TYPE_A,
	"AAAA":  dnsv1.RecordType_RECORD_TYPE_AAAA,
	"CNAME": dnsv1.RecordType_RECORD_TYPE_CNAME,
	"MX":    dnsv1.RecordType_RECORD_TYPE_MX,
	"TXT":   dnsv1.RecordType_RECORD_TYPE_TXT,
	"NS":    dnsv1.RecordType_RECORD_TYPE_NS,
	"SRV":   dnsv1.RecordType_RECORD_TYPE_SRV,
	"CAA":   dnsv1.RecordType_RECORD_TYPE_CAA,
}

const recordTypeList = "A, AAAA, CNAME, MX, TXT, NS, SRV, CAA"

// recordTypeArg accepts either the familiar DNS spelling ("MX") or the proto
// enum name ("RECORD_TYPE_MX"), because a model reading a previous answer will
// have seen the second.
func recordTypeArg(a args, name string, required bool) (dnsv1.RecordType, error) {
	value, err := stringArg(a, name, required)
	if err != nil {
		return dnsv1.RecordType_RECORD_TYPE_UNSPECIFIED, err
	}
	if value == "" {
		return dnsv1.RecordType_RECORD_TYPE_UNSPECIFIED, nil
	}
	normalised := strings.ToUpper(value)
	normalised = strings.TrimPrefix(normalised, "RECORD_TYPE_")
	recordType, known := recordTypes[normalised]
	if !known {
		return dnsv1.RecordType_RECORD_TYPE_UNSPECIFIED, invalid(name, "must be one of "+recordTypeList)
	}
	return recordType, nil
}

var frameworks = map[string]hostingv1.Framework{
	"STATIC": hostingv1.Framework_FRAMEWORK_STATIC,
	"NODE":   hostingv1.Framework_FRAMEWORK_NODE,
	"NEXTJS": hostingv1.Framework_FRAMEWORK_NEXTJS,
}

func frameworkArg(a args, name string, required bool) (hostingv1.Framework, error) {
	value, err := stringArg(a, name, required)
	if err != nil {
		return hostingv1.Framework_FRAMEWORK_UNSPECIFIED, err
	}
	if value == "" {
		return hostingv1.Framework_FRAMEWORK_UNSPECIFIED, nil
	}
	normalised := strings.ToUpper(value)
	normalised = strings.TrimPrefix(normalised, "FRAMEWORK_")
	framework, known := frameworks[normalised]
	if !known {
		return hostingv1.Framework_FRAMEWORK_UNSPECIFIED, invalid(name, "must be one of static, node, nextjs")
	}
	return framework, nil
}

var wwwModes = map[string]hostingv1.WwwMode{
	"SERVE":    hostingv1.WwwMode_WWW_MODE_SERVE,
	"REDIRECT": hostingv1.WwwMode_WWW_MODE_REDIRECT,
}

func wwwModeArg(a args, name string, required bool) (hostingv1.WwwMode, error) {
	value, err := stringArg(a, name, required)
	if err != nil {
		return hostingv1.WwwMode_WWW_MODE_UNSPECIFIED, err
	}
	if value == "" {
		return hostingv1.WwwMode_WWW_MODE_UNSPECIFIED, nil
	}
	normalised := strings.ToUpper(value)
	normalised = strings.TrimPrefix(normalised, "WWW_MODE_")
	mode, known := wwwModes[normalised]
	if !known {
		return hostingv1.WwwMode_WWW_MODE_UNSPECIFIED, invalid(name, "must be serve or redirect")
	}
	return mode, nil
}

var importModes = map[string]dnsv1.ZoneImportMode{
	"CREATE":  dnsv1.ZoneImportMode_ZONE_IMPORT_MODE_CREATE,
	"REPLACE": dnsv1.ZoneImportMode_ZONE_IMPORT_MODE_REPLACE,
}

func importModeArg(a args, name string) (dnsv1.ZoneImportMode, error) {
	value, err := stringArg(a, name, false)
	if err != nil {
		return dnsv1.ZoneImportMode_ZONE_IMPORT_MODE_UNSPECIFIED, err
	}
	if value == "" {
		return dnsv1.ZoneImportMode_ZONE_IMPORT_MODE_CREATE, nil
	}
	normalised := strings.ToUpper(value)
	normalised = strings.TrimPrefix(normalised, "ZONE_IMPORT_MODE_")
	mode, known := importModes[normalised]
	if !known {
		return dnsv1.ZoneImportMode_ZONE_IMPORT_MODE_UNSPECIFIED, invalid(name, "must be create or replace")
	}
	return mode, nil
}

// timestampArg parses an RFC 3339 instant, the same spelling every timestamp
// in an answer from this server carries.
func timestampArg(a args, name string) (*timestamppb.Timestamp, error) {
	value, err := stringArg(a, name, false)
	if err != nil {
		return nil, err
	}
	if value == "" {
		return nil, nil
	}
	instant, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return nil, invalid(name, "must be an RFC 3339 timestamp, for example 2026-09-05T09:00:00Z")
	}
	return timestamppb.New(instant), nil
}
