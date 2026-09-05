package deephost

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"
	deephostv1 "github.com/castlemilk/dns/gen/go/deephost/v1"
	"github.com/castlemilk/dns/gen/go/deephost/v1/deephostv1connect"
	"github.com/castlemilk/dns/internal/hosting/engineapi"
)

// stubServer implements only the RPCs a test drives; everything else answers
// CodeUnimplemented, which is exactly what an older DeepHost does.
type stubServer struct {
	deephostv1connect.UnimplementedControlPlaneServiceHandler

	mu            sync.Mutex
	authorization []string
	gitToken      string
	deployBytes   []byte
	deployFrames  int
	failCreateApp error
}

func (s *stubServer) CreateGitBuild(
	_ context.Context,
	request *connect.Request[deephostv1.CreateGitBuildRequest],
) (*connect.Response[deephostv1.CreateGitBuildResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.authorization = append(s.authorization, request.Header().Get("Authorization"))
	s.gitToken = request.Msg.GetGitToken()
	return connect.NewResponse(&deephostv1.CreateGitBuildResponse{BuildId: "b-abc123"}), nil
}

func (s *stubServer) CreateApp(
	_ context.Context,
	_ *connect.Request[deephostv1.CreateAppRequest],
) (*connect.Response[deephostv1.CreateAppResponse], error) {
	if s.failCreateApp != nil {
		return nil, s.failCreateApp
	}
	return connect.NewResponse(&deephostv1.CreateAppResponse{Tenant: "simple", Name: "simple-acme-dev"}), nil
}

func (s *stubServer) GetBuild(
	_ context.Context,
	_ *connect.Request[deephostv1.GetBuildRequest],
) (*connect.Response[deephostv1.GetBuildResponse], error) {
	return connect.NewResponse(&deephostv1.GetBuildResponse{Build: &deephostv1.BuildView{
		Id:             "b-abc123",
		Phase:          "Succeeded",
		ReleaseId:      "rel-b-abc123",
		CreatedUnixMs:  1_700_000_000_000,
		StartedUnixMs:  1_700_000_001_000,
		FinishedUnixMs: 1_700_000_039_000,
		Revision:       "main",
		RepoUrl:        "https://github.com/owner/name",
	}}), nil
}

func (s *stubServer) ListDomains(
	_ context.Context,
	_ *connect.Request[deephostv1.ListDomainsRequest],
) (*connect.Response[deephostv1.ListDomainsResponse], error) {
	return connect.NewResponse(&deephostv1.ListDomainsResponse{Domains: []*deephostv1.DomainView{{
		Host: "acme.dev", App: "simple-acme-dev", ReadinessReported: true, Ready: true,
		TlsMode: "issued", CertificateReady: false, Reason: "certificate not created yet",
	}}}), nil
}

func (s *stubServer) Deploy(
	_ context.Context,
	stream *connect.ClientStream[deephostv1.DeployChunk],
) (*connect.Response[deephostv1.DeployResponse], error) {
	body := &bytes.Buffer{}
	frames := 0
	for stream.Receive() {
		chunk := stream.Msg()
		if data := chunk.GetData(); len(data) > 0 {
			frames++
			body.Write(data)
		}
	}
	if err := stream.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.deployBytes = body.Bytes()
	s.deployFrames = frames
	s.mu.Unlock()
	return connect.NewResponse(&deephostv1.DeployResponse{BuildId: "b-upload1"}), nil
}

func newStubClient(t *testing.T, server deephostv1connect.ControlPlaneServiceHandler) *Client {
	t.Helper()
	mux := http.NewServeMux()
	path, handler := deephostv1connect.NewControlPlaneServiceHandler(server)
	mux.Handle(path, handler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	httpServer := httptest.NewServer(mux)
	t.Cleanup(httpServer.Close)
	return New(httpServer.URL, "test-token", WithHTTPClient(httpServer.Client()))
}

func TestClientSendsTheBearerOnEveryCallAndKeepsTheGitTokenInTheBody(t *testing.T) {
	t.Parallel()

	server := &stubServer{}
	client := newStubClient(t, server)

	buildID, err := client.CreateGitBuild(context.Background(), "simple", "acme-dev", engineapi.GitBuildInput{
		Framework: engineapi.FrameworkStatic,
		RepoURL:   "https://github.com/owner/name",
		Revision:  "main",
		GitToken:  "ghp_" + strings.Repeat("x", 30),
	})
	if err != nil {
		t.Fatalf("CreateGitBuild: %v", err)
	}
	if buildID != "b-abc123" {
		t.Errorf("build id = %q", buildID)
	}

	server.mu.Lock()
	defer server.mu.Unlock()
	if len(server.authorization) != 1 || server.authorization[0] != "Bearer test-token" {
		t.Errorf("Authorization = %v, want the engine token from the transport", server.authorization)
	}
	if !strings.HasPrefix(server.gitToken, "ghp_") {
		t.Errorf("git_token did not reach the engine: %q", server.gitToken)
	}
}

func TestClientMapsUnimplementedOntoErrUnsupported(t *testing.T) {
	t.Parallel()

	client := newStubClient(t, &stubServer{})
	if _, err := client.ListBuilds(context.Background(), "simple", "acme-dev", 5); !errors.Is(err, engineapi.ErrUnsupported) {
		t.Fatalf("ListBuilds error = %v, want ErrUnsupported", err)
	}
	if err := client.DeleteDomain(context.Background(), "simple", "acme-dev", "acme.dev"); !errors.Is(err, engineapi.ErrUnsupported) {
		t.Fatalf("DeleteDomain error = %v, want ErrUnsupported", err)
	}
}

func TestClientReadsBuildTimestampsAndDomainReadiness(t *testing.T) {
	t.Parallel()

	client := newStubClient(t, &stubServer{})
	build, err := client.GetBuild(context.Background(), "simple", "acme-dev", "b-abc123")
	if err != nil {
		t.Fatalf("GetBuild: %v", err)
	}
	if build.CreatedAt.IsZero() || build.StartedAt.IsZero() || build.FinishedAt.IsZero() {
		t.Fatalf("build timestamps = %+v", build)
	}
	if got := build.FinishedAt.Sub(build.StartedAt).Seconds(); got != 38 {
		t.Errorf("build duration = %v seconds, want 38", got)
	}

	domains, err := client.ListDomains(context.Background(), "simple", "acme-dev")
	if err != nil {
		t.Fatalf("ListDomains: %v", err)
	}
	if len(domains) != 1 || !domains[0].ReadinessReported || domains[0].CertificateReady {
		t.Fatalf("domains = %+v", domains)
	}
	if domains[0].Reason != "certificate not created yet" {
		t.Errorf("reason = %q", domains[0].Reason)
	}
}

func TestClientStreamsDeployIn64KiBFrames(t *testing.T) {
	t.Parallel()

	server := &stubServer{}
	client := newStubClient(t, server)

	payload := bytes.Repeat([]byte("a"), (64<<10)+7)
	buildID, err := client.Deploy(context.Background(), "simple", "acme-dev",
		engineapi.FrameworkStatic, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if buildID != "b-upload1" {
		t.Errorf("build id = %q", buildID)
	}

	server.mu.Lock()
	defer server.mu.Unlock()
	if !bytes.Equal(server.deployBytes, payload) {
		t.Errorf("the engine received %d bytes, want %d", len(server.deployBytes), len(payload))
	}
	if server.deployFrames != 2 {
		t.Errorf("frames = %d, want 2 for a payload just over 64 KiB", server.deployFrames)
	}
}

func TestClientHealthChecksTheUnauthenticatedRoute(t *testing.T) {
	t.Parallel()

	client := newStubClient(t, &stubServer{})
	if err := client.Health(context.Background()); err != nil {
		t.Fatalf("Health: %v", err)
	}
}

func TestClientErrorsCarryNoRequestBody(t *testing.T) {
	t.Parallel()

	server := &stubServer{failCreateApp: connect.NewError(connect.CodeAlreadyExists,
		errors.New(`app "other-tenant-site" belongs to another tenant or app`))}
	client := newStubClient(t, server)

	_, err := client.CreateApp(context.Background(), "simple", "acme-dev", engineapi.AppSpec{
		Framework: engineapi.FrameworkStatic, Repository: "https://github.com/owner/name",
	})
	if connect.CodeOf(err) != connect.CodeAlreadyExists {
		t.Fatalf("code = %s, want already_exists", connect.CodeOf(err))
	}
	if strings.Contains(err.Error(), "test-token") {
		t.Errorf("the error carried the engine token: %v", err)
	}
}

// envServer implements the three environment-variable RPCs plus the two
// redirect-carrying ones, so the wire mapping is exercised against a real
// Connect handler rather than a hand-written struct.
type envServer struct {
	deephostv1connect.UnimplementedControlPlaneServiceHandler

	mu           sync.Mutex
	setRequests  []*deephostv1.SetAppEnvVarRequest
	listFilter   string
	deleted      []string
	domainSpecs  []*deephostv1.CreateDomainRequest
	domainReport string
	releaseSHA   string
}

func (s *envServer) SetAppEnvVar(
	_ context.Context,
	request *connect.Request[deephostv1.SetAppEnvVarRequest],
) (*connect.Response[deephostv1.SetAppEnvVarResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setRequests = append(s.setRequests, request.Msg)
	return connect.NewResponse(&deephostv1.SetAppEnvVarResponse{
		Variable: &deephostv1.EnvVarView{
			Name:          request.Msg.GetName(),
			Environment:   "production",
			LastSetUnixMs: 1772000000000,
		},
	}), nil
}

func (s *envServer) DeleteAppEnvVar(
	_ context.Context,
	request *connect.Request[deephostv1.DeleteAppEnvVarRequest],
) (*connect.Response[deephostv1.DeleteAppEnvVarResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleted = append(s.deleted, request.Msg.GetEnvironment()+"/"+request.Msg.GetName())
	return connect.NewResponse(&deephostv1.DeleteAppEnvVarResponse{}), nil
}

func (s *envServer) ListAppEnvVars(
	_ context.Context,
	request *connect.Request[deephostv1.ListAppEnvVarsRequest],
) (*connect.Response[deephostv1.ListAppEnvVarsResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listFilter = request.Msg.GetEnvironment()
	return connect.NewResponse(&deephostv1.ListAppEnvVarsResponse{
		Variables: []*deephostv1.EnvVarView{
			{Name: "DATABASE_URL", Environment: "production", LastSetUnixMs: 1772000000000},
			{Name: "FEATURE_FLAG", Environment: "preview"},
		},
	}), nil
}

func (s *envServer) CreateDomain(
	_ context.Context,
	request *connect.Request[deephostv1.CreateDomainRequest],
) (*connect.Response[deephostv1.CreateDomainResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.domainSpecs = append(s.domainSpecs, request.Msg)
	return connect.NewResponse(&deephostv1.CreateDomainResponse{}), nil
}

func (s *envServer) ListDomains(
	_ context.Context,
	_ *connect.Request[deephostv1.ListDomainsRequest],
) (*connect.Response[deephostv1.ListDomainsResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return connect.NewResponse(&deephostv1.ListDomainsResponse{Domains: []*deephostv1.DomainView{{
		Host: "www.acme.dev", App: "simple-acme-dev", ReadinessReported: true, Ready: true,
		TlsMode: "issued", CertificateReady: true, RedirectTo: s.domainReport,
	}}}), nil
}

func (s *envServer) ListReleases(
	_ context.Context,
	_ *connect.Request[deephostv1.ListReleasesRequest],
) (*connect.Response[deephostv1.ListReleasesResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return connect.NewResponse(&deephostv1.ListReleasesResponse{Releases: []*deephostv1.ReleaseView{{
		Id: "rel-b-abc123", BuildId: "b-abc123", Ready: true, TrafficWeight: 100, GitSha: s.releaseSHA,
	}}}), nil
}

func TestClientCarriesEnvVarsInBothDirectionsWithoutAValueComingBack(t *testing.T) {
	t.Parallel()

	server := &envServer{}
	client := newStubClient(t, server)
	ctx := context.Background()

	view, err := client.SetAppEnvVar(ctx, "simple", "acme-dev", engineapi.EnvVarInput{
		Environment: "production", Name: "DATABASE_URL", Value: "postgres://user:pw@db/app",
	})
	if err != nil {
		t.Fatalf("SetAppEnvVar: %v", err)
	}
	if view.Name != "DATABASE_URL" || view.Environment != "production" {
		t.Errorf("view = %+v", view)
	}
	if view.LastSet.IsZero() {
		t.Error("last set is zero; the engine reported a stamp")
	}

	server.mu.Lock()
	sent := server.setRequests[0]
	server.mu.Unlock()
	if sent.GetValue() != "postgres://user:pw@db/app" {
		t.Errorf("the value did not reach the engine intact: %q", sent.GetValue())
	}
	// EnvVarView has no value field at all, so there is nothing to assert
	// beyond the fact that the returned struct cannot hold one.

	variables, err := client.ListAppEnvVars(ctx, "simple", "acme-dev", "preview")
	if err != nil {
		t.Fatalf("ListAppEnvVars: %v", err)
	}
	if len(variables) != 2 {
		t.Fatalf("variables = %d, want 2", len(variables))
	}
	if variables[1].Name != "FEATURE_FLAG" || !variables[1].LastSet.IsZero() {
		t.Errorf("an unstamped variable = %+v, want a zero last set", variables[1])
	}
	server.mu.Lock()
	filter := server.listFilter
	server.mu.Unlock()
	if filter != "preview" {
		t.Errorf("environment filter = %q", filter)
	}

	if err := client.DeleteAppEnvVar(ctx, "simple", "acme-dev", "preview", "FEATURE_FLAG"); err != nil {
		t.Fatalf("DeleteAppEnvVar: %v", err)
	}
	server.mu.Lock()
	deleted := server.deleted
	server.mu.Unlock()
	if len(deleted) != 1 || deleted[0] != "preview/FEATURE_FLAG" {
		t.Errorf("deleted = %v", deleted)
	}
}

func TestClientMapsUnimplementedEnvRPCsOntoErrUnsupported(t *testing.T) {
	t.Parallel()

	client := newStubClient(t, &stubServer{})
	ctx := context.Background()

	if _, err := client.SetAppEnvVar(ctx, "simple", "acme-dev", engineapi.EnvVarInput{
		Name: "DB", Value: "x",
	}); !errors.Is(err, engineapi.ErrUnsupported) {
		t.Errorf("SetAppEnvVar error = %v, want ErrUnsupported", err)
	}
	if _, err := client.ListAppEnvVars(ctx, "simple", "acme-dev", ""); !errors.Is(err, engineapi.ErrUnsupported) {
		t.Errorf("ListAppEnvVars error = %v, want ErrUnsupported", err)
	}
	if err := client.DeleteAppEnvVar(ctx, "simple", "acme-dev", "", "DB"); !errors.Is(err, engineapi.ErrUnsupported) {
		t.Errorf("DeleteAppEnvVar error = %v, want ErrUnsupported", err)
	}
}

func TestClientSendsAndReadsTheDomainRedirect(t *testing.T) {
	t.Parallel()

	server := &envServer{domainReport: "acme.dev"}
	client := newStubClient(t, server)
	ctx := context.Background()

	err := client.CreateDomain(ctx, "simple", "acme-dev", engineapi.DomainSpec{
		Host: "www.acme.dev", RedirectTo: "acme.dev",
	})
	if err != nil {
		t.Fatalf("CreateDomain: %v", err)
	}
	server.mu.Lock()
	sent := server.domainSpecs[0]
	server.mu.Unlock()
	if sent.GetRedirectTo() != "acme.dev" || sent.GetHost() != "www.acme.dev" {
		t.Errorf("request = %+v", sent)
	}

	domains, err := client.ListDomains(ctx, "simple", "acme-dev")
	if err != nil {
		t.Fatalf("ListDomains: %v", err)
	}
	if len(domains) != 1 || domains[0].RedirectTo != "acme.dev" {
		t.Errorf("domains = %+v, want the reported redirect", domains)
	}
}

func TestClientReadsTheResolvedCommitFromAReleaseAndAcceptsNone(t *testing.T) {
	t.Parallel()

	sha := "4f2c1a9b3d5e7f0a1b2c3d4e5f60718293a4b5c6"
	server := &envServer{releaseSHA: sha}
	releases, err := newStubClient(t, server).ListReleases(context.Background(), "simple", "acme-dev")
	if err != nil {
		t.Fatalf("ListReleases: %v", err)
	}
	if len(releases) != 1 || releases[0].GitSHA != sha {
		t.Errorf("releases = %+v, want the engine's commit", releases)
	}

	// An engine that reports none leaves the field empty rather than guessing.
	releases, err = newStubClient(t, &envServer{}).ListReleases(context.Background(), "simple", "acme-dev")
	if err != nil {
		t.Fatalf("ListReleases: %v", err)
	}
	if len(releases) != 1 || releases[0].GitSHA != "" {
		t.Errorf("releases = %+v, want no commit", releases)
	}
}
