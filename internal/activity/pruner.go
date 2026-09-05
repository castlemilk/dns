package activity

import (
	"context"
	"fmt"
	"log/slog"
)

// DefaultMaxEvents is the ring size when ACTIVITY_MAX_EVENTS is unset.
const DefaultMaxEvents = 50_000

// Pruner trims the log to a fixed number of newest events. The janitor calls
// it; nothing else deletes an event, so an event is never mutated or removed
// individually and the log stays append-only within its ring.
type Pruner struct {
	store  EventStore
	keep   int
	logger *slog.Logger
}

// NewPruner builds the trimmer. A keep of zero or less falls back to
// DefaultMaxEvents so a misconfiguration cannot empty the log.
func NewPruner(store EventStore, keep int, logger *slog.Logger) *Pruner {
	if keep <= 0 {
		keep = DefaultMaxEvents
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Pruner{store: store, keep: keep, logger: logger.With("component", "activity.pruner")}
}

// Keep reports the configured ring size.
func (p *Pruner) Keep() int { return p.keep }

// Prune removes the oldest events beyond the ring size and returns how many
// were removed.
func (p *Pruner) Prune(ctx context.Context) (int, error) {
	if p.store == nil {
		return 0, nil
	}
	if err := ctx.Err(); err != nil {
		return 0, fmt.Errorf("prune activity events: %w", err)
	}
	removed, err := p.store.PruneEvents(ctx, p.keep)
	if err != nil {
		return 0, fmt.Errorf("prune activity events: %w", err)
	}
	if removed > 0 {
		p.logger.Info("trimmed the activity log", "removed", removed, "keep", p.keep)
	}
	return removed, nil
}
