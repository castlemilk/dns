package billing

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/castlemilk/dns/internal/platform"
)

// rebuildBudget bounds the whole reconstruction: one customer lookup plus one
// paginated subscription list.
const rebuildBudget = 60 * time.Second

// Rebuild reconstructs the subscriptions bucket from Stripe after platform.db
// was lost. It runs inside the control plane, so the Stripe key never leaves
// the pod, and it only ever writes rows whose zone still exists.
func (s *Service) Rebuild(parent context.Context, dryRun bool) (platform.RebuildReport, error) {
	if !s.configured() {
		return platform.RebuildReport{}, nil
	}
	ctx, cancel := context.WithTimeout(parent, rebuildBudget)
	defer cancel()

	var report platform.RebuildReport

	started := s.deps.Now()
	customerID, found, err := s.provider.FindCustomer(ctx, s.cfg.CustomerEmail)
	s.observe(ctx, "find_customer", started, err)
	if err != nil {
		return platform.RebuildReport{}, err
	}
	if !found {
		report.Warnings = append(report.Warnings, "billing customer not found by email; no subscriptions were restored")
		return report, nil
	}

	listStarted := s.deps.Now()
	items, err := s.provider.ListSubscriptions(ctx, customerID)
	s.observe(ctx, "list_subscriptions", listStarted, err)
	if err != nil {
		return platform.RebuildReport{}, err
	}

	zones, err := s.zoneIndex(ctx)
	if err != nil {
		return platform.RebuildReport{}, err
	}

	now := s.deps.Now()
	for _, item := range items {
		zoneID := item.Metadata["zone_id"]
		domain := item.Metadata["domain"]
		name, known := zones[zoneID]
		if zoneID == "" || !known {
			// A subscription whose zone is gone is left in Stripe: recreating a
			// row for a zone that no longer exists would invent state.
			label := domain
			if label == "" {
				label = item.ID
			}
			report.Warnings = append(report.Warnings,
				fmt.Sprintf("subscription for %s has no matching zone", label))
			continue
		}
		report.Count++
		if dryRun {
			continue
		}
		doc := platform.SubscriptionDoc{
			V:          platform.DocVersion,
			ZoneID:     zoneID,
			ZoneName:   name,
			CustomerID: customerID,
			UpdatedAt:  now,
		}
		applySubscriptionToDoc(&doc, item)
		if !item.Created.IsZero() {
			// The guards start at the subscription's own creation so a webhook
			// that predates the rebuild cannot roll the row backwards.
			doc.LastSubscriptionEventCreated = item.Created.Unix()
		}
		if err := s.deps.Store.PutSubscription(ctx, doc); err != nil {
			return platform.RebuildReport{}, err
		}
		if item.PaymentMethod != nil {
			s.storeCustomer(ctx, customerID, item.PaymentMethod)
		}
	}
	if !dryRun {
		// The card is read from the customer, not taken from whichever
		// subscription happened to sort first, and a customer with no card is
		// recorded as having none rather than as unknown.
		cardStarted := s.deps.Now()
		live, err := s.provider.GetCustomer(ctx, customerID)
		s.observe(ctx, "get_customer", cardStarted, err)
		if err != nil {
			report.Warnings = append(report.Warnings, "the card on file could not be read from the billing provider")
			s.storeCustomer(ctx, customerID, nil)
		} else {
			s.setPaymentMethod(ctx, customerID, live.PaymentMethod)
		}
	}
	sort.Strings(report.Warnings)
	return report, nil
}

// zoneIndex maps zone id to zone name for every zone this control plane serves.
func (s *Service) zoneIndex(ctx context.Context) (map[string]string, error) {
	if s.deps.Zones == nil {
		return map[string]string{}, nil
	}
	values, err := s.deps.Zones.ListZones(ctx)
	if err != nil {
		return nil, err
	}
	index := make(map[string]string, len(values))
	for _, value := range values {
		index[value.ID] = value.Name
	}
	return index, nil
}
