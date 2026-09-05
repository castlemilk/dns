package platform

import (
	"context"
	"log/slog"
	"time"

	platformv1 "github.com/castlemilk/dns/gen/go/platform/v1"
	"github.com/castlemilk/dns/internal/activity"
	"github.com/castlemilk/dns/internal/enginedns"
	"github.com/castlemilk/dns/internal/telemetry"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Deps is everything an engine facade is handed at construction. It is frozen:
// hosting, mail and billing all take exactly this value, so their constructors
// never grow a parameter as the facades do.
type Deps struct {
	// Store is the platform state file. Nil is not valid for a configured
	// engine.
	Store *Store
	// Zones is the only way an engine writes DNS. *control.Handler implements
	// it through its ZoneMutator() adapter.
	Zones enginedns.ZoneMutator
	// Serial gives every zone one queue, so two workers can never flap a
	// record between two answers.
	Serial *enginedns.Serializer
	// Recorder receives one event per mutation. activity.Nop() is valid.
	Recorder activity.Recorder
	// Logger is a secretguard-wrapped logger.
	Logger *slog.Logger
	// Metrics is the shared instrument set; telemetry.Disabled() is valid.
	Metrics *telemetry.Metrics
	// Clock is the facade's time source; nil means time.Now.
	Clock func() time.Time
}

// Now reads the clock, defaulting to the wall clock in UTC.
func (d Deps) Now() time.Time {
	if d.Clock == nil {
		return time.Now().UTC()
	}
	return d.Clock().UTC()
}

// Log returns a usable logger even when none was supplied.
func (d Deps) Log() *slog.Logger {
	if d.Logger == nil {
		return slog.Default()
	}
	return d.Logger
}

// Record is a nil-safe recorder call.
func (d Deps) Record(ctx context.Context, event activity.Event) {
	if d.Recorder == nil {
		return
	}
	d.Recorder.Record(ctx, event)
}

// Meter returns a usable instrument set.
func (d Deps) Meter() *telemetry.Metrics { return telemetry.Select(d.Metrics) }

// EngineStatus is the facade-side view of one engine, mapped onto
// platform.v1.EngineStatus by the status service. Reason is already redacted
// and bounded by its producer; MissingEnv holds variable NAMES only.
type EngineStatus struct {
	Kind         platformv1.EngineKind
	Configured   bool
	Reachable    bool
	CheckedAt    time.Time
	Reason       string
	MissingEnv   []string
	Provider     string
	EndpointHost string
	Capabilities map[string]bool
	Version      string
}

// Proto renders the status for the wire.
func (s EngineStatus) Proto() *platformv1.EngineStatus {
	message := &platformv1.EngineStatus{
		Kind:         s.Kind,
		Configured:   s.Configured,
		Reachable:    s.Reachable,
		Reason:       s.Reason,
		MissingEnv:   append([]string(nil), s.MissingEnv...),
		Provider:     s.Provider,
		EndpointHost: s.EndpointHost,
		Version:      s.Version,
	}
	if !s.CheckedAt.IsZero() {
		message.CheckedAt = timestamppb.New(s.CheckedAt.UTC())
	}
	if len(s.Capabilities) > 0 {
		message.Capabilities = make(map[string]bool, len(s.Capabilities))
		for key, value := range s.Capabilities {
			message.Capabilities[key] = value
		}
	}
	return message
}

// EngineName maps an engine kind onto the bounded telemetry attribute and the
// activity event's `engine` detail.
func EngineName(kind platformv1.EngineKind) string {
	switch kind {
	case platformv1.EngineKind_ENGINE_KIND_DNS:
		return "dns"
	case platformv1.EngineKind_ENGINE_KIND_HOSTING:
		return "hosting"
	case platformv1.EngineKind_ENGINE_KIND_MAIL:
		return "mail"
	case platformv1.EngineKind_ENGINE_KIND_BILLING:
		return "billing"
	default:
		return "other"
	}
}

// Probe is one engine's health check. The prober calls Probe on its own
// cadence and reads Status without blocking on the engine.
type Probe interface {
	Kind() platformv1.EngineKind
	Probe(ctx context.Context) error
	Status() EngineStatus
}

// RebuildReport is what one engine reconstructed.
type RebuildReport struct {
	Count    uint32
	Warnings []string
}

// Rebuilder reconstructs an engine's rows from the engine itself after
// platform.db was lost. It runs inside the control plane, so no engine
// credential leaves the pod.
type Rebuilder interface {
	Kind() platformv1.EngineKind
	Rebuild(ctx context.Context, dryRun bool) (RebuildReport, error)
}
