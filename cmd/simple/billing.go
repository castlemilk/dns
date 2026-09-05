package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"

	"connectrpc.com/connect"

	billingv1 "github.com/castlemilk/dns/gen/go/billing/v1"
	"github.com/castlemilk/dns/internal/simplecli/auth"
)

func cmdBilling(ctx context.Context, e *env, o *options, args []string) error {
	if len(args) == 0 {
		return &usageError{message: "billing: choose a command", usage: billingUsage}
	}
	switch args[0] {
	case "status":
		return cmdBillingStatus(ctx, e, o, args[1:])
	case "summary":
		return cmdBillingSummary(ctx, e, o, args[1:])
	case "subscribe":
		return cmdBillingSubscribe(ctx, e, o, args[1:])
	case "confirm":
		return cmdBillingConfirm(ctx, e, o, args[1:])
	case "portal":
		return cmdBillingPortal(ctx, e, o, args[1:])
	case "invoices":
		return cmdBillingInvoices(ctx, e, o, args[1:])
	case "retry-webhooks":
		return cmdBillingRetryWebhooks(ctx, e, o, args[1:])
	case "help", "-h", "--help":
		return &helpError{usage: billingUsage}
	default:
		return &usageError{message: fmt.Sprintf("unknown billing command %q", args[0]), usage: billingUsage}
	}
}

func cmdBillingStatus(ctx context.Context, e *env, o *options, args []string) error {
	fs, err := o.parse("simple billing status", billingUsage, args, nil)
	if err != nil {
		return err
	}
	if err := noExtraArgs(fs, 0, billingUsage); err != nil {
		return err
	}
	clients, err := o.connect(e)
	if err != nil {
		return err
	}
	callCtx, cancel := o.call(ctx)
	defer cancel()
	response, err := clients.Billing.GetBillingStatus(callCtx,
		connect.NewRequest(&billingv1.GetBillingStatusRequest{}))
	if err != nil {
		return fail(clients, err)
	}
	status := response.Msg
	return newPrinter(e, o).emit(status, func(w io.Writer) error {
		return fields(w,
			pair("engine", engineDetail(status.GetEngine())),
			pair("provider", orDash(status.GetProvider())),
			pair("live mode", yesNo(status.GetLivemode())),
			pair("price", orDash(status.GetPrice().GetLabel())),
			pair("informational only", yesNo(status.GetInformationalOnly())),
			pair("webhook", webhookSummary(status)),
			pair("dead webhooks", formatUint(status.GetDeadWebhooks())),
			pair("reconciled", formatTime(status.GetReconciledAt())),
			pair("note", orDash(status.GetPolicyNote())),
		)
	})
}

func webhookSummary(status *billingv1.GetBillingStatusResponse) string {
	if !status.GetWebhookConfigured() {
		return "not configured, so subscription changes are only seen when a command asks"
	}
	return fmt.Sprintf("configured with %d secret(s), last event %s, %d pending",
		status.GetWebhookSecrets(), formatTime(status.GetLastWebhookAt()), status.GetPendingWebhooks())
}

func cmdBillingSummary(ctx context.Context, e *env, o *options, args []string) error {
	fs, err := o.parse("simple billing summary", billingUsage, args, nil)
	if err != nil {
		return err
	}
	if err := noExtraArgs(fs, 0, billingUsage); err != nil {
		return err
	}
	clients, err := o.connect(e)
	if err != nil {
		return err
	}
	callCtx, cancel := o.call(ctx)
	defer cancel()
	response, err := clients.Billing.GetBillingSummary(callCtx,
		connect.NewRequest(&billingv1.GetBillingSummaryRequest{}))
	if err != nil {
		return fail(clients, err)
	}
	summary := response.Msg
	return newPrinter(e, o).emit(summary, func(w io.Writer) error {
		if err := fields(w,
			pair("customer", yesNo(summary.GetCustomerExists())),
			pair("payment method", paymentMethodSummary(summary.GetPaymentMethod())),
			pair("active", formatUint(summary.GetActiveCount())),
			pair("needs attention", formatUint(summary.GetAttentionCount())),
		); err != nil {
			return err
		}
		if len(summary.GetDomains()) == 0 {
			return nil
		}
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
		table := newTable(w, "DOMAIN", "STATE", "PERIOD END", "CANCEL AT", "LAST INVOICE", "SUBSCRIPTION")
		for _, domain := range summary.GetDomains() {
			table.row(
				domain.GetZoneName(),
				label(domain.GetState().String(), "SUBSCRIPTION_STATE_"),
				formatTime(domain.GetCurrentPeriodEnd()),
				formatTime(domain.GetCancelAt()),
				orDash(domain.GetLastInvoiceStatus()),
				orDash(domain.GetSubscriptionId()),
			)
		}
		return table.flush()
	})
}

func paymentMethodSummary(method *billingv1.PaymentMethod) string {
	if method == nil || method.GetLast4() == "" {
		return "unknown"
	}
	return fmt.Sprintf("%s ending %s, expires %02d/%d",
		method.GetBrand(), method.GetLast4(), method.GetExpMonth(), method.GetExpYear())
}

func cmdBillingSubscribe(ctx context.Context, e *env, o *options, args []string) error {
	var returnPath string
	var openBrowser bool
	fs, err := o.parse("simple billing subscribe", billingUsage, args, func(fs *flag.FlagSet) {
		fs.StringVar(&returnPath, "return-path", "", "console path to come back to (default /billing)")
		fs.BoolVar(&openBrowser, "open", false, "open the checkout page in a browser")
	})
	if err != nil {
		return err
	}
	reference, err := argument(fs, 0, "domain", billingUsage)
	if err != nil {
		return err
	}
	if err := noExtraArgs(fs, 1, billingUsage); err != nil {
		return err
	}
	clients, err := o.connect(e)
	if err != nil {
		return err
	}
	zone, err := lookupZoneWithTimeout(ctx, o, clients, reference)
	if err != nil {
		return err
	}
	callCtx, cancel := o.call(ctx)
	defer cancel()
	response, err := clients.Billing.CreateCheckoutSession(callCtx,
		connect.NewRequest(&billingv1.CreateCheckoutSessionRequest{
			ZoneId: zone.GetId(), ReturnPath: returnPath,
		}))
	if err != nil {
		return fail(clients, err)
	}
	result := response.Msg
	if err := newPrinter(e, o).emit(result, func(w io.Writer) error {
		return fields(w,
			pair("domain", zone.GetName()),
			pair("checkout", result.GetUrl()),
			pair("session", result.GetSessionId()),
			pair("expires", formatTime(result.GetExpiresAt())),
			pair("then run", "simple billing confirm "+result.GetSessionId()),
		)
	}); err != nil {
		return err
	}
	if openBrowser {
		return openURL(e, result.GetUrl())
	}
	return nil
}

func cmdBillingConfirm(ctx context.Context, e *env, o *options, args []string) error {
	fs, err := o.parse("simple billing confirm", billingUsage, args, nil)
	if err != nil {
		return err
	}
	sessionID, err := argument(fs, 0, "session_id", billingUsage)
	if err != nil {
		return err
	}
	if err := noExtraArgs(fs, 1, billingUsage); err != nil {
		return err
	}
	clients, err := o.connect(e)
	if err != nil {
		return err
	}
	callCtx, cancel := o.call(ctx)
	defer cancel()
	response, err := clients.Billing.ConfirmCheckout(callCtx,
		connect.NewRequest(&billingv1.ConfirmCheckoutRequest{SessionId: sessionID}))
	if err != nil {
		return fail(clients, err)
	}
	result := response.Msg
	return newPrinter(e, o).emit(result, func(w io.Writer) error {
		return fields(w,
			pair("domain", orDash(result.GetDomain().GetZoneName())),
			pair("state", label(result.GetDomain().GetState().String(), "SUBSCRIPTION_STATE_")),
			pair("completed", yesNo(result.GetCompleted())),
			pair("payment required", yesNo(result.GetPaymentRequired())),
			pair("provider", orDash(result.GetProvider())),
		)
	})
}

func cmdBillingPortal(ctx context.Context, e *env, o *options, args []string) error {
	var flow, domain, returnPath string
	var openBrowser bool
	fs, err := o.parse("simple billing portal", billingUsage, args, func(fs *flag.FlagSet) {
		fs.StringVar(&flow, "flow", "", "payment_method_update or subscription_cancel")
		fs.StringVar(&domain, "domain", "", "the zone a subscription_cancel flow is about")
		fs.StringVar(&returnPath, "return-path", "", "console path to come back to")
		fs.BoolVar(&openBrowser, "open", false, "open the portal in a browser")
	})
	if err != nil {
		return err
	}
	if err := noExtraArgs(fs, 0, billingUsage); err != nil {
		return err
	}
	clients, err := o.connect(e)
	if err != nil {
		return err
	}
	request := &billingv1.CreatePortalSessionRequest{Flow: flow, ReturnPath: returnPath}
	if strings.TrimSpace(domain) != "" {
		zone, err := lookupZoneWithTimeout(ctx, o, clients, domain)
		if err != nil {
			return err
		}
		request.ZoneId = zone.GetId()
	}
	callCtx, cancel := o.call(ctx)
	defer cancel()
	response, err := clients.Billing.CreatePortalSession(callCtx, connect.NewRequest(request))
	if err != nil {
		return fail(clients, err)
	}
	if err := newPrinter(e, o).emit(response.Msg, func(w io.Writer) error {
		return fields(w, pair("portal", response.Msg.GetUrl()))
	}); err != nil {
		return err
	}
	if openBrowser {
		return openURL(e, response.Msg.GetUrl())
	}
	return nil
}

func cmdBillingInvoices(ctx context.Context, e *env, o *options, args []string) error {
	var limit uint
	var cursor string
	fs, err := o.parse("simple billing invoices", billingUsage, args, func(fs *flag.FlagSet) {
		fs.UintVar(&limit, "limit", 0, "how many invoices (default 25, max 100)")
		fs.StringVar(&cursor, "cursor", "", "continue after this invoice id")
	})
	if err != nil {
		return err
	}
	if err := noExtraArgs(fs, 0, billingUsage); err != nil {
		return err
	}
	clients, err := o.connect(e)
	if err != nil {
		return err
	}
	callCtx, cancel := o.call(ctx)
	defer cancel()
	response, err := clients.Billing.ListInvoices(callCtx, connect.NewRequest(&billingv1.ListInvoicesRequest{
		Limit: uint32(limit), Cursor: cursor, //nolint:gosec // bounded server-side
	}))
	if err != nil {
		return fail(clients, err)
	}
	result := response.Msg
	return newPrinter(e, o).emit(result, func(w io.Writer) error {
		if !result.GetLive() {
			// An empty list from an unreachable provider is not "no invoices".
			return writeLine(w, "the billing provider could not be reached, so no invoices could be listed")
		}
		if len(result.GetInvoices()) == 0 {
			return writeLine(w, "no invoices")
		}
		table := newTable(w, "NUMBER", "DOMAIN", "CREATED", "TOTAL", "STATUS", "INVOICE ID")
		for _, invoice := range result.GetInvoices() {
			table.row(
				orDash(invoice.GetNumber()),
				orDash(invoice.GetZoneName()),
				formatTime(invoice.GetCreatedAt()),
				formatMoney(invoice.GetTotal(), invoice.GetCurrency()),
				orDash(invoice.GetStatus()),
				invoice.GetId(),
			)
		}
		if err := table.flush(); err != nil {
			return err
		}
		if next := result.GetNextCursor(); next != "" {
			return writeLine(w, "\nmore: --cursor "+next)
		}
		return nil
	})
}

// formatMoney renders minor units as the amount a human recognises, without
// pretending to know every currency's exponent beyond the usual two.
func formatMoney(minor int64, currency string) string {
	currency = strings.ToUpper(strings.TrimSpace(currency))
	if currency == "" {
		return strconv.FormatInt(minor, 10)
	}
	return fmt.Sprintf("%.2f %s", float64(minor)/100, currency)
}

func cmdBillingRetryWebhooks(ctx context.Context, e *env, o *options, args []string) error {
	fs, err := o.parse("simple billing retry-webhooks", billingUsage, args, nil)
	if err != nil {
		return err
	}
	if err := noExtraArgs(fs, 0, billingUsage); err != nil {
		return err
	}
	clients, err := o.connect(e)
	if err != nil {
		return err
	}
	callCtx, cancel := o.call(ctx)
	defer cancel()
	response, err := clients.Billing.RetryDeadWebhooks(callCtx,
		connect.NewRequest(&billingv1.RetryDeadWebhooksRequest{}))
	if err != nil {
		return fail(clients, err)
	}
	return newPrinter(e, o).emit(response.Msg, func(w io.Writer) error {
		return fields(w, pair("re-queued", formatUint(response.Msg.GetRequeued())))
	})
}

// openURL opens a page the control plane returned. A browser that will not open
// is reported rather than hidden, because the URL has already been printed and
// the operator can follow it themselves.
func openURL(e *env, target string) error {
	open := e.openBrowser
	if open == nil {
		open = auth.OpenBrowser
	}
	if err := open(target); err != nil {
		return writeLine(e.stderr, "could not open a browser; follow the URL above instead")
	}
	return nil
}
