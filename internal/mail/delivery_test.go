package mail

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	mailv1 "github.com/castlemilk/dns/gen/go/mail/v1"
	"github.com/castlemilk/dns/internal/config"
	"github.com/castlemilk/dns/internal/platform"
)

// webhookSecret is the shared secret the receiver's tests configure. It is a
// test value; the production one is MAIL_WEBHOOK_SECRET and never appears here.
const webhookSecret = "test-mail-webhook-secret-0123456789"

// withWebhook is the harness option that turns the receiver on.
func withWebhook(cfg *config.Mail) { cfg.WebhookSecret = webhookSecret }

// deliveryEventJSON renders one event exactly as the mail server posts it: a
// flat data object with `to` a single recipient string.
func deliveryEventJSON(id, kind, to, from string, at time.Time) string {
	return fmt.Sprintf(`{"id":%q,"createdAt":%q,"type":%q,"data":{"to":%q,"from":%q,"queueName":"local"}}`,
		id, at.UTC().Format(time.RFC3339), kind, to, from)
}

func deliveryBody(events ...string) string {
	return `{"events":[` + strings.Join(events, ",") + `]}`
}

// post sends one delivery to the receiver with a correct bearer and signature.
func post(t *testing.T, handler http.Handler, body string, mutate ...func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/mail/v1/delivery-events", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+webhookSecret)
	mac := hmac.New(sha256.New, []byte(webhookSecret))
	mac.Write([]byte(body))
	request.Header.Set("X-Signature", base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	for _, apply := range mutate {
		apply(request)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func TestTheReceiverIsNotMountedWithoutASecret(t *testing.T) {
	h := newHarness(t)
	if h.service.WebhookHandler() != nil {
		t.Error("the delivery-event receiver exists without MAIL_WEBHOOK_SECRET")
	}
	status, err := h.service.GetMailStatus(t.Context(), connect.NewRequest(&mailv1.GetMailStatusRequest{}))
	if err != nil {
		t.Fatalf("GetMailStatus: %v", err)
	}
	if status.Msg.GetDeliveryStatsAvailable() {
		t.Error("delivery_stats_available is true with no receiver")
	}
	if status.Msg.GetDeliveryStatsNote() != DeliveryStatsNote {
		t.Errorf("note = %q, want the unchanged honest copy", status.Msg.GetDeliveryStatsNote())
	}

	// And no domain claims a figure it cannot have.
	zoneValue := h.zoneNamed(t, "acme.dev")
	bound := bind(t, h, zoneValue, nil)
	if bound.GetDomain().GetDelivery() != nil {
		t.Error("a domain reported delivery stats with no receiver")
	}
}

func TestTheReceiverRefusesEveryUnauthenticatedShape(t *testing.T) {
	h := newHarness(t, withWebhook)
	handler := h.service.WebhookHandler()
	if handler == nil {
		t.Fatal("the receiver was not mounted with a secret set")
	}
	zoneValue := h.zoneNamed(t, "acme.dev")
	bind(t, h, zoneValue, nil)

	body := deliveryBody(deliveryEventJSON("1", eventDelivered, "ada@acme.dev", "sender@example.org", h.now()))
	badMAC := hmac.New(sha256.New, []byte("another secret"))
	badMAC.Write([]byte(body))

	tests := []struct {
		name   string
		mutate func(*http.Request)
		status int
	}{
		{
			name:   "no bearer at all",
			mutate: func(r *http.Request) { r.Header.Del("Authorization") },
			status: http.StatusUnauthorized,
		},
		{
			name:   "the wrong bearer",
			mutate: func(r *http.Request) { r.Header.Set("Authorization", "Bearer not-the-secret") },
			status: http.StatusUnauthorized,
		},
		{
			name:   "a bearer without the scheme",
			mutate: func(r *http.Request) { r.Header.Set("Authorization", webhookSecret) },
			status: http.StatusUnauthorized,
		},
		{
			// A signature that is present must be right. Silently accepting a
			// wrong one would make the header decorative.
			name: "a signature signed with another key",
			mutate: func(r *http.Request) {
				r.Header.Set("X-Signature", base64.StdEncoding.EncodeToString(badMAC.Sum(nil)))
			},
			status: http.StatusUnauthorized,
		},
		{
			name:   "a signature that is not base64",
			mutate: func(r *http.Request) { r.Header.Set("X-Signature", "not base64 at all!!") },
			status: http.StatusUnauthorized,
		},
		{
			// Only a browser sends Origin, and no page may drive this route.
			name:   "a browser origin",
			mutate: func(r *http.Request) { r.Header.Set("Origin", "https://console.example") },
			status: http.StatusForbidden,
		},
		{
			name:   "a declared body larger than the limit",
			mutate: func(r *http.Request) { r.ContentLength = WebhookBodyLimit + 1 },
			status: http.StatusRequestEntityTooLarge,
		},
		{
			name:   "the wrong method",
			mutate: func(r *http.Request) { r.Method = http.MethodGet },
			status: http.StatusMethodNotAllowed,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := post(t, handler, body, test.mutate)
			if recorder.Code != test.status {
				t.Errorf("status = %d, want %d", recorder.Code, test.status)
			}
			h.service.flushDeliveries(t.Context())
			doc, err := h.store.GetMailDomain(t.Context(), zoneValue.ID)
			if err != nil {
				t.Fatalf("GetMailDomain: %v", err)
			}
			if len(doc.Delivery) != 0 {
				t.Errorf("a refused delivery was counted: %v", doc.Delivery)
			}
		})
	}

	// A body that is not JSON is refused after the credential passes, so a
	// malformed delivery is distinguishable from an unauthorised one.
	if recorder := post(t, handler, "{not json"); recorder.Code != http.StatusBadRequest {
		t.Errorf("malformed body status = %d, want 400", recorder.Code)
	}

	// The secret must never reach a response body or the log.
	recorder := post(t, handler, body, func(r *http.Request) { r.Header.Set("Authorization", "Bearer wrong") })
	if strings.Contains(recorder.Body.String(), webhookSecret) {
		t.Error("the response body leaked the secret")
	}
	if strings.Contains(h.logs.String(), webhookSecret) {
		t.Error("the log leaked the secret")
	}
}

func TestDeliveryEventsAreCountedPerBoundDomain(t *testing.T) {
	h := newHarness(t, withWebhook)
	handler := h.service.WebhookHandler()
	acme := h.zoneNamed(t, "acme.dev")
	other := h.zoneNamed(t, "other.dev")
	bind(t, h, acme, nil)
	bind(t, h, other, nil)

	now := h.now()
	body := deliveryBody(
		// Two messages acme.dev sent, delivered.
		deliveryEventJSON("1", eventDelivered, "someone@example.org", "ada@acme.dev", now),
		deliveryEventJSON("2", eventDelivered, "another@example.org", "ada@acme.dev", now),
		// One it sent that hard-bounced.
		deliveryEventJSON("3", eventBounced, "nobody@nowhere.invalid", "ada@acme.dev", now),
		// The other bound domain sent one.
		deliveryEventJSON("4", eventDelivered, "team@example.org", "grace@other.dev", now),
		// Mail acme.dev received. It is not counted: the server refuses a
		// message to an address that does not exist before it is queued, so
		// counting inbound successes would add successes and no failures to a
		// pair of numbers that reads as a sending record.
		deliveryEventJSON("5", eventDelivered, "ada@acme.dev", "sender@example.org", now),
		deliveryEventJSON("6", eventBounced, "ada@acme.dev", "sender@example.org", now),
		// A domain this control plane does not host: counted nowhere.
		deliveryEventJSON("7", eventDelivered, "someone@unhosted.example", "x@unhosted.example", now),
		// A bounce notification's null return path belongs to no domain.
		deliveryEventJSON("8", eventDelivered, "ada@acme.dev", "<>", now),
		// An event type that is not one of the two counted ones.
		deliveryEventJSON("9", "delivery.delivered", "someone@example.org", "ada@acme.dev", now),
	)
	if recorder := post(t, handler, body); recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body %q", recorder.Code, recorder.Body.String())
	}
	h.service.flushDeliveries(t.Context())

	// acme.dev: the two messages it sent and delivered, and the one it sent
	// that bounced. delivery.delivered is not counted, because dsn-success
	// already covers the same recipient.
	assertDelivery(t, h, acme.ID, 2, 1)
	assertDelivery(t, h, other.ID, 1, 0)
}

// The pair of numbers the console shows is read as a sending record — a bounce
// rate — so it counts one direction. A domain that only receives mail reports
// zero rather than reporting its inbound successes next to a bounce count that
// can never include the commonest inbound failure.
func TestReceivedMailIsNotCountedAsSent(t *testing.T) {
	h := newHarness(t, withWebhook)
	handler := h.service.WebhookHandler()
	acme := h.zoneNamed(t, "acme.dev")
	bind(t, h, acme, nil)

	now := h.now()
	events := make([]string, 0, 20)
	for index := range 20 {
		events = append(events, deliveryEventJSON(
			fmt.Sprint("in", index), eventDelivered, "ada@acme.dev", "newsletter@example.org", now))
	}
	if recorder := post(t, handler, deliveryBody(events...)); recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	h.service.flushDeliveries(t.Context())
	assertDelivery(t, h, acme.ID, 0, 0)

	// One message the domain itself sent, which bounced: the figure is 0/1, the
	// true record of what it sent, not 20/1.
	post(t, handler, deliveryBody(deliveryEventJSON(
		"out", eventBounced, "nobody@nowhere.invalid", "ada@acme.dev", now)))
	h.service.flushDeliveries(t.Context())
	assertDelivery(t, h, acme.ID, 0, 1)
}

// One event whose sender and recipient are both in the same bound zone counts
// once, not twice.
func TestAnInternalMessageIsCountedOnce(t *testing.T) {
	h := newHarness(t, withWebhook)
	handler := h.service.WebhookHandler()
	acme := h.zoneNamed(t, "acme.dev")
	bind(t, h, acme, nil)

	body := deliveryBody(deliveryEventJSON("1", eventDelivered, "ada@acme.dev", "grace@acme.dev", h.now()))
	post(t, handler, body)
	h.service.flushDeliveries(t.Context())
	assertDelivery(t, h, acme.ID, 1, 0)
}

// The mail server retries a delivery it could not complete, so the same event
// must not be counted twice.
func TestARetriedDeliveryIsCountedOnce(t *testing.T) {
	h := newHarness(t, withWebhook)
	handler := h.service.WebhookHandler()
	acme := h.zoneNamed(t, "acme.dev")
	bind(t, h, acme, nil)

	body := deliveryBody(deliveryEventJSON("1788", eventDelivered, "s@example.org", "ada@acme.dev", h.now()))
	for range 3 {
		if recorder := post(t, handler, body); recorder.Code != http.StatusOK {
			t.Fatalf("status = %d", recorder.Code)
		}
	}
	h.service.flushDeliveries(t.Context())
	assertDelivery(t, h, acme.ID, 1, 0)
}

// The window is bounded: an hour that ages out of it stops being counted and
// stops being stored.
func TestTheDeliveryWindowIsBoundedAndRolls(t *testing.T) {
	h := newHarness(t, withWebhook)
	handler := h.service.WebhookHandler()
	acme := h.zoneNamed(t, "acme.dev")
	bind(t, h, acme, nil)
	// The window is only reported as complete once this process has been
	// counting for its whole width, so the tests below start from there.
	h.service.deliverySince = h.now().Add(-48 * time.Hour)

	// One event an hour for thirty hours. Each one is posted with the clock
	// where the mail server's own timestamp says it happened.
	for hour := range 30 {
		h.advance(time.Hour)
		body := deliveryBody(deliveryEventJSON(
			fmt.Sprint("e", hour), eventDelivered, "s@example.org", "ada@acme.dev", h.now()))
		if recorder := post(t, handler, body); recorder.Code != http.StatusOK {
			t.Fatalf("status = %d", recorder.Code)
		}
		h.service.flushDeliveries(t.Context())
	}

	doc, err := h.store.GetMailDomain(t.Context(), acme.ID)
	if err != nil {
		t.Fatalf("GetMailDomain: %v", err)
	}
	if len(doc.Delivery) > deliveryBuckets {
		t.Errorf("stored %d hours, the row keeps at most %d", len(doc.Delivery), deliveryBuckets)
	}
	// The rolling count is the last 24 hours, not all thirty.
	stats := h.service.deliveryStats(doc)
	if stats.GetDelivered() != 24 {
		t.Errorf("delivered over the window = %d, want 24", stats.GetDelivered())
	}
	if stats.GetWindowHours() != 24 {
		t.Errorf("window_hours = %d, want 24", stats.GetWindowHours())
	}
}

// A count is never claimed for a window this process did not observe. A restart
// resets the start of the window; the console then shows a shorter, true one
// rather than a complete, false one.
func TestTheReportedWindowNeverPredatesThisProcess(t *testing.T) {
	h := newHarness(t, withWebhook)
	acme := h.zoneNamed(t, "acme.dev")
	bind(t, h, acme, nil)
	// One accepted delivery, so there is a receiver to report a window for at
	// all; the window itself is what this test is about.
	post(t, h.service.WebhookHandler(), deliveryBody(
		deliveryEventJSON("1", eventDelivered, "s@example.org", "ada@acme.dev", h.now())))
	h.service.flushDeliveries(t.Context())
	doc, err := h.store.GetMailDomain(t.Context(), acme.ID)
	if err != nil {
		t.Fatalf("GetMailDomain: %v", err)
	}

	h.service.deliverySince = h.now().Add(-2 * time.Hour)
	stats := h.service.deliveryStats(doc)
	if got := stats.GetSince().AsTime(); !got.Equal(h.now().Add(-2 * time.Hour).UTC().Truncate(time.Hour)) {
		t.Errorf("since = %s, want the moment this process started counting", got)
	}
	// And the width the response reports is the width observed, so the console
	// says "the last 3 hours" rather than labelling it a day.
	if stats.GetWindowHours() != 3 {
		t.Errorf("window_hours = %d, want the 3 hours actually counted", stats.GetWindowHours())
	}

	// Once the process has been up longer than the window, the window wins.
	h.service.deliverySince = h.now().Add(-72 * time.Hour)
	stats = h.service.deliveryStats(doc)
	if got := stats.GetSince().AsTime(); !got.Equal(windowStart(h.now())) {
		t.Errorf("since = %s, want the start of the rolling window", got)
	}
}

// The mail server buffers a webhook it could not deliver and retries it, so the
// deliveries that arrive just after this control plane starts routinely carry a
// createdAt from before it did. Such an event is accepted, so it must also be
// counted: dating it in an hour the response will never sum stores a real
// delivery in platform.db and shows the operator a zero.
func TestAnEventDatedBeforeCountingStartedIsStillCounted(t *testing.T) {
	h := newHarness(t, withWebhook)
	handler := h.service.WebhookHandler()
	acme := h.zoneNamed(t, "acme.dev")
	bind(t, h, acme, nil)

	// Half an hour after this process started counting, the mail server posts
	// the two events it had buffered from just before that: one delivered, one
	// bounced, both dated in the previous hour.
	h.advance(30 * time.Minute)
	buffered := h.now().Add(-40 * time.Minute)
	body := deliveryBody(
		deliveryEventJSON("b1", eventDelivered, "s@example.org", "ada@acme.dev", buffered),
		deliveryEventJSON("b2", eventBounced, "nobody@nowhere.invalid", "ada@acme.dev", buffered),
	)
	if recorder := post(t, handler, body); recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body %q", recorder.Code, recorder.Body.String())
	}
	h.service.flushDeliveries(t.Context())
	assertDelivery(t, h, acme.ID, 1, 1)

	// And what is stored is what is reported: no bucket is written outside the
	// window that sums them.
	doc, err := h.store.GetMailDomain(t.Context(), acme.ID)
	if err != nil {
		t.Fatalf("GetMailDomain: %v", err)
	}
	since := h.service.deliveryStats(doc).GetSince().AsTime()
	for _, entry := range doc.Delivery {
		if entry.Hour.UTC().Before(since) {
			t.Errorf("hour %s holds %d delivered and %d bounced that no response counts",
				entry.Hour.UTC(), entry.Delivered, entry.Bounced)
		}
	}

	// The same holds at the other end of the window once the process has been
	// counting for longer than it: an event dated a few minutes before the
	// oldest reported hour is counted at arrival rather than lost to it.
	h.service.deliverySince = h.now().Add(-72 * time.Hour)
	stale := windowStart(h.now()).Add(-10 * time.Minute)
	if recorder := post(t, handler, deliveryBody(deliveryEventJSON(
		"b3", eventDelivered, "s@example.org", "ada@acme.dev", stale))); recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	h.service.flushDeliveries(t.Context())
	assertDelivery(t, h, acme.ID, 2, 1)
}

// Counts belong to a binding. Unbinding a domain takes its row, and its
// history, with it.
func TestUnbindingDropsTheDeliveryHistory(t *testing.T) {
	h := newHarness(t, withWebhook)
	handler := h.service.WebhookHandler()
	acme := h.zoneNamed(t, "acme.dev")
	bind(t, h, acme, nil)
	post(t, handler, deliveryBody(deliveryEventJSON("1", eventDelivered, "s@example.org", "ada@acme.dev", h.now())))
	h.service.flushDeliveries(t.Context())
	assertDelivery(t, h, acme.ID, 1, 0)

	if _, err := h.service.UnbindMailDomain(t.Context(), connect.NewRequest(&mailv1.UnbindMailDomainRequest{
		ZoneId: acme.ID,
	})); err != nil {
		t.Fatalf("UnbindMailDomain: %v", err)
	}
	// A late event for a zone that is no longer bound is dropped rather than
	// resurrecting the row.
	post(t, handler, deliveryBody(deliveryEventJSON("2", eventDelivered, "s@example.org", "ada@acme.dev", h.now())))
	h.service.flushDeliveries(t.Context())
	if _, err := h.store.GetMailDomain(t.Context(), acme.ID); err == nil {
		t.Error("a delivery event recreated an unbound row")
	}
}

// A secret set on this side is half of the wiring, and the half this control
// plane cannot check is the one that fails silently: a webhook that was never
// created, or was created without the restart that loads it, looks exactly like
// a quiet day. So nothing is reported as counted until the mail server has
// actually posted something, and the note says which half to check.
func TestNothingIsCountedUntilTheMailServerHasPosted(t *testing.T) {
	h := newHarness(t, withWebhook)
	acme := h.zoneNamed(t, "acme.dev")
	bound := bind(t, h, acme, nil)
	if bound.GetDomain().GetDelivery() != nil {
		t.Error("a domain reported delivery stats before any event was ever received")
	}

	status, err := h.service.GetMailStatus(t.Context(), connect.NewRequest(&mailv1.GetMailStatusRequest{}))
	if err != nil {
		t.Fatalf("GetMailStatus: %v", err)
	}
	if status.Msg.GetDeliveryStatsAvailable() {
		t.Error("delivery_stats_available is true before the mail server has posted anything")
	}
	if status.Msg.GetDeliveryStatsNote() != DeliveryStatsPendingNote {
		t.Errorf("note = %q, want the copy naming the missing webhook", status.Msg.GetDeliveryStatsNote())
	}
	if status.Msg.GetDeliveryWindowHours() != 0 {
		t.Errorf("delivery_window_hours = %d, want none while nothing is counted", status.Msg.GetDeliveryWindowHours())
	}

	// A whole day of waiting does not turn the absence into a zero.
	h.advance(26 * time.Hour)
	doc, err := h.store.GetMailDomain(t.Context(), acme.ID)
	if err != nil {
		t.Fatalf("GetMailDomain: %v", err)
	}
	if stats := h.service.deliveryStats(doc); stats != nil {
		t.Errorf("stats after 26 hours of silence = %v, want none", stats)
	}

	// One delivery from the mail server — even one carrying no event this
	// deployment counts — is the evidence the receiver is reached.
	if recorder := post(t, h.service.WebhookHandler(), `{"events":[]}`); recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	status, err = h.service.GetMailStatus(t.Context(), connect.NewRequest(&mailv1.GetMailStatusRequest{}))
	if err != nil {
		t.Fatalf("GetMailStatus: %v", err)
	}
	if !status.Msg.GetDeliveryStatsAvailable() {
		t.Error("delivery_stats_available is false after the mail server posted")
	}
	if status.Msg.GetDeliveryWindowHours() != 24 {
		t.Errorf("delivery_window_hours = %d, want 24", status.Msg.GetDeliveryWindowHours())
	}
	if status.Msg.GetDeliveryStatsNote() != DeliveryStatsWebhookNote {
		t.Errorf("note = %q, want the webhook copy", status.Msg.GetDeliveryStatsNote())
	}
	if stats := h.service.deliveryStats(doc); stats == nil || !stats.GetAvailable() {
		t.Error("a domain reports no stats although the receiver has been reached")
	}
}

// Deliveries that arrive and are refused are the other silent failure: the
// commonest cause is a signature key on the mail server that does not match
// MAIL_WEBHOOK_SECRET, and the console must say so rather than show nothing.
func TestRejectedDeliveriesAreReportedRatherThanCountedAsZero(t *testing.T) {
	h := newHarness(t, withWebhook)
	handler := h.service.WebhookHandler()
	acme := h.zoneNamed(t, "acme.dev")
	bind(t, h, acme, nil)

	body := deliveryBody(deliveryEventJSON("1", eventDelivered, "s@example.org", "ada@acme.dev", h.now()))
	wrongKey := hmac.New(sha256.New, []byte("the mail server's other signature key"))
	wrongKey.Write([]byte(body))
	for range 3 {
		recorder := post(t, handler, body, func(r *http.Request) {
			r.Header.Set("X-Signature", base64.StdEncoding.EncodeToString(wrongKey.Sum(nil)))
		})
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", recorder.Code)
		}
	}

	status, err := h.service.GetMailStatus(t.Context(), connect.NewRequest(&mailv1.GetMailStatusRequest{}))
	if err != nil {
		t.Fatalf("GetMailStatus: %v", err)
	}
	if status.Msg.GetDeliveryStatsAvailable() {
		t.Error("delivery_stats_available is true although every delivery was refused")
	}
	note := status.Msg.GetDeliveryStatsNote()
	for _, want := range []string{"3 deliveries", "signature didn't verify", "signature key"} {
		if !strings.Contains(note, want) {
			t.Errorf("note = %q, want it to mention %q", note, want)
		}
	}
	if strings.Contains(note, webhookSecret) {
		t.Error("the note leaked the secret")
	}

	// Each reason is throttled on its own, so a flood of one does not hide the
	// first sighting of another.
	h.logs.Reset()
	post(t, handler, body, func(r *http.Request) { r.Header.Del("Authorization") })
	if !strings.Contains(h.logs.String(), "reason=unauthorized") {
		t.Errorf("a new rejection reason was not logged: %q", h.logs.String())
	}
}

func TestMergeDeliveryHours(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	hour := func(offset int, delivered, bounced uint32) platform.MailDeliveryHourDoc {
		return platform.MailDeliveryHourDoc{
			Hour:      base.Add(time.Duration(offset) * time.Hour),
			Delivered: delivered,
			Bounced:   bounced,
		}
	}

	tests := []struct {
		name          string
		stored, added []platform.MailDeliveryHourDoc
		cutoff        time.Time
		want          []platform.MailDeliveryHourDoc
	}{
		{
			name:   "the same hour is summed",
			stored: []platform.MailDeliveryHourDoc{hour(0, 2, 1)},
			added:  []platform.MailDeliveryHourDoc{hour(0, 3, 0)},
			cutoff: base.Add(-time.Hour),
			want:   []platform.MailDeliveryHourDoc{hour(0, 5, 1)},
		},
		{
			name:   "hours are ordered oldest first",
			stored: []platform.MailDeliveryHourDoc{hour(2, 1, 0)},
			added:  []platform.MailDeliveryHourDoc{hour(0, 1, 0), hour(1, 1, 0)},
			cutoff: base.Add(-time.Hour),
			want:   []platform.MailDeliveryHourDoc{hour(0, 1, 0), hour(1, 1, 0), hour(2, 1, 0)},
		},
		{
			name:   "an hour before the cutoff is dropped",
			stored: []platform.MailDeliveryHourDoc{hour(-30, 9, 9), hour(0, 1, 0)},
			added:  nil,
			cutoff: base,
			want:   []platform.MailDeliveryHourDoc{hour(0, 1, 0)},
		},
		{
			name:   "nothing at all stays nothing",
			cutoff: base,
			want:   []platform.MailDeliveryHourDoc{},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := mergeDeliveryHours(test.stored, test.added, test.cutoff)
			if len(got) != len(test.want) {
				t.Fatalf("merged %v, want %v", got, test.want)
			}
			for index := range got {
				if got[index] != test.want[index] {
					t.Errorf("merged[%d] = %v, want %v", index, got[index], test.want[index])
				}
			}
		})
	}

	// However many hours arrive, the row stays bounded.
	var many []platform.MailDeliveryHourDoc
	for offset := range 200 {
		many = append(many, hour(offset, 1, 0))
	}
	if got := mergeDeliveryHours(nil, many, base.Add(-time.Hour)); len(got) != deliveryBuckets {
		t.Errorf("kept %d hours, want the bound of %d", len(got), deliveryBuckets)
	}
}

// assertDelivery checks one row's rolling counts through the same path the
// console reads them by.
func assertDelivery(t *testing.T, h *harness, zoneID string, delivered, bounced uint32) {
	t.Helper()
	doc, err := h.store.GetMailDomain(t.Context(), zoneID)
	if err != nil {
		t.Fatalf("GetMailDomain: %v", err)
	}
	stats := h.service.deliveryStats(doc)
	if stats == nil {
		t.Fatal("no delivery stats with a receiver configured")
	}
	if stats.GetDelivered() != delivered || stats.GetBounced() != bounced {
		t.Errorf("%s: delivered/bounced = %d/%d, want %d/%d",
			zoneID, stats.GetDelivered(), stats.GetBounced(), delivered, bounced)
	}
}

// The receiver runs on the HTTP goroutines and the flush on the reconciler's,
// so the two share the tally under -race.
func TestTheTallyIsSafeUnderConcurrentDeliveriesAndFlushes(t *testing.T) {
	h := newHarness(t, withWebhook)
	handler := h.service.WebhookHandler()
	acme := h.zoneNamed(t, "acme.dev")
	bind(t, h, acme, nil)

	var wait sync.WaitGroup
	for worker := range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for index := range 25 {
				body := deliveryBody(deliveryEventJSON(
					fmt.Sprint("w", worker, "-", index), eventDelivered, "s@example.org", "ada@acme.dev", h.now()))
				if recorder := post(t, handler, body); recorder.Code != http.StatusOK {
					t.Errorf("status = %d", recorder.Code)
					return
				}
			}
		}()
	}
	wait.Add(1)
	go func() {
		defer wait.Done()
		for range 20 {
			h.service.flushDeliveries(context.Background())
		}
	}()
	wait.Wait()
	h.service.flushDeliveries(t.Context())

	// Every one of the 200 distinct events is counted exactly once.
	assertDelivery(t, h, acme.ID, 200, 0)
}

// signatureKey is the second value a deployment may configure: the mail
// server's signatureKey, which on Stalwart is a setting of its own.
const signatureKey = "test-mail-webhook-signature-key-0123456789"

// withSignatureKey turns the receiver on with a signature key that is not the
// bearer, which is the configuration in which X-Signature is a second factor.
func withSignatureKey(cfg *config.Mail) {
	cfg.WebhookSecret = webhookSecret
	cfg.WebhookSignatureKey = signatureKey
}

// signWith replaces X-Signature with an HMAC keyed by key.
func signWith(key, body string) func(*http.Request) {
	return func(r *http.Request) {
		mac := hmac.New(sha256.New, []byte(key))
		mac.Write([]byte(body))
		r.Header.Set("X-Signature", base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	}
}

// With a signature key of its own, the bearer alone is no longer enough: a
// delivery signed with the bearer, or not signed at all, is refused, and only
// one carrying the second key is counted.
func TestASeparateSignatureKeyIsASecondFactor(t *testing.T) {
	h := newHarness(t, withSignatureKey)
	handler := h.service.WebhookHandler()
	acme := h.zoneNamed(t, "acme.dev")
	bind(t, h, acme, nil)

	body := deliveryBody(deliveryEventJSON("1", eventDelivered, "s@example.org", "ada@acme.dev", h.now()))

	// post() signs with the bearer, which is the wrong key here.
	if got := post(t, handler, body).Code; got != http.StatusUnauthorized {
		t.Errorf("a delivery signed with the bearer = %d, want 401", got)
	}
	if got := post(t, handler, body, func(r *http.Request) {
		r.Header.Del("X-Signature")
	}).Code; got != http.StatusUnauthorized {
		t.Errorf("an unsigned delivery = %d, want 401", got)
	}
	// Nothing has been accepted, so the status reports the refusals and the
	// remedy names the variable that has to match the mail server.
	status, err := h.service.GetMailStatus(t.Context(), connect.NewRequest(&mailv1.GetMailStatusRequest{}))
	if err != nil {
		t.Fatalf("GetMailStatus: %v", err)
	}
	note := status.Msg.GetDeliveryStatsNote()
	if status.Msg.GetDeliveryStatsAvailable() {
		t.Error("delivery_stats_available is true although every delivery was refused")
	}
	if !strings.Contains(note, "MAIL_WEBHOOK_SIGNATURE_KEY") {
		t.Errorf("note = %q, want it to name the signature key variable", note)
	}
	if strings.Contains(note, signatureKey) || strings.Contains(note, webhookSecret) {
		t.Error("the note leaked a secret")
	}

	if got := post(t, handler, body, signWith(signatureKey, body)).Code; got != http.StatusOK {
		t.Errorf("a delivery signed with the signature key = %d, want 200", got)
	}
	h.service.flushDeliveries(t.Context())
	assertDelivery(t, h, acme.ID, 1, 0)
}

// Without a second key the behaviour is the one the runbook documents: the
// bearer is the whole of the receiver's security, an unsigned delivery is
// accepted on it, and a signature keyed with the bearer verifies.
func TestWithoutASignatureKeyTheBearerIsTheWholeCheck(t *testing.T) {
	h := newHarness(t, withWebhook)
	handler := h.service.WebhookHandler()
	acme := h.zoneNamed(t, "acme.dev")
	bind(t, h, acme, nil)

	body := deliveryBody(deliveryEventJSON("1", eventDelivered, "s@example.org", "ada@acme.dev", h.now()))
	if got := post(t, handler, body, func(r *http.Request) {
		r.Header.Del("X-Signature")
	}).Code; got != http.StatusOK {
		t.Errorf("an unsigned delivery = %d, want 200", got)
	}
	if got := post(t, handler, deliveryBody(
		deliveryEventJSON("2", eventDelivered, "s@example.org", "ada@acme.dev", h.now()))).Code; got != http.StatusOK {
		t.Errorf("a delivery signed with the bearer = %d, want 200", got)
	}
	h.service.flushDeliveries(t.Context())
	assertDelivery(t, h, acme.ID, 2, 0)
}
