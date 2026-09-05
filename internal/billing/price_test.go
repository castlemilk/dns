package billing

import (
	"regexp"
	"strings"
	"testing"
)

func TestFormatPriceLabelIsTheOnePriceStringTheProductShows(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		price Price
		want  string
	}{
		{"the product's price", Price{UnitAmount: 600, Currency: "usd", Interval: "month"}, "$6 / domain / month"},
		{"with cents", Price{UnitAmount: 650, Currency: "usd", Interval: "month"}, "$6.50 / domain / month"},
		{"euros", Price{UnitAmount: 600, Currency: "eur", Interval: "month"}, "€6 / domain / month"},
		{"pounds", Price{UnitAmount: 599, Currency: "gbp", Interval: "year"}, "£5.99 / domain / year"},
		{"a zero-decimal currency", Price{UnitAmount: 600, Currency: "jpy", Interval: "month"}, "¥600 / domain / month"},
		{"an unfamiliar currency", Price{UnitAmount: 600, Currency: "chf", Interval: "month"}, "CHF 6 / domain / month"},
		{"an unfamiliar zero-decimal currency", Price{UnitAmount: 600, Currency: "vnd", Interval: "month"}, "VND 600 / domain / month"},
		{"no price yet", Price{}, ""},
		{"a one-off price", Price{UnitAmount: 600, Currency: "usd"}, ""},
		{"a free price", Price{UnitAmount: 0, Currency: "usd", Interval: "month"}, ""},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := formatPriceLabel(testCase.price); got != testCase.want {
				t.Fatalf("label = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestCustomerKeyCarriesNoEmailAddress(t *testing.T) {
	t.Parallel()
	key := customerKey("ops@example.test")
	if strings.Contains(key, "@") || strings.Contains(key, "example") {
		t.Fatalf("key = %q; it must not carry the address", key)
	}
	if !regexp.MustCompile(`^customer:[0-9a-f]{16}$`).MatchString(key) {
		t.Fatalf("key = %q", key)
	}
	if customerKey("ops@example.test") != key {
		t.Fatal("the key is not stable for the same address")
	}
	if customerKey("someone@example.test") == key {
		t.Fatal("two addresses share a key")
	}
}

func TestEndpointHostCarriesNoSchemeOrPath(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"https://api.stripe.com":     "api.stripe.com",
		"http://127.0.0.1:8087":      "127.0.0.1:8087",
		"https://api.stripe.com/v1/": "api.stripe.com",
		"":                           "",
	}
	for base, want := range cases {
		if got := endpointHost(base); got != want {
			t.Errorf("endpointHost(%q) = %q, want %q", base, got, want)
		}
	}
}

func TestPluralReadsNaturally(t *testing.T) {
	t.Parallel()
	if got := plural(1, "domain", "domains"); got != "1 domain" {
		t.Errorf("plural(1) = %q", got)
	}
	if got := plural(3, "domain", "domains"); got != "3 domains" {
		t.Errorf("plural(3) = %q", got)
	}
}
