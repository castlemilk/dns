package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testAPIKey = "vultr-test-secret"
	testLabel  = "dns-authoritative-syd"
	testLBID   = "lb-0001"
)

func TestPlanIsReadOnlyAndDeterministic(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		requests.Add(1)
		assertAuthorization(t, req)
		if req.Method != http.MethodGet || req.URL.Path != "/v2/load-balancers" {
			http.Error(w, "unexpected request", http.StatusNotFound)
			return
		}
		writeJSON(t, w, http.StatusOK, map[string]any{
			"load_balancers": []loadBalancer{},
			"meta":           map[string]any{"links": map[string]string{"next": ""}},
		})
	})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	args := baseArgs()
	first, _, err := invoke(t, server, args, testAPIKey, immediateSleep)
	if err != nil {
		t.Fatalf("first plan: %v", err)
	}
	second, _, err := invoke(t, server, args, testAPIKey, immediateSleep)
	if err != nil {
		t.Fatalf("second plan: %v", err)
	}
	if first != second {
		t.Fatalf("plan output is not deterministic\nfirst:\n%s\nsecond:\n%s", first, second)
	}
	if requests.Load() != 2 {
		t.Fatalf("requests = %d, want two discovery GETs", requests.Load())
	}

	var report planReport
	if err := json.Unmarshal([]byte(first), &report); err != nil {
		t.Fatalf("decode plan: %v", err)
	}
	if report.Mode != "plan" || report.Action != actionCreate || report.Region != apiRegion || report.Nodes != 1 {
		t.Fatalf("report = %+v", report)
	}
	if !strings.Contains(report.CostImpact, "billable") || !strings.Contains(report.CostImpact, "increasing --nodes increases cost") {
		t.Fatalf("cost impact is not explicit: %q", report.CostImpact)
	}
	if len(report.Changes) != 1 || report.Changes[0].Field != "load_balancer" {
		t.Fatalf("changes = %+v", report.Changes)
	}
	assertSecretAbsent(t, first)
}

func TestApplyCreatesAndWaitsForReady(t *testing.T) {
	t.Parallel()

	var createBody []byte
	var createMu sync.Mutex
	var polls atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		assertAuthorization(t, req)
		switch {
		case req.Method == http.MethodGet && req.URL.Path == "/v2/load-balancers":
			writeJSON(t, w, http.StatusOK, map[string]any{
				"load_balancers": []loadBalancer{},
				"meta":           map[string]any{"links": map[string]string{"next": ""}},
			})
		case req.Method == http.MethodPost && req.URL.Path == "/v2/load-balancers":
			data, err := io.ReadAll(req.Body)
			if err != nil {
				t.Errorf("read create body: %v", err)
				http.Error(w, "read body", http.StatusBadRequest)
				return
			}
			createMu.Lock()
			createBody = slices.Clone(data)
			createMu.Unlock()
			writeJSON(t, w, http.StatusAccepted, map[string]any{
				"load_balancer": loadBalancer{ID: testLBID, Label: testLabel, Region: apiRegion, Status: "pending"},
			})
		case req.Method == http.MethodGet && req.URL.Path == "/v2/load-balancers/"+testLBID:
			poll := polls.Add(1)
			current := loadBalancer{ID: testLBID, Label: testLabel, Region: apiRegion, Status: "pending"}
			if poll >= 2 {
				current.Status = "active"
				current.IPv4 = "192.0.2.53"
				current.IPv6 = "2001:db8::53"
			}
			writeJSON(t, w, http.StatusOK, map[string]any{"load_balancer": current})
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	stdout, stderr, err := invoke(t, server, append([]string{"--apply"}, baseArgs()...), testAPIKey, immediateSleep)
	if err != nil {
		t.Fatalf("apply create: %v\nstderr: %s", err, stderr)
	}
	if polls.Load() != 2 {
		t.Fatalf("polls = %d, want 2", polls.Load())
	}

	createMu.Lock()
	body := slices.Clone(createBody)
	createMu.Unlock()
	var request createLoadBalancerRequest
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatalf("decode create request: %v", err)
	}
	desired := mustDesired(t)
	if request.Region != apiRegion || request.Label != testLabel || request.Nodes != defaultNodes {
		t.Fatalf("create identity = %+v", request)
	}
	if !slices.Equal(request.Instances, desired.Instances) {
		t.Fatalf("instances = %v, want %v", request.Instances, desired.Instances)
	}
	if request.HealthCheck != desired.HealthCheck {
		t.Fatalf("health check = %+v, want %+v", request.HealthCheck, desired.HealthCheck)
	}
	if !reflect.DeepEqual(request.ForwardingRules, desired.ForwardingRules) {
		t.Fatalf("forwarding rules = %+v, want %+v", request.ForwardingRules, desired.ForwardingRules)
	}
	if !reflect.DeepEqual(request.FirewallRules, desired.FirewallRules) {
		t.Fatalf("firewall rules = %+v, want %+v", request.FirewallRules, desired.FirewallRules)
	}

	plan, applied := decodeReports(t, stdout)
	if plan.Action != actionCreate || applied.Action != actionCreate || applied.Status != "active" {
		t.Fatalf("reports: plan=%+v apply=%+v", plan, applied)
	}
	if applied.IPv4 == "" || applied.IPv6 == "" {
		t.Fatalf("apply result missing addresses: %+v", applied)
	}
	assertSecretAbsent(t, stdout+stderr)
}

func TestApplyUpdatesManagedFieldsAndPreservesUnrelatedRules(t *testing.T) {
	t.Parallel()

	desired := mustDesiredNodes(t, 3)
	current := loadBalancer{
		ID:        testLBID,
		Label:     testLabel,
		Region:    apiRegion,
		Status:    "active",
		IPv4:      "192.0.2.53",
		IPv6:      "2001:db8::53",
		Instances: []string{"old-worker"},
		Nodes:     1,
		HealthCheck: healthCheck{
			Protocol: "http",
			Port:     8080,
			Path:     "/health",
		},
		ForwardingRules: []forwardingRule{
			{ID: "keep-forward", FrontendProtocol: "https", FrontendPort: 443, BackendProtocol: "https", BackendPort: 8443},
			{ID: "replace-tcp", FrontendProtocol: "tcp", FrontendPort: 53, BackendProtocol: "tcp", BackendPort: 53},
			{ID: "replace-udp", FrontendProtocol: "udp", FrontendPort: 53, BackendProtocol: "tcp", BackendPort: 53},
		},
		FirewallRules: []firewallRule{
			{ID: "keep-firewall", Port: 443, IPType: "v4", Source: "198.51.100.0/24"},
			{ID: "replace-firewall", Port: 53, IPType: "v4", Source: "198.51.100.0/24"},
		},
	}
	ready := loadBalancer{
		ID:              testLBID,
		Label:           testLabel,
		Region:          apiRegion,
		Status:          "active",
		IPv4:            current.IPv4,
		IPv6:            current.IPv6,
		Instances:       desired.Instances,
		Nodes:           desired.Nodes,
		HealthCheck:     desired.HealthCheck,
		ForwardingRules: mergeForwardingRules(current.ForwardingRules, desired.ForwardingRules),
		FirewallRules:   mergeFirewallRules(current.FirewallRules, desired.FirewallRules),
	}

	var detailReads atomic.Int32
	var patchBody []byte
	var patchMu sync.Mutex
	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		assertAuthorization(t, req)
		switch {
		case req.Method == http.MethodGet && req.URL.Path == "/v2/load-balancers":
			writeJSON(t, w, http.StatusOK, map[string]any{
				"load_balancers": []loadBalancer{{ID: testLBID, Label: testLabel, Region: apiRegion}},
				"meta":           map[string]any{"links": map[string]string{"next": ""}},
			})
		case req.Method == http.MethodGet && req.URL.Path == "/v2/load-balancers/"+testLBID:
			if detailReads.Add(1) == 1 {
				writeJSON(t, w, http.StatusOK, map[string]any{"load_balancer": current})
				return
			}
			writeJSON(t, w, http.StatusOK, map[string]any{"load_balancer": ready})
		case req.Method == http.MethodPatch && req.URL.Path == "/v2/load-balancers/"+testLBID:
			data, err := io.ReadAll(req.Body)
			if err != nil {
				t.Errorf("read patch body: %v", err)
				http.Error(w, "read body", http.StatusBadRequest)
				return
			}
			patchMu.Lock()
			patchBody = slices.Clone(data)
			patchMu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	args := append([]string{"--apply", "--nodes=3"}, baseArgs()...)
	stdout, _, err := invoke(t, server, args, testAPIKey, immediateSleep)
	if err != nil {
		t.Fatalf("apply update: %v", err)
	}
	if detailReads.Load() != 2 {
		t.Fatalf("detail reads = %d, want discovery plus readiness poll", detailReads.Load())
	}

	patchMu.Lock()
	body := slices.Clone(patchBody)
	patchMu.Unlock()
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("decode raw patch: %v", err)
	}
	for _, forbidden := range []string{"region", "label", "generic_info", "status", "ipv4", "ipv6"} {
		if _, ok := raw[forbidden]; ok {
			t.Errorf("patch unexpectedly contains unrelated field %q: %s", forbidden, body)
		}
	}
	var patch updateLoadBalancerRequest
	if err := json.Unmarshal(body, &patch); err != nil {
		t.Fatalf("decode patch: %v", err)
	}
	if patch.Instances == nil || patch.Nodes == nil || patch.HealthCheck == nil || patch.ForwardingRules == nil || patch.FirewallRules == nil {
		t.Fatalf("patch omitted a managed change: %+v", patch)
	}
	if !containsForwardingRule(*patch.ForwardingRules, forwardingRule{FrontendProtocol: "https", FrontendPort: 443, BackendProtocol: "https", BackendPort: 8443}) {
		t.Errorf("unrelated forwarding rule was not preserved: %+v", *patch.ForwardingRules)
	}
	if !containsFirewallRule(*patch.FirewallRules, firewallRule{Port: 443, IPType: "v4", Source: "198.51.100.0/24"}) {
		t.Errorf("unrelated firewall rule was not preserved: %+v", *patch.FirewallRules)
	}
	for _, rule := range *patch.ForwardingRules {
		if rule.ID != "" {
			t.Errorf("forwarding request retained response-only id: %+v", rule)
		}
	}
	for _, rule := range *patch.FirewallRules {
		if rule.ID != "" {
			t.Errorf("firewall request retained response-only id: %+v", rule)
		}
	}

	plan, applied := decodeReports(t, stdout)
	if plan.Action != actionUpdate || applied.Action != actionUpdate {
		t.Fatalf("reports: plan=%+v apply=%+v", plan, applied)
	}
	if plan.Nodes != 3 || !strings.Contains(plan.CostImpact, "3 billable") {
		t.Fatalf("three-node cost impact is not explicit: %+v", plan)
	}
	fields := make([]string, 0, len(plan.Changes))
	for _, item := range plan.Changes {
		fields = append(fields, item.Field)
	}
	wantFields := []string{"instances", "nodes", "health_check", "forwarding_rules", "firewall_rules"}
	if !slices.Equal(fields, wantFields) {
		t.Fatalf("change order = %v, want %v", fields, wantFields)
	}
}

func TestApplyNoOpMakesNoMutation(t *testing.T) {
	t.Parallel()

	desired := mustDesired(t)
	current := loadBalancer{
		ID:          testLBID,
		Label:       testLabel,
		Region:      apiRegion,
		Status:      "active",
		IPv4:        "192.0.2.53",
		IPv6:        "2001:db8::53",
		Instances:   []string{"worker-b", "worker-a"},
		Nodes:       desired.Nodes,
		HealthCheck: desired.HealthCheck,
		ForwardingRules: []forwardingRule{
			withForwardingID(desired.ForwardingRules[1], "udp-id"),
			withForwardingID(desired.ForwardingRules[0], "tcp-id"),
		},
		FirewallRules: []firewallRule{
			withFirewallID(desired.FirewallRules[1], "v6-id"),
			withFirewallID(desired.FirewallRules[0], "v4-id"),
		},
	}

	var mutations atomic.Int32
	var detailReads atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		assertAuthorization(t, req)
		switch {
		case req.Method == http.MethodGet && req.URL.Path == "/v2/load-balancers":
			writeJSON(t, w, http.StatusOK, map[string]any{
				"load_balancers": []loadBalancer{{ID: testLBID, Label: testLabel, Region: apiRegion}},
				"meta":           map[string]any{"links": map[string]string{"next": ""}},
			})
		case req.Method == http.MethodGet && req.URL.Path == "/v2/load-balancers/"+testLBID:
			detailReads.Add(1)
			writeJSON(t, w, http.StatusOK, map[string]any{"load_balancer": current})
		case req.Method == http.MethodPost || req.Method == http.MethodPatch || req.Method == http.MethodDelete:
			mutations.Add(1)
			http.Error(w, "mutation not expected", http.StatusInternalServerError)
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	stdout, _, err := invoke(t, server, append([]string{"--apply"}, baseArgs()...), testAPIKey, immediateSleep)
	if err != nil {
		t.Fatalf("apply no-op: %v", err)
	}
	if mutations.Load() != 0 {
		t.Fatalf("mutations = %d, want 0", mutations.Load())
	}
	if detailReads.Load() != 1 {
		t.Fatalf("detail reads = %d, want no redundant readiness poll", detailReads.Load())
	}
	plan, applied := decodeReports(t, stdout)
	if plan.Action != actionNoOp || len(plan.Changes) != 0 || applied.Action != actionNoOp {
		t.Fatalf("reports: plan=%+v apply=%+v", plan, applied)
	}
}

func TestDuplicateExactLabelAcrossPagesFailsClosed(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		requests.Add(1)
		assertAuthorization(t, req)
		if req.Method != http.MethodGet || req.URL.Path != "/v2/load-balancers" {
			http.Error(w, "unexpected request", http.StatusNotFound)
			return
		}
		if req.URL.Query().Get("cursor") == "" {
			writeJSON(t, w, http.StatusOK, map[string]any{
				"load_balancers": []loadBalancer{{ID: "lb-z", Label: testLabel}},
				"meta":           map[string]any{"links": map[string]string{"next": "next-page"}},
			})
			return
		}
		if req.URL.Query().Get("cursor") != "next-page" {
			http.Error(w, "bad cursor", http.StatusBadRequest)
			return
		}
		writeJSON(t, w, http.StatusOK, map[string]any{
			"load_balancers": []loadBalancer{{ID: "lb-a", Label: testLabel}},
			"meta":           map[string]any{"links": map[string]string{"next": ""}},
		})
	})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	stdout, _, err := invoke(t, server, baseArgs(), testAPIKey, immediateSleep)
	if err == nil {
		t.Fatal("duplicate discovery unexpectedly succeeded")
	}
	if got := err.Error(); !strings.Contains(got, "2 load balancers") || !strings.Contains(got, "lb-a, lb-z") {
		t.Fatalf("error = %q", got)
	}
	if requests.Load() != 2 {
		t.Fatalf("requests = %d, want two paginated reads only", requests.Load())
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want no plan for ambiguous discovery", stdout)
	}
}

func TestAPIRejectionIsTypedAndSecretIsRedacted(t *testing.T) {
	t.Parallel()

	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		assertAuthorization(t, req)
		writeJSON(t, w, http.StatusUnprocessableEntity, map[string]string{
			"error": "rejected Authorization: Bearer " + testAPIKey,
		})
	})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	stdout, stderr, err := invoke(t, server, baseArgs(), testAPIKey, immediateSleep)
	if err == nil {
		t.Fatal("API rejection unexpectedly succeeded")
	}
	var rejection *apiError
	if !errors.As(err, &rejection) {
		t.Fatalf("error type = %T, want *apiError: %v", err, err)
	}
	if rejection.StatusCode != http.StatusUnprocessableEntity || rejection.Method != http.MethodGet {
		t.Fatalf("API error = %+v", rejection)
	}
	if !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("error does not identify redaction: %v", err)
	}
	assertSecretAbsent(t, err.Error()+stdout+stderr)
}

func TestApplyReadinessWaitIsBounded(t *testing.T) {
	t.Parallel()

	var detailReads atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		assertAuthorization(t, req)
		switch req.URL.Path {
		case "/v2/load-balancers":
			writeJSON(t, w, http.StatusOK, map[string]any{
				"load_balancers": []loadBalancer{{ID: testLBID, Label: testLabel, Region: apiRegion}},
				"meta":           map[string]any{"links": map[string]string{"next": ""}},
			})
		case "/v2/load-balancers/" + testLBID:
			detailReads.Add(1)
			writeJSON(t, w, http.StatusOK, map[string]any{
				"load_balancer": pendingDesired(t),
			})
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	blockingSleep := func(ctx context.Context, _ time.Duration) error {
		<-ctx.Done()
		return ctx.Err()
	}
	args := append([]string{"--apply", "--wait-timeout=20ms", "--poll-interval=1ms"}, baseArgs()...)
	_, _, err := invoke(t, server, args, testAPIKey, blockingSleep)
	if err == nil || !strings.Contains(err.Error(), "last status=\"pending\"") {
		t.Fatalf("error = %v, want bounded readiness timeout", err)
	}
	if detailReads.Load() != 2 {
		t.Fatalf("detail reads = %d, want discovery detail plus one readiness read", detailReads.Load())
	}
}

func TestDesiredStateValidation(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		label     string
		instances []string
		nodes     int
		wantError string
	}{
		"empty label":        {label: " ", instances: []string{"worker"}, nodes: 3, wantError: "label"},
		"no instances":       {label: testLabel, nodes: 3, wantError: "instance"},
		"duplicate instance": {label: testLabel, instances: []string{"worker", "worker"}, nodes: 3, wantError: "more than once"},
		"even nodes":         {label: testLabel, instances: []string{"worker"}, nodes: 2, wantError: "odd"},
		"too many nodes":     {label: testLabel, instances: []string{"worker"}, nodes: 101, wantError: "1 to 99"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := newDesiredState(test.label, test.instances, test.nodes)
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("error = %v, want substring %q", err, test.wantError)
			}
		})
	}
}

func TestRunRequiresAPIKeyFromEnvironment(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := run(
		context.Background(),
		baseArgs(),
		func(string) string { return "" },
		&stdout,
		&stderr,
		runDependencies{apiBaseURL: "https://api.invalid"},
	)
	if err == nil || !strings.Contains(err.Error(), "VULTR_API_KEY is required") {
		t.Fatalf("error = %v", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func invoke(
	t *testing.T,
	server *httptest.Server,
	args []string,
	apiKey string,
	sleep func(context.Context, time.Duration) error,
) (string, string, error) {
	t.Helper()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := run(
		context.Background(),
		args,
		func(name string) string {
			if name != "VULTR_API_KEY" {
				t.Errorf("unexpected environment lookup %q", name)
				return ""
			}
			return apiKey
		},
		&stdout,
		&stderr,
		runDependencies{apiBaseURL: server.URL, httpClient: server.Client(), sleep: sleep},
	)
	return stdout.String(), stderr.String(), err
}

func baseArgs() []string {
	return []string{
		"--label=" + testLabel,
		"--instance-id=worker-b",
		"--instance-id=worker-a",
		"--wait-timeout=1s",
		"--poll-interval=1ms",
	}
}

func mustDesired(t *testing.T) desiredState {
	t.Helper()
	return mustDesiredNodes(t, defaultNodes)
}

func mustDesiredNodes(t *testing.T, nodes int) desiredState {
	t.Helper()
	desired, err := newDesiredState(testLabel, []string{"worker-b", "worker-a"}, nodes)
	if err != nil {
		t.Fatalf("desired state: %v", err)
	}
	return desired
}

func pendingDesired(t *testing.T) loadBalancer {
	t.Helper()
	desired := mustDesired(t)
	return loadBalancer{
		ID:              testLBID,
		Label:           testLabel,
		Region:          apiRegion,
		Status:          "pending",
		Instances:       desired.Instances,
		Nodes:           desired.Nodes,
		HealthCheck:     desired.HealthCheck,
		ForwardingRules: desired.ForwardingRules,
		FirewallRules:   desired.FirewallRules,
	}
}

func decodeReports(t *testing.T, output string) (planReport, applyReport) {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(output))
	var plan planReport
	if err := decoder.Decode(&plan); err != nil {
		t.Fatalf("decode plan report: %v\n%s", err, output)
	}
	var applied applyReport
	if err := decoder.Decode(&applied); err != nil {
		t.Fatalf("decode apply report: %v\n%s", err, output)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		t.Fatalf("unexpected extra report: %v", err)
	}
	return plan, applied
}

func assertAuthorization(t *testing.T, req *http.Request) {
	t.Helper()
	if got, want := req.Header.Get("Authorization"), "Bearer "+testAPIKey; got != want {
		t.Errorf("Authorization = %q, want %q", got, want)
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Errorf("encode response: %v", err)
	}
}

func assertSecretAbsent(t *testing.T, value string) {
	t.Helper()
	if strings.Contains(value, testAPIKey) {
		t.Fatalf("secret leaked in output: %q", value)
	}
}

func immediateSleep(context.Context, time.Duration) error {
	return nil
}

func containsForwardingRule(rules []forwardingRule, want forwardingRule) bool {
	return slices.Contains(rules, want)
}

func containsFirewallRule(rules []firewallRule, want firewallRule) bool {
	return slices.Contains(rules, want)
}

func withForwardingID(rule forwardingRule, id string) forwardingRule {
	rule.ID = id
	return rule
}

func withFirewallID(rule firewallRule, id string) firewallRule {
	rule.ID = id
	return rule
}
