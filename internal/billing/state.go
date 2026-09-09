package billing

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	billingv1 "github.com/castlemilk/dns/gen/go/billing/v1"
	"github.com/castlemilk/dns/internal/activity"
	"github.com/castlemilk/dns/internal/billing/provider"
	"github.com/castlemilk/dns/internal/platform"
)

// Subscription states as they are persisted in SubscriptionDoc.State. They map
// one-to-one onto billing.v1.SubscriptionState.
const (
	StateUnbilled   = "UNBILLED"
	StatePending    = "PENDING"
	StateActive     = "ACTIVE"
	StateTrialing   = "TRIALING"
	StatePastDue    = "PAST_DUE"
	StateCanceling  = "CANCELING"
	StateCanceled   = "CANCELED"
	StateIncomplete = "INCOMPLETE"
)

// Outcomes recorded against a webhook event id.
const (
	outcomeDone    = platform.WebhookOutcomeDone
	outcomeStale   = platform.WebhookOutcomeStale
	outcomeIgnored = platform.WebhookOutcomeIgnored
)

// Event families. The per-zone monotonic guard is per family so a late invoice
// event can never mask a subscription transition, and vice versa.
const (
	familySubscription = "subscription"
	familyInvoice      = "invoice"
)

// reconcilePrefix marks the synthesised events the reconciler applies. They are
// never persisted in webhook_events: only a real delivery is deduplicated.
const reconcilePrefix = "reconcile-"

// subscriptionStateFor maps a Stripe subscription status onto the facade's
// state. cancel_at on an otherwise active subscription is CANCELING, which is
// the only state Stripe does not name itself.
func subscriptionStateFor(item Subscription) string {
	switch item.Status {
	case "active":
		if item.CancelAtPeriodEnd || !item.CancelAt.IsZero() {
			return StateCanceling
		}
		return StateActive
	case "trialing":
		if item.CancelAtPeriodEnd || !item.CancelAt.IsZero() {
			return StateCanceling
		}
		return StateTrialing
	case "past_due":
		return StatePastDue
	case "canceled":
		return StateCanceled
	case "incomplete", "incomplete_expired", "unpaid", "paused":
		return StateIncomplete
	default:
		return StateIncomplete
	}
}

// protoState maps a stored state onto the wire enum.
func protoState(state string) billingv1.SubscriptionState {
	switch state {
	case StateUnbilled, "":
		return billingv1.SubscriptionState_SUBSCRIPTION_STATE_UNBILLED
	case StatePending:
		return billingv1.SubscriptionState_SUBSCRIPTION_STATE_PENDING
	case StateActive:
		return billingv1.SubscriptionState_SUBSCRIPTION_STATE_ACTIVE
	case StateTrialing:
		return billingv1.SubscriptionState_SUBSCRIPTION_STATE_TRIALING
	case StatePastDue:
		return billingv1.SubscriptionState_SUBSCRIPTION_STATE_PAST_DUE
	case StateCanceling:
		return billingv1.SubscriptionState_SUBSCRIPTION_STATE_CANCELING
	case StateCanceled:
		return billingv1.SubscriptionState_SUBSCRIPTION_STATE_CANCELED
	default:
		return billingv1.SubscriptionState_SUBSCRIPTION_STATE_INCOMPLETE
	}
}

// applyEvent is how every delivery reaches the subscriptions bucket: the webhook
// queue, the reconciler and ConfirmCheckout all go through it, so a change is
// idempotent by event id and monotonic by event created time within its family.
// Those three run concurrently, and the expirer and CreateCheckoutSession write
// rows of their own, so each read-modify-write below takes that zone's lock
// (rowLocks) — the guards only hold if one row changes one pass at a time.
func (s *Service) applyEvent(ctx context.Context, event Event) (string, error) {
	switch event.Type {
	case provider.EventCheckoutCompleted:
		session, err := provider.DecodeCheckoutSession(event.Object)
		if err != nil {
			return "", err
		}
		result, err := s.completeCheckout(ctx, event, session.ID)
		return result.Outcome, err
	case provider.EventCheckoutExpired:
		session, err := provider.DecodeCheckoutSession(event.Object)
		if err != nil {
			return "", err
		}
		return s.applyCheckoutExpired(ctx, event, session)
	case provider.EventSubscriptionCreated, provider.EventSubscriptionUpdated, provider.EventSubscriptionDeleted:
		item, err := provider.DecodeSubscription(event.Object)
		if err != nil {
			return "", err
		}
		return s.applySubscription(ctx, event, item)
	case provider.EventInvoicePaid, provider.EventInvoicePaymentFailed:
		item, err := provider.DecodeInvoice(event.Object)
		if err != nil {
			return "", err
		}
		return s.applyInvoice(ctx, event, item)
	case provider.EventCustomerUpdated, provider.EventPaymentMethodAttached, provider.EventPaymentMethodDetached:
		return s.refreshPaymentMethod(ctx)
	default:
		// A type this facade did not subscribe to is acknowledged and dropped:
		// answering non-2xx would only make Stripe retry something nothing
		// here will ever act on.
		return outcomeIgnored, nil
	}
}

// applyReconcile applies a subscription the reconciler just read, as if Stripe
// had delivered a customer.subscription.updated for it.
func (s *Service) applyReconcile(ctx context.Context, zoneID string, item Subscription) (string, error) {
	event := Event{
		ID:      fmt.Sprintf("%s%d-%s", reconcilePrefix, s.deps.Now().UnixMilli(), zoneID),
		Type:    provider.EventSubscriptionUpdated,
		Created: s.deps.Now(),
	}
	return s.applySubscription(ctx, event, item)
}

// checkoutResult is what one checkout application produced. ConfirmCheckout
// answers the console from it; the webhook path only needs the outcome.
type checkoutResult struct {
	Doc             platform.SubscriptionDoc
	Outcome         string
	Completed       bool
	PaymentRequired bool
}

// completeCheckout is the shared body of checkout.session.completed and
// ConfirmCheckout: both re-read the session from the provider rather than
// trusting the payload, and both are safe to run twice.
func (s *Service) completeCheckout(ctx context.Context, event Event, sessionID string) (checkoutResult, error) {
	if sessionID == "" {
		return checkoutResult{Outcome: outcomeIgnored}, nil
	}
	started := s.deps.Now()
	session, err := s.provider.GetCheckout(ctx, sessionID)
	s.observe(ctx, "get_checkout", started, err)
	if err != nil {
		return checkoutResult{}, err
	}
	// no_payment_required means Stripe took nothing (a 100 % coupon, a trial
	// without a card); the console says so rather than implying a charge.
	paymentRequired := session.PaymentStatus != "no_payment_required"
	// Stripe creates the subscription only when the session completes, so an
	// open session is honestly "not finished yet" — which is what the success
	// page polls on.
	completed := session.Status == "complete" && session.PaymentStatus != "unpaid"

	zoneID, zoneName, found, err := s.resolveZone(ctx, checkoutZoneHints(session))
	if err != nil {
		return checkoutResult{}, err
	}
	if !found {
		s.recordIgnored(ctx, event, "checkout session")
		return checkoutResult{Outcome: outcomeIgnored, PaymentRequired: paymentRequired}, nil
	}

	defer s.rows.lock(zoneID)()
	doc, err := s.loadDoc(ctx, zoneID, zoneName)
	if err != nil {
		return checkoutResult{}, err
	}
	// The session's own created time is the guard, not the facade clock:
	// otherwise a ConfirmCheckout landing before the webhooks would mark the
	// real customer.subscription.created and invoice.paid events stale.
	created := session.Created
	if created.IsZero() {
		created = event.Created
	}
	if stale(doc, familySubscription, created) {
		return checkoutResult{Doc: doc, Outcome: outcomeStale, Completed: completed, PaymentRequired: paymentRequired}, nil
	}

	if !completed {
		// Not yet payable: leave the row PENDING and let the next delivery or
		// the success page's poll settle it.
		return checkoutResult{Doc: doc, Outcome: outcomeDone, PaymentRequired: paymentRequired}, nil
	}

	// A completion can be applied twice — ConfirmCheckout and the webhook both
	// run it — so the row write stays idempotent while the activity line is
	// written once, by whoever got there first.
	alreadySettled := s.checkoutSettled(ctx, session.ID)
	doc.CustomerID = session.CustomerID
	doc.CheckoutSessionID = ""
	doc.CheckoutExpiresAt = nil
	if session.SubscriptionID != "" {
		subStarted := s.deps.Now()
		item, subErr := s.provider.GetSubscription(ctx, session.SubscriptionID)
		s.observe(ctx, "get_subscription", subStarted, subErr)
		if subErr != nil {
			return checkoutResult{}, subErr
		}
		applySubscriptionToDoc(&doc, item)
		s.storeCustomer(ctx, item.CustomerID, item.PaymentMethod)
	}
	mark(&doc, familySubscription, created)
	doc.LastEventID = event.ID
	doc.UpdatedAt = s.deps.Now()
	if err := s.deps.Store.PutSubscription(ctx, doc); err != nil {
		return checkoutResult{}, err
	}
	s.markCheckoutCompleted(ctx, session.ID)

	if !alreadySettled {
		s.deps.Record(ctx, activity.Event{
			ZoneID: doc.ZoneID, ZoneName: doc.ZoneName,
			Actor: s.actorFor(event), Kind: activity.KindBillingCheckoutCompleted,
			Severity: activity.SeverityInfo,
			Summary:  fmt.Sprintf("Checkout completed — %s is subscribed", doc.ZoneName),
			Details: map[string]string{
				"stripe.event_id":        event.ID,
				"stripe.subscription_id": doc.SubscriptionID,
				"stripe.status":          doc.State,
			},
			CorrelationID: event.ID,
		})
	}
	return checkoutResult{Doc: doc, Outcome: outcomeDone, Completed: true, PaymentRequired: paymentRequired}, nil
}

func (s *Service) applyCheckoutExpired(ctx context.Context, event Event, session CheckoutSession) (string, error) {
	zoneID, zoneName, found, err := s.resolveZone(ctx, checkoutZoneHints(session))
	if err != nil {
		return "", err
	}
	if !found {
		s.recordIgnored(ctx, event, "checkout session")
		return outcomeIgnored, nil
	}
	defer s.rows.lock(zoneID)()
	doc, err := s.loadDoc(ctx, zoneID, zoneName)
	if err != nil {
		return "", err
	}
	if stale(doc, familySubscription, event.Created) {
		return outcomeStale, nil
	}
	if doc.State != StatePending {
		return outcomeDone, nil
	}
	expireRow(&doc)
	mark(&doc, familySubscription, event.Created)
	doc.LastEventID = event.ID
	doc.UpdatedAt = s.deps.Now()
	if err := s.deps.Store.PutSubscription(ctx, doc); err != nil {
		return "", err
	}
	s.recordExpired(ctx, doc, event.ID, s.actorFor(event))
	return outcomeDone, nil
}

func (s *Service) applySubscription(ctx context.Context, event Event, item Subscription) (string, error) {
	zoneID, zoneName, found, err := s.resolveZone(ctx, subscriptionZoneHints(item))
	if err != nil {
		return "", err
	}
	if !found {
		s.recordIgnored(ctx, event, "subscription")
		return outcomeIgnored, nil
	}
	defer s.rows.lock(zoneID)()
	doc, err := s.loadDoc(ctx, zoneID, zoneName)
	if err != nil {
		return "", err
	}
	deleted := event.Type == provider.EventSubscriptionDeleted
	// An event names a subscription; the row tracks one. They have to be the
	// same subscription before the event is allowed to move the row.
	//
	// Without this the event bound to the zone alone, so a delete for a
	// superseded subscription rewrote a live, paid row: Stripe kept charging
	// A$10 a month on the real subscription while the console showed the domain
	// CANCELED, and the portal button pointed at the dead id, so the operator
	// could not stop the billing they could see. The delete path made it worse
	// by design — cancellation deliberately bypasses the staleness guard below,
	// so an event older than everything already applied still won.
	//
	// A row that tracks nothing yet, or one whose subscription has already
	// ended, may legitimately be claimed by a new subscription: that is a
	// customer resubscribing. A row pointing at a live subscription may not.
	if doc.SubscriptionID != "" && item.ID != "" && item.ID != doc.SubscriptionID &&
		doc.State != StateCanceled && doc.State != StateUnbilled {
		s.recordIgnored(ctx, event, "subscription")
		return outcomeIgnored, nil
	}
	// A cancellation is terminal, so it wins even when it arrives out of
	// order: nothing that happened before it can revive the subscription.
	if !deleted && stale(doc, familySubscription, event.Created) {
		return outcomeStale, nil
	}

	before := doc.State
	applySubscriptionToDoc(&doc, item)
	if deleted {
		doc.State = StateCanceled
	}
	if !stale(doc, familySubscription, event.Created) {
		mark(&doc, familySubscription, event.Created)
	}
	doc.LastEventID = event.ID
	doc.UpdatedAt = s.deps.Now()
	if err := s.deps.Store.PutSubscription(ctx, doc); err != nil {
		return "", err
	}
	if item.PaymentMethod != nil {
		s.storeCustomer(ctx, item.CustomerID, item.PaymentMethod)
	}

	kind := activity.KindBillingSubscriptionUpdated
	summary := fmt.Sprintf("Subscription for %s is %s", doc.ZoneName, humanState(doc))
	if doc.State == StateCanceled {
		kind = activity.KindBillingSubscriptionCanceled
		summary = fmt.Sprintf("Subscription for %s is canceled", doc.ZoneName)
	}
	if before != doc.State || event.Type != provider.EventSubscriptionUpdated {
		s.deps.Record(ctx, activity.Event{
			ZoneID: doc.ZoneID, ZoneName: doc.ZoneName,
			Actor: s.actorFor(event), Kind: kind, Severity: severityFor(doc.State),
			Summary: summary,
			Details: map[string]string{
				"stripe.event_id":        event.ID,
				"stripe.subscription_id": doc.SubscriptionID,
				"stripe.status":          doc.State,
			},
			CorrelationID: event.ID,
		})
	}
	return outcomeDone, nil
}

func (s *Service) applyInvoice(ctx context.Context, event Event, item InvoiceEvent) (string, error) {
	zoneID, zoneName, found, err := s.resolveZone(ctx, invoiceZoneHints(item))
	if err != nil {
		return "", err
	}
	if !found {
		s.recordIgnored(ctx, event, "invoice")
		return outcomeIgnored, nil
	}
	defer s.rows.lock(zoneID)()
	doc, err := s.loadDoc(ctx, zoneID, zoneName)
	if err != nil {
		return "", err
	}
	if stale(doc, familyInvoice, event.Created) {
		return outcomeStale, nil
	}

	// An invoice always tells us the truth about a payment, so the invoice
	// status is recorded whatever else has happened. It may only speak for the
	// subscription's own state while the subscription family has nothing newer
	// to say and the row is not already terminal. Stripe emits
	// invoice.payment_failed and customer.subscription.deleted seconds apart
	// during a dunning cancellation and orders neither, and this facade's own
	// retry back-off can defer one past the other, so without this guard a late
	// invoice would resurrect a canceled subscription as PAST_DUE — the state
	// the console badges "Attention" for a domain with no subscription at all.
	speaksForSubscription := !stale(doc, familySubscription, event.Created) && !terminalState(doc.State)

	paid := event.Type == provider.EventInvoicePaid
	if paid {
		doc.LastInvoiceStatus = "paid"
		if speaksForSubscription && !item.PeriodEnd.IsZero() &&
			(doc.CurrentPeriodEnd == nil || item.PeriodEnd.After(*doc.CurrentPeriodEnd)) {
			end := item.PeriodEnd
			doc.CurrentPeriodEnd = &end
		}
	} else {
		doc.LastInvoiceStatus = "failed"
		if speaksForSubscription {
			doc.State = StatePastDue
		}
	}
	if doc.SubscriptionID == "" {
		doc.SubscriptionID = item.SubscriptionID
	}
	if doc.CustomerID == "" {
		doc.CustomerID = item.CustomerID
	}
	mark(&doc, familyInvoice, event.Created)
	doc.LastEventID = event.ID
	doc.UpdatedAt = s.deps.Now()
	if err := s.deps.Store.PutSubscription(ctx, doc); err != nil {
		return "", err
	}

	kind := activity.KindBillingInvoicePaid
	severity := activity.SeverityInfo
	summary := fmt.Sprintf("Invoice paid for %s", doc.ZoneName)
	if !paid {
		kind = activity.KindBillingInvoiceFailed
		severity = activity.SeverityWarn
		summary = fmt.Sprintf("Payment failed for %s", doc.ZoneName)
	}
	s.deps.Record(ctx, activity.Event{
		ZoneID: doc.ZoneID, ZoneName: doc.ZoneName,
		Actor: s.actorFor(event), Kind: kind, Severity: severity,
		Summary: summary,
		Details: map[string]string{
			"stripe.event_id":        event.ID,
			"stripe.subscription_id": doc.SubscriptionID,
			"stripe.status":          doc.State,
		},
		CorrelationID: event.ID,
	})
	return outcomeDone, nil
}

// refreshPaymentMethod re-reads the card the customer would be charged on. It
// asks for the customer rather than walking subscriptions, for two reasons: the
// customer's default payment method is what "the card on file" means, and one
// answered read is the only thing that can tell "there is no card" from "we did
// not look" — which is what lets a removed card be cleared instead of shown
// forever. A failed read returns the error so the queue retries it; it never
// silently leaves the stored card standing for a customer who has none.
func (s *Service) refreshPaymentMethod(ctx context.Context) (string, error) {
	stored, found, err := s.deps.Store.GetCustomer(ctx)
	if err != nil {
		return "", err
	}
	if !found || stored.CustomerID == "" {
		// No customer has been created yet, so there is nothing this event can
		// be about.
		return outcomeIgnored, nil
	}
	started := s.deps.Now()
	live, err := s.provider.GetCustomer(ctx, stored.CustomerID)
	s.observe(ctx, "get_customer", started, err)
	if err != nil {
		return "", err
	}
	s.setPaymentMethod(ctx, stored.CustomerID, live.PaymentMethod)
	return outcomeDone, nil
}

// --- row helpers ----------------------------------------------------------

func applySubscriptionToDoc(doc *platform.SubscriptionDoc, item Subscription) {
	doc.SubscriptionID = item.ID
	if item.CustomerID != "" {
		doc.CustomerID = item.CustomerID
	}
	doc.State = subscriptionStateFor(item)
	doc.CheckoutSessionID = ""
	doc.CheckoutExpiresAt = nil
	if !item.CurrentPeriodEnd.IsZero() {
		end := item.CurrentPeriodEnd
		doc.CurrentPeriodEnd = &end
	}
	if !item.CancelAt.IsZero() {
		cancel := item.CancelAt
		doc.CancelAt = &cancel
	} else {
		doc.CancelAt = nil
	}
}

// terminalState reports the states a subscription cannot leave on its own. A
// canceled subscription no longer exists at the provider: only a new checkout
// can move the row, so nothing an invoice or a payment says may.
func terminalState(state string) bool { return state == StateCanceled }

// expireRow releases a PENDING row: the domain keeps working, it simply has no
// subscription again.
func expireRow(doc *platform.SubscriptionDoc) {
	doc.State = StateUnbilled
	doc.CheckoutSessionID = ""
	doc.CheckoutExpiresAt = nil
}

// loadDoc reads the row for a zone, creating an UNBILLED one when the zone has
// never been billed.
func (s *Service) loadDoc(ctx context.Context, zoneID, zoneName string) (platform.SubscriptionDoc, error) {
	doc, err := s.deps.Store.GetSubscription(ctx, zoneID)
	if err != nil && !errors.Is(err, platform.ErrNotFound) {
		return platform.SubscriptionDoc{}, err
	}
	if errors.Is(err, platform.ErrNotFound) {
		doc = platform.SubscriptionDoc{
			V:         platform.DocVersion,
			ZoneID:    zoneID,
			State:     StateUnbilled,
			UpdatedAt: s.deps.Now(),
		}
	}
	doc.V = platform.DocVersion
	if zoneName != "" {
		doc.ZoneName = zoneName
	}
	return doc, nil
}

// rowLocks serializes the read-modify-write on one zone's row. applyEvent is
// reached from the queue worker, the reconciler and ConfirmCheckout, and the
// expirer and CreateCheckoutSession write rows of their own, so the single
// writer the state machine assumes has to be enforced rather than asserted: the
// store makes each write atomic, not the load-decide-store sequence around it.
// The lock is per zone, so two domains never wait on each other, and the map
// holds one entry per zone this deployment has ever billed.
type rowLocks struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// lock takes one zone's lock and returns its release, so a caller can write
// `defer s.rows.lock(zoneID)()`.
func (l *rowLocks) lock(zoneID string) func() {
	l.mu.Lock()
	if l.locks == nil {
		l.locks = make(map[string]*sync.Mutex)
	}
	row, ok := l.locks[zoneID]
	if !ok {
		row = &sync.Mutex{}
		l.locks[zoneID] = row
	}
	l.mu.Unlock()
	row.Lock()
	return row.Unlock
}

func stale(doc platform.SubscriptionDoc, family string, created time.Time) bool {
	if created.IsZero() {
		return false
	}
	switch family {
	case familyInvoice:
		return created.Unix() < doc.LastInvoiceEventCreated
	default:
		return created.Unix() < doc.LastSubscriptionEventCreated
	}
}

func mark(doc *platform.SubscriptionDoc, family string, created time.Time) {
	if created.IsZero() {
		return
	}
	switch family {
	case familyInvoice:
		doc.LastInvoiceEventCreated = created.Unix()
	default:
		doc.LastSubscriptionEventCreated = created.Unix()
	}
}

// checkoutSettled reports whether a session this control plane created has
// already been applied.
func (s *Service) checkoutSettled(ctx context.Context, sessionID string) bool {
	doc, err := s.deps.Store.GetCheckout(ctx, sessionID)
	return err == nil && doc.Completed
}

// markCheckoutCompleted records that a session this control plane created has
// been settled, so the expirer leaves it alone.
func (s *Service) markCheckoutCompleted(ctx context.Context, sessionID string) {
	doc, err := s.deps.Store.GetCheckout(ctx, sessionID)
	if err != nil {
		return
	}
	if doc.Completed {
		return
	}
	doc.Completed = true
	if err := s.deps.Store.PutCheckout(ctx, doc); err != nil {
		s.deps.Log().Warn("record a completed checkout session", "error", err)
	}
}

// setPaymentMethod records what the provider has just said the customer's card
// is, including that there is none. storeCustomer deliberately cannot do this:
// everywhere else a nil card means "this object does not carry one", and
// overwriting a real card with that would invent a state.
func (s *Service) setPaymentMethod(ctx context.Context, customerID string, method *PaymentMethod) {
	doc, _, err := s.deps.Store.GetCustomer(ctx)
	if err != nil {
		s.deps.Log().Warn("read the billing customer", "error", err)
		return
	}
	doc.V = platform.DocVersion
	if customerID != "" {
		doc.CustomerID = customerID
	}
	if doc.Email == "" {
		doc.Email = s.cfg.CustomerEmail
	}
	doc.PaymentMethod = platform.PaymentMethodDoc{}
	if method != nil {
		doc.PaymentMethod = platform.PaymentMethodDoc{
			Brand:    method.Brand,
			Last4:    method.Last4,
			ExpMonth: method.ExpMonth,
			ExpYear:  method.ExpYear,
		}
	}
	doc.UpdatedAt = s.deps.Now()
	if err := s.deps.Store.PutCustomer(ctx, doc); err != nil {
		s.deps.Log().Warn("store the billing customer", "error", err)
	}
}

// storeCustomer records the customer id, and the card when the object being
// applied carried one. A nil method means "this object says nothing about the
// card", so the stored one is left alone; setPaymentMethod is what clears it.
func (s *Service) storeCustomer(ctx context.Context, customerID string, method *PaymentMethod) {
	if customerID == "" && method == nil {
		return
	}
	doc, _, err := s.deps.Store.GetCustomer(ctx)
	if err != nil {
		s.deps.Log().Warn("read the billing customer", "error", err)
		return
	}
	doc.V = platform.DocVersion
	if customerID != "" {
		doc.CustomerID = customerID
	}
	if doc.Email == "" {
		doc.Email = s.cfg.CustomerEmail
	}
	if method != nil {
		doc.PaymentMethod = platform.PaymentMethodDoc{
			Brand:    method.Brand,
			Last4:    method.Last4,
			ExpMonth: method.ExpMonth,
			ExpYear:  method.ExpYear,
		}
	}
	doc.UpdatedAt = s.deps.Now()
	if err := s.deps.Store.PutCustomer(ctx, doc); err != nil {
		s.deps.Log().Warn("store the billing customer", "error", err)
	}
}

// --- zone resolution ------------------------------------------------------

// zoneHints is everything an event tells us about which zone it belongs to, in
// the order the spec resolves them.
type zoneHints struct {
	metadataZoneID    string
	clientReferenceID string
	subscriptionID    string
	sessionID         string
	domain            string
}

func checkoutZoneHints(session CheckoutSession) zoneHints {
	return zoneHints{
		metadataZoneID:    session.Metadata["zone_id"],
		clientReferenceID: session.ClientReferenceID,
		subscriptionID:    session.SubscriptionID,
		sessionID:         session.ID,
		domain:            session.Metadata["domain"],
	}
}

func subscriptionZoneHints(item Subscription) zoneHints {
	return zoneHints{
		metadataZoneID: item.Metadata["zone_id"],
		subscriptionID: item.ID,
		domain:         item.Metadata["domain"],
	}
}

func invoiceZoneHints(item InvoiceEvent) zoneHints {
	return zoneHints{
		metadataZoneID: item.Metadata["zone_id"],
		subscriptionID: item.SubscriptionID,
		domain:         item.Metadata["domain"],
	}
}

// resolveZone finds the zone an event belongs to. It reports found=false for an
// event about a zone this control plane does not know, which is answered 2xx
// and recorded rather than retried forever.
func (s *Service) resolveZone(ctx context.Context, hints zoneHints) (string, string, bool, error) {
	for _, candidate := range []string{hints.metadataZoneID, hints.clientReferenceID} {
		if candidate == "" {
			continue
		}
		if name, ok, err := s.zoneName(ctx, candidate); err != nil {
			return "", "", false, err
		} else if ok {
			return candidate, name, true, nil
		}
		// The zone is gone but the row may still exist, so the operator can
		// still see and cancel the subscription.
		if _, err := s.deps.Store.GetSubscription(ctx, candidate); err == nil {
			return candidate, hints.domain, true, nil
		}
	}
	if hints.sessionID != "" {
		if doc, err := s.deps.Store.GetCheckout(ctx, hints.sessionID); err == nil && doc.ZoneID != "" {
			name, _, err := s.zoneName(ctx, doc.ZoneID)
			if err != nil {
				return "", "", false, err
			}
			return doc.ZoneID, name, true, nil
		}
	}
	if hints.subscriptionID != "" {
		docs, err := s.deps.Store.ListSubscriptions(ctx)
		if err != nil {
			return "", "", false, err
		}
		for _, doc := range docs {
			if doc.SubscriptionID == hints.subscriptionID {
				name, _, err := s.zoneName(ctx, doc.ZoneID)
				if err != nil {
					return "", "", false, err
				}
				if name == "" {
					name = doc.ZoneName
				}
				return doc.ZoneID, name, true, nil
			}
		}
	}
	return "", "", false, nil
}

// zoneName reads a zone's name. A missing zone is not an error: the row is kept
// so the operator can still cancel the subscription in the portal.
func (s *Service) zoneName(ctx context.Context, zoneID string) (string, bool, error) {
	if s.deps.Zones == nil || zoneID == "" {
		return "", false, nil
	}
	value, err := s.deps.Zones.GetZone(ctx, zoneID)
	if err == nil {
		return value.Name, true, nil
	}
	if connect.CodeOf(err) == connect.CodeNotFound {
		return "", false, nil
	}
	return "", false, err
}

func (s *Service) recordIgnored(ctx context.Context, event Event, object string) {
	s.deps.Record(ctx, activity.Event{
		Actor:    s.actorFor(event),
		Kind:     activity.KindBillingEventIgnored,
		Severity: activity.SeverityInfo,
		Summary:  fmt.Sprintf("Ignored a billing %s for a domain this control plane does not manage", object),
		Details: map[string]string{
			"stripe.event_id": event.ID,
			"stripe.type":     event.Type,
		},
		CorrelationID: event.ID,
	})
}

func (s *Service) recordExpired(ctx context.Context, doc platform.SubscriptionDoc, eventID, actor string) {
	s.deps.Record(ctx, activity.Event{
		ZoneID: doc.ZoneID, ZoneName: doc.ZoneName,
		Actor: actor, Kind: activity.KindBillingCheckoutExpired,
		Severity:      activity.SeverityInfo,
		Summary:       fmt.Sprintf("Checkout for %s expired before it was completed", doc.ZoneName),
		Details:       map[string]string{"stripe.event_id": eventID},
		CorrelationID: eventID,
	})
}

// actorFor names who caused a change. Only a real delivery is attributed to
// Stripe; the events the reconciler and ConfirmCheckout synthesise carry a
// prefix so the activity log stays honest about who acted.
func (s *Service) actorFor(event Event) string {
	switch {
	case strings.HasPrefix(event.ID, reconcilePrefix), strings.HasPrefix(event.ID, expirePrefix), event.ID == "":
		return activity.ActorReconciler
	case strings.HasPrefix(event.ID, confirmPrefix):
		return activity.ActorOperator
	default:
		return activity.ActorBillingWebhook
	}
}

func severityFor(state string) activity.Severity {
	switch state {
	case StatePastDue, StateIncomplete:
		return activity.SeverityWarn
	default:
		return activity.SeverityInfo
	}
}

// humanState is the sentence fragment used in an activity summary.
func humanState(doc platform.SubscriptionDoc) string {
	switch doc.State {
	case StateActive, StateTrialing:
		if doc.CurrentPeriodEnd != nil {
			return fmt.Sprintf("%s until %s", lower(doc.State), doc.CurrentPeriodEnd.Format("2 Jan 2006"))
		}
		return lower(doc.State)
	case StateCanceling:
		if doc.CancelAt != nil {
			return fmt.Sprintf("canceling on %s", doc.CancelAt.Format("2 Jan 2006"))
		}
		return "canceling"
	case StatePastDue:
		return "past due"
	default:
		return lower(doc.State)
	}
}

func lower(state string) string {
	switch state {
	case StateActive:
		return "active"
	case StateTrialing:
		return "trialing"
	case StateCanceled:
		return "canceled"
	case StateIncomplete:
		return "incomplete"
	case StateUnbilled:
		return "unbilled"
	case StatePending:
		return "pending"
	default:
		return state
	}
}
