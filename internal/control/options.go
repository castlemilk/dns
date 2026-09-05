package control

import (
	"context"
	"time"

	"github.com/castlemilk/dns/internal/activity"
)

// BindingChecker reports whether a zone still carries an engine binding. The
// platform store implements it; the handler holds only this interface so
// internal/control never depends on internal/platform.
type BindingChecker interface {
	HasBindings(ctx context.Context, zoneID string) (site bool, mail bool, err error)
}

// BindingCheckerFunc adapts a function to BindingChecker.
type BindingCheckerFunc func(ctx context.Context, zoneID string) (bool, bool, error)

func (f BindingCheckerFunc) HasBindings(ctx context.Context, zoneID string) (bool, bool, error) {
	return f(ctx, zoneID)
}

// Option customises a Handler after construction.
type Option func(*Handler)

// WithRecorder installs the activity recorder. Without it every mutation still
// succeeds and simply produces no event, which is what the authority role and
// most tests want.
func WithRecorder(recorder activity.Recorder) Option {
	return func(h *Handler) {
		if recorder != nil {
			h.recorder = recorder
		}
	}
}

// WithBindingChecker refuses DeleteZone while a site or mail binding exists, so
// a domain cannot be deleted out from under an engine that is still serving it.
func WithBindingChecker(checker BindingChecker) Option {
	return func(h *Handler) {
		if checker != nil {
			h.bindings = checker
		}
	}
}

// WithBindingCheckerFunc is WithBindingChecker for a bare function, so a caller
// may pass a method value such as store.HasBindings.
func WithBindingCheckerFunc(check func(ctx context.Context, zoneID string) (bool, bool, error)) Option {
	if check == nil {
		return func(*Handler) {}
	}
	return WithBindingChecker(BindingCheckerFunc(check))
}

// WithObserverTimeout bounds one round of zone-observer notifications.
func WithObserverTimeout(timeout time.Duration) Option {
	return func(h *Handler) {
		if timeout > 0 {
			h.observerTimeout = timeout
		}
	}
}
