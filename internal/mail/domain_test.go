package mail

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	mailv1 "github.com/castlemilk/dns/gen/go/mail/v1"
	"github.com/castlemilk/dns/internal/activity"
	"github.com/castlemilk/dns/internal/config"
	"github.com/castlemilk/dns/internal/enginedns"
	"github.com/castlemilk/dns/internal/zone"
	"google.golang.org/protobuf/proto"
)

// bind is the happy path every test builds on.
func bind(t *testing.T, h *harness, zoneValue zone.Zone, request *mailv1.BindMailDomainRequest) *mailv1.BindMailDomainResponse {
	t.Helper()
	if request == nil {
		request = &mailv1.BindMailDomainRequest{}
	}
	request.ZoneId = zoneValue.ID
	response, err := h.service.BindMailDomain(t.Context(), connect.NewRequest(request))
	if err != nil {
		t.Fatalf("BindMailDomain: %v", err)
	}
	return response.Msg
}

// summarize renders a zone's records as "TYPE name value" lines, sorted, so a
// test can assert the whole published set at once.
func summarize(records []zone.Record, source string) []string {
	var lines []string
	for _, record := range records {
		// Managed NS and SOA records carry the user source; they are the
		// store's own and are never part of what a test is asserting about.
		if record.Managed || record.Source != source {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s %s %s", record.Type, record.Name, record.Value))
	}
	sort.Strings(lines)
	return lines
}

func TestBindWritesExactlyTheConfiguredRecordSet(t *testing.T) {
	h := newHarness(t)
	zoneValue := h.zoneNamed(t, "acme.dev")

	// publish_client_autoconfig off, so this test still describes exactly the
	// policy set §3.3 prescribes; the autoconfiguration set has its own tests.
	response := bind(t, h, zoneValue, &mailv1.BindMailDomainRequest{
		PublishClientAutoconfig: proto.Bool(false),
	})
	if response.GetDomain().GetState() != mailv1.MailDomainState_MAIL_DOMAIN_STATE_BOUND {
		t.Fatalf("state = %v, reason %q", response.GetDomain().GetState(), response.GetDomain().GetReason())
	}

	got := summarize(h.records(t, zoneValue.ID), zone.SourceMail)
	want := []string{
		`MX @ 10 mail.local.test.`,
		`TXT @ "v=spf1 mx -all"`,
		`TXT _dmarc "v=DMARC1; p=quarantine; rua=mailto:postmaster@acme.dev"`,
		`TXT v1-ed25519-20260903._domainkey "v=DKIM1; k=ed25519; h=sha256; p=5t9iUKLc7NJ5SmfF8N9DBNAn74vuywt3j5mCnUDQmCM="`,
	}
	// The RSA record is long and its exact key is the fake's; assert it
	// separately so the rest stays readable.
	if len(got) != 5 {
		t.Fatalf("published %d records:\n%s", len(got), strings.Join(got, "\n"))
	}
	rsa := got[4]
	got = slices.Delete(got, 4, 5)
	if !slices.Equal(got, want) {
		t.Errorf("published set:\n got %v\nwant %v", got, want)
	}
	if !strings.HasPrefix(rsa, `TXT v1-rsa-20260903._domainkey "v=DKIM1; k=rsa;`) {
		t.Errorf("rsa record = %s", rsa)
	}

	// Nothing else from the server's zone file — no SRV, MTA-STS, autoconfig,
	// TLS-RPT, and nothing for the mail host's own name.
	for _, record := range h.records(t, zoneValue.ID) {
		if record.Source != zone.SourceMail {
			continue
		}
		if strings.Contains(record.Name, "_tcp") || strings.Contains(record.Name, "mta-sts") ||
			strings.Contains(record.Name, "autoconfig") || strings.Contains(record.Name, "autodiscover") ||
			strings.Contains(record.Name, "_smtp._tls") {
			t.Errorf("published a record the deployment cannot serve: %s %s", record.Type, record.Name)
		}
	}

	if !h.events.has(activity.KindMailDomainBound) {
		t.Errorf("no bind event was recorded; kinds = %v", h.events.kinds())
	}
	if !response.GetDomain().GetRecordsInSync() {
		t.Error("records_in_sync is false right after a bind")
	}
}

func TestBindUsesTheConfiguredMxPriorityAndSpfInclude(t *testing.T) {
	h := newHarness(t, func(cfg *config.Mail) {
		cfg.MXPriority = 20
		cfg.SPFInclude = "spf.simple.host"
		cfg.ReportAddress = "dmarc@simple.host"
		cfg.DMARCPolicy = "reject"
	})
	zoneValue := h.zoneNamed(t, "acme.dev")
	bind(t, h, zoneValue, nil)

	got := summarize(h.records(t, zoneValue.ID), zone.SourceMail)
	if !slices.Contains(got, `MX @ 20 mail.local.test.`) {
		t.Errorf("MX priority not honoured: %v", got)
	}
	if !slices.Contains(got, `TXT @ "v=spf1 mx include:spf.simple.host -all"`) {
		t.Errorf("SPF include not honoured: %v", got)
	}
	if !slices.Contains(got, `TXT _dmarc "v=DMARC1; p=reject; rua=mailto:dmarc@simple.host"`) {
		t.Errorf("DMARC not honoured: %v", got)
	}
}

func TestBindHonoursTheRequestedDmarcPolicy(t *testing.T) {
	h := newHarness(t)
	zoneValue := h.zoneNamed(t, "acme.dev")
	bind(t, h, zoneValue, &mailv1.BindMailDomainRequest{DmarcPolicy: "none"})
	got := summarize(h.records(t, zoneValue.ID), zone.SourceMail)
	if !slices.Contains(got, `TXT _dmarc "v=DMARC1; p=none; rua=mailto:postmaster@acme.dev"`) {
		t.Errorf("policy not honoured: %v", got)
	}
}

func TestBindRejectsAnUnknownDmarcPolicy(t *testing.T) {
	h := newHarness(t)
	zoneValue := h.zoneNamed(t, "acme.dev")
	_, err := h.service.BindMailDomain(t.Context(), connect.NewRequest(&mailv1.BindMailDomainRequest{
		ZoneId: zoneValue.ID, DmarcPolicy: "relaxed",
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("code = %v (%v)", connect.CodeOf(err), err)
	}
}

// The mail server's own DMARC is hard-coded p=reject and its MX names whatever
// it currently calls itself. Neither may reach the customer's zone.
func TestBindNeverCopiesTheEnginesOwnPolicyRecords(t *testing.T) {
	h := newHarness(t)
	zoneValue := h.zoneNamed(t, "acme.dev")
	bind(t, h, zoneValue, nil)
	for _, record := range h.records(t, zoneValue.ID) {
		if record.Type == zone.TypeTXT && strings.Contains(record.Value, "p=reject") {
			t.Errorf("the engine's own DMARC policy was published: %s", record.Value)
		}
	}
}

func TestBindRefusesWhenTheServerHostnameDiffers(t *testing.T) {
	h := newHarness(t)
	h.engine.SetHostname("mx9.elsewhere.test")
	zoneValue := h.zoneNamed(t, "acme.dev")
	_, err := h.service.BindMailDomain(t.Context(), connect.NewRequest(&mailv1.BindMailDomainRequest{ZoneId: zoneValue.ID}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("code = %v (%v)", connect.CodeOf(err), err)
	}
	if !strings.Contains(err.Error(), "mx9.elsewhere.test") {
		t.Errorf("the message does not name the mismatch: %v", err)
	}
	if len(summarize(h.records(t, zoneValue.ID), zone.SourceMail)) != 0 {
		t.Error("a refused bind published records")
	}
}

// A server that renames itself after a bind must not have its new name
// published as MX by a background worker.
func TestAHostnameChangeAfterBindDegradesWithoutTouchingMx(t *testing.T) {
	h := newHarness(t)
	zoneValue := h.zoneNamed(t, "acme.dev")
	bind(t, h, zoneValue, nil)
	before := summarize(h.records(t, zoneValue.ID), zone.SourceMail)

	h.engine.SetHostname("mx9.elsewhere.test")
	if err := h.service.ReconcileAll(t.Context()); err != nil {
		t.Fatalf("ReconcileAll: %v", err)
	}
	after := summarize(h.records(t, zoneValue.ID), zone.SourceMail)
	if !slices.Equal(before, after) {
		t.Errorf("the reconciler rewrote records after a hostname change:\n before %v\n after  %v", before, after)
	}
	doc, err := h.store.GetMailDomain(t.Context(), zoneValue.ID)
	if err != nil {
		t.Fatalf("GetMailDomain: %v", err)
	}
	if doc.State != StateDegraded || !strings.Contains(doc.Reason, "mx9.elsewhere.test") {
		t.Errorf("row = %s / %q", doc.State, doc.Reason)
	}
}

func TestBindRefusesConflictsAndWritesNothing(t *testing.T) {
	h := newHarness(t)
	zoneValue := h.zoneNamed(t, "acme.dev")
	h.record(t, zoneValue.ID, "@", zone.TypeMX, 3600, "10 mail.google.com.")

	_, err := h.service.BindMailDomain(t.Context(), connect.NewRequest(&mailv1.BindMailDomainRequest{ZoneId: zoneValue.ID}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("code = %v (%v)", connect.CodeOf(err), err)
	}
	if !strings.Contains(err.Error(), "conflict") {
		t.Errorf("message = %v", err)
	}
	// Nothing was persisted, created on the engine, or written to DNS.
	if _, storeErr := h.store.GetMailDomain(t.Context(), zoneValue.ID); storeErr == nil {
		t.Error("a refused bind persisted a row")
	}
	if domains := h.engineDomains(t); len(domains) != 0 {
		t.Errorf("a refused bind created %d engine domains", len(domains))
	}
	if len(summarize(h.records(t, zoneValue.ID), zone.SourceMail)) != 0 {
		t.Error("a refused bind published records")
	}
}

func TestBindReplacesConflictsWhenAsked(t *testing.T) {
	h := newHarness(t)
	zoneValue := h.zoneNamed(t, "acme.dev")
	h.record(t, zoneValue.ID, "@", zone.TypeMX, 3600, "10 mail.google.com.")
	h.record(t, zoneValue.ID, "@", zone.TypeTXT, 3600, "v=spf1 include:_spf.google.com ~all")
	// An unrelated verification TXT at the apex is none of this facade's
	// business and must survive.
	h.record(t, zoneValue.ID, "@", zone.TypeTXT, 3600, "google-site-verification=abc123")

	bind(t, h, zoneValue, &mailv1.BindMailDomainRequest{ReplaceConflictingRecords: true})

	user := summarize(h.records(t, zoneValue.ID), zone.SourceUser)
	if !slices.Contains(user, `TXT @ "google-site-verification=abc123"`) {
		t.Errorf("an unrelated TXT was removed: %v", user)
	}
	for _, line := range user {
		if strings.Contains(line, "google.com") {
			t.Errorf("a conflicting record survived: %s", line)
		}
	}
}

func TestBindAdoptsAnEqualUserRecordKeepingItsId(t *testing.T) {
	h := newHarness(t)
	zoneValue := h.zoneNamed(t, "acme.dev")
	existing := h.record(t, zoneValue.ID, "@", zone.TypeMX, 3600, "10 mail.local.test.")

	bind(t, h, zoneValue, nil)

	for _, record := range h.records(t, zoneValue.ID) {
		if record.ID == existing.ID {
			if record.Source != zone.SourceMail {
				t.Errorf("the equal record was not adopted: %#v", record)
			}
			return
		}
	}
	t.Error("the adopted record lost its id")
}

// A DKIM key stored as the parenthesised two-string form must be recognised as
// the same record, never duplicated.
func TestBindAdoptsASplitDkimRecord(t *testing.T) {
	h := newHarness(t)
	zoneValue := h.zoneNamed(t, "acme.dev")

	// Bind once to learn the value the server wants, then start over with that
	// value pre-published in split form.
	bind(t, h, zoneValue, nil)
	var rsaValue string
	for _, record := range h.records(t, zoneValue.ID) {
		if strings.HasPrefix(record.Name, "v1-rsa-") {
			rsaValue = enginedns.TxtText(record.Value)
		}
	}
	if rsaValue == "" {
		t.Fatal("no RSA record was published")
	}

	other := newHarness(t)
	otherZone := other.zoneNamed(t, "acme.dev")
	split := fmt.Sprintf("%q %q", rsaValue[:200], rsaValue[200:])
	existing := other.record(t, otherZone.ID, "v1-rsa-20260903._domainkey", zone.TypeTXT, 3600, split)

	bind(t, other, otherZone, nil)

	count := 0
	for _, record := range other.records(t, otherZone.ID) {
		if strings.HasPrefix(record.Name, "v1-rsa-") {
			count++
			if record.ID != existing.ID {
				t.Errorf("a duplicate DKIM record was created: %#v", record)
			}
			if record.Source != zone.SourceMail {
				t.Errorf("the split record was not adopted: %#v", record)
			}
		}
	}
	if count != 1 {
		t.Errorf("the zone holds %d RSA DKIM records, want 1", count)
	}
}

// An imported zone carries TTL 0; the plan must align the RRset before adding
// a member, or the store refuses the write.
func TestBindAlignsATtlZeroRrset(t *testing.T) {
	h := newHarness(t)
	zoneValue := h.zoneNamed(t, "acme.dev")
	imported, err := zone.NormalizeImportedRecord("acme.dev", "@", zone.TypeTXT, 0, "google-site-verification=abc123")
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if _, err := h.zones.CreateRecord(t.Context(), zoneValue.ID, imported); err != nil {
		t.Fatalf("create imported record: %v", err)
	}

	bind(t, h, zoneValue, nil)

	for _, record := range h.records(t, zoneValue.ID) {
		if record.Name == "@" && record.Type == zone.TypeTXT && record.TTL == 0 {
			t.Errorf("a TTL-0 member survived the alignment: %#v", record)
		}
	}
}

func TestBindWaitsForTheRsaKey(t *testing.T) {
	h := newHarness(t)
	h.engine.SetDkimDelay(2 * time.Second)
	zoneValue := h.zoneNamed(t, "acme.dev")

	// The fake's clock is the harness clock, and the facade's poll sleeps on the
	// wall clock. Keep advancing until the bind returns rather than advancing
	// once after a fixed sleep: a single advance that lands before the fake
	// stamps the domain's creation time only moves rsaAt forward with it, and
	// the bind would then wait out its whole budget.
	done := make(chan struct{})
	advanced := make(chan struct{})
	go func() {
		defer close(advanced)
		for {
			select {
			case <-done:
				return
			case <-time.After(20 * time.Millisecond):
				h.advance(3 * time.Second)
			}
		}
	}()
	response := bind(t, h, zoneValue, nil)
	close(done)
	<-advanced

	if response.GetDomain().GetDkimPending() {
		t.Error("the bind reported the keys as pending after waiting for them")
	}
	published := summarize(h.records(t, zoneValue.ID), zone.SourceMail)
	rsa, ed := 0, 0
	for _, line := range published {
		if strings.Contains(line, "v1-rsa-") {
			rsa++
		}
		if strings.Contains(line, "v1-ed25519-") {
			ed++
		}
	}
	if rsa != 1 || ed != 1 {
		t.Errorf("published %d RSA and %d Ed25519 records:\n%s", rsa, ed, strings.Join(published, "\n"))
	}
}

func TestALongDkimDelayBindsDegradedAndTheReconcilerFinishesIt(t *testing.T) {
	h := newHarness(t)
	h.engine.SetDkimDelay(time.Hour)
	zoneValue := h.zoneNamed(t, "acme.dev")

	// Shorten the in-handler wait so the test does not sit for 20 s; the
	// behaviour under test is what happens when it expires.
	response := bindWithShortWait(t, h, zoneValue)
	if !response.GetDomain().GetDkimPending() {
		t.Fatal("dkim_pending is false although the RSA key is missing")
	}
	if response.GetDomain().GetState() != mailv1.MailDomainState_MAIL_DOMAIN_STATE_DEGRADED {
		t.Errorf("state = %v", response.GetDomain().GetState())
	}
	published := summarize(h.records(t, zoneValue.ID), zone.SourceMail)
	if slices.ContainsFunc(published, func(line string) bool { return strings.Contains(line, "v1-rsa-") }) {
		t.Errorf("an absent RSA key was published: %v", published)
	}

	h.advance(2 * time.Hour)
	if err := h.service.ReconcileAll(t.Context()); err != nil {
		t.Fatalf("ReconcileAll: %v", err)
	}
	published = summarize(h.records(t, zoneValue.ID), zone.SourceMail)
	if !slices.ContainsFunc(published, func(line string) bool { return strings.Contains(line, "v1-rsa-") }) {
		t.Errorf("the reconciler did not publish the RSA key:\n%s", strings.Join(published, "\n"))
	}
	doc, err := h.store.GetMailDomain(t.Context(), zoneValue.ID)
	if err != nil {
		t.Fatalf("GetMailDomain: %v", err)
	}
	if doc.State != StateBound || doc.DkimPending {
		t.Errorf("row = %s pending=%v reason=%q", doc.State, doc.DkimPending, doc.Reason)
	}
	if !h.events.has(activity.KindMailRecordsChanged) {
		t.Errorf("no records.changed event: %v", h.events.kinds())
	}
}

// bindWithShortWait runs a bind whose DKIM wait expires immediately, so the
// expiry path is exercised without sitting for the production budget.
func bindWithShortWait(t *testing.T, h *harness, zoneValue zone.Zone) *mailv1.BindMailDomainResponse {
	t.Helper()
	h.service.dkimBudget = 0
	defer func() { h.service.dkimBudget = dkimReadyBudget }()
	return bind(t, h, zoneValue, nil)
}

func TestBindPersistsTheRowBeforeAnyEngineWriteAndResumes(t *testing.T) {
	h := newHarness(t)
	zoneValue := h.zoneNamed(t, "acme.dev")

	// Simulate a crash right after the row was persisted: the row exists, the
	// engine domain does not.
	now := h.now()
	if err := h.store.PutMailDomain(t.Context(), mailDomainDoc(zoneValue, now)); err != nil {
		t.Fatalf("PutMailDomain: %v", err)
	}
	if err := h.service.ReconcileAll(t.Context()); err != nil {
		t.Fatalf("ReconcileAll: %v", err)
	}

	doc, err := h.store.GetMailDomain(t.Context(), zoneValue.ID)
	if err != nil {
		t.Fatalf("GetMailDomain: %v", err)
	}
	if doc.State != StateBound {
		t.Fatalf("the interrupted bind did not finish: %s / %q", doc.State, doc.Reason)
	}
	if doc.EngineDomainID == "" {
		t.Error("no engine domain was created")
	}
	if len(summarize(h.records(t, zoneValue.ID), zone.SourceMail)) != 5 {
		t.Errorf("records after resume: %v", summarize(h.records(t, zoneValue.ID), zone.SourceMail))
	}
}

func TestBindRefusesADomainSomebodyElseCreated(t *testing.T) {
	h := newHarness(t)
	zoneValue := h.zoneNamed(t, "acme.dev")
	// A domain of the same name that this control plane did not create: the
	// description does not match, so it is never quietly taken over.
	if _, err := h.engine.CreateDomain(t.Context(), DomainInput{
		Name: "acme.dev", Description: "hand made", ReportAddressURI: "mailto:postmaster",
	}); err != nil {
		t.Fatalf("CreateDomain: %v", err)
	}
	_, err := h.service.BindMailDomain(t.Context(), connect.NewRequest(&mailv1.BindMailDomainRequest{ZoneId: zoneValue.ID}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("code = %v (%v)", connect.CodeOf(err), err)
	}
	if !strings.Contains(err.Error(), "already exists on the mail server") {
		t.Errorf("message = %v", err)
	}
}

func TestBindIsRefusedTwice(t *testing.T) {
	h := newHarness(t)
	zoneValue := h.zoneNamed(t, "acme.dev")
	bind(t, h, zoneValue, nil)
	_, err := h.service.BindMailDomain(t.Context(), connect.NewRequest(&mailv1.BindMailDomainRequest{ZoneId: zoneValue.ID}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("code = %v (%v)", connect.CodeOf(err), err)
	}
}

func TestDryRunWritesNothingAndNamesThePlaceholders(t *testing.T) {
	h := newHarness(t)
	zoneValue := h.zoneNamed(t, "acme.dev")
	response := bind(t, h, zoneValue, &mailv1.BindMailDomainRequest{DryRun: true})

	if !response.GetDryRun() {
		t.Error("dry_run was not echoed")
	}
	if response.GetDomain() != nil {
		t.Error("a dry run returned a domain row")
	}
	if _, err := h.store.GetMailDomain(t.Context(), zoneValue.ID); err == nil {
		t.Error("a dry run persisted a row")
	}
	if domains := h.engineDomains(t); len(domains) != 0 {
		t.Errorf("a dry run created %d engine domains", len(domains))
	}
	placeholders := 0
	for _, change := range response.GetDnsPlan() {
		if change.GetName() == "<selector>._domainkey" {
			placeholders++
			if !strings.Contains(change.GetWhy(), "generated by the mail server on bind") {
				t.Errorf("placeholder why = %q", change.GetWhy())
			}
		}
	}
	if placeholders != 2 {
		t.Errorf("got %d DKIM placeholders, want one per algorithm", placeholders)
	}
}

func TestZoneNamesThatAreNotHostnamesAreRefused(t *testing.T) {
	h := newHarness(t)
	// A single-label zone is a legal store name but not a mail domain.
	zoneValue := h.zoneNamed(t, "localhost")
	_, err := h.service.BindMailDomain(t.Context(), connect.NewRequest(&mailv1.BindMailDomainRequest{ZoneId: zoneValue.ID}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("code = %v (%v)", connect.CodeOf(err), err)
	}
}

func TestUpdateRepublishesTheDmarcRecord(t *testing.T) {
	h := newHarness(t)
	zoneValue := h.zoneNamed(t, "acme.dev")
	bind(t, h, zoneValue, nil)

	policy := "reject"
	if _, err := h.service.UpdateMailDomain(t.Context(), connect.NewRequest(&mailv1.UpdateMailDomainRequest{
		ZoneId: zoneValue.ID, DmarcPolicy: &policy,
	})); err != nil {
		t.Fatalf("UpdateMailDomain: %v", err)
	}
	got := summarize(h.records(t, zoneValue.ID), zone.SourceMail)
	if !slices.Contains(got, `TXT _dmarc "v=DMARC1; p=reject; rua=mailto:postmaster@acme.dev"`) {
		t.Errorf("DMARC was not republished: %v", got)
	}
}

func TestUnbindRefusesWhileMailboxesExist(t *testing.T) {
	h := newHarness(t)
	zoneValue := h.zoneNamed(t, "acme.dev")
	bind(t, h, zoneValue, nil)
	createMailbox(t, h, zoneValue.ID, "mara")

	_, err := h.service.UnbindMailDomain(t.Context(), connect.NewRequest(&mailv1.UnbindMailDomainRequest{ZoneId: zoneValue.ID}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("code = %v (%v)", connect.CodeOf(err), err)
	}
	if !strings.Contains(err.Error(), "1 mailboxes") {
		t.Errorf("message = %v", err)
	}

	// With the flag but without the typed confirmation it is still refused.
	_, err = h.service.UnbindMailDomain(t.Context(), connect.NewRequest(&mailv1.UnbindMailDomainRequest{
		ZoneId: zoneValue.ID, DeleteMailboxes: true,
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("code = %v (%v)", connect.CodeOf(err), err)
	}
}

func TestUnbindDestroysEverythingInOrder(t *testing.T) {
	h := newHarness(t)
	zoneValue := h.zoneNamed(t, "acme.dev")
	bind(t, h, zoneValue, nil)
	createMailbox(t, h, zoneValue.ID, "mara")
	createForwarder(t, h, zoneValue.ID, "team", []string{"someone@example.com"})

	response, err := h.service.UnbindMailDomain(t.Context(), connect.NewRequest(&mailv1.UnbindMailDomainRequest{
		ZoneId: zoneValue.ID, DeleteMailboxes: true, ConfirmZoneName: "acme.dev",
	}))
	if err != nil {
		t.Fatalf("UnbindMailDomain: %v", err)
	}
	if response.Msg.GetMailboxesDeleted() != 1 || response.Msg.GetForwardersDeleted() != 1 {
		t.Errorf("deleted %d mailboxes and %d forwarders", response.Msg.GetMailboxesDeleted(), response.Msg.GetForwardersDeleted())
	}
	// The engine holds nothing: the objectIsLinked refusal only clears when
	// accounts, lists and signatures are gone first.
	if domains := h.engineDomains(t); len(domains) != 0 {
		t.Errorf("the engine still holds %d domains", len(domains))
	}
	if len(summarize(h.records(t, zoneValue.ID), zone.SourceMail)) != 0 {
		t.Errorf("records survived the unbind: %v", summarize(h.records(t, zoneValue.ID), zone.SourceMail))
	}
	if _, err := h.store.GetMailDomain(t.Context(), zoneValue.ID); err == nil {
		t.Error("the row survived the unbind")
	}
}

func TestUnbindCanKeepTheRecordsAsEditableUserRecords(t *testing.T) {
	h := newHarness(t)
	zoneValue := h.zoneNamed(t, "acme.dev")
	bind(t, h, zoneValue, nil)

	published := summarize(h.records(t, zoneValue.ID), zone.SourceMail)
	if _, err := h.service.UnbindMailDomain(t.Context(), connect.NewRequest(&mailv1.UnbindMailDomainRequest{
		ZoneId: zoneValue.ID, KeepDnsRecords: true,
	})); err != nil {
		t.Fatalf("UnbindMailDomain: %v", err)
	}
	if len(summarize(h.records(t, zoneValue.ID), zone.SourceMail)) != 0 {
		t.Error("records are still engine-owned")
	}
	// Every record the binding published — the policy set and the
	// client-autoconfiguration set alike — survives as an editable user record.
	if kept := summarize(h.records(t, zoneValue.ID), zone.SourceUser); !slices.Equal(kept, published) {
		t.Errorf("kept records:\n got %v\nwant %v", kept, published)
	}
}

func TestOrphanedRecordsAreReleasedNotDeleted(t *testing.T) {
	h := newHarness(t)
	zoneValue := h.zoneNamed(t, "acme.dev")
	bind(t, h, zoneValue, nil)

	published := summarize(h.records(t, zoneValue.ID), zone.SourceMail)
	// Simulate a crash between the engine step and the row delete.
	if err := h.store.DeleteMailDomain(t.Context(), zoneValue.ID); err != nil {
		t.Fatalf("DeleteMailDomain: %v", err)
	}
	if err := h.service.ReconcileAll(t.Context()); err != nil {
		t.Fatalf("ReconcileAll: %v", err)
	}
	if len(summarize(h.records(t, zoneValue.ID), zone.SourceMail)) != 0 {
		t.Error("orphaned records are still engine-owned")
	}
	if released := summarize(h.records(t, zoneValue.ID), zone.SourceUser); !slices.Equal(released, published) {
		t.Errorf("orphaned records were not released intact:\n got %v\nwant %v", released, published)
	}
	if !h.events.has(activity.KindDNSEngineOrphaned) {
		t.Errorf("no orphan event: %v", h.events.kinds())
	}
}

func TestAMissingZoneMarksTheRowOnce(t *testing.T) {
	h := newHarness(t)
	zoneValue := h.zoneNamed(t, "acme.dev")
	bind(t, h, zoneValue, nil)
	if err := h.zones.Delete(t.Context(), zoneValue.ID); err != nil {
		t.Fatalf("delete zone: %v", err)
	}

	for range 2 {
		if err := h.service.ReconcileAll(t.Context()); err != nil {
			t.Fatalf("ReconcileAll: %v", err)
		}
	}
	doc, err := h.store.GetMailDomain(t.Context(), zoneValue.ID)
	if err != nil {
		t.Fatalf("GetMailDomain: %v", err)
	}
	if !doc.ZoneMissing || doc.Reason != reasonZoneMissing {
		t.Errorf("row = %#v", doc)
	}
	count := 0
	for _, event := range h.events.all() {
		if event.Kind == activity.KindMailZoneMissing {
			count++
		}
	}
	if count != 1 {
		t.Errorf("recorded the missing zone %d times, want once", count)
	}
}

// published answers "is DKIM working?", so it has to be an observation of the
// right key rather than of any record at that name. A record at
// "<selector>._domainkey" holding a stale key is the worst state there is — a
// receiver treats a signature that does not verify as a forgery — and it used to
// be reported as published next to a record row saying the same record was not
// present.
func TestADkimKeyIsPublishedOnlyWhenTheZoneCarriesIt(t *testing.T) {
	h := newHarness(t)
	zoneValue := h.zoneNamed(t, "acme.dev")
	bind(t, h, zoneValue, &mailv1.BindMailDomainRequest{PublishClientAutoconfig: proto.Bool(false)})

	const selector = "v1-ed25519-20260903"
	read := func(t *testing.T) *mailv1.DkimKey {
		t.Helper()
		response, err := h.service.GetMailDomain(t.Context(), connect.NewRequest(&mailv1.GetMailDomainRequest{
			ZoneId: zoneValue.ID,
		}))
		if err != nil {
			t.Fatalf("GetMailDomain: %v", err)
		}
		for _, key := range response.Msg.GetDomain().GetDkim() {
			if key.GetSelector() == selector {
				// The record row for the same selector must never disagree with
				// the key line; they are the same claim about the same record.
				for _, record := range response.Msg.GetDomain().GetRecords() {
					if record.GetName() == selector+"._domainkey" && record.GetMatches() != key.GetPublished() {
						t.Errorf("dkim published = %t but the record row says matches = %t",
							key.GetPublished(), record.GetMatches())
					}
				}
				return key
			}
		}
		t.Fatalf("no %s key on the wire", selector)
		return nil
	}

	if !read(t).GetPublished() {
		t.Error("the key this facade just published is not reported as published")
	}

	// A restore of an old backup: every record comes back as the operator's,
	// and this one comes back holding last year's key.
	restored := make([]zone.Record, 0, 8)
	for _, record := range h.records(t, zoneValue.ID) {
		if record.Managed {
			continue
		}
		if record.Name == selector+"._domainkey" {
			record.Value = `"v=DKIM1; k=ed25519; h=sha256; p=STALEKEYFROMLASTYEARAAAAAAAAAAAAAAAAAAAAAAA="`
		}
		restored = append(restored, zone.Record{
			Name:  record.Name,
			Type:  record.Type,
			TTL:   record.TTL,
			Value: record.Value,
		})
	}
	if _, err := h.zones.ImportZone(t.Context(), zoneValue.Name, restored, zone.ImportReplace, false); err != nil {
		t.Fatalf("import the restored zone: %v", err)
	}
	if read(t).GetPublished() {
		t.Error("a stale key at the selector's name is reported as published")
	}
}

// mailbox_count, used_bytes, forwarder_count and has_postmaster are plain
// scalars: when the read that fills them fails they go on the wire as zero and
// false, which reads as "no mailboxes, no postmaster". The reason has to say
// otherwise, because it is the only field on the message that can.
func TestAFailedCountReadSaysTheFiguresAreUnknown(t *testing.T) {
	h := newHarness(t)
	zoneValue := h.zoneNamed(t, "acme.dev")
	bind(t, h, zoneValue, nil)
	createMailbox(t, h, zoneValue.ID, "postmaster")

	healthy, err := h.service.GetMailDomain(t.Context(), connect.NewRequest(&mailv1.GetMailDomainRequest{
		ZoneId: zoneValue.ID,
	}))
	if err != nil {
		t.Fatalf("GetMailDomain: %v", err)
	}
	if healthy.Msg.GetDomain().GetMailboxCount() != 1 || !healthy.Msg.GetDomain().GetHasPostmaster() {
		t.Fatalf("healthy domain = %#v", healthy.Msg.GetDomain())
	}
	if !healthy.Msg.GetDomain().GetCountsAvailable() {
		t.Error("counts_available is false although both engine reads answered")
	}
	healthyStatus, err := h.service.GetMailStatus(t.Context(), connect.NewRequest(&mailv1.GetMailStatusRequest{}))
	if err != nil {
		t.Fatalf("GetMailStatus: %v", err)
	}
	if !healthyStatus.Msg.GetCountsAvailable() || healthyStatus.Msg.GetMailboxes() != 1 {
		t.Errorf("status = %#v, want a complete sum of 1 mailbox", healthyStatus.Msg)
	}

	h.engine.SetTransportError(errors.New("dial tcp 10.0.0.1:443: i/o timeout"))
	degraded, err := h.service.GetMailDomain(t.Context(), connect.NewRequest(&mailv1.GetMailDomainRequest{
		ZoneId: zoneValue.ID,
	}))
	if err != nil {
		t.Fatalf("GetMailDomain: %v", err)
	}
	domain := degraded.Msg.GetDomain()
	if domain.GetState() != mailv1.MailDomainState_MAIL_DOMAIN_STATE_DEGRADED {
		t.Errorf("state = %v, want degraded", domain.GetState())
	}
	// The zeros are unavoidable on this wire; what must not happen is a
	// message that carries them without saying what they mean.
	if !strings.Contains(domain.GetReason(), "unknown") {
		t.Errorf("reason = %q, want it to say the figures are unknown", domain.GetReason())
	}
	if domain.GetMailboxCount() != 0 || domain.GetHasPostmaster() {
		t.Errorf("counts survived a failed read: %#v", domain)
	}
	// The zeros go on the wire either way; counts_available is what stops the
	// console rendering them as "0 mailboxes" and "no postmaster".
	if domain.GetCountsAvailable() {
		t.Error("counts_available is true although the engine read failed")
	}
	// The fake fails one call per SetTransportError; the GetMailDomain above
	// consumed the first, so arm it again for the status read.
	h.engine.SetTransportError(errors.New("dial tcp 10.0.0.1:443: i/o timeout"))
	status, err := h.service.GetMailStatus(t.Context(), connect.NewRequest(&mailv1.GetMailStatusRequest{}))
	if err != nil {
		t.Fatalf("GetMailStatus: %v", err)
	}
	if status.Msg.GetCountsAvailable() {
		t.Errorf("counts_available is true although a per-domain read failed: %#v", status.Msg)
	}
}

func TestReconcileNeverDeletesAUserRecord(t *testing.T) {
	h := newHarness(t)
	zoneValue := h.zoneNamed(t, "acme.dev")
	bind(t, h, zoneValue, nil)

	// A restore brings back a zone that carries a second, conflicting MX. The
	// zone store refuses to add a member to an RRset an engine owns through the
	// ordinary create path, so an import is how this conflict reaches a bound
	// zone — and a restore is exactly the case the rule below exists for, since
	// every restored record comes back as the operator's.
	restored := make([]zone.Record, 0, len(h.records(t, zoneValue.ID))+1)
	for _, record := range h.records(t, zoneValue.ID) {
		if record.Managed {
			continue
		}
		restored = append(restored, zone.Record{
			Name:  record.Name,
			Type:  record.Type,
			TTL:   record.TTL,
			Value: record.Value,
		})
	}
	restored = append(restored, zone.Record{
		Name:  "@",
		Type:  zone.TypeMX,
		TTL:   3600,
		Value: "20 backup.example.net.",
	})
	if _, err := h.zones.ImportZone(t.Context(), zoneValue.Name, restored, zone.ImportReplace, false); err != nil {
		t.Fatalf("import the restored zone: %v", err)
	}
	if err := h.service.ReconcileAll(t.Context()); err != nil {
		t.Fatalf("ReconcileAll: %v", err)
	}
	if !slices.Contains(summarize(h.records(t, zoneValue.ID), zone.SourceUser), `MX @ 20 backup.example.net.`) {
		t.Error("the reconciler deleted a user record")
	}
	doc, err := h.store.GetMailDomain(t.Context(), zoneValue.ID)
	if err != nil {
		t.Fatalf("GetMailDomain: %v", err)
	}
	if doc.State != StateDegraded {
		t.Errorf("a conflicting user record did not degrade the row: %s", doc.State)
	}
}

func TestRebuildRecoversBindingsFromTheEngine(t *testing.T) {
	h := newHarness(t)
	zoneValue := h.zoneNamed(t, "acme.dev")
	bind(t, h, zoneValue, nil)

	// Lose the platform store's row, as a restore without platform.db does.
	if err := h.store.DeleteMailDomain(t.Context(), zoneValue.ID); err != nil {
		t.Fatalf("DeleteMailDomain: %v", err)
	}
	report, err := h.service.Rebuild(t.Context(), true)
	if err != nil {
		t.Fatalf("Rebuild(dry): %v", err)
	}
	if report.Count != 1 {
		t.Fatalf("dry run counted %d", report.Count)
	}
	if _, err := h.store.GetMailDomain(t.Context(), zoneValue.ID); err == nil {
		t.Error("the dry run wrote a row")
	}

	report, err = h.service.Rebuild(t.Context(), false)
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if report.Count != 1 {
		t.Fatalf("rebuild counted %d", report.Count)
	}
	doc, err := h.store.GetMailDomain(t.Context(), zoneValue.ID)
	if err != nil {
		t.Fatalf("GetMailDomain: %v", err)
	}
	if doc.EngineDomainID == "" {
		t.Error("the recovered row has no engine domain")
	}
}

func TestRebuildIgnoresDomainsThisControlPlaneDoesNotOwn(t *testing.T) {
	h := newHarness(t)
	if _, err := h.engine.CreateDomain(t.Context(), DomainInput{
		Name: "someone-else.test", Description: "hand made", ReportAddressURI: "mailto:postmaster",
	}); err != nil {
		t.Fatalf("CreateDomain: %v", err)
	}
	report, err := h.service.Rebuild(t.Context(), false)
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if report.Count != 0 {
		t.Errorf("adopted %d foreign domains", report.Count)
	}
}
