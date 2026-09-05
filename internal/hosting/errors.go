package hosting

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	"github.com/castlemilk/dns/internal/secretguard"
)

// NotConfigured is the exact sentence an operator sees when the hosting block
// is absent. It names the variables and never a value.
const NotConfigured = "Website hosting isn't configured on the control API. Set HOSTING_API_URL, HOSTING_API_TOKEN and HOSTING_GATEWAY_HOSTNAME (or HOSTING_GATEWAY_ADDRESSES)."

// Operator-facing copy for every engine failure the facade maps (spec2 §4.5).
// The engine's own text is never surfaced for these cases: it names another
// tenant's app, or a credential variable's state.
const (
	CopyCredentialsRejected = "The hosting engine rejected the control API's credentials (HOSTING_API_TOKEN)."
	CopyTenantScope         = "The hosting token is not scoped to tenant %s."
	CopyHostTaken           = "%s is already served by another site on the hosting platform."
	CopyAppNameTaken        = "This site's app name is taken on the hosting platform — contact the operator."
	CopyAppGone             = "The hosting engine no longer knows this site — detach and attach again."
	CopyUploadTooLarge      = "The upload exceeds the hosting engine's 512 MiB limit."
	CopyUnreachable         = "The hosting engine did not answer."
	CopyGatewayUnknown      = "gateway addresses unknown — set HOSTING_GATEWAY_HOSTNAME or HOSTING_GATEWAY_ADDRESSES"
)

// engineError is the facade's public error for an engine failure. It carries
// only copy this package wrote, never an engine response body.
type engineError string

func (e engineError) Error() string { return string(e) }

// notConfigured is the refusal every mutating RPC returns when hosting has no
// configuration.
func notConfigured() error {
	return connect.NewError(connect.CodeFailedPrecondition, engineError(NotConfigured))
}

func invalidArgument(field, message string) error {
	return connect.NewError(connect.CodeInvalidArgument, engineError(field+": "+message))
}

func failedPrecondition(message string) error {
	return connect.NewError(connect.CodeFailedPrecondition, engineError(message))
}

func notFound(message string) error {
	return connect.NewError(connect.CodeNotFound, engineError(message))
}

// mapEngineError turns an engine failure into the code and copy an operator
// sees. operation names what the facade was doing, so a caller can attach it to
// an event without leaking the engine's text.
//
// Unauthenticated and Unavailable both mean "the control API's credentials did
// not work or the engine is down": DeepHost's auth middleware answers 401/503
// at the HTTP layer, which Connect surfaces as those two codes.
func (s *Service) mapEngineError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return connect.NewError(connect.CodeCanceled, engineError(CopyUnreachable))
	}
	if errors.Is(err, ErrUnsupported) {
		return connect.NewError(connect.CodeFailedPrecondition,
			engineError("This hosting engine version does not support that operation."))
	}
	switch connect.CodeOf(err) {
	case connect.CodeUnauthenticated, connect.CodeUnavailable:
		return connect.NewError(connect.CodeUnavailable, engineError(CopyCredentialsRejected))
	case connect.CodePermissionDenied:
		return connect.NewError(connect.CodeUnavailable,
			engineError(fmt.Sprintf(CopyTenantScope, s.cfg.Tenant)))
	case connect.CodeInvalidArgument:
		return connect.NewError(connect.CodeInvalidArgument, engineError(redactEngineText(err)))
	case connect.CodeAlreadyExists:
		return connect.NewError(connect.CodeFailedPrecondition, engineError(CopyAppNameTaken))
	case connect.CodeNotFound:
		return connect.NewError(connect.CodeFailedPrecondition, engineError(CopyAppGone))
	case connect.CodeResourceExhausted:
		return connect.NewError(connect.CodeResourceExhausted, engineError(CopyUploadTooLarge))
	case connect.CodeDeadlineExceeded, connect.CodeCanceled:
		return connect.NewError(connect.CodeUnavailable, engineError(CopyUnreachable))
	default:
		return connect.NewError(connect.CodeUnavailable, engineError(CopyUnreachable))
	}
}

// isHostTaken reports whether an AlreadyExists came from CreateDomain finding
// the host on another app, as opposed to an idempotent re-registration. It is
// the one engine message this facade matches on: the text that follows it names
// another tenant's app, so it is never shown — CopyHostTaken is shown instead.
func isHostTaken(err error) bool {
	if err == nil {
		return false
	}
	if connect.CodeOf(err) != connect.CodeAlreadyExists {
		return false
	}
	return strings.Contains(err.Error(), "belongs to app")
}

func isNotFound(err error) bool {
	return err != nil && connect.CodeOf(err) == connect.CodeNotFound
}

func isUnsupported(err error) bool { return err != nil && errors.Is(err, ErrUnsupported) }

// reason bounds and redacts free text that came from the engine before it is
// stored in a document or an event.
func reason(text string) string {
	text = secretguard.Redact(strings.TrimSpace(text))
	text = strings.Map(func(char rune) rune {
		if char == '\n' || char == '\t' {
			return ' '
		}
		if char < 0x20 || char == 0x7f {
			return -1
		}
		return char
	}, text)
	runes := []rune(text)
	if len(runes) > 300 {
		return string(runes[:300])
	}
	return string(runes)
}

// redactEngineText is what an InvalidArgument from the engine may show: its own
// text, redacted and bounded. InvalidArgument is the one code whose message is
// about the caller's input rather than the platform's internals.
func redactEngineText(err error) string {
	var connectErr *connect.Error
	if errors.As(err, &connectErr) {
		return reason(connectErr.Message())
	}
	return reason(err.Error())
}
