package billing

import (
	"github.com/castlemilk/dns/internal/billing/provider"
)

// The billing provider contract. spec2 §6.1 writes these names as
// billing.Provider, billing.Price and so on; they are declared in the leaf
// package internal/billing/provider and aliased here, because this package
// constructs the Stripe implementation and so must import it — which makes the
// reverse import (implementation → facade) a cycle. Aliases, not wrappers, so
// the two names are the same type everywhere.
type (
	// Provider is everything the facade needs from Stripe.
	Provider = provider.Provider
	// Price is one recurring price, in minor units.
	Price = provider.Price
	// CheckoutInput is one hosted Checkout session request.
	CheckoutInput = provider.CheckoutInput
	// CheckoutSession is the provider's view of a Checkout session.
	CheckoutSession = provider.CheckoutSession
	// Subscription is the provider's view of one domain's subscription.
	Subscription = provider.Subscription
	// PaymentMethod is the card summary shown on the Billing page.
	PaymentMethod = provider.PaymentMethod
	// PortalInput selects a Billing Portal session.
	PortalInput = provider.PortalInput
	// Invoice is one row of the invoices table.
	Invoice = provider.Invoice
	// Event is one verified webhook delivery.
	Event = provider.Event
	// InvoiceEvent is what an invoice.* event tells the state machine.
	InvoiceEvent = provider.InvoiceEvent
)

// ErrAPIVersionMismatch is returned when a signed event's api_version belongs
// to a different release train than this build's SDK.
var ErrAPIVersionMismatch = provider.ErrAPIVersionMismatch
