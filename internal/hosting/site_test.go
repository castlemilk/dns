package hosting

import (
	"strings"
	"testing"

	"github.com/castlemilk/dns/internal/platform"
)

func TestAppNameIsDeterministicAndFitsTheEngineIdentity(t *testing.T) {
	t.Parallel()

	// A 63-character label is the longest a DNS label can be, and the tenant is
	// capped at 24 characters by HOSTING_TENANT's validation. `slug(tenant)-app`
	// becomes a Kubernetes object name, so it must never exceed 63 characters
	// for any combination.
	longest := strings.Repeat("a", 63) + ".example"
	for length := 1; length <= 24; length++ {
		tenant := strings.Repeat("t", length)
		app := AppName(tenant, longest)
		identity := AppRef(tenant, app)
		if len(identity) > maxAppIdentity {
			t.Errorf("tenant length %d: identity %q is %d characters", length, identity, len(identity))
		}
		if app != AppName(tenant, longest) {
			t.Errorf("tenant length %d: app name is not deterministic", length)
		}
	}
}

func TestAppNameSeparatesZonesWhoseSlugsCollide(t *testing.T) {
	t.Parallel()

	first := AppName("simple", "acme.dev")
	second := AppName("simple", "acme-dev.com")
	if first == second {
		t.Fatalf("acme.dev and acme-dev.com both map to %q", first)
	}
	if !strings.HasPrefix(first, "acme-dev-") {
		t.Errorf("app name = %q, want the slugged zone as its prefix", first)
	}
}

func TestSlugMatchesTheEngineRule(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"Acme.Dev":     "acme-dev",
		"  spaced  ":   "spaced",
		"a__b":         "a-b",
		"---":          "",
		"studio.photo": "studio-photo",
	}
	for input, want := range cases {
		if got := Slug(input); got != want {
			t.Errorf("Slug(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestHostnameStateMapsEngineReadiness(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		view DomainView
		want string
	}{
		{"old server reports nothing", DomainView{}, HostStateRegistered},
		{"routes not applied", DomainView{ReadinessReported: true}, HostStateRegistered},
		{"gateway provider none", DomainView{ReadinessReported: true, Ready: true, TLSMode: TLSModeNone}, HostStateUnrouted},
		{"platform certificate", DomainView{ReadinessReported: true, Ready: true, TLSMode: TLSModeShared}, HostStateRoutesReady},
		{"certificate pending", DomainView{ReadinessReported: true, Ready: true, TLSMode: TLSModeIssued}, HostStateCertificatePending},
		{"certificate ready", DomainView{ReadinessReported: true, Ready: true, TLSMode: TLSModeIssued, CertificateReady: true}, HostStateCertificateReady},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := hostnameState(test.view); got != test.want {
				t.Errorf("hostnameState = %q, want %q", got, test.want)
			}
		})
	}
}

func TestMergeHostnamesKeepsObservedStateAndDropsRemovedHosts(t *testing.T) {
	t.Parallel()

	desired := []platform.HostnameDoc{
		{Host: "acme.dev", Role: HostRoleApex, State: HostStateMissing},
	}
	observed := []platform.HostnameDoc{
		{Host: "acme.dev", Role: HostRoleApex, State: HostStateCertificateReady},
		{Host: "www.acme.dev", Role: HostRoleWWW, State: HostStateCertificateReady},
	}
	merged := mergeHostnames(desired, observed)
	if len(merged) != 1 {
		t.Fatalf("merged %d hosts, want 1", len(merged))
	}
	if merged[0].State != HostStateCertificateReady {
		t.Errorf("apex state = %q, want the observed state to survive", merged[0].State)
	}
}

func TestValidHostZoneRejectsNamesThatAreNotHostnames(t *testing.T) {
	t.Parallel()

	valid := []string{"acme.dev", "a-b.example.test", "xn--p1ai.example"}
	for _, name := range valid {
		if !validHostZone(name) {
			t.Errorf("validHostZone(%q) = false, want true", name)
		}
	}
	// Phase-1 zone names may carry characters a host may not.
	invalid := []string{"", "single", "acme dev.test", "acme/dev.test", "acme:1.test", "-acme.test", "a@b.test"}
	for _, name := range invalid {
		if validHostZone(name) {
			t.Errorf("validHostZone(%q) = true, want false", name)
		}
	}
}
