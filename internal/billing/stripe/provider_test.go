package stripe_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/castlemilk/dns/internal/billing/fakestripe"
	"github.com/castlemilk/dns/internal/billing/provider"
	"github.com/castlemilk/dns/internal/billing/stripe"
	stripego "github.com/stripe/stripe-go/v86"
	"github.com/stripe/stripe-go/v86/webhook"
)

const (
	testSecretKey = "sk_test_fake"
	testWebhook   = "whsec_fake_local_0000000000000000"
	testPriceID   = "price_fake_domain_monthly"
)

// serverFixture starts the fake Stripe API on a loopback socket and returns it
// with a production client pointed at it. Every request shape asserted below is
// therefore the shape the real client would send to api.stripe.com.
type serverFixture struct {
	server   *fakestripe.Server
	provider *stripe.Provider
}

func newFixture(t *testing.T, secrets ...string) *serverFixture {
	t.Helper()
	if len(secrets) == 0 {
		secrets = []string{testWebhook}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server, err := fakestripe.New(fakestripe.Options{
		Listener:  listener,
		StatePath: filepath.Join(t.TempDir(), "fakestripe.json"),
		Secret:    secrets[0],
		PriceID:   testPriceID,
	})
	if err != nil {
		t.Fatalf("fakestripe.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := server.Serve(ctx); err != nil {
			t.Errorf("serve fake stripe: %v", err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	client, err := stripe.New(stripe.Config{
		SecretKey:      testSecretKey,
		APIBase:        server.BaseURL(),
		WebhookSecrets: secrets,
		Timeout:        5 * time.Second,
	})
	if err != nil {
		t.Fatalf("stripe.New: %v", err)
	}
	waitReady(t, server.BaseURL())
	return &serverFixture{server: server, provider: client}
}

func waitReady(t *testing.T, base string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		response, err := http.Get(base + "/v1/prices/" + testPriceID) //nolint:noctx // loopback readiness poll
		if err == nil {
			closeBody(t, response)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the fake Stripe server never became ready")
}

// lastRequest returns the newest recorded request for a path prefix.
func lastRequest(t *testing.T, server *fakestripe.Server, method, prefix string) fakestripe.RecordedRequest {
	t.Helper()
	requests := server.Requests()
	for index := len(requests) - 1; index >= 0; index-- {
		if requests[index].Method == method && strings.HasPrefix(requests[index].Path, prefix) {
			return requests[index]
		}
	}
	t.Fatalf("no %s request to %s was recorded (%d requests)", method, prefix, len(requests))
	return fakestripe.RecordedRequest{}
}

func TestGetPriceReadsTheRecurringPrice(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t)
	price, err := fixture.provider.GetPrice(context.Background(), testPriceID)
	if err != nil {
		t.Fatalf("GetPrice: %v", err)
	}
	if price.ID != testPriceID || price.UnitAmount != 600 || price.Currency != "usd" || price.Interval != "month" {
		t.Fatalf("price = %#v", price)
	}
}

func TestGetPriceMapsAnUnknownIDToNotFound(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t)
	_, err := fixture.provider.GetPrice(context.Background(), "prod_not_a_price")
	if !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("GetPrice error = %v, want ErrNotFound", err)
	}
}

func TestCustomerLookupAndCreationCarryTheExpectedShapes(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t)
	ctx := context.Background()

	if _, found, err := fixture.provider.FindCustomer(ctx, "ops@example.test"); err != nil || found {
		t.Fatalf("FindCustomer on an empty account = (%v, %v)", found, err)
	}
	find := lastRequest(t, fixture.server, http.MethodGet, "/v1/customers")
	if find.Get("email") != "ops@example.test" || find.Get("limit") != "1" {
		t.Fatalf("customer list form = %v", find.Form)
	}

	id, err := fixture.provider.EnsureCustomer(ctx, "ops@example.test", "customer:0123456789abcdef")
	if err != nil {
		t.Fatalf("EnsureCustomer: %v", err)
	}
	if !strings.HasPrefix(id, "cus_") {
		t.Fatalf("customer id = %q", id)
	}
	create := lastRequest(t, fixture.server, http.MethodPost, "/v1/customers")
	if create.IdempotencyKey != "customer:0123456789abcdef" {
		t.Fatalf("idempotency key = %q", create.IdempotencyKey)
	}
	if create.Get("email") != "ops@example.test" || create.Get("metadata[source]") != "simple" {
		t.Fatalf("customer create form = %v", create.Form)
	}

	// The email lookup now finds the customer, which is what keeps a rebuilt
	// platform store from opening a second one.
	found, ok, err := fixture.provider.FindCustomer(ctx, "ops@example.test")
	if err != nil || !ok || found != id {
		t.Fatalf("FindCustomer after create = (%q, %v, %v), want %q", found, ok, err, id)
	}
}

func TestCreateCheckoutSendsASubscriptionSession(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t)
	ctx := context.Background()

	customerID, err := fixture.provider.EnsureCustomer(ctx, "ops@example.test", "customer:abc")
	if err != nil {
		t.Fatalf("EnsureCustomer: %v", err)
	}
	expires := time.Now().Add(23 * time.Hour).Truncate(time.Second)
	session, err := fixture.provider.CreateCheckout(ctx, provider.CheckoutInput{
		CustomerID:     customerID,
		ZoneID:         "z-1",
		ZoneName:       "acme.dev",
		PriceID:        testPriceID,
		SuccessURL:     "http://localhost:3000/billing/success?session_id={CHECKOUT_SESSION_ID}&return=%2Fbilling",
		CancelURL:      "http://localhost:3000/billing?canceled=z-1",
		IdempotencyKey: "checkout:z-1:1",
		ExpiresAt:      expires,
	})
	if err != nil {
		t.Fatalf("CreateCheckout: %v", err)
	}
	if session.Status != "open" || session.URL == "" {
		t.Fatalf("session = %#v", session)
	}

	request := lastRequest(t, fixture.server, http.MethodPost, "/v1/checkout/sessions")
	want := map[string]string{
		"mode":                                 "subscription",
		"line_items[0][price]":                 testPriceID,
		"line_items[0][quantity]":              "1",
		"customer":                             customerID,
		"client_reference_id":                  "z-1",
		"billing_address_collection":           "auto",
		"subscription_data[metadata][zone_id]": "z-1",
		"subscription_data[metadata][domain]":  "acme.dev",
		"subscription_data[description]":       "simple – acme.dev",
	}
	for field, value := range want {
		if got := request.Get(field); got != value {
			t.Errorf("%s = %q, want %q", field, got, value)
		}
	}
	if request.IdempotencyKey != "checkout:z-1:1" {
		t.Errorf("idempotency key = %q", request.IdempotencyKey)
	}
	seconds, err := strconv.ParseInt(request.Get("expires_at"), 10, 64)
	if err != nil {
		t.Fatalf("expires_at = %q: %v", request.Get("expires_at"), err)
	}
	if limit := time.Now().Add(23 * time.Hour).Unix(); seconds > limit {
		t.Errorf("expires_at = %d, want <= now+23h (%d)", seconds, limit)
	}

	// A retry with the same key replays the session rather than opening a
	// second one.
	again, err := fixture.provider.CreateCheckout(ctx, provider.CheckoutInput{
		CustomerID: customerID, ZoneID: "z-1", ZoneName: "acme.dev", PriceID: testPriceID,
		IdempotencyKey: "checkout:z-1:1", ExpiresAt: expires,
	})
	if err != nil {
		t.Fatalf("CreateCheckout (retry): %v", err)
	}
	if again.ID != session.ID {
		t.Fatalf("retry opened a second session: %q then %q", session.ID, again.ID)
	}
}

func TestRetrievalsExpandTheFieldsTheFacadeNeeds(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t)
	ctx := context.Background()

	session := completeOneCheckout(t, fixture)

	fetched, err := fixture.provider.GetCheckout(ctx, session.ID)
	if err != nil {
		t.Fatalf("GetCheckout: %v", err)
	}
	if fetched.Status != "complete" || fetched.PaymentStatus != "paid" || fetched.SubscriptionID == "" {
		t.Fatalf("session = %#v", fetched)
	}
	if expand := lastRequest(t, fixture.server, http.MethodGet, "/v1/checkout/sessions/").Expand(); len(expand) != 1 || expand[0] != "line_items" {
		t.Errorf("checkout expand = %v, want [line_items]", expand)
	}

	subscription, err := fixture.provider.GetSubscription(ctx, fetched.SubscriptionID)
	if err != nil {
		t.Fatalf("GetSubscription: %v", err)
	}
	if subscription.Status != "active" || subscription.CurrentPeriodEnd.IsZero() {
		t.Fatalf("subscription = %#v", subscription)
	}
	if subscription.PaymentMethod == nil || subscription.PaymentMethod.Last4 != "4242" {
		t.Fatalf("expanded payment method = %#v", subscription.PaymentMethod)
	}
	if subscription.Metadata["zone_id"] != "z-1" || subscription.Metadata["domain"] != "acme.dev" {
		t.Fatalf("subscription metadata = %v", subscription.Metadata)
	}
	if expand := lastRequest(t, fixture.server, http.MethodGet, "/v1/subscriptions/").Expand(); len(expand) != 1 || expand[0] != "default_payment_method" {
		t.Errorf("subscription expand = %v, want [default_payment_method]", expand)
	}

	items, err := fixture.provider.ListSubscriptions(ctx, fetched.CustomerID)
	if err != nil {
		t.Fatalf("ListSubscriptions: %v", err)
	}
	if len(items) != 1 || items[0].ID != subscription.ID {
		t.Fatalf("ListSubscriptions = %#v", items)
	}
	list := lastRequest(t, fixture.server, http.MethodGet, "/v1/subscriptions")
	if list.Get("status") != "all" || list.Get("customer") != fetched.CustomerID {
		t.Errorf("subscription list form = %v", list.Form)
	}
}

func TestGetCustomerReadsTheCardOnFile(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t)
	ctx := context.Background()

	customerID, err := fixture.provider.EnsureCustomer(ctx, "ops@example.test", "customer:card")
	if err != nil {
		t.Fatalf("EnsureCustomer: %v", err)
	}
	// A customer with no card answers with no card. That is the whole point of
	// the call: the facade has to be able to tell "none" from "did not look".
	empty, err := fixture.provider.GetCustomer(ctx, customerID)
	if err != nil {
		t.Fatalf("GetCustomer: %v", err)
	}
	if empty.ID != customerID || empty.PaymentMethod != nil {
		t.Fatalf("customer before a card = %#v", empty)
	}
	if expand := lastRequest(t, fixture.server, http.MethodGet, "/v1/customers/").Expand(); len(expand) != 1 ||
		expand[0] != "invoice_settings.default_payment_method" {
		t.Errorf("customer expand = %v, want [invoice_settings.default_payment_method]", expand)
	}

	card, err := http.PostForm(fixture.server.BaseURL()+"/hosted/portal/"+customerID+"/update_card", nil) //nolint:noctx // loopback fake
	if err != nil {
		t.Fatalf("update the card in the hosted portal: %v", err)
	}
	closeBody(t, card)

	attached, err := fixture.provider.GetCustomer(ctx, customerID)
	if err != nil {
		t.Fatalf("GetCustomer after attaching a card: %v", err)
	}
	if attached.PaymentMethod == nil || attached.PaymentMethod.Brand != "visa" || attached.PaymentMethod.Last4 != "4242" {
		t.Fatalf("card on file = %#v", attached.PaymentMethod)
	}

	removed, err := http.PostForm(fixture.server.BaseURL()+"/hosted/portal/"+customerID+"/remove_card", nil) //nolint:noctx // loopback fake
	if err != nil {
		t.Fatalf("remove the card in the hosted portal: %v", err)
	}
	closeBody(t, removed)

	cleared, err := fixture.provider.GetCustomer(ctx, customerID)
	if err != nil {
		t.Fatalf("GetCustomer after removing the card: %v", err)
	}
	if cleared.PaymentMethod != nil {
		t.Fatalf("card after removal = %#v", cleared.PaymentMethod)
	}

	if _, err := fixture.provider.GetCustomer(ctx, "cus_missing0001"); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("GetCustomer(unknown) = %v, want ErrNotFound", err)
	}
}

func TestListInvoicesPagesWithStartingAfter(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t)
	ctx := context.Background()

	session := completeOneCheckout(t, fixture)
	fetched, err := fixture.provider.GetCheckout(ctx, session.ID)
	if err != nil {
		t.Fatalf("GetCheckout: %v", err)
	}
	// A second invoice, so the first page has more.
	if _, err := http.PostForm(fixture.server.BaseURL()+"/hosted/portal/"+fetched.CustomerID+"/paid", //nolint:noctx // loopback fake
		map[string][]string{"subscription": {fetched.SubscriptionID}}); err != nil {
		t.Fatalf("portal mark paid: %v", err)
	}

	first, next, err := fixture.provider.ListInvoices(ctx, fetched.CustomerID, "", 1)
	if err != nil {
		t.Fatalf("ListInvoices: %v", err)
	}
	if len(first) != 1 || next == "" {
		t.Fatalf("first page = %d invoices, next = %q", len(first), next)
	}
	if first[0].ZoneName != "acme.dev" || first[0].Total != 600 || first[0].Status != "paid" {
		t.Fatalf("invoice = %#v", first[0])
	}
	request := lastRequest(t, fixture.server, http.MethodGet, "/v1/invoices")
	if request.Get("limit") != "1" || request.Get("customer") != fetched.CustomerID {
		t.Errorf("invoice list form = %v", request.Form)
	}

	second, _, err := fixture.provider.ListInvoices(ctx, fetched.CustomerID, next, 1)
	if err != nil {
		t.Fatalf("ListInvoices (page 2): %v", err)
	}
	if len(second) != 1 || second[0].ID == first[0].ID {
		t.Fatalf("second page = %#v", second)
	}
	if got := lastRequest(t, fixture.server, http.MethodGet, "/v1/invoices").Get("starting_after"); got != next {
		t.Errorf("starting_after = %q, want %q", got, next)
	}
}

func TestCreatePortalSendsTheFlowDeepLink(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t)
	ctx := context.Background()

	session := completeOneCheckout(t, fixture)
	fetched, err := fixture.provider.GetCheckout(ctx, session.ID)
	if err != nil {
		t.Fatalf("GetCheckout: %v", err)
	}

	url, err := fixture.provider.CreatePortal(ctx, provider.PortalInput{
		CustomerID:     fetched.CustomerID,
		ReturnURL:      "http://localhost:3000/billing",
		Flow:           "subscription_cancel",
		SubscriptionID: fetched.SubscriptionID,
	})
	if err != nil {
		t.Fatalf("CreatePortal: %v", err)
	}
	if url == "" {
		t.Fatal("CreatePortal returned no URL")
	}
	request := lastRequest(t, fixture.server, http.MethodPost, "/v1/billing_portal/sessions")
	if request.Get("flow_data[type]") != "subscription_cancel" ||
		request.Get("flow_data[subscription_cancel][subscription]") != fetched.SubscriptionID {
		t.Fatalf("portal form = %v", request.Form)
	}
	if request.Get("return_url") != "http://localhost:3000/billing" {
		t.Errorf("return_url = %q", request.Get("return_url"))
	}

	if _, err := fixture.provider.CreatePortal(ctx, provider.PortalInput{
		CustomerID: fetched.CustomerID, ReturnURL: "http://localhost:3000/billing", Flow: "nope",
	}); err == nil {
		t.Fatal("an unknown portal flow was accepted")
	}
}

func TestConstructEventVerifiesAgainstEverySecretInOrder(t *testing.T) {
	t.Parallel()
	const rotated = "whsec_rotated_0000000000000000000"
	fixture := newFixture(t, testWebhook, rotated)

	body := []byte(`{"id":"evt_1","object":"event","api_version":"` + stripego.APIVersion +
		`","type":"invoice.paid","created":1,"data":{"object":{"id":"in_1"}}}`)

	for index, secret := range []string{testWebhook, rotated} {
		signed := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{Payload: body, Secret: secret})
		event, matched, err := fixture.provider.ConstructEvent(body, signed.Header)
		if err != nil {
			t.Fatalf("ConstructEvent with secret %d: %v", index, err)
		}
		if matched != index {
			t.Errorf("secret index = %d, want %d", matched, index)
		}
		if event.ID != "evt_1" || event.Type != "invoice.paid" || event.ObjectID != "in_1" {
			t.Errorf("event = %#v", event)
		}
	}

	cases := []struct {
		name   string
		header string
		want   error
	}{
		{"missing", "", webhook.ErrNotSigned},
		{"malformed", "nonsense", webhook.ErrInvalidHeader},
		{"tampered", tamper(webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{Payload: body, Secret: testWebhook}).Header), webhook.ErrNoValidSignature},
		{"stale", webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{
			Payload: body, Secret: testWebhook, Timestamp: time.Now().Add(-10 * time.Minute),
		}).Header, webhook.ErrTooOld},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, _, err := fixture.provider.ConstructEvent(body, testCase.header); !errors.Is(err, testCase.want) {
				t.Fatalf("error = %v, want %v", err, testCase.want)
			}
		})
	}
}

func TestConstructEventRejectsAForeignAPIVersion(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t)
	body := []byte(`{"id":"evt_2","object":"event","api_version":"2019-12-03.acacia","type":"invoice.paid","created":1,"data":{"object":{}}}`)
	signed := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{Payload: body, Secret: testWebhook})
	if _, _, err := fixture.provider.ConstructEvent(body, signed.Header); !errors.Is(err, provider.ErrAPIVersionMismatch) {
		t.Fatalf("error = %v, want ErrAPIVersionMismatch", err)
	}
}

func TestLivemodeFollowsTheKeyPrefix(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t)
	if fixture.provider.Livemode() {
		t.Fatal("a test key reported livemode")
	}
	live, err := stripe.New(stripe.Config{SecretKey: "sk_live_x", APIBase: "https://api.stripe.com"})
	if err != nil {
		t.Fatalf("stripe.New: %v", err)
	}
	if !live.Livemode() {
		t.Fatal("a live key did not report livemode")
	}
	if live.Name() != "stripe" {
		t.Fatalf("Name = %q", live.Name())
	}
}

// completeOneCheckout creates a session and walks the hosted page, which is how
// a browser completes a fake payment.
func completeOneCheckout(t *testing.T, fixture *serverFixture) provider.CheckoutSession {
	t.Helper()
	ctx := context.Background()
	customerID, err := fixture.provider.EnsureCustomer(ctx, "ops@example.test", "customer:abc")
	if err != nil {
		t.Fatalf("EnsureCustomer: %v", err)
	}
	session, err := fixture.provider.CreateCheckout(ctx, provider.CheckoutInput{
		CustomerID: customerID,
		ZoneID:     "z-1",
		ZoneName:   "acme.dev",
		PriceID:    testPriceID,
		SuccessURL: "http://localhost:3000/billing/success?session_id={CHECKOUT_SESSION_ID}",
		CancelURL:  "http://localhost:3000/billing",
		ExpiresAt:  time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateCheckout: %v", err)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.PostForm(session.URL+"/complete", nil) //nolint:noctx // loopback fake
	if err != nil {
		t.Fatalf("complete the hosted checkout: %v", err)
	}
	defer closeBody(t, response)
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("hosted checkout status = %d", response.StatusCode)
	}
	// The fake also attaches a card, so default_payment_method has something to
	// expand once the portal has been used.
	card, err := http.PostForm(fixture.server.BaseURL()+"/hosted/portal/"+customerID+"/update_card", nil) //nolint:noctx // loopback fake
	if err != nil {
		t.Fatalf("update the card in the hosted portal: %v", err)
	}
	defer closeBody(t, card)
	return session
}

// tamper flips the last hex digit of a signature so the HMAC no longer matches.
func tamper(header string) string {
	if header == "" {
		return header
	}
	last := header[len(header)-1]
	replacement := byte('a')
	if last == 'a' {
		replacement = 'b'
	}
	return header[:len(header)-1] + string(replacement)
}

// closeBody keeps errcheck honest about response bodies in tests.
func closeBody(t *testing.T, response *http.Response) {
	t.Helper()
	if err := response.Body.Close(); err != nil {
		t.Errorf("close the response body: %v", err)
	}
}
