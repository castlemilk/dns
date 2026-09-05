// Package billing is the facade over Stripe. The browser never talks to
// Stripe: it calls billing.v1.BillingService on this control plane, which holds
// the secret key, verifies webhook signatures, and keeps one durable row per
// domain in the platform store.
//
// Billing is informational in this release — no RPC anywhere gates on
// subscription state — so an unconfigured or unreachable provider degrades to
// "no billing information", never to a lockout.
package billing

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	billingv1 "github.com/castlemilk/dns/gen/go/billing/v1"
	"github.com/castlemilk/dns/gen/go/billing/v1/billingv1connect"
	platformv1 "github.com/castlemilk/dns/gen/go/platform/v1"
	"github.com/castlemilk/dns/internal/activity"
	"github.com/castlemilk/dns/internal/billing/fakestripe"
	"github.com/castlemilk/dns/internal/billing/stripe"
	"github.com/castlemilk/dns/internal/config"
	"github.com/castlemilk/dns/internal/platform"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Request-shaping bounds and validation.
const (
	// checkoutWindow is how long a Checkout session stays open. Stripe's
	// maximum is 24 h; 23 h keeps clock skew from having the session refused.
	checkoutWindow = 23 * time.Hour
	// defaultInvoiceLimit and maxInvoiceLimit bound one invoices page.
	defaultInvoiceLimit = 25
	maxInvoiceLimit     = 100
	// defaultReturnPath is where Checkout and the portal come back to.
	defaultReturnPath = "/billing"
)

var (
	// returnPathPattern is the exact set of pages the console sends a customer
	// back to. Anything else would make this RPC an open redirect through
	// Stripe.
	returnPathPattern = regexp.MustCompile(`^/(billing|domains/[a-z0-9.-]+)$`)
	// sessionIDPattern and invoiceCursorPattern bound what is forwarded to the
	// provider as an object id.
	sessionIDPattern     = regexp.MustCompile(`^cs_[A-Za-z0-9_]{8,}$`)
	invoiceCursorPattern = regexp.MustCompile(`^in_[A-Za-z0-9]{8,}$`)
)

// Service implements billing.v1.BillingService, the webhook queue worker, the
// reconciler and the checkout expirer.
type Service struct {
	cfg      config.Billing
	deps     platform.Deps
	provider Provider
	fake     *fakestripe.Server
	webhook  *webhookHandler
	apiBase  string

	queueWake     chan struct{}
	reconcileWake chan struct{}
	// rows serializes the read-modify-write on one domain's row across the
	// queue worker, the reconciler, the expirer and the RPCs.
	rows rowLocks

	mu           sync.RWMutex
	reachable    bool
	probed       bool
	statusReason string
	checkedAt    time.Time
	price        Price
	priceLabel   string
	reconciledAt time.Time
	attempts     map[string]int
}

// New builds the billing facade. The second result is the Stripe webhook HTTP
// handler, or nil when billing is not configured. The signature is frozen:
// internal/app only mounts what it is handed, so the fake Stripe server's
// lifecycle lives here rather than in the wiring.
func New(cfg config.Billing, deps platform.Deps) (*Service, http.Handler) {
	service := &Service{
		cfg:           cfg,
		deps:          deps,
		apiBase:       cfg.APIBase,
		queueWake:     make(chan struct{}, 1),
		reconcileWake: make(chan struct{}, 1),
		checkedAt:     deps.Now(),
		attempts:      map[string]int{},
	}
	if !cfg.Configured() {
		return service, nil
	}

	var listener net.Listener
	if cfg.Fake() {
		bound, err := net.Listen("tcp", cfg.FakeAddr)
		if err != nil {
			// A bind failure is not fatal: the control plane keeps serving DNS
			// and the Settings page says billing is unreachable, naming the
			// variable the operator set.
			deps.Log().Error("bind the fake Stripe server", "error", err)
			service.statusReason = "BILLING_FAKE_ADDR could not be bound"
			return service, nil
		}
		listener = bound
		service.apiBase = "http://" + bound.Addr().String()
	}

	client, err := stripe.New(stripe.Config{
		SecretKey:      cfg.SecretKey,
		APIBase:        service.apiBase,
		WebhookSecrets: cfg.WebhookSecrets,
	})
	if err != nil {
		closeListener(listener, deps)
		deps.Log().Error("build the Stripe client", "error", reason(err))
		service.statusReason = "the Stripe client could not be built from the configured variables"
		return service, nil
	}
	service.provider = client
	service.webhook = newWebhookHandler(service)

	if listener != nil {
		fake, err := fakestripe.New(fakestripe.Options{
			Listener:  listener,
			StatePath: cfg.FakeStatePath,
			Secret:    cfg.WebhookSecrets[0],
			Webhook:   service.webhook,
			PriceID:   cfg.PriceID,
			Clock:     deps.Clock,
			Logger:    deps.Log(),
		})
		if err != nil {
			closeListener(listener, deps)
			deps.Log().Error("start the fake Stripe server", "error", reason(err))
			service.statusReason = "the fake Stripe server could not be started"
			return service, service.webhook
		}
		service.fake = fake
	}
	return service, service.webhook
}

func closeListener(listener net.Listener, deps platform.Deps) {
	if listener == nil {
		return
	}
	if err := listener.Close(); err != nil {
		deps.Log().Warn("close the fake Stripe listener", "error", err)
	}
}

// FakeServer exposes the in-process fake for tests and for the acceptance
// script. It is nil unless BILLING_PROVIDER=fake.
func (s *Service) FakeServer() *fakestripe.Server { return s.fake }

// WebhookHandler is the route internal/app mounts. It is nil when billing is
// not configured.
func (s *Service) WebhookHandler() http.Handler {
	if s.webhook == nil {
		return nil
	}
	return s.webhook
}

// configured reports whether there is a provider to call.
func (s *Service) configured() bool { return s.provider != nil && s.deps.Store != nil }

// Run serves the fake Stripe server (when there is one) next to the webhook
// queue, the reconciler and the checkout expirer.
func (s *Service) Run(ctx context.Context) error {
	if !s.configured() {
		<-ctx.Done()
		return nil
	}
	group, groupCtx := errgroup.WithContext(ctx)
	if s.fake != nil {
		group.Go(func() error { return s.fake.Serve(groupCtx) })
	}
	group.Go(func() error { return s.runQueue(groupCtx) })
	group.Go(func() error { return s.runReconciler(groupCtx) })
	group.Go(func() error { return s.runExpirer(groupCtx) })
	if err := group.Wait(); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

func (s *Service) Kind() platformv1.EngineKind { return platformv1.EngineKind_ENGINE_KIND_BILLING }

// Probe reads the configured price. It is the whole health check: a key that
// can retrieve its own price can create a Checkout session, and a price that is
// not a monthly recurring one is a misconfiguration an operator must see.
func (s *Service) Probe(ctx context.Context) error {
	if !s.configured() {
		return nil
	}
	started := s.deps.Now()
	price, err := s.provider.GetPrice(ctx, s.cfg.PriceID)
	s.observe(ctx, "get_price", started, err)
	if err != nil {
		s.setUnreachable(reason(err))
		return err
	}
	if price.Interval != "month" {
		err := errors.New("STRIPE_PRICE_ID is not a monthly recurring price")
		s.setUnreachable(err.Error())
		return err
	}
	s.setReachable(price)
	return nil
}

func (s *Service) setUnreachable(text string) {
	s.mu.Lock()
	s.reachable = false
	s.probed = true
	s.statusReason = text
	s.checkedAt = s.deps.Now()
	s.price = Price{}
	s.priceLabel = ""
	s.mu.Unlock()
}

func (s *Service) setReachable(price Price) {
	s.mu.Lock()
	recovered := s.probed && !s.reachable
	s.reachable = true
	s.probed = true
	s.statusReason = ""
	s.checkedAt = s.deps.Now()
	s.price = price
	s.priceLabel = formatPriceLabel(price)
	s.mu.Unlock()
	if recovered {
		// An outage may have swallowed a delivery; converge now rather than at
		// the next six-hourly pass.
		s.wakeReconciler()
	}
}

func (s *Service) setReconciledAt(at time.Time) {
	s.mu.Lock()
	s.reconciledAt = at
	s.mu.Unlock()
}

// Status is the cached view the prober publishes. It never calls the provider.
func (s *Service) Status() platform.EngineStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	status := platform.EngineStatus{
		Kind:         platformv1.EngineKind_ENGINE_KIND_BILLING,
		Configured:   s.cfg.Configured(),
		Reachable:    s.reachable,
		CheckedAt:    s.checkedAt,
		Reason:       s.statusReason,
		MissingEnv:   s.cfg.MissingEnv(),
		Provider:     s.cfg.Provider,
		EndpointHost: endpointHost(s.apiBase),
	}
	if s.reachable {
		status.Version = stripe.APIVersion()
	}
	return status
}

// PriceLabel is the server-formatted price and the only source of the price
// anywhere: the console reads it from GetBillingStatus, the landing page from
// GET /public/v1/plan. It is "" until the price probe has succeeded.
func (s *Service) PriceLabel() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.priceLabel
}

func (s *Service) livemode() bool {
	if s.provider == nil {
		return false
	}
	return s.provider.Livemode()
}

// --- RPCs -----------------------------------------------------------------

func (s *Service) GetBillingStatus(
	ctx context.Context,
	_ *connect.Request[billingv1.GetBillingStatusRequest],
) (*connect.Response[billingv1.GetBillingStatusResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, connect.NewError(connect.CodeCanceled, err)
	}
	s.mu.RLock()
	price := s.price
	label := s.priceLabel
	reconciled := s.reconciledAt
	s.mu.RUnlock()

	response := &billingv1.GetBillingStatusResponse{
		Engine:            s.Status().Proto(),
		Provider:          s.cfg.Provider,
		Livemode:          s.livemode(),
		InformationalOnly: true,
		PolicyNote:        platform.PolicyNote,
		WebhookConfigured: s.cfg.Configured() && len(s.cfg.WebhookSecrets) > 0,
		WebhookSecrets:    uint32(len(s.cfg.WebhookSecrets)),
	}
	if label != "" {
		response.Price = &billingv1.Price{
			Id:         price.ID,
			UnitAmount: price.UnitAmount,
			Currency:   price.Currency,
			Interval:   price.Interval,
			Label:      label,
		}
	}
	if !reconciled.IsZero() {
		response.ReconciledAt = timestamppb.New(reconciled.UTC())
	}
	if s.deps.Store != nil {
		stats, err := s.deps.Store.WebhookStats(ctx)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, errors.New("internal server error"))
		}
		response.PendingWebhooks = stats.Pending
		response.DeadWebhooks = stats.Dead
		if !stats.LastReceivedAt.IsZero() {
			response.LastWebhookAt = timestamppb.New(stats.LastReceivedAt.UTC())
		}
	}
	return connect.NewResponse(response), nil
}

// GetBillingSummary is not one of §1.1's Get*Status/List* exceptions, so an
// unconfigured engine refuses it rather than answering an empty summary that
// reads like "no domain is billed".
func (s *Service) GetBillingSummary(
	ctx context.Context,
	_ *connect.Request[billingv1.GetBillingSummaryRequest],
) (*connect.Response[billingv1.GetBillingSummaryResponse], error) {
	if !s.configured() {
		return nil, notConfigured()
	}
	zones, err := s.deps.Zones.ListZones(ctx)
	if err != nil {
		return nil, s.internalError("list zones for the billing summary", err)
	}
	docs, err := s.deps.Store.ListSubscriptions(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal server error"))
	}
	byZone := make(map[string]platform.SubscriptionDoc, len(docs))
	for _, doc := range docs {
		byZone[doc.ZoneID] = doc
	}

	response := &billingv1.GetBillingSummaryResponse{}
	seen := make(map[string]struct{}, len(zones))
	sort.Slice(zones, func(left, right int) bool { return zones[left].Name < zones[right].Name })
	for _, value := range zones {
		seen[value.ID] = struct{}{}
		doc, ok := byZone[value.ID]
		if !ok {
			doc = platform.SubscriptionDoc{ZoneID: value.ID, State: StateUnbilled}
		}
		doc.ZoneName = value.Name
		doc.ZoneMissing = false
		response.Domains = append(response.Domains, domainProto(doc))
	}
	// Rows whose zone is gone stay visible so the operator can still cancel the
	// subscription in the portal.
	var orphans []platform.SubscriptionDoc
	for _, doc := range docs {
		if _, ok := seen[doc.ZoneID]; ok {
			continue
		}
		doc.ZoneMissing = true
		orphans = append(orphans, doc)
	}
	sort.Slice(orphans, func(left, right int) bool { return orphans[left].ZoneName < orphans[right].ZoneName })
	for _, doc := range orphans {
		response.Domains = append(response.Domains, domainProto(doc))
	}

	for _, row := range response.GetDomains() {
		switch row.GetState() {
		case billingv1.SubscriptionState_SUBSCRIPTION_STATE_ACTIVE,
			billingv1.SubscriptionState_SUBSCRIPTION_STATE_TRIALING,
			billingv1.SubscriptionState_SUBSCRIPTION_STATE_CANCELING:
			response.ActiveCount++
		case billingv1.SubscriptionState_SUBSCRIPTION_STATE_PAST_DUE,
			billingv1.SubscriptionState_SUBSCRIPTION_STATE_INCOMPLETE:
			response.AttentionCount++
		case billingv1.SubscriptionState_SUBSCRIPTION_STATE_UNSPECIFIED,
			billingv1.SubscriptionState_SUBSCRIPTION_STATE_UNBILLED,
			billingv1.SubscriptionState_SUBSCRIPTION_STATE_PENDING,
			billingv1.SubscriptionState_SUBSCRIPTION_STATE_CANCELED:
		}
	}

	customer, found, err := s.deps.Store.GetCustomer(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal server error"))
	}
	response.CustomerExists = found && customer.CustomerID != ""
	if method := customer.PaymentMethod; method.Brand != "" || method.Last4 != "" {
		response.PaymentMethod = &billingv1.PaymentMethod{
			Brand:    method.Brand,
			Last4:    method.Last4,
			ExpMonth: uint32(max(method.ExpMonth, 0)),
			ExpYear:  uint32(max(method.ExpYear, 0)),
		}
	}
	return connect.NewResponse(response), nil
}

func (s *Service) CreateCheckoutSession(
	ctx context.Context,
	request *connect.Request[billingv1.CreateCheckoutSessionRequest],
) (*connect.Response[billingv1.CreateCheckoutSessionResponse], error) {
	if !s.configured() {
		return nil, notConfigured()
	}
	zoneID := strings.TrimSpace(request.Msg.GetZoneId())
	if zoneID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("zone_id: choose a domain to subscribe"))
	}
	returnPath, err := checkReturnPath(request.Msg.GetReturnPath())
	if err != nil {
		return nil, err
	}
	zoneName, found, err := s.zoneName(ctx, zoneID)
	if err != nil {
		return nil, s.internalError("read the zone a checkout was requested for", err)
	}
	if !found {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("that domain no longer exists"))
	}

	// Held until the row has been written: a second checkout for the same
	// domain, or a delivery for the one being opened, must not interleave with
	// the decision below.
	defer s.rows.lock(zoneID)()
	doc, err := s.loadDoc(ctx, zoneID, zoneName)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal server error"))
	}
	switch doc.State {
	case StateActive, StateTrialing, StatePastDue, StateCanceling:
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("%s already has a subscription — manage it in the billing portal", zoneName))
	case StateUnbilled, StatePending, StateCanceled, StateIncomplete:
	}

	now := s.deps.Now()
	if reused, ok, err := s.reuseOpenCheckout(ctx, doc, now); err != nil {
		return nil, err
	} else if ok {
		return connect.NewResponse(reused), nil
	}

	customerID, err := s.ensureCustomer(ctx)
	if err != nil {
		return nil, providerError(err)
	}

	attempt := s.nextAttempt(ctx, doc)
	expires := now.Add(checkoutWindow)
	input := CheckoutInput{
		CustomerID:     customerID,
		ZoneID:         zoneID,
		ZoneName:       zoneName,
		PriceID:        s.cfg.PriceID,
		SuccessURL:     s.cfg.PublicURL + "/billing/success?session_id={CHECKOUT_SESSION_ID}&return=" + url.QueryEscape(returnPath),
		CancelURL:      s.cfg.PublicURL + returnPath + "?canceled=" + url.QueryEscape(zoneID),
		IdempotencyKey: fmt.Sprintf("checkout:%s:%d", zoneID, attempt),
		ExpiresAt:      expires,
	}
	started := s.deps.Now()
	session, err := s.provider.CreateCheckout(ctx, input)
	s.observe(ctx, "create_checkout", started, err)
	if err != nil {
		return nil, providerError(err)
	}
	if session.Status != "open" || session.URL == "" {
		// The idempotency key replayed a session that is already settled (a
		// crash-and-restart can land on the same key). Ask for a new one.
		attempt = s.bumpAttempt(zoneID)
		input.IdempotencyKey = fmt.Sprintf("checkout:%s:%d", zoneID, attempt)
		retried := s.deps.Now()
		session, err = s.provider.CreateCheckout(ctx, input)
		s.observe(ctx, "create_checkout", retried, err)
		if err != nil {
			return nil, providerError(err)
		}
	}
	if session.URL == "" {
		return nil, connect.NewError(connect.CodeUnavailable, copyError(Unreachable))
	}
	if !session.ExpiresAt.IsZero() {
		expires = session.ExpiresAt
	}

	if err := s.deps.Store.PutCheckout(ctx, platform.CheckoutDoc{
		V:         platform.DocVersion,
		SessionID: session.ID,
		ZoneID:    zoneID,
		CreatedAt: now,
		ExpiresAt: expires,
		Attempt:   attempt,
	}); err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal server error"))
	}
	doc.State = StatePending
	doc.CustomerID = customerID
	doc.CheckoutSessionID = session.ID
	doc.CheckoutExpiresAt = &expires
	doc.UpdatedAt = now
	if err := s.deps.Store.PutSubscription(ctx, doc); err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal server error"))
	}
	s.deps.Record(ctx, activity.Event{
		ZoneID: zoneID, ZoneName: zoneName,
		Actor: activity.ActorOperator, Kind: activity.KindBillingCheckoutStarted,
		Severity:      activity.SeverityInfo,
		Summary:       "Started a subscription checkout for " + zoneName,
		Details:       map[string]string{"stripe.session_id": session.ID},
		CorrelationID: session.ID,
	})

	return connect.NewResponse(&billingv1.CreateCheckoutSessionResponse{
		Url:       session.URL,
		SessionId: session.ID,
		ExpiresAt: timestamppb.New(expires.UTC()),
	}), nil
}

// reuseOpenCheckout hands back the URL of an open, unexpired session rather
// than opening a second one for the same domain.
func (s *Service) reuseOpenCheckout(
	ctx context.Context,
	doc platform.SubscriptionDoc,
	now time.Time,
) (*billingv1.CreateCheckoutSessionResponse, bool, error) {
	if doc.State != StatePending || doc.CheckoutSessionID == "" {
		return nil, false, nil
	}
	if doc.CheckoutExpiresAt == nil || !doc.CheckoutExpiresAt.After(now) {
		return nil, false, nil
	}
	started := s.deps.Now()
	session, err := s.provider.GetCheckout(ctx, doc.CheckoutSessionID)
	s.observe(ctx, "get_checkout", started, err)
	if err != nil || session.Status != "open" || session.URL == "" {
		return nil, false, nil
	}
	return &billingv1.CreateCheckoutSessionResponse{
		Url:       session.URL,
		SessionId: session.ID,
		ExpiresAt: timestamppb.New(doc.CheckoutExpiresAt.UTC()),
	}, true, nil
}

// ensureCustomer resolves the one billing customer this deployment uses: the
// stored id, else the customer Stripe already has for the configured email,
// else a new one. Looking the email up before creating is what keeps a rebuilt
// store — or an expired idempotency window — from opening a second customer.
func (s *Service) ensureCustomer(ctx context.Context) (string, error) {
	doc, found, err := s.deps.Store.GetCustomer(ctx)
	if err != nil {
		return "", err
	}
	if found && doc.CustomerID != "" {
		return doc.CustomerID, nil
	}
	started := s.deps.Now()
	existing, ok, err := s.provider.FindCustomer(ctx, s.cfg.CustomerEmail)
	s.observe(ctx, "find_customer", started, err)
	if err != nil {
		return "", err
	}
	if !ok {
		createStarted := s.deps.Now()
		existing, err = s.provider.EnsureCustomer(ctx, s.cfg.CustomerEmail, customerKey(s.cfg.CustomerEmail))
		s.observe(ctx, "ensure_customer", createStarted, err)
		if err != nil {
			return "", err
		}
	}
	s.storeCustomer(ctx, existing, nil)
	return existing, nil
}

// nextAttempt is the counter in the idempotency key. It is seeded from the
// session this zone last opened, so a retry after a crash reuses the key and
// Stripe replays the session it already created instead of opening a second.
func (s *Service) nextAttempt(ctx context.Context, doc platform.SubscriptionDoc) int {
	s.mu.Lock()
	value, ok := s.attempts[doc.ZoneID]
	s.mu.Unlock()
	if ok {
		return value + 1
	}
	seed := 0
	if doc.CheckoutSessionID != "" {
		if stored, err := s.deps.Store.GetCheckout(ctx, doc.CheckoutSessionID); err == nil {
			seed = stored.Attempt
		}
	}
	s.mu.Lock()
	s.attempts[doc.ZoneID] = seed
	s.mu.Unlock()
	return seed + 1
}

func (s *Service) bumpAttempt(zoneID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attempts[zoneID]++
	return s.attempts[zoneID] + 1
}

func (s *Service) ConfirmCheckout(
	ctx context.Context,
	request *connect.Request[billingv1.ConfirmCheckoutRequest],
) (*connect.Response[billingv1.ConfirmCheckoutResponse], error) {
	if !s.configured() {
		return nil, notConfigured()
	}
	sessionID := strings.TrimSpace(request.Msg.GetSessionId())
	if !sessionIDPattern.MatchString(sessionID) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("session_id: that is not a checkout session id"))
	}
	// The session must be one this control plane created. Without that check the
	// route would read back any session in the Stripe account.
	if _, err := s.deps.Store.GetCheckout(ctx, sessionID); err != nil {
		if errors.Is(err, platform.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("this control plane did not create that checkout session"))
		}
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal server error"))
	}

	result, err := s.completeCheckout(ctx, Event{ID: confirmPrefix + sessionID, Type: "checkout.session.completed"}, sessionID)
	if err != nil {
		return nil, providerError(err)
	}
	response := &billingv1.ConfirmCheckoutResponse{
		Completed:       result.Completed,
		Provider:        s.cfg.Provider,
		PaymentRequired: result.PaymentRequired,
	}
	if result.Doc.ZoneID != "" {
		response.Domain = domainProto(result.Doc)
	}
	return connect.NewResponse(response), nil
}

func (s *Service) CreatePortalSession(
	ctx context.Context,
	request *connect.Request[billingv1.CreatePortalSessionRequest],
) (*connect.Response[billingv1.CreatePortalSessionResponse], error) {
	if !s.configured() {
		return nil, notConfigured()
	}
	flow := strings.TrimSpace(request.Msg.GetFlow())
	switch flow {
	case "", "payment_method_update", "subscription_cancel":
	default:
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("flow: unknown billing portal flow"))
	}
	returnPath, err := checkReturnPath(request.Msg.GetReturnPath())
	if err != nil {
		return nil, err
	}

	customer, found, err := s.deps.Store.GetCustomer(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal server error"))
	}
	if !found || customer.CustomerID == "" {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			errors.New("there is no billing customer yet — subscribe a domain first"))
	}

	input := PortalInput{CustomerID: customer.CustomerID, ReturnURL: s.cfg.PublicURL + returnPath, Flow: flow}
	if flow == "subscription_cancel" {
		zoneID := strings.TrimSpace(request.Msg.GetZoneId())
		if zoneID == "" {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("zone_id: choose the domain to cancel"))
		}
		doc, err := s.deps.Store.GetSubscription(ctx, zoneID)
		if err != nil || doc.SubscriptionID == "" {
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("that domain has no subscription to cancel"))
		}
		input.SubscriptionID = doc.SubscriptionID
	}

	started := s.deps.Now()
	target, err := s.provider.CreatePortal(ctx, input)
	s.observe(ctx, "create_portal", started, err)
	if err != nil {
		return nil, providerError(err)
	}
	return connect.NewResponse(&billingv1.CreatePortalSessionResponse{Url: target}), nil
}

// ListInvoices answers like every other List* RPC: an unconfigured or
// unreachable provider yields an honest empty list with live=false, never an
// error that turns the whole page red.
func (s *Service) ListInvoices(
	ctx context.Context,
	request *connect.Request[billingv1.ListInvoicesRequest],
) (*connect.Response[billingv1.ListInvoicesResponse], error) {
	if !s.configured() {
		return connect.NewResponse(&billingv1.ListInvoicesResponse{}), nil
	}
	limit := int(request.Msg.GetLimit())
	if limit <= 0 {
		limit = defaultInvoiceLimit
	}
	if limit > maxInvoiceLimit {
		limit = maxInvoiceLimit
	}
	cursor := strings.TrimSpace(request.Msg.GetCursor())
	if cursor != "" && !invoiceCursorPattern.MatchString(cursor) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("cursor: that is not an invoice id"))
	}

	customer, found, err := s.deps.Store.GetCustomer(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal server error"))
	}
	if !found || customer.CustomerID == "" {
		return connect.NewResponse(&billingv1.ListInvoicesResponse{Live: true}), nil
	}

	started := s.deps.Now()
	invoices, next, err := s.provider.ListInvoices(ctx, customer.CustomerID, cursor, limit)
	s.observe(ctx, "list_invoices", started, err)
	if err != nil {
		s.deps.Log().Warn("list invoices from the billing provider", "error", reason(err))
		return connect.NewResponse(&billingv1.ListInvoicesResponse{}), nil
	}
	response := &billingv1.ListInvoicesResponse{NextCursor: next, Live: true}
	for _, item := range invoices {
		row := &billingv1.Invoice{
			Id:               item.ID,
			Number:           item.Number,
			ZoneName:         item.ZoneName,
			Total:            item.Total,
			Currency:         item.Currency,
			Status:           item.Status,
			HostedInvoiceUrl: item.HostedURL,
			InvoicePdfUrl:    item.PDFURL,
		}
		if !item.Created.IsZero() {
			row.CreatedAt = timestamppb.New(item.Created.UTC())
		}
		response.Invoices = append(response.Invoices, row)
	}
	return connect.NewResponse(response), nil
}

func (s *Service) RetryDeadWebhooks(
	ctx context.Context,
	_ *connect.Request[billingv1.RetryDeadWebhooksRequest],
) (*connect.Response[billingv1.RetryDeadWebhooksResponse], error) {
	if !s.configured() {
		return nil, notConfigured()
	}
	requeued, err := s.deps.Store.RetryDeadWebhooks(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal server error"))
	}
	if requeued > 0 {
		s.deps.Record(ctx, activity.Event{
			Actor:    activity.ActorOperator,
			Kind:     activity.KindBillingWebhookRetried,
			Severity: activity.SeverityInfo,
			Summary:  fmt.Sprintf("Re-queued %s that could not be applied", plural(requeued, "billing event", "billing events")),
			Details:  map[string]string{"requeued": fmt.Sprint(requeued)},
		})
		s.wakeQueue()
		s.wakeReconciler()
	}
	return connect.NewResponse(&billingv1.RetryDeadWebhooksResponse{Requeued: uint32(max(requeued, 0))}), nil
}

// --- shared helpers -------------------------------------------------------

// confirmPrefix marks the synthesised event ConfirmCheckout applies, so the
// activity log attributes the change to the operator rather than to Stripe.
const confirmPrefix = "confirm-"

func checkReturnPath(raw string) (string, error) {
	path := strings.TrimSpace(raw)
	if path == "" {
		return defaultReturnPath, nil
	}
	if !returnPathPattern.MatchString(path) {
		return "", connect.NewError(connect.CodeInvalidArgument,
			errors.New("return_path: only /billing and /domains/<domain> are allowed"))
	}
	return path, nil
}

// customerKey is the idempotency key for creating the billing customer. It
// carries a hash of the email rather than the email itself, so no address
// reaches a Stripe request header.
func customerKey(email string) string {
	return "customer:" + shortHash(email)
}

func domainProto(doc platform.SubscriptionDoc) *billingv1.DomainBilling {
	row := &billingv1.DomainBilling{
		ZoneId:            doc.ZoneID,
		ZoneName:          doc.ZoneName,
		State:             protoState(doc.State),
		SubscriptionId:    doc.SubscriptionID,
		LastEventId:       doc.LastEventID,
		LastInvoiceStatus: doc.LastInvoiceStatus,
		ZoneMissing:       doc.ZoneMissing,
	}
	if doc.State == StatePending {
		row.CheckoutSessionId = doc.CheckoutSessionID
		if doc.CheckoutExpiresAt != nil {
			row.CheckoutExpiresAt = timestamppb.New(doc.CheckoutExpiresAt.UTC())
		}
	}
	if doc.CurrentPeriodEnd != nil {
		row.CurrentPeriodEnd = timestamppb.New(doc.CurrentPeriodEnd.UTC())
	}
	if doc.CancelAt != nil {
		row.CancelAt = timestamppb.New(doc.CancelAt.UTC())
	}
	if !doc.UpdatedAt.IsZero() {
		row.UpdatedAt = timestamppb.New(doc.UpdatedAt.UTC())
	}
	return row
}

// endpointHost is the host[:port] of a base URL, with no scheme, path or
// userinfo, so it is safe to show an operator.
func endpointHost(base string) string {
	parsed, err := url.Parse(base)
	if err != nil {
		return ""
	}
	return parsed.Host
}

var (
	_ billingv1connect.BillingServiceHandler = (*Service)(nil)
	_ platform.Probe                         = (*Service)(nil)
	_ platform.Rebuilder                     = (*Service)(nil)
	_ platform.PriceReporter                 = (*Service)(nil)
)
