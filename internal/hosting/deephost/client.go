// Package deephost is the hosting.Engine (internal/hosting/engineapi.Engine)
// implemented over the generated
// deephost.v1.ControlPlaneService Connect client.
//
// It holds the engine's bearer token and never lets it out: the token is added
// by a transport, not by a request field, and no error this package returns
// carries a request body. The per-deploy git token is the one secret that does
// travel in a request; the request struct that carries it is zeroed as soon as
// the call returns, and the token is never wrapped into an error.
package deephost

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect"
	deephostv1 "github.com/castlemilk/dns/gen/go/deephost/v1"
	"github.com/castlemilk/dns/gen/go/deephost/v1/deephostv1connect"
	"github.com/castlemilk/dns/internal/hosting/engineapi"
)

// Timeouts. Unary calls are bounded twice — by the response-header timeout of
// their transport and by a per-call context — so a control plane that accepts
// the connection and then stops answering cannot pin a worker. Deploy gets
// neither: DeepHost writes the response headers only after it has spooled the
// whole archive, hashed it, put it in object storage and created the Build, so
// a response-header timeout there would abort every upload larger than a few
// megabytes. It is bounded solely by DeployTimeout.
const (
	// UnaryTimeout bounds one unary call.
	UnaryTimeout = 10 * time.Second
	// DeployTimeout bounds a whole upload stream.
	DeployTimeout = 5 * time.Minute
	// deployFrameBytes is the frame size DeepHost's own CLI uses.
	deployFrameBytes = 64 << 10
)

// ignore drops an error deliberately. It exists because this repository's
// errcheck configuration rejects the `_ = f()` form.
func ignore(error) {}

// Client is the Engine backed by a real DeepHost control plane.
type Client struct {
	unary  deephostv1connect.ControlPlaneServiceClient
	stream deephostv1connect.ControlPlaneServiceClient
	http   *http.Client
	base   string
}

// Option customises the client. Tests use WithHTTPClient to point at an
// httptest server; production uses none.
type Option func(*settings)

type settings struct {
	unaryHTTP  *http.Client
	streamHTTP *http.Client
}

// WithHTTPClient replaces both the unary and the streaming HTTP client. It
// exists for tests; production builds its own transports so the two timeout
// regimes stay separate.
func WithHTTPClient(client *http.Client) Option {
	return func(s *settings) {
		s.unaryHTTP = client
		s.streamHTTP = client
	}
}

// New builds a client for baseURL. token may be empty (the kind cluster runs
// without auth); when set it is sent as `Authorization: Bearer <token>` on
// every call by a transport, so it can never be logged as a request field.
func New(baseURL, token string, options ...Option) *Client {
	applied := settings{}
	for _, option := range options {
		if option != nil {
			option(&applied)
		}
	}
	if applied.unaryHTTP == nil {
		applied.unaryHTTP = &http.Client{
			Transport: bearerTransport(unaryTransport(), token),
			Timeout:   UnaryTimeout + 5*time.Second,
		}
	} else {
		applied.unaryHTTP = withBearer(applied.unaryHTTP, token)
	}
	if applied.streamHTTP == nil {
		applied.streamHTTP = &http.Client{
			Transport: bearerTransport(streamingTransport(), token),
			Timeout:   0,
		}
	} else {
		applied.streamHTTP = withBearer(applied.streamHTTP, token)
	}

	base := strings.TrimRight(baseURL, "/")
	return &Client{
		unary:  deephostv1connect.NewControlPlaneServiceClient(applied.unaryHTTP, base),
		stream: deephostv1connect.NewControlPlaneServiceClient(applied.streamHTTP, base),
		http:   applied.unaryHTTP,
		base:   base,
	}
}

func withBearer(client *http.Client, token string) *http.Client {
	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	copied := *client
	copied.Transport = bearerTransport(transport, token)
	return &copied
}

// unaryTransport bounds the time between sending a request and seeing the
// response headers, so a half-open connection is noticed in seconds.
func unaryTransport() http.RoundTripper {
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return http.DefaultTransport
	}
	cloned := transport.Clone()
	cloned.ResponseHeaderTimeout = UnaryTimeout
	cloned.ForceAttemptHTTP2 = true
	return cloned
}

// streamingTransport is the same transport without ResponseHeaderTimeout.
func streamingTransport() http.RoundTripper {
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return http.DefaultTransport
	}
	cloned := transport.Clone()
	cloned.ResponseHeaderTimeout = 0
	cloned.ForceAttemptHTTP2 = true
	return cloned
}

type authRoundTripper struct {
	next  http.RoundTripper
	token string
}

func bearerTransport(next http.RoundTripper, token string) http.RoundTripper {
	if token == "" {
		return next
	}
	return authRoundTripper{next: next, token: token}
}

func (t authRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	cloned := request.Clone(request.Context())
	cloned.Header.Set("Authorization", "Bearer "+t.token)
	return t.next.RoundTrip(cloned)
}

// call bounds one unary RPC and maps its error.
func call[Req, Res any](
	ctx context.Context,
	invoke func(context.Context, *connect.Request[Req]) (*connect.Response[Res], error),
	message *Req,
) (*Res, error) {
	callCtx, cancel := context.WithTimeout(ctx, UnaryTimeout)
	defer cancel()
	response, err := invoke(callCtx, connect.NewRequest(message))
	if err != nil {
		return nil, mapError(err)
	}
	return response.Msg, nil
}

// mapError turns a Connect error into the sentinel the facade reasons about.
// CodeUnimplemented (an HTTP 404 from a server without the RPC) becomes
// ErrUnsupported; everything else keeps its Connect code so the facade's error
// table can map it onto operator copy.
func mapError(err error) error {
	if err == nil {
		return nil
	}
	if connect.CodeOf(err) == connect.CodeUnimplemented {
		return fmt.Errorf("%w", engineapi.ErrUnsupported)
	}
	return err
}

// Health is the unauthenticated liveness probe. A non-200 is an error; the body
// is never read into the error, so a proxy's HTML page cannot leak into a
// status message.
func (c *Client) Health(ctx context.Context) error {
	callCtx, cancel := context.WithTimeout(ctx, UnaryTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(callCtx, http.MethodGet, c.base+"/healthz", nil)
	if err != nil {
		return fmt.Errorf("hosting engine health: %w", err)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("hosting engine health: %w", err)
	}
	defer func() {
		// Drain a bounded prefix so the connection can be reused, then close.
		// Neither error changes what health means.
		_, copyErr := io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
		ignore(copyErr)
		ignore(response.Body.Close())
	}()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("hosting engine health: HTTP %d", response.StatusCode)
	}
	return nil
}

func (c *Client) CreateApp(ctx context.Context, tenant, app string, spec engineapi.AppSpec) (string, error) {
	message := &deephostv1.CreateAppRequest{
		Tenant:           tenant,
		Name:             app,
		Framework:        frameworkToProto(spec.Framework),
		Repository:       spec.Repository,
		ProductionBranch: spec.Branch,
	}
	if spec.RootDirectory != "" {
		message.BuildSettings = &deephostv1.BuildSettings{RootDirectory: spec.RootDirectory}
	}
	response, err := call(ctx, c.unary.CreateApp, message)
	if err != nil {
		return "", err
	}
	return response.GetName(), nil
}

func (c *Client) UpdateApp(ctx context.Context, tenant, app string, patch engineapi.AppPatch) error {
	message := &deephostv1.UpdateAppRequest{Tenant: tenant, Name: app}
	if patch.Repository != nil {
		message.Repository = patch.Repository
	}
	if patch.Branch != nil {
		message.ProductionBranch = patch.Branch
	}
	if patch.RootDirectory != nil {
		message.BuildSettings = &deephostv1.BuildSettings{RootDirectory: *patch.RootDirectory}
	}
	if message.Repository == nil && message.ProductionBranch == nil && message.BuildSettings == nil {
		return nil
	}
	_, err := call(ctx, c.unary.UpdateApp, message)
	return err
}

func (c *Client) GetApp(ctx context.Context, tenant, app string) (engineapi.AppView, error) {
	response, err := call(ctx, c.unary.GetApp, &deephostv1.GetAppRequest{Tenant: tenant, Name: app})
	if err != nil {
		return engineapi.AppView{}, err
	}
	return appView(response.GetApp()), nil
}

func (c *Client) ListApps(ctx context.Context, tenant string) ([]engineapi.AppView, error) {
	response, err := call(ctx, c.unary.ListApps, &deephostv1.ListAppsRequest{Tenant: tenant})
	if err != nil {
		return nil, err
	}
	apps := make([]engineapi.AppView, 0, len(response.GetApps()))
	for _, app := range response.GetApps() {
		apps = append(apps, appView(app))
	}
	return apps, nil
}

func (c *Client) DeleteApp(ctx context.Context, tenant, app string) error {
	_, err := call(ctx, c.unary.DeleteApp, &deephostv1.DeleteAppRequest{Tenant: tenant, Name: app})
	return err
}

// CreateGitBuild forwards the per-deploy token once. The request struct is
// zeroed before returning so the token does not sit in a heap object for the
// lifetime of the call frame, and no error path formats the request.
func (c *Client) CreateGitBuild(ctx context.Context, tenant, app string, in engineapi.GitBuildInput) (string, error) {
	message := &deephostv1.CreateGitBuildRequest{
		Tenant:    tenant,
		App:       app,
		Framework: frameworkToProto(in.Framework),
		Source: &deephostv1.GitSource{
			RepoUrl:  in.RepoURL,
			Revision: in.Revision,
			Path:     in.Path,
		},
		GitToken: in.GitToken,
	}
	defer func() { message.GitToken = "" }()

	response, err := call(ctx, c.unary.CreateGitBuild, message)
	if err != nil {
		return "", err
	}
	return response.GetBuildId(), nil
}

// Deploy streams archive as 64 KiB frames on the streaming client.
func (c *Client) Deploy(
	ctx context.Context,
	tenant, app string,
	framework engineapi.Framework,
	archive io.Reader,
) (string, error) {
	streamCtx, cancel := context.WithTimeout(ctx, DeployTimeout)
	defer cancel()

	stream := c.stream.Deploy(streamCtx)
	start := &deephostv1.DeployChunk{
		Payload: &deephostv1.DeployChunk_Start{Start: &deephostv1.DeployStart{
			Tenant:    tenant,
			Name:      app,
			Framework: frameworkToProto(framework),
		}},
	}
	if err := stream.Send(start); err != nil {
		return "", mapError(err)
	}

	buffer := make([]byte, deployFrameBytes)
	for {
		read, readErr := archive.Read(buffer)
		if read > 0 {
			frame := &deephostv1.DeployChunk{
				Payload: &deephostv1.DeployChunk_Data{Data: append([]byte(nil), buffer[:read]...)},
			}
			if err := stream.Send(frame); err != nil {
				return "", mapError(err)
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return "", fmt.Errorf("read upload archive: %w", readErr)
		}
	}

	response, err := stream.CloseAndReceive()
	if err != nil {
		return "", mapError(err)
	}
	return response.Msg.GetBuildId(), nil
}

func (c *Client) GetBuild(ctx context.Context, tenant, app, buildID string) (engineapi.BuildView, error) {
	response, err := call(ctx, c.unary.GetBuild, &deephostv1.GetBuildRequest{
		Tenant: tenant, App: app, BuildId: buildID,
	})
	if err != nil {
		return engineapi.BuildView{}, err
	}
	return buildView(response.GetBuild()), nil
}

func (c *Client) ListBuilds(ctx context.Context, tenant, app string, limit int) ([]engineapi.BuildView, error) {
	if limit < 0 {
		limit = 0
	}
	if limit > 200 {
		limit = 200
	}
	response, err := call(ctx, c.unary.ListBuilds, &deephostv1.ListBuildsRequest{
		Tenant: tenant, App: app, Limit: int32(limit),
	})
	if err != nil {
		return nil, err
	}
	builds := make([]engineapi.BuildView, 0, len(response.GetBuilds()))
	for _, build := range response.GetBuilds() {
		builds = append(builds, buildView(build))
	}
	return builds, nil
}

func (c *Client) GetBuildLogs(ctx context.Context, tenant, app, buildID string, tail int) ([]string, bool, error) {
	if tail <= 0 {
		tail = 500
	}
	if tail > 1000 {
		tail = 1000
	}
	response, err := call(ctx, c.unary.GetBuildLogs, &deephostv1.GetBuildLogsRequest{
		Tenant: tenant, App: app, BuildId: buildID, TailLines: int32(tail),
	})
	if err != nil {
		return nil, false, err
	}
	return response.GetLines(), response.GetComplete(), nil
}

func (c *Client) ListReleases(ctx context.Context, tenant, app string) ([]engineapi.ReleaseView, error) {
	response, err := call(ctx, c.unary.ListReleases, &deephostv1.ListReleasesRequest{Tenant: tenant, App: app})
	if err != nil {
		return nil, err
	}
	releases := make([]engineapi.ReleaseView, 0, len(response.GetReleases()))
	for _, release := range response.GetReleases() {
		releases = append(releases, engineapi.ReleaseView{
			ID:            release.GetId(),
			BuildID:       release.GetBuildId(),
			Ready:         release.GetReady(),
			TrafficWeight: release.GetTrafficWeight(),
			GitSHA:        release.GetGitSha(),
			CreatedAt:     unixMillis(release.GetCreatedUnixMs()),
		})
	}
	return releases, nil
}

func (c *Client) PromoteRelease(ctx context.Context, tenant, app, releaseID string) error {
	_, err := call(ctx, c.unary.PromoteRelease, &deephostv1.PromoteReleaseRequest{
		Tenant: tenant, App: app, ReleaseId: releaseID,
	})
	return err
}

// CreateDomain registers a host, and is also how a host's redirect mode is
// changed: DeepHost has no UpdateDomain, so calling it again for a host already
// on this app flips Domain.spec.redirectTo and moves the host in or out of the
// router's map. A server without the field ignores it silently, which is why
// nothing here treats a successful call as proof the redirect took effect.
func (c *Client) CreateDomain(ctx context.Context, tenant, app string, spec engineapi.DomainSpec) error {
	_, err := call(ctx, c.unary.CreateDomain, &deephostv1.CreateDomainRequest{
		Tenant: tenant, App: app, Host: spec.Host,
		TlsSecret: spec.TLSSecret, RedirectTo: spec.RedirectTo,
	})
	return err
}

func (c *Client) ListDomains(ctx context.Context, tenant, app string) ([]engineapi.DomainView, error) {
	response, err := call(ctx, c.unary.ListDomains, &deephostv1.ListDomainsRequest{Tenant: tenant, App: app})
	if err != nil {
		return nil, err
	}
	domains := make([]engineapi.DomainView, 0, len(response.GetDomains()))
	for _, domain := range response.GetDomains() {
		domains = append(domains, engineapi.DomainView{
			Host:              domain.GetHost(),
			App:               domain.GetApp(),
			ReadinessReported: domain.GetReadinessReported(),
			Ready:             domain.GetReady(),
			TLSMode:           domain.GetTlsMode(),
			CertificateReady:  domain.GetCertificateReady(),
			Reason:            domain.GetReason(),
			RedirectTo:        domain.GetRedirectTo(),
		})
	}
	return domains, nil
}

func (c *Client) DeleteDomain(ctx context.Context, tenant, app, host string) error {
	_, err := call(ctx, c.unary.DeleteDomain, &deephostv1.DeleteDomainRequest{
		Tenant: tenant, App: app, Host: host,
	})
	return err
}

// SetAppEnvVar forwards one value once. The request struct is zeroed before
// returning, exactly as CreateGitBuild does with the per-deploy git token, and
// no error path formats the request.
func (c *Client) SetAppEnvVar(
	ctx context.Context,
	tenant, app string,
	in engineapi.EnvVarInput,
) (engineapi.EnvVarView, error) {
	message := &deephostv1.SetAppEnvVarRequest{
		Tenant:      tenant,
		App:         app,
		Name:        in.Name,
		Value:       in.Value,
		Environment: in.Environment,
	}
	defer func() { message.Value = "" }()

	response, err := call(ctx, c.unary.SetAppEnvVar, message)
	if err != nil {
		return engineapi.EnvVarView{}, err
	}
	return envVarView(response.GetVariable()), nil
}

func (c *Client) DeleteAppEnvVar(ctx context.Context, tenant, app, environment, name string) error {
	_, err := call(ctx, c.unary.DeleteAppEnvVar, &deephostv1.DeleteAppEnvVarRequest{
		Tenant: tenant, App: app, Name: name, Environment: environment,
	})
	return err
}

func (c *Client) ListAppEnvVars(
	ctx context.Context,
	tenant, app, environment string,
) ([]engineapi.EnvVarView, error) {
	response, err := call(ctx, c.unary.ListAppEnvVars, &deephostv1.ListAppEnvVarsRequest{
		Tenant: tenant, App: app, Environment: environment,
	})
	if err != nil {
		return nil, err
	}
	variables := make([]engineapi.EnvVarView, 0, len(response.GetVariables()))
	for _, variable := range response.GetVariables() {
		variables = append(variables, envVarView(variable))
	}
	return variables, nil
}

func envVarView(variable *deephostv1.EnvVarView) engineapi.EnvVarView {
	return engineapi.EnvVarView{
		Name:        variable.GetName(),
		Environment: variable.GetEnvironment(),
		LastSet:     unixMillis(variable.GetLastSetUnixMs()),
	}
}

func appView(app *deephostv1.AppView) engineapi.AppView {
	return engineapi.AppView{
		Name:          app.GetName(),
		Framework:     frameworkFromProto(app.GetFramework()),
		Domains:       append([]string(nil), app.GetDomains()...),
		LatestRelease: app.GetLatestRelease(),
		Ready:         app.GetReady(),
		Repository:    app.GetRepository(),
		Branch:        app.GetProductionBranch(),
	}
}

func buildView(build *deephostv1.BuildView) engineapi.BuildView {
	return engineapi.BuildView{
		ID:         build.GetId(),
		Phase:      build.GetPhase(),
		ReleaseID:  build.GetReleaseId(),
		Reason:     build.GetReason(),
		Revision:   build.GetRevision(),
		RepoURL:    build.GetRepoUrl(),
		CreatedAt:  unixMillis(build.GetCreatedUnixMs()),
		StartedAt:  unixMillis(build.GetStartedUnixMs()),
		FinishedAt: unixMillis(build.GetFinishedUnixMs()),
	}
}

// unixMillis converts an engine timestamp, keeping the zero value zero so the
// facade can tell "not reported" from "reported as the epoch".
func unixMillis(value int64) time.Time {
	if value <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(value).UTC()
}

func frameworkToProto(framework engineapi.Framework) deephostv1.Framework {
	switch framework {
	case engineapi.FrameworkStatic:
		return deephostv1.Framework_FRAMEWORK_STATIC
	case engineapi.FrameworkNode:
		return deephostv1.Framework_FRAMEWORK_NODE
	case engineapi.FrameworkNextJS:
		return deephostv1.Framework_FRAMEWORK_NEXTJS
	default:
		return deephostv1.Framework_FRAMEWORK_UNSPECIFIED
	}
}

func frameworkFromProto(framework deephostv1.Framework) engineapi.Framework {
	switch framework {
	case deephostv1.Framework_FRAMEWORK_NODE:
		return engineapi.FrameworkNode
	case deephostv1.Framework_FRAMEWORK_NEXTJS:
		return engineapi.FrameworkNextJS
	case deephostv1.Framework_FRAMEWORK_STATIC:
		return engineapi.FrameworkStatic
	default:
		return ""
	}
}

var _ engineapi.Engine = (*Client)(nil)
