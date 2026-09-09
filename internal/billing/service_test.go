package billing

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	billingv1 "github.com/castlemilk/dns/gen/go/billing/v1"
	"github.com/castlemilk/dns/internal/activity"
	"github.com/castlemilk/dns/internal/platform"
)

func TestCheckoutCompletesAndTheRowGoesActive(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	ctx := context.Background()

	created, err := h.service.CreateCheckoutSession(ctx, connect.NewRequest(&billingv1.CreateCheckoutSessionRequest{
		ZoneId: "z1", ReturnPath: "/domains/acme.dev",
	}))
	if err != nil {
		t.Fatalf("CreateCheckoutSession: %v", err)
	}
	if !strings.HasPrefix(created.Msg.GetSessionId(), "cs_") || created.Msg.GetUrl() == "" {
		t.Fatalf("session = %#v", created.Msg)
	}
	if pending := h.subscription("z1"); pending.State != StatePending || pending.CheckoutSessionID != created.Msg.GetSessionId() {
		t.Fatalf("row before completion = %#v", pending)
	}
	if !h.events.has(activity.KindBillingCheckoutStarted) {
		t.Fatalf("activity kinds = %v", h.events.kinds())
	}

	h.completeCheckoutInBrowser(created.Msg.GetUrl())
	h.drain()

	doc := h.subscription("z1")
	if doc.State != StateActive {
		t.Fatalf("state = %q, want ACTIVE", doc.State)
	}
	if doc.SubscriptionID == "" || doc.CustomerID == "" {
		t.Fatalf("row = %#v", doc)
	}
	if doc.LastInvoiceStatus != "paid" {
		t.Fatalf("last invoice status = %q, want paid", doc.LastInvoiceStatus)
	}
	if doc.CurrentPeriodEnd == nil || !doc.CurrentPeriodEnd.After(h.clock.Now()) {
		t.Fatalf("current period end = %v", doc.CurrentPeriodEnd)
	}
	if doc.CheckoutSessionID != "" || doc.CheckoutExpiresAt != nil {
		t.Fatalf("the checkout was not cleared: %#v", doc)
	}
	for _, kind := range []activity.Kind{
		activity.KindBillingCheckoutCompleted,
		activity.KindBillingSubscriptionUpdated,
		activity.KindBillingInvoicePaid,
	} {
		if !h.events.has(kind) {
			t.Errorf("no %s event; kinds = %v", kind, h.events.kinds())
		}
	}

	stats, err := h.store.WebhookStats(ctx)
	if err != nil {
		t.Fatalf("WebhookStats: %v", err)
	}
	if stats.Pending != 0 || stats.Dead != 0 {
		t.Fatalf("webhook stats = %#v", stats)
	}
}

func TestCreateCheckoutRefusesADomainThatIsAlreadySubscribed(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	ctx := context.Background()

	created, err := h.service.CreateCheckoutSession(ctx, connect.NewRequest(&billingv1.CreateCheckoutSessionRequest{ZoneId: "z1"}))
	if err != nil {
		t.Fatalf("CreateCheckoutSession: %v", err)
	}
	h.completeCheckoutInBrowser(created.Msg.GetUrl())
	h.drain()

	_, err = h.service.CreateCheckoutSession(ctx, connect.NewRequest(&billingv1.CreateCheckoutSessionRequest{ZoneId: "z1"}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("error = %v, want FailedPrecondition", err)
	}
	if !strings.Contains(err.Error(), "billing portal") {
		t.Fatalf("message = %q", err.Error())
	}
}

func TestCreateCheckoutReusesAnOpenSession(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	ctx := context.Background()

	first, err := h.service.CreateCheckoutSession(ctx, connect.NewRequest(&billingv1.CreateCheckoutSessionRequest{ZoneId: "z1"}))
	if err != nil {
		t.Fatalf("CreateCheckoutSession: %v", err)
	}
	second, err := h.service.CreateCheckoutSession(ctx, connect.NewRequest(&billingv1.CreateCheckoutSessionRequest{ZoneId: "z1"}))
	if err != nil {
		t.Fatalf("CreateCheckoutSession (again): %v", err)
	}
	if second.Msg.GetSessionId() != first.Msg.GetSessionId() {
		t.Fatalf("a second session was opened: %q then %q", first.Msg.GetSessionId(), second.Msg.GetSessionId())
	}
	creations := 0
	for _, request := range h.fake.Requests() {
		if request.Method == http.MethodPost && request.Path == "/v1/checkout/sessions" {
			creations++
		}
	}
	if creations != 1 {
		t.Fatalf("checkout sessions created = %d, want 1", creations)
	}
}

func TestCreateCheckoutValidatesItsInput(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	ctx := context.Background()

	cases := []struct {
		name    string
		request *billingv1.CreateCheckoutSessionRequest
		want    connect.Code
	}{
		{"no zone", &billingv1.CreateCheckoutSessionRequest{}, connect.CodeInvalidArgument},
		{"unknown zone", &billingv1.CreateCheckoutSessionRequest{ZoneId: "nope"}, connect.CodeNotFound},
		{"foreign return path", &billingv1.CreateCheckoutSessionRequest{
			ZoneId: "z1", ReturnPath: "https://evil.test/",
		}, connect.CodeInvalidArgument},
		{"traversing return path", &billingv1.CreateCheckoutSessionRequest{
			ZoneId: "z1", ReturnPath: "/domains/../../evil",
		}, connect.CodeInvalidArgument},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := h.service.CreateCheckoutSession(ctx, connect.NewRequest(testCase.request))
			if connect.CodeOf(err) != testCase.want {
				t.Fatalf("code = %v, want %v (err %v)", connect.CodeOf(err), testCase.want, err)
			}
		})
	}
}

func TestConfirmCheckoutBeforeTheWebhooksStillEndsPaid(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	ctx := context.Background()

	created, err := h.service.CreateCheckoutSession(ctx, connect.NewRequest(&billingv1.CreateCheckoutSessionRequest{ZoneId: "z1"}))
	if err != nil {
		t.Fatalf("CreateCheckoutSession: %v", err)
	}
	// The browser lands on /billing/success first; the three deliveries are
	// queued but not yet applied.
	h.completeCheckoutInBrowser(created.Msg.GetUrl())

	confirmed, err := h.service.ConfirmCheckout(ctx, connect.NewRequest(&billingv1.ConfirmCheckoutRequest{
		SessionId: created.Msg.GetSessionId(),
	}))
	if err != nil {
		t.Fatalf("ConfirmCheckout: %v", err)
	}
	if !confirmed.Msg.GetCompleted() || !confirmed.Msg.GetPaymentRequired() {
		t.Fatalf("confirm = %#v", confirmed.Msg)
	}
	if confirmed.Msg.GetDomain().GetState() != billingv1.SubscriptionState_SUBSCRIPTION_STATE_ACTIVE {
		t.Fatalf("state after confirm = %v", confirmed.Msg.GetDomain().GetState())
	}
	if confirmed.Msg.GetProvider() != "fake" {
		t.Fatalf("provider = %q", confirmed.Msg.GetProvider())
	}

	// The webhooks land afterwards. Guarding on the session's own created time
	// rather than the confirm clock is what keeps invoice.paid from being
	// dropped as stale.
	h.drain()
	doc := h.subscription("z1")
	if doc.State != StateActive || doc.LastInvoiceStatus != "paid" {
		t.Fatalf("row after the webhooks = %#v", doc)
	}
	// The confirm is attributed to the operator, not to Stripe.
	for _, event := range h.events.all() {
		if event.Kind == activity.KindBillingCheckoutCompleted && event.Actor != activity.ActorOperator {
			t.Fatalf("checkout completion actor = %q, want %q", event.Actor, activity.ActorOperator)
		}
	}
}

func TestConfirmCheckoutRejectsSessionsThisControlPlaneDidNotCreate(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	ctx := context.Background()

	if _, err := h.service.ConfirmCheckout(ctx, connect.NewRequest(&billingv1.ConfirmCheckoutRequest{
		SessionId: "not-a-session",
	})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("malformed id code = %v", connect.CodeOf(err))
	}
	if _, err := h.service.ConfirmCheckout(ctx, connect.NewRequest(&billingv1.ConfirmCheckoutRequest{
		SessionId: "cs_live_someoneelses",
	})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("unknown id code = %v", connect.CodeOf(err))
	}
}

func TestPortalActionsMoveTheRowThroughItsStates(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	ctx := context.Background()

	created, err := h.service.CreateCheckoutSession(ctx, connect.NewRequest(&billingv1.CreateCheckoutSessionRequest{ZoneId: "z1"}))
	if err != nil {
		t.Fatalf("CreateCheckoutSession: %v", err)
	}
	h.completeCheckoutInBrowser(created.Msg.GetUrl())
	h.drain()
	doc := h.subscription("z1")

	steps := []struct {
		action string
		want   string
	}{
		{"past_due", StatePastDue},
		{"paid", StateActive},
		{"cancel_at_period_end", StateCanceling},
		{"cancel_now", StateCanceled},
	}
	for _, step := range steps {
		h.portal(doc.CustomerID, doc.SubscriptionID, step.action)
		h.drain()
		if got := h.subscription("z1").State; got != step.want {
			t.Fatalf("after %s state = %q, want %q", step.action, got, step.want)
		}
	}
	if !h.events.has(activity.KindBillingSubscriptionCanceled) || !h.events.has(activity.KindBillingInvoiceFailed) {
		t.Fatalf("activity kinds = %v", h.events.kinds())
	}

	// A canceled domain can subscribe again.
	if _, err := h.service.CreateCheckoutSession(ctx,
		connect.NewRequest(&billingv1.CreateCheckoutSessionRequest{ZoneId: "z1"})); err != nil {
		t.Fatalf("CreateCheckoutSession after cancellation: %v", err)
	}
}

func TestCreatePortalSessionRequiresACustomerAndAKnownFlow(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	ctx := context.Background()

	if _, err := h.service.CreatePortalSession(ctx, connect.NewRequest(&billingv1.CreatePortalSessionRequest{})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("code without a customer = %v", connect.CodeOf(err))
	}

	created, err := h.service.CreateCheckoutSession(ctx, connect.NewRequest(&billingv1.CreateCheckoutSessionRequest{ZoneId: "z1"}))
	if err != nil {
		t.Fatalf("CreateCheckoutSession: %v", err)
	}
	h.completeCheckoutInBrowser(created.Msg.GetUrl())
	h.drain()

	portal, err := h.service.CreatePortalSession(ctx, connect.NewRequest(&billingv1.CreatePortalSessionRequest{
		Flow: "subscription_cancel", ZoneId: "z1", ReturnPath: "/domains/acme.dev",
	}))
	if err != nil {
		t.Fatalf("CreatePortalSession: %v", err)
	}
	if !strings.HasPrefix(portal.Msg.GetUrl(), h.fake.BaseURL()+"/hosted/portal/") {
		t.Fatalf("portal url = %q", portal.Msg.GetUrl())
	}
	if _, err := h.service.CreatePortalSession(ctx, connect.NewRequest(&billingv1.CreatePortalSessionRequest{
		Flow: "delete_everything",
	})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("unknown flow code = %v", connect.CodeOf(err))
	}
	if _, err := h.service.CreatePortalSession(ctx, connect.NewRequest(&billingv1.CreatePortalSessionRequest{
		Flow: "subscription_cancel", ZoneId: "z2",
	})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("cancel flow for an unbilled zone = %v", connect.CodeOf(err))
	}
}

func TestGetBillingSummaryJoinsZonesWithRows(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{zones: map[string]string{"z1": "acme.dev", "z2": "beta.dev"}})
	ctx := context.Background()

	created, err := h.service.CreateCheckoutSession(ctx, connect.NewRequest(&billingv1.CreateCheckoutSessionRequest{ZoneId: "z1"}))
	if err != nil {
		t.Fatalf("CreateCheckoutSession: %v", err)
	}
	h.completeCheckoutInBrowser(created.Msg.GetUrl())
	h.drain()

	// A row whose zone was deleted stays visible so the operator can cancel it.
	if err := h.store.PutSubscription(ctx, platform.SubscriptionDoc{
		V: platform.DocVersion, ZoneID: "gone", ZoneName: "gone.dev",
		SubscriptionID: "sub_gone", State: StatePastDue, UpdatedAt: h.clock.Now(),
	}); err != nil {
		t.Fatalf("PutSubscription: %v", err)
	}

	summary, err := h.service.GetBillingSummary(ctx, connect.NewRequest(&billingv1.GetBillingSummaryRequest{}))
	if err != nil {
		t.Fatalf("GetBillingSummary: %v", err)
	}
	rows := summary.Msg.GetDomains()
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	byZone := map[string]*billingv1.DomainBilling{}
	for _, row := range rows {
		byZone[row.GetZoneId()] = row
	}
	if byZone["z1"].GetState() != billingv1.SubscriptionState_SUBSCRIPTION_STATE_ACTIVE {
		t.Errorf("z1 state = %v", byZone["z1"].GetState())
	}
	if byZone["z2"].GetState() != billingv1.SubscriptionState_SUBSCRIPTION_STATE_UNBILLED {
		t.Errorf("z2 state = %v", byZone["z2"].GetState())
	}
	if !byZone["gone"].GetZoneMissing() {
		t.Errorf("the deleted zone's row is not marked missing")
	}
	if summary.Msg.GetActiveCount() != 1 || summary.Msg.GetAttentionCount() != 1 {
		t.Errorf("counts = active %d attention %d", summary.Msg.GetActiveCount(), summary.Msg.GetAttentionCount())
	}
	if !summary.Msg.GetCustomerExists() {
		t.Error("customer_exists is false after a completed checkout")
	}
}

// A zone-store failure is a server-side fault. Every other read in service.go
// answers it with one fixed sentence; these two RPCs used to hand the caller
// the store's own error text, which for bbolt names the database path.
func TestAZoneStoreFailureIsNotReportedVerbatimToTheCaller(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	ctx := context.Background()
	h.zones.fail(errors.New("bolt: open /var/lib/simpledns/platform.db: permission denied"))

	cases := []struct {
		name string
		call func() error
	}{
		{"GetBillingSummary", func() error {
			_, err := h.service.GetBillingSummary(ctx, connect.NewRequest(&billingv1.GetBillingSummaryRequest{}))
			return err
		}},
		{"CreateCheckoutSession", func() error {
			_, err := h.service.CreateCheckoutSession(ctx,
				connect.NewRequest(&billingv1.CreateCheckoutSessionRequest{ZoneId: "z1"}))
			return err
		}},
	}
	for _, testCase := range cases {
		err := testCase.call()
		if err == nil {
			t.Fatalf("%s succeeded with a failing zone store", testCase.name)
		}
		if got := connect.CodeOf(err); got != connect.CodeInternal {
			t.Errorf("%s code = %v, want %v", testCase.name, got, connect.CodeInternal)
		}
		if strings.Contains(err.Error(), "platform.db") || strings.Contains(err.Error(), "bolt") {
			t.Errorf("%s leaked the store error to the caller: %q", testCase.name, err.Error())
		}
		if !strings.Contains(err.Error(), "internal server error") {
			t.Errorf("%s message = %q, want the fixed operator copy", testCase.name, err.Error())
		}
	}
}

func TestListInvoicesPagesAndValidatesTheCursor(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	ctx := context.Background()

	created, err := h.service.CreateCheckoutSession(ctx, connect.NewRequest(&billingv1.CreateCheckoutSessionRequest{ZoneId: "z1"}))
	if err != nil {
		t.Fatalf("CreateCheckoutSession: %v", err)
	}
	h.completeCheckoutInBrowser(created.Msg.GetUrl())
	h.drain()
	doc := h.subscription("z1")
	h.portal(doc.CustomerID, doc.SubscriptionID, "paid")
	h.drain()

	first, err := h.service.ListInvoices(ctx, connect.NewRequest(&billingv1.ListInvoicesRequest{Limit: 1}))
	if err != nil {
		t.Fatalf("ListInvoices: %v", err)
	}
	if !first.Msg.GetLive() || len(first.Msg.GetInvoices()) != 1 || first.Msg.GetNextCursor() == "" {
		t.Fatalf("first page = %#v", first.Msg)
	}
	if first.Msg.GetInvoices()[0].GetZoneName() != "acme.dev" || first.Msg.GetInvoices()[0].GetTotal() != 600 {
		t.Fatalf("invoice = %#v", first.Msg.GetInvoices()[0])
	}
	second, err := h.service.ListInvoices(ctx, connect.NewRequest(&billingv1.ListInvoicesRequest{
		Limit: 1, Cursor: first.Msg.GetNextCursor(),
	}))
	if err != nil {
		t.Fatalf("ListInvoices (page 2): %v", err)
	}
	if len(second.Msg.GetInvoices()) != 1 || second.Msg.GetInvoices()[0].GetId() == first.Msg.GetInvoices()[0].GetId() {
		t.Fatalf("second page = %#v", second.Msg)
	}

	if _, err := h.service.ListInvoices(ctx, connect.NewRequest(&billingv1.ListInvoicesRequest{
		Cursor: "sub_not_an_invoice",
	})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("cursor validation code = %v", connect.CodeOf(err))
	}

	// An unreachable provider is an honest empty list, not an error page.
	h.fake.FailAPI(http.StatusServiceUnavailable, time.Hour)
	down, err := h.service.ListInvoices(ctx, connect.NewRequest(&billingv1.ListInvoicesRequest{}))
	if err != nil {
		t.Fatalf("ListInvoices while unreachable: %v", err)
	}
	if down.Msg.GetLive() || len(down.Msg.GetInvoices()) != 0 {
		t.Fatalf("unreachable page = %#v", down.Msg)
	}
}

func TestGetBillingStatusReportsThePriceAndWebhookHealth(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	ctx := context.Background()

	status, err := h.service.GetBillingStatus(ctx, connect.NewRequest(&billingv1.GetBillingStatusRequest{}))
	if err != nil {
		t.Fatalf("GetBillingStatus: %v", err)
	}
	if got := status.Msg.GetPrice().GetLabel(); got != "$6 / domain / month" {
		t.Fatalf("price label = %q", got)
	}
	if h.service.PriceLabel() != status.Msg.GetPrice().GetLabel() {
		t.Fatalf("PriceLabel = %q", h.service.PriceLabel())
	}
	if !status.Msg.GetInformationalOnly() || status.Msg.GetPolicyNote() != platform.PolicyNote {
		t.Fatalf("policy = %v / %q", status.Msg.GetInformationalOnly(), status.Msg.GetPolicyNote())
	}
	if !status.Msg.GetWebhookConfigured() || status.Msg.GetWebhookSecrets() != 1 {
		t.Fatalf("webhook config = %v / %d", status.Msg.GetWebhookConfigured(), status.Msg.GetWebhookSecrets())
	}
	if status.Msg.GetLivemode() {
		t.Fatal("a test key reported livemode")
	}
	engine := status.Msg.GetEngine()
	if !engine.GetConfigured() || !engine.GetReachable() || engine.GetProvider() != "fake" {
		t.Fatalf("engine = %#v", engine)
	}
	if strings.Contains(engine.GetEndpointHost(), "/") || engine.GetEndpointHost() == "" {
		t.Fatalf("endpoint host = %q", engine.GetEndpointHost())
	}
}

func TestRebuildReconstructsSubscriptionsFromTheProvider(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{zones: map[string]string{"z1": "acme.dev", "z2": "beta.dev"}})
	ctx := context.Background()

	created, err := h.service.CreateCheckoutSession(ctx, connect.NewRequest(&billingv1.CreateCheckoutSessionRequest{ZoneId: "z1"}))
	if err != nil {
		t.Fatalf("CreateCheckoutSession: %v", err)
	}
	h.completeCheckoutInBrowser(created.Msg.GetUrl())
	h.drain()

	if err := h.store.DeleteSubscription(ctx, "z1"); err != nil {
		t.Fatalf("DeleteSubscription: %v", err)
	}

	dry, err := h.service.Rebuild(ctx, true)
	if err != nil {
		t.Fatalf("Rebuild (dry run): %v", err)
	}
	if dry.Count != 1 {
		t.Fatalf("dry run count = %d, want 1", dry.Count)
	}
	if _, err := h.store.GetSubscription(ctx, "z1"); err == nil {
		t.Fatal("the dry run wrote a row")
	}

	report, err := h.service.Rebuild(ctx, false)
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if report.Count != 1 {
		t.Fatalf("count = %d, want 1", report.Count)
	}
	doc := h.subscription("z1")
	if doc.State != StateActive || doc.ZoneName != "acme.dev" || doc.SubscriptionID == "" {
		t.Fatalf("rebuilt row = %#v", doc)
	}

	// A subscription whose zone is gone is reported, never invented.
	h.zones.remove("z1")
	if err := h.store.DeleteSubscription(ctx, "z1"); err != nil {
		t.Fatalf("DeleteSubscription: %v", err)
	}
	orphaned, err := h.service.Rebuild(ctx, false)
	if err != nil {
		t.Fatalf("Rebuild (zone gone): %v", err)
	}
	if orphaned.Count != 0 || len(orphaned.Warnings) != 1 ||
		!strings.Contains(orphaned.Warnings[0], "acme.dev") {
		t.Fatalf("report = %#v", orphaned)
	}
}

func TestEnsureCustomerReusesTheCustomerFoundByEmail(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	ctx := context.Background()

	// Stripe already has the customer (a rebuilt store, or an earlier
	// deployment). The facade must find it rather than opening a second one.
	existing, err := h.service.provider.EnsureCustomer(ctx, testCustomerEmail, "customer:preexisting")
	if err != nil {
		t.Fatalf("EnsureCustomer (fixture): %v", err)
	}

	resolved, err := h.service.ensureCustomer(ctx)
	if err != nil {
		t.Fatalf("ensureCustomer: %v", err)
	}
	if resolved != existing {
		t.Fatalf("resolved %q, want the existing %q", resolved, existing)
	}
	creations := 0
	for _, request := range h.fake.Requests() {
		if request.Method == http.MethodPost && request.Path == "/v1/customers" {
			creations++
		}
	}
	if creations != 1 {
		t.Fatalf("customers created = %d, want 1", creations)
	}
	doc, found, err := h.store.GetCustomer(ctx)
	if err != nil || !found || doc.CustomerID != existing {
		t.Fatalf("stored customer = (%#v, %v, %v)", doc, found, err)
	}
}

func TestExpirerReleasesAPendingRow(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	ctx := context.Background()

	if _, err := h.service.CreateCheckoutSession(ctx,
		connect.NewRequest(&billingv1.CreateCheckoutSessionRequest{ZoneId: "z1"})); err != nil {
		t.Fatalf("CreateCheckoutSession: %v", err)
	}
	if err := h.service.expireOnce(ctx); err != nil {
		t.Fatalf("expireOnce: %v", err)
	}
	if state := h.subscription("z1").State; state != StatePending {
		t.Fatalf("state before the deadline = %q", state)
	}

	h.clock.Advance(24 * time.Hour)
	if err := h.service.expireOnce(ctx); err != nil {
		t.Fatalf("expireOnce after the deadline: %v", err)
	}
	doc := h.subscription("z1")
	if doc.State != StateUnbilled || doc.CheckoutSessionID != "" {
		t.Fatalf("row after expiry = %#v", doc)
	}
	if !h.events.has(activity.KindBillingCheckoutExpired) {
		t.Fatalf("activity kinds = %v", h.events.kinds())
	}
}

func TestAPaymentThatLandsAfterTheDeadlineIsNotAnExpiry(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	ctx := context.Background()

	created, err := h.service.CreateCheckoutSession(ctx, newCheckoutRequest("z1"))
	if err != nil {
		t.Fatalf("CreateCheckoutSession: %v", err)
	}
	// The customer pays, and the deliveries have not been applied yet — a
	// Stripe outage, or simply the sweep landing first.
	h.completeCheckoutInBrowser(created.Msg.GetUrl())
	h.clock.Advance(24 * time.Hour)

	if err := h.service.expireOnce(ctx); err != nil {
		t.Fatalf("expireOnce: %v", err)
	}
	doc := h.subscription("z1")
	if doc.State != StateActive {
		t.Fatalf("state after the sweep = %q, want ACTIVE: the customer paid", doc.State)
	}
	if h.events.has(activity.KindBillingCheckoutExpired) {
		t.Fatalf("the log claims a checkout expired for a domain that is billed: %v", h.events.kinds())
	}
	if !h.events.has(activity.KindBillingCheckoutCompleted) {
		t.Fatalf("the completion was never recorded: %v", h.events.kinds())
	}

	// The deliveries arrive afterwards and change nothing.
	h.drain()
	if state := h.subscription("z1").State; state != StateActive {
		t.Fatalf("state after the late deliveries = %q", state)
	}
	completions := 0
	for _, event := range h.events.all() {
		if event.Kind == activity.KindBillingCheckoutCompleted {
			completions++
		}
	}
	if completions != 1 {
		t.Fatalf("checkout completions recorded = %d, want 1", completions)
	}
}

func TestAnUnansweredProviderLeavesAnOverdueCheckoutPending(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	ctx := context.Background()

	if _, err := h.service.CreateCheckoutSession(ctx, newCheckoutRequest("z1")); err != nil {
		t.Fatalf("CreateCheckoutSession: %v", err)
	}
	h.fake.FailAPI(http.StatusServiceUnavailable, 30*24*time.Hour)
	h.clock.Advance(24 * time.Hour)

	// The deadline has passed but nothing can say whether the customer paid.
	// "Still waiting" is the truth; releasing the row would assert something
	// this control plane never checked.
	if err := h.service.expireOnce(ctx); err != nil {
		t.Fatalf("expireOnce: %v", err)
	}
	if state := h.subscription("z1").State; state != StatePending {
		t.Fatalf("state while the provider is down = %q, want PENDING", state)
	}
	if h.events.has(activity.KindBillingCheckoutExpired) {
		t.Fatalf("an expiry was recorded without asking the provider: %v", h.events.kinds())
	}

	// Once the provider answers, and says the session was never completed, the
	// row is released.
	h.fake.FailAPI(0, 0)
	if err := h.service.expireOnce(ctx); err != nil {
		t.Fatalf("expireOnce after recovery: %v", err)
	}
	doc := h.subscription("z1")
	if doc.State != StateUnbilled || doc.CheckoutSessionID != "" {
		t.Fatalf("row after expiry = %#v", doc)
	}
	if !h.events.has(activity.KindBillingCheckoutExpired) {
		t.Fatalf("activity kinds = %v", h.events.kinds())
	}
}

func TestAFailedReconcilePassDoesNotClaimToHaveRun(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	ctx := context.Background()

	created, err := h.service.CreateCheckoutSession(ctx, newCheckoutRequest("z1"))
	if err != nil {
		t.Fatalf("CreateCheckoutSession: %v", err)
	}
	h.completeCheckoutInBrowser(created.Msg.GetUrl())
	h.drain()
	if err := h.service.reconcileOnce(ctx); err != nil {
		t.Fatalf("reconcileOnce: %v", err)
	}
	first, err := h.service.GetBillingStatus(ctx, connect.NewRequest(&billingv1.GetBillingStatusRequest{}))
	if err != nil {
		t.Fatalf("GetBillingStatus: %v", err)
	}
	if first.Msg.GetReconciledAt() == nil {
		t.Fatal("a successful pass did not report reconciled_at")
	}

	// The provider goes down. "Last reconcile: a few seconds ago" for a pass
	// that read nothing would be a state on screen no engine reported.
	h.fake.FailAPI(http.StatusServiceUnavailable, 30*24*time.Hour)
	h.clock.Advance(6 * time.Hour)
	if err := h.service.reconcileOnce(ctx); err == nil {
		t.Fatal("a pass in which every read failed reported success")
	}
	after, err := h.service.GetBillingStatus(ctx, connect.NewRequest(&billingv1.GetBillingStatusRequest{}))
	if err != nil {
		t.Fatalf("GetBillingStatus: %v", err)
	}
	if !after.Msg.GetReconciledAt().AsTime().Equal(first.Msg.GetReconciledAt().AsTime()) {
		t.Fatalf("reconciled_at advanced on a failed pass: %v then %v",
			first.Msg.GetReconciledAt().AsTime(), after.Msg.GetReconciledAt().AsTime())
	}
	// The operator is told how far the pass actually got, so the stale
	// timestamp is explained rather than mysterious.
	said := false
	for _, event := range h.events.all() {
		if event.Kind == activity.KindBillingReconciled && event.Severity == activity.SeverityWarn {
			said = true
			if event.Details["checked"] != "0" || event.Details["billed"] != "1" {
				t.Fatalf("failed reconcile details = %v", event.Details)
			}
		}
	}
	if !said {
		t.Fatalf("a failed pass recorded nothing: %v", h.events.kinds())
	}
}

func TestARemovedCardStopsBeingShown(t *testing.T) {
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

	h.portal(doc.CustomerID, doc.SubscriptionID, "update_card")
	h.drain()
	summary, err := h.service.GetBillingSummary(ctx, newSummaryRequest())
	if err != nil {
		t.Fatalf("GetBillingSummary: %v", err)
	}
	if summary.Msg.GetPaymentMethod().GetLast4() != "4242" {
		t.Fatalf("payment method after attaching a card = %#v", summary.Msg.GetPaymentMethod())
	}

	// The customer removes the card in the portal. The Billing page must stop
	// showing it: a card Stripe no longer holds is not "on file".
	h.portal(doc.CustomerID, doc.SubscriptionID, "remove_card")
	h.drain()
	customer, _, err := h.store.GetCustomer(ctx)
	if err != nil {
		t.Fatalf("GetCustomer: %v", err)
	}
	if customer.PaymentMethod.Brand != "" || customer.PaymentMethod.Last4 != "" {
		t.Fatalf("stored card after removal = %#v", customer.PaymentMethod)
	}
	cleared, err := h.service.GetBillingSummary(ctx, newSummaryRequest())
	if err != nil {
		t.Fatalf("GetBillingSummary: %v", err)
	}
	if cleared.Msg.GetPaymentMethod() != nil {
		t.Fatalf("summary still reports a card: %#v", cleared.Msg.GetPaymentMethod())
	}
}

func TestOutOfOrderDeliveriesStillSettleTheRow(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	ctx := context.Background()
	// Stripe fans deliveries out concurrently and orders nothing. Every
	// ordering property this facade claims has to survive that, not just the
	// creation order the fake finds convenient.
	h.fake.DeliverOutOfOrder(true)

	created, err := h.service.CreateCheckoutSession(ctx, newCheckoutRequest("z1"))
	if err != nil {
		t.Fatalf("CreateCheckoutSession: %v", err)
	}
	h.completeCheckoutInBrowser(created.Msg.GetUrl())
	h.drain()
	doc := h.subscription("z1")
	if doc.State != StateActive || doc.SubscriptionID == "" || doc.LastInvoiceStatus != "paid" {
		t.Fatalf("row after an unordered checkout = %#v", doc)
	}
	if doc.CurrentPeriodEnd == nil {
		t.Fatal("the period end was lost in the reordering")
	}

	// A dunning cancellation: the failed invoice, the past-due update and the
	// cancellation, all in flight together.
	h.portal(doc.CustomerID, doc.SubscriptionID, "past_due")
	h.portal(doc.CustomerID, doc.SubscriptionID, "cancel_now")
	h.drain()
	if state := h.subscription("z1").State; state != StateCanceled {
		t.Fatalf("state after an unordered cancellation = %q, want CANCELED", state)
	}
	summary, err := h.service.GetBillingSummary(ctx, newSummaryRequest())
	if err != nil {
		t.Fatalf("GetBillingSummary: %v", err)
	}
	if summary.Msg.GetAttentionCount() != 0 {
		t.Fatalf("attention count for a canceled domain = %d", summary.Msg.GetAttentionCount())
	}
}

func TestReconcilerConvergesARowChangedBehindTheFacade(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	ctx := context.Background()

	created, err := h.service.CreateCheckoutSession(ctx, connect.NewRequest(&billingv1.CreateCheckoutSessionRequest{ZoneId: "z1"}))
	if err != nil {
		t.Fatalf("CreateCheckoutSession: %v", err)
	}
	h.completeCheckoutInBrowser(created.Msg.GetUrl())
	h.drain()
	doc := h.subscription("z1")

	// The portal cancels the subscription while the webhook never arrives.
	h.portal(doc.CustomerID, doc.SubscriptionID, "cancel_now")
	for _, entry := range mustNextWebhooks(t, h) {
		if err := h.store.AckWebhook(ctx, entry.Key, platform.WebhookOutcomeIgnored, "dropped by the test", time.Time{}); err != nil {
			t.Fatalf("AckWebhook: %v", err)
		}
	}
	if state := h.subscription("z1").State; state != StateActive {
		t.Fatalf("state with the delivery dropped = %q, want ACTIVE", state)
	}

	if err := h.service.reconcileOnce(ctx); err != nil {
		t.Fatalf("reconcileOnce: %v", err)
	}
	if state := h.subscription("z1").State; state != StateCanceled {
		t.Fatalf("state after reconciling = %q, want CANCELED", state)
	}
	if !h.events.has(activity.KindBillingReconciled) {
		t.Fatalf("activity kinds = %v", h.events.kinds())
	}
	status, err := h.service.GetBillingStatus(ctx, connect.NewRequest(&billingv1.GetBillingStatusRequest{}))
	if err != nil {
		t.Fatalf("GetBillingStatus: %v", err)
	}
	if status.Msg.GetReconciledAt() == nil {
		t.Fatal("reconciled_at was not reported")
	}
}

func TestQueueRetriesUntilTheProviderRecovers(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	ctx := context.Background()

	created, err := h.service.CreateCheckoutSession(ctx, connect.NewRequest(&billingv1.CreateCheckoutSessionRequest{ZoneId: "z1"}))
	if err != nil {
		t.Fatalf("CreateCheckoutSession: %v", err)
	}
	// The API goes down for two hours right after the customer pays. The
	// hosted pages keep working, so the three deliveries still arrive.
	h.fake.FailAPI(http.StatusServiceUnavailable, 2*time.Hour)
	h.completeCheckoutInBrowser(created.Msg.GetUrl())
	h.drain()

	stats, err := h.store.WebhookStats(ctx)
	if err != nil {
		t.Fatalf("WebhookStats: %v", err)
	}
	if stats.Pending == 0 {
		t.Fatal("nothing was left pending while the provider was down")
	}
	if stats.Dead != 0 {
		t.Fatalf("dead entries during a short outage = %d", stats.Dead)
	}

	h.clock.Advance(3 * time.Hour)
	h.drain()
	after, err := h.store.WebhookStats(ctx)
	if err != nil {
		t.Fatalf("WebhookStats: %v", err)
	}
	if after.Pending != 0 || after.Dead != 0 {
		t.Fatalf("queue after recovery = %#v", after)
	}
	if state := h.subscription("z1").State; state != StateActive {
		t.Fatalf("state after recovery = %q", state)
	}
}

func TestQueueDeadLettersAfterSevenDaysAndRetryRedrivesIt(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	ctx := context.Background()

	created, err := h.service.CreateCheckoutSession(ctx, connect.NewRequest(&billingv1.CreateCheckoutSessionRequest{ZoneId: "z1"}))
	if err != nil {
		t.Fatalf("CreateCheckoutSession: %v", err)
	}
	h.fake.FailAPI(http.StatusServiceUnavailable, 30*24*time.Hour)
	h.completeCheckoutInBrowser(created.Msg.GetUrl())
	h.drain()

	// Seven days on, and only once the entry has actually exhausted its
	// back-off, it is set aside with its payload intact.
	h.clock.Advance(8 * 24 * time.Hour)
	for range minAttemptsBeforeDead {
		h.drain()
		h.clock.Advance(time.Hour)
	}
	stats, err := h.store.WebhookStats(ctx)
	if err != nil {
		t.Fatalf("WebhookStats: %v", err)
	}
	if stats.Dead != 1 {
		t.Fatalf("dead entries = %d, want 1", stats.Dead)
	}
	if !h.events.has(activity.KindBillingWebhookDead) {
		t.Fatalf("activity kinds = %v", h.events.kinds())
	}

	// The operator re-drives it while the cause is still there — a second
	// outage, or a fix that did not take. A re-driven entry keeps its original
	// queue key, so it is already past the seven-day horizon: it must still get
	// the back-off the retry promises rather than dying on its first attempt.
	requeued, err := h.service.RetryDeadWebhooks(ctx, connect.NewRequest(&billingv1.RetryDeadWebhooksRequest{}))
	if err != nil {
		t.Fatalf("RetryDeadWebhooks: %v", err)
	}
	if requeued.Msg.GetRequeued() != 1 {
		t.Fatalf("requeued = %d", requeued.Msg.GetRequeued())
	}
	h.drain()
	stillFailing, err := h.store.WebhookStats(ctx)
	if err != nil {
		t.Fatalf("WebhookStats: %v", err)
	}
	if stillFailing.Pending != 1 || stillFailing.Dead != 0 {
		t.Fatalf("queue after one failing attempt = %#v, want it still pending", stillFailing)
	}

	// The operator fixes the cause; the entry applies.
	h.fake.FailAPI(0, 0)
	h.clock.Advance(time.Hour)
	h.drain()
	final, err := h.store.WebhookStats(ctx)
	if err != nil {
		t.Fatalf("WebhookStats: %v", err)
	}
	if final.Pending != 0 || final.Dead != 0 {
		t.Fatalf("queue after the retry = %#v", final)
	}
	if !h.events.has(activity.KindBillingWebhookRetried) {
		t.Fatalf("activity kinds = %v", h.events.kinds())
	}
}

func TestBackoffDoublesToTheHourCap(t *testing.T) {
	t.Parallel()
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{0, 5 * time.Second},
		{1, 5 * time.Second},
		{2, 10 * time.Second},
		{3, 20 * time.Second},
		{10, 2560 * time.Second},
		{11, time.Hour},
		{50, time.Hour},
	}
	for _, testCase := range cases {
		if got := backoff(testCase.attempt); got != testCase.want {
			t.Errorf("backoff(%d) = %v, want %v", testCase.attempt, got, testCase.want)
		}
	}
}

func TestAnUnconfiguredFacadeIsHonestRatherThanEmpty(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{noFake: true})
	ctx := context.Background()

	if h.webhook != nil {
		t.Fatal("an unconfigured facade returned a webhook handler")
	}
	status, err := h.service.GetBillingStatus(ctx, connect.NewRequest(&billingv1.GetBillingStatusRequest{}))
	if err != nil {
		t.Fatalf("GetBillingStatus: %v", err)
	}
	if status.Msg.GetEngine().GetConfigured() || status.Msg.GetPrice() != nil {
		t.Fatalf("status = %#v", status.Msg)
	}
	wantEnv := []string{"BILLING_PROVIDER", "STRIPE_SECRET_KEY", "STRIPE_WEBHOOK_SECRET", "STRIPE_PRICE_ID", "BILLING_CUSTOMER_EMAIL"}
	if got := status.Msg.GetEngine().GetMissingEnv(); len(got) != len(wantEnv) {
		t.Fatalf("missing env = %v, want %v", got, wantEnv)
	}
	if h.service.PriceLabel() != "" {
		t.Fatalf("PriceLabel = %q, want empty", h.service.PriceLabel())
	}

	for _, call := range []struct {
		name string
		run  func() error
	}{
		{"GetBillingSummary", func() error {
			_, err := h.service.GetBillingSummary(ctx, connect.NewRequest(&billingv1.GetBillingSummaryRequest{}))
			return err
		}},
		{"CreateCheckoutSession", func() error {
			_, err := h.service.CreateCheckoutSession(ctx, connect.NewRequest(&billingv1.CreateCheckoutSessionRequest{ZoneId: "z1"}))
			return err
		}},
		{"ConfirmCheckout", func() error {
			_, err := h.service.ConfirmCheckout(ctx, connect.NewRequest(&billingv1.ConfirmCheckoutRequest{SessionId: "cs_x"}))
			return err
		}},
		{"CreatePortalSession", func() error {
			_, err := h.service.CreatePortalSession(ctx, connect.NewRequest(&billingv1.CreatePortalSessionRequest{}))
			return err
		}},
		{"RetryDeadWebhooks", func() error {
			_, err := h.service.RetryDeadWebhooks(ctx, connect.NewRequest(&billingv1.RetryDeadWebhooksRequest{}))
			return err
		}},
	} {
		err := call.run()
		if connect.CodeOf(err) != connect.CodeFailedPrecondition {
			t.Errorf("%s code = %v, want FailedPrecondition", call.name, connect.CodeOf(err))
		}
		if err == nil || !strings.Contains(err.Error(), "BILLING_PROVIDER") {
			t.Errorf("%s message = %v", call.name, err)
		}
	}

	invoices, err := h.service.ListInvoices(ctx, connect.NewRequest(&billingv1.ListInvoicesRequest{}))
	if err != nil {
		t.Fatalf("ListInvoices: %v", err)
	}
	if len(invoices.Msg.GetInvoices()) != 0 || invoices.Msg.GetLive() {
		t.Fatalf("invoices = %#v", invoices.Msg)
	}

	if err := h.service.Probe(ctx); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	report, err := h.service.Rebuild(ctx, false)
	if err != nil || report.Count != 0 {
		t.Fatalf("Rebuild = (%#v, %v)", report, err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- h.service.Run(runCtx) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop with its context")
	}
}

func mustNextWebhooks(t *testing.T, h *harness) []platform.QueuedWebhook {
	t.Helper()
	entries, err := h.store.NextWebhooks(context.Background(), 32, h.clock.Now())
	if err != nil {
		t.Fatalf("NextWebhooks: %v", err)
	}
	return entries
}

// TestFakeReplayAndStaleHooksBehaveLikeStripe covers the two acceptance
// gestures from spec2 §10.3: replaying a delivered event must change nothing,
// and a delivery signed outside the tolerance window must be refused.
func TestFakeReplayAndStaleHooksBehaveLikeStripe(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	ctx := context.Background()

	created, err := h.service.CreateCheckoutSession(ctx, newCheckoutRequest("z1"))
	if err != nil {
		t.Fatalf("CreateCheckoutSession: %v", err)
	}
	h.completeCheckoutInBrowser(created.Msg.GetUrl())
	h.drain()

	events := h.fake.Events()
	if len(events) != 3 {
		t.Fatalf("the fake generated %d events, want 3", len(events))
	}
	before := len(h.events.all())

	replay := fakePost(t, h.fake.BaseURL()+"/__fake/replay/"+events[0].ID)
	if replay != http.StatusOK {
		t.Fatalf("replay status = %d", replay)
	}
	h.drain()
	if after := len(h.events.all()); after != before {
		t.Fatalf("a replay recorded %d new activity events", after-before)
	}

	// The last delivery the fake made is the stale one; the webhook route
	// refused it, which is what the delivery log shows.
	if status := fakePost(t, h.fake.BaseURL()+"/__fake/stale/"+events[0].ID); status != http.StatusOK {
		t.Fatalf("stale hook status = %d", status)
	}
	deliveries := h.fake.Deliveries()
	last := deliveries[len(deliveries)-1]
	if last.Status != http.StatusBadRequest {
		t.Fatalf("stale delivery was answered %d, want 400", last.Status)
	}
	if !strings.Contains(last.Body, outcomeTooOld) {
		t.Fatalf("stale delivery body = %q", last.Body)
	}
}

func fakePost(t *testing.T, url string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("post %s: %v", url, err)
	}
	defer closeBody(t, response)
	return response.StatusCode
}

// TestPayingTwiceIsRefusedRatherThanSold covers the double charge.
//
// reuseOpenCheckout only short-circuited while the stored session was still
// "open". A customer who paid and clicked Subscribe again before the webhook
// landed — the row still says PENDING — fell straight through to a second
// hosted checkout for a domain they had just paid for. Completing it creates a
// second Stripe subscription: A$10 a month twice for one domain, with the row
// and the reconciler tracking only one of them, so nothing downstream notices.
func TestPayingTwiceIsRefusedRatherThanSold(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	ctx := context.Background()

	first, err := h.service.CreateCheckoutSession(ctx, connect.NewRequest(&billingv1.CreateCheckoutSessionRequest{
		ZoneId: "z1", ReturnPath: "/billing",
	}))
	if err != nil {
		t.Fatalf("first checkout: %v", err)
	}

	// The customer pays. The webhook has not been delivered, so the row is
	// still PENDING — which is the whole point of the race.
	h.completeCheckoutInBrowser(first.Msg.GetUrl())
	if got := h.subscription("z1").State; got != StatePending {
		t.Fatalf("row state = %q before the delivery, want %q", got, StatePending)
	}

	second, err := h.service.CreateCheckoutSession(ctx, connect.NewRequest(&billingv1.CreateCheckoutSessionRequest{
		ZoneId: "z1", ReturnPath: "/billing",
	}))
	if err == nil {
		t.Fatalf("a second checkout was opened for a domain already paid for: %s", second.Msg.GetUrl())
	}
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("code = %s, want FailedPrecondition", connect.CodeOf(err))
	}

	// And only one checkout was ever opened against Stripe, so there is only
	// one thing the customer can be charged for.
	opened := 0
	for _, request := range h.fake.Requests() {
		if request.Method == http.MethodPost && request.Path == "/v1/checkout/sessions" {
			opened++
		}
	}
	if opened != 1 {
		t.Errorf("%d checkout sessions were created for one domain, want 1", opened)
	}
}

// TestCheckoutCanBeReopenedAfterTheKeyWasUsed covers the idempotency key that
// never advanced.
//
// nextAttempt returned value+1 without ever storing the increment, so every
// call produced the same key while the parameters underneath it moved with the
// clock. Stripe answers that with a 400 idempotency_error, which the console
// renders as "the billing provider didn't answer, try again" — advice that
// could not work for 24 hours.
func TestCheckoutCanBeReopenedAfterTheKeyWasUsed(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{})
	ctx := context.Background()

	first, err := h.service.CreateCheckoutSession(ctx, connect.NewRequest(&billingv1.CreateCheckoutSessionRequest{
		ZoneId: "z1", ReturnPath: "/billing",
	}))
	if err != nil {
		t.Fatalf("first checkout: %v", err)
	}

	// The customer wanders off and the session expires.
	h.clock.Advance(checkoutWindow + time.Minute)

	second, err := h.service.CreateCheckoutSession(ctx, connect.NewRequest(&billingv1.CreateCheckoutSessionRequest{
		ZoneId: "z1", ReturnPath: "/billing",
	}))
	if err != nil {
		t.Fatalf("reopening a checkout after the first expired: %v", err)
	}
	if second.Msg.GetSessionId() == first.Msg.GetSessionId() {
		t.Error("the second checkout reused the first session; the attempt counter did not advance")
	}
}
