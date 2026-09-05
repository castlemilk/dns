package hosting

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"connectrpc.com/connect"
	hostingv1 "github.com/castlemilk/dns/gen/go/hosting/v1"
	"github.com/castlemilk/dns/internal/activity"
	"github.com/castlemilk/dns/internal/platform"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Bounds on one variable. They are the hosting engine's own limits, checked
// here as well so a value that cannot be stored is refused before it is sent
// anywhere — and refused by a message that names the limit, never the value.
const (
	// maxEnvNameLength bounds a variable name. A Kubernetes Secret data key is
	// limited to 253 characters and carries the environment and a separator as
	// well, so 128 leaves room without being a limit anyone meets.
	maxEnvNameLength = 128
	// maxEnvValueBytes is DeepHost's own cap on one value.
	maxEnvValueBytes = 32 << 10
)

// Copy for the environment-variable surface. Every sentence names what is
// missing or what has to happen next; none of them can carry a value.
const (
	// CopyEnvUnsupported is what an engine without the RPCs produces. It is a
	// version statement, not a failure of the request.
	CopyEnvUnsupported = "This hosting engine version cannot store environment variables. " +
		"Upgrade the hosting engine to set them from here."
	// CopyEnvRedeploy is the honesty rule applied to a value that has been
	// written: the running site is unchanged until it is built again.
	CopyEnvRedeploy = "The running site keeps the values it started with. Deploy again for this change to take effect."
	// CopyEnvNoApp is the refusal for a site whose engine app is not known yet.
	CopyEnvNoApp = "This site has no app on the hosting engine yet."
)

// environments is every environment a variable may belong to, in the order the
// console shows them.
var environments = []string{EnvironmentProduction, EnvironmentPreview, EnvironmentDevelopment}

// SetSiteEnvVar writes one variable. The value is forwarded to the engine and
// then dropped: it is never stored in platform.db, never logged, never returned
// and never placed in an activity detail or a metric attribute. The event this
// records names the variable and the environment, which is the whole of what
// the facade knows about it afterwards.
func (s *Service) SetSiteEnvVar(
	ctx context.Context,
	request *connect.Request[hostingv1.SetSiteEnvVarRequest],
) (*connect.Response[hostingv1.SetSiteEnvVarResponse], error) {
	if !s.configured() {
		return nil, notConfigured()
	}
	doc, err := s.envSite(ctx, request.Msg.GetZoneId())
	if err != nil {
		return nil, err
	}
	name, err := validEnvName(request.Msg.GetName())
	if err != nil {
		return nil, err
	}
	environment, err := validEnvironment(request.Msg.GetEnvironment())
	if err != nil {
		return nil, err
	}
	if err := validEnvValue(request.Msg.GetValue()); err != nil {
		return nil, err
	}

	view, err := s.engine.SetAppEnvVar(ctx, s.cfg.Tenant, doc.App, EnvVarInput{
		Environment: environment,
		Name:        name,
		Value:       request.Msg.GetValue(),
	})
	if err != nil {
		return nil, s.envError(ctx, err)
	}
	s.noteEnvSupported(ctx)

	s.deps.Record(ctx, activity.Event{
		ZoneID: doc.ZoneID, ZoneName: doc.ZoneName, Actor: activity.ActorOperator,
		Kind: activity.KindSiteEnvVarSet, Severity: activity.SeverityInfo,
		Summary: fmt.Sprintf("Set the %s environment variable %s for %s", environment, name, doc.ZoneName),
		Details: map[string]string{"name": name, "environment": environment},
	})
	return connect.NewResponse(&hostingv1.SetSiteEnvVarResponse{
		Variable: envVarProto(view, name, environment),
		Note:     CopyEnvRedeploy,
	}), nil
}

// DeleteSiteEnvVar removes one variable.
func (s *Service) DeleteSiteEnvVar(
	ctx context.Context,
	request *connect.Request[hostingv1.DeleteSiteEnvVarRequest],
) (*connect.Response[hostingv1.DeleteSiteEnvVarResponse], error) {
	if !s.configured() {
		return nil, notConfigured()
	}
	doc, err := s.envSite(ctx, request.Msg.GetZoneId())
	if err != nil {
		return nil, err
	}
	name, err := validEnvName(request.Msg.GetName())
	if err != nil {
		return nil, err
	}
	environment, err := validEnvironment(request.Msg.GetEnvironment())
	if err != nil {
		return nil, err
	}

	if err := s.engine.DeleteAppEnvVar(ctx, s.cfg.Tenant, doc.App, environment, name); err != nil {
		if isNotFound(err) {
			// NotFound here is about the variable, not the app, so the engine's
			// "the site is gone" copy would be wrong.
			return nil, notFound(fmt.Sprintf("%s is not set for %s", name, environment))
		}
		return nil, s.envError(ctx, err)
	}
	s.noteEnvSupported(ctx)

	s.deps.Record(ctx, activity.Event{
		ZoneID: doc.ZoneID, ZoneName: doc.ZoneName, Actor: activity.ActorOperator,
		Kind: activity.KindSiteEnvVarDeleted, Severity: activity.SeverityInfo,
		Summary: fmt.Sprintf("Removed the %s environment variable %s from %s", environment, name, doc.ZoneName),
		Details: map[string]string{"name": name, "environment": environment},
	})
	return connect.NewResponse(&hostingv1.DeleteSiteEnvVarResponse{Note: CopyEnvRedeploy}), nil
}

// ListSiteEnvVars returns names, environments and last-set stamps. An engine
// without the RPCs answers supported=false with the empty list, so the console
// can say "this engine cannot report them" instead of "there are none".
func (s *Service) ListSiteEnvVars(
	ctx context.Context,
	request *connect.Request[hostingv1.ListSiteEnvVarsRequest],
) (*connect.Response[hostingv1.ListSiteEnvVarsResponse], error) {
	if !s.configured() {
		return nil, notConfigured()
	}
	doc, err := s.envSite(ctx, request.Msg.GetZoneId())
	if err != nil {
		return nil, err
	}
	environment := strings.TrimSpace(request.Msg.GetEnvironment())
	if environment != "" {
		if environment, err = validEnvironment(environment); err != nil {
			return nil, err
		}
	}

	views, err := s.engine.ListAppEnvVars(ctx, s.cfg.Tenant, doc.App, environment)
	if err != nil {
		if isUnsupported(err) {
			s.noteEnvUnsupported(ctx)
			return connect.NewResponse(&hostingv1.ListSiteEnvVarsResponse{
				Supported: false,
				Note:      CopyEnvUnsupported,
			}), nil
		}
		return nil, s.mapEngineError(err)
	}
	s.noteEnvSupported(ctx)

	response := &hostingv1.ListSiteEnvVarsResponse{
		Variables: make([]*hostingv1.EnvVar, 0, len(views)),
		Supported: true,
	}
	for _, view := range views {
		response.Variables = append(response.Variables, envVarProto(view, view.Name, view.Environment))
	}
	if len(response.Variables) > 0 {
		response.Note = CopyEnvRedeploy
	}
	return connect.NewResponse(response), nil
}

// envSite reads the site a variable belongs to and refuses one whose engine app
// is not known, which is the only state in which the engine has nowhere to put
// the value.
func (s *Service) envSite(ctx context.Context, zoneID string) (platform.SiteDoc, error) {
	doc, err := s.site(ctx, zoneID)
	if err != nil {
		return platform.SiteDoc{}, err
	}
	if doc.App == "" {
		return platform.SiteDoc{}, failedPrecondition(CopyEnvNoApp)
	}
	return doc, nil
}

// envError maps an engine failure. Unimplemented is the one code that is a
// statement about the server rather than the request, so it is answered with
// the version copy and remembered as a capability.
func (s *Service) envError(ctx context.Context, err error) error {
	if isUnsupported(err) {
		s.noteEnvUnsupported(ctx)
		return failedPrecondition(CopyEnvUnsupported)
	}
	return s.mapEngineError(err)
}

func (s *Service) noteEnvSupported(ctx context.Context) {
	s.updateCapabilities(ctx, func(caps *platform.CapabilitiesDoc) { caps.EnvVars = true })
}

func (s *Service) noteEnvUnsupported(ctx context.Context) {
	s.updateCapabilities(ctx, func(caps *platform.CapabilitiesDoc) { caps.EnvVars = false })
}

// envVarProto renders one variable. name and environment come from the request
// when the engine echoes neither, so the console always has something to key a
// row on.
func envVarProto(view EnvVarView, name, environment string) *hostingv1.EnvVar {
	variable := &hostingv1.EnvVar{Name: orDefault(view.Name, name), Environment: orDefault(view.Environment, environment)}
	if !view.LastSet.IsZero() {
		variable.LastSet = timestamppb.New(view.LastSet.UTC())
	}
	return variable
}

// validEnvName applies the process environment variable rule. It is stricter
// than POSIX allows in one way — a leading digit is refused — because a shell
// cannot export such a name and a site would never see it.
func validEnvName(value string) (string, error) {
	name := strings.TrimSpace(value)
	if name == "" {
		return "", invalidArgument("name", "is required")
	}
	if len(name) > maxEnvNameLength {
		return "", invalidArgument("name", fmt.Sprintf("must be at most %d characters", maxEnvNameLength))
	}
	for index, char := range name {
		switch {
		case char >= 'A' && char <= 'Z', char >= 'a' && char <= 'z', char == '_':
		case char >= '0' && char <= '9' && index > 0:
		default:
			return "", invalidArgument("name",
				"may contain only letters, digits and '_', and may not start with a digit")
		}
	}
	return name, nil
}

// validEnvironment defaults an empty environment to production and refuses
// anything outside the three the console offers.
func validEnvironment(value string) (string, error) {
	environment := strings.ToLower(strings.TrimSpace(value))
	if environment == "" {
		return EnvironmentProduction, nil
	}
	if !slices.Contains(environments, environment) {
		return "", invalidArgument("environment", "must be production, preview or development")
	}
	return environment, nil
}

// validEnvValue bounds the value. Neither branch puts any part of it in the
// error: the message names the rule and the limit only.
func validEnvValue(value string) error {
	if len(value) > maxEnvValueBytes {
		return invalidArgument("value", fmt.Sprintf("must be at most %d bytes", maxEnvValueBytes))
	}
	if !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
		return invalidArgument("value", "must be valid UTF-8 text with no NUL byte")
	}
	return nil
}
