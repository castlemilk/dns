package fakestripe

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// stateVersion is stamped on the file. A newer file refuses to load rather than
// silently dropping objects a later build wrote.
const stateVersion = 1

// state is everything the fake remembers. It is persisted as JSON so a
// control-plane restart keeps the customers, subscriptions and invoices a local
// session created — the platform-store rebuild needs something to rebuild from.
type state struct {
	V             int              `json:"v"`
	Seq           int              `json:"seq"`
	Customers     []customerRecord `json:"customers,omitempty"`
	Sessions      []sessionRecord  `json:"sessions,omitempty"`
	Subscriptions []subscription   `json:"subscriptions,omitempty"`
	Invoices      []invoice        `json:"invoices,omitempty"`
	Events        []Event          `json:"events,omitempty"`
}

// customerRecord is one fake customer plus the card the portal attached.
type customerRecord struct {
	Customer customer       `json:"customer"`
	Card     *paymentMethod `json:"card,omitempty"`
}

// sessionRecord is one Checkout session plus the subscription data the session
// carries. The extra fields are kept beside the object rather than inside it so
// the JSON the API returns stays exactly Stripe-shaped.
type sessionRecord struct {
	Session        checkoutSession   `json:"session"`
	PriceID        string            `json:"price_id,omitempty"`
	SubMetadata    map[string]string `json:"sub_metadata,omitempty"`
	SubDescription string            `json:"sub_description,omitempty"`
}

// customer is Stripe's customer object, trimmed to what this fake serves.
// InvoiceSettings is only ever populated on a response, the way Stripe returns
// it when the caller expands invoice_settings.default_payment_method; the card
// itself lives on customerRecord so the persisted state has one copy of it.
type customer struct {
	ID              string                   `json:"id"`
	Object          string                   `json:"object"`
	Email           string                   `json:"email"`
	Created         int64                    `json:"created"`
	Livemode        bool                     `json:"livemode"`
	Metadata        map[string]string        `json:"metadata,omitempty"`
	InvoiceSettings *customerInvoiceSettings `json:"invoice_settings,omitempty"`
}

// customerInvoiceSettings carries the customer's default payment method, which
// is what "the card on file" means: a subscription's default only says what
// that one subscription would be charged on.
type customerInvoiceSettings struct {
	DefaultPaymentMethod *paymentMethod `json:"default_payment_method"`
}

// paymentMethod is Stripe's payment_method object with its card summary.
type paymentMethod struct {
	ID     string `json:"id"`
	Object string `json:"object"`
	Type   string `json:"type"`
	Card   struct {
		Brand    string `json:"brand"`
		Last4    string `json:"last4"`
		ExpMonth int    `json:"exp_month"`
		ExpYear  int    `json:"exp_year"`
	} `json:"card"`
}

// checkoutSession is Stripe's checkout.session object.
type checkoutSession struct {
	ID                string            `json:"id"`
	Object            string            `json:"object"`
	Mode              string            `json:"mode"`
	Status            string            `json:"status"`
	PaymentStatus     string            `json:"payment_status"`
	URL               string            `json:"url,omitempty"`
	Customer          string            `json:"customer,omitempty"`
	Subscription      string            `json:"subscription,omitempty"`
	ClientReferenceID string            `json:"client_reference_id,omitempty"`
	SuccessURL        string            `json:"success_url,omitempty"`
	CancelURL         string            `json:"cancel_url,omitempty"`
	Created           int64             `json:"created"`
	ExpiresAt         int64             `json:"expires_at"`
	AmountTotal       int64             `json:"amount_total"`
	Currency          string            `json:"currency"`
	Livemode          bool              `json:"livemode"`
	Metadata          map[string]string `json:"metadata,omitempty"`
}

// subscriptionItem carries the billing period: since basil the period lives on
// the item, not on the subscription.
type subscriptionItem struct {
	ID                 string `json:"id"`
	Object             string `json:"object"`
	CurrentPeriodStart int64  `json:"current_period_start"`
	CurrentPeriodEnd   int64  `json:"current_period_end"`
	Price              price  `json:"price"`
}

type subscriptionItemList struct {
	Object  string             `json:"object"`
	Data    []subscriptionItem `json:"data"`
	HasMore bool               `json:"has_more"`
}

// subscription is Stripe's subscription object.
type subscription struct {
	ID                   string               `json:"id"`
	Object               string               `json:"object"`
	Customer             string               `json:"customer"`
	Status               string               `json:"status"`
	Created              int64                `json:"created"`
	CancelAt             *int64               `json:"cancel_at"`
	CanceledAt           *int64               `json:"canceled_at"`
	CancelAtPeriodEnd    bool                 `json:"cancel_at_period_end"`
	Description          string               `json:"description,omitempty"`
	Metadata             map[string]string    `json:"metadata,omitempty"`
	Items                subscriptionItemList `json:"items"`
	DefaultPaymentMethod *paymentMethod       `json:"default_payment_method"`
	Livemode             bool                 `json:"livemode"`
}

// price is Stripe's price object.
type price struct {
	ID         string          `json:"id"`
	Object     string          `json:"object"`
	Active     bool            `json:"active"`
	Currency   string          `json:"currency"`
	UnitAmount int64           `json:"unit_amount"`
	Type       string          `json:"type"`
	Recurring  *priceRecurring `json:"recurring"`
	Livemode   bool            `json:"livemode"`
}

type priceRecurring struct {
	Interval      string `json:"interval"`
	IntervalCount int64  `json:"interval_count"`
	UsageType     string `json:"usage_type"`
}

type invoiceSubscriptionDetails struct {
	Subscription string            `json:"subscription"`
	Metadata     map[string]string `json:"metadata,omitempty"`
}

type invoiceParent struct {
	Type                string                      `json:"type"`
	SubscriptionDetails *invoiceSubscriptionDetails `json:"subscription_details,omitempty"`
}

type invoiceLine struct {
	ID     string `json:"id"`
	Object string `json:"object"`
	Period struct {
		Start int64 `json:"start"`
		End   int64 `json:"end"`
	} `json:"period"`
}

type invoiceLineList struct {
	Object  string        `json:"object"`
	Data    []invoiceLine `json:"data"`
	HasMore bool          `json:"has_more"`
}

// invoice is Stripe's invoice object.
type invoice struct {
	ID               string          `json:"id"`
	Object           string          `json:"object"`
	Number           string          `json:"number"`
	Customer         string          `json:"customer"`
	Status           string          `json:"status"`
	Total            int64           `json:"total"`
	AmountPaid       int64           `json:"amount_paid"`
	Currency         string          `json:"currency"`
	Created          int64           `json:"created"`
	HostedInvoiceURL string          `json:"hosted_invoice_url,omitempty"`
	InvoicePDF       string          `json:"invoice_pdf,omitempty"`
	Parent           *invoiceParent  `json:"parent"`
	Lines            invoiceLineList `json:"lines"`
	Livemode         bool            `json:"livemode"`
}

// Event is one webhook event the fake generated. It is exported so tests can
// assert on what was delivered.
type Event struct {
	ID         string    `json:"id"`
	Object     string    `json:"object"`
	APIVersion string    `json:"api_version"`
	Type       string    `json:"type"`
	Created    int64     `json:"created"`
	Livemode   bool      `json:"livemode"`
	Data       eventData `json:"data"`
}

type eventData struct {
	Object json.RawMessage `json:"object"`
}

// loadState reads the state file. A missing file is an empty state; a corrupt
// one is an error, because silently starting from scratch would look like a
// customer disappearing.
func loadState(path string) (state, error) {
	if path == "" {
		return state{V: stateVersion}, nil
	}
	raw, err := os.ReadFile(path) //nolint:gosec // the path comes from BILLING_FAKE_STATE_PATH, validated by config
	if errors.Is(err, os.ErrNotExist) {
		return state{V: stateVersion}, nil
	}
	if err != nil {
		return state{}, fmt.Errorf("fakestripe: read state: %w", err)
	}
	if len(raw) == 0 {
		return state{V: stateVersion}, nil
	}
	var loaded state
	if err := json.Unmarshal(raw, &loaded); err != nil {
		return state{}, fmt.Errorf("fakestripe: parse state: %w", err)
	}
	if loaded.V > stateVersion {
		return state{}, fmt.Errorf("fakestripe: state file version %d is newer than this binary", loaded.V)
	}
	loaded.V = stateVersion
	return loaded, nil
}

// saveState writes the file atomically with mode 0600. It is called with the
// server's mutex held.
func saveState(path string, value state) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("fakestripe: create state directory: %w", err)
	}
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("fakestripe: encode state: %w", err)
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, raw, 0o600); err != nil {
		return fmt.Errorf("fakestripe: write state: %w", err)
	}
	if err := os.Rename(temporary, path); err != nil {
		return fmt.Errorf("fakestripe: replace state: %w", err)
	}
	return nil
}

func unixSeconds(at time.Time) int64 { return at.UTC().Unix() }
