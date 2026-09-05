package provider_test

import (
	"testing"
	"time"

	"github.com/castlemilk/dns/internal/billing/provider"
)

func TestDecodeCheckoutSessionAcceptsExpandedAndBareIDs(t *testing.T) {
	t.Parallel()
	bare := []byte(`{"id":"cs_1","object":"checkout.session","status":"complete","payment_status":"paid",
		"customer":"cus_1","subscription":"sub_1","client_reference_id":"z1",
		"metadata":{"zone_id":"z1"},"created":1700000000,"expires_at":1700086400}`)
	session, err := provider.DecodeCheckoutSession(bare)
	if err != nil {
		t.Fatalf("DecodeCheckoutSession: %v", err)
	}
	if session.CustomerID != "cus_1" || session.SubscriptionID != "sub_1" {
		t.Fatalf("session = %#v", session)
	}
	if session.Created.Unix() != 1700000000 || session.ExpiresAt.Unix() != 1700086400 {
		t.Fatalf("timestamps = %v / %v", session.Created, session.ExpiresAt)
	}

	expanded := []byte(`{"id":"cs_2","customer":{"id":"cus_2","object":"customer"},
		"subscription":{"id":"sub_2","object":"subscription"}}`)
	session, err = provider.DecodeCheckoutSession(expanded)
	if err != nil {
		t.Fatalf("DecodeCheckoutSession (expanded): %v", err)
	}
	if session.CustomerID != "cus_2" || session.SubscriptionID != "sub_2" {
		t.Fatalf("expanded session = %#v", session)
	}
	if !session.Created.IsZero() {
		t.Fatalf("a missing created time became %v", session.Created)
	}
}

func TestDecodeSubscriptionReadsThePeriodFromItsItem(t *testing.T) {
	t.Parallel()
	// Since basil the period lives on the item, not on the subscription.
	raw := []byte(`{"id":"sub_1","object":"subscription","customer":"cus_1","status":"active",
		"created":1,"cancel_at":1700000000,"cancel_at_period_end":true,
		"metadata":{"zone_id":"z1","domain":"acme.dev"},
		"items":{"object":"list","data":[{"current_period_end":1700086400}]},
		"default_payment_method":{"id":"pm_1","object":"payment_method","type":"card",
			"card":{"brand":"visa","last4":"4242","exp_month":12,"exp_year":2030}}}`)
	item, err := provider.DecodeSubscription(raw)
	if err != nil {
		t.Fatalf("DecodeSubscription: %v", err)
	}
	if item.CurrentPeriodEnd.Unix() != 1700086400 {
		t.Fatalf("current period end = %v", item.CurrentPeriodEnd)
	}
	if item.CancelAt.Unix() != 1700000000 || !item.CancelAtPeriodEnd {
		t.Fatalf("cancellation = %v / %v", item.CancelAt, item.CancelAtPeriodEnd)
	}
	if item.Metadata["domain"] != "acme.dev" {
		t.Fatalf("metadata = %v", item.Metadata)
	}
	if item.PaymentMethod == nil || item.PaymentMethod.Brand != "visa" ||
		item.PaymentMethod.Last4 != "4242" || item.PaymentMethod.ExpMonth != 12 || item.PaymentMethod.ExpYear != 2030 {
		t.Fatalf("payment method = %#v", item.PaymentMethod)
	}

	// An unexpanded default_payment_method is a bare id, which carries no card
	// summary; reporting a blank card would be worse than reporting none.
	unexpanded, err := provider.DecodeSubscription([]byte(`{"id":"sub_2","default_payment_method":"pm_2"}`))
	if err != nil {
		t.Fatalf("DecodeSubscription (unexpanded): %v", err)
	}
	if unexpanded.PaymentMethod != nil {
		t.Fatalf("payment method = %#v, want nil", unexpanded.PaymentMethod)
	}
}

func TestDecodeInvoiceReadsTheSubscriptionParent(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"id":"in_1","object":"invoice","status":"paid","customer":"cus_1",
		"parent":{"type":"subscription_details","subscription_details":{
			"subscription":"sub_1","metadata":{"zone_id":"z1","domain":"acme.dev"}}},
		"lines":{"object":"list","data":[{"period":{"start":1,"end":1700086400}}]}}`)
	invoice, err := provider.DecodeInvoice(raw)
	if err != nil {
		t.Fatalf("DecodeInvoice: %v", err)
	}
	if invoice.SubscriptionID != "sub_1" || invoice.Metadata["zone_id"] != "z1" {
		t.Fatalf("invoice = %#v", invoice)
	}
	if invoice.PeriodEnd.Unix() != 1700086400 {
		t.Fatalf("period end = %v", invoice.PeriodEnd)
	}

	// An invoice with no subscription parent (a one-off) carries no zone.
	orphan, err := provider.DecodeInvoice([]byte(`{"id":"in_2","object":"invoice","status":"open"}`))
	if err != nil {
		t.Fatalf("DecodeInvoice (orphan): %v", err)
	}
	if orphan.SubscriptionID != "" || len(orphan.Metadata) != 0 || !orphan.PeriodEnd.IsZero() {
		t.Fatalf("orphan invoice = %#v", orphan)
	}
}

func TestDecodeCustomerAndExpandableID(t *testing.T) {
	t.Parallel()
	customer, err := provider.DecodeCustomer([]byte(`{"id":"cus_1","object":"customer","email":"ops@example.test"}`))
	if err != nil {
		t.Fatalf("DecodeCustomer: %v", err)
	}
	if customer.ID != "cus_1" || customer.Email != "ops@example.test" {
		t.Fatalf("customer = %#v", customer)
	}

	cases := map[string]string{
		`"sub_1"`:                   "sub_1",
		`{"id":"sub_2"}`:            "sub_2",
		`null`:                      "",
		`{"object":"subscription"}`: "",
		`[1,2,3]`:                   "",
	}
	for raw, want := range cases {
		if got := provider.ExpandableID([]byte(raw)); got != want {
			t.Errorf("ExpandableID(%s) = %q, want %q", raw, got, want)
		}
	}
	if got := provider.ExpandableID(nil); got != "" {
		t.Errorf("ExpandableID(nil) = %q", got)
	}
}

func TestDecodersRejectMalformedJSON(t *testing.T) {
	t.Parallel()
	broken := []byte(`{`)
	if _, err := provider.DecodeCheckoutSession(broken); err == nil {
		t.Error("a malformed checkout session decoded")
	}
	if _, err := provider.DecodeSubscription(broken); err == nil {
		t.Error("a malformed subscription decoded")
	}
	if _, err := provider.DecodeInvoice(broken); err == nil {
		t.Error("a malformed invoice decoded")
	}
	if _, err := provider.DecodeCustomer(broken); err == nil {
		t.Error("a malformed customer decoded")
	}
}

func TestZeroTimestampsStayZero(t *testing.T) {
	t.Parallel()
	item, err := provider.DecodeSubscription([]byte(`{"id":"sub_1","cancel_at":0,"canceled_at":0,"created":0}`))
	if err != nil {
		t.Fatalf("DecodeSubscription: %v", err)
	}
	for name, value := range map[string]time.Time{
		"created":     item.Created,
		"cancel_at":   item.CancelAt,
		"canceled_at": item.CanceledAt,
	} {
		if !value.IsZero() {
			t.Errorf("%s = %v, want the zero time", name, value)
		}
	}
}
