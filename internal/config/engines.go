package config

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// FakeURL is the sentinel a local operator sets for HOSTING_API_URL or
// MAIL_API_URL to select the in-process fake engine. It is rejected in
// production so a real deployment can never serve fabricated data.
const FakeURL = "fake://"

// Provider names for BILLING_PROVIDER.
const (
	BillingProviderStripe = "stripe"
	BillingProviderFake   = "fake"
)

// Defaults for the engine blocks. They are exported so the chart, the tests and
// the .env.example documentation can all name one source.
const (
	DefaultActivityMaxEvents   = 50_000
	DefaultHostingTenant       = "simple"
	DefaultBuildDeadline       = 40 * time.Minute
	DefaultUploadMaxBytes      = int64(64 << 20)
	DefaultMailMXPriority      = uint16(10)
	DefaultMailDMARCPolicy     = "quarantine"
	DefaultMailboxesPerDomain  = 5
	DefaultMailQuotaBytes      = int64(5 << 30)
	DefaultStripeAPIBase       = "https://api.stripe.com"
	DefaultBillingPublicURL    = "http://localhost:3000"
	DefaultBillingFakeAddr     = "127.0.0.1:8087"
	DefaultFakeStripePriceID   = "price_fake_domain_monthly"
	DefaultFakeStripeSecretKey = "sk_test_fake"
	DefaultFakeWebhookSecret   = "whsec_fake_local_0000000000000000"
)

// Activity bounds the event ring.
type Activity struct {
	MaxEvents int
}

// Hosting is the DeepHost facade's configuration. Configured reports whether
// the block is present at all; Load has already refused a partial block, so a
// configured block is a complete one.
type Hosting struct {
	APIURL              string
	APIToken            string
	Tenant              string
	GatewayHostname     string
	GatewayAddresses    []netip.Addr
	GatewayAllowedCIDRs []netip.Prefix
	GatewayResolver     string
	AppsSuffix          string
	AppsTLSSecret       string
	BuildDeadline       time.Duration
	UploadsEnabled      bool
	UploadMaxBytes      int64
	UploadMaxTotalBytes int64
	UploadDir           string
	// Production mirrors Config.Production. The hosting facade needs it for
	// the gateway address filter (spec2 §3.5) — a loopback answer is honest
	// locally and must be dropped in production — and hosting.New receives
	// only this block, so the flag is carried here rather than re-read from
	// the environment.
	Production bool
}

func (h Hosting) Configured() bool { return h.APIURL != "" }

// Fake reports whether the block selects the in-process fake engine.
func (h Hosting) Fake() bool { return h.APIURL == FakeURL }

// MissingEnv names the variables an operator must set to configure hosting. It
// never contains a value.
func (h Hosting) MissingEnv() []string {
	if h.Configured() {
		return nil
	}
	return []string{"HOSTING_API_URL", "HOSTING_API_TOKEN", "HOSTING_GATEWAY_HOSTNAME"}
}

// EndpointHost is the host[:port] of the engine, with no scheme, path or
// userinfo, so it is safe to show an operator and to log.
func (h Hosting) EndpointHost() string { return endpointHost(h.APIURL) }

// Mail is the Stalwart facade's configuration.
type Mail struct {
	APIURL             string
	APIToken           string
	Hostname           string
	MXPriority         uint16
	SPFInclude         string
	DMARCPolicy        string
	ReportAddress      string
	MailboxesPerDomain int
	DefaultQuotaBytes  int64

	// WebhookSecret authenticates the mail server's delivery-event deliveries.
	// It is empty by default: with no secret the receiver is not mounted, no
	// delivery counters exist and the console keeps saying the numbers are not
	// available. Setting it is what turns the feature on, on both sides.
	WebhookSecret string

	// WebhookSignatureKey is the mail server's signatureKey, which on Stalwart
	// is a setting of its own, unrelated to the httpAuth bearer above. Left
	// empty, this deployment has only one value for both, so a signature is
	// keyed with the bearer and adds nothing to it; set, it is a key the caller
	// must hold as well as the bearer, and an unsigned delivery is refused.
	WebhookSignatureKey string
}

// SignatureKey is the key an X-Signature is verified with: the separate
// signature key when this deployment configures one, and otherwise the bearer,
// which is what a mail server configured with a single value signs with.
func (m Mail) SignatureKey() string {
	if m.WebhookSignatureKey != "" {
		return m.WebhookSignatureKey
	}
	return m.WebhookSecret
}

// RequireSignature reports whether a delivery must carry an X-Signature. It is
// true exactly when the signature has a key of its own: only then does the
// header prove anything the bearer did not already prove.
func (m Mail) RequireSignature() bool { return m.WebhookSignatureKey != "" }

func (m Mail) Configured() bool { return m.APIURL != "" }

// DeliveryEventsConfigured reports whether this deployment accepts delivery
// events from the mail server. Nothing counts, stores or shows a delivered or
// bounced figure while it is false.
func (m Mail) DeliveryEventsConfigured() bool { return m.Configured() && m.WebhookSecret != "" }

func (m Mail) Fake() bool { return m.APIURL == FakeURL }

func (m Mail) MissingEnv() []string {
	if m.Configured() {
		return nil
	}
	return []string{"MAIL_API_URL", "MAIL_API_TOKEN", "MAIL_HOSTNAME"}
}

func (m Mail) EndpointHost() string { return endpointHost(m.APIURL) }

// Billing is the Stripe facade's configuration. WebhookSecrets holds one to
// three signing secrets so a rotation's overlap needs no restart.
type Billing struct {
	Provider       string
	SecretKey      string
	WebhookSecrets []string
	PriceID        string
	APIBase        string
	PublicURL      string
	CustomerEmail  string
	FakeAddr       string
	FakeStatePath  string
}

func (b Billing) Configured() bool { return b.Provider != "" }

func (b Billing) Fake() bool { return b.Provider == BillingProviderFake }

func (b Billing) MissingEnv() []string {
	if b.Configured() {
		return nil
	}
	return []string{"BILLING_PROVIDER", "STRIPE_SECRET_KEY", "STRIPE_WEBHOOK_SECRET", "STRIPE_PRICE_ID", "BILLING_CUSTOMER_EMAIL"}
}

func (b Billing) EndpointHost() string { return endpointHost(b.APIBase) }

// engineEnv is every variable an engine block owns. The authority role rejects
// all of them, and the list is the only place a new variable has to be added
// for that rejection to cover it.
var engineEnv = []string{
	"DNS_PLATFORM_PATH",
	"ACTIVITY_MAX_EVENTS",
	"HOSTING_API_URL",
	"HOSTING_API_TOKEN",
	"HOSTING_TENANT",
	"HOSTING_GATEWAY_HOSTNAME",
	"HOSTING_GATEWAY_ADDRESSES",
	"HOSTING_GATEWAY_ALLOWED_CIDRS",
	"HOSTING_GATEWAY_RESOLVER",
	"HOSTING_APPS_SUFFIX",
	"HOSTING_APPS_TLS_SECRET",
	"HOSTING_BUILD_DEADLINE",
	"HOSTING_UPLOADS_ENABLED",
	"HOSTING_UPLOAD_MAX_BYTES",
	"HOSTING_UPLOAD_MAX_TOTAL_BYTES",
	"HOSTING_UPLOAD_DIR",
	"MAIL_API_URL",
	"MAIL_API_TOKEN",
	"MAIL_HOSTNAME",
	"MAIL_MX_PRIORITY",
	"MAIL_SPF_INCLUDE",
	"MAIL_DMARC_POLICY",
	"MAIL_REPORT_ADDRESS",
	"MAIL_MAILBOXES_PER_DOMAIN",
	"MAIL_DEFAULT_QUOTA_BYTES",
	"MAIL_WEBHOOK_SECRET",
	"STRIPE_SECRET_KEY",
	"STRIPE_WEBHOOK_SECRET",
	"STRIPE_PRICE_ID",
	"STRIPE_API_BASE",
	"BILLING_PROVIDER",
	"BILLING_PUBLIC_URL",
	"BILLING_CUSTOMER_EMAIL",
	"BILLING_FAKE_ADDR",
	"BILLING_FAKE_STATE_PATH",
}

// hostingEnv is every HOSTING_* variable, used to detect a partial block.
var hostingEnv = []string{
	"HOSTING_API_TOKEN", "HOSTING_TENANT", "HOSTING_GATEWAY_HOSTNAME", "HOSTING_GATEWAY_ADDRESSES",
	"HOSTING_GATEWAY_ALLOWED_CIDRS", "HOSTING_GATEWAY_RESOLVER", "HOSTING_APPS_SUFFIX",
	"HOSTING_APPS_TLS_SECRET", "HOSTING_BUILD_DEADLINE", "HOSTING_UPLOADS_ENABLED",
	"HOSTING_UPLOAD_MAX_BYTES", "HOSTING_UPLOAD_MAX_TOTAL_BYTES", "HOSTING_UPLOAD_DIR",
}

var mailEnv = []string{
	"MAIL_API_TOKEN", "MAIL_HOSTNAME", "MAIL_MX_PRIORITY", "MAIL_SPF_INCLUDE", "MAIL_DMARC_POLICY",
	"MAIL_REPORT_ADDRESS", "MAIL_MAILBOXES_PER_DOMAIN", "MAIL_DEFAULT_QUOTA_BYTES",
	"MAIL_WEBHOOK_SECRET",
}

var billingEnv = []string{
	"STRIPE_SECRET_KEY", "STRIPE_WEBHOOK_SECRET", "STRIPE_PRICE_ID", "STRIPE_API_BASE",
	"BILLING_PUBLIC_URL", "BILLING_CUSTOMER_EMAIL", "BILLING_FAKE_ADDR", "BILLING_FAKE_STATE_PATH",
}

func rejectEngineEnv(getenv func(string) string, role Role) error {
	for _, name := range engineEnv {
		if strings.TrimSpace(getenv(name)) != "" {
			return fmt.Errorf("%s is not used by role %s", name, role)
		}
	}
	return nil
}

func firstSet(getenv func(string) string, names []string) string {
	for _, name := range names {
		if strings.TrimSpace(getenv(name)) != "" {
			return name
		}
	}
	return ""
}

func loadActivity(getenv func(string) string) (Activity, error) {
	value := Activity{MaxEvents: DefaultActivityMaxEvents}
	if raw := strings.TrimSpace(getenv("ACTIVITY_MAX_EVENTS")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1000 || parsed > 1_000_000 {
			return Activity{}, fmt.Errorf("ACTIVITY_MAX_EVENTS must be between 1000 and 1000000")
		}
		value.MaxEvents = parsed
	}
	return value, nil
}

func loadHosting(getenv func(string) string, production bool) (Hosting, error) {
	apiURL := strings.TrimSpace(getenv("HOSTING_API_URL"))
	if apiURL == "" {
		if name := firstSet(getenv, hostingEnv); name != "" {
			return Hosting{}, fmt.Errorf("HOSTING_API_URL is required when %s is set", name)
		}
		return Hosting{}, nil
	}

	value := Hosting{
		APIURL:          apiURL,
		APIToken:        strings.TrimSpace(getenv("HOSTING_API_TOKEN")),
		Tenant:          envOr(getenv, "HOSTING_TENANT", DefaultHostingTenant),
		GatewayResolver: strings.TrimSpace(getenv("HOSTING_GATEWAY_RESOLVER")),
		AppsSuffix:      strings.TrimSpace(getenv("HOSTING_APPS_SUFFIX")),
		AppsTLSSecret:   strings.TrimSpace(getenv("HOSTING_APPS_TLS_SECRET")),
		BuildDeadline:   DefaultBuildDeadline,
		UploadMaxBytes:  DefaultUploadMaxBytes,
		UploadDir:       envOr(getenv, "HOSTING_UPLOAD_DIR", filepath.Join(os.TempDir(), "simpledns-uploads")),
		Production:      production,
	}

	if apiURL != FakeURL {
		parsed, err := url.Parse(apiURL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" ||
			parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return Hosting{}, fmt.Errorf("HOSTING_API_URL must be an absolute HTTP or HTTPS URL without userinfo, a query, or a fragment")
		}
		if production && parsed.Scheme == "http" && !clusterLocalHost(parsed.Hostname()) {
			return Hosting{}, fmt.Errorf("HOSTING_API_URL must use HTTPS in production unless its host is cluster-local or loopback")
		}
		if value.APIToken == "" && !clusterLocalHost(parsed.Hostname()) {
			return Hosting{}, fmt.Errorf("HOSTING_API_TOKEN is required when HOSTING_API_URL is not cluster-local or loopback")
		}
	} else if production {
		return Hosting{}, fmt.Errorf("HOSTING_API_URL must not be %s when DNS_PRODUCTION=true", FakeURL)
	}

	if err := validateBearerToken("HOSTING_API_TOKEN", value.APIToken, false); err != nil {
		return Hosting{}, err
	}
	if production {
		if err := validateProductionToken("HOSTING_API_TOKEN", value.APIToken); err != nil {
			return Hosting{}, err
		}
	}
	if !validTenant(value.Tenant) {
		return Hosting{}, fmt.Errorf("HOSTING_TENANT must be 1-24 characters matching ^[a-z0-9]([a-z0-9-]{0,22}[a-z0-9])?$")
	}

	if raw := strings.TrimSpace(getenv("HOSTING_GATEWAY_HOSTNAME")); raw != "" {
		if !validHostname(raw) {
			return Hosting{}, fmt.Errorf("HOSTING_GATEWAY_HOSTNAME must be a hostname")
		}
		value.GatewayHostname = strings.TrimSuffix(strings.ToLower(raw), ".")
	}
	for _, raw := range splitList(getenv("HOSTING_GATEWAY_ADDRESSES")) {
		address, err := netip.ParseAddr(raw)
		if err != nil {
			return Hosting{}, fmt.Errorf("HOSTING_GATEWAY_ADDRESSES must be a comma-separated list of IP addresses")
		}
		address = address.Unmap()
		if production && !routableAddress(address) {
			return Hosting{}, fmt.Errorf("HOSTING_GATEWAY_ADDRESSES must contain only global unicast addresses when DNS_PRODUCTION=true")
		}
		value.GatewayAddresses = append(value.GatewayAddresses, address)
	}
	if value.GatewayHostname == "" && len(value.GatewayAddresses) == 0 {
		return Hosting{}, fmt.Errorf("HOSTING_GATEWAY_HOSTNAME or HOSTING_GATEWAY_ADDRESSES is required when HOSTING_API_URL is set")
	}
	for _, raw := range splitList(getenv("HOSTING_GATEWAY_ALLOWED_CIDRS")) {
		prefix, err := netip.ParsePrefix(raw)
		if err != nil {
			return Hosting{}, fmt.Errorf("HOSTING_GATEWAY_ALLOWED_CIDRS must be a comma-separated list of CIDR prefixes")
		}
		value.GatewayAllowedCIDRs = append(value.GatewayAllowedCIDRs, prefix.Masked())
	}
	if value.GatewayResolver != "" {
		host, port, err := net.SplitHostPort(value.GatewayResolver)
		if err != nil || host == "" || port == "" {
			return Hosting{}, fmt.Errorf("HOSTING_GATEWAY_RESOLVER must be host:port")
		}
	}
	if value.AppsSuffix != "" {
		if !validHostname(value.AppsSuffix) {
			return Hosting{}, fmt.Errorf("HOSTING_APPS_SUFFIX must be a hostname")
		}
		value.AppsSuffix = strings.TrimSuffix(strings.ToLower(value.AppsSuffix), ".")
		if value.AppsTLSSecret == "" {
			return Hosting{}, fmt.Errorf("HOSTING_APPS_TLS_SECRET is required when HOSTING_APPS_SUFFIX is set")
		}
	}
	if value.AppsTLSSecret != "" && !validKubernetesName(value.AppsTLSSecret) {
		return Hosting{}, fmt.Errorf("HOSTING_APPS_TLS_SECRET must be a Kubernetes object name")
	}

	if raw := strings.TrimSpace(getenv("HOSTING_BUILD_DEADLINE")); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil || parsed < 5*time.Minute || parsed > 6*time.Hour {
			return Hosting{}, fmt.Errorf("HOSTING_BUILD_DEADLINE must be a duration between 5m and 6h")
		}
		value.BuildDeadline = parsed
	}
	if raw := strings.TrimSpace(getenv("HOSTING_UPLOADS_ENABLED")); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			return Hosting{}, fmt.Errorf("HOSTING_UPLOADS_ENABLED must be true or false")
		}
		value.UploadsEnabled = parsed
	}
	if raw := strings.TrimSpace(getenv("HOSTING_UPLOAD_MAX_BYTES")); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 1<<20 || parsed > 512<<20 {
			return Hosting{}, fmt.Errorf("HOSTING_UPLOAD_MAX_BYTES must be between 1048576 and 536870912")
		}
		value.UploadMaxBytes = parsed
	}
	value.UploadMaxTotalBytes = 2 * value.UploadMaxBytes
	if raw := strings.TrimSpace(getenv("HOSTING_UPLOAD_MAX_TOTAL_BYTES")); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < value.UploadMaxBytes || parsed > 4<<30 {
			return Hosting{}, fmt.Errorf("HOSTING_UPLOAD_MAX_TOTAL_BYTES must be at least HOSTING_UPLOAD_MAX_BYTES and at most 4294967296")
		}
		value.UploadMaxTotalBytes = parsed
	}
	return value, nil
}

// validateUploadDir is separate from loadHosting because it touches the
// filesystem: it is called once at startup, never from a pure-parse test.
func validateUploadDir(hosting Hosting, dataPath string, production bool) error {
	if !hosting.Configured() || !hosting.UploadsEnabled {
		return nil
	}
	if err := os.MkdirAll(hosting.UploadDir, 0o700); err != nil {
		return fmt.Errorf("HOSTING_UPLOAD_DIR is not writable")
	}
	probe, err := os.CreateTemp(hosting.UploadDir, ".writable-*")
	if err != nil {
		return fmt.Errorf("HOSTING_UPLOAD_DIR is not writable")
	}
	name := probe.Name()
	closeErr := probe.Close()
	removeErr := os.Remove(name)
	if closeErr != nil || removeErr != nil {
		return fmt.Errorf("HOSTING_UPLOAD_DIR is not writable")
	}
	if !production {
		return nil
	}
	same, known := sameFilesystem(hosting.UploadDir, filepath.Dir(dataPath))
	if known && same {
		return fmt.Errorf("HOSTING_UPLOAD_DIR must not share a filesystem with DNS_DATA_PATH when DNS_PRODUCTION=true")
	}
	return nil
}

func loadMail(getenv func(string) string, production bool) (Mail, error) {
	apiURL := strings.TrimSpace(getenv("MAIL_API_URL"))
	if apiURL == "" {
		if name := firstSet(getenv, mailEnv); name != "" {
			return Mail{}, fmt.Errorf("MAIL_API_URL is required when %s is set", name)
		}
		return Mail{}, nil
	}

	value := Mail{
		APIURL:             apiURL,
		APIToken:           strings.TrimSpace(getenv("MAIL_API_TOKEN")),
		Hostname:           strings.TrimSpace(getenv("MAIL_HOSTNAME")),
		MXPriority:         DefaultMailMXPriority,
		SPFInclude:         strings.TrimSpace(getenv("MAIL_SPF_INCLUDE")),
		DMARCPolicy:        envOr(getenv, "MAIL_DMARC_POLICY", DefaultMailDMARCPolicy),
		ReportAddress:      strings.TrimSpace(getenv("MAIL_REPORT_ADDRESS")),
		MailboxesPerDomain: DefaultMailboxesPerDomain,
		DefaultQuotaBytes:  DefaultMailQuotaBytes,
		WebhookSecret:      strings.TrimSpace(getenv("MAIL_WEBHOOK_SECRET")),

		WebhookSignatureKey: strings.TrimSpace(getenv("MAIL_WEBHOOK_SIGNATURE_KEY")),
	}

	if apiURL != FakeURL {
		parsed, err := url.Parse(apiURL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" ||
			parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return Mail{}, fmt.Errorf("MAIL_API_URL must be an absolute HTTP or HTTPS URL without userinfo, a query, or a fragment")
		}
		if production && parsed.Scheme == "http" && !clusterLocalHost(parsed.Hostname()) {
			return Mail{}, fmt.Errorf("MAIL_API_URL must use HTTPS in production unless its host is cluster-local or loopback")
		}
	} else if production {
		return Mail{}, fmt.Errorf("MAIL_API_URL must not be %s when DNS_PRODUCTION=true", FakeURL)
	}

	if value.APIToken == "" {
		return Mail{}, fmt.Errorf("MAIL_API_TOKEN is required when MAIL_API_URL is set")
	}
	if err := validateBearerToken("MAIL_API_TOKEN", value.APIToken, true); err != nil {
		return Mail{}, err
	}
	// The same production floor the two DNS bearers, HOSTING_API_TOKEN and
	// MAIL_WEBHOOK_SECRET carry. This is the credential that creates and
	// destroys mailboxes on Stalwart; it had no floor at all.
	if production {
		if err := validateProductionToken("MAIL_API_TOKEN", value.APIToken); err != nil {
			return Mail{}, err
		}
	}
	if value.Hostname == "" {
		return Mail{}, fmt.Errorf("MAIL_HOSTNAME is required when MAIL_API_URL is set")
	}
	if !validHostname(value.Hostname) {
		return Mail{}, fmt.Errorf("MAIL_HOSTNAME must be a hostname")
	}
	value.Hostname = strings.TrimSuffix(strings.ToLower(value.Hostname), ".")

	if raw := strings.TrimSpace(getenv("MAIL_MX_PRIORITY")); raw != "" {
		parsed, err := strconv.ParseUint(raw, 10, 16)
		if err != nil {
			return Mail{}, fmt.Errorf("MAIL_MX_PRIORITY must be between 0 and 65535")
		}
		value.MXPriority = uint16(parsed)
	}
	if value.SPFInclude != "" {
		if !validHostname(value.SPFInclude) {
			return Mail{}, fmt.Errorf("MAIL_SPF_INCLUDE must be a hostname")
		}
		value.SPFInclude = strings.TrimSuffix(strings.ToLower(value.SPFInclude), ".")
	}
	switch value.DMARCPolicy {
	case "none", "quarantine", "reject":
	default:
		return Mail{}, fmt.Errorf("MAIL_DMARC_POLICY must be none, quarantine, or reject")
	}
	if value.ReportAddress != "" && !validEmail(value.ReportAddress) {
		return Mail{}, fmt.Errorf("MAIL_REPORT_ADDRESS must be an email address")
	}
	if raw := strings.TrimSpace(getenv("MAIL_MAILBOXES_PER_DOMAIN")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 500 {
			return Mail{}, fmt.Errorf("MAIL_MAILBOXES_PER_DOMAIN must be between 1 and 500")
		}
		value.MailboxesPerDomain = parsed
	}
	if raw := strings.TrimSpace(getenv("MAIL_DEFAULT_QUOTA_BYTES")); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 64<<20 || parsed > 1<<40 {
			return Mail{}, fmt.Errorf("MAIL_DEFAULT_QUOTA_BYTES must be between 67108864 and 1099511627776")
		}
		value.DefaultQuotaBytes = parsed
	}
	// The secret is compared against a bearer the mail server sends and used as
	// an HMAC key, so it gets the same shape rules as every other bearer here,
	// and the same production length floor: it is the only credential on an
	// otherwise unauthenticated route.
	if err := validateBearerToken("MAIL_WEBHOOK_SECRET", value.WebhookSecret, false); err != nil {
		return Mail{}, err
	}
	if production {
		if err := validateProductionToken("MAIL_WEBHOOK_SECRET", value.WebhookSecret); err != nil {
			return Mail{}, err
		}
	}
	// The signature key is optional and independent: Stalwart's signatureKey is
	// a separate setting from its httpAuth bearer, so a deployment that sets
	// them to two different values says so here, and the receiver then requires
	// every delivery to be signed with this one. Without it the two sides share
	// one value and the signature is keyed with the bearer.
	if err := validateBearerToken("MAIL_WEBHOOK_SIGNATURE_KEY", value.WebhookSignatureKey, false); err != nil {
		return Mail{}, err
	}
	if value.WebhookSignatureKey != "" && value.WebhookSecret == "" {
		return Mail{}, fmt.Errorf("MAIL_WEBHOOK_SIGNATURE_KEY requires MAIL_WEBHOOK_SECRET: without the bearer no delivery-event receiver is mounted")
	}
	if production {
		if err := validateProductionToken("MAIL_WEBHOOK_SIGNATURE_KEY", value.WebhookSignatureKey); err != nil {
			return Mail{}, err
		}
	}
	return value, nil
}

func loadBilling(getenv func(string) string, production bool, platformPath string) (Billing, error) {
	provider := strings.ToLower(strings.TrimSpace(getenv("BILLING_PROVIDER")))
	if provider == "" {
		if name := firstSet(getenv, billingEnv); name != "" {
			return Billing{}, fmt.Errorf("BILLING_PROVIDER is required when %s is set", name)
		}
		return Billing{}, nil
	}
	switch provider {
	case BillingProviderStripe:
	case BillingProviderFake:
		if production {
			return Billing{}, fmt.Errorf("BILLING_PROVIDER must not be %s when DNS_PRODUCTION=true", BillingProviderFake)
		}
	default:
		return Billing{}, fmt.Errorf("BILLING_PROVIDER must be empty, stripe, or fake")
	}

	value := Billing{
		Provider:      provider,
		SecretKey:     strings.TrimSpace(getenv("STRIPE_SECRET_KEY")),
		PriceID:       strings.TrimSpace(getenv("STRIPE_PRICE_ID")),
		APIBase:       envOr(getenv, "STRIPE_API_BASE", DefaultStripeAPIBase),
		PublicURL:     strings.TrimSpace(getenv("BILLING_PUBLIC_URL")),
		CustomerEmail: strings.TrimSpace(getenv("BILLING_CUSTOMER_EMAIL")),
		FakeAddr:      envOr(getenv, "BILLING_FAKE_ADDR", DefaultBillingFakeAddr),
		FakeStatePath: envOr(getenv, "BILLING_FAKE_STATE_PATH", filepath.Join(filepath.Dir(platformPath), "fakestripe.json")),
	}
	value.WebhookSecrets = append(value.WebhookSecrets, splitList(getenv("STRIPE_WEBHOOK_SECRET"))...)

	if value.PublicURL == "" && !production {
		value.PublicURL = DefaultBillingPublicURL
	}
	if value.PublicURL == "" {
		return Billing{}, fmt.Errorf("BILLING_PUBLIC_URL is required when BILLING_PROVIDER is set")
	}
	publicURL, err := url.Parse(value.PublicURL)
	if err != nil || (publicURL.Scheme != "http" && publicURL.Scheme != "https") || publicURL.Host == "" ||
		publicURL.User != nil || publicURL.RawQuery != "" || publicURL.Fragment != "" {
		return Billing{}, fmt.Errorf("BILLING_PUBLIC_URL must be an absolute HTTP or HTTPS URL without userinfo, a query, or a fragment")
	}
	if production && publicURL.Scheme != "https" {
		return Billing{}, fmt.Errorf("BILLING_PUBLIC_URL must use HTTPS when DNS_PRODUCTION=true")
	}
	value.PublicURL = strings.TrimSuffix(value.PublicURL, "/")

	if production && value.APIBase != DefaultStripeAPIBase {
		return Billing{}, fmt.Errorf("STRIPE_API_BASE must be %s when DNS_PRODUCTION=true", DefaultStripeAPIBase)
	}

	switch provider {
	case BillingProviderStripe:
		if value.SecretKey == "" {
			return Billing{}, fmt.Errorf("STRIPE_SECRET_KEY is required when BILLING_PROVIDER=stripe")
		}
		if !strings.HasPrefix(value.SecretKey, "sk_") && !strings.HasPrefix(value.SecretKey, "rk_") {
			return Billing{}, fmt.Errorf("STRIPE_SECRET_KEY must start with sk_ or rk_")
		}
		live := strings.HasPrefix(value.SecretKey, "sk_live_") || strings.HasPrefix(value.SecretKey, "rk_live_")
		if live != production {
			return Billing{}, fmt.Errorf("STRIPE_SECRET_KEY must use a live key if and only if DNS_PRODUCTION=true")
		}
		if err := validateBearerToken("STRIPE_SECRET_KEY", value.SecretKey, true); err != nil {
			return Billing{}, err
		}
		if len(value.WebhookSecrets) == 0 {
			return Billing{}, fmt.Errorf("STRIPE_WEBHOOK_SECRET is required when BILLING_PROVIDER=stripe")
		}
		if value.PriceID == "" {
			return Billing{}, fmt.Errorf("STRIPE_PRICE_ID is required when BILLING_PROVIDER=stripe")
		}
		if !strings.HasPrefix(value.PriceID, "price_") {
			return Billing{}, fmt.Errorf("STRIPE_PRICE_ID must start with price_")
		}
		if value.CustomerEmail == "" {
			return Billing{}, fmt.Errorf("BILLING_CUSTOMER_EMAIL is required when BILLING_PROVIDER=stripe")
		}
		if !validEmail(value.CustomerEmail) {
			return Billing{}, fmt.Errorf("BILLING_CUSTOMER_EMAIL must be an email address")
		}
	case BillingProviderFake:
		if value.SecretKey == "" {
			value.SecretKey = DefaultFakeStripeSecretKey
		}
		if value.PriceID == "" {
			value.PriceID = DefaultFakeStripePriceID
		}
		if len(value.WebhookSecrets) == 0 {
			value.WebhookSecrets = []string{DefaultFakeWebhookSecret}
		}
		host, port, err := net.SplitHostPort(value.FakeAddr)
		if err != nil || port == "" {
			return Billing{}, fmt.Errorf("BILLING_FAKE_ADDR must be host:port")
		}
		address, err := netip.ParseAddr(host)
		if err != nil || !address.IsLoopback() {
			return Billing{}, fmt.Errorf("BILLING_FAKE_ADDR must bind a loopback address")
		}
		if value.FakeStatePath == "" {
			return Billing{}, fmt.Errorf("BILLING_FAKE_STATE_PATH cannot be empty")
		}
		// The fake provider is the production Stripe client pointed at an
		// in-process server, so the API base follows BILLING_FAKE_ADDR rather
		// than STRIPE_API_BASE. Reporting api.stripe.com here would tell an
		// operator the Settings page is talking to Stripe when it is not.
		value.APIBase = "http://" + value.FakeAddr

	}
	if len(value.WebhookSecrets) > 3 {
		return Billing{}, fmt.Errorf("STRIPE_WEBHOOK_SECRET must be a comma-separated list of at most 3 secrets")
	}
	for _, secret := range value.WebhookSecrets {
		if !strings.HasPrefix(secret, "whsec_") {
			return Billing{}, fmt.Errorf("STRIPE_WEBHOOK_SECRET entries must start with whsec_")
		}
		if err := validateBearerToken("STRIPE_WEBHOOK_SECRET", secret, true); err != nil {
			return Billing{}, err
		}
	}
	return value, nil
}

func endpointHost(rawURL string) string {
	if rawURL == "" || rawURL == FakeURL {
		return strings.TrimSuffix(rawURL, "://")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return parsed.Host
}

func validTenant(value string) bool {
	if len(value) < 1 || len(value) > 24 {
		return false
	}
	for index, char := range value {
		switch {
		case char >= 'a' && char <= 'z', char >= '0' && char <= '9':
		case char == '-' && index > 0 && index < len(value)-1:
		default:
			return false
		}
	}
	return true
}

func validHostname(value string) bool {
	value = strings.TrimSuffix(strings.TrimSpace(value), ".")
	if value == "" || len(value) > 253 {
		return false
	}
	for _, label := range strings.Split(value, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		for index, char := range label {
			switch {
			case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z', char >= '0' && char <= '9':
			case char == '-' && index > 0 && index < len(label)-1:
			default:
				return false
			}
		}
	}
	return true
}

func validKubernetesName(value string) bool {
	if value == "" || len(value) > 253 {
		return false
	}
	for index, char := range value {
		switch {
		case char >= 'a' && char <= 'z', char >= '0' && char <= '9':
		case (char == '-' || char == '.') && index > 0 && index < len(value)-1:
		default:
			return false
		}
	}
	return true
}

func validEmail(value string) bool {
	local, domain, ok := strings.Cut(value, "@")
	if !ok || local == "" || strings.ContainsAny(value, " \t\r\n") {
		return false
	}
	return validHostname(domain) && strings.Contains(domain, ".")
}

func routableAddress(address netip.Addr) bool {
	return address.IsValid() && address.IsGlobalUnicast() && !address.IsPrivate() &&
		!address.IsLoopback() && !address.IsUnspecified() && !address.IsLinkLocalUnicast()
}
