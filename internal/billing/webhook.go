package billing

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/castlemilk/dns/internal/activity"
	"github.com/stripe/stripe-go/v86/webhook"
)

// WebhookBodyLimit is the largest delivery this route accepts. internal/app
// applies the same cap outside; the handler applies it again so it is correct
// wherever it is mounted.
const WebhookBodyLimit = 256 << 10

// Rate limits (§6.4). There is deliberately no per-source-IP bucket: behind a
// gateway every delivery shares one RemoteAddr and X-Forwarded-For is
// spoofable, so an IP bucket would let unsigned traffic starve Stripe's real
// deliveries.
//
// This is the only unauthenticated route on the control plane, and it is
// exposed on the public gateway, so the bound has to be on the work an
// anonymous caller can cause — reading up to WebhookBodyLimit and computing an
// HMAC per configured secret — not merely on what the answer is labelled. The
// admission bucket is therefore taken before the body is read, and a delivery
// that goes on to verify has its token refunded, so genuine traffic is bounded
// only by the much larger verified bucket. A caller that finds the bucket empty
// is answered 429, which Stripe retries with back-off; the control plane
// staying up is worth more than a delivery arriving on its first attempt, and
// the reconciler converges whatever a retry does not.
const (
	// admissionPerMinute is far above Stripe's real rate for one deployment
	// (a handful of deliveries a minute) and still a fraction of the control
	// container's CPU limit at the maximum body size.
	admissionPerMinute = 1200
	verifiedPerMinute  = 600
	// verifyConcurrency bounds how many bodies are being read and verified at
	// once. The bucket caps the rate of that work; this caps how much of it can
	// happen simultaneously, so a burst cannot occupy a control plane whose
	// whole CPU limit is half a core.
	verifyConcurrency = 4
	// rejectEventInterval bounds how often a rejection is written to the
	// activity log, so a flood produces one line a minute rather than 1 000.
	rejectEventInterval = time.Minute
)

// Webhook outcomes, as counted by deephost.billing.webhooks.
const (
	outcomeAccepted           = "accepted"
	outcomeDuplicate          = "duplicate"
	outcomeBadSignature       = "bad_signature"
	outcomeTooOld             = "too_old"
	outcomeNotSigned          = "not_signed"
	outcomeInvalidHeader      = "invalid_header"
	outcomeAPIVersionMismatch = "api_version_mismatch"
	outcomeRateLimited        = "rate_limited"
	outcomeLivemodeMismatch   = "livemode_mismatch"
	outcomeOversize           = "oversize"
	outcomeOriginRejected     = "origin_rejected"
	outcomeUnsupportedMedia   = "unsupported_media"
	outcomeProcessingError    = "processing_error"
)

// webhookHandler serves POST /billing/v1/stripe/webhook. It is unauthenticated
// by necessity — Stripe cannot present the operator bearer — so the signature
// is the only credential, and it is checked before anything else costs work.
type webhookHandler struct {
	service   *Service
	admission *bucket
	verified  *bucket
	slots     chan struct{}

	mu           sync.Mutex
	lastRejectAt time.Time
}

func newWebhookHandler(service *Service) *webhookHandler {
	now := service.deps.Now
	return &webhookHandler{
		service:   service,
		admission: newBucket(admissionPerMinute, admissionPerMinute, now),
		verified:  newBucket(verifiedPerMinute, verifiedPerMinute, now),
		slots:     make(chan struct{}, verifyConcurrency),
	}
}

func (h *webhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		h.answer(ctx, w, http.StatusMethodNotAllowed, outcomeProcessingError, "", -1)
		return
	}
	// Stripe never sends an Origin header. A request that does is a browser,
	// which means someone is trying to drive this route from a page.
	if r.Header.Get("Origin") != "" {
		h.answer(ctx, w, http.StatusForbidden, outcomeOriginRejected, "", -1)
		return
	}
	if mediaType := r.Header.Get("Content-Type"); !strings.HasPrefix(strings.ToLower(mediaType), "application/json") {
		h.answer(ctx, w, http.StatusUnsupportedMediaType, outcomeUnsupportedMedia, "", -1)
		return
	}
	if r.ContentLength > WebhookBodyLimit {
		h.answer(ctx, w, http.StatusRequestEntityTooLarge, outcomeOversize, "", -1)
		return
	}

	// Everything above this line costs a header lookup. Everything below it
	// costs a read and an HMAC, so the admission token is taken here, before
	// the body is touched, and given back once the delivery has verified.
	if !h.admission.allow() {
		h.recordRejection(r, outcomeRateLimited)
		h.answer(ctx, w, http.StatusTooManyRequests, outcomeRateLimited, "", -1)
		return
	}
	release, entered := h.enter(ctx)
	if !entered {
		h.answer(ctx, w, http.StatusTooManyRequests, outcomeRateLimited, "", -1)
		return
	}
	defer release()

	// The raw body is read in full before anything parses it: the signature is
	// computed over exactly these bytes.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, WebhookBodyLimit))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			h.answer(ctx, w, http.StatusRequestEntityTooLarge, outcomeOversize, "", -1)
			return
		}
		h.answer(ctx, w, http.StatusBadRequest, outcomeProcessingError, "", -1)
		return
	}

	event, secretIndex, err := h.service.provider.ConstructEvent(body, r.Header.Get("Stripe-Signature"))
	if err != nil {
		outcome := signatureOutcome(err)
		h.recordRejection(r, outcome)
		h.answer(ctx, w, http.StatusBadRequest, outcome, "", secretIndex)
		return
	}
	// This delivery was signed by a secret this deployment configured, so it
	// did not cost an anonymous caller anything: give the token back.
	h.admission.refund()
	// A signed delivery still has to belong to the same world as the key. A
	// test-mode endpoint secret is shaped exactly like a live one, so an
	// operator can paste the wrong one during a rotation and have test-mode
	// objects applied to live rows while the Settings page reads "Mode: live".
	// Refusing here is what makes that visible instead of silent.
	if event.Livemode != h.service.livemode() {
		h.recordRejection(r, outcomeLivemodeMismatch)
		h.answer(ctx, w, http.StatusBadRequest, outcomeLivemodeMismatch, event.Type, secretIndex)
		return
	}
	if !h.verified.allow() {
		// Far above Stripe's real rate: 429 tells it to retry rather than
		// silently dropping a genuine delivery.
		h.answer(ctx, w, http.StatusTooManyRequests, outcomeRateLimited, event.Type, secretIndex)
		return
	}

	queued, err := h.service.deps.Store.EnqueueWebhook(ctx, event.ID, event.Type, body, h.service.deps.Now())
	if err != nil {
		h.service.deps.Log().Error("queue a stripe webhook", "event_id", event.ID, "event_type", event.Type, "error", err)
		h.answer(ctx, w, http.StatusInternalServerError, outcomeProcessingError, event.Type, secretIndex)
		return
	}
	if !queued {
		h.answer(ctx, w, http.StatusOK, outcomeDuplicate, event.Type, secretIndex)
		return
	}
	h.service.deps.Log().Info("queued a stripe webhook",
		"event_id", event.ID, "event_type", event.Type, "secret_index", secretIndex)
	h.service.wakeQueue()
	h.answer(ctx, w, http.StatusOK, outcomeAccepted, event.Type, secretIndex)
}

// enter takes one of the verification slots, waiting until the request is
// abandoned rather than growing the amount of work in flight. It reports false
// when the caller gave up first, in which case nothing was read.
func (h *webhookHandler) enter(ctx context.Context) (func(), bool) {
	select {
	case h.slots <- struct{}{}:
		return func() { <-h.slots }, true
	case <-ctx.Done():
		return nil, false
	}
}

// answer writes the JSON body and counts the outcome. The response never echoes
// any part of the payload.
func (h *webhookHandler) answer(ctx context.Context, w http.ResponseWriter, status int, outcome, eventType string, secretIndex int) {
	h.service.deps.Meter().BillingWebhook(ctx, outcome, eventType, secretIndex)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	// The body is a fixed status word; the payload is never echoed.
	if _, err := w.Write(statusBody(outcome)); err != nil {
		h.service.deps.Log().Debug("write a webhook response", "error", err)
	}
}

// statusBody is what the response says. Stripe ignores it; an operator reading
// a request log does not.
func statusBody(outcome string) []byte {
	status := outcome
	switch outcome {
	case outcomeAccepted:
		status = "queued"
	case outcomeDuplicate:
		status = "duplicate"
	}
	// The status words are fixed identifiers, so this cannot fail to encode.
	body, err := json.Marshal(map[string]string{"status": status})
	if err != nil {
		return []byte(`{"status":"processing_error"}`)
	}
	return body
}

// recordRejection writes at most one warn event a minute, carrying the reason
// and nothing from the payload.
func (h *webhookHandler) recordRejection(r *http.Request, outcome string) {
	now := h.service.deps.Now()
	h.mu.Lock()
	if !h.lastRejectAt.IsZero() && now.Sub(h.lastRejectAt) < rejectEventInterval {
		h.mu.Unlock()
		return
	}
	h.lastRejectAt = now
	h.mu.Unlock()

	h.service.deps.Record(r.Context(), activity.Event{
		Actor:    activity.ActorBillingWebhook,
		Kind:     activity.KindBillingWebhookRejected,
		Severity: activity.SeverityWarn,
		Summary:  "Refused a Stripe webhook delivery: " + outcome,
		Details:  map[string]string{"reason": outcome},
	})
}

// signatureOutcome maps the SDK's sentinel errors onto bounded metric values.
func signatureOutcome(err error) string {
	switch {
	case errors.Is(err, webhook.ErrNotSigned):
		return outcomeNotSigned
	case errors.Is(err, webhook.ErrInvalidHeader):
		return outcomeInvalidHeader
	case errors.Is(err, webhook.ErrTooOld):
		return outcomeTooOld
	case errors.Is(err, webhook.ErrNoValidSignature):
		return outcomeBadSignature
	case errors.Is(err, ErrAPIVersionMismatch):
		return outcomeAPIVersionMismatch
	default:
		// A signed but unparsable body, or a thin event notification sent to
		// the snapshot endpoint. It is not a signature failure, but it is not
		// something a retry will fix either.
		return outcomeProcessingError
	}
}

// bucket is a token bucket with a per-minute refill. It is small enough to keep
// here rather than pulling in a dependency, and it takes its clock from the
// facade so a test can drive it deterministically.
type bucket struct {
	mu        sync.Mutex
	tokens    float64
	capacity  float64
	perSecond float64
	last      time.Time
	now       func() time.Time
}

func newBucket(capacity, perMinute int, now func() time.Time) *bucket {
	return &bucket{
		tokens:    float64(capacity),
		capacity:  float64(capacity),
		perSecond: float64(perMinute) / 60,
		now:       now,
	}
}

// refund returns a token taken by allow. It is how work that turned out to be
// legitimate stops counting against the bound on anonymous work.
func (b *bucket) refund() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tokens = min(b.capacity, b.tokens+1)
}

// allow takes one token, reporting false when the bucket is empty.
func (b *bucket) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	if b.last.IsZero() {
		b.last = now
	}
	if elapsed := now.Sub(b.last); elapsed > 0 {
		b.tokens = min(b.capacity, b.tokens+elapsed.Seconds()*b.perSecond)
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
