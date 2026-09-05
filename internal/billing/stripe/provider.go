// Package stripe is the production billing provider: stripe-go v86 pointed at
// an API base. The base is api.stripe.com in production and the in-process fake
// (internal/billing/fakestripe) for local development and tests, so exactly the
// same request shaping, pagination and webhook signature verification run in
// every environment.
package stripe

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/castlemilk/dns/internal/billing/provider"
	stripego "github.com/stripe/stripe-go/v86"
	"github.com/stripe/stripe-go/v86/webhook"
)

// Tolerance is the signature timestamp window. Stripe's own default; never 0.
const Tolerance = 300 * time.Second

// DefaultTimeout bounds one API call. Every facade call happens either in a
// request handler or in a worker iteration that is itself bounded, so a hung
// Stripe cannot hold either open.
const DefaultTimeout = 10 * time.Second

// maxIdempotencyKey is Stripe's documented limit for the header.
const maxIdempotencyKey = 255

// Config is what the facade knows about Stripe. It holds secrets and is never
// logged or returned.
type Config struct {
	// SecretKey is STRIPE_SECRET_KEY (sk_/rk_).
	SecretKey string
	// APIBase is STRIPE_API_BASE, or the fake server's base URL.
	APIBase string
	// WebhookSecrets are the one to three signing secrets, in order. The index
	// of the one that verified a delivery is reported so a rotation is visible.
	WebhookSecrets []string
	// Timeout bounds one HTTP call; zero means DefaultTimeout.
	Timeout time.Duration
}

// Provider implements provider.Provider against a Stripe-shaped API.
type Provider struct {
	client   *stripego.Client
	secrets  []string
	livemode bool
	apiBase  string
}

// New builds the client. It never dials: the first call does.
func New(cfg Config) (*Provider, error) {
	if strings.TrimSpace(cfg.SecretKey) == "" {
		return nil, errors.New("billing/stripe: the secret key is required")
	}
	if strings.TrimSpace(cfg.APIBase) == "" {
		return nil, errors.New("billing/stripe: the API base is required")
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	// stripe.NewBackends(httpClient) builds a BackendConfig carrying only the
	// HTTP client and ignores URL, so a client built that way would always talk
	// to api.stripe.com and the fake would never be reached. The backend is
	// therefore constructed explicitly with the configured URL.
	backend := stripego.GetBackendWithConfig(stripego.APIBackend, &stripego.BackendConfig{
		URL:               stripego.String(cfg.APIBase),
		HTTPClient:        &http.Client{Timeout: timeout},
		MaxNetworkRetries: stripego.Int64(2),
		// The SDK's default logger writes request failures to stderr outside
		// the application's slog handler, which is where secret redaction
		// lives. Silence it; the facade logs what an operator needs.
		LeveledLogger:   &stripego.LeveledLogger{Level: stripego.LevelNull},
		EnableTelemetry: stripego.Bool(false),
	})
	client := stripego.NewClient(cfg.SecretKey, stripego.WithBackends(&stripego.Backends{API: backend}))

	return &Provider{
		client:   client,
		secrets:  append([]string(nil), cfg.WebhookSecrets...),
		livemode: strings.HasPrefix(cfg.SecretKey, "sk_live_") || strings.HasPrefix(cfg.SecretKey, "rk_live_"),
		apiBase:  cfg.APIBase,
	}, nil
}

// Name is the provider label shown on the Settings page. The fake reports
// "fake" through the facade, which knows which base it configured; the client
// itself is always the Stripe client.
func (p *Provider) Name() string { return "stripe" }

// Livemode reports whether the configured key is a live key.
func (p *Provider) Livemode() bool { return p.livemode }

// APIBase is the base URL this provider talks to. It carries no credential and
// is used by the facade for the endpoint host it shows an operator.
func (p *Provider) APIBase() string { return p.apiBase }

func (p *Provider) GetPrice(ctx context.Context, id string) (provider.Price, error) {
	price, err := p.client.V1Prices.Retrieve(ctx, id, &stripego.PriceRetrieveParams{})
	if err != nil {
		return provider.Price{}, mapError(err)
	}
	return priceView(price), nil
}

func (p *Provider) FindCustomer(ctx context.Context, email string) (string, bool, error) {
	list := p.client.V1Customers.List(ctx, &stripego.CustomerListParams{
		Email:      stripego.String(email),
		ListParams: stripego.ListParams{Limit: stripego.Int64(1), Single: true},
	})
	if err := list.Err(); err != nil {
		return "", false, mapError(err)
	}
	for _, customer := range list.Data() {
		if customer != nil && customer.ID != "" {
			return customer.ID, true, nil
		}
	}
	return "", false, nil
}

// GetCustomer reads one customer and the card it would be charged on. The
// default payment method is expanded because the customer object otherwise
// carries only its id, and the Billing page shows a brand and a last four. A
// customer with no default payment method comes back with a nil card, which is
// how the facade learns a card was removed.
func (p *Provider) GetCustomer(ctx context.Context, id string) (provider.Customer, error) {
	params := &stripego.CustomerRetrieveParams{}
	params.AddExpand("invoice_settings.default_payment_method")
	customer, err := p.client.V1Customers.Retrieve(ctx, id, params)
	if err != nil {
		return provider.Customer{}, mapError(err)
	}
	return customerView(customer), nil
}

func (p *Provider) EnsureCustomer(ctx context.Context, email, idempotencyKey string) (string, error) {
	params := &stripego.CustomerCreateParams{
		Email:    stripego.String(email),
		Metadata: map[string]string{"source": "simple"},
	}
	if key := boundKey(idempotencyKey); key != "" {
		params.SetIdempotencyKey(key)
	}
	customer, err := p.client.V1Customers.Create(ctx, params)
	if err != nil {
		return "", mapError(err)
	}
	return customer.ID, nil
}

func (p *Provider) CreateCheckout(ctx context.Context, in provider.CheckoutInput) (provider.CheckoutSession, error) {
	params := &stripego.CheckoutSessionCreateParams{
		Mode: stripego.String(string(stripego.CheckoutSessionModeSubscription)),
		LineItems: []*stripego.CheckoutSessionCreateLineItemParams{
			{Price: stripego.String(in.PriceID), Quantity: stripego.Int64(1)},
		},
		SuccessURL:               stripego.String(in.SuccessURL),
		CancelURL:                stripego.String(in.CancelURL),
		ClientReferenceID:        stripego.String(in.ZoneID),
		BillingAddressCollection: stripego.String("auto"),
		SubscriptionData: &stripego.CheckoutSessionCreateSubscriptionDataParams{
			Metadata:    map[string]string{"zone_id": in.ZoneID, "domain": in.ZoneName},
			Description: stripego.String("simple – " + in.ZoneName),
		},
	}
	if in.CustomerID != "" {
		params.Customer = stripego.String(in.CustomerID)
	}
	if !in.ExpiresAt.IsZero() {
		params.ExpiresAt = stripego.Int64(in.ExpiresAt.Unix())
	}
	if key := boundKey(in.IdempotencyKey); key != "" {
		params.SetIdempotencyKey(key)
	}
	session, err := p.client.V1CheckoutSessions.Create(ctx, params)
	if err != nil {
		return provider.CheckoutSession{}, mapError(err)
	}
	return checkoutView(session), nil
}

func (p *Provider) GetCheckout(ctx context.Context, sessionID string) (provider.CheckoutSession, error) {
	params := &stripego.CheckoutSessionRetrieveParams{}
	params.AddExpand("line_items")
	session, err := p.client.V1CheckoutSessions.Retrieve(ctx, sessionID, params)
	if err != nil {
		return provider.CheckoutSession{}, mapError(err)
	}
	return checkoutView(session), nil
}

func (p *Provider) GetSubscription(ctx context.Context, id string) (provider.Subscription, error) {
	params := &stripego.SubscriptionRetrieveParams{}
	params.AddExpand("default_payment_method")
	subscription, err := p.client.V1Subscriptions.Retrieve(ctx, id, params)
	if err != nil {
		return provider.Subscription{}, mapError(err)
	}
	return subscriptionView(subscription), nil
}

func (p *Provider) ListSubscriptions(ctx context.Context, customerID string) ([]provider.Subscription, error) {
	list := p.client.V1Subscriptions.List(ctx, &stripego.SubscriptionListParams{
		Customer:   stripego.String(customerID),
		Status:     stripego.String("all"),
		ListParams: stripego.ListParams{Limit: stripego.Int64(100)},
	})
	var result []provider.Subscription
	for subscription, err := range list.All(ctx) {
		if err != nil {
			return nil, mapError(err)
		}
		if subscription == nil {
			continue
		}
		result = append(result, subscriptionView(subscription))
	}
	return result, nil
}

func (p *Provider) CreatePortal(ctx context.Context, in provider.PortalInput) (string, error) {
	params := &stripego.BillingPortalSessionCreateParams{
		Customer:  stripego.String(in.CustomerID),
		ReturnURL: stripego.String(in.ReturnURL),
	}
	switch in.Flow {
	case "":
	case "payment_method_update":
		params.FlowData = &stripego.BillingPortalSessionCreateFlowDataParams{
			Type: stripego.String("payment_method_update"),
		}
	case "subscription_cancel":
		params.FlowData = &stripego.BillingPortalSessionCreateFlowDataParams{
			Type: stripego.String("subscription_cancel"),
			SubscriptionCancel: &stripego.BillingPortalSessionCreateFlowDataSubscriptionCancelParams{
				Subscription: stripego.String(in.SubscriptionID),
			},
		}
	default:
		return "", fmt.Errorf("billing/stripe: unsupported portal flow %q", in.Flow)
	}
	session, err := p.client.V1BillingPortalSessions.Create(ctx, params)
	if err != nil {
		return "", mapError(err)
	}
	return session.URL, nil
}

func (p *Provider) ListInvoices(ctx context.Context, customerID, startingAfter string, limit int) ([]provider.Invoice, string, error) {
	if limit <= 0 {
		limit = 25
	}
	params := &stripego.InvoiceListParams{
		Customer: stripego.String(customerID),
		// Single keeps the iterator on the page the caller asked for: the
		// facade pages the console with its own cursor rather than walking the
		// whole account inside one request.
		ListParams: stripego.ListParams{Limit: stripego.Int64(int64(limit)), Single: true},
	}
	if startingAfter != "" {
		params.StartingAfter = stripego.String(startingAfter)
	}
	list := p.client.V1Invoices.List(ctx, params)
	if err := list.Err(); err != nil {
		return nil, "", mapError(err)
	}
	invoices := make([]provider.Invoice, 0, len(list.Data()))
	last := ""
	for _, invoice := range list.Data() {
		if invoice == nil {
			continue
		}
		invoices = append(invoices, invoiceView(invoice))
		last = invoice.ID
	}
	next := ""
	if list.Meta().HasMore && last != "" {
		next = last
	}
	return invoices, next, nil
}

// ConstructEvent verifies the signature against every configured secret in
// order and returns the index of the first that matched, so a rotation window
// is observable. The API-version compatibility check is done here rather than
// inside the SDK so a mismatch is a distinct error the route can count.
func (p *Provider) ConstructEvent(payload []byte, signatureHeader string) (provider.Event, int, error) {
	if len(p.secrets) == 0 {
		return provider.Event{}, -1, webhook.ErrNoValidSignature
	}
	var lastErr error
	for index, secret := range p.secrets {
		event, err := webhook.ConstructEventWithOptions(payload, signatureHeader, secret, webhook.ConstructEventOptions{
			Tolerance: Tolerance,
			// Checked below so the mismatch is reportable rather than folded
			// into a generic parse failure.
			IgnoreAPIVersionMismatch: true,
		})
		if err != nil {
			lastErr = err
			// Only a signature comparison depends on which secret was used;
			// every other failure is secret-independent, so trying the next
			// secret would only repeat it.
			if errors.Is(err, webhook.ErrNoValidSignature) {
				continue
			}
			return provider.Event{}, -1, err
		}
		if !compatibleAPIVersion(event.APIVersion) {
			return provider.Event{}, index, provider.ErrAPIVersionMismatch
		}
		view := provider.Event{
			ID:         event.ID,
			Type:       string(event.Type),
			APIVersion: event.APIVersion,
			Created:    time.Unix(event.Created, 0).UTC(),
			Livemode:   event.Livemode,
		}
		if event.Data != nil {
			view.Object = event.Data.Raw
			view.ObjectID = provider.ExpandableID(event.Data.Raw)
		}
		return view, index, nil
	}
	if lastErr == nil {
		lastErr = webhook.ErrNoValidSignature
	}
	return provider.Event{}, -1, lastErr
}

// compatibleAPIVersion mirrors the SDK's release-train comparison: an event is
// compatible when its api_version carries the same train as the version this
// build was compiled against.
func compatibleAPIVersion(eventVersion string) bool {
	_, train, found := strings.Cut(eventVersion, ".")
	if !found || train == "" {
		return false
	}
	return train == stripego.APIMajorVersion
}

// APIVersion is the version an operator must register the webhook endpoint
// with. It is exported so the runbook and the tests name one source.
func APIVersion() string { return stripego.APIVersion }

func boundKey(key string) string {
	if len(key) > maxIdempotencyKey {
		return key[:maxIdempotencyKey]
	}
	return key
}

var (
	_ provider.Provider = (*Provider)(nil)
)
