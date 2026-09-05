// Package provider declares the billing provider contract: the interface the
// facade calls and the neutral value types both sides speak, plus decoders for
// the JSON objects a webhook event carries.
//
// It exists as its own package purely to break an import cycle. spec2 §1.9 puts
// the interface in internal/billing and the implementation in
// internal/billing/stripe; since internal/billing constructs the provider it
// must import the stripe package, so the shared types cannot live in
// internal/billing. They live here, and internal/billing re-exports every name
// as an alias, so `billing.Provider`, `billing.Price` and friends read exactly
// as the spec writes them.
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// ErrAPIVersionMismatch is returned by ConstructEvent when a signed event's
// api_version belongs to a different release train than the SDK this binary was
// built against. It is a distinct error so the webhook route can answer 400 and
// count the outcome instead of silently dropping the delivery.
var ErrAPIVersionMismatch = errors.New("billing: webhook event API version is not compatible with this build")

// ErrNotFound is returned when the provider has no object with the requested
// id. The facade maps it onto connect.CodeNotFound.
var ErrNotFound = errors.New("billing: not found")

// Price is one recurring price. UnitAmount is in minor units.
type Price struct {
	ID         string
	UnitAmount int64
	Currency   string
	Interval   string
}

// CheckoutInput is everything one hosted Checkout session needs. The
// idempotency key makes a retried CreateCheckoutSession reuse Stripe's session
// rather than open a second one.
type CheckoutInput struct {
	CustomerID     string
	ZoneID         string
	ZoneName       string
	PriceID        string
	SuccessURL     string
	CancelURL      string
	IdempotencyKey string
	ExpiresAt      time.Time
}

// CheckoutSession is the provider's view of a hosted Checkout session.
type CheckoutSession struct {
	ID                string
	URL               string
	Status            string // open | complete | expired
	PaymentStatus     string // paid | unpaid | no_payment_required
	SubscriptionID    string
	CustomerID        string
	ClientReferenceID string
	Metadata          map[string]string
	Created           time.Time
	ExpiresAt         time.Time
}

// Subscription is the provider's view of one domain's subscription.
type Subscription struct {
	ID                string
	Status            string
	CustomerID        string
	Created           time.Time
	CurrentPeriodEnd  time.Time
	CancelAt          time.Time
	CanceledAt        time.Time
	CancelAtPeriodEnd bool
	Metadata          map[string]string
	PaymentMethod     *PaymentMethod
}

// PaymentMethod is the card summary shown on the Billing page. It never holds a
// PAN or any other card data beyond what Stripe returns for display.
type PaymentMethod struct {
	Brand    string
	Last4    string
	ExpMonth int
	ExpYear  int
}

// PortalInput selects a Billing Portal session, optionally deep-linked to one
// flow. SubscriptionID is required for the subscription_cancel flow.
type PortalInput struct {
	CustomerID     string
	ReturnURL      string
	Flow           string
	SubscriptionID string
}

// Invoice is one row of the Billing page's invoice table.
type Invoice struct {
	ID        string
	Number    string
	ZoneName  string
	Created   time.Time
	Total     int64
	Currency  string
	Status    string
	HostedURL string
	PDFURL    string
}

// Customer is the minimum the facade keeps about the billing customer.
// PaymentMethod is the customer's own default — the card Stripe would charge —
// so a nil value on a customer the provider actually answered for means "no
// card on file", not "did not look". That distinction is what lets the facade
// clear a card the customer has removed instead of showing it forever.
type Customer struct {
	ID            string
	Email         string
	PaymentMethod *PaymentMethod
}

// Event is one verified webhook delivery. Object is the raw JSON of
// data.object, decoded by the Decode* helpers below according to Type.
type Event struct {
	ID         string
	Type       string
	APIVersion string
	Created    time.Time
	Livemode   bool
	Object     json.RawMessage
	ObjectID   string
}

// Event type names this facade acts on.
const (
	EventCheckoutCompleted     = "checkout.session.completed"
	EventCheckoutExpired       = "checkout.session.expired"
	EventSubscriptionCreated   = "customer.subscription.created"
	EventSubscriptionUpdated   = "customer.subscription.updated"
	EventSubscriptionDeleted   = "customer.subscription.deleted"
	EventInvoicePaid           = "invoice.paid"
	EventInvoicePaymentFailed  = "invoice.payment_failed"
	EventCustomerUpdated       = "customer.updated"
	EventPaymentMethodAttached = "payment_method.attached"
	EventPaymentMethodDetached = "payment_method.detached"
)

// Provider is everything the billing facade needs from Stripe. One
// implementation exists (internal/billing/stripe); the "fake" provider is that
// same implementation pointed at the in-process fake API, so dev and CI
// exercise the production request shaping and the real signature verification.
type Provider interface {
	Name() string
	Livemode() bool
	GetPrice(ctx context.Context, id string) (Price, error)
	FindCustomer(ctx context.Context, email string) (customerID string, found bool, err error)
	// GetCustomer reads one customer, including the card it would be charged
	// on. It is the authority for "what is on file": a subscription's default
	// payment method only says what that subscription would be charged on.
	GetCustomer(ctx context.Context, id string) (Customer, error)
	EnsureCustomer(ctx context.Context, email, idempotencyKey string) (customerID string, err error)
	CreateCheckout(ctx context.Context, in CheckoutInput) (CheckoutSession, error)
	GetCheckout(ctx context.Context, sessionID string) (CheckoutSession, error)
	GetSubscription(ctx context.Context, id string) (Subscription, error)
	ListSubscriptions(ctx context.Context, customerID string) ([]Subscription, error)
	CreatePortal(ctx context.Context, in PortalInput) (url string, err error)
	ListInvoices(ctx context.Context, customerID, startingAfter string, limit int) ([]Invoice, string, error)
	ConstructEvent(payload []byte, signatureHeader string) (event Event, secretIndex int, err error)
}

// --- wire decoding --------------------------------------------------------
//
// A webhook carries the object as Stripe rendered it, not as the SDK's typed
// structs (whose expandable fields change shape between an expanded and an
// unexpanded response). Decoding here keeps the facade's state machine working
// on plain values and keeps every shape assumption in one file.

// InvoiceEvent is what an invoice.* event tells the state machine.
type InvoiceEvent struct {
	ID             string
	Status         string
	SubscriptionID string
	CustomerID     string
	Metadata       map[string]string
	PeriodEnd      time.Time
}

type wireCheckoutSession struct {
	ID                string            `json:"id"`
	URL               string            `json:"url"`
	Status            string            `json:"status"`
	PaymentStatus     string            `json:"payment_status"`
	ClientReferenceID string            `json:"client_reference_id"`
	Customer          json.RawMessage   `json:"customer"`
	Subscription      json.RawMessage   `json:"subscription"`
	Metadata          map[string]string `json:"metadata"`
	Created           int64             `json:"created"`
	ExpiresAt         int64             `json:"expires_at"`
}

type wireSubscription struct {
	ID                string            `json:"id"`
	Status            string            `json:"status"`
	Customer          json.RawMessage   `json:"customer"`
	Created           int64             `json:"created"`
	CancelAt          int64             `json:"cancel_at"`
	CanceledAt        int64             `json:"canceled_at"`
	CancelAtPeriodEnd bool              `json:"cancel_at_period_end"`
	Metadata          map[string]string `json:"metadata"`
	Items             struct {
		Data []struct {
			CurrentPeriodEnd int64 `json:"current_period_end"`
		} `json:"data"`
	} `json:"items"`
	DefaultPaymentMethod json.RawMessage `json:"default_payment_method"`
}

type wirePaymentMethod struct {
	Card struct {
		Brand    string `json:"brand"`
		Last4    string `json:"last4"`
		ExpMonth int    `json:"exp_month"`
		ExpYear  int    `json:"exp_year"`
	} `json:"card"`
	// The fake server and some Stripe surfaces report the card summary flat;
	// accept both rather than losing the display fields.
	Brand    string `json:"brand"`
	Last4    string `json:"last4"`
	ExpMonth int    `json:"exp_month"`
	ExpYear  int    `json:"exp_year"`
}

type wireInvoice struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	Customer json.RawMessage
	Parent   *struct {
		SubscriptionDetails *struct {
			Subscription json.RawMessage   `json:"subscription"`
			Metadata     map[string]string `json:"metadata"`
		} `json:"subscription_details"`
	} `json:"parent"`
	Lines struct {
		Data []struct {
			Period struct {
				End int64 `json:"end"`
			} `json:"period"`
		} `json:"data"`
	} `json:"lines"`
}

// DecodeCheckoutSession reads a checkout.session object.
func DecodeCheckoutSession(raw []byte) (CheckoutSession, error) {
	var wire wireCheckoutSession
	if err := json.Unmarshal(raw, &wire); err != nil {
		return CheckoutSession{}, err
	}
	return CheckoutSession{
		ID:                wire.ID,
		URL:               wire.URL,
		Status:            wire.Status,
		PaymentStatus:     wire.PaymentStatus,
		SubscriptionID:    ExpandableID(wire.Subscription),
		CustomerID:        ExpandableID(wire.Customer),
		ClientReferenceID: wire.ClientReferenceID,
		Metadata:          wire.Metadata,
		Created:           unix(wire.Created),
		ExpiresAt:         unix(wire.ExpiresAt),
	}, nil
}

// DecodeSubscription reads a subscription object.
func DecodeSubscription(raw []byte) (Subscription, error) {
	var wire wireSubscription
	if err := json.Unmarshal(raw, &wire); err != nil {
		return Subscription{}, err
	}
	subscription := Subscription{
		ID:                wire.ID,
		Status:            wire.Status,
		CustomerID:        ExpandableID(wire.Customer),
		Created:           unix(wire.Created),
		CancelAt:          unix(wire.CancelAt),
		CanceledAt:        unix(wire.CanceledAt),
		CancelAtPeriodEnd: wire.CancelAtPeriodEnd,
		Metadata:          wire.Metadata,
	}
	if len(wire.Items.Data) > 0 {
		subscription.CurrentPeriodEnd = unix(wire.Items.Data[0].CurrentPeriodEnd)
	}
	subscription.PaymentMethod = decodePaymentMethod(wire.DefaultPaymentMethod)
	return subscription, nil
}

// DecodeInvoice reads an invoice object.
func DecodeInvoice(raw []byte) (InvoiceEvent, error) {
	var wire wireInvoice
	if err := json.Unmarshal(raw, &wire); err != nil {
		return InvoiceEvent{}, err
	}
	event := InvoiceEvent{ID: wire.ID, Status: wire.Status, CustomerID: ExpandableID(wire.Customer)}
	if wire.Parent != nil && wire.Parent.SubscriptionDetails != nil {
		event.SubscriptionID = ExpandableID(wire.Parent.SubscriptionDetails.Subscription)
		event.Metadata = wire.Parent.SubscriptionDetails.Metadata
	}
	if len(wire.Lines.Data) > 0 {
		event.PeriodEnd = unix(wire.Lines.Data[0].Period.End)
	}
	return event, nil
}

// DecodeCustomer reads a customer object. The default payment method is only
// present when the sender expanded it — a webhook payload carries a bare id —
// so a nil card here means "this payload does not say", which is why the facade
// re-reads the customer rather than acting on the event's object.
func DecodeCustomer(raw []byte) (Customer, error) {
	var wire struct {
		ID              string `json:"id"`
		Email           string `json:"email"`
		InvoiceSettings *struct {
			DefaultPaymentMethod json.RawMessage `json:"default_payment_method"`
		} `json:"invoice_settings"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return Customer{}, err
	}
	customer := Customer{ID: wire.ID, Email: wire.Email}
	if wire.InvoiceSettings != nil {
		customer.PaymentMethod = decodePaymentMethod(wire.InvoiceSettings.DefaultPaymentMethod)
	}
	return customer, nil
}

// ExpandableID reads an id out of a Stripe field that is either a bare id
// string or the expanded object.
func ExpandableID(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var object struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &object); err != nil {
		return ""
	}
	return object.ID
}

func decodePaymentMethod(raw json.RawMessage) *PaymentMethod {
	if len(raw) == 0 {
		return nil
	}
	var wire wirePaymentMethod
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil
	}
	method := PaymentMethod{
		Brand:    wire.Card.Brand,
		Last4:    wire.Card.Last4,
		ExpMonth: wire.Card.ExpMonth,
		ExpYear:  wire.Card.ExpYear,
	}
	if method.Brand == "" {
		method.Brand = wire.Brand
	}
	if method.Last4 == "" {
		method.Last4 = wire.Last4
	}
	if method.ExpMonth == 0 {
		method.ExpMonth = wire.ExpMonth
	}
	if method.ExpYear == 0 {
		method.ExpYear = wire.ExpYear
	}
	if method.Brand == "" && method.Last4 == "" {
		return nil
	}
	return &method
}

func unix(seconds int64) time.Time {
	if seconds <= 0 {
		return time.Time{}
	}
	return time.Unix(seconds, 0).UTC()
}
