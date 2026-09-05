package config

import (
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Role string

const (
	RoleAll       Role = "all"
	RoleControl   Role = "control"
	RoleAuthority Role = "authority"

	defaultHTTPMaxBodyBytes     = 1 << 20
	defaultSnapshotMaxBodyBytes = 8 << 20
	maximumHTTPMaxBodyBytes     = 64 << 20
	maximumSnapshotMaxBodyBytes = 1 << 30
	snapshotPath                = "/internal/v1/snapshot"
)

type Config struct {
	Role                 Role
	Production           bool
	HTTPAddr             string
	DNSAddr              string
	DataPath             string
	Nameservers          []string
	CORSOrigins          []string
	BootstrapZone        string
	MaxUDPSize           uint16
	ShutdownTimeout      time.Duration
	APIBearerToken       string
	SnapshotBearerToken  string
	SnapshotURL          string
	SnapshotCachePath    string
	SnapshotPollInterval time.Duration
	SnapshotMaxStaleness time.Duration
	SnapshotHTTPTimeout  time.Duration
	HTTPMaxBodyBytes     int64
	SnapshotMaxBodyBytes int64

	// PlatformPath is the bbolt file holding sites, mail bindings,
	// subscriptions and the event log. It is opened only by writer roles and
	// never by dns-restore, so the zone store's bucket invariants are
	// untouched.
	//
	// It is a flat field, not a `Platform` sub-struct: spec2 §1.4 and every
	// call site spell it `cfg.PlatformPath`, and a store path is one value
	// rather than an engine block with its own all-or-nothing validation.
	// spec2 §1.8 has been amended to match.
	PlatformPath string
	Activity     Activity
	Hosting      Hosting
	Mail         Mail
	Billing      Billing
}

func Load(getenv func(string) string) (Config, error) {
	dataPath := envOr(getenv, "DNS_DATA_PATH", "./data/dns.db")
	value := Config{
		Role:                 Role(strings.ToLower(envOr(getenv, "DNS_ROLE", string(RoleAll)))),
		HTTPAddr:             envOr(getenv, "DNS_HTTP_ADDR", ":8080"),
		DNSAddr:              envOr(getenv, "DNS_LISTEN_ADDR", ":1053"),
		DataPath:             dataPath,
		Nameservers:          splitList(envOr(getenv, "DNS_NAMESERVERS", "ns1.example.net,ns2.example.net")),
		CORSOrigins:          splitList(envOr(getenv, "DNS_CORS_ORIGINS", "http://localhost:3000")),
		BootstrapZone:        strings.TrimSpace(getenv("DNS_BOOTSTRAP_ZONE")),
		MaxUDPSize:           1232,
		ShutdownTimeout:      10 * time.Second,
		APIBearerToken:       strings.TrimSpace(getenv("DNS_API_BEARER_TOKEN")),
		SnapshotBearerToken:  strings.TrimSpace(getenv("DNS_SNAPSHOT_BEARER_TOKEN")),
		SnapshotURL:          strings.TrimSpace(getenv("DNS_SNAPSHOT_URL")),
		SnapshotCachePath:    envOr(getenv, "DNS_SNAPSHOT_CACHE_PATH", filepath.Join(filepath.Dir(dataPath), "snapshot.json")),
		SnapshotPollInterval: 5 * time.Second,
		SnapshotMaxStaleness: time.Minute,
		SnapshotHTTPTimeout:  5 * time.Second,
		HTTPMaxBodyBytes:     defaultHTTPMaxBodyBytes,
		SnapshotMaxBodyBytes: defaultSnapshotMaxBodyBytes,
	}

	if raw := strings.TrimSpace(getenv("DNS_PRODUCTION")); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			return Config{}, fmt.Errorf("DNS_PRODUCTION must be true or false")
		}
		value.Production = parsed
	}

	if raw := strings.TrimSpace(getenv("DNS_MAX_UDP_SIZE")); raw != "" {
		parsed, err := strconv.ParseUint(raw, 10, 16)
		if err != nil || parsed < 512 {
			return Config{}, fmt.Errorf("DNS_MAX_UDP_SIZE must be between 512 and 65535")
		}
		value.MaxUDPSize = uint16(parsed)
	}
	if raw := strings.TrimSpace(getenv("DNS_SHUTDOWN_TIMEOUT")); raw != "" {
		parsed, err := positiveDuration("DNS_SHUTDOWN_TIMEOUT", raw)
		if err != nil {
			return Config{}, err
		}
		value.ShutdownTimeout = parsed
	}
	if raw := strings.TrimSpace(getenv("DNS_SNAPSHOT_POLL_INTERVAL")); raw != "" {
		parsed, err := positiveDuration("DNS_SNAPSHOT_POLL_INTERVAL", raw)
		if err != nil {
			return Config{}, err
		}
		value.SnapshotPollInterval = parsed
	}
	if raw := strings.TrimSpace(getenv("DNS_SNAPSHOT_MAX_STALENESS")); raw != "" {
		parsed, err := positiveDuration("DNS_SNAPSHOT_MAX_STALENESS", raw)
		if err != nil {
			return Config{}, err
		}
		value.SnapshotMaxStaleness = parsed
	}
	if raw := strings.TrimSpace(getenv("DNS_SNAPSHOT_HTTP_TIMEOUT")); raw != "" {
		parsed, err := positiveDuration("DNS_SNAPSHOT_HTTP_TIMEOUT", raw)
		if err != nil {
			return Config{}, err
		}
		value.SnapshotHTTPTimeout = parsed
	}
	if raw := strings.TrimSpace(getenv("DNS_HTTP_MAX_BODY_BYTES")); raw != "" {
		parsed, err := positiveBytes("DNS_HTTP_MAX_BODY_BYTES", raw)
		if err != nil {
			return Config{}, err
		}
		value.HTTPMaxBodyBytes = parsed
	}
	if raw := strings.TrimSpace(getenv("DNS_SNAPSHOT_MAX_BODY_BYTES")); raw != "" {
		parsed, err := positiveBytes("DNS_SNAPSHOT_MAX_BODY_BYTES", raw)
		if err != nil {
			return Config{}, err
		}
		value.SnapshotMaxBodyBytes = parsed
	}
	if value.HTTPMaxBodyBytes > maximumHTTPMaxBodyBytes {
		return Config{}, fmt.Errorf("DNS_HTTP_MAX_BODY_BYTES must be at most %d", maximumHTTPMaxBodyBytes)
	}
	if value.SnapshotMaxBodyBytes > maximumSnapshotMaxBodyBytes {
		return Config{}, fmt.Errorf("DNS_SNAPSHOT_MAX_BODY_BYTES must be at most %d", maximumSnapshotMaxBodyBytes)
	}

	switch value.Role {
	case RoleAll, RoleControl, RoleAuthority:
	default:
		return Config{}, fmt.Errorf("DNS_ROLE must be all, control, or authority")
	}
	if value.HTTPAddr == "" {
		return Config{}, fmt.Errorf("DNS_HTTP_ADDR cannot be empty")
	}
	if (value.Role == RoleAll || value.Role == RoleAuthority) && value.DNSAddr == "" {
		return Config{}, fmt.Errorf("DNS_LISTEN_ADDR cannot be empty for role %s", value.Role)
	}
	if (value.Role == RoleAll || value.Role == RoleControl) && value.DataPath == "" {
		return Config{}, fmt.Errorf("DNS_DATA_PATH cannot be empty for role %s", value.Role)
	}
	if (value.Role == RoleAll || value.Role == RoleControl) && len(value.Nameservers) == 0 {
		return Config{}, fmt.Errorf("DNS_NAMESERVERS must contain at least one hostname")
	}
	if value.Production && (value.Role == RoleAll || value.Role == RoleControl) {
		for _, nameserver := range value.Nameservers {
			if reservedNameserver(nameserver) {
				return Config{}, fmt.Errorf("DNS_NAMESERVERS cannot use reserved names when DNS_PRODUCTION=true")
			}
		}
	}
	if err := validateBearerToken("DNS_API_BEARER_TOKEN", value.APIBearerToken, value.Role == RoleControl || (value.Role == RoleAll && value.Production)); err != nil {
		return Config{}, err
	}
	if err := validateBearerToken("DNS_SNAPSHOT_BEARER_TOKEN", value.SnapshotBearerToken, value.Role == RoleControl || value.Role == RoleAuthority); err != nil {
		return Config{}, err
	}
	if value.Production {
		if err := validateProductionToken("DNS_API_BEARER_TOKEN", value.APIBearerToken); err != nil {
			return Config{}, err
		}
		if err := validateProductionToken("DNS_SNAPSHOT_BEARER_TOKEN", value.SnapshotBearerToken); err != nil {
			return Config{}, err
		}
	}
	if value.APIBearerToken != "" && value.SnapshotBearerToken != "" && value.APIBearerToken == value.SnapshotBearerToken {
		return Config{}, fmt.Errorf("DNS_API_BEARER_TOKEN and DNS_SNAPSHOT_BEARER_TOKEN must be different")
	}
	if value.Role == RoleAuthority {
		if value.SnapshotURL == "" {
			return Config{}, fmt.Errorf("DNS_SNAPSHOT_URL is required for role authority")
		}
		parsed, err := url.Parse(value.SnapshotURL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || parsed.RawQuery != "" {
			return Config{}, fmt.Errorf("DNS_SNAPSHOT_URL must be an absolute HTTP or HTTPS URL without userinfo, a query, or a fragment")
		}
		if parsed.Path != snapshotPath {
			return Config{}, fmt.Errorf("DNS_SNAPSHOT_URL must point to %s", snapshotPath)
		}
		if value.Production && parsed.Scheme == "http" && !clusterLocalHost(parsed.Hostname()) {
			return Config{}, fmt.Errorf("DNS_SNAPSHOT_URL must use HTTPS in production unless its host is cluster-local or loopback")
		}
		if value.SnapshotCachePath == "" {
			return Config{}, fmt.Errorf("DNS_SNAPSHOT_CACHE_PATH is required for role authority")
		}
		if filepath.Clean(value.SnapshotCachePath) == filepath.Clean(value.DataPath) {
			return Config{}, fmt.Errorf("DNS_SNAPSHOT_CACHE_PATH must not equal DNS_DATA_PATH")
		}
		if value.SnapshotMaxStaleness < value.SnapshotPollInterval {
			return Config{}, fmt.Errorf("DNS_SNAPSHOT_MAX_STALENESS must be at least DNS_SNAPSHOT_POLL_INTERVAL")
		}
	}

	if value.Role == RoleAuthority {
		// An authority is a read-only snapshot consumer. It holds no engine
		// credential and mounts none of the platform routes, so an engine
		// variable here is a misconfiguration worth failing on rather than a
		// value to ignore silently.
		if err := rejectEngineEnv(getenv, value.Role); err != nil {
			return Config{}, err
		}
		return value, nil
	}

	value.PlatformPath = envOr(getenv, "DNS_PLATFORM_PATH", filepath.Join(filepath.Dir(dataPath), "platform.db"))
	if value.PlatformPath == "" {
		return Config{}, fmt.Errorf("DNS_PLATFORM_PATH cannot be empty for role %s", value.Role)
	}
	if filepath.Clean(value.PlatformPath) == filepath.Clean(value.DataPath) {
		return Config{}, fmt.Errorf("DNS_PLATFORM_PATH must not equal DNS_DATA_PATH")
	}
	if filepath.Clean(value.PlatformPath) == filepath.Clean(value.SnapshotCachePath) {
		return Config{}, fmt.Errorf("DNS_PLATFORM_PATH must not equal DNS_SNAPSHOT_CACHE_PATH")
	}

	activity, err := loadActivity(getenv)
	if err != nil {
		return Config{}, err
	}
	value.Activity = activity

	hosting, err := loadHosting(getenv, value.Production)
	if err != nil {
		return Config{}, err
	}
	value.Hosting = hosting

	mail, err := loadMail(getenv, value.Production)
	if err != nil {
		return Config{}, err
	}
	value.Mail = mail

	billing, err := loadBilling(getenv, value.Production, value.PlatformPath)
	if err != nil {
		return Config{}, err
	}
	value.Billing = billing

	// The new services spend engine credentials, return a plaintext mailbox
	// password once, and destroy apps and mailboxes. An unauthenticated control
	// API in front of them is not an acceptable local convenience.
	if value.APIBearerToken == "" && spendsCredentials(value) {
		return Config{}, fmt.Errorf("DNS_API_BEARER_TOKEN is required when a real hosting or mail engine is configured, or when BILLING_PROVIDER=%s", BillingProviderStripe)
	}
	if err := validateUploadDir(value.Hosting, value.DataPath, value.Production); err != nil {
		return Config{}, err
	}
	return value, nil
}

// RequiresOperatorToken reports whether this configuration may not be served
// without DNS_API_BEARER_TOKEN. Load already refuses such a configuration; the
// predicate is exported so the place that mounts the routes can refuse it too,
// and so the two can never drift apart.
func RequiresOperatorToken(value Config) bool { return spendsCredentials(value) }

// spendsCredentials reports whether this process can reach something real. The
// credential being spent is reachability to an engine, not the bearer that
// happens to authenticate it: HOSTING_API_TOKEN is optional for a loopback or
// cluster-local DeepHost (loadHosting), and keying on it left a fully live
// engine — one that creates and destroys apps, and answers on the shared
// cluster — behind an unauthenticated control API.
func spendsCredentials(value Config) bool {
	if value.Hosting.Configured() && !value.Hosting.Fake() {
		return true
	}
	if value.Mail.Configured() && !value.Mail.Fake() {
		return true
	}
	return value.Billing.Provider == BillingProviderStripe
}

func positiveDuration(name, raw string) (time.Duration, error) {
	parsed, err := time.ParseDuration(raw)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", name)
	}
	return parsed, nil
}

func positiveBytes(name, raw string) (int64, error) {
	parsed, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive byte count", name)
	}
	return parsed, nil
}

func validateBearerToken(name, value string, required bool) error {
	if value == "" {
		if required {
			return fmt.Errorf("%s is required for this role", name)
		}
		return nil
	}
	if len(value) > 4096 {
		return fmt.Errorf("%s must be at most 4096 bytes", name)
	}
	for _, char := range value {
		if char <= 0x20 || char > 0x7e {
			return fmt.Errorf("%s must contain only visible ASCII characters without spaces", name)
		}
	}
	return nil
}

func validateProductionToken(name, value string) error {
	if value != "" && len(value) < 32 {
		return fmt.Errorf("%s must be at least 32 bytes when DNS_PRODUCTION=true", name)
	}
	return nil
}

func reservedNameserver(value string) bool {
	value = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(value), "."))
	for _, suffix := range []string{"example.com", "example.net", "example.org", "invalid", "localhost", "test"} {
		if value == suffix || strings.HasSuffix(value, "."+suffix) {
			return true
		}
	}
	return false
}

func clusterLocalHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "localhost" {
		return true
	}
	if address := net.ParseIP(host); address != nil {
		return address.IsLoopback()
	}
	return !strings.Contains(host, ".") || strings.HasSuffix(host, ".svc") || strings.HasSuffix(host, ".svc.cluster.local")
}

func envOr(getenv func(string) string, key, fallback string) string {
	if value := strings.TrimSpace(getenv(key)); value != "" {
		return value
	}
	return fallback
}

func splitList(value string) []string {
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}
