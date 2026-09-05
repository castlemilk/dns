package billing

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	billingv1 "github.com/castlemilk/dns/gen/go/billing/v1"
	"github.com/castlemilk/dns/internal/activity"
	"github.com/castlemilk/dns/internal/billing/fakestripe"
	"github.com/castlemilk/dns/internal/config"
	"github.com/castlemilk/dns/internal/enginedns"
	"github.com/castlemilk/dns/internal/platform"
	"github.com/castlemilk/dns/internal/zone"
	"github.com/stripe/stripe-go/v86/webhook"
)

const (
	testCustomerEmail = "ops@example.test"
	testPublicURL     = "http://localhost:3000"
	testWebhookSecret = "whsec_fake_local_0000000000000000"
	testPriceID       = "price_fake_domain_monthly"
)

// testClock is a mutable wall clock. It starts at the real time so signatures
// the fake produces verify inside Stripe's 300 s tolerance, and only tests that
// do not sign advance it.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *testClock { return &testClock{now: time.Now().UTC()} }

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(by time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(by)
	c.mu.Unlock()
}

// fakeZones is the zone side of the world: billing reads names and existence,
// and never writes DNS.
type fakeZones struct {
	mu       sync.Mutex
	zones    map[string]zone.Zone
	failWith error
}

func newZones(names map[string]string) *fakeZones {
	values := make(map[string]zone.Zone, len(names))
	for id, name := range names {
		values[id] = zone.Zone{ID: id, Name: name}
	}
	return &fakeZones{zones: values}
}

func (f *fakeZones) remove(id string) {
	f.mu.Lock()
	delete(f.zones, id)
	f.mu.Unlock()
}

// fail makes both reads answer the given error, standing in for a zone store
// whose own failure text must not reach the browser.
func (f *fakeZones) fail(err error) {
	f.mu.Lock()
	f.failWith = err
	f.mu.Unlock()
}

func (f *fakeZones) GetZone(_ context.Context, zoneID string) (zone.Zone, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failWith != nil {
		return zone.Zone{}, f.failWith
	}
	value, ok := f.zones[zoneID]
	if !ok {
		return zone.Zone{}, connect.NewError(connect.CodeNotFound, errors.New("that domain no longer exists"))
	}
	return value, nil
}

func (f *fakeZones) ListZones(context.Context) ([]zone.Zone, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failWith != nil {
		return nil, f.failWith
	}
	values := make([]zone.Zone, 0, len(f.zones))
	for _, value := range f.zones {
		values = append(values, value)
	}
	return values, nil
}

func (f *fakeZones) ApplyRecordSet(context.Context, string, []enginedns.Op, string, string) (enginedns.Applied, error) {
	return enginedns.Applied{}, errors.New("billing never writes DNS")
}

// recorder captures activity events so a test can assert what an operator sees.
type recorder struct {
	mu     sync.Mutex
	events []activity.Event
}

func (r *recorder) Record(_ context.Context, event activity.Event) {
	r.mu.Lock()
	r.events = append(r.events, event)
	r.mu.Unlock()
}

func (r *recorder) kinds() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	kinds := make([]string, 0, len(r.events))
	for _, event := range r.events {
		kinds = append(kinds, string(event.Kind))
	}
	return kinds
}

func (r *recorder) has(kind activity.Kind) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, event := range r.events {
		if event.Kind == kind {
			return true
		}
	}
	return false
}

func (r *recorder) all() []activity.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]activity.Event(nil), r.events...)
}

// harness wires the facade to a real platform store and a live fake Stripe
// server on a loopback socket, so every test drives the production client.
type harness struct {
	t       *testing.T
	service *Service
	webhook http.Handler
	store   *platform.Store
	fake    *fakestripe.Server
	zones   *fakeZones
	events  *recorder
	clock   *testClock
	secrets []string
}

type harnessOptions struct {
	zones   map[string]string
	secrets []string
	noFake  bool
}

func newHarness(t *testing.T, options harnessOptions) *harness {
	t.Helper()
	if options.zones == nil {
		options.zones = map[string]string{"z1": "acme.dev"}
	}
	if len(options.secrets) == 0 {
		options.secrets = []string{testWebhookSecret}
	}
	directory := t.TempDir()
	store, err := platform.Open(filepath.Join(directory, "platform.db"))
	if err != nil {
		t.Fatalf("open the platform store: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close the platform store: %v", err)
		}
	})

	clock := newClock()
	events := &recorder{}
	zones := newZones(options.zones)
	deps := platform.Deps{
		Store:    store,
		Zones:    zones,
		Serial:   enginedns.NewSerializer(),
		Recorder: events,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Clock:    clock.Now,
	}
	cfg := config.Billing{
		Provider:       config.BillingProviderFake,
		SecretKey:      config.DefaultFakeStripeSecretKey,
		WebhookSecrets: options.secrets,
		PriceID:        testPriceID,
		APIBase:        "http://127.0.0.1:0",
		PublicURL:      testPublicURL,
		CustomerEmail:  testCustomerEmail,
		FakeAddr:       "127.0.0.1:0",
		FakeStatePath:  filepath.Join(directory, "fakestripe.json"),
	}
	if options.noFake {
		cfg = config.Billing{}
	}

	service, hook := New(cfg, deps)
	harness := &harness{
		t: t, service: service, webhook: hook, store: store,
		fake: service.fake, zones: zones, events: events, clock: clock,
		secrets: options.secrets,
	}
	if options.noFake {
		return harness
	}
	if service.fake == nil {
		t.Fatal("the fake Stripe server was not started")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := service.fake.Serve(ctx); err != nil {
			t.Errorf("serve the fake Stripe API: %v", err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	harness.waitReachable()
	return harness
}

// waitReachable probes until the fake answers, which is also the facade's own
// readiness signal.
func (h *harness) waitReachable() {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := h.service.Probe(context.Background()); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.t.Fatal("the billing engine never became reachable")
}

// deliver posts a signed body to the webhook route exactly as Stripe would.
func (h *harness) deliver(body []byte, mutate ...func(*http.Request)) *httptest.ResponseRecorder {
	h.t.Helper()
	signed := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{Payload: body, Secret: h.secrets[0]})
	return h.deliverWithHeader(body, signed.Header, mutate...)
}

func (h *harness) deliverWithHeader(body []byte, header string, mutate ...func(*http.Request)) *httptest.ResponseRecorder {
	h.t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/billing/v1/stripe/webhook", strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	if header != "" {
		request.Header.Set("Stripe-Signature", header)
	}
	for _, apply := range mutate {
		apply(request)
	}
	recorder := httptest.NewRecorder()
	h.webhook.ServeHTTP(recorder, request)
	return recorder
}

// drain applies everything the queue has, the way the worker would.
func (h *harness) drain() {
	h.t.Helper()
	h.service.drainQueue(context.Background())
}

// completeCheckoutInBrowser walks the hosted Checkout page, which is what a
// customer's browser does; the fake delivers the three signed webhooks in
// process while the request is being served.
func (h *harness) completeCheckoutInBrowser(sessionURL string) {
	h.t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.PostForm(sessionURL+"/complete", nil) //nolint:noctx // loopback fake
	if err != nil {
		h.t.Fatalf("complete the hosted checkout: %v", err)
	}
	defer closeBody(h.t, response)
	if response.StatusCode != http.StatusSeeOther {
		h.t.Fatalf("hosted checkout status = %d", response.StatusCode)
	}
}

// portal drives one hosted portal action for a subscription.
func (h *harness) portal(customerID, subscriptionID, action string) {
	h.t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.PostForm(h.fake.BaseURL()+"/hosted/portal/"+customerID+"/"+action, //nolint:noctx // loopback fake
		map[string][]string{"subscription": {subscriptionID}})
	if err != nil {
		h.t.Fatalf("portal %s: %v", action, err)
	}
	defer closeBody(h.t, response)
	if response.StatusCode != http.StatusSeeOther {
		h.t.Fatalf("portal %s status = %d", action, response.StatusCode)
	}
}

func (h *harness) subscription(zoneID string) platform.SubscriptionDoc {
	h.t.Helper()
	doc, err := h.store.GetSubscription(context.Background(), zoneID)
	if err != nil {
		h.t.Fatalf("read the subscription row for %s: %v", zoneID, err)
	}
	return doc
}

// newCheckoutRequest and newSummaryRequest keep the call sites of the RPCs
// short in tests that are about state rather than about the request shape.
func newCheckoutRequest(zoneID string) *connect.Request[billingv1.CreateCheckoutSessionRequest] {
	return connect.NewRequest(&billingv1.CreateCheckoutSessionRequest{ZoneId: zoneID})
}

func newSummaryRequest() *connect.Request[billingv1.GetBillingSummaryRequest] {
	return connect.NewRequest(&billingv1.GetBillingSummaryRequest{})
}

// closeBody keeps errcheck honest about response bodies in tests.
func closeBody(t *testing.T, response *http.Response) {
	t.Helper()
	if err := response.Body.Close(); err != nil {
		t.Errorf("close the response body: %v", err)
	}
}
