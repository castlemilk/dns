package stripe

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/castlemilk/dns/internal/billing/provider"
	"github.com/castlemilk/dns/internal/secretguard"
	stripego "github.com/stripe/stripe-go/v86"
)

// maxErrorText bounds the operator-facing text taken from an API error.
const maxErrorText = 200

// mapError turns a stripe-go error into something the facade can show and log.
// The message is redacted and bounded; the status code decides whether the
// facade reports "not found" or a generic failure.
func mapError(err error) error {
	if err == nil {
		return nil
	}
	var apiErr *stripego.Error
	if errors.As(err, &apiErr) {
		if apiErr.HTTPStatusCode == http.StatusNotFound {
			return fmt.Errorf("%w: %s", provider.ErrNotFound, bound(apiErr.Msg))
		}
		message := apiErr.Msg
		if message == "" {
			message = string(apiErr.Type)
		}
		if message == "" {
			message = "the billing provider rejected the request"
		}
		return fmt.Errorf("billing provider error (%d): %s", apiErr.HTTPStatusCode, bound(message))
	}
	return errors.New(bound(err.Error()))
}

func bound(text string) string {
	text = secretguard.Redact(text)
	if len(text) > maxErrorText {
		return text[:maxErrorText] + "…"
	}
	return text
}

func priceView(price *stripego.Price) provider.Price {
	if price == nil {
		return provider.Price{}
	}
	view := provider.Price{
		ID:         price.ID,
		UnitAmount: price.UnitAmount,
		Currency:   string(price.Currency),
	}
	if price.Recurring != nil {
		view.Interval = string(price.Recurring.Interval)
	}
	return view
}

func checkoutView(session *stripego.CheckoutSession) provider.CheckoutSession {
	if session == nil {
		return provider.CheckoutSession{}
	}
	view := provider.CheckoutSession{
		ID:                session.ID,
		URL:               session.URL,
		Status:            string(session.Status),
		PaymentStatus:     string(session.PaymentStatus),
		ClientReferenceID: session.ClientReferenceID,
		Metadata:          session.Metadata,
		Created:           seconds(session.Created),
		ExpiresAt:         seconds(session.ExpiresAt),
	}
	if session.Customer != nil {
		view.CustomerID = session.Customer.ID
	}
	if session.Subscription != nil {
		view.SubscriptionID = session.Subscription.ID
	}
	return view
}

func subscriptionView(subscription *stripego.Subscription) provider.Subscription {
	if subscription == nil {
		return provider.Subscription{}
	}
	view := provider.Subscription{
		ID:                subscription.ID,
		Status:            string(subscription.Status),
		Created:           seconds(subscription.Created),
		CancelAt:          seconds(subscription.CancelAt),
		CanceledAt:        seconds(subscription.CanceledAt),
		CancelAtPeriodEnd: subscription.CancelAtPeriodEnd,
		Metadata:          subscription.Metadata,
	}
	if subscription.Customer != nil {
		view.CustomerID = subscription.Customer.ID
	}
	// Since basil the period lives on the subscription's items, not on the
	// subscription itself.
	if subscription.Items != nil && len(subscription.Items.Data) > 0 && subscription.Items.Data[0] != nil {
		view.CurrentPeriodEnd = seconds(subscription.Items.Data[0].CurrentPeriodEnd)
	}
	view.PaymentMethod = cardView(subscription.DefaultPaymentMethod)
	return view
}

// customerView is the customer as the facade sees it, including the card the
// customer would be charged on when it was expanded.
func customerView(customer *stripego.Customer) provider.Customer {
	if customer == nil {
		return provider.Customer{}
	}
	view := provider.Customer{ID: customer.ID, Email: customer.Email}
	if settings := customer.InvoiceSettings; settings != nil {
		view.PaymentMethod = cardView(settings.DefaultPaymentMethod)
	}
	return view
}

// cardView is the display summary of a payment method, or nil when there is no
// card — a payment method that was not expanded carries no card either, and the
// facade must not read "no card" out of "not expanded", so every caller expands.
func cardView(method *stripego.PaymentMethod) *provider.PaymentMethod {
	if method == nil || method.Card == nil {
		return nil
	}
	return &provider.PaymentMethod{
		Brand:    string(method.Card.Brand),
		Last4:    method.Card.Last4,
		ExpMonth: int(method.Card.ExpMonth),
		ExpYear:  int(method.Card.ExpYear),
	}
}

func invoiceView(invoice *stripego.Invoice) provider.Invoice {
	if invoice == nil {
		return provider.Invoice{}
	}
	view := provider.Invoice{
		ID:        invoice.ID,
		Number:    invoice.Number,
		Created:   seconds(invoice.Created),
		Total:     invoice.Total,
		Currency:  string(invoice.Currency),
		Status:    string(invoice.Status),
		HostedURL: invoice.HostedInvoiceURL,
		PDFURL:    invoice.InvoicePDF,
	}
	if invoice.Parent != nil && invoice.Parent.SubscriptionDetails != nil {
		view.ZoneName = invoice.Parent.SubscriptionDetails.Metadata["domain"]
	}
	return view
}

func seconds(value int64) time.Time {
	if value <= 0 {
		return time.Time{}
	}
	return time.Unix(value, 0).UTC()
}
