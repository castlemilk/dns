package billing

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	billingv1 "github.com/castlemilk/dns/gen/go/billing/v1"
	"github.com/castlemilk/dns/internal/billing/provider"
	"github.com/stripe/stripe-go/v86/webhook"
)

// stubProvider answers whatever a probe test needs. Every other test drives the
// production Stripe client against the fake API; this one exists because a
// price that is not monthly cannot be produced by the fake.
type stubProvider struct {
	price    Price
	priceErr error
}

func (s *stubProvider) Name() string   { return "stripe" }
func (s *stubProvider) Livemode() bool { return false }

func (s *stubProvider) GetPrice(context.Context, string) (Price, error) {
	return s.price, s.priceErr
}

func (s *stubProvider) FindCustomer(context.Context, string) (string, bool, error) {
	return "", false, nil
}

func (s *stubProvider) GetCustomer(context.Context, string) (provider.Customer, error) {
	return provider.Customer{}, errors.New("not used")
}

func (s *stubProvider) EnsureCustomer(context.Context, string, string) (string, error) {
	return "", errors.New("not used")
}

func (s *stubProvider) CreateCheckout(context.Context, CheckoutInput) (CheckoutSession, error) {
	return CheckoutSession{}, errors.New("not used")
}

func (s *stubProvider) GetCheckout(context.Context, string) (CheckoutSession, error) {
	return CheckoutSession{}, errors.New("not used")
}

func (s *stubProvider) GetSubscription(context.Context, string) (Subscription, error) {
	return Subscription{}, errors.New("not used")
}

func (s *stubProvider) ListSubscriptions(context.Context, string) ([]Subscription, error) {
	return nil, errors.New("not used")
}

func (s *stubProvider) CreatePortal(context.Context, PortalInput) (string, error) {
	return "", errors.New("not used")
}

func (s *stubProvider) ListInvoices(context.Context, string, string, int) ([]Invoice, string, error) {
	return nil, "", errors.New("not used")
}

func (s *stubProvider) ConstructEvent([]byte, string) (Event, int, error) {
	return Event{}, -1, errors.New("not used")
}

var _ Provider = (*stubProvider)(nil)

func TestProbeRefusesAPriceThatIsNotMonthlyAndRecurring(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	ctx := context.Background()

	h.service.provider = &stubProvider{price: Price{ID: testPriceID, UnitAmount: 6000, Currency: "usd", Interval: "year"}}
	err := h.service.Probe(ctx)
	if err == nil {
		t.Fatal("a yearly price was accepted")
	}
	if !strings.Contains(err.Error(), "STRIPE_PRICE_ID") {
		t.Fatalf("error = %v; it must name the variable", err)
	}
	status := h.service.Status()
	if status.Reachable || status.Reason != err.Error() {
		t.Fatalf("status = %#v", status)
	}
	if h.service.PriceLabel() != "" {
		t.Fatalf("price label = %q, want empty while the price is refused", h.service.PriceLabel())
	}

	// The reported reason never carries the provider's raw text verbatim when
	// that text looks like a credential.
	h.service.provider = &stubProvider{priceErr: errors.New("no such price: sk_test_leak")}
	if err := h.service.Probe(ctx); err == nil {
		t.Fatal("a failing price lookup was accepted")
	}
	if reported := h.service.Status().Reason; strings.Contains(reported, "sk_test_leak") {
		t.Fatalf("status reason leaks a key: %q", reported)
	}
}

func TestProbeRecoveryWakesTheReconciler(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	ctx := context.Background()

	// Drain the wake channel the readiness probe may already have filled.
	select {
	case <-h.service.reconcileWake:
	default:
	}

	h.service.provider = &stubProvider{priceErr: errors.New("unreachable")}
	if err := h.service.Probe(ctx); err == nil {
		t.Fatal("a failing probe reported success")
	}
	select {
	case <-h.service.reconcileWake:
		t.Fatal("a failure woke the reconciler")
	default:
	}

	h.service.provider = &stubProvider{price: Price{ID: testPriceID, UnitAmount: 600, Currency: "usd", Interval: "month"}}
	if err := h.service.Probe(ctx); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	select {
	case <-h.service.reconcileWake:
	default:
		t.Fatal("recovery did not wake the reconciler")
	}
}

// TestTheWholeFlowOverRealHTTP mounts the webhook route the way internal/app
// does — no bearer, a 256 KiB body cap — and drives the hosted Checkout page
// with an ordinary HTTP client, so the path a browser and Stripe actually take
// is exercised end to end.
func TestTheWholeFlowOverRealHTTP(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	ctx := context.Background()

	mux := http.NewServeMux()
	mux.Handle("POST /billing/v1/stripe/webhook", http.MaxBytesHandler(h.webhook, WebhookBodyLimit))
	control := httptest.NewServer(mux)
	t.Cleanup(control.Close)

	created, err := h.service.CreateCheckoutSession(ctx, newCheckoutRequest("z1"))
	if err != nil {
		t.Fatalf("CreateCheckoutSession: %v", err)
	}
	h.completeCheckoutInBrowser(created.Msg.GetUrl())

	// Replay the three deliveries the fake made, this time over a real socket.
	for _, event := range h.fake.Events() {
		body := mustEventJSON(t, event)
		signed := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{Payload: body, Secret: testWebhookSecret})
		post(t, control.URL, body, signed.Header, http.StatusOK)
	}
	h.drain()

	if state := h.subscription("z1").State; state != StateActive {
		t.Fatalf("state = %q, want ACTIVE", state)
	}

	// The same route over the same socket still refuses a browser and an
	// oversize body.
	body := []byte(`{"id":"evt_x","object":"event","type":"invoice.paid","created":1,"data":{"object":{}}}`)
	signed := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{Payload: body, Secret: testWebhookSecret})
	post(t, control.URL, body, signed.Header, http.StatusBadRequest) // no api_version
	oversize := append([]byte(`{"pad":"`), append([]byte(strings.Repeat("x", WebhookBodyLimit+16)), []byte(`"}`)...)...)
	post(t, control.URL, oversize, signed.Header, http.StatusRequestEntityTooLarge)
}

func mustEventJSON(t *testing.T, event any) []byte {
	t.Helper()
	body, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("encode event: %v", err)
	}
	return body
}

func post(t *testing.T, base string, body []byte, signature string, want int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		base+"/billing/v1/stripe/webhook", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Stripe-Signature", signature)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("post the webhook: %v", err)
	}
	defer closeBody(t, response)
	if response.StatusCode != want {
		t.Fatalf("status = %d, want %d", response.StatusCode, want)
	}
}

func TestUnconfiguredFacadeReportsNoWebhookConfiguration(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{noFake: true})
	status, err := h.service.GetBillingStatus(context.Background(),
		connect.NewRequest(&billingv1.GetBillingStatusRequest{}))
	if err != nil {
		t.Fatalf("GetBillingStatus: %v", err)
	}
	if status.Msg.GetWebhookConfigured() || status.Msg.GetWebhookSecrets() != 0 {
		t.Fatalf("webhook config = %v / %d", status.Msg.GetWebhookConfigured(), status.Msg.GetWebhookSecrets())
	}
	if status.Msg.GetProvider() != "" {
		t.Fatalf("provider = %q", status.Msg.GetProvider())
	}
	if _, ok := h.service.provider.(*stubProvider); ok {
		t.Fatal("an unconfigured facade holds a provider")
	}
	if h.service.provider != nil {
		t.Fatalf("provider = %#v, want nil", h.service.provider)
	}
	if h.service.FakeServer() != nil || h.service.WebhookHandler() != nil {
		t.Fatal("an unconfigured facade exposed a fake server or a webhook handler")
	}
	if _, err := provider.DecodeSubscription([]byte("not json")); err == nil {
		t.Fatal("a malformed subscription object decoded")
	}
}
