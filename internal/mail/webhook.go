package mail

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// WebhookBodyLimit is the largest delivery this route accepts. The mail server
// batches its events, and a batch of a few hundred is well under this;
// internal/app applies the same cap outside, and the handler applies it again
// so it is correct wherever it is mounted.
const WebhookBodyLimit = 1 << 20

// webhookMaxEvents bounds one delivery. A body inside the size limit cannot
// hold many more than this, but the parse loop is bounded explicitly so a
// pathological body cannot make the handler do unbounded work.
const webhookMaxEvents = 5000

// webhookSeenLimit bounds the replay set. The mail server's event ids are
// monotonic per process; keeping the last few thousand is enough to absorb the
// retry of a delivery this control plane already counted, and the set is
// dropped wholesale when it fills rather than being aged one entry at a time.
const webhookSeenLimit = 8192

// webhookRejectInterval bounds how often a rejected delivery is logged, so a
// flood produces one line a minute rather than thousands. The throttle is per
// reason (deliveryReceiver.reject): a flood of unauthenticated noise must not
// swallow the first sighting of a signature that does not verify, which is the
// shape a misconfigured webhook takes.
const webhookRejectInterval = time.Minute

// WebhookHandler serves POST /mail/v1/delivery-events, or nil when the route
// must not exist at all.
//
// The route is mounted only when MAIL_WEBHOOK_SECRET is set. A deployment that
// has not configured it has no receiver, no counters and the unchanged
// "delivery counts aren't available" copy — which is the honest state and the
// one every existing deployment stays in.
//
// It is also not mounted when the engine could not be built at all: the flush
// that drains the counters into platform.db runs on the reconciler's goroutine,
// which does not start without an engine, so a route here would accept events
// and never store them.
func (s *Service) WebhookHandler() http.Handler {
	if !s.cfg.DeliveryEventsConfigured() || s.engine == nil {
		return nil
	}
	return &webhookHandler{service: s}
}

type webhookHandler struct {
	service *Service

	mu   sync.Mutex
	seen map[string]struct{}
}

// deliveryEnvelope is the body the mail server posts: a batch of events, each
// with an id, a type and a flat data object.
type deliveryEnvelope struct {
	Events []deliveryEvent `json:"events"`
}

type deliveryEvent struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"createdAt"`
	Type      string    `json:"type"`
	Data      struct {
		// From is the envelope sender, which is the only address these counts
		// are attributed by; see countedZone.
		From string `json:"from"`
	} `json:"data"`
}

func (h *webhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// The mail server is not a browser. A request carrying an Origin is a page
	// trying to drive this route, and no page ever should.
	if r.Header.Get("Origin") != "" {
		h.reject(w, http.StatusForbidden, "origin_rejected")
		return
	}
	if r.ContentLength > WebhookBodyLimit {
		h.reject(w, http.StatusRequestEntityTooLarge, "oversize")
		return
	}

	// The bearer is checked before the body is read, so an unauthenticated
	// caller never gets this process to allocate a megabyte.
	if !h.authorized(r) {
		h.reject(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, WebhookBodyLimit+1))
	if err != nil {
		h.reject(w, http.StatusBadRequest, "unreadable")
		return
	}
	if len(body) > WebhookBodyLimit {
		h.reject(w, http.StatusRequestEntityTooLarge, "oversize")
		return
	}
	// The signature is a second factor only when MAIL_WEBHOOK_SIGNATURE_KEY
	// gives it a key of its own — Stalwart's signatureKey is a setting separate
	// from its httpAuth, so the two can differ. With that key set, an unsigned
	// delivery is refused: the header is the proof the caller holds the second
	// value, and accepting a delivery without it would make the second value
	// optional and therefore worth nothing.
	//
	// Without it, this deployment has one value for both settings, the HMAC is
	// keyed with the bearer this request already presented, and a signature
	// proves nothing further — so an unsigned delivery is accepted, and a wrong
	// one is still refused, because that says the two sides disagree about the
	// secret, which is the diagnosis GetMailStatus reports. docs/mail-runbook.md,
	// "Wiring it up", says which of the two a deployment is in.
	signature := r.Header.Get("X-Signature")
	if signature == "" && h.service.cfg.RequireSignature() {
		h.reject(w, http.StatusUnauthorized, "missing_signature")
		return
	}
	if signature != "" && !h.signatureValid(signature, body) {
		h.reject(w, http.StatusUnauthorized, "bad_signature")
		return
	}

	var envelope deliveryEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		h.reject(w, http.StatusBadRequest, "malformed")
		return
	}
	h.count(r, envelope.Events)

	// 200 with no body: the mail server discards a delivery it could not make
	// after discardAfter, and there is nothing for it to read back.
	w.WriteHeader(http.StatusOK)
}

// authorized compares the bearer in constant time. The token is never logged
// and never appears in a response.
func (h *webhookHandler) authorized(r *http.Request) bool {
	header := r.Header.Get("Authorization")
	token, ok := strings.CutPrefix(header, "Bearer ")
	if !ok {
		return false
	}
	want := h.service.cfg.WebhookSecret
	return subtle.ConstantTimeCompare([]byte(strings.TrimSpace(token)), []byte(want)) == 1
}

// signatureValid checks the mail server's X-Signature: base64 of
// HMAC-SHA256(signatureKey, body), verified against v0.16.20. The key is
// MAIL_WEBHOOK_SIGNATURE_KEY when this deployment sets one, and otherwise the
// bearer, because that one value then stands in for both of the mail server's
// settings — see ServeHTTP for what each of the two does and does not buy.
func (h *webhookHandler) signatureValid(signature string, body []byte) bool {
	expected, err := base64.StdEncoding.DecodeString(strings.TrimSpace(signature))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(h.service.cfg.SignatureKey()))
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), expected)
}

// count attributes the two counted event types to the bound zone that sent the
// message.
//
// An event names addresses, not zones, so the bound rows are read once per
// delivery and the sender's domain is looked up in that map. Anything that
// resolves to no bound zone is dropped without a trace: this control plane does
// not keep statistics about domains it does not host.
func (h *webhookHandler) count(r *http.Request, events []deliveryEvent) {
	service := h.service
	// The delivery was authenticated, within the size limit and well formed, so
	// the mail server has demonstrably reached this receiver. That is the
	// evidence deliveryStats needs before it will report any figure at all, and
	// it is recorded whether or not this particular batch concerns a domain
	// this deployment hosts.
	service.receiver.accept(service.deps.Now())
	if len(events) == 0 {
		return
	}
	if len(events) > webhookMaxEvents {
		events = events[:webhookMaxEvents]
	}
	docs, err := service.deps.Store.ListMailDomains(r.Context())
	if err != nil {
		service.deps.Log().Warn("read the bound mail domains for a delivery event", "error", err)
		return
	}
	byName := make(map[string]string, len(docs))
	for _, doc := range docs {
		byName[strings.ToLower(doc.ZoneName)] = doc.ZoneID
	}

	now := service.deps.Now()
	// The first hour deliveryStats will report. An event dated before it has to
	// be counted at arrival or not at all, because an hour outside the reported
	// window is summed by nobody.
	since := service.countingSince(now.UTC())
	for _, event := range events {
		var delivered bool
		switch event.Type {
		case eventDelivered:
			delivered = true
		case eventBounced:
		default:
			continue
		}
		if !h.firstSighting(event.ID) {
			continue
		}
		at := event.CreatedAt
		// An event with no timestamp, or one dated outside the window this
		// process reports, is counted at the moment it arrived rather than
		// dropped: the delivery itself is the evidence, and a stale-dated event
		// would otherwise be tallied, flushed to platform.db and then summed by
		// nothing — an accepted delivery that shows up as a zero.
		//
		// Stale is the ordinary case rather than the exotic one. The mail server
		// buffers and retries a webhook it could not deliver, so the deliveries
		// that arrive in the first minutes after this process starts routinely
		// carry a createdAt from before it did.
		if at.IsZero() || at.After(now.Add(time.Hour)) || at.UTC().Before(since) {
			at = now
		}
		if zoneID := countedZone(byName, event); zoneID != "" {
			service.deliveries.add(zoneID, at, delivered)
		}
	}
}

// countedZone is the bound zone one event counts towards: the zone of the
// address that sent the message, or "" when this deployment did not send it.
//
// Attribution is deliberately one-sided, and the reason is arithmetic rather
// than taste. The two counted events are not symmetric between the directions:
// a message delivered to a hosted mailbox produces delivery.dsn-success, but a
// message to a local address that does not exist is refused at RCPT TO with
// smtp.mailbox-does-not-exist and is never queued, so it produces no counted
// event at all (docs/mail-runbook.md, "Delivery statistics"). Counting the
// recipient side as well would therefore add every inbound success and no
// inbound failure to the same pair of numbers a reader takes for a sending
// record: 500 newsletters received and one of three sent messages bounced would
// read as "500 delivered · 1 bounced", a 0.2% bounce rate for a domain whose
// real one is 33%.
//
// So delivered/bounced is the mail this domain sent, which is the figure the
// pair is read as. Mail received is not counted here at all rather than folded
// into a number that cannot carry it; DeliveryStats has no way to say which
// direction it is describing, so there is nothing to label a mixed figure with.
func countedZone(byName map[string]string, event deliveryEvent) string {
	// "<>" is the null return path of a bounce notification; it belongs to no
	// domain and must not be attributed to one.
	from := strings.TrimSpace(event.Data.From)
	if from == "" || from == "<>" {
		return ""
	}
	return zoneIDForAddress(byName, from)
}

// firstSighting reports whether this event id has not been counted yet. The
// mail server retries a delivery it could not complete, so without this a
// retried batch would be counted twice.
func (h *webhookHandler) firstSighting(id string) bool {
	if id == "" {
		// An event with no id cannot be deduplicated. Counting it is the lesser
		// error: the server always sets one, so this is a shape that should not
		// occur rather than a retry that would double.
		return true
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.seen == nil {
		h.seen = make(map[string]struct{}, webhookSeenLimit)
	}
	if _, ok := h.seen[id]; ok {
		return false
	}
	if len(h.seen) >= webhookSeenLimit {
		h.seen = make(map[string]struct{}, webhookSeenLimit)
	}
	h.seen[id] = struct{}{}
	return true
}

// reject answers a refused delivery, counts it against its reason and logs at
// most one line a minute for that reason. The reason is a bounded word, never
// the caller's input.
//
// The tally is not only for the log: with a secret set and no delivery ever
// accepted, GetMailStatus reports the reason deliveries are being refused, so a
// bearer or signature key that does not match this control plane's says so on
// the page instead of hiding in a log line.
func (h *webhookHandler) reject(w http.ResponseWriter, status int, reason string) {
	count, report := h.service.receiver.reject(reason, h.service.deps.Now())
	if report {
		h.service.deps.Log().Warn("rejected a mail delivery event", "reason", reason, "rejected", count)
	}
	http.Error(w, reason, status)
}
