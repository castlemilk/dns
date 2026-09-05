package platform

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"runtime/debug"
	"sort"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	platformv1 "github.com/castlemilk/dns/gen/go/platform/v1"
	"github.com/castlemilk/dns/gen/go/platform/v1/platformv1connect"
	"github.com/castlemilk/dns/internal/activity"
	"github.com/castlemilk/dns/internal/config"
	"github.com/castlemilk/dns/internal/secretguard"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// PolicyNote is the one sentence that states the billing policy. It is rendered
// on the Billing page, the connect flow's billing step and the landing page's
// pricing footnote, and it is the only place that wording lives.
const PolicyNote = "Billing is informational in this release: a domain without a subscription keeps working. Subscribe to keep its status current."

// PriceReporter is implemented by the billing facade once it has probed the
// configured price. The plan route and the status service read the formatted
// label from it; there is no build-time price constant anywhere.
type PriceReporter interface {
	PriceLabel() string
}

// StatusService answers platform.v1.PlatformService and owns the two raw routes
// that belong to the platform rather than to one engine.
type StatusService struct {
	cfg        config.Config
	deps       Deps
	prober     *Prober
	rebuilders []Rebuilder
	startedAt  time.Time
	version    string
	price      PriceReporter

	// rebuilding serialises RebuildPlatformStore: EmptyForRebuild is a read and
	// the rebuilders write afterwards, so two concurrent calls would both see an
	// empty store and both reconstruct it.
	rebuilding atomic.Bool
}

// NewStatusService builds the platform service. probes and rebuilders are the
// engine facades; the service never imports them.
func NewStatusService(cfg config.Config, deps Deps, probes []Probe, rebuilders []Rebuilder) *StatusService {
	service := &StatusService{
		cfg:        cfg,
		deps:       deps,
		prober:     NewProber(probes, deps),
		rebuilders: rebuilders,
		startedAt:  deps.Now(),
		version:    buildVersion(),
	}
	for _, probe := range probes {
		if reporter, ok := probe.(PriceReporter); ok {
			service.price = reporter
		}
	}
	return service
}

// Prober exposes the engine prober so the app can run it as a worker and other
// facades can subscribe to reachability transitions.
func (s *StatusService) Prober() *Prober { return s.prober }

// StartedAt is the process start instant reported to the console.
func (s *StatusService) StartedAt() time.Time { return s.startedAt }

// Version is the module version, or "dev" for a local build.
func (s *StatusService) Version() string { return s.version }

func (s *StatusService) GetPlatformStatus(
	ctx context.Context,
	request *connect.Request[platformv1.GetPlatformStatusRequest],
) (*connect.Response[platformv1.GetPlatformStatusResponse], error) {
	if request.Msg.GetProbe() {
		// Rate-limited inside the prober: a client holding the button down
		// cannot turn the console into a load generator against the engines.
		s.prober.ProbeAll(ctx, false)
	}
	statuses := s.prober.Statuses()
	engines := make([]*platformv1.EngineStatus, 0, len(statuses))
	for _, status := range statuses {
		engines = append(engines, status.Proto())
	}
	response := &platformv1.GetPlatformStatusResponse{
		Engines:              engines,
		Nameservers:          append([]string(nil), s.cfg.Nameservers...),
		Version:              s.version,
		StartedAt:            timestamppb.New(s.startedAt.UTC()),
		Production:           s.cfg.Production,
		BillingInformational: true,
		PlatformStoreName:    filepath.Base(s.cfg.PlatformPath),
		PlatformBackupRoute:  s.cfg.SnapshotBearerToken != "",
	}
	if s.cfg.Mail.Configured() {
		response.MailboxesPerDomain = uint32(max(s.cfg.Mail.MailboxesPerDomain, 0))
	}
	return connect.NewResponse(response), nil
}

func (s *StatusService) RebuildPlatformStore(
	ctx context.Context,
	request *connect.Request[platformv1.RebuildPlatformStoreRequest],
) (*connect.Response[platformv1.RebuildPlatformStoreResponse], error) {
	if s.deps.Store == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("the platform store is not open"))
	}
	// One rebuild at a time: the emptiness check and the writes that follow are
	// not one transaction, so two callers could both pass the check.
	if !s.rebuilding.CompareAndSwap(false, true) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("a platform store rebuild is already running"))
	}
	defer s.rebuilding.Store(false)

	dryRun := request.Msg.GetDryRun()
	empty, err := s.deps.Store.EmptyForRebuild(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal server error"))
	}
	if !empty {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("the platform store is not empty"))
	}

	response := &platformv1.RebuildPlatformStoreResponse{DryRun: dryRun}
	for _, rebuilder := range s.rebuilders {
		if rebuilder == nil {
			continue
		}
		report, err := rebuilder.Rebuild(ctx, dryRun)
		if err != nil {
			// One unreachable engine must not lose the other two: report it as
			// a warning and keep going, so a partial rebuild is visible rather
			// than silently absent. The text comes from an engine client, so it
			// is redacted before it reaches the response.
			response.Warnings = append(response.Warnings,
				secretguard.Redact(fmt.Sprintf("%s: %s", EngineName(rebuilder.Kind()), err.Error())))
			continue
		}
		switch rebuilder.Kind() {
		case platformv1.EngineKind_ENGINE_KIND_HOSTING:
			response.Sites += report.Count
		case platformv1.EngineKind_ENGINE_KIND_MAIL:
			response.MailDomains += report.Count
		case platformv1.EngineKind_ENGINE_KIND_BILLING:
			response.Subscriptions += report.Count
		case platformv1.EngineKind_ENGINE_KIND_DNS, platformv1.EngineKind_ENGINE_KIND_UNSPECIFIED:
		}
		for _, warning := range report.Warnings {
			response.Warnings = append(response.Warnings, secretguard.Redact(warning))
		}
	}
	sort.Strings(response.Warnings)

	if !dryRun {
		s.deps.Record(ctx, activity.Event{
			Actor:    activity.ActorSystem,
			Kind:     activity.KindPlatformRebuilt,
			Severity: activity.SeverityWarn,
			Summary: fmt.Sprintf("Rebuilt the platform store from the engines: %d sites, %d mail domains, %d subscriptions",
				response.Sites, response.MailDomains, response.Subscriptions),
			Details: map[string]string{
				"sites":         fmt.Sprint(response.Sites),
				"mail_domains":  fmt.Sprint(response.MailDomains),
				"subscriptions": fmt.Sprint(response.Subscriptions),
			},
		})
	}
	return connect.NewResponse(response), nil
}

// RecordStarted writes the one platform.started event. The app calls it after
// the stores are open and the engines are constructed.
func (s *StatusService) RecordStarted(ctx context.Context) {
	configured := make([]string, 0, 3)
	if s.cfg.Hosting.Configured() {
		configured = append(configured, "hosting")
	}
	if s.cfg.Mail.Configured() {
		configured = append(configured, "mail")
	}
	if s.cfg.Billing.Configured() {
		configured = append(configured, "billing")
	}
	engines := "none"
	if len(configured) > 0 {
		engines = joinComma(configured)
	}
	s.deps.Record(ctx, activity.Event{
		Actor:    activity.ActorSystem,
		Kind:     activity.KindPlatformStarted,
		Severity: activity.SeverityInfo,
		Summary:  "Control plane started (engines: " + engines + ")",
		Details: map[string]string{
			"version": s.version,
			"role":    string(s.cfg.Role),
			"engines": engines,
		},
	})
}

// PriceLabel is the server-formatted price, or "" when billing is not
// configured or the price probe has not succeeded.
func (s *StatusService) PriceLabel() string {
	if s.price == nil {
		return ""
	}
	return s.price.PriceLabel()
}

// dnsProbe reports the control plane's own status. It is always configured and
// always reachable: the process answering the question is the engine.
type dnsProbe struct {
	cfg     config.Config
	version string
	now     func() time.Time
}

// NewDNSProbe returns the status entry for this control plane itself, which is
// always the first engine in the response.
func NewDNSProbe(cfg config.Config, deps Deps) Probe {
	return &dnsProbe{cfg: cfg, version: buildVersion(), now: deps.Now}
}

func (p *dnsProbe) Kind() platformv1.EngineKind { return platformv1.EngineKind_ENGINE_KIND_DNS }

func (p *dnsProbe) Probe(context.Context) error { return nil }

func (p *dnsProbe) Status() EngineStatus {
	return EngineStatus{
		Kind:       platformv1.EngineKind_ENGINE_KIND_DNS,
		Configured: true,
		Reachable:  true,
		CheckedAt:  p.now(),
		Provider:   "simpledns",
		Version:    p.version,
	}
}

func buildVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Version == "" || info.Main.Version == "(devel)" {
		return "dev"
	}
	return info.Main.Version
}

func joinComma(values []string) string {
	result := ""
	for index, value := range values {
		if index > 0 {
			result += ", "
		}
		result += value
	}
	return result
}

var (
	_ platformv1connect.PlatformServiceHandler = (*StatusService)(nil)
	_ http.Handler                             = (*planHandler)(nil)
)
