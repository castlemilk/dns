package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/castlemilk/dns/gen/go/activity/v1/activityv1connect"
	"github.com/castlemilk/dns/gen/go/billing/v1/billingv1connect"
	"github.com/castlemilk/dns/gen/go/dns/v1/dnsv1connect"
	"github.com/castlemilk/dns/gen/go/hosting/v1/hostingv1connect"
	"github.com/castlemilk/dns/gen/go/mail/v1/mailv1connect"
	"github.com/castlemilk/dns/gen/go/platform/v1/platformv1connect"
	"github.com/castlemilk/dns/internal/activity"
	"github.com/castlemilk/dns/internal/billing"
	"github.com/castlemilk/dns/internal/config"
	"github.com/castlemilk/dns/internal/control"
	"github.com/castlemilk/dns/internal/enginedns"
	"github.com/castlemilk/dns/internal/hosting"
	"github.com/castlemilk/dns/internal/mail"
	"github.com/castlemilk/dns/internal/platform"
	"github.com/castlemilk/dns/internal/telemetry"
	"github.com/castlemilk/dns/internal/zone"
)

// This file is the end-to-end test the specification's verification bar asks
// for: the CLI driven against a control plane started in process — the real
// zone store, the real control handler, the real platform status service and
// the real hosting, mail and billing facades — with no engine configured.
//
// Everything else in this package runs against the in-memory fake in
// fake_test.go, which is the right tool for shaping answers. This one exists
// for the opposite reason: nothing here is shaped by the test, so the honest
// "not configured" output and the variable names in it are the ones an
// operator actually sees.
//
// It still opens no socket beyond loopback and writes nothing outside
// t.TempDir().

// inProcessPlane is a control plane assembled from the production packages.
type inProcessPlane struct {
	url   string
	token string
}

// startInProcessPlane wires the writer role the way internal/app does, minus
// the DNS listener, the snapshot feed and the raw routes the CLI never calls.
// cfg carries no engine block at all, so hosting, mail and billing are
// unconfigured and each reports the variables it is waiting for.
func startInProcessPlane(t *testing.T) inProcessPlane {
	t.Helper()

	dir := t.TempDir()
	cfg := config.Config{
		Role:         config.RoleAll,
		DataPath:     filepath.Join(dir, "dns.db"),
		PlatformPath: filepath.Join(dir, "platform.db"),
		Nameservers:  []string{"ns1.simple.test", "ns2.simple.test"},
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	metrics := telemetry.Disabled()

	zoneStore, err := zone.Open(cfg.DataPath, cfg.Nameservers, zone.WithMetrics(metrics))
	if err != nil {
		t.Fatalf("open zone store: %v", err)
	}
	t.Cleanup(func() {
		if err := zoneStore.Close(); err != nil {
			t.Errorf("close zone store: %v", err)
		}
	})

	platformStore, err := platform.Open(cfg.PlatformPath, platform.WithMetrics(metrics))
	if err != nil {
		t.Fatalf("open platform store: %v", err)
	}
	t.Cleanup(func() {
		if err := platformStore.Close(); err != nil {
			t.Errorf("close platform store: %v", err)
		}
	})

	activityLog := activity.NewLog(platformStore, logger, metrics)
	controlHandler := control.NewHandler(
		zoneStore, nil, logger, time.Now().UTC(), metrics,
		control.WithRecorder(activityLog),
		control.WithBindingChecker(platformStore),
	)
	t.Cleanup(controlHandler.WaitForObservers)

	deps := platform.Deps{
		Store:    platformStore,
		Zones:    controlHandler.ZoneMutator(),
		Serial:   enginedns.NewSerializer(),
		Recorder: activityLog,
		Logger:   logger,
		Metrics:  metrics,
	}
	hostingService, _ := hosting.New(cfg.Hosting, deps)
	mailService := mail.New(cfg.Mail, deps)
	billingService, _ := billing.New(cfg.Billing, deps)
	statusService := platform.NewStatusService(cfg, deps, []platform.Probe{
		platform.NewDNSProbe(cfg, deps),
		hostingService,
		mailService,
		billingService,
	}, nil)
	controlHandler.AddZoneObserver(hostingService.Observer())
	controlHandler.AddZoneObserver(mailService.Observer())

	// One probe pass before serving, because the app runs the prober as a
	// worker and `simple status` without --probe reads the last result.
	statusService.Prober().ProbeAll(context.Background(), false)

	const token = "in-process-operator-token"
	mux := http.NewServeMux()
	// Each generated constructor returns (path, handler), so it is passed
	// straight through; every service goes behind the same operator bearer.
	mount := func(path string, handler http.Handler) {
		mux.Handle(path, bearer(token, handler))
	}
	mount(dnsv1connect.NewDNSServiceHandler(controlHandler))
	mount(platformv1connect.NewPlatformServiceHandler(statusService))
	mount(hostingv1connect.NewHostingServiceHandler(hostingService))
	mount(mailv1connect.NewMailServiceHandler(mailService))
	mount(billingv1connect.NewBillingServiceHandler(billingService))
	mount(activityv1connect.NewActivityServiceHandler(activityLog))

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return inProcessPlane{url: server.URL, token: token}
}

func bearer(token string, next http.Handler) http.Handler {
	expected := sha256.Sum256([]byte("Bearer " + token))
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		got := sha256.Sum256([]byte(request.Header.Get("Authorization")))
		if subtle.ConstantTimeCompare(expected[:], got[:]) != 1 {
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusUnauthorized)
			ignore(writeLine(writer, `{"code":"unauthenticated","message":"operator token required"}`))
			return
		}
		next.ServeHTTP(writer, request)
	})
}

// TestInProcessStatusIsHonestAboutUnconfiguredEngines is the assertion the
// specification names: against a real control plane with no engine configured,
// the CLI reports each engine as not configured and names the variables it is
// waiting for, and names no value.
func TestInProcessStatusIsHonestAboutUnconfiguredEngines(t *testing.T) {
	plane := startInProcessPlane(t)
	h := newHarness(t)

	if code := h.run("--api-url", plane.url, "--token", plane.token, "status"); code != 0 {
		t.Fatalf("status exit = %d, want 0\nstdout: %s\nstderr: %s", code, h.out(), h.err())
	}

	for _, want := range []string{
		"not configured; set HOSTING_API_URL, HOSTING_API_TOKEN, HOSTING_GATEWAY_HOSTNAME",
		"not configured; set MAIL_API_URL, MAIL_API_TOKEN, MAIL_HOSTNAME",
		"not configured; set BILLING_PROVIDER",
		"ns1.simple.test",
	} {
		contains(t, h.out(), want, "the status table")
	}
	absent(t, h.out(), plane.token, "the operator credential")
}

// TestInProcessJSONStatusNamesVariablesNotValues pins the machine-readable
// half of the same honesty: missing_env carries names, configured is a real
// false rather than an omitted field, and no engine invents a provider.
func TestInProcessJSONStatusNamesVariablesNotValues(t *testing.T) {
	plane := startInProcessPlane(t)
	h := newHarness(t)

	if code := h.run("--api-url", plane.url, "--token", plane.token, "--json", "status", "--probe"); code != 0 {
		t.Fatalf("status --json exit = %d, want 0\nstderr: %s", code, h.err())
	}
	document := h.decode()

	engines, ok := document["engines"].([]any)
	if !ok || len(engines) != 4 {
		t.Fatalf("engines = %#v, want four entries", document["engines"])
	}
	seen := map[string][]string{}
	for _, value := range engines {
		engine := object(t, value, "an engine")
		kind := text(t, engine["kind"], "kind")
		configured, present := engine["configured"].(bool)
		if !present {
			t.Fatalf("%s: configured is absent; an omitted false reads as \"no problem\"", kind)
		}
		if kind == "ENGINE_KIND_DNS" {
			if !configured {
				t.Errorf("%s: configured = false, want true", kind)
			}
			continue
		}
		if configured {
			t.Errorf("%s: configured = true, but no engine block was set", kind)
		}
		names, present := engine["missing_env"].([]any)
		if !present {
			t.Fatalf("%s: missing_env is absent, so the CLI cannot name the variables", kind)
		}
		for _, name := range names {
			seen[kind] = append(seen[kind], text(t, name, "a variable name"))
		}
	}

	for kind, want := range map[string]string{
		"ENGINE_KIND_HOSTING": "HOSTING_API_URL",
		"ENGINE_KIND_MAIL":    "MAIL_API_URL",
		"ENGINE_KIND_BILLING": "BILLING_PROVIDER",
	} {
		if !strings.Contains(strings.Join(seen[kind], ","), want) {
			t.Errorf("%s missing_env = %v, want it to name %s", kind, seen[kind], want)
		}
	}
	absent(t, h.out(), plane.token, "the operator credential")
}

// TestInProcessZoneLifecycle drives a zone through the real store: create it,
// add a record, read it back, and delete it. It is the proof that the CLI's
// requests are ones this control plane accepts, not only ones the fake does.
func TestInProcessZoneLifecycle(t *testing.T) {
	plane := startInProcessPlane(t)
	base := []string{"--api-url", plane.url, "--token", plane.token}

	h := newHarness(t)
	h.mustRun(append(append([]string{}, base...), "domain", "add", "example.test")...)
	contains(t, h.out(), "example.test", "the created zone")

	h = newHarness(t)
	h.mustRun(append(append([]string{}, base...), "record", "add", "example.test",
		"--name", "www", "--type", "A", "--value", "203.0.113.10", "--ttl", "300")...)

	h = newHarness(t)
	h.mustRun(append(append([]string{}, base...), "--json", "record", "list", "example.test", "--name", "www")...)
	records, ok := h.decode()["records"].([]any)
	if !ok || len(records) != 1 {
		t.Fatalf("records = %#v, want exactly the one just added", h.decode()["records"])
	}
	record := object(t, records[0], "the record")
	if got := text(t, record["value"], "value"); got != "203.0.113.10" {
		t.Errorf("value = %q, want 203.0.113.10", got)
	}
	recordID := text(t, record["id"], "id")

	h = newHarness(t)
	if code := h.run(append(append([]string{}, base...), "record", "delete", "example.test", recordID, "--yes")...); code != 0 {
		t.Fatalf("record delete exit = %d, want 0\nstderr: %s", code, h.err())
	}

	h = newHarness(t)
	h.mustRun(append(append([]string{}, base...), "domain", "delete", "example.test", "--yes")...)

	h = newHarness(t)
	h.mustRun(append(append([]string{}, base...), "--json", "domain", "list")...)
	if zones := field[[]any](t, h.decode(), "zones"); len(zones) != 0 {
		t.Errorf("zones = %#v, want none left", zones)
	}
}

// object and text read one value out of a decoded document, failing the test
// rather than yielding a zero, so no assertion in this file is blank.
func object(t *testing.T, value any, what string) map[string]any {
	t.Helper()
	document, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("%s = %#v, want an object", what, value)
	}
	return document
}

func text(t *testing.T, value any, what string) string {
	t.Helper()
	str, ok := value.(string)
	if !ok {
		t.Fatalf("%s = %#v, want a string", what, value)
	}
	return str
}

// TestInProcessEngineCallsAreRefusedNotFaked pins the other half of honesty: a
// command that needs an engine fails with the control plane's own precondition
// rather than an invented empty answer.
func TestInProcessEngineCallsAreRefusedNotFaked(t *testing.T) {
	plane := startInProcessPlane(t)
	base := []string{"--api-url", plane.url, "--token", plane.token}

	h := newHarness(t)
	h.mustRun(append(append([]string{}, base...), "domain", "add", "example.test")...)

	h = newHarness(t)
	code := h.run(append(append([]string{}, base...), "site", "show", "example.test")...)
	if code != 1 {
		t.Fatalf("site show exit = %d, want 1\nstdout: %s\nstderr: %s", code, h.out(), h.err())
	}
	contains(t, h.err(), "HOSTING_API_URL", "the refusal naming the variable")
	absent(t, h.out(), "no sites", "an invented empty answer")
}
