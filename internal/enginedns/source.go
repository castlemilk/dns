// Package enginedns holds the DNS vocabulary shared by the hosting and mail
// facades: record provenance, the per-zone serializer that keeps two workers
// from flapping one zone, the mutator the control handler implements, and the
// planner that turns a desired record set into adopt/align/create/remove
// operations. It depends only on internal/zone.
//
// Two names differ from spec2 because Go cannot express the spec's shape:
//
//   - spec2 §3.4 writes the planner as
//     `enginedns.Plan(z, source, desired) (Plan, error)`, but one package
//     cannot declare both `func Plan` and `type Plan`. The function keeps the
//     name and the result type is [ZonePlan].
//   - spec2 §3.2 declares `type Op struct{…}` here; it is instead an alias,
//     `Op = zone.Op` (see mutator.go), so a plan's operations can be handed
//     straight to zone.Store.ApplyRecordSet with no translation layer.
//
// Write `enginedns.ZonePlan` where the spec says `enginedns.Plan` as a type.
package enginedns

import (
	dnsv1 "github.com/castlemilk/dns/gen/go/dns/v1"
	"github.com/castlemilk/dns/internal/zone"
)

// Record provenance, re-exported from internal/zone so engine code has one
// import for the whole DNS vocabulary.
const (
	SourceUser    = zone.SourceUser
	SourceHosting = zone.SourceHosting
	SourceMail    = zone.SourceMail
)

// ValidSource reports whether source is one of the three persisted values.
func ValidSource(source string) bool {
	return zone.ValidSource(source)
}

// SourceToProto maps a stored provenance value onto the wire enum. The user
// source is reported as RECORD_SOURCE_USER, never UNSPECIFIED, so a client can
// tell "this control plane does not report provenance" from "the user wrote
// this record".
func SourceToProto(source string) dnsv1.RecordSource {
	switch source {
	case zone.SourceHosting:
		return dnsv1.RecordSource_RECORD_SOURCE_HOSTING
	case zone.SourceMail:
		return dnsv1.RecordSource_RECORD_SOURCE_MAIL
	default:
		return dnsv1.RecordSource_RECORD_SOURCE_USER
	}
}

// SourceFromProto maps the wire enum back onto a stored value. It reports false
// for UNSPECIFIED and for any value this binary does not know.
func SourceFromProto(value dnsv1.RecordSource) (string, bool) {
	switch value {
	case dnsv1.RecordSource_RECORD_SOURCE_USER:
		return zone.SourceUser, true
	case dnsv1.RecordSource_RECORD_SOURCE_HOSTING:
		return zone.SourceHosting, true
	case dnsv1.RecordSource_RECORD_SOURCE_MAIL:
		return zone.SourceMail, true
	default:
		return zone.SourceUser, false
	}
}

// SourceLabel names an engine in operator-facing copy.
func SourceLabel(source string) string {
	switch source {
	case zone.SourceHosting:
		return "Website"
	case zone.SourceMail:
		return "Email"
	default:
		return "Custom"
	}
}
