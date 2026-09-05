package billing

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"
	"github.com/castlemilk/dns/internal/billing/provider"
	"github.com/castlemilk/dns/internal/secretguard"
)

// NotConfigured is the exact sentence an operator sees when the billing block
// is absent. It names the variables and never a value.
const NotConfigured = "Billing isn't configured on the control API. Set BILLING_PROVIDER, STRIPE_SECRET_KEY, STRIPE_WEBHOOK_SECRET, STRIPE_PRICE_ID and BILLING_CUSTOMER_EMAIL."

// Unreachable is what a caller sees when the provider is configured but did not
// answer. It never carries the provider's raw error text.
const Unreachable = "The billing provider didn't answer. Try again in a moment."

// maxReason bounds the operator-facing failure text kept on the engine status.
const maxReason = 300

// copyError carries an operator-facing sentence verbatim. It is a type rather
// than errors.New because the sentences end in a full stop: they are UI copy,
// not wrapped error strings.
type copyError string

func (e copyError) Error() string { return string(e) }

func notConfigured() error {
	return connect.NewError(connect.CodeFailedPrecondition, copyError(NotConfigured))
}

// providerError maps one provider failure onto a Connect code. The provider's
// message is already redacted and bounded by internal/billing/stripe; a
// not-found is the only shape the console reacts to differently, so everything
// else is Unavailable with fixed copy — an operator reads the reason on the
// Settings page, where it belongs.
func providerError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, provider.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, errors.New("the billing provider has no such object"))
	case errors.Is(err, context.Canceled):
		return connect.NewError(connect.CodeCanceled, err)
	case errors.Is(err, context.DeadlineExceeded):
		return connect.NewError(connect.CodeDeadlineExceeded, copyError(Unreachable))
	default:
		return connect.NewError(connect.CodeUnavailable, copyError(Unreachable))
	}
}

// internalError collapses a store or zone-store failure onto the same fixed
// copy every other read in service.go answers with. A bbolt error can carry the
// database path and a zone-store error its own internals, so the text stays in
// the server log and the caller is told only that the read failed.
func (s *Service) internalError(operation string, err error) error {
	s.deps.Log().Error(operation, "error", reason(err))
	return connect.NewError(connect.CodeInternal, errors.New("internal server error"))
}

// errorType is the bounded telemetry attribute for one failed engine call.
func errorType(err error) string {
	switch {
	case err == nil:
		return "none"
	case errors.Is(err, provider.ErrNotFound):
		return "not_found"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	default:
		return "unavailable"
	}
}

// reason bounds and redacts text taken from the provider before it is shown on
// the Settings page or stored on the engine status.
func reason(err error) string {
	if err == nil {
		return ""
	}
	text := secretguard.Redact(err.Error())
	if len(text) > maxReason {
		return text[:maxReason] + "…"
	}
	return text
}

// observe records one engine call. It is the only place billing touches the
// engine instruments, so every provider call is counted the same way.
func (s *Service) observe(ctx context.Context, operation string, started time.Time, err error) {
	outcome := "success"
	if err != nil {
		outcome = "error"
	}
	s.deps.Meter().EngineRequest(ctx, "billing", operation, outcome, errorType(err), time.Since(started))
}
