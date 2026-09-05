// Package hosting is the facade over the DeepHost control plane: it owns the
// site model (one site per zone), the deploy state machine with facade-owned
// timestamps, the DNS records a site needs, and the background workers that
// keep all three in step with the engine.
//
// The browser never talks to DeepHost. Every call goes through this package,
// which holds the engine's bearer token, writes DNS only through
// enginedns.ZoneMutator inside the zone's serializer, and records one activity
// event per state change.
package hosting

import "github.com/castlemilk/dns/internal/hosting/engineapi"

// The engine contract lives in internal/hosting/engineapi so the two Engine
// implementations can be sub-packages of this one without an import cycle (see
// that package's doc comment). Everything below is an alias, so the names read
// as spec2 §4.1 writes them: hosting.Engine, hosting.BuildView, and so on.
type (
	// Framework is the runtime DeepHost executes an artifact with.
	Framework = engineapi.Framework
	// AppSpec is what an app is created with; domains are never sent.
	AppSpec = engineapi.AppSpec
	// AppPatch carries only the fields UpdateApp should change.
	AppPatch = engineapi.AppPatch
	// AppView is the engine's view of one app.
	AppView = engineapi.AppView
	// GitBuildInput starts a build from a repository.
	GitBuildInput = engineapi.GitBuildInput
	// BuildView is the engine's view of one build.
	BuildView = engineapi.BuildView
	// ReleaseView is the engine's view of one release.
	ReleaseView = engineapi.ReleaseView
	// DomainView is the engine's view of one registered host.
	DomainView = engineapi.DomainView
	// DomainSpec is what a host is registered with.
	DomainSpec = engineapi.DomainSpec
	// EnvVarView is one runtime environment variable, without its value.
	EnvVarView = engineapi.EnvVarView
	// EnvVarInput carries one variable's value to the engine, once.
	EnvVarInput = engineapi.EnvVarInput
	// Engine is everything the facade needs from a hosting control plane.
	Engine = engineapi.Engine
)

// Frameworks, build phases and TLS modes, re-exported.
const (
	FrameworkStatic = engineapi.FrameworkStatic
	FrameworkNode   = engineapi.FrameworkNode
	FrameworkNextJS = engineapi.FrameworkNextJS

	BuildPhasePending   = engineapi.BuildPhasePending
	BuildPhaseBuilding  = engineapi.BuildPhaseBuilding
	BuildPhaseSucceeded = engineapi.BuildPhaseSucceeded
	BuildPhaseFailed    = engineapi.BuildPhaseFailed

	TLSModeIssued = engineapi.TLSModeIssued
	TLSModeShared = engineapi.TLSModeShared
	TLSModeNone   = engineapi.TLSModeNone

	EnvironmentProduction  = engineapi.EnvironmentProduction
	EnvironmentPreview     = engineapi.EnvironmentPreview
	EnvironmentDevelopment = engineapi.EnvironmentDevelopment
)

// ErrUnsupported is what an Engine returns for an RPC this server does not
// implement. The capability prober turns it into a persisted flag rather than
// an error an operator sees.
var ErrUnsupported = engineapi.ErrUnsupported
