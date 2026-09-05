package mail

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	"github.com/castlemilk/dns/internal/mail/stalwart"
	"github.com/castlemilk/dns/internal/secretguard"
	"github.com/castlemilk/dns/internal/zone"
)

// NotConfigured is the exact sentence an operator sees when the mail block is
// absent. It names the variables and never a value.
const NotConfigured = "Mail isn't configured on the control API. Set MAIL_API_URL, MAIL_API_TOKEN and MAIL_HOSTNAME."

// DeliveryStatsNote is the honest answer to "where are the delivery numbers?".
// Community Stalwart exposes no per-domain history — x:Metric/query answers
// forbidden and the Prometheus counters are global — so no per-domain figure is
// produced anywhere in this facade.
const DeliveryStatsNote = "Delivery counts aren't available: this mail server edition doesn't expose per-domain history. The queue shows what's in flight right now."

// DeliveryStatsWebhookNote replaces it once MAIL_WEBHOOK_SECRET is set and the
// mail server has been observed posting to this control plane's receiver. The
// counts are then the server's own delivery events, counted here — so the note
// says where they come from, when they started and, because the pair reads as a
// sending record, which direction they describe.
const DeliveryStatsWebhookNote = "Delivery counts are the mail these domains sent: the mail server's own delivery events, counted here since this control plane last started. Mail received isn't counted — the mail server refuses a message to an address that doesn't exist before it is queued, so an inbound figure would count the successes and none of the failures."

// DeliveryStatsPendingNote is what an operator gets while MAIL_WEBHOOK_SECRET is
// set here but no delivery has ever been accepted.
//
// The wiring has two halves and this control plane can only see its own. A hook
// that was never created, one created without the restart that loads it, a URL
// the mail server cannot reach and a server that simply has not handled any mail
// yet are indistinguishable from here — so the note names what is missing
// instead of letting a zero stand in for a day of observation.
const DeliveryStatsPendingNote = "Delivery counts aren't available yet: MAIL_WEBHOOK_SECRET is set here, but the mail server hasn't posted any delivery events to this control plane since it started. Check that the webhook on the mail server points at this control plane's /mail/v1/delivery-events, and that the server was restarted after the webhook was created or changed — a webhook only takes effect on reload."

// deliveryRejectedNote is what an operator gets when deliveries are arriving at
// the receiver and being refused. It does not claim the caller is the mail
// server — this control plane cannot know that — but it names the reason and
// what to check, because a mismatched bearer or signature key looks exactly like
// a silent zero from the console.
func deliveryRejectedNote(reason string, count uint64) string {
	deliveries := "deliveries"
	if count == 1 {
		deliveries = "delivery"
	}
	return fmt.Sprintf(
		"Delivery counts aren't available: MAIL_WEBHOOK_SECRET is set here, but %d %s to this control plane's delivery-event receiver %s been rejected — %s. %s",
		count, deliveries, map[bool]string{true: "has", false: "have"}[count == 1],
		deliveryRejectReason(reason), deliveryRejectRemedy(reason))
}

// deliveryRejectReason turns one of the receiver's bounded reason words into a
// sentence fragment. An unknown word is passed through as itself: the words come
// from this package, never from a caller.
func deliveryRejectReason(reason string) string {
	switch reason {
	case "unauthorized":
		return "the bearer token didn't match"
	case "bad_signature":
		return "the signature didn't verify"
	case "missing_signature":
		return "the delivery carried no X-Signature, and this deployment configures a signature key of its own"
	case "oversize":
		return "the body was larger than this receiver accepts"
	case "malformed", "unreadable":
		return "the body wasn't the delivery-event JSON this receiver expects"
	case "origin_rejected":
		return "the request carried a browser Origin header"
	default:
		return reason
	}
}

// deliveryRejectRemedy names the setting to check for the reasons a
// misconfiguration produces, and says nothing for the ones it does not.
func deliveryRejectRemedy(reason string) string {
	switch reason {
	case "unauthorized":
		return "MAIL_WEBHOOK_SECRET must be the webhook's bearer token on the mail server, and the server restarted after it changed."
	case "bad_signature":
		return "The webhook's signature key on the mail server must be MAIL_WEBHOOK_SIGNATURE_KEY, or MAIL_WEBHOOK_SECRET where this deployment sets no separate key, and the server restarted after it changed."
	case "missing_signature":
		return "Set the webhook's signature key on the mail server to MAIL_WEBHOOK_SIGNATURE_KEY, or unset that variable here so the bearer is the only credential."
	default:
		return "Nothing has been counted from those deliveries."
	}
}

// Operator-facing sentences for the engine failures §5.7 enumerates. They are
// constants because they are UI copy: the engine's own text is never shown
// verbatim except for the one field-level case below.
const (
	msgUnauthorized  = "The mail engine rejected the control API key (MAIL_API_TOKEN)."
	msgEdition       = "Not available on this mail server edition."
	msgLinked        = "Delete the mailboxes and forwarders first."
	msgObjectMissing = "The mail engine no longer has this object; the binding will be repaired on the next reconcile."
	msgUnavailable   = "The mail engine did not answer."
	msgInternal      = "internal server error"
)

// copyError carries an operator-facing sentence verbatim. It is a type rather
// than errors.New because the sentences end in a full stop: they are copy, not
// wrapped error strings.
type copyError string

func (e copyError) Error() string { return string(e) }

func notConfigured() error {
	return connect.NewError(connect.CodeFailedPrecondition, copyError(NotConfigured))
}

func invalidArgument(field, message string) error {
	return connect.NewError(connect.CodeInvalidArgument, &zone.ValidationError{Field: field, Message: message})
}

func failedPrecondition(message string) error {
	return connect.NewError(connect.CodeFailedPrecondition, copyError(message))
}

// engineError maps one engine failure onto the Connect code and the copy §5.7
// prescribes. It never leaks the engine's message except for the field-level
// invalid-argument case, and that text is redacted first.
//
// unsupportedFilter is deliberately Internal: every filter this facade sends is
// listed in the server's own schema, so seeing one is a bug here, not something
// an operator can act on.
func engineError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, context.Canceled):
		return connect.NewError(connect.CodeCanceled, copyError(msgUnavailable))
	case errors.Is(err, context.DeadlineExceeded):
		return connect.NewError(connect.CodeUnavailable, copyError(msgUnavailable))
	case errors.Is(err, stalwart.ErrUnauthorized):
		return connect.NewError(connect.CodeUnavailable, copyError(msgUnauthorized))
	case stalwart.IsForbidden(err):
		return connect.NewError(connect.CodeUnimplemented, copyError(msgEdition))
	case isUnsupportedFilter(err):
		return connect.NewError(connect.CodeInternal, copyError(msgInternal))
	case stalwart.IsLinked(err):
		return connect.NewError(connect.CodeFailedPrecondition, copyError(msgLinked))
	case stalwart.IsInvalid(err):
		return connect.NewError(connect.CodeInvalidArgument, &zone.ValidationError{
			Field:   invalidField(err),
			Message: secretguard.Redact(invalidMessage(err)),
		})
	case stalwart.IsMissing(err):
		return connect.NewError(connect.CodeNotFound, copyError(msgObjectMissing))
	case errors.Is(err, stalwart.ErrTransport):
		return connect.NewError(connect.CodeUnavailable, copyError(msgUnavailable))
	default:
		return connect.NewError(connect.CodeUnavailable, copyError(msgUnavailable))
	}
}

// duplicateAddressError is the AlreadyExists mapping. The address is the
// caller's own input, so naming it is safe and is what the operator needs.
func duplicateAddressError(address string) error {
	return connect.NewError(connect.CodeAlreadyExists, copyError(address+" already exists"))
}

// engineErrorFor maps an engine failure, upgrading a duplicate-address refusal
// to the AlreadyExists sentence for the address the caller asked for.
func engineErrorFor(address string, err error) error {
	if stalwart.IsDuplicate(err) {
		return duplicateAddressError(address)
	}
	return engineError(err)
}

func isUnsupportedFilter(err error) bool {
	kind, ok := stalwart.MethodErrorType(err)
	return ok && kind == stalwart.ErrorTypeUnsupportedFilter
}

func invalidField(err error) string {
	if setErr, ok := stalwart.SetErrorOf(err); ok {
		if property := setErr.Property(); property != "" {
			return property
		}
	}
	return "request"
}

func invalidMessage(err error) string {
	if setErr, ok := stalwart.SetErrorOf(err); ok && setErr.Description != "" {
		return setErr.Description
	}
	var methodErr *stalwart.MethodError
	if errors.As(err, &methodErr) && methodErr.Description != "" {
		return methodErr.Description
	}
	return "the mail engine rejected this value"
}

// reasonOf renders one engine failure as the short, redacted sentence a status
// row or a degraded row carries.
func reasonOf(err error) string {
	if err == nil {
		return ""
	}
	var connectErr *connect.Error
	if errors.As(err, &connectErr) {
		return secretguard.Redact(connectErr.Message())
	}
	text := secretguard.Redact(err.Error())
	if len(text) > 200 {
		text = strings.TrimSpace(text[:200]) + "…"
	}
	return text
}

// engineOperationOutcome renders a bounded telemetry outcome and error class
// for one engine call.
func engineOperationOutcome(err error) (string, string) {
	if err == nil {
		return "success", "none"
	}
	var connectErr *connect.Error
	if errors.As(engineError(err), &connectErr) {
		return "error", connectErr.Code().String()
	}
	return "error", "unknown"
}

// hostnameMismatch is the refusal that keeps a renamed or replaced mail server
// from silently taking over a customer's mail routing.
func hostnameMismatch(configured, actual string) string {
	return fmt.Sprintf("MAIL_HOSTNAME (%s) differs from the mail server's hostname (%s)", configured, actual)
}
