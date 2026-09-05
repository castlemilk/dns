package fakestripe_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/castlemilk/dns/internal/billing/fakestripe"
	stripego "github.com/stripe/stripe-go/v86"
	"github.com/stripe/stripe-go/v86/webhook"
)

const secret = "whsec_fake_local_0000000000000000"

// recordingWebhook stands in for the platform's webhook route.
type recordingWebhook struct {
	mu       sync.Mutex
	payloads [][]byte
	headers  []string
	status   int
}

func (r *recordingWebhook) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	body, err := io.ReadAll(request.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	r.mu.Lock()
	r.payloads = append(r.payloads, body)
	r.headers = append(r.headers, request.Header.Get("Stripe-Signature"))
	status := r.status
	r.mu.Unlock()
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
}

func (r *recordingWebhook) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.payloads)
}

func newServer(t *testing.T, statePath string, hook http.Handler) *fakestripe.Server {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() {
		if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("close the listener: %v", err)
		}
	})
	server, err := fakestripe.New(fakestripe.Options{
		Listener:  listener,
		StatePath: statePath,
		Secret:    secret,
		Webhook:   hook,
	})
	if err != nil {
		t.Fatalf("fakestripe.New: %v", err)
	}
	return server
}

func post(t *testing.T, handler http.Handler, path string, form string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func get(t *testing.T, handler http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	return recorder
}

func decode(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &value); err != nil {
		t.Fatalf("decode %s: %v", recorder.Body.String(), err)
	}
	return value
}

func TestStateSurvivesAServerRestart(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "fakestripe.json")

	first := newServer(t, path, nil)
	created := decode(t, post(t, first.Handler(), "/v1/customers", "email=ops@example.test"))
	id := stringField(t, created, "id")
	if !strings.HasPrefix(id, "cus_fake") {
		t.Fatalf("customer id = %q", id)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("state file: %v", err)
	}

	second := newServer(t, path, nil)
	listed := decode(t, get(t, second.Handler(), "/v1/customers?email=ops@example.test&limit=1"))
	data, ok := listed["data"].([]any)
	if !ok || len(data) != 1 {
		t.Fatalf("customer list after restart = %v", listed)
	}
	row, ok := data[0].(map[string]any)
	if !ok {
		t.Fatalf("customer row = %#v", data[0])
	}
	if row["id"] != id {
		t.Fatalf("customer after restart = %v, want %q", row["id"], id)
	}
}

func TestHostedCheckoutDeliversThreeSignedEvents(t *testing.T) {
	t.Parallel()
	hook := &recordingWebhook{}
	server := newServer(t, filepath.Join(t.TempDir(), "state.json"), hook)

	session := decode(t, post(t, server.Handler(), "/v1/checkout/sessions",
		"mode=subscription&line_items[0][price]=price_x&client_reference_id=z1"+
			"&success_url=http://app.test/ok?session_id={CHECKOUT_SESSION_ID}"+
			"&subscription_data[metadata][zone_id]=z1&subscription_data[metadata][domain]=acme.dev"))
	id := stringField(t, session, "id")

	response := post(t, server.Handler(), "/hosted/checkout/"+id+"/complete", "")
	if response.Code != http.StatusSeeOther {
		t.Fatalf("complete status = %d", response.Code)
	}
	if location := response.Header().Get("Location"); location != "http://app.test/ok?session_id="+id {
		t.Fatalf("redirect = %q", location)
	}
	if hook.count() != 3 {
		t.Fatalf("delivered %d events, want 3", hook.count())
	}

	wantTypes := []string{"checkout.session.completed", "customer.subscription.created", "invoice.paid"}
	for index, payload := range hook.payloads {
		event, err := webhook.ConstructEventWithOptions(payload, hook.headers[index], secret,
			webhook.ConstructEventOptions{Tolerance: 300 * time.Second})
		if err != nil {
			t.Fatalf("event %d did not verify: %v", index, err)
		}
		if string(event.Type) != wantTypes[index] {
			t.Errorf("event %d type = %q, want %q", index, event.Type, wantTypes[index])
		}
		if event.APIVersion != stripego.APIVersion {
			t.Errorf("event %d api_version = %q, want %q", index, event.APIVersion, stripego.APIVersion)
		}
	}

	// The session is settled: retrieving it shows the subscription Stripe only
	// creates on completion.
	fetched := decode(t, get(t, server.Handler(), "/v1/checkout/sessions/"+id))
	if fetched["status"] != "complete" || fetched["payment_status"] != "paid" {
		t.Fatalf("session after completion = %v", fetched)
	}
	if subscription := stringField(t, fetched, "subscription"); !strings.HasPrefix(subscription, "sub_fake") {
		t.Fatalf("subscription = %v", fetched["subscription"])
	}
}

func TestOutOfOrderDeliveryStillDeliversEverySignedEvent(t *testing.T) {
	t.Parallel()
	hook := &recordingWebhook{}
	server := newServer(t, filepath.Join(t.TempDir(), "state.json"), hook)
	// Real Stripe fans deliveries out concurrently and guarantees no order.
	// Reproducing that is the only way a test can hold the platform's state
	// machine to the ordering it claims.
	server.DeliverOutOfOrder(true)

	session := decode(t, post(t, server.Handler(), "/v1/checkout/sessions",
		"mode=subscription&line_items[0][price]=price_x&client_reference_id=z1"+
			"&success_url=http://app.test/ok?session_id={CHECKOUT_SESSION_ID}"+
			"&subscription_data[metadata][zone_id]=z1&subscription_data[metadata][domain]=acme.dev"))
	id := stringField(t, session, "id")

	if response := post(t, server.Handler(), "/hosted/checkout/"+id+"/complete", ""); response.Code != http.StatusSeeOther {
		t.Fatalf("complete status = %d", response.Code)
	}
	if hook.count() != 3 {
		t.Fatalf("delivered %d events, want 3", hook.count())
	}
	delivered := map[string]bool{}
	hook.mu.Lock()
	payloads := append([][]byte(nil), hook.payloads...)
	headers := append([]string(nil), hook.headers...)
	hook.mu.Unlock()
	for index, payload := range payloads {
		event, err := webhook.ConstructEventWithOptions(payload, headers[index], secret,
			webhook.ConstructEventOptions{Tolerance: 300 * time.Second})
		if err != nil {
			t.Fatalf("event %d did not verify: %v", index, err)
		}
		delivered[string(event.Type)] = true
	}
	for _, want := range []string{"checkout.session.completed", "customer.subscription.created", "invoice.paid"} {
		if !delivered[want] {
			t.Fatalf("%s was lost in the reordering; delivered %v", want, delivered)
		}
	}
}

func TestReplayAndStaleHooksResignTheSameEvent(t *testing.T) {
	t.Parallel()
	hook := &recordingWebhook{}
	server := newServer(t, filepath.Join(t.TempDir(), "state.json"), hook)

	id, err := server.EmitEvent("invoice.paid", map[string]any{"id": "in_fake00000001"})
	if err != nil {
		t.Fatalf("EmitEvent: %v", err)
	}
	if hook.count() != 1 {
		t.Fatalf("delivered %d events", hook.count())
	}

	if response := post(t, server.Handler(), "/__fake/replay/"+id, ""); response.Code != http.StatusOK {
		t.Fatalf("replay status = %d", response.Code)
	}
	if hook.count() != 2 {
		t.Fatalf("replay delivered %d events, want 2", hook.count())
	}

	if response := post(t, server.Handler(), "/__fake/stale/"+id, ""); response.Code != http.StatusOK {
		t.Fatalf("stale status = %d", response.Code)
	}
	// The third delivery is signed ten minutes in the past, well outside the
	// 300 s tolerance.
	last := hook.count() - 1
	if _, err := webhook.ConstructEventWithOptions(hook.payloads[last], hook.headers[last], secret,
		webhook.ConstructEventOptions{Tolerance: 300 * time.Second}); err == nil {
		t.Fatal("the stale delivery verified inside the tolerance window")
	}
}

func TestEmitEventWithAPIVersionCarriesTheGivenVersion(t *testing.T) {
	t.Parallel()
	hook := &recordingWebhook{}
	server := newServer(t, filepath.Join(t.TempDir(), "state.json"), hook)

	if _, err := server.EmitEventWithAPIVersion("invoice.paid", map[string]any{"id": "in_1"}, "2019-12-03.acacia"); err != nil {
		t.Fatalf("EmitEventWithAPIVersion: %v", err)
	}
	var event struct {
		APIVersion string `json:"api_version"`
	}
	if err := json.Unmarshal(hook.payloads[0], &event); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if event.APIVersion != "2019-12-03.acacia" {
		t.Fatalf("api_version = %q", event.APIVersion)
	}
}

func TestFailAPIAnswersTheConfiguredStatusUntilTheClockPasses(t *testing.T) {
	t.Parallel()
	server := newServer(t, filepath.Join(t.TempDir(), "state.json"), nil)

	var mu sync.Mutex
	now := time.Now()
	server.WithClock(func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	})

	server.FailAPI(http.StatusServiceUnavailable, 2*time.Hour)
	if code := get(t, server.Handler(), "/v1/prices/price_x").Code; code != http.StatusServiceUnavailable {
		t.Fatalf("status while failing = %d", code)
	}
	// The hosted pages keep working while the API is "down".
	if code := get(t, server.Handler(), "/hosted/checkout/cs_missing").Code; code != http.StatusNotFound {
		t.Fatalf("hosted page status while failing = %d", code)
	}

	mu.Lock()
	now = now.Add(3 * time.Hour)
	mu.Unlock()
	if code := get(t, server.Handler(), "/v1/prices/price_x").Code; code != http.StatusOK {
		t.Fatalf("status after recovery = %d", code)
	}
}

func TestUnknownRoutesAnswerAStripeShapedError(t *testing.T) {
	t.Parallel()
	server := newServer(t, filepath.Join(t.TempDir(), "state.json"), nil)
	response := get(t, server.Handler(), "/v1/charges")
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d", response.Code)
	}
	body := decode(t, response)
	failure, ok := body["error"].(map[string]any)
	if !ok || failure["type"] != "invalid_request_error" {
		t.Fatalf("error body = %v", body)
	}
}

func TestResetTruncatesTheState(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state.json")
	server := newServer(t, path, nil)
	post(t, server.Handler(), "/v1/customers", "email=ops@example.test")
	if err := server.Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	listed := decode(t, get(t, server.Handler(), "/v1/customers"))
	data, ok := listed["data"].([]any)
	if !ok {
		t.Fatalf("customer list = %#v", listed["data"])
	}
	if len(data) != 0 {
		t.Fatalf("customers after reset = %v", data)
	}
	if len(server.Requests()) != 1 {
		t.Fatalf("requests after reset = %d, want 1 (the listing itself)", len(server.Requests()))
	}
}

func TestServeStopsWithItsContext(t *testing.T) {
	t.Parallel()
	server := newServer(t, filepath.Join(t.TempDir(), "state.json"), nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not stop when its context was cancelled")
	}
}

// stringField reads a required string out of a decoded JSON object.
func stringField(t *testing.T, object map[string]any, field string) string {
	t.Helper()
	value, ok := object[field].(string)
	if !ok {
		t.Fatalf("%s = %#v, want a string", field, object[field])
	}
	return value
}
