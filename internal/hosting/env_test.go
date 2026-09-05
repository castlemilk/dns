package hosting

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"connectrpc.com/connect"
	hostingv1 "github.com/castlemilk/dns/gen/go/hosting/v1"
	"github.com/castlemilk/dns/internal/activity"
)

// envSecret is a value-shaped string. Nothing in this package redacts it: the
// tests below pass only because the facade never writes it anywhere.
const envSecret = "postgres://user:s3cr3t-not-a-real-password@db.internal:5432/app"

func setEnvVar(
	t *testing.T,
	h *harness,
	zoneID, name, value, environment string,
) *hostingv1.SetSiteEnvVarResponse {
	t.Helper()
	response, err := h.service.SetSiteEnvVar(context.Background(), connect.NewRequest(&hostingv1.SetSiteEnvVarRequest{
		ZoneId: zoneID, Name: name, Value: value, Environment: environment,
	}))
	if err != nil {
		t.Fatalf("SetSiteEnvVar(%s): %v", name, err)
	}
	return response.Msg
}

func listEnvVars(t *testing.T, h *harness, zoneID, environment string) *hostingv1.ListSiteEnvVarsResponse {
	t.Helper()
	response, err := h.service.ListSiteEnvVars(context.Background(), connect.NewRequest(&hostingv1.ListSiteEnvVarsRequest{
		ZoneId: zoneID, Environment: environment,
	}))
	if err != nil {
		t.Fatalf("ListSiteEnvVars: %v", err)
	}
	return response.Msg
}

func TestEnvVarRoundTripReachesTheEngineAndListsWithoutValues(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	zoneID := h.zoneID(t, "acme.dev")
	h.seedGateway(t, "198.51.100.10")
	attach(t, h, zoneID)

	set := setEnvVar(t, h, zoneID, "DATABASE_URL", envSecret, "production")
	if set.GetVariable().GetName() != "DATABASE_URL" {
		t.Errorf("name = %q", set.GetVariable().GetName())
	}
	if set.GetVariable().GetEnvironment() != EnvironmentProduction {
		t.Errorf("environment = %q", set.GetVariable().GetEnvironment())
	}
	if set.GetVariable().GetLastSet() == nil {
		t.Error("last_set is unset; the engine stamped the write")
	}
	if set.GetNote() != CopyEnvRedeploy {
		t.Errorf("note = %q, want the redeploy copy", set.GetNote())
	}

	// The value reached the engine intact. EnvValue is a fake-only accessor;
	// no method on engineapi.Engine can read a value back.
	app := AppName("simple-test", "acme.dev")
	stored, ok := h.engine.EnvValue("simple-test", app, "production", "DATABASE_URL")
	if !ok || stored != envSecret {
		t.Fatalf("engine value = %q (%t), want the value as sent", stored, ok)
	}

	setEnvVar(t, h, zoneID, "DATABASE_URL", "preview-value", "preview")
	setEnvVar(t, h, zoneID, "FEATURE_FLAG", "on", "")

	list := listEnvVars(t, h, zoneID, "")
	if !list.GetSupported() {
		t.Fatal("supported = false against an engine that answered")
	}
	got := make([]string, 0, len(list.GetVariables()))
	for _, variable := range list.GetVariables() {
		got = append(got, variable.GetEnvironment()+"/"+variable.GetName())
		if variable.GetLastSet() == nil {
			t.Errorf("%s has no last_set", variable.GetName())
		}
	}
	want := []string{"preview/DATABASE_URL", "production/DATABASE_URL", "production/FEATURE_FLAG"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("variables = %v, want %v sorted by environment then name", got, want)
	}

	// An empty environment on the write defaulted to production.
	if _, ok := h.engine.EnvValue("simple-test", app, "production", "FEATURE_FLAG"); !ok {
		t.Error("an empty environment did not default to production")
	}

	filtered := listEnvVars(t, h, zoneID, "preview")
	if len(filtered.GetVariables()) != 1 || filtered.GetVariables()[0].GetName() != "DATABASE_URL" {
		t.Errorf("preview filter returned %d variables", len(filtered.GetVariables()))
	}
}

func TestEnvVarValueNeverReachesTheStoreLogsEventsOrTheResponse(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	zoneID := h.zoneID(t, "acme.dev")
	h.seedGateway(t, "198.51.100.10")
	attach(t, h, zoneID)

	response := setEnvVar(t, h, zoneID, "DATABASE_URL", envSecret, "production")
	if strings.Contains(response.String(), envSecret) {
		t.Error("the response carried the value")
	}
	if strings.Contains(listEnvVars(t, h, zoneID, "").String(), envSecret) {
		t.Error("the list carried the value")
	}
	if strings.Contains(h.logs.String(), envSecret) {
		t.Error("the value appears in the control-plane log")
	}

	found := false
	for _, event := range h.events.all() {
		if event.Kind != activity.KindSiteEnvVarSet {
			continue
		}
		found = true
		if strings.Contains(event.Summary, envSecret) {
			t.Errorf("the value appears in event summary %q", event.Summary)
		}
		if event.Details["name"] != "DATABASE_URL" || event.Details["environment"] != "production" {
			t.Errorf("event details = %v, want the name and the environment", event.Details)
		}
		for key, value := range event.Details {
			if strings.Contains(value, envSecret) {
				t.Errorf("the value appears in event detail %q", key)
			}
		}
	}
	if !found {
		t.Error("no site.env.set event was recorded")
	}

	for _, call := range h.engine.Calls() {
		for key, value := range call.Args {
			if strings.Contains(value, envSecret) {
				t.Errorf("the fake engine recorded the value in %q", key)
			}
		}
		if call.Method == "SetAppEnvVar" && call.Args["value"] != "true" {
			t.Errorf("SetAppEnvVar args = %v, want the boolean fact", call.Args)
		}
	}

	if err := h.store.Close(); err != nil {
		t.Fatalf("close platform store: %v", err)
	}
	raw, err := os.ReadFile(h.store.Path())
	if err != nil {
		t.Fatalf("read platform store: %v", err)
	}
	if bytes.Contains(raw, []byte(envSecret)) {
		t.Error("the value is present in platform.db")
	}
}

func TestDeleteSiteEnvVarRemovesOneVariableAndReportsAMissingOne(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	zoneID := h.zoneID(t, "acme.dev")
	h.seedGateway(t, "198.51.100.10")
	attach(t, h, zoneID)
	setEnvVar(t, h, zoneID, "DATABASE_URL", envSecret, "production")
	setEnvVar(t, h, zoneID, "DATABASE_URL", envSecret, "preview")

	response, err := h.service.DeleteSiteEnvVar(context.Background(), connect.NewRequest(&hostingv1.DeleteSiteEnvVarRequest{
		ZoneId: zoneID, Name: "DATABASE_URL", Environment: "production",
	}))
	if err != nil {
		t.Fatalf("DeleteSiteEnvVar: %v", err)
	}
	if response.Msg.GetNote() != CopyEnvRedeploy {
		t.Errorf("note = %q", response.Msg.GetNote())
	}
	if !h.events.has(activity.KindSiteEnvVarDeleted) {
		t.Error("no site.env.deleted event was recorded")
	}

	// Only that environment's copy went.
	list := listEnvVars(t, h, zoneID, "")
	if len(list.GetVariables()) != 1 || list.GetVariables()[0].GetEnvironment() != "preview" {
		t.Errorf("after the delete the engine holds %d variables", len(list.GetVariables()))
	}

	_, err = h.service.DeleteSiteEnvVar(context.Background(), connect.NewRequest(&hostingv1.DeleteSiteEnvVarRequest{
		ZoneId: zoneID, Name: "DATABASE_URL", Environment: "production",
	}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("a repeated delete = %s, want not_found", connect.CodeOf(err))
	}
	if strings.Contains(err.Error(), CopyAppGone) {
		t.Errorf("a missing variable was reported as a missing site: %v", err)
	}
}

func TestEnvVarInputsAreValidatedBeforeTheEngineIsCalled(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	zoneID := h.zoneID(t, "acme.dev")
	h.seedGateway(t, "198.51.100.10")
	attach(t, h, zoneID)

	tests := []struct {
		name        string
		variable    string
		value       string
		environment string
		want        connect.Code
	}{
		{name: "empty name", variable: "", want: connect.CodeInvalidArgument},
		{name: "leading digit", variable: "1DB", want: connect.CodeInvalidArgument},
		{name: "dash", variable: "DATABASE-URL", want: connect.CodeInvalidArgument},
		{name: "space", variable: "DATABASE URL", want: connect.CodeInvalidArgument},
		{name: "too long", variable: strings.Repeat("A", maxEnvNameLength+1), want: connect.CodeInvalidArgument},
		{name: "unknown environment", variable: "DB", environment: "staging", want: connect.CodeInvalidArgument},
		{name: "value too large", variable: "DB", value: strings.Repeat("a", maxEnvValueBytes+1), want: connect.CodeInvalidArgument},
		{name: "value with a NUL", variable: "DB", value: "a\x00b", want: connect.CodeInvalidArgument},
		{name: "value that is not utf-8", variable: "DB", value: "\xff\xfe", want: connect.CodeInvalidArgument},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			before := len(h.engine.Calls())
			_, err := h.service.SetSiteEnvVar(context.Background(), connect.NewRequest(&hostingv1.SetSiteEnvVarRequest{
				ZoneId: zoneID, Name: test.variable, Value: test.value, Environment: test.environment,
			}))
			if connect.CodeOf(err) != test.want {
				t.Fatalf("code = %s, want %s", connect.CodeOf(err), test.want)
			}
			if test.value != "" && strings.Contains(err.Error(), test.value) {
				t.Errorf("the error echoed the value: %v", err)
			}
			for _, call := range h.engine.Calls()[before:] {
				if call.Method == "SetAppEnvVar" {
					t.Error("a rejected input reached the engine")
				}
			}
		})
	}
}

func TestEnvVarsOnAnUnknownZoneAreRefused(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	zoneID := h.zoneID(t, "acme.dev")

	_, err := h.service.SetSiteEnvVar(context.Background(), connect.NewRequest(&hostingv1.SetSiteEnvVarRequest{
		ZoneId: zoneID, Name: "DB", Value: "x",
	}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("code = %s, want not_found for a zone with no site", connect.CodeOf(err))
	}
	_, err = h.service.ListSiteEnvVars(context.Background(), connect.NewRequest(&hostingv1.ListSiteEnvVarsRequest{
		ZoneId: "",
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("code = %s, want invalid_argument for an empty zone id", connect.CodeOf(err))
	}
}

func TestAnEngineWithoutTheEnvRPCsIsReportedAsACapabilityNotAFailure(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	zoneID := h.zoneID(t, "acme.dev")
	h.seedGateway(t, "198.51.100.10")
	attach(t, h, zoneID)
	h.engine.SetOldServer(true)

	list := listEnvVars(t, h, zoneID, "")
	if list.GetSupported() {
		t.Error("supported = true against a server without the RPCs")
	}
	if len(list.GetVariables()) != 0 {
		t.Errorf("variables = %d, want none", len(list.GetVariables()))
	}
	if list.GetNote() != CopyEnvUnsupported {
		t.Errorf("note = %q, want the version copy", list.GetNote())
	}
	if caps := h.service.capabilities(context.Background()); caps.EnvVars {
		t.Error("capability env_vars = true against a server without the RPCs")
	}

	_, err := h.service.SetSiteEnvVar(context.Background(), connect.NewRequest(&hostingv1.SetSiteEnvVarRequest{
		ZoneId: zoneID, Name: "DB", Value: "x",
	}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("code = %s, want failed_precondition", connect.CodeOf(err))
	}
	if !strings.Contains(err.Error(), CopyEnvUnsupported) {
		t.Errorf("error = %v, want the version copy", err)
	}
	_, err = h.service.DeleteSiteEnvVar(context.Background(), connect.NewRequest(&hostingv1.DeleteSiteEnvVarRequest{
		ZoneId: zoneID, Name: "DB",
	}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("delete code = %s, want failed_precondition", connect.CodeOf(err))
	}

	// The same facade against an engine that gains the RPCs reports the
	// capability without a restart.
	h.engine.SetOldServer(false)
	setEnvVar(t, h, zoneID, "DB", "x", "")
	if caps := h.service.capabilities(context.Background()); !caps.EnvVars {
		t.Error("capability env_vars stayed false after the engine answered")
	}
}

func TestEnvVarsAreRefusedWhenHostingIsNotConfigured(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.service.engine = nil

	_, err := h.service.ListSiteEnvVars(context.Background(), connect.NewRequest(&hostingv1.ListSiteEnvVarsRequest{
		ZoneId: "any",
	}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("code = %s, want failed_precondition", connect.CodeOf(err))
	}
	if !strings.Contains(err.Error(), NotConfigured) {
		t.Errorf("error = %v, want the not-configured copy", err)
	}
}

func TestTheCapabilityProberAnswersTheEnvRPCsFromAnAttachedSite(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	ctx := context.Background()

	// With no site there is nothing to ask against, and the prober says
	// nothing rather than guessing.
	h.service.probeCapabilities(ctx)
	if h.service.capabilities(ctx).EnvVars {
		t.Error("capability env_vars = true with no site attached")
	}

	zoneID := h.zoneID(t, "acme.dev")
	h.seedGateway(t, "198.51.100.10")
	attach(t, h, zoneID)

	h.service.probeCapabilities(ctx)
	if !h.service.capabilities(ctx).EnvVars {
		t.Error("capability env_vars = false against a server with the RPCs")
	}

	h.engine.SetOldServer(true)
	h.service.probeCapabilities(ctx)
	if h.service.capabilities(ctx).EnvVars {
		t.Error("capability env_vars stayed true against a server without the RPCs")
	}
}
