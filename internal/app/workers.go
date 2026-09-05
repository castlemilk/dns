package app

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/castlemilk/dns/internal/activity"
	"github.com/castlemilk/dns/internal/billing"
	"github.com/castlemilk/dns/internal/config"
	"github.com/castlemilk/dns/internal/control"
	"github.com/castlemilk/dns/internal/enginedns"
	"github.com/castlemilk/dns/internal/hosting"
	"github.com/castlemilk/dns/internal/mail"
	"github.com/castlemilk/dns/internal/platform"
	"github.com/castlemilk/dns/internal/telemetry"
	"golang.org/x/sync/errgroup"
)

// gatewayResolveBudget bounds the one synchronous gateway resolution done
// before HTTP starts serving, so a slow resolver delays start-up by seconds,
// never by minutes.
const gatewayResolveBudget = 3 * time.Second

// startupReconcileBudget bounds the single ReconcileAll pass that re-adopts
// engine records after a restore and sweeps orphans.
const startupReconcileBudget = 30 * time.Second

// engines is everything constructed from config for a writer role: the four
// facades, the platform service and the janitor, plus the workers they own.
type engines struct {
	status  *platform.StatusService
	hosting *hosting.Service
	mail    *mail.Service
	billing *billing.Service
	log     *activity.Log
	janitor *platform.Janitor

	uploads http.Handler
	webhook http.Handler
	// mailWebhook receives the mail server's delivery events. It is nil unless
	// MAIL_WEBHOOK_SECRET is set, so a deployment that has not opted in has no
	// such route at all.
	mailWebhook http.Handler
	logger      *slog.Logger
}

// newEngines builds the facades and runs the two start-up steps that must
// finish before the first request: resolving the gateway address set, so an
// AttachSite in the first minute after a restart does not fail "gateway
// unknown", and one reconcile pass, so records restored from a snapshot are
// re-adopted rather than showing as custom until the first periodic pass.
func newEngines(
	ctx context.Context,
	cfg config.Config,
	controlHandler *control.Handler,
	store *platform.Store,
	activityLog *activity.Log,
	logger *slog.Logger,
	metrics *telemetry.Metrics,
) *engines {
	deps := platform.Deps{
		Store:    store,
		Zones:    controlHandler.ZoneMutator(),
		Serial:   enginedns.NewSerializer(),
		Recorder: activityLog,
		Logger:   logger,
		Metrics:  metrics,
	}

	hostingService, uploads := hosting.New(cfg.Hosting, deps)
	mailService := mail.New(cfg.Mail, deps)
	billingService, webhook := billing.New(cfg.Billing, deps)

	probes := []platform.Probe{
		platform.NewDNSProbe(cfg, deps),
		hostingService,
		mailService,
		billingService,
	}
	rebuilders := []platform.Rebuilder{hostingService, mailService, billingService}

	built := &engines{
		status:      platform.NewStatusService(cfg, deps, probes, rebuilders),
		hosting:     hostingService,
		mail:        mailService,
		billing:     billingService,
		log:         activityLog,
		janitor:     platform.NewJanitor(store, cfg, deps),
		uploads:     uploads,
		webhook:     webhook,
		mailWebhook: mailService.WebhookHandler(),
		logger:      logger,
	}

	// Observers are added after construction: the engines are built after the
	// control handler that notifies them.
	controlHandler.AddZoneObserver(hostingService.Observer())
	controlHandler.AddZoneObserver(mailService.Observer())

	if cfg.Hosting.Configured() {
		resolveCtx, cancel := context.WithTimeout(ctx, gatewayResolveBudget)
		if err := hostingService.ResolveGatewayOnce(resolveCtx); err != nil {
			logger.Warn("resolve hosting gateway addresses at start", "error", err)
		}
		cancel()
	}
	reconcileCtx, cancel := context.WithTimeout(ctx, startupReconcileBudget)
	if cfg.Hosting.Configured() {
		if err := hostingService.ReconcileAll(reconcileCtx); err != nil {
			logger.Warn("reconcile hosting records at start", "error", err)
		}
	}
	if cfg.Mail.Configured() {
		if err := mailService.ReconcileAll(reconcileCtx); err != nil {
			logger.Warn("reconcile mail records at start", "error", err)
		}
	}
	cancel()

	built.status.RecordStarted(ctx)
	logEngine(logger, "hosting", cfg.Hosting.Configured(), cfg.Hosting.EndpointHost())
	logEngine(logger, "mail", cfg.Mail.Configured(), cfg.Mail.EndpointHost())
	logEngine(logger, "billing", cfg.Billing.Configured(), cfg.Billing.EndpointHost())
	return built
}

// logEngine writes one start-up line per engine. It records the host only —
// never the URL's path, userinfo or any token.
func logEngine(logger *slog.Logger, engine string, configured bool, endpointHost string) {
	logger.Info("engine configuration", "engine", engine, "configured", configured, "endpoint_host", endpointHost)
}

// handlers assembles what the mux needs. The plan route is always mounted on a
// writer role; the backup route only exists when a snapshot token is set,
// because it hands out the whole store.
func (e *engines) handlers(cfg config.Config) *platformHandlers {
	result := &platformHandlers{
		platform:       e.status,
		hosting:        e.hosting,
		mail:           e.mail,
		billing:        e.billing,
		activity:       e.log,
		webhook:        e.webhook,
		mailWebhook:    e.mailWebhook,
		uploads:        e.uploads,
		plan:           e.status.PlanHandler(),
		uploadMaxBytes: cfg.Hosting.UploadMaxBytes,
		requireBearer:  config.RequiresOperatorToken(cfg),
	}
	if cfg.SnapshotBearerToken != "" {
		result.backup = e.status.BackupHandler()
	}
	if result.uploadMaxBytes <= 0 {
		result.uploadMaxBytes = config.DefaultUploadMaxBytes
	}
	return result
}

// namedWorker is one background loop.
type namedWorker struct {
	name string
	run  func(context.Context) error
}

// workers lists every background loop a writer role runs. The engine prober
// runs whatever the config says; the facades' own workers are no-ops when their
// engine is not configured, so the composition does not branch.
func (e *engines) workers() []namedWorker {
	return []namedWorker{
		{name: "engine-prober", run: e.status.Prober().Run},
		{name: "platform-janitor", run: e.janitor.Run},
		{name: "hosting", run: e.hosting.Run},
		{name: "mail", run: e.mail.Run},
		{name: "billing", run: e.billing.Run},
	}
}

// runWithWorkers runs the listeners next to every background worker and stops
// the workers before the caller's deferred store closes run. A worker that
// fails is logged and dropped rather than taking the DNS service down with it:
// losing the janitor must not lose the authority feed.
func runWithWorkers(
	ctx context.Context,
	logger *slog.Logger,
	built *engines,
	serve func(context.Context) error,
) error {
	group, groupCtx := errgroup.WithContext(ctx)
	for _, worker := range built.workers() {
		if worker.run == nil {
			continue
		}
		group.Go(func() error {
			if err := worker.run(groupCtx); err != nil && !errors.Is(err, context.Canceled) {
				logger.Error("background worker stopped", "worker", worker.name, "error", err)
			}
			return nil
		})
	}
	group.Go(func() error { return serve(groupCtx) })
	if err := group.Wait(); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}
