package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	activityv1 "github.com/castlemilk/dns/gen/go/activity/v1"
	"github.com/castlemilk/dns/gen/go/activity/v1/activityv1connect"
	billingv1 "github.com/castlemilk/dns/gen/go/billing/v1"
	"github.com/castlemilk/dns/gen/go/billing/v1/billingv1connect"
	dnsv1 "github.com/castlemilk/dns/gen/go/dns/v1"
	"github.com/castlemilk/dns/gen/go/dns/v1/dnsv1connect"
	hostingv1 "github.com/castlemilk/dns/gen/go/hosting/v1"
	"github.com/castlemilk/dns/gen/go/hosting/v1/hostingv1connect"
	mailv1 "github.com/castlemilk/dns/gen/go/mail/v1"
	"github.com/castlemilk/dns/gen/go/mail/v1/mailv1connect"
	platformv1 "github.com/castlemilk/dns/gen/go/platform/v1"
	"github.com/castlemilk/dns/gen/go/platform/v1/platformv1connect"
)

// The fake control plane is an in-memory implementation of the six services,
// served over loopback by httptest. It is a fake rather than a mock: commands
// mutate it and later commands see the result, so a test asserts on what the
// platform ended up like instead of on which methods were called.
//
// No test in this package reaches anything but this server.

const testToken = "operator-token-for-tests"

type plane struct {
	mutex sync.Mutex

	// fail maps a Connect procedure ("/dns.v1.DNSService/ListZones") to the
	// error to answer it with, which is how the error-mapping table drives
	// every Connect code through the CLI.
	fail map[string]error

	// requireToken, when set, makes every call without it Unauthenticated.
	requireToken string

	// seen records the Authorization header of the last call, so a test can
	// prove the credential travelled on the header and nowhere else.
	seenAuthorization string
	seenProcedures    []string

	zones     []*dnsv1.Zone
	nextID    int
	sites     map[string]*hostingv1.Site
	envVars   map[string][]*hostingv1.EnvVar
	deploys   []*hostingv1.Deploy
	mailbound map[string]*mailv1.MailDomain
	mailboxes map[string][]*mailv1.Mailbox
	forwarder map[string][]*mailv1.Forwarder
	events    []*activityv1.Event

	engines []*platformv1.EngineStatus
}

func newPlane() *plane {
	stamp := timestamppb.New(time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC))
	return &plane{
		fail: map[string]error{},
		// Ids start well clear of the seeded ones, so a newly created record
		// can never collide with rec-1 and hide a bug behind a lucky match.
		nextID: 100,
		zones: []*dnsv1.Zone{{
			Id:          "zone-1",
			Name:        "example.com",
			Serial:      7,
			CreatedAt:   stamp,
			UpdatedAt:   stamp,
			Nameservers: []string{"ns1.deephost.test.", "ns2.deephost.test."},
			Records: []*dnsv1.Record{
				{Id: "rec-1", Name: "@", Type: dnsv1.RecordType_RECORD_TYPE_A, Ttl: 300, Value: "203.0.113.10",
					Source: dnsv1.RecordSource_RECORD_SOURCE_USER},
				{Id: "rec-2", Name: "www", Type: dnsv1.RecordType_RECORD_TYPE_CNAME, Ttl: 300, Value: "example.com.",
					Managed: true, Source: dnsv1.RecordSource_RECORD_SOURCE_HOSTING},
			},
		}},
		sites:     map[string]*hostingv1.Site{},
		envVars:   map[string][]*hostingv1.EnvVar{},
		mailbound: map[string]*mailv1.MailDomain{},
		mailboxes: map[string][]*mailv1.Mailbox{},
		forwarder: map[string][]*mailv1.Forwarder{},
		events: []*activityv1.Event{{
			Id: "0000019a2f-0001", Time: stamp, ZoneId: "zone-1", ZoneName: "example.com",
			Actor: "operator", Kind: "zone.created", Severity: activityv1.Severity_SEVERITY_INFO,
			Summary: "example.com was created", Details: map[string]string{"zone": "example.com"},
		}},
		engines: []*platformv1.EngineStatus{
			{Kind: platformv1.EngineKind_ENGINE_KIND_DNS, Configured: true, Reachable: true,
				Provider: "simpledns", EndpointHost: "127.0.0.1:8080", CheckedAt: stamp},
			{Kind: platformv1.EngineKind_ENGINE_KIND_HOSTING, Configured: false, Reachable: false,
				MissingEnv: []string{"HOSTING_API_URL", "HOSTING_API_TOKEN"}},
			{Kind: platformv1.EngineKind_ENGINE_KIND_MAIL, Configured: false, Reachable: false,
				MissingEnv: []string{"MAIL_API_URL"}},
			{Kind: platformv1.EngineKind_ENGINE_KIND_BILLING, Configured: false, Reachable: false,
				MissingEnv: []string{"STRIPE_SECRET_KEY"}},
		},
	}
}

// start serves the plane on loopback for the lifetime of the test.
func (p *plane) start(t *testing.T) *httptest.Server {
	t.Helper()
	interceptors := connect.WithInterceptors(connect.UnaryInterceptorFunc(p.intercept))
	mux := http.NewServeMux()
	mux.Handle(dnsv1connect.NewDNSServiceHandler(p, interceptors))
	mux.Handle(platformv1connect.NewPlatformServiceHandler(p, interceptors))
	mux.Handle(hostingv1connect.NewHostingServiceHandler(p, interceptors))
	mux.Handle(mailv1connect.NewMailServiceHandler(p, interceptors))
	mux.Handle(billingv1connect.NewBillingServiceHandler(p, interceptors))
	mux.Handle(activityv1connect.NewActivityServiceHandler(p, interceptors))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func (p *plane) intercept(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, request connect.AnyRequest) (connect.AnyResponse, error) {
		procedure := request.Spec().Procedure
		p.mutex.Lock()
		p.seenAuthorization = request.Header().Get("Authorization")
		p.seenProcedures = append(p.seenProcedures, procedure)
		wanted := p.requireToken
		forced, forcedOK := p.fail[procedure]
		p.mutex.Unlock()

		if wanted != "" && request.Header().Get("Authorization") != "Bearer "+wanted {
			return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("bearer token required"))
		}
		if forcedOK {
			return nil, forced
		}
		return next(ctx, request)
	}
}

func (p *plane) failWith(procedure string, err error) {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	p.fail[procedure] = err
}

func (p *plane) lock() func() {
	p.mutex.Lock()
	return p.mutex.Unlock
}

func (p *plane) id(prefix string) string {
	id := fmt.Sprintf("%s-%d", prefix, p.nextID)
	p.nextID++
	return id
}

func (p *plane) findZone(zoneID string) *dnsv1.Zone {
	for _, zone := range p.zones {
		if zone.GetId() == zoneID {
			return zone
		}
	}
	return nil
}

func notFound(what string) error {
	return connect.NewError(connect.CodeNotFound, errors.New(what+": not found"))
}

// ---------------------------------------------------------------- dns.v1

var _ dnsv1connect.DNSServiceHandler = (*plane)(nil)

func (p *plane) ListZones(
	_ context.Context, _ *connect.Request[dnsv1.ListZonesRequest],
) (*connect.Response[dnsv1.ListZonesResponse], error) {
	defer p.lock()()
	records := 0
	for _, zone := range p.zones {
		records += len(zone.GetRecords())
	}
	return connect.NewResponse(&dnsv1.ListZonesResponse{
		Zones: p.zones,
		Status: &dnsv1.ServerStatus{
			ZoneCount:   uint32(len(p.zones)), //nolint:gosec // test data
			RecordCount: uint32(records),      //nolint:gosec // test data
		},
	}), nil
}

func (p *plane) CreateZone(
	_ context.Context, request *connect.Request[dnsv1.CreateZoneRequest],
) (*connect.Response[dnsv1.CreateZoneResponse], error) {
	defer p.lock()()
	name := strings.TrimSpace(request.Msg.GetName())
	if name == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("name: is required"))
	}
	for _, zone := range p.zones {
		if strings.EqualFold(zone.GetName(), name) {
			return nil, connect.NewError(connect.CodeAlreadyExists, errors.New("name: already exists"))
		}
	}
	zone := &dnsv1.Zone{Id: p.id("zone"), Name: name, Serial: 1, Nameservers: []string{"ns1.deephost.test."}}
	p.zones = append(p.zones, zone)
	return connect.NewResponse(&dnsv1.CreateZoneResponse{Zone: zone}), nil
}

func (p *plane) DeleteZone(
	_ context.Context, request *connect.Request[dnsv1.DeleteZoneRequest],
) (*connect.Response[dnsv1.DeleteZoneResponse], error) {
	defer p.lock()()
	for index, zone := range p.zones {
		if zone.GetId() == request.Msg.GetZoneId() {
			p.zones = append(p.zones[:index], p.zones[index+1:]...)
			return connect.NewResponse(&dnsv1.DeleteZoneResponse{}), nil
		}
	}
	return nil, notFound("zone")
}

func (p *plane) CreateRecord(
	_ context.Context, request *connect.Request[dnsv1.CreateRecordRequest],
) (*connect.Response[dnsv1.CreateRecordResponse], error) {
	defer p.lock()()
	zone := p.findZone(request.Msg.GetZoneId())
	if zone == nil {
		return nil, notFound("zone")
	}
	zone.Records = append(zone.Records, &dnsv1.Record{
		Id:     p.id("rec"),
		Name:   request.Msg.GetName(),
		Type:   request.Msg.GetType(),
		Ttl:    request.Msg.GetTtl(),
		Value:  request.Msg.GetValue(),
		Source: dnsv1.RecordSource_RECORD_SOURCE_USER,
	})
	zone.Serial++
	return connect.NewResponse(&dnsv1.CreateRecordResponse{Zone: zone}), nil
}

func (p *plane) UpdateRecord(
	_ context.Context, request *connect.Request[dnsv1.UpdateRecordRequest],
) (*connect.Response[dnsv1.UpdateRecordResponse], error) {
	defer p.lock()()
	zone := p.findZone(request.Msg.GetZoneId())
	if zone == nil {
		return nil, notFound("zone")
	}
	for _, record := range zone.GetRecords() {
		if record.GetId() == request.Msg.GetRecordId() {
			record.Name = request.Msg.GetName()
			record.Type = request.Msg.GetType()
			record.Ttl = request.Msg.GetTtl()
			record.Value = request.Msg.GetValue()
			zone.Serial++
			return connect.NewResponse(&dnsv1.UpdateRecordResponse{Zone: zone}), nil
		}
	}
	return nil, notFound("record")
}

func (p *plane) DeleteRecord(
	_ context.Context, request *connect.Request[dnsv1.DeleteRecordRequest],
) (*connect.Response[dnsv1.DeleteRecordResponse], error) {
	defer p.lock()()
	zone := p.findZone(request.Msg.GetZoneId())
	if zone == nil {
		return nil, notFound("zone")
	}
	for index, record := range zone.GetRecords() {
		if record.GetId() == request.Msg.GetRecordId() {
			zone.Records = append(zone.Records[:index], zone.Records[index+1:]...)
			zone.Serial++
			return connect.NewResponse(&dnsv1.DeleteRecordResponse{Zone: zone}), nil
		}
	}
	return nil, notFound("record")
}

func (p *plane) ImportZone(
	_ context.Context, request *connect.Request[dnsv1.ImportZoneRequest],
) (*connect.Response[dnsv1.ImportZoneResponse], error) {
	defer p.lock()()
	zone := &dnsv1.Zone{Id: "zone-imported", Name: request.Msg.GetName(), Serial: 1}
	if !request.Msg.GetDryRun() {
		p.zones = append(p.zones, zone)
	}
	return connect.NewResponse(&dnsv1.ImportZoneResponse{
		Zone:     zone,
		Warnings: []string{"line 4: unsupported record type ignored"},
		DryRun:   request.Msg.GetDryRun(),
	}), nil
}

func (p *plane) ExportZone(
	_ context.Context, request *connect.Request[dnsv1.ExportZoneRequest],
) (*connect.Response[dnsv1.ExportZoneResponse], error) {
	defer p.lock()()
	zone := p.findZone(request.Msg.GetZoneId())
	if zone == nil {
		return nil, notFound("zone")
	}
	return connect.NewResponse(&dnsv1.ExportZoneResponse{
		Name:     zone.GetName(),
		ZoneFile: "$ORIGIN " + zone.GetName() + ".\n@ 300 IN A 203.0.113.10\n",
	}), nil
}

// ----------------------------------------------------------- platform.v1

var _ platformv1connect.PlatformServiceHandler = (*plane)(nil)

func (p *plane) GetPlatformStatus(
	_ context.Context, request *connect.Request[platformv1.GetPlatformStatusRequest],
) (*connect.Response[platformv1.GetPlatformStatusResponse], error) {
	defer p.lock()()
	return connect.NewResponse(&platformv1.GetPlatformStatusResponse{
		Engines:              p.engines,
		Nameservers:          []string{"ns1.deephost.test.", "ns2.deephost.test."},
		Version:              "v1.2.3",
		Production:           false,
		BillingInformational: true,
		PlatformStoreName:    "platform.db",
		MailboxesPerDomain:   10,
	}), nil
}

func (p *plane) RebuildPlatformStore(
	_ context.Context, request *connect.Request[platformv1.RebuildPlatformStoreRequest],
) (*connect.Response[platformv1.RebuildPlatformStoreResponse], error) {
	defer p.lock()()
	return connect.NewResponse(&platformv1.RebuildPlatformStoreResponse{
		Sites: 1, MailDomains: 0, Subscriptions: 0,
		Warnings: []string{"app deephost-acme has no matching zone"},
		DryRun:   request.Msg.GetDryRun(),
	}), nil
}

// ------------------------------------------------------------ hosting.v1

var _ hostingv1connect.HostingServiceHandler = (*plane)(nil)

func (p *plane) GetHostingStatus(
	_ context.Context, _ *connect.Request[hostingv1.GetHostingStatusRequest],
) (*connect.Response[hostingv1.GetHostingStatusResponse], error) {
	defer p.lock()()
	return connect.NewResponse(&hostingv1.GetHostingStatusResponse{
		Engine:                  p.engines[1],
		GatewayHostname:         "gateway.deephost.test",
		GatewayAddresses:        []string{"203.0.113.10"},
		GatewaySource:           "static",
		GatewayPendingAddresses: []string{"203.0.113.20", "203.0.113.21"},
		GatewayResolver:         "system",
		Sites:                   uint32(len(p.sites)), //nolint:gosec // test data
		BuildDeadlineSeconds:    900,
		UploadsEnabled:          true,
		AppsSuffix:              "apps.deephost.test",
		GatewayPendingSince:     timestamppb.New(time.Date(2026, 9, 4, 9, 0, 0, 0, time.UTC)),
		ActiveDeploys:           0,
		UploadMaxBytes:          1 << 20,
		GatewayResolvedAt:       nil,
		GatewayError:            "",
	}), nil
}

func (p *plane) ListSites(
	_ context.Context, _ *connect.Request[hostingv1.ListSitesRequest],
) (*connect.Response[hostingv1.ListSitesResponse], error) {
	defer p.lock()()
	sites := make([]*hostingv1.Site, 0, len(p.sites))
	for _, site := range p.sites {
		sites = append(sites, site)
	}
	return connect.NewResponse(&hostingv1.ListSitesResponse{Sites: sites}), nil
}

func (p *plane) GetSite(
	_ context.Context, request *connect.Request[hostingv1.GetSiteRequest],
) (*connect.Response[hostingv1.GetSiteResponse], error) {
	defer p.lock()()
	site, ok := p.sites[request.Msg.GetZoneId()]
	if !ok {
		return nil, notFound("site")
	}
	return connect.NewResponse(&hostingv1.GetSiteResponse{Site: site}), nil
}

func (p *plane) AttachSite(
	_ context.Context, request *connect.Request[hostingv1.AttachSiteRequest],
) (*connect.Response[hostingv1.AttachSiteResponse], error) {
	defer p.lock()()
	zone := p.findZone(request.Msg.GetZoneId())
	if zone == nil {
		return nil, notFound("zone")
	}
	plan := []*platformv1.RecordChange{
		{Op: "create", Name: "@", Type: "A", Value: "203.0.113.10", Ttl: 300, Why: "the site's apex address"},
	}
	if request.Msg.GetDryRun() {
		return connect.NewResponse(&hostingv1.AttachSiteResponse{DnsPlan: plan, DryRun: true}), nil
	}
	site := &hostingv1.Site{
		ZoneId:    zone.GetId(),
		ZoneName:  zone.GetName(),
		App:       "acme-1a2b3c",
		Framework: request.Msg.GetFramework(),
		State:     hostingv1.SiteState_SITE_STATE_ATTACHING,
		WwwMode:   request.Msg.GetWwwMode(),
		Dns:       &hostingv1.SiteDns{InSync: true},
	}
	p.sites[zone.GetId()] = site
	return connect.NewResponse(&hostingv1.AttachSiteResponse{Site: site, DnsPlan: plan}), nil
}

func (p *plane) UpdateSite(
	_ context.Context, request *connect.Request[hostingv1.UpdateSiteRequest],
) (*connect.Response[hostingv1.UpdateSiteResponse], error) {
	defer p.lock()()
	site, ok := p.sites[request.Msg.GetZoneId()]
	if !ok {
		return nil, notFound("site")
	}
	if request.Msg.GetWwwMode() != hostingv1.WwwMode_WWW_MODE_UNSPECIFIED {
		site.WwwMode = request.Msg.GetWwwMode()
	}
	if request.Msg.Repository != nil {
		site.Repository = request.Msg.GetRepository()
	}
	return connect.NewResponse(&hostingv1.UpdateSiteResponse{Site: site}), nil
}

func (p *plane) DetachSite(
	_ context.Context, request *connect.Request[hostingv1.DetachSiteRequest],
) (*connect.Response[hostingv1.DetachSiteResponse], error) {
	defer p.lock()()
	if _, ok := p.sites[request.Msg.GetZoneId()]; !ok {
		return nil, notFound("site")
	}
	delete(p.sites, request.Msg.GetZoneId())
	return connect.NewResponse(&hostingv1.DetachSiteResponse{Note: "the app was deleted"}), nil
}

func (p *plane) ReapplySiteDns(
	_ context.Context, request *connect.Request[hostingv1.ReapplySiteDnsRequest],
) (*connect.Response[hostingv1.ReapplySiteDnsResponse], error) {
	defer p.lock()()
	site, ok := p.sites[request.Msg.GetZoneId()]
	if !ok {
		return nil, notFound("site")
	}
	return connect.NewResponse(&hostingv1.ReapplySiteDnsResponse{
		Site: site, DryRun: request.Msg.GetDryRun(),
		DnsPlan: []*platformv1.RecordChange{{Op: "keep", Name: "@", Type: "A", Value: "203.0.113.10"}},
	}), nil
}

func (p *plane) CreateDeploy(
	_ context.Context, request *connect.Request[hostingv1.CreateDeployRequest],
) (*connect.Response[hostingv1.CreateDeployResponse], error) {
	defer p.lock()()
	site, ok := p.sites[request.Msg.GetZoneId()]
	if !ok {
		return nil, notFound("site")
	}
	deploy := &hostingv1.Deploy{
		Id:       p.id("deploy"),
		ZoneId:   site.GetZoneId(),
		ZoneName: site.GetZoneName(),
		Kind:     hostingv1.DeployKind_DEPLOY_KIND_GIT,
		Phase:    hostingv1.DeployPhase_DEPLOY_PHASE_QUEUED,
		Source: &hostingv1.DeploySource{
			Repository:        request.Msg.GetRepository(),
			Revision:          request.Msg.GetRevision(),
			PrivateRepository: request.Msg.GetGitToken() != "",
		},
	}
	p.deploys = append(p.deploys, deploy)
	site.LatestDeployId = deploy.GetId()
	return connect.NewResponse(&hostingv1.CreateDeployResponse{Deploy: deploy}), nil
}

func (p *plane) ListDeploys(
	_ context.Context, _ *connect.Request[hostingv1.ListDeploysRequest],
) (*connect.Response[hostingv1.ListDeploysResponse], error) {
	defer p.lock()()
	return connect.NewResponse(&hostingv1.ListDeploysResponse{Deploys: p.deploys}), nil
}

func (p *plane) GetDeploy(
	_ context.Context, request *connect.Request[hostingv1.GetDeployRequest],
) (*connect.Response[hostingv1.GetDeployResponse], error) {
	defer p.lock()()
	for _, deploy := range p.deploys {
		if deploy.GetId() == request.Msg.GetDeployId() {
			return connect.NewResponse(&hostingv1.GetDeployResponse{Deploy: deploy}), nil
		}
	}
	return nil, notFound("deploy")
}

func (p *plane) GetDeployLog(
	_ context.Context, request *connect.Request[hostingv1.GetDeployLogRequest],
) (*connect.Response[hostingv1.GetDeployLogResponse], error) {
	defer p.lock()()
	return connect.NewResponse(&hostingv1.GetDeployLogResponse{
		Lines:    []string{"npm install", "build succeeded"},
		Complete: true,
		Source:   hostingv1.LogSource_LOG_SOURCE_SNAPSHOT,
		Note:     "this log was stored when the build finished",
	}), nil
}

func (p *plane) RollbackSite(
	_ context.Context, request *connect.Request[hostingv1.RollbackSiteRequest],
) (*connect.Response[hostingv1.RollbackSiteResponse], error) {
	defer p.lock()()
	if _, ok := p.sites[request.Msg.GetZoneId()]; !ok {
		return nil, notFound("site")
	}
	return connect.NewResponse(&hostingv1.RollbackSiteResponse{Deploy: &hostingv1.Deploy{
		Id:               p.id("deploy"),
		Kind:             hostingv1.DeployKind_DEPLOY_KIND_ROLLBACK,
		Phase:            hostingv1.DeployPhase_DEPLOY_PHASE_RELEASING,
		RolledBackFrom:   request.Msg.GetDeployId(),
		ZoneId:           request.Msg.GetZoneId(),
		Source:           &hostingv1.DeploySource{},
		RevisionResolved: "",
	}}), nil
}

func (p *plane) ConfirmGatewayAddresses(
	_ context.Context, request *connect.Request[hostingv1.ConfirmGatewayAddressesRequest],
) (*connect.Response[hostingv1.ConfirmGatewayAddressesResponse], error) {
	defer p.lock()()
	return connect.NewResponse(&hostingv1.ConfirmGatewayAddressesResponse{
		Addresses: request.Msg.GetAddresses(), SitesQueued: uint32(len(p.sites)), //nolint:gosec // test data
	}), nil
}

func (p *plane) SetSiteEnvVar(
	_ context.Context, request *connect.Request[hostingv1.SetSiteEnvVarRequest],
) (*connect.Response[hostingv1.SetSiteEnvVarResponse], error) {
	defer p.lock()()
	if _, ok := p.sites[request.Msg.GetZoneId()]; !ok {
		return nil, notFound("site")
	}
	if request.Msg.GetValue() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("value: is required"))
	}
	environment := request.Msg.GetEnvironment()
	if environment == "" {
		environment = "production"
	}
	variable := &hostingv1.EnvVar{Name: request.Msg.GetName(), Environment: environment}
	key := request.Msg.GetZoneId()
	p.envVars[key] = append(p.envVars[key], variable)
	return connect.NewResponse(&hostingv1.SetSiteEnvVarResponse{
		Variable: variable,
		Note:     "the running site keeps the values it started with until the next deploy",
	}), nil
}

func (p *plane) DeleteSiteEnvVar(
	_ context.Context, request *connect.Request[hostingv1.DeleteSiteEnvVarRequest],
) (*connect.Response[hostingv1.DeleteSiteEnvVarResponse], error) {
	defer p.lock()()
	key := request.Msg.GetZoneId()
	kept := make([]*hostingv1.EnvVar, 0, len(p.envVars[key]))
	for _, variable := range p.envVars[key] {
		if variable.GetName() != request.Msg.GetName() {
			kept = append(kept, variable)
		}
	}
	p.envVars[key] = kept
	return connect.NewResponse(&hostingv1.DeleteSiteEnvVarResponse{Note: "it is gone at the next deploy"}), nil
}

func (p *plane) ListSiteEnvVars(
	_ context.Context, request *connect.Request[hostingv1.ListSiteEnvVarsRequest],
) (*connect.Response[hostingv1.ListSiteEnvVarsResponse], error) {
	defer p.lock()()
	return connect.NewResponse(&hostingv1.ListSiteEnvVarsResponse{
		Variables: p.envVars[request.Msg.GetZoneId()], Supported: true,
	}), nil
}

// --------------------------------------------------------------- mail.v1

var _ mailv1connect.MailServiceHandler = (*plane)(nil)

func (p *plane) GetMailStatus(
	_ context.Context, _ *connect.Request[mailv1.GetMailStatusRequest],
) (*connect.Response[mailv1.GetMailStatusResponse], error) {
	defer p.lock()()
	return connect.NewResponse(&mailv1.GetMailStatusResponse{
		Engine:             p.engines[2],
		MailHostname:       "mail.deephost.test",
		Edition:            "community",
		MailboxesPerDomain: 10,
		Domains:            uint32(len(p.mailbound)), //nolint:gosec // test data
		CountsAvailable:    false,
		DeliveryStatsNote:  "no delivery-event receiver is configured, so nothing is counted",
	}), nil
}

func (p *plane) ListMailDomains(
	_ context.Context, _ *connect.Request[mailv1.ListMailDomainsRequest],
) (*connect.Response[mailv1.ListMailDomainsResponse], error) {
	defer p.lock()()
	domains := make([]*mailv1.MailDomain, 0, len(p.mailbound))
	for _, domain := range p.mailbound {
		domains = append(domains, domain)
	}
	return connect.NewResponse(&mailv1.ListMailDomainsResponse{Domains: domains}), nil
}

func (p *plane) GetMailDomain(
	_ context.Context, request *connect.Request[mailv1.GetMailDomainRequest],
) (*connect.Response[mailv1.GetMailDomainResponse], error) {
	defer p.lock()()
	domain, ok := p.mailbound[request.Msg.GetZoneId()]
	if !ok {
		return nil, notFound("mail domain")
	}
	return connect.NewResponse(&mailv1.GetMailDomainResponse{
		Domain: domain,
		Queue:  &mailv1.QueueSummary{Available: true, Scheduled: 1},
	}), nil
}

func (p *plane) BindMailDomain(
	_ context.Context, request *connect.Request[mailv1.BindMailDomainRequest],
) (*connect.Response[mailv1.BindMailDomainResponse], error) {
	defer p.lock()()
	zone := p.findZone(request.Msg.GetZoneId())
	if zone == nil {
		return nil, notFound("zone")
	}
	policy := request.Msg.GetDmarcPolicy()
	if policy == "" {
		policy = "none"
	}
	domain := &mailv1.MailDomain{
		ZoneId: zone.GetId(), ZoneName: zone.GetName(),
		State:                   mailv1.MailDomainState_MAIL_DOMAIN_STATE_BOUND,
		DmarcPolicy:             policy,
		RecordsInSync:           true,
		CountsAvailable:         true,
		MailboxLimit:            10,
		PublishClientAutoconfig: request.Msg.PublishClientAutoconfig == nil || request.Msg.GetPublishClientAutoconfig(),
	}
	if !request.Msg.GetDryRun() {
		p.mailbound[zone.GetId()] = domain
	}
	return connect.NewResponse(&mailv1.BindMailDomainResponse{
		Domain: domain, DryRun: request.Msg.GetDryRun(),
		DnsPlan: []*platformv1.RecordChange{{Op: "create", Name: "@", Type: "MX", Value: "10 mail.deephost.test."}},
	}), nil
}

func (p *plane) UpdateMailDomain(
	_ context.Context, request *connect.Request[mailv1.UpdateMailDomainRequest],
) (*connect.Response[mailv1.UpdateMailDomainResponse], error) {
	defer p.lock()()
	domain, ok := p.mailbound[request.Msg.GetZoneId()]
	if !ok {
		return nil, notFound("mail domain")
	}
	if request.Msg.DmarcPolicy != nil {
		domain.DmarcPolicy = request.Msg.GetDmarcPolicy()
	}
	if request.Msg.PublishClientAutoconfig != nil {
		domain.PublishClientAutoconfig = request.Msg.GetPublishClientAutoconfig()
	}
	return connect.NewResponse(&mailv1.UpdateMailDomainResponse{Domain: domain}), nil
}

func (p *plane) UnbindMailDomain(
	_ context.Context, request *connect.Request[mailv1.UnbindMailDomainRequest],
) (*connect.Response[mailv1.UnbindMailDomainResponse], error) {
	defer p.lock()()
	domain, ok := p.mailbound[request.Msg.GetZoneId()]
	if !ok {
		return nil, notFound("mail domain")
	}
	mailboxes := p.mailboxes[request.Msg.GetZoneId()]
	if len(mailboxes) > 0 && !request.Msg.GetDeleteMailboxes() {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			errors.New("delete_mailboxes: is required while mailboxes exist"))
	}
	if request.Msg.GetDeleteMailboxes() && request.Msg.GetConfirmZoneName() != domain.GetZoneName() {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("confirm_zone_name: must equal the zone name"))
	}
	deleted := len(mailboxes)
	delete(p.mailbound, request.Msg.GetZoneId())
	delete(p.mailboxes, request.Msg.GetZoneId())
	return connect.NewResponse(&mailv1.UnbindMailDomainResponse{
		MailboxesDeleted: uint32(deleted), //nolint:gosec // test data
	}), nil
}

func (p *plane) ReapplyMailDns(
	_ context.Context, request *connect.Request[mailv1.ReapplyMailDnsRequest],
) (*connect.Response[mailv1.ReapplyMailDnsResponse], error) {
	defer p.lock()()
	domain, ok := p.mailbound[request.Msg.GetZoneId()]
	if !ok {
		return nil, notFound("mail domain")
	}
	return connect.NewResponse(&mailv1.ReapplyMailDnsResponse{
		Domain: domain, DryRun: request.Msg.GetDryRun(),
	}), nil
}

func (p *plane) ListMailboxes(
	_ context.Context, request *connect.Request[mailv1.ListMailboxesRequest],
) (*connect.Response[mailv1.ListMailboxesResponse], error) {
	defer p.lock()()
	return connect.NewResponse(&mailv1.ListMailboxesResponse{
		Mailboxes: p.mailboxes[request.Msg.GetZoneId()], Limit: 10,
	}), nil
}

func (p *plane) CreateMailbox(
	_ context.Context, request *connect.Request[mailv1.CreateMailboxRequest],
) (*connect.Response[mailv1.CreateMailboxResponse], error) {
	defer p.lock()()
	domain, ok := p.mailbound[request.Msg.GetZoneId()]
	if !ok {
		return nil, notFound("mail domain")
	}
	mailbox := &mailv1.Mailbox{
		Id: p.id("mbx"), ZoneId: domain.GetZoneId(),
		Address:     request.Msg.GetLocalPart() + "@" + domain.GetZoneName(),
		LocalPart:   request.Msg.GetLocalPart(),
		DisplayName: request.Msg.GetDisplayName(),
		QuotaBytes:  request.Msg.GetQuotaBytes(),
	}
	p.mailboxes[domain.GetZoneId()] = append(p.mailboxes[domain.GetZoneId()], mailbox)
	return connect.NewResponse(&mailv1.CreateMailboxResponse{
		Mailbox:  mailbox,
		Password: "correct-horse-battery-staple",
		ImapHost: "mail.deephost.test", ImapPort: 993,
		SmtpHost: "mail.deephost.test", SmtpPort: 465,
	}), nil
}

func (p *plane) UpdateMailbox(
	_ context.Context, request *connect.Request[mailv1.UpdateMailboxRequest],
) (*connect.Response[mailv1.UpdateMailboxResponse], error) {
	defer p.lock()()
	for _, mailbox := range p.mailboxes[request.Msg.GetZoneId()] {
		if mailbox.GetId() == request.Msg.GetMailboxId() {
			if request.Msg.DisplayName != nil {
				mailbox.DisplayName = request.Msg.GetDisplayName()
			}
			if request.Msg.QuotaBytes != nil {
				mailbox.QuotaBytes = request.Msg.GetQuotaBytes()
			}
			return connect.NewResponse(&mailv1.UpdateMailboxResponse{Mailbox: mailbox}), nil
		}
	}
	return nil, notFound("mailbox")
}

func (p *plane) ResetMailboxPassword(
	_ context.Context, request *connect.Request[mailv1.ResetMailboxPasswordRequest],
) (*connect.Response[mailv1.ResetMailboxPasswordResponse], error) {
	defer p.lock()()
	for _, mailbox := range p.mailboxes[request.Msg.GetZoneId()] {
		if mailbox.GetId() == request.Msg.GetMailboxId() {
			return connect.NewResponse(&mailv1.ResetMailboxPasswordResponse{Password: "fresh-password-1"}), nil
		}
	}
	return nil, notFound("mailbox")
}

func (p *plane) DeleteMailbox(
	_ context.Context, request *connect.Request[mailv1.DeleteMailboxRequest],
) (*connect.Response[mailv1.DeleteMailboxResponse], error) {
	defer p.lock()()
	key := request.Msg.GetZoneId()
	for index, mailbox := range p.mailboxes[key] {
		if mailbox.GetId() == request.Msg.GetMailboxId() {
			if request.Msg.GetConfirmAddress() != mailbox.GetAddress() {
				return nil, connect.NewError(connect.CodeInvalidArgument,
					errors.New("confirm_address: must equal the mailbox address"))
			}
			p.mailboxes[key] = append(p.mailboxes[key][:index], p.mailboxes[key][index+1:]...)
			return connect.NewResponse(&mailv1.DeleteMailboxResponse{}), nil
		}
	}
	return nil, notFound("mailbox")
}

func (p *plane) ListForwarders(
	_ context.Context, request *connect.Request[mailv1.ListForwardersRequest],
) (*connect.Response[mailv1.ListForwardersResponse], error) {
	defer p.lock()()
	return connect.NewResponse(&mailv1.ListForwardersResponse{
		Forwarders: p.forwarder[request.Msg.GetZoneId()],
	}), nil
}

func (p *plane) CreateForwarder(
	_ context.Context, request *connect.Request[mailv1.CreateForwarderRequest],
) (*connect.Response[mailv1.CreateForwarderResponse], error) {
	defer p.lock()()
	domain, ok := p.mailbound[request.Msg.GetZoneId()]
	if !ok {
		return nil, notFound("mail domain")
	}
	forwarder := &mailv1.Forwarder{
		Id: p.id("fwd"), ZoneId: domain.GetZoneId(),
		Address:   request.Msg.GetLocalPart() + "@" + domain.GetZoneName(),
		LocalPart: request.Msg.GetLocalPart(),
		Kind:      mailv1.ForwarderKind_FORWARDER_KIND_LIST,
		Targets:   request.Msg.GetTargets(),
		External:  true,
	}
	p.forwarder[domain.GetZoneId()] = append(p.forwarder[domain.GetZoneId()], forwarder)
	return connect.NewResponse(&mailv1.CreateForwarderResponse{Forwarder: forwarder}), nil
}

func (p *plane) DeleteForwarder(
	_ context.Context, request *connect.Request[mailv1.DeleteForwarderRequest],
) (*connect.Response[mailv1.DeleteForwarderResponse], error) {
	defer p.lock()()
	key := request.Msg.GetZoneId()
	for index, forwarder := range p.forwarder[key] {
		if forwarder.GetId() == request.Msg.GetForwarderId() {
			p.forwarder[key] = append(p.forwarder[key][:index], p.forwarder[key][index+1:]...)
			return connect.NewResponse(&mailv1.DeleteForwarderResponse{}), nil
		}
	}
	return nil, notFound("forwarder")
}

func (p *plane) GetMailQueue(
	_ context.Context, request *connect.Request[mailv1.GetMailQueueRequest],
) (*connect.Response[mailv1.GetMailQueueResponse], error) {
	defer p.lock()()
	return connect.NewResponse(&mailv1.GetMailQueueResponse{
		Summary: &mailv1.QueueSummary{Available: true, Scheduled: 2},
		Messages: []*mailv1.QueuedMessage{{
			Id: "msg-1", From: "postmaster@example.com", To: []string{"someone@example.net"},
			Status: "Scheduled", Queue: "remote",
		}},
	}), nil
}

// ------------------------------------------------------------ billing.v1

var _ billingv1connect.BillingServiceHandler = (*plane)(nil)

func (p *plane) GetBillingStatus(
	_ context.Context, _ *connect.Request[billingv1.GetBillingStatusRequest],
) (*connect.Response[billingv1.GetBillingStatusResponse], error) {
	defer p.lock()()
	return connect.NewResponse(&billingv1.GetBillingStatusResponse{
		Engine:            p.engines[3],
		Provider:          "fake",
		InformationalOnly: true,
		PolicyNote:        "nothing here changes what the platform serves",
		Price:             &billingv1.Price{Id: "price_1", UnitAmount: 600, Currency: "usd", Label: "$6 / domain / month"},
	}), nil
}

func (p *plane) GetBillingSummary(
	_ context.Context, _ *connect.Request[billingv1.GetBillingSummaryRequest],
) (*connect.Response[billingv1.GetBillingSummaryResponse], error) {
	defer p.lock()()
	rows := make([]*billingv1.DomainBilling, 0, len(p.zones))
	for _, zone := range p.zones {
		rows = append(rows, &billingv1.DomainBilling{
			ZoneId: zone.GetId(), ZoneName: zone.GetName(),
			State: billingv1.SubscriptionState_SUBSCRIPTION_STATE_UNBILLED,
		})
	}
	return connect.NewResponse(&billingv1.GetBillingSummaryResponse{Domains: rows}), nil
}

func (p *plane) CreateCheckoutSession(
	_ context.Context, request *connect.Request[billingv1.CreateCheckoutSessionRequest],
) (*connect.Response[billingv1.CreateCheckoutSessionResponse], error) {
	defer p.lock()()
	return connect.NewResponse(&billingv1.CreateCheckoutSessionResponse{
		Url: "https://checkout.test/session", SessionId: "cs_test_1",
	}), nil
}

func (p *plane) ConfirmCheckout(
	_ context.Context, request *connect.Request[billingv1.ConfirmCheckoutRequest],
) (*connect.Response[billingv1.ConfirmCheckoutResponse], error) {
	defer p.lock()()
	return connect.NewResponse(&billingv1.ConfirmCheckoutResponse{
		Domain: &billingv1.DomainBilling{
			ZoneName: "example.com",
			State:    billingv1.SubscriptionState_SUBSCRIPTION_STATE_ACTIVE,
		},
		Completed: true, Provider: "fake",
	}), nil
}

func (p *plane) CreatePortalSession(
	_ context.Context, _ *connect.Request[billingv1.CreatePortalSessionRequest],
) (*connect.Response[billingv1.CreatePortalSessionResponse], error) {
	defer p.lock()()
	return connect.NewResponse(&billingv1.CreatePortalSessionResponse{Url: "https://portal.test/session"}), nil
}

func (p *plane) ListInvoices(
	_ context.Context, _ *connect.Request[billingv1.ListInvoicesRequest],
) (*connect.Response[billingv1.ListInvoicesResponse], error) {
	defer p.lock()()
	return connect.NewResponse(&billingv1.ListInvoicesResponse{
		Live: true,
		Invoices: []*billingv1.Invoice{{
			Id: "in_1", Number: "INV-001", ZoneName: "example.com",
			Total: 600, Currency: "usd", Status: "paid",
		}},
	}), nil
}

func (p *plane) RetryDeadWebhooks(
	_ context.Context, _ *connect.Request[billingv1.RetryDeadWebhooksRequest],
) (*connect.Response[billingv1.RetryDeadWebhooksResponse], error) {
	defer p.lock()()
	return connect.NewResponse(&billingv1.RetryDeadWebhooksResponse{Requeued: 2}), nil
}

// ----------------------------------------------------------- activity.v1

var _ activityv1connect.ActivityServiceHandler = (*plane)(nil)

func (p *plane) ListEvents(
	_ context.Context, request *connect.Request[activityv1.ListEventsRequest],
) (*connect.Response[activityv1.ListEventsResponse], error) {
	defer p.lock()()
	matched := make([]*activityv1.Event, 0, len(p.events))
	for _, event := range p.events {
		if zoneID := request.Msg.GetZoneId(); zoneID != "" && event.GetZoneId() != zoneID {
			continue
		}
		if kinds := request.Msg.GetKinds(); len(kinds) > 0 && !matchesKind(event.GetKind(), kinds) {
			continue
		}
		if since := request.Msg.GetSince(); since != nil && event.GetTime().AsTime().Before(since.AsTime()) {
			continue
		}
		matched = append(matched, event)
	}
	return connect.NewResponse(&activityv1.ListEventsResponse{
		Events: matched, TotalRetained: uint64(len(p.events)), //nolint:gosec // test data
	}), nil
}

func matchesKind(kind string, wanted []string) bool {
	for _, candidate := range wanted {
		if strings.HasSuffix(candidate, ".") && strings.HasPrefix(kind, candidate) {
			return true
		}
		if kind == candidate {
			return true
		}
	}
	return false
}

func (p *plane) GetEvent(
	_ context.Context, request *connect.Request[activityv1.GetEventRequest],
) (*connect.Response[activityv1.GetEventResponse], error) {
	defer p.lock()()
	for _, event := range p.events {
		if event.GetId() == request.Msg.GetId() {
			return connect.NewResponse(&activityv1.GetEventResponse{Event: event}), nil
		}
	}
	return nil, notFound("event")
}
