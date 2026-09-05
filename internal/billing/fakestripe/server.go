// Package fakestripe is an in-process stand-in for the Stripe API, the hosted
// Checkout page and the hosted Billing Portal, plus the signed webhooks Stripe
// would deliver. It is not a fake provider: the production stripe-go client
// talks to it, so local development and CI exercise the real request shaping,
// the real pagination and the real signature verification.
//
// It binds a loopback address only, refuses to run in production (enforced by
// config), and keeps its objects in a JSON file so a control-plane restart does
// not lose the local account.
package fakestripe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	stripego "github.com/stripe/stripe-go/v86"
	"github.com/stripe/stripe-go/v86/webhook"
)

// Defaults for the fake account. They match config's fake defaults.
const (
	// PriceUnitAmount is the $6 the product charges, in minor units.
	PriceUnitAmount = 600
	// PriceCurrency is the only currency the fake prices in.
	PriceCurrency = "usd"
	// PeriodLength is how long a fake subscription period lasts.
	PeriodLength = 30 * 24 * time.Hour
	// staleOffset is how far in the past /__fake/stale/<id> signs a delivery,
	// which is well outside the 300 s tolerance.
	staleOffset = 10 * time.Minute
	// maxRecordedRequests bounds the request log so a long-running local
	// session cannot grow without limit.
	maxRecordedRequests = 500
)

// RecordedRequest is one API call the client made, as the fake saw it. Tests
// assert the production client's request shaping from these.
type RecordedRequest struct {
	Method         string
	Path           string
	Form           url.Values
	IdempotencyKey string
	At             time.Time
}

// Expand returns the expand[] values of the request, in order.
func (r RecordedRequest) Expand() []string {
	var result []string
	for index := 0; ; index++ {
		values, ok := r.Form[fmt.Sprintf("expand[%d]", index)]
		if !ok || len(values) == 0 {
			break
		}
		result = append(result, values[0])
	}
	return result
}

// Get returns the first value of a form field.
func (r RecordedRequest) Get(field string) string { return r.Form.Get(field) }

// Delivery is one webhook the fake sent to the platform handler.
type Delivery struct {
	EventID string
	Type    string
	Status  int
	Body    string
}

// Options configures the server. Listener is required: the caller binds
// BILLING_FAKE_ADDR so a bind failure is reported where the operator set it.
type Options struct {
	Listener  net.Listener
	StatePath string
	Secret    string
	Webhook   http.Handler
	PriceID   string
	Clock     func() time.Time
	Logger    *slog.Logger
}

// Server is the fake API. Its zero value is not usable; call New.
type Server struct {
	listener net.Listener
	base     string
	path     string
	secret   string
	webhook  http.Handler
	priceID  string
	logger   *slog.Logger
	handler  http.Handler

	mu         sync.Mutex
	now        func() time.Time
	state      state
	requests   []RecordedRequest
	deliveries []Delivery
	failStatus int
	failUntil  time.Time
	outOfOrder bool
}

// New builds the server over an already-bound listener.
func New(options Options) (*Server, error) {
	if options.Listener == nil {
		return nil, errors.New("fakestripe: a listener is required")
	}
	if strings.TrimSpace(options.Secret) == "" {
		return nil, errors.New("fakestripe: a signing secret is required")
	}
	loaded, err := loadState(options.StatePath)
	if err != nil {
		return nil, err
	}
	clock := options.Clock
	if clock == nil {
		clock = time.Now
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}
	priceID := options.PriceID
	if priceID == "" {
		priceID = "price_fake_domain_monthly"
	}
	server := &Server{
		listener: options.Listener,
		base:     "http://" + options.Listener.Addr().String(),
		path:     options.StatePath,
		secret:   options.Secret,
		webhook:  options.Webhook,
		priceID:  priceID,
		logger:   logger,
		now:      clock,
		state:    loaded,
	}
	server.handler = server.routes()
	return server, nil
}

// BaseURL is the URL the Stripe client must be pointed at.
func (s *Server) BaseURL() string { return s.base }

// Handler exposes the mux so a test can drive it without a socket.
func (s *Server) Handler() http.Handler { return s.handler }

// Serve runs until ctx is done, then shuts the server down.
func (s *Server) Serve(ctx context.Context) error {
	server := &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
	done := make(chan error, 1)
	go func() {
		err := server.Serve(s.listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			s.logger.Debug("shut the fake Stripe server down", "error", err)
		}
		<-done
		return nil
	}
}

// WithClock replaces the server's clock. Tests use it to age signatures and to
// drive FailAPI windows.
func (s *Server) WithClock(clock func() time.Time) {
	if clock == nil {
		clock = time.Now
	}
	s.mu.Lock()
	s.now = clock
	s.mu.Unlock()
}

// FailAPI makes every /v1 call answer status until the clock has advanced by
// duration. The hosted pages and the test hooks keep working, so a test can
// still drive a checkout while the API is "down".
func (s *Server) FailAPI(status int, duration time.Duration) {
	s.mu.Lock()
	s.failStatus = status
	s.failUntil = s.now().Add(duration)
	s.mu.Unlock()
}

// Reset truncates the state and the recorded requests.
func (s *Server) Reset() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = state{V: stateVersion}
	s.requests = nil
	s.deliveries = nil
	s.failStatus = 0
	s.failUntil = time.Time{}
	return saveState(s.path, s.state)
}

// Requests returns every API call the fake saw, oldest first.
func (s *Server) Requests() []RecordedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]RecordedRequest(nil), s.requests...)
}

// Events returns every event the fake generated, oldest first.
func (s *Server) Events() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Event(nil), s.state.Events...)
}

// Deliveries returns the outcome of every webhook the fake sent.
func (s *Server) Deliveries() []Delivery {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Delivery(nil), s.deliveries...)
}

// SignedPayload signs body as Stripe would at instant at.
func (s *Server) SignedPayload(body []byte, at time.Time) ([]byte, string) {
	signed := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{
		Payload:   body,
		Secret:    s.secret,
		Timestamp: at,
	})
	return body, signed.Header
}

// EmitEvent builds a signed event of the given type around object and delivers
// it to the platform's webhook handler.
func (s *Server) EmitEvent(eventType string, object any) (string, error) {
	return s.EmitEventWithAPIVersion(eventType, object, stripego.APIVersion)
}

// EmitEventWithAPIVersion is EmitEvent with an explicit api_version, so a test
// can prove an endpoint registered with the wrong version is rejected.
func (s *Server) EmitEventWithAPIVersion(eventType string, object any, apiVersion string) (string, error) {
	raw, err := json.Marshal(object)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	event := s.newEventLocked(eventType, apiVersion, raw)
	if err := s.persistLocked(); err != nil {
		s.mu.Unlock()
		return "", err
	}
	at := s.now()
	s.mu.Unlock()
	s.deliver(event, at)
	return event.ID, nil
}

// --- routing --------------------------------------------------------------

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /v1/customers", s.api(s.createCustomer))
	mux.HandleFunc("GET /v1/customers", s.api(s.listCustomers))
	mux.HandleFunc("GET /v1/customers/{id}", s.api(s.getCustomer))
	mux.HandleFunc("GET /v1/prices/{id}", s.api(s.getPrice))
	mux.HandleFunc("POST /v1/checkout/sessions", s.api(s.createCheckoutSession))
	mux.HandleFunc("GET /v1/checkout/sessions/{id}", s.api(s.getCheckoutSession))
	mux.HandleFunc("GET /v1/subscriptions/{id}", s.api(s.getSubscription))
	mux.HandleFunc("GET /v1/subscriptions", s.api(s.listSubscriptions))
	mux.HandleFunc("POST /v1/billing_portal/sessions", s.api(s.createPortalSession))
	mux.HandleFunc("GET /v1/invoices", s.api(s.listInvoices))

	mux.HandleFunc("GET /hosted/checkout/{id}", s.hostedCheckout)
	mux.HandleFunc("POST /hosted/checkout/{id}/complete", s.hostedCheckoutComplete)
	// The page auto-submits through a <meta http-equiv="refresh">, which can
	// only issue a GET; this is the target it points at.
	mux.HandleFunc("GET /hosted/checkout/{id}/auto", s.hostedCheckoutComplete)
	mux.HandleFunc("GET /hosted/checkout/{id}/cancel", s.hostedCheckoutCancel)
	mux.HandleFunc("GET /hosted/portal/{customer}", s.hostedPortal)
	mux.HandleFunc("POST /hosted/portal/{customer}/{action}", s.hostedPortalAction)
	mux.HandleFunc("GET /hosted/invoice/{id}", s.hostedInvoice)

	mux.HandleFunc("POST /__fake/replay/{id}", s.replayEvent)
	mux.HandleFunc("POST /__fake/stale/{id}", s.staleEvent)

	mux.HandleFunc("/", s.notFound)
	return mux
}

// api records the request, applies the FailAPI window and hands control to the
// endpoint.
func (s *Server) api(next func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			s.writeError(w, http.StatusBadRequest, "invalid_request_error", "the request body could not be parsed")
			return
		}
		s.record(r)
		if status, failing := s.failing(); failing {
			s.writeError(w, status, "api_error", "the fake Stripe API is failing on purpose")
			return
		}
		next(w, r)
	}
}

func (s *Server) record(r *http.Request) {
	form := url.Values{}
	for key, values := range r.Form {
		form[key] = append([]string(nil), values...)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, RecordedRequest{
		Method:         r.Method,
		Path:           r.URL.Path,
		Form:           form,
		IdempotencyKey: r.Header.Get("Idempotency-Key"),
		At:             s.now(),
	})
	if len(s.requests) > maxRecordedRequests {
		s.requests = s.requests[len(s.requests)-maxRecordedRequests:]
	}
}

func (s *Server) failing() (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failStatus == 0 || !s.now().Before(s.failUntil) {
		return 0, false
	}
	return s.failStatus, true
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	s.writeError(w, http.StatusNotFound, "invalid_request_error",
		fmt.Sprintf("Unrecognized request URL (%s: %s).", r.Method, r.URL.Path))
}

// --- API endpoints --------------------------------------------------------

func (s *Server) createCustomer(w http.ResponseWriter, r *http.Request) {
	email := r.Form.Get("email")
	key := r.Header.Get("Idempotency-Key")

	s.mu.Lock()
	defer s.mu.Unlock()
	// An idempotency key replays the customer it created, which is what keeps a
	// retried CreateCheckoutSession from opening a second Stripe customer.
	if key != "" {
		for _, record := range s.state.Customers {
			if record.Customer.Metadata["idempotency_key"] == key {
				writeJSON(w, http.StatusOK, record.Customer)
				return
			}
		}
	}
	created := customer{
		ID:       s.nextIDLocked("cus"),
		Object:   "customer",
		Email:    email,
		Created:  unixSeconds(s.now()),
		Metadata: map[string]string{},
	}
	for field, values := range r.Form {
		if strings.HasPrefix(field, "metadata[") && strings.HasSuffix(field, "]") && len(values) > 0 {
			created.Metadata[strings.TrimSuffix(strings.TrimPrefix(field, "metadata["), "]")] = values[0]
		}
	}
	if key != "" {
		created.Metadata["idempotency_key"] = key
	}
	s.state.Customers = append(s.state.Customers, customerRecord{Customer: created})
	if err := s.persistLocked(); err != nil {
		s.writeError(w, http.StatusInternalServerError, "api_error", "the fake could not persist its state")
		return
	}
	writeJSON(w, http.StatusOK, created)
}

func (s *Server) listCustomers(w http.ResponseWriter, r *http.Request) {
	email := r.Form.Get("email")
	limit := formInt(r.Form.Get("limit"), 10)

	s.mu.Lock()
	defer s.mu.Unlock()
	data := make([]customer, 0, limit)
	for _, record := range s.state.Customers {
		if email != "" && record.Customer.Email != email {
			continue
		}
		if len(data) >= limit {
			break
		}
		data = append(data, record.Customer)
	}
	writeJSON(w, http.StatusOK, listOf("/v1/customers", data, false))
}

func (s *Server) getCustomer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	defer s.mu.Unlock()
	index := s.customerIndexLocked(id)
	if index < 0 {
		s.writeError(w, http.StatusNotFound, "invalid_request_error", "No such customer: "+id)
		return
	}
	writeJSON(w, http.StatusOK, s.withCustomerCardLocked(index))
}

func (s *Server) getPrice(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !strings.HasPrefix(id, "price_") {
		s.writeError(w, http.StatusNotFound, "invalid_request_error", "No such price: "+id)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	writeJSON(w, http.StatusOK, fakePrice(id))
}

func (s *Server) createCheckoutSession(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Idempotency-Key")

	s.mu.Lock()
	defer s.mu.Unlock()
	if key != "" {
		for _, record := range s.state.Sessions {
			if record.Session.Metadata["idempotency_key"] == key {
				writeJSON(w, http.StatusOK, record.Session)
				return
			}
		}
	}
	id := s.nextIDLocked("cs")
	created := s.now()
	expires := created.Add(23 * time.Hour)
	if raw := r.Form.Get("expires_at"); raw != "" {
		if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil && parsed > 0 {
			expires = time.Unix(parsed, 0).UTC()
		}
	}
	session := checkoutSession{
		ID:                id,
		Object:            "checkout.session",
		Mode:              r.Form.Get("mode"),
		Status:            "open",
		PaymentStatus:     "unpaid",
		URL:               s.base + "/hosted/checkout/" + id,
		Customer:          r.Form.Get("customer"),
		ClientReferenceID: r.Form.Get("client_reference_id"),
		SuccessURL:        r.Form.Get("success_url"),
		CancelURL:         r.Form.Get("cancel_url"),
		Created:           unixSeconds(created),
		ExpiresAt:         unixSeconds(expires),
		AmountTotal:       PriceUnitAmount,
		Currency:          PriceCurrency,
		Metadata:          map[string]string{},
	}
	if key != "" {
		session.Metadata["idempotency_key"] = key
	}
	record := sessionRecord{
		Session:        session,
		PriceID:        r.Form.Get("line_items[0][price]"),
		SubDescription: r.Form.Get("subscription_data[description]"),
		SubMetadata:    prefixedMap(r.Form, "subscription_data[metadata]["),
	}
	s.state.Sessions = append(s.state.Sessions, record)
	if err := s.persistLocked(); err != nil {
		s.writeError(w, http.StatusInternalServerError, "api_error", "the fake could not persist its state")
		return
	}
	writeJSON(w, http.StatusOK, session)
}

func (s *Server) getCheckoutSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	defer s.mu.Unlock()
	index := s.sessionIndexLocked(id)
	if index < 0 {
		s.writeError(w, http.StatusNotFound, "invalid_request_error", "No such checkout session: "+id)
		return
	}
	writeJSON(w, http.StatusOK, s.state.Sessions[index].Session)
}

func (s *Server) getSubscription(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	defer s.mu.Unlock()
	index := s.subscriptionIndexLocked(id)
	if index < 0 {
		s.writeError(w, http.StatusNotFound, "invalid_request_error", "No such subscription: "+id)
		return
	}
	writeJSON(w, http.StatusOK, s.withCardLocked(s.state.Subscriptions[index]))
}

func (s *Server) listSubscriptions(w http.ResponseWriter, r *http.Request) {
	customerID := r.Form.Get("customer")
	status := r.Form.Get("status")
	limit := formInt(r.Form.Get("limit"), 10)
	after := r.Form.Get("starting_after")

	s.mu.Lock()
	defer s.mu.Unlock()
	var matching []subscription
	for _, item := range s.state.Subscriptions {
		if customerID != "" && item.Customer != customerID {
			continue
		}
		if status != "all" && status != "" && item.Status != status {
			continue
		}
		if status == "" && item.Status == "canceled" {
			continue
		}
		matching = append(matching, s.withCardLocked(item))
	}
	page, hasMore := paginate(matching, after, limit, func(item subscription) string { return item.ID })
	writeJSON(w, http.StatusOK, listOf("/v1/subscriptions", page, hasMore))
}

func (s *Server) createPortalSession(w http.ResponseWriter, r *http.Request) {
	customerID := r.Form.Get("customer")
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.customerIndexLocked(customerID) < 0 {
		s.writeError(w, http.StatusNotFound, "invalid_request_error", "No such customer: "+customerID)
		return
	}
	returnURL := r.Form.Get("return_url")
	target := s.base + "/hosted/portal/" + customerID
	if returnURL != "" {
		target += "?return_url=" + url.QueryEscape(returnURL)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":         s.nextIDLocked("bps"),
		"object":     "billing_portal.session",
		"created":    unixSeconds(s.now()),
		"customer":   customerID,
		"return_url": returnURL,
		"url":        target,
		"livemode":   false,
	})
}

func (s *Server) listInvoices(w http.ResponseWriter, r *http.Request) {
	customerID := r.Form.Get("customer")
	limit := formInt(r.Form.Get("limit"), 10)
	after := r.Form.Get("starting_after")

	s.mu.Lock()
	defer s.mu.Unlock()
	// Stripe lists invoices newest first.
	var matching []invoice
	for index := len(s.state.Invoices) - 1; index >= 0; index-- {
		item := s.state.Invoices[index]
		if customerID != "" && item.Customer != customerID {
			continue
		}
		matching = append(matching, item)
	}
	page, hasMore := paginate(matching, after, limit, func(item invoice) string { return item.ID })
	writeJSON(w, http.StatusOK, listOf("/v1/invoices", page, hasMore))
}

// --- hosted pages ---------------------------------------------------------

func (s *Server) hostedCheckout(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	index := s.sessionIndexLocked(id)
	if index < 0 {
		s.mu.Unlock()
		http.Error(w, "no such checkout session", http.StatusNotFound)
		return
	}
	record := s.state.Sessions[index]
	s.mu.Unlock()

	if record.Session.Status != "open" {
		s.redirect(w, r, successURL(record.Session))
		return
	}
	domain := record.SubMetadata["domain"]
	if domain == "" {
		domain = record.Session.ClientReferenceID
	}
	page := `<!doctype html><meta charset="utf-8"><title>Fake Stripe Checkout</title>` +
		// The page submits itself so the brief's "the checkout completes
		// immediately" holds, with a second of delay so a human can read it.
		`<meta http-equiv="refresh" content="1;url=` + html.EscapeString(s.base+"/hosted/checkout/"+id+"/auto") + `">` +
		`<body style="font-family:system-ui;margin:3rem auto;max-width:32rem">` +
		`<h1>Fake Stripe Checkout</h1>` +
		`<p>` + html.EscapeString(domain) + ` · $6.00 / month</p>` +
		`<form method="post" action="` + html.EscapeString("/hosted/checkout/"+id+"/complete") + `">` +
		`<button type="submit">Complete payment</button></form>` +
		`<p><a href="` + html.EscapeString("/hosted/checkout/"+id+"/cancel") + `">Cancel</a></p>` +
		`<p>No real payment is taken. This page exists so a local console can walk the whole flow.</p>` +
		`</body>`
	writeHTML(w, page)
}

// hostedCheckoutCancel returns the browser to the session's cancel URL.
func (s *Server) hostedCheckoutCancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	index := s.sessionIndexLocked(id)
	if index < 0 {
		s.mu.Unlock()
		http.Error(w, "no such checkout session", http.StatusNotFound)
		return
	}
	target := s.state.Sessions[index].Session.CancelURL
	s.mu.Unlock()
	if target == "" {
		target = s.base
	}
	s.redirect(w, r, target)
}

func (s *Server) hostedCheckoutComplete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	s.mu.Lock()
	index := s.sessionIndexLocked(id)
	if index < 0 {
		s.mu.Unlock()
		http.Error(w, "no such checkout session", http.StatusNotFound)
		return
	}
	record := s.state.Sessions[index]
	if record.Session.Status != "open" {
		target := successURL(record.Session)
		s.mu.Unlock()
		s.redirect(w, r, target)
		return
	}
	now := s.now()
	customerID := record.Session.Customer
	if customerID == "" {
		created := customer{
			ID:      s.nextIDLocked("cus"),
			Object:  "customer",
			Created: unixSeconds(now),
		}
		s.state.Customers = append(s.state.Customers, customerRecord{Customer: created})
		customerID = created.ID
	}
	priceID := record.PriceID
	if priceID == "" {
		priceID = s.priceID
	}
	created := subscription{
		ID:          s.nextIDLocked("sub"),
		Object:      "subscription",
		Customer:    customerID,
		Status:      "active",
		Created:     unixSeconds(now),
		Description: record.SubDescription,
		Metadata:    copyMap(record.SubMetadata),
		Items: subscriptionItemList{
			Object: "list",
			Data: []subscriptionItem{{
				ID:                 s.nextIDLocked("si"),
				Object:             "subscription_item",
				CurrentPeriodStart: unixSeconds(now),
				CurrentPeriodEnd:   unixSeconds(now.Add(PeriodLength)),
				Price:              fakePrice(priceID),
			}},
		},
	}
	s.state.Subscriptions = append(s.state.Subscriptions, created)

	s.state.Sessions[index].Session.Status = "complete"
	s.state.Sessions[index].Session.PaymentStatus = "paid"
	s.state.Sessions[index].Session.Subscription = created.ID
	s.state.Sessions[index].Session.Customer = customerID
	s.state.Sessions[index].Session.URL = ""
	session := s.state.Sessions[index].Session

	paid := s.newInvoiceLocked(created, "paid", now)

	sessionRaw := mustJSON(session)
	subscriptionRaw := mustJSON(s.withCardLocked(created))
	invoiceRaw := mustJSON(paid)
	events := []Event{
		s.newEventLocked("checkout.session.completed", stripego.APIVersion, sessionRaw),
		s.newEventLocked("customer.subscription.created", stripego.APIVersion, subscriptionRaw),
		s.newEventLocked("invoice.paid", stripego.APIVersion, invoiceRaw),
	}
	if err := s.persistLocked(); err != nil {
		s.mu.Unlock()
		http.Error(w, "the fake could not persist its state", http.StatusInternalServerError)
		return
	}
	target := successURL(session)
	s.mu.Unlock()

	s.deliverAll(events, now)
	s.redirect(w, r, target)
}

func (s *Server) hostedPortal(w http.ResponseWriter, r *http.Request) {
	customerID := r.PathValue("customer")
	returnURL := r.URL.Query().Get("return_url")

	s.mu.Lock()
	if s.customerIndexLocked(customerID) < 0 {
		s.mu.Unlock()
		http.Error(w, "no such customer", http.StatusNotFound)
		return
	}
	var rows []subscription
	for _, item := range s.state.Subscriptions {
		if item.Customer == customerID {
			rows = append(rows, item)
		}
	}
	s.mu.Unlock()

	page := `<!doctype html><meta charset="utf-8"><title>Fake Stripe Portal</title>` +
		`<body style="font-family:system-ui;margin:3rem auto;max-width:44rem">` +
		`<h1>Fake Stripe Portal</h1>`
	if len(rows) == 0 {
		page += `<p>This customer has no subscriptions yet.</p>`
	}
	for _, item := range rows {
		domain := item.Metadata["domain"]
		if domain == "" {
			domain = item.ID
		}
		page += `<section style="border:1px solid #ccc;padding:1rem;margin:1rem 0">` +
			`<h2>` + html.EscapeString(domain) + `</h2>` +
			`<p>Status: ` + html.EscapeString(item.Status) + `</p>`
		for _, action := range []struct{ id, label string }{
			{"cancel_at_period_end", "Cancel at period end"},
			{"cancel_now", "Cancel now"},
			{"past_due", "Mark past due"},
			{"paid", "Mark paid"},
			{"update_card", "Update card"},
			{"remove_card", "Remove card"},
		} {
			page += `<form method="post" style="display:inline" action="` +
				html.EscapeString("/hosted/portal/"+customerID+"/"+action.id) + `">` +
				`<input type="hidden" name="subscription" value="` + html.EscapeString(item.ID) + `">` +
				`<input type="hidden" name="return_url" value="` + html.EscapeString(returnURL) + `">` +
				`<button type="submit">` + action.label + `</button></form> `
		}
		page += `</section>`
	}
	if returnURL != "" {
		page += `<p><a href="` + html.EscapeString(returnURL) + `">Return</a></p>`
	}
	page += `</body>`
	writeHTML(w, page)
}

func (s *Server) hostedPortalAction(w http.ResponseWriter, r *http.Request) {
	customerID := r.PathValue("customer")
	action := r.PathValue("action")
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	subscriptionID := r.Form.Get("subscription")
	returnURL := r.Form.Get("return_url")

	s.mu.Lock()
	customerIndex := s.customerIndexLocked(customerID)
	if customerIndex < 0 {
		s.mu.Unlock()
		http.Error(w, "no such customer", http.StatusNotFound)
		return
	}
	// The card actions are customer-level; every other action needs a
	// subscription of this customer.
	customerLevel := action == "update_card" || action == "remove_card"
	index := s.subscriptionIndexLocked(subscriptionID)
	if !customerLevel && (index < 0 || s.state.Subscriptions[index].Customer != customerID) {
		s.mu.Unlock()
		http.Error(w, "no such subscription", http.StatusNotFound)
		return
	}
	now := s.now()
	var events []Event
	switch action {
	case "cancel_at_period_end":
		cancelAt := s.state.Subscriptions[index].Items.Data[0].CurrentPeriodEnd
		s.state.Subscriptions[index].CancelAtPeriodEnd = true
		s.state.Subscriptions[index].CancelAt = &cancelAt
		events = append(events, s.newEventLocked("customer.subscription.updated", stripego.APIVersion,
			mustJSON(s.withCardLocked(s.state.Subscriptions[index]))))
	case "cancel_now":
		canceledAt := unixSeconds(now)
		s.state.Subscriptions[index].Status = "canceled"
		s.state.Subscriptions[index].CanceledAt = &canceledAt
		events = append(events, s.newEventLocked("customer.subscription.deleted", stripego.APIVersion,
			mustJSON(s.withCardLocked(s.state.Subscriptions[index]))))
	case "past_due":
		s.state.Subscriptions[index].Status = "past_due"
		failed := s.newInvoiceLocked(s.state.Subscriptions[index], "open", now)
		events = append(events,
			s.newEventLocked("invoice.payment_failed", stripego.APIVersion, mustJSON(failed)),
			s.newEventLocked("customer.subscription.updated", stripego.APIVersion,
				mustJSON(s.withCardLocked(s.state.Subscriptions[index]))))
	case "paid":
		s.state.Subscriptions[index].Status = "active"
		paid := s.newInvoiceLocked(s.state.Subscriptions[index], "paid", now)
		events = append(events,
			s.newEventLocked("invoice.paid", stripego.APIVersion, mustJSON(paid)),
			s.newEventLocked("customer.subscription.updated", stripego.APIVersion,
				mustJSON(s.withCardLocked(s.state.Subscriptions[index]))))
	case "update_card":
		s.state.Customers[customerIndex].Card = s.newCardLocked()
		events = append(events, s.newEventLocked("customer.updated", stripego.APIVersion,
			mustJSON(s.state.Customers[customerIndex].Customer)))
	case "remove_card":
		// Removing the card is a customer-level action too. Stripe's event
		// carries the customer with a bare default_payment_method id — never
		// the card — so the facade has to re-read the customer to learn there
		// is no longer one.
		s.state.Customers[customerIndex].Card = nil
		events = append(events, s.newEventLocked("customer.updated", stripego.APIVersion,
			mustJSON(s.state.Customers[customerIndex].Customer)))
	default:
		s.mu.Unlock()
		http.Error(w, "unknown portal action", http.StatusNotFound)
		return
	}
	if err := s.persistLocked(); err != nil {
		s.mu.Unlock()
		http.Error(w, "the fake could not persist its state", http.StatusInternalServerError)
		return
	}
	s.mu.Unlock()

	s.deliverAll(events, now)
	target := returnURL
	if target == "" {
		target = s.base + "/hosted/portal/" + customerID
	}
	s.redirect(w, r, target)
}

func (s *Server) hostedInvoice(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	var found *invoice
	for index := range s.state.Invoices {
		if s.state.Invoices[index].ID == id {
			found = &s.state.Invoices[index]
			break
		}
	}
	if found == nil {
		s.mu.Unlock()
		http.Error(w, "no such invoice", http.StatusNotFound)
		return
	}
	copied := *found
	s.mu.Unlock()
	writeHTML(w, `<!doctype html><meta charset="utf-8"><title>Fake Stripe invoice</title>`+
		`<body style="font-family:system-ui;margin:3rem auto;max-width:32rem">`+
		`<h1>Invoice `+html.EscapeString(copied.Number)+`</h1>`+
		`<p>Status: `+html.EscapeString(copied.Status)+`</p>`+
		`<p>Total: $`+html.EscapeString(formatAmount(copied.Total))+` `+html.EscapeString(strings.ToUpper(copied.Currency))+`</p>`+
		`</body>`)
}

// --- test hooks -----------------------------------------------------------

func (s *Server) replayEvent(w http.ResponseWriter, r *http.Request) {
	s.redeliver(w, r.PathValue("id"), 0)
}

func (s *Server) staleEvent(w http.ResponseWriter, r *http.Request) {
	s.redeliver(w, r.PathValue("id"), staleOffset)
}

func (s *Server) redeliver(w http.ResponseWriter, id string, age time.Duration) {
	s.mu.Lock()
	var found *Event
	for index := range s.state.Events {
		if s.state.Events[index].ID == id {
			found = &s.state.Events[index]
			break
		}
	}
	if found == nil {
		s.mu.Unlock()
		http.Error(w, "no such event", http.StatusNotFound)
		return
	}
	event := *found
	at := s.now().Add(-age)
	s.mu.Unlock()

	delivery := s.deliver(event, at)
	writeJSON(w, http.StatusOK, map[string]any{"event": event.ID, "status": delivery.Status})
}

// --- helpers --------------------------------------------------------------

// DeliverOutOfOrder makes the fake fan the deliveries of one state change out
// concurrently, in a random order, which is what Stripe does: deliveries are
// not ordered and each is retried independently. Ordered, one-at-a-time
// delivery stays the default so a test that is not about ordering is
// deterministic.
func (s *Server) DeliverOutOfOrder(enabled bool) {
	s.mu.Lock()
	s.outOfOrder = enabled
	s.mu.Unlock()
}

// deliverAll posts every delivery one state change produced. In order by
// default; concurrently and shuffled once DeliverOutOfOrder is on, so a test
// can prove the state machine does not depend on the order Stripe happens to
// send in.
func (s *Server) deliverAll(events []Event, at time.Time) {
	s.mu.Lock()
	shuffle := s.outOfOrder
	s.mu.Unlock()
	if !shuffle || len(events) < 2 {
		for _, event := range events {
			s.deliver(event, at)
		}
		return
	}
	var wait sync.WaitGroup
	for _, index := range rand.Perm(len(events)) {
		wait.Add(1)
		go func(event Event) {
			defer wait.Done()
			s.deliver(event, at)
		}(events[index])
	}
	wait.Wait()
}

// deliver signs one event and hands it to the platform's webhook handler in
// process, which is exactly what Stripe would POST.
func (s *Server) deliver(event Event, at time.Time) Delivery {
	result := Delivery{EventID: event.ID, Type: event.Type}
	if s.webhook == nil {
		return result
	}
	body, err := json.Marshal(event)
	if err != nil {
		s.logger.Warn("fake stripe could not encode an event", "event_type", event.Type, "error", err)
		return result
	}
	_, header := s.SignedPayload(body, at)
	request := httptest.NewRequest(http.MethodPost, "/billing/v1/stripe/webhook", strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Stripe-Signature", header)
	recorder := httptest.NewRecorder()
	s.webhook.ServeHTTP(recorder, request)

	result.Status = recorder.Code
	result.Body = strings.TrimSpace(recorder.Body.String())
	s.mu.Lock()
	s.deliveries = append(s.deliveries, result)
	s.mu.Unlock()
	if result.Status >= 400 {
		s.logger.Warn("fake stripe webhook delivery was refused",
			"event_type", event.Type, "status", result.Status)
	}
	return result
}

func (s *Server) newEventLocked(eventType, apiVersion string, object json.RawMessage) Event {
	event := Event{
		ID:         s.nextIDLocked("evt"),
		Object:     "event",
		APIVersion: apiVersion,
		Type:       eventType,
		Created:    unixSeconds(s.now()),
		Data:       eventData{Object: object},
	}
	s.state.Events = append(s.state.Events, event)
	return event
}

func (s *Server) newInvoiceLocked(item subscription, status string, now time.Time) invoice {
	id := s.nextIDLocked("in")
	created := invoice{
		ID:       id,
		Object:   "invoice",
		Number:   fmt.Sprintf("FAKE-%04d", s.state.Seq),
		Customer: item.Customer,
		Status:   status,
		Total:    PriceUnitAmount,
		Currency: PriceCurrency,
		Created:  unixSeconds(now),
		Parent: &invoiceParent{
			Type: "subscription_details",
			SubscriptionDetails: &invoiceSubscriptionDetails{
				Subscription: item.ID,
				Metadata:     copyMap(item.Metadata),
			},
		},
		Lines: invoiceLineList{Object: "list"},
	}
	if status == "paid" {
		created.AmountPaid = PriceUnitAmount
		created.HostedInvoiceURL = s.base + "/hosted/invoice/" + id
	}
	line := invoiceLine{ID: s.nextIDLocked("il"), Object: "line_item"}
	line.Period.Start = unixSeconds(now)
	line.Period.End = unixSeconds(now.Add(PeriodLength))
	if len(item.Items.Data) > 0 {
		line.Period.End = item.Items.Data[0].CurrentPeriodEnd
	}
	created.Lines.Data = append(created.Lines.Data, line)
	s.state.Invoices = append(s.state.Invoices, created)
	return created
}

func (s *Server) newCardLocked() *paymentMethod {
	card := &paymentMethod{ID: s.nextIDLocked("pm"), Object: "payment_method", Type: "card"}
	card.Card.Brand = "visa"
	card.Card.Last4 = "4242"
	card.Card.ExpMonth = 12
	card.Card.ExpYear = 2030
	return card
}

// withCustomerCardLocked renders a customer with its default payment method
// expanded, which is what the real API returns for
// expand[]=invoice_settings.default_payment_method. A customer with no card
// answers with a null default payment method rather than omitting the field, so
// the caller can tell "no card" from "not expanded".
func (s *Server) withCustomerCardLocked(index int) customer {
	record := s.state.Customers[index]
	view := record.Customer
	view.InvoiceSettings = &customerInvoiceSettings{DefaultPaymentMethod: record.Card}
	return view
}

// withCardLocked attaches the customer's card to a subscription view, which is
// what expanding default_payment_method does on the real API.
func (s *Server) withCardLocked(item subscription) subscription {
	index := s.customerIndexLocked(item.Customer)
	if index >= 0 {
		item.DefaultPaymentMethod = s.state.Customers[index].Card
	}
	return item
}

func (s *Server) nextIDLocked(prefix string) string {
	s.state.Seq++
	// No underscore after the prefix: the console validates a session id
	// against ^cs_[A-Za-z0-9_]{8,}$ and an invoice cursor against
	// ^in_[A-Za-z0-9]{8,}$, and the latter rejects underscores.
	return fmt.Sprintf("%s_fake%08d", prefix, s.state.Seq)
}

func (s *Server) persistLocked() error {
	if err := saveState(s.path, s.state); err != nil {
		s.logger.Warn("fake stripe could not persist its state", "error", err)
		return err
	}
	return nil
}

func (s *Server) customerIndexLocked(id string) int {
	for index := range s.state.Customers {
		if s.state.Customers[index].Customer.ID == id {
			return index
		}
	}
	return -1
}

func (s *Server) sessionIndexLocked(id string) int {
	for index := range s.state.Sessions {
		if s.state.Sessions[index].Session.ID == id {
			return index
		}
	}
	return -1
}

func (s *Server) subscriptionIndexLocked(id string) int {
	for index := range s.state.Subscriptions {
		if s.state.Subscriptions[index].ID == id {
			return index
		}
	}
	return -1
}

func (s *Server) redirect(w http.ResponseWriter, r *http.Request, target string) {
	if target == "" {
		target = s.base
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// writeError renders a Stripe-shaped error body. It touches no server state, so
// it is safe to call with or without the mutex held.
func (s *Server) writeError(w http.ResponseWriter, status int, kind, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"type": kind, "message": message},
	})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, `{"error":{"type":"api_error"}}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := w.Write(body); err != nil {
		return
	}
}

func writeHTML(w http.ResponseWriter, page string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if _, err := w.Write([]byte(page)); err != nil {
		return
	}
}

func listOf[T any](path string, data []T, hasMore bool) map[string]any {
	if data == nil {
		data = []T{}
	}
	return map[string]any{"object": "list", "url": path, "has_more": hasMore, "data": data}
}

func paginate[T any](items []T, after string, limit int, id func(T) string) ([]T, bool) {
	start := 0
	if after != "" {
		for index, item := range items {
			if id(item) == after {
				start = index + 1
				break
			}
		}
	}
	if start > len(items) {
		start = len(items)
	}
	rest := items[start:]
	if limit > 0 && len(rest) > limit {
		return rest[:limit], true
	}
	return rest, false
}

func fakePrice(id string) price {
	return price{
		ID:         id,
		Object:     "price",
		Active:     true,
		Currency:   PriceCurrency,
		UnitAmount: PriceUnitAmount,
		Type:       "recurring",
		Recurring:  &priceRecurring{Interval: "month", IntervalCount: 1, UsageType: "licensed"},
	}
}

func successURL(session checkoutSession) string {
	if session.SuccessURL == "" {
		return ""
	}
	return strings.ReplaceAll(session.SuccessURL, "{CHECKOUT_SESSION_ID}", session.ID)
}

func prefixedMap(form url.Values, prefix string) map[string]string {
	result := map[string]string{}
	for field, values := range form {
		if !strings.HasPrefix(field, prefix) || !strings.HasSuffix(field, "]") || len(values) == 0 {
			continue
		}
		result[strings.TrimSuffix(strings.TrimPrefix(field, prefix), "]")] = values[0]
	}
	return result
}

func copyMap(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func formInt(raw string, fallback int) int {
	if raw == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func formatAmount(minor int64) string {
	return fmt.Sprintf("%d.%02d", minor/100, minor%100)
}

func mustJSON(value any) json.RawMessage {
	raw, err := json.Marshal(value)
	if err != nil {
		// Every value marshalled here is a struct of strings, numbers and
		// maps, so this is unreachable; returning an empty object keeps the
		// fake from panicking in a developer's console.
		return json.RawMessage(`{}`)
	}
	return raw
}
