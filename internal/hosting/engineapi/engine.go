// Package engineapi is the contract between the hosting facade and a hosting
// control plane: the view types, the Engine interface and the sentinel for an
// RPC a server does not implement.
//
// It is a leaf package for one reason. spec2 §1.9 puts the Engine interface in
// internal/hosting and its two implementations in internal/hosting/deephost
// and internal/hosting/fakehosting — but internal/hosting constructs both from
// config, so the three packages would import each other in a cycle. Splitting
// the contract out breaks it; internal/hosting re-exports every name here as an
// alias, so `hosting.Engine`, `hosting.BuildView` and the rest still read
// exactly as the spec writes them.
package engineapi

import (
	"context"
	"errors"
	"io"
	"time"
)

// Framework is the runtime DeepHost executes an artifact with. The three
// values map 1:1 onto deephost.v1.Framework and onto hosting.v1.Framework.
type Framework string

// The frameworks DeepHost's builder supports.
const (
	FrameworkStatic Framework = "static"
	FrameworkNode   Framework = "node"
	FrameworkNextJS Framework = "nextjs"
)

// Valid reports whether f is one of the three supported frameworks.
func (f Framework) Valid() bool {
	switch f {
	case FrameworkStatic, FrameworkNode, FrameworkNextJS:
		return true
	default:
		return false
	}
}

// AppSpec is what an app is created with. Domains are deliberately absent: the
// domain watcher registers hosts with CreateDomain, and DeepHost's CreateApp is
// an upsert that replaces the whole spec — sending an empty domains list on a
// resumed attach would wipe the router's host map for the app.
type AppSpec struct {
	Framework     Framework
	Repository    string
	Branch        string
	RootDirectory string
}

// AppPatch carries only the fields UpdateApp should change. A nil pointer means
// "leave as it is"; a pointer to "" clears the field.
type AppPatch struct {
	Repository    *string
	Branch        *string
	RootDirectory *string
}

// AppView is the engine's view of one app.
type AppView struct {
	Name          string
	Framework     Framework
	Domains       []string
	LatestRelease string
	Ready         bool
	Repository    string
	Branch        string
}

// GitBuildInput starts a build from a repository. GitToken lives only for the
// duration of the call: the client zeroes it afterwards and it never reaches a
// log, an error, an event or the platform store.
type GitBuildInput struct {
	Framework Framework
	RepoURL   string
	Revision  string
	Path      string
	GitToken  string
}

// BuildView is the engine's view of one build. The three timestamps are zero
// when the server does not report them (an old server, spec2 §2.3), and the
// facade then falls back to its own observations.
type BuildView struct {
	ID         string
	Phase      string
	ReleaseID  string
	Reason     string
	Revision   string
	RepoURL    string
	CreatedAt  time.Time
	StartedAt  time.Time
	FinishedAt time.Time
}

// Engine build phases, as DeepHost's Build CR reports them. An empty phase
// means the operator has not reconciled the Build yet.
const (
	BuildPhasePending   = "Pending"
	BuildPhaseBuilding  = "Building"
	BuildPhaseSucceeded = "Succeeded"
	BuildPhaseFailed    = "Failed"
)

// Terminal reports whether the engine will not move this build again.
func (b BuildView) Terminal() bool {
	return b.Phase == BuildPhaseSucceeded || b.Phase == BuildPhaseFailed
}

// ReleaseView is the engine's view of one release.
type ReleaseView struct {
	ID            string
	BuildID       string
	Ready         bool
	TrafficWeight int32
	GitSHA        string
	CreatedAt     time.Time
}

// DomainSpec is what a host is registered with. It is a struct rather than a
// parameter list because RedirectTo arrived after TLSSecret and a third bare
// string in a row is an argument-order bug waiting to happen.
type DomainSpec struct {
	// Host is the fully qualified name to register.
	Host string
	// TLSSecret names an existing secret covering the host (shared wildcard
	// mode). The facade only ever sets it for the platform's own auto
	// hostname; a customer host gets its own certificate.
	TLSSecret string
	// RedirectTo, when set, asks the engine to answer 301 to that host instead
	// of serving the app. An engine without the field ignores it, which is why
	// the facade reads DomainView.RedirectTo back rather than assuming.
	RedirectTo string
}

// DomainView is the engine's view of one registered host. ReadinessReported is
// false on a server that predates the readiness fields; every other field is
// then meaningless and the facade reports the host as REGISTERED.
type DomainView struct {
	Host              string
	App               string
	ReadinessReported bool
	Ready             bool
	TLSMode           string
	CertificateReady  bool
	Reason            string
	// RedirectTo is the host this one 301-redirects to. Empty means either
	// "this host serves the app" or "this server has no redirect field": the
	// two are indistinguishable on the wire, so the facade only claims a
	// redirect when it has seen one reported.
	RedirectTo string
}

// EnvVarView is one runtime environment variable. It deliberately has no value
// field: values are write-only, so there is nothing here a secret could travel
// back in.
type EnvVarView struct {
	Name        string
	Environment string
	// LastSet is zero when the engine has no record of when it was written.
	LastSet time.Time
}

// EnvVarInput carries one variable to the engine. Value lives only for the
// duration of the call, like GitBuildInput.GitToken: the client zeroes the
// request that carried it and it never reaches a log, an error, an event or the
// platform store.
type EnvVarInput struct {
	Environment string
	Name        string
	Value       string
}

// Environments a variable can belong to. An empty environment means production
// on every engine call.
const (
	EnvironmentProduction  = "production"
	EnvironmentPreview     = "preview"
	EnvironmentDevelopment = "development"
)

// TLS modes DeepHost reports for a registered host.
const (
	TLSModeIssued = "issued"
	TLSModeShared = "shared"
	TLSModeNone   = "none"
)

// ErrUnsupported is what an Engine returns for an RPC this server does not
// implement (Connect maps the 404 to CodeUnimplemented). The capability prober
// turns it into a persisted capability flag rather than an error the operator
// sees.
var ErrUnsupported = errors.New("hosting engine: unsupported by this server")

// Engine is everything the facade needs from a hosting control plane. Both the
// DeepHost client and the in-memory fake implement it; nothing else in this
// package knows which one it is talking to.
type Engine interface {
	// Health is the unauthenticated GET /healthz.
	Health(ctx context.Context) error

	// CreateApp is only ever called after GetApp returned NotFound: DeepHost's
	// CreateApp is an upsert that replaces the whole spec.
	CreateApp(ctx context.Context, tenant, app string, spec AppSpec) (appRef string, err error)
	UpdateApp(ctx context.Context, tenant, app string, patch AppPatch) error
	GetApp(ctx context.Context, tenant, app string) (AppView, error)
	ListApps(ctx context.Context, tenant string) ([]AppView, error)
	DeleteApp(ctx context.Context, tenant, app string) error

	CreateGitBuild(ctx context.Context, tenant, app string, in GitBuildInput) (buildID string, err error)
	// Deploy streams a tar.zst archive in 64 KiB frames over a transport with
	// no response-header timeout: DeepHost writes the response headers only
	// after spooling, hashing, the object-store put and the Build creation.
	Deploy(ctx context.Context, tenant, app string, framework Framework, archive io.Reader) (buildID string, err error)
	GetBuild(ctx context.Context, tenant, app, buildID string) (BuildView, error)
	// ListBuilds returns ErrUnsupported on a server without the RPC.
	ListBuilds(ctx context.Context, tenant, app string, limit int) ([]BuildView, error)
	GetBuildLogs(ctx context.Context, tenant, app, buildID string, tail int) (lines []string, complete bool, err error)

	ListReleases(ctx context.Context, tenant, app string) ([]ReleaseView, error)
	PromoteRelease(ctx context.Context, tenant, app, releaseID string) error

	// CreateDomain is also the update path: calling it again for a host that
	// is already registered on the same app changes its redirect mode.
	CreateDomain(ctx context.Context, tenant, app string, spec DomainSpec) error
	ListDomains(ctx context.Context, tenant, app string) ([]DomainView, error)
	// DeleteDomain returns ErrUnsupported on a server without the RPC.
	DeleteDomain(ctx context.Context, tenant, app, host string) error

	// The three environment-variable RPCs all return ErrUnsupported on a
	// server without them. SetAppEnvVar is the only way a value travels; no
	// other method accepts or returns one.
	SetAppEnvVar(ctx context.Context, tenant, app string, in EnvVarInput) (EnvVarView, error)
	DeleteAppEnvVar(ctx context.Context, tenant, app, environment, name string) error
	ListAppEnvVars(ctx context.Context, tenant, app, environment string) ([]EnvVarView, error)
}
