package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"time"
)

type action string

const (
	actionCreate action = "create"
	actionUpdate action = "update"
	actionNoOp   action = "no-op"
)

type change struct {
	Field  string `json:"field"`
	Before any    `json:"before"`
	After  any    `json:"after"`
}

type reconciliationPlan struct {
	Action         action   `json:"action"`
	Region         string   `json:"region"`
	Label          string   `json:"label"`
	Nodes          int      `json:"nodes"`
	LoadBalancerID string   `json:"load_balancer_id,omitempty"`
	Changes        []change `json:"changes"`

	createRequest createLoadBalancerRequest
	updateRequest updateLoadBalancerRequest
	current       loadBalancer
}

type planReport struct {
	Mode           string   `json:"mode"`
	Action         action   `json:"action"`
	Region         string   `json:"region"`
	Label          string   `json:"label"`
	Nodes          int      `json:"nodes"`
	CostImpact     string   `json:"cost_impact"`
	LoadBalancerID string   `json:"load_balancer_id,omitempty"`
	Changes        []change `json:"changes"`
}

type applyReport struct {
	Result         string `json:"result"`
	Action         action `json:"action"`
	LoadBalancerID string `json:"load_balancer_id"`
	Status         string `json:"status"`
	IPv4           string `json:"ipv4"`
	IPv6           string `json:"ipv6"`
}

type reconciler struct {
	client       *apiClient
	waitTimeout  time.Duration
	pollInterval time.Duration
	sleep        func(context.Context, time.Duration) error
}

func newDesiredState(label string, instances []string, nodes int) (desiredState, error) {
	label = strings.TrimSpace(label)
	if label == "" {
		return desiredState{}, errors.New("label must not be empty")
	}
	if nodes < 1 || nodes > 99 || nodes%2 == 0 {
		return desiredState{}, fmt.Errorf("nodes must be an odd number from 1 to 99, got %d", nodes)
	}
	if len(instances) == 0 {
		return desiredState{}, errors.New("at least one --instance-id is required")
	}

	normalizedInstances := make([]string, 0, len(instances))
	seen := make(map[string]struct{}, len(instances))
	for _, instance := range instances {
		instance = strings.TrimSpace(instance)
		if instance == "" {
			return desiredState{}, errors.New("instance IDs must not be empty")
		}
		if _, ok := seen[instance]; ok {
			return desiredState{}, fmt.Errorf("instance ID %q was provided more than once", instance)
		}
		seen[instance] = struct{}{}
		normalizedInstances = append(normalizedInstances, instance)
	}
	sort.Strings(normalizedInstances)

	return desiredState{
		Region:    apiRegion,
		Label:     label,
		Instances: normalizedInstances,
		Nodes:     nodes,
		HealthCheck: healthCheck{
			Protocol:           "tcp",
			Port:               dnsBackendPort,
			Path:               "/",
			CheckInterval:      healthCheckInterval,
			ResponseTimeout:    healthResponseTime,
			UnhealthyThreshold: healthFailureLimit,
			HealthyThreshold:   healthRecoveryLimit,
		},
		ForwardingRules: []forwardingRule{
			{FrontendProtocol: "tcp", FrontendPort: dnsFrontendPort, BackendProtocol: "tcp", BackendPort: dnsBackendPort},
			{FrontendProtocol: "udp", FrontendPort: dnsFrontendPort, BackendProtocol: "udp", BackendPort: dnsBackendPort},
		},
		FirewallRules: []firewallRule{
			{Port: dnsFrontendPort, IPType: "v4", Source: "0.0.0.0/0"},
			{Port: dnsFrontendPort, IPType: "v6", Source: "::/0"},
		},
	}, nil
}

func (r *reconciler) plan(ctx context.Context, desired desiredState) (reconciliationPlan, error) {
	loadBalancers, err := r.client.listLoadBalancers(ctx)
	if err != nil {
		return reconciliationPlan{}, fmt.Errorf("discover load balancers: %w", err)
	}

	matches := make([]loadBalancer, 0, 1)
	for _, candidate := range loadBalancers {
		if candidate.Label == desired.Label {
			matches = append(matches, candidate)
		}
	}
	if len(matches) > 1 {
		ids := make([]string, 0, len(matches))
		for _, match := range matches {
			ids = append(ids, match.ID)
		}
		sort.Strings(ids)
		return reconciliationPlan{}, fmt.Errorf("refusing to reconcile: %d load balancers have exact label %q (ids: %s)", len(matches), desired.Label, strings.Join(ids, ", "))
	}
	if len(matches) == 0 {
		request := createLoadBalancerRequest(desired)
		return reconciliationPlan{
			Action:        actionCreate,
			Region:        desired.Region,
			Label:         desired.Label,
			Nodes:         desired.Nodes,
			Changes:       []change{{Field: "load_balancer", Before: nil, After: desired}},
			createRequest: request,
		}, nil
	}

	current, err := r.client.getLoadBalancer(ctx, matches[0].ID)
	if err != nil {
		return reconciliationPlan{}, fmt.Errorf("read load balancer %q: %w", matches[0].ID, err)
	}
	if current.ID == "" || current.ID != matches[0].ID || current.Label != desired.Label {
		return reconciliationPlan{}, errors.New("API returned inconsistent load-balancer identity during discovery")
	}
	if current.Region != desired.Region {
		return reconciliationPlan{}, fmt.Errorf("refusing to reconcile load balancer %q in region %q; expected immutable region %q", current.ID, current.Region, desired.Region)
	}

	plan := reconciliationPlan{
		Action:         actionNoOp,
		Region:         desired.Region,
		Label:          desired.Label,
		Nodes:          desired.Nodes,
		LoadBalancerID: current.ID,
		Changes:        []change{},
		current:        current,
	}

	currentInstances := normalizeInstances(current.Instances)
	if !slices.Equal(currentInstances, desired.Instances) {
		plan.updateRequest.Instances = pointerTo(slices.Clone(desired.Instances))
		plan.Changes = append(plan.Changes, change{Field: "instances", Before: currentInstances, After: desired.Instances})
	}
	if current.Nodes != desired.Nodes {
		plan.updateRequest.Nodes = pointerTo(desired.Nodes)
		plan.Changes = append(plan.Changes, change{Field: "nodes", Before: current.Nodes, After: desired.Nodes})
	}

	currentHealth := normalizeHealthCheck(current.HealthCheck)
	desiredHealth := normalizeHealthCheck(desired.HealthCheck)
	if currentHealth != desiredHealth {
		plan.updateRequest.HealthCheck = pointerTo(desiredHealth)
		plan.Changes = append(plan.Changes, change{Field: "health_check", Before: currentHealth, After: desiredHealth})
	}

	currentForwarding := normalizeForwardingRules(current.ForwardingRules)
	desiredForwarding := mergeForwardingRules(current.ForwardingRules, desired.ForwardingRules)
	if !slices.Equal(currentForwarding, desiredForwarding) {
		plan.updateRequest.ForwardingRules = pointerTo(desiredForwarding)
		plan.Changes = append(plan.Changes, change{Field: "forwarding_rules", Before: currentForwarding, After: desiredForwarding})
	}

	currentFirewall := normalizeFirewallRules(current.FirewallRules)
	desiredFirewall := mergeFirewallRules(current.FirewallRules, desired.FirewallRules)
	if !slices.Equal(currentFirewall, desiredFirewall) {
		plan.updateRequest.FirewallRules = pointerTo(desiredFirewall)
		plan.Changes = append(plan.Changes, change{Field: "firewall_rules", Before: currentFirewall, After: desiredFirewall})
	}

	if len(plan.Changes) > 0 {
		plan.Action = actionUpdate
	}
	return plan, nil
}

func (r *reconciler) apply(ctx context.Context, plan reconciliationPlan) (loadBalancer, error) {
	switch plan.Action {
	case actionCreate:
		current, err := r.client.createLoadBalancer(ctx, plan.createRequest)
		if err != nil {
			return loadBalancer{}, fmt.Errorf("create load balancer: %w", err)
		}
		if current.ID == "" {
			return loadBalancer{}, errors.New("create load balancer: Vultr API returned an empty id")
		}
		return r.waitUntilReady(ctx, current.ID)
	case actionUpdate:
		if err := r.client.updateLoadBalancer(ctx, plan.LoadBalancerID, plan.updateRequest); err != nil {
			return loadBalancer{}, fmt.Errorf("update load balancer %q: %w", plan.LoadBalancerID, err)
		}
		return r.waitUntilReady(ctx, plan.LoadBalancerID)
	case actionNoOp:
		if isReady(plan.current) {
			return plan.current, nil
		}
		return r.waitUntilReady(ctx, plan.LoadBalancerID)
	default:
		return loadBalancer{}, fmt.Errorf("unsupported reconciliation action %q", plan.Action)
	}
}

func (r *reconciler) waitUntilReady(ctx context.Context, id string) (loadBalancer, error) {
	waitCtx, cancel := context.WithTimeout(ctx, r.waitTimeout)
	defer cancel()

	var last loadBalancer
	for {
		current, err := r.client.getLoadBalancer(waitCtx, id)
		if err != nil {
			return loadBalancer{}, fmt.Errorf("poll load balancer %q: %w", id, err)
		}
		last = current
		if isReady(current) {
			return current, nil
		}
		if strings.EqualFold(current.Status, "failed") || strings.EqualFold(current.Status, "error") {
			return loadBalancer{}, fmt.Errorf("load balancer %q entered terminal status %q", id, current.Status)
		}
		if err := r.sleep(waitCtx, r.pollInterval); err != nil {
			return loadBalancer{}, fmt.Errorf(
				"wait for load balancer %q to become active with IPv4 and IPv6 (last status=%q ipv4=%q ipv6=%q): %w",
				id,
				last.Status,
				last.IPv4,
				last.IPv6,
				err,
			)
		}
	}
}

func isReady(loadBalancer loadBalancer) bool {
	return strings.EqualFold(loadBalancer.Status, "active") && loadBalancer.IPv4 != "" && loadBalancer.IPv6 != ""
}

func renderPlan(w io.Writer, mode string, plan reconciliationPlan) error {
	report := planReport{
		Mode:           mode,
		Action:         plan.Action,
		Region:         plan.Region,
		Label:          plan.Label,
		Nodes:          plan.Nodes,
		CostImpact:     fmt.Sprintf("%d billable Vultr load-balancer node(s); increasing --nodes increases cost", plan.Nodes),
		LoadBalancerID: plan.LoadBalancerID,
		Changes:        plan.Changes,
	}
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		return fmt.Errorf("write plan: %w", err)
	}
	return nil
}

func renderApplyResult(w io.Writer, plan reconciliationPlan, current loadBalancer) error {
	report := applyReport{
		Result:         "applied",
		Action:         plan.Action,
		LoadBalancerID: current.ID,
		Status:         current.Status,
		IPv4:           current.IPv4,
		IPv6:           current.IPv6,
	}
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		return fmt.Errorf("write apply result: %w", err)
	}
	return nil
}

func normalizeInstances(instances []string) []string {
	normalized := slices.Clone(instances)
	sort.Strings(normalized)
	return normalized
}

func normalizeHealthCheck(check healthCheck) healthCheck {
	check.Protocol = strings.ToLower(check.Protocol)
	return check
}

func normalizeForwardingRules(rules []forwardingRule) []forwardingRule {
	normalized := make([]forwardingRule, 0, len(rules))
	for _, rule := range rules {
		rule.ID = ""
		rule.FrontendProtocol = strings.ToLower(rule.FrontendProtocol)
		rule.BackendProtocol = strings.ToLower(rule.BackendProtocol)
		normalized = append(normalized, rule)
	}
	sort.Slice(normalized, func(i, j int) bool {
		return forwardingRuleKey(normalized[i]) < forwardingRuleKey(normalized[j])
	})
	return normalized
}

func mergeForwardingRules(current, managed []forwardingRule) []forwardingRule {
	merged := make([]forwardingRule, 0, len(current)+len(managed))
	for _, rule := range current {
		if !isManagedForwardingRule(rule) {
			merged = append(merged, rule)
		}
	}
	merged = append(merged, managed...)
	return normalizeForwardingRules(merged)
}

func isManagedForwardingRule(rule forwardingRule) bool {
	protocol := strings.ToLower(rule.FrontendProtocol)
	return rule.FrontendPort == dnsFrontendPort && (protocol == "tcp" || protocol == "udp")
}

func forwardingRuleKey(rule forwardingRule) string {
	return strings.Join([]string{
		rule.FrontendProtocol,
		integerString(rule.FrontendPort),
		rule.BackendProtocol,
		integerString(rule.BackendPort),
	}, ":")
}

func normalizeFirewallRules(rules []firewallRule) []firewallRule {
	normalized := make([]firewallRule, 0, len(rules))
	for _, rule := range rules {
		rule.ID = ""
		rule.IPType = strings.ToLower(rule.IPType)
		normalized = append(normalized, rule)
	}
	sort.Slice(normalized, func(i, j int) bool {
		return firewallRuleKey(normalized[i]) < firewallRuleKey(normalized[j])
	})
	return normalized
}

func mergeFirewallRules(current, managed []firewallRule) []firewallRule {
	merged := make([]firewallRule, 0, len(current)+len(managed))
	for _, rule := range current {
		if !isManagedFirewallRule(rule) {
			merged = append(merged, rule)
		}
	}
	merged = append(merged, managed...)
	return normalizeFirewallRules(merged)
}

func isManagedFirewallRule(rule firewallRule) bool {
	ipType := strings.ToLower(rule.IPType)
	return rule.Port == dnsFrontendPort && (ipType == "v4" || ipType == "v6")
}

func firewallRuleKey(rule firewallRule) string {
	return strings.Join([]string{rule.IPType, integerString(rule.Port), rule.Source}, ":")
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
