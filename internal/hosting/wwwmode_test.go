package hosting

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"
	hostingv1 "github.com/castlemilk/dns/gen/go/hosting/v1"
	"github.com/castlemilk/dns/internal/activity"
	"github.com/castlemilk/dns/internal/platform"
)

// hostname finds one host on a rendered site.
func hostname(t *testing.T, site *hostingv1.Site, host string) *hostingv1.Hostname {
	t.Helper()
	for _, entry := range site.GetHostnames() {
		if entry.GetHost() == host {
			return entry
		}
	}
	t.Fatalf("site does not report the host %q", host)
	return nil
}

func getSite(t *testing.T, h *harness, zoneID string) *hostingv1.Site {
	t.Helper()
	response, err := h.service.GetSite(context.Background(), connect.NewRequest(&hostingv1.GetSiteRequest{ZoneId: zoneID}))
	if err != nil {
		t.Fatalf("GetSite: %v", err)
	}
	return response.Msg.GetSite()
}

// lastCreateDomain returns the arguments of the most recent CreateDomain for a
// host, so a test can assert what the facade actually asked the engine for.
func lastCreateDomain(t *testing.T, h *harness, host string) map[string]string {
	t.Helper()
	var args map[string]string
	for _, call := range h.engine.Calls() {
		if call.Method == "CreateDomain" && call.Args["host"] == host {
			args = call.Args
		}
	}
	if args == nil {
		t.Fatalf("the facade never called CreateDomain for %q", host)
	}
	return args
}

func TestAttachWithWwwRedirectRegistersTheRedirectAndReportsIt(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	zoneID := h.zoneID(t, "acme.dev")
	h.seedGateway(t, "198.51.100.10")
	attach(t, h, zoneID, func(request *hostingv1.AttachSiteRequest) {
		request.WwwMode = hostingv1.WwwMode_WWW_MODE_REDIRECT
	})
	h.service.watchDomains(context.Background())

	if args := lastCreateDomain(t, h, "www.acme.dev"); args["redirect_to"] != "acme.dev" {
		t.Errorf("www registered with redirect_to = %q, want acme.dev", args["redirect_to"])
	}
	if args := lastCreateDomain(t, h, "acme.dev"); args["redirect_to"] != "" {
		t.Errorf("the apex was registered with redirect_to = %q, want none", args["redirect_to"])
	}

	site := getSite(t, h, zoneID)
	if site.GetWwwMode() != hostingv1.WwwMode_WWW_MODE_REDIRECT {
		t.Errorf("www_mode = %v, want REDIRECT", site.GetWwwMode())
	}
	if got := hostname(t, site, "www.acme.dev").GetRedirectTo(); got != "acme.dev" {
		t.Errorf("www redirect_to = %q, want what the engine reports", got)
	}
	if got := hostname(t, site, "acme.dev").GetRedirectTo(); got != "" {
		t.Errorf("apex redirect_to = %q, want empty", got)
	}
	if !h.service.capabilities(context.Background()).DomainRedirect {
		t.Error("capability domain_redirect stayed false after the engine reported a redirect")
	}

	// The DNS is unchanged: the redirect happens over HTTP, so www still needs
	// a CNAME at the apex for anything to reach the gateway at all.
	found := false
	for _, record := range h.engineRecords(t, zoneID) {
		if record.Name == "www" {
			found = true
		}
	}
	if !found {
		t.Error("a redirecting www host lost its DNS record")
	}
}

func TestAttachDefaultsToServeAndReportsNoRedirect(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	zoneID := h.zoneID(t, "acme.dev")
	h.seedGateway(t, "198.51.100.10")
	attach(t, h, zoneID)
	h.service.watchDomains(context.Background())

	if args := lastCreateDomain(t, h, "www.acme.dev"); args["redirect_to"] != "" {
		t.Errorf("www registered with redirect_to = %q, want none by default", args["redirect_to"])
	}
	site := getSite(t, h, zoneID)
	if site.GetWwwMode() != hostingv1.WwwMode_WWW_MODE_SERVE {
		t.Errorf("www_mode = %v, want SERVE", site.GetWwwMode())
	}
	if got := hostname(t, site, "www.acme.dev").GetRedirectTo(); got != "" {
		t.Errorf("www redirect_to = %q, want empty", got)
	}
	if h.service.capabilities(context.Background()).DomainRedirect {
		t.Error("capability domain_redirect turned true with nothing redirecting")
	}
}

func TestUpdateSiteSwitchesWwwModeBothWays(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	zoneID := h.zoneID(t, "acme.dev")
	h.seedGateway(t, "198.51.100.10")
	attach(t, h, zoneID)
	h.service.watchDomains(context.Background())

	// update returns the site exactly as UpdateSite rendered it, before any
	// watcher pass, so a stale redirect in the response is caught here.
	update := func(mode hostingv1.WwwMode) *hostingv1.Site {
		t.Helper()
		response, err := h.service.UpdateSite(context.Background(), connect.NewRequest(&hostingv1.UpdateSiteRequest{
			ZoneId: zoneID, WwwMode: mode,
		}))
		if err != nil {
			t.Fatalf("UpdateSite(%v): %v", mode, err)
		}
		return response.Msg.GetSite()
	}

	// The response of the switch itself already reports what the engine holds,
	// not the observation from before the switch.
	if got := hostname(t, update(hostingv1.WwwMode_WWW_MODE_REDIRECT), "www.acme.dev").GetRedirectTo(); got != "acme.dev" {
		t.Errorf("the update response reported redirect_to = %q", got)
	}
	if args := lastCreateDomain(t, h, "www.acme.dev"); args["redirect_to"] != "acme.dev" {
		t.Errorf("after the switch redirect_to = %q", args["redirect_to"])
	}
	h.service.watchDomains(context.Background())
	if hostname(t, getSite(t, h, zoneID), "www.acme.dev").GetRedirectTo() != "acme.dev" {
		t.Error("the site does not report the redirect the engine holds")
	}
	if !h.events.has(activity.KindSiteWwwModeChanged) {
		t.Error("no site.www_mode.changed event was recorded")
	}

	site := update(hostingv1.WwwMode_WWW_MODE_SERVE)
	if got := hostname(t, site, "www.acme.dev").GetRedirectTo(); got != "" {
		t.Errorf("the update response still reported redirect_to = %q", got)
	}
	if args := lastCreateDomain(t, h, "www.acme.dev"); args["redirect_to"] != "" {
		t.Errorf("after switching back redirect_to = %q, want none", args["redirect_to"])
	}
	if site.GetWwwMode() != hostingv1.WwwMode_WWW_MODE_SERVE {
		t.Errorf("www_mode = %v, want SERVE", site.GetWwwMode())
	}
	h.service.watchDomains(context.Background())
	if got := hostname(t, getSite(t, h, zoneID), "www.acme.dev").GetRedirectTo(); got != "" {
		t.Errorf("www redirect_to = %q after the next watcher pass", got)
	}
}

func TestUpdateSiteLeavesWwwModeAloneWhenUnspecified(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	zoneID := h.zoneID(t, "acme.dev")
	h.seedGateway(t, "198.51.100.10")
	attach(t, h, zoneID, func(request *hostingv1.AttachSiteRequest) {
		request.WwwMode = hostingv1.WwwMode_WWW_MODE_REDIRECT
	})
	h.service.watchDomains(context.Background())

	branch := "release"
	_, err := h.service.UpdateSite(context.Background(), connect.NewRequest(&hostingv1.UpdateSiteRequest{
		ZoneId: zoneID, Branch: &branch,
	}))
	if err != nil {
		t.Fatalf("UpdateSite: %v", err)
	}
	if h.site(t, zoneID).WWWMode != WWWModeRedirect {
		t.Error("a partial update changed the www mode")
	}
	if h.events.has(activity.KindSiteWwwModeChanged) {
		t.Error("a partial update recorded a mode change")
	}
}

func TestWwwRedirectIsRefusedWithoutAWwwHost(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	zoneID := h.zoneID(t, "acme.dev")
	h.seedGateway(t, "198.51.100.10")

	_, err := h.service.AttachSite(context.Background(), connect.NewRequest(&hostingv1.AttachSiteRequest{
		ZoneId:    zoneID,
		Framework: hostingv1.Framework_FRAMEWORK_STATIC,
		SkipWww:   true,
		WwwMode:   hostingv1.WwwMode_WWW_MODE_REDIRECT,
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("code = %s, want invalid_argument", connect.CodeOf(err))
	}
	if _, storeErr := h.store.GetSite(context.Background(), zoneID); storeErr == nil {
		t.Error("a refused attach left a site row behind")
	}

	attach(t, h, zoneID, func(request *hostingv1.AttachSiteRequest) { request.SkipWww = true })
	_, err = h.service.UpdateSite(context.Background(), connect.NewRequest(&hostingv1.UpdateSiteRequest{
		ZoneId: zoneID, WwwMode: hostingv1.WwwMode_WWW_MODE_REDIRECT,
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("update code = %s, want invalid_argument", connect.CodeOf(err))
	}
}

func TestTheWatcherRepairsARedirectThatDriftedAndLeavesAnOldEngineAlone(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	zoneID := h.zoneID(t, "acme.dev")
	h.seedGateway(t, "198.51.100.10")
	attach(t, h, zoneID, func(request *hostingv1.AttachSiteRequest) {
		request.WwwMode = hostingv1.WwwMode_WWW_MODE_REDIRECT
	})
	ctx := context.Background()
	h.service.watchDomains(ctx)

	// Something outside this control plane cleared the redirect.
	if err := h.engine.CreateDomain(ctx, "simple-test", AppName("simple-test", "acme.dev"), DomainSpec{
		Host: "www.acme.dev",
	}); err != nil {
		t.Fatalf("clear the redirect: %v", err)
	}
	h.service.watchDomains(ctx)
	h.service.watchDomains(ctx)

	if got := hostname(t, getSite(t, h, zoneID), "www.acme.dev").GetRedirectTo(); got != "acme.dev" {
		t.Errorf("after the repair redirect_to = %q, want acme.dev", got)
	}
}

func TestAnEngineWithoutRedirectSupportIsNotReRegisteredForEver(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	zoneID := h.zoneID(t, "acme.dev")
	h.seedGateway(t, "198.51.100.10")
	h.engine.SetOldServer(true)
	attach(t, h, zoneID, func(request *hostingv1.AttachSiteRequest) {
		request.WwwMode = hostingv1.WwwMode_WWW_MODE_REDIRECT
	})
	ctx := context.Background()
	h.service.watchDomains(ctx)

	before := 0
	for _, call := range h.engine.Calls() {
		if call.Method == "CreateDomain" {
			before++
		}
	}
	h.service.watchDomains(ctx)
	h.service.watchDomains(ctx)
	after := 0
	for _, call := range h.engine.Calls() {
		if call.Method == "CreateDomain" {
			after++
		}
	}
	if after != before {
		t.Errorf("the watcher re-registered %d hosts against a server with no redirect field", after-before)
	}

	// The site says what it asked for; the host says what the engine reports.
	site := getSite(t, h, zoneID)
	if site.GetWwwMode() != hostingv1.WwwMode_WWW_MODE_REDIRECT {
		t.Error("the requested mode was not kept")
	}
	if got := hostname(t, site, "www.acme.dev").GetRedirectTo(); got != "" {
		t.Errorf("www redirect_to = %q, want empty from a server that reports none", got)
	}
	if h.service.capabilities(ctx).DomainRedirect {
		t.Error("capability domain_redirect turned true without the engine reporting one")
	}
}

func TestRedirectTargetForOnlyEverAppliesToTheWwwHost(t *testing.T) {
	t.Parallel()

	doc := platform.SiteDoc{ZoneName: "acme.dev", WWWMode: WWWModeRedirect}
	tests := []struct {
		role string
		want string
	}{
		{role: HostRoleWWW, want: "acme.dev"},
		{role: HostRoleApex, want: ""},
		{role: HostRoleAuto, want: ""},
	}
	for _, test := range tests {
		if got := redirectTargetFor(doc, test.role); got != test.want {
			t.Errorf("redirectTargetFor(%s) = %q, want %q", test.role, got, test.want)
		}
	}
	serving := platform.SiteDoc{ZoneName: "acme.dev"}
	if got := redirectTargetFor(serving, HostRoleWWW); got != "" {
		t.Errorf("a serving site asked for the redirect %q", got)
	}
}

func TestRebuildRecoversTheRedirectingWwwHostFromTheEngine(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	zoneID := h.zoneID(t, "acme.dev")
	h.seedGateway(t, "198.51.100.10")
	attach(t, h, zoneID, func(request *hostingv1.AttachSiteRequest) {
		request.WwwMode = hostingv1.WwwMode_WWW_MODE_REDIRECT
	})
	ctx := context.Background()
	h.service.watchDomains(ctx)

	// A redirecting host is not in App.spec.domains, so a rebuild that trusted
	// that list alone would drop the www host and then delete its DNS record.
	if err := h.store.DeleteSite(ctx, zoneID); err != nil {
		t.Fatalf("delete site: %v", err)
	}
	if _, err := h.service.Rebuild(ctx, false); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}

	site := h.site(t, zoneID)
	if !site.WWW {
		t.Fatal("the rebuilt site lost its www host")
	}
	if site.WWWMode != WWWModeRedirect {
		t.Errorf("www_mode = %q, want it recovered from the engine", site.WWWMode)
	}
	found := false
	for _, record := range h.engineRecords(t, zoneID) {
		if record.Name == "www" {
			found = true
		}
	}
	if !found {
		t.Error("the rebuild deleted the www record of a redirecting site")
	}
}

// The activity log is the record of what happened, so it may not announce a
// redirect on an engine that does not make one. Finding 43's console half says
// so on screen; this is the same statement in the log.
func TestTheWwwModeEventReportsWhatTheEngineDidNotWhatWasAsked(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	zoneID := h.zoneID(t, "acme.dev")
	h.seedGateway(t, "198.51.100.10")
	h.engine.SetOldServer(true)
	attach(t, h, zoneID)
	ctx := context.Background()
	h.service.watchDomains(ctx)

	if _, err := h.service.UpdateSite(ctx, connect.NewRequest(&hostingv1.UpdateSiteRequest{
		ZoneId: zoneID, WwwMode: hostingv1.WwwMode_WWW_MODE_REDIRECT,
	})); err != nil {
		t.Fatalf("UpdateSite: %v", err)
	}

	var event activity.Event
	for _, candidate := range h.events.all() {
		if candidate.Kind == activity.KindSiteWwwModeChanged {
			event = candidate
		}
	}
	if event.Kind == "" {
		t.Fatalf("recorded %v, want a site.www_mode.changed event", h.events.kinds())
	}
	if strings.Contains(event.Summary, "now redirects to") {
		t.Errorf("summary = %q, but the engine reports no redirect on www.acme.dev", event.Summary)
	}
	if !strings.Contains(event.Summary, "reports no redirect") {
		t.Errorf("summary = %q, want it to say the engine reports no redirect", event.Summary)
	}
	if event.Severity != activity.SeverityWarn {
		t.Errorf("severity = %q, want warn for a request the engine did not carry out", event.Severity)
	}
	if event.Details["www_mode"] != WWWModeRedirect {
		t.Errorf("details = %v, want the requested mode kept", event.Details)
	}
}

// The same event on an engine that does redirect names the target the engine
// reports, not the one that was requested.
func TestTheWwwModeEventNamesTheRedirectTheEngineReports(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	zoneID := h.zoneID(t, "acme.dev")
	h.seedGateway(t, "198.51.100.10")
	attach(t, h, zoneID)
	ctx := context.Background()
	h.service.watchDomains(ctx)

	if _, err := h.service.UpdateSite(ctx, connect.NewRequest(&hostingv1.UpdateSiteRequest{
		ZoneId: zoneID, WwwMode: hostingv1.WwwMode_WWW_MODE_REDIRECT,
	})); err != nil {
		t.Fatalf("UpdateSite: %v", err)
	}
	var event activity.Event
	for _, candidate := range h.events.all() {
		if candidate.Kind == activity.KindSiteWwwModeChanged {
			event = candidate
		}
	}
	if event.Summary != "www.acme.dev now redirects to acme.dev" {
		t.Errorf("summary = %q", event.Summary)
	}
	if event.Severity != activity.SeverityInfo {
		t.Errorf("severity = %q, want info", event.Severity)
	}
}
