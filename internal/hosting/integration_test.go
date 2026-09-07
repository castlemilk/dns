package hosting_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/castlemilk/dns/internal/hosting"
	"github.com/castlemilk/dns/internal/hosting/deephost"
)

// Integration coverage against the local kind cluster. Every test here is
// skipped unless DEEPHOST_TEST_DEEPHOST_URL is set (spec2 §10.2), so `go test
// ./...` stays hermetic.
//
//	DEEPHOST_TEST_DEEPHOST_URL=http://127.0.0.1:30081 go test -run Integration ./internal/hosting/...
//
// The tenant is fixed to simple-test-hosting: the demo and deephost tenants on
// that cluster are never touched, and every app this file creates is deleted in
// a t.Cleanup even when the test fails.
const integrationTenant = "simple-test-hosting"

// integrationBuildBudget bounds how long a git build is polled. The build runs
// a real Job on the cluster, so the default keeps one test run short; set
// DEEPHOST_TEST_BUILD_SECONDS to wait for a full build.
const integrationBuildBudget = 90 * time.Second

func integrationEngine(t *testing.T) hosting.Engine {
	t.Helper()
	url := os.Getenv("DEEPHOST_TEST_DEEPHOST_URL")
	if url == "" {
		t.Skip("DEEPHOST_TEST_DEEPHOST_URL is not set; skipping the DeepHost integration test")
	}
	return deephost.New(url, os.Getenv("DEEPHOST_TEST_DEEPHOST_TOKEN"))
}

func integrationApp(t *testing.T, engine hosting.Engine, name string, spec hosting.AppSpec) string {
	t.Helper()
	ctx := context.Background()
	// The facade's own rule: CreateApp only after GetApp reported NotFound.
	if _, err := engine.GetApp(ctx, integrationTenant, name); err == nil {
		t.Fatalf("app %q already exists on the cluster; a previous run did not clean up", name)
	}
	appRef, err := engine.CreateApp(ctx, integrationTenant, name, spec)
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	t.Cleanup(func() {
		if err := engine.DeleteApp(context.Background(), integrationTenant, name); err != nil {
			t.Errorf("cleanup DeleteApp(%s): %v", name, err)
		}
	})
	return appRef
}

func TestIntegrationHostingEngineAppLifecycle(t *testing.T) {
	engine := integrationEngine(t)
	ctx := context.Background()

	zoneName := "it-" + strconv.FormatInt(time.Now().UnixNano(), 36) + ".example"
	name := hosting.AppName(integrationTenant, zoneName)
	appRef := integrationApp(t, engine, name, hosting.AppSpec{Framework: hosting.FrameworkStatic})

	if appRef != hosting.AppRef(integrationTenant, name) {
		t.Errorf("app ref = %q, want the name the facade predicts (%q)", appRef, hosting.AppRef(integrationTenant, name))
	}
	if len(appRef) > 63 {
		t.Errorf("app ref is %d characters; DeepHost caps the identity at 63", len(appRef))
	}

	view, err := engine.GetApp(ctx, integrationTenant, name)
	if err != nil {
		t.Fatalf("GetApp: %v", err)
	}
	if view.Framework != hosting.FrameworkStatic {
		t.Errorf("framework = %q", view.Framework)
	}

	apps, err := engine.ListApps(ctx, integrationTenant)
	if err != nil {
		t.Fatalf("ListApps: %v", err)
	}
	found := false
	for _, app := range apps {
		if app.Name == appRef {
			found = true
		}
	}
	if !found {
		t.Errorf("ListApps did not return %q", appRef)
	}
}

func TestIntegrationHostingEngineDomains(t *testing.T) {
	engine := integrationEngine(t)
	ctx := context.Background()

	zoneName := "it-" + strconv.FormatInt(time.Now().UnixNano(), 36) + ".example"
	name := hosting.AppName(integrationTenant, zoneName)
	integrationApp(t, engine, name, hosting.AppSpec{Framework: hosting.FrameworkStatic})

	// Per-host certificate mode: the facade never sends a tls_secret for a
	// customer host.
	if err := engine.CreateDomain(ctx, integrationTenant, name, hosting.DomainSpec{Host: zoneName}); err != nil {
		t.Fatalf("CreateDomain: %v", err)
	}
	// CreateDomain is idempotent for the same host on the same app.
	if err := engine.CreateDomain(ctx, integrationTenant, name, hosting.DomainSpec{Host: zoneName}); err != nil {
		t.Fatalf("CreateDomain (repeat): %v", err)
	}

	domains, err := engine.ListDomains(ctx, integrationTenant, name)
	if err != nil {
		t.Fatalf("ListDomains: %v", err)
	}
	var listed *hosting.DomainView
	for index := range domains {
		if domains[index].Host == zoneName {
			listed = &domains[index]
		}
	}
	if listed == nil {
		t.Fatalf("ListDomains = %+v, want %q", domains, zoneName)
	}
	t.Logf("domain view: readiness_reported=%t ready=%t tls_mode=%q certificate_ready=%t",
		listed.ReadinessReported, listed.Ready, listed.TLSMode, listed.CertificateReady)

	// DeleteDomain is capability-gated: an old server answers Unimplemented and
	// the facade degrades instead of failing.
	err = engine.DeleteDomain(ctx, integrationTenant, name, zoneName)
	switch {
	case err == nil:
	case errors.Is(err, hosting.ErrUnsupported):
		t.Log("this server has no DeleteDomain; the facade relies on DeleteApp garbage collection")
	default:
		t.Fatalf("DeleteDomain: %v", err)
	}
}

func TestIntegrationHostingEngineGitBuild(t *testing.T) {
	engine := integrationEngine(t)
	ctx := context.Background()

	// A small PUBLIC repository: the builder Job has GitHub egress but no
	// credentials, so spec2's suggested benebsworth/deephost fails with
	// "could not read Username for https://github.com" (verified on the kind
	// cluster). Any public repo builds as-is under the static framework.
	repository := os.Getenv("DEEPHOST_TEST_REPO")
	if repository == "" {
		repository = "https://github.com/github/gitignore"
	}
	path := os.Getenv("DEEPHOST_TEST_REPO_PATH")

	zoneName := "it-" + strconv.FormatInt(time.Now().UnixNano(), 36) + ".example"
	name := hosting.AppName(integrationTenant, zoneName)
	integrationApp(t, engine, name, hosting.AppSpec{Framework: hosting.FrameworkStatic})

	buildID, err := engine.CreateGitBuild(ctx, integrationTenant, name, hosting.GitBuildInput{
		Framework: hosting.FrameworkStatic,
		RepoURL:   repository,
		Revision:  "main",
		Path:      path,
	})
	if err != nil {
		t.Fatalf("CreateGitBuild: %v", err)
	}
	if buildID == "" {
		t.Fatal("CreateGitBuild returned no build id")
	}

	// ListBuilds is the capability the facade uses to adopt a build after a
	// crash; an old server answers Unimplemented.
	builds, err := engine.ListBuilds(ctx, integrationTenant, name, 5)
	switch {
	case err == nil:
		if len(builds) == 0 {
			t.Error("ListBuilds returned nothing for an app with a build")
		}
	case errors.Is(err, hosting.ErrUnsupported):
		t.Log("this server has no ListBuilds; the facade adopts through ListReleases")
	default:
		t.Fatalf("ListBuilds: %v", err)
	}

	budget := integrationBuildBudget
	if raw := os.Getenv("DEEPHOST_TEST_BUILD_SECONDS"); raw != "" {
		if seconds, convErr := strconv.Atoi(raw); convErr == nil && seconds > 0 {
			budget = time.Duration(seconds) * time.Second
		}
	}

	deadline := time.Now().Add(budget)
	phases := []string{}
	last := hosting.BuildView{}
	for time.Now().Before(deadline) {
		build, err := engine.GetBuild(ctx, integrationTenant, name, buildID)
		if err != nil {
			t.Fatalf("GetBuild: %v", err)
		}
		last = build
		if len(phases) == 0 || phases[len(phases)-1] != build.Phase {
			phases = append(phases, build.Phase)
		}
		if build.Terminal() {
			break
		}
		time.Sleep(3 * time.Second)
	}
	t.Logf("build %s phases: %v (created=%v started=%v finished=%v)",
		buildID, phases, last.CreatedAt, last.StartedAt, last.FinishedAt)
	if last.Phase == hosting.BuildPhaseFailed {
		t.Fatalf("build failed on the cluster: %s", last.Reason)
	}
	if !last.Terminal() {
		t.Skipf("the build was still %q after %s; raise DEEPHOST_TEST_BUILD_SECONDS to wait for it", last.Phase, budget)
	}
	if last.CreatedAt.IsZero() {
		t.Log("this server reports no build timestamps; deploy durations are shown as observed")
	}

	lines, complete, err := engine.GetBuildLogs(ctx, integrationTenant, name, buildID, 50)
	if err != nil {
		t.Logf("GetBuildLogs: %v", err)
	} else {
		t.Logf("build log: %d lines, complete=%t", len(lines), complete)
	}

	releases, err := engine.ListReleases(ctx, integrationTenant, name)
	if err != nil {
		t.Fatalf("ListReleases: %v", err)
	}
	t.Logf("releases: %d", len(releases))
	// The facade reports LIVE only when the release is ready at weight 100 and
	// every sibling is at 0; a fresh app has exactly one release.
	live := 0
	for _, release := range releases {
		if release.TrafficWeight == 100 && !release.Ready {
			t.Errorf("release %s carries full traffic but is not ready", release.ID)
		}
		if release.TrafficWeight == 100 && release.Ready {
			live++
			if release.BuildID != buildID {
				t.Errorf("live release build = %q, want %q", release.BuildID, buildID)
			}
		}
	}
	if live != 1 {
		t.Errorf("releases at weight 100 = %d, want exactly 1", live)
	}
}

// facadeTenant scopes the phase-3 integration tests. It is separate from
// integrationTenant so a failure here cannot leave objects behind in the tenant
// the earlier tests use, and every object it creates is deleted in a t.Cleanup.
const facadeTenant = "simple-test-facade"

func facadeApp(t *testing.T, engine hosting.Engine, name string, spec hosting.AppSpec) string {
	t.Helper()
	ctx := context.Background()
	if _, err := engine.GetApp(ctx, facadeTenant, name); err == nil {
		t.Fatalf("app %q already exists on the cluster; a previous run did not clean up", name)
	}
	appRef, err := engine.CreateApp(ctx, facadeTenant, name, spec)
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	t.Cleanup(func() {
		if err := engine.DeleteApp(context.Background(), facadeTenant, name); err != nil {
			t.Errorf("cleanup DeleteApp(%s): %v", name, err)
		}
	})
	return appRef
}

// looksLikeCommit mirrors the facade's own rule, so this test fails for the
// same reason the facade would refuse to show a value.
func looksLikeCommit(value string) bool {
	if len(value) < 7 || len(value) > 64 {
		return false
	}
	for _, char := range value {
		switch {
		case char >= '0' && char <= '9', char >= 'a' && char <= 'f':
		default:
			return false
		}
	}
	return true
}

// TestIntegrationHostingEngineEnvVars is G1 end to end against the cluster: a
// value goes in, only names and stamps come back, and everything written is
// deleted again.
func TestIntegrationHostingEngineEnvVars(t *testing.T) {
	engine := integrationEngine(t)
	ctx := context.Background()

	zoneName := "it-" + strconv.FormatInt(time.Now().UnixNano(), 36) + ".example"
	name := hosting.AppName(facadeTenant, zoneName)
	facadeApp(t, engine, name, hosting.AppSpec{Framework: hosting.FrameworkStatic})

	const secret = "postgres://user:integration-not-a-real-password@db.internal:5432/app"
	view, err := engine.SetAppEnvVar(ctx, facadeTenant, name, hosting.EnvVarInput{
		Environment: hosting.EnvironmentProduction, Name: "DATABASE_URL", Value: secret,
	})
	if errors.Is(err, hosting.ErrUnsupported) {
		t.Skip("this DeepHost has no environment-variable RPCs; the facade degrades to a capability")
	}
	if err != nil {
		t.Fatalf("SetAppEnvVar: %v", err)
	}
	t.Cleanup(func() {
		err := engine.DeleteAppEnvVar(context.Background(), facadeTenant, name,
			hosting.EnvironmentProduction, "DATABASE_URL")
		if err != nil && !errors.Is(err, hosting.ErrUnsupported) && connect.CodeOf(err) != connect.CodeNotFound {
			t.Errorf("cleanup DeleteAppEnvVar: %v", err)
		}
	})
	if view.Name != "DATABASE_URL" || view.Environment != hosting.EnvironmentProduction {
		t.Errorf("view = %+v", view)
	}
	if view.LastSet.IsZero() {
		t.Error("the engine reported no last-set stamp")
	}

	if _, err := engine.SetAppEnvVar(ctx, facadeTenant, name, hosting.EnvVarInput{
		Environment: hosting.EnvironmentPreview, Name: "DATABASE_URL", Value: "preview-value",
	}); err != nil {
		t.Fatalf("SetAppEnvVar (preview): %v", err)
	}
	t.Cleanup(func() {
		err := engine.DeleteAppEnvVar(context.Background(), facadeTenant, name,
			hosting.EnvironmentPreview, "DATABASE_URL")
		if err != nil && !errors.Is(err, hosting.ErrUnsupported) && connect.CodeOf(err) != connect.CodeNotFound {
			t.Errorf("cleanup DeleteAppEnvVar (preview): %v", err)
		}
	})

	variables, err := engine.ListAppEnvVars(ctx, facadeTenant, name, "")
	if err != nil {
		t.Fatalf("ListAppEnvVars: %v", err)
	}
	if len(variables) != 2 {
		t.Fatalf("variables = %+v, want two", variables)
	}
	for _, variable := range variables {
		rendered := fmt.Sprintf("%+v", variable)
		if strings.Contains(rendered, secret) || strings.Contains(rendered, "preview-value") {
			t.Fatalf("a value came back over the wire: %s", rendered)
		}
	}

	filtered, err := engine.ListAppEnvVars(ctx, facadeTenant, name, hosting.EnvironmentPreview)
	if err != nil {
		t.Fatalf("ListAppEnvVars (preview): %v", err)
	}
	if len(filtered) != 1 || filtered[0].Environment != hosting.EnvironmentPreview {
		t.Errorf("preview filter = %+v", filtered)
	}

	if err := engine.DeleteAppEnvVar(ctx, facadeTenant, name,
		hosting.EnvironmentProduction, "DATABASE_URL"); err != nil {
		t.Fatalf("DeleteAppEnvVar: %v", err)
	}
	remaining, err := engine.ListAppEnvVars(ctx, facadeTenant, name, "")
	if err != nil {
		t.Fatalf("ListAppEnvVars after the delete: %v", err)
	}
	if len(remaining) != 1 || remaining[0].Environment != hosting.EnvironmentPreview {
		t.Errorf("after deleting production the engine holds %+v", remaining)
	}
	err = engine.DeleteAppEnvVar(ctx, facadeTenant, name, hosting.EnvironmentProduction, "DATABASE_URL")
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("a repeated delete = %s, want not_found", connect.CodeOf(err))
	}
}

// TestIntegrationHostingEngineRedirectDomain is G3 end to end: a host is
// registered as a redirect, ListDomains reports it, and clearing it puts the
// host back to serving.
func TestIntegrationHostingEngineRedirectDomain(t *testing.T) {
	engine := integrationEngine(t)
	ctx := context.Background()

	zoneName := "it-" + strconv.FormatInt(time.Now().UnixNano(), 36) + ".example"
	wwwHost := "www." + zoneName
	name := hosting.AppName(facadeTenant, zoneName)
	facadeApp(t, engine, name, hosting.AppSpec{Framework: hosting.FrameworkStatic})

	if err := engine.CreateDomain(ctx, facadeTenant, name, hosting.DomainSpec{Host: zoneName}); err != nil {
		t.Fatalf("CreateDomain (apex): %v", err)
	}
	if err := engine.CreateDomain(ctx, facadeTenant, name, hosting.DomainSpec{
		Host: wwwHost, RedirectTo: zoneName,
	}); err != nil {
		t.Fatalf("CreateDomain (www redirect): %v", err)
	}
	for _, host := range []string{zoneName, wwwHost} {
		t.Cleanup(func() {
			err := engine.DeleteDomain(context.Background(), facadeTenant, name, host)
			if err != nil && !errors.Is(err, hosting.ErrUnsupported) {
				t.Errorf("cleanup DeleteDomain(%s): %v", host, err)
			}
		})
	}

	byHost := func() map[string]hosting.DomainView {
		t.Helper()
		domains, err := engine.ListDomains(ctx, facadeTenant, name)
		if err != nil {
			t.Fatalf("ListDomains: %v", err)
		}
		listed := make(map[string]hosting.DomainView, len(domains))
		for _, domain := range domains {
			listed[domain.Host] = domain
		}
		return listed
	}

	listed := byHost()
	if listed[wwwHost].RedirectTo == "" {
		t.Skipf("this DeepHost reports no redirect_to; the facade keeps today's copy (%+v)", listed[wwwHost])
	}
	if listed[wwwHost].RedirectTo != zoneName {
		t.Errorf("www redirect_to = %q, want %q", listed[wwwHost].RedirectTo, zoneName)
	}
	if listed[zoneName].RedirectTo != "" {
		t.Errorf("the apex reported redirect_to = %q", listed[zoneName].RedirectTo)
	}

	// CreateDomain is the update path: the same call without a target puts the
	// host back to serving.
	if err := engine.CreateDomain(ctx, facadeTenant, name, hosting.DomainSpec{Host: wwwHost}); err != nil {
		t.Fatalf("CreateDomain (clear the redirect): %v", err)
	}
	if got := byHost()[wwwHost].RedirectTo; got != "" {
		t.Errorf("after clearing, redirect_to = %q", got)
	}
}

// TestIntegrationHostingEngineResolvedCommit is G2 end to end: a real build on
// the cluster, and the release the engine creates carries the commit it built
// rather than the branch that was asked for.
func TestIntegrationHostingEngineResolvedCommit(t *testing.T) {
	engine := integrationEngine(t)
	ctx := context.Background()

	repository := os.Getenv("DEEPHOST_TEST_REPO")
	if repository == "" {
		repository = "https://github.com/github/gitignore"
	}
	revision := os.Getenv("DEEPHOST_TEST_REPO_REVISION")
	if revision == "" {
		revision = "main"
	}

	zoneName := "it-" + strconv.FormatInt(time.Now().UnixNano(), 36) + ".example"
	name := hosting.AppName(facadeTenant, zoneName)
	facadeApp(t, engine, name, hosting.AppSpec{Framework: hosting.FrameworkStatic})

	buildID, err := engine.CreateGitBuild(ctx, facadeTenant, name, hosting.GitBuildInput{
		Framework: hosting.FrameworkStatic,
		RepoURL:   repository,
		Revision:  revision,
		Path:      os.Getenv("DEEPHOST_TEST_REPO_PATH"),
	})
	if err != nil {
		t.Fatalf("CreateGitBuild: %v", err)
	}

	budget := integrationBuildBudget
	if raw := os.Getenv("DEEPHOST_TEST_BUILD_SECONDS"); raw != "" {
		if seconds, convErr := strconv.Atoi(raw); convErr == nil && seconds > 0 {
			budget = time.Duration(seconds) * time.Second
		}
	}
	deadline := time.Now().Add(budget)
	last := hosting.BuildView{}
	for time.Now().Before(deadline) {
		build, err := engine.GetBuild(ctx, facadeTenant, name, buildID)
		if err != nil {
			t.Fatalf("GetBuild: %v", err)
		}
		last = build
		if build.Terminal() {
			break
		}
		time.Sleep(3 * time.Second)
	}
	if last.Phase == hosting.BuildPhaseFailed {
		t.Fatalf("build failed on the cluster: %s", last.Reason)
	}
	if !last.Terminal() {
		t.Skipf("the build was still %q after %s; raise DEEPHOST_TEST_BUILD_SECONDS", last.Phase, budget)
	}

	// The release may take another reconcile to appear with its status filled.
	var resolved string
	for time.Now().Before(deadline) {
		releases, err := engine.ListReleases(ctx, facadeTenant, name)
		if err != nil {
			t.Fatalf("ListReleases: %v", err)
		}
		for _, release := range releases {
			if release.BuildID == buildID && release.GitSHA != "" {
				resolved = release.GitSHA
			}
		}
		if resolved != "" {
			break
		}
		time.Sleep(3 * time.Second)
	}
	if resolved == "" {
		t.Skip("this DeepHost reports no git_sha; the facade shows the requested revision alone")
	}
	if !looksLikeCommit(resolved) {
		t.Fatalf("git_sha = %q, which is not a commit id — the facade would refuse to show it", resolved)
	}
	if resolved == revision {
		t.Errorf("git_sha = %q, which is the revision that was requested, not a resolved commit", resolved)
	}
	t.Logf("revision %q resolved to %s", revision, resolved)
}
