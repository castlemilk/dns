package billing

import (
	"context"
	"errors"
	"time"

	"github.com/castlemilk/dns/internal/activity"
	"github.com/castlemilk/dns/internal/billing/provider"
	"github.com/castlemilk/dns/internal/platform"
)

// expireInterval is how often a PENDING row past its checkout deadline is
// released. Stripe also sends checkout.session.expired; this is the local
// belt-and-braces so a domain never sits "Subscribing…" forever because one
// delivery was lost.
const (
	expireInterval = 5 * time.Minute
	expireBudget   = 20 * time.Second
)

func (s *Service) runExpirer(ctx context.Context) error {
	for {
		timer := time.NewTimer(expireInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
		if err := s.expireOnce(ctx); err != nil && ctx.Err() == nil {
			s.deps.Log().Warn("expire pending billing checkouts", "error", reason(err))
		}
	}
}

// expirePrefix marks the completion the expirer applies when the provider turns
// out to have completed a session this control plane had given up on. No
// delivery arrived, so the activity line is attributed to the reconciler rather
// than to Stripe.
const expirePrefix = "expire-"

// expireOnce settles every PENDING row whose checkout deadline has passed. The
// deadline only says our session should be over by now; whether the customer
// paid is the provider's to answer, so each overdue row is checked before it is
// released. Releasing on the local clock alone is how the append-only log ends
// up asserting that a checkout expired for a domain that is billed and charged.
func (s *Service) expireOnce(parent context.Context) error {
	if !s.configured() {
		return nil
	}
	ctx, cancel := context.WithTimeout(parent, expireBudget)
	defer cancel()

	docs, err := s.deps.Store.ListSubscriptions(ctx)
	if err != nil {
		return err
	}
	for _, doc := range docs {
		if ctx.Err() != nil {
			// Out of budget. The rest keep their PENDING state, which is the
			// truth — nothing has been decided about them — and the next sweep
			// picks them up.
			return nil
		}
		if !checkoutOverdue(doc, s.deps.Now()) {
			continue
		}
		if err := s.settleOverdueCheckout(ctx, doc.ZoneID); err != nil {
			// An unanswered provider is not an expiry. The row stays PENDING,
			// the console keeps saying "waiting for the payment", and the next
			// sweep asks again.
			s.deps.Log().Warn("check an overdue checkout with the billing provider",
				"zone_id", doc.ZoneID, "error", reason(err))
		}
	}
	return nil
}

// checkoutOverdue reports whether a row is still waiting on a checkout whose
// deadline has passed.
func checkoutOverdue(doc platform.SubscriptionDoc, now time.Time) bool {
	return doc.State == StatePending && doc.CheckoutExpiresAt != nil && !doc.CheckoutExpiresAt.After(now)
}

// settleOverdueCheckout asks the provider what became of one overdue session. A
// completed session is applied as the completion it is — the customer paid,
// however late the delivery was — and anything else releases the row.
func (s *Service) settleOverdueCheckout(ctx context.Context, zoneID string) error {
	doc, err := s.deps.Store.GetSubscription(ctx, zoneID)
	if err != nil {
		return err
	}
	if !checkoutOverdue(doc, s.deps.Now()) {
		return nil
	}
	if sessionID := doc.CheckoutSessionID; sessionID != "" {
		result, err := s.completeCheckout(ctx, Event{
			ID:      expirePrefix + sessionID,
			Type:    provider.EventCheckoutCompleted,
			Created: s.deps.Now(),
		}, sessionID)
		switch {
		case errors.Is(err, provider.ErrNotFound):
			// A definite answer: there is no such session, so it cannot have
			// been completed. Releasing the row states what is known.
		case err != nil:
			return err
		case result.Completed:
			// The provider had it completed all along: completeCheckout has
			// settled the row and recorded it. There is nothing to expire.
			return nil
		}
	}
	return s.releasePendingRow(ctx, zoneID)
}

// releasePendingRow puts an unpaid, overdue row back to UNBILLED. The domain
// keeps working; it simply has no subscription, which is what it never had.
func (s *Service) releasePendingRow(ctx context.Context, zoneID string) error {
	defer s.rows.lock(zoneID)()
	doc, err := s.deps.Store.GetSubscription(ctx, zoneID)
	if err != nil {
		return err
	}
	now := s.deps.Now()
	if !checkoutOverdue(doc, now) {
		return nil
	}
	sessionID := doc.CheckoutSessionID
	expireRow(&doc)
	doc.UpdatedAt = now
	if err := s.deps.Store.PutSubscription(ctx, doc); err != nil {
		return err
	}
	s.deps.Record(ctx, activity.Event{
		ZoneID: doc.ZoneID, ZoneName: doc.ZoneName,
		Actor: activity.ActorReconciler, Kind: activity.KindBillingCheckoutExpired,
		Severity: activity.SeverityInfo,
		Summary:  "Checkout for " + doc.ZoneName + " expired before it was completed",
		Details:  map[string]string{"stripe.session_id": sessionID},
	})
	return nil
}
