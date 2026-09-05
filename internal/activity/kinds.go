package activity

import "slices"

// Kind is the dotted name of an event. It is a defined type so a producer
// cannot pass a free-form string without an explicit conversion, and every
// value it can hold is in the allowlist below — which is also the bound on the
// telemetry attribute, so an unbounded kind can never reach a metric.
type Kind string

// The complete allowlist. Adding a kind means adding it here and to the
// telemetry allowlist; TestKindsAreAllowlisted keeps the two in step.
const (
	KindZoneCreated  Kind = "zone.created"
	KindZoneDeleted  Kind = "zone.deleted"
	KindZoneImported Kind = "zone.imported"

	KindDNSRecordCreated    Kind = "dns.record.created"
	KindDNSRecordUpdated    Kind = "dns.record.updated"
	KindDNSRecordDeleted    Kind = "dns.record.deleted"
	KindDNSRecordsRewritten Kind = "dns.records.rewritten"
	KindDNSEngineRestored   Kind = "dns.engine.restored"
	KindDNSEngineOrphaned   Kind = "dns.engine.orphaned"

	KindSiteAttached           Kind = "site.attached"
	KindSiteReady              Kind = "site.ready"
	KindSiteUpdated            Kind = "site.updated"
	KindSiteDetached           Kind = "site.detached"
	KindSiteDetachPartial      Kind = "site.detach.partial"
	KindSiteDNSReapplied       Kind = "site.dns.reapplied"
	KindSiteHostnameRegistered Kind = "site.hostname.registered"
	KindSiteHostnameReady      Kind = "site.hostname.ready"
	KindSiteZoneMissing        Kind = "site.zone_missing"
	KindSiteWwwModeChanged     Kind = "site.www_mode.changed"
	// KindSiteEnvVarSet and KindSiteEnvVarDeleted name the variable and the
	// environment. A value never reaches an event: nothing in the hosting
	// facade puts one in a summary or a detail.
	KindSiteEnvVarSet     Kind = "site.env.set"
	KindSiteEnvVarDeleted Kind = "site.env.deleted"

	KindDeployRequested  Kind = "deploy.requested"
	KindDeployBuilding   Kind = "deploy.building"
	KindDeployLive       Kind = "deploy.live"
	KindDeploySuperseded Kind = "deploy.superseded"
	KindDeployFailed     Kind = "deploy.failed"
	KindDeployAbandoned  Kind = "deploy.abandoned"
	KindDeployLost       Kind = "deploy.lost"
	KindDeployRollback   Kind = "deploy.rollback"
	KindDeployRecovered  Kind = "deploy.recovered"

	KindHostingGatewayChanged       Kind = "hosting.gateway.changed"
	KindHostingGatewayPending       Kind = "hosting.gateway.pending"
	KindHostingGatewayConfirmed     Kind = "hosting.gateway.confirmed"
	KindHostingGatewayRejected      Kind = "hosting.gateway.rejected"
	KindHostingGatewayResolveFailed Kind = "hosting.gateway.resolve_failed"

	KindMailDomainBound      Kind = "mail.domain.bound"
	KindMailDomainUpdated    Kind = "mail.domain.updated"
	KindMailDomainUnbound    Kind = "mail.domain.unbound"
	KindMailRecordsChanged   Kind = "mail.records.changed"
	KindMailRecordsInvalid   Kind = "mail.records.invalid"
	KindMailDNSReapplied     Kind = "mail.dns.reapplied"
	KindMailZoneMissing      Kind = "mail.zone_missing"
	KindMailboxCreated       Kind = "mail.mailbox.created"
	KindMailboxUpdated       Kind = "mail.mailbox.updated"
	KindMailboxPasswordReset Kind = "mail.mailbox.password_reset"
	KindMailboxDeleted       Kind = "mail.mailbox.deleted"
	KindForwarderCreated     Kind = "mail.forwarder.created"
	KindForwarderDeleted     Kind = "mail.forwarder.deleted"

	KindBillingCheckoutStarted      Kind = "billing.checkout.started"
	KindBillingCheckoutCompleted    Kind = "billing.checkout.completed"
	KindBillingCheckoutExpired      Kind = "billing.checkout.expired"
	KindBillingSubscriptionUpdated  Kind = "billing.subscription.updated"
	KindBillingSubscriptionCanceled Kind = "billing.subscription.canceled"
	KindBillingInvoicePaid          Kind = "billing.invoice.paid"
	KindBillingInvoiceFailed        Kind = "billing.invoice.failed"
	KindBillingEventIgnored         Kind = "billing.event.ignored"
	KindBillingWebhookRejected      Kind = "billing.webhook.rejected"
	KindBillingWebhookDead          Kind = "billing.webhook.dead"
	KindBillingWebhookRetried       Kind = "billing.webhook.retried"
	KindBillingReconciled           Kind = "billing.reconciled"

	KindEngineUnreachable Kind = "engine.unreachable"
	KindEngineRecovered   Kind = "engine.recovered"

	KindPlatformStarted Kind = "platform.started"
	KindPlatformRebuilt Kind = "platform.rebuilt"

	// KindOther is what an unrecognised kind becomes, so the metric attribute
	// and the store stay bounded no matter what a producer passes.
	KindOther Kind = "other"
)

var kinds = map[Kind]struct{}{
	KindZoneCreated:  {},
	KindZoneDeleted:  {},
	KindZoneImported: {},

	KindDNSRecordCreated:    {},
	KindDNSRecordUpdated:    {},
	KindDNSRecordDeleted:    {},
	KindDNSRecordsRewritten: {},
	KindDNSEngineRestored:   {},
	KindDNSEngineOrphaned:   {},

	KindSiteAttached:           {},
	KindSiteReady:              {},
	KindSiteUpdated:            {},
	KindSiteDetached:           {},
	KindSiteDetachPartial:      {},
	KindSiteDNSReapplied:       {},
	KindSiteHostnameRegistered: {},
	KindSiteHostnameReady:      {},
	KindSiteZoneMissing:        {},
	KindSiteWwwModeChanged:     {},
	KindSiteEnvVarSet:          {},
	KindSiteEnvVarDeleted:      {},

	KindDeployRequested:  {},
	KindDeployBuilding:   {},
	KindDeployLive:       {},
	KindDeploySuperseded: {},
	KindDeployFailed:     {},
	KindDeployAbandoned:  {},
	KindDeployLost:       {},
	KindDeployRollback:   {},
	KindDeployRecovered:  {},

	KindHostingGatewayChanged:       {},
	KindHostingGatewayPending:       {},
	KindHostingGatewayConfirmed:     {},
	KindHostingGatewayRejected:      {},
	KindHostingGatewayResolveFailed: {},

	KindMailDomainBound:      {},
	KindMailDomainUpdated:    {},
	KindMailDomainUnbound:    {},
	KindMailRecordsChanged:   {},
	KindMailRecordsInvalid:   {},
	KindMailDNSReapplied:     {},
	KindMailZoneMissing:      {},
	KindMailboxCreated:       {},
	KindMailboxUpdated:       {},
	KindMailboxPasswordReset: {},
	KindMailboxDeleted:       {},
	KindForwarderCreated:     {},
	KindForwarderDeleted:     {},

	KindBillingCheckoutStarted:      {},
	KindBillingCheckoutCompleted:    {},
	KindBillingCheckoutExpired:      {},
	KindBillingSubscriptionUpdated:  {},
	KindBillingSubscriptionCanceled: {},
	KindBillingInvoicePaid:          {},
	KindBillingInvoiceFailed:        {},
	KindBillingEventIgnored:         {},
	KindBillingWebhookRejected:      {},
	KindBillingWebhookDead:          {},
	KindBillingWebhookRetried:       {},
	KindBillingReconciled:           {},

	KindEngineUnreachable: {},
	KindEngineRecovered:   {},

	KindPlatformStarted: {},
	KindPlatformRebuilt: {},

	KindOther: {},
}

// ValidKind reports whether kind is in the allowlist.
func ValidKind(kind Kind) bool {
	_, ok := kinds[kind]
	return ok
}

// NormalizeKind maps anything outside the allowlist onto KindOther, so no
// caller can widen a metric attribute or the stored vocabulary.
func NormalizeKind(kind Kind) Kind {
	if ValidKind(kind) {
		return kind
	}
	return KindOther
}

// Kinds returns the allowlist in sorted order. The telemetry allowlist test
// and the UI's filter list both read it.
func Kinds() []Kind {
	all := make([]Kind, 0, len(kinds))
	for kind := range kinds {
		all = append(all, kind)
	}
	slices.Sort(all)
	return all
}
