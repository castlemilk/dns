package hosting

import (
	"context"
	"errors"
	"io"
	"time"

	"connectrpc.com/connect"
	"github.com/castlemilk/dns/internal/telemetry"
)

// meteredEngine records one `deephost.engine.requests` measurement per engine
// call. It wraps whichever Engine the facade is using, so the metric exists
// whether the control plane is talking to DeepHost or to the in-memory fake,
// and no call site has to remember to instrument itself.
//
// The attributes are bounded by construction: the operation is the method name,
// the outcome is one of three words, and the error type is the Connect code —
// never a message, a host or an id.
type meteredEngine struct {
	inner   Engine
	metrics *telemetry.Metrics
}

func newMeteredEngine(inner Engine, metrics *telemetry.Metrics) Engine {
	if inner == nil {
		return nil
	}
	return meteredEngine{inner: inner, metrics: telemetry.Select(metrics)}
}

func (m meteredEngine) observe(ctx context.Context, operation string, started time.Time, err error) {
	outcome, errorType := "success", ""
	switch {
	case err == nil:
	case errors.Is(err, ErrUnsupported):
		outcome, errorType = "unsupported", "unimplemented"
	default:
		outcome, errorType = "error", connect.CodeOf(err).String()
	}
	m.metrics.EngineRequest(ctx, "hosting", operation, outcome, errorType, time.Since(started))
}

func (m meteredEngine) Health(ctx context.Context) error {
	started := time.Now()
	err := m.inner.Health(ctx)
	m.observe(ctx, "health", started, err)
	return err
}

func (m meteredEngine) CreateApp(ctx context.Context, tenant, app string, spec AppSpec) (string, error) {
	started := time.Now()
	ref, err := m.inner.CreateApp(ctx, tenant, app, spec)
	m.observe(ctx, "create_app", started, err)
	return ref, err
}

func (m meteredEngine) UpdateApp(ctx context.Context, tenant, app string, patch AppPatch) error {
	started := time.Now()
	err := m.inner.UpdateApp(ctx, tenant, app, patch)
	m.observe(ctx, "update_app", started, err)
	return err
}

func (m meteredEngine) GetApp(ctx context.Context, tenant, app string) (AppView, error) {
	started := time.Now()
	view, err := m.inner.GetApp(ctx, tenant, app)
	m.observe(ctx, "get_app", started, err)
	return view, err
}

func (m meteredEngine) ListApps(ctx context.Context, tenant string) ([]AppView, error) {
	started := time.Now()
	views, err := m.inner.ListApps(ctx, tenant)
	m.observe(ctx, "list_apps", started, err)
	return views, err
}

func (m meteredEngine) DeleteApp(ctx context.Context, tenant, app string) error {
	started := time.Now()
	err := m.inner.DeleteApp(ctx, tenant, app)
	m.observe(ctx, "delete_app", started, err)
	return err
}

func (m meteredEngine) CreateGitBuild(ctx context.Context, tenant, app string, in GitBuildInput) (string, error) {
	started := time.Now()
	buildID, err := m.inner.CreateGitBuild(ctx, tenant, app, in)
	m.observe(ctx, "create_git_build", started, err)
	return buildID, err
}

func (m meteredEngine) Deploy(
	ctx context.Context,
	tenant, app string,
	framework Framework,
	archive io.Reader,
) (string, error) {
	started := time.Now()
	buildID, err := m.inner.Deploy(ctx, tenant, app, framework, archive)
	m.observe(ctx, "deploy", started, err)
	return buildID, err
}

func (m meteredEngine) GetBuild(ctx context.Context, tenant, app, buildID string) (BuildView, error) {
	started := time.Now()
	view, err := m.inner.GetBuild(ctx, tenant, app, buildID)
	m.observe(ctx, "get_build", started, err)
	return view, err
}

func (m meteredEngine) ListBuilds(ctx context.Context, tenant, app string, limit int) ([]BuildView, error) {
	started := time.Now()
	views, err := m.inner.ListBuilds(ctx, tenant, app, limit)
	m.observe(ctx, "list_builds", started, err)
	return views, err
}

func (m meteredEngine) GetBuildLogs(
	ctx context.Context,
	tenant, app, buildID string,
	tail int,
) ([]string, bool, error) {
	started := time.Now()
	lines, complete, err := m.inner.GetBuildLogs(ctx, tenant, app, buildID, tail)
	m.observe(ctx, "get_build_logs", started, err)
	return lines, complete, err
}

func (m meteredEngine) ListReleases(ctx context.Context, tenant, app string) ([]ReleaseView, error) {
	started := time.Now()
	views, err := m.inner.ListReleases(ctx, tenant, app)
	m.observe(ctx, "list_releases", started, err)
	return views, err
}

func (m meteredEngine) PromoteRelease(ctx context.Context, tenant, app, releaseID string) error {
	started := time.Now()
	err := m.inner.PromoteRelease(ctx, tenant, app, releaseID)
	m.observe(ctx, "promote_release", started, err)
	return err
}

func (m meteredEngine) CreateDomain(ctx context.Context, tenant, app string, spec DomainSpec) error {
	started := time.Now()
	err := m.inner.CreateDomain(ctx, tenant, app, spec)
	m.observe(ctx, "create_domain", started, err)
	return err
}

func (m meteredEngine) ListDomains(ctx context.Context, tenant, app string) ([]DomainView, error) {
	started := time.Now()
	views, err := m.inner.ListDomains(ctx, tenant, app)
	m.observe(ctx, "list_domains", started, err)
	return views, err
}

func (m meteredEngine) DeleteDomain(ctx context.Context, tenant, app, host string) error {
	started := time.Now()
	err := m.inner.DeleteDomain(ctx, tenant, app, host)
	m.observe(ctx, "delete_domain", started, err)
	return err
}

// SetAppEnvVar's measurement names the operation and its outcome. The
// variable's name is not an attribute and its value never leaves the input
// struct, so no part of a secret can reach telemetry.
func (m meteredEngine) SetAppEnvVar(
	ctx context.Context,
	tenant, app string,
	in EnvVarInput,
) (EnvVarView, error) {
	started := time.Now()
	view, err := m.inner.SetAppEnvVar(ctx, tenant, app, in)
	m.observe(ctx, "set_app_env_var", started, err)
	return view, err
}

func (m meteredEngine) DeleteAppEnvVar(ctx context.Context, tenant, app, environment, name string) error {
	started := time.Now()
	err := m.inner.DeleteAppEnvVar(ctx, tenant, app, environment, name)
	m.observe(ctx, "delete_app_env_var", started, err)
	return err
}

func (m meteredEngine) ListAppEnvVars(
	ctx context.Context,
	tenant, app, environment string,
) ([]EnvVarView, error) {
	started := time.Now()
	views, err := m.inner.ListAppEnvVars(ctx, tenant, app, environment)
	m.observe(ctx, "list_app_env_vars", started, err)
	return views, err
}

var _ Engine = meteredEngine{}
