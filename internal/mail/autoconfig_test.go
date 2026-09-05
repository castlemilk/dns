package mail

import (
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"
	mailv1 "github.com/castlemilk/dns/gen/go/mail/v1"
	"github.com/castlemilk/dns/internal/enginedns"
	"github.com/castlemilk/dns/internal/mail/fakemail"
	"github.com/castlemilk/dns/internal/zone"
	"google.golang.org/protobuf/proto"
)

// autoconfigSet is the exact client-autoconfiguration set the fake — which
// renders the zone file captured from v0.16.20 — offers for acme.dev.
var autoconfigSet = []string{
	`CNAME autoconfig mail.local.test.`,
	`CNAME autodiscover mail.local.test.`,
	`SRV _caldavs._tcp 0 1 443 mail.local.test.`,
	`SRV _carddavs._tcp 0 1 443 mail.local.test.`,
	`SRV _imaps._tcp 0 1 993 mail.local.test.`,
	`SRV _jmap._tcp 0 1 443 mail.local.test.`,
	`SRV _pop3s._tcp 0 1 995 mail.local.test.`,
	`SRV _submissions._tcp 0 1 465 mail.local.test.`,
}

// autoconfigLines picks the client-autoconfiguration records out of a
// summarized set, so a test can assert on them without restating the policy
// records beside them.
func autoconfigLines(lines []string) []string {
	var found []string
	for _, line := range lines {
		if strings.HasPrefix(line, "SRV ") || strings.HasPrefix(line, "CNAME ") {
			found = append(found, line)
		}
	}
	return found
}

func TestParseAutoconfigRecords(t *testing.T) {
	t.Parallel()
	const zoneFile = `_submissions._tcp.probe.test. IN SRV 0 1 465 mail.local.test.
_imaps._tcp.probe.test. IN SRV 0 1 993 mail.local.test.
_pop3s._tcp.probe.test. IN SRV 0 1 995 mail.local.test.
_jmap._tcp.probe.test. IN SRV 0 1 443 mail.local.test.
_caldavs._tcp.probe.test. IN SRV 0 1 443 mail.local.test.
_carddavs._tcp.probe.test. IN SRV 0 1 443 mail.local.test.
autoconfig.probe.test. IN CNAME mail.local.test.
autodiscover.probe.test. IN CNAME mail.local.test.
mta-sts.probe.test. IN CNAME mail.local.test.
_mta-sts.probe.test. IN TXT "v=STSv1; id=4966920683339627112"
_smtp._tls.probe.test. IN TXT "v=TLSRPTv1; rua=mailto:postmaster@probe.test"
ua-auto-config.probe.test. IN CNAME mail.local.test.
_ua-auto-config.probe.test. IN TXT "v=UAAC1; a=sha256; d=ChkkELbHwgcLV5jhN3d6hfEbPq1fXmI1kESn0hHLMgc="
probe.test. IN MX 10 mail.local.test.
_imaps._tcp.other.test. IN SRV 0 1 993 mail.local.test.
`

	tests := []struct {
		name     string
		zoneFile string
		hostname string
		want     []string
	}{
		{
			name:     "the allow-listed records, in allow-list order",
			zoneFile: zoneFile,
			hostname: "mail.local.test",
			want: []string{
				"SRV _submissions._tcp 0 1 465 mail.local.test.",
				"SRV _imaps._tcp 0 1 993 mail.local.test.",
				"SRV _pop3s._tcp 0 1 995 mail.local.test.",
				"SRV _jmap._tcp 0 1 443 mail.local.test.",
				"SRV _caldavs._tcp 0 1 443 mail.local.test.",
				"SRV _carddavs._tcp 0 1 443 mail.local.test.",
				"CNAME autoconfig mail.local.test.",
				"CNAME autodiscover mail.local.test.",
			},
		},
		{
			// A server that renamed itself renders its new name. Publishing it
			// would point a customer's clients at a host this deployment does
			// not vouch for, so every record is dropped instead.
			name:     "a target that is not MAIL_HOSTNAME is dropped",
			zoneFile: zoneFile,
			hostname: "mail.somewhere.else",
			want:     nil,
		},
		{
			name:     "no hostname configured publishes nothing",
			zoneFile: zoneFile,
			hostname: "",
			want:     nil,
		},
		{
			name:     "an empty zone file publishes nothing",
			zoneFile: "",
			hostname: "mail.local.test",
			want:     nil,
		},
		{
			name:     "a malformed SRV is dropped rather than published broken",
			zoneFile: "_imaps._tcp.probe.test. IN SRV 0 1 nineninethree mail.local.test.\n",
			hostname: "mail.local.test",
			want:     nil,
		},
		{
			name:     "an SRV with the wrong field count is dropped",
			zoneFile: "_imaps._tcp.probe.test. IN SRV 993 mail.local.test.\n",
			hostname: "mail.local.test",
			want:     nil,
		},
		{
			// The first line wins; a server cannot make this facade publish two
			// records at one owner by repeating it.
			name: "a repeated owner is taken once",
			zoneFile: "_imaps._tcp.probe.test. IN SRV 0 1 993 mail.local.test.\n" +
				"_imaps._tcp.probe.test. IN SRV 5 5 993 mail.local.test.\n",
			hostname: "mail.local.test",
			want:     []string{"SRV _imaps._tcp 0 1 993 mail.local.test."},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			records := parseAutoconfigRecords(test.zoneFile, "probe.test", test.hostname)
			var got []string
			for _, record := range records {
				got = append(got, record.Type+" "+record.Name+" "+record.Value)
			}
			if !slices.Equal(got, test.want) {
				t.Errorf("parseAutoconfigRecords:\n got %v\nwant %v", got, test.want)
			}
		})
	}
}

func TestBindPublishesTheClientAutoconfigRecordsByDefault(t *testing.T) {
	h := newHarness(t)
	zoneValue := h.zoneNamed(t, "acme.dev")

	response := bind(t, h, zoneValue, nil)
	if !response.GetDomain().GetPublishClientAutoconfig() {
		t.Error("publish_client_autoconfig is false after a default bind")
	}

	got := autoconfigLines(summarize(h.records(t, zoneValue.ID), zone.SourceMail))
	if !slices.Equal(got, autoconfigSet) {
		t.Errorf("published autoconfiguration set:\n got %v\nwant %v", got, autoconfigSet)
	}
	if !response.GetDomain().GetRecordsInSync() {
		t.Error("records_in_sync is false right after a bind")
	}

	// The records the deployment cannot serve stay out however the switch is
	// set: MTA-STS needs an HTTPS policy endpoint, TLS-RPT reports on policies
	// this facade does not publish, and ua-auto-config is the server's own
	// scheme with the same certificate problem as MTA-STS.
	for _, record := range h.records(t, zoneValue.ID) {
		if record.Source != zone.SourceMail {
			continue
		}
		switch record.Name {
		case "mta-sts", "_mta-sts", "_smtp._tls", "ua-auto-config", "_ua-auto-config":
			t.Errorf("published %s %s, which this deployment cannot serve", record.Type, record.Name)
		}
	}
}

func TestBindCanOptOutOfTheClientAutoconfigRecords(t *testing.T) {
	h := newHarness(t)
	zoneValue := h.zoneNamed(t, "acme.dev")

	response := bind(t, h, zoneValue, &mailv1.BindMailDomainRequest{
		PublishClientAutoconfig: proto.Bool(false),
	})
	if response.GetDomain().GetPublishClientAutoconfig() {
		t.Error("publish_client_autoconfig is true after an explicit opt-out")
	}
	if got := autoconfigLines(summarize(h.records(t, zoneValue.ID), zone.SourceMail)); len(got) != 0 {
		t.Errorf("published %v despite the opt-out", got)
	}
}

// An existing binding is the case that matters most: it was made before these
// records existed, so its row carries the zero value and nothing may appear in
// its zone until somebody asks.
func TestAnExistingBindingNeverGainsTheRecordsOnItsOwn(t *testing.T) {
	h := newHarness(t)
	zoneValue := h.zoneNamed(t, "acme.dev")
	bind(t, h, zoneValue, &mailv1.BindMailDomainRequest{PublishClientAutoconfig: proto.Bool(false)})

	// A row written by an older build has no such field at all; the decoded
	// zero value is what the reconciler then works from.
	doc, err := h.store.GetMailDomain(t.Context(), zoneValue.ID)
	if err != nil {
		t.Fatalf("GetMailDomain: %v", err)
	}
	doc.PublishClientAutoconfig = false
	if err := h.store.PutMailDomain(t.Context(), doc); err != nil {
		t.Fatalf("PutMailDomain: %v", err)
	}

	for range 3 {
		if err := h.service.ReconcileAll(t.Context()); err != nil {
			t.Fatalf("ReconcileAll: %v", err)
		}
	}
	if got := autoconfigLines(summarize(h.records(t, zoneValue.ID), zone.SourceMail)); len(got) != 0 {
		t.Errorf("the reconciler added %v to an existing binding", got)
	}
}

func TestUpdateTurnsTheClientAutoconfigRecordsOnAndOffAgain(t *testing.T) {
	h := newHarness(t)
	zoneValue := h.zoneNamed(t, "acme.dev")
	bind(t, h, zoneValue, &mailv1.BindMailDomainRequest{PublishClientAutoconfig: proto.Bool(false)})

	on, err := h.service.UpdateMailDomain(t.Context(), connect.NewRequest(&mailv1.UpdateMailDomainRequest{
		ZoneId:                  zoneValue.ID,
		PublishClientAutoconfig: proto.Bool(true),
	}))
	if err != nil {
		t.Fatalf("UpdateMailDomain(on): %v", err)
	}
	if !on.Msg.GetDomain().GetPublishClientAutoconfig() {
		t.Error("publish_client_autoconfig is false after turning it on")
	}
	if got := autoconfigLines(summarize(h.records(t, zoneValue.ID), zone.SourceMail)); !slices.Equal(got, autoconfigSet) {
		t.Errorf("after turning it on:\n got %v\nwant %v", got, autoconfigSet)
	}
	// The DMARC policy must survive an update that only moved this switch.
	if on.Msg.GetDomain().GetDmarcPolicy() != "quarantine" {
		t.Errorf("dmarc policy = %q, want quarantine", on.Msg.GetDomain().GetDmarcPolicy())
	}

	off, err := h.service.UpdateMailDomain(t.Context(), connect.NewRequest(&mailv1.UpdateMailDomainRequest{
		ZoneId:                  zoneValue.ID,
		PublishClientAutoconfig: proto.Bool(false),
	}))
	if err != nil {
		t.Fatalf("UpdateMailDomain(off): %v", err)
	}
	if off.Msg.GetDomain().GetPublishClientAutoconfig() {
		t.Error("publish_client_autoconfig is true after turning it off")
	}
	if got := autoconfigLines(summarize(h.records(t, zoneValue.ID), zone.SourceMail)); len(got) != 0 {
		t.Errorf("turning it off left %v behind", got)
	}
	// Turning it off removes only this engine's records; the policy set stays.
	if got := summarize(h.records(t, zoneValue.ID), zone.SourceMail); len(got) != 5 {
		t.Errorf("the policy set did not survive: %v", got)
	}
}

// Drift is the reconciler's job: a record an operator deleted comes back, and
// one they retargeted is reported rather than silently overwritten.
func TestTheReconcilerRepublishesADeletedAutoconfigRecord(t *testing.T) {
	h := newHarness(t)
	zoneValue := h.zoneNamed(t, "acme.dev")
	bind(t, h, zoneValue, nil)

	var target zone.Record
	for _, record := range h.records(t, zoneValue.ID) {
		if record.Name == "_imaps._tcp" && record.Source == zone.SourceMail {
			target = record
		}
	}
	if target.ID == "" {
		t.Fatal("the _imaps._tcp record was not published")
	}
	// An engine record cannot be deleted through the ordinary CRUD path, so the
	// deletion goes through the engine mutator — exactly as an operator's
	// REPLACE import would.
	if _, _, err := h.zones.ApplyRecordSet(t.Context(), zoneValue.ID,
		[]enginedns.Op{{Kind: enginedns.OpDelete, RecordID: target.ID, Record: target}}); err != nil {
		t.Fatalf("ApplyRecordSet: %v", err)
	}
	if err := h.service.ReconcileAll(t.Context()); err != nil {
		t.Fatalf("ReconcileAll: %v", err)
	}
	if got := autoconfigLines(summarize(h.records(t, zoneValue.ID), zone.SourceMail)); !slices.Equal(got, autoconfigSet) {
		t.Errorf("after the reconcile:\n got %v\nwant %v", got, autoconfigSet)
	}
}

// An identical record an operator made by hand is adopted, not duplicated —
// the same rule the policy records follow.
func TestAnExistingUserAutoconfigRecordIsAdopted(t *testing.T) {
	h := newHarness(t)
	zoneValue := h.zoneNamed(t, "acme.dev")
	existing := h.record(t, zoneValue.ID, "autoconfig", zone.TypeCNAME, 300, "mail.local.test.")

	bind(t, h, zoneValue, nil)

	var adopted bool
	for _, record := range h.records(t, zoneValue.ID) {
		if record.ID != existing.ID {
			continue
		}
		adopted = true
		if record.Source != zone.SourceMail {
			t.Errorf("the existing record was not adopted: source = %q", record.Source)
		}
	}
	if !adopted {
		t.Error("the existing record was replaced rather than adopted")
	}
}

// A user record that stands in the way is reported, never deleted by a worker.
func TestAConflictingUserAutoconfigRecordIsReported(t *testing.T) {
	h := newHarness(t)
	zoneValue := h.zoneNamed(t, "acme.dev")
	h.record(t, zoneValue.ID, "autodiscover", zone.TypeCNAME, 300, "outlook.example.net.")

	_, err := h.service.BindMailDomain(t.Context(), connect.NewRequest(&mailv1.BindMailDomainRequest{
		ZoneId: zoneValue.ID,
	}))
	if err == nil {
		t.Fatal("the bind did not refuse a conflicting autodiscover record")
	}
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("code = %v, want FailedPrecondition", connect.CodeOf(err))
	}
	var kept bool
	for _, record := range h.records(t, zoneValue.ID) {
		if record.Name == "autodiscover" && record.Source == zone.SourceUser {
			kept = true
		}
	}
	if !kept {
		t.Error("the refused bind removed the operator's record")
	}
	if _, err := h.store.GetMailDomain(t.Context(), zoneValue.ID); err == nil {
		t.Error("the refused bind persisted a row")
	}
}

// A dry run for a domain the engine has not created yet cannot know the ports,
// so it names the owners and says where they come from rather than inventing
// values.
func TestBindDryRunNamesTheAutoconfigRecordsItWouldCreate(t *testing.T) {
	h := newHarness(t)
	zoneValue := h.zoneNamed(t, "acme.dev")

	response := bind(t, h, zoneValue, &mailv1.BindMailDomainRequest{DryRun: true})
	names := map[string]string{}
	for _, change := range response.GetDnsPlan() {
		names[change.GetName()] = change.GetType()
	}
	for _, owner := range append(slices.Clone(autoconfigSRVOwners), autoconfigCNAMEOwners...) {
		if _, ok := names[owner]; !ok {
			t.Errorf("the dry run did not name %s", owner)
		}
	}
	if got := summarize(h.records(t, zoneValue.ID), zone.SourceMail); len(got) != 0 {
		t.Errorf("a dry run wrote records: %v", got)
	}

	// And an opt-out dry run must not offer them.
	optOut := bind(t, h, zoneValue, &mailv1.BindMailDomainRequest{
		DryRun:                  true,
		PublishClientAutoconfig: proto.Bool(false),
	})
	for _, change := range optOut.GetDnsPlan() {
		if change.GetName() == "_imaps._tcp" {
			t.Error("an opt-out dry run offered the autoconfiguration records")
		}
	}
}

// A rebuild has no stored switch to read, so it reads the zone: a binding whose
// records are published had it on.
func TestRebuildRecoversTheAutoconfigSwitchFromTheZone(t *testing.T) {
	for _, on := range []bool{true, false} {
		t.Run(map[bool]string{true: "published", false: "not published"}[on], func(t *testing.T) {
			h := newHarness(t)
			zoneValue := h.zoneNamed(t, "acme.dev")
			bind(t, h, zoneValue, &mailv1.BindMailDomainRequest{PublishClientAutoconfig: proto.Bool(on)})

			if err := h.store.DeleteMailDomain(t.Context(), zoneValue.ID); err != nil {
				t.Fatalf("DeleteMailDomain: %v", err)
			}
			if _, err := h.service.Rebuild(t.Context(), false); err != nil {
				t.Fatalf("Rebuild: %v", err)
			}
			doc, err := h.store.GetMailDomain(t.Context(), zoneValue.ID)
			if err != nil {
				t.Fatalf("GetMailDomain: %v", err)
			}
			if doc.PublishClientAutoconfig != on {
				t.Errorf("recovered switch = %v, want %v", doc.PublishClientAutoconfig, on)
			}
		})
	}
}

// The mail host's own name is what every one of these records targets, so the
// same zone file yields nothing at all for a facade configured with another
// hostname. The engine's real output is used rather than a fixture, so the
// allow-list is checked against what the server actually renders.
func TestAutoconfigRecordsFollowTheConfiguredHostname(t *testing.T) {
	h := newHarness(t)
	zoneValue := h.zoneNamed(t, "acme.dev")
	bind(t, h, zoneValue, nil)

	doc, err := h.store.GetMailDomain(t.Context(), zoneValue.ID)
	if err != nil {
		t.Fatalf("GetMailDomain: %v", err)
	}
	domain, err := h.engine.GetDomain(t.Context(), doc.EngineDomainID)
	if err != nil {
		t.Fatalf("GetDomain: %v", err)
	}
	if got := parseAutoconfigRecords(domain.DNSZoneFile, "acme.dev", "mail.elsewhere.test"); len(got) != 0 {
		t.Errorf("published %d records for a hostname the server does not use", len(got))
	}
	got := parseAutoconfigRecords(domain.DNSZoneFile, "acme.dev", fakemail.DefaultHostname)
	if len(got) != len(autoconfigSet) {
		t.Errorf("parsed %d records, want %d", len(got), len(autoconfigSet))
	}
}
