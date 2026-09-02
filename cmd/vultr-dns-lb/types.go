package main

const (
	apiRegion           = "syd"
	defaultLabel        = "simpledns-authoritative-syd"
	defaultNodes        = 1
	dnsFrontendPort     = 53
	dnsBackendPort      = 30053
	healthCheckInterval = 15
	healthResponseTime  = 5
	healthFailureLimit  = 5
	healthRecoveryLimit = 5
)

type healthCheck struct {
	Protocol           string `json:"protocol"`
	Port               int    `json:"port"`
	Path               string `json:"path"`
	CheckInterval      int    `json:"check_interval"`
	ResponseTimeout    int    `json:"response_timeout"`
	UnhealthyThreshold int    `json:"unhealthy_threshold"`
	HealthyThreshold   int    `json:"healthy_threshold"`
}

type forwardingRule struct {
	ID               string `json:"id,omitempty"`
	FrontendProtocol string `json:"frontend_protocol"`
	FrontendPort     int    `json:"frontend_port"`
	BackendProtocol  string `json:"backend_protocol"`
	BackendPort      int    `json:"backend_port"`
}

type firewallRule struct {
	ID     string `json:"id,omitempty"`
	Port   int    `json:"port"`
	IPType string `json:"ip_type"`
	Source string `json:"source"`
}

type loadBalancer struct {
	ID              string           `json:"id"`
	Region          string           `json:"region"`
	Label           string           `json:"label"`
	Status          string           `json:"status"`
	IPv4            string           `json:"ipv4"`
	IPv6            string           `json:"ipv6"`
	Instances       []string         `json:"instances"`
	Nodes           int              `json:"nodes"`
	HealthCheck     healthCheck      `json:"health_check"`
	ForwardingRules []forwardingRule `json:"forwarding_rules"`
	FirewallRules   []firewallRule   `json:"firewall_rules"`
}

type desiredState struct {
	Region          string           `json:"region"`
	Label           string           `json:"label"`
	Instances       []string         `json:"instances"`
	Nodes           int              `json:"nodes"`
	HealthCheck     healthCheck      `json:"health_check"`
	ForwardingRules []forwardingRule `json:"forwarding_rules"`
	FirewallRules   []firewallRule   `json:"firewall_rules"`
}

type createLoadBalancerRequest struct {
	Region          string           `json:"region"`
	Label           string           `json:"label"`
	Instances       []string         `json:"instances"`
	Nodes           int              `json:"nodes"`
	HealthCheck     healthCheck      `json:"health_check"`
	ForwardingRules []forwardingRule `json:"forwarding_rules"`
	FirewallRules   []firewallRule   `json:"firewall_rules"`
}

type updateLoadBalancerRequest struct {
	Instances       *[]string         `json:"instances,omitempty"`
	Nodes           *int              `json:"nodes,omitempty"`
	HealthCheck     *healthCheck      `json:"health_check,omitempty"`
	ForwardingRules *[]forwardingRule `json:"forwarding_rules,omitempty"`
	FirewallRules   *[]firewallRule   `json:"firewall_rules,omitempty"`
}
