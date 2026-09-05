// Package activity is the append-only record of what happened: DNS changes,
// deploys, mail and billing state, engine transitions. It is a leaf package —
// it imports only internal/telemetry and internal/secretguard — so every
// producer can depend on it and the durable store (internal/platform) can
// implement its interface without a cycle.
package activity

import (
	"context"
	"strings"
	"time"
)

// DocVersion is the schema version stamped on every stored event.
const DocVersion = 1

// Severity is the operator-facing weight of an event.
type Severity string

const (
	SeverityInfo  Severity = "info"
	SeverityWarn  Severity = "warn"
	SeverityError Severity = "error"
)

// Valid reports whether s is one of the three severities.
func (s Severity) Valid() bool {
	switch s {
	case SeverityInfo, SeverityWarn, SeverityError:
		return true
	default:
		return false
	}
}

// Actors. The operator token is the only human principal, so an operator event
// says "operator"; everything else names the subsystem that acted.
const (
	ActorOperator       = "operator"
	ActorHostingEngine  = "hosting-engine"
	ActorMailEngine     = "mail-engine"
	ActorBillingWebhook = "billing-webhook"
	ActorReconciler     = "reconciler"
	ActorSystem         = "system"
)

// Bounds applied by the sanitiser. They keep one event small enough that a
// misbehaving producer cannot fill the store or the log.
const (
	MaxSummaryRunes    = 200
	MaxDetailKeys      = 20
	MaxDetailValueSize = 512
)

// Event is what a producer records. The log assigns the id and the timestamp.
type Event struct {
	ZoneID        string
	ZoneName      string
	Actor         string
	Kind          Kind
	Severity      Severity
	Summary       string
	Details       map[string]string
	CorrelationID string
}

// EventDoc is the stored form. Unknown JSON fields are ignored on read, so
// adding a field is a no-op migration.
type EventDoc struct {
	V             int               `json:"v"`
	ID            string            `json:"id"`
	Time          time.Time         `json:"time"`
	ZoneID        string            `json:"zone_id,omitempty"`
	ZoneName      string            `json:"zone_name,omitempty"`
	Actor         string            `json:"actor"`
	Kind          string            `json:"kind"`
	Severity      string            `json:"severity"`
	Summary       string            `json:"summary"`
	Details       map[string]string `json:"details,omitempty"`
	CorrelationID string            `json:"correlation_id,omitempty"`
}

// ListFilter selects a page of events, newest first.
type ListFilter struct {
	// ZoneID selects one zone; empty means every zone.
	ZoneID string
	// Kinds are exact kinds, or prefixes ending in a dot ("deploy.").
	Kinds []string
	// Since drops events older than this instant.
	Since time.Time
	// Limit caps the page; the handler clamps it to [1, MaxListLimit].
	Limit int
	// Cursor is the id of the event to continue before.
	Cursor string
}

// MatchesKind reports whether kind passes the filter's kind list. It is
// exported so a store implementation and the handler agree on the rule.
func (f ListFilter) MatchesKind(kind string) bool {
	if len(f.Kinds) == 0 {
		return true
	}
	for _, wanted := range f.Kinds {
		if wanted == "" {
			continue
		}
		if strings.HasSuffix(wanted, ".") {
			if strings.HasPrefix(kind, wanted) {
				return true
			}
			continue
		}
		if kind == wanted {
			return true
		}
	}
	return false
}

// EventStore is the durable half of the log, implemented by
// *platform.Store. Every method takes a context and reports its own errors;
// the log never panics on a store failure.
type EventStore interface {
	AppendEvent(ctx context.Context, doc EventDoc) (string, error)
	ListEvents(ctx context.Context, filter ListFilter) ([]EventDoc, string, error)
	GetEvent(ctx context.Context, id string) (EventDoc, error)
	CountEvents(ctx context.Context) (uint64, error)
	PruneEvents(ctx context.Context, keep int) (int, error)
}

// Recorder is what producers hold. Record never returns an error and never
// blocks on one: an unrecordable event is logged and counted, because losing an
// audit line must not fail the operation that produced it.
type Recorder interface {
	Record(ctx context.Context, event Event)
}

type nopRecorder struct{}

func (nopRecorder) Record(context.Context, Event) {}

// Nop returns a recorder that discards everything. Tests and the authority role
// use it.
func Nop() Recorder { return nopRecorder{} }
