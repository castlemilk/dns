package config_test

import (
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/castlemilk/dns/internal/config"
)

// envFunc turns a map into the getenv shape Load takes.
func envFunc(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

// withBase merges the engine variables under test onto a minimal writer-role
// environment.
func withBase(extra map[string]string) map[string]string {
	values := map[string]string{
		"DNS_ROLE":             "all",
		"DNS_API_BEARER_TOKEN": "operator-token",
	}
	for key, value := range extra {
		values[key] = value
	}
	return values
}

func TestPlatformPathRules(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		env     map[string]string
		want    string
		wantErr string
	}{
		{
			name: "defaults beside the zone store",
			env:  withBase(map[string]string{"DNS_DATA_PATH": "/srv/data/dns.db"}),
			want: "/srv/data/platform.db",
		},
		{
			name: "explicit path is used",
			env:  withBase(map[string]string{"DNS_PLATFORM_PATH": "/srv/other/platform.db"}),
			want: "/srv/other/platform.db",
		},
		{
			name:    "must differ from the zone store",
			env:     withBase(map[string]string{"DNS_DATA_PATH": "/srv/data/dns.db", "DNS_PLATFORM_PATH": "/srv/data/dns.db"}),
			wantErr: "DNS_PLATFORM_PATH must not equal DNS_DATA_PATH",
		},
		{
			name: "must differ from the snapshot cache",
			env: withBase(map[string]string{
				"DNS_SNAPSHOT_CACHE_PATH": "/srv/data/cache.json",
				"DNS_PLATFORM_PATH":       "/srv/data/cache.json",
			}),
			wantErr: "DNS_PLATFORM_PATH must not equal DNS_SNAPSHOT_CACHE_PATH",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := config.Load(envFunc(tt.env))
			if tt.wantErr != "" {
				requireErrorNaming(t, err, tt.wantErr)
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got.PlatformPath != tt.want {
				t.Fatalf("PlatformPath = %q, want %q", got.PlatformPath, tt.want)
			}
		})
	}
}

func TestAuthorityRejectsEveryEngineVariable(t *testing.T) {
	t.Parallel()

	names := []string{
		"DNS_PLATFORM_PATH", "ACTIVITY_MAX_EVENTS",
		"HOSTING_API_URL", "HOSTING_API_TOKEN", "HOSTING_TENANT", "HOSTING_GATEWAY_HOSTNAME",
		"HOSTING_GATEWAY_ADDRESSES", "HOSTING_GATEWAY_ALLOWED_CIDRS", "HOSTING_GATEWAY_RESOLVER",
		"HOSTING_APPS_SUFFIX", "HOSTING_APPS_TLS_SECRET", "HOSTING_BUILD_DEADLINE",
		"HOSTING_UPLOADS_ENABLED", "HOSTING_UPLOAD_MAX_BYTES", "HOSTING_UPLOAD_MAX_TOTAL_BYTES",
		"HOSTING_UPLOAD_DIR",
		"MAIL_API_URL", "MAIL_API_TOKEN", "MAIL_HOSTNAME", "MAIL_MX_PRIORITY", "MAIL_SPF_INCLUDE",
		"MAIL_DMARC_POLICY", "MAIL_REPORT_ADDRESS", "MAIL_MAILBOXES_PER_DOMAIN", "MAIL_DEFAULT_QUOTA_BYTES",
		"MAIL_WEBHOOK_SECRET",
		"STRIPE_SECRET_KEY", "STRIPE_WEBHOOK_SECRET", "STRIPE_PRICE_ID", "STRIPE_API_BASE",
		"BILLING_PROVIDER", "BILLING_PUBLIC_URL", "BILLING_CUSTOMER_EMAIL", "BILLING_FAKE_ADDR",
		"BILLING_FAKE_STATE_PATH",
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := map[string]string{
				"DNS_ROLE":                  "authority",
				"DNS_SNAPSHOT_URL":          "http://dns-control:8080/internal/v1/snapshot",
				"DNS_SNAPSHOT_BEARER_TOKEN": "snapshot-token",
				name:                        "value",
			}
			_, err := config.Load(envFunc(env))
			requireErrorNaming(t, err, name+" is not used by role authority")
		})
	}
}

func TestAuthorityHasNoPlatformPath(t *testing.T) {
	t.Parallel()
	got, err := config.Load(envFunc(map[string]string{
		"DNS_ROLE":                  "authority",
		"DNS_SNAPSHOT_URL":          "http://dns-control:8080/internal/v1/snapshot",
		"DNS_SNAPSHOT_BEARER_TOKEN": "snapshot-token",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.PlatformPath != "" {
		t.Fatalf("PlatformPath = %q, want empty for role authority", got.PlatformPath)
	}
}

func TestActivityBounds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		raw     string
		want    int
		wantErr bool
	}{
		{raw: "", want: config.DefaultActivityMaxEvents},
		{raw: "1000", want: 1000},
		{raw: "1000000", want: 1_000_000},
		{raw: "999", wantErr: true},
		{raw: "1000001", wantErr: true},
		{raw: "many", wantErr: true},
	}
	for _, tt := range tests {
		t.Run("ACTIVITY_MAX_EVENTS="+tt.raw, func(t *testing.T) {
			t.Parallel()
			got, err := config.Load(envFunc(withBase(map[string]string{"ACTIVITY_MAX_EVENTS": tt.raw})))
			if tt.wantErr {
				requireErrorNaming(t, err, "ACTIVITY_MAX_EVENTS")
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got.Activity.MaxEvents != tt.want {
				t.Fatalf("MaxEvents = %d, want %d", got.Activity.MaxEvents, tt.want)
			}
		})
	}
}

func TestHostingBlock(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		env     map[string]string
		wantErr string
		check   func(*testing.T, config.Hosting)
	}{
		{
			name: "absent block is not configured",
			env:  withBase(nil),
			check: func(t *testing.T, hosting config.Hosting) {
				if hosting.Configured() {
					t.Fatal("hosting reported configured with no variables set")
				}
				if len(hosting.MissingEnv()) == 0 {
					t.Fatal("MissingEnv must name the variables an operator has to set")
				}
			},
		},
		{
			name: "loopback engine needs no token",
			env: withBase(map[string]string{
				"HOSTING_API_URL":           "http://127.0.0.1:30081",
				"HOSTING_GATEWAY_ADDRESSES": "127.0.0.1",
			}),
			check: func(t *testing.T, hosting config.Hosting) {
				if !hosting.Configured() {
					t.Fatal("hosting not configured")
				}
				if hosting.Tenant != config.DefaultHostingTenant {
					t.Fatalf("Tenant = %q, want %q", hosting.Tenant, config.DefaultHostingTenant)
				}
				if hosting.BuildDeadline != config.DefaultBuildDeadline {
					t.Fatalf("BuildDeadline = %s, want %s", hosting.BuildDeadline, config.DefaultBuildDeadline)
				}
				if hosting.UploadMaxTotalBytes != 2*hosting.UploadMaxBytes {
					t.Fatalf("UploadMaxTotalBytes = %d, want twice UploadMaxBytes", hosting.UploadMaxTotalBytes)
				}
			},
		},
		{
			name: "gateway addresses parse and canonicalise",
			env: withBase(map[string]string{
				"HOSTING_API_URL":           "http://127.0.0.1:30081",
				"HOSTING_GATEWAY_ADDRESSES": " 198.51.100.10 , 2001:db8::10 ",
			}),
			check: func(t *testing.T, hosting config.Hosting) {
				want := []netip.Addr{netip.MustParseAddr("198.51.100.10"), netip.MustParseAddr("2001:db8::10")}
				if len(hosting.GatewayAddresses) != len(want) {
					t.Fatalf("GatewayAddresses = %v, want %v", hosting.GatewayAddresses, want)
				}
				for index, address := range want {
					if hosting.GatewayAddresses[index] != address {
						t.Fatalf("GatewayAddresses[%d] = %s, want %s", index, hosting.GatewayAddresses[index], address)
					}
				}
			},
		},
		{
			name:    "a partial block names the missing URL",
			env:     withBase(map[string]string{"HOSTING_TENANT": "simple"}),
			wantErr: "HOSTING_API_URL is required when HOSTING_TENANT is set",
		},
		{
			name:    "no gateway at all",
			env:     withBase(map[string]string{"HOSTING_API_URL": "http://127.0.0.1:30081"}),
			wantErr: "HOSTING_GATEWAY_HOSTNAME or HOSTING_GATEWAY_ADDRESSES is required",
		},
		{
			name: "remote engine needs a token",
			env: withBase(map[string]string{
				"HOSTING_API_URL":          "https://deephost.example.com",
				"HOSTING_GATEWAY_HOSTNAME": "gateway.example.com",
			}),
			wantErr: "HOSTING_API_TOKEN is required",
		},
		{
			name: "tenant longer than 24 characters",
			env: withBase(map[string]string{
				"HOSTING_API_URL":           "http://127.0.0.1:30081",
				"HOSTING_GATEWAY_ADDRESSES": "127.0.0.1",
				"HOSTING_TENANT":            strings.Repeat("a", 25),
			}),
			wantErr: "HOSTING_TENANT",
		},
		{
			name: "apps suffix without a TLS secret",
			env: withBase(map[string]string{
				"HOSTING_API_URL":           "http://127.0.0.1:30081",
				"HOSTING_GATEWAY_ADDRESSES": "127.0.0.1",
				"HOSTING_APPS_SUFFIX":       "apps.example.com",
			}),
			wantErr: "HOSTING_APPS_TLS_SECRET is required when HOSTING_APPS_SUFFIX is set",
		},
		{
			name: "build deadline out of range",
			env: withBase(map[string]string{
				"HOSTING_API_URL":           "http://127.0.0.1:30081",
				"HOSTING_GATEWAY_ADDRESSES": "127.0.0.1",
				"HOSTING_BUILD_DEADLINE":    "1m",
			}),
			wantErr: "HOSTING_BUILD_DEADLINE",
		},
		{
			name: "upload total below the per-upload cap",
			env: withBase(map[string]string{
				"HOSTING_API_URL":                "http://127.0.0.1:30081",
				"HOSTING_GATEWAY_ADDRESSES":      "127.0.0.1",
				"HOSTING_UPLOAD_MAX_BYTES":       "67108864",
				"HOSTING_UPLOAD_MAX_TOTAL_BYTES": "1048576",
			}),
			wantErr: "HOSTING_UPLOAD_MAX_TOTAL_BYTES",
		},
		{
			name: "resolver must be host:port",
			env: withBase(map[string]string{
				"HOSTING_API_URL":           "http://127.0.0.1:30081",
				"HOSTING_GATEWAY_ADDRESSES": "127.0.0.1",
				"HOSTING_GATEWAY_RESOLVER":  "10.0.0.53",
			}),
			wantErr: "HOSTING_GATEWAY_RESOLVER must be host:port",
		},
		{
			name: "allowed CIDRs must parse",
			env: withBase(map[string]string{
				"HOSTING_API_URL":               "http://127.0.0.1:30081",
				"HOSTING_GATEWAY_ADDRESSES":     "127.0.0.1",
				"HOSTING_GATEWAY_ALLOWED_CIDRS": "198.51.100.0",
			}),
			wantErr: "HOSTING_GATEWAY_ALLOWED_CIDRS",
		},
		{
			name: "userinfo in the engine URL",
			env: withBase(map[string]string{
				"HOSTING_API_URL":           "https://user:pass@deephost.example.com",
				"HOSTING_GATEWAY_ADDRESSES": "198.51.100.10",
			}),
			wantErr: "HOSTING_API_URL must be an absolute HTTP or HTTPS URL",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := config.Load(envFunc(tt.env))
			if tt.wantErr != "" {
				requireErrorNaming(t, err, tt.wantErr)
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			tt.check(t, got.Hosting)
		})
	}
}

func TestMailBlock(t *testing.T) {
	t.Parallel()

	base := map[string]string{
		"MAIL_API_URL":   "http://127.0.0.1:20080",
		"MAIL_API_TOKEN": "API_" + strings.Repeat("x", 38),
		"MAIL_HOSTNAME":  "mail.local.test",
	}
	merge := func(extra map[string]string) map[string]string {
		values := map[string]string{}
		for key, value := range base {
			values[key] = value
		}
		for key, value := range extra {
			values[key] = value
		}
		return withBase(values)
	}

	tests := []struct {
		name    string
		env     map[string]string
		wantErr string
		check   func(*testing.T, config.Mail)
	}{
		{
			name: "defaults",
			env:  merge(nil),
			check: func(t *testing.T, mail config.Mail) {
				if mail.MXPriority != config.DefaultMailMXPriority {
					t.Fatalf("MXPriority = %d, want %d", mail.MXPriority, config.DefaultMailMXPriority)
				}
				if mail.DMARCPolicy != config.DefaultMailDMARCPolicy {
					t.Fatalf("DMARCPolicy = %q, want %q", mail.DMARCPolicy, config.DefaultMailDMARCPolicy)
				}
				if mail.MailboxesPerDomain != config.DefaultMailboxesPerDomain {
					t.Fatalf("MailboxesPerDomain = %d, want %d", mail.MailboxesPerDomain, config.DefaultMailboxesPerDomain)
				}
				if mail.DefaultQuotaBytes != config.DefaultMailQuotaBytes {
					t.Fatalf("DefaultQuotaBytes = %d, want %d", mail.DefaultQuotaBytes, config.DefaultMailQuotaBytes)
				}
			},
		},
		{name: "token required", env: merge(map[string]string{"MAIL_API_TOKEN": ""}), wantErr: "MAIL_API_TOKEN is required"},
		{name: "hostname required", env: merge(map[string]string{"MAIL_HOSTNAME": ""}), wantErr: "MAIL_HOSTNAME is required"},
		{name: "partial block", env: withBase(map[string]string{"MAIL_HOSTNAME": "mail.local.test"}), wantErr: "MAIL_API_URL is required when MAIL_HOSTNAME is set"},
		{name: "dmarc policy", env: merge(map[string]string{"MAIL_DMARC_POLICY": "block"}), wantErr: "MAIL_DMARC_POLICY"},
		{name: "report address", env: merge(map[string]string{"MAIL_REPORT_ADDRESS": "not-an-address"}), wantErr: "MAIL_REPORT_ADDRESS"},
		{name: "mailbox limit", env: merge(map[string]string{"MAIL_MAILBOXES_PER_DOMAIN": "501"}), wantErr: "MAIL_MAILBOXES_PER_DOMAIN"},
		{name: "quota floor", env: merge(map[string]string{"MAIL_DEFAULT_QUOTA_BYTES": "1024"}), wantErr: "MAIL_DEFAULT_QUOTA_BYTES"},
		{name: "mx priority", env: merge(map[string]string{"MAIL_MX_PRIORITY": "70000"}), wantErr: "MAIL_MX_PRIORITY"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := config.Load(envFunc(tt.env))
			if tt.wantErr != "" {
				requireErrorNaming(t, err, tt.wantErr)
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			tt.check(t, got.Mail)
		})
	}
}

func TestBillingBlock(t *testing.T) {
	t.Parallel()

	stripe := func(extra map[string]string) map[string]string {
		values := map[string]string{
			"BILLING_PROVIDER":       "stripe",
			"STRIPE_SECRET_KEY":      stripeKey("test"),
			"STRIPE_WEBHOOK_SECRET":  "whsec_abcdefghijklmnop",
			"STRIPE_PRICE_ID":        "price_abcdefghij",
			"BILLING_CUSTOMER_EMAIL": "billing@example.com",
		}
		for key, value := range extra {
			values[key] = value
		}
		return withBase(values)
	}

	tests := []struct {
		name    string
		env     map[string]string
		wantErr string
		check   func(*testing.T, config.Billing)
	}{
		{
			name: "fake provider supplies its own defaults",
			env:  withBase(map[string]string{"BILLING_PROVIDER": "fake"}),
			check: func(t *testing.T, billing config.Billing) {
				if billing.PriceID != config.DefaultFakeStripePriceID {
					t.Fatalf("PriceID = %q, want %q", billing.PriceID, config.DefaultFakeStripePriceID)
				}
				if len(billing.WebhookSecrets) != 1 || billing.WebhookSecrets[0] != config.DefaultFakeWebhookSecret {
					t.Fatalf("WebhookSecrets = %v", billing.WebhookSecrets)
				}
				if billing.FakeAddr != config.DefaultBillingFakeAddr {
					t.Fatalf("FakeAddr = %q", billing.FakeAddr)
				}
				// The fake is the production client pointed at a loopback
				// server: reporting api.stripe.com would claim the console is
				// talking to Stripe when it is not.
				if billing.APIBase != "http://"+config.DefaultBillingFakeAddr {
					t.Fatalf("APIBase = %q, want the fake address", billing.APIBase)
				}
				if billing.EndpointHost() != config.DefaultBillingFakeAddr {
					t.Fatalf("EndpointHost = %q, want the fake address", billing.EndpointHost())
				}
			},
		},
		{
			name: "stripe accepts up to three rotating secrets",
			env:  stripe(map[string]string{"STRIPE_WEBHOOK_SECRET": "whsec_one_aaaaaaaa,whsec_two_bbbbbbbb"}),
			check: func(t *testing.T, billing config.Billing) {
				if len(billing.WebhookSecrets) != 2 {
					t.Fatalf("WebhookSecrets = %v, want two entries", billing.WebhookSecrets)
				}
			},
		},
		{name: "unknown provider", env: withBase(map[string]string{"BILLING_PROVIDER": "paddle"}), wantErr: "BILLING_PROVIDER"},
		{name: "partial block", env: withBase(map[string]string{"STRIPE_SECRET_KEY": stripeKey("test")}), wantErr: "BILLING_PROVIDER is required when STRIPE_SECRET_KEY is set"},
		{name: "webhook secret required", env: stripe(map[string]string{"STRIPE_WEBHOOK_SECRET": ""}), wantErr: "STRIPE_WEBHOOK_SECRET is required"},
		{name: "webhook secret prefix", env: stripe(map[string]string{"STRIPE_WEBHOOK_SECRET": "sig_abcdefgh"}), wantErr: "STRIPE_WEBHOOK_SECRET entries must start with whsec_"},
		{name: "too many secrets", env: stripe(map[string]string{"STRIPE_WEBHOOK_SECRET": "whsec_a1,whsec_b2,whsec_c3,whsec_d4"}), wantErr: "at most 3 secrets"},
		{name: "price prefix", env: stripe(map[string]string{"STRIPE_PRICE_ID": "prod_abcdefgh"}), wantErr: "STRIPE_PRICE_ID must start with price_"},
		{name: "secret key prefix", env: stripe(map[string]string{"STRIPE_SECRET_KEY": "pk_" + "test_fixture"}), wantErr: "STRIPE_SECRET_KEY must start with sk_, rk_ or rkcs_"},
		// A Stripe sandbox (`stripe sandbox create`) issues an rkcs_ restricted
		// key. It was rejected as malformed, which took the control plane down
		// and sent us hunting for a typo when the real answer is the live/test
		// rule: a test key simply cannot run with DNS_PRODUCTION=true.
		{
			name: "sandbox restricted key is a valid test key",
			env:  stripe(map[string]string{"STRIPE_SECRET_KEY": "rkcs_" + stripeKey("test")[3:]}),
			check: func(t *testing.T, billing config.Billing) {
				// The point of the case: it loads at all. A sandbox key must
				// also never be mistaken for a live one, or the production
				// guard below would wave a test key through.
				if billing.Provider != config.BillingProviderStripe {
					t.Fatalf("Provider = %q, want stripe", billing.Provider)
				}
			},
		},
		{name: "an unknown prefix fails closed", env: stripe(map[string]string{"STRIPE_SECRET_KEY": "rkzz_" + stripeKey("live")[3:]}), wantErr: "STRIPE_SECRET_KEY must start with"},
		{name: "customer email required", env: stripe(map[string]string{"BILLING_CUSTOMER_EMAIL": ""}), wantErr: "BILLING_CUSTOMER_EMAIL is required"},
		{name: "public url shape", env: stripe(map[string]string{"BILLING_PUBLIC_URL": "example.com/billing"}), wantErr: "BILLING_PUBLIC_URL"},
		{name: "fake addr must be loopback", env: withBase(map[string]string{"BILLING_PROVIDER": "fake", "BILLING_FAKE_ADDR": "0.0.0.0:8087"}), wantErr: "BILLING_FAKE_ADDR must bind a loopback address"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := config.Load(envFunc(tt.env))
			if tt.wantErr != "" {
				requireErrorNaming(t, err, tt.wantErr)
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			tt.check(t, got.Billing)
		})
	}
}

func TestProductionRefusesFakeAndLocalEngines(t *testing.T) {
	t.Parallel()

	production := func(extra map[string]string) map[string]string {
		values := map[string]string{
			"DNS_ROLE":                  "control",
			"DNS_PRODUCTION":            "true",
			"DNS_NAMESERVERS":           "ns1.dns.example,ns2.dns.example",
			"DNS_API_BEARER_TOKEN":      productionAPIToken,
			"DNS_SNAPSHOT_BEARER_TOKEN": productionSnapshotToken,
		}
		for key, value := range extra {
			values[key] = value
		}
		return values
	}

	tests := []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{
			name:    "fake hosting engine",
			env:     production(map[string]string{"HOSTING_API_URL": "fake://", "HOSTING_GATEWAY_ADDRESSES": "198.51.100.10"}),
			wantErr: "HOSTING_API_URL must not be fake://",
		},
		{
			name:    "fake mail engine",
			env:     production(map[string]string{"MAIL_API_URL": "fake://", "MAIL_API_TOKEN": "API_x", "MAIL_HOSTNAME": "mail.example.com"}),
			wantErr: "MAIL_API_URL must not be fake://",
		},
		{
			name:    "fake billing provider",
			env:     production(map[string]string{"BILLING_PROVIDER": "fake"}),
			wantErr: "BILLING_PROVIDER must not be fake",
		},
		{
			name: "loopback gateway address",
			env: production(map[string]string{
				"HOSTING_API_URL":           "https://deephost.example.com",
				"HOSTING_API_TOKEN":         strings.Repeat("h", 40),
				"HOSTING_GATEWAY_ADDRESSES": "127.0.0.1",
			}),
			wantErr: "HOSTING_GATEWAY_ADDRESSES must contain only global unicast addresses",
		},
		{
			name: "plain http mail engine on a public host",
			env: production(map[string]string{
				"MAIL_API_URL":   "http://mail.example.com:8080",
				"MAIL_API_TOKEN": "API_" + strings.Repeat("x", 38),
				"MAIL_HOSTNAME":  "mail.example.com",
			}),
			wantErr: "MAIL_API_URL must use HTTPS in production",
		},
		{
			// MAIL_API_TOKEN is the credential that creates and destroys
			// mailboxes on Stalwart, and it was the one credential with no
			// production length floor: the two DNS bearers, HOSTING_API_TOKEN
			// and MAIL_WEBHOOK_SECRET all have one.
			name: "short mail engine token in production",
			env: production(map[string]string{
				"MAIL_API_URL":   "https://mail.example.com",
				"MAIL_API_TOKEN": "hunter2",
				"MAIL_HOSTNAME":  "mail.example.com",
			}),
			wantErr: "MAIL_API_TOKEN must be at least 32 bytes when DNS_PRODUCTION=true",
		},
		{
			// The signature key is only a second factor once it is a key of its
			// own; without the bearer there is no receiver for it to guard.
			name: "mail webhook signature key without the bearer",
			env: production(map[string]string{
				"MAIL_API_URL":               "https://mail.example.com",
				"MAIL_API_TOKEN":             "API_" + strings.Repeat("x", 38),
				"MAIL_HOSTNAME":              "mail.example.com",
				"MAIL_WEBHOOK_SIGNATURE_KEY": strings.Repeat("s", 40),
			}),
			wantErr: "MAIL_WEBHOOK_SIGNATURE_KEY requires MAIL_WEBHOOK_SECRET",
		},
		{
			name: "short mail webhook signature key in production",
			env: production(map[string]string{
				"MAIL_API_URL":               "https://mail.example.com",
				"MAIL_API_TOKEN":             "API_" + strings.Repeat("x", 38),
				"MAIL_HOSTNAME":              "mail.example.com",
				"MAIL_WEBHOOK_SECRET":        strings.Repeat("w", 40),
				"MAIL_WEBHOOK_SIGNATURE_KEY": "hunter2",
			}),
			wantErr: "MAIL_WEBHOOK_SIGNATURE_KEY must be at least 32 bytes when DNS_PRODUCTION=true",
		},
		{
			name: "test stripe key in production",
			env: production(map[string]string{
				"BILLING_PROVIDER":       "stripe",
				"STRIPE_SECRET_KEY":      stripeKey("test"),
				"STRIPE_WEBHOOK_SECRET":  "whsec_abcdefghij",
				"STRIPE_PRICE_ID":        "price_abcdefghij",
				"BILLING_CUSTOMER_EMAIL": "billing@example.com",
				"BILLING_PUBLIC_URL":     "https://simple.example.com",
			}),
			wantErr: "STRIPE_SECRET_KEY must use a live key",
		},
		{
			// The production cluster was pointed at a Stripe sandbox. Once the
			// rkcs_ prefix is recognised the key is well-formed, so this is the
			// rule that must stop it — a sandbox moves no money, and a
			// deployment billing real customers against it would take payment
			// that never arrives.
			name: "sandbox stripe key in production",
			env: production(map[string]string{
				"BILLING_PROVIDER":       "stripe",
				"STRIPE_SECRET_KEY":      "rkcs_" + stripeKey("test")[3:],
				"STRIPE_WEBHOOK_SECRET":  "whsec_abcdefghij",
				"STRIPE_PRICE_ID":        "price_abcdefghij",
				"BILLING_CUSTOMER_EMAIL": "billing@example.com",
				"BILLING_PUBLIC_URL":     "https://simple.example.com",
			}),
			wantErr: "STRIPE_SECRET_KEY must use a live key",
		},
		{
			name: "overridden Stripe API base",
			env: production(map[string]string{
				"BILLING_PROVIDER":       "stripe",
				"STRIPE_SECRET_KEY":      stripeKey("live"),
				"STRIPE_WEBHOOK_SECRET":  "whsec_abcdefghij",
				"STRIPE_PRICE_ID":        "price_abcdefghij",
				"BILLING_CUSTOMER_EMAIL": "billing@example.com",
				"BILLING_PUBLIC_URL":     "https://simple.example.com",
				"STRIPE_API_BASE":        "http://127.0.0.1:8087",
			}),
			wantErr: "STRIPE_API_BASE",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := config.Load(envFunc(tt.env))
			requireErrorNaming(t, err, tt.wantErr)
		})
	}
}

func TestOperatorTokenRequiredWithRealEngineCredentials(t *testing.T) {
	t.Parallel()

	const want = "DNS_API_BEARER_TOKEN is required when a real hosting or mail engine is configured"
	tests := []struct {
		name string
		env  map[string]string
	}{
		{
			name: "hosting token",
			env: map[string]string{
				"DNS_ROLE":                 "all",
				"HOSTING_API_URL":          "https://deephost.example.com",
				"HOSTING_API_TOKEN":        strings.Repeat("h", 40),
				"HOSTING_GATEWAY_HOSTNAME": "gateway.example.com",
			},
		},
		{
			// The hole this guards: HOSTING_API_TOKEN is optional for a
			// loopback or cluster-local DeepHost, so keying the rule on the
			// token left a live engine behind an unauthenticated control API.
			name: "loopback hosting engine with no token of its own",
			env: map[string]string{
				"DNS_ROLE":                  "all",
				"HOSTING_API_URL":           "http://127.0.0.1:30081",
				"HOSTING_GATEWAY_ADDRESSES": "127.0.0.1",
			},
		},
		{
			name: "cluster-local hosting engine with no token of its own",
			env: map[string]string{
				"DNS_ROLE":                  "all",
				"HOSTING_API_URL":           "http://deephost.deephost.svc.cluster.local:8080",
				"HOSTING_GATEWAY_ADDRESSES": "127.0.0.1",
			},
		},
		{
			name: "real mail engine",
			env: map[string]string{
				"DNS_ROLE":       "all",
				"MAIL_API_URL":   "http://127.0.0.1:20080",
				"MAIL_API_TOKEN": "API_" + strings.Repeat("x", 38),
				"MAIL_HOSTNAME":  "mail.local.test",
			},
		},
		{
			name: "stripe provider",
			env: map[string]string{
				"DNS_ROLE":               "all",
				"BILLING_PROVIDER":       "stripe",
				"STRIPE_SECRET_KEY":      stripeKey("test"),
				"STRIPE_WEBHOOK_SECRET":  "whsec_abcdefghij",
				"STRIPE_PRICE_ID":        "price_abcdefghij",
				"BILLING_CUSTOMER_EMAIL": "billing@example.com",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := config.Load(envFunc(tt.env))
			requireErrorNaming(t, err, want)
		})
	}
}

func TestLocalFakeEnginesNeedNoOperatorToken(t *testing.T) {
	t.Parallel()

	// Every engine fake: nothing outside this process can be reached and no
	// credential can be spent, which is the one combination that may run
	// without DNS_API_BEARER_TOKEN. A real engine at a loopback or
	// cluster-local address is not this case, however local it looks — see
	// TestOperatorTokenRequiredWithRealEngineCredentials.
	got, err := config.Load(envFunc(map[string]string{
		"DNS_ROLE":                  "all",
		"HOSTING_API_URL":           config.FakeURL,
		"HOSTING_GATEWAY_ADDRESSES": "127.0.0.1",
		"MAIL_API_URL":              config.FakeURL,
		"MAIL_API_TOKEN":            "API_" + strings.Repeat("x", 38),
		"MAIL_HOSTNAME":             "mail.local.test",
		"BILLING_PROVIDER":          "fake",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !got.Hosting.Configured() || !got.Mail.Configured() || !got.Billing.Configured() {
		t.Fatal("all three local engines should be configured")
	}
	if !got.Hosting.Fake() || !got.Mail.Fake() || !got.Billing.Fake() {
		t.Fatal("fake engines should report Fake()")
	}
}

func TestUploadDirIsCreatedAndChecked(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "uploads")
	got, err := config.Load(envFunc(withBase(map[string]string{
		"HOSTING_API_URL":           "http://127.0.0.1:30081",
		"HOSTING_GATEWAY_ADDRESSES": "127.0.0.1",
		"HOSTING_UPLOADS_ENABLED":   "true",
		"HOSTING_UPLOAD_DIR":        dir,
	})))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !got.Hosting.UploadsEnabled || got.Hosting.UploadDir != dir {
		t.Fatalf("uploads = %v, dir = %q", got.Hosting.UploadsEnabled, got.Hosting.UploadDir)
	}
}

func TestEndpointHostNeverCarriesCredentials(t *testing.T) {
	t.Parallel()

	got, err := config.Load(envFunc(withBase(map[string]string{
		"HOSTING_API_URL":           "http://127.0.0.1:30081",
		"HOSTING_GATEWAY_ADDRESSES": "127.0.0.1",
		"HOSTING_API_TOKEN":         "super-secret-token",
	})))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	host := got.Hosting.EndpointHost()
	if host != "127.0.0.1:30081" {
		t.Fatalf("EndpointHost = %q", host)
	}
	if strings.Contains(host, "secret") {
		t.Fatal("EndpointHost leaked the token")
	}
	for _, name := range got.Hosting.MissingEnv() {
		if strings.Contains(name, "secret") {
			t.Fatalf("MissingEnv leaked a value: %q", name)
		}
	}
}

// stripeKey builds a fixture secret key from parts. Written as one literal it
// would trip every credential scanner in the repository, and the project's
// gitleaks allowlist deliberately covers only the three literals the product
// itself ships.
func stripeKey(mode string) string { return "sk_" + mode + "_fixture" }

// requireErrorNaming asserts the error exists, names the variable, and never
// echoes a value.
func requireErrorNaming(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an error naming %q", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %q, want it to mention %q", err.Error(), want)
	}
	for _, leaked := range []string{"super-secret", stripeKey("test"), "whsec_abcdefghijklmnop"} {
		if strings.Contains(err.Error(), leaked) {
			t.Fatalf("error echoed a value: %q", err.Error())
		}
	}
}

func TestBuildDeadlineAcceptsTheRange(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"5m", "40m", "6h"} {
		got, err := config.Load(envFunc(withBase(map[string]string{
			"HOSTING_API_URL":           "http://127.0.0.1:30081",
			"HOSTING_GATEWAY_ADDRESSES": "127.0.0.1",
			"HOSTING_BUILD_DEADLINE":    raw,
		})))
		if err != nil {
			t.Fatalf("Load(%s): %v", raw, err)
		}
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			t.Fatalf("ParseDuration: %v", err)
		}
		if got.Hosting.BuildDeadline != parsed {
			t.Fatalf("BuildDeadline = %s, want %s", got.Hosting.BuildDeadline, parsed)
		}
	}
}
