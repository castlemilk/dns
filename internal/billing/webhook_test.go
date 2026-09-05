package billing

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/castlemilk/dns/internal/activity"
	stripego "github.com/stripe/stripe-go/v86"
	"github.com/stripe/stripe-go/v86/webhook"
)

// eventBody builds a snapshot event exactly as Stripe renders one.
func eventBody(id, eventType string, created time.Time, object string) []byte {
	return fmt.Appendf(nil,
		`{"id":%q,"object":"event","api_version":%q,"type":%q,"created":%d,"livemode":false,"data":{"object":%s}}`,
		id, stripego.APIVersion, eventType, created.Unix(), object)
}

func subscriptionObject(id, status string, periodEnd time.Time) string {
	return fmt.Sprintf(
		`{"id":%q,"object":"subscription","customer":"cus_fake00000001","status":%q,"created":1,`+
			`"cancel_at":null,"canceled_at":null,"cancel_at_period_end":false,`+
			`"metadata":{"zone_id":"z1","domain":"acme.dev"},`+
			`"items":{"object":"list","data":[{"id":"si_1","object":"subscription_item","current_period_end":%d}]}}`,
		id, status, periodEnd.Unix())
}

func invoiceObject(id, status, subscriptionID string, periodEnd time.Time) string {
	return fmt.Sprintf(
		`{"id":%q,"object":"invoice","status":%q,"customer":"cus_fake00000001","total":600,"currency":"usd",`+
			`"parent":{"type":"subscription_details","subscription_details":{"subscription":%q,`+
			`"metadata":{"zone_id":"z1","domain":"acme.dev"}}},`+
			`"lines":{"object":"list","data":[{"id":"il_1","object":"line_item","period":{"start":1,"end":%d}}]}}`,
		id, status, subscriptionID, periodEnd.Unix())
}

func TestWebhookAcceptsASignedEventOnce(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	body := eventBody("evt_1", "customer.subscription.updated", h.clock.Now(),
		subscriptionObject("sub_1", "active", h.clock.Now().Add(30*24*time.Hour)))

	first := h.deliver(body)
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), `"queued"`) {
		t.Fatalf("first delivery = %d %s", first.Code, first.Body.String())
	}
	second := h.deliver(body)
	if second.Code != http.StatusOK || !strings.Contains(second.Body.String(), `"duplicate"`) {
		t.Fatalf("replay = %d %s", second.Code, second.Body.String())
	}

	h.drain()
	if state := h.subscription("z1").State; state != StateActive {
		t.Fatalf("state = %q", state)
	}
	// The replay changed nothing: a duplicate never reaches applyEvent.
	updates := 0
	for _, event := range h.events.all() {
		if event.Kind == activity.KindBillingSubscriptionUpdated {
			updates++
		}
	}
	if updates != 1 {
		t.Fatalf("subscription events recorded = %d, want 1", updates)
	}
}

func TestWebhookRejectsEverySignatureFailureWithFourHundred(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	body := eventBody("evt_2", "invoice.paid", h.clock.Now(), invoiceObject("in_2", "paid", "sub_1", h.clock.Now()))

	cases := []struct {
		name   string
		header string
	}{
		{"missing header", ""},
		{"malformed header", "not-a-signature"},
		{"tampered signature", func() string {
			signed := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{Payload: body, Secret: testWebhookSecret})
			// The last character is one hex digit of the HMAC, so substituting a
			// fixed "0" leaves the header untouched one time in sixteen and the
			// signature still verifies. Pick a replacement that differs from
			// whatever is there.
			last := signed.Header[len(signed.Header)-1]
			replacement := byte('0')
			if last == replacement {
				replacement = '1'
			}
			return signed.Header[:len(signed.Header)-1] + string(replacement)
		}()},
		{"stale timestamp", webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{
			Payload: body, Secret: testWebhookSecret, Timestamp: time.Now().Add(-10 * time.Minute),
		}).Header},
		{"wrong secret", webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{
			Payload: body, Secret: "whsec_someone_elses_secret_000000",
		}).Header},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			response := h.deliverWithHeader(body, testCase.header)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", response.Code, response.Body.String())
			}
		})
	}

	stats, err := h.store.WebhookStats(context.Background())
	if err != nil {
		t.Fatalf("WebhookStats: %v", err)
	}
	if stats.Pending != 0 {
		t.Fatalf("a rejected delivery was queued: %#v", stats)
	}
	if !h.events.has(activity.KindBillingWebhookRejected) {
		t.Fatalf("no rejection was recorded; kinds = %v", h.events.kinds())
	}
	// The rejection carries the reason and nothing from the payload or the
	// signature.
	for _, event := range h.events.all() {
		if event.Kind != activity.KindBillingWebhookRejected {
			continue
		}
		if len(event.Details) != 1 || event.Details["reason"] == "" {
			t.Fatalf("rejection details = %v", event.Details)
		}
		if strings.Contains(event.Summary, "whsec_") || strings.Contains(event.Summary, "evt_2") {
			t.Fatalf("rejection summary leaks: %q", event.Summary)
		}
	}
}

func TestWebhookRejectsAForeignAPIVersion(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	body := []byte(`{"id":"evt_3","object":"event","api_version":"2019-12-03.acacia",` +
		`"type":"invoice.paid","created":1,"data":{"object":{}}}`)
	response := h.deliver(body)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.Code)
	}
	if !strings.Contains(response.Body.String(), outcomeAPIVersionMismatch) {
		t.Fatalf("body = %s", response.Body.String())
	}
}

func TestWebhookRefusesBrowsersAndWrongMedia(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	body := eventBody("evt_4", "invoice.paid", h.clock.Now(), invoiceObject("in_4", "paid", "sub_1", h.clock.Now()))

	// Stripe never sends Origin; a browser always does.
	origin := h.deliver(body, func(r *http.Request) { r.Header.Set("Origin", "https://console.test") })
	if origin.Code != http.StatusForbidden {
		t.Fatalf("Origin status = %d, want 403", origin.Code)
	}
	media := h.deliver(body, func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") })
	if media.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("Content-Type status = %d, want 415", media.Code)
	}
	request := httptest.NewRequest(http.MethodGet, "/billing/v1/stripe/webhook", nil)
	recorder := httptest.NewRecorder()
	h.webhook.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", recorder.Code)
	}
}

func TestWebhookRefusesAnOversizeBody(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	// A body larger than the 256 KiB cap, signed correctly: the size check
	// still refuses it before any HMAC is computed.
	padding := strings.Repeat("x", WebhookBodyLimit+1024)
	body := eventBody("evt_5", "invoice.paid", h.clock.Now(),
		fmt.Sprintf(`{"id":"in_5","object":"invoice","status":"paid","description":%q}`, padding))
	response := h.deliver(body)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", response.Code)
	}
	if !strings.Contains(response.Body.String(), outcomeOversize) {
		t.Fatalf("body = %s", response.Body.String())
	}
}

// countingReader reports whether the handler read a request body at all.
type countingReader struct {
	reader io.Reader
	read   int
}

func (c *countingReader) Read(buffer []byte) (int, error) {
	count, err := c.reader.Read(buffer)
	c.read += count
	return count, err
}

func TestAnUnsignedFloodIsRefusedBeforeItCostsAnything(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	unsigned := []byte(`{"id":"evt_flood","object":"event","type":"invoice.paid","created":1,"data":{"object":{}}}`)

	refused, rateLimited := 0, 0
	for range admissionPerMinute + 100 {
		response := h.deliverWithHeader(unsigned, "")
		switch response.Code {
		case http.StatusBadRequest:
			refused++
		case http.StatusTooManyRequests:
			rateLimited++
		default:
			t.Fatalf("unsigned status = %d %s", response.Code, response.Body.String())
		}
	}
	if refused == 0 || rateLimited == 0 {
		t.Fatalf("refused = %d, rate limited = %d; the admission bucket did not bound the flood", refused, rateLimited)
	}

	// The bound has to be on the work, not on the status word: once the bucket
	// is empty the body is not read and no HMAC is computed.
	tracked := &countingReader{reader: strings.NewReader(string(unsigned))}
	request := httptest.NewRequest(http.MethodPost, "/billing/v1/stripe/webhook", tracked)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	h.webhook.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status while the bucket is empty = %d %s", recorder.Code, recorder.Body.String())
	}
	if tracked.read != 0 {
		t.Fatalf("a rate-limited request still read %d bytes of body", tracked.read)
	}

	// A 429 is what Stripe retries, and the retry is accepted: the bucket
	// refills at a rate far above Stripe's own.
	h.clock.Advance(time.Minute)
	body := eventBody("evt_6", "customer.subscription.updated", h.clock.Now(),
		subscriptionObject("sub_6", "active", h.clock.Now().Add(time.Hour)))
	response := h.deliver(body)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"queued"`) {
		t.Fatalf("signed delivery after the flood = %d %s", response.Code, response.Body.String())
	}
}

func TestASignedDeliveryDoesNotSpendTheAnonymousBudget(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	// The admission bucket bounds work an unauthenticated caller can cause. A
	// delivery that verifies was not anonymous, so its token is given back:
	// leave five tokens and spend ten signed deliveries against them.
	const spare = 5
	for range admissionPerMinute - spare {
		if !h.service.webhook.admission.allow() {
			t.Fatal("the admission bucket drained early")
		}
	}
	for index := range 10 {
		body := eventBody(fmt.Sprintf("evt_busy_%d", index), "customer.subscription.updated", h.clock.Now(),
			subscriptionObject("sub_1", "active", h.clock.Now().Add(time.Hour)))
		if response := h.deliver(body); response.Code != http.StatusOK {
			t.Fatalf("signed delivery %d = %d %s", index, response.Code, response.Body.String())
		}
	}
	for index := range spare {
		if !h.service.webhook.admission.allow() {
			t.Fatalf("only %d of %d tokens survived ten verified deliveries", index, spare)
		}
	}
	if h.service.webhook.admission.allow() {
		t.Fatal("the admission bucket handed out more tokens than it holds")
	}
}

func TestADeliveryFromTheOtherModeIsRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	// The fake, like any test-mode key, is not livemode. A validly signed
	// delivery that says it is belongs to another world: a test-mode endpoint
	// secret pasted during a rotation would otherwise apply test objects to
	// live rows while the Settings page still reads "Mode: live".
	body := eventBody("evt_livemode", "customer.subscription.updated", h.clock.Now(),
		subscriptionObject("sub_1", "active", h.clock.Now().Add(time.Hour)))
	body = []byte(strings.Replace(string(body), `"livemode":false`, `"livemode":true`, 1))

	response := h.deliver(body)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.Code)
	}
	if !strings.Contains(response.Body.String(), outcomeLivemodeMismatch) {
		t.Fatalf("body = %s", response.Body.String())
	}
	stats, err := h.store.WebhookStats(context.Background())
	if err != nil {
		t.Fatalf("WebhookStats: %v", err)
	}
	if stats.Pending != 0 {
		t.Fatalf("a delivery from the other mode was queued: %#v", stats)
	}
	// The operator has to be able to see this: it is a misconfiguration only
	// they can fix.
	if !h.events.has(activity.KindBillingWebhookRejected) {
		t.Fatalf("activity kinds = %v", h.events.kinds())
	}
}

func TestVerifiedDeliveriesAboveTheCeilingAnswerTooManyRequests(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	// Drain the verified bucket without paying for 600 store writes; the
	// bucket is the thing under test, not bbolt.
	for range verifiedPerMinute {
		if !h.service.webhook.verified.allow() {
			t.Fatal("the verified bucket drained early")
		}
	}
	body := eventBody("evt_7", "invoice.paid", h.clock.Now(), invoiceObject("in_7", "paid", "sub_1", h.clock.Now()))
	response := h.deliver(body)
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", response.Code)
	}

	// A 429 tells Stripe to retry, so nothing was consumed: after the bucket
	// refills the same delivery is accepted.
	h.clock.Advance(time.Minute)
	if again := h.deliver(body); again.Code != http.StatusOK {
		t.Fatalf("status after the bucket refilled = %d", again.Code)
	}
}

func TestASecondSigningSecretVerifiesDuringARotation(t *testing.T) {
	t.Parallel()
	const rotated = "whsec_rotated_000000000000000000"
	h := newHarness(t, harnessOptions{secrets: []string{testWebhookSecret, rotated}})

	body := eventBody("evt_8", "customer.subscription.updated", h.clock.Now(),
		subscriptionObject("sub_8", "active", h.clock.Now().Add(time.Hour)))
	signed := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{Payload: body, Secret: rotated})
	response := h.deliverWithHeader(body, signed.Header)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d %s", response.Code, response.Body.String())
	}
	h.drain()
	if state := h.subscription("z1").State; state != StateActive {
		t.Fatalf("state = %q", state)
	}
}

func TestAnEventForAnUnknownDomainIsAcknowledgedAndRecorded(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	object := strings.Replace(
		subscriptionObject("sub_9", "active", h.clock.Now().Add(time.Hour)),
		`"zone_id":"z1"`, `"zone_id":"z-someone-else"`, 1)
	body := eventBody("evt_9", "customer.subscription.updated", h.clock.Now(), object)

	if response := h.deliver(body); response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	h.drain()
	if !h.events.has(activity.KindBillingEventIgnored) {
		t.Fatalf("kinds = %v", h.events.kinds())
	}
	stats, err := h.store.WebhookStats(context.Background())
	if err != nil {
		t.Fatalf("WebhookStats: %v", err)
	}
	if stats.Pending != 0 || stats.Dead != 0 {
		t.Fatalf("an ignored event stayed queued: %#v", stats)
	}
}

func TestAnEventTypeTheFacadeDoesNotHandleIsDropped(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	body := eventBody("evt_10", "charge.succeeded", h.clock.Now(), `{"id":"ch_1","object":"charge"}`)
	if response := h.deliver(body); response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	h.drain()
	stats, err := h.store.WebhookStats(context.Background())
	if err != nil {
		t.Fatalf("WebhookStats: %v", err)
	}
	if stats.Pending != 0 {
		t.Fatalf("an unhandled type stayed queued: %#v", stats)
	}
}

func TestQueuedPayloadDecodingRejectsAnEmptyEnvelope(t *testing.T) {
	t.Parallel()
	if _, err := decodeQueuedEvent([]byte(`{"object":"event"}`)); err == nil {
		t.Fatal("an envelope with no id or type decoded")
	}
	event, err := decodeQueuedEvent([]byte(`{"id":"evt_1","type":"invoice.paid","created":1700000000,"data":{"object":{"id":"in_1"}}}`))
	if err != nil {
		t.Fatalf("decodeQueuedEvent: %v", err)
	}
	if event.ID != "evt_1" || event.Type != "invoice.paid" || event.Created.Unix() != 1700000000 {
		t.Fatalf("event = %#v", event)
	}
	var object map[string]any
	if err := json.Unmarshal(event.Object, &object); err != nil || object["id"] != "in_1" {
		t.Fatalf("object = %s (%v)", event.Object, err)
	}
}

func TestReceivedAtReadsTheQueueKey(t *testing.T) {
	t.Parallel()
	if got := receivedAt("not-a-key"); !got.IsZero() {
		t.Fatalf("receivedAt(garbage) = %v", got)
	}
	if got := receivedAt("00000191d0d5c000-0001"); got.IsZero() {
		t.Fatal("a well-formed key did not decode")
	}
}
