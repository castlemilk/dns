package mail

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	mailv1 "github.com/castlemilk/dns/gen/go/mail/v1"
	"github.com/castlemilk/dns/gen/go/mail/v1/mailv1connect"
	platformv1 "github.com/castlemilk/dns/gen/go/platform/v1"
	"github.com/castlemilk/dns/internal/config"
	"github.com/castlemilk/dns/internal/enginedns"
	"github.com/castlemilk/dns/internal/mail/fakemail"
	"github.com/castlemilk/dns/internal/mail/stalwart"
	"github.com/castlemilk/dns/internal/platform"
	"github.com/castlemilk/dns/internal/zone"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Row states persisted in platform.MailDomainDoc.State.
const (
	StateBinding   = "BINDING"
	StateBound     = "BOUND"
	StateDegraded  = "DEGRADED"
	StateUnbinding = "UNBINDING"
)

// Reasons a row carries. They are copy, so they live next to each other.
const (
	reasonZoneMissing = "zone no longer exists"
	reasonDkimPending = "DKIM keys still generating"
)

// engineName is the bounded telemetry attribute for this facade's engine.
const engineName = "mail"

// Service implements mail.v1.MailService.
type Service struct {
	cfg    config.Mail
	deps   platform.Deps
	engine Engine

	// dkimBudget bounds the in-handler wait for the mail server's DKIM keys.
	// It is a field rather than the constant so a test can prove what happens
	// when the wait expires without sitting for the production budget.
	dkimBudget time.Duration

	// initErr is a construction failure (a URL the client could not accept).
	// It is reported through Status and every mutating RPC rather than
	// panicking, so a bad value degrades one engine instead of the process.
	initErr error

	statusMu           sync.RWMutex
	reachable          bool
	checkedAt          time.Time
	reason             string
	edition            string
	missingPermissions []string

	// deliveries accumulates the mail server's delivery events between flushes;
	// deliverySince is when this process started counting, so a window shorter
	// than the counters claim is reported as the shorter window rather than as
	// a complete one. receiver holds what the delivery-event route has actually
	// seen, which is what decides whether a count may be reported at all.
	deliveries    *deliveryTally
	deliverySince time.Time
	receiver      *deliveryReceiver

	poke chan struct{}
}

// New builds the mail facade. The signature is frozen by WP0.
func New(cfg config.Mail, deps platform.Deps) *Service {
	service := newService(cfg, deps)
	if !cfg.Configured() {
		return service
	}
	if cfg.Fake() {
		service.engine = fakemail.New()
		return service
	}
	client, err := stalwart.New(cfg.APIURL, cfg.APIToken)
	if err != nil {
		// The message names the variable, never the value.
		service.initErr = err
		deps.Log().Error("build the mail engine client", "error", err)
		return service
	}
	service.engine = client
	return service
}

// newWithEngine builds a facade around a supplied engine. Tests use it; there
// is no production path that injects an engine, because the engine is decided
// by configuration alone.
func newWithEngine(cfg config.Mail, deps platform.Deps, engine Engine) *Service {
	service := newService(cfg, deps)
	service.engine = engine
	return service
}

// newService is the shared construction both entry points use.
func newService(cfg config.Mail, deps platform.Deps) *Service {
	return &Service{
		cfg:           cfg,
		deps:          deps,
		dkimBudget:    dkimReadyBudget,
		deliveries:    newDeliveryTally(),
		deliverySince: deps.Now(),
		receiver:      newDeliveryReceiver(),
		poke:          make(chan struct{}, 1),
	}
}

// Kind identifies this engine in the platform status.
func (s *Service) Kind() platformv1.EngineKind { return platformv1.EngineKind_ENGINE_KIND_MAIL }

// Status is what the Settings page renders. It never blocks on the engine: the
// prober owns the freshness of these fields.
func (s *Service) Status() platform.EngineStatus {
	s.statusMu.RLock()
	defer s.statusMu.RUnlock()
	status := platform.EngineStatus{
		Kind:         platformv1.EngineKind_ENGINE_KIND_MAIL,
		Configured:   s.cfg.Configured(),
		Reachable:    s.reachable,
		CheckedAt:    s.checkedAt,
		Reason:       s.reason,
		MissingEnv:   s.cfg.MissingEnv(),
		Provider:     s.provider(),
		EndpointHost: s.cfg.EndpointHost(),
		Version:      s.edition,
	}
	if status.CheckedAt.IsZero() {
		status.CheckedAt = s.deps.Now()
	}
	return status
}

func (s *Service) provider() string {
	switch {
	case !s.cfg.Configured():
		return ""
	case s.cfg.Fake():
		return "fake"
	default:
		return "stalwart"
	}
}

// Probe is the prober's health check: the credential works, the permissions it
// holds cover what this facade calls, and the server still calls itself
// MAIL_HOSTNAME.
//
// The hostname check is part of reachability rather than a warning because a
// server whose hostname moved cannot be published for: the MX record this
// facade writes would name a host that no longer answers for these mailboxes.
func (s *Service) Probe(ctx context.Context) error {
	if !s.cfg.Configured() {
		s.setStatus(false, "", nil, nil)
		return nil
	}
	if s.engine == nil {
		s.setStatus(false, "", nil, s.initErr)
		return s.initErr
	}

	info, err := observeEngine(ctx, s, "account", s.engine.Account)
	if err != nil {
		s.setStatus(false, "", nil, err)
		return err
	}
	missing := missingPermissions(info.Permissions)

	settings, err := observeEngine(ctx, s, "system_settings", s.engine.SystemSettings)
	if err != nil {
		s.setStatus(false, info.Edition, missing, err)
		return err
	}
	if !strings.EqualFold(settings.DefaultHostname, s.cfg.Hostname) {
		mismatch := copyError(hostnameMismatch(s.cfg.Hostname, settings.DefaultHostname))
		s.setStatus(false, info.Edition, missing, mismatch)
		return mismatch
	}
	s.setStatus(true, info.Edition, missing, nil)
	return nil
}

func (s *Service) setStatus(reachable bool, edition string, missing []string, err error) {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	s.reachable = reachable
	s.checkedAt = s.deps.Now()
	s.edition = edition
	s.missingPermissions = missing
	if reachable {
		s.reason = ""
		return
	}
	s.reason = reasonOf(err)
}

// missingPermissions names the sys* permissions the credential does not hold.
// An absent name is reported, never assumed: a key that silently cannot destroy
// a domain would fail an unbind halfway through instead of at the Settings page.
func missingPermissions(held []string) []string {
	have := make(map[string]struct{}, len(held))
	for _, name := range held {
		have[name] = struct{}{}
	}
	var missing []string
	for _, name := range RequiredPermissions {
		if _, ok := have[name]; !ok {
			missing = append(missing, name)
		}
	}
	return missing
}

// Observer receives operator DNS mutations that can invalidate the published
// mail set: a REPLACE import, a deleted record, a deleted zone. Each one wakes
// the reconciler rather than writing from the observer, so every write stays on
// the single reconciler path.
func (s *Service) Observer() enginedns.ZoneObserver {
	if !s.cfg.Configured() {
		return enginedns.NopObserver{}
	}
	return observer{service: s}
}

type observer struct{ service *Service }

func (o observer) ZoneReplaced(_ context.Context, _ string) { o.service.wake() }

// RecordDeleted wakes the reconciler for any deletion. DeleteRecord refuses
// every record an engine owns before it notifies, so the deleted record is
// always the operator's — and the operator's record is exactly the thing that
// can be standing in the way of a record this engine could not publish.
func (o observer) RecordDeleted(_ context.Context, _ string, _ zone.Record) {
	o.service.wake()
}

func (o observer) ZoneDeleted(_ context.Context, _ string) { o.service.wake() }

// wake nudges the reconciler without blocking the caller. A pending wake is
// enough: the pass reads every row anyway.
func (s *Service) wake() {
	select {
	case s.poke <- struct{}{}:
	default:
	}
}

// Run drives the mail reconciler: every reconcileInterval, and whenever an
// operator mutation or a bind wakes it.
func (s *Service) Run(ctx context.Context) error {
	if !s.cfg.Configured() || s.engine == nil {
		<-ctx.Done()
		return nil
	}
	timer := time.NewTimer(jitter(reconcileInterval))
	defer timer.Stop()
	// The flush ticker shares this goroutine so the delivery counters have
	// exactly one writer, and so a burst of events never turns into a burst of
	// store writes.
	flush := time.NewTicker(deliveryFlushInterval)
	defer flush.Stop()
	for {
		select {
		case <-ctx.Done():
			// One last flush, so a clean shutdown does not throw away the
			// events counted since the previous tick.
			s.flushDeliveries(context.WithoutCancel(ctx))
			return nil
		case <-flush.C:
			s.flushDeliveries(ctx)
			continue
		case <-timer.C:
		case <-s.poke:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}
		s.flushDeliveries(ctx)
		if err := s.ReconcileAll(ctx); err != nil && ctx.Err() == nil {
			s.deps.Log().Warn("reconcile mail domains", "error", err)
		}
		timer.Reset(jitter(reconcileInterval))
	}
}

// requireEngine is the guard every RPC that touches the engine runs first.
func (s *Service) requireEngine() error {
	if !s.cfg.Configured() || s.engine == nil {
		return notConfigured()
	}
	return nil
}

// GetMailStatus reports the engine, the mail hostname, what statistics this
// edition can honestly provide, and the live totals.
func (s *Service) GetMailStatus(
	ctx context.Context,
	_ *connect.Request[mailv1.GetMailStatusRequest],
) (*connect.Response[mailv1.GetMailStatusResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, connect.NewError(connect.CodeCanceled, err)
	}
	s.statusMu.RLock()
	edition := s.edition
	missing := slices.Clone(s.missingPermissions)
	s.statusMu.RUnlock()

	// The delivery figures are available only once the mail server has been
	// observed posting to the receiver; until then the note says so and names
	// the half of the wiring this control plane cannot check for itself.
	available, note := s.deliveryStatsState()
	response := &mailv1.GetMailStatusResponse{
		Engine:                 s.Status().Proto(),
		MailHostname:           s.cfg.Hostname,
		Edition:                edition,
		MissingPermissions:     missing,
		DeliveryStatsAvailable: available,
		DeliveryStatsNote:      note,
	}
	if available {
		response.DeliveryWindowHours = uint32(deliveryWindow / time.Hour)
	}
	if lastEvent := s.receiverLastEventAt(); !lastEvent.IsZero() {
		response.ReceiverLastEventAt = timestamppb.New(lastEvent.UTC())
	}
	if !s.cfg.Configured() {
		return connect.NewResponse(response), nil
	}
	response.MailboxesPerDomain = uint32(max(s.cfg.MailboxesPerDomain, 0))

	docs, err := s.deps.Store.ListMailDomains(ctx)
	if err != nil {
		return nil, internalError(err)
	}
	response.Domains = uint32(len(docs))
	if s.engine == nil {
		return connect.NewResponse(response), nil
	}
	for _, doc := range docs {
		if doc.EngineDomainID == "" {
			continue
		}
		accounts, err := observeEngineArg(ctx, s, "list_accounts", doc.EngineDomainID, s.engine.ListAccounts)
		if err != nil {
			// One unreachable read must not turn the whole page into an error;
			// the engine row already says the engine did not answer. The sums
			// reached here are partial, and a partial sum is indistinguishable
			// from a complete one on the wire, so counts_available stays false
			// and the console must not present them as totals.
			return connect.NewResponse(response), nil
		}
		response.Mailboxes += uint32(len(accounts))
		for _, account := range accounts {
			response.UsedBytes += account.UsedBytes
		}
	}
	// Every bound domain answered.
	response.CountsAvailable = true
	return connect.NewResponse(response), nil
}

// internalError is the catch-all for a store failure: the operator sees a
// neutral sentence and the detail goes to the log.
func internalError(err error) error {
	return connect.NewError(connect.CodeInternal, fmt.Errorf("internal server error: %w", errors.New(reasonOf(err))))
}

// observeEngine runs one no-argument engine call and records it.
func observeEngine[T any](ctx context.Context, s *Service, operation string, fn func(context.Context) (T, error)) (T, error) {
	started := time.Now()
	value, err := fn(ctx)
	outcome, class := engineOperationOutcome(err)
	s.deps.Meter().EngineRequest(ctx, engineName, operation, outcome, class, time.Since(started))
	return value, err
}

// observeEngineArg runs one single-argument engine call and records it.
func observeEngineArg[A, T any](ctx context.Context, s *Service, operation string, arg A, fn func(context.Context, A) (T, error)) (T, error) {
	started := time.Now()
	value, err := fn(ctx, arg)
	outcome, class := engineOperationOutcome(err)
	s.deps.Meter().EngineRequest(ctx, engineName, operation, outcome, class, time.Since(started))
	return value, err
}

// observeEngineFind runs one engine call that answers "value, found" and
// records it.
func observeEngineFind[A, T any](ctx context.Context, s *Service, operation string, arg A, fn func(context.Context, A) (T, bool, error)) (T, bool, error) {
	started := time.Now()
	value, found, err := fn(ctx, arg)
	outcome, class := engineOperationOutcome(err)
	s.deps.Meter().EngineRequest(ctx, engineName, operation, outcome, class, time.Since(started))
	return value, found, err
}

// observeEngineDo runs one engine call with no result and records it.
func observeEngineDo(ctx context.Context, s *Service, operation string, fn func(context.Context) error) error {
	started := time.Now()
	err := fn(ctx)
	outcome, class := engineOperationOutcome(err)
	s.deps.Meter().EngineRequest(ctx, engineName, operation, outcome, class, time.Since(started))
	return err
}

var (
	_ mailv1connect.MailServiceHandler = (*Service)(nil)
	_ platform.Probe                   = (*Service)(nil)
	_ platform.Rebuilder               = (*Service)(nil)
)
