package billing

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	billingv1 "github.com/castlemilk/dns/gen/go/billing/v1"
	"github.com/castlemilk/dns/internal/billing/provider"
	"github.com/castlemilk/dns/internal/platform"
)

func TestSubscriptionStateForMapsEveryStripeStatus(t *testing.T) {
	t.Parallel()
	future := time.Now().Add(24 * time.Hour)
	cases := []struct {
		name string
		item Subscription
		want string
	}{
		{"active", Subscription{Status: "active"}, StateActive},
		{"trialing", Subscription{Status: "trialing"}, StateTrialing},
		{"past due", Subscription{Status: "past_due"}, StatePastDue},
		{"canceled", Subscription{Status: "canceled"}, StateCanceled},
		{"incomplete", Subscription{Status: "incomplete"}, StateIncomplete},
		{"incomplete expired", Subscription{Status: "incomplete_expired"}, StateIncomplete},
		{"unpaid", Subscription{Status: "unpaid"}, StateIncomplete},
		{"paused", Subscription{Status: "paused"}, StateIncomplete},
		{"unknown", Subscription{Status: "who_knows"}, StateIncomplete},
		{"cancelling at period end", Subscription{Status: "active", CancelAtPeriodEnd: true}, StateCanceling},
		{"cancelling at a date", Subscription{Status: "active", CancelAt: future}, StateCanceling},
		{"trial cancelling", Subscription{Status: "trialing", CancelAtPeriodEnd: true}, StateCanceling},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := subscriptionStateFor(testCase.item); got != testCase.want {
				t.Fatalf("state = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestProtoStateCoversEveryStoredState(t *testing.T) {
	t.Parallel()
	cases := map[string]billingv1.SubscriptionState{
		"":              billingv1.SubscriptionState_SUBSCRIPTION_STATE_UNBILLED,
		StateUnbilled:   billingv1.SubscriptionState_SUBSCRIPTION_STATE_UNBILLED,
		StatePending:    billingv1.SubscriptionState_SUBSCRIPTION_STATE_PENDING,
		StateActive:     billingv1.SubscriptionState_SUBSCRIPTION_STATE_ACTIVE,
		StateTrialing:   billingv1.SubscriptionState_SUBSCRIPTION_STATE_TRIALING,
		StatePastDue:    billingv1.SubscriptionState_SUBSCRIPTION_STATE_PAST_DUE,
		StateCanceling:  billingv1.SubscriptionState_SUBSCRIPTION_STATE_CANCELING,
		StateCanceled:   billingv1.SubscriptionState_SUBSCRIPTION_STATE_CANCELED,
		StateIncomplete: billingv1.SubscriptionState_SUBSCRIPTION_STATE_INCOMPLETE,
	}
	for state, want := range cases {
		if got := protoState(state); got != want {
			t.Errorf("protoState(%q) = %v, want %v", state, got, want)
		}
	}
}

func TestInvoiceAndSubscriptionEventsGuardEachOtherSeparately(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	ctx := context.Background()
	base := h.clock.Now()

	// The invoice arrives first, carrying a later created time than the
	// subscription event that follows it. A single guard would drop the
	// subscription event; per-family guards keep both.
	paid := Event{
		ID: "evt_invoice", Type: provider.EventInvoicePaid, Created: base.Add(time.Minute),
		Object: []byte(invoiceObject("in_1", "paid", "sub_1", base.Add(30*24*time.Hour))),
	}
	if outcome, err := h.service.applyEvent(ctx, paid); err != nil || outcome != outcomeDone {
		t.Fatalf("invoice.paid = (%q, %v)", outcome, err)
	}
	created := Event{
		ID: "evt_subscription", Type: provider.EventSubscriptionCreated, Created: base,
		Object: []byte(subscriptionObject("sub_1", "active", base.Add(30*24*time.Hour))),
	}
	if outcome, err := h.service.applyEvent(ctx, created); err != nil || outcome != outcomeDone {
		t.Fatalf("customer.subscription.created = (%q, %v)", outcome, err)
	}

	doc := h.subscription("z1")
	if doc.State != StateActive || doc.LastInvoiceStatus != "paid" || doc.SubscriptionID != "sub_1" {
		t.Fatalf("row = %#v", doc)
	}
}

func TestAnOlderSubscriptionEventIsRecordedStale(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	ctx := context.Background()
	now := h.clock.Now()

	if err := h.store.PutSubscription(ctx, platform.SubscriptionDoc{
		V: platform.DocVersion, ZoneID: "z1", ZoneName: "acme.dev", SubscriptionID: "sub_1",
		State: StateActive, LastSubscriptionEventCreated: now.Add(time.Hour).Unix(), UpdatedAt: now,
	}); err != nil {
		t.Fatalf("PutSubscription: %v", err)
	}

	outcome, err := h.service.applyEvent(ctx, Event{
		ID: "evt_late", Type: provider.EventSubscriptionUpdated, Created: now,
		Object: []byte(subscriptionObject("sub_1", "past_due", now)),
	})
	if err != nil {
		t.Fatalf("applyEvent: %v", err)
	}
	if outcome != outcomeStale {
		t.Fatalf("outcome = %q, want stale", outcome)
	}
	if state := h.subscription("z1").State; state != StateActive {
		t.Fatalf("a stale event changed the row: state = %q", state)
	}
}

func TestACancellationAlwaysWinsHoweverLateItArrives(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	ctx := context.Background()
	now := h.clock.Now()

	if err := h.store.PutSubscription(ctx, platform.SubscriptionDoc{
		V: platform.DocVersion, ZoneID: "z1", ZoneName: "acme.dev", SubscriptionID: "sub_1",
		State: StateActive, LastSubscriptionEventCreated: now.Add(time.Hour).Unix(), UpdatedAt: now,
	}); err != nil {
		t.Fatalf("PutSubscription: %v", err)
	}

	outcome, err := h.service.applyEvent(ctx, Event{
		ID: "evt_deleted", Type: provider.EventSubscriptionDeleted, Created: now.Add(-time.Hour),
		Object: []byte(subscriptionObject("sub_1", "canceled", now)),
	})
	if err != nil {
		t.Fatalf("applyEvent: %v", err)
	}
	if outcome != outcomeDone {
		t.Fatalf("outcome = %q, want done", outcome)
	}
	if state := h.subscription("z1").State; state != StateCanceled {
		t.Fatalf("state = %q, want CANCELED", state)
	}
}

func TestAFailedInvoiceMovesTheRowToPastDue(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	ctx := context.Background()
	now := h.clock.Now()

	if _, err := h.service.applyEvent(ctx, Event{
		ID: "evt_created", Type: provider.EventSubscriptionCreated, Created: now,
		Object: []byte(subscriptionObject("sub_1", "active", now.Add(30*24*time.Hour))),
	}); err != nil {
		t.Fatalf("applyEvent (created): %v", err)
	}
	if _, err := h.service.applyEvent(ctx, Event{
		ID: "evt_failed", Type: provider.EventInvoicePaymentFailed, Created: now.Add(time.Minute),
		Object: []byte(invoiceObject("in_2", "open", "sub_1", now.Add(30*24*time.Hour))),
	}); err != nil {
		t.Fatalf("applyEvent (failed): %v", err)
	}
	doc := h.subscription("z1")
	if doc.State != StatePastDue || doc.LastInvoiceStatus != "failed" {
		t.Fatalf("row = %#v", doc)
	}
}

func TestALateInvoiceCannotResurrectACanceledSubscription(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	ctx := context.Background()
	now := h.clock.Now()

	if _, err := h.service.applyEvent(ctx, Event{
		ID: "evt_created", Type: provider.EventSubscriptionCreated, Created: now,
		Object: []byte(subscriptionObject("sub_1", "active", now.Add(30*24*time.Hour))),
	}); err != nil {
		t.Fatalf("applyEvent (created): %v", err)
	}
	// Stripe emits invoice.payment_failed and customer.subscription.deleted
	// seconds apart during a dunning cancellation and orders neither; this
	// facade's own retry back-off can defer one past the other too.
	if _, err := h.service.applyEvent(ctx, Event{
		ID: "evt_deleted", Type: provider.EventSubscriptionDeleted, Created: now.Add(2 * time.Minute),
		Object: []byte(subscriptionObject("sub_1", "canceled", now.Add(30*24*time.Hour))),
	}); err != nil {
		t.Fatalf("applyEvent (deleted): %v", err)
	}
	if _, err := h.service.applyEvent(ctx, Event{
		ID: "evt_failed", Type: provider.EventInvoicePaymentFailed, Created: now.Add(time.Minute),
		Object: []byte(invoiceObject("in_1", "open", "sub_1", now.Add(30*24*time.Hour))),
	}); err != nil {
		t.Fatalf("applyEvent (late failed invoice): %v", err)
	}

	doc := h.subscription("z1")
	if doc.State != StateCanceled {
		t.Fatalf("state = %q, want CANCELED: an invoice cannot revive a canceled subscription", doc.State)
	}
	// The payment itself is still recorded: it did fail, and saying so is not
	// the same as claiming the subscription is past due.
	if doc.LastInvoiceStatus != "failed" {
		t.Fatalf("last invoice status = %q, want failed", doc.LastInvoiceStatus)
	}
	summary, err := h.service.GetBillingSummary(ctx, newSummaryRequest())
	if err != nil {
		t.Fatalf("GetBillingSummary: %v", err)
	}
	if summary.Msg.GetAttentionCount() != 0 {
		t.Fatalf("attention count = %d: a domain with no subscription was badged",
			summary.Msg.GetAttentionCount())
	}

	// The mirror: a late payment must not extend a canceled subscription's
	// period either.
	before := *doc.CurrentPeriodEnd
	if _, err := h.service.applyEvent(ctx, Event{
		ID: "evt_paid", Type: provider.EventInvoicePaid, Created: now.Add(90 * time.Second),
		Object: []byte(invoiceObject("in_2", "paid", "sub_1", now.Add(365*24*time.Hour))),
	}); err != nil {
		t.Fatalf("applyEvent (late paid invoice): %v", err)
	}
	after := h.subscription("z1")
	if after.State != StateCanceled || !after.CurrentPeriodEnd.Equal(before) {
		t.Fatalf("row after a late payment = %#v, want CANCELED until %v", after, before)
	}
}

func TestAFailedInvoiceIsNotAppliedOverANewerSubscriptionEvent(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	ctx := context.Background()
	now := h.clock.Now()

	// The subscription was reinstated at T+1h; a failed invoice from T is
	// recorded as the payment history it is, without moving the state Stripe
	// has since told us about.
	if err := h.store.PutSubscription(ctx, platform.SubscriptionDoc{
		V: platform.DocVersion, ZoneID: "z1", ZoneName: "acme.dev", SubscriptionID: "sub_1",
		State: StateActive, LastSubscriptionEventCreated: now.Add(time.Hour).Unix(), UpdatedAt: now,
	}); err != nil {
		t.Fatalf("PutSubscription: %v", err)
	}
	if _, err := h.service.applyEvent(ctx, Event{
		ID: "evt_failed", Type: provider.EventInvoicePaymentFailed, Created: now,
		Object: []byte(invoiceObject("in_1", "open", "sub_1", now.Add(30*24*time.Hour))),
	}); err != nil {
		t.Fatalf("applyEvent: %v", err)
	}
	doc := h.subscription("z1")
	if doc.State != StateActive || doc.LastInvoiceStatus != "failed" {
		t.Fatalf("row = %#v, want ACTIVE with a failed last invoice", doc)
	}
}

func TestConcurrentApplicationsCannotLoseACancellation(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	ctx := context.Background()
	base := h.clock.Now()

	// applyEvent is reached from three goroutines in production — the queue
	// worker, the reconciler and ConfirmCheckout — so "one writer" has to hold
	// when two of them land on the same row at once. Without the per-row lock
	// the two load-modify-store passes interleave and whichever writes last
	// wins with what it read, quietly undoing the cancellation.
	for round := range 25 {
		if err := h.store.PutSubscription(ctx, platform.SubscriptionDoc{
			V: platform.DocVersion, ZoneID: "z1", ZoneName: "acme.dev", SubscriptionID: "sub_1",
			State: StateActive, UpdatedAt: base,
		}); err != nil {
			t.Fatalf("PutSubscription: %v", err)
		}
		var wait sync.WaitGroup
		wait.Add(2)
		go func() {
			defer wait.Done()
			if _, err := h.service.applyEvent(ctx, Event{
				ID: fmt.Sprintf("evt_deleted_%d", round), Type: provider.EventSubscriptionDeleted,
				Created: base.Add(2 * time.Minute),
				Object:  []byte(subscriptionObject("sub_1", "canceled", base.Add(30*24*time.Hour))),
			}); err != nil {
				t.Errorf("applyEvent (deleted): %v", err)
			}
		}()
		go func() {
			defer wait.Done()
			if _, err := h.service.applyEvent(ctx, Event{
				ID: fmt.Sprintf("evt_paid_%d", round), Type: provider.EventInvoicePaid,
				Created: base.Add(time.Minute),
				Object:  []byte(invoiceObject(fmt.Sprintf("in_%d", round), "paid", "sub_1", base.Add(30*24*time.Hour))),
			}); err != nil {
				t.Errorf("applyEvent (paid): %v", err)
			}
		}()
		wait.Wait()
		if state := h.subscription("z1").State; state != StateCanceled {
			t.Fatalf("round %d: state = %q, want CANCELED", round, state)
		}
	}
}

func TestCustomerUpdatedRefreshesTheStoredCard(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	ctx := context.Background()

	created, err := h.service.CreateCheckoutSession(ctx, newCheckoutRequest("z1"))
	if err != nil {
		t.Fatalf("CreateCheckoutSession: %v", err)
	}
	h.completeCheckoutInBrowser(created.Msg.GetUrl())
	h.drain()
	doc := h.subscription("z1")

	customer, _, err := h.store.GetCustomer(ctx)
	if err != nil {
		t.Fatalf("GetCustomer: %v", err)
	}
	if customer.PaymentMethod.Last4 != "" {
		t.Fatalf("a card was recorded before one was attached: %#v", customer.PaymentMethod)
	}

	h.portal(doc.CustomerID, doc.SubscriptionID, "update_card")
	h.drain()

	customer, found, err := h.store.GetCustomer(ctx)
	if err != nil || !found {
		t.Fatalf("GetCustomer = (%v, %v)", found, err)
	}
	if customer.PaymentMethod.Brand != "visa" || customer.PaymentMethod.Last4 != "4242" {
		t.Fatalf("payment method = %#v", customer.PaymentMethod)
	}

	summary, err := h.service.GetBillingSummary(ctx, newSummaryRequest())
	if err != nil {
		t.Fatalf("GetBillingSummary: %v", err)
	}
	if summary.Msg.GetPaymentMethod().GetLast4() != "4242" {
		t.Fatalf("summary payment method = %#v", summary.Msg.GetPaymentMethod())
	}
}

func TestResolveZoneFallsBackThroughEveryHint(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	ctx := context.Background()

	if err := h.store.PutSubscription(ctx, platform.SubscriptionDoc{
		V: platform.DocVersion, ZoneID: "z1", ZoneName: "acme.dev", SubscriptionID: "sub_known",
		State: StateActive, UpdatedAt: h.clock.Now(),
	}); err != nil {
		t.Fatalf("PutSubscription: %v", err)
	}
	if err := h.store.PutCheckout(ctx, platform.CheckoutDoc{
		V: platform.DocVersion, SessionID: "cs_known0001", ZoneID: "z1", CreatedAt: h.clock.Now(),
	}); err != nil {
		t.Fatalf("PutCheckout: %v", err)
	}

	cases := []struct {
		name  string
		hints zoneHints
		want  string
		found bool
	}{
		{"metadata", zoneHints{metadataZoneID: "z1"}, "z1", true},
		{"client reference", zoneHints{clientReferenceID: "z1"}, "z1", true},
		{"checkout session", zoneHints{sessionID: "cs_known0001"}, "z1", true},
		{"subscription id", zoneHints{subscriptionID: "sub_known"}, "z1", true},
		{"nothing known", zoneHints{metadataZoneID: "z-other", subscriptionID: "sub_other"}, "", false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			zoneID, name, found, err := h.service.resolveZone(ctx, testCase.hints)
			if err != nil {
				t.Fatalf("resolveZone: %v", err)
			}
			if found != testCase.found || zoneID != testCase.want {
				t.Fatalf("resolveZone = (%q, %q, %v)", zoneID, name, found)
			}
			if found && name != "acme.dev" {
				t.Fatalf("zone name = %q", name)
			}
		})
	}
}

func TestARowWhoseZoneIsGoneIsStillResolvable(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	ctx := context.Background()

	if err := h.store.PutSubscription(ctx, platform.SubscriptionDoc{
		V: platform.DocVersion, ZoneID: "z1", ZoneName: "acme.dev", SubscriptionID: "sub_1",
		State: StateActive, UpdatedAt: h.clock.Now(),
	}); err != nil {
		t.Fatalf("PutSubscription: %v", err)
	}
	h.zones.remove("z1")

	// The domain is gone but the subscription is not: the operator still has to
	// be able to see and cancel it.
	if _, err := h.service.applyEvent(ctx, Event{
		ID: "evt_gone", Type: provider.EventSubscriptionDeleted, Created: h.clock.Now(),
		Object: []byte(subscriptionObject("sub_1", "canceled", h.clock.Now())),
	}); err != nil {
		t.Fatalf("applyEvent: %v", err)
	}
	doc := h.subscription("z1")
	if doc.State != StateCanceled || doc.ZoneName != "acme.dev" {
		t.Fatalf("row = %#v", doc)
	}
}

func TestHumanStateReadsAsASentence(t *testing.T) {
	t.Parallel()
	end := time.Date(2026, time.October, 3, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		doc  platform.SubscriptionDoc
		want string
	}{
		{platform.SubscriptionDoc{State: StateActive, CurrentPeriodEnd: &end}, "active until 3 Oct 2026"},
		{platform.SubscriptionDoc{State: StateActive}, "active"},
		{platform.SubscriptionDoc{State: StateCanceling, CancelAt: &end}, "canceling on 3 Oct 2026"},
		{platform.SubscriptionDoc{State: StateCanceling}, "canceling"},
		{platform.SubscriptionDoc{State: StatePastDue}, "past due"},
		{platform.SubscriptionDoc{State: StateIncomplete}, "incomplete"},
	}
	for _, testCase := range cases {
		if got := humanState(testCase.doc); got != testCase.want {
			t.Errorf("humanState(%q) = %q, want %q", testCase.doc.State, got, testCase.want)
		}
	}
}

// TestADeadSubscriptionCannotCancelALiveOne covers an event binding to the zone
// instead of to the subscription the row is actually tracking.
//
// applySubscription resolved the zone from the event's metadata and then
// overwrote the row from the event, with no check that the event's subscription
// is the one the row holds. A delete for a superseded subscription therefore
// rewrote a live, paid row — and the delete path deliberately bypasses the
// staleness guard, so even an event older than everything already applied won.
//
// Stripe then keeps charging A$10 a month on the real subscription while the
// console shows the domain CANCELED, and the portal button points at the dead
// id, so the operator cannot stop the billing they can see.
func TestADeadSubscriptionCannotCancelALiveOne(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	ctx := context.Background()

	// Get the zone to a real, paid subscription.
	created, err := h.service.CreateCheckoutSession(ctx, connect.NewRequest(&billingv1.CreateCheckoutSessionRequest{
		ZoneId: "z1", ReturnPath: "/billing",
	}))
	if err != nil {
		t.Fatalf("CreateCheckoutSession: %v", err)
	}
	h.completeCheckoutInBrowser(created.Msg.GetUrl())
	h.drain()

	live := h.subscription("z1")
	if live.State != StateActive || live.SubscriptionID == "" {
		t.Fatalf("row is not active before the test: %#v", live)
	}

	// A delete arrives for a DIFFERENT subscription that happens to carry the
	// same zone in its metadata — a subscription this row superseded.
	superseded := Subscription{
		ID:         live.SubscriptionID + "-old",
		CustomerID: live.CustomerID,
		Status:     "canceled",
		Metadata:   map[string]string{"zone_id": "z1"},
	}
	outcome, err := h.service.applySubscription(ctx,
		Event{ID: "evt_stale_delete", Type: provider.EventSubscriptionDeleted, Created: h.clock.Now()},
		superseded)
	if err != nil {
		t.Fatalf("applySubscription: %v", err)
	}
	if outcome != outcomeIgnored {
		t.Errorf("outcome = %q, want %q — an event for another subscription must not move this row", outcome, outcomeIgnored)
	}

	after := h.subscription("z1")
	if after.State != StateActive {
		t.Errorf("state = %q after a delete for a superseded subscription, want ACTIVE — Stripe is still charging for it", after.State)
	}
	if after.SubscriptionID != live.SubscriptionID {
		t.Errorf("SubscriptionID = %q, want %q — the portal button would now aim at a dead subscription",
			after.SubscriptionID, live.SubscriptionID)
	}
}

// TestTheRealSubscriptionCanStillBeCanceled is the other half: the guard must
// not stop a genuine cancellation of the subscription the row tracks.
func TestTheRealSubscriptionCanStillBeCanceled(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	ctx := context.Background()

	created, err := h.service.CreateCheckoutSession(ctx, connect.NewRequest(&billingv1.CreateCheckoutSessionRequest{
		ZoneId: "z1", ReturnPath: "/billing",
	}))
	if err != nil {
		t.Fatalf("CreateCheckoutSession: %v", err)
	}
	h.completeCheckoutInBrowser(created.Msg.GetUrl())
	h.drain()
	live := h.subscription("z1")

	if _, err := h.service.applySubscription(ctx,
		Event{ID: "evt_real_delete", Type: provider.EventSubscriptionDeleted, Created: h.clock.Now()},
		Subscription{
			ID:         live.SubscriptionID,
			CustomerID: live.CustomerID,
			Status:     "canceled",
			Metadata:   map[string]string{"zone_id": "z1"},
		}); err != nil {
		t.Fatalf("applySubscription: %v", err)
	}
	if got := h.subscription("z1").State; got != StateCanceled {
		t.Errorf("state = %q after cancelling the tracked subscription, want CANCELED", got)
	}
}
