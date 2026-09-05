package billing

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/castlemilk/dns/internal/activity"
)

// Reconciler cadence. Six hours is far longer than Stripe's own retry horizon
// for a single delivery, so this is a safety net for a long outage rather than
// the primary path — webhooks are.
const (
	reconcileInterval = 6 * time.Hour
	// reconcileBudget bounds one pass, which is one provider call per billed
	// zone.
	reconcileBudget = 2 * time.Minute
)

// runReconciler converges every row that carries a subscription id. It also
// runs when the prober sees billing come back, so an outage that swallowed a
// delivery is repaired without an operator doing anything.
func (s *Service) runReconciler(ctx context.Context) error {
	for {
		timer := time.NewTimer(reconcileInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-s.reconcileWake:
			timer.Stop()
		case <-timer.C:
		}
		if err := s.reconcileOnce(ctx); err != nil && ctx.Err() == nil {
			s.deps.Log().Warn("reconcile billing subscriptions", "error", reason(err))
		}
	}
}

// wakeReconciler nudges the reconciler without blocking.
func (s *Service) wakeReconciler() {
	select {
	case s.reconcileWake <- struct{}{}:
	default:
	}
}

// reconcileOnce re-reads every subscription from the provider and applies it
// through the same state machine a webhook uses. It stamps reconciled_at only
// when every billed domain was actually read and applied, so "Last reconcile"
// on the Settings page names a pass that converged the whole account rather
// than one that failed or ran out of its budget half way.
func (s *Service) reconcileOnce(parent context.Context) error {
	if !s.configured() {
		return nil
	}
	ctx, cancel := context.WithTimeout(parent, reconcileBudget)
	defer cancel()

	docs, err := s.deps.Store.ListSubscriptions(ctx)
	if err != nil {
		s.deps.Meter().BillingReconcile(ctx, "error")
		return err
	}
	changed, checked, billed := 0, 0, 0
	var failure error
	for _, doc := range docs {
		if doc.SubscriptionID == "" {
			continue
		}
		billed++
		started := s.deps.Now()
		item, err := s.provider.GetSubscription(ctx, doc.SubscriptionID)
		s.observe(ctx, "get_subscription", started, err)
		if err != nil {
			failure = err
			continue
		}
		before := doc.State
		beforeEnd := doc.CurrentPeriodEnd
		if _, err := s.applyReconcile(ctx, doc.ZoneID, item); err != nil {
			failure = err
			continue
		}
		after, err := s.deps.Store.GetSubscription(ctx, doc.ZoneID)
		if err != nil {
			failure = err
			continue
		}
		checked++
		if after.State != before || !sameInstant(after.CurrentPeriodEnd, beforeEnd) {
			changed++
		}
	}

	switch {
	case failure != nil:
		// reconciled_at is the Settings page's "Last reconcile", and the page
		// has no way to qualify it: advancing it after a pass that never
		// reached some domains — or, during a provider outage, reached none —
		// would put a state on screen that no engine reported. The field is
		// left where it was, so it goes visibly stale, and the activity log
		// says how far the pass actually got.
		s.deps.Meter().BillingReconcile(ctx, "error")
		s.deps.Record(ctx, activity.Event{
			Actor:    activity.ActorReconciler,
			Kind:     activity.KindBillingReconciled,
			Severity: activity.SeverityWarn,
			Summary: fmt.Sprintf(
				"Could not reconcile billing with the provider: %d of %s checked",
				checked, plural(billed, "billed domain", "billed domains")),
			Details: map[string]string{
				"checked": strconv.Itoa(checked),
				"billed":  strconv.Itoa(billed),
				"error":   reason(failure),
			},
		})
		return failure
	case changed > 0:
		s.setReconciledAt(s.deps.Now())
		s.deps.Meter().BillingReconcile(ctx, "changed")
		s.deps.Record(ctx, activity.Event{
			Actor:    activity.ActorReconciler,
			Kind:     activity.KindBillingReconciled,
			Severity: activity.SeverityInfo,
			Summary:  fmt.Sprintf("Reconciled billing with the provider: %s updated", plural(changed, "domain", "domains")),
			Details:  map[string]string{"changed": strconv.Itoa(changed)},
		})
	default:
		s.setReconciledAt(s.deps.Now())
		s.deps.Meter().BillingReconcile(ctx, "unchanged")
	}
	return nil
}

func sameInstant(left, right *time.Time) bool {
	switch {
	case left == nil && right == nil:
		return true
	case left == nil || right == nil:
		return false
	default:
		return left.Equal(*right)
	}
}

func plural(count int, singular, many string) string {
	if count == 1 {
		return "1 " + singular
	}
	return strconv.Itoa(count) + " " + many
}
