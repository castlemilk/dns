package deephostmcp

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	dnsv1 "github.com/castlemilk/dns/gen/go/dns/v1"
	"github.com/castlemilk/dns/gen/go/dns/v1/dnsv1connect"
	hostingv1 "github.com/castlemilk/dns/gen/go/hosting/v1"
	"github.com/castlemilk/dns/gen/go/hosting/v1/hostingv1connect"
	mailv1 "github.com/castlemilk/dns/gen/go/mail/v1"
	"github.com/castlemilk/dns/gen/go/mail/v1/mailv1connect"
	platformv1 "github.com/castlemilk/dns/gen/go/platform/v1"
	"github.com/castlemilk/dns/gen/go/platform/v1/platformv1connect"
	"github.com/castlemilk/dns/internal/deephostcli/client"
)

// The fakes below embed the generated client interfaces and override only the
// calls a test makes. A call nobody stubbed is a nil dereference rather than a
// silent zero answer, so a test cannot pass by exercising a method it never
// meant to reach.

type fakeDNS struct {
	dnsv1connect.DNSServiceClient

	zones []*dnsv1.Zone

	listErr error

	deletedZones   []string
	deletedRecords []*dnsv1.DeleteRecordRequest
	created        []*dnsv1.CreateRecordRequest
	updated        []*dnsv1.UpdateRecordRequest
}

func (f *fakeDNS) ListZones(context.Context, *connect.Request[dnsv1.ListZonesRequest]) (*connect.Response[dnsv1.ListZonesResponse], error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return connect.NewResponse(&dnsv1.ListZonesResponse{
		Zones:  f.zones,
		Status: &dnsv1.ServerStatus{ZoneCount: uint32(len(f.zones))},
	}), nil
}

func (f *fakeDNS) DeleteZone(_ context.Context, request *connect.Request[dnsv1.DeleteZoneRequest]) (*connect.Response[dnsv1.DeleteZoneResponse], error) {
	f.deletedZones = append(f.deletedZones, request.Msg.GetZoneId())
	return connect.NewResponse(&dnsv1.DeleteZoneResponse{}), nil
}

func (f *fakeDNS) DeleteRecord(_ context.Context, request *connect.Request[dnsv1.DeleteRecordRequest]) (*connect.Response[dnsv1.DeleteRecordResponse], error) {
	f.deletedRecords = append(f.deletedRecords, request.Msg)
	return connect.NewResponse(&dnsv1.DeleteRecordResponse{Zone: f.zones[0]}), nil
}

func (f *fakeDNS) CreateRecord(_ context.Context, request *connect.Request[dnsv1.CreateRecordRequest]) (*connect.Response[dnsv1.CreateRecordResponse], error) {
	f.created = append(f.created, request.Msg)
	return connect.NewResponse(&dnsv1.CreateRecordResponse{Zone: f.zones[0]}), nil
}

func (f *fakeDNS) UpdateRecord(_ context.Context, request *connect.Request[dnsv1.UpdateRecordRequest]) (*connect.Response[dnsv1.UpdateRecordResponse], error) {
	f.updated = append(f.updated, request.Msg)
	return connect.NewResponse(&dnsv1.UpdateRecordResponse{Zone: f.zones[0]}), nil
}

type fakePlatform struct {
	platformv1connect.PlatformServiceClient

	response *platformv1.GetPlatformStatusResponse
	probed   bool
}

func (f *fakePlatform) GetPlatformStatus(_ context.Context, request *connect.Request[platformv1.GetPlatformStatusRequest]) (*connect.Response[platformv1.GetPlatformStatusResponse], error) {
	f.probed = request.Msg.GetProbe()
	return connect.NewResponse(f.response), nil
}

type fakeMail struct {
	mailv1connect.MailServiceClient

	mailboxes []*mailv1.Mailbox
	password  string

	created  []*mailv1.CreateMailboxRequest
	deleted  []*mailv1.DeleteMailboxRequest
	unbound  []*mailv1.UnbindMailDomainRequest
	resetIDs []string
}

func (f *fakeMail) ListMailboxes(context.Context, *connect.Request[mailv1.ListMailboxesRequest]) (*connect.Response[mailv1.ListMailboxesResponse], error) {
	return connect.NewResponse(&mailv1.ListMailboxesResponse{Mailboxes: f.mailboxes, Limit: 25}), nil
}

func (f *fakeMail) CreateMailbox(_ context.Context, request *connect.Request[mailv1.CreateMailboxRequest]) (*connect.Response[mailv1.CreateMailboxResponse], error) {
	f.created = append(f.created, request.Msg)
	return connect.NewResponse(&mailv1.CreateMailboxResponse{
		Mailbox:           &mailv1.Mailbox{Id: "mb-1", ZoneId: request.Msg.GetZoneId(), Address: request.Msg.GetLocalPart() + "@acme.dev", LocalPart: request.Msg.GetLocalPart()},
		Password:          f.password,
		RetrievalProtocol: "imaps", RetrievalHost: "mail.acme.dev", RetrievalPort: 993,
		SmtpHost: "mail.acme.dev", SmtpPort: 465,
	}), nil
}

func (f *fakeMail) ResetMailboxPassword(_ context.Context, request *connect.Request[mailv1.ResetMailboxPasswordRequest]) (*connect.Response[mailv1.ResetMailboxPasswordResponse], error) {
	f.resetIDs = append(f.resetIDs, request.Msg.GetMailboxId())
	return connect.NewResponse(&mailv1.ResetMailboxPasswordResponse{Password: f.password}), nil
}

func (f *fakeMail) DeleteMailbox(_ context.Context, request *connect.Request[mailv1.DeleteMailboxRequest]) (*connect.Response[mailv1.DeleteMailboxResponse], error) {
	f.deleted = append(f.deleted, request.Msg)
	return connect.NewResponse(&mailv1.DeleteMailboxResponse{}), nil
}

func (f *fakeMail) UnbindMailDomain(_ context.Context, request *connect.Request[mailv1.UnbindMailDomainRequest]) (*connect.Response[mailv1.UnbindMailDomainResponse], error) {
	f.unbound = append(f.unbound, request.Msg)
	return connect.NewResponse(&mailv1.UnbindMailDomainResponse{}), nil
}

type fakeHosting struct {
	hostingv1connect.HostingServiceClient

	site    *hostingv1.Site
	envVars []*hostingv1.EnvVar

	// listErr is what an engine that is not configured answers with.
	listErr error

	envSet   []*hostingv1.SetSiteEnvVarRequest
	envUnset []*hostingv1.DeleteSiteEnvVarRequest
	detached []*hostingv1.DetachSiteRequest
}

func (f *fakeHosting) ListSites(context.Context, *connect.Request[hostingv1.ListSitesRequest]) (*connect.Response[hostingv1.ListSitesResponse], error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return connect.NewResponse(&hostingv1.ListSitesResponse{Sites: []*hostingv1.Site{f.site}}), nil
}

func (f *fakeHosting) GetSite(context.Context, *connect.Request[hostingv1.GetSiteRequest]) (*connect.Response[hostingv1.GetSiteResponse], error) {
	return connect.NewResponse(&hostingv1.GetSiteResponse{Site: f.site}), nil
}

func (f *fakeHosting) ListSiteEnvVars(context.Context, *connect.Request[hostingv1.ListSiteEnvVarsRequest]) (*connect.Response[hostingv1.ListSiteEnvVarsResponse], error) {
	return connect.NewResponse(&hostingv1.ListSiteEnvVarsResponse{Variables: f.envVars, Supported: true}), nil
}

func (f *fakeHosting) SetSiteEnvVar(_ context.Context, request *connect.Request[hostingv1.SetSiteEnvVarRequest]) (*connect.Response[hostingv1.SetSiteEnvVarResponse], error) {
	f.envSet = append(f.envSet, request.Msg)
	return connect.NewResponse(&hostingv1.SetSiteEnvVarResponse{
		Variable: &hostingv1.EnvVar{Name: request.Msg.GetName(), Environment: "production"},
		Note:     "The running site keeps its current values until the next deploy.",
	}), nil
}

func (f *fakeHosting) DeleteSiteEnvVar(_ context.Context, request *connect.Request[hostingv1.DeleteSiteEnvVarRequest]) (*connect.Response[hostingv1.DeleteSiteEnvVarResponse], error) {
	f.envUnset = append(f.envUnset, request.Msg)
	return connect.NewResponse(&hostingv1.DeleteSiteEnvVarResponse{Note: "removed"}), nil
}

func (f *fakeHosting) DetachSite(_ context.Context, request *connect.Request[hostingv1.DetachSiteRequest]) (*connect.Response[hostingv1.DetachSiteResponse], error) {
	f.detached = append(f.detached, request.Msg)
	return connect.NewResponse(&hostingv1.DetachSiteResponse{}), nil
}

// platform is the fixture every test starts from: one zone, one mailbox, one
// site, and no engine that has to be reached over a socket.
type platform struct {
	dns      *fakeDNS
	mail     *fakeMail
	hosting  *fakeHosting
	platform *fakePlatform
}

func newPlatform() *platform {
	return &platform{
		dns: &fakeDNS{zones: []*dnsv1.Zone{{
			Id: "z-1", Name: "acme.dev", Serial: 7,
			Nameservers: []string{"ns1.deephost.test."},
			Records: []*dnsv1.Record{
				{Id: "r-1", Name: "@", Type: dnsv1.RecordType_RECORD_TYPE_A, Ttl: 300, Value: "203.0.113.10"},
				{Id: "r-2", Name: "www", Type: dnsv1.RecordType_RECORD_TYPE_CNAME, Ttl: 300, Value: "acme.dev.", Managed: true, Source: dnsv1.RecordSource_RECORD_SOURCE_HOSTING},
			},
		}}},
		mail: &fakeMail{
			mailboxes: []*mailv1.Mailbox{{Id: "mb-1", ZoneId: "z-1", Address: "mara@acme.dev", LocalPart: "mara"}},
			password:  "correct-horse-battery-staple",
		},
		hosting: &fakeHosting{
			site:    &hostingv1.Site{ZoneId: "z-1", ZoneName: "acme.dev", App: "acme-dev", LatestDeployId: "d-2", LiveDeployId: "d-1"},
			envVars: []*hostingv1.EnvVar{{Name: "API_BASE", Environment: "production"}},
		},
		platform: &fakePlatform{response: &platformv1.GetPlatformStatusResponse{
			Engines: []*platformv1.EngineStatus{
				{Kind: platformv1.EngineKind_ENGINE_KIND_DNS, Configured: true, Reachable: true, Provider: "simpledns"},
				{Kind: platformv1.EngineKind_ENGINE_KIND_HOSTING, Configured: false, MissingEnv: []string{"HOSTING_API_URL"}},
			},
			Nameservers: []string{"ns1.deephost.test."},
			Version:     "test",
		}},
	}
}

func (p *platform) clients() *client.Clients {
	return &client.Clients{
		BaseURL:  "https://api.deephost.test",
		DNS:      p.dns,
		Platform: p.platform,
		Hosting:  p.hosting,
		Mail:     p.mail,
	}
}

// server builds a server on the fakes. A credentialed server is what an
// operator who has logged in has; pass "" for one that has not.
func (p *platform) server(t *testing.T, token string) *Server {
	t.Helper()
	return New(Options{Token: token, Version: "test", Clients: p.clients()})
}
