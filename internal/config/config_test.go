package config_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/castlemilk/dns/internal/config"
)

const (
	productionAPIToken      = "api-token-0123456789abcdef01234567"
	productionSnapshotToken = "snapshot-0123456789abcdef01234567"
)

func TestLoadDefaults(t *testing.T) {
	t.Parallel()

	got, err := config.Load(func(string) string { return "" })
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := config.Config{
		Role:                 config.RoleAll,
		HTTPAddr:             ":8080",
		DNSAddr:              ":1053",
		DataPath:             "./data/dns.db",
		Nameservers:          []string{"ns1.example.net", "ns2.example.net"},
		CORSOrigins:          []string{"http://localhost:3000"},
		MaxUDPSize:           1232,
		ShutdownTimeout:      10 * time.Second,
		SnapshotCachePath:    "data/snapshot.json",
		SnapshotPollInterval: 5 * time.Second,
		SnapshotMaxStaleness: time.Minute,
		SnapshotHTTPTimeout:  5 * time.Second,
		HTTPMaxBodyBytes:     1 << 20,
		SnapshotMaxBodyBytes: 8 << 20,
		PlatformPath:         "data/platform.db",
		Activity:             config.Activity{MaxEvents: config.DefaultActivityMaxEvents},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Load defaults = %#v, want %#v", got, want)
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Parallel()

	env := map[string]string{
		"DNS_ROLE":                    " ALL ",
		"DNS_HTTP_ADDR":               " 127.0.0.1:8081 ",
		"DNS_LISTEN_ADDR":             " :5353 ",
		"DNS_DATA_PATH":               " /data/zones.db ",
		"DNS_NAMESERVERS":             " ns1.example.test, ,ns2.example.test ",
		"DNS_CORS_ORIGINS":            " https://one.example, https://two.example ",
		"DNS_BOOTSTRAP_ZONE":          " Example.Test. ",
		"DNS_MAX_UDP_SIZE":            "4096",
		"DNS_SHUTDOWN_TIMEOUT":        "25s",
		"DNS_API_BEARER_TOKEN":        "api-secret",
		"DNS_SNAPSHOT_BEARER_TOKEN":   "snapshot-secret",
		"DNS_SNAPSHOT_CACHE_PATH":     "/cache/last-good.json",
		"DNS_SNAPSHOT_POLL_INTERVAL":  "7s",
		"DNS_SNAPSHOT_MAX_STALENESS":  "45s",
		"DNS_SNAPSHOT_HTTP_TIMEOUT":   "4s",
		"DNS_HTTP_MAX_BODY_BYTES":     "2048",
		"DNS_SNAPSHOT_MAX_BODY_BYTES": "8192",
	}
	got, err := config.Load(func(key string) string { return env[key] })
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := config.Config{
		Role:                 config.RoleAll,
		HTTPAddr:             "127.0.0.1:8081",
		DNSAddr:              ":5353",
		DataPath:             "/data/zones.db",
		Nameservers:          []string{"ns1.example.test", "ns2.example.test"},
		CORSOrigins:          []string{"https://one.example", "https://two.example"},
		BootstrapZone:        "Example.Test.",
		MaxUDPSize:           4096,
		ShutdownTimeout:      25 * time.Second,
		APIBearerToken:       "api-secret",
		SnapshotBearerToken:  "snapshot-secret",
		SnapshotCachePath:    "/cache/last-good.json",
		SnapshotPollInterval: 7 * time.Second,
		SnapshotMaxStaleness: 45 * time.Second,
		SnapshotHTTPTimeout:  4 * time.Second,
		HTTPMaxBodyBytes:     2048,
		SnapshotMaxBodyBytes: 8192,
		PlatformPath:         "/data/platform.db",
		Activity:             config.Activity{MaxEvents: config.DefaultActivityMaxEvents},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Load overrides = %#v, want %#v", got, want)
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{name: "UDP size below minimum", env: map[string]string{"DNS_MAX_UDP_SIZE": "511"}, want: "DNS_MAX_UDP_SIZE must be between 512 and 65535"},
		{name: "UDP size overflow", env: map[string]string{"DNS_MAX_UDP_SIZE": "65536"}, want: "DNS_MAX_UDP_SIZE must be between 512 and 65535"},
		{name: "UDP size malformed", env: map[string]string{"DNS_MAX_UDP_SIZE": "large"}, want: "DNS_MAX_UDP_SIZE must be between 512 and 65535"},
		{name: "zero timeout", env: map[string]string{"DNS_SHUTDOWN_TIMEOUT": "0s"}, want: "DNS_SHUTDOWN_TIMEOUT must be a positive duration"},
		{name: "negative timeout", env: map[string]string{"DNS_SHUTDOWN_TIMEOUT": "-1s"}, want: "DNS_SHUTDOWN_TIMEOUT must be a positive duration"},
		{name: "malformed timeout", env: map[string]string{"DNS_SHUTDOWN_TIMEOUT": "later"}, want: "DNS_SHUTDOWN_TIMEOUT must be a positive duration"},
		{name: "empty nameserver list", env: map[string]string{"DNS_NAMESERVERS": ", ,"}, want: "DNS_NAMESERVERS must contain at least one hostname"},
		{name: "invalid role", env: map[string]string{"DNS_ROLE": "secondary"}, want: "DNS_ROLE must be all, control, or authority"},
		{name: "invalid production flag", env: map[string]string{"DNS_PRODUCTION": "sometimes"}, want: "DNS_PRODUCTION must be true or false"},
		{name: "production placeholder", env: map[string]string{"DNS_PRODUCTION": "true"}, want: "DNS_NAMESERVERS cannot use reserved names when DNS_PRODUCTION=true"},
		{name: "production all API token required", env: map[string]string{"DNS_PRODUCTION": "true", "DNS_NAMESERVERS": "ns1.dns.benebsworth.com"}, want: "DNS_API_BEARER_TOKEN is required for this role"},
		{name: "control API token required", env: map[string]string{"DNS_ROLE": "control", "DNS_NAMESERVERS": "ns1.dns.test"}, want: "DNS_API_BEARER_TOKEN is required for this role"},
		{name: "control snapshot token required", env: map[string]string{"DNS_ROLE": "control", "DNS_NAMESERVERS": "ns1.dns.test", "DNS_API_BEARER_TOKEN": "api"}, want: "DNS_SNAPSHOT_BEARER_TOKEN is required for this role"},
		{name: "control tokens distinct", env: map[string]string{"DNS_ROLE": "control", "DNS_NAMESERVERS": "ns1.dns.test", "DNS_API_BEARER_TOKEN": "same", "DNS_SNAPSHOT_BEARER_TOKEN": "same"}, want: "DNS_API_BEARER_TOKEN and DNS_SNAPSHOT_BEARER_TOKEN must be different"},
		{name: "authority snapshot token required", env: map[string]string{"DNS_ROLE": "authority"}, want: "DNS_SNAPSHOT_BEARER_TOKEN is required for this role"},
		{name: "all nonempty tokens distinct", env: map[string]string{"DNS_API_BEARER_TOKEN": "same", "DNS_SNAPSHOT_BEARER_TOKEN": "same"}, want: "DNS_API_BEARER_TOKEN and DNS_SNAPSHOT_BEARER_TOKEN must be different"},
		{name: "authority URL required", env: map[string]string{"DNS_ROLE": "authority", "DNS_SNAPSHOT_BEARER_TOKEN": "snapshot"}, want: "DNS_SNAPSHOT_URL is required for role authority"},
		{name: "authority URL invalid", env: map[string]string{"DNS_ROLE": "authority", "DNS_SNAPSHOT_BEARER_TOKEN": "snapshot", "DNS_SNAPSHOT_URL": "dns-control:8080"}, want: "DNS_SNAPSHOT_URL must be an absolute HTTP or HTTPS URL without userinfo, a query, or a fragment"},
		{name: "authority URL wrong path", env: map[string]string{"DNS_ROLE": "authority", "DNS_SNAPSHOT_BEARER_TOKEN": "snapshot", "DNS_SNAPSHOT_URL": "http://control/snapshot"}, want: "DNS_SNAPSHOT_URL must point to /internal/v1/snapshot"},
		{name: "production API token too short", env: map[string]string{"DNS_PRODUCTION": "true", "DNS_NAMESERVERS": "ns1.dns.benebsworth.com", "DNS_API_BEARER_TOKEN": "short"}, want: "DNS_API_BEARER_TOKEN must be at least 32 bytes when DNS_PRODUCTION=true"},
		{name: "production snapshot token too short", env: map[string]string{"DNS_ROLE": "authority", "DNS_PRODUCTION": "true", "DNS_SNAPSHOT_BEARER_TOKEN": "short"}, want: "DNS_SNAPSHOT_BEARER_TOKEN must be at least 32 bytes when DNS_PRODUCTION=true"},
		{name: "production authority public HTTP", env: map[string]string{"DNS_ROLE": "authority", "DNS_PRODUCTION": "true", "DNS_SNAPSHOT_BEARER_TOKEN": productionSnapshotToken, "DNS_SNAPSHOT_URL": "http://snapshots.example.com/internal/v1/snapshot"}, want: "DNS_SNAPSHOT_URL must use HTTPS in production unless its host is cluster-local or loopback"},
		{name: "authority cache collides with database", env: map[string]string{"DNS_ROLE": "authority", "DNS_SNAPSHOT_BEARER_TOKEN": "snapshot", "DNS_SNAPSHOT_URL": "http://control/internal/v1/snapshot", "DNS_SNAPSHOT_CACHE_PATH": "./data/dns.db"}, want: "DNS_SNAPSHOT_CACHE_PATH must not equal DNS_DATA_PATH"},
		{name: "poll interval invalid", env: map[string]string{"DNS_SNAPSHOT_POLL_INTERVAL": "0s"}, want: "DNS_SNAPSHOT_POLL_INTERVAL must be a positive duration"},
		{name: "staleness below poll", env: map[string]string{"DNS_ROLE": "authority", "DNS_SNAPSHOT_BEARER_TOKEN": "snapshot", "DNS_SNAPSHOT_URL": "http://control/internal/v1/snapshot", "DNS_SNAPSHOT_POLL_INTERVAL": "10s", "DNS_SNAPSHOT_MAX_STALENESS": "9s"}, want: "DNS_SNAPSHOT_MAX_STALENESS must be at least DNS_SNAPSHOT_POLL_INTERVAL"},
		{name: "HTTP body bytes invalid", env: map[string]string{"DNS_HTTP_MAX_BODY_BYTES": "0"}, want: "DNS_HTTP_MAX_BODY_BYTES must be a positive byte count"},
		{name: "snapshot body bytes invalid", env: map[string]string{"DNS_SNAPSHOT_MAX_BODY_BYTES": "many"}, want: "DNS_SNAPSHOT_MAX_BODY_BYTES must be a positive byte count"},
		{name: "HTTP body bytes excessive", env: map[string]string{"DNS_HTTP_MAX_BODY_BYTES": "67108865"}, want: "DNS_HTTP_MAX_BODY_BYTES must be at most 67108864"},
		{name: "snapshot body bytes excessive", env: map[string]string{"DNS_SNAPSHOT_MAX_BODY_BYTES": "1073741825"}, want: "DNS_SNAPSHOT_MAX_BODY_BYTES must be at most 1073741824"},
		{name: "non-ASCII API token", env: map[string]string{"DNS_API_BEARER_TOKEN": "sëcret"}, want: "DNS_API_BEARER_TOKEN must contain only visible ASCII characters without spaces"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := config.Load(func(key string) string { return tt.env[key] })
			if err == nil {
				t.Fatalf("Load succeeded, want error %q", tt.want)
			}
			if err.Error() != tt.want {
				t.Errorf("Load error = %q, want %q", err, tt.want)
			}
		})
	}
}

func TestLoadSplitRoles(t *testing.T) {
	t.Parallel()

	t.Run("control", func(t *testing.T) {
		t.Parallel()
		env := map[string]string{
			"DNS_ROLE":                  "control",
			"DNS_PRODUCTION":            "true",
			"DNS_NAMESERVERS":           "ns1.dns.benebsworth.com,ns2.dns.benebsworth.com",
			"DNS_API_BEARER_TOKEN":      productionAPIToken,
			"DNS_SNAPSHOT_BEARER_TOKEN": productionSnapshotToken,
		}
		got, err := config.Load(func(key string) string { return env[key] })
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got.Role != config.RoleControl || !got.Production {
			t.Fatalf("role/production = %q/%t", got.Role, got.Production)
		}
	})

	t.Run("authority ignores writer settings", func(t *testing.T) {
		t.Parallel()
		env := map[string]string{
			"DNS_ROLE":                  "authority",
			"DNS_PRODUCTION":            "true",
			"DNS_NAMESERVERS":           ", ,",
			"DNS_SNAPSHOT_BEARER_TOKEN": productionSnapshotToken,
			"DNS_SNAPSHOT_URL":          "http://dns-control:8080/internal/v1/snapshot",
			"DNS_SNAPSHOT_CACHE_PATH":   "/cache/snapshot.json",
		}
		got, err := config.Load(func(key string) string { return env[key] })
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got.Role != config.RoleAuthority || got.SnapshotCachePath != "/cache/snapshot.json" {
			t.Fatalf("authority config = %#v", got)
		}
	})
}

func TestProductionRejectsReservedNameserverSuffixes(t *testing.T) {
	t.Parallel()
	for _, nameserver := range []string{
		"NS1.Example.COM.", "example.net", "deep.ns.example.org.",
		"ns.invalid", "localhost.", "ns.test.",
	} {
		nameserver := nameserver
		t.Run(nameserver, func(t *testing.T) {
			t.Parallel()
			env := map[string]string{
				"DNS_PRODUCTION":       "true",
				"DNS_NAMESERVERS":      nameserver,
				"DNS_API_BEARER_TOKEN": productionAPIToken,
			}
			if _, err := config.Load(func(key string) string { return env[key] }); err == nil || !strings.Contains(err.Error(), "reserved names") {
				t.Fatalf("Load(%q) error = %v", nameserver, err)
			}
		})
	}
}

func TestProductionAuthorityAllowsOnlySecureOrLocalSnapshotURLs(t *testing.T) {
	t.Parallel()
	for _, snapshotURL := range []string{
		"http://dns-control:8080/internal/v1/snapshot",
		"http://dns-control.dns.svc:8080/internal/v1/snapshot",
		"http://dns-control.dns.svc.cluster.local:8080/internal/v1/snapshot",
		"http://127.0.0.1:8080/internal/v1/snapshot",
		"http://[::1]:8080/internal/v1/snapshot",
		"https://snapshots.dns.example/internal/v1/snapshot",
	} {
		snapshotURL := snapshotURL
		t.Run(snapshotURL, func(t *testing.T) {
			t.Parallel()
			env := map[string]string{
				"DNS_ROLE":                  "authority",
				"DNS_PRODUCTION":            "true",
				"DNS_SNAPSHOT_BEARER_TOKEN": productionSnapshotToken,
				"DNS_SNAPSHOT_URL":          snapshotURL,
			}
			if _, err := config.Load(func(key string) string { return env[key] }); err != nil {
				t.Fatalf("Load(%q): %v", snapshotURL, err)
			}
		})
	}
}
