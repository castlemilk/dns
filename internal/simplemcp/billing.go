package simplemcp

import (
	"context"

	"connectrpc.com/connect"

	activityv1 "github.com/castlemilk/dns/gen/go/activity/v1"
	billingv1 "github.com/castlemilk/dns/gen/go/billing/v1"
)

// billingStatus answers with both halves of what `simple billing status`
// shows. The per-domain summary is a second call, so when only it fails the
// engine status is still reported, with the reason the rest is missing: a
// caller must never read an absent list as "no subscriptions".
func (s *Server) billingStatus(ctx context.Context, _ args) (any, error) {
	status, err := s.clients.Billing.GetBillingStatus(ctx, connect.NewRequest(&billingv1.GetBillingStatusRequest{}))
	if err != nil {
		return nil, err
	}
	answer, err := view(status.Msg)
	if err != nil {
		return nil, err
	}
	summary, summaryErr := s.clients.Billing.GetBillingSummary(ctx, connect.NewRequest(&billingv1.GetBillingSummaryRequest{}))
	if summaryErr != nil {
		answer["summary_available"] = false
		answer["summary_unavailable_reason"] = s.translate(summaryErr).Message
		return answer, nil
	}
	rendered, err := view(summary.Msg)
	if err != nil {
		return nil, err
	}
	answer["summary_available"] = true
	answer["summary"] = rendered
	return answer, nil
}

func (s *Server) billingSubscribe(ctx context.Context, a args) (any, error) {
	zone, err := s.resolveZone(ctx, a)
	if err != nil {
		return nil, err
	}
	returnPath, err := stringArg(a, "return_path", false)
	if err != nil {
		return nil, err
	}
	response, err := s.clients.Billing.CreateCheckoutSession(ctx, connect.NewRequest(&billingv1.CreateCheckoutSessionRequest{
		ZoneId: zone.ID, ReturnPath: returnPath,
	}))
	if err != nil {
		return nil, err
	}
	return viewWith(response.Msg, map[string]any{
		"zone_id": zone.ID,
		"domain":  zone.Name,
		"note":    "Give this URL to the person who is paying. It opens a checkout page that only they can complete, and it expires.",
	})
}

func (s *Server) billingPortal(ctx context.Context, a args) (any, error) {
	flow, err := stringArg(a, "flow", false)
	if err != nil {
		return nil, err
	}
	returnPath, err := stringArg(a, "return_path", false)
	if err != nil {
		return nil, err
	}
	zone, err := s.optionalZone(ctx, a)
	if err != nil {
		return nil, err
	}
	if flow == "subscription_cancel" && zone.ID == "" {
		return nil, invalid("domain", "required when flow is subscription_cancel")
	}
	response, err := s.clients.Billing.CreatePortalSession(ctx, connect.NewRequest(&billingv1.CreatePortalSessionRequest{
		Flow: flow, ZoneId: zone.ID, ReturnPath: returnPath,
	}))
	if err != nil {
		return nil, err
	}
	return viewWith(response.Msg, map[string]any{
		"note": "Give this URL to the customer. It is a session only they can use.",
	})
}

func (s *Server) billingInvoices(ctx context.Context, a args) (any, error) {
	limit, err := uint32Arg(a, "limit")
	if err != nil {
		return nil, err
	}
	cursor, err := stringArg(a, "cursor", false)
	if err != nil {
		return nil, err
	}
	response, err := s.clients.Billing.ListInvoices(ctx, connect.NewRequest(&billingv1.ListInvoicesRequest{
		Limit: limit, Cursor: cursor,
	}))
	if err != nil {
		return nil, err
	}
	// `live: false` means the provider could not be reached and the empty list
	// is not an answer about invoices. It stays on the wire; do not summarise
	// it away.
	return view(response.Msg)
}

func (s *Server) activityList(ctx context.Context, a args) (any, error) {
	zone, err := s.optionalZone(ctx, a)
	if err != nil {
		return nil, err
	}
	kinds, err := stringsArg(a, "kinds", false)
	if err != nil {
		return nil, err
	}
	since, err := timestampArg(a, "since")
	if err != nil {
		return nil, err
	}
	limit, err := uint32Arg(a, "limit")
	if err != nil {
		return nil, err
	}
	cursor, err := stringArg(a, "cursor", false)
	if err != nil {
		return nil, err
	}
	response, err := s.clients.Activity.ListEvents(ctx, connect.NewRequest(&activityv1.ListEventsRequest{
		ZoneId: zone.ID, Kinds: kinds, Since: since, Limit: limit, Cursor: cursor,
	}))
	if err != nil {
		return nil, err
	}
	return view(response.Msg)
}
